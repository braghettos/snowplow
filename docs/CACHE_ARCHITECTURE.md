# Snowplow Cache Architecture

## Overview

Snowplow uses a **three-layer Redis cache** backed by Kubernetes informers for event-driven consistency. The cache eliminates repeated Kubernetes API calls and expensive resolution pipelines (HTTP fan-out, JQ evaluations) by storing data at multiple levels of the request lifecycle.

All cache state lives in a single Redis instance (sidecar at `localhost:6379`, configurable via `REDIS_ADDRESS`). Caching can be entirely disabled by setting `CACHE_ENABLED=false`.

```
┌──────────────────────────────────────────────────────────────────┐
│                        User Request                              │
│                    GET /call?resource=...                         │
└────────────────────────────┬─────────────────────────────────────┘
                             │
                    ┌────────▼────────┐
                    │   L1 (Resolved) │  per-user, fully resolved JSON
                    │   HIT → return  │
                    └────────┬────────┘
                             │ MISS
                    ┌────────▼────────┐
                    │   L2 (HTTP)     │  per-user, raw HTTP responses
                    │   used during   │  from K8s API calls in pipeline
                    │   resolution    │
                    └────────┬────────┘
                             │ MISS
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

### L2 — HTTP Responses (per-user)

Caches raw HTTP responses produced during widget/RESTAction resolution. These are the intermediate K8s API call results (e.g. fetching compositions, namespaces, secrets) that the resolution pipeline makes on behalf of a specific user.

| Operation | Key Format | Example |
|-----------|-----------|---------|
| Per-user HTTP | `snowplow:http:{username}:{METHOD}:{sha256_hash}` | `snowplow:http:admin:GET:a1b2c3d4e5f6a7b8` |

The path (including query parameters) is SHA-256 hashed to keep key length bounded. Keys are per-user because responses may differ per user due to RBAC filtering.

**TTL**: `1h`. Freshness is maintained by targeted invalidation, not TTL.

**Create**: Populated during resolution when the pipeline makes HTTP calls to the Kubernetes API. L2 keys are also registered in reverse indexes (`snowplow:l2gvr:{gvrKey}`) inline during resolution.

**L3 → L2 Promotion**: When an L2 miss occurs for a K8s API path, the resolver checks whether L3 already has the raw object (via `ParseK8sAPIPath`). If found, the L3 data is copied into the L2 key and the reverse index is updated — no live API call is needed. This is particularly effective after the first request registers informers for dynamic CRDs via `SAddGVR`.

**Update**: L2 keys are **not updated in-place**. When a watched resource changes, all L2 keys that depend on the changed GVR are **deleted** (via the reverse index). They are naturally rebuilt during the next L1 refresh or user request.

**Delete**: Targeted deletion via the `snowplow:l2gvr:{gvrKey}` reverse index (see [Reverse Indexes](#reverse-indexes)).

### L1 — Resolved Output (per-user)

The highest-value layer. Stores the **fully resolved JSON** output of widget and RESTAction dispatchers — the final response sent to the frontend. An L1 hit eliminates the entire resolution pipeline: no K8s API calls, no HTTP fan-out, no JQ evaluations.

| Operation | Key Format | Example |
|-----------|-----------|---------|
| Unpaginated | `snowplow:resolved:{username}:{group/version/resource}:{namespace}:{name}` | `snowplow:resolved:admin:widgets.templates.krateo.io/v1beta1/pages:krateo-system:dashboard-page` |
| Paginated | `snowplow:resolved:{username}:{group/version/resource}:{namespace}:{name}:p{page}-pp{perPage}` | `snowplow:resolved:admin:templates.krateo.io/v1/restactions:krateo-system:comp-list:p1-pp10` |

**TTL**: `1h`. Freshness is maintained by proactive background refresh, not TTL.

**Create**: Populated when:
1. **User request** — On L1 miss, the full resolution pipeline runs. The result is stored with `SetResolvedRaw` and the GVR dependencies are recorded in reverse indexes.
2. **Startup L1 warmup** — `WarmL1ForAllUsers` discovers all users from `*-clientconfig` secrets, then resolves every **widget** instance (from `cache-warmup.yaml` GVRs with group `widgets.templates.krateo.io`) for each user. RESTActions are **excluded** from startup L1 warmup because they trigger deeply nested HTTP fan-out (e.g. `compositions-list` generates 200+ K8s API calls), causing excessive startup time. RESTActions populate L1 on first real user request, which is fast due to warm L3/L2 caches.
3. **Request-time child pre-warming** — When a parent widget (e.g. `RoutesLoader`) is resolved, its child widget references (`status.resourcesRefs.items`) are resolved concurrently in a background goroutine.
4. **Proactive L1 refresh** — When a resource changes, existing L1 keys are re-resolved in background instead of being deleted (stale-while-revalidate).

**Update (Stale-While-Revalidate)**: When a watched resource fires an `add` or `update` informer event:
1. L3 is updated in-place (see above).
2. L2 keys dependent on the changed GVR are deleted.
3. L1 keys dependent on the changed GVR are **NOT deleted**. Instead, they are re-resolved in a background goroutine via `L1RefreshFunc`. The old value continues to be served to users until the refresh completes and atomically overwrites the key.
4. A per-GVR in-flight guard (`sync.Map`) prevents thundering herd — if a refresh for a GVR is already running, subsequent events for the same GVR are skipped (the next event will trigger a new refresh).

**Delete**: On `delete` events (resource removed from cluster), L1 keys are fully invalidated via the reverse index.

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

Reverse indexes are Redis SETs that map a GVR to the L1/L2 cache keys that **depend on it**. They enable targeted invalidation: when a resource changes, only the cache entries that actually used that resource are affected.

| Index Key | Members |
|-----------|---------|
| `snowplow:l1gvr:{group/version/resource}` | Set of `snowplow:resolved:...` keys |
| `snowplow:l2gvr:{group/version/resource}` | Set of `snowplow:http:...` keys |

**Population**: During resolution, a `DependencyTracker` records every GVR accessed. L2 reverse indexes are populated inline during resolution (in `api/resolve.go`). After resolution completes, the dispatcher calls `SAddWithTTL` to register the L1 keys in the appropriate reverse index SETs.

**TTL**: Reverse index SETs share the same TTL as the cache entries they reference (`DefaultResourceTTL`). The TTL is refreshed every time a member is added.

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

| Event | L3 Action | L2 Action | L1 Action |
|-------|-----------|-----------|-----------|
| `add` | Upsert GET key, patch LIST keys | Delete dependent L2 keys | Background refresh dependent L1 keys |
| `update` | Upsert GET key, patch LIST keys | Delete dependent L2 keys | Background refresh dependent L1 keys |
| `delete` | Delete GET key, patch LIST keys | Delete dependent L2 keys | **Delete** dependent L1 keys |

The key design choice: on `add`/`update`, L1 keys are **refreshed** (re-resolved in background) rather than deleted. This implements the **stale-while-revalidate** pattern — users always get a fast L1 hit, even during periods of frequent resource changes (e.g. composition controllers updating `lastTransitionTime` every few seconds).

**GVR registration**: Informers are registered in two ways:
1. **Startup** — All GVRs from `cache-warmup.yaml` are pre-registered via `PreRegisterGVRs`.
2. **Dynamic** — When a request accesses a new GVR, `SAddGVR` triggers `onNewGVR`, which calls `AddGVR` to register a new informer at runtime. This includes dynamic CRDs accessed during RESTAction resolution (e.g. composition-instance resources), which call `SAddGVR` from `api/resolve.go` when parsing K8s API paths.
3. **Periodic sync** — Every 60 seconds, `syncNewGVRs` reads the `snowplow:watched-gvrs` Redis SET and registers any new GVRs that were added by other code paths.

### L1 Refresh Callback

The `L1RefreshFunc` callback is injected from `main.go` and implemented in `dispatchers/l1_refresh.go`. When the watcher detects an add/update event with dependent L1 keys:

1. The watcher spawns a goroutine with a 60-second timeout.
2. The callback parses each L1 key to extract `{username, GVR, namespace, name, page, perPage}`.
3. Keys are grouped by username. For each user, the endpoint config is loaded from their `-clientconfig` Secret. User groups are extracted from the client certificate data to ensure correct RBAC impersonation.
4. Each L1 key is re-resolved concurrently (semaphore-bounded at 10):
   - **Widgets** (`widgets.templates.krateo.io` group): `objects.Get` → `widgets.Resolve` → `SetResolvedRaw`
   - **RESTActions** (`templates.krateo.io` group, `restactions` resource): `objects.Get` → convert to typed `RESTAction` → `restactions.Resolve` → `SetResolvedRaw`
5. Reverse indexes are updated with the new GVR dependencies from the `DependencyTracker`.

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
   ├── Filter widget GVRs only (group = widgets.templates.krateo.io)
   │   └── RESTActions are excluded (deep HTTP fan-out at startup)
   └── For each user × each widget instance:
       ├── Skip if L1 key already exists
       ├── Resolve widget (reads from L3 cache)
       ├── Store resolved output in L1
       └── If widget has spec.apiRef → registerApiRefGVRDeps:
           ├── Fetch referenced RESTAction CR from L3
           ├── Extract K8s API groups from API item paths (static + JQ templates)
           ├── Use discovery API to find all GVRs in those groups
           ├── SAddGVR for each → start informers
           └── Register widget L1 key in GVR reverse indexes
```

---

## Key Design Choices

### Why three layers instead of one?

Each layer serves a different purpose with different cache hit characteristics:
- **L3** is shared across all users and updated atomically by informers. Even when L1/L2 miss, L3 prevents hitting the Kubernetes API.
- **L2** captures per-user HTTP responses from the resolution pipeline. Since responses may differ per user (RBAC-filtered), L2 must be per-user.
- **L1** stores the final assembled output. A single L1 hit replaces dozens of L3 reads, JQ evaluations, and HTTP calls.

### Why stale-while-revalidate instead of delete-on-change?

Composition controllers frequently update `.status.lastTransitionTime` every few seconds, even when no meaningful state changes. Under a delete-on-change strategy, this causes constant L1 cache misses — every user request triggers a full re-resolution (~800ms). With stale-while-revalidate:
- Users always get a fast response (L1 hit, <5ms).
- The background refresh updates the cached output with fresh data.
- The per-GVR in-flight guard prevents overlapping refreshes from piling up.

### Why reverse indexes instead of pattern-based invalidation?

Redis `SCAN` with wildcards is O(N) where N is the total number of keys. Reverse indexes (Redis SETs) provide O(1) membership reads and targeted O(K) deletes where K is only the number of actually-dependent keys. This is critical at scale when thousands of L1/L2 keys exist but only a handful depend on the changed GVR.

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
| `snowplow:http:` | L2 | Per-user | Raw HTTP response from resolution |
| `snowplow:resolved:` | L1 | Per-user | Fully resolved widget/RESTAction output |
| `snowplow:rbac:` | — | Per-user | SelfSubjectAccessReview result |
| `snowplow:userconfig:` | — | Per-user | User endpoint config from Secret |
| `snowplow:l1gvr:` | Index | Shared | Reverse index: GVR → dependent L1 keys |
| `snowplow:l2gvr:` | Index | Shared | Reverse index: GVR → dependent L2 keys |
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
| `raw_hits` / `raw_misses` | L1 resolved output cache performance |
| `call_hits` / `call_misses` | `/call` handler L3-direct cache performance |
| `l3_promotions` | Times L3 data was used to serve an L2 miss |
| `negative_hits` | Times a 404 sentinel was served |
| `expiry_refreshes` | Times a key was proactively re-fetched after TTL expiry |

---

## Performance Optimizations

### Parallel Widget `resourcesRefs` Resolution

Widgets with many child references (e.g. `RoutesLoader` with 42 routes) resolve all `ResourceRef` items concurrently using a goroutine pool with a semaphore (concurrency = 20). Each reference independently checks RBAC for up to 4 verbs. Previously sequential, this reduces wall-clock time from ~168 sequential operations to ~9 batches of concurrent operations for a typical routes page.

### Dynamic CRD Informer Registration

When RESTAction resolution makes HTTP calls to K8s API paths (e.g. listing composition instances), the parsed GVR is registered for informer watching via `SAddGVR`. This ensures L3 cache is populated for dynamic CRDs, enabling L3 → L2 promotion on subsequent requests. Without this, every RESTAction call for dynamic CRDs would hit the live API.

---

## Known Limitations

1. **RESTAction API items are sequential by design.** Items are processed in topological order and share a mutable JQ `dict`. Even without explicit `DependsOn`, items may reference data produced by earlier items via JQ expressions, making parallelization unsafe.

2. **RESTActions are excluded from L1 startup warmup; apiRef widgets use transitive GVR registration.** RESTActions are excluded due to deep HTTP fan-out. Widgets with `spec.apiRef` ARE warmed at startup — after resolution, the referenced RESTAction's API item paths are parsed to extract K8s API groups (including from JQ templates like `${ "/apis/composition.krateo.io/" + ... }`). The K8s discovery API is used to find all GVRs in those groups, which are then registered with `SAddGVR` (starting informers) and added to the widget's L1 GVR reverse index. This ensures L1 refresh fires when dynamic CRD data arrives, even if the RESTAction returned empty during warmup.

3. **L1 refresh uses the user's client certificate groups.** Groups are extracted from the `*-clientconfig` Secret's client certificate data. If the certificate is missing or malformed, the refresh runs with an empty groups list, potentially producing incorrect RBAC results.

---

## Source Files

| File | Responsibility |
|------|---------------|
| `internal/cache/redis.go` | RedisCache core: connection, CRUD, TTL, atomic ops |
| `internal/cache/keys.go` | Key format builders and parsers |
| `internal/cache/watcher.go` | ResourceWatcher: informers, L3 updates, L1/L2 refresh/invalidation |
| `internal/cache/warmup.go` | Startup L3 warmup and discovery caching |
| `internal/cache/tracker.go` | DependencyTracker: records GVR dependencies during resolution |
| `internal/cache/context.go` | Context helpers for cache injection |
| `internal/cache/rbac_watcher.go` | RBACWatcher: role/binding informers |
| `internal/cache/user_watcher.go` | UserSecretWatcher: clientconfig Secret informers |
| `internal/cache/metrics.go` | Global cache hit/miss counters |
| `internal/handlers/dispatchers/widgets.go` | Widget dispatcher with L1 cache read/write |
| `internal/handlers/dispatchers/restactions.go` | RESTAction dispatcher with L1 cache read/write |
| `internal/handlers/dispatchers/prewarm.go` | L1 startup warmup and request-time child pre-warming |
| `internal/handlers/dispatchers/l1_refresh.go` | L1RefreshFunc: background re-resolution on resource changes |
| `config/cache-warmup.yaml` | GVRs and categories to pre-populate at startup |
