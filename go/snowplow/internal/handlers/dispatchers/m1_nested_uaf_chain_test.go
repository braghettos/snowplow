// m1_nested_uaf_chain_test.go — THE DECISIVE ARM for why the UAF gate needs TWO
// bump sites, and the ONE test in this package that is EXPECTED TO FAIL on this
// branch alone.
//
// ============================ READ THIS FIRST ============================
// TestM1_NestedUAFChild_NoCellPut_RequiresRefilterBump is RED on
// fix/1.12.3-uaf-put-decline and GREEN on the assembled 1.12.3 tree. That is
// deliberate, agreed with the team lead, and it is the POINT of the test.
//
// This branch carries only the DECLARATION-based bump (apiref.Resolve fires when
// the apiRef'd RESTAction itself declares a userAccessFilter). The
// EXECUTION-based bump — cache.BumpUAFTouched at the top of the live refilter
// entry point, applyUserAccessFilterOnPig in
// internal/resolvers/restactions/api/refilter.go — lands in the sibling branch
// fix/1.12.3-authz-hardening and is a HARD TAG CONDITION for 1.12.3. The lead
// merges this branch LAST, after that one, so the assembly is green.
//
// If you are reading this because the test failed: check whether
// refilter.go calls cache.BumpUAFTouched. If it does not, the merge order was
// wrong or the authz work regressed — do NOT "fix" this test by weakening it,
// and do NOT tag without the refilter bump. If it does, and this still fails,
// something real is broken.
// =========================================================================
//
// THE SHAPE IT PROVES. A widget apiRefs RA-A. RA-A declares NO userAccessFilter,
// but one of its api-steps consumes RA-B, which does. The refiltered rows flow
// B → A → widget.widgetData → the widgets per-binding cell.
//
// Nothing in the declaration limb can see this:
//   - the WIDGET declares no UAF;
//   - RA-A, the apiRef'd RA that apiref.Resolve holds and inspects, declares no
//     UAF either — so the bump on this branch does not fire;
//   - RA-B declares it, but it is reached through RA-A's inner call, several
//     frames below the only place this branch looks.
// So the sink stays at 0, the gate reads "no refilter", and the narrowed body is
// cached under the shared per-binding key. Only a bump AT THE REFILTER catches it.
//
// WHY IT IS NOT CLOSED BY ACCIDENT TODAY. Both reviewers noted the live corpus
// happens to contain no such chain: 0 of 49 RAs consume the restactions
// endpoint. That is a property of today's corpus, not of the code — one
// customer RA away from being false, with no admission rule preventing it. A
// mitigation that depends on nobody having written a particular CR yet is not a
// mitigation, which is why the refilter bump is a tag condition rather than a
// nice-to-have.
//
// The second arm below is the same chain WITH the refilter bump simulated; it
// passes on this branch and documents exactly what the authz commit buys.

package dispatchers

import (
	"context"
	"net/http/httptest"
	"testing"

	templatesv1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets/apiref"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	m1ParentRA = "ra-a-parent-no-uaf" // apiRef'd by the widget; declares NO UAF
	m1ChildRA  = "ra-b-child-uaf"     // consumed by RA-A's inner step; declares UAF
)

// m1ParentRestAction is RA-A: a plain RESTAction whose second api-step dispatches
// at RA-B (the restactions endpoint). It declares no userAccessFilter of its own,
// so HasUserAccessFilterStage is FALSE for it — which is precisely why the
// declaration limb is blind to the chain.
func m1ParentRestAction() *templatesv1.RESTAction {
	return &templatesv1.RESTAction{
		Spec: templatesv1.RESTActionSpec{
			API: []*templatesv1.API{
				{Name: "seed"},
				// The nested hop: this step resolves RA-B.
				{Name: "children", EndpointRef: &templatesv1.Reference{Name: "krateo-snowplow", Namespace: h1NS}},
			},
		},
	}
}

// m1ChildRestAction is RA-B: it DOES declare a userAccessFilter, so its resolve
// narrows per requester.
func m1ChildRestAction() *templatesv1.RESTAction {
	return &templatesv1.RESTAction{
		Spec: templatesv1.RESTActionSpec{
			API: []*templatesv1.API{
				{Name: "list"},
				{Name: "refilter", UserAccessFilter: &templatesv1.UserAccessFilterSpec{
					Verb: "get", Group: "", Resource: "configmaps", NamespaceFrom: ".metadata.name",
				}},
			},
		},
	}
}

// m1PreconditionChainShape asserts the fixture really is the blind spot: the
// PARENT declares no UAF (so the declaration limb cannot fire) while the CHILD
// does (so the body genuinely is narrowed). Without both, the arm proves nothing.
func m1PreconditionChainShape(t *testing.T) {
	t.Helper()
	if m1ParentRestAction().HasUserAccessFilterStage() {
		t.Fatal("PRECONDITION: RA-A must declare NO userAccessFilter — the whole point is that the declaration limb " +
			"is blind to it. A parent that declares one would be caught by the apiref bump and the arm would be vacuous.")
	}
	if !m1ChildRestAction().HasUserAccessFilterStage() {
		t.Fatal("PRECONDITION: RA-B must declare a userAccessFilter — otherwise no refilter runs anywhere in the " +
			"chain and there is nothing to leak.")
	}
}

// m1DriveWidgetOverChain dispatches the widget whose apiRef is RA-A and returns
// the derived widgets key + handle. simulateRefilterBump models whether the
// EXECUTION-based bump exists: false = this branch (declaration bump only),
// true = the assembled tree once refilter.go calls cache.BumpUAFTouched.
func m1DriveWidgetOverChain(t *testing.T, simulateRefilterBump bool) (string, cacheHandle) {
	t.Helper()

	reqCtx := a1UserCtx(a1Alice)
	cr := h1WidgetUnstructured(map[string]any{
		"apiRef":             map[string]any{"name": m1ParentRA, "namespace": h1NS},
		"widgetDataTemplate": []any{map[string]any{"forPath": ".rows", "expression": "${ .rows }"}},
	})
	key, handle, inputs := dispatchCacheLookupKey(reqCtx, "widgets",
		h1WidgetGVR.Group, h1WidgetGVR.Version, h1WidgetGVR.Resource,
		h1NS, h1WName, -1, -1, effectiveKeyExtras(reqCtx, cr.Object, nil))
	if handle == nil || key == "" || inputs == nil || inputs.BindingUID == "" {
		t.Fatalf("PRECONDITION: expected a live, non-empty-BindingUID widgets key; key=%q inputs=%+v", key, inputs)
	}

	restore := installWidgetFakes(t, cr, func() bool { return true },
		func(ctx context.Context, _ widgets.ResolveOptions) (*widgets.Widget, error) {
			// Model the production chain frame by frame.
			//
			// FRAME 1 — apiref.Resolve holds RA-A and applies the declaration
			// bump. This is the REAL production helper, called with the REAL
			// parent RA, so its verdict is not scripted: it fires iff RA-A
			// declares a UAF stage, which it does not.
			apiref.BumpUAFSinkIfDeclaredForTest(ctx, m1ParentRestAction())

			// FRAME 2 — RA-A's inner step resolves RA-B, whose refilter runs and
			// narrows the rows. On this branch NOTHING marks the ctx here; with
			// the authz branch's refilter bump, this is where it happens.
			if simulateRefilterBump {
				cache.BumpUAFTouched(ctx)
			}

			out := h1WidgetUnstructured(map[string]any{})
			if err := unstructured.SetNestedSlice(out.Object,
				[]any{"tenant-a-row-visible-only-to-alice"}, "status", "widgetData", "rows"); err != nil {
				t.Fatalf("seam: %v", err)
			}
			return out, nil
		})
	defer restore()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/call", nil).WithContext(reqCtx)
	Widgets().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("widget dispatch must serve 200; got %d body=%s", rec.Code, rec.Body.String())
	}
	return key, handle
}

// TestM1_NestedUAFChild_NoCellPut_RequiresRefilterBump — EXPECTED RED ON THIS
// BRANCH. See the file header before touching it.
func TestM1_NestedUAFChild_NoCellPut_RequiresRefilterBump(t *testing.T) {
	a1BuildTwoTenantWatcher(t)
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)
	m1PreconditionChainShape(t)

	// simulateRefilterBump=false — this branch exactly: declaration bump only.
	key, handle := m1DriveWidgetOverChain(t, false)

	if entry, ok := handle.Get(key); ok {
		t.Fatalf("M1 (EXPECTED RED ON fix/1.12.3-uaf-put-decline; GREEN once fix/1.12.3-authz-hardening merges):\n"+
			"  A widget whose apiRef'd RESTAction %q declares NO userAccessFilter, but whose inner step consumes %q "+
			"which DOES, was CACHED under the shared per-binding widgets key %q.\n"+
			"  Body: %q\n"+
			"  The rows in that cell were narrowed for THIS requester; a co-bound user keys onto the same cell and is "+
			"served them.\n"+
			"  WHY THIS BRANCH CANNOT CATCH IT: the only bump here is declaration-based, in apiref.Resolve, and it "+
			"inspects RA-A — which declares nothing. RA-B's declaration is several resolver frames below the deepest "+
			"frame this branch looks at. Only a bump AT THE REFILTER (cache.BumpUAFTouched in "+
			"applyUserAccessFilterOnPig, internal/resolvers/restactions/api/refilter.go, sibling branch "+
			"fix/1.12.3-authz-hardening) marks this chain.\n"+
			"  DO NOT weaken this test, and DO NOT tag 1.12.3 without that refilter bump: without it the release "+
			"note \"UAF-touched resolves are no longer cached in any class\" is false, and the chain is closed only "+
			"by the accident that 0 of 49 live RAs currently consume the restactions endpoint.",
			m1ParentRA, m1ChildRA, key, entry.RawJSON)
	}

	if got := cache.WidgetsUAFPutDeclined(); got != 1 {
		t.Fatalf("M1: the nested-chain decline must route through the widgets-class gate; counter=%d want 1", got)
	}
}

// TestM1_NestedUAFChild_ClosedByRefilterBump is the same chain WITH the
// execution-based bump present. It passes on this branch and is the control that
// makes the RED above meaningful: it shows the leak is closed by the refilter
// bump specifically, and not by anything else in the fix.
//
// Together the two arms are the proof that the declaration limb and the sink
// limb are NOT redundant — each catches a case the other cannot.
func TestM1_NestedUAFChild_ClosedByRefilterBump(t *testing.T) {
	a1BuildTwoTenantWatcher(t)
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)
	m1PreconditionChainShape(t)

	// simulateRefilterBump=true — the assembled tree.
	key, handle := m1DriveWidgetOverChain(t, true)

	if entry, ok := handle.Get(key); ok {
		t.Fatalf("M1 control: WITH the refilter bump the nested-UAF chain must NOT be cached; found %q under %q. "+
			"If this fails, the sink limb of the gate is broken — which would mean the widgets carrier (R-1) is "+
			"open too, not just the nested chain.", entry.RawJSON, key)
	}
	if got := cache.WidgetsUAFPutDeclined(); got != 1 {
		t.Fatalf("M1 control: the decline must tick the widgets-class counter exactly once; got %d", got)
	}
}
