package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// compile-time check: *MemCache satisfies Cache.
var _ Cache = (*MemCache)(nil)

// ── L1 budget knobs (Q-L1-BUDGET, 0.25.319) ──────────────────────────────────
//
// Per architect 2026-05-08: the L1 kv map had no byte cap, no entry cap, and
// no LRU. At 49K bench × 1004 users it sustained 2.32 GiB; only TTL × write-
// rate bounded growth. These knobs put a hard ceiling on the L1 footprint and
// add LRU sweep so the eviction order is predictable. The L2 sweeper at
// l2_refilter.go:549-607 is the structural template.
const (
	envL1MaxBytes   = "CACHE_L1_MAX_BYTES"
	envL1MaxEntries = "CACHE_L1_MAX_ENTRIES"

	defaultL1MaxBytes   int64 = 2 << 30 // 2 GiB
	defaultL1MaxEntries int   = 200_000
)

// l1MaxBytes reads CACHE_L1_MAX_BYTES on every call (cheap; the budget
// check fires once per StartEviction tick + once per write that overflows).
// Returns the default when unset, empty, or unparseable.
func l1MaxBytes() int64 {
	v := strings.TrimSpace(os.Getenv(envL1MaxBytes))
	if v == "" {
		return defaultL1MaxBytes
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return defaultL1MaxBytes
	}
	return n
}

// l1MaxEntries reads CACHE_L1_MAX_ENTRIES on every call.
func l1MaxEntries() int {
	v := strings.TrimSpace(os.Getenv(envL1MaxEntries))
	if v == "" {
		return defaultL1MaxEntries
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultL1MaxEntries
	}
	return n
}

// ── Entry types ──────────────────────────────────────────────────────────────

// memEntry stores a single key-value pair with an optional expiry.
//
// LRU tracking (Q-L1-BUDGET, 0.25.319): lastAccess is updated on every
// successful GetRaw via an atomic.Int64 store. The byte-budget sweeper sorts
// entries by lastAccess ascending and evicts the oldest until under both the
// byte cap and entry cap. Updates are racy-but-monotonic (occasional clobber
// of a slightly older nano timestamp by a slightly newer one is fine for LRU).
type memEntry struct {
	data       []byte
	expiresAt  int64        // unix nano; 0 = never expires
	lastAccess atomic.Int64 // unix nano; updated on every GetRaw hit
}

// memSetEntry stores a set of string members with an optional expiry.
type memSetEntry struct {
	members   map[string]bool
	expiresAt int64
	mu        sync.RWMutex
}

// ── MemCache ─────────────────────────────────────────────────────────────────

// MemCache is a pure in-process implementation of the Cache interface using
// sync.Map for storage. Zero external dependencies — no Redis, no disk.
// Suitable for single-replica deployments or integration tests.
type MemCache struct {
	// Key-value store for all cached data (L1 resolved, object cache,
	// API results, user config, strings, negative cache sentinels).
	kv sync.Map // string -> *memEntry

	// Set store for Redis SET-like operations (dep tracking, GVR set,
	// user set, resolved index, list index).
	sets sync.Map // string -> *memSetEntry

	// RBAC decision cache. Key format: "user\x00field" where field is
	// the same format as RBACField (verb:group/resource:namespace).
	rbac sync.Map // string -> *memEntry (data = "true" or "false")

	// GVR TTLs and notifier.
	gvrTTLs     sync.Map
	onNewGVR    atomic.Value // stores gvrNotifyFunc
	resourceTTL time.Duration

	// Q-L1-BUDGET (0.25.319) — atomic counters for budget accounting +
	// /metrics/runtime exposure. residentBytes is the sum of len(data) for
	// every live entry in c.kv (NOT sets/rbac — those are bounded by other
	// means). entryCount mirrors len(c.kv) without Range.
	residentBytes atomic.Int64
	entryCount    atomic.Int64

	// Synchronous L1 admission (0.25.327) — try-lock so only one writer at
	// a time runs the inline LRU sweep. Concurrent writers that find the
	// flag set skip the sweep and let the in-progress one (or the next
	// writer / 30-s ticker) catch up. Avoids unbounded latency when many
	// writers simultaneously trip the budget.
	lruSweeping atomic.Bool
}

// NewMem creates a new in-process cache with the given default resource TTL.
func NewMem(resourceTTL time.Duration) *MemCache {
	return &MemCache{resourceTTL: resourceTTL}
}

// ── Per-GVR TTL (called via type assertion) ──────────────────────────────────

func (c *MemCache) RegisterGVRTTL(gvr schema.GroupVersionResource, ttl time.Duration) {
	if c == nil || ttl == 0 {
		return
	}
	c.gvrTTLs.Store(GVRToKey(gvr), ttl)
}

func (c *MemCache) TTLForGVR(gvr schema.GroupVersionResource) time.Duration {
	if c == nil {
		return 0
	}
	if v, ok := c.gvrTTLs.Load(GVRToKey(gvr)); ok {
		return v.(time.Duration)
	}
	return c.resourceTTL
}

// SetGVRNotifier registers a callback invoked when a GVR is seen for the
// first time via SAddGVR.
func (c *MemCache) SetGVRNotifier(fn func(context.Context, schema.GroupVersionResource)) {
	if c != nil {
		c.onNewGVR.Store(fn)
	}
}

// ── Background eviction ──────────────────────────────────────────────────────

// StartEviction launches a background goroutine that removes expired entries
// every 30 seconds. The goroutine exits when ctx is cancelled.
//
// Q-L1-BUDGET (0.25.319): in addition to the TTL pass, after the TTL sweep
// completes we check residentBytes / entryCount against the configured caps
// and run an LRU sweep when over. Mirrors the L2 sweeper at
// l2_refilter.go:557-607.
func (c *MemCache) StartEviction(ctx context.Context) {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				now := time.Now().UnixNano()
				c.kv.Range(func(k, v any) bool {
					if e := v.(*memEntry); e.expiresAt > 0 && e.expiresAt < now {
						if c.kvDelete(k.(string)) != nil {
							GlobalMetrics.L1EvictionsTTL.Add(1)
						}
					}
					return true
				})
				c.sets.Range(func(k, v any) bool {
					se := v.(*memSetEntry)
					if se.expiresAt > 0 && se.expiresAt < now {
						c.sets.Delete(k)
					}
					return true
				})
				c.rbac.Range(func(k, v any) bool {
					if e := v.(*memEntry); e.expiresAt > 0 && e.expiresAt < now {
						c.rbac.Delete(k)
					}
					return true
				})

				// LRU budget sweep — runs after TTL pass so we evict only
				// what's still alive (TTL-expired entries are already gone).
				// 0.25.327: count this path as the async (ticker-driven) sweep
				// to distinguish it from the synchronous-admission path in
				// kvStore.
				GlobalMetrics.L1AsyncSweepCount.Add(1)
				c.sweepLRUIfOverBudget()
			}
		}
	}()
}

// sweepLRUIfOverBudget evicts least-recently-accessed kv entries until both
// residentBytes <= 0.9 × cap and entryCount <= 0.9 × cap. The 10% slack
// matches L2's strategy and avoids thrash on bursty writers.
//
// Concurrent writers may push residentBytes up while we sort; that's fine —
// the next tick (or the next sync-admission writer) will catch up. Race-free
// because every counter delta is applied via atomic ops in kvStore/kvDelete.
//
// Unbounded — runs to completion. Used by the 30-s ticker (StartEviction)
// and by tests that want deterministic post-state. Synchronous-admission
// callers in kvStore use sweepLRUIfOverBudgetUntil with a deadline.
func (c *MemCache) sweepLRUIfOverBudget() {
	c.sweepLRUIfOverBudgetUntil(time.Time{})
}

// sweepLRUIfOverBudgetUntil is the deadline-aware sweeper. A zero deadline
// disables the budget (used by the ticker). A non-zero deadline aborts the
// eviction loop early once exceeded so writer-path latency stays bounded;
// any remaining over-budget entries roll forward to the next sync-admission
// writer or the 30-s ticker.
func (c *MemCache) sweepLRUIfOverBudgetUntil(deadline time.Time) {
	maxBytes := l1MaxBytes()
	maxEntries := l1MaxEntries()
	curBytes := c.residentBytes.Load()
	curCount := c.entryCount.Load()

	if curBytes <= maxBytes && curCount <= int64(maxEntries) {
		return
	}

	type item struct {
		key string
		ts  int64
		sz  int
	}
	items := make([]item, 0, curCount)
	c.kv.Range(func(k, v any) bool {
		ks, _ := k.(string)
		e, ok := v.(*memEntry)
		if !ok || e == nil {
			return true
		}
		items = append(items, item{key: ks, ts: e.lastAccess.Load(), sz: len(e.data)})
		return true
	})
	sort.Slice(items, func(i, j int) bool { return items[i].ts < items[j].ts })

	targetBytes := int64(float64(maxBytes) * 0.9)
	targetCount := int64(float64(maxEntries) * 0.9)

	evicted := int64(0)
	for i, it := range items {
		if c.residentBytes.Load() <= targetBytes && c.entryCount.Load() <= targetCount {
			break
		}
		// Bound the writer-path sweep: check the deadline every 64 entries
		// (cheap; time.Now is ~10ns). Any remaining work rolls forward.
		if !deadline.IsZero() && i&63 == 0 && time.Now().After(deadline) {
			break
		}
		if c.kvDelete(it.key) != nil {
			evicted++
		}
	}
	if evicted > 0 {
		GlobalMetrics.L1EvictionsLRU.Add(evicted)
	}
}

// maybeSweepInline is the synchronous-admission entry point. Called from
// kvStore after every write that actually changes counters. Fast path: if
// the budget is not exceeded, return immediately (no allocation, no Range).
//
// Slow path: try-lock c.lruSweeping so only one writer at a time runs the
// sweep — concurrent writers see the flag set and skip (the in-progress
// sweep will catch their excess on the same pass, or the next caller/ticker
// will). The sweep itself is bounded by a 10 ms wallclock deadline so
// writer-path latency is capped even when the kv map has 200K entries.
func (c *MemCache) maybeSweepInline() {
	if c.residentBytes.Load() <= l1MaxBytes() && c.entryCount.Load() <= int64(l1MaxEntries()) {
		return
	}
	if !c.lruSweeping.CompareAndSwap(false, true) {
		return
	}
	defer c.lruSweeping.Store(false)
	GlobalMetrics.L1SyncSweepCount.Add(1)
	c.sweepLRUIfOverBudgetUntil(time.Now().Add(10 * time.Millisecond))
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// expiresAt computes the absolute expiry timestamp from a TTL.
// A zero TTL means no expiry.
func expiresAt(ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	return time.Now().Add(ttl).UnixNano()
}

// isExpired returns true if the entry has a non-zero expiry that is in the past.
func isExpired(expiresAt int64) bool {
	return expiresAt > 0 && expiresAt < time.Now().UnixNano()
}

// cloneBytes returns a copy of b so callers cannot mutate cache internals.
func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}

// ── Q-L1-BUDGET (0.25.319) — counter-aware kv mutation helpers ─────────────
//
// Every code path that adds, replaces, or removes a *memEntry from c.kv MUST
// route through one of these helpers so c.residentBytes and c.entryCount stay
// accurate. The byte delta is computed from the *new* entry's data length
// minus the previous entry's data length (if any). LRU access is stamped on
// store so freshly-written entries do not get evicted on the very next sweep.

// kvStore inserts or replaces an entry, updating residentBytes/entryCount and
// stamping lastAccess. Returns nothing — the caller does not need to know
// whether a replace happened.
//
// 0.25.327: after the counters are updated, maybeSweepInline runs the LRU
// sweep synchronously when residentBytes > maxBytes or entryCount >
// maxEntries. The sweep is try-locked + deadline-bounded so concurrent
// writers don't pile up and a single writer's worst-case latency stays
// inside the 10 ms budget. Pre-0.25.327 the only sweep was the 30-s ticker,
// which left a window where resident_bytes could balloon past the cap.
func (c *MemCache) kvStore(key string, e *memEntry) {
	now := time.Now().UnixNano()
	e.lastAccess.Store(now)
	prev, loaded := c.kv.Swap(key, e)
	if loaded {
		if pe, ok := prev.(*memEntry); ok && pe != nil {
			c.residentBytes.Add(int64(len(e.data)) - int64(len(pe.data)))
			c.maybeSweepInline()
			return
		}
	}
	c.residentBytes.Add(int64(len(e.data)))
	c.entryCount.Add(1)
	c.maybeSweepInline()
}

// kvDelete removes an entry, decrementing the counters. Returns the deleted
// entry (or nil) so callers (TTL pass, LRU sweep) can also subtract internal
// state if they cached the size.
func (c *MemCache) kvDelete(key string) *memEntry {
	prev, loaded := c.kv.LoadAndDelete(key)
	if !loaded {
		return nil
	}
	pe, ok := prev.(*memEntry)
	if !ok || pe == nil {
		return nil
	}
	c.residentBytes.Add(-int64(len(pe.data)))
	c.entryCount.Add(-1)
	return pe
}

// L1ResidentBytes returns the current resident byte count of c.kv. Exposed
// for /metrics/runtime.
func (c *MemCache) L1ResidentBytes() int64 {
	if c == nil {
		return 0
	}
	return c.residentBytes.Load()
}

// L1EntryCount returns the current entry count in c.kv.
func (c *MemCache) L1EntryCount() int64 {
	if c == nil {
		return 0
	}
	return c.entryCount.Load()
}

// L1MaxBytes / L1MaxEntries surface the configured caps so /metrics/runtime
// can publish utilisation percentages without re-reading env. Values are
// re-read on every call (env may change at runtime in dev/test).
func L1MaxBytes() int64 { return l1MaxBytes() }

// L1MaxEntries returns the configured entry cap.
func L1MaxEntries() int { return l1MaxEntries() }

// SetCount / SetMemberTotal expose c.sets cardinality for /metrics/runtime
// (Q-DIAG-PPROF, 0.25.321) — pair distinguishes "few large sets" from
// "many small sets" in the heap-shift RCA. Both nil-safe.
func (c *MemCache) SetCount() int64 {
	if c == nil {
		return 0
	}
	var n int64
	c.sets.Range(func(_, _ any) bool { n++; return true })
	return n
}

func (c *MemCache) SetMemberTotal() int64 {
	if c == nil {
		return 0
	}
	var n int64
	c.sets.Range(func(_, v any) bool {
		if se, ok := v.(*memSetEntry); ok && se != nil {
			se.mu.RLock()
			n += int64(len(se.members))
			se.mu.RUnlock()
		}
		return true
	})
	return n
}

// touchAccess updates the lastAccess timestamp on a hit. No-op if e is nil.
func touchAccess(e *memEntry) {
	if e != nil {
		e.lastAccess.Store(time.Now().UnixNano())
	}
}

// ── Core read/write ──────────────────────────────────────────────────────────

func (c *MemCache) GetRaw(_ context.Context, key string) ([]byte, bool, error) {
	if c == nil {
		return nil, false, nil
	}
	v, ok := c.kv.Load(key)
	if !ok {
		return nil, false, nil
	}
	e := v.(*memEntry)
	if isExpired(e.expiresAt) {
		if c.kvDelete(key) != nil {
			GlobalMetrics.L1EvictionsTTL.Add(1)
		}
		return nil, false, nil
	}
	if bytes.Equal(e.data, []byte(notFoundSentinel)) {
		// Touch sentinel too — keeps LRU consistent for negative cache.
		touchAccess(e)
		return nil, false, nil
	}
	touchAccess(e)
	return cloneBytes(e.data), true, nil
}

func (c *MemCache) Get(ctx context.Context, key string, dest any) (bool, error) {
	if c == nil {
		return false, nil
	}
	raw, ok, err := c.GetRaw(ctx, key)
	if !ok || err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, dest)
}

func (c *MemCache) GetRawMulti(ctx context.Context, keys []string) map[string][]byte {
	if c == nil || len(keys) == 0 {
		return nil
	}
	result := make(map[string][]byte, len(keys))
	for _, k := range keys {
		raw, ok, _ := c.GetRaw(ctx, k)
		if ok {
			result[k] = raw
		}
	}
	return result
}

func (c *MemCache) Exists(_ context.Context, key string) bool {
	if c == nil {
		return false
	}
	v, ok := c.kv.Load(key)
	if !ok {
		return false
	}
	e := v.(*memEntry)
	if isExpired(e.expiresAt) {
		if c.kvDelete(key) != nil {
			GlobalMetrics.L1EvictionsTTL.Add(1)
		}
		return false
	}
	return true
}

func (c *MemCache) Set(_ context.Context, key string, val any) error {
	if c == nil {
		return nil
	}
	data, err := json.Marshal(val)
	if err != nil {
		return err
	}
	c.kvStore(key, &memEntry{data: data, expiresAt: expiresAt(c.resourceTTL)})
	return nil
}

func (c *MemCache) SetWithTTL(_ context.Context, key string, val any, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	data, err := json.Marshal(val)
	if err != nil {
		return err
	}
	c.kvStore(key, &memEntry{data: data, expiresAt: expiresAt(ttl)})
	return nil
}

func (c *MemCache) SetRaw(_ context.Context, key string, val []byte) error {
	if c == nil {
		return nil
	}
	c.kvStore(key, &memEntry{data: cloneBytes(val), expiresAt: expiresAt(c.resourceTTL)})
	return nil
}

func (c *MemCache) SetForGVR(_ context.Context, gvr schema.GroupVersionResource, key string, val any) error {
	if c == nil {
		return nil
	}
	data, err := json.Marshal(val)
	if err != nil {
		return err
	}
	c.kvStore(key, &memEntry{data: data, expiresAt: expiresAt(c.TTLForGVR(gvr))})
	return nil
}

func (c *MemCache) SetMultiForGVR(_ context.Context, gvr schema.GroupVersionResource, entries map[string]any) error {
	if c == nil || len(entries) == 0 {
		return nil
	}
	ttl := c.TTLForGVR(gvr)
	exp := expiresAt(ttl)
	for key, val := range entries {
		data, err := json.Marshal(val)
		if err != nil {
			return err
		}
		c.kvStore(key, &memEntry{data: data, expiresAt: exp})
	}
	return nil
}

func (c *MemCache) SetRawForGVR(_ context.Context, gvr schema.GroupVersionResource, key string, val []byte) error {
	if c == nil {
		return nil
	}
	c.kvStore(key, &memEntry{data: cloneBytes(val), expiresAt: expiresAt(c.TTLForGVR(gvr))})
	return nil
}

// SetResolvedRaw stores a fully-resolved widget/restaction output with
// ResolvedCacheTTL. Also tracks the key in a per-user resolved index set
// for O(1) invalidation.
func (c *MemCache) SetResolvedRaw(_ context.Context, key string, val []byte) error {
	if c == nil {
		return nil
	}
	c.kvStore(key, &memEntry{data: cloneBytes(val), expiresAt: expiresAt(ResolvedCacheTTL)})

	// Maintain per-user resolved index.
	info, hasInfo := ParseResolvedKey(key)
	if hasInfo && info.Username != "" {
		idxKey := UserResolvedIndexKey(info.Username)
		c.saddInternal(idxKey, key, ReverseIndexTTL)
	}
	return nil
}

// SetAPIResultRaw stores an API result with APIResultCacheTTL (60s).
func (c *MemCache) SetAPIResultRaw(_ context.Context, key string, val []byte) error {
	if c == nil {
		return nil
	}
	c.kvStore(key, &memEntry{data: cloneBytes(val), expiresAt: expiresAt(APIResultCacheTTL)})
	return nil
}

func (c *MemCache) Delete(_ context.Context, keys ...string) error {
	if c == nil || len(keys) == 0 {
		return nil
	}
	for _, k := range keys {
		c.kvDelete(k)
	}
	return nil
}

// ── Negative-cache ───────────────────────────────────────────────────────────

func (c *MemCache) GetNotFound(_ context.Context, key string) bool {
	if c == nil {
		return false
	}
	v, ok := c.kv.Load(key)
	if !ok {
		return false
	}
	e := v.(*memEntry)
	if isExpired(e.expiresAt) {
		if c.kvDelete(key) != nil {
			GlobalMetrics.L1EvictionsTTL.Add(1)
		}
		return false
	}
	return bytes.Equal(e.data, []byte(notFoundSentinel))
}

func (c *MemCache) SetNotFound(_ context.Context, key string) error {
	if c == nil {
		return nil
	}
	c.kvStore(key, &memEntry{
		data:      []byte(notFoundSentinel),
		expiresAt: expiresAt(notFoundTTL),
	})
	return nil
}

// ── Atomic ops ───────────────────────────────────────────────────────────────

// AtomicUpdateJSON performs a read-modify-write. Since this is in-process,
// we use a simple load/store approach. There are no current callers that
// race on the same key, so the window is acceptable.
func (c *MemCache) AtomicUpdateJSON(_ context.Context, key string, fn func([]byte) ([]byte, error), ttl time.Duration) error {
	if c == nil {
		return nil
	}
	var existing []byte
	if v, ok := c.kv.Load(key); ok {
		e := v.(*memEntry)
		if !isExpired(e.expiresAt) && !bytes.Equal(e.data, []byte(notFoundSentinel)) {
			existing = cloneBytes(e.data)
		}
	}
	newVal, err := fn(existing)
	if err != nil || newVal == nil {
		return err
	}
	c.kvStore(key, &memEntry{data: newVal, expiresAt: expiresAt(ttl)})
	return nil
}

// ── Key scanning ─────────────────────────────────────────────────────────────

// ScanKeys returns all non-expired keys matching the given glob pattern.
// Supports * and ? wildcards via path.Match (compatible with Redis SCAN
// glob semantics for the patterns used in this codebase).
//
// CAVEAT: path.Match treats `/` as a directory separator and refuses to
// match `*` across one. Many snowplow cache keys embed `<group>/<version>/
// <resource>` literally (e.g. snowplow:resolved:{user}:templates.krateo.io/
// v1/restactions:...). Glob patterns like `snowplow:resolved:*` therefore
// match ZERO restaction L1 entries — callers iterating per-prefix should
// use DeleteByPrefix or DBSizeByPrefix below, which do prefix matching
// directly without path.Match.
func (c *MemCache) ScanKeys(_ context.Context, pattern string) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	var keys []string
	c.kv.Range(func(k, v any) bool {
		key := k.(string)
		e := v.(*memEntry)
		if isExpired(e.expiresAt) {
			if c.kvDelete(key) != nil {
				GlobalMetrics.L1EvictionsTTL.Add(1)
			}
			return true
		}
		if matched, _ := path.Match(pattern, key); matched {
			keys = append(keys, key)
		}
		return true
	})
	return keys, nil
}

// DeleteByPrefix removes every (non-expired) key whose name starts with
// the given prefix. Returns the count of deletions. Concrete method on
// *MemCache only — callers reach it via the type assertion in
// cache.FlushResolvedPrefix because the Cache interface intentionally
// stays minimal. Used by the v3 startup hook to evict stale L1 outer
// entries written under an incompatible cache schema.
func (c *MemCache) DeleteByPrefix(_ context.Context, prefix string) (int, error) {
	if c == nil || prefix == "" {
		return 0, nil
	}
	// Two-phase: collect first to avoid mutating during Range.
	var victims []string
	c.kv.Range(func(k, v any) bool {
		key := k.(string)
		e := v.(*memEntry)
		if isExpired(e.expiresAt) {
			if c.kvDelete(key) != nil {
				GlobalMetrics.L1EvictionsTTL.Add(1)
			}
			return true
		}
		if strings.HasPrefix(key, prefix) {
			victims = append(victims, key)
		}
		return true
	})
	for _, k := range victims {
		c.kvDelete(k)
	}
	return len(victims), nil
}

// ── Set operations ───────────────────────────────────────────────────────────

// saddInternal adds a member to a set with TTL. Shared by public methods.
// Returns 1 if the member was newly added, 0 if it already existed (dedup).
func (c *MemCache) saddInternal(key, member string, ttl time.Duration) int {
	exp := expiresAt(ttl)
	for {
		v, loaded := c.sets.LoadOrStore(key, &memSetEntry{
			members:   map[string]bool{member: true},
			expiresAt: exp,
		})
		if !loaded {
			return 1 // freshly created — member is new
		}
		se := v.(*memSetEntry)
		se.mu.Lock()
		if isExpired(se.expiresAt) {
			// Expired entry — replace it.
			se.mu.Unlock()
			c.sets.Delete(key)
			continue // retry LoadOrStore
		}
		added := 0
		if !se.members[member] {
			added = 1
		}
		se.members[member] = true
		se.expiresAt = exp
		se.mu.Unlock()
		return added
	}
}

func (c *MemCache) SAddWithTTL(_ context.Context, key, member string, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	c.saddInternal(key, member, ttl)
	return nil
}

// SAddWithTTLN behaves like SAddWithTTL but returns the number of members
// actually added (0 means the member was already present). Used for dedup
// detection in instrumentation; zero added cost — saddInternal already
// computes the count.
func (c *MemCache) SAddWithTTLN(_ context.Context, key, member string, ttl time.Duration) (int, error) {
	if c == nil {
		return 0, nil
	}
	return c.saddInternal(key, member, ttl), nil
}

// SCard returns the cardinality of the set at key. Returns 0 if the set is
// absent or expired. O(1) — used for sampled cluster-wide dep size gauges.
func (c *MemCache) SCard(_ context.Context, key string) (int64, error) {
	if c == nil {
		return 0, nil
	}
	v, ok := c.sets.Load(key)
	if !ok {
		return 0, nil
	}
	se := v.(*memSetEntry)
	se.mu.Lock()
	defer se.mu.Unlock()
	if isExpired(se.expiresAt) {
		return 0, nil
	}
	return int64(len(se.members)), nil
}

func (c *MemCache) SAddMultiWithTTL(_ context.Context, key string, members []string, ttl time.Duration) error {
	if c == nil || len(members) == 0 {
		return nil
	}
	exp := expiresAt(ttl)
	initial := make(map[string]bool, len(members))
	for _, m := range members {
		initial[m] = true
	}
	for {
		v, loaded := c.sets.LoadOrStore(key, &memSetEntry{
			members:   initial,
			expiresAt: exp,
		})
		if !loaded {
			return nil
		}
		se := v.(*memSetEntry)
		se.mu.Lock()
		if isExpired(se.expiresAt) {
			se.mu.Unlock()
			c.sets.Delete(key)
			continue
		}
		for _, m := range members {
			se.members[m] = true
		}
		se.expiresAt = exp
		se.mu.Unlock()
		return nil
	}
}

func (c *MemCache) SRemMembers(_ context.Context, key string, members ...string) error {
	if c == nil || len(members) == 0 {
		return nil
	}
	v, ok := c.sets.Load(key)
	if !ok {
		return nil
	}
	se := v.(*memSetEntry)
	se.mu.Lock()
	defer se.mu.Unlock()
	for _, m := range members {
		delete(se.members, m)
	}
	return nil
}

func (c *MemCache) ReplaceSetWithTTL(_ context.Context, key string, members []string, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	if len(members) == 0 {
		c.sets.Delete(key)
		return nil
	}
	newMembers := make(map[string]bool, len(members))
	for _, m := range members {
		newMembers[m] = true
	}
	c.sets.Store(key, &memSetEntry{
		members:   newMembers,
		expiresAt: expiresAt(ttl),
	})
	return nil
}

func (c *MemCache) SMembers(_ context.Context, key string) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	v, ok := c.sets.Load(key)
	if !ok {
		return nil, nil
	}
	se := v.(*memSetEntry)
	se.mu.RLock()
	defer se.mu.RUnlock()
	if isExpired(se.expiresAt) {
		c.sets.Delete(key)
		return nil, nil
	}
	result := make([]string, 0, len(se.members))
	for m := range se.members {
		result = append(result, m)
	}
	return result, nil
}

// ── List assembly ────────────────────────────────────────────────────────────

// AssembleListFromIndex returns the assembled raw JSON of an UnstructuredList
// for the given (gvr, namespace).
//
// Q-MIRROR-REMOVAL (0.25.316): the implementation now reads from the
// informer's in-memory store via cache.InformerReaderFromContext(ctx) instead
// of MGET-ing snowplow:get:* mirror keys. The informer already holds every
// watched object byte-for-byte (transform-stripped on entry); the mirror was
// pure redundancy and accounted for ~567K cache_keys at bench scale.
//
// Returns (nil, false, nil) if no InformerReader is in ctx (e.g. unit tests
// constructing a bare MemCache without a ResourceWatcher) or the GVR has no
// registered informer yet. Existing list-index SETs in MemCache (populated
// at warmup) are intentionally ignored — they are append-only and would
// drift forever otherwise.
func (c *MemCache) AssembleListFromIndex(ctx context.Context, gvr schema.GroupVersionResource, namespace string) ([]byte, bool, error) {
	if c == nil {
		return nil, false, nil
	}

	ir := InformerReaderFromContext(ctx)
	if ir == nil {
		// No informer wired — fall back to the legacy mirror-based path
		// for backward compatibility with tests that seed snowplow:get:*
		// keys directly. Production always installs WithInformerReader in
		// the request middleware (see main.go withCache) and in the
		// background L1 refresh ctx.
		return c.assembleListFromMirror(ctx, gvr, namespace)
	}

	objs, ok := ir.ListObjects(gvr, namespace)
	if !ok {
		return nil, false, nil
	}

	// Assemble into an UnstructuredList JSON. Identical wire format to the
	// legacy mirror path so callers see no shape change.
	var buf bytes.Buffer
	buf.WriteString(`{"apiVersion":"`)
	if gvr.Group == "" {
		buf.WriteString(gvr.Version)
	} else {
		buf.WriteString(gvr.Group)
		buf.WriteByte('/')
		buf.WriteString(gvr.Version)
	}
	buf.WriteString(`","kind":"List","metadata":{"resourceVersion":""},"items":[`)

	first := true
	for _, uns := range objs {
		if uns == nil {
			continue
		}
		raw, err := json.Marshal(uns.Object)
		if err != nil {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		buf.Write(raw)
		first = false
	}
	buf.WriteString(`]}`)

	return buf.Bytes(), true, nil
}

// assembleListFromMirror is the pre-Q-MIRROR-REMOVAL fallback path. Kept
// only to support unit tests that seed snowplow:get:* keys directly without
// constructing a full ResourceWatcher. Production never reaches this branch
// because the request middleware always installs an InformerReader.
func (c *MemCache) assembleListFromMirror(ctx context.Context, gvr schema.GroupVersionResource, namespace string) ([]byte, bool, error) {
	idxKey := ListIndexKey(gvr, namespace)
	members, err := c.SMembers(ctx, idxKey)
	if err != nil || len(members) == 0 {
		return nil, false, err
	}

	getKeys := make([]string, len(members))
	for i, member := range members {
		if namespace == "" {
			parts := strings.SplitN(member, "/", 2)
			if len(parts) == 2 {
				getKeys[i] = GetKey(gvr, parts[0], parts[1])
			} else {
				getKeys[i] = GetKey(gvr, "", member)
			}
		} else {
			getKeys[i] = GetKey(gvr, namespace, member)
		}
	}

	results := c.GetRawMulti(ctx, getKeys)
	if len(results) == 0 {
		return nil, false, nil
	}

	var buf bytes.Buffer
	buf.WriteString(`{"apiVersion":"`)
	if gvr.Group == "" {
		buf.WriteString(gvr.Version)
	} else {
		buf.WriteString(gvr.Group)
		buf.WriteByte('/')
		buf.WriteString(gvr.Version)
	}
	buf.WriteString(`","kind":"List","metadata":{"resourceVersion":""},"items":[`)

	first := true
	for _, k := range getKeys {
		raw, ok := results[k]
		if !ok || IsNotFoundRaw(raw) {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		buf.Write(raw)
		first = false
	}
	buf.WriteString(`]}`)

	return buf.Bytes(), true, nil
}

// ── RBAC ─────────────────────────────────────────────────────────────────────

// rbacKey builds the internal RBAC map key: "user\x00field".
func rbacKey(username, field string) string {
	return username + "\x00" + field
}

func (c *MemCache) IsRBACAllowed(_ context.Context, username, verb string, gr schema.GroupResource, namespace string) (allowed, cached bool) {
	if c == nil {
		return false, false
	}
	field := RBACField(verb, gr, namespace)
	v, ok := c.rbac.Load(rbacKey(username, field))
	if !ok {
		return false, false
	}
	e := v.(*memEntry)
	if isExpired(e.expiresAt) {
		c.rbac.Delete(rbacKey(username, field))
		return false, false
	}
	return string(e.data) == "true", true
}

func (c *MemCache) SetRBACResult(_ context.Context, username, verb string, gr schema.GroupResource, namespace string, allowed bool, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	field := RBACField(verb, gr, namespace)
	val := "false"
	if allowed {
		val = "true"
	}
	c.rbac.Store(rbacKey(username, field), &memEntry{
		data:      []byte(val),
		expiresAt: expiresAt(ttl),
	})
	return nil
}

func (c *MemCache) DeleteUserRBAC(_ context.Context, username string) error {
	if c == nil {
		return nil
	}
	prefix := username + "\x00"
	c.rbac.Range(func(k, _ any) bool {
		if strings.HasPrefix(k.(string), prefix) {
			c.rbac.Delete(k)
		}
		return true
	})
	return nil
}

// ── GVR tracking ─────────────────────────────────────────────────────────────

func (c *MemCache) SAddGVR(ctx context.Context, gvr schema.GroupVersionResource) error {
	if c == nil {
		return nil
	}
	member := GVRToKey(gvr)
	v, ok := c.sets.Load(WatchedGVRsKey)
	isNew := false
	if !ok {
		// First GVR — create the set.
		c.sets.Store(WatchedGVRsKey, &memSetEntry{
			members: map[string]bool{member: true},
		})
		isNew = true
	} else {
		se := v.(*memSetEntry)
		se.mu.Lock()
		if !se.members[member] {
			se.members[member] = true
			isNew = true
		}
		se.mu.Unlock()
	}
	if isNew {
		if fn, ok := c.onNewGVR.Load().(gvrNotifyFunc); ok && fn != nil {
			fn(ctx, gvr)
		}
	}
	return nil
}

// ── User tracking ────────────────────────────────────────────────────────────

func (c *MemCache) SAddUser(ctx context.Context, username string) error {
	if c == nil {
		return nil
	}
	// No TTL for active-users set — matches Redis behavior (no EXPIRE on SADD).
	return c.SAddWithTTL(ctx, ActiveUsersKey, username, 0)
}

func (c *MemCache) SRemUser(ctx context.Context, username string) error {
	if c == nil {
		return nil
	}
	return c.SRemMembers(ctx, ActiveUsersKey, username)
}

// ── String helpers ───────────────────────────────────────────────────────────

func (c *MemCache) SetStringWithTTL(_ context.Context, key, value string, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	c.kvStore(key, &memEntry{data: []byte(value), expiresAt: expiresAt(ttl)})
	return nil
}

func (c *MemCache) GetString(_ context.Context, key string) (string, bool, error) {
	if c == nil {
		return "", false, nil
	}
	v, ok := c.kv.Load(key)
	if !ok {
		return "", false, nil
	}
	e := v.(*memEntry)
	if isExpired(e.expiresAt) {
		if c.kvDelete(key) != nil {
			GlobalMetrics.L1EvictionsTTL.Add(1)
		}
		return "", false, nil
	}
	touchAccess(e)
	return string(e.data), true, nil
}

// ── Stats ────────────────────────────────────────────────────────────────────

func (c *MemCache) DBSize(_ context.Context) int64 {
	if c == nil {
		return 0
	}
	var n int64
	c.kv.Range(func(_, _ any) bool { n++; return true })
	c.sets.Range(func(_, _ any) bool { n++; return true })
	c.rbac.Range(func(_, _ any) bool { n++; return true })
	return n
}
