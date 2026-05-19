// h5_inversion_test.go — Ship H5 hermetic acceptance tests for the
// routing inversion (bytes-streaming as the default for every informer).
//
// H5 ends the per-group whack-a-mole: bytesResourceOverrides is deleted;
// streaming is the DEFAULT and the stock path is reachable only by the
// typed-RBAC EXCEPTION. The predicate is isStreamingException(gvr) —
// true iff gvr has a typed override (typedResourceOverrides). One
// predicate drives BOTH the watcher.go informer routing AND the
// strip.go bytes-override re-gate.
//
// Coverage of the PM-gate ACs that are hermetically verifiable:
//
//   - TestH5_AC1_StreamingIsTheDefault          -> AC-1 (inversion)
//   - TestH5_AC2_TypedRBACException              -> AC-2 (RBAC -> stock)
//   - TestH5_AC3_WatchEventOfStreamingGVRBecomesBytes -> AC-3 (the HARD
//     pre-commit WATCH-path gate)
//   - TestH5_AC4_PredicateIsSingleAndTypedDerived-> AC-4
//   - TestH5_AC6_ToggleOffRevertsWholeFleet      -> AC-6 (safety)
//   - TestH5_AC7_SyntheticNewGVRStreams          -> AC-7 (the structural
//     whack-a-mole-ended falsifier)
//
// AC-5 (non-RBAC resolve correctness) is covered by the H2a/H4
// functional-correctness regression suite (TestH2a_FieldFidelity_*,
// TestH4_AC2_*) — all still pass post-H5.
package cache

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	clientcache "k8s.io/client-go/tools/cache"
)

// TestH5_AC1_StreamingIsTheDefault — AC-1.
//
// Every dynamic GVR that is not a typed-RBAC exception routes to
// *streamingDynamicInformer on first EnsureResourceType — composition,
// widgets, restactions, repoes, AND an arbitrary synthetic CRD GVR.
// No allow-list; the inversion makes streaming the default.
func TestH5_AC1_StreamingIsTheDefault(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv(envCompositionStreamingList, "true")

	gvrs := map[string]schema.GroupVersionResource{
		"composition": {Group: "composition.krateo.io", Version: "v1", Resource: "githubscaffoldingwithcompositionpages"},
		"widgets":     {Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "panels"},
		"restactions": {Group: "templates.krateo.io", Version: "v1", Resource: "restactions"},
		"repoes":      {Group: "github.krateo.io", Version: "v1alpha1", Resource: "repoes"},
		"synthetic":   {Group: "totally-new-vendor.example.io", Version: "v1", Resource: "neverseenbefores"},
	}
	for label, gvr := range gvrs {
		t.Run(label, func(t *testing.T) {
			ResetDepsForTest()
			t.Cleanup(ResetDepsForTest)
			ResetAutoDiscoverGroupsForTest()
			t.Cleanup(ResetAutoDiscoverGroupsForTest)

			rw := newRouteRaceWatcher(t, true, gvr)
			t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
			rw.EnsureResourceType(gvr)

			if !isStreamingInformer(rw, gvr) {
				t.Fatalf("AC-1 FAIL: %s GVR did NOT route to the streaming informer — "+
					"H5: streaming is the default for every non-typed-RBAC GVR", label)
			}
		})
	}
}

// TestH5_AC2_TypedRBACException — AC-2.
//
// The 4 typed-RBAC GVRs route to the STOCK path, NOT
// *streamingDynamicInformer — RBAC genuinely needs the stock informer
// to deliver *unstructured.Unstructured to the typed transform. They
// are eager-registered by NewResourceWatcher.
func TestH5_AC2_TypedRBACException(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv(envCompositionStreamingList, "true")
	ResetDepsForTest()
	t.Cleanup(ResetDepsForTest)

	// newSyntheticRemoveWatcher's NewResourceWatcher eager-registers the
	// 4 RBAC GVRs. No extra GVRs needed.
	rw := newRouteRaceWatcher(t, true)
	t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })

	for _, gvr := range RBACResourceTypes {
		if isStreamingInformer(rw, gvr) {
			t.Fatalf("AC-2 FAIL: typed-RBAC GVR %s routed to the streaming informer — "+
				"RBAC must take the stock path (a bytesObject would fail stripAndType's cast)", gvr)
		}
		rw.mu.RLock()
		_, registered := rw.informers[gvr]
		rw.mu.RUnlock()
		if !registered {
			t.Fatalf("AC-2 FAIL: typed-RBAC GVR %s not registered", gvr)
		}
	}
}

// TestH5_AC3_WatchEventOfStreamingGVRBecomesBytes — AC-3, the HARD
// pre-commit WATCH-path gate.
//
// SetTransform runs on WATCH-delivered ADD/UPDATE events, not only LIST
// items. A streaming GVR's WATCH events arrive as
// *unstructured.Unstructured (H2a streamed only the ListFunc, not the
// WatchFunc). The strip.go bytes-override block is re-gated to
// `!isStreamingException(gvr)` so those WATCH events ARE converted to
// *bytesObject — otherwise the store drifts back to map[string]any
// under churn and H5's gain erodes.
//
// This test drives a WATCH event of a NON-composition streaming GVR
// (restactions) through StripBulkyFieldsForResourceType and asserts it
// becomes a *bytesObject — proving the re-gate covers the now-default
// streaming set, not the deleted allow-list.
func TestH5_AC3_WatchEventOfStreamingGVRBecomesBytes(t *testing.T) {
	resetStripLoggingForTest()
	t.Setenv("CACHE_ENABLED", "true")

	// A non-composition, non-RBAC streaming GVR.
	restactionGVR := schema.GroupVersionResource{
		Group: "templates.krateo.io", Version: "v1", Resource: "restactions",
	}
	if isStreamingException(restactionGVR) {
		t.Fatal("precondition: restactions must NOT be a streaming exception")
	}

	// A WATCH event delivers a plain *unstructured.Unstructured — the
	// stock dynamic WatchFunc decode shape.
	watchEvent := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "templates.krateo.io/v1",
		"kind":       "RESTAction",
		"metadata": map[string]interface{}{
			"namespace":  "krateo-system",
			"name":       "sidebar-nav-menu-items",
			"generation": int64(2),
		},
		"spec":   map[string]interface{}{"api": []interface{}{}},
		"status": map[string]interface{}{"ready": true},
	}}

	tf := StripBulkyFieldsForResourceType("templates.krateo.io/v1/restactions", restactionGVR)
	out, err := tf(watchEvent)
	if err != nil {
		t.Fatalf("transform of a WATCH *Unstructured returned error: %v", err)
	}

	bo, ok := out.(*bytesObject)
	if !ok {
		t.Fatalf("AC-3 FAIL: a streaming-routed (non-composition) GVR's WATCH event through "+
			"the transform produced %T, want *bytesObject — the bytes-override re-gate does NOT "+
			"cover the now-default streaming set; the store would drift to maps under churn", out)
	}

	// Field fidelity on the converted WATCH object.
	decoded, derr := bo.Decode()
	if derr != nil {
		t.Fatalf("Decode of the WATCH-converted bytesObject: %v", derr)
	}
	if decoded.GetName() != "sidebar-nav-menu-items" {
		t.Fatalf("WATCH-converted object identity wrong: %s", decoded.GetName())
	}
	if decoded.Object["spec"] == nil || decoded.Object["status"] == nil {
		t.Fatal("WATCH-converted object lost spec/status")
	}

	// A typed-RBAC GVR's WATCH event must still produce a typed struct
	// (the typed branch runs first; the bytes-override is never reached).
	resetStripLoggingForTest()
	crGVR := rbacTypedGVRs[2] // clusterroles
	rbacEvent := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]interface{}{"name": "cr-watch"},
	}}
	tfRBAC := StripBulkyFieldsForResourceType("rbac.authorization.k8s.io/v1/clusterroles", crGVR)
	outRBAC, err := tfRBAC(rbacEvent)
	if err != nil {
		t.Fatalf("RBAC transform: %v", err)
	}
	if _, isBytes := outRBAC.(*bytesObject); isBytes {
		t.Fatalf("AC-3 FAIL: a typed-RBAC GVR's WATCH event became a *bytesObject — "+
			"it must take the typed path (got %T)", outRBAC)
	}
}

// TestH5_AC4_PredicateIsSingleAndTypedDerived — AC-4.
//
// isStreamingException is the single routing predicate, and it is
// DERIVED from typedResourceOverrides membership — not a hardcoded
// RBAC-GVR literal. A GVR is excepted iff it has a typed override.
func TestH5_AC4_PredicateIsSingleAndTypedDerived(t *testing.T) {
	// The 4 typed-RBAC GVRs are excepted.
	for _, gvr := range rbacTypedGVRs {
		if !isStreamingException(gvr) {
			t.Fatalf("AC-4 FAIL: typed-RBAC GVR %s is not a streaming exception", gvr)
		}
	}
	// Everything else is not excepted.
	for _, gvr := range []schema.GroupVersionResource{
		{Group: "composition.krateo.io", Version: "v1", Resource: "x"},
		{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "panels"},
		{Group: "apps", Version: "v1", Resource: "deployments"},
		{Group: "brand-new.io", Version: "v1", Resource: "things"},
	} {
		if isStreamingException(gvr) {
			t.Fatalf("AC-4 FAIL: non-typed GVR %s is wrongly a streaming exception", gvr)
		}
	}

	// The predicate is typedResourceOverrides-DERIVED, not a literal:
	// give a synthetic GVR a typed override and it becomes excepted;
	// remove it and it streams again. DisableTypedOverrideForTest /
	// the typed-override map is the discriminant.
	syntheticTyped := schema.GroupVersionResource{
		Group: "synthetic-typed.example.io", Version: "v1", Resource: "things",
	}
	if isStreamingException(syntheticTyped) {
		t.Fatal("precondition: synthetic GVR must not be excepted before it has a typed override")
	}
	typedResourceOverrides[syntheticTyped] = stripAndTypeRole // any typed override
	defer delete(typedResourceOverrides, syntheticTyped)
	if !isStreamingException(syntheticTyped) {
		t.Fatal("AC-4 FAIL: a GVR given a typed override is NOT excepted — the predicate is " +
			"not typedResourceOverrides-derived")
	}
}

// TestH5_AC6_ToggleOffRevertsWholeFleet — AC-6 (safety).
//
// With the streaming toggle off, EVERY dynamic GVR — composition,
// widgets, a synthetic GVR — falls back to the stock informer (the
// `if gi == nil` path). A clean full-fleet revert. CACHE_ENABLED=false
// behaviour is covered by the existing passthrough tests.
func TestH5_AC6_ToggleOffRevertsWholeFleet(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv(envCompositionStreamingList, "false") // toggle OFF

	gvrs := map[string]schema.GroupVersionResource{
		"composition": {Group: "composition.krateo.io", Version: "v1", Resource: "githubscaffoldingwithcompositionpages"},
		"widgets":     {Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "panels"},
		"synthetic":   {Group: "another-new-vendor.example.io", Version: "v1", Resource: "things"},
	}
	for label, gvr := range gvrs {
		t.Run(label, func(t *testing.T) {
			ResetDepsForTest()
			t.Cleanup(ResetDepsForTest)
			ResetAutoDiscoverGroupsForTest()
			t.Cleanup(ResetAutoDiscoverGroupsForTest)

			rw := newRouteRaceWatcher(t, true, gvr)
			t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
			rw.EnsureResourceType(gvr)

			if isStreamingInformer(rw, gvr) {
				t.Fatalf("AC-6 FAIL: toggle off but %s GVR still got a streaming informer — "+
					"the whole fleet must revert to stock", label)
			}
			rw.mu.RLock()
			_, registered := rw.informers[gvr]
			rw.mu.RUnlock()
			if !registered {
				t.Fatalf("AC-6 FAIL: %s GVR not registered with the toggle off — "+
					"the stock fallback is unreachable", label)
			}
		})
	}
}

// TestH5_AC7_SyntheticNewGVRStreams — AC-7, the structural falsifier.
//
// THE WHACK-A-MOLE-ENDED PROOF: a synthetic, never-before-seen dynamic
// GVR — a group in NO allow-list (there is no allow-list anymore) —
// registers and yields a *streamingDynamicInformer with ZERO allow-list
// edit. If the inversion were reverted to a positive allow-list, this
// synthetic GVR would route to stock and the test would fail. This is
// the definitive proof that no future GVR can silently re-create the
// NewFilteredDynamicInformer.func3 heap driver.
func TestH5_AC7_SyntheticNewGVRStreams(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv(envCompositionStreamingList, "true")
	ResetDepsForTest()
	t.Cleanup(ResetDepsForTest)
	ResetAutoDiscoverGroupsForTest()
	t.Cleanup(ResetAutoDiscoverGroupsForTest)

	// A GVR no ship ever allow-listed — a hypothetical future customer
	// CRD group.
	futureGVR := schema.GroupVersionResource{
		Group:    "hypothetical-future-product.acme.example",
		Version:  "v1",
		Resource: "widgetsofthefuture",
	}
	// It is not a typed-RBAC exception (no typed override).
	if isStreamingException(futureGVR) {
		t.Fatal("precondition: the synthetic future GVR must not be a streaming exception")
	}

	rw := newRouteRaceWatcher(t, true, futureGVR)
	t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
	rw.EnsureResourceType(futureGVR)

	if !isStreamingInformer(rw, futureGVR) {
		t.Fatal("AC-7 FAIL: a synthetic never-allow-listed GVR did NOT route to the streaming " +
			"informer — the per-group whack-a-mole is not structurally ended; a future GVR " +
			"can still re-create the stock-informer func3 heap driver")
	}
}

// TestH5_StreamingInformerStoreHoldsBytes — end-to-end belt-and-suspenders
// for AC-1 + AC-3: a streaming informer for a non-composition GVR, Run
// against an empty LIST + injected WATCH ADD events, ends up holding
// *bytesObject in its indexer (the WATCH-event conversion, proven on a
// running informer, for a now-default-streaming GVR).
func TestH5_StreamingInformerStoreHoldsBytes(t *testing.T) {
	resetStripLoggingForTest()
	t.Setenv("CACHE_ENABLED", "true")

	restactionGVR := schema.GroupVersionResource{
		Group: "templates.krateo.io", Version: "v1", Resource: "restactions",
	}

	fw := watch.NewFake()
	lw := &watchInjectingListWatch{watcher: fw}
	inf := clientcache.NewSharedIndexInformer(
		lw,
		&unstructured.Unstructured{},
		0,
		clientcache.Indexers{clientcache.NamespaceIndex: clientcache.MetaNamespaceIndexFunc},
	)
	tf := StripBulkyFieldsForResourceType("templates.krateo.io/v1/restactions", restactionGVR)
	if err := inf.SetTransform(func(obj interface{}) (interface{}, error) { return tf(obj) }); err != nil {
		t.Fatalf("SetTransform: %v", err)
	}

	stopCh := make(chan struct{})
	defer close(stopCh)
	go inf.Run(stopCh)
	if !clientcache.WaitForCacheSync(stopCh, inf.HasSynced) {
		t.Fatal("informer failed to sync")
	}

	const n = 5
	for i := 0; i < n; i++ {
		fw.Add(&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "templates.krateo.io/v1", "kind": "RESTAction",
			"metadata": map[string]interface{}{
				"namespace": "krateo-system", "name": "ra-" + string(rune('a'+i)),
			},
			"spec": map[string]interface{}{"api": []interface{}{}},
		}})
	}
	if !waitForIndexerCount(inf, n, 2*time.Second) {
		t.Fatalf("indexer absorbed %d/%d WATCH events", len(inf.GetStore().List()), n)
	}
	for _, obj := range inf.GetStore().List() {
		if _, ok := obj.(*bytesObject); !ok {
			t.Fatalf("a streaming non-composition GVR's WATCH-delivered object stored as %T, "+
				"want *bytesObject — the store would drift to *Unstructured under churn", obj)
		}
	}
}
