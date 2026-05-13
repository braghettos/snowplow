// resolved.go — Tag 0.30.7 binding: in-process L1 resolved-output cache
// (bounded LRU + byte-budget + time-to-live only).
//
// Per implementation plan §"Tag 0.30.7 — What's implemented":
//
//   - Bounded LRU over `(restaction_path|widget_path, user_identity,
//     query_hash)`. Entry count cap (default 100 000) AND byte-budget
//     cap (default 2 GB). Eviction is single least-recently-used — no
//     complex sweep machinery (Q-L1-BUDGET / audit guidance).
//   - Invalidation in this sub-ship: time-to-live only. DELETE-driven
//     invalidation lands at 0.30.8 per feedback_l1_invalidation_delete_only.md.
//
// Layering rule (project_redis_removal.md): the cache subsystem stays
// removable via CACHE_ENABLED. When `Disabled()` is true the resolver
// cache is never instantiated; dispatchers take the exact 0.30.6 path.
// Even with CACHE_ENABLED=true, RESOLVED_CACHE_ENABLED=false bypasses
// the L1 layer while keeping the rest of cache=on alive (typed-RBAC
// indexer, informer factory, EvaluateRBAC gate).
//
// Sub-ship A (0.30.7) does NOT add:
//   - DELETE-driven eviction (0.30.8).
//   - Dependency tracking (0.30.8).
//   - Refresher (0.30.8).
//   - Per-class queueing (0.30.11).
// Per the plan, none of these are sneaked in here.

package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Resolver-cache env knobs (defaults match chart-0.30.7 spec).
const (
	envResolvedCacheEnabled     = "RESOLVED_CACHE_ENABLED"
	envResolvedCacheMaxEntries  = "RESOLVED_CACHE_MAX_ENTRIES"
	envResolvedCacheMaxBytes    = "RESOLVED_CACHE_MAX_BYTES"
	envResolvedCacheTTLSeconds  = "RESOLVED_CACHE_TTL_SECONDS"
	envResolvedCacheSummaryEvery = "RESOLVED_CACHE_SUMMARY_EVERY_SECONDS"

	defaultResolvedCacheMaxEntries = 100_000
	defaultResolvedCacheMaxBytes   = int64(2) * 1024 * 1024 * 1024 // 2 GiB
	defaultResolvedCacheTTLSeconds = 3600
	defaultResolvedCacheSummaryEverySeconds = 300 // 5 min aggregate INFO line
)

// ResolvedEntry is the L1 cache value. The pre-encoded JSON bytes are
// what we hand back on a hit; storing the encoded form (rather than the
// runtime *RESTAction / *Widget object) avoids racey shared-state on
// the hit path — readers get an immutable []byte slice.
//
// Sub-ship A keeps the dependency fields nil/empty. They are reserved
// for sub-ship B (0.30.8) so callers can update wiring without a second
// breaking change.
type ResolvedEntry struct {
	RawJSON   []byte    // pre-encoded resolver output, ready to write
	CreatedAt time.Time // for TTL eviction

	// Reserved for 0.30.8 (do not populate at 0.30.7):
	//   ResourceTypeDeps []schema.GroupVersionResource
	//   NamespaceDeps    []string
}

// ResolvedKeyInputs is the canonical key-input bundle. The exact set
// of fields is binding: any change shifts the key space and instantly
// invalidates every in-flight cached entry — bump the constant
// resolvedKeyVersion below as part of any such change so the salt
// guarantees clean separation across rolling restarts.
type ResolvedKeyInputs struct {
	HandlerKind string   // "restactions" or "widgets"
	Group       string   // dispatched CR's GVR Group
	Version     string   // dispatched CR's GVR Version
	Resource    string   // dispatched CR's GVR Resource
	Namespace   string   // dispatched CR namespace
	Name        string   // dispatched CR name
	Username    string   // bind-identity username
	Groups      []string // bind-identity groups (will be sorted before hash)
	PerPage     int
	Page        int
	Extras      map[string]any
}

// resolvedKeyVersion is folded into every key hash so a key-schema
// change forces a clean break across rolling pods. Bump on any change
// to ResolvedKeyInputs fields or the key-encoding logic.
const resolvedKeyVersion = "v1"

// ResolvedCacheStore is the L1 resolved-output cache: a bounded LRU
// guarded by a single mutex with a per-entry byte budget. Constructed
// lazily by ResolvedCache(); never read or written without holding mu.
//
// Exported only so dispatchers and tests can take a handle; production
// code MUST go through cache.ResolvedCache() rather than instantiating
// stores directly.
type ResolvedCacheStore struct {
	mu sync.Mutex

	// LRU eviction order: front = most-recently-used.
	order *list.List
	// Lookup index. Value is *list.Element whose Value is *lruItem.
	index map[string]*list.Element

	maxEntries int
	maxBytes   int64
	ttl        time.Duration

	curBytes int64

	// Falsifier counters (atomic; safe to read without mu).
	hitTotal       atomic.Uint64
	missTotal      atomic.Uint64
	evictLRUTotal  atomic.Uint64
	evictTTLTotal  atomic.Uint64
	storeTotal     atomic.Uint64
}

type lruItem struct {
	key   string
	entry *ResolvedEntry
	bytes int64
}

var (
	resolvedCacheInstance *ResolvedCacheStore
	resolvedCacheOnce     sync.Once
	resolvedCacheStarted  atomic.Bool
)

// ResolvedCacheEnabled reports whether the L1 resolved-output cache is
// active. Two gates must both be true:
//  1. CACHE_ENABLED=true (entire cache subsystem). Anything else and we
//     are in pure 0.25.x parity mode; the resolver runs on every call.
//  2. RESOLVED_CACHE_ENABLED!=false (per-feature toggle). Defaults to
//     true when CACHE_ENABLED=true; explicit "false"/"0"/"no" disables.
//
// This split lets cache=on serve EvaluateRBAC + the typed-RBAC indexer
// while leaving L1 disabled for back-out scenarios.
func ResolvedCacheEnabled() bool {
	if Disabled() {
		return false
	}
	switch os.Getenv(envResolvedCacheEnabled) {
	case "false", "0", "no":
		return false
	default:
		return true
	}
}

// ResolvedCache returns the singleton resolved-output cache, lazily
// initialising it on first use. Returns nil when ResolvedCacheEnabled()
// is false — callers MUST nil-check.
func ResolvedCache() *ResolvedCacheStore {
	if !ResolvedCacheEnabled() {
		return nil
	}
	resolvedCacheOnce.Do(func() {
		resolvedCacheInstance = newResolvedCache(
			intFromEnv(envResolvedCacheMaxEntries, defaultResolvedCacheMaxEntries),
			int64FromEnv(envResolvedCacheMaxBytes, defaultResolvedCacheMaxBytes),
			time.Duration(intFromEnv(envResolvedCacheTTLSeconds, defaultResolvedCacheTTLSeconds))*time.Second,
		)
		startResolvedCacheSummary(resolvedCacheInstance)
	})
	return resolvedCacheInstance
}

// newResolvedCache constructs a fresh cache. Exported for tests; in
// production the singleton path goes through ResolvedCache().
func newResolvedCache(maxEntries int, maxBytes int64, ttl time.Duration) *ResolvedCacheStore {
	if maxEntries <= 0 {
		maxEntries = defaultResolvedCacheMaxEntries
	}
	if maxBytes <= 0 {
		maxBytes = defaultResolvedCacheMaxBytes
	}
	if ttl <= 0 {
		ttl = time.Duration(defaultResolvedCacheTTLSeconds) * time.Second
	}
	return &ResolvedCacheStore{
		order:      list.New(),
		index:      map[string]*list.Element{},
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		ttl:        ttl,
	}
}

// ComputeKey produces the canonical cache key for the supplied inputs.
// The output is a hex-encoded SHA-256 over a versioned, sorted byte
// representation of every field; tests cover stability + sensitivity.
func ComputeKey(in ResolvedKeyInputs) string {
	h := sha256.New()
	// version prefix — any future schema bump rotates the entire key
	// space on rolling restart.
	h.Write([]byte(resolvedKeyVersion))
	h.Write([]byte{0})
	h.Write([]byte(in.HandlerKind))
	h.Write([]byte{0})
	h.Write([]byte(in.Group))
	h.Write([]byte{0})
	h.Write([]byte(in.Version))
	h.Write([]byte{0})
	h.Write([]byte(in.Resource))
	h.Write([]byte{0})
	h.Write([]byte(in.Namespace))
	h.Write([]byte{0})
	h.Write([]byte(in.Name))
	h.Write([]byte{0})
	h.Write([]byte(in.Username))
	h.Write([]byte{0})

	// Groups: sort for stability across binding renderers.
	sortedGroups := append([]string(nil), in.Groups...)
	sort.Strings(sortedGroups)
	for _, g := range sortedGroups {
		h.Write([]byte(g))
		h.Write([]byte{0})
	}
	h.Write([]byte{0xff}) // groups terminator

	h.Write([]byte(strconv.Itoa(in.PerPage)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(in.Page)))
	h.Write([]byte{0})

	// Extras: canonicalise via sorted-key JSON. We deliberately use
	// json.Marshal on a SORTED-KEY surrogate instead of MarshalIndent
	// to keep the byte count tight; the surrogate is built by
	// canonicaliseExtras below.
	if len(in.Extras) > 0 {
		if buf, err := canonicaliseExtras(in.Extras); err == nil {
			h.Write(buf)
		} else {
			// On marshal failure (cyclic / non-JSON value), fall
			// back to a deterministic-but-pessimistic dump of
			// fmt.Sprintf so the key still varies with content.
			h.Write([]byte(fmt.Sprintf("%v", in.Extras)))
		}
	}
	h.Write([]byte{0})

	return hex.EncodeToString(h.Sum(nil))
}

// canonicaliseExtras emits a sorted-key JSON encoding of m. Nested
// maps are recursively canonicalised; everything else round-trips
// through json.Marshal as-is.
func canonicaliseExtras(m map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []byte
	out = append(out, '{')
	for i, k := range keys {
		if i > 0 {
			out = append(out, ',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		out = append(out, kb...)
		out = append(out, ':')
		v := m[k]
		if nested, ok := v.(map[string]any); ok {
			vb, err := canonicaliseExtras(nested)
			if err != nil {
				return nil, err
			}
			out = append(out, vb...)
			continue
		}
		vb, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		out = append(out, vb...)
	}
	out = append(out, '}')
	return out, nil
}

// Get returns the cached entry for key, or (nil, false). A TTL-expired
// entry is treated as a miss and is dropped during the same call so
// memory pressure is bounded. Increments hit/miss counters atomically.
func (c *ResolvedCacheStore) Get(key string) (*ResolvedEntry, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.index[key]
	if !ok {
		c.missTotal.Add(1)
		return nil, false
	}
	item := el.Value.(*lruItem)
	if c.ttl > 0 && time.Since(item.entry.CreatedAt) > c.ttl {
		c.removeElementLocked(el)
		c.evictTTLTotal.Add(1)
		c.missTotal.Add(1)
		return nil, false
	}
	// LRU touch: move to front.
	c.order.MoveToFront(el)
	c.hitTotal.Add(1)
	return item.entry, true
}

// Put stores entry under key, evicting LRU tail entries until both
// entry-count and byte-budget caps are satisfied. The entry's CreatedAt
// is set to time.Now() if zero. Putting under a key that already exists
// replaces the entry and adjusts curBytes accordingly.
func (c *ResolvedCacheStore) Put(key string, entry *ResolvedEntry) {
	if c == nil || entry == nil {
		return
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	bytes := int64(len(entry.RawJSON))

	c.mu.Lock()
	defer c.mu.Unlock()

	// Replace-in-place semantics if key already present.
	if el, ok := c.index[key]; ok {
		old := el.Value.(*lruItem)
		c.curBytes -= old.bytes
		old.entry = entry
		old.bytes = bytes
		c.curBytes += bytes
		c.order.MoveToFront(el)
		c.storeTotal.Add(1)
		c.evictUntilUnderCapsLocked()
		return
	}

	item := &lruItem{key: key, entry: entry, bytes: bytes}
	el := c.order.PushFront(item)
	c.index[key] = el
	c.curBytes += bytes
	c.storeTotal.Add(1)

	c.evictUntilUnderCapsLocked()
}

// Len returns the number of entries currently held. Safe to call
// without external locking; takes the internal mutex.
func (c *ResolvedCacheStore) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Bytes returns the current byte usage. Safe under concurrent traffic.
func (c *ResolvedCacheStore) Bytes() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.curBytes
}

// Stats returns a snapshot of the falsifier counters. Numbers are
// atomic and may drift between fields by a single call, which is fine
// for log aggregation.
type ResolvedCacheStats struct {
	Entries       int
	Bytes         int64
	MaxEntries    int
	MaxBytes      int64
	HitTotal      uint64
	MissTotal     uint64
	StoreTotal    uint64
	EvictLRUTotal uint64
	EvictTTLTotal uint64
}

func (c *ResolvedCacheStore) Stats() ResolvedCacheStats {
	if c == nil {
		return ResolvedCacheStats{}
	}
	c.mu.Lock()
	entries := c.order.Len()
	bytes := c.curBytes
	c.mu.Unlock()
	return ResolvedCacheStats{
		Entries:       entries,
		Bytes:         bytes,
		MaxEntries:    c.maxEntries,
		MaxBytes:      c.maxBytes,
		HitTotal:      c.hitTotal.Load(),
		MissTotal:     c.missTotal.Load(),
		StoreTotal:    c.storeTotal.Load(),
		EvictLRUTotal: c.evictLRUTotal.Load(),
		EvictTTLTotal: c.evictTTLTotal.Load(),
	}
}

// HitRate computes a simple cumulative hit rate. Returns 0 when there
// has been no traffic. Useful for the 5-min summary line and for the
// post-deploy falsifier (<50% hit rate = STOP per plan).
func (s ResolvedCacheStats) HitRate() float64 {
	total := s.HitTotal + s.MissTotal
	if total == 0 {
		return 0
	}
	return float64(s.HitTotal) / float64(total)
}

// evictUntilUnderCapsLocked drops tail entries (least recently used)
// until BOTH caps are satisfied. Must be called with mu held.
func (c *ResolvedCacheStore) evictUntilUnderCapsLocked() {
	for c.order.Len() > c.maxEntries || c.curBytes > c.maxBytes {
		tail := c.order.Back()
		if tail == nil {
			return
		}
		c.removeElementLocked(tail)
		c.evictLRUTotal.Add(1)
	}
}

// removeElementLocked drops el from order + index and adjusts the byte
// counter. Must be called with mu held.
func (c *ResolvedCacheStore) removeElementLocked(el *list.Element) {
	item := el.Value.(*lruItem)
	delete(c.index, item.key)
	c.order.Remove(el)
	c.curBytes -= item.bytes
	if c.curBytes < 0 {
		// Defensive — should never happen with non-negative bytes.
		c.curBytes = 0
	}
}

// startResolvedCacheSummary launches a single bounded goroutine that
// emits a `resolved_cache.summary` INFO line every N seconds. The
// goroutine self-suppresses on duplicate starts via resolvedCacheStarted.
// We never expose a stop method: the goroutine's lifetime is the
// process's lifetime and it does only constant work per tick.
func startResolvedCacheSummary(c *ResolvedCacheStore) {
	if c == nil {
		return
	}
	if !resolvedCacheStarted.CompareAndSwap(false, true) {
		return
	}
	every := time.Duration(intFromEnv(envResolvedCacheSummaryEvery, defaultResolvedCacheSummaryEverySeconds)) * time.Second
	if every <= 0 {
		every = time.Duration(defaultResolvedCacheSummaryEverySeconds) * time.Second
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			s := c.Stats()
			// Falsifier shape per plan §"Code-path falsifier":
			//   resolved_cache.summary entries=N bytes=B hit_rate=0.NN
			//   evict_lru=X evict_delete=Y
			// evict_delete is always 0 in sub-ship A (lands at 0.30.8).
			slog.Info("resolved_cache.summary",
				slog.String("subsystem", "cache"),
				slog.Int("entries", s.Entries),
				slog.Int64("bytes", s.Bytes),
				slog.Float64("hit_rate", s.HitRate()),
				slog.Uint64("evict_lru", s.EvictLRUTotal),
				slog.Uint64("evict_ttl", s.EvictTTLTotal),
				slog.Uint64("evict_delete", 0),
				slog.Uint64("hit_total", s.HitTotal),
				slog.Uint64("miss_total", s.MissTotal),
				slog.Uint64("store_total", s.StoreTotal),
				slog.Int("max_entries", s.MaxEntries),
				slog.Int64("max_bytes", s.MaxBytes),
			)
		}
	}()
}

// resetResolvedCacheForTest tears the singleton down so each test sees
// a clean cache. Exported only via the *_test.go shim — production
// code MUST NOT call this.
func resetResolvedCacheForTest() {
	resolvedCacheInstance = nil
	resolvedCacheOnce = sync.Once{}
	resolvedCacheStarted.Store(false)
}

// intFromEnv parses an env var as int with a default fallback. We
// intentionally accept any non-int value as "use default" with no
// logging — env-knob misconfiguration is a deploy issue and the test
// suite covers correct parses.
func intFromEnv(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func int64FromEnv(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}
