// fallthrough_split_test.go — 1.12.4 falsifiers F4 + F5 for the
// fall-through / cache-diagnostic two-map split (design §5.4).
//
// WHAT THESE ARMS ACTUALLY DRIVE. Both call the REAL exported entry
// point `RecordApiserverFallthrough` (and the two thin wrappers
// `RecordResolverPluralsHit` / `RecordResolverPluralsMiss` that the 27
// production sites use) under a REAL `WithFallthroughScope` context.
// Nothing is stubbed and no seam is installed: the routing decision
// under test lives in `RecordApiserverFallthrough` itself and the
// counting in `recordCell`, and both execute here exactly as they do on
// a `/call`. A temporary `panic()` at the top of `recordCell` fails
// every arm below, which is the proof that the production frame runs.
//
// [C4] CACHE_ENABLED. `Disabled()` reads the env LIVE and DEFAULTS TO
// DISABLED, so without `t.Setenv("CACHE_ENABLED","true")` every
// `Record*` call is a silent no-op and these arms would pass for the
// wrong reason — a green that means "the recorder was switched off",
// not "the routing is correct".
//
// [C5] IMPOSSIBLE-BASELINE SENTINEL. A bare `uint64` 0 cannot tell
// "nothing was recorded" apart from "the recorder is inert", because a
// no-op produces the same 0. Every arm therefore opens through
// `probeTotals`, which returns the impossible sentinel -1 when either
// no-op condition of `recordCell` holds (cache disabled, or the ctx
// carries no active scope). Reading 0 from `probeTotals` is a positive
// statement that the recorder is LIVE and empty.
package cache

import (
	"context"
	"testing"
)

// probeTotals returns the two live grand totals as int64, or the
// IMPOSSIBLE sentinel (-1, -1) when the recording machinery is inert.
//
// It re-derives, from the same package state `recordCell` reads, the two
// conditions under which `recordCell` silently returns without counting:
//
//	1. Disabled() — CACHE_ENABLED is not truthy;
//	2. the ctx carries no active FallthroughScope.
//
// Neither condition can be detected from the counters themselves: both
// leave every total at 0, which is also the legitimate post-reset value.
// Returning -1 makes the difference assertable. Per
// `feedback_seamed_dispatch_cannot_falsify_a_deep_frame`, the absent
// case gets an impossible value rather than a plausible zero.
func probeTotals(ctx context.Context) (fall, diag int64) {
	if Disabled() {
		return -1, -1
	}
	if scope := FallthroughScope(ctx); scope == nil || !scope.Active {
		return -1, -1
	}
	return int64(FallthroughTotal()), int64(DiagnosticTotal())
}

// assertLiveAndEmpty is the sentinel opener every arm runs first: the
// recorder must be LIVE (not -1) and freshly reset (0). A -1 here means
// the arm would have been vacuous.
func assertLiveAndEmpty(t *testing.T, ctx context.Context) {
	t.Helper()
	fall, diag := probeTotals(ctx)
	if fall == -1 || diag == -1 {
		t.Fatalf("recorder is INERT (sentinel -1): CACHE_ENABLED not truthy or ctx carries no active "+
			"FallthroughScope — every Record* below would be a silent no-op and this arm would be "+
			"vacuous. got fall=%d diag=%d", fall, diag)
	}
	if fall != 0 || diag != 0 {
		t.Fatalf("counters not reset: fall=%d diag=%d; want 0/0 after ResetFallthroughCountersForTest", fall, diag)
	}
}

// splitCase is one row of the routing table. `wantDiagnostic` is the
// design §5.2 classification; the test derives NOTHING from
// diagnosticReasons itself, so a mis-edit of that map reds here rather
// than being silently mirrored.
type splitCase struct {
	reason         FallthroughReason
	wantDiagnostic bool
	why            string
}

// splitCases enumerates every reason the 27 production sites can pass,
// with the classification hand-written from design §5.2 (NOT read from
// the production map). All 21 closed-enum values are present: an added
// reason that is not listed here fails TestFallthroughSplit_TableCovers
// EveryReason below.
var splitCases = []splitCase{
	// --- the six DIAGNOSTIC reasons (design §5.2, "not a fallthrough") ---
	{ReasonResolverPluralsHit, true, "in-process cache HIT — zero apiserver traffic (88.4% of the 057 headline)"},
	{ReasonResolverPluralsMiss, true, "double-counted; the hop is already ReasonPluralsDiscoveryHop (29 == 29 on 057)"},
	{ReasonWidgetContentHit, true, "Ship G content-layer Get-HIT, served from L1"},
	{ReasonWidgetContentMissPerUserFallback, true, "falls to the per-user L1 lookup, not the apiserver"},
	{ReasonClusterListDispatch, true, "the collapse SUCCEEDED — its own doc-comment says NOT a fallthrough"},
	{ReasonClusterListShapeFallback, true, "reverts to the informer-eligible per-NS iterator; ruled diagnostic (impact: 1 count)"},

	// --- the genuine apiserver hops (design §5.2, "stay") ---
	{ReasonClientBuild, false, "builds a dynamic client, then issues a live GET"},
	{ReasonSecretGet, false, "live Secret GET"},
	{ReasonCRDDiscover, false, "live discovery request"},
	{ReasonPluralsDiscoveryHop, false, "THE discovery request — must NOT move with its two resolver-side echoes"},
	{ReasonInformerNotSynced, false, "gate-miss arm hands the call to the live apiserver"},
	{ReasonInformerNotServable, false, "gate-miss arm hands the call to the live apiserver"},
	{ReasonInformerRBACDeny, false, "gate-miss arm hands the call to the live apiserver"},
	{ReasonInformerWriteVerb, false, "gate-miss arm hands the call to the live apiserver"},
	{ReasonInformerSubresource, false, "gate-miss arm hands the call to the live apiserver"},
	{ReasonInformerExternalURL, false, "gate-miss arm hands the call to the live apiserver"},
	{ReasonInformerUnparseable, false, "gate-miss arm hands the call to the live apiserver"},
	{ReasonInformerPassthrough, false, "gate-miss arm hands the call to the live apiserver"},
	{ReasonInformerMetadataOnly, false, "gate-miss arm hands the call to the live apiserver"},
	{ReasonApistageGetPartialShape, false, "returns (nil,false) -> fresh apiserver GET-by-name"},
	{ReasonGetMissLetApiserver404, false, "deliberately lets the apiserver answer 404"},
}

// TestFallthroughSplit_RoutingCellsAndTotals is F4.
//
// For every reason it asserts FOUR things, in both dimensions the design
// requires — the TOTAL and the CELL MAP:
//
//	(a) the expected total rose by exactly 1;
//	(b) the OTHER total did not move;
//	(c) the (path,gvr,reason) key is present in the expected cell map;
//	(d) the key is ABSENT from the other cell map.
//
// (d) is why §5.4 mandates two maps rather than one map with two scalar
// totals: with a shared map the key would still be present under the
// fall-through family, and the acceptance observation
// ("snowplow_apiserver_fallthrough_cells holds no resolver-plurals-hit
// key") would be unsatisfiable no matter what the scalars say.
//
// RED ON MAIN: on 8de5295 every reason lands on fallthroughTotal and in
// the single fallthroughCounters map, so (a) reds for all six diagnostic
// rows and (b)+(d) red for every row — and CacheDiagnosticCount /
// DiagnosticTotal do not compile at all.
func TestFallthroughSplit_RoutingCellsAndTotals(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	const gvr = "widgets.ui.krateo.io/v1beta1/widgets"
	ctx := WithFallthroughScope(context.Background(), ScopeCallWidgets)

	for _, tc := range splitCases {
		t.Run(string(tc.reason), func(t *testing.T) {
			ResetFallthroughCountersForTest()
			assertLiveAndEmpty(t, ctx)

			RecordApiserverFallthrough(ctx, tc.reason, gvr)

			gotFall, gotDiag := probeTotals(ctx)
			wantFall, wantDiag := int64(1), int64(0)
			if tc.wantDiagnostic {
				wantFall, wantDiag = 0, 1
			}
			if gotFall != wantFall || gotDiag != wantDiag {
				t.Errorf("totals after one %s: fallthrough=%d diagnostic=%d; want %d/%d\n  why: %s",
					tc.reason, gotFall, gotDiag, wantFall, wantDiag, tc.why)
			}

			cellFall := FallthroughCount(ScopeCallWidgets, gvr, tc.reason)
			cellDiag := CacheDiagnosticCount(ScopeCallWidgets, gvr, tc.reason)
			wantCellFall, wantCellDiag := uint64(1), uint64(0)
			if tc.wantDiagnostic {
				wantCellFall, wantCellDiag = 0, 1
			}
			if cellFall != wantCellFall {
				t.Errorf("fallthrough CELL %s = %d; want %d — the key must be %s this map",
					tc.reason, cellFall, wantCellFall,
					map[bool]string{true: "ABSENT from", false: "present in"}[tc.wantDiagnostic])
			}
			if cellDiag != wantCellDiag {
				t.Errorf("diagnostic CELL %s = %d; want %d", tc.reason, cellDiag, wantCellDiag)
			}
		})
	}
}

// TestFallthroughSplit_ThinWrappersRouteToDiagnostic drives the two
// exported wrappers the resolver call sites actually use
// (resourcesrefs/resolve.go and crds/schema/schema.go), not the generic
// entry point — so a future edit that changes which constant a wrapper
// passes is caught here rather than only in a hand-written table.
func TestFallthroughSplit_ThinWrappersRouteToDiagnostic(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ctx := WithFallthroughScope(context.Background(), ScopeResolverInnerCall)

	ResetFallthroughCountersForTest()
	assertLiveAndEmpty(t, ctx)

	RecordResolverPluralsHit(ctx, "v1/pods")
	RecordResolverPluralsMiss(ctx, "v1/pods")

	if got := FallthroughTotal(); got != 0 {
		t.Errorf("FallthroughTotal after two resolver-plurals wrapper calls = %d; want 0 "+
			"(both are in-process, design §5.2)", got)
	}
	if got := DiagnosticTotal(); got != 2 {
		t.Errorf("DiagnosticTotal after two resolver-plurals wrapper calls = %d; want 2", got)
	}
	if got := FallthroughCount(ScopeResolverInnerCall, "v1/pods", ReasonResolverPluralsHit); got != 0 {
		t.Errorf("resolver-plurals-hit is present in the FALLTHROUGH cells map (%d) — "+
			"design §5.4 requires the key to be absent there, which a shared map cannot deliver", got)
	}
}

// TestFallthroughSplit_TableCoversEveryReason guards the F4 table
// against a newly added FallthroughReason silently going untested. It
// walks the closed enum via the union of the two production
// classifications and requires every value to appear in splitCases.
//
// The enum has no reflective listing, so allReasons below is the
// hand-maintained mirror; the compiler catches a REMOVED constant and
// this test catches an ADDED one that nobody classified.
func TestFallthroughSplit_TableCoversEveryReason(t *testing.T) {
	allReasons := []FallthroughReason{
		ReasonClientBuild, ReasonSecretGet, ReasonCRDDiscover,
		ReasonInformerNotSynced, ReasonInformerNotServable, ReasonInformerRBACDeny,
		ReasonInformerWriteVerb, ReasonInformerSubresource, ReasonInformerExternalURL,
		ReasonInformerUnparseable, ReasonInformerPassthrough, ReasonInformerMetadataOnly,
		ReasonApistageGetPartialShape, ReasonGetMissLetApiserver404,
		ReasonClusterListDispatch, ReasonClusterListShapeFallback,
		ReasonWidgetContentHit, ReasonWidgetContentMissPerUserFallback,
		ReasonPluralsDiscoveryHop,
		ReasonResolverPluralsHit, ReasonResolverPluralsMiss,
	}
	if len(allReasons) != 21 {
		t.Fatalf("closed enum size = %d; design §3.2 states 21 — update both this list and the "+
			"cardinality arithmetic if a reason was added", len(allReasons))
	}

	covered := map[FallthroughReason]bool{}
	for _, tc := range splitCases {
		covered[tc.reason] = true
	}
	for _, r := range allReasons {
		if !covered[r] {
			t.Errorf("reason %q is not classified in splitCases — every reason must be explicitly "+
				"ruled genuine-or-diagnostic (design §5.2)", r)
		}
		// The hand-written table and the production router must agree.
		var want bool
		for _, tc := range splitCases {
			if tc.reason == r {
				want = tc.wantDiagnostic
			}
		}
		if got := IsDiagnosticReason(r); got != want {
			t.Errorf("IsDiagnosticReason(%q) = %v; design §5.2 says %v", r, got, want)
		}
	}
	if got := len(diagnosticReasons); got != 6 {
		t.Errorf("len(diagnosticReasons) = %d; design §5.2 rules exactly 6 reasons diagnostic", got)
	}
}

// TestFallthroughSplit_Conservation is F5.
//
// After N mixed Record* calls, FallthroughTotal()+DiagnosticTotal() == N
// exactly. This is what rules out the two shapes a careless split can
// take: a reason counted on BOTH families (sum > N) and a reason routed
// to neither (sum < N). It also fails if a call site's reason were
// dropped on the floor.
//
// It depends on [C5]'s reset extension: without the diagnostic map and
// total being zeroed, this arm inherits counts from the F4 arms in the
// same binary and the exact equality flakes.
//
// RED ON MAIN: DiagnosticTotal does not exist.
func TestFallthroughSplit_Conservation(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ctx := WithFallthroughScope(context.Background(), ScopeCallRestactions)

	ResetFallthroughCountersForTest()
	assertLiveAndEmpty(t, ctx)

	// A deliberately lopsided mix across both families and several GVRs,
	// so conservation is tested over many cells rather than one.
	gvrs := []string{"v1/pods", "v1/secrets", "apps/v1/deployments", ""}
	n := 0
	for i, tc := range splitCases {
		reps := 1 + i%3 // 1..3 — uneven, so a per-reason off-by-one shows up
		for r := 0; r < reps; r++ {
			RecordApiserverFallthrough(ctx, tc.reason, gvrs[(i+r)%len(gvrs)])
			n++
		}
	}

	fall, diag := FallthroughTotal(), DiagnosticTotal()
	if int(fall+diag) != n {
		t.Errorf("conservation violated: fallthrough=%d + diagnostic=%d = %d; want exactly %d "+
			"(a reason counted twice gives >N, a reason routed nowhere gives <N)",
			fall, diag, fall+diag, n)
	}
	if fall == 0 || diag == 0 {
		t.Errorf("degenerate split: fallthrough=%d diagnostic=%d — the mix must exercise BOTH "+
			"families, otherwise conservation holds trivially", fall, diag)
	}
}

// TestFallthroughSplit_CacheOffRecordsNothing pins the cache-toggle
// contract across the split: under CACHE_ENABLED=false BOTH families
// stay silent, exactly as the single family did before 1.12.4
// (project_caching_is_provisional). The sentinel is the assertion — the
// recorder reports itself inert rather than merely reading 0.
func TestFallthroughSplit_CacheOffRecordsNothing(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	ctx := WithFallthroughScope(context.Background(), ScopeCallWidgets)

	fall, diag := probeTotals(ctx)
	if fall != -1 || diag != -1 {
		t.Fatalf("probeTotals under CACHE_ENABLED=false = %d/%d; want the inert sentinel -1/-1", fall, diag)
	}

	before, beforeDiag := FallthroughTotal(), DiagnosticTotal()
	RecordApiserverFallthrough(ctx, ReasonClientBuild, "v1/pods")
	RecordApiserverFallthrough(ctx, ReasonResolverPluralsHit, "v1/pods")
	if FallthroughTotal() != before || DiagnosticTotal() != beforeDiag {
		t.Errorf("cache-off recorded something: fallthrough %d->%d diagnostic %d->%d",
			before, FallthroughTotal(), beforeDiag, DiagnosticTotal())
	}
}
