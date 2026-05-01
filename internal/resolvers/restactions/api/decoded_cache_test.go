package api

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func mkObj(ns, name, rv string, gen int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       ns,
			"resourceVersion": rv,
			"generation":      gen,
		},
		"spec": map[string]any{
			"replicas": int32(3),
		},
	}}
	return u
}

func mkKey(group, ver, res, ns, name string) decodedCacheKey {
	return decodedCacheKey{
		gvr:  schema.GroupVersionResource{Group: group, Version: ver, Resource: res},
		ns:   ns,
		name: name,
	}
}

// TestDecodedTreeCache_GetHitMiss covers the basic GET-branch lifecycle:
// store, retrieve, return float64-normalized numerics. No RV change.
func TestDecodedTreeCache_GetHitMiss(t *testing.T) {
	c := newDecodedTreeCache(8)
	key := mkKey("apps", "v1", "deployments", "default", "demo")

	if _, hit := c.Get(key, 1); hit {
		t.Fatal("expected miss on empty cache")
	}

	canonical := safeCopyJSON(mkObj("default", "demo", "10", 7).Object)
	c.Set(key, 1, canonical)

	got, hit := c.Get(key, 1)
	if !hit {
		t.Fatal("expected hit after Set")
	}
	m := got.(map[string]any)
	meta := m["metadata"].(map[string]any)
	if _, ok := meta["generation"].(float64); !ok {
		t.Fatalf("metadata.generation = %T, want float64", meta["generation"])
	}
}

// TestDecodedTreeCache_StaleStampInvalidates verifies that a stamp mismatch
// (i.e. the underlying object's RV changed) is treated as a miss AND the
// stale entry is dropped immediately. This avoids paying the stale-then-miss
// cost on subsequent reads.
func TestDecodedTreeCache_StaleStampInvalidates(t *testing.T) {
	c := newDecodedTreeCache(8)
	key := mkKey("apps", "v1", "deployments", "default", "demo")
	c.Set(key, 1, map[string]any{"v": float64(1)})
	if c.Len() != 1 {
		t.Fatalf("Len after Set = %d, want 1", c.Len())
	}

	if _, hit := c.Get(key, 2); hit {
		t.Fatal("expected miss on stamp mismatch")
	}
	if c.Len() != 0 {
		t.Fatalf("Len after stale Get = %d, want 0 (entry should be evicted)", c.Len())
	}
}

// TestDecodedTreeCache_ReturnedTreeIndependent verifies that mutating the
// tree returned from Get does NOT corrupt the cached canonical tree, so
// the next Get of the same key returns a clean copy.
//
// This is the gojq-safety contract: gojq.normalizeNumbers / deleteEmpty
// mutate the input map in-place.
func TestDecodedTreeCache_ReturnedTreeIndependent(t *testing.T) {
	c := newDecodedTreeCache(8)
	key := mkKey("", "v1", "pods", "default", "demo")
	canonical := map[string]any{
		"items": []any{
			map[string]any{"k": "v1"},
		},
	}
	c.Set(key, 1, canonical)

	a, _ := c.Get(key, 1)
	b, _ := c.Get(key, 1)

	// Mutate a; b must remain untouched.
	a.(map[string]any)["items"].([]any)[0].(map[string]any)["k"] = "MUTATED-A"

	bItems := b.(map[string]any)["items"].([]any)
	if bItems[0].(map[string]any)["k"] != "v1" {
		t.Fatalf("second Get returned aliased tree: items[0].k = %v",
			bItems[0].(map[string]any)["k"])
	}

	// Also check the canonical tree itself is untouched.
	if canonical["items"].([]any)[0].(map[string]any)["k"] != "v1" {
		t.Fatalf("canonical tree was mutated through Get")
	}
}

// TestDecodedTreeCache_LRUEviction confirms that the LRU policy evicts the
// oldest entry when capacity is exceeded.
func TestDecodedTreeCache_LRUEviction(t *testing.T) {
	c := newDecodedTreeCache(2)
	k1 := mkKey("", "v1", "pods", "ns", "a")
	k2 := mkKey("", "v1", "pods", "ns", "b")
	k3 := mkKey("", "v1", "pods", "ns", "c")
	c.Set(k1, 1, map[string]any{"x": float64(1)})
	c.Set(k2, 1, map[string]any{"x": float64(2)})
	// Touch k1 so k2 becomes LRU.
	if _, hit := c.Get(k1, 1); !hit {
		t.Fatal("k1 should be hit")
	}
	c.Set(k3, 1, map[string]any{"x": float64(3)})

	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
	if _, hit := c.Get(k2, 1); hit {
		t.Fatal("k2 should have been evicted (it was LRU)")
	}
	if _, hit := c.Get(k1, 1); !hit {
		t.Fatal("k1 must still be present")
	}
	if _, hit := c.Get(k3, 1); !hit {
		t.Fatal("k3 must still be present")
	}
}

// TestDecodedTreeCache_DisabledCapacity verifies that capacity<=0 disables
// the cache entirely (Get always miss, Set is no-op). Used as a safety
// fallback if we ever want to disable the cache via config.
func TestDecodedTreeCache_DisabledCapacity(t *testing.T) {
	c := newDecodedTreeCache(0)
	key := mkKey("", "v1", "pods", "ns", "a")
	c.Set(key, 1, map[string]any{"x": float64(1)})
	if _, hit := c.Get(key, 1); hit {
		t.Fatal("disabled cache must always miss")
	}
}

// TestStampForObject_RVDrivesStamp confirms that the stamp depends only on
// resourceVersion, not on any other field. Two objects with the same RV but
// different generation/spec must produce the same stamp.
func TestStampForObject_RVDrivesStamp(t *testing.T) {
	a := mkObj("ns", "demo", "100", 7)
	b := mkObj("ns", "demo", "100", 999) // different generation, same RV
	c := mkObj("ns", "demo", "101", 7)   // different RV

	if stampForObject(a) != stampForObject(b) {
		t.Fatal("same RV must produce same stamp regardless of other fields")
	}
	if stampForObject(a) == stampForObject(c) {
		t.Fatal("different RV must produce different stamp")
	}
	if stampForObject(nil) != 0 {
		t.Fatal("nil object stamp must be zero")
	}
}

// TestStampForList_OrderIndependent verifies that the list fingerprint is
// invariant under permutations of the input slice. This matters because
// the informer indexer's iteration order is not contractually stable.
func TestStampForList_OrderIndependent(t *testing.T) {
	a := mkObj("ns", "a", "10", 1)
	b := mkObj("ns", "b", "11", 1)
	cc := mkObj("ns", "c", "12", 1)

	stamp1 := stampForList([]*unstructured.Unstructured{a, b, cc})
	stamp2 := stampForList([]*unstructured.Unstructured{cc, a, b})
	if stamp1 != stamp2 {
		t.Fatalf("list stamp must be order-independent: %d vs %d", stamp1, stamp2)
	}
}

// TestStampForList_DetectsRVChange verifies that any item RV change produces
// a different fingerprint. A cache hit on the old fingerprint after an UPDATE
// would serve stale data; this test guards that path.
func TestStampForList_DetectsRVChange(t *testing.T) {
	a := mkObj("ns", "a", "10", 1)
	b := mkObj("ns", "b", "11", 1)

	stamp1 := stampForList([]*unstructured.Unstructured{a, b})
	bUpdated := mkObj("ns", "b", "999", 1) // RV changed
	stamp2 := stampForList([]*unstructured.Unstructured{a, bUpdated})
	if stamp1 == stamp2 {
		t.Fatal("RV change on any item must change the list stamp")
	}
}

// TestStampForList_DetectsAddRemove verifies that adding or removing an item
// changes the fingerprint.
func TestStampForList_DetectsAddRemove(t *testing.T) {
	a := mkObj("ns", "a", "10", 1)
	b := mkObj("ns", "b", "11", 1)

	stamp1 := stampForList([]*unstructured.Unstructured{a})
	stamp2 := stampForList([]*unstructured.Unstructured{a, b})
	if stamp1 == stamp2 {
		t.Fatal("adding an item must change the list stamp")
	}

	stamp3 := stampForList([]*unstructured.Unstructured{})
	if stamp1 == stamp3 {
		t.Fatal("empty list must have different stamp than non-empty")
	}
	if stamp3 == 0 {
		t.Fatal("empty-list stamp must be non-zero (distinguishable from unset)")
	}
}

// TestDecodedTreeCache_Int64RegressionGuard exercises the v0.25.283
// production-panic shape: an unstructured tree with int64 leaves that
// would crash gojq.normalizeNumbers. After Set+Get the tree must contain
// only float64 numerics so this branch is gojq-safe.
func TestDecodedTreeCache_Int64RegressionGuard(t *testing.T) {
	c := newDecodedTreeCache(4)
	key := mkKey("apps", "v1", "deployments", "default", "demo")

	// The canonical stored in the cache is already safeCopyJSON-normalized
	// at the call site (Resolve normalizes once, then both writes the L1
	// raw cache AND populates the decoded cache from the same float64 tree).
	src := mkObj("default", "demo", "10", 7).Object
	canonical := safeCopyJSON(src)
	c.Set(key, 1, canonical)

	got, hit := c.Get(key, 1)
	if !hit {
		t.Fatal("expected hit")
	}
	gotMeta := got.(map[string]any)["metadata"].(map[string]any)
	if _, ok := gotMeta["generation"].(float64); !ok {
		t.Fatalf("metadata.generation = %T, want float64 (gojq panics on int64)",
			gotMeta["generation"])
	}

	// Equally critical: the source informer object must NOT have been mutated
	// by either safeCopyJSON or the cache. K8s informer objects are shared
	// pointers — mutating them through this code path would corrupt other
	// readers.
	srcMeta := src["metadata"].(map[string]any)
	if _, ok := srcMeta["generation"].(int64); !ok {
		t.Fatalf("source metadata.generation = %T, want int64 unchanged",
			srcMeta["generation"])
	}
}

// TestDecodedTreeCache_NilSafe asserts that a nil receiver is safe to call.
// This lets the call site optionally disable the cache by passing nil.
func TestDecodedTreeCache_NilSafe(t *testing.T) {
	var c *decodedTreeCache
	key := mkKey("", "v1", "pods", "ns", "a")
	if _, hit := c.Get(key, 1); hit {
		t.Fatal("nil cache must always miss")
	}
	c.Set(key, 1, map[string]any{"x": float64(1)}) // must not panic
	if c.Len() != 0 {
		t.Fatal("nil cache Len must be 0")
	}
}

// TestDecodedTreeCache_OverwriteSameKey verifies that Setting an existing
// key updates the entry in place (refreshes both stamp and tree) without
// growing the cache.
func TestDecodedTreeCache_OverwriteSameKey(t *testing.T) {
	c := newDecodedTreeCache(8)
	key := mkKey("", "v1", "pods", "ns", "a")
	c.Set(key, 1, map[string]any{"x": float64(1)})
	c.Set(key, 2, map[string]any{"x": float64(2)})

	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (overwrite must not grow)", c.Len())
	}
	got, hit := c.Get(key, 2)
	if !hit {
		t.Fatal("expected hit on new stamp")
	}
	if !reflect.DeepEqual(got, map[string]any{"x": float64(2)}) {
		t.Fatalf("got %v, want {x:2}", got)
	}
}
