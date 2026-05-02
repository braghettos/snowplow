package cache

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/workqueue"
)

// stubInformerReader is a minimal InformerReader for unit tests. It records
// whether GetObject/ListObjects were invoked so tests can assert the
// cold-identity guard short-circuits before any informer read.
type stubInformerReader struct {
	listCalls atomic.Int64
	getCalls  atomic.Int64
	listFn    func(gvr schema.GroupVersionResource, ns string) ([]*unstructured.Unstructured, bool)
	getFn     func(gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, bool)
}

func (s *stubInformerReader) ListObjects(gvr schema.GroupVersionResource, ns string) ([]*unstructured.Unstructured, bool) {
	s.listCalls.Add(1)
	if s.listFn != nil {
		return s.listFn(gvr, ns)
	}
	return nil, false
}

func (s *stubInformerReader) GetObject(gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, bool) {
	s.getCalls.Add(1)
	if s.getFn != nil {
		return s.getFn(gvr, ns, name)
	}
	return nil, false
}

// newTestWatcher returns a ResourceWatcher wired with a fresh MemCache and
// just-enough internals to drive triggerL1RefreshBatch / refresh paths
// from unit tests. Workers are NOT started — tests inspect the queues
// directly.
func newTestWatcher() *ResourceWatcher {
	c := NewMem(time.Hour)
	rw := &ResourceWatcher{
		cache:       c,
		watched:     make(map[string]bool),
		eventCh:     make(chan l1Event, 100),
		dynamicGVRs: make(map[string]schema.GroupVersionResource),
		hotQ: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](1*time.Second, 30*time.Second),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "test-hot"},
		),
		warmQ: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](1*time.Second, 30*time.Second),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "test-warm"},
		),
		coldQ: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](1*time.Second, 30*time.Second),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "test-cold"},
		),
	}
	// Install a no-op L1 refresh function so triggerL1RefreshBatch's
	// nil-guard passes.
	rw.l1Refresh.Store(L1RefreshFunc(func(_ context.Context, _ schema.GroupVersionResource, _ []string) []string {
		return nil
	}))
	rw.synced.Store(true)
	return rw
}

// drainQueueItems drains all currently-queued items from q (without
// invoking workers) and returns them in arbitrary order. Each Get is
// matched with Done so the queue doesn't claim items are still in flight.
func drainQueueItems(q workqueue.TypedRateLimitingInterface[string]) []string {
	var out []string
	for q.Len() > 0 {
		item, shutdown := q.Get()
		if shutdown {
			break
		}
		out = append(out, item)
		q.Done(item)
	}
	return out
}

func sampleGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-1-52-3", Resource: "compositions"}
}

// seedClusterDep registers the api-result key as a member of the
// cluster-wide dep set for the given GVR, mimicking what
// RegisterL1Dependencies does after an initial resolve.
func seedClusterDep(t *testing.T, c Cache, gvr schema.GroupVersionResource, apiKey string) {
	t.Helper()
	clusterDep := L1ResourceDepKey(GVRToKey(gvr), "", "")
	if err := c.SAddWithTTL(context.Background(), clusterDep, apiKey, ReverseIndexTTL); err != nil {
		t.Fatalf("seedClusterDep SAddWithTTL: %v", err)
	}
}

// ctxWithIR returns a background context carrying the supplied
// InformerReader so refreshSingleAPIResult routes informer reads through
// the test stub.
func ctxWithIR(ir InformerReader) context.Context {
	return WithInformerReader(context.Background(), ir)
}

// ── Tests ────────────────────────────────────────────────────────────────────

// TestTriggerL1RefreshBatch_AddEnqueuesRefreshNotDelete verifies the SWR
// contract for ADD: the api-result key keeps its (stale) bytes immediately
// after the batch runs, and a refresh sentinel lands on hotQ for later
// processing by the worker.
func TestTriggerL1RefreshBatch_AddEnqueuesRefreshNotDelete(t *testing.T) {
	rw := newTestWatcher()
	gvr := sampleGVR()
	apiKey := APIResultKey("identity-A", gvr, "", "")
	stale := []byte(`{"items":["stale"]}`)

	if err := rw.cache.SetAPIResultRaw(context.Background(), apiKey, stale); err != nil {
		t.Fatalf("seed SetAPIResultRaw: %v", err)
	}
	seedClusterDep(t, rw.cache, gvr, apiKey)

	rw.triggerL1RefreshBatch(context.Background(), []l1Event{{
		gvr: gvr, gvrKey: GVRToKey(gvr), ns: "", name: "new-comp", eventType: "add",
	}})

	// Stale bytes still served — SWR.
	got, hit, _ := rw.cache.GetRaw(context.Background(), apiKey)
	if !hit || string(got) != string(stale) {
		t.Fatalf("expected stale bytes still cached after ADD batch (hit=%v got=%q)", hit, got)
	}

	// Sentinel enqueued on hotQ.
	items := drainQueueItems(rw.hotQ)
	wantItem := apiResultRefreshPrefix + apiKey
	found := false
	for _, it := range items {
		if it == wantItem {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected hotQ to contain sentinel %q, got %v", wantItem, items)
	}
}

// TestRefreshSingleAPIResult_OverwritesInPlace verifies the worker entry
// point: stale bytes are replaced with a fresh marshal of the informer
// state, and the new entry contains the full item list.
func TestRefreshSingleAPIResult_OverwritesInPlace(t *testing.T) {
	rw := newTestWatcher()
	gvr := sampleGVR()
	apiKey := APIResultKey("identity-A", gvr, "default", "")
	stale := []byte(`{"items":["stale"]}`)
	if err := rw.cache.SetAPIResultRaw(context.Background(), apiKey, stale); err != nil {
		t.Fatalf("seed SetAPIResultRaw: %v", err)
	}

	const N = 5000
	items := make([]*unstructured.Unstructured, 0, N)
	for i := 0; i < N; i++ {
		u := &unstructured.Unstructured{}
		u.SetName("comp-" + itoa(i))
		u.SetNamespace("default")
		items = append(items, u)
	}
	stub := &stubInformerReader{
		listFn: func(_ schema.GroupVersionResource, _ string) ([]*unstructured.Unstructured, bool) {
			return items, true
		},
	}

	rw.refreshSingleAPIResult(ctxWithIR(stub), apiKey)

	got, hit, _ := rw.cache.GetRaw(context.Background(), apiKey)
	if !hit {
		t.Fatalf("api-result key missing after refresh")
	}
	if string(got) == string(stale) {
		t.Fatalf("refresh did not overwrite stale bytes")
	}
	var doc map[string]any
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatalf("refresh produced non-JSON bytes: %v", err)
	}
	gotItems, _ := doc["items"].([]any)
	if len(gotItems) != N {
		t.Fatalf("refresh items=%d, want %d", len(gotItems), N)
	}
}

// TestRefreshSingleAPIResult_SkipsColdIdentity verifies the cold-identity
// guard: if the api-result key is absent (TTL-evicted), the worker must
// short-circuit before touching the informer, avoiding proactive warm-up.
func TestRefreshSingleAPIResult_SkipsColdIdentity(t *testing.T) {
	rw := newTestWatcher()
	gvr := sampleGVR()
	apiKey := APIResultKey("identity-cold", gvr, "", "")

	// Seed the dep set (orphan) but NOT the api-result key.
	seedClusterDep(t, rw.cache, gvr, apiKey)

	stub := &stubInformerReader{
		listFn: func(_ schema.GroupVersionResource, _ string) ([]*unstructured.Unstructured, bool) {
			return []*unstructured.Unstructured{}, true
		},
	}

	rw.refreshSingleAPIResult(ctxWithIR(stub), apiKey)

	if _, hit, _ := rw.cache.GetRaw(context.Background(), apiKey); hit {
		t.Fatalf("cold identity was warmed up despite TTL-eviction guard")
	}
	if got := stub.listCalls.Load(); got != 0 {
		t.Fatalf("expected 0 informer ListObjects calls, got %d", got)
	}
	if got := stub.getCalls.Load(); got != 0 {
		t.Fatalf("expected 0 informer GetObject calls, got %d", got)
	}
}

// TestTriggerL1RefreshBatch_UpdateDoesNotEnqueueRefresh asserts that
// UPDATE/PATCH events keep the existing SWR-via-TTL contract: no eager
// delete, no async refresh enqueue.
func TestTriggerL1RefreshBatch_UpdateDoesNotEnqueueRefresh(t *testing.T) {
	rw := newTestWatcher()
	gvr := sampleGVR()
	apiKey := APIResultKey("identity-A", gvr, "", "")
	stale := []byte(`{"items":["stale"]}`)
	if err := rw.cache.SetAPIResultRaw(context.Background(), apiKey, stale); err != nil {
		t.Fatalf("seed SetAPIResultRaw: %v", err)
	}
	seedClusterDep(t, rw.cache, gvr, apiKey)

	rw.triggerL1RefreshBatch(context.Background(), []l1Event{{
		gvr: gvr, gvrKey: GVRToKey(gvr), ns: "", name: "comp", eventType: "update",
	}})

	got, hit, _ := rw.cache.GetRaw(context.Background(), apiKey)
	if !hit || string(got) != string(stale) {
		t.Fatalf("UPDATE must not touch api-result key; got hit=%v val=%q", hit, got)
	}
	for _, it := range drainQueueItems(rw.hotQ) {
		if strings.HasPrefix(it, apiResultRefreshPrefix) {
			t.Fatalf("UPDATE enqueued api-result refresh sentinel %q", it)
		}
	}
}

// TestTriggerL1RefreshBatch_DeleteEagerlyDeletes is a regression guard for
// DELETE: api-result key is gone, no refresh enqueue.
func TestTriggerL1RefreshBatch_DeleteEagerlyDeletes(t *testing.T) {
	rw := newTestWatcher()
	gvr := sampleGVR()
	apiKey := APIResultKey("identity-A", gvr, "", "")
	stale := []byte(`{"items":["stale"]}`)
	if err := rw.cache.SetAPIResultRaw(context.Background(), apiKey, stale); err != nil {
		t.Fatalf("seed SetAPIResultRaw: %v", err)
	}
	seedClusterDep(t, rw.cache, gvr, apiKey)

	rw.triggerL1RefreshBatch(context.Background(), []l1Event{{
		gvr: gvr, gvrKey: GVRToKey(gvr), ns: "", name: "comp", eventType: "delete",
	}})

	if _, hit, _ := rw.cache.GetRaw(context.Background(), apiKey); hit {
		t.Fatalf("DELETE must remove api-result key; still cached")
	}
	for _, it := range drainQueueItems(rw.hotQ) {
		if strings.HasPrefix(it, apiResultRefreshPrefix) {
			t.Fatalf("DELETE must not enqueue api-result refresh; got %q", it)
		}
	}
}

// TestTriggerL1RefreshBatch_BatchedAddDeleteHandlesEager covers the
// `hasAdd && !hasDelete` guard: when a batch contains both ADD and DELETE,
// the eager Delete wins and no refresh enqueue happens (the next reader
// will lazy-load).
func TestTriggerL1RefreshBatch_BatchedAddDeleteHandlesEager(t *testing.T) {
	rw := newTestWatcher()
	gvr := sampleGVR()
	apiKey := APIResultKey("identity-A", gvr, "", "")
	stale := []byte(`{"items":["stale"]}`)
	if err := rw.cache.SetAPIResultRaw(context.Background(), apiKey, stale); err != nil {
		t.Fatalf("seed SetAPIResultRaw: %v", err)
	}
	seedClusterDep(t, rw.cache, gvr, apiKey)

	rw.triggerL1RefreshBatch(context.Background(), []l1Event{
		{gvr: gvr, gvrKey: GVRToKey(gvr), ns: "", name: "comp-a", eventType: "add"},
		{gvr: gvr, gvrKey: GVRToKey(gvr), ns: "", name: "comp-d", eventType: "delete"},
	})

	if _, hit, _ := rw.cache.GetRaw(context.Background(), apiKey); hit {
		t.Fatalf("mixed ADD+DELETE: delete must win and remove the api-result key")
	}
	for _, it := range drainQueueItems(rw.hotQ) {
		if strings.HasPrefix(it, apiResultRefreshPrefix) {
			t.Fatalf("mixed ADD+DELETE must not enqueue refresh (delete won); got %q", it)
		}
	}
}

// TestEnqueueAPIResultRefresh_DedupesIdenticalKeys verifies the storm
// shield: the K8s workqueue's idempotent Add collapses N enqueues for the
// same key to at most 2 distinct items in the worst case (in-flight +
// latest queued — standard controller pattern).
func TestEnqueueAPIResultRefresh_DedupesIdenticalKeys(t *testing.T) {
	rw := newTestWatcher()
	gvr := sampleGVR()
	apiKey := APIResultKey("identity-A", gvr, "", "")

	for i := 0; i < 100; i++ {
		rw.enqueueAPIResultRefresh([]string{apiKey})
	}

	items := drainQueueItems(rw.hotQ)
	if len(items) > 2 {
		t.Fatalf("workqueue dedup failed: expected <= 2 items, got %d (%v)", len(items), items)
	}
	if len(items) == 0 {
		t.Fatalf("expected at least one enqueued sentinel, got 0")
	}
	want := apiResultRefreshPrefix + apiKey
	for _, it := range items {
		if it != want {
			t.Fatalf("unexpected queue item %q (want %q)", it, want)
		}
	}
}

// TestSWR_ReadDuringRefreshSeesStale codifies the SWR contract: a reader
// that races the refresh worker either sees the previous bytes or the new
// bytes — never empty, never blocked.
func TestSWR_ReadDuringRefreshSeesStale(t *testing.T) {
	rw := newTestWatcher()
	gvr := sampleGVR()
	apiKey := APIResultKey("identity-A", gvr, "default", "")
	v1 := []byte(`{"v":1}`)
	if err := rw.cache.SetAPIResultRaw(context.Background(), apiKey, v1); err != nil {
		t.Fatalf("seed SetAPIResultRaw: %v", err)
	}

	gate := make(chan struct{})
	stub := &stubInformerReader{
		listFn: func(_ schema.GroupVersionResource, _ string) ([]*unstructured.Unstructured, bool) {
			<-gate
			return []*unstructured.Unstructured{}, true
		},
	}

	done := make(chan struct{})
	go func() {
		rw.refreshSingleAPIResult(ctxWithIR(stub), apiKey)
		close(done)
	}()

	// While the worker is blocked inside ListObjects, the reader sees v1.
	got, hit, _ := rw.cache.GetRaw(context.Background(), apiKey)
	if !hit || string(got) != string(v1) {
		t.Fatalf("SWR violated: reader saw %q (hit=%v) during refresh", got, hit)
	}

	close(gate)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("refresh did not complete in time")
	}

	got2, hit2, _ := rw.cache.GetRaw(context.Background(), apiKey)
	if !hit2 {
		t.Fatalf("api-result key missing post-refresh")
	}
	if string(got2) == string(v1) {
		t.Fatalf("refresh did not overwrite stale bytes; still %q", got2)
	}
}

// TestParseAPIResultKey_RoundTrip ensures the parser is symmetric with
// APIResultKey for both the LIST (no name) and GET (with name) forms.
func TestParseAPIResultKey_RoundTrip(t *testing.T) {
	gvr := sampleGVR()
	cases := []struct {
		identity, ns, name string
	}{
		{"id-a", "", ""},
		{"id-b", "ns-1", ""},
		{"id-c", "", "n-1"},
		{"id-d", "ns-2", "n-2"},
	}
	for _, tc := range cases {
		k := APIResultKey(tc.identity, gvr, tc.ns, tc.name)
		gotID, gotGVR, gotNS, gotName, ok := ParseAPIResultKey(k)
		if !ok {
			t.Errorf("ParseAPIResultKey(%q) = ok=false; want ok=true", k)
			continue
		}
		if gotID != tc.identity || gotNS != tc.ns || gotName != tc.name {
			t.Errorf("round-trip mismatch for %q: got (id=%q ns=%q name=%q), want (id=%q ns=%q name=%q)",
				k, gotID, gotNS, gotName, tc.identity, tc.ns, tc.name)
		}
		if gotGVR != gvr {
			t.Errorf("round-trip GVR mismatch for %q: got %v, want %v", k, gotGVR, gvr)
		}
	}
	if _, _, _, _, ok := ParseAPIResultKey("not-a-snowplow-key"); ok {
		t.Errorf("ParseAPIResultKey accepted malformed key")
	}
}

// ── Test helpers ─────────────────────────────────────────────────────────────

// itoa is a tiny strconv.Itoa shim that avoids importing strconv just for
// the test file.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
