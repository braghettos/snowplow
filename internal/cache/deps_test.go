// deps_test.go — Tag 0.30.8 unit tests for the DepTracker.
//
// Coverage: Record + RecordList idempotency, four-bucket lookup
// (exact / ns-list / cluster-name / cluster-list), DELETE evicts from
// the wired L1 store + clears reverse index, UPDATE enqueues into the
// refresh hook WITHOUT evicting, RemoveL1Key purges all forward and
// reverse records, cap-reached drops + warn-once, concurrent
// Record + OnDelete is race-free.

package cache

import (
	"context"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func newTestDepTracker(t *testing.T, maxRecords int64) *DepTracker {
	t.Helper()
	d := newDepTracker(maxRecords)
	return d
}

func gvrCompositions() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1",
		Resource: "compositions",
	}
}

// --- Record + lookup ---------------------------------------------------------

func TestDeps_RecordExactObject(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()
	d.Record("L1A", gvr, "bench-ns-01", "app-1")

	if got := d.totalRecords.Load(); got != 1 {
		t.Fatalf("totalRecords=%d want 1", got)
	}
	// Idempotent re-record.
	d.Record("L1A", gvr, "bench-ns-01", "app-1")
	if got := d.totalRecords.Load(); got != 1 {
		t.Fatalf("idempotent re-record bumped count: %d", got)
	}
}

func TestDeps_RecordListEncodedAsWildcard(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()
	d.RecordList("L1A", gvr, "bench-ns-01")

	// Internally the list-bucket key has Name="*".
	bucket := DepKey{GVR: gvr, Namespace: "bench-ns-01", Name: listWildcard}
	if _, ok := d.forward.Load(bucket); !ok {
		t.Fatalf("list-bucket missing for %v", bucket)
	}
}

func TestDeps_FourBucketLookup(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()

	d.Record("L1_exact", gvr, "bench-ns-01", "app-1") // exact
	d.RecordList("L1_nslist", gvr, "bench-ns-01")     // ns-list
	d.Record("L1_clustname", gvr, "", "app-1")        // cluster-name (rare)
	d.RecordList("L1_clustlist", gvr, "")             // cluster-list

	matched := d.collectMatches(gvr, "bench-ns-01", "app-1")
	want := []string{"L1_exact", "L1_nslist", "L1_clustname", "L1_clustlist"}
	for _, k := range want {
		if _, ok := matched[k]; !ok {
			t.Errorf("collectMatches missing %q (got %v)", k, matched)
		}
	}

	// A different name in the same ns matches only the ns-list +
	// cluster-list buckets (NOT the exact or cluster-name buckets).
	matched2 := d.collectMatches(gvr, "bench-ns-01", "app-2")
	if _, ok := matched2["L1_exact"]; ok {
		t.Errorf("exact bucket leaked into different-name lookup")
	}
	if _, ok := matched2["L1_nslist"]; !ok {
		t.Errorf("ns-list bucket missing for sibling lookup")
	}
	if _, ok := matched2["L1_clustlist"]; !ok {
		t.Errorf("cluster-list bucket missing for sibling lookup")
	}
}

// --- OnDelete (R2/R7 three-way classification, 0.30.110) --------------------

// inputsFor builds a ResolvedKeyInputs whose dispatched-object identity
// equals (gvr, ns, name) — used to make a store entry a SELF-
// representation of that object so OnDelete classifies it as evict.
func inputsFor(gvr schema.GroupVersionResource, ns, name string) *ResolvedKeyInputs {
	return &ResolvedKeyInputs{
		CacheEntryClass: "restactions",
		Group:           gvr.Group,
		Version:         gvr.Version,
		Resource:        gvr.Resource,
		Namespace:       ns,
		Name:            name,
	}
}

// TestDeps_OnDelete_EvictsSelfRepresentation — an entry whose OWN
// dispatched object is the deleted object is a self-representation and
// MUST be evicted.
func TestDeps_OnDelete_EvictsSelfRepresentation(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)

	gvr := gvrCompositions()
	// L1A's own object IS (gvr, bench-ns-01, app-1).
	store.Put("L1A", &ResolvedEntry{
		RawJSON: []byte(`{"a":1}`),
		Inputs:  inputsFor(gvr, "bench-ns-01", "app-1"),
	})
	d.Record("L1A", gvr, "bench-ns-01", "app-1")

	got := d.OnDelete(gvr, "bench-ns-01", "app-1")
	if got != 1 {
		t.Fatalf("OnDelete returned %d evicted, want 1 (self-representation)", got)
	}
	if _, ok := store.Get("L1A"); ok {
		t.Errorf("L1A (self-representation) should have been evicted by DELETE")
	}
	if d.totalRecords.Load() != 0 {
		t.Errorf("reverse cleanup didn't run for the evicted key: totalRecords=%d", d.totalRecords.Load())
	}
	if d.evictDeleteTotal.Load() != 1 {
		t.Errorf("evictDeleteTotal=%d want 1", d.evictDeleteTotal.Load())
	}
}

// TestDeps_OnDelete_DirtyMarksDependentGet — an entry that exact-GET-
// depends on a DIFFERENT object must be dirty-marked, not evicted
// (the F2 falsifier as a permanent regression test).
func TestDeps_OnDelete_DirtyMarksDependentGet(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)

	gvr := gvrCompositions()
	// L1A's own object is (gvr, ns, "owner") — but it GET-depends on
	// the SEPARATE object (gvr, ns, "dependency").
	store.Put("L1A", &ResolvedEntry{
		RawJSON: []byte(`{}`),
		Inputs:  inputsFor(gvr, "ns", "owner"),
	})
	d.Record("L1A", gvr, "ns", "dependency")

	var marked []string
	var mu sync.Mutex
	d.SetRefreshHook(func(k string) { mu.Lock(); marked = append(marked, k); mu.Unlock() })

	got := d.OnDelete(gvr, "ns", "dependency")
	if got != 0 {
		t.Fatalf("OnDelete evicted %d, want 0 (dependent-GET, not self)", got)
	}
	if _, ok := store.Get("L1A"); !ok {
		t.Fatalf("L1A over-evicted — dependent-GET entry must survive")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(marked) != 1 || marked[0] != "L1A" {
		t.Fatalf("dependent-GET entry not dirty-marked: %v", marked)
	}
	if d.dirtyMarkTotal.Load() != 1 {
		t.Errorf("dirtyMarkTotal=%d want 1", d.dirtyMarkTotal.Load())
	}
	if d.evictDeleteTotal.Load() != 0 {
		t.Errorf("evictDeleteTotal=%d want 0 (no self-representation)", d.evictDeleteTotal.Load())
	}
}

func TestDeps_OnDelete_NilStoreIsNoOp(t *testing.T) {
	// Dep tracker should never panic when no store is wired (unit
	// tests can exercise it standalone). With no store, isSelf-
	// Representation cannot read Inputs → every match is conservatively
	// dirty-marked (0 evicted). No self-eviction → RemoveL1Key does not
	// run, so the dep records are deliberately preserved for the
	// (degraded) refresher path.
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()
	d.Record("L1A", gvr, "ns", "n")
	if got := d.OnDelete(gvr, "ns", "n"); got != 0 {
		t.Fatalf("expected 0 evictions with no store, got %d", got)
	}
	// dirty-marked → records preserved (the entry, as far as the tracker
	// can tell, still exists; only a non-store consumer can confirm).
	if d.dirtyMarkTotal.Load() != 1 {
		t.Fatalf("dirtyMarkTotal=%d want 1 (no-store match dirty-marks)", d.dirtyMarkTotal.Load())
	}
}

// --- OnUpdate ----------------------------------------------------------------

func TestDeps_OnUpdate_EnqueuesAndDoesNotEvict(t *testing.T) {
	// Binding rule (feedback_l1_invalidation_delete_only.md): UPDATE
	// MUST NOT evict; it enqueues into the refresher.
	d := newTestDepTracker(t, 1_000)
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)

	gvr := gvrCompositions()
	store.Put("L1A", &ResolvedEntry{RawJSON: []byte(`{}`)})
	d.Record("L1A", gvr, "ns", "n")

	var enqueued []string
	var mu sync.Mutex
	d.SetRefreshHook(func(l1Key string) {
		mu.Lock()
		enqueued = append(enqueued, l1Key)
		mu.Unlock()
	})

	got := d.OnUpdate(gvr, "ns", "n")
	if got != 1 {
		t.Fatalf("OnUpdate returned %d enqueued, want 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(enqueued) != 1 || enqueued[0] != "L1A" {
		t.Fatalf("enqueue list wrong: %v", enqueued)
	}
	// CRITICAL: UPDATE must NOT have evicted.
	if _, ok := store.Get("L1A"); !ok {
		t.Fatalf("OnUpdate evicted L1A — violates feedback_l1_invalidation_delete_only.md")
	}
	// Reverse records must still exist.
	if d.totalRecords.Load() != 1 {
		t.Fatalf("OnUpdate dropped dep records; totalRecords=%d", d.totalRecords.Load())
	}
	if d.enqueueUpdateTotal.Load() != 1 {
		t.Fatalf("enqueueUpdateTotal=%d want 1", d.enqueueUpdateTotal.Load())
	}
}

func TestDeps_OnUpdate_NoHookIsNoOp(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()
	d.Record("L1A", gvr, "ns", "n")
	got := d.OnUpdate(gvr, "ns", "n")
	if got != 1 {
		t.Fatalf("OnUpdate must still return matched count even without a hook, got %d", got)
	}
}

// --- RemoveL1Key (LRU cleanup) ----------------------------------------------

func TestDeps_RemoveL1Key_PurgesForwardAndReverse(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()
	d.Record("L1A", gvr, "ns", "n1")
	d.Record("L1A", gvr, "ns", "n2")
	d.RecordList("L1A", gvr, "ns")

	if got := d.totalRecords.Load(); got != 3 {
		t.Fatalf("setup: totalRecords=%d want 3", got)
	}

	d.RemoveL1Key("L1A")

	if got := d.totalRecords.Load(); got != 0 {
		t.Fatalf("RemoveL1Key left %d records", got)
	}
	// Forward buckets should be empty (or pruned).
	d.forward.Range(func(k, v any) bool {
		ks := v.(*keySet)
		if ks.count.Load() != 0 {
			t.Errorf("forward bucket %v retained %d keys", k, ks.count.Load())
		}
		return true
	})
}

// --- Bounded growth ---------------------------------------------------------

func TestDeps_CapDropsAndWarnsOnce(t *testing.T) {
	d := newTestDepTracker(t, 2) // tiny cap for the test
	gvr := gvrCompositions()
	d.Record("L1A", gvr, "ns", "n1")
	d.Record("L1A", gvr, "ns", "n2")
	d.Record("L1A", gvr, "ns", "n3") // dropped
	d.Record("L1A", gvr, "ns", "n4") // dropped

	if got := d.totalRecords.Load(); got != 2 {
		t.Fatalf("totalRecords=%d want 2 (cap)", got)
	}
	if got := d.recordDroppedCap.Load(); got != 2 {
		t.Fatalf("recordDroppedCap=%d want 2", got)
	}
}

// --- AC-O10 — cap metering --------------------------------------------------

// TestACO10_CapMeteredAgainstMaxRecords asserts totalRecords is metered
// against DEPS_MAX_RECORDS: once the cap is hit, new edges are dropped
// (recordDroppedCap ticks) and totalRecords never exceeds the cap.
func TestACO10_CapMeteredAgainstMaxRecords(t *testing.T) {
	const cap = 5
	d := newTestDepTracker(t, cap)
	gvr := gvrCompositions()

	// Record 8 distinct edges under one L1 key — 5 land, 3 drop.
	for i := 0; i < 8; i++ {
		d.Record("L1A", gvr, "ns", "n"+itoa(i))
	}
	if got := d.totalRecords.Load(); got != cap {
		t.Fatalf("AC-O10: totalRecords=%d want %d (capped)", got, cap)
	}
	if got := d.recordDroppedCap.Load(); got != 3 {
		t.Fatalf("AC-O10: recordDroppedCap=%d want 3", got)
	}
	// The cap-drop WARN is one-shot.
	if !d.capWarned.Load() {
		t.Fatalf("AC-O10: capWarned flag never set — one-shot WARN did not fire")
	}
	if got := d.Stats().RecordDroppedCap; got != 3 {
		t.Fatalf("AC-O10: Stats().RecordDroppedCap=%d want 3", got)
	}
}

// --- AC-O15 — empty-l1Key loud-fail -----------------------------------------

// TestACO15_EmptyKeyProdCounterAndNoPanic asserts that in production
// mode (depsTestMode false — the default) an empty-l1Key Record /
// RecordList / WithL1KeyContext bumps recordDroppedNoKey and does NOT
// panic.
func TestACO15_EmptyKeyProdCounterAndNoPanic(t *testing.T) {
	resetDepsForTest() // also clears depsTestMode → production semantics
	d := Deps()
	gvr := gvrCompositions()

	d.Record("", gvr, "ns", "n")
	d.RecordList("", gvr, "ns")
	_ = WithL1KeyContext(context.Background(), "")

	if got := d.Stats().RecordDroppedNoKey; got != 3 {
		t.Fatalf("AC-O15: recordDroppedNoKey=%d want 3 (Record+RecordList+WithL1KeyContext)", got)
	}
	if got := d.Stats().RecordTotal; got != 0 {
		t.Fatalf("AC-O15: an empty-key call recorded an edge; RecordTotal=%d want 0", got)
	}
}

// TestACO15_EmptyKeyTestModePanics asserts that with the test-only
// toggle on, an empty-l1Key call panics.
func TestACO15_EmptyKeyTestModePanics(t *testing.T) {
	resetDepsForTest()
	SetDepsTestMode(true)
	defer SetDepsTestMode(false)

	d := Deps()
	gvr := gvrCompositions()

	assertPanics(t, "Record", func() { d.Record("", gvr, "ns", "n") })
	assertPanics(t, "RecordList", func() { d.RecordList("", gvr, "ns") })
	assertPanics(t, "WithL1KeyContext", func() { _ = WithL1KeyContext(context.Background(), "") })
}

func assertPanics(t *testing.T, label string, fn func()) {
	t.Helper()
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatalf("AC-O15: %s with empty l1Key did not panic in test mode", label)
		}
	}()
	fn()
}

// TestACO15_TestModeToggleIsNotEnvDriven asserts the test-mode toggle is
// a Go variable, not an env var — a customer cannot enable
// process-killing behaviour via the environment.
func TestACO15_TestModeToggleIsNotEnvDriven(t *testing.T) {
	resetDepsForTest()
	// Setting any plausible env var must NOT enable test mode.
	t.Setenv("DEPS_TEST_MODE", "true")
	t.Setenv("TEST_MODE", "true")
	if depsTestMode.Load() {
		t.Fatalf("AC-O15: depsTestMode is env-driven — it must only be flipped by SetDepsTestMode")
	}
	// The empty-key call must take the prod (counter) path, not panic.
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("AC-O15: empty-key call panicked despite env vars only — toggle leaked to env")
		}
	}()
	Deps().Record("", gvrCompositions(), "ns", "n")
}

// --- Concurrency -----------------------------------------------------------

func TestDeps_ConcurrentRecordAndDelete_RaceFree(t *testing.T) {
	d := newTestDepTracker(t, 100_000)
	store := newResolvedCache(10_000, 1<<24, time.Hour)
	d.SetStore(store)

	gvr := gvrCompositions()

	var wg sync.WaitGroup
	const N = 200

	// Writers: Record + Put.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			key := "L1_" + itoa(i)
			store.Put(key, &ResolvedEntry{RawJSON: []byte("x")})
			d.Record(key, gvr, "ns", "n_"+itoa(i%50))
		}
	}()

	// Deleters: OnDelete sweeps half the names.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			d.OnDelete(gvr, "ns", "n_"+itoa(i%50))
		}
	}()

	// Updaters: OnUpdate sweeps the other half. No real hook needed —
	// we just exercise the lookup path.
	d.SetRefreshHook(func(_ string) {})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			d.OnUpdate(gvr, "ns", "n_"+itoa(i%50))
		}
	}()

	wg.Wait()
	// No assertion on final counts — the goal is race-free under `-race`.
}

// --- 0.30.94 Edge type 3: context helpers + recording semantics --------------

// TestL1KeyContextHelpers asserts the context round-trip + the empty-key
// invariant ("" ≡ do-not-record, parent ctx returned unchanged).
func TestL1KeyContextHelpers(t *testing.T) {
	// Empty key returns parent ctx unchanged (saves allocation; the
	// resolver still sees "" via L1KeyFromContext and skips recording).
	parent := context.Background()
	if got := WithL1KeyContext(parent, ""); got != parent {
		t.Errorf("WithL1KeyContext(empty) must return parent unchanged")
	}
	if got := L1KeyFromContext(parent); got != "" {
		t.Errorf("L1KeyFromContext(bare-ctx)=%q want \"\"", got)
	}
	// nil ctx is tolerated (defensive — production never passes nil).
	if got := L1KeyFromContext(nil); got != "" {
		t.Errorf("L1KeyFromContext(nil)=%q want \"\"", got)
	}
	// Round-trip a real key.
	want := "k1:abcdef"
	child := WithL1KeyContext(parent, want)
	if got := L1KeyFromContext(child); got != want {
		t.Errorf("round-trip mismatch: got %q want %q", got, want)
	}
	// Child preserves cancellation chain.
	dctx, cancel := context.WithCancel(parent)
	cancel()
	chained := WithL1KeyContext(dctx, want)
	if chained.Err() == nil {
		t.Errorf("chained ctx must inherit cancellation from parent")
	}
}

// TestRecordingInnerCallDep_ListScope mimics the resolver loop: the
// resolver derives (gvr, ns, name="") from an apiserver list-form path
// and calls RecordList. A subsequent OnDelete on ANY object in that GVR
// + ns DIRTY-MARKS the L1 key (R2/R7: list-dep → dirty-mark, not evict —
// the dependent entry's own object still exists).
func TestRecordingInnerCallDep_ListScope(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)

	store.Put("L1_admin_list", &ResolvedEntry{RawJSON: []byte(`{"items":[]}`)})

	// Synthesise the resolver-side recording path for a list-form call:
	//   path = /apis/composition.krateo.io/v1/namespaces/bench-ns-02/<resource>
	// ParseAPIServerPathToDep returns name="".
	gvr, ns, name, ok := ParseAPIServerPathToDep(
		"/apis/composition.krateo.io/v1/namespaces/bench-ns-02/githubscaffoldingwithcompositionpages")
	if !ok {
		t.Fatalf("ParseAPIServerPathToDep returned ok=false")
	}
	wantGVR := schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1",
		Resource: "githubscaffoldingwithcompositionpages",
	}
	if gvr != wantGVR {
		t.Fatalf("gvr mismatch: got %v want %v", gvr, wantGVR)
	}
	if name != "" {
		t.Fatalf("list-form must have name=\"\"; got %q", name)
	}
	d.RecordList("L1_admin_list", gvr, ns)

	var marked []string
	var mu sync.Mutex
	d.SetRefreshHook(func(k string) { mu.Lock(); marked = append(marked, k); mu.Unlock() })

	// OnDelete for ANY object of that GVR in ns dirty-marks via list-scope.
	got := d.OnDelete(gvr, "bench-ns-02", "bench-app-02-XX")
	if got != 0 {
		t.Fatalf("OnDelete evicted %d, want 0 (list-dep dirty-marks)", got)
	}
	if _, exists := store.Get("L1_admin_list"); !exists {
		t.Errorf("L1_admin_list must SURVIVE — list-dep is dirty-marked, not evicted")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(marked) != 1 || marked[0] != "L1_admin_list" {
		t.Errorf("L1_admin_list should have been dirty-marked via list-scope dep; got %v", marked)
	}
}

// TestRecordingInnerCallDep_ExactObject mimics the resolver loop for a
// path with a named object: /apis/.../<resource>/<name>. The widget A
// entry GET-depends on the named object as a SEPARATE dependency; a
// list-scope dep is recorded under widget B. A DELETE of the named
// object DIRTY-MARKS both (R2/R7) — neither is the deleted object's own
// self-representation.
func TestRecordingInnerCallDep_ExactObject(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)

	rGVR := gvrCompositions()
	// Widget A's own object is (rGVR, ns, "widget-a-owner") — it merely
	// GET-depends on the composition object below.
	store.Put("L1_widget_a", &ResolvedEntry{
		RawJSON: []byte(`{}`),
		Inputs:  inputsFor(rGVR, "bench-ns-02", "widget-a-owner"),
	})
	store.Put("L1_widget_b", &ResolvedEntry{
		RawJSON: []byte(`{}`),
		Inputs:  inputsFor(rGVR, "bench-ns-02", "widget-b-owner"),
	})

	// Resolver records exact-object dep for the named GET call.
	gvr, ns, name, ok := ParseAPIServerPathToDep(
		"/apis/composition.krateo.io/v1/namespaces/bench-ns-02/githubscaffoldingwithcompositionpages/bench-app-02-06")
	if !ok {
		t.Fatalf("ParseAPIServerPathToDep returned ok=false")
	}
	if name != "bench-app-02-06" {
		t.Fatalf("expected name=bench-app-02-06, got %q", name)
	}
	d.Record("L1_widget_a", gvr, ns, name)
	// A second entry depends on the list-scope bucket for the same gvr+ns.
	d.RecordList("L1_widget_b", gvr, ns)

	var marked []string
	var mu sync.Mutex
	d.SetRefreshHook(func(k string) { mu.Lock(); marked = append(marked, k); mu.Unlock() })

	// DELETE the named object — neither widget is its self-representation,
	// so both are dirty-marked (exact-GET-dep + list-dep).
	got := d.OnDelete(gvr, "bench-ns-02", "bench-app-02-06")
	if got != 0 {
		t.Fatalf("OnDelete evicted %d, want 0 (both deps are non-self)", got)
	}
	if _, exists := store.Get("L1_widget_a"); !exists {
		t.Errorf("L1_widget_a (exact-GET-dep) must survive — dirty-mark, not evict")
	}
	if _, exists := store.Get("L1_widget_b"); !exists {
		t.Errorf("L1_widget_b (list-scope dep) must survive — dirty-mark, not evict")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(marked) != 2 {
		t.Errorf("expected 2 dirty-marks (widget A + widget B); got %v", marked)
	}
}

// TestIteratorEmitsPerNamespaceEdges simulates the load-bearing admin
// compositions-list dispatch: the resolver iterator enumerates one
// list-form path per namespace × 49 namespaces. Each call enters the
// resolver loop and records a distinct (gvr, ns_k, "*") edge. A DELETE
// for an object in ANY of the 49 namespaces must evict the single
// admin L1 entry.
//
// Per plan §0.30.94 "Admin compositions-list: 49 namespaces × 1 LIST
// each records 49 list-scope edges".
func TestIteratorEmitsPerNamespaceEdges(t *testing.T) {
	d := newTestDepTracker(t, 10_000)
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)

	const adminL1 = "L1_admin_compositions"
	store.Put(adminL1, &ResolvedEntry{RawJSON: []byte(`{}`)})

	const N = 49
	// gvr derived from the iterator path — single GVR across all 49 iterations.
	var iterGVR schema.GroupVersionResource
	for i := 0; i < N; i++ {
		ns := "bench-ns-" + itoa(i)
		path := "/apis/composition.krateo.io/v1/namespaces/" + ns +
			"/githubscaffoldingwithcompositionpages"
		gvr, parsedNS, parsedName, ok := ParseAPIServerPathToDep(path)
		if !ok {
			t.Fatalf("parse failed for path=%q", path)
		}
		if parsedName != "" {
			t.Fatalf("expected list-form; got name=%q for path=%q", parsedName, path)
		}
		if i == 0 {
			iterGVR = gvr
		} else if gvr != iterGVR {
			t.Fatalf("iterator GVR drifted at i=%d: got %v want %v", i, gvr, iterGVR)
		}
		d.RecordList(adminL1, gvr, parsedNS)
	}

	// 49 distinct list-scope buckets recorded under one L1 key.
	if got := d.totalRecords.Load(); got != int64(N) {
		t.Fatalf("totalRecords=%d want %d", got, N)
	}

	// collectMatches for ANY of the 49 namespaces returns the L1 key.
	for i := 0; i < N; i++ {
		ns := "bench-ns-" + itoa(i)
		matched := d.collectMatches(iterGVR, ns, "any-name")
		if _, ok := matched[adminL1]; !ok {
			t.Errorf("collectMatches(ns=%s) missing admin L1 key; got=%v", ns, matched)
		}
	}

	// DELETE in one of the 49 namespaces DIRTY-MARKS the admin L1 entry
	// (R2/R7: list-dep → dirty-mark — the admin entry itself still
	// exists; one composition in one of its 49 listed namespaces went
	// away, so the entry is stale-while-revalidate, not evicted).
	var marked []string
	var mu sync.Mutex
	d.SetRefreshHook(func(k string) { mu.Lock(); marked = append(marked, k); mu.Unlock() })

	got := d.OnDelete(iterGVR, "bench-ns-25", "bench-app-XX")
	if got != 0 {
		t.Fatalf("OnDelete evicted %d, want 0 (any-of-49 list-scope dirty-marks)", got)
	}
	if _, exists := store.Get(adminL1); !exists {
		t.Errorf("admin L1 entry must SURVIVE — list-dep is dirty-marked, not evicted")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(marked) != 1 || marked[0] != adminL1 {
		t.Errorf("admin L1 entry should have been dirty-marked; got %v", marked)
	}
}

// TestOnDelete_DirtyMarksViaListScope: a DELETE on a member of a
// list-scope dependency dirty-marks the dependent entry (R2/R7) and
// leaves evictDeleteTotal at 0 — list-deps are never self-evictions.
func TestOnDelete_DirtyMarksViaListScope(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)

	gvr := gvrCompositions()
	const l1Key = "L1_admin_list_dirty"
	store.Put(l1Key, &ResolvedEntry{RawJSON: []byte(`{"items":[]}`)})
	d.RecordList(l1Key, gvr, "bench-ns-02")

	if got := d.evictDeleteTotal.Load(); got != 0 {
		t.Fatalf("evictDeleteTotal=%d (must start at 0)", got)
	}

	var marked []string
	var mu sync.Mutex
	d.SetRefreshHook(func(k string) { mu.Lock(); marked = append(marked, k); mu.Unlock() })

	evicted := d.OnDelete(gvr, "bench-ns-02", "bench-app-02-06")
	if evicted != 0 {
		t.Fatalf("OnDelete evicted=%d want 0 (list-dep dirty-marks)", evicted)
	}
	if got := d.evictDeleteTotal.Load(); got != 0 {
		t.Fatalf("evictDeleteTotal=%d want 0 — list-dep is not a self-eviction", got)
	}
	if got := d.dirtyMarkTotal.Load(); got != 1 {
		t.Fatalf("dirtyMarkTotal=%d want 1", got)
	}
	if _, ok := store.Get(l1Key); !ok {
		t.Fatalf("L1 entry must survive — list-dep is dirty-marked")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(marked) != 1 || marked[0] != l1Key {
		t.Fatalf("L1 entry should have been dirty-marked; got %v", marked)
	}
}

// --- Ship D (0.30.114) — CRD-lifecycle dep-tracker hooks --------------------

// TestDeps_OnResourceTypeAvailable_DirtyMarksListDeps asserts D1: when a
// CRD newly appears, OnResourceTypeAvailable dirty-marks every LIST-scope
// dep for that GVR (any namespace + cluster-wide) and NOTHING else — an
// exact-object GET-dep is NOT a stale-negative LIST and must be left
// alone.
func TestDeps_OnResourceTypeAvailable_DirtyMarksListDeps(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()

	d.RecordList("L1_nslist", gvr, "bench-ns-01")  // ns LIST-dep — must mark
	d.RecordList("L1_clusterlist", gvr, "")        // cluster LIST-dep — must mark
	d.Record("L1_exact", gvr, "bench-ns-01", "n1") // exact GET-dep — must NOT mark

	var marked []string
	var mu sync.Mutex
	d.SetRefreshHook(func(k string) { mu.Lock(); marked = append(marked, k); mu.Unlock() })

	got := d.OnResourceTypeAvailable(gvr)
	if got != 2 {
		t.Fatalf("OnResourceTypeAvailable marked %d, want 2 (ns-list + cluster-list)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[string]bool{"L1_nslist": true, "L1_clusterlist": true}
	for _, k := range marked {
		if !want[k] {
			t.Fatalf("OnResourceTypeAvailable dirty-marked unexpected key %q (got %v)", k, marked)
		}
		delete(want, k)
	}
	if len(want) != 0 {
		t.Fatalf("OnResourceTypeAvailable missed LIST-deps %v (marked %v)", want, marked)
	}
	if d.dirtyMarkTotal.Load() != 2 {
		t.Fatalf("dirtyMarkTotal=%d want 2", d.dirtyMarkTotal.Load())
	}
}

// TestDeps_OnResourceTypeAvailable_NoMatchIsNoOp asserts AC-D4: a CRD-add
// for a GVR with no LIST-dep dirty-marks nothing, and a repeat call is
// also a no-op (idempotent — no LIST-dep was consumed/removed).
func TestDeps_OnResourceTypeAvailable_NoMatchIsNoOp(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()
	other := schema.GroupVersionResource{Group: gvr.Group, Version: "v1", Resource: "others"}

	d.RecordList("L1_other", other, "ns") // LIST-dep for a different GVR

	var marked []string
	var mu sync.Mutex
	d.SetRefreshHook(func(k string) { mu.Lock(); marked = append(marked, k); mu.Unlock() })

	if got := d.OnResourceTypeAvailable(gvr); got != 0 {
		t.Fatalf("OnResourceTypeAvailable marked %d, want 0 (no matching LIST-dep)", got)
	}
	// Idempotent: a second call is still a no-op.
	if got := d.OnResourceTypeAvailable(gvr); got != 0 {
		t.Fatalf("repeated OnResourceTypeAvailable marked %d, want 0", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(marked) != 0 {
		t.Fatalf("OnResourceTypeAvailable dirty-marked %v, want none", marked)
	}
	if d.dirtyMarkTotal.Load() != 0 {
		t.Fatalf("dirtyMarkTotal=%d want 0", d.dirtyMarkTotal.Load())
	}
}

// TestDeps_OnResourceTypeRemoved_DirtyMarksListAndGetDeps asserts D2: a
// CRD removal dirty-marks BOTH LIST-deps AND dependent GET-deps for the
// GVR, and NEVER evicts (a CRD removal is a type-removal, not a single
// object's DELETE — feedback_l1_invalidation_delete_only.md).
func TestDeps_OnResourceTypeRemoved_DirtyMarksListAndGetDeps(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)
	gvr := gvrCompositions()

	store.Put("L1_list", &ResolvedEntry{RawJSON: []byte(`{}`)})
	store.Put("L1_get", &ResolvedEntry{RawJSON: []byte(`{}`)})
	// A self-representation of the GVR — even THIS must be dirty-marked,
	// not evicted: a CRD removal is never a single-object DELETE.
	store.Put("L1_self", &ResolvedEntry{
		RawJSON: []byte(`{}`),
		Inputs:  inputsFor(gvr, "ns", "self-obj"),
	})

	d.RecordList("L1_list", gvr, "ns")         // LIST-dep
	d.Record("L1_get", gvr, "ns", "thing-1")   // dependent GET-dep
	d.Record("L1_self", gvr, "ns", "self-obj") // self-representation GET-dep

	var marked []string
	var mu sync.Mutex
	d.SetRefreshHook(func(k string) { mu.Lock(); marked = append(marked, k); mu.Unlock() })

	got := d.OnResourceTypeRemoved(gvr)
	if got != 3 {
		t.Fatalf("OnResourceTypeRemoved marked %d, want 3 (LIST + GET + self)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(marked) != 3 {
		t.Fatalf("OnResourceTypeRemoved marked %v, want 3 keys", marked)
	}
	for _, k := range []string{"L1_list", "L1_get", "L1_self"} {
		if _, ok := store.Get(k); !ok {
			t.Fatalf("OnResourceTypeRemoved EVICTED %q — CRD removal must dirty-mark, never evict", k)
		}
	}
	if d.evictDeleteTotal.Load() != 0 {
		t.Fatalf("evictDeleteTotal=%d want 0 — CRD removal must never evict", d.evictDeleteTotal.Load())
	}
	if d.dirtyMarkTotal.Load() != 3 {
		t.Fatalf("dirtyMarkTotal=%d want 3", d.dirtyMarkTotal.Load())
	}
}

// TestDeps_OnResourceTypeRemoved_NoMatchIsNoOp asserts AC-D4: a CRD
// removal for a GVR with no deps is a no-op and idempotent on repeat.
func TestDeps_OnResourceTypeRemoved_NoMatchIsNoOp(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()
	other := schema.GroupVersionResource{Group: gvr.Group, Version: "v1", Resource: "others"}
	d.RecordList("L1_other", other, "ns")

	var marked []string
	var mu sync.Mutex
	d.SetRefreshHook(func(k string) { mu.Lock(); marked = append(marked, k); mu.Unlock() })

	if got := d.OnResourceTypeRemoved(gvr); got != 0 {
		t.Fatalf("OnResourceTypeRemoved marked %d, want 0 (no matching dep)", got)
	}
	if got := d.OnResourceTypeRemoved(gvr); got != 0 {
		t.Fatalf("repeated OnResourceTypeRemoved marked %d, want 0", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(marked) != 0 {
		t.Fatalf("OnResourceTypeRemoved dirty-marked %v, want none", marked)
	}
}

// TestDeps_OnResourceType_NilReceiverSafe asserts the Ship D hooks are
// nil-receiver-safe (the no-op tracker contract under CACHE_ENABLED=false
// — AC-D5).
func TestDeps_OnResourceType_NilReceiverSafe(t *testing.T) {
	var d *DepTracker
	if got := d.OnResourceTypeAvailable(gvrCompositions()); got != 0 {
		t.Fatalf("nil-receiver OnResourceTypeAvailable returned %d, want 0", got)
	}
	if got := d.OnResourceTypeRemoved(gvrCompositions()); got != 0 {
		t.Fatalf("nil-receiver OnResourceTypeRemoved returned %d, want 0", got)
	}
}

// --- helpers ---------------------------------------------------------------

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	buf := make([]byte, 0, 10)
	for i > 0 {
		buf = append([]byte{digits[i%10]}, buf...)
		i /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
