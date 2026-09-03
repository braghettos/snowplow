// a1_uaf_widget_cross_user_test.go — R-2: THE ACCEPTANCE ARM FOR THE HOT CARRIER.
//
// The restactions acceptance arm (a1_uaf_cross_user_isolation_test.go) proved
// the leak closed on the path a /call takes directly against a UAF RESTAction.
// That path is nearly cold. The path the PORTAL renders is the widget:
//
//	widgets.Resolve → resolveApiRef → apiref.Resolve → restactions.Resolve
//
// and the refiltered rows come back folded into the WIDGET's own
// status.widgetData, which is Put under the widgets-class per-BINDING key.
// Live measurement: 66 widgets apiRef a UAF RA, and that cell served 298,064
// hits against 365 misses over 5d7h. The first A-1 cut gated the declaration on
// the RESTAction and went green while this carrier stayed open.
//
// THE LESSON THIS FILE ENCODES: a leak with N carriers needs an acceptance arm
// PER CARRIER. One green arm on one carrier is not evidence about the others —
// it is evidence about the one. So this is the SAME two-user shape and the SAME
// assertion style as the restactions arm, routed through Widgets().ServeHTTP.
//
// WHAT IS REAL AND WHAT IS SEAMED. The dispatch, the key derivation, the cache
// handle, the Put-gate chain and the response bytes are production. The widgets
// resolver is a seam (no apiserver here), and it does two things a real
// widgets.Resolve does through its apiRef chain:
//
//  1. calls cache.BumpUAFTouched(ctx) — the same one-line bump production makes
//     at the apiRef chokepoint (apiref.bumpUAFSinkIfDeclared, which fires when
//     the apiRef'd RA declares a UAF stage) and, once dev-authz-hardening's
//     commit lands, again inside the refilter itself;
//  2. derives the widgetData by calling the REAL rbac.EvaluateRBAC per candidate
//     namespace under the requester's identity — so each user's body is produced
//     by that user's real RBAC, not by a canned fixture.
//
// RED on origin/main: alice's widget body is Put under the shared key, bob HITs
// it, and bob is served alice's tenant. GREEN here: the sink makes the widgets
// Put decline, so bob resolves under his own identity.

package dispatchers

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/rbac"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// a1WidgetUAFResolve is the widgets resolver seam. It stands in for the whole
// widgets.Resolve → apiRef → UAF RESTAction chain: it marks the resolve as
// refilter-narrowed and then narrows for real, per requester, via EvaluateRBAC.
func a1WidgetUAFResolve(t *testing.T, uaf bool) func(context.Context, widgets.ResolveOptions) (*widgets.Widget, error) {
	t.Helper()
	return func(ctx context.Context, _ widgets.ResolveOptions) (*widgets.Widget, error) {
		out := h1WidgetUnstructured(map[string]any{})
		if !uaf {
			// CONTROL: a widget whose apiRef RA declares no userAccessFilter.
			// Identity-invariant body, no bump — must still be cached.
			if err := unstructured.SetNestedField(out.Object, "same-for-everyone", "status", "widgetData", "shared"); err != nil {
				t.Fatalf("control seam: set widgetData: %v", err)
			}
			return out, nil
		}
		// The bump production makes at the apiRef chokepoint for a UAF-declaring
		// apiRef'd RA. This is what the widgets Put-gate reads.
		cache.BumpUAFTouched(ctx)

		ui, err := xcontext.UserInfo(ctx)
		if err != nil {
			t.Fatalf("seam: the handler must pass the requester's identity to the resolver; got %v", err)
		}
		kept := []any{}
		for _, ns := range []string{a1TenantA, a1TenantB} {
			allowed, _, err := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
				Username: ui.Username, Groups: ui.Groups,
				Verb: "get", Group: "", Resource: "configmaps", Namespace: ns,
			})
			if err != nil {
				t.Fatalf("seam: per-object EvaluateRBAC(%s, %s): %v", ui.Username, ns, err)
			}
			if allowed {
				kept = append(kept, ns)
			}
		}
		if err := unstructured.SetNestedSlice(out.Object, kept, "status", "widgetData", "namespaces"); err != nil {
			t.Fatalf("seam: set narrowed widgetData: %v", err)
		}
		return out, nil
	}
}

// TestR2_UAFCrossUser_WidgetNoSharedCellServe is the R-1 carrier's acceptance arm.
func TestR2_UAFCrossUser_WidgetNoSharedCellServe(t *testing.T) {
	a1BuildTwoTenantWatcher(t)
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)

	aliceCtx, bobCtx := a1UserCtx(a1Alice), a1UserCtx(a1Bob)

	// --- PRECONDITION (i): divergent per-object verdicts. -------------------
	verdict := func(user, ns string) bool {
		t.Helper()
		allowed, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
			Username: user, Groups: []string{a1Group},
			Verb: "get", Group: "", Resource: "configmaps", Namespace: ns,
		})
		if err != nil {
			t.Fatalf("PRECONDITION (i): EvaluateRBAC(%s, %s): %v", user, ns, err)
		}
		return allowed
	}
	if !verdict(a1Alice, a1TenantA) || verdict(a1Alice, a1TenantB) ||
		!verdict(a1Bob, a1TenantB) || verdict(a1Bob, a1TenantA) {
		t.Fatalf("PRECONDITION (i) FAILED: alice must see only %s and bob only %s — without divergent per-object "+
			"verdicts the two widget bodies would be identical and this arm could not detect a cross-user serve",
			a1TenantA, a1TenantB)
	}

	// --- PRECONDITION (ii): both users key onto ONE widgets cell. -----------
	// Derived by the PRODUCTION widgets key path (effectiveKeyExtras +
	// dispatchCacheLookupKey), not reconstructed.
	cr := h1WidgetUnstructured(map[string]any{
		"apiRef":             map[string]any{"name": a1RAName2, "namespace": h1NS},
		"widgetDataTemplate": []any{map[string]any{"forPath": ".namespaces", "expression": "${ .namespaces }"}},
	})
	aliceExtras := effectiveKeyExtras(aliceCtx, cr.Object, nil)
	bobExtras := effectiveKeyExtras(bobCtx, cr.Object, nil)
	aliceKey, handle, aliceIn := dispatchCacheLookupKey(aliceCtx, "widgets",
		h1WidgetGVR.Group, h1WidgetGVR.Version, h1WidgetGVR.Resource, h1NS, h1WName, -1, -1, aliceExtras)
	bobKey, _, bobIn := dispatchCacheLookupKey(bobCtx, "widgets",
		h1WidgetGVR.Group, h1WidgetGVR.Version, h1WidgetGVR.Resource, h1NS, h1WName, -1, -1, bobExtras)
	if handle == nil || aliceIn == nil || bobIn == nil {
		t.Fatalf("PRECONDITION (ii): expected a live widgets cache handle and derived inputs")
	}
	if aliceIn.BindingUID == "" || aliceIn.BindingUID != bobIn.BindingUID {
		t.Fatalf("PRECONDITION (ii) FAILED: both users must derive the SAME non-empty widgets BindingUID (that sharing "+
			"IS the defect); alice=%q bob=%q", aliceIn.BindingUID, bobIn.BindingUID)
	}
	if aliceKey != bobKey {
		t.Fatalf("PRECONDITION (ii) FAILED: the two users' PRODUCTION-derived WIDGETS keys must be IDENTICAL — that one "+
			"shared cell is what alice's narrowed widgetData would be written into and bob served from. alice=%q bob=%q",
			aliceKey, bobKey)
	}
	if _, ok := handle.Get(aliceKey); ok {
		t.Fatalf("PRECONDITION: the shared widgets key must be cold before alice's request")
	}

	serve := func(t *testing.T, ctx context.Context, uaf bool) *httptest.ResponseRecorder {
		t.Helper()
		restore := installWidgetFakes(t, cr, func() bool { return true }, a1WidgetUAFResolve(t, uaf))
		defer restore()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/call", nil).WithContext(ctx)
		Widgets().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("widget dispatch must serve 200; got %d body=%s", rec.Code, rec.Body.String())
		}
		return rec
	}

	aliceRec := serve(t, aliceCtx, true)
	if !bytes.Contains(aliceRec.Body.Bytes(), []byte(a1TenantA)) || bytes.Contains(aliceRec.Body.Bytes(), []byte(a1TenantB)) {
		t.Fatalf("SETUP CHECK: alice's own widget body must contain %s and not %s (the narrowing must actually be "+
			"happening, else there is nothing to leak); got %s", a1TenantA, a1TenantB, aliceRec.Body.String())
	}

	bobRec := serve(t, bobCtx, true)
	body := bobRec.Body.Bytes()

	// --- THE ACCEPTANCE ASSERTIONS, on bob's served widget bytes. -----------
	if bytes.Contains(body, []byte(a1TenantA)) {
		t.Fatalf("R-1 CROSS-TENANT LEAK (WIDGET CARRIER): bob's widget response contains %q — a namespace his OWN RBAC "+
			"denies him. He was served alice's userAccessFilter-narrowed widgetData out of the shared widgets cell at "+
			"key %q (both users fold the same BindingUID %q; widgets.Resolve folds the apiRef'd RA's refiltered rows "+
			"into status.widgetData and the hit path serves entry.RawJSON verbatim). THIS IS THE HOT CARRIER — the "+
			"restactions-only gate does not close it. bob's body: %s",
			a1TenantA, aliceKey, aliceIn.BindingUID, bobRec.Body.String())
	}
	if !bytes.Contains(body, []byte(a1TenantB)) {
		t.Fatalf("R-1: bob's widget response is MISSING %q, which his own RBAC permits — he must be served his own "+
			"correctly-narrowed widgetData, not merely denied alice's. body: %s", a1TenantB, bobRec.Body.String())
	}

	// --- The mechanism: the shared widgets cell was never written. ----------
	if entry, ok := handle.Get(aliceKey); ok {
		t.Fatalf("R-1: the shared widgets cell must stay EMPTY when the resolve ran a userAccessFilter refilter; "+
			"found %q under %q", entry.RawJSON, aliceKey)
	}
	if got := cache.WidgetsUAFPutDeclined(); got != 2 {
		t.Fatalf("R-1: both widget dispatches must have declined their Put through the widgets-class gate; "+
			"snowplow_widgets_uaf_put_declined_total=%d, want 2", got)
	}
	if got := cache.RestactionsUAFPutDeclined(); got != 0 {
		t.Fatalf("R-1: a widgets decline must be counted under the WIDGETS counter, not the restactions one; "+
			"restactions counter moved to %d", got)
	}

	// --- CONTROL: an identical widget whose apiRef RA has NO userAccessFilter
	// still caches. Without this, a gate that simply stopped caching widgets
	// wholesale would pass every assertion above. ---------------------------
	cache.ResetUAFPutDeclineCountersForTest()
	ctlRec := serve(t, aliceCtx, false)
	if !bytes.Contains(ctlRec.Body.Bytes(), []byte("same-for-everyone")) {
		t.Fatalf("control: expected the non-UAF widget body; got %s", ctlRec.Body.String())
	}
	entry, ok := handle.Get(aliceKey)
	if !ok {
		t.Fatalf("R-1 CONTROL BROKE: a widget whose resolve ran NO refilter was not Put under %q — the gate is "+
			"over-broad (it has disabled the widgets cache wholesale, which would make every assertion above vacuous)",
			aliceKey)
	}
	if !bytes.Equal(entry.RawJSON, ctlRec.Body.Bytes()) {
		t.Fatalf("control: the Put'd bytes must equal the served bytes; put=%q served=%q", entry.RawJSON, ctlRec.Body.Bytes())
	}
	if got := cache.WidgetsUAFPutDeclined(); got != 0 {
		t.Fatalf("control: a non-refiltered widget must not tick the decline counter; got %d", got)
	}
}
