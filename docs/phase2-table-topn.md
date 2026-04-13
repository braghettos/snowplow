# Phase 2: Generic Materialized Aggregates Framework

**Status**: Design (pending implementation)
**Baseline**: v0.25.168 (Phase 1 cumulative-slice pagination shipped)
**Target**: 1s cold / 500ms warm / 1s fresh at 50K compositions

---

## 1. Problem Statement

### Why the dashboard is O(N)

The compositions dashboard has two widgets backed by `compositions-list`:

- **PieChart**: counts compositions by Ready condition status (True/False/Unknown)
- **Table**: shows the most recent N compositions sorted by date descending

Both widgets currently resolve by:

1. Fetching ALL compositions via the `compositions-list` restaction (49K items
   at scale) -- MGET of 49K per-item L3 keys, ~2.5s
2. Running the restaction's JQ filter to map 49K items into `.list` -- ~2.1s
3. Running the widget's `widgetDataTemplate` JQ against the full `.list` -- ~3s
4. JSON-encoding, compressing, and sending the response -- ~2.3s

**Total cold cost: ~10s at 49K compositions.** Every L3 change invalidates the
L1 entry and forces a full re-resolve.

### Why pagination alone cannot fix this

Phase 1 pagination reduces HTTP response size for early pages, but the
restaction resolution still fetches ALL 49K compositions to produce `.list`.
The widget JQ slices from that full list. Page 980 costs the same as
no-pagination.

### Why materialized aggregates fix this

Pre-compute aggregate values incrementally on every informer event
(ADD/UPDATE/DELETE). The write-path cost is O(log K) per event (K = aggregate
size). The read-path cost is O(K) for topN or O(1) for countBy. The response
for a dashboard page load becomes constant-time regardless of cluster size.

---

## 2. Architecture Overview

### 2.1 Design Principles

1. **Generic framework**: NOT hardcoded for compositions. Any GVR can register
   for aggregates via the warmup config (`cache-warmup.yaml`).
2. **Two aggregate types**: `topN` (sorted set by a field) and `countBy`
   (count items grouped by a field value). Framework code is GVR-agnostic --
   it operates on `unstructured.Unstructured` objects from informer events.
3. **Restactions are completely untouched**: No changes to restaction specs,
   JQ filters, or API call orchestration. Restactions can have any shape.
4. **Widget JQ opt-in (Option A)**: Widgets opt into aggregates by updating
   their `widgetDataTemplate` JQ to check `if .aggregate then ... else ... end`.
   The aggregate is injected into the data source that the widget JQ receives.
5. **Per-namespace RBAC buckets**: One global aggregate per GVR with
   per-namespace granularity. At read time, snowplow filters to namespaces the
   user can `list` (via `rbac.UserCan`). No per-user aggregate storage.

### 2.2 Data Flow

```
Informer event (ADD/UPDATE/DELETE on any registered GVR)
    |
    v
handleEvent()          [existing: writes L3 per-item GET key + index SET]
    |
    v
aggregates.OnEvent()   [NEW: updates materialized aggregates in Redis]
    |
    +---> ZSET + HASH  snowplow:agg:topN:{gvrKey}:{ns}       (sorted items)
    +---> HASH         snowplow:agg:countBy:{gvrKey}:{ns}     (grouped counts)
    |
    v
l3gen bump + L1 refresh
    |
    v
Widget resolution on L1 miss:
    1. Resolve restaction as usual (produces ds = data source map)
    2. Detect that the apiRef's restaction reads from a GVR with a
       registered aggregate
    3. Read aggregate from Redis, RBAC-filter by user's allowed namespaces
    4. Inject aggregate as ds["aggregate"] (alongside existing ds["list"] etc.)
    5. Widget's widgetDataTemplate JQ evaluates:
       - if .aggregate exists: use pre-computed data (O(1))
       - else: fall back to existing .list iteration (O(N))
```

### 2.3 Key Distinction: Option A (Widget JQ Change)

The aggregate does NOT bypass the widget resolver or replace the JQ engine.
Instead, the aggregate data is injected as an additional field (`.aggregate`)
into the data source map that the widget's `widgetDataTemplate` JQ already
receives. The widget JQ checks for `.aggregate` and uses it if present,
falling back to the existing `.list`-based computation if absent.

This means:
- **Restactions**: ZERO changes. The restaction still resolves as before.
- **Widget CRD schema**: ZERO changes. The `widgetDataTemplate` field shape
  is unchanged (still an array of `{forPath, expression}` objects).
- **Frontend**: ZERO changes. The frontend receives the same `widgetData`
  shape as before. Auto-pagination sees `continue=false` on page 1 when
  aggregates are used (the aggregate already contains the complete answer).
- **Widget YAML**: Updated JQ expressions that check for `.aggregate`.

---

## 3. Aggregate Types

### 3.1 topN

Maintains the top N items sorted by a specified field (e.g.
`.metadata.creationTimestamp`). Stored as a Redis ZSET (scores) + companion
HASH (item data), per namespace.

**Redis keys**:
- `snowplow:agg:topN:{gvrKey}:{ns}` -- ZSET, score = field value as float64,
  member = item UID
- `snowplow:agg:topN-data:{gvrKey}:{ns}` -- HASH, field = UID, value = JSON
  summary of the item

The ZSET stores ALL items (not just top-K) because:
1. Different users may have different RBAC-allowed namespace sets, requiring
   different top-K windows after filtering
2. Deletions require the item to exist in the set
3. ZCARD gives exact total count per namespace
4. Memory is acceptable: 49K items * ~200 bytes = ~10 MB per ZSET

**Operations (all pipelined, 1 round-trip)**:

| Event | Redis commands | Cost |
|-------|---------------|------|
| ADD | `ZADD topN {score} {uid}` + `HSET topN-data {uid} {json}` | O(log N) |
| UPDATE | `ZADD topN {score} {uid}` + `HSET topN-data {uid} {json}` | O(log N) |
| DELETE | `ZREM topN {uid}` + `HDEL topN-data {uid}` | O(log N) |
| Read | `ZREVRANGE topN 0 {K-1}` -> uids, then `HMGET topN-data {uids...}` | O(log N + K) |
| Count | `ZCARD topN` | O(1) |

**Injected shape** (as `.aggregate.topN`):
```json
{
  "items": [
    {"uid": "...", "name": "...", "ns": "...", "ts": "...", "kind": "...", "av": "...", "conditions": [...]},
    ...
  ],
  "total": 48998
}
```

### 3.2 countBy

Counts items grouped by the value of a specified field path (e.g.
`.status.conditions[?(@.type=="Ready")].status`). Stored as a Redis HASH per
namespace.

**Redis key**: `snowplow:agg:countBy:{gvrKey}:{ns}`

HASH fields are the distinct values of the grouped field plus `total`:
```
ready_true:  2450
ready_false: 25
unknown:     46523
total:       48998
```

**Operations (all O(1))**:

| Event | Redis commands | Cost |
|-------|---------------|------|
| ADD | `HINCRBY {bucket} 1` + `HINCRBY total 1` | O(1) |
| UPDATE | `HINCRBY {old_bucket} -1` + `HINCRBY {new_bucket} 1` | O(1) |
| DELETE | `HINCRBY {bucket} -1` + `HINCRBY total -1` | O(1) |
| Read | `HGETALL countBy:{key}:{ns}` | O(1) |

For UPDATE: the informer callback provides both old and new objects. Classify
each into a bucket, decrement old, increment new (no-op if bucket unchanged).

**Injected shape** (as `.aggregate.countBy`):
```json
{
  "ready_true": 2450,
  "ready_false": 25,
  "unknown": 46523,
  "total": 48998
}
```

---

## 4. RBAC: Per-Namespace Buckets + UserCan at Read Time

### Design

All aggregates are stored **per namespace**. There is no per-user aggregate
storage. RBAC filtering happens at read time:

1. The widget resolver identifies the GVR(s) the aggregate covers
2. It reads the list of all namespaces that have data in the aggregate
   (from the aggregate's namespace registry or from L3 index keys)
3. For each namespace, it calls `rbac.UserCan(ctx, rbac.UserCanOptions{
   Verb: "list", GroupResource: gvr.GroupResource(), Namespace: ns})`
4. Only namespaces where the user has `list` permission are included

This uses the existing informer-backed RBAC cache (`snowplow:rbac:{user}`
HASH in Redis, 24h TTL). Cache hits are O(1) per namespace. At 500
namespaces, this is 500 RBAC cache lookups -- all pipelined against the
in-memory RBAC HASH, total cost <1ms.

### User Scenarios

**admin (cluster-admin)**: `UserCan` returns true for all namespaces.
Aggregate sums all namespace buckets. Sees total=48998.

**cyberjoker (demo-system only for compositions)**: `UserCan` returns true
only for demo-system. Aggregate sums only the demo-system bucket. demo-system
has 0 compositions, so sees total=0.

**future user (bench-ns-0 through bench-ns-9)**: `UserCan` returns true for
10 namespaces. Sees ~980 compositions (10 * ~98 per namespace).

### Why Not Per-User Aggregates

1000 users * 49K items = 49M ZSET entries. ~10 GB Redis. Unacceptable.
Per-namespace + read-time RBAC filtering uses ~20 MB total regardless of
user count.

### Cost at Read Time (500 namespaces, K=50)

For **topN** (table):
- Read allowed namespaces: 500 RBAC lookups from cache = <1ms
- Read top-K from each allowed namespace's ZSET: 500 `ZREVRANGE` commands
  pipelined in 1 round-trip, each returning up to K items
- Merge-sort in Go: up to 500*K items merged, take top K = <2ms
- **Total: ~5ms**

For **countBy** (piechart):
- Read allowed namespaces: <1ms (same as above)
- `HGETALL` from each allowed namespace: 500 commands pipelined = 1 round-trip
- Sum fields in Go: <0.1ms
- **Total: ~3ms**

---

## 5. Aggregate Registration

### Warmup Config Extension

Aggregates are registered in `cache-warmup.yaml` under a new `aggregates`
section. The framework code is GVR-agnostic -- it uses the config to know
which GVRs to track and what aggregate type to maintain.

```yaml
warmup:
  # ... existing gvrs, l1RestActions, autoDiscoverGroups, categories ...

  # Materialized aggregates: pre-computed values maintained incrementally
  # by informer events. Each entry registers one aggregate type for one GVR.
  aggregates:
    # Compositions piechart: count by Ready condition status
    - group: composition.krateo.io
      version: v1-2-2
      resource: githubscaffoldingwithcompositionpages
      type: countBy
      field: conditions.Ready    # maps to StatusBucket classification
      ttl: 6h

    # Compositions table: top N items sorted by creation timestamp
    - group: composition.krateo.io
      version: v1-2-2
      resource: githubscaffoldingwithcompositionpages
      type: topN
      field: creationTimestamp    # .metadata.creationTimestamp as ZSET score
      limit: 50                  # max items to return per read (across all ns)
      ttl: 6h
```

When a new CRD appears via `autoDiscoverGroups` and matches a registered
aggregate config entry (by group suffix), the aggregate is auto-registered
for the new GVR. This handles the case where composition CRD names/versions
change without config edits.

### Matching Logic

```go
// In the aggregate manager, when a new GVR is discovered:
func (m *Manager) TryRegister(gvr schema.GroupVersionResource) {
    for _, cfg := range m.config.Aggregates {
        if gvr.Group == cfg.Group || strings.HasSuffix(gvr.Group, cfg.Group) {
            m.register(gvrKey(gvr), cfg.Type, cfg.Field, cfg.Limit, cfg.TTL)
        }
    }
}
```

---

## 6. Integration Points

### 6.1 Widget Resolver: Aggregate Injection

The widget resolver injects the aggregate into the data source BEFORE
evaluating the widget's `widgetDataTemplate` JQ. The restaction is resolved
as usual -- the aggregate is an addition, not a replacement.

In `internal/resolvers/widgets/resolve.go`, the `resolveApiRef` function
returns `ds` (the data source map). After `resolveApiRef` returns, and before
the JQ evaluation, inject the aggregate:

```go
func Resolve(ctx context.Context, opts ResolveOptions) (*Widget, error) {
    // ... existing code: resolve apiRef ...
    ds, err := resolveApiRef(ctx, opts)
    // ... error handling ...

    // ── Aggregate injection ──────────────────────────────────────────
    // If the apiRef's restaction reads from a GVR with a registered
    // aggregate, read the aggregate from Redis, RBAC-filter it, and
    // inject it as ds["aggregate"]. The widget JQ can then check for
    // .aggregate and use the pre-computed data.
    if aggMgr := aggregates.FromContext(ctx); aggMgr != nil && ds != nil {
        injectAggregate(ctx, aggMgr, opts, ds)
    }

    // ... existing code: inject .slice, evaluate JQ, etc. (UNCHANGED) ...
}

func injectAggregate(ctx context.Context, aggMgr *aggregates.Manager,
    opts ResolveOptions, ds map[string]any) {

    apiRef, err := GetApiRef(opts.In.Object)
    if err != nil {
        return
    }

    // Look up which GVRs this apiRef depends on that have registered aggregates.
    gvrKeys := aggMgr.GVRsForApiRef(apiRef.Name, apiRef.Namespace)
    if len(gvrKeys) == 0 {
        return
    }

    // Get user's allowed namespaces via RBAC.
    allowedNS := getAllowedNamespaces(ctx, gvrKeys)

    agg := map[string]any{}

    // Read each registered aggregate type for these GVRs.
    for _, gk := range gvrKeys {
        if aggMgr.HasTopN(gk) {
            items, total, err := aggMgr.ReadTopN(ctx, gk, allowedNS,
                0, aggMgr.Limit(gk))
            if err == nil {
                agg["topN"] = map[string]any{
                    "items": items,
                    "total": total,
                }
            }
        }
        if aggMgr.HasCountBy(gk) {
            counts, total, err := aggMgr.ReadCountBy(ctx, gk, allowedNS)
            if err == nil {
                counts["total"] = total
                agg["countBy"] = counts
            }
        }
    }

    if len(agg) > 0 {
        ds["aggregate"] = agg
    }
}

func getAllowedNamespaces(ctx context.Context, gvrKeys []GVRKey) []string {
    // For each GVR, get all namespaces that have data, then filter
    // by rbac.UserCan(ctx, {Verb: "list", GroupResource: gvr.GroupResource(), Namespace: ns}).
    // Returns the intersection of namespaces the user can list.
    // Uses the existing RBAC cache (O(1) per lookup).
    ...
}
```

The key point: the restaction resolution, JQ evaluation pipeline, L1 cache
write, and all other existing code paths are UNCHANGED. The aggregate is
just an extra field in `ds` that the JQ can optionally consume.

### 6.2 Widget YAML Changes (Option A)

Widgets opt into aggregates by updating their `widgetDataTemplate` JQ
expressions. Each expression checks `if .aggregate then ... else ... end`.

#### PieChart: compositions-piechart

The piechart counts compositions by Ready condition. With aggregates, the
countBy data provides these counts directly.

**Before** (current JQ for `series.data[0].value`):
```jq
${
  (if .slice then .list[0 : (.slice.page * .slice.perPage)] else .list end)
  | map(select((.conditions // [])
      | map(select(.type == "Ready" and .status == "True"))
      | length > 0))
  | length
}
```

**After** (aggregate-aware):
```jq
${
  if .aggregate.countBy then
    .aggregate.countBy.ready_true
  else
    (if .slice then .list[0 : (.slice.page * .slice.perPage)] else .list end)
    | map(select((.conditions // [])
        | map(select(.type == "Ready" and .status == "True"))
        | length > 0))
    | length
  end
}
```

All five piechart expressions follow the same pattern: check
`.aggregate.countBy`, read the pre-computed field if present, fall back to
the `.list` iteration otherwise.

#### Table: compositions-table

The table maps compositions to row arrays and sorts by date. With aggregates,
the topN data provides the pre-sorted, pre-sliced items.

**Before** (current JQ for `data`):
```jq
${
  (
    (if .slice then .list[0 : (.slice.page * .slice.perPage)] else .list end)
    | map([{valueKey: "key", ...}, {valueKey: "name", ...}, ...])
    | sort_by(...)
    | reverse
  )
}
```

**After** (aggregate-aware):
```jq
${
  if .aggregate.topN then
    (
      .aggregate.topN.items
      | map([
          { valueKey: "key",        kind: "jsonSchemaType", type: "string", stringValue: .uid },
          { valueKey: "name",       kind: "jsonSchemaType", type: "string", stringValue: .name },
          { valueKey: "namespace",  kind: "jsonSchemaType", type: "string", stringValue: .ns },
          { valueKey: "date",       kind: "jsonSchemaType", type: "string", stringValue: .ts },
          { valueKey: "status",     kind: "jsonSchemaType", type: "string",
            stringValue: (
              if (.conditions | length > 0) then
                (.conditions[]? | select(.type == "Ready") | "Ready: " + .status)?
                // "Status not available"
              else
                "Status not available"
              end
            ) },
          { valueKey: "kind",       kind: "jsonSchemaType", type: "string", stringValue: .kind },
          { valueKey: "apiversion", kind: "jsonSchemaType", type: "string", stringValue: .av }
        ])
    )
  else
    (
      (if .slice then .list[0 : (.slice.page * .slice.perPage)] else .list end)
      | map([
          { valueKey: "key",        kind: "jsonSchemaType", type: "string", stringValue: .uid },
          { valueKey: "name",       kind: "jsonSchemaType", type: "string", stringValue: .name },
          { valueKey: "namespace",  kind: "jsonSchemaType", type: "string", stringValue: .ns },
          { valueKey: "date",       kind: "jsonSchemaType", type: "string", stringValue: .ts },
          { valueKey: "status",     kind: "jsonSchemaType", type: "string",
            stringValue: (
              if (.conditions | length > 0) then
                (.conditions[]? | select(.type == "Ready") | "Ready: " + .status)?
                // "Status not available"
              else
                "Status not available"
              end
            ) },
          { valueKey: "kind",       kind: "jsonSchemaType", type: "string", stringValue: .kind },
          { valueKey: "apiversion", kind: "jsonSchemaType", type: "string", stringValue: .av }
        ])
      | sort_by( ([.[] | select(.valueKey == "date") | .stringValue] | first) )
      | reverse
    )
  end
}
```

Note: the aggregate topN items are already sorted by date descending (from
the ZSET's ZREVRANGE), so no `sort_by | reverse` is needed in the aggregate
branch.

### 6.3 Restaction Changes

**NONE.** The restaction `compositions-list` is not modified. It still
resolves the same way. The aggregate data is injected separately into the
widget's data source alongside the restaction output.

### 6.4 Frontend Changes

**NONE.** The frontend receives the same `widgetData` shape as before. When
aggregates are active:
- The piechart's first (and only) response already has the correct totals.
  `slice.continue` is false on page 1 -- auto-pagination stops immediately.
- The table's first response has the top N items. `slice.continue` is false
  on page 1 because the aggregate branch produces a complete, final dataset
  (no cumulative slicing needed). The frontend sees `continue=false` and
  does not fire additional page requests.

### 6.5 CRD Schema Changes

**NONE.** No CRD schema changes for frontend or snowplow.

---

## 7. Watcher Integration

### 7.1 handleEvent Changes

In `handleEvent`, after the existing L3 per-item writes, call the aggregate
manager:

```go
if rw.aggregates != nil && rw.aggregates.IsRegistered(gvrKey) {
    summary := aggregates.ExtractSummary(uns)
    if summary != nil {
        key := aggregates.AggregateKey{GVRKey: gvrKey, Namespace: ns}
        var oldSummary *aggregates.ItemSummary
        if eventType == "update" {
            if oldUns, ok := toUnstructured(old); ok {
                oldSummary = aggregates.ExtractSummary(oldUns)
            }
        }
        rw.aggregates.OnEvent(ctx, key, eventType, oldSummary, summary)
    }
}
```

The `old` parameter in handleEvent's signature must be preserved (not
discarded as `_`). The informer callbacks already provide `old` in UpdateFunc.

### 7.2 ExtractSummary

`ExtractSummary` is GVR-agnostic. It reads standard Kubernetes metadata
fields from any `unstructured.Unstructured` object:

```go
type ItemSummary struct {
    UID        string `json:"uid"`
    Name       string `json:"name"`
    Namespace  string `json:"ns"`
    Timestamp  string `json:"ts"`
    Kind       string `json:"kind"`
    APIVersion string `json:"av"`
    Conditions []Cond `json:"conditions"`
}

func ExtractSummary(uns *unstructured.Unstructured) *ItemSummary {
    return &ItemSummary{
        UID:        string(uns.GetUID()),
        Name:       uns.GetName(),
        Namespace:  uns.GetNamespace(),
        Timestamp:  uns.GetCreationTimestamp().Format(time.RFC3339),
        Kind:       uns.GetKind(),
        APIVersion: uns.GetAPIVersion(),
        Conditions: extractConditions(uns),
    }
}
```

The `extractConditions` function reads `.status.conditions` from the
unstructured object. This is a standard Kubernetes convention used by all
composition types.

### 7.3 StatusBucket Classification

For `countBy` aggregates with `field: conditions.Ready`:

```go
func StatusBucket(conditions []Cond) string {
    for _, c := range conditions {
        if c.Type == "Ready" {
            switch c.Status {
            case "True":
                return "ready_true"
            case "False":
                return "ready_false"
            default:
                return "unknown"
            }
        }
    }
    return "unknown"
}
```

---

## 8. Aggregate Lifecycle

### 8.1 Startup: Full Rebuild

On pod startup, before L1 warmup, the aggregate manager rebuilds all
registered aggregates from L3 data:

1. Read registered aggregate configs from `cache-warmup.yaml`
2. For each GVR with aggregates, scan `snowplow:list-idx:{gvrKey}:{ns}` to
   get all item names per namespace
3. MGET all item data from L3 GET keys
4. Rebuild ZSET + HASH from scratch (DEL old keys, then batch ZADD/HSET)

Cost at 50K: ~2s (reuses existing `AssembleListFromIndex`). Runs once.

### 8.2 Steady State: Incremental Updates

After startup, `handleEvent` updates aggregates incrementally on every
informer event. No full rebuild needed.

### 8.3 Periodic Consistency Check

Every 5 minutes (configurable), compare ZCARD of each topN ZSET against the
SCARD of the corresponding L3 list index. If they diverge by more than 1%,
trigger a full rebuild. This catches:
- Missed events (informer reconnect gaps)
- Redis key expiry races
- Bugs in incremental update logic

---

## 9. Pagination and Slice Behavior with Aggregates

When the aggregate path is used, the widget JQ produces a complete, final
result on the first page request. The widget resolver detects this and sets
`slice.continue = false`:

- The existing slice logic in `resolve.go` checks `len(ds["list"])` to
  compute `hasNext`. When aggregates are active, the JQ does not use `.list`
  for the output -- it uses `.aggregate.topN.items` (already limited to K
  items) or `.aggregate.countBy` (scalar counts). The `.list` array is still
  present in `ds` (the restaction still resolves), but the widget output
  is small and complete.
- The frontend sees `continue=false` and stops auto-pagination.

**Optimization (future)**: When aggregates are active, skip the restaction
resolution entirely (the full `.list` is not needed). This saves the 2.5s
MGET + 2.1s JQ cost. Implementation: check if ALL widget JQ expressions
use `.aggregate` (no `.list` references) and if so, skip `resolveApiRef`.
This is a Phase 2.5 optimization, not required for the initial launch.

---

## 10. Performance Estimates

### Table Widget at 50K Compositions

| Metric | Phase 1 (current) | Phase 2 (aggregate) | Improvement |
|--------|-------------------|---------------------|-------------|
| Page 1 cold | ~3s | ~200ms | 15x |
| Page 1 cold (with restaction skip, Phase 2.5) | ~3s | ~50ms | 60x |
| L1 hit (any page) | ~60ms | ~60ms | same |
| L1 refresh (background) | ~10s | ~200ms | 50x |
| Response size (page 1) | ~5 KB | ~5 KB | same |

### Piechart Widget at 50K Compositions

| Metric | Phase 1 (current) | Phase 2 (aggregate) | Improvement |
|--------|-------------------|---------------------|-------------|
| Cold | ~0.9s | ~50ms | 18x |
| L1 hit | ~60ms | ~60ms | same |
| Response size | ~2 KB | ~500 B | 4x |

### Write Path Overhead

| Operation | Current handleEvent | With aggregates | Overhead |
|-----------|-------------------|-----------------|----------|
| Redis commands per event | 3-5 | 5-9 (+ZADD+HSET+HINCRBY) | +2-4 commands |
| Latency per event | ~1ms | ~2ms | +1ms |
| Pipeline round-trips | 1 | 1 (all commands batched) | 0 |

### Redis Memory Overhead

| Structure | Count | Per-item | Total |
|-----------|-------|----------|-------|
| ZSET (topN, 500 ns) | 500 | ~20 KB | 10 MB |
| HASH (topN-data, 500 ns) | 500 | ~20 KB | 10 MB |
| HASH (countBy, 500 ns) | 500 | ~200 B | 100 KB |
| **Total** | | | **~20 MB** |

---

## 11. What Changes Per File

### Snowplow (`braghettos/snowplow`)

| File | Change | Size |
|------|--------|------|
| `internal/cache/aggregates/aggregates.go` | **NEW**: Manager, OnEvent, ReadTopN, ReadCountBy, Rebuild | L |
| `internal/cache/aggregates/summary.go` | **NEW**: ExtractSummary, StatusBucket, ItemSummary | S |
| `internal/cache/aggregates/aggregates_test.go` | **NEW**: unit tests for ZSET/HASH operations, RBAC filtering | M |
| `internal/cache/watcher.go` | **EDIT**: handleEvent calls aggregates.OnEvent for registered GVRs | S |
| `internal/cache/watcher.go` | **EDIT**: ResourceWatcher struct gets `aggregates *aggregates.Manager` | S |
| `internal/cache/keys.go` | **EDIT**: add AggTopNKey, AggTopNDataKey, AggCountByKey functions | S |
| `internal/resolvers/widgets/resolve.go` | **EDIT**: add injectAggregate call after resolveApiRef | S |
| `internal/resolvers/widgets/aggregate.go` | **NEW**: injectAggregate, getAllowedNamespaces | M |
| `internal/handlers/dispatchers/prewarm.go` | **EDIT**: call aggregates.Rebuild on startup | S |
| `main.go` | **EDIT**: create aggregates.Manager from warmup config, pass to ResourceWatcher | S |

### Portal (`braghettos/portal`)

| File | Change | Size |
|------|--------|------|
| `blueprint/templates/piechart.dashboard-compositions-panel-row-piechart.yaml` | **EDIT**: JQ checks `.aggregate.countBy` | S |
| `blueprint/templates/table.dashboard-compositions-panel-row-table.yaml` | **EDIT**: JQ checks `.aggregate.topN` | S |

### Frontend (`braghettos/frontend-draganddrop`)

No changes.

**Total new code**: ~500-700 lines (aggregates package + widget integration).
**Total edited code**: ~80 lines across existing files + widget YAML JQ updates.

---

## 12. Implementation Order

| Step | Task | Depends on | Estimate |
|------|------|------------|----------|
| 2.0 | Warmup config: add `aggregates` section, parse in main.go | -- | 0.5 day |
| 2.1 | `internal/cache/aggregates` package: Manager, OnEvent, ReadTopN, ReadCountBy | 2.0 | 2 days |
| 2.2 | Wire into `watcher.go` handleEvent + startup Rebuild | 2.1 | 0.5 day |
| 2.3 | Widget resolver: `injectAggregate` + `getAllowedNamespaces` | 2.1 | 1 day |
| 2.4 | Portal: update piechart + table widget YAML JQ expressions | 2.3 | 0.5 day |
| 2.5 | Consistency check (periodic ZCARD vs SCARD) | 2.1 | 0.5 day |
| 2.6 | Measurement at 50K: verify north-star metrics | 2.4 | 1 day |
| 2.7 | (Optional) Skip restaction resolution when all JQ uses `.aggregate` | 2.6 | 1 day |

**Total**: ~6-7 days of implementation.

---

## 13. Rollout and Migration

### Feature Flag

Aggregates are enabled/disabled via environment variable:

```
AGGREGATES_ENABLED=true   # default: false during rollout
```

When disabled, `injectAggregate` is a no-op. The widget JQ `if .aggregate`
check falls through to the existing `.list` path. Zero risk to existing
behavior.

### Rollout Sequence

1. **Deploy with AGGREGATES_ENABLED=false**: aggregates are built in the
   background by handleEvent. Verify ZSET/HASH contents match expected values
   by comparing against the restaction JQ output.
2. **Enable aggregates**: the widget JQ detects `.aggregate` and uses it.
   Verify widgetData output matches the non-aggregate path exactly.
3. **Remove flag**: once stable for 1 week, make aggregates the default.

### Backward Compatibility

- Widgets without aggregate-aware JQ continue to work unchanged (`.aggregate`
  is ignored by JQ expressions that don't reference it).
- Widgets with aggregate-aware JQ fall back gracefully when aggregates are
  disabled (the `else` branch uses `.list`).
- No CRD schema changes, no restaction changes, no frontend changes.

---

## 14. Risks and Open Questions

### Risks

1. **Stale aggregates after pod restart**: The startup Rebuild (section 8.1)
   fixes this. Cost is ~2s at 50K, runs once.

2. **RBAC cache staleness**: If a user gains/loses namespace access, the
   aggregate read uses stale RBAC data until the next RBAC cache refresh
   (24h TTL). This is the same staleness window as current L1 cache.

3. **Conditions change without creationTimestamp change**: A composition's
   status conditions can change (Ready: False -> True) without changing
   creationTimestamp. The UPDATE handler updates the HASH companion data and
   the countBy counts. The ZSET score stays the same. Correct behavior.

4. **Multi-CRD merge correctness**: The table must merge compositions across
   multiple CRDs sorted by date. The Go merge-sort handles this correctly.
   At 1-5 CRDs, merge cost is negligible.

5. **Widget JQ duplication**: The aggregate-aware JQ duplicates the cell
   mapping logic in both the `if` and `else` branches. This is acceptable
   because the JQ is declarative and self-contained in the widget YAML.
   A future improvement could extract the cell mapping into a reusable
   JQ function.

### Open Questions

1. **Should aggregates eventually replace the restaction entirely for these
   widgets?** Phase 2.5 proposes skipping restaction resolution when all JQ
   expressions use `.aggregate`. This is an optimization, not a requirement.
   The restaction remains as the canonical data source and fallback.

2. **What about the `allowedResources` field in the table widget spec?**
   Currently empty. If populated in the future, it would filter which
   composition kinds appear in the table. The aggregate reader can filter by
   GVR (each CRD has its own ZSET). No design change needed.

3. **What about other sort orders?** The topN ZSET is ordered by
   creationTimestamp. Other sort orders (by name, by status) would need
   separate ZSETs. This is a future concern -- the current table widget only
   sorts by date.

---

## Appendix A: Example Redis State at 50K

```
# One composition CRD with 48000 items across 500 namespaces:

snowplow:agg:topN:composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages:bench-ns-001
  Type: ZSET
  Members: ~96 (48000 / 500)
  Memory: ~20 KB

snowplow:agg:topN-data:composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages:bench-ns-001
  Type: HASH
  Fields: ~96
  Memory: ~20 KB

snowplow:agg:countBy:composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages:bench-ns-001
  Type: HASH
  Fields: 4 (ready_true, ready_false, unknown, total)
  Memory: ~200 bytes

# Total across 500 namespaces:
#   ZSET (topN): 500 * 20 KB = 10 MB
#   HASH (topN-data): 500 * 20 KB = 10 MB
#   HASH (countBy): 500 * 200 B = 100 KB
#   Total aggregate memory: ~20 MB
```

## Appendix B: Read Path Pseudocode (Widget Resolver)

```go
// In resolve.go, after resolveApiRef returns ds:

func injectAggregate(ctx context.Context, aggMgr *aggregates.Manager,
    opts ResolveOptions, ds map[string]any) {

    apiRef, _ := GetApiRef(opts.In.Object)
    gvrKeys := aggMgr.GVRsForApiRef(apiRef.Name, apiRef.Namespace)
    if len(gvrKeys) == 0 {
        return // no aggregates for this widget's apiRef
    }

    // Get user's RBAC-allowed namespaces for these GVRs.
    allowedNS := getAllowedNamespaces(ctx, gvrKeys)

    agg := map[string]any{}

    for _, gk := range gvrKeys {
        if aggMgr.HasTopN(gk) {
            items, total, _ := aggMgr.ReadTopN(ctx, gk, allowedNS, 0, aggMgr.Limit(gk))
            agg["topN"] = map[string]any{"items": items, "total": total}
        }
        if aggMgr.HasCountBy(gk) {
            counts, total, _ := aggMgr.ReadCountBy(ctx, gk, allowedNS)
            counts["total"] = total
            agg["countBy"] = counts
        }
    }

    if len(agg) > 0 {
        ds["aggregate"] = agg
    }
    // ds now has both ds["list"] (from restaction) and ds["aggregate"] (from framework).
    // The widget JQ decides which to use via `if .aggregate then ... else ... end`.
}

func getAllowedNamespaces(ctx context.Context, gvrKeys []GVRKey) []string {
    // Get all namespaces with data from the aggregate index.
    allNS := aggMgr.NamespacesForGVR(gvrKeys[0])

    // Filter by RBAC: call rbac.UserCan for each namespace.
    var allowed []string
    for _, ns := range allNS {
        if rbac.UserCan(ctx, rbac.UserCanOptions{
            Verb:          "list",
            GroupResource: gvrKeys[0].GroupResource(),
            Namespace:     ns,
        }) {
            allowed = append(allowed, ns)
        }
    }
    return allowed
}
```

## Appendix C: Write Path Pseudocode (handleEvent)

```go
// In watcher.go handleEvent, after L3 per-item writes:

if rw.aggregates != nil && rw.aggregates.IsRegistered(gvrKey) {
    summary := aggregates.ExtractSummary(uns)
    if summary != nil {
        key := aggregates.AggregateKey{
            GVRKey:    gvrKey,
            Namespace: ns,
        }
        var oldSummary *aggregates.ItemSummary
        if eventType == "update" {
            if oldUns, ok := toUnstructured(old); ok {
                oldSummary = aggregates.ExtractSummary(oldUns)
            }
        }
        // All Redis commands pipelined in a single round-trip:
        //   topN:    ZADD + HSET  (or ZREM + HDEL for delete)
        //   countBy: HINCRBY x2   (decrement old bucket, increment new)
        rw.aggregates.OnEvent(ctx, key, eventType, oldSummary, summary)
    }
}
```

## Appendix D: Widget YAML Diffs

### PieChart (compositions-piechart)

Each of the 5 expressions gains an `if .aggregate.countBy then ... else ... end`
wrapper. Example for `series.data[0].value` (Ready:True count):

```yaml
- forPath: series.data[0].value
  expression: >
    ${
      if .aggregate.countBy then
        .aggregate.countBy.ready_true
      else
        (if .slice then .list[0 : (.slice.page * .slice.perPage)] else .list end)
        | map(select((.conditions // [])
            | map(select(.type == "Ready" and .status == "True"))
            | length > 0))
        | length
      end
    }
```

### Table (compositions-table)

The single `data` expression gains an `if .aggregate.topN then ... else ... end`
wrapper. The aggregate branch maps `.aggregate.topN.items` through the same
cell-formatting logic but skips `sort_by | reverse` (items are pre-sorted).

See section 6.2 for the full JQ.
