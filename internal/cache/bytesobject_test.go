// bytesobject_test.go — Ship H1 hermetic acceptance tests.
//
// White-box (package cache) so the tests can exercise the unexported
// bytesObject, newBytesObject, decodeBytesObject, asRuntimeObject, and
// the bytes-override transform directly.
//
// Coverage maps to the PM-gate acceptance criteria that are
// hermetically verifiable (no cluster):
//
//   - TestBytesObject_FieldFidelity                 -> AC-H1.1
//   - TestBytesObject_MetaAccessor_FINDING2         -> AC-H1.3 (the
//     load-bearing meta.Accessor / indexer-key risk)
//   - TestBytesObject_IndexerKeyAndNamespace        -> AC-H1.3
//   - TestBytesObject_AllFiveCastSites_Falsifier    -> AC-H1.2 (the
//     silent-drop falsifier — see the dedicated falsifier file too)
//   - TestBytesObject_DecodeIsFreshTreePerCall      -> AC-H1.8 (SB-4)
//   - TestBytesObject_TransformGatedByCacheEnabled  -> AC-H1.6 / SB-2
//   - TestBytesObject_TransformRoutedByGroup        -> SB-3
package cache

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientcache "k8s.io/client-go/tools/cache"
)

// compositionGVR is the canonical composition-group GVR used by the H1
// tests. The resource segment is a representative dynamically-named
// composition CRD; H1 routes the whole group, so the exact resource
// string is not load-bearing.
var compositionGVR = schema.GroupVersionResource{
	Group:    "composition.krateo.io",
	Version:  "v1",
	Resource: "githubscaffoldingwithcompositionpages",
}

// makeComposition builds a representative composition Unstructured with
// a deep spec/status tree so field-fidelity and decode tests exercise
// nested maps, slices, and scalars of every JSON type.
func makeComposition(ns, name, crdKind string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "composition.krateo.io/v1",
		"kind":       crdKind,
		"metadata": map[string]interface{}{
			"name":            name,
			"namespace":       ns,
			"resourceVersion": "12345",
			"uid":             "uid-" + name,
			"generation":      int64(7),
			"labels":          map[string]interface{}{"app": name, "tier": "bench"},
			"annotations":     map[string]interface{}{"krateo.io/owner": "bench"},
		},
		"spec": map[string]interface{}{
			"replicas": int64(3),
			"nested": map[string]interface{}{
				"flag":   true,
				"ratio":  0.75,
				"labels": []interface{}{"a", "b", "c"},
				"deep": map[string]interface{}{
					"value": "leaf",
				},
			},
			"items": []interface{}{
				map[string]interface{}{"id": int64(1), "name": "one"},
				map[string]interface{}{"id": int64(2), "name": "two"},
			},
		},
		"status": map[string]interface{}{
			"phase": "Ready",
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True"},
			},
		},
	}}
}

// TestBytesObject_FieldFidelity — AC-H1.1.
//
// For a representative sample spanning multiple composition CRDs, the
// object reconstructed by decode-on-access must be deep-equal to the
// object the plain informer would have stored. The control is the
// source Unstructured itself (newBytesObject does not strip — the
// pre-existing defaultStripUnstructured policy runs BEFORE
// newBytesObject in the real transform; here we feed an already-clean
// object so the comparison is exact).
func TestBytesObject_FieldFidelity(t *testing.T) {
	crds := []string{
		"GithubScaffoldingWithCompositionPage",
		"PostgreSQLComposition",
		"FireworksAppComposition",
	}
	for ci, crd := range crds {
		for i := 0; i < 8; i++ { // >= 20 objects across >= 3 CRDs
			name := "comp"
			for n := 0; n <= i+ci; n++ {
				name += "x"
			}
			src := makeComposition("bench-ns-01", name, crd)

			bo, err := newBytesObject(src)
			if err != nil {
				t.Fatalf("newBytesObject(%s/%s): %v", crd, name, err)
			}
			got, err := bo.Decode()
			if err != nil {
				t.Fatalf("Decode(%s/%s): %v", crd, name, err)
			}
			if !reflect.DeepEqual(src.Object, got.Object) {
				t.Fatalf("field fidelity lost for %s/%s\n want=%#v\n  got=%#v",
					crd, name, src.Object, got.Object)
			}
		}
	}
}

// TestBytesObject_MetaAccessor_FINDING2 — AC-H1.3, the highest-risk
// implementation item.
//
// meta.Accessor MUST succeed on a *bytesObject and return the correct
// namespace/name/uid/etc. If this fails, MetaNamespaceKeyFunc and
// MetaNamespaceIndexFunc fail and objects are never indexed.
func TestBytesObject_MetaAccessor_FINDING2(t *testing.T) {
	src := makeComposition("bench-ns-07", "comp-accessor", "GithubScaffoldingWithCompositionPage")
	bo, err := newBytesObject(src)
	if err != nil {
		t.Fatalf("newBytesObject: %v", err)
	}

	acc, err := meta.Accessor(bo)
	if err != nil {
		t.Fatalf("meta.Accessor(*bytesObject) failed — FINDING 2 regression: %v", err)
	}
	if acc.GetNamespace() != "bench-ns-07" {
		t.Fatalf("accessor namespace = %q, want bench-ns-07", acc.GetNamespace())
	}
	if acc.GetName() != "comp-accessor" {
		t.Fatalf("accessor name = %q, want comp-accessor", acc.GetName())
	}
	if acc.GetResourceVersion() != "12345" {
		t.Fatalf("accessor resourceVersion = %q, want 12345", acc.GetResourceVersion())
	}
	if string(acc.GetUID()) != "uid-comp-accessor" {
		t.Fatalf("accessor uid = %q, want uid-comp-accessor", acc.GetUID())
	}

	// MetaNamespaceKeyFunc must produce the canonical ns/name key.
	key, err := clientcache.MetaNamespaceKeyFunc(bo)
	if err != nil {
		t.Fatalf("MetaNamespaceKeyFunc(*bytesObject): %v", err)
	}
	if key != "bench-ns-07/comp-accessor" {
		t.Fatalf("key = %q, want bench-ns-07/comp-accessor", key)
	}

	// MetaNamespaceIndexFunc must yield the namespace.
	idx, err := clientcache.MetaNamespaceIndexFunc(bo)
	if err != nil {
		t.Fatalf("MetaNamespaceIndexFunc(*bytesObject): %v", err)
	}
	if len(idx) != 1 || idx[0] != "bench-ns-07" {
		t.Fatalf("index = %v, want [bench-ns-07]", idx)
	}
}

// TestBytesObject_IndexerKeyAndNamespace — AC-H1.3.
//
// Builds a real clientcache.Indexer with the SAME key-func +
// index-func the informer uses (MetaNamespaceKeyFunc +
// {NamespaceIndex: MetaNamespaceIndexFunc}), populates it with
// bytesObjects, and asserts GetByKey + ByIndex are count-equal to a
// control indexer of plain Unstructured. No object may be unindexed.
func TestBytesObject_IndexerKeyAndNamespace(t *testing.T) {
	indexers := clientcache.Indexers{clientcache.NamespaceIndex: clientcache.MetaNamespaceIndexFunc}
	bytesIdx := clientcache.NewIndexer(clientcache.MetaNamespaceKeyFunc, indexers)
	controlIdx := clientcache.NewIndexer(clientcache.MetaNamespaceKeyFunc, indexers)

	namespaces := []string{"bench-ns-01", "bench-ns-02", "bench-ns-03"}
	perNS := 9
	for _, ns := range namespaces {
		for i := 0; i < perNS; i++ {
			name := "comp-" + ns + "-" + string(rune('a'+i))
			src := makeComposition(ns, name, "GithubScaffoldingWithCompositionPage")

			bo, err := newBytesObject(src)
			if err != nil {
				t.Fatalf("newBytesObject: %v", err)
			}
			if err := bytesIdx.Add(bo); err != nil {
				t.Fatalf("bytesIdx.Add — FINDING 2 (object not indexable): %v", err)
			}
			if err := controlIdx.Add(src); err != nil {
				t.Fatalf("controlIdx.Add: %v", err)
			}
		}
	}

	// (a) GetByKey resolves every inserted object.
	for _, ns := range namespaces {
		for i := 0; i < perNS; i++ {
			key := ns + "/comp-" + ns + "-" + string(rune('a'+i))
			obj, exists, err := bytesIdx.GetByKey(key)
			if err != nil || !exists {
				t.Fatalf("bytesIdx.GetByKey(%q): exists=%v err=%v — object unindexed", key, exists, err)
			}
			if _, ok := obj.(*bytesObject); !ok {
				t.Fatalf("bytesIdx.GetByKey(%q): got %T, want *bytesObject", key, obj)
			}
		}
	}

	// (b) ByIndex(NamespaceIndex, ns) count-equal to the control.
	totalBytes, totalControl := 0, 0
	for _, ns := range namespaces {
		gotBytes, err := bytesIdx.ByIndex(clientcache.NamespaceIndex, ns)
		if err != nil {
			t.Fatalf("bytesIdx.ByIndex(%q): %v", ns, err)
		}
		gotControl, err := controlIdx.ByIndex(clientcache.NamespaceIndex, ns)
		if err != nil {
			t.Fatalf("controlIdx.ByIndex(%q): %v", ns, err)
		}
		if len(gotBytes) != len(gotControl) {
			t.Fatalf("namespace %q: bytes indexer returned %d objects, control returned %d — index integrity lost",
				ns, len(gotBytes), len(gotControl))
		}
		if len(gotBytes) != perNS {
			t.Fatalf("namespace %q: expected %d objects, got %d", ns, perNS, len(gotBytes))
		}
		totalBytes += len(gotBytes)
		totalControl += len(gotControl)
	}
	if totalBytes != totalControl || totalBytes != len(namespaces)*perNS {
		t.Fatalf("total indexed: bytes=%d control=%d want=%d", totalBytes, totalControl, len(namespaces)*perNS)
	}
}

// TestBytesObject_DecodeIsFreshTreePerCall — AC-H1.8 / SB-4.
//
// Decode() must allocate a brand-new map every call: two callers must
// receive non-aliased trees, and mutating one must not affect the
// other or the bytesObject's `raw`. This is the 0.30.128 shared-map
// crash-class guard.
func TestBytesObject_DecodeIsFreshTreePerCall(t *testing.T) {
	src := makeComposition("bench-ns-01", "comp-fresh", "GithubScaffoldingWithCompositionPage")
	bo, err := newBytesObject(src)
	if err != nil {
		t.Fatalf("newBytesObject: %v", err)
	}

	a, err := bo.Decode()
	if err != nil {
		t.Fatalf("Decode a: %v", err)
	}
	b, err := bo.Decode()
	if err != nil {
		t.Fatalf("Decode b: %v", err)
	}

	// The two top-level maps must be distinct allocations.
	if reflect.ValueOf(a.Object).Pointer() == reflect.ValueOf(b.Object).Pointer() {
		t.Fatal("Decode() returned aliased maps — SB-4 violation (memoized shared tree)")
	}

	// Mutating tree a must not leak into tree b.
	a.Object["spec"].(map[string]interface{})["replicas"] = int64(999)
	if got := b.Object["spec"].(map[string]interface{})["replicas"]; got != int64(3) {
		t.Fatalf("mutation of tree a leaked into tree b: replicas=%v", got)
	}

	// A third decode must still see the original value (raw immutable).
	c, err := bo.Decode()
	if err != nil {
		t.Fatalf("Decode c: %v", err)
	}
	if got := c.Object["spec"].(map[string]interface{})["replicas"]; got != int64(3) {
		t.Fatalf("bytesObject.raw was mutated: replicas=%v", got)
	}
}

// TestBytesObject_ConcurrentDecode_Race — AC-H1.8.
//
// >= 16 goroutines decode + read the same bytesObject concurrently.
// Run under `go test -race`; a memoized shared tree (SB-4 violation)
// trips the race detector here.
func TestBytesObject_ConcurrentDecode_Race(t *testing.T) {
	src := makeComposition("bench-ns-01", "comp-race", "GithubScaffoldingWithCompositionPage")
	bo, err := newBytesObject(src)
	if err != nil {
		t.Fatalf("newBytesObject: %v", err)
	}

	const goroutines = 24
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				uns, err := bo.Decode()
				if err != nil {
					t.Errorf("Decode: %v", err)
					return
				}
				// Read + mutate the private tree — must not race.
				spec, _ := uns.Object["spec"].(map[string]interface{})
				if spec == nil {
					t.Error("decoded object missing spec")
					return
				}
				spec["replicas"] = int64(i)
			}
		}()
	}
	wg.Wait()
}

// TestBytesObject_TransformRoutedByGroup — updated for the Ship H5
// routing inversion.
//
// Pre-H5 the bytes-override converted only allow-listed groups and left
// everything else *unstructured. Post-H5 the transform converts EVERY
// non-typed-RBAC object to a *bytesObject (the `!isStreamingException`
// re-gate) — a composition AND an arbitrary `apps` deployment both
// become bytesObjects. A typed-RBAC GVR is the exception: the typed
// override runs first and produces a typed struct.
func TestBytesObject_TransformRoutedByGroup(t *testing.T) {
	resetStripLoggingForTest()
	t.Setenv("CACHE_ENABLED", "true")

	// Composition group -> bytesObject.
	tfComp := StripBulkyFieldsForResourceType("composition.krateo.io/v1/x", compositionGVR)
	outComp, err := tfComp(makeComposition("bench-ns-01", "c1", "GithubScaffoldingWithCompositionPage"))
	if err != nil {
		t.Fatalf("composition transform: %v", err)
	}
	if _, ok := outComp.(*bytesObject); !ok {
		t.Fatalf("composition object stored as %T, want *bytesObject", outComp)
	}

	// H5: a non-composition, non-RBAC group -> ALSO *bytesObject
	// (streaming is the default; the bytes-override re-gate covers it).
	deployGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	tfDeploy := StripBulkyFieldsForResourceType("apps/v1/deployments", deployGVR)
	deploy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]interface{}{"name": "d1", "namespace": "default"},
	}}
	outDeploy, err := tfDeploy(deploy)
	if err != nil {
		t.Fatalf("deployment transform: %v", err)
	}
	if _, ok := outDeploy.(*bytesObject); !ok {
		t.Fatalf("H5: non-RBAC deployment object stored as %T, want *bytesObject "+
			"(streaming is the default — the bytes-override is no longer allow-listed)", outDeploy)
	}

	// The typed-RBAC exception: a typed-RBAC GVR is NOT converted to a
	// bytesObject — the typed override runs first and yields a typed
	// struct (here a *rbacv1.ClusterRole).
	crGVR := rbacTypedGVRs[2] // clusterroles
	tfRBAC := StripBulkyFieldsForResourceType("rbac.authorization.k8s.io/v1/clusterroles", crGVR)
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]interface{}{"name": "cr1"},
	}}
	outCR, err := tfRBAC(cr)
	if err != nil {
		t.Fatalf("clusterrole transform: %v", err)
	}
	if _, ok := outCR.(*bytesObject); ok {
		t.Fatalf("H5: typed-RBAC object became a *bytesObject — it must take the typed path, "+
			"not the bytes-override (got %T)", outCR)
	}
}

// TestBytesObject_TransformGatedByCacheEnabled — AC-H1.6 / SB-2.
//
// With CACHE_ENABLED unset/false the bytes-override is inert: a
// composition object is stored as a plain *unstructured.Unstructured,
// byte-identical to the pre-rebuild path.
func TestBytesObject_TransformGatedByCacheEnabled(t *testing.T) {
	resetStripLoggingForTest()
	t.Setenv("CACHE_ENABLED", "false")

	tf := StripBulkyFieldsForResourceType("composition.krateo.io/v1/x", compositionGVR)
	src := makeComposition("bench-ns-01", "c-gated", "GithubScaffoldingWithCompositionPage")
	out, err := tf(src)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if _, ok := out.(*bytesObject); ok {
		t.Fatal("CACHE_ENABLED=false produced a *bytesObject — SB-2 gate violation")
	}
	uns, ok := out.(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("CACHE_ENABLED=false stored %T, want *unstructured.Unstructured", out)
	}
	// The default strip still runs (it is not gated) — but content is
	// otherwise untouched: spec/status preserved.
	if uns.Object["spec"] == nil || uns.Object["status"] == nil {
		t.Fatal("CACHE_ENABLED=false path lost spec/status")
	}
}

// TestBytesObject_TransformPreservesStripPolicy — SB-1.
//
// newBytesObject runs AFTER defaultStripUnstructured in the real
// transform. This test feeds the transform an object carrying
// managedFields + the last-applied annotation and confirms the
// resulting bytesObject's decoded form has them stripped (existing
// policy preserved) while every spec/status field is retained (no NEW
// field removal).
func TestBytesObject_TransformPreservesStripPolicy(t *testing.T) {
	resetStripLoggingForTest()
	t.Setenv("CACHE_ENABLED", "true")

	src := makeComposition("bench-ns-01", "c-strip", "GithubScaffoldingWithCompositionPage")
	meta := src.Object["metadata"].(map[string]interface{})
	meta["managedFields"] = []interface{}{
		map[string]interface{}{"manager": "kubectl"},
	}
	meta["annotations"].(map[string]interface{})[lastAppliedAnnotation] = `{"big":"blob"}`

	tf := StripBulkyFieldsForResourceType("composition.krateo.io/v1/x", compositionGVR)
	out, err := tf(src)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	bo, ok := out.(*bytesObject)
	if !ok {
		t.Fatalf("stored %T, want *bytesObject", out)
	}
	decoded, err := bo.Decode()
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	// Existing strip policy applied.
	if len(decoded.GetManagedFields()) != 0 {
		t.Fatal("managedFields not stripped — existing policy not preserved")
	}
	if _, present := decoded.GetAnnotations()[lastAppliedAnnotation]; present {
		t.Fatal("last-applied annotation not stripped")
	}
	// No NEW field removal: spec/status fully retained.
	spec, _ := decoded.Object["spec"].(map[string]interface{})
	if spec == nil || spec["replicas"] == nil || spec["nested"] == nil || spec["items"] == nil {
		t.Fatalf("spec field removed by bytes conversion: %#v", decoded.Object["spec"])
	}
	if decoded.Object["status"] == nil {
		t.Fatal("status removed by bytes conversion")
	}
	// A surviving annotation is retained.
	if decoded.GetAnnotations()["krateo.io/owner"] != "bench" {
		t.Fatal("non-last-applied annotation lost")
	}
}

// TestDecodeBytesObject_Passthrough confirms decodeBytesObject handles
// both shapes: a plain *unstructured.Unstructured is returned unchanged
// (the CACHE_ENABLED=false / non-composition path), a *bytesObject is
// decoded, and an unrelated value yields (nil,false).
func TestDecodeBytesObject_Passthrough(t *testing.T) {
	uns := makeComposition("ns", "n", "K")
	if got, ok := decodeBytesObject(uns); !ok || got != uns {
		t.Fatalf("decodeBytesObject(Unstructured) = (%v,%v), want passthrough", got, ok)
	}

	bo, _ := newBytesObject(uns)
	got, ok := decodeBytesObject(bo)
	if !ok || got == nil {
		t.Fatal("decodeBytesObject(*bytesObject) failed to decode")
	}
	if !reflect.DeepEqual(uns.Object, got.Object) {
		t.Fatal("decodeBytesObject(*bytesObject) lost fidelity")
	}

	if _, ok := decodeBytesObject("not an object"); ok {
		t.Fatal("decodeBytesObject(string) should return ok=false")
	}
	if _, ok := decodeBytesObject(nil); ok {
		t.Fatal("decodeBytesObject(nil) should return ok=false")
	}
}

// TestBytesObject_RuntimeObjectContract confirms the runtime.Object
// surface: GetObjectKind reports the GVK and DeepCopyObject yields an
// independent, fidelity-preserving copy.
func TestBytesObject_RuntimeObjectContract(t *testing.T) {
	src := makeComposition("ns", "n", "GithubScaffoldingWithCompositionPage")
	bo, err := newBytesObject(src)
	if err != nil {
		t.Fatalf("newBytesObject: %v", err)
	}

	gvk := bo.GetObjectKind().GroupVersionKind()
	if gvk.Group != "composition.krateo.io" || gvk.Kind != "GithubScaffoldingWithCompositionPage" {
		t.Fatalf("GetObjectKind GVK = %v, want composition.krateo.io/.../GithubScaffoldingWithCompositionPage", gvk)
	}

	cp, ok := bo.DeepCopyObject().(*bytesObject)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T, want *bytesObject", bo.DeepCopyObject())
	}
	if &cp.raw == &bo.raw {
		t.Fatal("DeepCopyObject did not copy raw slice header")
	}
	cpDecoded, err := cp.Decode()
	if err != nil {
		t.Fatalf("Decode copy: %v", err)
	}
	if !reflect.DeepEqual(src.Object, cpDecoded.Object) {
		t.Fatal("DeepCopyObject lost field fidelity")
	}
}

// TestBytesObject_BytesGroupNeverMetadataOnly — B1, updated for the
// Ship H5 routing inversion.
//
// A bytes-streaming GVR MUST NOT route to the metadata-only path (it
// SKIPS SetTransform → the bytes-override would never run). Pre-H5 the
// guard was an allow-list; H5 re-expressed it as `!isStreamingException`
// — true for every non-typed-RBAC GVR. Consequence: shouldUseMetadataOnly
// now returns false for EVERY non-RBAC GVR (Rule 2) AND for the RBAC
// GVRs (Rule 1) — the metadata-only path is fully inert post-H5. This
// test confirms that: a composition GVR, an arbitrary GVR, and an
// annotated GVR all resolve to NOT-metadata-only.
func TestBytesObject_BytesGroupNeverMetadataOnly(t *testing.T) {
	resetMetadataOnlyAnnotationsForTest()
	defer resetMetadataOnlyAnnotationsForTest()

	// A composition GVR is never metadata-only.
	if shouldUseMetadataOnly(compositionGVR) {
		t.Fatal("composition GVR routed metadata-only — the bytes-override would be a silent no-op")
	}

	// Even with the exact GVR annotated `krateo.io/cache-mode: metadata`,
	// Rule 2 (!isStreamingException) wins and returns false.
	annotatedGVRs.Store(compositionGVR, struct{}{})
	if shouldUseMetadataOnly(compositionGVR) {
		t.Fatal("annotated composition GVR routed metadata-only — Rule 2 not winning over the annotation rule")
	}

	// H5: an arbitrary non-composition, non-RBAC GVR is ALSO never
	// metadata-only — the metadata-only path is inert post-inversion.
	// (Pre-H5 an annotated non-composition GVR DID route metadata-only;
	// H5 deliberately makes that impossible — every non-RBAC GVR streams
	// to bytes, and streaming and metadata-only are mutually exclusive.)
	arbitrary := schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "things"}
	annotatedGVRs.Store(arbitrary, struct{}{})
	if shouldUseMetadataOnly(arbitrary) {
		t.Fatal("H5: an annotated non-RBAC GVR routed metadata-only — post-inversion the " +
			"metadata-only path must be inert (every non-RBAC GVR streams to bytes)")
	}
}

// TestStreamingLister_BytesGroupPanics — B2 (updated for the Ship H5
// routing inversion).
//
// streamingDynamicInformer.Lister() must panic loudly — a streaming
// informer's store is always *bytesObject-backed and dynamiclister
// would silently drop every value. Pre-H5 the panic was conditional on
// a bytes-override group; post-H5 a streamingDynamicInformer is
// constructed ONLY for streaming GVRs, so the panic is unconditional.
// Asserted here for two GVRs — a composition GVR and an arbitrary
// non-allow-list GVR — both must panic.
func TestStreamingLister_BytesGroupPanics(t *testing.T) {
	for _, gvr := range []schema.GroupVersionResource{
		compositionGVR,
		{Group: "apps", Version: "v1", Resource: "deployments"},
	} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Lister() for %s did not panic — B2 silent-drop trap not loud", gvr)
				}
			}()
			d := &streamingDynamicInformer{gvr: gvr}
			_ = d.Lister()
		}()
	}
}

// jsonRoundTrip is a helper asserting `raw` is itself valid JSON of the
// expected shape — guards against newBytesObject storing a corrupt
// payload.
func TestBytesObject_RawIsValidJSON(t *testing.T) {
	src := makeComposition("ns", "n", "K")
	bo, err := newBytesObject(src)
	if err != nil {
		t.Fatalf("newBytesObject: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(bo.raw, &m); err != nil {
		t.Fatalf("bo.raw is not valid JSON: %v", err)
	}
	if m["kind"] != "K" {
		t.Fatalf("raw JSON kind = %v, want K", m["kind"])
	}
	// embedded ObjectMeta must satisfy metav1.Object at compile + run.
	var _ metav1.Object = bo
}
