# Snowplow L3+L1 Post-Deploy Analysis Plan

**Created:** 2026-03-18  
**Context:** After committing L3+L1 cache simplification (tag 0.25.19), pipeline build, deploy, and test run.

---

## 1. Status Summary

| Step | Status | Notes |
|------|--------|-------|
| Commit & push | ✅ Done | `ba876ef` on main |
| Tag 0.25.19 | ✅ Pushed | Triggers `release-tag` workflow |
| Pipeline (image build) | ⏳ In progress | ~25–30 min typical; check [Actions](https://github.com/braghettos/snowplow/actions) |
| Deploy image | ⏸ Pending | After pipeline completes |
| Run test | ⏸ Pending | After deploy |
| Evaluate report | ⏸ Pending | After test completes |

---

## 2. Deploy Image (after pipeline completes)

When the `release-tag` workflow shows **completed** for 0.25.19:

```bash
# Option A: kubectl set image (if deployment exists)
kubectl set image deployment/snowplow snowplow=ghcr.io/braghettos/snowplow:0.25.19 -n krateo-system
kubectl rollout status deployment/snowplow -n krateo-system --timeout=300s

# Option B: Helm/ArgoCD — update image tag in values or Application manifest to 0.25.19
```

Ensure `CACHE_ENABLED=true` (or Redis sidecar present) for cache to be active.

---

## 3. Run Test

**Important:** Tests will not start until the expected image is deployed. Set `EXPECTED_IMAGE_TAG` to the version you deployed (e.g. `0.25.19`). The script verifies the deployment image before running.

```bash
cd /path/to/snowplow
export SNOWPLOW_URL="http://<snowplow-svc>:8081"   # or LoadBalancer IP
export AUTHN_URL="http://<authn-svc>:8082"
export FRONTEND_URL="http://<frontend-svc>:8080"   # optional, for phase 4
export EXPECTED_IMAGE_TAG=0.25.19                  # required — must match deployed image

# Full test (all 4 phases)
python3 e2e/bench/snowplow_test.py

# Or via wrapper (creates .venv-bench, installs playwright)
./e2e/bench/run_full_matrix.sh

# Quick smoke (functional + latency + scaling stages 1–3 only)
python3 e2e/bench/snowplow_test.py --phases 1,2,3 --smoke

# Functional + latency only (no scaling, no browser)
python3 e2e/bench/snowplow_test.py --phases 1,2
```

To bypass the image check (e.g. when kubectl is unavailable): `export SKIP_IMAGE_CHECK=1`

Results are written to:
- `/tmp/snowplow_test_results.json` — functional test results
- `/tmp/scaling_matrix_results.json` — scaling phase (if run)
- `/tmp/browser_results.json` — browser phase (if run)

---

## 4. Evaluate Test Report

### 4.1 Functional Phase (Phase 1)

| Check | What to look for |
|-------|------------------|
| **Pass rate** | All `passed: true` in `snowplow_test_results.json` |
| **T1 L3 warmup** | Redis keys exist for restactions, pages, navmenus, compositiondefs, CRDs |
| **T2–T3 L1 hits** | `raw_hits` or `get_hits` delta ≥ 1 after warmup |
| **T4 L3 direct read** | `l3_promotions` > 0, L3 object keys exist |
| **T7–T8 negative cache** | 2nd request faster than 1st; post-TTL expiry slower again |
| **T9 informer CRUD** | ADD/UPDATE/DELETE reflected in GET responses |
| **T14 L1 refresh** | L1 hit after resource update (no cold miss) |

### 4.2 Latency Phase (Phase 2)

| Check | What to look for |
|-------|------------------|
| **Speedup** | Cached p50 < nocache p50 for most endpoints |
| **Backend avg** | Overall speedup ≥ 1.5x typical |
| **Frontend avg** | If measured, similar or better speedup |

### 4.3 Scaling Phase (Phase 3)

| Check | What to look for |
|-------|------------------|
| **Stage progression** | Cold/WARM p50 stable or growing slowly with load |
| **Cache ON vs OFF** | Cache ON consistently faster at same stage |
| **No crashes** | No `concurrent map iteration` or panic in logs |

### 4.4 Browser Phase (Phase 4)

| Check | What to look for |
|-------|------------------|
| **TTFB** | < 500ms for warm requests |
| **XHR waterfall** | Shorter with cache than without |
| **Transfer** | Reasonable payload sizes |

---

## 5. Issues to Analyze and Possible Improvements

### 5.1 If Functional Tests Fail

| Failure pattern | Likely cause | Improvement |
|-----------------|--------------|-------------|
| T1 warmup keys missing | L3 warmup not running or wrong GVRs | Verify `cache-warmup.yaml` GVRs; check logs for "L3 warmup" |
| T2/T3 no cache hits | L1 not populated or wrong cache key | Check `snowplow:resolved:*` keys; verify JWT subject in key |
| T4 L3 promotions = 0 | L3 direct read path not used | Verify GET/LIST paths use L3 in resolve.go |
| T7/T8 negative cache wrong | TTL or sentinel logic | Check `NegativeCacheTTL` constant; Redis key expiry |
| T9 informer not reflecting | Watcher not firing or wrong GVR | Check informer logs; verify watched GVRs |
| T14 L1 refresh fails | Refresh goroutine not running | Check "L1 refresh:" in logs; verify `l1Refresh` callback |

### 5.2 If Latency Is Worse With Cache

| Symptom | Likely cause | Improvement |
|---------|--------------|-------------|
| Cached slower than nocache | Redis latency or serialization overhead | Profile Redis round-trips; consider connection pooling |
| High p99 variance | Thundering herd or lock contention | Add request coalescing; check mutex hotspots |
| Frontend slow despite backend fast | Proxy or network | Measure frontend vs backend separately |

### 5.3 If Scaling Phase Crashes

| Symptom | Likely cause | Improvement |
|---------|--------------|-------------|
| `concurrent map iteration and map write` | Shared map access | Audit remaining maps; use `sync.Map` or `atomic.Value` |
| Panic in watcher | Unhandled event | Add `defer recover()` in all goroutines |
| OOM | Too many informers or cache entries | Limit watched GVRs; add TTL eviction |

### 5.4 General Improvements

| Area | Possible improvement |
|------|----------------------|
| **Observability** | Add Prometheus metrics for L1/L3 hit rates, latency histograms |
| **Resilience** | Circuit breaker for Redis; fallback to K8s API on cache failure |
| **Performance** | Connection pooling for Redis; lazy L1 warmup |
| **Testability** | Mock Redis in unit tests; add benchmark for cache hit path |
| **Documentation** | Runbook for cache troubleshooting; update CACHE_ARCHITECTURE.md with findings |

---

## 6. Next Steps After Evaluation

1. **Pipeline completed?** Check https://github.com/braghettos/snowplow/actions for run #42.
2. **Deploy** 0.25.19 image using the commands in §2.
3. **Run** `snowplow_test.py` (or `run_full_matrix.sh`) with correct env vars.
4. **Inspect** `/tmp/snowplow_test_results.json` and any phase-specific outputs.
5. **Update this plan** with actual failures and root causes.
6. **Create follow-up tasks** for each improvement identified in §5.
