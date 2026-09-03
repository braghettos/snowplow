// a1_uaf_resolve_identity_agreement_test.go — the identity-agreement arm
// adv-cache-isolation asked for (#57 loopback identity-shift class).
//
// THE GAP THIS CLOSES IN MY OWN FALSIFIERS. The A-1/R-2 acceptance arms narrow by
// calling rbac.EvaluateRBAC with the identity read off the ctx the resolver seam
// is handed. Production narrows inside refilter.go, also under the ctx the
// resolver is handed — but that ctx is NOT the handler's request context. Each
// dispatcher rebuilds it (xcontext.BuildContext(req.Context())) and then layers
// on the SA transport pair, the nested-resolve guards, the inbound loopback
// header reseed, and three sinks. If any of that SHIFTED the identity, my arms
// would still pass (they read whatever ctx they are given) while production
// narrowed for somebody else.
//
// That is not hypothetical. #57 is exactly this class: the authenticated-loopback
// prewarm resolves a nested /call under the authn-JWT GROUP identity rather than
// the caller's, so the identity at the refilter frame is not the identity the
// cache key was derived for. Under A-1 that would be doubly wrong — the gate,
// the key and the narrowing would each be reasoning about a different subject.
//
// So this arm pins the invariant my other arms ASSUME:
//
//	identity(handler request ctx) == identity(resolve ctx the resolver sees)
//	                              == the ctx that carries the UAF sink
//
// One ctx, one subject, for both dispatchers. If a future change inserts a hop
// that re-signs or re-derives the identity between the handler and the resolver,
// this goes RED here instead of silently invalidating every acceptance arm.

package dispatchers

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"

	xcontext "github.com/krateo-platformops/plumbing/context"
	templatesv1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/resolvers/restactions"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets"
)

// observedIdentity is what a resolver seam can see about the ctx it is handed.
type observedIdentity struct {
	username  string
	groups    []string
	err       error
	sinkFound bool
	sinkCount int64
}

// TestA1_RefilterIdentityEqualsRequestIdentity_RESTActions drives the REAL
// restActionHandler and captures, from inside the resolver seam, the identity
// and the UAF sink visible on the resolve ctx.
func TestA1_RefilterIdentityEqualsRequestIdentity_RESTActions(t *testing.T) {
	a1BuildTwoTenantWatcher(t)
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)

	for _, user := range []string{a1Alice, a1Bob} {
		t.Run(user, func(t *testing.T) {
			reqCtx := a1UserCtx(user)

			// Ground truth: the identity ON THE REQUEST, read the same way the
			// dispatcher's own key derivation reads it.
			wantUI, err := xcontext.UserInfo(reqCtx)
			if err != nil {
				t.Fatalf("setup: the request ctx must carry an identity; got %v", err)
			}

			var seen observedIdentity
			cr := a1RAUnstructured(true) // UAF-bearing
			cr.SetName(a1RAName2)
			restore := installRAFakes(t, cr, func() bool { return true },
				func(ctx context.Context, _ restactions.ResolveOptions) (*templatesv1.RESTAction, error) {
					// This is the frame refilter.go runs in: whatever identity is
					// on THIS ctx is the identity every per-object EvaluateRBAC
					// will narrow under.
					ui, uerr := xcontext.UserInfo(ctx)
					sink := cache.UAFTouchedSinkFromContext(ctx)
					seen = observedIdentity{
						username: ui.Username, groups: ui.Groups, err: uerr,
						sinkFound: sink != nil,
					}
					cache.BumpUAFTouched(ctx)
					if sink != nil {
						seen.sinkCount = sink.Count()
					}
					out := &templatesv1.RESTAction{}
					out.SetName(a1RAName2)
					out.SetNamespace(h1NS)
					return out, nil
				})
			defer restore()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/call", nil).WithContext(reqCtx)
			RESTAction().ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("dispatch must serve 200; got %d body=%s", rec.Code, rec.Body.String())
			}

			assertIdentityAgreement(t, "restactions", wantUI.Username, wantUI.Groups, seen)
		})
	}
}

// TestA1_RefilterIdentityEqualsRequestIdentity_Widgets is the same invariant on
// the R-1 carrier's handler. The widget path stacks MORE ctx layers than the
// restactions one (the nested-resolve ancestor seed, the loopback header reseed,
// the seed-resolve memo, the apiRef chokepoint), so it is the more likely place
// for an identity shift to be introduced later.
func TestA1_RefilterIdentityEqualsRequestIdentity_Widgets(t *testing.T) {
	a1BuildTwoTenantWatcher(t)
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)

	cr := h1WidgetUnstructured(map[string]any{
		"apiRef":             map[string]any{"name": a1RAName2, "namespace": h1NS},
		"widgetDataTemplate": []any{map[string]any{"forPath": ".namespaces", "expression": "${ .namespaces }"}},
	})

	for _, user := range []string{a1Alice, a1Bob} {
		t.Run(user, func(t *testing.T) {
			reqCtx := a1UserCtx(user)
			wantUI, err := xcontext.UserInfo(reqCtx)
			if err != nil {
				t.Fatalf("setup: the request ctx must carry an identity; got %v", err)
			}

			var seen observedIdentity
			restore := installWidgetFakes(t, cr, func() bool { return true },
				func(ctx context.Context, _ widgets.ResolveOptions) (*widgets.Widget, error) {
					ui, uerr := xcontext.UserInfo(ctx)
					sink := cache.UAFTouchedSinkFromContext(ctx)
					seen = observedIdentity{
						username: ui.Username, groups: ui.Groups, err: uerr,
						sinkFound: sink != nil,
					}
					cache.BumpUAFTouched(ctx)
					if sink != nil {
						seen.sinkCount = sink.Count()
					}
					return h1WidgetUnstructured(map[string]any{}), nil
				})
			defer restore()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/call", nil).WithContext(reqCtx)
			Widgets().ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("widget dispatch must serve 200; got %d body=%s", rec.Code, rec.Body.String())
			}

			assertIdentityAgreement(t, "widgets", wantUI.Username, wantUI.Groups, seen)
		})
	}
}

// assertIdentityAgreement is the shared check: the resolve ctx carries the SAME
// subject as the request, and the UAF sink rides that same ctx.
func assertIdentityAgreement(t *testing.T, class, wantUser string, wantGroups []string, seen observedIdentity) {
	t.Helper()

	if seen.err != nil {
		t.Fatalf("%s: the resolver was handed a ctx with NO identity (%v). The refilter narrows per object under "+
			"THIS ctx — with no identity it would fail closed or narrow under the empty subject, and every "+
			"acceptance arm that reads identity off this ctx would be reasoning about a different subject than "+
			"production.", class, seen.err)
	}
	if seen.username != wantUser {
		t.Fatalf("%s IDENTITY SHIFT (#57 class): the request was authenticated as %q but the resolver — the frame the "+
			"userAccessFilter refilter runs in — was handed %q. The refilter would narrow for the WRONG SUBJECT, "+
			"while the cache key and the UAF gate were derived for the right one. This also invalidates every A-1/R-2 "+
			"acceptance arm, which narrows using the identity read off this ctx.", class, wantUser, seen.username)
	}
	if !reflect.DeepEqual(seen.groups, wantGroups) {
		t.Fatalf("%s IDENTITY SHIFT (groups): request groups %v but the resolver saw %v. Groups are RBAC-determining "+
			"(the shared grant in this fixture is via a group), so a group-set shift moves the per-object verdicts "+
			"exactly as a username shift does.", class, wantGroups, seen.groups)
	}

	// The sink must ride the SAME ctx the identity does — that is what makes
	// "the body was narrowed" and "the gate declined" statements about one
	// subject rather than two.
	if !seen.sinkFound {
		t.Fatalf("%s: no UAFTouchedSink on the resolve ctx. The refilter's bump would land on a nil receiver and every "+
			"Put site would read Count()==0 — the R-1 gate would be inert on this path.", class)
	}
	if seen.sinkCount < 1 {
		t.Fatalf("%s: the sink on the resolve ctx did not record the bump (count=%d) — the ctx carries a sink that is "+
			"not the one the Put site reads.", class, seen.sinkCount)
	}
}
