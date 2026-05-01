package api

import (
	"container/list"
	"hash/fnv"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// decodedTreeCache caches the safeCopyJSON-normalized tree produced from an
// informer hit, keyed by (GVR, ns, name|"") and stamped with the informer's
// per-object resourceVersion (or, for lists, an order-independent fingerprint
// over each item's name+rv pair).
//
// Why this exists:
//
// In the api.Resolve informer-hit branch, every successful informer fetch
// performed two walks of the same map[string]any tree:
//
//	raw, _ := json.Marshal(directData)        // walk #1: bytes for L1 cache
//	safeData := safeCopyJSON(directData)      // walk #2: mutable copy for gojq
//
// At 50K scale, the same (GVR, ns) list is re-resolved by every dashboard
// pageload until an UPDATE event invalidates the L1 raw-bytes cache, so both
// walks repeat against the same source data. v0.25.282 prof identified the
// marshal+unmarshal round-trip as the dominant allocator (~25% alloc_space
// at the hot path); v0.25.284 replaced the unmarshal half with safeCopyJSON
// but the marshal half plus the safeCopy walk still run on every miss.
//
// This cache short-circuits walk #2 entirely when the underlying informer
// objects have not changed since the last decode. It does NOT replace the
// raw-bytes L1 cache; the two are independent. When this cache misses, the
// existing marshal+safeCopy path runs unchanged.
//
// Concurrency: a single sync.Mutex guards the LRU. All operations are O(1)
// amortized. The mutex is released before safeCopyJSON runs, so concurrent
// reads do not serialise on the safe-copy walk.
type decodedTreeCache struct {
	mu       sync.Mutex
	capacity int
	entries  map[decodedCacheKey]*list.Element
	order    *list.List // front = most-recent, back = LRU
}

type decodedCacheKey struct {
	gvr  schema.GroupVersionResource
	ns   string
	name string // empty for list calls
}

type decodedCacheEntry struct {
	key   decodedCacheKey
	stamp uint64 // RV-derived stamp: hash(rv) for GET, fingerprint for LIST
	tree  any    // safeCopyJSON-normalized canonical tree (NOT mutated after Set)
}

// newDecodedTreeCache returns a cache with the given LRU capacity. capacity<=0
// disables the cache (Get always misses, Set is a no-op).
func newDecodedTreeCache(capacity int) *decodedTreeCache {
	return &decodedTreeCache{
		capacity: capacity,
		entries:  make(map[decodedCacheKey]*list.Element),
		order:    list.New(),
	}
}

// Get returns a fresh, mutation-safe copy of the cached tree if and only if
// the stored stamp matches the caller's stamp. The returned tree is produced
// via safeCopyJSON over the cached canonical tree, so the caller may mutate
// it freely (gojq.normalizeNumbers / deleteEmpty).
func (c *decodedTreeCache) Get(key decodedCacheKey, stamp uint64) (any, bool) {
	if c == nil || c.capacity <= 0 {
		return nil, false
	}
	c.mu.Lock()
	el, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		return nil, false
	}
	entry := el.Value.(*decodedCacheEntry)
	if entry.stamp != stamp {
		// Stale entry. Drop it now so the next caller hits the cold path
		// without paying for another miss-then-stale lookup.
		c.order.Remove(el)
		delete(c.entries, key)
		c.mu.Unlock()
		return nil, false
	}
	c.order.MoveToFront(el)
	canonical := entry.tree
	c.mu.Unlock()
	// safeCopyJSON outside the lock — concurrent reads of different keys
	// must not serialise on the walk.
	return safeCopyJSON(canonical), true
}

// Set stores the given canonical tree under key/stamp. The cache takes
// ownership of canonical (caller must not mutate it after Set). Evicts the
// LRU entry if over capacity.
func (c *decodedTreeCache) Set(key decodedCacheKey, stamp uint64, canonical any) {
	if c == nil || c.capacity <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		entry := el.Value.(*decodedCacheEntry)
		entry.stamp = stamp
		entry.tree = canonical
		c.order.MoveToFront(el)
		return
	}
	entry := &decodedCacheEntry{key: key, stamp: stamp, tree: canonical}
	el := c.order.PushFront(entry)
	c.entries[key] = el
	for c.order.Len() > c.capacity {
		victim := c.order.Back()
		if victim == nil {
			break
		}
		c.order.Remove(victim)
		delete(c.entries, victim.Value.(*decodedCacheEntry).key)
	}
}

// Len reports the current entry count. Used by tests.
func (c *decodedTreeCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// stampForObject returns the cache stamp for a single informer object.
// Uses a fnv64 hash of resourceVersion so the stamp fits in a uint64 and
// matches the type used by stampForList.
func stampForObject(obj *unstructured.Unstructured) uint64 {
	if obj == nil {
		return 0
	}
	rv := obj.GetResourceVersion()
	h := fnv.New64a()
	_, _ = h.Write([]byte(rv))
	return h.Sum64()
}

// stampForList returns an order-independent fingerprint over the list items.
// Each item contributes fnv64(name + "\x00" + rv); contributions are XORed
// together so the result is invariant under reorderings of the slice.
//
// Order-independence is required because the informer indexer order is not
// guaranteed stable across cache rebuilds; XOR over a 64-bit hash gives us
// a collision rate <10^-11 at our cardinality (50K items per list).
//
// The empty-list case returns a non-zero sentinel so an empty list is
// distinguishable from "key not yet seen".
func stampForList(items []*unstructured.Unstructured) uint64 {
	const emptySentinel uint64 = 0x9E3779B97F4A7C15 // golden ratio bits
	if len(items) == 0 {
		return emptySentinel
	}
	var acc uint64
	h := fnv.New64a()
	for _, it := range items {
		if it == nil {
			continue
		}
		h.Reset()
		_, _ = h.Write([]byte(it.GetName()))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(it.GetResourceVersion()))
		acc ^= h.Sum64()
	}
	if acc == 0 {
		// All items hashed to the same value (degenerate). Shift to avoid
		// colliding with the unset/zero stamp.
		acc = emptySentinel
	}
	return acc
}
