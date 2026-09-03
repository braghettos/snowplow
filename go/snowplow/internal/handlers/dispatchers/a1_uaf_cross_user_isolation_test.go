// a1_uaf_cross_user_isolation_test.go — THE A-1 ACCEPTANCE ARM.
//
// The other A-1 arms prove each Put SITE declines. This one proves the DEFECT
// is closed: that a second user who shares the first-match binding is not served
// the first user's userAccessFilter-narrowed rows.
//
// WHY THIS ARM EXISTS AND WHY IT IS SHAPED THIS WAY
// (feedback_seed_group_shared_cell_needs_resolve_output_arm,
// feedback_consultation_mutation_is_not_key_correctness): a key-side-only test
// — "alice and bob derive different keys" or "no Put was called" — is
// INADMISSIBLE as acceptance for a shared-cell leak. It passes a mutation that
// keeps the keys apart on paper while the served BODY still crosses over. So
// this arm asserts on the RESOLVE OUTPUT that leaves the handler: what is in
// BOB'S RESPONSE BYTES.
//
// THE TWO PRECONDITIONS make the arm impossible to pass vacuously. Both are
// t.Fatal, not t.Skip:
//
//	(i)  alice's and bob's per-namespace RBAC verdicts genuinely DIVERGE —
//	     alice sees tenant-a and not tenant-b, bob the mirror. If they did not
//	     diverge, "bob has no alice-only rows" would be trivially true.
//	(ii) their DERIVED CACHE KEYS are byte-EQUAL (and their BindingUIDs
//	     non-empty and equal). This is the shared cell. If the keys differed,
//	     the leak could not occur and the arm would prove nothing.
//
// Both are computed by the PRODUCTION derivations — real rbac.EvaluateRBAC over
// a real published snapshot, and the real dispatchCacheLookupKey — never
// hand-fed constants.
//
// THE NARROWING IS REAL, NOT SCRIPTED. The resolver seam is stubbed (there is no
// apiserver here), but the stub does NOT return a canned per-user body: it runs
// the userAccessFilter contract itself — for each candidate object it calls the
// REAL rbac.EvaluateRBAC with the REQUESTING identity read off the handler's own
// ctx and the object's own namespace, exactly as refilter.go's evalSingle does.
// So each user's body is DERIVED from that user's real RBAC, and a body that
// crosses over can only have come from the cache.
//
// RED on origin/main: main Puts alice's narrowed body under the shared key;
// bob's request HITS it, the hit path writes entry.RawJSON verbatim with no
// re-narrowing, and bob's response contains tenant-a and not tenant-b — this
// test fails on both assertions. GREEN on this branch: no Put ever happens, so
// bob's request resolves under his own identity.
//
// KEEP THIS TEST THROUGH 1.13.0. It is written against the OBSERVABLE (bob's
// body), not against the mitigation, so when v7 folds the UAF scope into the key
// and re-enables the Put, this arm must still pass — then proving the KEY
// separates them rather than that nothing is cached.

package dispatchers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/jwtutil"
	templatesv1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/rbac"
	"github.com/krateo-platformops/snowplow/internal/resolvers/restactions"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

const (
	a1Group   = "portal"    // the group BOTH users present — the shared grant
	a1Alice   = "alice"     // narrowed to tenant-a
	a1Bob     = "bob"       // narrowed to tenant-b
	a1TenantA = "tenant-a"  // the object alice may read and bob may not
	a1TenantB = "tenant-b"  // the object bob may read and alice may not
	a1RAName2 = "ns-lister" // the UAF-bearing RESTAction both users dispatch
)

// a1BuildTwoTenantWatcher publishes the exact RBAC shape both red-teams used to
// reproduce A-1:
//
//   - ONE ClusterRoleBinding granting `get restactions` to Group:portal. It is
//     the FIRST-MATCH dispatch-authorizing binding for BOTH users, so both fold
//     the SAME BindingUID into the key — this is what makes the cell shared.
//   - DIVERGENT per-namespace RoleBindings (alice in tenant-a, bob in tenant-b)
//     granting `get configmaps`. These are what the userAccessFilter stage
//     re-evaluates PER OBJECT, and they are invisible to the key.
//
// The shape is deliberately SYMMETRIC (one CRB via the shared group, one RB
// each) so the per-subject RBACSubGen the key also folds comes out equal for the
// two users — the arm asserts that equality rather than assuming it.
func a1BuildTwoTenantWatcher(t *testing.T) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)
	cache.ResetDepsForTest()
	t.Cleanup(cache.ResetDepsForTest)

	crbGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	crGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	rbGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
	rGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}

	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)
	listKinds := map[schema.GroupVersionResource]string{
		h1RAGVR: "RESTActionList",
		crbGVR:  "ClusterRoleBindingList",
		crGVR:   "ClusterRoleList",
		rbGVR:   "RoleBindingList",
		rGVR:    "RoleList",
	}

	cmRule := []rbacv1.PolicyRule{{Verbs: []string{"get", "list"}, APIGroups: []string{""}, Resources: []string{"configmaps"}}}
	seed := []runtime.Object{
		// The SHARED dispatch grant — one CRB, via the group both users present.
		&rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "ra-reader"},
			Rules: []rbacv1.PolicyRule{{
				Verbs: []string{"get", "list"}, APIGroups: []string{h1RAGVR.Group}, Resources: []string{h1RAGVR.Resource},
			}},
		},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "portal-ra-bind", UID: types.UID("uid-portal-shared")},
			Subjects:   []rbacv1.Subject{{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: a1Group}},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "ra-reader"},
		},
		// The DIVERGENT per-object grants the UAF stage re-evaluates.
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: a1TenantA, Name: "cm-reader"}, Rules: cmRule},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: a1TenantA, Name: "alice-cm", UID: types.UID("uid-rb-alice")},
			Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: a1Alice}},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "cm-reader"},
		},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: a1TenantB, Name: "cm-reader"}, Rules: cmRule},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: a1TenantB, Name: "bob-cm", UID: types.UID("uid-rb-bob")},
			Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: a1Bob}},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "cm-reader"},
		},
	}

	wctx, wcancel := context.WithCancel(context.Background())
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, seed...)
	rw, err := cache.NewResourceWatcher(wctx, dyn)
	if err != nil {
		wcancel()
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(ctx, 5*time.Second); err != nil {
		rw.Stop()
		wcancel()
		t.Fatalf("WaitForCacheSync: %v", err)
	}
	// Synchronous publish — NewResourceWatcher's first snapshot is published from
	// an async goroutine that races WaitForCacheSync (the #158 evaltest flake).
	cache.RebuildRBACSnapshotForTest(rw)
	prev := cache.Global()
	cache.SetGlobal(rw)
	t.Cleanup(func() {
		rw.Stop()
		wcancel()
		cache.SetGlobal(prev)
		cache.PublishRBACSnapshotForTest(nil)
	})
}

// a1UserCtx is a /call request context for one of the two co-bound users. Both
// present the SAME group — the grant that makes them share a binding.
func a1UserCtx(user string) context.Context {
	return xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: user, Groups: []string{a1Group}}))
}

// a1UAFResolve is the resolver seam standing in for restactions.Resolve. It
// implements the userAccessFilter contract for real: it enumerates the same two
// candidate namespaces for every requester and keeps each one ONLY if the REAL
// rbac.EvaluateRBAC permits THIS requester `get configmaps` in it — the same
// call refilter.go's evalSingle makes per object. Nothing about the output is
// keyed to a username literal, so the divergence between alice's and bob's
// bodies is produced by RBAC, not by the fixture.
func a1UAFResolve(t *testing.T) func(context.Context, restactions.ResolveOptions) (*templatesv1.RESTAction, error) {
	t.Helper()
	return func(ctx context.Context, _ restactions.ResolveOptions) (*templatesv1.RESTAction, error) {
		ui, err := xcontext.UserInfo(ctx)
		if err != nil {
			t.Fatalf("resolver seam: the handler must pass the requester's identity down to the resolver; got %v", err)
		}
		kept := []string{}
		for _, ns := range []string{a1TenantA, a1TenantB} {
			allowed, _, err := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
				Username: ui.Username, Groups: ui.Groups,
				Verb: "get", Group: "", Resource: "configmaps", Namespace: ns,
			})
			if err != nil {
				t.Fatalf("resolver seam: per-object EvaluateRBAC(%s, %s): %v", ui.Username, ns, err)
			}
			if allowed {
				kept = append(kept, ns)
			}
		}
		raw, err := json.Marshal(map[string]any{"namespaces": kept})
		if err != nil {
			t.Fatalf("resolver seam: marshal narrowed status: %v", err)
		}
		out := &templatesv1.RESTAction{Status: &runtime.RawExtension{Raw: raw}}
		out.SetName(a1RAName2)
		out.SetNamespace(h1NS)
		return out, nil
	}
}

// TestA1_UAFCrossUser_NoSharedCellServe — THE acceptance falsifier for A-1.
func TestA1_UAFCrossUser_NoSharedCellServe(t *testing.T) {
	a1BuildTwoTenantWatcher(t)
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)

	aliceCtx, bobCtx := a1UserCtx(a1Alice), a1UserCtx(a1Bob)

	// --- PRECONDITION (i): the per-object verdicts genuinely DIVERGE. --------
	verdict := func(ctx context.Context, user, ns string) bool {
		t.Helper()
		allowed, _, err := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
			Username: user, Groups: []string{a1Group},
			Verb: "get", Group: "", Resource: "configmaps", Namespace: ns,
		})
		if err != nil {
			t.Fatalf("PRECONDITION (i): EvaluateRBAC(%s, %s): %v", user, ns, err)
		}
		return allowed
	}
	if !verdict(aliceCtx, a1Alice, a1TenantA) || verdict(aliceCtx, a1Alice, a1TenantB) {
		t.Fatalf("PRECONDITION (i) FAILED: alice must be permitted in %s and DENIED in %s (got %v/%v). "+
			"Without divergent per-object verdicts the two users' UAF bodies would be identical and this arm "+
			"could not detect a cross-user serve.",
			a1TenantA, a1TenantB, verdict(aliceCtx, a1Alice, a1TenantA), verdict(aliceCtx, a1Alice, a1TenantB))
	}
	if !verdict(bobCtx, a1Bob, a1TenantB) || verdict(bobCtx, a1Bob, a1TenantA) {
		t.Fatalf("PRECONDITION (i) FAILED: bob must be permitted in %s and DENIED in %s (got %v/%v)",
			a1TenantB, a1TenantA, verdict(bobCtx, a1Bob, a1TenantB), verdict(bobCtx, a1Bob, a1TenantA))
	}

	// --- PRECONDITION (ii): they share ONE cell — equal non-empty BindingUID
	// AND byte-equal derived keys, both from the PRODUCTION derivation. -------
	aliceKey, handle, aliceIn := dispatchCacheLookupKey(aliceCtx, "restactions",
		h1RAGVR.Group, h1RAGVR.Version, h1RAGVR.Resource, h1NS, a1RAName2, -1, -1, nil)
	bobKey, _, bobIn := dispatchCacheLookupKey(bobCtx, "restactions",
		h1RAGVR.Group, h1RAGVR.Version, h1RAGVR.Resource, h1NS, a1RAName2, -1, -1, nil)
	if handle == nil || aliceIn == nil || bobIn == nil {
		t.Fatalf("PRECONDITION (ii): expected a live cache handle and derived inputs; handle=%v alice=%v bob=%v",
			handle != nil, aliceIn != nil, bobIn != nil)
	}
	if aliceIn.BindingUID == "" || bobIn.BindingUID == "" {
		t.Fatalf("PRECONDITION (ii) FAILED: both users must derive a NON-EMPTY first-match BindingUID "+
			"(an empty one is declined by the #95 guard for an unrelated reason, which would make this arm vacuous); "+
			"alice=%q bob=%q", aliceIn.BindingUID, bobIn.BindingUID)
	}
	if aliceIn.BindingUID != bobIn.BindingUID {
		t.Fatalf("PRECONDITION (ii) FAILED: the two users must share the SAME first-match binding (that sharing IS "+
			"the defect); alice=%q bob=%q", aliceIn.BindingUID, bobIn.BindingUID)
	}
	if aliceKey != bobKey {
		t.Fatalf("PRECONDITION (ii) FAILED: the two users' PRODUCTION-derived cache keys must be IDENTICAL — that "+
			"single shared cell is what one user's narrowed body would be written into and the other served from. "+
			"alice=%q bob=%q (subgen alice=%d bob=%d)", aliceKey, bobKey, aliceIn.RBACSubGen, bobIn.RBACSubGen)
	}
	if _, ok := handle.Get(aliceKey); ok {
		t.Fatalf("PRECONDITION: the shared key must be cold before alice's request")
	}

	// --- Drive the REAL dispatch: alice first (the would-be poisoner), then
	// bob (the victim). Same CR, same coordinates, same key. ------------------
	serve := func(t *testing.T, ctx context.Context) *httptest.ResponseRecorder {
		t.Helper()
		cr := a1RAUnstructured(true) // UAF-bearing
		cr.SetName(a1RAName2)
		restore := installRAFakes(t, cr, func() bool { return true }, a1UAFResolve(t))
		defer restore()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/call", nil).WithContext(ctx)
		RESTAction().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("dispatch must serve 200; got %d body=%s", rec.Code, rec.Body.String())
		}
		return rec
	}

	aliceRec := serve(t, aliceCtx)
	if !bytes.Contains(aliceRec.Body.Bytes(), []byte(a1TenantA)) || bytes.Contains(aliceRec.Body.Bytes(), []byte(a1TenantB)) {
		t.Fatalf("SETUP CHECK: alice's own body must contain %s and not %s (the UAF narrowing must actually be "+
			"happening, else there is nothing to leak); got %s", a1TenantA, a1TenantB, aliceRec.Body.String())
	}

	bobRec := serve(t, bobCtx)
	body := bobRec.Body.Bytes()

	// --- THE ACCEPTANCE ASSERTIONS, on the bytes that left the handler. ------
	if bytes.Contains(body, []byte(a1TenantA)) {
		t.Fatalf("A-1 CROSS-TENANT LEAK: bob's response contains %q — a namespace his OWN RBAC denies him. "+
			"He was served alice's userAccessFilter-narrowed rows out of the shared cell at key %q (both users fold "+
			"the same BindingUID %q and the hit path writes entry.RawJSON verbatim with no re-narrowing). "+
			"bob's body: %s", a1TenantA, aliceKey, aliceIn.BindingUID, bobRec.Body.String())
	}
	if !bytes.Contains(body, []byte(a1TenantB)) {
		t.Fatalf("A-1: bob's response is MISSING %q, which his own RBAC permits — he must be served his own "+
			"correctly-narrowed result, not merely denied alice's. body: %s", a1TenantB, bobRec.Body.String())
	}

	// --- And the mechanism that achieved it: nothing was cached. -------------
	if entry, ok := handle.Get(aliceKey); ok {
		t.Fatalf("A-1: the shared cell must stay EMPTY for a UAF-bearing RESTAction; found %q under %q",
			entry.RawJSON, aliceKey)
	}
	if got := cache.RestactionsUAFPutDeclined(); got != 2 {
		t.Fatalf("A-1: both dispatches must have declined their Put through the shared gate; counter=%d, want 2", got)
	}
}
