package cache

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/workqueue"
)

// newTestWatcher returns a ResourceWatcher wired with a fresh MemCache and
// just-enough internals to drive triggerL1RefreshBatch from unit tests.
// Workers are NOT started — tests inspect the queues directly.
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
	rw.l1Refresh.Store(L1RefreshFunc(func(_ context.Context, _ schema.GroupVersionResource, _ []string) []string {
		return nil
	}))
	rw.synced.Store(true)
	return rw
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

// TestTriggerL1RefreshBatch_AddEagerlyDeletes verifies that ADD events evict
// api-result keys synchronously. The user-facing tier (snowplow:resolved:*)
// is refreshed by processItem → ResolveWidget, whose inner /call paths read
// api-result first. Eager eviction forces those inner reads to fall through
// to the (already-fresh) informer instead of returning pre-event bytes.
func TestTriggerL1RefreshBatch_AddEagerlyDeletes(t *testing.T) {
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

	if _, hit, _ := rw.cache.GetRaw(context.Background(), apiKey); hit {
		t.Fatalf("ADD must remove api-result key; still cached")
	}
}

// TestTriggerL1RefreshBatch_UpdateDoesNotTouchAPIResult asserts that
// UPDATE/PATCH events leave api-result alone — natural TTL (60s) handles
// in-place mutations.
func TestTriggerL1RefreshBatch_UpdateDoesNotTouchAPIResult(t *testing.T) {
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
}

// TestTriggerL1RefreshBatch_DeleteEagerlyDeletes is a regression guard for
// DELETE: api-result key is gone.
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
}

// TestTriggerL1RefreshBatch_BatchedAddDelete: a mixed batch must still evict
// (both branches now share identical eager-delete semantics).
func TestTriggerL1RefreshBatch_BatchedAddDelete(t *testing.T) {
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
		t.Fatalf("mixed ADD+DELETE: api-result key must be removed")
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
