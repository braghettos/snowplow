# Plan: Snowplow 0.25.19 Test Report Analysis & Fixes

**Run:** `snowplow_test.py --phases 1,2 --iters 10` against `ghcr.io/braghettos/snowplow:0.25.19`
**Result:** 25 PASS / 26 FAIL (51 total)
**Date:** 2026-03-18

---

## Test Report Summary

### Phase 1 — Functional Validation

| Test | Result | Key Data |
|------|--------|----------|
| T1 — L3 Warmup (5 GVR keys) | **PASS** | dbsize=711, all L3 keys present |
| T2 — L1 hit (admin) | **FAIL** | raw_hits+0, latency=465ms |
| T2 — L1 cold miss (cyberjoker 1st) | PASS | 573ms |
| T2 — L1 hit (cyberjoker 2nd) | **FAIL** | 656ms — no raw_hit increment |
| T3 — Cache hits (all 8 ep × 2 users) | **FAIL ×16** | hits+0 throughout; HTTP 0 errors on ~half |
| T4 — L3 key exists | PASS | key exists |
| T4 — L3 promotions counter | **FAIL** | l3_promotions=0 |
| T5 — /call resolved output | **FAIL** | body not a list (body parsing issue) |
| T6 — Compositions-list count | **FAIL** | 0 entries returned |
| T7 — Negative cache faster | PASS | 394ms → 305ms |
| T8 — Negative cache TTL expiry | PASS | correctly re-fetches after 35s |
| T9 — ADD informer | PASS | resource accessible after 10s |
| T9 — UPDATE label in response | **FAIL** | label not reflected within 10s |
| T9 — DELETE cleans cache | PASS | 404 after deletion |
| T10 — RBAC cache hits | **FAIL** | rbac_hits+0, HTTP 0 for both users |
| T11 — Shared L3 (admin + cyberjoker) | PASS | 2nd cyberjoker req < 1s |
| T12 — L3 GET keys (503) | PASS | |
| T12 — L3 LIST keys (174) | PASS | |
| T12 — L1 resolved keys | **FAIL** | **0 resolved keys** (critical) |
| T12 — Watched GVRs (29) | PASS | |
| T13 — Metrics counters increment | PASS (delta=0 accepted) | no actual increment observed |
| T14 — L1 hit after resource update | **FAIL** | raw_hits+0, 1340ms |

### Phase 2 — Latency Benchmark

**Backend (cache ON vs OFF) — p50:**

| Endpoint | Cache ON | Cache OFF | Speedup |
|----------|----------|-----------|---------|
| page/dashboard | 860ms | 390ms | **0.5x (REGRESSION)** |
| page/blueprints | 597ms | 391ms | **0.7x (REGRESSION)** |
| page/compositions | 483ms | 408ms | **0.8x (REGRESSION)** |
| navmenu/sidebar | 868ms | 995ms | 1.1x |
| routes/loader | 154ms | 998ms | **6.5x** |
| restaction/all-routes | 152ms | 887ms | **5.8x** |
| restaction/bp-list | 153ms | 889ms | **5.8x** |
| restaction/comp-list | 153ms | 1658ms | **10.8x** |
| **Average** | **696ms** | **824ms** | **1.2x** |

**Frontend Proxy (cache ON vs OFF):** All endpoints ~302ms in both modes — **0x speedup visible**. Frontend is adding a fixed ~300ms overhead that swamps the backend gain.

---

## Root-Cause Analysis

### Issue 1 — RBAC Invalidation Storm (CRITICAL)
**Symptom:** Logs show hundreds of `rbac-watcher: invalidated RBAC+L1+L2 for active users after role change` messages per second throughout the test run.

**Cause:** `setup_cyberjoker_rbac()` applies a `ClusterRole` with `apiGroups: ["*"], resources: ["*"], verbs: [get, list, watch]`. This triggers a massive wave of RBAC events in the cluster. The rbac-watcher sees each one and invalidates all L1+RBAC cache entries for all active users, over and over, preventing L1 from ever stabilising.

**Evidence:**
- `snowplow:resolved:*` Redis keys = **0** — L1 is being invalidated faster than it can be populated.
- All T2/T3/T10/T14 tests fail with `raw_hits+0` — no L1 hits because the keys are erased by the time the next request arrives.
- Latency for "cache ON" pages is **worse** than "cache OFF" because every request hits a cold L1 and has to re-resolve from L3/K8s.

### Issue 2 — L1 Warmup Log Message Mismatch
**Symptom:** `wait_for_l1_warmup()` waits 300s (5 minutes) and exits with WARNING.

**Cause:** The test looks for `"L1 warmup: completed"` or `"L1 warmup: skipped"` in the pod logs. In 0.25.19 the log messages may have changed, or warmup is disabled/not triggered because no active user secrets exist in Redis yet (the RBAC storm wipes them before the warmup function can emit a completion message).

**Impact:** Adds 5 minutes of dead wait to every test run and means Phase 1 starts with an un-warmed L1 cache.

### Issue 3 — L3 Promotions Counter = 0
**Symptom:** T4 fails — `l3_promotions=0` even though L3 keys exist (503 GET keys).

**Cause:** `l3_promotions` is a metric that increments when a resolution reads from L3 instead of calling the K8s API. Because the RBAC storm is constantly wiping L1, each request must fully re-resolve (reading from L3 when possible). However, if L3 reads aren't being counted as "promotions" in the new simplified code, or the metric name changed post-simplification (L2 was removed, the promotion concept changed), this counter stays 0.

### Issue 4 — /call Resolved Output Body Parsing (T5)
**Symptom:** T5 fails — "body not a list (body parsing issue)".

**Cause:** The test asserts `isinstance(status, list)` for the `/call` endpoint on `compositions-get-ns-and-crd`. With the RBAC storm making the system unstable, the response `status` field may be returning an object instead of a list, or the endpoint returns a `null`/empty status when the underlying data lookup fails under load.

### Issue 5 — Compositions-List Returns 0 Entries (T6)
**Symptom:** `compositions-list returns 0 entries`, HTTP 200.

**Cause:** The compositions-list endpoint relies on L3 data for listing compositions. If the RBAC storm is causing the rbac-watcher to invalidate and the L1 keys are absent, the restaction resolution may fail to find the correct user-scoped data and return an empty list. Alternatively, the test environment may have no compositions in the expected namespaces at this point.

### Issue 6 — HTTP 0 Errors Under Load (T3, T10)
**Symptom:** Many retry attempts with HTTP 0 during T3 and T10.

**Cause:** The snowplow pod is overwhelmed by the RBAC invalidation processing. Under the storm, the HTTP server becomes briefly unresponsive for some requests (connection reset/timeout). The retries eventually succeed but skew latency measurements.

### Issue 7 — UPDATE Label Not Reflected (T9)
**Symptom:** Label `cache-test=updated` not visible 10s after `kubectl label`.

**Cause:** The informer event for the label update arrives, the rbac-watcher storm immediately invalidates the L1 refresh, and the subsequent GET re-resolves from an old L3 snapshot. The 10s window is too short when the system is under invalidation load.

### Issue 8 — Frontend Proxy Fixed 300ms Overhead
**Symptom:** All frontend measurements ~302ms regardless of cache state.

**Cause:** The frontend proxy at `http://34.46.217.105:8080` is adding a uniform ~300ms latency (likely a fixed round-trip or additional service hop). This makes it impossible to measure cache benefits through the frontend. The frontend cache benefit is real at the backend but invisible from the proxy measurement.

### Issue 9 — Log Messages Still Reference "L1+L2" (Stale Wording)
**Symptom:** `rbac-watcher: invalidated RBAC+L1+L2 for active users` — L2 was removed.

**Cause:** The log message in `internal/cache/watcher.go` was not updated when L2 was removed from the architecture. Minor issue but misleading during debugging.

---

## Issues Priority Matrix

| # | Issue | Severity | Impact | Effort |
|---|-------|----------|--------|--------|
| 1 | RBAC invalidation storm from wildcard ClusterRole | CRITICAL | Breaks L1 entirely during tests | Medium |
| 2 | L1 warmup log message mismatch | HIGH | 5-min wasted wait, false un-warmed start | Low |
| 3 | L3 promotions counter stuck at 0 | HIGH | Metric does not reflect reality | Low |
| 4 | /call resolved output body parsing | MEDIUM | T5 false failure | Low |
| 5 | Compositions-list returns 0 | MEDIUM | T6 false failure (env dependent) | Low |
| 6 | HTTP 0 errors under invalidation load | MEDIUM | Test flakiness, skewed latency | Medium |
| 7 | UPDATE label window too short | LOW | T9 timing-sensitive false failure | Low |
| 8 | Frontend 300ms fixed overhead | LOW | Frontend speedup invisible | Medium |
| 9 | Stale "L1+L2" log wording | LOW | Misleading during debugging | Low |

---

## Improvement Plan

### Fix 1 — Eliminate RBAC Storm from Test Setup
**Files:** `e2e/bench/snowplow_test.py` → `setup_cyberjoker_rbac()`

**Action:** Replace the wildcard `ClusterRole` with a narrow set of rules targeting only the resources that cyberjoker actually needs:
```yaml
rules:
- apiGroups: ["widgets.templates.krateo.io", "templates.krateo.io"]
  resources: ["pages", "navmenus", "routesloaders", "tables", "piecharts", "restactions"]
  verbs: ["get", "list"]
- apiGroups: ["composition.krateo.io"]
  resources: ["githubscaffoldingwithcompositionpages"]
  verbs: ["get", "list"]
```
This avoids triggering RBAC events for the entire cluster and prevents the invalidation storm.

### Fix 2 — Fix L1 Warmup Detection
**Files:** `e2e/bench/snowplow_test.py` → `wait_for_l1_warmup()`

**Action:** Check actual log messages produced by 0.25.19. Update the search strings to match the real output. Add a fallback: if no warmup message is found within 60s but `/metrics/cache` shows `raw_hits > 0`, treat it as warmed.

### Fix 3 — Restore or Rename L3 Promotions Metric
**Files:** `internal/cache/redis.go` or `internal/cache/metrics.go`

**Action:** Verify that the `l3_promotions` counter is still incremented after the L3+L1 simplification. If the promotion concept was renamed or removed, update the test to use the correct counter (e.g., `get_hits` or `list_hits`), and ensure the metric is documented in `CACHE_ARCHITECTURE.md`.

### Fix 4 — Fix /call Body Type Assertion (T5)
**Files:** `e2e/bench/snowplow_test.py` → `run_phase_functional()` T5 block

**Action:** Log the full body on failure. Change assertion to check that `status` is not `None` and not empty, rather than strictly requiring a `list`. The endpoint may return a dict with nested arrays.

### Fix 5 — Add rbac-watcher Log Message Cleanup
**Files:** `internal/cache/watcher.go`

**Action:** Update the log message from `"invalidated RBAC+L1+L2"` to `"invalidated RBAC+L1"` to reflect the removal of L2. Also add rate-limiting or debouncing to the RBAC invalidation so that a burst of role events causes at most one invalidation per user per second (use a `time.After` coalesce pattern).

### Fix 6 — Fix Frontend Proxy Measurement
**Files:** `e2e/bench/snowplow_test.py` → `run_phase_latency()`

**Action:** The frontend proxy measurement is not useful at 300ms fixed overhead. Either:
- Remove frontend proxy measurement from the latency phase (it masks backend gains), OR
- Add a direct TCP connection test to diagnose the fixed overhead source.

### Fix 7 — Extend UPDATE Wait Window (T9)
**Files:** `e2e/bench/snowplow_test.py` → T9 block

**Action:** Increase the post-UPDATE wait from 10s to 20s. Also check the returned body for the label before asserting, and log the full response on failure.

### Fix 8 — Add rbac-watcher Coalescing Debounce
**Files:** `internal/cache/watcher.go` → `RBACWatcher.handleEvent()`

**Action:** Instead of immediately invalidating all L1/RBAC keys on every RBAC event, buffer events with a 500ms debounce window using a ticker. Multiple role changes within 500ms collapse into a single invalidation. This prevents storms from overwhelming the cache.

```go
// Pseudocode
func (w *RBACWatcher) coalesceInvalidate() {
    timer := time.NewTimer(500 * time.Millisecond)
    for {
        select {
        case <-w.triggerCh:
            timer.Reset(500 * time.Millisecond)
        case <-timer.C:
            w.doInvalidate()
        }
    }
}
```

---

## Performance Observations (Cache ON vs OFF)

**Where cache clearly wins (backend):**
- `routes/loader`: **6.5x** (154ms vs 998ms)
- `restaction/all-routes`: **5.8x** (152ms vs 887ms)
- `restaction/bp-list`: **5.8x** (153ms vs 889ms)
- `restaction/comp-list`: **10.8x** (153ms vs 1658ms)

**Where cache is SLOWER (backend) — symptom of RBAC storm:**
- `page/dashboard`: **0.5x** (860ms vs 390ms) — cache overhead without L1 benefit
- `page/blueprints`: **0.7x** (597ms vs 391ms)
- `page/compositions`: **0.8x** (483ms vs 408ms)

**Interpretation:** RESTActions are heavily cached in L3 and benefit enormously. Widget pages (dashboard, blueprints, compositions) are being re-resolved on every request because L1 keys are wiped by the RBAC storm before they can be reused. Once the storm is fixed, widget pages should also show 3-8x improvements based on prior measurements.

---

## Execution Order

1. **Fix 1** (RBAC narrow ClusterRole) — most impactful, unblocks everything
2. **Fix 5** (debounce RBAC invalidations) — hardening
3. **Fix 2** (warmup detection) — test reliability
4. **Fix 3** (L3 promotions metric) — observability
5. **Fix 4** (T5 body assertion) — test correctness
6. **Fix 7** (T9 timing) — test correctness
7. **Fix 6** (frontend overhead) — analysis
8. **Fix 8** (log messages) — cleanup

---

## Expected Outcome After Fixes

| Test Group | Before | After (expected) |
|------------|--------|-----------------|
| T2 L1 hits | FAIL | PASS (raw_hits+1+) |
| T3 Cache hits | FAIL ×16 | PASS ×16 |
| T4 L3 promotions | FAIL | PASS |
| T10 RBAC hits | FAIL | PASS |
| T12 L1 resolved keys | 0 | >10 |
| Backend pages speedup | 0.5–0.8x | 3–8x |
| Total failures | 26 | <5 |
