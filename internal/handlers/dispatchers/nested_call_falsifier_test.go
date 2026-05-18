// nested_call_falsifier_test.go — Ship 0.30.123 (#155) falsifiers for
// in-process nested /call resolution.
//
// THE MECHANISM: when a RESTAction stage's `path` is a /call?resource=...
// loopback into snowplow's own /call endpoint, the api resolver resolves
// the referenced RESTAction IN-PROCESS (dispatchers.ResolveNestedCall)
// instead of issuing an HTTP request with no Authorization header. This
// lets a JWT-less / SA-credentialed resolve complete an exportJwt
// loopback stage — the 0.30.120 poison — and unblocks F2's startup
// SA-prewarm.
//
// SIX FALSIFIERS:
//   F1 — HEADLINE, capturable pre-fix: a JWT-less resolve of an outer
//        RESTAction with a /call-loopback stage produces NON-EMPTY
//        content. Run with RESOLVER_INPROCESS_NESTED_CALL=false the
//        loopback path is skipped (byte-identical to 0.30.121) and the
//        stage yields EMPTY — the captured pre-fix artifact. Flag on
//        (default): non-empty.
//   F2 — the in-process result is byte-identical to a direct
//        restactions.Resolve of the inner RESTAction.
//   F3 — an exportJwt RESTAction refreshes with CORRECT non-empty
//        content; the layer-(b) stage-error sink stays 0.
//   F4 — recursion: a RESTAction whose /call stage references itself
//        terminates with a `depth limit exceeded` error in
//        dict[call.ErrorKey] (no stack overflow / hang).
//   F5 — a denied dispatch (identity not RBAC-authorized for the inner
//        RESTAction) surfaces a 403-class error, NOT empty.
//   F6 — the depth-8 cap surfaces a bounded ERROR rendered AS an error
//        by the outer response handler — NOT silent empty content.
//
// The api resolver is driven via api.Resolve directly with a one-stage
// outer RESTAction whose stage is a /call loopback; the nested resolver
// seam is swapped with a stub (api.RegisterNestedCallResolver) so the
// loopback branch is exercised without a live cluster. F3/F5 drive the
// real dispatchers.ResolveNestedCall against the watcher harness.

package dispatchers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	restactionsapi "github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
)

// --- shared fixtures ------------------------------------------------------

// nestedCallLoopbackPath is a /call?resource=...&apiVersion=... loopback
// URL — the shape util.ParseCallPathToObjectRef recognises. Host-qualified
// so the test also covers the host-prefixed form.
const nestedCallLoopbackPath = "http://snowplow.krateo-system.svc:8081/call" +
	"?resource=restactions&apiVersion=templates.krateo.io/v1" +
	"&name=inner-restaction&namespace=krateo-system"

// loopbackStage builds a one-stage outer RESTAction whose single stage is
// a /call loopback. continueOnError mirrors the compositions-list shape
// (the exportJwt stage runs continueOnError).
func loopbackStage(id string, continueOnErr bool) *templates.API {
	return &templates.API{
		Name:            id,
		Path:            nestedCallLoopbackPath,
		Verb:            ptr.To(http.MethodGet),
		ContinueOnError: ptr.To(continueOnErr),
		ErrorKey:        ptr.To(id + "Error"),
		// Filter projects the stage output under the id so a non-empty
		// nested result is visible in dict[id].
		Filter: ptr.To("." + id),
	}
}

// nestedResolveJWTLess drives api.Resolve for a one-stage outer
// RESTAction under a JWT-LESS identity — WithUserInfo only, NO
// AccessToken, NO per-user Endpoint. A WithInternalEndpoint placeholder
// lets the resolver's endpoint resolution succeed (the loopback branch
// never dispatches over it). Returns the resolved dict.
func nestedResolveJWTLess(t *testing.T, stage *templates.API) map[string]any {
	t.Helper()
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "sa-prewarmer"}),
	)
	ctx = cache.WithInternalEndpoint(ctx,
		&endpoints.Endpoint{ServerURL: "http://test.invalid"})
	return restactionsapi.Resolve(ctx, restactionsapi.ResolveOptions{
		// A non-nil RC keeps api.Resolve off its rest.InClusterConfig()
		// early-return (which fails outside a cluster). The loopback
		// branch never dereferences RC's contents — a bare config is enough.
		RC:                  &rest.Config{},
		Items:               []*templates.API{stage},
		RESTActionNamespace: "krateo-system",
		RESTActionName:      "outer-restaction",
	})
}

// --- F1 — HEADLINE: JWT-less /call-loopback resolve is non-empty ---------

// TestF1_NestedCall_JWTLessLoopbackNonEmpty is the headline falsifier and
// the capturable pre-fix artifact.
//
//   * Flag OFF (RESOLVER_INPROCESS_NESTED_CALL=false) — the loopback
//     branch is skipped; the /call stage takes the HTTP path; with no
//     Authorization header / unreachable endpoint it fails, continueOnError
//     swallows it, dict[id] is EMPTY. This is byte-identical to the
//     0.30.121 / 0.30.120-poison behaviour — the PRE-FIX artifact.
//   * Flag ON (default) — the loopback branch resolves the inner
//     RESTAction IN-PROCESS via the registered resolver; dict[id] is
//     NON-EMPTY.
func TestF1_NestedCall_JWTLessLoopbackNonEmpty(t *testing.T) {
	// The in-process result the stub nested resolver returns — a
	// non-trivial inner RESTAction Status.Raw.
	innerStatus := []byte(`{"list":[{"uid":"real-composition"}]}`)

	restore := setNestedCallResolverForTest(
		func(_ context.Context, _ templates.ObjectReference, _, _ int, _ map[string]any) ([]byte, error) {
			return innerStatus, nil
		})
	t.Cleanup(restore)

	// --- PRE-FIX arm: flag OFF ---
	t.Run("prefix_flagOff_empty", func(t *testing.T) {
		t.Setenv("RESOLVER_INPROCESS_NESTED_CALL", "false")
		dict := nestedResolveJWTLess(t, loopbackStage("compositions", true))
		// The /call stage took the HTTP path, failed JWT-less, and
		// continueOnError swallowed it — dict["compositions"] is empty/nil.
		if v, ok := dict["compositions"]; ok && v != nil {
			if m, isMap := v.(map[string]any); !isMap || len(m) > 0 {
				t.Fatalf("PRE-FIX expectation: flag-off /call loopback should yield "+
					"EMPTY content (the 0.30.120 poison); got %#v", v)
			}
		}
	})

	// --- POST-FIX arm: flag ON (default) ---
	t.Run("postfix_flagOn_nonEmpty", func(t *testing.T) {
		t.Setenv("RESOLVER_INPROCESS_NESTED_CALL", "true")
		dict := nestedResolveJWTLess(t, loopbackStage("compositions", true))
		v, ok := dict["compositions"]
		if !ok || v == nil {
			t.Fatalf("F1: flag-on JWT-less /call loopback produced NO content — "+
				"the in-process nested resolver did not run; dict=%#v", dict)
		}
		raw, _ := json.Marshal(v)
		if !strings.Contains(string(raw), "real-composition") {
			t.Fatalf("F1: in-process nested /call result missing the inner content; "+
				"got %s", raw)
		}
	})
}

// --- F2 — in-process result byte-identical to direct inner resolve -------

// TestF2_NestedCall_ByteIdenticalToInnerResolve asserts the bytes the
// loopback branch feeds into the stage handler are EXACTLY the bytes the
// nested resolver returned (which, in production, is the inner
// RESTAction's Status.Raw — i.e. == a direct restactions.Resolve(inner)).
// The stub returns a known payload; the test asserts dict[id] carries it
// verbatim, proving the loopback branch does not mutate / re-wrap the
// nested result.
func TestF2_NestedCall_ByteIdenticalToInnerResolve(t *testing.T) {
	innerStatus := []byte(`{"items":[{"k":"v"}],"meta":{"n":3}}`)
	restore := setNestedCallResolverForTest(
		func(_ context.Context, _ templates.ObjectReference, _, _ int, _ map[string]any) ([]byte, error) {
			return innerStatus, nil
		})
	t.Cleanup(restore)

	t.Setenv("RESOLVER_INPROCESS_NESTED_CALL", "true")
	dict := nestedResolveJWTLess(t, loopbackStage("inner", true))

	got, ok := dict["inner"]
	if !ok {
		t.Fatalf("F2: dict missing the loopback stage output")
	}
	// dict["inner"] is the JSON-decoded innerStatus (the stage handler
	// json-decodes the nested bytes). Re-marshal and compare structurally
	// to the inner resolver's output decoded the same way.
	var want, have any
	if err := json.Unmarshal(innerStatus, &want); err != nil {
		t.Fatalf("F2: unmarshal inner status: %v", err)
	}
	gotBytes, _ := json.Marshal(got)
	if err := json.Unmarshal(gotBytes, &have); err != nil {
		t.Fatalf("F2: re-unmarshal stage output: %v", err)
	}
	wantBytes, _ := json.Marshal(want)
	if string(gotBytes) != string(wantBytes) {
		t.Fatalf("F2: in-process nested result NOT byte-identical to the inner "+
			"resolve.\n want: %s\n got:  %s", wantBytes, gotBytes)
	}
}

// --- F4 / F6 — recursion depth bound -------------------------------------

// TestF4F6_NestedCall_DepthLimitBoundedError drives a SELF-REFERENTIAL
// nested /call: the stub resolver, instead of returning content, recurses
// by invoking the real ResolveNestedCall-style depth check — modelled
// here by a stub that re-enters with an incremented depth context until
// the cap. The real recursion bound lives in ResolveNestedCall (the
// dispatchers impl), exercised directly in TestF4_RealResolveNestedCall
// DepthCap below. This test asserts the OUTER resolver surfaces a
// `depth limit exceeded` error in dict[ErrorKey] — NOT a hang, NOT empty
// (F4 + F6: a bounded error rendered AS an error, never silent empty).
func TestF4F6_NestedCall_DepthLimitBoundedError(t *testing.T) {
	t.Setenv("RESOLVER_INPROCESS_NESTED_CALL", "true")

	// The stub emulates a /call stage whose nested resolve hit the depth
	// cap — exactly what ResolveNestedCall returns at NestedCallMaxDepth.
	restore := setNestedCallResolverForTest(
		func(_ context.Context, _ templates.ObjectReference, _, _ int, _ map[string]any) ([]byte, error) {
			return nil, fmt.Errorf("nested /call depth limit exceeded (%d)",
				cache.NestedCallMaxDepth())
		})
	t.Cleanup(restore)

	// continueOnError=true so the error lands in dict[ErrorKey] and the
	// resolve completes (the F6 property: the cap is an ERROR, surfaced,
	// not a silent empty).
	dict := nestedResolveJWTLess(t, loopbackStage("selfref", true))

	errVal, ok := dict["selfrefError"]
	if !ok || errVal == nil {
		t.Fatalf("F4/F6: depth-limit hit produced NO error key — a recursion cap "+
			"that yields silent empty content is a masked failure; dict=%#v", dict)
	}
	if s, _ := errVal.(string); !strings.Contains(s, "depth limit exceeded") {
		t.Fatalf("F4/F6: depth-limit error key = %#v; want a `depth limit exceeded` "+
			"message rendered AS an error", errVal)
	}
}

// TestF4_RealResolveNestedCall_DepthCap drives the REAL ResolveNestedCall
// recursion bound directly: a context already at NestedCallMaxDepth must
// return a bounded `depth limit exceeded` error WITHOUT objects.Get,
// WITHOUT resolving — and never panic / overflow.
func TestF4_RealResolveNestedCall_DepthCap(t *testing.T) {
	ctx := cache.WithNestedCallDepth(context.Background(), cache.NestedCallMaxDepth())
	ref := templates.ObjectReference{
		Reference:  templates.Reference{Name: "x", Namespace: "n"},
		Resource:   "restactions",
		APIVersion: "templates.krateo.io/v1",
	}
	raw, err := ResolveNestedCall(ctx, ref, 0, 0, nil)
	if err == nil {
		t.Fatalf("F4: ResolveNestedCall at the depth cap must return an error, got nil")
	}
	if !strings.Contains(err.Error(), "depth limit exceeded") {
		t.Fatalf("F4: depth-cap error = %q; want a `depth limit exceeded` message", err)
	}
	if raw != nil {
		t.Fatalf("F4: depth-cap must return nil bytes, got %d", len(raw))
	}
}

// TestF4_DepthCapBoundaryBelowCapProceeds asserts a depth ONE BELOW the
// cap does NOT short-circuit on the depth check (it proceeds to
// objects.Get — which then fails hermetically with a fetch error, NOT a
// depth error). This proves the cap is an upper bound, not off-by-one.
func TestF4_DepthCapBoundaryBelowCapProceeds(t *testing.T) {
	ctx := cache.WithNestedCallDepth(context.Background(), cache.NestedCallMaxDepth()-1)
	ref := templates.ObjectReference{
		Reference:  templates.Reference{Name: "x", Namespace: "n"},
		Resource:   "restactions",
		APIVersion: "templates.krateo.io/v1",
	}
	_, err := ResolveNestedCall(ctx, ref, 0, 0, nil)
	// It WILL error (no cluster), but the error must NOT be the depth cap
	// — at depth cap-1 the call must proceed past the depth guard.
	if err != nil && strings.Contains(err.Error(), "depth limit exceeded") {
		t.Fatalf("F4: at depth cap-1 the call must NOT hit the depth limit; got %v", err)
	}
}

// --- F3 / F5 watcher harness ---------------------------------------------

// nestedCallInnerGVR is the inner RESTAction's GVR.
var nestedCallInnerGVR = schema.GroupVersionResource{
	Group:    "templates.krateo.io",
	Version:  "v1",
	Resource: "restactions",
}

// nestedInnerRESTAction builds an unstructured inner RESTAction CR with a
// single trivial api stage (no inner /call — it just resolves to a small
// dict). It is the target of the nested /call.
func nestedInnerRESTAction(ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "templates.krateo.io/v1",
		"kind":       "RESTAction",
		"metadata": map[string]any{
			"namespace": ns,
			"name":      name,
		},
		"spec": map[string]any{
			// Top-level filter projects a constant — the inner resolve
			// produces deterministic non-empty content with no real
			// apiserver call (the api stage list is empty, so dict is {}
			// and the filter yields the constant object).
			"filter": `{"resolved":true,"name":"` + name + `"}`,
			"api":    []any{},
		},
	}}
}

// nestedCallClusterRole grants `get` on templates.krateo.io restactions.
func nestedCallGetRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "nested-call-restaction-getter"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{nestedCallInnerGVR.Group},
				Resources: []string{nestedCallInnerGVR.Resource},
				Verbs:     []string{"get"}},
		},
	}
}

// nestedCallRoleBinding binds user to the getter ClusterRole in ns.
func nestedCallRoleBinding(ns, user string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "nested-call-binding-" + user},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.UserKind, APIGroup: "rbac.authorization.k8s.io", Name: user},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole",
			Name: "nested-call-restaction-getter",
		},
	}
}

// newNestedCallWatcher builds a cache=on watcher seeded with one inner
// RESTAction CR plus the RBAC grants passed in `bindings`. The inner
// restactions informer is registered + synced so objects.Get serves it.
func newNestedCallWatcher(t *testing.T, ns, name string, bindings ...*rbacv1.RoleBinding) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("RESOLVER_USE_INFORMER", "true")
	cache.ResetResolvedCacheForTest()
	cache.ResetDepsForTest()
	t.Cleanup(func() {
		cache.ResetResolvedCacheForTest()
		cache.ResetDepsForTest()
	})

	seed := []k8sruntime.Object{nestedInnerRESTAction(ns, name), nestedCallGetRole()}
	for _, b := range bindings {
		seed = append(seed, b)
	}

	scheme := k8sruntime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)
	listKinds := map[schema.GroupVersionResource]string{
		nestedCallInnerGVR: "RESTActionList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:               "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:        "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:        "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, seed...)

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() { rw.Stop() })

	added, syncCh := rw.EnsureResourceType(nestedCallInnerGVR)
	if !added {
		t.Fatalf("EnsureResourceType(restactions): want added=true")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("inner restactions informer did not sync")
	}
	syncCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(syncCtx, 5*time.Second); err != nil {
		t.Fatalf("WaitForCacheSync (RBAC informers): %v", err)
	}
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })
}

// --- F5 — denied dispatch surfaces an error, NOT empty -------------------

// TestF5_NestedCall_DeniedDispatchIsErrorNotEmpty drives the REAL
// ResolveNestedCall for an identity with NO RBAC grant for the inner
// RESTAction. The dispatch MUST fail closed — ResolveNestedCall returns
// an ERROR and nil bytes, NEVER empty-but-valid content (which would
// mask the denial). Two fail-closed gates can catch it:
//   - objects.Get's informer-serve filterGetByRBAC (Tag 0.30.101) — for
//     a denied user the informer GET is refused and, with no user
//     Endpoint to fall through to, objects.Get returns a fetch error;
//   - if the GET somehow succeeded, ResolveNestedCall's own
//     checkDispatchRBAC (the load-bearing in-process RBAC gate) denies
//     with an explicit "forbidden" error.
// Either way the contract holds: a denied nested /call is an error with
// nil bytes — never silent empty content.
func TestF5_NestedCall_DeniedDispatchIsErrorNotEmpty(t *testing.T) {
	const ns, name = "krateo-system", "inner-restaction"
	// NO RoleBinding for "denied-user" — the inner RESTAction exists but
	// the identity has no `get` grant.
	newNestedCallWatcher(t, ns, name)

	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "denied-user"}),
	)
	ref := templates.ObjectReference{
		Reference:  templates.Reference{Name: name, Namespace: ns},
		Resource:   nestedCallInnerGVR.Resource,
		APIVersion: nestedCallInnerGVR.Group + "/" + nestedCallInnerGVR.Version,
	}
	raw, err := ResolveNestedCall(ctx, ref, 0, 0, nil)
	if err == nil {
		t.Fatalf("F5: a denied nested /call must FAIL CLOSED with an ERROR — "+
			"got nil error with %d bytes (the denial was masked as content)", len(raw))
	}
	if raw != nil {
		t.Fatalf("F5: a denied nested /call must return nil bytes, got %d — "+
			"NEVER empty-but-valid content (that masks the RBAC denial)", len(raw))
	}
	// The error must be a denial / fetch-failure — NOT a resolve that
	// produced empty content. (In a real cluster the apiserver fall-through
	// surfaces a literal 403; hermetically the informer-serve RBAC refusal
	// + absent user Endpoint surfaces a fetch error — both are fail-closed.)
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "forbidden") && !strings.Contains(low, "fetch") &&
		!strings.Contains(low, "endpoint") {
		t.Fatalf("F5: denied dispatch error = %q; want a fail-closed denial / "+
			"fetch-failure error, not a resolve that swallowed the denial", err)
	}
}

// TestF5_RealCheckDispatchRBAC_DeniesUnauthorized is the focused
// companion to F5: it asserts the load-bearing checkDispatchRBAC gate
// itself denies an unauthorized identity. ResolveNestedCall calls this
// gate AFTER objects.Get; this test exercises the gate directly so the
// "single most important correctness line" is covered even though, in
// the hermetic objects.Get path, the informer-serve RBAC filter denies
// first.
func TestF5_RealCheckDispatchRBAC_DeniesUnauthorized(t *testing.T) {
	const ns, name = "krateo-system", "inner-restaction"
	newNestedCallWatcher(t, ns, name, nestedCallRoleBinding(ns, "authorized-user"))

	deniedCtx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "denied-user"}),
	)
	if checkDispatchRBAC(deniedCtx, nestedCallInnerGVR, ns) {
		t.Fatalf("F5: checkDispatchRBAC ALLOWED an identity with no `get` grant — "+
			"the in-process nested /call RBAC gate is the single load-bearing "+
			"correctness line and must deny")
	}
	authedCtx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "authorized-user"}),
	)
	if !checkDispatchRBAC(authedCtx, nestedCallInnerGVR, ns) {
		t.Fatalf("F5: checkDispatchRBAC DENIED an identity that holds the `get` grant")
	}
}

// --- F3 — exportJwt RESTAction refreshes with correct non-empty content --

// TestF3_NestedCall_AuthorizedResolveNonEmpty drives the REAL
// ResolveNestedCall for an AUTHORIZED identity and asserts it returns
// CORRECT, NON-EMPTY content (not (nil,nil) skip-to-TTL, not empty). This
// is the F2-unblock property: a JWT-less / SA-credentialed nested /call
// of an (exportJwt) RESTAction now resolves real content. It also
// confirms the layer-(b) stage-error sink stays 0 — a clean resolve
// records no stage error, so the refresher's error-aware Put-gate would
// NOT decline the Put (the backstop is intact but does not false-fire).
func TestF3_NestedCall_AuthorizedResolveNonEmpty(t *testing.T) {
	const ns, name = "krateo-system", "inner-restaction"
	newNestedCallWatcher(t, ns, name, nestedCallRoleBinding(ns, "authorized-user"))

	// A stage-error sink on ctx — the layer-(b) seam. A clean nested
	// resolve must leave it at 0.
	base := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "authorized-user"}),
	)
	ctx, sink := cache.WithStageErrorSink(base)

	ref := templates.ObjectReference{
		Reference:  templates.Reference{Name: name, Namespace: ns},
		Resource:   nestedCallInnerGVR.Resource,
		APIVersion: nestedCallInnerGVR.Group + "/" + nestedCallInnerGVR.Version,
	}
	raw, err := ResolveNestedCall(ctx, ref, 0, 0, nil)
	if err != nil {
		t.Fatalf("F3: authorized nested /call must resolve cleanly, got error: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("F3: authorized nested /call produced EMPTY content — the F2-unblock "+
			"property requires real non-empty content, not a skip-to-TTL")
	}
	if !strings.Contains(string(raw), "resolved") {
		t.Fatalf("F3: nested /call result missing the inner resolved content; got %s", raw)
	}
	// Layer (b): a clean resolve records NO stage error.
	if got := sink.Load(); got != 0 {
		t.Fatalf("F3: layer-(b) stage-error sink = %d after a CLEAN nested resolve; "+
			"want 0 (the error-aware Put-gate must not false-fire on a good resolve)", got)
	}
}
