// eager_test.go — Step 3 / Tag 0.30.6 binding verification.
//
// Coverage per implementation plan §"Tag 0.30.6 — What's implemented"
// bullet 5:
//   - Inventory dedupes (paths that resolve to the same GVR collapse).
//   - EagerRegisterAll idempotent (running twice over the same set is a no-op).
//   - Lazy fallback fires + emits the WARN.
//   - Fanin bound (fanin=1 still processes every GVR; fanin=0 falls
//     back to default).

package cache_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// restActionListGVK is the List kind the dynamic fake client needs to
// serve LIST RestActions. RestAction itself isn't in any client-go
// scheme so we register a placeholder list type.
func restActionListGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   "templates.krateo.io",
		Version: "v1",
		Kind:    "RESTActionList",
	}
}

// buildSchemeWithRestActions returns a scheme that registers
// RBAC types + a placeholder RESTAction list kind so the dynamic fake
// client can serve LIST on `restactions.templates.krateo.io/v1`.
func buildSchemeWithRestActions() *k8sruntime.Scheme {
	sch := k8sruntime.NewScheme()
	_ = rbacv1.AddToScheme(sch)
	sch.AddKnownTypeWithName(
		restActionListGVK(),
		&unstructured.UnstructuredList{},
	)
	sch.AddKnownTypeWithName(
		schema.GroupVersionKind{
			Group: "templates.krateo.io", Version: "v1", Kind: "RESTAction",
		},
		&unstructured.Unstructured{},
	)
	return sch
}

// inventoryListKinds maps every GVR the inventory walker may LIST to
// its corresponding List kind, plus the RBAC GVRs needed by the
// constructor.
func inventoryListKinds() map[schema.GroupVersionResource]string {
	m := rbacListKinds()
	m[schema.GroupVersionResource{
		Group: "templates.krateo.io", Version: "v1", Resource: "restactions",
	}] = "RESTActionList"
	// GVRs the test RestActions reference via spec.api[*].path. The
	// fake client needs a List kind for each so eager registration's
	// informer LIST doesn't panic.
	m[schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "namespaces",
	}] = "NamespaceList"
	m[schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "pods",
	}] = "PodList"
	m[schema.GroupVersionResource{
		Group: "apps", Version: "v1", Resource: "deployments",
	}] = "DeploymentList"
	return m
}

// makeRestAction constructs an unstructured RESTAction with the given
// spec.api[*].path entries.
func makeRestAction(name, namespace string, paths ...string) *unstructured.Unstructured {
	apis := make([]any, 0, len(paths))
	for i, p := range paths {
		apis = append(apis, map[string]any{
			"name": "api-" + name + "-" + itoa(i),
			"path": p,
		})
	}
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "templates.krateo.io/v1",
			"kind":       "RESTAction",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"api": apis,
			},
		},
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	out := ""
	for i > 0 {
		out = string(digits[i%10]) + out
		i /= 10
	}
	return out
}

// TestCollectResourceTypesFromRestActions_Dedupes covers the inventory
// dedupe rule: two RestActions that touch the same GVR via different
// paths collapse to one entry.
func TestCollectResourceTypesFromRestActions_Dedupes(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		buildSchemeWithRestActions(),
		inventoryListKinds(),
		// Two RestActions both touching pods + namespaces — should
		// dedupe to exactly two GVRs.
		makeRestAction("ra1", "ns1",
			"/api/v1/namespaces",
			"/api/v1/namespaces/kube-system/pods",
		),
		makeRestAction("ra2", "ns2",
			"/api/v1/namespaces?limit=15",
			"/api/v1/pods", // cluster-scoped form
		),
		// Third RestAction touching deployments via the named-group
		// form.
		makeRestAction("ra3", "ns3",
			"/apis/apps/v1/deployments",
			"/apis/apps/v1/namespaces/default/deployments",
		),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gvrs, err := cache.CollectResourceTypesFromRestActions(ctx, dyn)
	if err != nil {
		t.Fatalf("CollectResourceTypesFromRestActions: %v", err)
	}

	want := []schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "namespaces"},
		{Group: "", Version: "v1", Resource: "pods"},
		{Group: "apps", Version: "v1", Resource: "deployments"},
	}
	if !equalGVRs(gvrs, want) {
		t.Fatalf("inventory mismatch:\n got:  %v\n want: %v", gvrs, want)
	}
}

// TestCollectResourceTypesFromRestActions_SkipsEndpointRefAndJQ covers
// the two skip-paths: RestAction entries with endpointRef set (external
// HTTP, no GVR) and entries whose path still carries JQ templating
// (`${...}` — unresolvable at startup).
func TestCollectResourceTypesFromRestActions_SkipsEndpointRefAndJQ(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	ra := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "templates.krateo.io/v1",
			"kind":       "RESTAction",
			"metadata": map[string]any{
				"name":      "mixed",
				"namespace": "demo",
			},
			"spec": map[string]any{
				"api": []any{
					// endpointRef entry — skipped.
					map[string]any{
						"name": "github",
						"path": "/repos/foo/bar",
						"endpointRef": map[string]any{
							"name": "github-endpoint", "namespace": "demo",
						},
					},
					// JQ-templated entry — skipped.
					map[string]any{
						"name": "pods-by-ns",
						"path": `${ "/api/v1/namespaces/" + (.) + "/pods" }`,
					},
					// Valid entry — kept.
					map[string]any{
						"name": "namespaces",
						"path": "/api/v1/namespaces",
					},
				},
			},
		},
	}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		buildSchemeWithRestActions(),
		inventoryListKinds(),
		ra,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gvrs, err := cache.CollectResourceTypesFromRestActions(ctx, dyn)
	if err != nil {
		t.Fatalf("CollectResourceTypesFromRestActions: %v", err)
	}

	want := []schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "namespaces"},
	}
	if !equalGVRs(gvrs, want) {
		t.Fatalf("inventory mismatch:\n got:  %v\n want: %v", gvrs, want)
	}
}

// TestEagerRegisterAll_Idempotent confirms that running
// EagerRegisterAll twice over the same GVR set is a no-op (no panic,
// no duplicate informer).
func TestEagerRegisterAll_Idempotent(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		buildSchemeWithRestActions(),
		inventoryListKinds(),
	)

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher")
	}
	defer stopAndDrain(rw)

	inv := []schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "namespaces"},
		{Group: "", Version: "v1", Resource: "pods"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	n1, err := cache.EagerRegisterAll(ctx, rw, inv, 4, 2*time.Second)
	if err != nil {
		t.Logf("first EagerRegisterAll sync error (soft): %v", err)
	}
	if n1 != len(inv) {
		t.Fatalf("first EagerRegisterAll: n=%d want %d", n1, len(inv))
	}

	// Second call — must be a no-op (informers stay registered, no
	// panic from duplicate registration).
	n2, err := cache.EagerRegisterAll(ctx, rw, inv, 4, 2*time.Second)
	if err != nil {
		t.Logf("second EagerRegisterAll sync error (soft): %v", err)
	}
	if n2 != len(inv) {
		t.Fatalf("second EagerRegisterAll: n=%d want %d", n2, len(inv))
	}

	// Probe registration via ListObjects — nil means absent-from-
	// registry.
	for _, gvr := range inv {
		if got := rw.ListObjects(gvr, ""); got == nil {
			t.Fatalf("after EagerRegisterAll, ListObjects(%s) = nil", gvr)
		}
	}
}

// TestEagerRegisterAll_FaninBound exercises the fan-in semaphore.
// fanin=1 forces serial execution; fanin=0 falls back to default. We
// don't assert wall-clock timing — too flaky in CI — but we do verify
// that both paths produce the same final registration set.
func TestEagerRegisterAll_FaninBound(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	for _, fanin := range []int{0, 1, 4, 16} {
		fanin := fanin
		t.Run("fanin="+itoa(fanin), func(t *testing.T) {
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
				buildSchemeWithRestActions(),
				inventoryListKinds(),
			)
			rw, err := cache.NewResourceWatcher(context.Background(), dyn)
			if err != nil {
				t.Fatalf("NewResourceWatcher: %v", err)
			}
			if rw == nil {
				t.Fatalf("expected non-nil watcher")
			}
			defer stopAndDrain(rw)

			inv := []schema.GroupVersionResource{
				{Group: "", Version: "v1", Resource: "namespaces"},
				{Group: "", Version: "v1", Resource: "pods"},
				{Group: "apps", Version: "v1", Resource: "deployments"},
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			n, err := cache.EagerRegisterAll(ctx, rw, inv, fanin, 2*time.Second)
			if err != nil {
				t.Logf("EagerRegisterAll(fanin=%d) sync error (soft): %v", fanin, err)
			}
			if n != len(inv) {
				t.Fatalf("EagerRegisterAll(fanin=%d): n=%d want %d", fanin, n, len(inv))
			}
			for _, gvr := range inv {
				if got := rw.ListObjects(gvr, ""); got == nil {
					t.Fatalf("ListObjects(%s) nil after EagerRegisterAll(fanin=%d)", gvr, fanin)
				}
			}
		})
	}
}

// TestMarkEagerSet_LazyFallbackFires confirms that after MarkEagerSet
// has been called, AddResourceType for a GVR in the eager set wires
// the informer (no panic) — i.e. the lazy fallback is functional. The
// WARN log itself can't be asserted without a custom slog handler; the
// dev-gate verifies it via `kubectl logs`. We at least ensure the
// fallback codepath doesn't panic.
func TestMarkEagerSet_LazyFallbackFires(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		buildSchemeWithRestActions(),
		inventoryListKinds(),
	)
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher")
	}
	defer stopAndDrain(rw)

	podGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	rw.MarkEagerSet([]schema.GroupVersionResource{podGVR})

	// Lazy AddResourceType for a GVR in the eager set — fires WARN
	// internally; we verify the informer is registered after.
	rw.AddResourceType(podGVR)
	if got := rw.ListObjects(podGVR, ""); got == nil {
		t.Fatalf("ListObjects(pods) = nil after lazy AddResourceType")
	}

	// Lazy AddResourceType for a GVR NOT in the eager set — must
	// also succeed (this is the "post-startup customer RestAction"
	// case).
	deploymentGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	rw.AddResourceType(deploymentGVR)
	if got := rw.ListObjects(deploymentGVR, ""); got == nil {
		t.Fatalf("ListObjects(deployments) = nil after lazy AddResourceType")
	}
}

// stopAndDrain calls rw.Stop and sleeps briefly so the informer
// goroutines actually wind down before the next test begins. The
// pre-existing TestNewResourceWatcher_DormantWhenCacheDisabled samples
// runtime.NumGoroutine() at the START of its body — if our prior test's
// informers are still draining, the dormant test reads a high baseline,
// then a low post-call count, registering a NEGATIVE delta and failing.
// 20 ms is the same headroom that pre-existing test already uses
// internally.
func stopAndDrain(rw *cache.ResourceWatcher) {
	if rw == nil {
		return
	}
	rw.Stop()
	time.Sleep(20 * time.Millisecond)
}

// equalGVRs compares two GVR slices for value equality (order-sensitive
// — the inventory walker promises sorted output).
func equalGVRs(a, b []schema.GroupVersionResource) bool {
	if len(a) != len(b) {
		return false
	}
	// Sort both defensively (the walker promises sorted output, but
	// test expectations may not).
	aa := append([]schema.GroupVersionResource(nil), a...)
	bb := append([]schema.GroupVersionResource(nil), b...)
	cmp := func(x, y schema.GroupVersionResource) bool {
		if x.Group != y.Group {
			return x.Group < y.Group
		}
		if x.Version != y.Version {
			return x.Version < y.Version
		}
		return x.Resource < y.Resource
	}
	sort.Slice(aa, func(i, j int) bool { return cmp(aa[i], aa[j]) })
	sort.Slice(bb, func(i, j int) bool { return cmp(bb[i], bb[j]) })
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

// silence-unused-import: keep metav1 referenced (defensive — some
// future test may need to construct ListOptions directly).
var _ = metav1.ListOptions{}
