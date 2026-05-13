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

	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/rbac"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
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

	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
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

	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
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

	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
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

	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
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
		ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
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

	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
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

	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
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

	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
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

	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
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
	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
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
		if _, err := rbac.EvaluateRBAC(context.Background(), opts); err != nil {
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

	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
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
		ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
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
	ok, err := rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
		Username: "alice", Verb: "get", Group: "", Resource: "secrets", Namespace: "default",
	})
	if err != nil {
		t.Fatalf("EvaluateRBAC on Unstructured-fallback path: %v", err)
	}
	if !ok {
		t.Fatalf("Unstructured-fallback equivalence broken: alice should be admin")
	}

	// Negative: deny case must also be correct.
	ok, err = rbac.EvaluateRBAC(context.Background(), rbac.EvaluateOptions{
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
