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
