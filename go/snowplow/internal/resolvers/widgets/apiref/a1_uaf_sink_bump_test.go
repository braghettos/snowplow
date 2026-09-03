// a1_uaf_sink_bump_test.go — R-1: the PRODUCTION BUMP at the apiRef chokepoint.
//
// The widgets/widgetContent Put sites decline on cache.UAFTouchedSink, but they
// can never detect a userAccessFilter themselves: a widget CR declares none, and
// the RESTAction that does sits several resolver frames below them. Something in
// between has to mark the resolve, and apiref.Resolve is the natural place — it
// is THE chokepoint every widget→RESTAction read funnels through, and the first
// frame that holds the typed RA.
//
// Without this bump the entire R-1 gate is INERT in production: every widgets
// Put site would read Count()==0 and cache the narrowed body exactly as before.
// So this arm exists specifically to stop that failure mode
// (feedback_falsifier_must_actually_run_under_gate_tag_env — a gate that is
// never driven does not guard anything, and a green suite would not say so).
//
// SCOPE NOTE. A second, complementary bump belongs at the top of the live
// refilter entry point (applyUserAccessFilterOnPig,
// internal/resolvers/restactions/api/refilter.go), is owned by another dev, and
// IS NOT ON THIS BRANCH — it lands on fix/1.12.3-authz-hardening and is a hard
// tag condition. The two are not duplicates:
//   - THIS one is DECLARATION-based and fires whenever an apiRef'd RA declares a
//     UAF stage, including when the refilter then narrows nothing (an empty
//     result is still requester-dependent);
//   - THAT one is EXECUTION-based and fires wherever a refilter actually runs,
//     including RA→RA chains that never pass through apiref.
// Double-bumping is harmless — every gate reads Count()>0, never an exact count.

package apiref

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/krateo-platformops/plumbing/ptr"
	templatesv1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
)

// TestA1_ApirefBumpsUAFSink_ForDeclaringRAOnly drives the REAL predicate-to-bump
// wiring apiref.Resolve calls, against a REAL sink on ctx.
//
// RED on origin/main: bumpUAFSinkIfDeclared does not exist there, and nothing at
// the apiRef chokepoint marks the resolve — so the widgets Put-gate never fires
// and R-1 stays open.
func TestA1_ApirefBumpsUAFSink_ForDeclaringRAOnly(t *testing.T) {
	uafRA := &templatesv1.RESTAction{Spec: templatesv1.RESTActionSpec{
		Filter: ptr.To(raSliceJQ),
		API: []*templatesv1.API{
			{Name: "list"},
			{Name: "refilter", UserAccessFilter: &templatesv1.UserAccessFilterSpec{
				Verb: "get", Group: "", Resource: "configmaps", NamespaceFrom: ".metadata.namespace",
			}},
		},
	}}
	plainRA := &templatesv1.RESTAction{Spec: templatesv1.RESTActionSpec{
		Filter: ptr.To(raSliceJQ),
		API:    []*templatesv1.API{{Name: "list"}},
	}}

	// --- A UAF-declaring apiRef'd RA marks the enclosing resolve. -----------
	ctx, sink := cache.WithUAFTouchedSink(context.Background())
	bumpUAFSinkIfDeclared(ctx, uafRA)
	if sink.Count() != 1 {
		t.Fatalf("R-1 RED: the apiRef chokepoint did NOT mark the resolve for a userAccessFilter-declaring RESTAction "+
			"(sink count=%d, want 1). Without this bump every widgets/widgetContent Put site reads Count()==0 and "+
			"caches the per-requester-narrowed body under the shared per-binding key — the whole R-1 gate is inert.",
			sink.Count())
	}

	// --- A plain RA does NOT, so the gate stays off for every non-UAF widget. ---
	ctx2, sink2 := cache.WithUAFTouchedSink(context.Background())
	bumpUAFSinkIfDeclared(ctx2, plainRA)
	if sink2.Count() != 0 {
		t.Fatalf("a RESTAction with no userAccessFilter must NOT mark the resolve — otherwise every widget declines its "+
			"Put and the widgets cache is disabled wholesale; got count=%d", sink2.Count())
	}

	// --- Nil-safe on both axes: no sink on ctx, and a nil RA. ---------------
	bumpUAFSinkIfDeclared(context.Background(), uafRA) // no sink installed: must not panic
	ctx3, sink3 := cache.WithUAFTouchedSink(context.Background())
	bumpUAFSinkIfDeclared(ctx3, nil)
	if sink3.Count() != 0 {
		t.Fatalf("a nil RESTAction must not mark the resolve; got %d", sink3.Count())
	}
}

// TestA1_ApirefResolveCallsTheBump pins the WIRING: Resolve must actually call
// the bump. The behavioural arm above proves the helper is correct; this proves
// production reaches it. Driving apiref.Resolve end-to-end would need objects.Get
// and a live RA fetch (there is no seam for it in this package), so the wiring is
// asserted at the source — the same posture the repo uses for its other
// "the site is wired" guards.
func TestA1_ApirefResolveCallsTheBump(t *testing.T) {
	raw, err := os.ReadFile("resolve.go")
	if err != nil {
		t.Fatalf("read resolve.go: %v", err)
	}
	if !strings.Contains(string(raw), "bumpUAFSinkIfDeclared(ctx, &ra)") {
		t.Fatal("R-1 wiring: apiref.Resolve must call bumpUAFSinkIfDeclared(ctx, &ra) right after it converts the " +
			"apiRef'd RESTAction. This is the ONLY frame between the widget (which cannot see the userAccessFilter " +
			"declaration) and the refilter (which is in another package, owned by another dev) — drop it and every " +
			"widgets-class Put-gate reads an empty sink and caches the narrowed body.")
	}
}

// TestM1_DeclarationLimbIsBlindToNestedUAFChild is the REAL-BOUNDARY residue of
// what an earlier, WRONG version of this arm tried to prove.
//
// THE MISTAKE, recorded because it is the more useful half. I first wrote this
// as TestM1_NestedUAFChild_NoCellPut_RequiresRefilterBump in the dispatchers
// package: a widget dispatch whose resolver seam called the real declaration
// bump for the parent and then, behind an `if simulateRefilterBump` flag, called
// cache.BumpUAFTouched to stand in for the child's refilter. It was committed
// deliberately RED with a message promising it would go GREEN once the refilter
// bump merged.
//
// It did not. dev-authz-hardening landed the bump and reproduced the arm
// unchanged on the assembled tree. The reason is plain in hindsight: the seam
// REPLACES widgets.Resolve, so the real widgets -> apiref -> RA-B ->
// applyUserAccessFilterOnPig chain never ran, refilter.go was never reached, and
// neither arm could be sensitive to whether the production bump existed. Both
// were simulations of the two frames; the "control" was green before the bump
// was written. That is exactly
// feedback_falsifier_must_drive_real_boundary_not_install_crossed_state — I
// hand-installed the crossed-over state and asserted on my own simulation.
// Worse, it was wired into the tag gate, so it would have blocked a correct
// release on a false signal.
//
// WHAT IS ACTUALLY TESTABLE HERE, and is real. The claim that belongs to THIS
// code is one half of the story: the declaration limb is STRUCTURALLY BLIND to a
// nested chain, because it inspects the RA the apiRef names and that RA declares
// nothing. That needs no simulation — drive the production helper with a real
// parent RA and observe no bump.
//
// THE OTHER HALF — that a refilter actually running DOES mark the resolve — is a
// producer question about refilter.go, and it is covered where refilter.go lives:
// TestA4_RefilterBumpsUAFTouchedSink
// (internal/resolvers/restactions/api/refilter_verb_bound_falsifier_test.go),
// which drives jsonHandlerBytes -> jsonHandlerCore -> applyUserAccessFilterOnPig
// with a live sink on ctx. That is the real boundary, and it is the arm the tag
// gate should name.
//
// THE CHAIN IS COVERED COMPOSITIONALLY, each link by a real arm:
//  1. a refilter run marks the ctx            -> TestA4_RefilterBumpsUAFTouchedSink
//  2. the mark rides the resolve ctx the gate reads
//     -> TestR1_SeedOneWidget_ResolveCtxCarriesTheGatesSink
//     and TestA1_RefilterIdentityEqualsRequestIdentity_*
//  3. a marked resolve declines its Put       -> TestR2_UAFCrossUser_WidgetNoSharedCellServe
//     (real dispatch, asserts on bob's served bytes)
//
// No single hermetic arm spans all three from this repo's test surface, and
// pretending otherwise is what produced the deleted test.
func TestM1_DeclarationLimbIsBlindToNestedUAFChild(t *testing.T) {
	// RA-A: the apiRef'd parent. Declares NO userAccessFilter; one of its steps
	// dispatches at RA-B.
	parent := &templatesv1.RESTAction{Spec: templatesv1.RESTActionSpec{
		API: []*templatesv1.API{
			{Name: "seed"},
			{Name: "children", EndpointRef: &templatesv1.Reference{Name: "krateo-snowplow", Namespace: "krateo-system"}},
		},
	}}
	// RA-B: the nested child. DOES declare one, so the resolved rows are narrowed.
	child := &templatesv1.RESTAction{Spec: templatesv1.RESTActionSpec{
		API: []*templatesv1.API{
			{Name: "list"},
			{Name: "refilter", UserAccessFilter: &templatesv1.UserAccessFilterSpec{
				Verb: "get", Group: "", Resource: "configmaps", NamespaceFrom: ".metadata.name",
			}},
		},
	}}

	// PRECONDITIONS: the fixture must really be the blind spot.
	if parent.HasUserAccessFilterStage() {
		t.Fatal("PRECONDITION: the parent must declare NO userAccessFilter — a parent that declares one is caught by " +
			"the declaration bump and says nothing about the nested case")
	}
	if !child.HasUserAccessFilterStage() {
		t.Fatal("PRECONDITION: the child must declare one — otherwise no refilter runs anywhere in the chain")
	}

	// THE ASSERTION, through the REAL production helper: apiref holds the PARENT,
	// so nothing marks the resolve.
	ctx, sink := cache.WithUAFTouchedSink(context.Background())
	bumpUAFSinkIfDeclared(ctx, parent)
	if got := sink.Count(); got != 0 {
		t.Fatalf("the declaration limb fired for a parent RA that declares no userAccessFilter (count=%d). If this "+
			"ever becomes non-zero the blindness argument no longer holds and the reasoning about why the refilter "+
			"bump is required needs revisiting — not a bug on its own, but a premise change.", got)
	}

	// And the control that makes the blindness meaningful rather than trivial:
	// the SAME helper DOES fire for the child, so the limb works — it is the
	// frame it is applied at that cannot see the chain, not the predicate.
	ctxChild, childSink := cache.WithUAFTouchedSink(context.Background())
	bumpUAFSinkIfDeclared(ctxChild, child)
	if got := childSink.Count(); got != 1 {
		t.Fatalf("the declaration limb must fire for the child RA, which does declare a userAccessFilter (count=%d, "+
			"want 1) — otherwise the negative above is vacuous", got)
	}
}
