// refilter_test.go — Tag 0.30.9 Sub-scope A: unit tests for
// applyUserAccessFilter and the JQ + EvaluateRBAC plumbing.
//
// The tests build a cache=on ResourceWatcher seeded with RBAC types,
// publish it via cache.SetGlobal, and assert that:
//   1. permitted objects pass through;
//   2. non-permitted objects are dropped;
//   3. user identity drives the keep/drop decision (admin sees all,
//      cyberjoker sees the granted subset);
//   4. JQ NamespaceFrom drives the per-object namespace correctly;
//   5. unrecognised shapes pass through (operator alert path).
//
// Per feedback_no_special_cases.md the tests exercise GENERIC GVRs
// — no per-resource carve-outs.

package api

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func refilterRBACListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:                "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:         "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:         "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
	}
}

// newRefilterTestWatcher builds + publishes a cache=on watcher seeded
// with the supplied RBAC objects. Mirrors the helper in
// internal/rbac/evaltest. Returns nothing — callers use cache.Global()
// implicitly via EvaluateRBAC.
func newRefilterTestWatcher(t *testing.T, seed ...runtime.Object) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")

	sch := runtime.NewScheme()
	if err := rbacv1.AddToScheme(sch); err != nil {
		t.Fatalf("rbacv1.AddToScheme: %v", err)
	}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		sch, refilterRBACListKinds(), seed...)

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher")
	}
	t.Cleanup(rw.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(ctx, 5*time.Second); err != nil {
		t.Fatalf("WaitForCacheSync: %v", err)
	}

	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })
}

// ctxWithUser returns a context carrying UserInfo for username + groups.
// Mirrors the request-pipeline shape (handlers wire UserInfo via the
// auth middleware; tests wire it here directly).
func ctxWithUser(username string, groups ...string) context.Context {
	return xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: username,
			Groups:   groups,
		}),
	)
}

// TestApplyUserAccessFilter_DropsDeniedNamespaces is the load-bearing
// security test: cyberjoker has Role-Based Access Control granting
// read on compositions in namespace "bench-ns-01" only (a Role +
// RoleBinding scoped to that namespace). The SA-dispatched response
// contains three namespace names; refilter asks
// "can cyberjoker get compositions in this namespace?" against each.
// Only bench-ns-01 passes.
//
// Without refilter, the SA-dispatched response (cluster-wide read)
// would leak all three namespace names to cyberjoker — that's the
// data-leak risk Sub-scope A closes (per plan §"Risks"). The test
// asserts the refilter drops the denied entries.
//
// This mirrors the production portal pattern (cyberjoker's narrow
// RBAC scopes them to specific namespaces; the namespaces-list query
// gets refiltered to "show me only namespaces I can actually look
// inside").
func TestApplyUserAccessFilter_DropsDeniedNamespaces(t *testing.T) {
	// RBAC: namespace-scoped Role + RoleBinding granting "devs"
	// group GET on compositions in namespace bench-ns-01. cyberjoker
	// is a member of "devs".
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: "ns01-comp-reader", Namespace: "bench-ns-01"},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"get", "list", "watch"},
				APIGroups: []string{"composition.krateo.io"},
				Resources: []string{"compositions"},
			},
		},
	}
	binding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "ns01-comp-reader-binding", Namespace: "bench-ns-01"},
		Subjects: []rbacv1.Subject{
			{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "devs"},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "ns01-comp-reader",
		},
	}
	newRefilterTestWatcher(t, role, binding)

	apiCall := &templates.API{
		Name: "namespaces",
		UserAccessFilter: &templates.UserAccessFilterSpec{
			Verb:          "get",
			Group:         "composition.krateo.io",
			Resource:      "compositions",
			NamespaceFrom: ".metadata.name", // namespace == the namespace name itself
		},
	}
	dict := map[string]any{
		"namespaces": map[string]any{
			"kind":       "NamespaceList",
			"apiVersion": "v1",
			"items": []any{
				map[string]any{"metadata": map[string]any{"name": "bench-ns-01"}},
				map[string]any{"metadata": map[string]any{"name": "bench-ns-02"}},
				map[string]any{"metadata": map[string]any{"name": "bench-ns-03"}},
			},
		},
	}

	ctx := ctxWithUser("cyberjoker", "devs")
	res := applyUserAccessFilter(ctx, dict, apiCall)

	if res.Kept != 1 {
		t.Errorf("kept = %d; want 1 (bench-ns-01 only)", res.Kept)
	}
	if res.Dropped != 2 {
		t.Errorf("dropped = %d; want 2 (bench-ns-02 + bench-ns-03)", res.Dropped)
	}
	if res.EvaluateRBACCalls != 3 {
		t.Errorf("evaluate_rbac_calls = %d; want 3 (one per object)", res.EvaluateRBACCalls)
	}

	wrapper := dict["namespaces"].(map[string]any)
	items := wrapper["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items length = %d; want 1", len(items))
	}
	first := items[0].(map[string]any)
	meta := first["metadata"].(map[string]any)
	if meta["name"] != "bench-ns-01" {
		t.Errorf("retained item name = %v; want bench-ns-01", meta["name"])
	}
}

// TestApplyUserAccessFilter_AdminSeesAll covers the admin path:
// cluster-admin ClusterRole grants get/list/* on all resources. Every
// namespace passes refilter for admin.
func TestApplyUserAccessFilter_AdminSeesAll(t *testing.T) {
	role := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-admin"},
		Rules: []rbacv1.PolicyRule{
			{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}},
		},
	}
	binding := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
		Subjects: []rbacv1.Subject{
			{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "admin"},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin",
		},
	}
	newRefilterTestWatcher(t, role, binding)

	apiCall := &templates.API{
		Name: "namespaces",
		UserAccessFilter: &templates.UserAccessFilterSpec{
			Verb:          "get",
			Group:         "composition.krateo.io",
			Resource:      "compositions",
			NamespaceFrom: ".metadata.name",
		},
	}
	dict := map[string]any{
		"namespaces": map[string]any{
			"items": []any{
				map[string]any{"metadata": map[string]any{"name": "ns-a"}},
				map[string]any{"metadata": map[string]any{"name": "ns-b"}},
				map[string]any{"metadata": map[string]any{"name": "ns-c"}},
			},
		},
	}

	res := applyUserAccessFilter(ctxWithUser("admin"), dict, apiCall)
	if res.Kept != 3 {
		t.Errorf("admin kept = %d; want 3 (all)", res.Kept)
	}
	if res.Dropped != 0 {
		t.Errorf("admin dropped = %d; want 0", res.Dropped)
	}
}

// TestApplyUserAccessFilter_NoUserInfoFailsClosed asserts the
// fail-closed semantic: missing UserInfo in context produces an empty
// result set (not the full SA-dispatched response).
func TestApplyUserAccessFilter_NoUserInfoFailsClosed(t *testing.T) {
	// No RBAC seeded; even so, the function should never reach the
	// evaluator path because UserInfo extraction fails first.
	newRefilterTestWatcher(t)

	apiCall := &templates.API{
		Name: "namespaces",
		UserAccessFilter: &templates.UserAccessFilterSpec{
			Verb: "get", Group: "", Resource: "namespaces", NamespaceFrom: ".metadata.name",
		},
	}
	dict := map[string]any{
		"namespaces": map[string]any{
			"items": []any{
				map[string]any{"metadata": map[string]any{"name": "ns-a"}},
			},
		},
	}

	// Bare context — no UserInfo.
	_ = applyUserAccessFilter(context.Background(), dict, apiCall)

	wrapper := dict["namespaces"].(map[string]any)
	items := wrapper["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("fail-closed regression: items length = %d; want 0", len(items))
	}
}

// TestApplyUserAccessFilter_NilUAFIsNoOp covers the non-UAF API stage
// path: an apiCall without UserAccessFilter passes through unchanged.
func TestApplyUserAccessFilter_NilUAFIsNoOp(t *testing.T) {
	apiCall := &templates.API{Name: "namespaces"}
	dict := map[string]any{
		"namespaces": map[string]any{
			"items": []any{map[string]any{"metadata": map[string]any{"name": "ns-a"}}},
		},
	}
	res := applyUserAccessFilter(ctxWithUser("anyone"), dict, apiCall)
	if res.Kept+res.Dropped+res.EvaluateRBACCalls != 0 {
		t.Errorf("nil-UAF must be a no-op; got %+v", res)
	}
	wrapper := dict["namespaces"].(map[string]any)
	items := wrapper["items"].([]any)
	if len(items) != 1 {
		t.Errorf("nil-UAF must not mutate dict; got %d items", len(items))
	}
}

// Ship A (0.30.137): TestTrimJSONString removed alongside trimJSONString.
// The helper became dead code when evalJQString migrated to EvalValue
// (design §3.4.4 / AC-A.5). Its string/null/non-string edge-case coverage
// is subsumed by the evalJQString truth-table tests in jqvalue_test.go.

// --- Ship 0.30.129 — ResourcesFrom (RBAC-aware fan-out) -------------------

// TestResourcesFrom_MultiCRD_ORSemantics is AC-129.3: a UAF with
// resourcesFrom jq-deriving TWO synthetic composition CRD plurals from
// dict["crds"]. cyberjoker is permitted to `list` ONLY the first plural
// (compaaa) in bench-ns-01. OR semantics: a namespace is kept iff the
// user can list ANY plural there — so bench-ns-01 is KEPT (granted on
// compaaa), bench-ns-02/03 dropped (granted on neither).
func TestResourcesFrom_MultiCRD_ORSemantics(t *testing.T) {
	// RBAC: "devs" may list compaaa (only) in bench-ns-01. No grant for
	// compbbb anywhere — proves the keep came from the OR over compaaa.
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: "ns01-compaaa-lister", Namespace: "bench-ns-01"},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"list"},
				APIGroups: []string{"composition.krateo.io"},
				Resources: []string{"compaaa"},
			},
		},
	}
	binding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "ns01-compaaa-lister-binding", Namespace: "bench-ns-01"},
		Subjects: []rbacv1.Subject{
			{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "devs"},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "ns01-compaaa-lister",
		},
	}
	newRefilterTestWatcher(t, role, binding)

	apiCall := &templates.API{
		Name: "namespaces",
		UserAccessFilter: &templates.UserAccessFilterSpec{
			Verb:  "list",
			Group: "composition.krateo.io",
			// No static Resource — resourcesFrom supplies the set.
			ResourcesFrom: "[ (.crds // [])[] | .plural ]",
			NamespaceFrom: ".",
		},
	}
	// dict carries the runtime-discovered CRD plurals + the namespaces
	// stage output (a name-string array, the post-filter shape).
	dict := map[string]any{
		"crds": []any{
			map[string]any{"plural": "compaaa"},
			map[string]any{"plural": "compbbb"},
		},
		"namespaces": []any{"bench-ns-01", "bench-ns-02", "bench-ns-03"},
	}

	ctx := ctxWithUser("cyberjoker", "devs")
	res := applyUserAccessFilter(ctx, dict, apiCall)

	if res.Kept != 1 {
		t.Errorf("AC-129.3: kept = %d; want 1 (bench-ns-01 — granted on compaaa via OR)", res.Kept)
	}
	if res.Dropped != 2 {
		t.Errorf("AC-129.3: dropped = %d; want 2", res.Dropped)
	}
	kept, _ := dict["namespaces"].([]any)
	if len(kept) != 1 || kept[0] != "bench-ns-01" {
		t.Fatalf("AC-129.3: kept namespaces = %v; want [bench-ns-01]", kept)
	}
}

// TestResourcesFrom_Unset_ByteIdentical is AC-129.7: with ResourcesFrom
// unset, resolveUAFResources returns exactly [uaf.Resource] and the
// refilter behaves byte-identically to pre-0.30.129 — the static
// Resource is checked, existing UAFs unaffected.
func TestResourcesFrom_Unset_ByteIdentical(t *testing.T) {
	uaf := &templates.UserAccessFilterSpec{
		Verb: "get", Group: "composition.krateo.io", Resource: "compositions",
	}
	got, ok := resolveUAFResources(context.Background(), discardLogger(), uaf, map[string]any{})
	if !ok {
		t.Fatalf("AC-129.7: resolveUAFResources(unset) must succeed")
	}
	if len(got) != 1 || got[0] != "compositions" {
		t.Fatalf("AC-129.7: ResourcesFrom unset must yield exactly [uaf.Resource]; got %v", got)
	}
}

// TestResourcesFrom_FailClosed covers the AC-129.9 / fail-closed paths:
// a ResourcesFrom that errors, yields a non-array, or a non-string
// element returns ok=false (caller drops all); an empty array yields
// ([], true) and evalSingle then denies every item.
func TestResourcesFrom_FailClosed(t *testing.T) {
	log := discardLogger()
	dict := map[string]any{"crds": []any{map[string]any{"plural": "compaaa"}}}

	// JQ error → fail-closed.
	if _, ok := resolveUAFResources(context.Background(), log,
		&templates.UserAccessFilterSpec{ResourcesFrom: ".this | broken("}, dict); ok {
		t.Errorf("fail-closed: a ResourcesFrom JQ error must return ok=false")
	}
	// Non-array result → fail-closed.
	if _, ok := resolveUAFResources(context.Background(), log,
		&templates.UserAccessFilterSpec{ResourcesFrom: ".crds | length"}, dict); ok {
		t.Errorf("fail-closed: a non-array ResourcesFrom result must return ok=false")
	}
	// Non-string element → fail-closed.
	if _, ok := resolveUAFResources(context.Background(), log,
		&templates.UserAccessFilterSpec{ResourcesFrom: "[ (.crds // [])[] ]"}, dict); ok {
		t.Errorf("fail-closed: a non-string element must return ok=false")
	}
	// Empty array → ([], true): valid, but evalSingle then denies all.
	got, ok := resolveUAFResources(context.Background(), log,
		&templates.UserAccessFilterSpec{ResourcesFrom: "[]"}, dict)
	if !ok || len(got) != 0 {
		t.Fatalf("an empty ResourcesFrom array must yield ([], true); got %v ok=%v", got, ok)
	}
	if evalSingle(context.Background(), log, "u", []string{"g"},
		&templates.UserAccessFilterSpec{Verb: "list"}, nil, "ns-a") {
		t.Errorf("fail-closed: evalSingle with an empty resource set must DENY")
	}
}

// discardLogger is a no-op slog.Logger for the resolveUAFResources unit
// tests (they assert return values, not log output).
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
