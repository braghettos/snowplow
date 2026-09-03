// uaf_put_decline_metrics.go — 1.12.3 A-1 mitigation falsifier counters.
//
// THE DEFECT (A-1, reproduced by both red-teams against a real published RBAC
// snapshot): the `restactions` L1 key folds only BindingUID + RBACSubGen — the
// per-object userAccessFilter (UAF) narrowing SCOPE is not in the key. Two users
// who share the first-match binding for `get restactions` therefore derive the
// SAME key while their UAF-narrowed bodies legitimately differ, and the hit path
// writes entry.RawJSON verbatim with no re-narrowing: one user's rows are served
// to the other. The sibling raFullList cell folds BindingUID alone and has the
// same shape with fewer defences.
//
// THE 1.12.3 MITIGATION: do not cache what the key cannot separate. Every
// UAF-bearing RESTAction DECLINES the L1 Put (all three restactions Put sites)
// and BYPASSES the raFullList cell; the request is still served normally (200
// with its own correctly-narrowed body) — only the cache write is skipped.
// 1.13.0 replaces this with a UAF-scope digest folded into the key (v7), at
// which point caching is re-enabled for these RAs and these counters return to
// their pre-1.12.3 zero.
//
// WHY COUNTERS AND NOT ONLY LOGS (feedback_measurement_use_expvar_not_log_tails
// + the BumpExternalSkippedPut precedent): the decline fires on the hot path of
// every UAF /call, so its log line is DEBUG (LOG_LEVEL=warn is the prod floor —
// no WARN noise). The counters are the always-on "did the gate fire?" falsifier:
// a test asserts a delta without scraping logs, and an operator can watch the
// decline rate over /debug/vars.
//
// WHY IN internal/cache (not next to either call site): the restactions Put
// sites live in internal/handlers/dispatchers and the raFullList bypass lives in
// internal/resolvers/widgets/apiref. internal/cache is the package BOTH already
// import, exactly as ExternalSkippedPut / BumpExternalSkippedPut is shared
// across the dispatcher and the apiref serve path.

package cache

import (
	"expvar"
	"sync"
	"sync/atomic"
)

var (
	// restactionsUAFPutDeclinedTotal counts restactions-class L1 Puts declined
	// because the RESTAction declares a userAccessFilter stage. Bumped at ALL
	// THREE Put sites through the ONE shared helper (dispatchers.declineUAFPut):
	// the customer dispatch Put, the refresher re-Put, and the boot seed.
	restactionsUAFPutDeclinedTotal atomic.Uint64
	// raFullListUAFBypassTotal counts raFullList serve-path entries bypassed
	// because the apiRef'd RESTAction declares a userAccessFilter stage — the
	// apiRef path then resolves per request instead of serving (or populating)
	// the per-binding shared cell.
	raFullListUAFBypassTotal atomic.Uint64
)

// BumpRestactionsUAFPutDeclined increments the restactions UAF Put-decline
// counter. Called only from the shared decline helper, never inline at a site.
func BumpRestactionsUAFPutDeclined() { restactionsUAFPutDeclinedTotal.Add(1) }

// RestactionsUAFPutDeclined returns the process-wide restactions UAF
// Put-decline count.
func RestactionsUAFPutDeclined() uint64 { return restactionsUAFPutDeclinedTotal.Load() }

// BumpRAFullListUAFBypass increments the raFullList UAF bypass counter.
func BumpRAFullListUAFBypass() { raFullListUAFBypassTotal.Add(1) }

// RAFullListUAFBypass returns the process-wide raFullList UAF bypass count.
func RAFullListUAFBypass() uint64 { return raFullListUAFBypassTotal.Load() }

// ResetUAFPutDeclineCountersForTest zeroes both counters so a falsifier can
// assert an EXACT delta regardless of what earlier arms in the same test binary
// did. Production callers MUST NOT use this.
func ResetUAFPutDeclineCountersForTest() {
	restactionsUAFPutDeclinedTotal.Store(0)
	raFullListUAFBypassTotal.Store(0)
}

// uafPutDeclineMetricsOnce guards expvar.Publish against the duplicate-key
// panic (mirrors bindingsByGVRMetricsOnce).
var uafPutDeclineMetricsOnce sync.Once

func init() {
	// CFG-1 mirror: under CACHE_ENABLED=false there is no L1 Put to decline,
	// so these keys MUST NOT be registered (transparent-fallback contract).
	if Disabled() {
		return
	}
	registerUAFPutDeclineMetrics()
}

// registerUAFPutDeclineMetrics publishes the two A-1 decline counters. Guarded
// by sync.Once so it is safe from both init() and the test helper.
func registerUAFPutDeclineMetrics() {
	uafPutDeclineMetricsOnce.Do(func() {
		expvar.Publish("snowplow_restactions_uaf_put_declined_total", expvar.Func(func() any {
			return RestactionsUAFPutDeclined()
		}))
		expvar.Publish("snowplow_ra_full_list_uaf_bypass_total", expvar.Func(func() any {
			return RAFullListUAFBypass()
		}))
	})
}

// RegisterUAFPutDeclineMetricsForTest forces registration under tests that flip
// CACHE_ENABLED=true via t.Setenv after init() already ran with the var unset.
// Idempotent. Production callers MUST NOT use this.
func RegisterUAFPutDeclineMetricsForTest() {
	registerUAFPutDeclineMetrics()
}
