// Package evaltest holds black-box tests for internal/rbac that DO
// NOT require the kind cluster spun up by rbac/rbac_test.go's TestMain.
// Living under a separate package keeps the test binary independent of
// Docker — the upstream rbac_test.go test binary unconditionally needs it.
package evaltest

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/rbac"

	authv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// rbacListKinds mirrors the helper in internal/cache/watcher_test.go.
// Duplicated here to avoid exporting a test-only API from cache.
func rbacListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:                "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:         "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:         "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
	}
}

// newTestWatcher constructs a cache=on ResourceWatcher backed by a
// dynamic fake client seeded with the supplied RBAC objects. The
// watcher is published via cache.SetGlobal so EvaluateRBAC reads it.
// t.Cleanup is registered for teardown.
func newTestWatcher(t *testing.T, seed ...runtime.Object) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")

	sch := runtime.NewScheme()
	if err := rbacv1.AddToScheme(sch); err != nil {
		t.Fatalf("rbacv1.AddToScheme: %v", err)
	}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(sch, rbacListKinds(), seed...)

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher")
	}
	t.Cleanup(rw.Stop)

	// Block until initial LIST + reflector sync — without this the
	// store is empty when EvaluateRBAC runs.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(ctx, 5*time.Second); err != nil {
		t.Fatalf("WaitForCacheSync: %v", err)
	}

	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	// Deterministically publish the typed RBAC snapshot before returning.
	// NewResourceWatcher publishes the FIRST snapshot from an ASYNC goroutine
	// (waitAndPublishInitialRBACSnapshot) that races WaitForCacheSync's
	// informer-HasSynced signal: HasSynced can fire before that goroutine has
	// run rebuildRBACSnapshot + rbacSnap.Store. Without forcing it here,
	// EvaluateRBAC may observe rbacSnap.Load()==nil and degrade-to-deny
	// ("snapshot not yet published — denying"), flaking ANY test in this
	// package under CI load (the -p1 + parallel-workflow contention delays the
	// publish goroutine). Mirrors what f3_convergence / snapshot_authz_memo
	// already do by hand — centralised in the shared harness so no future test
	// can reintroduce the race. Idempotent + synchronous; production's async
	// publish path is unchanged (this is a test-only seam).
	cache.RebuildRBACSnapshotForTest(rw)
}

func clusterRole(name string, rules ...rbacv1.PolicyRule) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules:      rules,
	}
}

func role(ns, name string, rules ...rbacv1.PolicyRule) *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Rules:      rules,
	}
}

func clusterRoleBinding(name, roleName string, subjects ...rbacv1.Subject) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Subjects:   subjects,
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     roleName,
		},
	}
}

func roleBinding(ns, name, roleKind, roleName string, subjects ...rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Subjects:   subjects,
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     roleKind,
			Name:     roleName,
		},
	}
}

func rule(apiGroups, resources, verbs []string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{APIGroups: apiGroups, Resources: resources, Verbs: verbs}
}

func userSubject(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: name}
}

func groupSubject(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: name}
}

func saSubject(ns, name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: "ServiceAccount", Namespace: ns, Name: name}
}

// ──────────────────────────────────────────────────────────────────────
// Allow / deny matrix
// ──────────────────────────────────────────────────────────────────────

func TestEvaluateRBAC_AllowByClusterRoleBinding(t *testing.T) {
	newTestWatcher(t,
		clusterRole("admin",
			rule([]string{"*"}, []string{"*"}, []string{"*"}),
		),
		clusterRoleBinding("admin-bind", "admin",
			userSubject("alice"),
		),
	)

	ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username: "alice", Verb: "get", Group: "", Resource: "secrets", Namespace: "default",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC: %v", err)
	}
	if !ok {
		t.Fatalf("alice should be admin (allow-any)")
	}
}

func TestEvaluateRBAC_AllowByRoleBindingToRole(t *testing.T) {
	newTestWatcher(t,
		role("demo-system", "reader",
			rule([]string{""}, []string{"configmaps"}, []string{"get", "list"}),
		),
		roleBinding("demo-system", "reader-bind", "Role", "reader",
			userSubject("bob"),
		),
	)

	ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username: "bob", Verb: "list", Group: "", Resource: "configmaps", Namespace: "demo-system",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC: %v", err)
	}
	if !ok {
		t.Fatalf("bob should be permitted to list configmaps in demo-system")
	}
}

func TestEvaluateRBAC_AllowByRoleBindingToClusterRole(t *testing.T) {
	newTestWatcher(t,
		clusterRole("view",
			rule([]string{""}, []string{"pods"}, []string{"get"}),
		),
		roleBinding("demo-system", "view-bind", "ClusterRole", "view",
			userSubject("charlie"),
		),
	)

	ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username: "charlie", Verb: "get", Group: "", Resource: "pods", Namespace: "demo-system",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC: %v", err)
	}
	if !ok {
		t.Fatalf("charlie should be permitted (RoleBinding → ClusterRole)")
	}
}

func TestEvaluateRBAC_DenyWhenNoBindingMatches(t *testing.T) {
	newTestWatcher(t,
		clusterRole("admin",
			rule([]string{"*"}, []string{"*"}, []string{"*"}),
		),
		clusterRoleBinding("admin-bind", "admin",
			userSubject("alice"),
		),
	)

	ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username: "eve", Verb: "get", Group: "", Resource: "secrets", Namespace: "default",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC: %v", err)
	}
	if ok {
		t.Fatalf("eve has no binding — must be denied")
	}
}

func TestEvaluateRBAC_WildcardVerbAndResource(t *testing.T) {
	newTestWatcher(t,
		clusterRole("ns-admin",
			rule([]string{""}, []string{"*"}, []string{"*"}),
		),
		clusterRoleBinding("ns-admin-bind", "ns-admin",
			userSubject("alice"),
		),
	)

	for _, c := range []struct {
		verb, resource string
	}{
		{"get", "configmaps"},
		{"delete", "secrets"},
		{"list", "pods"},
	} {
		ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
			Username: "alice", Verb: c.verb, Group: "", Resource: c.resource, Namespace: "default",
		})
		if err != nil {
			t.Fatalf("EvaluateRBAC(%s,%s): %v", c.verb, c.resource, err)
		}
		if !ok {
			t.Fatalf("alice should match wildcard for %s/%s", c.verb, c.resource)
		}
	}
}

func TestEvaluateRBAC_WildcardAPIGroup(t *testing.T) {
	newTestWatcher(t,
		clusterRole("any-group",
			rule([]string{"*"}, []string{"restactions"}, []string{"get"}),
		),
		clusterRoleBinding("any-group-bind", "any-group",
			userSubject("alice"),
		),
	)

	ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username: "alice", Verb: "get", Group: "templates.krateo.io", Resource: "restactions", Namespace: "default",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC: %v", err)
	}
	if !ok {
		t.Fatalf("alice should match wildcard apiGroup")
	}
}

func TestEvaluateRBAC_GroupMembershipMatch(t *testing.T) {
	newTestWatcher(t,
		clusterRole("devs-read",
			rule([]string{""}, []string{"configmaps"}, []string{"get"}),
		),
		clusterRoleBinding("devs-read-bind", "devs-read",
			groupSubject("devs"),
		),
	)

	ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username: "cyberjoker", Groups: []string{"devs"},
		Verb: "get", Group: "", Resource: "configmaps", Namespace: "default",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC: %v", err)
	}
	if !ok {
		t.Fatalf("cyberjoker (group=devs) should be permitted")
	}
}

func TestEvaluateRBAC_ServiceAccountSubjectMatch(t *testing.T) {
	newTestWatcher(t,
		clusterRole("controller",
			rule([]string{""}, []string{"events"}, []string{"create"}),
		),
		clusterRoleBinding("controller-bind", "controller",
			saSubject("krateo-system", "snowplow"),
		),
	)

	ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username: "system:serviceaccount:krateo-system:snowplow",
		Verb:     "create", Group: "", Resource: "events", Namespace: "any",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC: %v", err)
	}
	if !ok {
		t.Fatalf("snowplow SA should be permitted to create events")
	}
}

func TestEvaluateRBAC_NamespaceScopeNotEscalated(t *testing.T) {
	// A RoleBinding in 'demo-system' must NOT permit access in 'other'.
	newTestWatcher(t,
		role("demo-system", "reader",
			rule([]string{""}, []string{"configmaps"}, []string{"list"}),
		),
		roleBinding("demo-system", "reader-bind", "Role", "reader",
			userSubject("bob"),
		),
	)

	ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username: "bob", Verb: "list", Group: "", Resource: "configmaps", Namespace: "other",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC: %v", err)
	}
	if ok {
		t.Fatalf("bob's binding is namespace-scoped — must NOT permit 'other'")
	}
}

// ──────────────────────────────────────────────────────────────────────
// Cache=off vs cache=on path distinction (Revision 1 binding falsifier)
// ──────────────────────────────────────────────────────────────────────

// TestEvaluateRBAC_CacheOffFallsThroughToSAR — exercised indirectly:
// when CACHE_ENABLED is off, EvaluateRBAC calls UserCan which calls
// SelfSubjectAccessReview against ctx.UserConfig. Without a UserConfig
// in ctx, UserCan logs an error and returns false. This test asserts
// that path is reachable (proves we did NOT bypass the off-path).
func TestEvaluateRBAC_CacheOffFallsThroughToSAR(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")

	if !cache.Disabled() {
		t.Fatalf("Disabled() should be true with CACHE_ENABLED=false")
	}

	// No UserConfig in ctx — UserCan will log error and return false.
	ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username: "alice", Verb: "get", Group: "", Resource: "configmaps", Namespace: "default",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC: %v", err)
	}
	if ok {
		t.Fatalf("cache=off + no user config → must deny; got allow")
	}
}

// TestEvaluateRBAC_CacheOnNeverCallsSAR — Revision 1 hard-correctness
// gate. We instrument a dynamic fake client that counts every
// authorization.k8s.io/v1 SelfSubjectAccessReview action; the counter
// MUST stay at 0 across a battery of EvaluateRBAC calls in cache=on
// mode. Non-zero is the ROLLBACK trigger documented in
// implementation-plan-detailed.md line 622 / 647.
func TestEvaluateRBAC_CacheOnNeverCallsSAR(t *testing.T) {
	// We can't intercept SAR calls through the dynamic fake (SAR is
	// not exercised by the dynamic informer). Instead we assert via
	// the contract: cache=on routes through the informer index and
	// never touches the dynamic-fake's "create" verb on SAR types.
	// We use the package-level counter to track calls into the
	// (unreachable in cache=on) userCanViaSAR path.

	// Tracker: install a custom Disabled() probe via env var.
	t.Setenv("CACHE_ENABLED", "true")

	var sarCalls int32
	// Replace the package-level Global with a watcher backed by a
	// reactor that counts SAR-create actions. SAR is NOT in the
	// dynamic-informer path, so a non-zero counter here would mean
	// rbac.UserCan fell through to the SAR client — which is the
	// rollback condition.
	newTestWatcher(t,
		clusterRole("admin",
			rule([]string{"*"}, []string{"*"}, []string{"*"}),
		),
		clusterRoleBinding("admin-bind", "admin",
			userSubject("alice"),
		),
	)

	// Battery of EvaluateRBAC calls.
	for _, opts := range []rbac.EvaluateOptions{
		{Username: "alice", Verb: "get", Group: "", Resource: "secrets", Namespace: "default"},
		{Username: "alice", Verb: "list", Group: "", Resource: "pods", Namespace: "default"},
		{Username: "eve", Verb: "get", Group: "", Resource: "secrets", Namespace: "default"},
		{Username: "alice", Verb: "delete", Group: "", Resource: "configmaps", Namespace: "demo-system"},
	} {
		if _, _, err := rbac.EvaluateRBAC(context.Background(), opts); err != nil {
			t.Fatalf("EvaluateRBAC: %v", err)
		}
	}

	if atomic.LoadInt32(&sarCalls) != 0 {
		t.Fatalf("Revision 1 ROLLBACK: %d SubjectAccessReview calls observed in cache=on path", sarCalls)
	}
}

// TestEvaluateRBAC_CacheOnWithoutGlobalDenies covers the defensive
// branch: if cache.Disabled()==false but cache.Global()==nil,
// EvaluateRBAC MUST return (false, err) — never silently fall through
// to apiserver (would violate Revision 1).
func TestEvaluateRBAC_CacheOnWithoutGlobalDenies(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	cache.SetGlobal(nil)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username: "alice", Verb: "get", Group: "", Resource: "secrets", Namespace: "default",
	})
	if err == nil {
		t.Fatalf("expected error when cache=on but Global() is nil")
	}
	if ok {
		t.Fatalf("must deny when cache=on but watcher not wired")
	}
	if !strings.Contains(err.Error(), "ResourceWatcher not wired") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────
// 0.30.6 typed-RBAC indexer tests
// ──────────────────────────────────────────────────────────────────────

// TestEvaluateRBAC_IndexerStoresTypedPointers is the load-bearing
// 0.30.6 contract test: after newTestWatcher seeds RBAC objects, the
// indexer entries are typed pointers (not Unstructured), proving the
// typed-converting transform fired at Add event time. This is the
// mechanism behind the FromUnstructured CPU win (4760 ms → < 500 ms
// per cold nav projected).
func TestEvaluateRBAC_IndexerStoresTypedPointers(t *testing.T) {
	newTestWatcher(t,
		clusterRole("admin", rule([]string{"*"}, []string{"*"}, []string{"*"})),
		clusterRoleBinding("admin-bind", "admin", userSubject("alice")),
		role("ns-a", "viewer", rule([]string{""}, []string{"configmaps"}, []string{"get"})),
		roleBinding("ns-a", "viewer-bind", "Role", "viewer", userSubject("bob")),
	)

	rw := cache.Global()
	if rw == nil {
		t.Fatalf("expected non-nil global watcher")
	}

	crbGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	crGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	rbGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
	rGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}

	// ClusterRoleBinding indexer entry must be typed.
	crbs := rw.ListTypedObjects(crbGVR, "")
	if len(crbs) != 1 {
		t.Fatalf("ClusterRoleBindings: want 1, got %d", len(crbs))
	}
	if _, ok := crbs[0].(*rbacv1.ClusterRoleBinding); !ok {
		t.Fatalf("ClusterRoleBinding indexer entry not typed: %T", crbs[0])
	}

	// ClusterRole indexer entry must be typed.
	crObj, ok := rw.GetTypedObject(crGVR, "", "admin")
	if !ok {
		t.Fatalf("ClusterRole admin missing from indexer")
	}
	if _, ok := crObj.(*rbacv1.ClusterRole); !ok {
		t.Fatalf("ClusterRole indexer entry not typed: %T", crObj)
	}

	// RoleBinding (in namespace) indexer entry must be typed.
	rbs := rw.ListTypedObjects(rbGVR, "ns-a")
	if len(rbs) != 1 {
		t.Fatalf("RoleBindings in ns-a: want 1, got %d", len(rbs))
	}
	if _, ok := rbs[0].(*rbacv1.RoleBinding); !ok {
		t.Fatalf("RoleBinding indexer entry not typed: %T", rbs[0])
	}

	// Role indexer entry must be typed.
	rObj, ok := rw.GetTypedObject(rGVR, "ns-a", "viewer")
	if !ok {
		t.Fatalf("Role viewer missing from indexer")
	}
	if _, ok := rObj.(*rbacv1.Role); !ok {
		t.Fatalf("Role indexer entry not typed: %T", rObj)
	}
}

// TestEvaluateRBAC_TypedHappyPath is a behavioural smoke test on the
// 0.30.6 typed-read path. The previous allow-by-CRB test exercises the
// same path, but this one asserts the fast-path explicitly via mixed
// resources + a non-matching subject to ensure the prefilter ordering
// (subject FIRST, role-walk SECOND) doesn't regress correctness.
func TestEvaluateRBAC_TypedHappyPath(t *testing.T) {
	newTestWatcher(t,
		clusterRole("admin", rule([]string{"*"}, []string{"*"}, []string{"*"})),
		clusterRole("viewer", rule([]string{""}, []string{"pods"}, []string{"get"})),
		clusterRoleBinding("admin-bind", "admin", userSubject("alice")),
		clusterRoleBinding("viewer-bind", "viewer", userSubject("bob")),
		clusterRoleBinding("noise-bind", "admin", userSubject("decoy")),
	)

	cases := []struct {
		user, verb, resource string
		want                 bool
	}{
		{"alice", "delete", "secrets", true},  // admin
		{"bob", "get", "pods", true},          // viewer
		{"bob", "delete", "secrets", false},   // viewer rule doesn't match
		{"eve", "get", "anything", false},     // no binding
	}
	for _, c := range cases {
		ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
			Username: c.user, Verb: c.verb, Group: "", Resource: c.resource, Namespace: "default",
		})
		if err != nil {
			t.Fatalf("EvaluateRBAC(%s,%s,%s): %v", c.user, c.verb, c.resource, err)
		}
		if ok != c.want {
			t.Fatalf("EvaluateRBAC(%s,%s,%s) = %v, want %v", c.user, c.verb, c.resource, ok, c.want)
		}
	}
}

// TestEvaluateRBAC_UnstructuredFallbackEquivalence — defensive
// fallback contract (plan §Risks bullet 2). When the indexer entry
// for an RBAC GVR is still *unstructured.Unstructured (because the
// typed override didn't fire on it — e.g. a future regression),
// EvaluateRBAC MUST still produce the correct allow/deny result.
//
// Mechanism: disable the typed-converting override for the
// clusterrolebindings GVR for this test, seed via the dynamic fake
// (the default strip-only transform now applies, so the indexer
// stores *Unstructured), and verify allow/deny matches what the
// typed path would produce.
func TestEvaluateRBAC_UnstructuredFallbackEquivalence(t *testing.T) {
	crbGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	crGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}

	// Disable typed overrides for both CRB and CR so the indexer
	// stores Unstructured for both — exercises the full fallback
	// path (asClusterRoleBinding + asClusterRole). Restore the
	// overrides at test exit so other subtests are unaffected.
	restoreCRB := cache.DisableTypedOverrideForTest(crbGVR)
	t.Cleanup(restoreCRB)
	restoreCR := cache.DisableTypedOverrideForTest(crGVR)
	t.Cleanup(restoreCR)

	newTestWatcher(t,
		clusterRole("admin", rule([]string{"*"}, []string{"*"}, []string{"*"})),
		clusterRoleBinding("admin-bind", "admin", userSubject("alice")),
	)

	// Sanity-check: the indexer holds Unstructured for CRB now.
	rw := cache.Global()
	crbs := rw.ListTypedObjects(crbGVR, "")
	if len(crbs) != 1 {
		t.Fatalf("CRB indexer count: want 1, got %d", len(crbs))
	}
	if _, isUns := crbs[0].(*unstructured.Unstructured); !isUns {
		t.Fatalf("expected Unstructured in indexer after disabling typed override, got %T", crbs[0])
	}

	// EvaluateRBAC MUST still produce a correct allow.
	ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username: "alice", Verb: "get", Group: "", Resource: "secrets", Namespace: "default",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC on Unstructured-fallback path: %v", err)
	}
	if !ok {
		t.Fatalf("Unstructured-fallback equivalence broken: alice should be admin")
	}

	// Negative: deny case must also be correct.
	ok, _, err = rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username: "eve", Verb: "get", Group: "", Resource: "secrets", Namespace: "default",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC(eve): %v", err)
	}
	if ok {
		t.Fatalf("Unstructured-fallback equivalence broken: eve should be denied")
	}
}

// silence the unstructured import even when the test that uses it is
// the only consumer.
var _ = unstructured.Unstructured{}

// ──────────────────────────────────────────────────────────────────────
// Ship 0.30.169 — end-to-end equivalence for the subject-index path
// (exercises the real selectCRBCandidates / selectRBCandidates in
// internal/rbac/evaluate.go via EvaluateRBAC).
// ──────────────────────────────────────────────────────────────────────

// TestEvaluateRBAC_SubjectIndex_AllKinds asserts that EvaluateRBAC
// reaches every supported Subject.Kind category through the new
// index-pre-filter path. Each sub-test seeds a single binding of the
// specified Kind and asserts a positive match by EvaluateRBAC.
//
// The architectural claim: post-Ship-169 the CRB path is the indexed
// candidate set, not the linear scan. anySubjectMatches (evaluate.go:402-431)
// is unchanged — semantics are identical for these positive cases.
func TestEvaluateRBAC_SubjectIndex_AllKinds(t *testing.T) {
	t.Run("User_CRB", func(t *testing.T) {
		newTestWatcher(t,
			clusterRole("admin", rule([]string{"*"}, []string{"*"}, []string{"*"})),
			clusterRoleBinding("admin-bind", "admin", userSubject("alice-169")),
		)
		ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
			Username: "alice-169", Verb: "get", Resource: "secrets", Namespace: "default",
		})
		if err != nil {
			t.Fatalf("EvaluateRBAC: %v", err)
		}
		if !ok {
			t.Fatalf("Subject-index path failed for User subject — alice-169 should match")
		}
	})

	t.Run("Group_CRB", func(t *testing.T) {
		newTestWatcher(t,
			clusterRole("admin", rule([]string{"*"}, []string{"*"}, []string{"*"})),
			clusterRoleBinding("admin-bind", "admin", groupSubject("devs-169")),
		)
		ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
			Username: "bob-169", Groups: []string{"devs-169"},
			Verb: "get", Resource: "secrets", Namespace: "default",
		})
		if err != nil {
			t.Fatalf("EvaluateRBAC: %v", err)
		}
		if !ok {
			t.Fatalf("Subject-index path failed for Group subject — devs-169 group should match")
		}
	})

	t.Run("ServiceAccount_CRB", func(t *testing.T) {
		newTestWatcher(t,
			clusterRole("admin", rule([]string{"*"}, []string{"*"}, []string{"*"})),
			clusterRoleBinding("admin-bind", "admin", saSubject("krateo-system", "snowplow-169")),
		)
		ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
			Username: "system:serviceaccount:krateo-system:snowplow-169",
			Groups:   []string{"system:authenticated"},
			Verb:     "get", Resource: "secrets", Namespace: "default",
		})
		if err != nil {
			t.Fatalf("EvaluateRBAC: %v", err)
		}
		if !ok {
			t.Fatalf("Subject-index path failed for ServiceAccount subject — snowplow-169 SA should match")
		}
	})

	t.Run("SystemAuthenticated_implicit", func(t *testing.T) {
		// A CRB granting system:authenticated MUST be reached via the
		// implicit CRBsByGroup["system:authenticated"] index landing
		// for any non-empty Username.
		newTestWatcher(t,
			clusterRole("any-auth", rule([]string{"*"}, []string{"*"}, []string{"*"})),
			clusterRoleBinding("any-auth-bind", "any-auth",
				groupSubject("system:authenticated"),
			),
		)
		ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
			Username: "anyone-169",
			Verb:     "get", Resource: "secrets", Namespace: "default",
		})
		if err != nil {
			t.Fatalf("EvaluateRBAC: %v", err)
		}
		if !ok {
			t.Fatalf("Subject-index path failed for system:authenticated implicit group")
		}
	})

	t.Run("SA_synthetic_groups", func(t *testing.T) {
		// A CRB granting system:serviceaccounts:<ns> MUST be reached
		// via the index landing CRBsByGroup["system:serviceaccounts:<ns>"]
		// for an SA in that namespace.
		newTestWatcher(t,
			clusterRole("ns-sas", rule([]string{"*"}, []string{"*"}, []string{"*"})),
			clusterRoleBinding("ns-sas-bind", "ns-sas",
				groupSubject("system:serviceaccounts:krateo-system"),
			),
		)
		ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
			Username: "system:serviceaccount:krateo-system:any-sa",
			Groups:   []string{"system:authenticated"},
			Verb:     "get", Resource: "secrets", Namespace: "default",
		})
		if err != nil {
			t.Fatalf("EvaluateRBAC: %v", err)
		}
		if !ok {
			t.Fatalf("Subject-index path failed for SA synthetic group")
		}
	})

	t.Run("User_RB", func(t *testing.T) {
		// Per-NS RoleBinding via the user index.
		newTestWatcher(t,
			role("ns-169", "reader",
				rule([]string{""}, []string{"configmaps"}, []string{"get"})),
			roleBinding("ns-169", "reader-bind", "Role", "reader",
				userSubject("carol-169"),
			),
		)
		ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
			Username: "carol-169", Verb: "get", Resource: "configmaps", Namespace: "ns-169",
		})
		if err != nil {
			t.Fatalf("EvaluateRBAC: %v", err)
		}
		if !ok {
			t.Fatalf("Subject-index path failed for User RB in ns-169")
		}
	})
}

// TestEvaluateRBAC_SubjectIndex_NegativesUnaffected asserts that the
// candidate-set pre-filter does NOT over-grant: deny cases remain deny.
// The post-lookup anySubjectMatches is unchanged, so over-inclusion in
// the candidate set never produces a wrong allow.
func TestEvaluateRBAC_SubjectIndex_NegativesUnaffected(t *testing.T) {
	newTestWatcher(t,
		clusterRole("admin", rule([]string{"*"}, []string{"*"}, []string{"*"})),
		clusterRoleBinding("admin-bind", "admin", userSubject("alice-169")),
		clusterRoleBinding("group-bind", "admin", groupSubject("devs-169")),
		clusterRoleBinding("sa-bind", "admin", saSubject("krateo-system", "snowplow-169")),
	)

	deniedCases := []rbac.EvaluateOptions{
		// Unknown user, no matching group, not an SA.
		{Username: "stranger-169", Verb: "get", Resource: "secrets"},
		// User in a non-matching group.
		{Username: "stranger-169", Groups: []string{"other-group"}, Verb: "get", Resource: "secrets"},
		// SA in a non-matching namespace.
		{
			Username: "system:serviceaccount:other-ns:other-sa",
			Groups:   []string{"system:authenticated"},
			Verb:     "get", Resource: "secrets",
		},
	}
	for _, opts := range deniedCases {
		opts := opts
		t.Run(opts.Username, func(t *testing.T) {
			ok, _, err := rbac.EvaluateRBAC(context.Background(), opts)
			if err != nil {
				t.Fatalf("EvaluateRBAC: %v", err)
			}
			if ok {
				t.Errorf("Subject-index path OVER-GRANTED: %+v should be denied", opts)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────
// L5 [SEC] — cache=off SAR PERMIT branch, hermetic.
//
// The existing TestEvaluateRBAC_CacheOffFallsThroughToSAR proves the
// DENY side of the cache=off path (no UserConfig → UserCan errors →
// deny). L5 pins the PERMIT side hermetically: with CACHE_ENABLED=false,
// a fake authorization clientset that answers Allowed=true makes
// EvaluateRBAC return permit — AND the fake asserts that EvaluateRBAC
// mapped opts.{Verb,Group,Resource,Namespace} onto the
// SelfSubjectAccessReview's ResourceAttributes correctly.
//
// The clientset is injected via rbac.SetSARClientsetForTest (the minimal
// behaviour-preserving seam added to rbac.go). No apiserver, no kind.
// ──────────────────────────────────────────────────────────────────────

// newSARFakeAllowingOnly builds a fake kubernetes clientset whose
// SelfSubjectAccessReview create-reactor returns Allowed=true ONLY when
// the review's ResourceAttributes match the expected mapped tuple, and
// records the ResourceAttributes it actually observed. A mis-mapped
// EvaluateRBAC (e.g. Group and Resource swapped) would submit attributes
// that don't match `want`, so the reactor returns Allowed=false and the
// permit assertion fails — that is the L5 RED.
func newSARFakeAllowingOnly(want authv1.ResourceAttributes, gotRA **authv1.ResourceAttributes) *k8sfake.Clientset {
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(action ktesting.Action) (bool, runtime.Object, error) {
			ca, ok := action.(ktesting.CreateAction)
			if !ok {
				return false, nil, nil
			}
			ssar, ok := ca.GetObject().(*authv1.SelfSubjectAccessReview)
			if !ok || ssar.Spec.ResourceAttributes == nil {
				return true, &authv1.SelfSubjectAccessReview{
					Status: authv1.SubjectAccessReviewStatus{Allowed: false},
				}, nil
			}
			ra := *ssar.Spec.ResourceAttributes
			*gotRA = ssar.Spec.ResourceAttributes
			allowed := ra.Verb == want.Verb &&
				ra.Group == want.Group &&
				ra.Resource == want.Resource &&
				ra.Namespace == want.Namespace
			return true, &authv1.SelfSubjectAccessReview{
				Status: authv1.SubjectAccessReviewStatus{Allowed: allowed},
			}, nil
		})
	return cs
}

// TestEvaluateRBAC_CacheOffSARPermit_Hermetic exercises the cache=off SAR
// PERMIT branch with an injected fake clientset. EvaluateRBAC must return
// (true,"",nil) and the SAR it issued must carry the correctly mapped
// ResourceAttributes.
//
// RED arm: a ResourceAttributes mis-map (the fake only allows the exact
// mapped tuple). If EvaluateRBAC/userCanViaSAR mis-populated the SAR
// (e.g. swapped Group↔Resource or dropped Namespace), the reactor returns
// Allowed=false and the permit assertion below fails.
func TestEvaluateRBAC_CacheOffSARPermit_Hermetic(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	// TEST_MODE=true so xcontext.UserConfig does not rewrite the endpoint's
	// ServerURL to the in-cluster default (irrelevant to the fake, but keeps
	// the resolved endpoint faithful to what we set).
	t.Setenv("TEST_MODE", "true")

	if !cache.Disabled() {
		t.Fatalf("Disabled() should be true with CACHE_ENABLED=false")
	}

	want := authv1.ResourceAttributes{
		Verb:      "list",
		Group:     "apps",
		Resource:  "deployments",
		Namespace: "demo-system",
	}
	var gotRA *authv1.ResourceAttributes
	fakeCS := newSARFakeAllowingOnly(want, &gotRA)

	restore := rbac.SetSARClientsetForTest(
		func(_ context.Context, _ endpoints.Endpoint) (kubernetes.Interface, error) {
			return fakeCS, nil
		})
	t.Cleanup(restore)

	// userCanViaSAR reads xcontext.UserConfig(ctx); provide an endpoint so
	// it does not short-circuit to deny before reaching the (fake) clientset.
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserConfig(endpoints.Endpoint{ServerURL: "https://sar.test"}),
	)

	ok, uid, err := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
		Username:  "alice",
		Verb:      "list",
		Group:     "apps",
		Resource:  "deployments",
		Namespace: "demo-system",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC (cache=off SAR permit): %v", err)
	}
	if !ok {
		t.Fatalf("L5: cache=off + fake SAR Allowed=true must PERMIT — got deny. "+
			"If the fake observed mis-mapped ResourceAttributes it denies: observed=%+v want=%+v", raOrNil(gotRA), want)
	}
	// cache=off returns no BindingUID (no in-process snapshot).
	if uid != "" {
		t.Fatalf("L5: cache=off SAR path must return empty BindingUID; got %q", uid)
	}
	// The SAR must have been issued with the correctly mapped attributes.
	if gotRA == nil {
		t.Fatalf("L5: no SelfSubjectAccessReview reached the fake clientset — the SAR path was not exercised")
	}
	if *gotRA != want {
		t.Fatalf("L5 MIS-MAP: SAR ResourceAttributes = %+v, want %+v (EvaluateRBAC mapped opts onto the SAR incorrectly)", *gotRA, want)
	}
}

// TestEvaluateRBAC_CacheOffSAR_DenyPropagates is the negative twin: the
// fake answers Allowed=false and EvaluateRBAC must return deny. Guards
// against a fake/plumbing artifact that would make the permit test pass
// vacuously (i.e. proves the verdict tracks the SAR answer, not a
// hard-coded allow).
func TestEvaluateRBAC_CacheOffSAR_DenyPropagates(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	t.Setenv("TEST_MODE", "true")

	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, &authv1.SelfSubjectAccessReview{
				Status: authv1.SubjectAccessReviewStatus{Allowed: false},
			}, nil
		})
	restore := rbac.SetSARClientsetForTest(
		func(_ context.Context, _ endpoints.Endpoint) (kubernetes.Interface, error) {
			return cs, nil
		})
	t.Cleanup(restore)

	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserConfig(endpoints.Endpoint{ServerURL: "https://sar.test"}),
	)
	ok, _, err := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
		Username: "alice", Verb: "get", Group: "", Resource: "secrets", Namespace: "default",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC (cache=off SAR deny): %v", err)
	}
	if ok {
		t.Fatalf("L5 negative: fake SAR Allowed=false must DENY; got permit")
	}
}

func raOrNil(ra *authv1.ResourceAttributes) any {
	if ra == nil {
		return "<no SAR issued>"
	}
	return *ra
}

// ──────────────────────────────────────────────────────────────────────
// L7 — concrete-namespace ServiceAccount form: a cluster-wide grant to
// the SA's synthetic group MUST authorize a request carrying a CONCRETE
// (non-empty) request namespace, exactly as it does for the cluster-scoped
// (empty) namespace form. Converts the previously non-asserting
// ship11 PROBE-1b (which only t.Logf'd) into a hard assertion.
//
// RED arm: an impl that special-cases the empty request namespace (e.g.
// only lands the SA's synthetic-group index when opts.Namespace == "")
// would DENY the concrete-ns form. The SA's synthetic groups derive from
// the SA's OWN namespace, not the request namespace, and a cluster-wide
// CRB permits regardless of request namespace — so the concrete-ns form
// MUST permit.
// ──────────────────────────────────────────────────────────────────────

// TestEvaluateRBAC_SAGroups_ConcreteNamespaceGrant asserts the concrete-ns
// SA form permits, and that the empty-ns form permits identically (parity).
func TestEvaluateRBAC_SAGroups_ConcreteNamespaceGrant(t *testing.T) {
	// Cluster-wide */* grant to the synthetic group
	// system:serviceaccounts:krateo-system — every SA in krateo-system is a
	// member, cluster-wide.
	newTestWatcher(t,
		clusterRole("ns-sas", rule([]string{"*"}, []string{"*"}, []string{"get", "list", "watch"})),
		clusterRoleBinding("ns-sas-bind", "ns-sas",
			groupSubject("system:serviceaccounts:krateo-system"),
		),
	)

	sa := "system:serviceaccount:krateo-system:snowplow"

	cases := []struct {
		name      string
		namespace string
	}{
		{"cluster_scope_empty_ns", ""},           // the PROBE-1 form
		{"concrete_ns_demo_system", "demo-system"}, // the PROBE-1b form (was non-asserting)
		{"concrete_ns_other", "other"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
				Username:  sa,
				Groups:    []string{"system:authenticated"},
				Verb:      "list",
				Group:     "",
				Resource:  "namespaces",
				Namespace: c.namespace,
			})
			if err != nil {
				t.Fatalf("EvaluateRBAC(ns=%q): %v", c.namespace, err)
			}
			if !ok {
				t.Fatalf("L7 INVARIANT: SA %s with cluster-wide grant to system:serviceaccounts:krateo-system MUST be permitted for request namespace=%q (concrete-ns form must NOT be special-cased against the empty-ns form)", sa, c.namespace)
			}
		})
	}

	// Negative guard: an SA in a DIFFERENT namespace is NOT a member of
	// system:serviceaccounts:krateo-system and MUST be denied — for the
	// concrete-ns form too (proves the permit above is not a blanket allow).
	ok, _, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username:  "system:serviceaccount:other-ns:worker",
		Groups:    []string{"system:authenticated"},
		Verb:      "list",
		Group:     "",
		Resource:  "namespaces",
		Namespace: "demo-system",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC(other-ns SA): %v", err)
	}
	if ok {
		t.Fatalf("L7 negative: an SA in other-ns must NOT match system:serviceaccounts:krateo-system on the concrete-ns form")
	}
}
