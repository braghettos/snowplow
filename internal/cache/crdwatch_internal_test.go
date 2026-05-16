// crdwatch_internal_test.go — 0.30.102 Tag B internal tests for the
// CRD-watch's unexported GVR-derivation logic. package cache so it can
// reach compositionGVRFromCRDObject + toUnstructuredMap.

package cache

import (
	"context"
	"testing"
	"time"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientcache "k8s.io/client-go/tools/cache"
)

// crdUnstructured builds an unstructured CustomResourceDefinition with
// the given group/plural and a single served+storage version.
func crdUnstructured(group, plural string, versions []map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": plural + "." + group},
		"spec": map[string]any{
			"group":    group,
			"names":    map[string]any{"plural": plural},
			"versions": toAnySlice(versions),
		},
	}}
}

func toAnySlice(in []map[string]any) []any {
	out := make([]any, len(in))
	for i := range in {
		out[i] = in[i]
	}
	return out
}

func TestCompositionGVRFromCRDObject_StorageVersionPreferred(t *testing.T) {
	crd := crdUnstructured("composition.krateo.io", "githubscaffoldings", []map[string]any{
		{"name": "v1alpha1", "served": true, "storage": false},
		{"name": "v1", "served": true, "storage": true},
	})
	gvr, ok := compositionGVRFromCRDObject(crd)
	if !ok {
		t.Fatalf("compositionGVRFromCRDObject must derive a GVR from a valid CRD")
	}
	want := schema.GroupVersionResource{
		Group: "composition.krateo.io", Version: "v1", Resource: "githubscaffoldings",
	}
	if gvr != want {
		t.Fatalf("gvr = %v, want %v (storage version must win)", gvr, want)
	}
}

func TestCompositionGVRFromCRDObject_FirstServedWhenNoStorage(t *testing.T) {
	crd := crdUnstructured("composition.krateo.io", "things", []map[string]any{
		{"name": "v1beta1", "served": true, "storage": false},
	})
	gvr, ok := compositionGVRFromCRDObject(crd)
	if !ok {
		t.Fatalf("expected a GVR from a served-only CRD")
	}
	if gvr.Version != "v1beta1" {
		t.Fatalf("version = %q, want v1beta1 (first served)", gvr.Version)
	}
}

func TestCompositionGVRFromCRDObject_RejectsNoServedVersion(t *testing.T) {
	crd := crdUnstructured("composition.krateo.io", "things", []map[string]any{
		{"name": "v1", "served": false, "storage": true},
	})
	if _, ok := compositionGVRFromCRDObject(crd); ok {
		t.Fatalf("a CRD with no SERVED version must not yield a GVR")
	}
}

func TestCompositionGVRFromCRDObject_RejectsNonCRD(t *testing.T) {
	notCRD := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "x"},
	}}
	if _, ok := compositionGVRFromCRDObject(notCRD); ok {
		t.Fatalf("a non-CRD object must not yield a GVR")
	}
}

func TestCompositionGVRFromCRDObject_UnwrapsTombstone(t *testing.T) {
	crd := crdUnstructured("composition.krateo.io", "things", []map[string]any{
		{"name": "v1", "served": true, "storage": true},
	})
	tomb := clientcache.DeletedFinalStateUnknown{Key: "things.composition.krateo.io", Obj: crd}
	gvr, ok := compositionGVRFromCRDObject(tomb)
	if !ok {
		t.Fatalf("compositionGVRFromCRDObject must unwrap a DeletedFinalStateUnknown tombstone")
	}
	if gvr.Resource != "things" {
		t.Fatalf("resource = %q, want things", gvr.Resource)
	}
}

func TestToUnstructuredMap(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{"k": "v"}}
	m, ok := toUnstructuredMap(u)
	if !ok || m["k"] != "v" {
		t.Fatalf("toUnstructuredMap(*Unstructured) failed: ok=%v m=%v", ok, m)
	}
	raw := map[string]any{"a": 1}
	m2, ok2 := toUnstructuredMap(raw)
	if !ok2 || m2["a"] != 1 {
		t.Fatalf("toUnstructuredMap(map) failed: ok=%v m=%v", ok2, m2)
	}
	if _, ok3 := toUnstructuredMap("not-an-object"); ok3 {
		t.Fatalf("toUnstructuredMap must reject a non-object")
	}
}

// TestReconcileAutoDiscoverCRDs_ClosesBootRace is the 0.30.105 falsifier
// for the CRD-watch boot replay-vs-discover ORDERING race.
//
// The race: StartCRDWatch's CRD informer replays every existing CRD
// through AddFunc ONCE at boot. The Phase 1 walk discovers composition
// groups AFTER that — so the composition CRD is replayed while
// matchesAutoDiscoverGroup(composition.krateo.io)==false and is dropped
// permanently; the composition informer never registers.
//
// This test reproduces the exact ordering: a composition CRD sits in the
// CRD informer's store, the CRD-watch event handler has already run with
// the group ABSENT (so no live registration), and ONLY THEN is the group
// added. ReconcileAutoDiscoverCRDs must re-scan the store and register
// the composition informer.
//
// NEGATIVE control inside the same test: before the reconcile (group
// added but no re-scan) the composition informer is NOT registered —
// proving the race is real and the reconcile is what closes it.
func TestReconcileAutoDiscoverCRDs_ClosesBootRace(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetAutoDiscoverGroupsForTest()
	t.Cleanup(ResetAutoDiscoverGroupsForTest)

	const (
		compGroup    = "composition.krateo.io"
		compResource = "githubscaffoldings"
		compVersion  = "v1"
	)
	compGVR := schema.GroupVersionResource{Group: compGroup, Version: compVersion, Resource: compResource}

	// The composition CRD that will sit in the CRD informer's store.
	compCRD := crdUnstructured(compGroup, compResource, []map[string]any{
		{"name": compVersion, "served": true, "storage": true},
	})

	sch := k8sruntime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:                "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:         "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:         "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
		customResourceDefinitionGVR: "CustomResourceDefinitionList",
		compGVR:                     "GithubScaffoldingList",
	}
	// Seed the fake client with the composition CRD so the CRD informer's
	// initial LIST replays it — exactly the boot replay.
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(sch, listKinds, compCRD)
	rw, err := NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher under CACHE_ENABLED=true")
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})

	// Start the CRD-watch. Its CRD informer replays compCRD through
	// AddFunc — but the auto-discover set is EMPTY, so the composition
	// CRD is dropped (the race condition).
	rw.StartCRDWatch(context.Background())

	// Wait for the CRD informer to sync so its store holds compCRD.
	deadline := time.Now().Add(5 * time.Second)
	for !rw.IsSynced(customResourceDefinitionGVR) {
		if time.Now().After(deadline) {
			t.Fatalf("CRD informer did not sync in time")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// NEGATIVE control: the group is not yet discovered, so the
	// composition informer must NOT be registered.
	if rw.IsRegistered(compGVR) {
		t.Fatalf("composition informer registered before its group was discovered — test setup error")
	}

	// The Phase 1 walk discovers the group LATE — after the CRD replay.
	AddAutoDiscoverGroup(compGroup)

	// NEGATIVE control: discovering the group alone does NOT register the
	// informer — AddFunc never re-fires for a CRD already replayed. This
	// is the boot race.
	if rw.IsRegistered(compGVR) {
		t.Fatalf("composition informer registered merely by AddAutoDiscoverGroup — " +
			"the boot race would not exist if this were true")
	}

	// The post-walk reconcile re-scans the CRD store and registers the
	// composition informer.
	registered := rw.ReconcileAutoDiscoverCRDs()
	if registered != 1 {
		t.Fatalf("ReconcileAutoDiscoverCRDs must register exactly 1 composition informer; got %d", registered)
	}
	if !rw.IsRegistered(compGVR) {
		t.Fatalf("boot-race falsifier FAIL: ReconcileAutoDiscoverCRDs did not register the "+
			"composition informer %v — the replay-vs-discover race is not closed", compGVR)
	}

	// Idempotent: a second reconcile registers nothing new.
	if again := rw.ReconcileAutoDiscoverCRDs(); again != 0 {
		t.Fatalf("a second ReconcileAutoDiscoverCRDs must be a no-op; registered %d", again)
	}
}
