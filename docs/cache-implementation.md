# Snowplow Cache Implementation

**Last updated**: 2026-04-11
**Codebase version**: v0.25.168+
**Audience**: Developers working on Snowplow, Krateo operators, and anyone evaluating the caching design

---

## Table of Contents

1. [Why Cache](#1-why-cache)
2. [Cache Layers](#2-cache-layers)
3. [Cache Fill -- How Data Enters Each Layer](#3-cache-fill----how-data-enters-each-layer)
4. [Cache Update -- How Data Changes](#4-cache-update----how-data-changes)
5. [Cache Invalidation -- How Data Leaves](#5-cache-invalidation----how-data-leaves)
6. [Cache Consistency Model](#6-cache-consistency-model)
7. [Cache Agnosticism](#7-cache-agnosticism)
8. [Startup Sequence](#8-startup-sequence)
9. [Concurrency Controls](#9-concurrency-controls)
10. [Performance Characteristics](#10-performance-characteristics)
11. [Known Limitations](#11-known-limitations)

---

## 1. Why Cache

Snowplow is the backend that powers the Krateo portal. The portal renders
dashboards by resolving **widgets**, each of which references a **RESTAction**
(a Kubernetes Custom Resource). A RESTAction defines one or more Kubernetes API
calls, a set of JQ transformations, and produces a JSON blob that the frontend
renders as a table, piechart, navigation menu, or similar UI element.

The problem is fan-out. A single dashboard page triggers 24-28 HTTP requests to
Snowplow (`/call?resource=widgets&...` and `/call?resource=restactions&...`).
Each RESTAction may fan out to multiple K8s API calls -- one per namespace in
the cluster. At 50,000 compositions across 500 namespaces, a single RESTAction
that lists compositions issues 500+ K8s API GET/LIST calls, each returning up
to 1,000 items. Without caching, a dashboard load takes approximately 5 seconds
per widget call. With caching, it takes approximately 0.3 seconds.

The cache is a **transparent, disposable acceleration layer**. It mirrors
Kubernetes API state in Redis for faster resolution. The Kubernetes API remains
the single source of truth. If Redis is wiped entirely, Snowplow rebuilds the
cache from K8s watches. Functionality is preserved; only latency degrades until
the cache warms up.

This is a deliberate architectural choice aligned with the Krateo manifesto
(see braghettos/krateo-manifesto discussion #5): the cache must be read-only,
disposable, and removable without breaking Krateo functionality. A persistent
datastore (e.g., PostgreSQL with custom views) would violate the manifesto by
creating a competing source of truth outside Kubernetes.

---

## 2. Cache Layers

The cache has four distinct layers. Each layer has different key patterns,
storage characteristics, and lifecycles.

### 2.1 L3 -- Informer Cache (Per-Object)

**Purpose**: Mirror individual Kubernetes objects in Redis, maintained in
real time by informer WATCH events.

**Redis key patterns**:

| Key pattern | Type | Description |
|---|---|---|
| `snowplow:get:{group/version/resource}:{namespace}:{name}` | STRING | Single K8s object, JSON + zstd compressed |
| `snowplow:list-idx:{group/version/resource}:{namespace}` | SET | Index of object names in a namespace. Members are `name` for namespace-scoped, `ns/name` for cluster-wide |
| `snowplow:list:{group/version/resource}:{namespace}` | STRING | Legacy monolithic list blob (dual-write during migration, being phased out) |
| `snowplow:l3gen:{gvrKey}:{namespace}` | STRING | Latest resourceVersion for a GVR+namespace, used by the l3gen scanner to detect changes |

**TTL**: Configurable per GVR via warmup config (default: 1 hour, defined as
`DefaultResourceTTL` in `internal/cache/redis.go:33`). Proactive expiry refresh
re-fetches resources before TTL expiry so the cache never goes cold.

**Populated by**: Informer `handleEvent` (add/update/delete), warmup
`warmGVR` (startup LIST), `reconcileGVR` (periodic reconciliation from
informer store).

**Read by**: RESTAction resolver (`internal/resolvers/restactions/api/resolve.go:201-289`)
as a substitute for live K8s API calls.

**Key construction**: `internal/cache/keys.go:67-81`

### 2.2 L1 -- Resolved Output Cache (Per-User)

**Purpose**: Store the fully-resolved, marshaled JSON output of a widget or
RESTAction. This is the exact HTTP response body. On a cache hit, Snowplow
skips the entire resolution pipeline: no K8s API calls, no JQ evaluation, no
JSON marshaling.

**Redis key pattern**:

| Key pattern | Type | Description |
|---|---|---|
| `snowplow:resolved:{username}:{group/version/resource}:{namespace}:{name}[:p{page}-pp{perPage}]` | STRING | Resolved JSON blob, zstd compressed |
| `snowplow:resolved-idx:{username}` | SET | Index of all L1 keys for a user (O(1) invalidation) |

**TTL**: 1 hour (`ResolvedCacheTTL` in `internal/cache/redis.go:34`). Freshness
is maintained by the event-driven invalidation system, not by TTL. The TTL
exists only for memory management -- stale entries from users who stopped
browsing are reclaimed.

**Populated by**: `l1cache.ResolveAndCache` (on widget/restaction resolve,
`internal/resolvers/restactions/l1cache/l1cache.go:117-131`), startup L1 prewarm
(`internal/handlers/dispatchers/prewarm.go:198-257`), and background L1 refresh
(`internal/handlers/dispatchers/l1_refresh.go:36-354`).

**Read by**: Widget HTTP handler (`internal/handlers/dispatchers/widgets.go:77`),
RESTAction HTTP handler (`internal/handlers/dispatchers/restactions.go:66`),
and widget apiref resolver (`internal/resolvers/widgets/apiref/resolve.go:62`).

**Key construction**: `internal/cache/keys.go:122-129`

L1 keys are per-user because different users may see different data due to RBAC
filtering. The cache key includes the username but not the user's permissions.
Permissions are checked at read time via the RBAC gate.

### 2.3 RBAC Cache (Per-User)

**Purpose**: Cache authorization decisions so repeated RBAC checks do not
require a SelfSubjectAccessReview API call to the K8s API server for every
object in every namespace.

**Redis key pattern**:

| Key pattern | Type | Description |
|---|---|---|
| `snowplow:rbac:{username}` | HASH | All RBAC decisions for a user. Fields: `{verb}:{group/resource}:{namespace}`, values: `1` (allowed) or `0` (denied) |

**TTL**: 24 hours (`rbacCacheTTL` in `internal/rbac/rbac.go:17`). Proactive
invalidation is handled by the RBAC watcher (informer-backed).

**Populated by**: `rbac.UserCan` on-demand via SelfSubjectAccessReview
(`internal/rbac/rbac.go:36-97`), `PreWarmRBACForUser` at user login or startup
(`internal/handlers/dispatchers/prewarm.go:290-398`), and
`WarmRBACForAllUsers` at startup.

**Read by**: `rbac.UserCan` (cache lookup before API call,
`internal/rbac/rbac.go:48-56`), and the L3 RBAC gate in the RESTAction resolver
(`internal/resolvers/restactions/api/resolve.go:237-251`).

### 2.4 Dependency Tracking Indexes

These are not a cache layer per se, but Redis data structures that enable
targeted cache invalidation.

**Redis key patterns**:

| Key pattern | Type | Description |
|---|---|---|
| `snowplow:l1dep:{gvrKey}:{ns}:{name}` | SET | Maps a specific K8s resource (or LIST of resources) to L1 keys that accessed it during resolution |
| `snowplow:l1api:{gvrKey}` | SET | Maps a GVR to L1 keys that depend on any resource of that type (API-level dep) |
| `snowplow:l1gvr:{gvrKey}` | SET | Broad GVR-level index (fallback, not used for targeted refresh due to cardinality) |

**TTL**: 2 hours (`ReverseIndexTTL` in `internal/cache/redis.go:35`).

**Populated by**: `RegisterL1Dependencies` and `RegisterL1ApiDeps`
(`internal/cache/keys.go:171-247`), called after every L1 resolve.

**Read by**: `scanL3Gens` (`internal/cache/watcher.go:362-396`) and
`reconcileGVR` (`internal/cache/watcher.go:1198-1222`) to find affected L1 keys
when L3 changes.

---

## 3. Cache Fill -- How Data Enters Each Layer

### 3.1 L3 Fill Paths

**Path 1: Informer handleEvent (runtime)**

When a Kubernetes informer receives an ADD or UPDATE event:

1. The informer's `SetTransform` strips `managedFields` and
   `last-applied-configuration` annotations before the object enters the
   in-memory store (`watcher.go:598-602`).
2. `handleEvent` fires (`watcher.go:671`).
3. For add/update: the object is written to a per-item GET key via
   `SetForGVR`, and added to the namespace and cluster-wide index SETs via
   `SAddWithTTL` (`watcher.go:716-733`).
4. For delete: the GET key is deleted, and the name is removed from the
   index SETs via `SRemMembers` (`watcher.go:706-714`).
5. The latest `resourceVersion` is stored in the l3gen key via a Lua script
   that ensures monotonic updates (`watcher.go:757-765`).

**Path 2: warmGVR (startup)**

During startup, the warmer issues a LIST call for each configured GVR via the
dynamic client (`warmup.go:207-209`). For each item in the LIST response:

1. Annotations are stripped (`warmup.go:219`).
2. A per-item GET key is written (`warmup.go:220-222`).
3. Per-namespace and cluster-wide index SETs are built and written atomically
   via `ReplaceSetWithTTL` (`warmup.go:241-256`).
4. Legacy monolithic list blobs are also written (dual-write for migration,
   `warmup.go:259-275`).

**Path 3: reconcileGVR (periodic reconciliation)**

After informer sync or on a debounced timer for dynamic GVRs:

1. Read the authoritative state from the informer's in-memory store (no API
   call) via `lister.List(labels.Everything())` (`watcher.go:1046-1047`).
2. Read the current L3 cluster-wide list from Redis (`watcher.go:1072-1085`).
3. Diff the two: find objects in L3 but not in informer (ghost objects from
   missed DELETEs), and objects in informer but not in L3 or with different
   resourceVersion (`watcher.go:1097-1143`).
4. Write new/updated objects via pipelined batch SETs (`SetMultiForGVR`,
   batched in groups of 500, `watcher.go:1114-1143`).
5. Delete orphaned GET keys (`watcher.go:1146-1152`).
6. Rebuild all index SETs from scratch via `rebuildListCaches`
   (`watcher.go:1162`, definition at `watcher.go:1290-1335`).

### 3.2 L1 Fill Paths

**Path 1: ResolveAndCache (foreground, on HTTP request)**

When a user's request results in an L1 cache miss:

1. The widget or RESTAction CR is fetched from K8s (or L3).
2. `l1cache.ResolveAndCache` is called (`l1cache.go:117-131`).
3. Inside the foreground singleflight group, the full resolution pipeline
   runs: schema conversion, endpoint resolution, K8s API fan-out (with L3
   intercept), JQ evaluation.
4. A `DependencyTracker` records every GVR and resource accessed during
   resolution (`tracker.go:22-91`, injected via context at `l1cache.go:183-184`).
5. The resolved JSON is marshaled, bulky annotations are stripped, and the
   result is written to Redis via `SetResolvedRaw` (`l1cache.go:222-228`).
6. Dependencies are registered via `RegisterL1Dependencies` and
   `RegisterL1ApiDeps` (`l1cache.go:224-228`).

**Path 2: WarmL1ForAllUsers (startup prewarm)**

During startup, after L3 warmup and reconciliation:

1. Users are discovered from their `-clientconfig` Secrets
   (`prewarm.go:205-213`).
2. For each user, explicitly configured RESTActions are resolved first
   (`prewarm.go:239-241`).
3. Then, all instances of all widget GVRs are resolved (`prewarm.go:244`).
4. Child widgets discovered during resolution are recursively pre-warmed
   (`prewarm.go:114-151`), up to a depth of 5.

**Path 3: L1 refresh (background, event-driven)**

When L3 changes, the L1 refresh loop re-resolves affected L1 entries:

1. The l3gen scanner (`watcher.go:259-464`) detects L3 changes via generation
   keys.
2. Affected L1 keys are found via dependency indexes.
3. `MakeL1Refresher` (`l1_refresh.go:36-354`) re-resolves each L1 key using
   per-user credentials.
4. For delete operations, incremental JSON patching (`l1_patch.go:41-202`)
   is attempted before full re-resolution to avoid the O(N) assembly cost.

### 3.3 RBAC Fill Paths

**Path 1: On-demand via UserCan**

When `rbac.UserCan` is called and the cache misses:

1. A SelfSubjectAccessReview is submitted to the K8s API server using the
   user's credentials (`rbac.go:70-88`).
2. The result (allowed/denied) is stored as a HASH field in the per-user
   RBAC key with 24-hour TTL (`rbac.go:92-94`).

**Path 2: PreWarmRBACForUser (at login)**

When a user's `-clientconfig` Secret is created or updated:

1. The UserSecretWatcher detects the change and calls `PreWarmRBACForUser`
   (`main.go:353-354`).
2. All watched GVR x namespace x verb combinations are enumerated
   (`prewarm.go:340-356`).
3. SelfSubjectAccessReview calls are made with bounded concurrency (20
   concurrent, `prewarm.go:371-392`).

**Path 3: WarmRBACForAllUsers (at startup)**

Called during Phase 4b of the startup sequence (`main.go:409`). Iterates all
discovered users and pre-warms their RBAC cache.

---

## 4. Cache Update -- How Data Changes

### 4.1 Composition Added

1. The CRD informer fires an ADD event for the new composition.
2. `handleEvent("add")` writes the composition to a GET key and adds it to
   the namespace and cluster-wide index SETs (`watcher.go:716-733`).
3. The l3gen key is updated with the new resourceVersion (`watcher.go:757-765`).
4. If the GVR is dynamic (auto-discovered), `scheduleDynamicReconcile` sets
   up a debounced timer (2 seconds, capped at 30 seconds) (`watcher.go:900-963`).
5. After the debounce, `reconcileGVR` runs: it diffs L3 against the informer
   store, rebuilds index SETs, and triggers L1 refresh via dependency indexes
   (`watcher.go:1019-1284`).
6. For static GVRs, the l3gen scanner (1-second tick, `watcher.go:244`)
   detects the l3gen change and finds affected L1 keys via
   `L1ResourceDepKey` and `L1ApiDepKey`. A stable-tick debounce ensures
   two consecutive scans see the same l3gen values before launching refresh
   (`watcher.go:306-330`).
7. The L1 refresh re-resolves the affected keys (typically 6-10: the
   compositions-list RESTAction, piechart widget, table widget per user)
   using background singleflight (`l1_refresh.go:256-267`).
8. Cascading refresh walks the dependency tree: compositions-list refresh
   triggers piechart/table refresh, which may trigger further dependents
   (`l1_refresh.go:276-325`).

### 4.2 Composition Deleted

1. The informer fires a DELETE event.
2. `handleEvent("delete")` deletes the GET key and removes the name from
   index SETs (`watcher.go:706-714`).
3. The l1Event is enqueued for DEP index cleanup (`watcher.go:807-820`),
   handled by the event consumer goroutine which deletes the per-resource
   dep key (`watcher.go:228-234`).
4. The change is recorded in `recentChanges` for incremental patching
   (`watcher.go:788-798`).
5. On l3gen scan or dynamic reconcile, the L1 refresh runs. If the
   change is a delete and `incrementalCount < fullRefreshInterval (10)`,
   the incremental path is tried first (`l1_refresh.go:184-199`):
   - `patchRESTActionL1` reads the existing L1 value, finds the deleted
     item in the `status.items` array by namespace/name, removes it,
     decrements the total count, and writes back (`l1_patch.go:41-202`).
   - If the patch succeeds, full re-resolution is skipped for that key.
   - After 10 consecutive incremental patches, a full re-resolve is forced
     as a safety net (`l1_refresh.go:186`).

### 4.3 Composition Status Updated

1. The informer fires an UPDATE event.
2. `handleEvent("update")` overwrites the GET key with the new object
   (`watcher.go:721`).
3. The l3gen key is updated (`watcher.go:757-765`).
4. L1 refresh triggers as in the ADD case. The current implementation does
   **not** distinguish metadata-only from status-only changes -- any update
   triggers a full re-resolve (see Known Limitations).

### 4.4 RBAC Changed (RoleBinding Created/Deleted)

1. The RBAC watcher's informer detects the Update or Delete event
   (`rbac_watcher.go:49-60`).
2. `scheduleInvalidateFromBinding` is called, which debounces invalidation
   over a 2-second window (`rbac_watcher.go:87-100`).
3. After the debounce, the affected users' RBAC HASH keys are deleted
   from Redis. The next `UserCan` call re-checks via SelfSubjectAccessReview.
4. Note: ADD events during the initial LIST are intentionally skipped to
   avoid an invalidation storm at startup (`rbac_watcher.go:44-46`).

### 4.5 New CRD Appears

1. The CRD informer (watching `apiextensions.k8s.io/v1/customresourcedefinitions`)
   fires an ADD or UPDATE event.
2. `autoRegisterCRDInformer` extracts the GVR from the CRD spec and checks
   if its group matches an `autoDiscoverGroups` pattern (e.g., `*.krateo.io`)
   (`watcher.go:826-878`).
3. If matched, `startInformer` registers a new informer for the GVR, starts
   the factory, waits for the informer to sync, and runs `reconcileGVR` to
   populate L3 (`watcher.go:623-661`).
4. The GVR is also registered in the Redis `snowplow:watched-gvrs` SET so
   it persists across pod restarts (`watcher.go:873`).
5. The GVR is tracked as "dynamic" (`watcher.go:876-878`), meaning future
   events for it use the debounced reconciliation path instead of l3gen
   scanning.

---

## 5. Cache Invalidation -- How Data Leaves

### 5.1 TTL Expiry

Each cache layer has configurable TTL:

| Layer | Default TTL | Source |
|---|---|---|
| L3 GET keys | 1 hour (per-GVR override via warmup config) | `redis.go:33` |
| L3 index SETs | Same as GET keys | Inherited from GVR TTL |
| L1 resolved keys | 1 hour | `redis.go:34` |
| RBAC HASH | 24 hours | `rbac.go:17` |
| Reverse index SETs | 2 hours | `redis.go:35` |
| Not-found sentinel | 30 seconds | `redis.go:36` |
| L1 ready sentinel | 5 minutes | `keys.go:30` |

Redis handles expiry automatically. The `StartExpiryRefresh` system subscribes
to Redis `__keyevent@0__:expired` notifications and proactively re-fetches
L3 resources so the cache never goes cold from TTL expiry alone
(`watcher.go:512-540`). This uses a bounded worker pool of 10 goroutines
(`watcher.go:49`).

### 5.2 Dependency-Driven Invalidation (L1)

L1 entries are **never** deleted on invalidation. Instead, old values continue
serving traffic while a background refresh runs (stale-while-revalidate). The
invalidation flow:

1. **Change detection**: Either the l3gen scanner (1-second tick,
   `watcher.go:244-254`) or `reconcileGVR` (debounced, `watcher.go:1198-1281`)
   detects that L3 data changed.

2. **Affected key lookup**: For each changed GVR+namespace, the system
   queries three dependency indexes:
   - `L1ResourceDepKey(gvrKey, ns, "")` -- namespaced LIST deps
   - `L1ResourceDepKey(gvrKey, "", "")` -- cluster-wide LIST deps
   - `L1ApiDepKey(gvrKey)` -- API-level deps
   - `L1ResourceDepKey(crdGVRKey, "", "")` -- CRD chain deps (for zero-state
     handling)
   (See `watcher.go:366-393` for the l3gen path and `watcher.go:1200-1219`
   for the reconcile path.)

3. **Transitive expansion**: `expandDependents` walks the L1 dependency tree
   up to 5 levels deep (`watcher.go:469-497`). For example, refreshing
   `compositions-list` (RESTAction) also needs to refresh `piechart` and
   `table` (widgets that depend on it).

4. **Bounded async refresh**: At most one refresh goroutine runs at a time
   (guarded by `l1RefreshRunning` atomic bool, `watcher.go:423`). If a
   refresh is already running, the current scan is skipped and the next
   tick retries. The pending `lastSeen` updates are committed only when
   the refresh actually launches (CAS pattern, `watcher.go:432-435`).

5. **User prioritization**: The refresh processes users in priority order:
   hot (last seen < 5 min), warm (< 60 min), cold (> 60 min)
   (`l1_refresh.go:126-158`). All active users get refreshed -- priority
   only affects ordering.

### 5.3 RBAC Invalidation

When the RBAC watcher detects a Role/RoleBinding/ClusterRole/ClusterRoleBinding
change, it debounces over 2 seconds and then deletes the affected users' RBAC
HASH keys from Redis (`rbac_watcher.go:66-82`). For binding-specific changes,
only the subjects named in the binding are invalidated. The next `UserCan`
call for those users triggers a fresh SelfSubjectAccessReview.

### 5.4 Negative Caching

When `objects.Get` returns a 404, a negative-cache sentinel
(`{"__snowplow_not_found__":true}`) is stored with a 30-second TTL
(`redis.go:443-450`). This prevents repeated K8s API calls for resources
that do not exist.

---

## 6. Cache Consistency Model

### 6.1 Stale-While-Revalidate

L1 entries are **never deleted** on invalidation. When L3 data changes, the
old L1 value continues serving HTTP requests while the background refresh
runs. Users see stale data for a brief window:

- **Best case (static GVR, single change)**: l3gen scanner detects the change
  after the stable-tick debounce (2 ticks at 1s = ~2s). Refresh completes
  in <1s for incremental patch. Total stale window: ~3s.

- **Typical case (dynamic GVR, burst)**: debounce waits 2s after the last
  event (capped at 30s). Reconcile + refresh takes 1-15s depending on scale.
  Total stale window: 3-45s.

- **Worst case (50K compositions, full re-resolve)**: 7-25s for the refresh
  to complete. Context timeout is 60s for l3gen scans, 120s for dynamic
  reconcile.

### 6.2 Eventual Consistency

At steady state, L1 matches the K8s API state within one refresh cycle. The
safety net (`fullRefreshInterval = 10` consecutive incremental patches, then
force full resolve, `l1_patch.go:17`) corrects accumulated drift from
incremental patching.

### 6.3 RBAC Consistency

The RBAC cache is informer-backed. The RBAC watcher watches
Roles, ClusterRoles, RoleBindings, and ClusterRoleBindings via shared
informers (`rbac_watcher.go:49-60`). Changes propagate within the informer
sync interval (~1s) plus the debounce window (2s). During this window, stale
RBAC decisions may be served, but they are overwritten on the next `UserCan`
call after invalidation.

### 6.4 Startup Consistency

After a pod restart, the informer's initial LIST/WATCH establishes the
authoritative in-memory state. The reconciliation pass (`Reconcile`,
`watcher.go:982-1017`) diffs L3 against the informer store and patches any
differences -- ghost objects from missed DELETEs during downtime are removed,
and missing or stale objects are added/updated. This ensures L3 is correct
before L1 prewarm runs.

---

## 7. Cache Agnosticism

The cache operates on `schema.GroupVersionResource` + namespace + name. It
does **not** know about:

- Widget types (PieChart, Table, Row, etc.)
- RESTAction semantics (what API calls a RESTAction makes)
- Krateo-specific CRDs (CompositionDefinitions, etc.)
- Dashboard structure or page layout

Any Kubernetes resource can be cached by adding its GVR to the warmup config
file (`/etc/snowplow/cache-warmup.yaml`). Dynamic CRD discovery via
`autoDiscoverGroups` handles new CRDs automatically -- when a
CompositionDefinition creates a new CRD whose group matches `*.krateo.io`,
the cache starts watching it without configuration changes.

The RBAC gate on L3 reads (`resolve.go:237-251`) checks `rbac.UserCan`
generically -- it does not know what the resource is, only whether the user
can list/get it in that namespace. The `UserCan` function issues a standard
SelfSubjectAccessReview, which is the same mechanism that `kubectl auth
can-i` uses.

L1 keys include the username because different users see different data (RBAC
filtering excludes resources the user cannot access). But the key does not
encode the user's permissions -- permissions are checked at read time during
resolution. If a user's RBAC changes, the old L1 entry serves stale data
until the next refresh, but the refresh will produce correct data with the
new permissions.

---

## 8. Startup Sequence

The startup sequence is defined in `startBackgroundServices`
(`main.go:313-423`). It runs in a background goroutine so the HTTP server
starts immediately (health probes respond during warmup).

### Phase 1: Start Long-Running Watchers

```
ResourceWatcher.Start(ctx)             -- registers informers, starts factory, begins l1Worker
RBACWatcher.Start(ctx)                 -- watches Roles/RoleBindings/ClusterRoles/ClusterRoleBindings
UserSecretWatcher.Start(ctx)           -- watches -clientconfig Secrets, triggers RBAC prewarm on creation
StartExpiryRefresh(ctx)                -- subscribes to Redis expired-key notifications
```

(`main.go:326-358`)

### Phase 2: Load Warmup Config and Register GVRs

```
LoadWarmupConfig(path)                 -- reads /etc/snowplow/cache-warmup.yaml
DiscoverCompositionGVRs(ctx)           -- discovers CRDs matching autoDiscoverGroups patterns
PreRegisterGVRs(ctx)                   -- calls SAddGVR for each configured GVR
```

(`main.go:365-378`)

### Phase 3: Wait for Informer Sync

```
WaitForSync(ctx)                       -- blocks until all informers complete initial LIST/WATCH
```

Timeout: 60 seconds. If informers do not sync, warmup proceeds anyway
(`main.go:381-388`).

### Phase 4: L3 Warmup and Reconciliation

```
warmer.Run(ctx)                        -- LIST all configured GVRs, populate L3 GET keys + index SETs
resourceWatcher.Reconcile(ctx)         -- diff L3 vs informer stores, patch discrepancies
WarmRBACForAllUsers(ctx)               -- pre-populate RBAC cache for all discovered users
```

(`main.go:392-409`)

### Phase 5: L1 Prewarm

```
WarmL1ForAllUsers(ctx)                 -- resolve all widgets + configured RESTActions for all users
```

Runs in a background goroutine with a 5-minute timeout. RESTActions are
warmed first (compositions-list, etc.), then widgets, then child widgets
recursively (`main.go:411-422`, `prewarm.go:198-257`).

---

## 9. Concurrency Controls

### 9.1 Singleflight

Two `singleflight.Group` instances dedup concurrent resolutions
(`l1cache.go:52-55`):

- **foregroundFlight**: Shared across HTTP RESTAction dispatchers AND widget
  apiref resolvers. If a widget request and a direct `/call` request both
  need the same RESTAction, only one resolution runs.
- **backgroundFlight**: Used by the L1 refresh loop. Separate from foreground
  so long (10-30s) background re-resolves do not block foreground HTTP callers.

Widget HTTP handlers also use a separate `widgetFlight` singleflight group
(`widgets.go:146`).

### 9.2 Bounded Goroutines

Every goroutine spawn in the cache system is bounded:

| Component | Bound | Mechanism | Source |
|---|---|---|---|
| L3 warmup | 4 concurrent GVRs | Semaphore | `warmup.go:21, 188` |
| L1 refresh (foreground) | 20 concurrent keys | Semaphore | `l1_refresh.go:27` |
| L1 refresh (background) | 8 concurrent keys | Semaphore | `l1_refresh.go:28` |
| Expiry refresh | 10 workers | Semaphore | `watcher.go:49` |
| RBAC prewarm | 20 concurrent checks | Semaphore | `prewarm.go:282` |
| L1 prewarm child widgets | 10 concurrent | Semaphore | `prewarm.go:84` |
| L1 refresh lifecycle | 1 at a time | `l1RefreshRunning` atomic bool | `watcher.go:109, 423` |

### 9.3 Per-GVR Reconcile Mutex

`reconcileGVR` is serialized per GVR via `reconcileMu` (`sync.Map` of
`*sync.Mutex`, keyed by GVR string). This prevents `startInformer` and
`scheduleDynamicReconcile` from running `reconcileGVR` concurrently for the
same GVR, which would cause TOCTOU bugs on L3 list writes (`watcher.go:99-102,
1036-1043`).

### 9.4 Dynamic Reconcile Debouncing

For auto-discovered GVRs (e.g., composition CRDs), informer events are
debounced before reconciliation:

- **Quiet periods**: reconcile fires 2 seconds after the last event
  (`dynamicReconcileDebounce`, `watcher.go:55`).
- **Sustained bursts**: reconcile fires at least every 30 seconds regardless
  (`maxReconcileDelay`, `watcher.go:60`).
- Timer cleanup happens BEFORE `reconcileGVR` runs, so new events during
  reconciliation create a fresh timer (`watcher.go:940-942`).

### 9.5 l3gen Stable-Tick Debounce

The l3gen scanner uses a stable-tick debounce: it only launches a refresh
when the current l3gen values match what was observed on the previous tick.
If values changed since the previous tick, events are still arriving and
the scanner waits for quiescence (`watcher.go:306-330`). This avoids
refreshing with partial L3 data during bursts.

---

## 10. Performance Characteristics

### 10.1 Measured Results (v0.25.149, 5K compositions x 500 namespaces x 2 users)

| Metric | Cache ON | Cache OFF | Speedup |
|---|---|---|---|
| S6: 5000 compositions convergence | 7.1s | 33s | 4.8x |
| S7: delete 1 composition convergence | 7.0s | 12s | 1.7x |
| S8: delete 1 namespace convergence | 7.4s | 12s | 1.6x |
| Warm dashboard waterfall (steady state) | 70-83ms (low scale), 1.7s (5K) | N/A | N/A |

### 10.2 Resource Usage (5K compositions)

| Resource | Measurement |
|---|---|
| Redis memory | 258 MB peak (per-item keys + zstd) |
| Pod restarts | 0 at 16 GiB memory limit |
| Heap (steady state) | ~3.3 GB |
| Heap (peak) | ~3.9 GB |

### 10.3 Compression

All Redis values above 256 bytes are zstd-compressed (`redis.go:39`).
The system supports transparent read-back of legacy gzip-compressed values
during migration (`redis.go:56-87`). The encoder and decoder are
package-level singletons, goroutine-safe and zero-allocation per call
(`redis.go:43-46`).

At 5K compositions, zstd achieves approximately 4:1 compression ratio on
composition JSON. This reduces Redis memory from ~1 GB (uncompressed) to
~258 MB.

### 10.4 Redis Operations

Key Redis optimization patterns:

- **Pipelining**: `SetMultiForGVR` batches writes in a single pipeline
  round-trip. At 50K items per reconcile, sequential SETs take ~7s; pipelined
  takes ~200ms per batch (`redis.go:174-189`).
- **MGET chunking**: `GetRawMulti` splits large MGET requests into chunks of
  100 to avoid blocking Redis for too long (`redis.go:357`).
- **SCAN over KEYS**: The l3gen scanner uses `ScanKeys` (Redis SCAN) instead
  of KEYS to avoid blocking the Redis event loop (`redis.go:645-655`).
- **UNLINK over DEL**: Bulk deletes (>1 key) use UNLINK for async memory
  reclaim (`redis.go:567-572`).
- **Atomic transactions**: `SetResolvedRaw` uses MULTI/EXEC to atomically
  write the L1 value, add it to the per-user index SET, and set the TTL
  (`redis.go:487-523`). `AtomicUpdateJSON` uses WATCH/MULTI/EXEC with
  exponential backoff and jitter for optimistic locking (`redis.go:580-641`).
- **Per-item index**: L3 uses Redis SETs as indexes (SADD/SREM are O(1))
  instead of monolithic JSON blobs (which required full
  read-modify-write on every change). This eliminated the CAS contention
  storm that was the dominant write bottleneck at scale.

---

## 11. Known Limitations

### 11.1 Status-Only Updates Trigger Full Re-Resolve

When a composition's status is updated (e.g., by a controller reconciling
its state), the informer fires an UPDATE event. `reconcileGVR` detects a
resourceVersion change but does not distinguish metadata changes from status
changes. This triggers a full L1 re-resolve (~7-25s at 50K) for what may be
an invisible change from the portal's perspective.

**Impact**: At scale, controller reconciliation loops that frequently update
status fields can cause continuous L1 refresh activity.

**Mitigation**: The l3gen stable-tick debounce and the `l1RefreshRunning`
atomic lock collapse rapid status updates into fewer refreshes, but the
fundamental issue remains.

### 11.2 Warmer LIST Is Not Chunked

The informer's initial LIST is chunked (`Limit=500` in
`watcher.go:148-152`) to limit transient memory allocation. However, the
warmer's LIST call in `warmGVR` uses `metav1.ListOptions{}` without a Limit
(`warmup.go:209`). If the warmer runs before the informer syncs (unlikely
but possible in a race), it decodes the entire LIST response at once,
risking an OOM at extreme scale.

### 11.3 L1 Key Cardinality

L1 keys are per-user x per-page x per-widget/restaction. At 1,000 users
x 980 pages x 2 widgets per page, Redis could store up to ~2 million L1
entries. Each entry is a compressed JSON blob (typically 1-100 KB). TTL
handles cleanup, but peak memory during a burst of new users can be high.

In practice, only active users have warm L1 entries. The activity
classification system (hot/warm/cold) prioritizes refresh for active users,
and TTL reclaims entries for inactive users within 1 hour.

### 11.4 Singleflight Dedup Is Per-Key

The singleflight dedup operates on L1 cache keys. If two users request the
same widget, they have different L1 keys (because the username is part of
the key) and no dedup occurs. Each user's request triggers a full
independent resolution. At 1,000 concurrent users loading the same dashboard,
this means 1,000 parallel resolutions.

The L3 cache provides the main scaling benefit in this scenario: all 1,000
resolutions read from L3 (Redis) instead of hitting the K8s API server, so
the per-user cost is JQ evaluation + JSON marshaling, not network round-trips.

### 11.5 Dynamic GVR l3gen Interaction

Dynamic GVRs (auto-discovered CRDs) skip l3gen key updates in `handleEvent`
(`watcher.go:753`) because their reconciliation is handled by
`scheduleDynamicReconcile`. This means the l3gen scanner will not detect
changes to dynamic GVRs until `reconcileGVR` runs and bumps the l3gen key.
This is by design -- it avoids redundant double-refresh -- but it means
the l3gen scanner and the dynamic reconcile path are two independent
invalidation mechanisms that must not overlap.

### 11.6 Incremental Patching Covers Deletes Only

The incremental L1 patch path (`l1_patch.go`) only handles DELETE operations.
ADD and UPDATE operations always trigger full re-resolution. This is because
adding or modifying an item requires re-running JQ transformations (e.g.,
updating a piechart count or a table sort order), which is not feasible
without the full resolution pipeline.

### 11.7 CRD Chain Dep Broadness

When any resource in a watched GVR changes, the l3gen scanner and reconcileGVR
also look up `L1ResourceDepKey` for the CRD GVR (cluster-wide LIST dep).
This is intentional -- it handles zero-state agnostically (e.g., first
composition creates a dependency on the CRD LIST). However, it means
that any composition change also refreshes L1 keys that depend on the
CRD list (typically `compositions-get-ns-and-crd` per user), even if the
CRD itself did not change.

---

## Appendix A: Redis Key Reference

| Key Pattern | Type | Layer | TTL | Description |
|---|---|---|---|---|
| `snowplow:get:{gvr}:{ns}:{name}` | STRING | L3 | Per-GVR (default 1h) | Single K8s object |
| `snowplow:list-idx:{gvr}:{ns}` | SET | L3 | Per-GVR (default 1h) | Index of item names for list assembly |
| `snowplow:list:{gvr}:{ns}` | STRING | L3 (legacy) | Per-GVR (default 1h) | Monolithic list blob (migration, being removed) |
| `snowplow:l3gen:{gvr}:{ns}` | STRING | L3 | Per-GVR (default 1h) | Latest resourceVersion for change detection |
| `snowplow:resolved:{user}:{gvr}:{ns}:{name}[:pagination]` | STRING | L1 | 1h | Resolved JSON output |
| `snowplow:resolved-idx:{user}` | SET | L1 | 2h | Per-user index of L1 keys |
| `snowplow:rbac:{user}` | HASH | RBAC | 24h | Per-user RBAC decisions |
| `snowplow:l1dep:{gvr}:{ns}:{name}` | SET | Dep index | 2h | Per-resource L1 dependency |
| `snowplow:l1api:{gvr}` | SET | Dep index | 2h | API-level L1 dependency |
| `snowplow:l1gvr:{gvr}` | SET | Dep index | 2h | Broad GVR-level L1 dependency |
| `snowplow:watched-gvrs` | SET | Control | None | Set of GVRs with active informers |
| `snowplow:active-users` | SET | Control | None | Set of users with valid clientconfig secrets |
| `snowplow:l1:ready` | STRING | Control | 5m | Unix timestamp of last L1 warmup completion |
| `snowplow:last-seen:{user}` | STRING | Control | 60m | Unix timestamp of last HTTP request from user |
| `snowplow:userconfig:{user}` | STRING | Control | Per-GVR | Cached user endpoint configuration |
| `snowplow:discovery:{category}` | STRING | Discovery | 1h | Cached API discovery results |

## Appendix B: Data Flow Diagram

```
Browser
  |
  |  GET /call?resource=widgets&name=piechart&namespace=demo
  v
HTTP Handler (widgets.go)
  |
  |-- L1 lookup: snowplow:resolved:{user}:widgets.templates.krateo.io/v1/piecharts:demo:piechart
  |   |
  |   +-- HIT  --> return cached JSON (70-83ms)
  |   +-- MISS --> singleflight dedup
  |                  |
  |                  v
  |                Widget Resolve (widgets/resolve.go)
  |                  |
  |                  +-- apiRef resolve (apiref/resolve.go)
  |                  |     |
  |                  |     +-- L1 lookup: snowplow:resolved:{user}:templates.krateo.io/v1/restactions:demo:compositions-list
  |                  |     |     |
  |                  |     |     +-- HIT  --> return cached status map
  |                  |     |     +-- MISS --> singleflight dedup
  |                  |     |                    |
  |                  |     |                    v
  |                  |     |                  RESTAction Resolve (api/resolve.go)
  |                  |     |                    |
  |                  |     |                    +-- For each API call in RESTAction:
  |                  |     |                    |     |
  |                  |     |                    |     +-- L3 lookup: snowplow:get:{gvr}:{ns}:{name}
  |                  |     |                    |     |     or AssembleListFromIndex(gvr, ns)
  |                  |     |                    |     |     |
  |                  |     |                    |     |     +-- HIT + RBAC OK --> use cached data
  |                  |     |                    |     |     +-- MISS or RBAC DENY --> live K8s API call
  |                  |     |                    |     |
  |                  |     |                    |     +-- DependencyTracker records GVR + resource access
  |                  |     |                    |
  |                  |     |                    +-- Marshal + strip annotations
  |                  |     |                    +-- Write L1 key + register dependencies
  |                  |     |
  |                  |     +-- Return status map to widget
  |                  |
  |                  +-- JQ evaluation (widgetDataTemplate)
  |                  +-- resourcesRefs resolution
  |                  +-- Marshal + strip annotations
  |                  +-- Write L1 key + register dependencies
  |
  +-- Return resolved JSON to browser
```

```
K8s API Server
  |
  |  WATCH event (add/update/delete)
  v
Informer (watcher.go handleEvent)
  |
  +-- L3 update: SET/DEL per-item GET key + SADD/SREM index SETs
  +-- l3gen bump (resourceVersion) for static GVRs
  +-- scheduleDynamicReconcile for dynamic GVRs
  +-- Record change in recentChanges buffer
  |
  v
l3gen scanner (1s tick) OR dynamic reconcile (2s debounce)
  |
  +-- Detect changed GVR+ns via l3gen keys or informer store diff
  +-- Query dependency indexes: L1ResourceDepKey, L1ApiDepKey, CRD chain
  +-- expandDependents: walk transitive deps up to depth 5
  |
  v
L1 Refresh (l1_refresh.go, max 1 concurrent)
  |
  +-- Classify users: hot > warm > cold
  +-- For each user's affected L1 keys:
  |     |
  |     +-- Try incremental patch (delete-only, l1_patch.go)
  |     |     +-- Success --> skip full resolve, cascade to dependents
  |     |     +-- Failure --> fall through to full resolve
  |     |
  |     +-- Full re-resolve via ResolveRESTActionBackground / ResolveWidgetBackground
  |     +-- Cascade: find and refresh transitive dependents
  |
  +-- Mark L1 ready (snowplow:l1:ready timestamp)
```
