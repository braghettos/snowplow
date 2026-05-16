// informer_dispatch_internal_bypass_test.go — 0.30.107 falsifier for the
// internal-dispatch RBAC bypass.
//
// THE BUG (0.30.106 boot log, "informer_dispatch.summary rbac_dropped=3"):
//   Phase 1's SA-credentialed startup walk resolves the navigation tree
//   under the snowplow service-account identity. That identity carries NO
//   Krateo Role/RoleBinding CRs. When the navmenu root's apiRef RESTAction
//   LISTs `navmenuitems` and the call hits the resolver pivot's served
//   LIST branch, filterListByRBAC ran a per-item EvaluateRBAC against the
//   SA identity, default-denied EVERY item, and served an EMPTY list. The
//   navmenu's resourcesRefsTemplate iterator `${ .navmenuitems }` then had
//   zero items — the navmenu→navmenuitem→page→datagrid descent died and
//   composition.krateo.io was never discovered.
//
// THE FIX: filterListByRBAC / filterGetByRBAC bypass the per-user RBAC
// narrowing when cache.IsInternalDispatch(ctx) is true — i.e. when an
// internal/startup driver (Phase 1's SA walk) is in flight. Phase 1 is
// identity-independent informer DISCOVERY, not per-user rendering.
//
// THE SECURITY INVARIANT (also tested here): the bypass NEVER widens a
// real user's view. An ordinary per-user request never sets the
// internal-dispatch marker, so for it filterListByRBAC narrows exactly as
// before — TestInternalDispatchBypass_RealUserStillNarrowed proves a
// narrow user on a NON-internal context still gets only their subset.
//
// Build modes: with `-tags rbac_narrow_baseline` the bypass test is
// skipped (the bypass is a Tag-0.30.107 behaviour). The real-user
// narrowing assertion runs in both modes.

package api

import (
	"net/http"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/client-go/rest"
)

// TestInternalDispatchBypass_Phase1SeesFullPartition is the headline
// falsifier. The Phase 1 SA identity has NO RBAC grant for ANY seeded
// namespace (it is not narrowUser and has no RoleBindings). On an
// ordinary context filterListByRBAC would serve ZERO items. With the
// internal-dispatch marker on the context, the served LIST MUST contain
// EVERY seeded item — informer discovery is identity-independent.
func TestInternalDispatchBypass_Phase1SeesFullPartition(t *testing.T) {
	if rbacNarrowBaseline {
		t.Skip("internal-dispatch bypass is a 0.30.107 behaviour")
	}
	newRBACNarrowWatcher(t)

	totalSeeded := (len(authorizedNamespaces) + len(deniedNamespaces)) * 2

	// The Phase 1 SA identity — a username with NO RoleBindings anywhere.
	// On an ordinary request this user would see zero items.
	saCtx := ctxWithUser("snowplow-serviceaccount")
	// Attach the internal-dispatch marker exactly as Phase 1's walk does
	// (cache.WithInternalRESTConfig). A bare non-nil *rest.Config is
	// sufficient — IsInternalDispatch only checks the marker is present.
	saCtx = cache.WithInternalRESTConfig(saCtx, &rest.Config{})
	if !cache.IsInternalDispatch(saCtx) {
		t.Fatalf("test setup: internal-dispatch marker not detected")
	}

	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/restactions")
	raw, served := dispatchViaInformer(saCtx, call)
	if !served {
		t.Fatalf("internal-dispatch LIST: expected served=true; got served=false")
	}
	got := servedItemNames(t, raw)
	if len(got) != totalSeeded {
		t.Fatalf("internal-dispatch LIST: want ALL %d seeded items (discovery is identity-independent); got %d (%v)",
			totalSeeded, len(got), sortedKeys(got))
	}
}

// TestInternalDispatchBypass_RealUserStillNarrowed is the security
// negative control: the SAME no-grant identity on a context WITHOUT the
// internal-dispatch marker MUST still be narrowed — the bypass cannot
// leak into a real per-user request.
func TestInternalDispatchBypass_RealUserStillNarrowed(t *testing.T) {
	newRBACNarrowWatcher(t)

	// Same no-grant identity, but NO internal-dispatch marker — an
	// ordinary per-user request.
	userCtx := ctxWithUser("snowplow-serviceaccount")
	if cache.IsInternalDispatch(userCtx) {
		t.Fatalf("test setup: ordinary context must NOT be internal-dispatch")
	}

	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/restactions")
	raw, served := dispatchViaInformer(userCtx, call)
	if !served {
		// Fell through to apiserver — also acceptable fail-closed: the
		// per-user token narrows there. Never a leak.
		if raw != nil {
			t.Fatalf("fail-closed fallthrough: want nil raw; got %d bytes", len(raw))
		}
		return
	}
	if got := servedItemNames(t, raw); len(got) != 0 {
		t.Fatalf("real-user narrowing: a no-grant user on a non-internal context served %d items (%v) — the bypass LEAKED",
			len(got), sortedKeys(got))
	}
}

// TestInternalDispatchBypass_GetByName covers the GET-by-name branch
// (filterGetByRBAC). The Phase 1 SA identity GETting a known object in a
// namespace it has no `get` grant for MUST receive the object when the
// internal-dispatch marker is set — the walk fetches child widget CRs by
// name and could not descend otherwise.
func TestInternalDispatchBypass_GetByName(t *testing.T) {
	if rbacNarrowBaseline {
		t.Skip("internal-dispatch bypass is a 0.30.107 behaviour")
	}
	newRBACNarrowWatcher(t)

	// GET a known object in a DENIED namespace (bench-ns-1-x). On an
	// ordinary context the no-grant SA identity would be denied; with the
	// internal-dispatch marker it must be served.
	saCtx := cache.WithInternalRESTConfig(ctxWithUser("snowplow-serviceaccount"), &rest.Config{})
	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/namespaces/bench-ns-1/restactions/bench-ns-1-x")
	raw, served := dispatchViaInformer(saCtx, call)
	if !served {
		t.Fatalf("internal-dispatch GET: expected served=true (discovery is identity-independent); got served=false")
	}
	if len(raw) == 0 {
		t.Fatalf("internal-dispatch GET: served empty bytes")
	}
}
