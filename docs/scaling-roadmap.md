# Snowplow Cache — Scaling Roadmap

**Baseline**: v0.25.131 (bounded-eager L1 refresh, full OTel, stale-while-revalidate Cache-Control)
**Current validated scale**: 1,200 compositions × 2 users (4.3-4.5s convergence, full content correctness)
**Target scale**: 50,000+ compositions × 1,000 users

---

## Phase A — Validate and Harden (Weeks 1-2)

### A1. Parallel user refresh in L1 refresh loop

**What**: The current `l1_refresh.go` processes users sequentially — user A's keys are fully refreshed before user B starts. Add an outer bounded worker pool (4 concurrent users) so multiple users' L1 keys refresh in parallel.

**Current code**: `l1_refresh.go:95` iterates `for username, keys := range byUser` sequentially.

**Proposed change**: Wrap the per-user loop in a goroutine pool with a semaphore of size 4. Each goroutine handles one user's full refresh (including cascade).

**Pros**:
- At 50 hot users × 5s each: reduces from 250s sequential to ~63s (4 concurrent)
- Simple implementation (~15 lines: add outer semaphore + WaitGroup)
- No correctness risk — each user's refresh is independent
- The `l1RefreshRunning` atomic lock already prevents concurrent refresh CYCLES; this parallelizes WITHIN a single cycle

**Cons**:
- 4 concurrent users × 8 concurrent keys/user = 32 simultaneous goroutines hitting K8s API + Redis
- RBAC API pressure increases 4x (32 concurrent SelfSubjectAccessReview calls vs 8 today)
- If one user's refresh panics, recovery is per-goroutine — the outer pool must handle this
- Memory: 4 concurrent 100MB JSON marshal/unmarshal = 400MB peak transient allocation

---

### A2. User activity classes (hot/warm/cold)

**What**: Track last-request timestamp per user (`snowplow:last-seen:{username}` with 60-min TTL). On L1 refresh, classify users and prioritize:
- **Hot** (last request < 5 min): refresh immediately
- **Warm** (5-60 min): defer to next refresh cycle
- **Cold** (> 60 min or no timestamp): skip entirely

**Current code**: `l1_refresh.go` filters by `active-users` set (has `-clientconfig` secret). All authenticated users are treated equally.

**Proposed change**:
1. In `userconfig.go` HTTP middleware: on every authenticated request, fire-and-forget `SET snowplow:last-seen:{username} <timestamp> EX 3600`
2. In `l1_refresh.go`: after filtering by active-users, GET `snowplow:last-seen:{username}` for each user, sort by recency, apply class thresholds

**Pros**:
- Typical enterprise at 1000 users: ~50 hot, ~200 warm, ~750 cold
- Background work per change: 50 hot × 5s / 4 concurrent = 63s (vs 250s for all authenticated)
- Warm users get refreshed in the next cycle (not dropped, just deferred)
- Cold users' L1 expires via TTL — zero waste, correct behavior on next login
- No external dependency — uses the same Redis instance

**Cons**:
- One additional Redis SET per HTTP request (fire-and-forget, ~0.1ms, but at 1000 users × 14 calls/page = 14,000 SETs/min)
- Redis memory: 1000 keys × ~50 bytes = 50KB (negligible)
- Threshold tuning: 5 min and 60 min are arbitrary — may need production data to calibrate
- Edge case: a user who logs in, makes one request, then closes the browser stays "hot" for 5 min and gets unnecessary refreshes
- Complexity: adds a third classification layer on top of active-users and L1 TTL

---

### A3. L1 refresh OTel metrics and backpressure alerting

**What**: Add OTel histogram for `l1.refresh.duration` and counter for `l1.refresh.users.active`. Upgrade the refresh context timeout log from WARN to ERROR and add a span event.

**Current code**: `l1_refresh.go` logs "L1 refresh: starting/done" at INFO. The 60s context timeout in `watcher.go:418` silently drops refreshes.

**Proposed change**:
1. Record `l1.refresh.duration` histogram after each refresh cycle
2. Record `l1.refresh.users.active` gauge with the count of users refreshed
3. On context timeout: `span.SetStatus(codes.Error)` + `slog.Error` (not WARN)
4. Add `l1.refresh.skipped_users` counter for cold users

**Pros**:
- Enables alerting before users notice degradation (refresh > 30s = approaching timeout)
- Makes the active-users filter observable (if skipped_users drops to 0, the filter is broken)
- Feeds HyperDX dashboards for capacity planning
- Zero performance impact (OTel metrics are atomic increments)

**Cons**:
- More OTel cardinality (one histogram per refresh cycle — manageable)
- Requires ClickHouse retention policy for histogram data (can grow large)
- Alert fatigue risk if thresholds are set too aggressively during burst deployments

---

### A4. Phase 7 test: 5,000 compositions × 500 namespaces

**What**: Extend the e2e test suite to validate at 5,000 compositions across 500 namespaces with 3 concurrent browser sessions (admin, cyberjoker, + a third test user).

**Current code**: Phase 6 deploys 1,200 compositions across 120 namespaces with 2 users.

**Proposed change**: New `--phases 7` option that:
1. Creates 500 bench namespaces
2. Deploys 5,000 compositions (10 per namespace)
3. Runs convergence verification with 3 concurrent Playwright browser contexts
4. Measures Redis memory, goroutine count, K8s API call rate via OTel
5. Content correctness check at scale

**Pros**:
- Finds the actual breaking point before customers hit it
- Validates all Phase A changes at realistic scale
- Measures metrics we've only extrapolated (Redis memory, resolution time at 5K)
- Tests concurrent-user interaction (singleflight dedup, RBAC contention)

**Cons**:
- Test duration: deploying 5,000 compositions takes ~80 minutes (vs 20 min for 1,200)
- Full Phase 7 run: ~3 hours including cleanup
- Requires a third test user with `-clientconfig` secret
- GKE cluster resource pressure: 5,000 CRs in etcd (50MB) is within limits but noticeable
- Cannot test 10K+ compositions without risking etcd pressure on the shared cluster

---

## Phase B — Pagination (Weeks 3-4)

### B1. Server-side paginated L1 storage

**What**: Instead of storing the entire compositions-list resolved output as a single L1 Redis key (~12MB at 1,200 compositions, ~100MB at 10K), store it as per-page keys: `snowplow:resolved:{user}:{gvr}:{ns}:{name}:p{page}:pp{perPage}`.

**Current code**: `restactions.go:219` calls `c.SetResolvedRaw(ctx, resolvedKey, raw)` with the FULL resolved output. The key format in `keys.go:340` already includes `page` and `perPage` but the resolver always produces the full result.

**Proposed change**:
1. In the RESTAction resolver (`resolve.go`), when page/perPage are specified, pass them to the K8s API as `?limit=N&continue=token` instead of fetching all and slicing
2. Store only the requested page in L1
3. When page/perPage are NOT specified (full list request), still store as a single key (backward compatible)

**IMPORTANT**: The Krateo frontend does NOT currently support paginated compositions-list rendering. This feature requires coordinated frontend + backend work:
- **Backend** (snowplow): accept and honor page/perPage parameters for compositions-list
- **Frontend**: modify the compositions table component to request paginated data, handle page navigation, and display total count from a separate aggregation widget (piechart already provides this)
- This is NOT a backend-only change. Frontend must be adapted first or simultaneously.

**Pros**:
- Breaks the 512MB Redis value limit: each page is ~1MB (100 items × 10KB) instead of 100MB
- 100x reduction in per-request JSON marshal/unmarshal time
- Browser receives 1MB instead of 100MB — instant rendering
- L1 memory per user drops from 100MB to ~3MB (3 pages cached via LRU)
- patchListCache on paginated keys: only the affected page needs re-resolve, not the entire list

**Cons**:
- **Frontend change required**: the current frontend requests the full compositions-list without pagination and renders it client-side with virtual scrolling/filtering. Adding server-side pagination requires frontend changes to the compositions table, search, sorting, and filtering components. This is a cross-team effort, not a snowplow-only change
- **Invalidation complexity**: when a composition is added/deleted, WHICH page keys need invalidation? A composition in namespace bench-ns-50 could affect page 5 or page 50 depending on sort order. Safest approach: invalidate ALL pages for that user's compositions-list (batch DELETE of page keys)
- **K8s API pagination semantics**: `?limit=100&continue=token` returns items in etcd key order, not alphabetical. Sort order depends on the continue token. If the frontend wants alphabetical sorting, server-side pagination may return inconsistent pages during mutations
- **Continue token staleness**: K8s continue tokens are tied to a specific resource version. If the resource changes between page fetches, the token becomes invalid. The client must restart from page 1
- **Cache coherence**: user views page 1 (items 1-100), then a new composition is added. Page 1 L1 key is invalidated. User navigates to page 2 — but page 2's L1 key is also invalidated because the new item shifted all subsequent pages. ALL page keys must be invalidated on any change, losing the per-page caching benefit
- **Aggregation split**: the compositions-list RESTAction uses a JQ filter that operates on the FULL array (counting ready/not-ready). Paginated K8s API results don't have aggregated counts. The piechart widget (which shows total count) would need a separate aggregation query — this may already work since the piechart is a separate widget
- **Backward compatibility**: unpaginated requests must still work (return full list). The system must handle both paths until the frontend is fully migrated

---

### B2. RBAC pre-warming for dynamic GVRs

**What**: On user's first request (or on `-clientconfig` secret creation), pre-compute and cache RBAC decisions for all dynamic GVR namespaces. Currently, RBAC is checked per-namespace per-GVR per-verb on demand via SelfSubjectAccessReview.

**Current code**: `rbac.go:50` calls `cache.RBACKey(username, verb, gr, namespace)` for each check. On cache miss, it makes a K8s SelfSubjectAccessReview API call.

**Proposed change**:
1. When a user's `-clientconfig` secret is created/updated (via UserSecretWatcher), enumerate all dynamic GVR namespaces
2. Batch SelfSubjectAccessReview calls for the user across all namespace × verb combinations
3. Store results in RBAC cache with 24h TTL
4. On RoleBinding/ClusterRoleBinding change (via RBACWatcher), invalidate and re-warm affected users

**Pros**:
- Eliminates cold-start RBAC latency: 500 namespaces × 5 verbs = 2,500 API calls → done once at login, not on first page load
- RBAC cache hit rate goes from ~84% (measured) to ~99%+
- Reduces K8s API server pressure from per-request to per-login
- 24h TTL matches certificate validity window (authn manages lifecycle)

**Cons**:
- Pre-warm cost: 2,500 SelfSubjectAccessReview calls per user login. At 100 logins/hour: 250K API calls/hour. May need rate limiting
- Memory: 1000 users × 2,500 RBAC entries × 100 bytes = 250MB in Redis. Significant but within budget
- Staleness: if an admin changes RoleBindings, the 24h-cached RBAC decisions are wrong until the RBACWatcher invalidates them. Current RBACWatcher handles this, but the invalidation + re-warm cycle takes time
- Complexity: pre-warming requires knowing ALL namespace × GVR combinations at login time. New namespaces created after login won't have pre-warmed RBAC until the next RoleBinding event
- Over-warming: pre-warms RBAC for namespaces the user may never visit (e.g., system namespaces). Wastes API calls and Redis memory

---

### B3. MGET batching for large namespace iterators

**What**: The resolver's L3 cache intercept uses `GetRawMulti` (Redis MGET) to pre-fetch keys for all namespaces in a single pipeline. At 500+ namespaces, this is a 500-key MGET that blocks Redis for the duration. Batch into chunks of 100.

**Current code**: `resolve.go` collects all iterator keys and calls `c.GetRawMulti(ctx, keys)` in one call. `redis.go:GetRawMulti` executes a single MGET pipeline.

**Proposed change**: In `GetRawMulti`, if `len(keys) > 100`, split into chunks of 100 and execute each chunk as a separate MGET pipeline call. Merge results.

**Pros**:
- Prevents Redis blocking: a 500-key MGET can take 5-10ms, during which no other Redis command can execute. 5 × 100-key MGET = same total time but interleaved with other clients
- Simple implementation: ~10 lines in `redis.go`
- No behavioral change for callers

**Cons**:
- Slightly higher total latency: 5 sequential MGETs vs 1 large MGET adds ~1ms overhead per chunk (Redis round-trip)
- Marginal benefit at current scale (120 namespaces = 1 MGET, fine)
- Only matters at 500+ namespaces, which is Phase B territory

---

## Phase C — 50K Ceiling (Weeks 5-8)

### C1. Switch from gzip to zstd compression

**What**: Replace `gzip.BestSpeed` in `redis.go:43` with zstd (Zstandard) compression. Zstd is 3-5x faster at similar compression ratios for JSON data.

**Current code**: `redis.go:47-59` uses `compress/gzip` with `gzip.BestSpeed`. Compression ratio is ~3:1 for composition JSON (measured: 12MB → 1.1MB at 1,200 compositions).

**Proposed change**: Use `github.com/klauspost/compress/zstd` with a pooled encoder at default compression level.

**Pros**:
- Compress: 200MB/s (gzip BestSpeed) → 800MB/s (zstd default). At 100MB payload: 500ms → 125ms
- Decompress: 400MB/s (gzip) → 1.5GB/s (zstd). At 100MB payload: 250ms → 67ms
- Better compression ratio: zstd achieves 4-5:1 on repetitive JSON vs gzip's 3:1
- Reduces Redis memory: 10K compositions per user goes from ~9.2MB to ~6.5MB gzipped
- Widely adopted: Kubernetes itself uses zstd for etcd snapshots

**Cons**:
- **Breaking change**: existing Redis data compressed with gzip cannot be read by zstd decoder. Requires either (a) migration script to re-compress all keys, or (b) dual-read: try zstd first, fall back to gzip, write new values as zstd
- Adds a Go dependency: `github.com/klauspost/compress` (~2MB binary size increase)
- zstd encoder pools require initialization (~10MB resident memory for dictionary tables)
- Testing: must verify compression ratio claims with actual composition data, not benchmarks

---

### C2. Incremental L1 update (patch, don't re-resolve)

**What**: When a single composition changes (ADD/UPDATE/DELETE), instead of re-resolving the ENTIRE compositions-list for each user (which at 50K compositions takes 30+ seconds), patch only the affected item in the existing L1 value.

**Current code**: L1 refresh calls `refreshSingleL1` which calls `ResolveWidgetBackground` → full RESTAction resolution → K8s API calls → JQ evaluation → full JSON marshal.

**Proposed change**:
1. When the trigger is a single-object change (not a bulk reconcile), read the user's existing L1 value
2. Find the affected composition in the serialized JSON (by namespace/name)
3. For ADD: insert the new composition's data
4. For DELETE: remove the composition entry
5. For UPDATE: replace the composition entry
6. Re-apply the JQ filter (e.g., update the piechart count) without re-fetching all compositions
7. Write the patched L1 value back

**Pros**:
- Resolution time for single-item change: from O(N) full-list to O(1) patch. At 50K compositions: 30s → <100ms
- Eliminates K8s API calls for the unchanged 49,999 compositions
- Reduces Redis read/write from 100MB to ~10KB (just the changed item)
- Convergence stays <5s even at 50K compositions

**Cons**:
- **JQ re-evaluation complexity**: the compositions-list JQ filter aggregates data (counts ready/not-ready, applies transforms). Patching a single item requires re-running the JQ filter on the full array — but the array is in the L1 value (already in memory after reading it). The JQ evaluation is ~2s at 10K items, so patching saves K8s API time but not JQ time
- **Correctness risk**: the patched L1 may diverge from what a full re-resolve would produce. The JQ filter may have cross-item dependencies (e.g., "show top 10 by creation date" — adding a new item changes the entire ranking). Periodic full re-resolve is needed as a safety net
- **Concurrency**: if two changes arrive simultaneously, both try to read-modify-write the same L1 key. Need CAS (WATCH/MULTI/EXEC) on L1 — same contention problem as patchListCache
- **Not applicable to all widgets**: only works for list-based widgets. Form widgets, detail views, and widgets with complex API chains still need full re-resolve
- **Implementation complexity**: High. Requires understanding each RESTAction's JQ filter semantics to know if patching is safe. May need a "patchable" annotation on RESTActions

---

### C3. Parallel topological levels in resolver

**What**: The RESTAction resolver already computes dependency levels via `topologicalLevels()` but then flattens them and resolves sequentially. Run each level in parallel with `sync.WaitGroup`.

**Current code**: `resolve.go:66` computes `levels` (array of arrays), then `resolve.go:101` iterates `for _, id := range names` (flattened) sequentially.

**Proposed change**: Replace the flat loop with:
```go
for _, level := range levels {
    var wg sync.WaitGroup
    for _, id := range level {
        wg.Add(1)
        go func(id string) {
            defer wg.Done()
            // resolve this API call
        }(id)
    }
    wg.Wait()
}
```

**Pros**:
- APIs within the same dependency level are independent — safe to run in parallel
- At 2 levels with 120 namespaces: level 1 (1 call: namespaces+CRDs) then level 2 (120 LIST calls in parallel). Current: 121 sequential calls
- Estimated savings: 30-40% of cold resolution time. At 10K compositions: ~20s → ~12s
- Simple change: ~20 lines, no new dependencies

**Cons**:
- Burst of goroutines: 120 concurrent K8s API calls may overwhelm the user's rate limit or the API server
- Need a bounded semaphore (e.g., 20 concurrent) within each level, not unbounded parallelism
- Error handling: if one call in a level fails, should the entire level fail? Current sequential approach stops on error; parallel needs error collection
- Context cancellation: if the HTTP request is cancelled, all parallel goroutines must stop. Need `context.WithCancel` propagation
- Marginal benefit at current scale (120 namespaces × 2ms per LIST = 240ms sequential, which is not the dominant cost)

---

## Phase D — 500K+ (Architectural Planning)

### D1. Sharded snowplow pods

**What**: Run N snowplow instances, each responsible for a subset of users (hash username → pod). Each pod watches all K8s resources but only maintains L1 for its assigned users.

**Pros**:
- Linear horizontal scaling: 4 pods = 4x user capacity
- Each pod's Redis L1 is 1/N the size
- L1 refresh work distributed across pods
- Failure isolation: one pod crashing affects 1/N users

**Cons**:
- Requires a load balancer that routes by username (not standard round-robin)
- State coordination: which pod owns which user? Consistent hashing with rebalancing on pod scale
- Each pod still watches ALL K8s resources (informer memory doesn't shrink)
- Complexity: deployment, service mesh, session affinity, health checks per shard
- Operational burden: N pods to monitor, N Redis instances (or shared external Redis with key prefixes)

---

### D2. Push-based invalidation (resolve on HTTP request only)

**What**: Stop proactive L1 refresh entirely. On L3 change, just mark L1 stale (similar to v0.25.130). Resolve ONLY when a user makes an HTTP request. Serve stale data until then.

**Pros**:
- Zero background work regardless of user count
- Simplifies the entire l3gen scanner → L1 refresh pipeline
- Each user pays their own resolution cost — no cross-user impact
- Natural load shedding: inactive users generate zero work

**Cons**:
- Convergence = time until user's NEXT request. If user has the page open with React Query 30s staleTime, convergence = 30s (vs current 5s)
- First user after a change sees stale data, then waits for background refresh. UX: stale → spinner → fresh (3 states instead of 2)
- Singleflight dedup is critical: 50 users requesting the same stale key = 1 resolution, but 49 users wait
- At 50K compositions, the first-user resolution takes 30+ seconds (the cold-start problem again)
- Diego explicitly requires <5s convergence — this approach trades convergence for scalability

---

### D3. External Redis cluster

**What**: Replace the Redis sidecar (1GB, single instance) with an external Redis cluster (16-64GB, HA, replicated).

**Pros**:
- Memory scales independently of the pod
- High availability: Redis Sentinel or Cluster mode survives pod restarts
- Shared across multiple snowplow pods (enables sharding)
- Managed options: GKE Memorystore (zero ops)

**Cons**:
- Network latency: sidecar = <0.1ms, external = 0.5-2ms per call. At 50 Redis calls per page load: +25-100ms
- Operational complexity: separate deployment, monitoring, backup, upgrades
- Cost: Memorystore 16GB = ~$200/month (vs sidecar = free)
- Connection pooling: 1000 concurrent users × N connections per user can exhaust Redis `maxclients` (default 10,000)
- Data persistence: if external Redis restarts, L1 is lost. Cold-start storm: 1000 users all miss L1 simultaneously

---

## Summary Matrix

| Phase | Feature | Scale unlocked | Effort | Risk |
|-------|---------|---------------|--------|------|
| A1 | Parallel user refresh | 1K users faster | 1 day | Low |
| A2 | Activity classes (hot/warm/cold) | 1K users efficient | 2 days | Low |
| A3 | OTel metrics + alerting | Production readiness | 1 day | None |
| A4 | Phase 7 test (5K compositions) | Validation | 3 days | None |
| B1 | Paginated L1 storage | 50K compositions | 1 week | Medium |
| B2 | RBAC pre-warming | 500 namespaces | 3 days | Medium |
| B3 | MGET batching | 500+ namespaces | 2 hours | Low |
| C1 | zstd compression | 30% less memory + 3x faster | 2 days | Medium |
| C2 | Incremental L1 patch | 50K compositions fast convergence | 1 week | High |
| C3 | Parallel topological levels | 30-40% faster resolution | 2 days | Low |
| D1 | Sharded pods | 5K+ users | 2 weeks | High |
| D2 | Push-based invalidation | Unlimited users (trades convergence) | 1 week | Medium |
| D3 | External Redis | 64GB+ memory | 3 days | Low |
