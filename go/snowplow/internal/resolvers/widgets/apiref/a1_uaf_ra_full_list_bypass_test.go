// a1_uaf_ra_full_list_bypass_test.go — 1.12.3 A-1 arm (e): the raFullList
// sibling of the restactions Put-decline.
//
// THE DEFECT HERE IS THE SAME SHAPE, WITH FEWER DEFENCES. raFullListServe
// caches the apiRef'd RESTAction's FULL resolve output — userAccessFilter stage
// included — under a cell keyed by seedFullListRAKey, which folds the RA-CR's
// first-match BindingUID ALONE (no RBACSubGen, unlike the restactions key). So
// two co-bound users with divergent per-object narrowings land on ONE cell and
// the second is served the first one's rows as a Go-slice.
//
// THE MITIGATION is a BYPASS rather than a Put-decline, because this path has
// both a serve side and a populate side and they share one entry point: a
// UAF-bearing RA returns served=false before the key is even derived, and
// apiref.Resolve falls through to the page-keyed resolve under the request's own
// identity — the function's ESTABLISHED fall-back contract, the same exit the
// not-sliceable and no-identity cases take.
//
// WHAT THIS ARM PINS, and why each assertion is here:
//   - served=false, so the caller re-resolves per request (the leak is closed);
//   - ZERO resolves are consumed by this path (it bails before the first-sight
//     double resolve) — proving the bypass is early, not a late discard;
//   - NO cell under the derived raKey (the populate side is unreachable);
//   - NO sliceability verdict recorded for the shape — a verdict is keyed by
//     (raKey × shape) and would let a LATER caller take the fast path;
//   - the bypass counter ticks;
//   - and a non-UAF CONTROL still verifies, Puts and serves, so a bypass that
//     had simply disabled the 4a layer wholesale cannot pass.
//
// RED on origin/main: served=true, the cell is Put, the verdict is recorded, and
// the counter does not exist.

package apiref

import (
	"sync/atomic"
	"testing"

	"github.com/krateo-platformops/plumbing/ptr"
	templatesv1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
)

// a1UAFRA is `ra(jq)` plus an api-step declaring a userAccessFilter — the ONLY
// difference between the two arms. Same Filter, so both arms derive the SAME
// sliceShape and the SAME raKey: the bypass cannot be an artefact of the fixture
// keying somewhere else.
func a1UAFRA(jq string) *templatesv1.RESTAction {
	return &templatesv1.RESTAction{Spec: templatesv1.RESTActionSpec{
		Filter: ptr.To(jq),
		API: []*templatesv1.API{
			{Name: "list-panels"},
			{Name: "refilter", UserAccessFilter: &templatesv1.UserAccessFilterSpec{
				Verb: "get", Group: "", Resource: "configmaps", NamespaceFrom: ".metadata.namespace",
			}},
		},
	}}
}

func TestA1_RAFullList_UAFBypassesCell_ControlServes(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	newF6Watcher(t, f6BuildFixture()...)
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)

	// DISTINCT coordinates from the other arms in this package. The sliceability
	// memo is a PROCESS-wide map that ResetResolvedCacheForTest does not clear and
	// this package cannot reach (resetSliceabilityMemoForTest is unexported to
	// internal/cache), so an arm that warms a verdict under the shared
	// "compositions-panels" coordinates leaks a known-sliceable verdict into
	// TestRAServe_VerifyThenHit's first-sight expectation. Own name, own raKey,
	// own shape, no cross-test coupling.
	const ns, name = "krateo-system", "a1-uaf-panels"
	panels := panelDict(40)
	ctx := ctxWithUser(t)

	// The raKey + sliceShape BOTH arms would use — derived by the production
	// single-source helpers, so "no cell / no verdict" is checked at the exact
	// coordinates the serve path would have written.
	_, raKey, ok := seedFullListRAKey(ctx, gvr(), ns, name, nil)
	if !ok || raKey == "" {
		t.Fatalf("PRECONDITION: expected a live raKey (non-empty first-match BindingUID) — without it raFullListServe "+
			"declines for the unrelated #95 reason and this arm would pass vacuously; ok=%v key=%q", ok, raKey)
	}
	shape := seedFullListShape(gvr(), ns, name, a1UAFRA(raSliceJQ))
	if shape != seedFullListShape(gvr(), ns, name, ra(raSliceJQ)) {
		t.Fatalf("PRECONDITION: the UAF and control fixtures must derive the SAME sliceShape (they share a Filter), " +
			"else the arms are not comparable")
	}

	// --- UAF arm: bypassed before anything is derived, resolved or stored. ---
	var uafCalls atomic.Int64
	got, served, err := raFullListServe(ctx, gvr(), ns, name, a1UAFRA(raSliceJQ),
		10, 1, nil, stubResolveRA(t, panels, &uafCalls))
	if err != nil {
		t.Fatalf("arm (e): the bypass must be a clean fall-back, not an error; got %v", err)
	}
	if served {
		t.Fatalf("arm (e) RED (A-1 LEAK): raFullListServe SERVED a userAccessFilter-bearing RESTAction from the "+
			"raFullList cell. That cell folds the RA-CR's BindingUID ALONE, so a co-bound user with a different "+
			"per-object narrowing is served these rows. It must return served=false so apiref.Resolve re-resolves "+
			"under the requester's own identity. got=%v", got)
	}
	if n := uafCalls.Load(); n != 0 {
		t.Fatalf("arm (e): the bypass must fire BEFORE the first-sight resolves (its output could never be stored, "+
			"so paying for it is waste); the resolve closure ran %d time(s)", n)
	}
	if entry, hit := cache.ResolvedCache().Get(raKey); hit {
		t.Fatalf("arm (e) RED: a raFullList cell was populated for a UAF-bearing RA under %q: %q", raKey, entry.RawJSON)
	}
	if _, known := cache.SliceabilityLookup(raKey, shape); known {
		t.Fatalf("arm (e): the bypass must record NO sliceability verdict — a recorded (raKey × shape) verdict would " +
			"send a LATER caller down the fast path into the very cell this bypass exists to avoid")
	}
	if n := cache.RAFullListUAFBypass(); n != 1 {
		t.Fatalf("arm (e): snowplow_ra_full_list_uaf_bypass_total must tick once per bypass; got %d", n)
	}

	// --- CONTROL arm: the same coordinates and shape WITHOUT the UAF stanza
	// still verify, Put and serve. ------------------------------------------
	var ctlCalls atomic.Int64
	resolve := stubResolveRA(t, panels, &ctlCalls)
	gotCtl, served, err := raFullListServe(ctx, gvr(), ns, name, ra(raSliceJQ), 10, 1, nil, resolve)
	if err != nil || !served {
		t.Fatalf("arm (e) CONTROL BROKE: a NON-UAF RA must still be served by the 4a path (the bypass is over-broad "+
			"and has disabled the layer wholesale, making the arm above vacuous); served=%v err=%v", served, err)
	}
	ref, _ := resolve(ctx, 10, 1)
	assertCanonEqual(t, gotCtl, ref, "control page1")
	if _, hit := cache.ResolvedCache().Get(raKey); !hit {
		t.Fatalf("arm (e) control: a verified-sliceable NON-UAF RA must POPULATE the raFullList cell under %q", raKey)
	}
	if n := cache.RAFullListUAFBypass(); n != 1 {
		t.Fatalf("arm (e) control: a NON-UAF RA must not tick the bypass counter; total went to %d", n)
	}
}

// TestA1_RAFullList_BypassSurvivesAWarmCell is the ordering arm. The bypass sits
// FIRST in raFullListServe, before the key derivation and the sliceability
// lookup — so it holds even when a warm, known-sliceable cell already exists
// under the RA's key (a cell a pre-1.12.3 process image, or a non-UAF sibling
// resolve, could have left behind). A bypass placed AFTER the fast-path lookup
// would serve that cell and the leak would survive the mitigation.
func TestA1_RAFullList_BypassSurvivesAWarmCell(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	newF6Watcher(t, f6BuildFixture()...)
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)

	// DISTINCT coordinates from the other arms in this package. The sliceability
	// memo is a PROCESS-wide map that ResetResolvedCacheForTest does not clear and
	// this package cannot reach (resetSliceabilityMemoForTest is unexported to
	// internal/cache), so an arm that warms a verdict under the shared
	// "compositions-panels" coordinates leaks a known-sliceable verdict into
	// TestRAServe_VerifyThenHit's first-sight expectation. Own name, own raKey,
	// own shape, no cross-test coupling.
	const ns, name = "krateo-system", "a1-uaf-panels"
	ctx := ctxWithUser(t)
	panels := panelDict(40)

	// Warm the cell + the known-sliceable verdict via the REAL non-UAF path.
	var calls atomic.Int64
	if _, served, err := raFullListServe(ctx, gvr(), ns, name, ra(raSliceJQ),
		10, 1, nil, stubResolveRA(t, panels, &calls)); err != nil || !served {
		t.Fatalf("setup: the non-UAF warm-up must serve; served=%v err=%v", served, err)
	}
	_, raKey, _ := seedFullListRAKey(ctx, gvr(), ns, name, nil)
	if _, hit := cache.ResolvedCache().Get(raKey); !hit {
		t.Fatalf("setup: expected a warm raFullList cell under %q", raKey)
	}
	if sliceable, known := cache.SliceabilityLookup(raKey, seedFullListShape(gvr(), ns, name, ra(raSliceJQ))); !known || !sliceable {
		t.Fatalf("setup: expected a known-SLICEABLE verdict (the fast path a mis-ordered bypass would fall into); known=%v sliceable=%v", known, sliceable)
	}

	// Now the SAME coordinates with a UAF-bearing RA: the warm cell must not be
	// served to it.
	var uafCalls atomic.Int64
	got, served, err := raFullListServe(ctx, gvr(), ns, name, a1UAFRA(raSliceJQ),
		10, 1, nil, stubResolveRA(t, panels, &uafCalls))
	if err != nil {
		t.Fatalf("bypass must be a clean fall-back; got %v", err)
	}
	if served {
		t.Fatalf("A-1 ORDERING RED: a UAF-bearing RA was served from a WARM raFullList cell — the bypass must sit "+
			"BEFORE the sliceability fast path, not after it. got=%v", got)
	}
	if n := cache.RAFullListUAFBypass(); n != 1 {
		t.Fatalf("the warm-cell bypass must still tick the counter once; got %d", n)
	}
	// The pre-existing non-UAF cell is untouched — the bypass declines to serve,
	// it does not evict a cell other callers legitimately use.
	if _, hit := cache.ResolvedCache().Get(raKey); !hit {
		t.Fatalf("the bypass must not evict the existing cell; it only declines to serve/populate it")
	}
}
