// Q-OOM-FIX (v0.25.313 RCA, 2026-05-08) — bounded LRU backing the
// RBACWatcher.EvaluateRBAC decision cache.
//
// Why hand-rolled vs hashicorp/golang-lru/v2: golang-lru is not in go.mod
// and adding it for ~50 LoC of trivial cache code triggers go.sum churn
// for a contract narrower than a single internal package's sole consumer.
// container/list + sync.Mutex is the smallest-surface-area choice.
//
// Concurrency: a single sync.Mutex guards the whole structure. Get is a
// write operation (it moves the entry to the MRU end) so an RWMutex
// would not buy us anything; the lock window is two map ops + one list
// splice, well under the cost of an RBAC lister scan.

package cache

import (
	"container/list"
	"strings"
	"sync"
)

// evalLRU is a bounded LRU cache mapping string keys to bool values.
// Eviction is strict-LRU: when len > cap, the least-recently-used entry
// is removed. RemoveWithPrefix supports the per-user invalidation path
// that mirrors purgeUserCacheData semantics.
type evalLRU struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List               // *evalLRUEntry, MRU at Front, LRU at Back
	index map[string]*list.Element // O(1) lookup
}

type evalLRUEntry struct {
	key   string
	value bool
}

// newEvalLRU constructs an empty cache bounded at cap entries. cap < 1
// is treated as "disabled" (every Get misses, Add is a no-op).
func newEvalLRU(cap int) *evalLRU {
	if cap < 1 {
		return &evalLRU{cap: 0}
	}
	return &evalLRU{
		cap:   cap,
		ll:    list.New(),
		index: make(map[string]*list.Element, cap/4),
	}
}

// Get returns the cached value for key and reports whether the key was
// present. On hit, the entry is promoted to the MRU end.
func (c *evalLRU) Get(key string) (bool, bool) {
	if c == nil || c.cap == 0 {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[key]
	if !ok {
		return false, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*evalLRUEntry).value, true
}

// Add inserts or updates an entry. If the cache is at capacity, the LRU
// entry is evicted. On update, the existing entry is promoted to MRU.
func (c *evalLRU) Add(key string, value bool) {
	if c == nil || c.cap == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok {
		el.Value.(*evalLRUEntry).value = value
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&evalLRUEntry{key: key, value: value})
	c.index[key] = el
	if c.ll.Len() > c.cap {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.index, oldest.Value.(*evalLRUEntry).key)
		}
	}
}

// Purge drops every entry. Mirrors hashicorp/golang-lru's Purge for API
// familiarity; called from RBACWatcher.invalidate() on broad RBAC change.
func (c *evalLRU) Purge() {
	if c == nil || c.cap == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	// Replace the map rather than range-deleting; faster on large caches
	// and lets the GC reclaim the old buckets in one shot.
	c.index = make(map[string]*list.Element, c.cap/4)
}

// RemoveWithPrefix deletes every entry whose key starts with prefix.
// Used by purgeUserCacheData() for per-user binding invalidation —
// keys are shaped "username|verb|group|resource|namespace" so the
// prefix "username|" cleanly scopes the eviction.
//
// O(n) over the live entries; this is acceptable because per-user
// invalidations are debounced by rbacDebounceWindow upstream and the
// cache is bounded at evalCacheCap.
func (c *evalLRU) RemoveWithPrefix(prefix string) {
	if c == nil || c.cap == 0 || prefix == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, el := range c.index {
		if strings.HasPrefix(k, prefix) {
			c.ll.Remove(el)
			delete(c.index, k)
		}
	}
}

// Len returns the current entry count. Used only by tests.
func (c *evalLRU) Len() int {
	if c == nil || c.cap == 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Contains reports whether key is currently resident WITHOUT promoting
// it to MRU. Used only by tests asserting eviction order.
func (c *evalLRU) Contains(key string) bool {
	if c == nil || c.cap == 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.index[key]
	return ok
}
