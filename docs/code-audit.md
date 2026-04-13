# Snowplow Codebase Audit

**Date**: 2026-04-11
**Scope**: internal/cache/, internal/handlers/dispatchers/, internal/resolvers/, main.go
**Branch**: main (HEAD 657314d)

---

## Summary

| Category                         | Findings | Est. lines removable |
|----------------------------------|----------|----------------------|
| Dead / unreachable code          | 8        | ~250                 |
| Legacy dual-write / blob paths   | 5        | ~80 (after migration)|
| Legacy RBAC fallback             | 2        | ~15                  |
| Unused key builders / functions  | 5        | ~40                  |
| Batch patterns to event-driven   | 2        | refactor             |
| Redundant code paths             | 3        | ~120                 |
| context.TODO() in production     | 4        | fix                  |
| Stale comments / dead doc        | 3        | ~20                  |
| Configuration knobs not wired    | 1        | ~5                   |
| TODOs / hacks in source          | 3        | varies               |

---

## 1. Dead / Unreachable Code

### 1.1 `resolveL1RefsForUser` -- never called
- **File**: `internal/handlers/dispatchers/prewarm.go`, line 805
- **What**: 74-line function `resolveL1RefsForUser` that resolves L1 refs for a single user. It was superseded by `resolveL1RefsCollect` (line 722), which returns the resolved objects for recursive child-walking. The old function only returns a count.
- **Evidence**: `grep -rn 'resolveL1RefsForUser(' internal/` shows the function is defined but never called anywhere.
- **Action**: Delete the entire function (lines 805-879).
- **Risk**: Safe to delete. No callers.

### 1.2 `collectAffectedL1Keys` -- never called
- **File**: `internal/cache/watcher.go`, line 894
- **What**: 31-line method `collectAffectedL1Keys` that aggregates L1 keys from dependency indexes for a specific resource. It was replaced by inline `collect()` closures inside `scanL3Gens` (line 356) and `reconcileGVR` (line 1234).
- **Evidence**: `grep -rn 'collectAffectedL1Keys(' internal/` shows only the definition, no callers.
- **Action**: Delete the entire method (lines 889-925).
- **Risk**: Safe to delete. No callers.

### 1.3 Dead comment block for `patchListCache`
- **File**: `internal/cache/watcher.go`, lines 1354-1360
- **What**: A comment block documenting `patchListCache` as "retained as dead code during the migration period." But the actual function body was already removed -- only the comment remains. The comment says "can be removed once per-item storage is validated" and per-item storage has been validated since v0.25.140.
- **Action**: Delete the dead comment block (lines 1354-1360).
- **Risk**: Safe to delete. Pure comment.

### 1.4 `SetPersist` -- never called
- **File**: `internal/cache/redis.go`, line 460
- **What**: `SetPersist` writes a key with TTL=0 (no expiry). Never called by any code in the codebase.
- **Evidence**: `grep -rn 'SetPersist(' internal/` shows only the definition.
- **Action**: Delete (lines 460-462).
- **Risk**: Safe to delete. No callers.

### 1.5 `DeletePattern` -- never called in production paths
- **File**: `internal/cache/redis.go`, line 663
- **What**: `DeletePattern` does SCAN + DEL on a glob pattern. Defined but never called from any production code path.
- **Evidence**: `grep -rn 'DeletePattern(' internal/` shows only the definition.
- **Action**: Delete (lines 663-669).
- **Risk**: Safe to delete. Could be useful as a utility, but unused means untested.

### 1.6 `SetString` -- never called
- **File**: `internal/cache/redis.go`, line 935
- **What**: `SetString` stores a string with TTL=0. All callers use `SetStringWithTTL` instead.
- **Evidence**: `grep -rn 'SetString(' internal/` only shows the definition (line 935) and `SetStringWithTTL` references.
- **Action**: Delete (lines 935-939).
- **Risk**: Safe to delete. No callers.

### 1.7 `GVRFromKey` -- never called
- **File**: `internal/cache/keys.go`, line 351
- **What**: Extracts a GVR from a `snowplow:get:` or `snowplow:list:` cache key. Never called by any production code.
- **Evidence**: `grep -rn 'GVRFromKey(' internal/` shows only the definition.
- **Action**: Delete (lines 351-362).
- **Risk**: Safe to delete.

### 1.8 `GetKeyPattern` and `ListKeyPattern` -- never called
- **File**: `internal/cache/keys.go`, lines 93 and 97
- **What**: Build glob patterns like `snowplow:get:{gvr}:*` and `snowplow:list:{gvr}:*`. Neither is called anywhere.
- **Evidence**: Grep confirms no callers in `internal/`.
- **Action**: Delete both functions (lines 93-99).
- **Risk**: Safe to delete.

---

## 2. Legacy Dual-Write Paths (Monolithic Blob)

The per-item index (B1) replaced monolithic `snowplow:list:*` blobs. However, several writers still dual-write both the index and the blob. The blobs waste Redis memory proportional to item count (at 50K: ~100MB per the scaling roadmap).

### 2.1 warmup.go dual-writes monolithic blobs
- **File**: `internal/cache/warmup.go`, lines 258-275
- **What**: `warmGVR` writes per-item index SETs (authoritative) AND legacy monolithic blobs (`ListKey(gvr, "")` and per-namespace `ListKey(gvr, ns)`). The comment says "dual-write for migration."
- **Action**: Remove the legacy blob writes (lines 258-275). The per-item index is authoritative and all readers use `AssembleListFromIndex` with blob fallback. Once blobs stop being written, the fallback becomes a no-op.
- **Risk**: Medium. Must verify that all LIST readers have the `AssembleListFromIndex` path. Currently confirmed: `resolve.go` (lines 210-218, 314-320), `prewarm.go` (lines 416, 463, 584), `list.go`. The blob fallback in these paths will simply miss, which is correct.

### 2.2 refreshListKey dual-writes blob
- **File**: `internal/cache/watcher.go`, lines 1457-1461
- **What**: When an expired list key is refreshed, `refreshListKey` writes per-item GET keys, per-item index SET, AND a legacy monolithic blob (line 1458-1460).
- **Action**: Remove the blob write on lines 1457-1461.
- **Risk**: Medium. Same analysis as 2.1.

### 2.3 reconcileGVR reads monolithic blob for diffing
- **File**: `internal/cache/watcher.go`, lines 1104-1119
- **What**: `reconcileGVR` reads the L3 cluster-wide list from the monolithic blob (`ListKey(gvr, "")`) to build `l3Map` for diffing against the informer store. After blob writes are removed, this read will always miss, causing `l3Map` to be empty, meaning every informer object will be classified as "added" on every reconcile.
- **Action**: Rewrite the L3 diff source to use `AssembleListFromIndex` instead of the blob. This is a precondition for removing blob writes.
- **Risk**: Medium. Functional change -- must be tested. Without this fix, removing blob writes would cause every reconcile to re-write all GET keys unnecessarily (correctness OK, but wasteful).

### 2.4 resolve.go reads blob as fallback
- **File**: `internal/resolvers/restactions/api/resolve.go`, lines 211-218, 314-320
- **What**: LIST intercept tries `AssembleListFromIndex` first, then falls back to reading the monolithic blob via `ListKey`. After blob writes are removed, this fallback will always miss, which is fine (falls through to HTTP call).
- **Action**: No immediate action needed. The fallback is harmless. Can be cleaned up after blob writes are removed (a simple deletion of 6 lines per occurrence).
- **Risk**: Low. The fallback is a cache miss, not an error.

### 2.5 `ListKey` function and `ParseListKey` kept for blob readers
- **File**: `internal/cache/keys.go`, lines 72-74, 97-99, 333-339
- **What**: `ListKey`, `ListKeyPattern`, and `ParseListKey` exist solely for the monolithic blob path. Once blobs are removed, these functions have no callers except the blob-writing code itself and the expiry handler.
- **Action**: Remove after blob writes are removed. Track as part of the blob cleanup.
- **Risk**: Low.

---

## 3. Legacy RBAC Fallback

### 3.1 IsRBACAllowed falls back to legacy STRING key
- **File**: `internal/cache/redis.go`, lines 845-849
- **What**: `IsRBACAllowed` first checks the HASH-based RBAC key, then falls back to the legacy flat STRING key (`RBACKey()`). The HASH migration has been live since v0.25.116. All writes go through `SetRBACResult` which writes to the HASH. The only way a STRING key could exist is if it survived from before v0.25.116 (over 2 weeks ago, well past the 24h TTL).
- **Action**: Remove the fallback (lines 845-849). Remove the `RBACKey` function (keys.go line 122-128) and its "Deprecated" comment.
- **Risk**: Low. The STRING keys have long since expired.

### 3.2 `RBACKey` function marked deprecated
- **File**: `internal/cache/keys.go`, lines 119-128
- **What**: Explicitly marked `// Deprecated: use RBACHashKey + RBACField instead.` Only caller is the fallback in 3.1 above.
- **Action**: Delete after removing the fallback.
- **Risk**: Safe.

---

## 4. Redundant Code Paths

### 4.1 `WarmRBACForAllUsers` duplicates namespace-loading logic
- **File**: `internal/handlers/dispatchers/prewarm.go`, lines 440-515
- **What**: `WarmRBACForAllUsers` (line 440) contains ~30 lines of namespace-loading logic (lines 458-483) that is a copy-paste of `getNamespacesFromL3` (line 411). It tries index assembly, falls back to blob, extracts names -- identical to the helper.
- **Action**: Replace lines 458-483 with `namespaces := getNamespacesFromL3(ctx, c)`.
- **Risk**: Safe. Pure refactor.

### 4.2 Duplicate RBAC iteration in `WarmRBACForAllUsers` vs `PreWarmRBACForUser`
- **File**: `internal/handlers/dispatchers/prewarm.go`
- **What**: `WarmRBACForAllUsers` (line 440) iterates GVR x namespace x verb SERIALLY (no concurrency), calling `rbac.UserCan` one by one. `PreWarmRBACForUser` (line 290) does the same but with bounded concurrency (sem of 20) and skips already-cached results. The serial version at startup is 6x slower than necessary.
- **Action**: Refactor `WarmRBACForAllUsers` to call `PreWarmRBACForUser` per user (or at least use bounded concurrency). This is a performance fix, not just cleanup.
- **Risk**: Low. `PreWarmRBACForUser` already handles the full flow.

### 4.3 `resolveL1RefsCollect` duplicates credential setup from `resolveL1RefsForUser`
- **File**: `internal/handlers/dispatchers/prewarm.go`, lines 722-803 vs 805-879
- **What**: Both functions build a user context (`BuildContext`), iterate refs with a semaphore, fetch CRs via `prewarmFetchCR`, resolve, and cache. The only difference: `resolveL1RefsCollect` returns `[]*unstructured.Unstructured`, the other returns `int64` count. Since `resolveL1RefsForUser` is dead code (finding 1.1), this is already resolved by deleting it.
- **Action**: Delete `resolveL1RefsForUser` (finding 1.1 covers this).
- **Risk**: Safe.

---

## 5. Batch Patterns That Should Be Event-Driven

### 5.1 `syncNewGVRs` polls Redis every 60s
- **File**: `internal/cache/watcher.go`, lines 162-173
- **What**: A goroutine ticks every 60s, reads the `snowplow:watched-gvrs` Redis SET, and registers any new GVRs as informers. This is a polling pattern when the system already has an event-driven path: `SAddGVR` calls `onNewGVR` (the GVR notifier), which calls `resourceWatcher.AddGVR`. Every code path that adds a GVR calls `SAddGVR`, which triggers the notifier.
- **Action**: Verify that all GVR addition paths go through `SAddGVR` (they do: warmup, auto-register, call.go). If confirmed, remove the 60s polling goroutine entirely. The only case it covers is GVRs added by external tools directly to Redis (which does not happen).
- **Risk**: Low-medium. The poller is a safety net for a case that does not exist in practice. If removed, a manually-added GVR in Redis would not be picked up until pod restart. Acceptable.

### 5.2 `WarmRBACForAllUsers` is serial, not concurrent
- **File**: `internal/handlers/dispatchers/prewarm.go`, lines 493-508
- **What**: The RBAC warmup at startup iterates all (user x GVR x namespace x verb) combinations serially. At 5 users x 20 GVRs x 120 namespaces x 6 verbs = 72,000 checks, at ~5ms each = ~360s. This blocks Phase 4b startup.
- **Action**: Use bounded concurrency (like `PreWarmRBACForUser` does) or call `PreWarmRBACForUser` for each user.
- **Risk**: Low. Already proven pattern.

---

## 6. context.TODO() in Production Code

### 6.1 JQ evaluations use context.TODO()
- **Files and lines**:
  - `internal/resolvers/widgets/resourcesrefstemplate/resolve.go`, lines 59, 88
  - `internal/resolvers/restactions/api/setup.go`, lines 36, 72
  - `internal/resolvers/restactions/api/handler.go`, line 56
  - `internal/resolvers/restactions/restactions.go`, line 66
- **What**: Six `context.TODO()` calls in JQ evaluation paths. These should propagate the caller's context so that cancellation (e.g., HTTP client disconnect, shutdown) stops JQ evaluation.
- **Action**: Thread the caller's `ctx` through to these calls. The functions already receive a context parameter.
- **Risk**: Low. Pure correctness improvement. JQ evaluation is typically fast (<100ms), so the practical impact is small, but under heavy load with stuck JQ filters this could leak goroutines during shutdown.

---

## 7. Stale Comments and Documentation

### 7.1 Comment references `patchListCache` in resolve.go
- **File**: `internal/resolvers/restactions/api/resolve.go`, near line 211
- **What**: Comments reference "L3 up-to-date via patchListCache / SetForGVR" -- but `patchListCache` was removed. The function no longer exists.
- **Action**: Update comment to reference the per-item index path.
- **Risk**: Safe.

### 7.2 `rebuildListCaches` comment says "BOTH per-item index AND monolithic blobs"
- **File**: `internal/cache/watcher.go`, lines 1304-1306
- **What**: The comment says "Writes BOTH per-item index SETs (new) AND monolithic blobs (legacy) during the migration period." But the blob writes were already removed (line 1347-1351 says "Legacy monolithic blob writes REMOVED"). The header comment is stale.
- **Action**: Update the comment to reflect that only per-item indexes are written.
- **Risk**: Safe. Pure comment fix.

### 7.3 Duplicate `expandDependents` comment at line 927-932
- **File**: `internal/cache/watcher.go`, lines 927-932
- **What**: A standalone comment block starting with "expandDependents walks the L1 dependency tree..." appears AFTER the actual `expandDependents` function (line 458). This orphaned comment is between `collectAffectedL1Keys` and `scheduleDynamicReconcile` -- it looks like a leftover from a code move. The real function and its doc are at line 455.
- **Action**: Delete the orphaned comment block (lines 927-932).
- **Risk**: Safe.

---

## 8. Unused Redis Key Patterns Still Written

### 8.1 `snowplow:l1gvr:*` written but never read for L1 refresh
- **File**: `internal/cache/keys.go`, line 158 (`L1GVRKey`); written at `keys.go` line 211 and `prewarm.go` line 951
- **What**: The `L1GVRKey` index maps a GVR to all L1 keys that depend on it. It is written during dependency registration (`RegisterL1Dependencies`, line 211) and during `registerApiRefGVRDeps` (prewarm.go line 951). However, the L1 refresh paths (both `scanL3Gens` and `reconcileGVR`) deliberately EXCLUDE this index: line 355 says "Deliberately excludes L1GVRKey (too broad -- 5000+ keys for popular GVRs)." The scanL3Gens and reconcileGVR paths only use `L1ResourceDepKey` and `L1ApiDepKey`.
- **Action**: Stop writing to `L1GVRKey` in `RegisterL1Dependencies`. The writes add 2 pipeline commands per L1 resolution with no reader. Remove the `L1GVRKey` function. The one remaining writer (`prewarm.go` line 951) should also be removed.
- **Risk**: Low-medium. The index was designed as a "fallback" but was never used as such. Confirm no external tools read it.

### 8.2 `snowplow:list:*` blobs written but readers are migrating away
- Covered in Section 2 (Legacy Dual-Write Paths). Action: stop writing after reconcileGVR diff source is migrated.

---

## 9. Configuration Knobs Not Wired

### 9.1 `--blizzard` / `BLIZZARD` flag parsed but not used
- **File**: `main.go`, line 63
- **What**: `blizzardOn` is parsed from `BLIZZARD` env var and `--blizzard` flag. It is assigned to `os.Setenv("TRACE", ...)` at line 86. However, no code in the snowplow codebase reads `TRACE` from the environment or checks `blizzardOn` after that point. The plumbing library may read it, but within snowplow it has no effect.
- **Action**: Verify if the plumbing library's `xcontext.Logger` or HTTP middleware uses `TRACE`. If not, remove the flag definition and env var set.
- **Risk**: Low. Need to check the plumbing library before removing.

---

## 10. Hacks and Workarounds

### 10.1 Proxy dispatcher hack for restactions routing
- **File**: `internal/handlers/proxy.go`, line 39
- **What**: Comment says `// Hack caused by new Widgets handlers`. The routing key for restactions is `"restactions." + gv.Group` (line 41) instead of just `gv.Group`. This was a workaround because widgets and restactions share the `templates.krateo.io` group but need different handlers. The dispatchers map at `dispatchers.go` line 9 hardcodes `"restactions.templates.krateo.io"`.
- **Action**: This hack is structural and works correctly. Low priority, but could be cleaned up by routing on `resource` name instead of `group` name. The current approach is fragile if more resource types under `templates.krateo.io` need dispatchers.
- **Risk**: Low. Works, just ugly.

### 10.2 `registerApiRefGVRDeps` creates a new `rest.InClusterConfig` per call
- **File**: `internal/handlers/dispatchers/prewarm.go`, line 937
- **What**: Every call to `registerApiRefGVRDeps` creates a fresh `rest.InClusterConfig()` (line 937) and then a fresh `discovery.NewDiscoveryClientForConfig` (inside `discoverGVRsForGroups`). At startup with 20 widgets x 5 users = 100 calls, each creating a new REST config and discovery client.
- **Action**: Pass the existing `rest.Config` as a parameter instead of creating a new one each time.
- **Risk**: Safe. Pure performance improvement.

### 10.3 `preWarmChildWidgets` uses `context.Background()` for goroutine
- **File**: `internal/handlers/dispatchers/prewarm.go`, line 142
- **What**: The goroutine uses `context.Background()` with a 30s timeout. This means the goroutine outlives pod shutdown (Bug 10 from the audit). However, since `preWarmComplete` is set early and disables this path (line 117), this only fires during initial warmup.
- **Action**: Use the process context (appCtx) instead of `context.Background()`. After initial warmup, this code path is dead, but during warmup it should respect shutdown signals.
- **Risk**: Low.

---

## 11. `AllResolvedPattern` Constant -- Never Used

- **File**: `internal/cache/keys.go`, line 153
- **What**: `AllResolvedPattern = "snowplow:resolved:*"` is defined but never used anywhere. It was intended for bulk L1 invalidation via SCAN+DEL, but all invalidation paths now use per-user indexes or targeted dep indexes.
- **Action**: Delete the constant.
- **Risk**: Safe. No callers.

---

## Priority-Ordered Action Plan

### P0 -- Remove now (safe, no behavior change)
1. Delete `resolveL1RefsForUser` (prewarm.go:805-879)
2. Delete `collectAffectedL1Keys` (watcher.go:889-925)
3. Delete dead `patchListCache` comment (watcher.go:1354-1360)
4. Delete orphaned `expandDependents` comment (watcher.go:927-932)
5. Delete `SetPersist` (redis.go:460-462)
6. Delete `DeletePattern` (redis.go:663-669)
7. Delete `SetString` (redis.go:935-939)
8. Delete `GVRFromKey` (keys.go:351-362)
9. Delete `GetKeyPattern` and `ListKeyPattern` (keys.go:93-99)
10. Delete `AllResolvedPattern` (keys.go:150-153)
11. Delete deprecated `RBACKey` + legacy RBAC fallback (keys.go:119-128, redis.go:845-849)
12. Fix stale comments (watcher.go:1304-1306, resolve.go patchListCache references)

**Estimated diff**: ~350 lines removed, 0 lines added, 0 behavior change.

### P1 -- Remove legacy blob dual-writes (needs testing)
1. Remove blob writes from `warmGVR` (warmup.go:258-275)
2. Remove blob write from `refreshListKey` (watcher.go:1457-1461)
3. Migrate `reconcileGVR` diff source from blob to `AssembleListFromIndex` (watcher.go:1104-1119)
4. After migration validated: remove `ListKey`, `ParseListKey`, `ListKeyPattern`

**Estimated impact**: ~100MB Redis memory saved at 50K, ~200ms saved per warmup GVR.

### P2 -- Refactors (improves performance / correctness)
1. DRY `WarmRBACForAllUsers` namespace loading (prewarm.go:458-483)
2. Add concurrency to `WarmRBACForAllUsers` (prewarm.go:493-508)
3. Thread caller context through `context.TODO()` JQ calls (6 sites)
4. Stop writing `L1GVRKey` index (keys.go:211, prewarm.go:951)
5. Pass existing `rest.Config` to `registerApiRefGVRDeps` (prewarm.go:937)
6. Consider removing `syncNewGVRs` 60s poller (watcher.go:162-173)
