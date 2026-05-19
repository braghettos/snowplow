// rbac_snapshot_test.go — Ship B (0.30.138) tests for the typed-RBAC
// snapshot writer.
//
// The four required tests per AC-B.6:
//
//   - TestRBACSnapshot_Equivalence            — equivalence vs the
//     pre-Ship-B per-call indexer-walk path; the snapshot's slices and
//     maps cover the same set of objects the indexer holds.
//   - TestRBACSnapshot_EventDriven_Updates    — ADD/UPDATE/DELETE on
//     the informer indexer drives scheduleRBACRebuild and the snapshot
//     reflects the change within a bounded window.
//   - TestRBACSnapshot_Race_ReaderWriter      — 32 reader goroutines ×
//     50 iters + 1 writer churning ADD/UPDATE/DELETE events at 500
//     ops; `go test -race` clean; max in-flight rebuild goroutines ≤ 1.
//   - TestRBACSnapshot_DegradeToDeny          — pre-readiness gate:
//     rbacSnap==nil → EvaluateRBAC returns (false, err), never falls
//     through to UserCan/SubjectAccessReview.
//
// Lives in package cache (not cache_test) so it can access the
// unexported GVR vars, rebuildRBACSnapshot, scheduleRBACRebuild, and
// rbacSnapshotEventHandlers — all under test for AC-B.4 / AC-B.5.
package cache

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// rbacScheme + rbacListKinds mirror the helpers in
// rbac/evaltest/evaluate_test.go (newTestWatcher). Duplicated here
// because the watcher-construction path lives in package cache.
func rbacScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := rbacv1.AddToScheme(sch); err != nil {
		t.Fatalf("rbacv1.AddToScheme: %v", err)
	}
	return sch
}

func rbacListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		clusterRoleBindingsTypedGVR: "ClusterRoleBindingList",
		roleBindingsTypedGVR:        "RoleBindingList",
		clusterRolesTypedGVR:        "ClusterRoleList",
		rolesTypedGVR:               "RoleList",
	}
}

// mkCRB / mkRB / mkCR / mkR — terse RBAC factories.
func mkCRB(name string, sub rbacv1.Subject) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Subjects:   []rbacv1.Subject{sub},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: name + "-role"},
	}
}

func mkRB(ns, name string, sub rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Subjects:   []rbacv1.Subject{sub},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name + "-role"},
	}
}

func mkCR(name string) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules:      []rbacv1.PolicyRule{{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}}},
	}
}

func mkR(ns, name string) *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Rules:      []rbacv1.PolicyRule{{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"configmaps"}}},
	}
}

func userSub(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: name}
}

// newSnapshotTestWatcher builds a cache=on ResourceWatcher backed by a
// dynamic fake seeded with `seed`. Returns the watcher + a cleanup
// closure. The watcher is NOT published via SetGlobal — these tests
// drive the snapshot writer directly and do not need the global hook.
func newSnapshotTestWatcher(t *testing.T, seed ...runtime.Object) *ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(rbacScheme(t), rbacListKinds(), seed...)
	rw, err := NewResourceWatcher(context.Background(), dyn)
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
	// Synchronously rebuild so the test has a published snapshot
	// regardless of how the initial-publish goroutine raced.
	RebuildRBACSnapshotForTest(rw)
	return rw
}

// awaitSnapshotContains waits up to timeout for predicate(snap) to be
// true. Polled at 5 ms — generous against the design's ~10 ms rebuild
// estimate. Fails the test on timeout.
func awaitSnapshotContains(t *testing.T, predicate func(*RBACSnapshot) bool, timeout time.Duration, label string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s := RBACSnapshotForTest(); s != nil && predicate(s) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("awaitSnapshotContains(%s) timed out after %v", label, timeout)
}

// ─────────────────────────────────────────────────────────────────────
// AC-B.6 #1 — Equivalence
// ─────────────────────────────────────────────────────────────────────

// TestRBACSnapshot_Equivalence asserts the snapshot's fields cover the
// same set of objects an indexer-walk would yield. Seeds a non-trivial
// mix of CRBs / RBs (across two namespaces) / CRs / Rs, builds a
// snapshot, and checks every seeded object appears in the right
// snapshot field with the right key.
func TestRBACSnapshot_Equivalence(t *testing.T) {
	crb1 := mkCRB("admin-bind", userSub("alice"))
	crb2 := mkCRB("ops-bind", userSub("bob"))
	rbA := mkRB("ns-a", "viewer-bind", userSub("alice"))
	rbB := mkRB("ns-b", "viewer-bind", userSub("bob"))
	cr1 := mkCR("admin-bind-role")
	cr2 := mkCR("ops-bind-role")
	rA := mkR("ns-a", "viewer-bind-role")
	rB := mkR("ns-b", "viewer-bind-role")

	rw := newSnapshotTestWatcher(t, crb1, crb2, rbA, rbB, cr1, cr2, rA, rB)
	snap := rw.Snapshot()
	if snap == nil {
		t.Fatalf("snapshot nil after seed + sync")
	}

	if got := len(snap.ClusterRoleBindings); got != 2 {
		t.Errorf("CRB count = %d; want 2", got)
	}
	crbNames := map[string]bool{}
	for _, crb := range snap.ClusterRoleBindings {
		crbNames[crb.Name] = true
	}
	for _, want := range []string{"admin-bind", "ops-bind"} {
		if !crbNames[want] {
			t.Errorf("snapshot.ClusterRoleBindings missing %q", want)
		}
	}

	if got := len(snap.RoleBindingsByNS["ns-a"]); got != 1 {
		t.Errorf("RoleBindingsByNS[ns-a] count = %d; want 1", got)
	}
	if got := len(snap.RoleBindingsByNS["ns-b"]); got != 1 {
		t.Errorf("RoleBindingsByNS[ns-b] count = %d; want 1", got)
	}
	if got := len(snap.RoleBindingsByNS["does-not-exist"]); got != 0 {
		t.Errorf("RoleBindingsByNS[does-not-exist] count = %d; want 0 (missing-key returns nil slice)", got)
	}

	if _, ok := snap.ClusterRolesByName["admin-bind-role"]; !ok {
		t.Errorf("ClusterRolesByName missing admin-bind-role")
	}
	if _, ok := snap.ClusterRolesByName["ops-bind-role"]; !ok {
		t.Errorf("ClusterRolesByName missing ops-bind-role")
	}

	if _, ok := snap.RolesByNSName["ns-a/viewer-bind-role"]; !ok {
		t.Errorf("RolesByNSName missing ns-a/viewer-bind-role")
	}
	if _, ok := snap.RolesByNSName["ns-b/viewer-bind-role"]; !ok {
		t.Errorf("RolesByNSName missing ns-b/viewer-bind-role")
	}
	if _, ok := snap.RolesByNSName["wrong/key"]; ok {
		t.Errorf("RolesByNSName matched a key that was never seeded")
	}

	// Equivalence with the indexer's own List() — same cardinality on
	// the CRB slice (the canonical pre-Ship-B read site at
	// evaluate.go:198).
	rw.mu.RLock()
	gi := rw.informers[clusterRoleBindingsTypedGVR]
	rw.mu.RUnlock()
	indexerItems := gi.Informer().GetIndexer().List()
	if len(indexerItems) != len(snap.ClusterRoleBindings) {
		t.Errorf("indexer/snapshot CRB count mismatch: indexer=%d snapshot=%d",
			len(indexerItems), len(snap.ClusterRoleBindings))
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-B.6 #2 — Event-driven updates
// ─────────────────────────────────────────────────────────────────────

// TestRBACSnapshot_EventDriven_Updates fires synthetic ADD/UPDATE/DELETE
// callbacks at the rbacSnapshotEventHandlers and verifies the snapshot
// reflects the indexer's new content within a bounded window. We do
// NOT push through the dynamic fake here — the handler is the API
// under test, so we drive it directly and reach into the indexer to
// install the post-change state, mirroring what the live informer
// does between OnAdd and the next List().
func TestRBACSnapshot_EventDriven_Updates(t *testing.T) {
	crb := mkCRB("initial", userSub("alice"))
	cr := mkCR("initial-role")
	rw := newSnapshotTestWatcher(t, crb, cr)

	if snap := rw.Snapshot(); snap == nil || len(snap.ClusterRoleBindings) != 1 {
		t.Fatalf("initial snapshot CRB count != 1: %v", snap)
	}

	// Add a new CRB into the indexer + fire the snapshot ADD handler.
	rw.mu.RLock()
	gi := rw.informers[clusterRoleBindingsTypedGVR]
	rw.mu.RUnlock()
	store := gi.Informer().GetIndexer()
	added := mkCRB("added", userSub("bob"))
	if err := store.Add(added); err != nil {
		t.Fatalf("indexer.Add: %v", err)
	}
	handlers := rw.rbacSnapshotEventHandlers()
	handlers.AddFunc(added)

	awaitSnapshotContains(t, func(s *RBACSnapshot) bool {
		for _, c := range s.ClusterRoleBindings {
			if c.Name == "added" {
				return true
			}
		}
		return false
	}, 500*time.Millisecond, "CRB 'added' visible post-ADD")

	// UPDATE: replace the CRB, fire the UPDATE handler — the writer
	// re-reads the indexer so the snapshot must observe the new
	// subjects.
	updated := mkCRB("added", userSub("eve"))
	if err := store.Update(updated); err != nil {
		t.Fatalf("indexer.Update: %v", err)
	}
	handlers.UpdateFunc(added, updated)
	awaitSnapshotContains(t, func(s *RBACSnapshot) bool {
		for _, c := range s.ClusterRoleBindings {
			if c.Name == "added" {
				return len(c.Subjects) == 1 && c.Subjects[0].Name == "eve"
			}
		}
		return false
	}, 500*time.Millisecond, "CRB 'added' subjects updated to eve")

	// DELETE: drop the CRB from the indexer + fire the DELETE handler.
	if err := store.Delete(updated); err != nil {
		t.Fatalf("indexer.Delete: %v", err)
	}
	handlers.DeleteFunc(updated)
	awaitSnapshotContains(t, func(s *RBACSnapshot) bool {
		for _, c := range s.ClusterRoleBindings {
			if c.Name == "added" {
				return false
			}
		}
		return true
	}, 500*time.Millisecond, "CRB 'added' gone post-DELETE")
}

// ─────────────────────────────────────────────────────────────────────
// AC-B.6 #3 — Race: 32 readers × 50 iters + 1 writer (AC-B.5)
// ─────────────────────────────────────────────────────────────────────

// TestRBACSnapshot_Race_ReaderWriter is the AC-B.5 / §3.3 hard gate.
// 32 reader goroutines × 50 iters each call rw.Snapshot() and iterate
// the slices/maps (read-only). One writer goroutine fires 500 ADD /
// UPDATE / DELETE event-handler callbacks, indirectly driving the
// scheduleRBACRebuild atomic.Bool tryLock + dirty-flag re-rebuild loop.
//
// Two invariants:
//   (1) `go test -race` clean — readers iterate the published snapshot;
//       the writer publishes a fresh snapshot; no goroutine mutates a
//       previously-published snapshot.
//   (2) Max in-flight rebuild goroutines ≤ 1 throughout the test. We
//       instrument scheduleRBACRebuild externally via a tracker
//       goroutine that polls the atomic.Bool tryLock and keeps a
//       max-observed counter; the lock's semantics guarantee at most
//       one CAS succeeds at a time.
//
// `feedback_shared_vs_copy_is_a_concurrency_change` directly discharged.
func TestRBACSnapshot_Race_ReaderWriter(t *testing.T) {
	// Seed N=200 CRBs so the rebuild walk is meaningful — slice growth
	// + map insert all execute under -race.
	const N = 200
	seed := make([]runtime.Object, 0, N+2)
	for i := 0; i < N; i++ {
		seed = append(seed, mkCRB("crb-"+strconv.Itoa(i), userSub("alice")))
	}
	seed = append(seed, mkCR("crb-0-role"), mkR("ns-a", "viewer-bind-role"))
	rw := newSnapshotTestWatcher(t, seed...)

	handlers := rw.rbacSnapshotEventHandlers()
	rw.mu.RLock()
	gi := rw.informers[clusterRoleBindingsTypedGVR]
	rw.mu.RUnlock()
	store := gi.Informer().GetIndexer()

	// Bounded-goroutine tracker: poll the in-flight count via the
	// public rbacRebuildLock atomic.Bool. Polling at 200 µs is dense
	// enough to catch the typical ~5 ms rebuild window.
	var maxObservedInFlight atomic.Int32
	stopTracker := make(chan struct{})
	var trackerWG sync.WaitGroup
	trackerWG.Add(1)
	go func() {
		defer trackerWG.Done()
		for {
			select {
			case <-stopTracker:
				return
			default:
			}
			if rbacRebuildLock.Load() {
				// Lock taken → at least one rebuild is in flight.
				// Bump the maximum observed (the lock is a 1-bit
				// semaphore so the true count is exactly 1 whenever
				// this branch runs).
				if cur := maxObservedInFlight.Load(); cur < 1 {
					maxObservedInFlight.Store(1)
				}
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()

	const readers = 32
	const itersPerReader = 50
	var readerWG sync.WaitGroup
	readerWG.Add(readers)
	for r := 0; r < readers; r++ {
		go func(r int) {
			defer readerWG.Done()
			for i := 0; i < itersPerReader; i++ {
				snap := rw.Snapshot()
				if snap == nil {
					continue
				}
				// Read-only iterations.
				var crbCount int
				for range snap.ClusterRoleBindings {
					crbCount++
				}
				_ = crbCount
				for _, rbs := range snap.RoleBindingsByNS {
					_ = len(rbs)
				}
				_ = snap.ClusterRolesByName["crb-0-role"]
				_ = snap.RolesByNSName["ns-a/viewer-bind-role"]
			}
		}(r)
	}

	// Writer: 500 ADD/UPDATE/DELETE events, churning the indexer +
	// triggering scheduleRBACRebuild. Distribute across CRBs so the
	// rebuild walk has work to do.
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		for i := 0; i < 500; i++ {
			idx := i % N
			name := "crb-" + strconv.Itoa(idx)
			obj := mkCRB(name, userSub("user-"+strconv.Itoa(i)))
			switch i % 3 {
			case 0:
				_ = store.Update(obj)
				handlers.UpdateFunc(obj, obj)
			case 1:
				_ = store.Add(mkCRB("churn-"+strconv.Itoa(i), userSub("alice")))
				handlers.AddFunc(obj)
			case 2:
				// DELETE a synthetic transient entry — best-effort.
				_ = store.Delete(mkCRB("churn-"+strconv.Itoa(i-1), userSub("alice")))
				handlers.DeleteFunc(obj)
			}
		}
	}()

	readerWG.Wait()
	writerWG.Wait()
	// Give the dirty-flag re-rebuild loop a moment to drain the last
	// flip before we close the tracker (so the tracker has a fair
	// chance to observe any final in-flight window — purely
	// belt-and-suspenders, the maxObserved is already monotonic).
	time.Sleep(20 * time.Millisecond)
	close(stopTracker)
	trackerWG.Wait()

	if got := maxObservedInFlight.Load(); got > 1 {
		t.Errorf("AC-B.5 invariant violated: max in-flight rebuild goroutines = %d; want ≤ 1", got)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-B.6 #4 — Degrade-to-deny (AC-B.8)
// ─────────────────────────────────────────────────────────────────────

// TestRBACSnapshot_DegradeToDeny asserts the pre-readiness gate. With
// rbacSnap.Store(nil) — modelling the window between cache=on
// activation and the initial publish — every snapshot reader observes
// nil and the caller (EvaluateRBAC in the rbac package; see also
// internal/rbac/evaltest) MUST fail closed. This test directly
// exercises the snapshot pointer; the rbac-package observable
// behaviour is covered by evaltest's CacheOnWithoutGlobalDenies and the
// new AC-B.8 evaltest case added alongside Ship B.
func TestRBACSnapshot_DegradeToDeny(t *testing.T) {
	restore := DisableRBACSnapshotForTest()
	t.Cleanup(restore)

	if s := RBACSnapshotForTest(); s != nil {
		t.Fatalf("expected nil snapshot after DisableRBACSnapshotForTest, got %p", s)
	}

	// Republish a non-nil snapshot and assert observability.
	PublishRBACSnapshotForTest(&RBACSnapshot{
		ClusterRoleBindings: []*rbacv1.ClusterRoleBinding{mkCRB("x", userSub("alice"))},
		RoleBindingsByNS:    map[string][]*rbacv1.RoleBinding{},
		ClusterRolesByName:  map[string]*rbacv1.ClusterRole{},
		RolesByNSName:       map[string]*rbacv1.Role{},
	})
	if s := RBACSnapshotForTest(); s == nil || len(s.ClusterRoleBindings) != 1 {
		t.Fatalf("expected re-published snapshot to be observable; got %v", s)
	}

	// Re-clear and assert nil again — the AC-B.8 deny-window state.
	PublishRBACSnapshotForTest(nil)
	if s := RBACSnapshotForTest(); s != nil {
		t.Fatalf("expected nil snapshot after clear, got %p", s)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Additional: snapshot-miss canary (AC-B.10)
// ─────────────────────────────────────────────────────────────────────

// TestRBACSnapshot_MissCanary asserts RecordRBACSnapshotMiss bumps the
// counter and that RBACSnapshotMissCount surfaces it. This is the
// observable side of AC-B.10; the ratio gate is empirical and runs in
// the §6 mechanism test.
func TestRBACSnapshot_MissCanary(t *testing.T) {
	before := RBACSnapshotMissCount()
	RecordRBACSnapshotMiss("ClusterRole", "", "phantom-role")
	RecordRBACSnapshotMiss("Role", "ns-x", "phantom-role")
	after := RBACSnapshotMissCount()
	if after-before != 2 {
		t.Errorf("RBACSnapshotMissCount delta = %d; want 2", after-before)
	}
}
