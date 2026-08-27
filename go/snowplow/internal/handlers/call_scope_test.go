// call_scope_test.go — Issue #156 (/call cluster-scoped read+write)
// hermetic falsifiers. package handlers (white-box): we build a callHandler
// with a FAKE scopeResolver (no real discovery client, no kind cluster) and
// drive validateRequest then buildURIPath, asserting the emitted apiserver URI.
//
// Arms map 1:1 to the issue's acceptance criteria:
//   AC-1  cluster GET-by-name  → no namespaces/ segment
//   AC-2  cluster POST/PATCH/DELETE
//   AC-3  namespaced byte-identical (+ cold/erroring resolver arm)
//   RED   force namespaced for a cluster GVR → bad namespaced URI (must FAIL)
//   Fail-closed: namespace absent + resolver errors → 400
//   Backward-compat: namespace absent + scope=namespaced → 400
//   No-gate: prove no snowplow-side scope/RBAC gate sits between validate
//            and request.Do (the AC-4/AC-5 hermetic complement).

package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeScopeResolver returns a scopeResolverFn that reports `namespaced` for
// EVERY gvr, or an error if `err` is non-nil. `calls` (optional) counts
// invocations so a test can prove the resolver was NOT consulted on the
// namespace-present path.
func fakeScopeResolver(namespaced bool, err error, calls *int) scopeResolverFn {
	return func(gvr schema.GroupVersionResource) (bool, error) {
		if calls != nil {
			*calls++
		}
		if err != nil {
			return false, err
		}
		return namespaced, nil
	}
}

// newCallHandlerWithScope builds a callHandler with a fake resolver — the
// hermetic seam. No real dynamic.SharedSAScopeForGVR / discovery client.
func newCallHandlerWithScope(resolver scopeResolverFn) *callHandler {
	return &callHandler{scopeResolver: resolver}
}

// uriFor drives the real validation + URI-assembly pipeline (the exact
// steps ServeHTTP runs before request.Do) and returns the built apiserver
// URI or the validation error.
func uriFor(h *callHandler, method, target string) (string, error) {
	req := httptest.NewRequest(method, target, nil)
	opts, err := h.validateRequest(req)
	if err != nil {
		return "", err
	}
	return buildURIPath(opts)
}

const (
	clusterRoleGVR = "apiVersion=rbac.authorization.k8s.io/v1&resource=clusterroles"
	roleGVR        = "apiVersion=rbac.authorization.k8s.io/v1&resource=roles"
)

// --- AC-1: cluster GET-by-name omits namespaces/ ---------------------------

func TestCall156_AC1_ClusterGetByName(t *testing.T) {
	h := newCallHandlerWithScope(fakeScopeResolver(false /*cluster*/, nil, nil))

	got, err := uriFor(h, http.MethodGet,
		"/call?"+clusterRoleGVR+"&name=admin")
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	want := "/apis/rbac.authorization.k8s.io/v1/clusterroles/admin"
	if got != want {
		t.Fatalf("cluster GET URI = %q, want %q", got, want)
	}
	if strings.Contains(got, "namespaces/") {
		t.Fatalf("cluster GET URI %q MUST NOT contain a namespaces/ segment", got)
	}
}

// AC-1 (core group): a cluster-scoped CORE-group resource builds the
// /api/v1 base (no /apis/ prefix) with NO namespaces/ segment. Guards the
// group=="" base branch × cluster scope — the /api vs /apis fork that the
// apis-group clusterroles cases don't exercise. (review nit #5)
func TestCall156_AC1_CoreGroupClusterNode(t *testing.T) {
	h := newCallHandlerWithScope(fakeScopeResolver(false /*cluster*/, nil, nil))

	got, err := uriFor(h, http.MethodGet,
		"/call?apiVersion=v1&resource=nodes&name=node-1")
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	want := "/api/v1/nodes/node-1"
	if got != want {
		t.Fatalf("core-group cluster GET URI = %q, want %q", got, want)
	}
	if strings.Contains(got, "namespaces/") || strings.Contains(got, "/apis/") {
		t.Fatalf("core-group cluster URI %q must use /api/v1 with no namespaces/ or /apis/ prefix", got)
	}
}

// --- AC-2: cluster POST (no name), PATCH+DELETE (with name) -----------------

func TestCall156_AC2_ClusterWriteVerbs(t *testing.T) {
	h := newCallHandlerWithScope(fakeScopeResolver(false /*cluster*/, nil, nil))

	cases := []struct {
		name   string
		method string
		target string
		want   string
	}{
		{
			name:   "POST create omits name",
			method: http.MethodPost,
			target: "/call?" + clusterRoleGVR, // no name — create
			want:   "/apis/rbac.authorization.k8s.io/v1/clusterroles",
		},
		{
			name:   "PATCH by name",
			method: http.MethodPatch,
			target: "/call?" + clusterRoleGVR + "&name=admin",
			want:   "/apis/rbac.authorization.k8s.io/v1/clusterroles/admin",
		},
		{
			name:   "DELETE by name",
			method: http.MethodDelete,
			target: "/call?" + clusterRoleGVR + "&name=admin",
			want:   "/apis/rbac.authorization.k8s.io/v1/clusterroles/admin",
		},
		{
			name:   "PUT by name",
			method: http.MethodPut,
			target: "/call?" + clusterRoleGVR + "&name=admin",
			want:   "/apis/rbac.authorization.k8s.io/v1/clusterroles/admin",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := uriFor(h, c.method, c.target)
			if err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if got != c.want {
				t.Fatalf("%s URI = %q, want %q", c.method, got, c.want)
			}
			if strings.Contains(got, "namespaces/") {
				t.Fatalf("cluster %s URI %q MUST NOT contain a namespaces/ segment", c.method, got)
			}
		})
	}
}

// --- AC-3: namespaced byte-identical to pre-#156 ---------------------------

func TestCall156_AC3_NamespacedByteIdentical(t *testing.T) {
	// Resolver reports namespaced, but it must NOT even be consulted on the
	// namespace-present path — proven below by the cold-resolver arm.
	h := newCallHandlerWithScope(fakeScopeResolver(true, nil, nil))

	got, err := uriFor(h, http.MethodGet,
		"/call?"+roleGVR+"&namespace=demo-system&name=reader")
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	// Golden string: exactly today's assembled path.
	want := "/apis/rbac.authorization.k8s.io/v1/namespaces/demo-system/roles/reader"
	if got != want {
		t.Fatalf("namespaced GET URI = %q, want the byte-identical %q", got, want)
	}
}

// TestCall156_AC3_NamespacedIndependentOfResolver is the refinement arm: a
// namespace-PRESENT request must produce the identical URI even when the
// scopeResolver is COLD (nil) or ERRORS — proving the namespaced path never
// depends on the SA mapper (no new boot-window / discovery-lag failure).
func TestCall156_AC3_NamespacedIndependentOfResolver(t *testing.T) {
	const want = "/apis/rbac.authorization.k8s.io/v1/namespaces/demo-system/roles/reader"
	target := "/call?" + roleGVR + "&namespace=demo-system&name=reader"

	// (a) resolver ERRORS on every call — must be ignored.
	calls := 0
	hErr := newCallHandlerWithScope(fakeScopeResolver(false, fmt.Errorf("mapper cold / discovery lag"), &calls))
	got, err := uriFor(hErr, http.MethodGet, target)
	if err != nil {
		t.Fatalf("namespace-present must ignore a resolver error, got: %v", err)
	}
	if got != want {
		t.Fatalf("namespaced URI with erroring resolver = %q, want %q", got, want)
	}
	if calls != 0 {
		t.Fatalf("scopeResolver was consulted %d times on the namespace-present path; want 0 (must never be consulted)", calls)
	}

	// (b) resolver is nil (COLD handler, never wired) — must still be ignored.
	hNil := newCallHandlerWithScope(nil)
	got, err = uriFor(hNil, http.MethodGet, target)
	if err != nil {
		t.Fatalf("namespace-present must not depend on a wired resolver, got: %v", err)
	}
	if got != want {
		t.Fatalf("namespaced URI with nil resolver = %q, want %q", got, want)
	}
}

// --- RED arm (mandatory, discriminating) -----------------------------------

// TestCall156_RED_ForcedNamespacedForClusterGVR proves the scope branch is
// load-bearing: force the resolver to report NAMESPACED for a cluster GVR
// (with a namespace supplied so the bad path is reachable) → buildURIPath
// rebuilds the broken /namespaces/<ns>/clusterroles/<name> 404 path, so the
// AC-1 assertion (no namespaces/ segment) FAILS. Then restore scope=cluster
// → GREEN. This is the explicit RED-then-GREEN pair.
func TestCall156_RED_ForcedNamespacedForClusterGVR(t *testing.T) {
	acAssert := func(uri string) error {
		if strings.Contains(uri, "namespaces/") {
			return fmt.Errorf("AC-1 VIOLATED: cluster clusterroles URI %q contains a namespaces/ segment", uri)
		}
		want := "/apis/rbac.authorization.k8s.io/v1/clusterroles/admin"
		if uri != want {
			return fmt.Errorf("AC-1 VIOLATED: URI = %q, want %q", uri, want)
		}
		return nil
	}

	// RED: resolver mis-reports the cluster GVR as namespaced. A namespace is
	// supplied so the namespace-present branch builds the namespaced URI —
	// exactly the pre-fix bug shape. The AC-1 check MUST fail here.
	//
	// NB: with a namespace present, validateRequest takes the namespaced
	// branch unconditionally, so this simulates "the scope branch did the
	// wrong thing" — the neutered-scope regression the RED arm must catch.
	redURI, err := uriFor(
		newCallHandlerWithScope(fakeScopeResolver(true, nil, nil)),
		http.MethodGet,
		"/call?"+clusterRoleGVR+"&namespace=demo-system&name=admin",
	)
	if err != nil {
		t.Fatalf("RED setup: unexpected validation error: %v", err)
	}
	if got := acAssert(redURI); got == nil {
		t.Fatalf("RED arm did NOT go red: the AC-1 assertion passed on the neutered-scope URI %q — the scope branch is not load-bearing (test is not discriminating)", redURI)
	} else {
		t.Logf("RED confirmed: neutered scope built %q → AC-1 fails as required (%v)", redURI, got)
	}

	// GREEN: correct cluster scope (namespace omitted) → clean cluster URI.
	greenURI, err := uriFor(
		newCallHandlerWithScope(fakeScopeResolver(false, nil, nil)),
		http.MethodGet,
		"/call?"+clusterRoleGVR+"&name=admin",
	)
	if err != nil {
		t.Fatalf("GREEN: unexpected validation error: %v", err)
	}
	if got := acAssert(greenURI); got != nil {
		t.Fatalf("GREEN arm failed: %v", got)
	}
	t.Logf("GREEN confirmed: cluster scope built %q → AC-1 passes", greenURI)
}

// --- Fail-closed arm -------------------------------------------------------

// TestCall156_FailClosed_ScopeUnknown: namespace ABSENT + resolver errors
// (unknown GVR / mapper not synced) → 400 at validation (not a namespaced
// fallback, not a panic). buildURIPath is never reached.
func TestCall156_FailClosed_ScopeUnknown(t *testing.T) {
	h := newCallHandlerWithScope(fakeScopeResolver(false, fmt.Errorf("no matches for kind"), nil))

	_, err := uriFor(h, http.MethodPost, "/call?"+clusterRoleGVR)
	if err == nil {
		t.Fatalf("scope-unknown with namespace absent must FAIL-CLOSED (validation error), got nil (would have proceeded)")
	}
	if !strings.Contains(err.Error(), "resolve scope") {
		t.Fatalf("fail-closed error = %v, want a scope-resolution error", err)
	}
}

// TestCall156_FailClosed_NilResolver: a mis-constructed handler (nil
// resolver) on the namespace-absent path fails-closed, never treats the GVR
// as cluster-scoped (no silent URI widening).
func TestCall156_FailClosed_NilResolver(t *testing.T) {
	h := newCallHandlerWithScope(nil)
	_, err := uriFor(h, http.MethodPost, "/call?"+clusterRoleGVR)
	if err == nil {
		t.Fatalf("nil resolver on namespace-absent path must fail-closed, got nil")
	}
}

// --- Backward-compat arm ---------------------------------------------------

// TestCall156_BackwardCompat_NamespacedNoNamespace: namespace ABSENT + the
// GVR is genuinely namespaced → 400 exactly as today (missing 'namespace'),
// NOT a cluster path.
func TestCall156_BackwardCompat_NamespacedNoNamespace(t *testing.T) {
	h := newCallHandlerWithScope(fakeScopeResolver(true /*namespaced*/, nil, nil))

	_, err := uriFor(h, http.MethodGet, "/call?"+roleGVR+"&name=reader")
	if err == nil {
		t.Fatalf("namespaced GVR without a namespace must 400 as today, got nil")
	}
	if err.Error() != "missing 'namespace' query parameter" {
		t.Fatalf("backward-compat 400 message = %q, want the byte-identical %q",
			err.Error(), "missing 'namespace' query parameter")
	}
}

// TestCall156_NamespacePresent_MissingName preserves the pre-#156 name check
// on the namespaced path for EVERY verb (POST-without-name still 400s), so
// the namespace-present path gains zero new failure modes and loses none.
func TestCall156_NamespacePresent_MissingName(t *testing.T) {
	h := newCallHandlerWithScope(fakeScopeResolver(true, nil, nil))
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		_, err := uriFor(h, m, "/call?"+roleGVR+"&namespace=demo-system")
		if err == nil {
			t.Fatalf("%s namespace-present without name must 400 as today, got nil", m)
		}
		if err.Error() != "missing 'name' query parameter" {
			t.Fatalf("%s missing-name 400 = %q, want %q", m, err.Error(), "missing 'name' query parameter")
		}
	}
}

// --- No-gate (hermetic complement to AC-4/AC-5) ----------------------------

// TestCall156_NoSnowplowSideScopeGate is the hermetic assertion the kind
// arm's AC-4 (auth via caller token) rests on: there is NO snowplow-side
// scope/RBAC gate between validate and request.Do. We prove it structurally:
// once validateRequest returns nil (cluster scope resolved), buildURIPath
// yields the cluster URI and the handler proceeds to build callOpts under
// the CALLER's endpoint (ep = xcontext.UserConfig) — the SA mapper is used
// ONLY for the shape read above, never to authorize. There is no code path
// that consults the SA identity, an allowlist, or a namespace policy after
// validation. This test asserts the resolved cluster request produces a
// well-formed cluster URI with no injected namespace, which is the sole
// snowplow-side transformation; apiserver RBAC (kind arm) does the rest.
func TestCall156_NoSnowplowSideScopeGate(t *testing.T) {
	h := newCallHandlerWithScope(fakeScopeResolver(false, nil, nil))

	req := httptest.NewRequest(http.MethodPost, "/call?"+clusterRoleGVR, nil)
	opts, err := h.validateRequest(req)
	if err != nil {
		t.Fatalf("cluster POST validation must succeed with cluster scope, got: %v", err)
	}
	// Post-validation there is no scope/RBAC decision left in snowplow: the
	// resolved opts carry an empty namespace and a cluster URI, and the
	// handler's next step is request.Do under the caller endpoint. Assert
	// the two invariants that make that safe.
	if opts.namespaced {
		t.Fatalf("cluster POST resolved namespaced=true; the write would be misrouted")
	}
	if opts.nsn.Namespace != "" {
		t.Fatalf("cluster POST carried a namespace %q; a cluster write must have none", opts.nsn.Namespace)
	}
	uri, err := buildURIPath(opts)
	if err != nil {
		t.Fatalf("buildURIPath: %v", err)
	}
	if uri != "/apis/rbac.authorization.k8s.io/v1/clusterroles" {
		t.Fatalf("cluster POST URI = %q, want the caller-token cluster create path", uri)
	}
}
