// crdwatch_lifecycle_falsifier_test.go — Ship D (0.30.114) pre-flight
// falsifier for CRD-lifecycle cache handling.
//
// Team rule feedback_falsifier_first_before_ship: these tests are
// written BEFORE the production fix and MUST fail against the unfixed
// 0.30.113 CRD-watch. The FAIL artifact is the pre-flight gate; only
// after capturing it does the implementation land.
//
//   FD1 — CRD-add does not dirty-mark a stale-negative LIST entry. A
//         compositions-list resolve that ran BEFORE the CRD existed
//         records a LIST-scope dep and caches `0 items`. When the CRD
//         later appears, crdwatch.registerCRDObject auto-registers the
//         informer via EnsureResourceType (added==true) — but nothing
//         calls into the DepTracker, so the stale L1 entry is never
//         dirty-marked. FAILS on 0.30.113: the refresh hook stays empty.
//
//   FD1-neg — CRD-add for a GVR with NO matching LIST-dep dirty-marks
//         nothing (AC-D4 idempotence / no-op-on-no-match). This is the
//         negative control: it must hold on both old and new code.
//
//   FD2 — CRD-delete is unhandled. crdwatch's informer handler wires no
//         DeleteFunc, so a CRD removal never reaches the DepTracker; an
//         L1 entry with a LIST-dep on the removed GVR serves stale rows
//         forever. FAILS on 0.30.113: OnResourceTypeRemoved does not
//         exist / is never called.
//
// CRITICAL fixture-safety: every GVR here is SYNTHETIC. No test touches
// a real cluster or a real composition CRD. The on-cluster validation
// is the tester's separate, fixture-safe job.

package cache

import (
	"context"
	"testing"
	"time"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// gvrSyntheticWidgets is a SYNTHETIC composition-style GVR used by the
// Ship D falsifiers. It is deliberately not a real production GVR — the
// falsifier never touches a cluster.
func gvrSyntheticWidgets() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "synthetic.krateo.io",
		Version:  "v1",
		Resource: "syntheticwidgets",
	}
}

// --- FD1 — CRD-add must dirty-mark a stale-negative LIST entry ---------------

// TestFalsifierFD1_CRDAddDirtyMarksStaleListEntry reproduces the
// stale-negative LIST bug: an L1 entry recorded a LIST-scope dep for a
// GVR while the CRD did not yet exist (so the list resolved to 0 items).
// When the CRD appears, the CRD-watch auto-registers the informer; the
// stale L1 entry MUST be dirty-marked so the refresher re-resolves it.
//
// The falsifier drives registerCRDObject — the same entry point the
// crdwatch informer handler (AddFunc/UpdateFunc) and the post-walk
// ReconcileAutoDiscoverCRDs scan both call — for a synthetic CRD whose
// group is navigation-discovered, with a pre-recorded synthetic
// LIST-dep.
//
// FAILS on 0.30.113: registerCRDObject never calls into the DepTracker,
// so the refresh hook stays empty.
func TestFalsifierFD1_CRDAddDirtyMarksStaleListEntry(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetAutoDiscoverGroupsForTest()
	t.Cleanup(ResetAutoDiscoverGroupsForTest)
	ResetDepsForTest()
	t.Cleanup(ResetDepsForTest)

	gvr := gvrSyntheticWidgets()

	// An L1 entry resolved a compositions-list BEFORE the CRD existed —
	// it recorded a LIST-scope dep and cached `0 items`.
	const staleListL1 = "L1_synthetic_list_stale"
	d := Deps()
	d.RecordList(staleListL1, gvr, "bench-ns-07")

	hook := &captureHook{}
	d.SetRefreshHook(hook.fn())

	rw := newSyntheticCRDWatchWatcher(t, gvr)

	// The group is navigation-discovered (Phase 1 walk equivalent).
	AddAutoDiscoverGroup(gvr.Group)

	// The CRD appears at runtime — drive the same entry point the
	// crdwatch informer handler uses.
	crd := crdUnstructured(gvr.Group, gvr.Resource, []map[string]any{
		{"name": gvr.Version, "served": true, "storage": true},
	})
	rw.registerCRDObject(crd, "crd-event")

	// EnsureResourceType must have observed a genuinely-new GVR.
	if !rw.IsRegistered(gvr) {
		t.Fatalf("FD1 setup: synthetic informer not registered for %v", gvr)
	}

	if !hook.has(staleListL1) {
		t.Fatalf("FD1: CRD-add did not dirty-mark the stale-negative LIST entry %q; "+
			"refresh hook=%v — registerCRDObject never calls OnResourceTypeAvailable",
			staleListL1, hook.snapshot())
	}
	if got := d.Stats().DirtyMarkTotal; got != 1 {
		t.Fatalf("FD1: dirtyMarkTotal=%d want 1", got)
	}
}

// TestFalsifierFD1Neg_CRDAddNoMatchingListDepIsNoOp is the negative
// control for AC-D4: a CRD-add for a GVR that has NO matching LIST-dep
// dirty-marks nothing. Holds on both old and new code.
func TestFalsifierFD1Neg_CRDAddNoMatchingListDepIsNoOp(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetAutoDiscoverGroupsForTest()
	t.Cleanup(ResetAutoDiscoverGroupsForTest)
	ResetDepsForTest()
	t.Cleanup(ResetDepsForTest)

	gvr := gvrSyntheticWidgets()
	other := schema.GroupVersionResource{
		Group: gvr.Group, Version: "v1", Resource: "otherthings",
	}

	d := Deps()
	// A LIST-dep exists, but for a DIFFERENT resource — must not match.
	d.RecordList("L1_other", other, "bench-ns-07")

	hook := &captureHook{}
	d.SetRefreshHook(hook.fn())

	rw := newSyntheticCRDWatchWatcher(t, gvr)
	AddAutoDiscoverGroup(gvr.Group)

	crd := crdUnstructured(gvr.Group, gvr.Resource, []map[string]any{
		{"name": gvr.Version, "served": true, "storage": true},
	})
	rw.registerCRDObject(crd, "crd-event")

	if got := len(hook.snapshot()); got != 0 {
		t.Fatalf("FD1-neg: CRD-add dirty-marked %d entries, want 0 "+
			"(no LIST-dep matches the new GVR): %v", got, hook.snapshot())
	}
	if got := d.Stats().DirtyMarkTotal; got != 0 {
		t.Fatalf("FD1-neg: dirtyMarkTotal=%d want 0", got)
	}
}

// --- FD2 — CRD-delete must dirty-mark LIST + dependent-GET deps --------------

// TestFalsifierFD2_CRDDeleteDirtyMarksDeps reproduces the unhandled
// CRD-delete bug: when a CRD is removed at runtime, L1 entries with a
// LIST-dep — or a dependent GET-dep — on the removed GVR serve stale
// rows forever. The DepTracker MUST dirty-mark them (NEVER evict — a
// CRD removal is a type-removal, not a single object's DELETE).
//
// The falsifier drives unregisterCRDObject — the entry point Ship D's
// new DeleteFunc routes through.
//
// FAILS on 0.30.113: crdwatch wires no DeleteFunc and
// OnResourceTypeRemoved does not exist.
func TestFalsifierFD2_CRDDeleteDirtyMarksDeps(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetAutoDiscoverGroupsForTest()
	t.Cleanup(ResetAutoDiscoverGroupsForTest)
	ResetDepsForTest()
	t.Cleanup(ResetDepsForTest)

	gvr := gvrSyntheticWidgets()

	d := Deps()
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)

	// A LIST-dep on the GVR.
	const listL1 = "L1_synthetic_list"
	store.Put(listL1, &ResolvedEntry{RawJSON: []byte(`{"items":[]}`)})
	d.RecordList(listL1, gvr, "bench-ns-07")

	// A dependent-GET-dep on a named object of the GVR — the depending
	// entry's OWN object is something else (a widget).
	const getL1 = "L1_synthetic_widget"
	store.Put(getL1, &ResolvedEntry{
		RawJSON: []byte(`{}`),
		Inputs: &ResolvedKeyInputs{
			CacheEntryClass: "widgets",
			Group:           "widgets.krateo.io",
			Version:         "v1",
			Resource:        "buttons",
			Namespace:       "bench-ns-07",
			Name:            "save-btn",
		},
	})
	d.Record(getL1, gvr, "bench-ns-07", "thing-1")

	hook := &captureHook{}
	d.SetRefreshHook(hook.fn())

	rw := newSyntheticCRDWatchWatcher(t, gvr)
	AddAutoDiscoverGroup(gvr.Group)

	crd := crdUnstructured(gvr.Group, gvr.Resource, []map[string]any{
		{"name": gvr.Version, "served": true, "storage": true},
	})
	// The CRD is removed at runtime — drive the Ship D delete entry point.
	rw.unregisterCRDObject(crd, "crd-event")

	// Both deps must be dirty-marked.
	if !hook.has(listL1) {
		t.Fatalf("FD2: CRD-delete did not dirty-mark the LIST-dep %q; hook=%v",
			listL1, hook.snapshot())
	}
	if !hook.has(getL1) {
		t.Fatalf("FD2: CRD-delete did not dirty-mark the dependent-GET-dep %q; hook=%v",
			getL1, hook.snapshot())
	}
	// NEVER evict — both entries must SURVIVE in the store.
	if _, ok := store.Get(listL1); !ok {
		t.Fatalf("FD2: CRD-delete EVICTED the LIST-dep entry %q — "+
			"a CRD removal must dirty-mark, never evict", listL1)
	}
	if _, ok := store.Get(getL1); !ok {
		t.Fatalf("FD2: CRD-delete EVICTED the dependent-GET entry %q — "+
			"a CRD removal must dirty-mark, never evict", getL1)
	}
	if got := d.Stats().EvictDeleteTotal; got != 0 {
		t.Fatalf("FD2: evictDeleteTotal=%d want 0 — CRD-delete must never evict", got)
	}
	if got := d.Stats().DirtyMarkTotal; got != 2 {
		t.Fatalf("FD2: dirtyMarkTotal=%d want 2 (LIST-dep + dependent-GET-dep)", got)
	}
}

// --- AC-D3 — the post-walk reconcile re-scan also fires D1 ------------------

// TestShipD_ReconcileAutoDiscoverCRDsFiresD1 asserts AC-D3: because D1 is
// routed through registerCRDObject, the post-walk ReconcileAutoDiscover-
// CRDs store re-scan automatically dirty-marks a stale-negative LIST
// entry for any CRD it registers for the first time — no extra wiring.
//
// It reproduces the boot ORDERING race: the CRD sits in the informer's
// store, the live AddFunc already replayed it with the group ABSENT (so
// no registration), the group is discovered LATE, and ONLY the post-walk
// reconcile registers the informer — and must fire the D1 dirty-mark.
func TestShipD_ReconcileAutoDiscoverCRDsFiresD1(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetAutoDiscoverGroupsForTest()
	t.Cleanup(ResetAutoDiscoverGroupsForTest)
	ResetDepsForTest()
	t.Cleanup(ResetDepsForTest)

	gvr := gvrSyntheticWidgets()

	// An L1 entry resolved a list BEFORE the CRD existed.
	const staleListL1 = "L1_synthetic_list_via_reconcile"
	d := Deps()
	d.RecordList(staleListL1, gvr, "bench-ns-09")
	hook := &captureHook{}
	d.SetRefreshHook(hook.fn())

	// Seed the fake client with the synthetic CRD so the CRD informer's
	// initial LIST replays it — exactly the boot replay.
	crd := crdUnstructured(gvr.Group, gvr.Resource, []map[string]any{
		{"name": gvr.Version, "served": true, "storage": true},
	})
	sch := k8sruntime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:               "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:        "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:        "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
		customResourceDefinitionGVR: "CustomResourceDefinitionList",
		gvr:                         "SyntheticWidgetList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(sch, listKinds, crd)
	rw, err := NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher under CACHE_ENABLED=true")
	}
	t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })

	// StartCRDWatch replays the CRD through AddFunc with the group ABSENT
	// → dropped (the boot race).
	rw.StartCRDWatch(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for !rw.IsSynced(customResourceDefinitionGVR) {
		if time.Now().After(deadline) {
			t.Fatalf("CRD informer did not sync in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rw.IsRegistered(gvr) {
		t.Fatalf("synthetic informer registered before its group was discovered — setup error")
	}

	// The Phase 1 walk discovers the group LATE.
	AddAutoDiscoverGroup(gvr.Group)
	// Group discovery alone does not register / dirty-mark.
	if len(hook.snapshot()) != 0 {
		t.Fatalf("AC-D3: dirty-mark fired before the post-walk reconcile: %v", hook.snapshot())
	}

	// The post-walk reconcile re-scans the CRD store, registers the
	// informer — and must fire the D1 dirty-mark.
	registered := rw.ReconcileAutoDiscoverCRDs()
	if registered != 1 {
		t.Fatalf("AC-D3: ReconcileAutoDiscoverCRDs registered %d, want 1", registered)
	}
	if !hook.has(staleListL1) {
		t.Fatalf("AC-D3: the post-walk reconcile registered the CRD but did NOT "+
			"dirty-mark the stale-negative LIST entry %q; hook=%v", staleListL1, hook.snapshot())
	}
	if got := d.Stats().DirtyMarkTotal; got != 1 {
		t.Fatalf("AC-D3: dirtyMarkTotal=%d want 1", got)
	}

	// AC-D4: a second reconcile registers nothing new AND does not
	// re-dirty-mark — added==false for the now-known GVR.
	if again := rw.ReconcileAutoDiscoverCRDs(); again != 0 {
		t.Fatalf("AC-D3: a second reconcile registered %d, want 0", again)
	}
	if got := d.Stats().DirtyMarkTotal; got != 1 {
		t.Fatalf("AC-D4: a second reconcile re-dirty-marked; dirtyMarkTotal=%d want 1", got)
	}
}

// newSyntheticCRDWatchWatcher builds a ResourceWatcher backed by a fake
// dynamic client seeded for the synthetic GVR + the CRD meta-query. The
// watcher is CACHE_ENABLED so registerCRDObject's EnsureResourceType
// path is live. SYNTHETIC ONLY — never touches a cluster.
func newSyntheticCRDWatchWatcher(t *testing.T, gvr schema.GroupVersionResource) *ResourceWatcher {
	t.Helper()
	sch := k8sruntime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:               "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:        "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:        "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
		customResourceDefinitionGVR: "CustomResourceDefinitionList",
		gvr:                         "SyntheticWidgetList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(sch, listKinds)
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
	return rw
}
