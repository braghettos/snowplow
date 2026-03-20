# Snowplow Cache Architecture

## Overview

Snowplow uses a **two-layer Redis cache** (L3 + L1) backed by Kubernetes informers for event-driven consistency. The cache eliminates repeated Kubernetes API calls and expensive resolution pipelines (HTTP fan-out, JQ evaluations) by storing data at two levels of the request lifecycle.

All Redis cache state lives in a single Redis instance (sidecar at `localhost:6379`, configurable via `REDIS_ADDRESS`). Caching can be entirely disabled by setting `CACHE_ENABLED=false` or `CACHE_ENABLED=0`.

```
┌──────────────────────────────────────────────────────────────────┐
│                        User Request                              │
│                    GET /call?resource=...                         │
└────────────────────────────┬─────────────────────────────────────┘
                             │
                    ┌────────▼────────┐
                    │   L1 (Resolved) │  per-user, fully resolved JSON (Redis)
                    │   HIT → return  │  gzip-compressed when ≥256 bytes
                    └────────┬────────┘
                             │ MISS
                    ┌────────▼────────┐
                    │   Resolution    │  HTTP fan-out + JQ evaluations
                    │   Pipeline      │  reads from L3 where possible
                    └────────┬────────┘
                             │
                    ┌────────▼────────┐
                    │   L3 (Objects)  │  shared, raw K8s objects
                    │   GET + LIST    │  populated by informers
                    └────────┬────────┘
                             │ MISS
                    ┌────────▼────────┐
                    │   Kubernetes    │
                    │   API Server    │
                    └─────────────────┘
```

---

## Cache Layers

### L3 — Raw Kubernetes Objects (shared, user-agnostic)

The foundational layer. Stores raw `Unstructured` Kubernetes objects as JSON.

| Operation | Key Format | Example |
|-----------|-----------|---------|
| GET (single object) | `snowplow:get:{group/version/resource}:{namespace}:{name}` | `snowplow:get:widgets.templates.krateo.io/v1beta1/pages:krateo-system:dashboard-page` |
| LIST (namespace-scoped) | `snowplow:list:{group/version/resource}:{namespace}` | `snowplow:list:widgets.templates.krateo.io/v1beta1/routes:krateo-system` |
| LIST (cluster-wide) | `snowplow:list:{group/version/resource}:` | `snowplow:list:core/v1/namespaces:` |
| Discovery | `snowplow:discovery:{category}` | `snowplow:discovery:krateo` |

**TTL**: Default `1h`, configurable per-GVR in `cache-warmup.yaml` (e.g. `24h` for namespaces, `6h` for RESTActions). TTL is purely for memory management — freshness is guaranteed by informer events, not TTL expiry.

**Create**: Populated during:
1. **Startup warmup** — `Warmer.Run()` fetches all GVRs listed in `cache-warmup.yaml`, storing both individual GET keys and LIST keys (cluster-wide + per-namespace).
2. **First access** — When a request touches a GVR not yet cached, the GET/LIST result is stored and an informer is dynamically registered for that GVR.

**Update**: Informer events (`add`, `update`) trigger immediate in-place updates:
- The **GET key** is overwritten with the new object via `SetForGVR`.
- The **LIST keys** (namespace + cluster-wide) are patched atomically using `WATCH/MULTI/EXEC` (optimistic locking). The matching item is found by namespace+name and replaced in-place; new items are appended.

**Delete**: Informer `delete` events remove the GET key and patch the LIST keys to remove the item.

**Expiry refresh**: When a GET or LIST key expires via TTL, the `ResourceWatcher.StartExpiryRefresh` subscriber re-fetches the resource from the Kubernetes API and re-populates the cache, preventing cold misses after TTL expiry.

### L1 — Resolved Output (per-user)

The highest-value layer. Stores the **fully resolved JSON** output of widget and RESTAction dispatchers — the final response sent to the frontend. An L1 hit eliminates the entire resolution pipeline: no K8s API calls, no HTTP fan-out, no JQ evaluations.

| Operation | Key Format | Example |
|-----------|-----------|---------|
| Unpaginated | `snowplow:resolved:{username}:{group/version/resource}:{namespace}:{name}` | `snowplow:resolved:admin:widgets.templates.krateo.io/v1beta1/pages:krateo-system:dashboard-page` |
| Paginated | `snowplow:resolved:{username}:{group/version/resource}:{namespace}:{name}:p{page}-pp{perPage}` | `snowplow:resolved:admin:templates.krateo.io/v1/restactions:krateo-system:comp-list:p1-pp10` |

**TTL**: `1h`. Freshness is maintained by proactive background refresh, not TTL.

**Compression**: Values ≥256 bytes are gzip-compressed before storing in Redis; decompressed on read.

**Create**: Populated when:
1. **User request** — On L1 miss, the full resolution pipeline runs (reading from L3 where possible). The result is stored with `SetResolvedRaw` and the GVR dependencies are recorded in reverse indexes.
2. **Startup L1 warmup** — `WarmL1ForAllUsers` discovers all users from `*-clientconfig` secrets, then resolves:
   - Every **widget** instance (from `cache-warmup.yaml` GVRs with group `widgets.templates.krateo.io`) for each user.
   - Every **RESTAction** listed in `l1RestActions` (e.g. `all-routes`, `blueprints-list`, `compositions-list`, `sidebar-nav-menu-items`) for each user.
3. **Request-time child pre-warming** — When a parent widget (e.g. `RoutesLoader`) is resolved, its child widget references (`status.resourcesRefs.items`) are resolved concurrently in a background goroutine.
4. **Proactive L1 refresh with child pre-warming** — When a resource changes, existing L1 keys are re-resolved in background instead of being deleted (stale-while-revalidate). After re-resolving a widget, `preWarmChildWidgets` is called on the refreshed result, so newly-discovered child widgets (e.g. composition-panels from compositions deployed after initial warmup) are recursively pre-warmed into L1 up to depth 5. This ensures the full widget tree is cached, not just the top-level widgets that existed at startup time.

**Update (Stale-While-Revalidate)**: When a watched resource fires an `add` or `update` informer event:
1. L3 is updated in-place (see above).
2. L1 keys dependent on the changed GVR are **NOT deleted**. Instead, they are re-resolved in a background goroutine via `L1RefreshFunc`. The old value continues to be served to users until the refresh completes and atomically overwrites the key.
3. A per-GVR in-flight guard (`sync.Map`) prevents thundering herd — if a refresh for a GVR is already running, subsequent events for the same GVR are skipped (the next event will trigger a new refresh).
4. After all L1 keys are refreshed, `MarkL1Ready` writes the current Unix epoch to the `snowplow:l1:ready` sentinel key, providing a deterministic signal that the refresh cycle is complete.

**Delete**: On `delete` events (resource removed from cluster), L1 keys are first **invalidated** (hard-deleted via the reverse index) to stop serving stale data, then a **background L1 refresh** is triggered for those same keys. This re-resolves parent widgets (e.g. compositions-list datagrid, piechart) to reflect the removed resource. The same per-GVR in-flight guard and `MarkL1Ready` sentinel apply, providing a deterministic signal that the post-deletion refresh is complete.

---

## Supplementary Cache Keys

### RBAC Cache

| Key Format | Example |
|-----------|---------|
| `snowplow:rbac:{username}:{verb}:{group/resource}:{namespace}` | `snowplow:rbac:admin:get:widgets.templates.krateo.io/pages:krateo-system` |

Caches `SelfSubjectAccessReview` results per user. Entries have a **24-hour safety-net TTL** — freshness is maintained by the `RBACWatcher`, but the TTL bounds worst-case staleness if watcher events are missed. Binding changes trigger targeted invalidation for the specific subjects; role changes invalidate all active users.

### User Config Cache

| Key Format | Example |
|-----------|---------|
| `snowplow:userconfig:{username}` | `snowplow:userconfig:admin` |

Caches the `Endpoint` loaded from the user's `{username}-clientconfig` Secret. Invalidated by the `UserSecretWatcher` when the Secret is updated or deleted.

### Negative Cache (404 sentinel)

When a GET request returns `404 Not Found`, a sentinel value (`{"__snowplow_not_found__":true}`) is stored with a short TTL (30 seconds). This prevents repeated API calls for resources that don't exist.

---

## Reverse Indexes

Reverse indexes are Redis SETs that map a GVR to the L1 cache keys that **depend on it**. They enable targeted invalidation: when a resource changes, only the cache entries that actually used that resource are affected.

| Index Key | Members |
|-----------|---------|
| `snowplow:l1gvr:{group/version/resource}` | Set of `snowplow:resolved:...` keys |

**Population**: During resolution, a `DependencyTracker` records every GVR accessed. After resolution completes, the dispatcher calls `SAddWithTTL` to register the L1 keys in the appropriate reverse index SETs.

**TTL**: Reverse index SETs use `ReverseIndexTTL` (2h), which is 2x the cache entry TTL (`DefaultResourceTTL` = 1h). This prevents silent expiry of reverse indexes for infrequently accessed GVRs. The TTL is refreshed every time a member is added.

**Example flow**:
1. Widget `dashboard-page` for user `admin` is resolved. During resolution, the pipeline reads compositions (`apiextensions.crossplane.io/v1/compositions`) and namespaces (`core/v1/namespaces`).
2. The `DependencyTracker` records both GVRs.
3. After resolution, the L1 key `snowplow:resolved:admin:widgets.../pages:krateo-system:dashboard-page` is added to:
   - `snowplow:l1gvr:apiextensions.crossplane.io/v1/compositions`
   - `snowplow:l1gvr:core/v1/namespaces`
4. When a composition changes, the watcher reads `snowplow:l1gvr:apiextensions.crossplane.io/v1/compositions`, finds the dashboard-page L1 key, and triggers a background refresh.

---

## Event-Driven Consistency

### ResourceWatcher

The `ResourceWatcher` maintains dynamic Kubernetes informers for every GVR in the watched set. It reacts to three event types:

| Event | L3 Action | L1 Action |
|-------|-----------|-----------|
| `add` | Upsert GET key, patch LIST keys | Background refresh dependent L1 keys |
| `update` | Upsert GET key, patch LIST keys | Background refresh dependent L1 keys |
| `delete` | Delete GET key, patch LIST keys | **Delete** dependent L1 keys |

The key design choice: on `add`/`update`, L1 keys are **refreshed** (re-resolved in background) rather than deleted. This implements the **stale-while-revalidate** pattern — users always get a fast L1 hit, even during periods of frequent resource changes (e.g. composition controllers updating `lastTransitionTime` every few seconds).

**GVR registration**: Informers are registered in two ways:
1. **Startup** — All GVRs from `cache-warmup.yaml` are pre-registered via `PreRegisterGVRs`.
2. **Dynamic** — When a request accesses a new GVR, `SAddGVR` triggers `onNewGVR`, which calls `AddGVR` to register a new informer at runtime.
3. **Periodic sync** — Every 60 seconds, `syncNewGVRs` reads the `snowplow:watched-gvrs` Redis SET and registers any new GVRs that were added by other code paths.

### L1 Refresh Callback

The `L1RefreshFunc` callback is injected from `main.go` and implemented in `dispatchers/l1_refresh.go`. When the watcher detects an add/update event with dependent L1 keys:

1. The watcher spawns a goroutine with a 60-second timeout.
2. The callback parses each L1 key to extract `{username, GVR, namespace, name, page, perPage}`.
3. Keys are grouped by username. For each user, the endpoint config is loaded from their `-clientconfig` Secret. User groups are extracted from the client certificate data to ensure correct RBAC impersonation.
4. Each L1 key is re-resolved concurrently (semaphore-bounded at 20) via the shared singleflight groups (see below):
   - **Widgets** (`widgets.templates.krateo.io` group): `objects.Get` → `ResolveWidgetDirect` (singleflight) → `SetResolvedRaw` → `preWarmChildWidgets` (recursive, depth 5)
   - **RESTActions** (`templates.krateo.io` group, `restactions` resource): `objects.Get` → `ResolveRESTActionDirect` (singleflight) → `SetResolvedRaw`
5. Reverse indexes are updated with the new GVR dependencies from the `DependencyTracker`.
6. After all keys are refreshed, `MarkL1Ready` writes the current epoch to `snowplow:l1:ready`.

### Singleflight Deduplication

Widget and RESTAction resolutions are wrapped in `singleflight.Group` instances (`widgetFlight` and `restactionFlight` in `dispatchers/singleflight.go`). This ensures that concurrent resolutions of the same L1 key — whether from an HTTP request or a background L1 refresh goroutine — are deduplicated: only one resolution runs, and all callers receive the same result.

This is critical at high cardinality (e.g. 1200 compositions). Without singleflight, a browser request and 20 concurrent L1 refresh goroutines could all try to resolve the same widget simultaneously, doing duplicate JQ + RBAC work and competing for CPU. With singleflight:
- If L1 refresh is already resolving a widget, the HTTP handler **joins the in-flight resolution** instead of starting a second one.
- If the HTTP handler starts first, the L1 refresh goroutine joins its resolution.
- `context.WithoutCancel` is used in the HTTP handler path so that if a client disconnects, the resolution still completes for other waiters.

### Child Pre-Warming During L1 Refresh

When an informer event triggers L1 refresh for a parent widget (e.g. `compositions-page-datagrid`), the resolution now also calls `preWarmChildWidgets` on the refreshed result. This recursively discovers and pre-warms all child widget references (e.g. `composition-panel`, `composition-panel-markdown`, `composition-panel-button-delete`) up to depth 5.

Previously, `preWarmChildWidgets` was only called during initial startup warmup and request-time resolution. This meant child widgets from resources deployed after startup (e.g. compositions created after cache was enabled) were never pre-warmed — every browser request for those children was an L1 miss requiring full resolution (18-19s per widget at 1200 compositions). With child pre-warming during L1 refresh, the informer event that updates the datagrid also ensures all visible composition-panel widgets and their descendants are cached.

### L3 → Resolution Pipeline (direct read)

During resolution, when the pipeline needs to make an HTTP GET to the Kubernetes API, it first checks if L3 already has the data (via `ParseK8sAPIPath`). If found and the user passes RBAC checks (`rbac.UserCan`), the L3 data is served directly — no live API call needed. For `/call` paths targeting other snowplow endpoints, L1 is checked for existing resolved output.

### RBACWatcher

Watches Roles, ClusterRoles, RoleBindings, and ClusterRoleBindings. On any change:
- **Binding changes** with explicit User subjects → invalidate only those users' RBAC keys.
- **Role changes** or bindings with Group/ServiceAccount subjects → invalidate all active users' RBAC keys.

### UserSecretWatcher

Watches `*-clientconfig` Secrets in the `AUTHN_NAMESPACE`:
- **Add**: Register username in `snowplow:active-users` SET.
- **Update**: Invalidate the `snowplow:userconfig:{username}` key (forces re-load on next request).
- **Delete**: Remove from active users, purge userconfig key and all RBAC keys for that user.

---

## Crash Recovery

All background goroutines (informer event handlers, L1 refresh, expiry refresh) are wrapped with `recover()` to prevent panics from crashing the process. The HTTP handler chain includes a recovery middleware that catches panics during request processing, logs the full stack trace, and returns a 500 error.

---

## Startup Sequence

The cache startup follows a five-phase sequence:

```
Phase 1: Start Watchers
   ├── ResourceWatcher (informers for all GVRs, bound to process lifetime)
   ├── RBACWatcher (roles + bindings, all namespaces)
   ├── UserSecretWatcher (*-clientconfig secrets in AUTHN_NAMESPACE)
   └── Expiry refresh subscriber (Redis keyspace notifications)

Phase 2: Pre-register GVRs
   └── SAddGVR for each entry in cache-warmup.yaml
       → triggers informer creation for each GVR

Phase 3: Wait for Informer Sync
   └── WaitForCacheSync (60s timeout)
       → all informers complete initial list-and-watch

Phase 4: L3 Warmup (Warmer.Run)
   └── For each GVR in cache-warmup.yaml:
       ├── LIST all resources
       ├── Store individual GET keys
       └── Store LIST keys (cluster + per-namespace)

Phase 5: L1 Warmup (background goroutine, 5min timeout)
   ├── Discover users from *-clientconfig secrets
   │   └── Extract groups from client certificate for correct RBAC
   ├── Widgets: filter GVRs with group = widgets.templates.krateo.io
   ├── RESTActions: resolve each entry in l1RestActions
   └── For each user × (each widget instance + each l1RestAction):
       ├── Skip if L1 key already exists
       ├── Resolve widget (reads from L3 cache)
       ├── Store resolved output in L1
       └── Register GVR dependencies in reverse indexes
```

---

## Key Design Choices

### Why two layers instead of one?

Each layer serves a different purpose with different cache hit characteristics:
- **L3** is shared across all users and updated atomically by informers. Even when L1 misses, L3 prevents hitting the Kubernetes API during resolution.
- **L1** stores the final assembled output. A single L1 hit replaces dozens of L3 reads, JQ evaluations, and HTTP calls.

### Why stale-while-revalidate instead of delete-on-change?

Composition controllers frequently update `.status.lastTransitionTime` every few seconds, even when no meaningful state changes. Under a delete-on-change strategy, this causes constant L1 cache misses — every user request triggers a full re-resolution (~800ms). With stale-while-revalidate:
- Users always get a fast response (L1 hit, <5ms).
- The background refresh updates the cached output with fresh data.
- The per-GVR in-flight guard prevents overlapping refreshes from piling up.

### Why reverse indexes instead of pattern-based invalidation?

Redis `SCAN` with wildcards is O(N) where N is the total number of keys. Reverse indexes (Redis SETs) provide O(1) membership reads and targeted O(K) deletes where K is only the number of actually-dependent keys. This is critical at scale when thousands of L1 keys exist but only a handful depend on the changed GVR.

### Why atomic list patching?

LIST cache keys contain arrays of objects. On add/update/delete, the watcher patches the list in-place using `WATCH/MULTI/EXEC` (optimistic locking with up to 3 retries). This avoids re-fetching the entire list from the API on every single-resource change.

### Why per-GVR TTL overrides?

Resources like namespaces and CRDs change rarely and are expensive to re-fetch. A 24h TTL reduces API load. RESTActions change more often and get a 6h TTL. Widget types use the default 1h TTL. All TTL values are safety nets — freshness is guaranteed by informer events.

---

## Redis Key Reference

| Prefix | Layer | Scope | Description |
|--------|-------|-------|-------------|
| `snowplow:get:` | L3 | Shared | Single Kubernetes object |
| `snowplow:list:` | L3 | Shared | List of Kubernetes objects |
| `snowplow:discovery:` | L3 | Shared | API discovery result |
| `snowplow:resolved:` | L1 | Per-user | Fully resolved widget/RESTAction output |
| `snowplow:rbac:` | — | Per-user | SelfSubjectAccessReview result |
| `snowplow:userconfig:` | — | Per-user | User endpoint config from Secret |
| `snowplow:l1gvr:` | Index | Shared | Reverse index: GVR → dependent L1 keys |
| `snowplow:l1:ready` | Meta | Shared | Unix epoch of last L1 refresh completion (sentinel) |
| `snowplow:watched-gvrs` | Meta | Shared | SET of all GVRs with active informers |
| `snowplow:active-users` | Meta | Shared | SET of usernames with active sessions |

---

## Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `CACHE_ENABLED` | `true` | Set to `false` or `0` to disable caching entirely |
| `REDIS_ADDRESS` | `localhost:6379` | Redis server address |
| `RESOURCE_TTL` | `1h` | Default TTL for cached Kubernetes resources |
| `WARMUP_CONFIG` | `/etc/snowplow/cache-warmup.yaml` | Path to warmup configuration file |
| `AUTHN_NAMESPACE` | (required) | Namespace containing `*-clientconfig` Secrets |

---

## Metrics

Available at `GET /metrics/cache`:

| Metric | Description |
|--------|-------------|
| `get_hits` / `get_misses` | L3 GET cache performance |
| `list_hits` / `list_misses` | L3 LIST cache performance |
| `rbac_hits` / `rbac_misses` | RBAC cache performance |
| `l1_hits` / `l1_misses` | L1 resolved output cache performance |
| `raw_hits` / `raw_misses` | Resolution pipeline cache performance |
| `call_hits` / `call_misses` | `/call` handler L3-direct cache performance |
| `l3_promotions` | Times L3 data was served directly during resolution |
| `negative_hits` | Times a 404 sentinel was served |
| `expiry_refreshes` | Times a key was proactively re-fetched after TTL expiry |

---

## Performance Optimizations

### Singleflight Resolution Deduplication

Concurrent resolutions of the same L1 key are deduplicated via `golang.org/x/sync/singleflight`. Shared `widgetFlight` and `restactionFlight` groups span both the HTTP handler and L1 refresh paths. Under high cardinality (1200+ compositions), this prevents duplicate CPU work (JQ evaluations, RBAC checks, HTTP fan-out) when multiple goroutines try to resolve the same widget simultaneously.

### Recursive Child Pre-Warming

`preWarmChildWidgets` walks `status.resourcesRefs.items` from any resolved widget and pre-warms every child whose GVR group is `widgets.templates.krateo.io`, recursively up to depth 5. This runs:
- During startup warmup (initial population)
- During request-time resolution (on L1 miss)
- During informer-triggered L1 refresh (ensures newly-deployed resources' child widgets are cached)

The function is agnostic to widget types — it follows whatever child refs the resolution produced.

### Parallel Widget `resourcesRefs` Resolution

Widgets with many child references (e.g. `RoutesLoader` with 42 routes) resolve all `ResourceRef` items concurrently using a goroutine pool with a semaphore (concurrency = 20). Each reference independently checks RBAC for up to 4 verbs. Previously sequential, this reduces wall-clock time from ~168 sequential operations to ~9 batches of concurrent operations for a typical routes page.

### Dynamic CRD Informer Registration

When RESTAction resolution makes HTTP calls to K8s API paths (e.g. listing composition instances), the parsed GVR is registered for informer watching via `SAddGVR`. This ensures L3 cache is populated for dynamic CRDs, enabling direct L3 reads on subsequent requests. Without this, every RESTAction call for dynamic CRDs would hit the live API.

---

## Known Limitations

1. **RESTAction API items are sequential by design.** Items are processed in topological order and share a mutable JQ `dict`. Even without explicit `DependsOn`, items may reference data produced by earlier items via JQ expressions, making parallelization unsafe.

2. **L1 startup warmup includes widgets and l1RestActions; apiRef widgets use transitive GVR registration.** RESTActions listed in `l1RestActions` in `cache-warmup.yaml` (e.g. `all-routes`, `blueprints-list`, `compositions-list`, `sidebar-nav-menu-items`) are warmed at startup. Other RESTActions populate L1 on first real user request.

3. **L1 refresh uses the user's client certificate groups.** Groups are extracted from the `*-clientconfig` Secret's client certificate data. If the certificate is missing or malformed, the refresh runs with an empty groups list, potentially producing incorrect RBAC results.

---

## Source Files

| File | Responsibility |
|------|---------------|
| `internal/cache/redis.go` | RedisCache core: connection, CRUD, TTL, atomic ops |
| `internal/cache/keys.go` | Key format builders and parsers |
| `internal/cache/watcher.go` | ResourceWatcher: informers, L3 updates, L1 refresh/invalidation |
| `internal/cache/warmup.go` | Startup L3 warmup and discovery caching |
| `internal/cache/tracker.go` | DependencyTracker: records GVR dependencies during resolution |
| `internal/cache/context.go` | Context helpers for cache injection |
| `internal/cache/rbac_watcher.go` | RBACWatcher: role/binding informers |
| `internal/cache/user_watcher.go` | UserSecretWatcher: clientconfig Secret informers |
| `internal/cache/metrics.go` | Global cache hit/miss counters |
| `internal/handlers/dispatchers/widgets.go` | Widget dispatcher with L1 cache read/write and singleflight dedup |
| `internal/handlers/dispatchers/restactions.go` | RESTAction dispatcher with L1 cache read/write and singleflight dedup |
| `internal/handlers/dispatchers/singleflight.go` | Shared singleflight groups for widget and RESTAction resolution |
| `internal/handlers/dispatchers/prewarm.go` | L1 startup warmup, request-time and refresh-time child pre-warming |
| `internal/handlers/dispatchers/l1_refresh.go` | L1RefreshFunc: background re-resolution with singleflight and child pre-warming |
| `config/cache-warmup.yaml` | GVRs, `l1RestActions`, and categories to pre-populate at startup |
