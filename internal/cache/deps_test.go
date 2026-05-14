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

	d.Record("L1_exact", gvr, "bench-ns-01", "app-1")    // exact
	d.RecordList("L1_nslist", gvr, "bench-ns-01")        // ns-list
	d.Record("L1_clustname", gvr, "", "app-1")           // cluster-name (rare)
	d.RecordList("L1_clustlist", gvr, "")                // cluster-list

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

// --- OnDelete ----------------------------------------------------------------

func TestDeps_OnDelete_EvictsAndClearsReverse(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)

	gvr := gvrCompositions()
	// Two L1 entries, both depending on (gvr, ns, "app-1").
	store.Put("L1A", &ResolvedEntry{RawJSON: []byte(`{"a":1}`)})
	store.Put("L1B", &ResolvedEntry{RawJSON: []byte(`{"b":2}`)})
	d.Record("L1A", gvr, "bench-ns-01", "app-1")
	d.Record("L1B", gvr, "bench-ns-01", "app-1")

	got := d.OnDelete(gvr, "bench-ns-01", "app-1")
	if got != 2 {
		t.Fatalf("OnDelete returned %d evicted, want 2", got)
	}
	if _, ok := store.Get("L1A"); ok {
		t.Errorf("L1A should have been evicted by DELETE")
	}
	if _, ok := store.Get("L1B"); ok {
		t.Errorf("L1B should have been evicted by DELETE")
	}
	if d.totalRecords.Load() != 0 {
		t.Errorf("reverse cleanup didn't run: totalRecords=%d", d.totalRecords.Load())
	}
	if d.evictDeleteTotal.Load() != 2 {
		t.Errorf("evictDeleteTotal=%d want 2", d.evictDeleteTotal.Load())
	}
}

func TestDeps_OnDelete_NilStoreIsNoOp(t *testing.T) {
	// Dep tracker should never panic when no store is wired (unit
	// tests can exercise it standalone). It still cleans up dep
	// records — the L1 layer is the only side it can't touch.
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()
	d.Record("L1A", gvr, "ns", "n")
	if got := d.OnDelete(gvr, "ns", "n"); got != 0 {
		t.Fatalf("expected 0 evictions with no store, got %d", got)
	}
	// But the dep records ARE cleaned (RemoveL1Key runs anyway).
	if d.totalRecords.Load() != 0 {
		t.Fatalf("totalRecords=%d after OnDelete; cleanup didn't fire", d.totalRecords.Load())
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
// + ns must evict the L1 key.
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

	// OnDelete for ANY object of that GVR in ns evicts via list-scope.
	got := d.OnDelete(gvr, "bench-ns-02", "bench-app-02-XX")
	if got != 1 {
		t.Fatalf("OnDelete returned %d evicted, want 1 (list-scope match)", got)
	}
	if _, exists := store.Get("L1_admin_list"); exists {
		t.Errorf("L1_admin_list should have been evicted via list-scope dep")
	}
}

// TestRecordingInnerCallDep_ExactObject mimics the resolver loop for a
// path with a named object: /apis/.../<resource>/<name>. Records an
// exact-object dep. Verifies four-bucket union: a DELETE for the same
// (gvr, ns, name) matches the exact bucket, and a separate list-scope
// dep recorded under a different L1 key also matches per the
// 4-bucket-union spec at deps.go:348.
func TestRecordingInnerCallDep_ExactObject(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)

	store.Put("L1_widget_a", &ResolvedEntry{RawJSON: []byte(`{}`)})
	store.Put("L1_widget_b", &ResolvedEntry{RawJSON: []byte(`{}`)})

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

	// DELETE the exact object — both deps must match (exact + ns-list).
	got := d.OnDelete(gvr, "bench-ns-02", "bench-app-02-06")
	if got != 2 {
		t.Fatalf("OnDelete returned %d evicted, want 2 (exact + ns-list union)", got)
	}
	if _, exists := store.Get("L1_widget_a"); exists {
		t.Errorf("L1_widget_a (exact dep) should have been evicted")
	}
	if _, exists := store.Get("L1_widget_b"); exists {
		t.Errorf("L1_widget_b (list-scope dep) should have been evicted")
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

	// DELETE in one of the 49 namespaces evicts the admin L1 entry.
	got := d.OnDelete(iterGVR, "bench-ns-25", "bench-app-XX")
	if got != 1 {
		t.Fatalf("OnDelete returned %d evicted, want 1 (any-of-49 list-scope)", got)
	}
	if _, exists := store.Get(adminL1); exists {
		t.Errorf("admin L1 entry should have been evicted")
	}
}

// TestOnDelete_EvictsViaListScope is the load-bearing Gate 1 unit-test:
// the dep tracker is populated with a list-scope dep (mimicking the
// resolver-side Edge type 3 recording), a DELETE is fired on a specific
// (gvr, ns, name) tuple in that scope, and the evict counter MUST tick.
// This is the failure mode the 0.30.92 probe captured at evict_delete=0.
func TestOnDelete_EvictsViaListScope(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)

	gvr := gvrCompositions()
	const l1Key = "L1_admin_list_evict"
	store.Put(l1Key, &ResolvedEntry{RawJSON: []byte(`{"items":[]}`)})
	d.RecordList(l1Key, gvr, "bench-ns-02")

	// Pre-condition: evict counter is zero.
	if got := d.evictDeleteTotal.Load(); got != 0 {
		t.Fatalf("evictDeleteTotal=%d (must start at 0)", got)
	}

	// Fire OnDelete — list-scope dep MUST match.
	evicted := d.OnDelete(gvr, "bench-ns-02", "bench-app-02-06")
	if evicted != 1 {
		t.Fatalf("OnDelete evicted=%d want 1", evicted)
	}
	if got := d.evictDeleteTotal.Load(); got != 1 {
		t.Fatalf("evictDeleteTotal=%d want 1 (this counter is the Gate 1 falsifier)", got)
	}
	if _, ok := store.Get(l1Key); ok {
		t.Fatalf("L1 entry not removed from store after OnDelete")
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
