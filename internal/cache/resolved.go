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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Resolver-cache env knobs (defaults match chart-0.30.7 spec).
const (
	envResolvedCacheEnabled      = "RESOLVED_CACHE_ENABLED"
	envResolvedCacheMaxEntries   = "RESOLVED_CACHE_MAX_ENTRIES"
	envResolvedCacheMaxBytes     = "RESOLVED_CACHE_MAX_BYTES"
	envResolvedCacheTTLSeconds   = "RESOLVED_CACHE_TTL_SECONDS"
	envResolvedCacheSummaryEvery = "RESOLVED_CACHE_SUMMARY_EVERY_SECONDS"

	// envResolvedCacheApistageEnabled is the Ship E (0.30.116) opt-in
	// gate for the per-api-stage L1 key-swap. Default OFF — flag-off the
	// RESTAction resolver runs byte-identical to 0.30.115 (AC-E1). It is
	// gated UNDER ResolvedCacheEnabled() (the api-stage L1 needs the
	// resolved-output store + the refresher).
	envResolvedCacheApistageEnabled = "RESOLVED_CACHE_APISTAGE_ENABLED"

	defaultResolvedCacheMaxEntries          = 100_000
	defaultResolvedCacheMaxBytes            = int64(2) * 1024 * 1024 * 1024 // 2 GiB
	defaultResolvedCacheTTLSeconds          = 3600
	defaultResolvedCacheSummaryEverySeconds = 300 // 5 min aggregate INFO line
)

// CacheEntryClassApistage is the ResolvedKeyInputs.CacheEntryClass
// discriminant for a per-api-stage L1 entry (Ship E, 0.30.116). The
// resolved-output store, the dep-tracker, the LRU/TTL machinery, and
// ComputeKey are all reused verbatim — "apistage" is just a third
// granularity of L1 key, not a new cache. The refresher's resolve-once
// seam branches on it to re-run a single stage rather than a whole
// RESTAction.
//
// NOTE the STRING VALUE is unchanged ("apistage"): it is hashed into the
// cache key (ComputeKey) and is the refresher registry key — rotating it
// would invalidate every in-flight entry. The 0.30.118 rename touches the
// Go const IDENTIFIER only.
const CacheEntryClassApistage = "apistage"

// ResolvedEntry is the L1 cache value. The pre-encoded JSON bytes are
// what we hand back on a hit; storing the encoded form (rather than the
// runtime *RESTAction / *Widget object) avoids racey shared-state on
// the hit path — readers get an immutable []byte slice.
//
// Sub-ship B (0.30.8) populates the Inputs field so the refresher can
// re-invoke the resolver on UPDATE/PATCH events. RawJSON + CreatedAt
// remain unchanged from sub-ship A.
type ResolvedEntry struct {
	RawJSON   []byte    // pre-encoded resolver output, ready to write
	CreatedAt time.Time // for TTL eviction

	// Inputs is the canonical key-input bundle the entry was resolved
	// from. The refresher uses it to drive a re-resolve when an
	// UPDATE/PATCH event fires for any of this entry's dep tuples.
	// Nil-safe: a missing Inputs (e.g., legacy 0.30.7 entries during a
	// rolling restart) skips refresh but still serves TTL+LRU correctly.
	Inputs *ResolvedKeyInputs

	// Items / ItemsAPIVersion / ItemsKind — Ship 0.30.121 R3 — the
	// pre-parsed LIST envelope for a CacheEntryClassApistage CONTENT
	// entry. F1's content-gate (gateListEnvelope) re-unmarshalled the
	// stored RawJSON envelope on EVERY content-Get-hit to run
	// filterListByRBAC over the items — the ~1.73 GiB double-unmarshal.
	// R3 parses the envelope's items ONCE at the content-entry Put site
	// and stores them here; the content-gate then runs filterListByRBAC
	// directly over Items and skips the unmarshal. Output is byte-
	// identical by construction (same parse -> filter -> marshalAsList
	// pipeline; only the unmarshal TIMING moves from per-hit to per-Put).
	//
	// Populated ONLY for CacheEntryClassApistage LIST content entries
	// (name=="" — a collection). Nil for restactions/widgets entries and
	// for apistage GET-by-name entries (gateGetEnvelope is left as-is).
	// A nil Items means "no pre-parse — gate via the RawJSON unmarshal
	// path" so the field is purely additive and back-compatible.
	Items           []*unstructured.Unstructured
	ItemsAPIVersion string
	ItemsKind       string
}

// ResolvedKeyInputs is the canonical key-input bundle. The exact set
// of fields is binding: any change shifts the key space and instantly
// invalidates every in-flight cached entry — bump the constant
// resolvedKeyVersion below as part of any such change so the salt
// guarantees clean separation across rolling restarts.
type ResolvedKeyInputs struct {
	// CacheEntryClass is the entry-class discriminant — one of the string
	// values "restactions", "widgets", or "apistage". (Renamed from
	// HandlerKind in 0.30.118; the string VALUES are unchanged — they are
	// hashed into the key and used as refresher registry keys.)
	CacheEntryClass string
	Group           string   // dispatched CR's GVR Group
	Version         string   // dispatched CR's GVR Version
	Resource        string   // dispatched CR's GVR Resource
	Namespace       string   // dispatched CR namespace
	Name            string   // dispatched CR name
	Username        string   // bind-identity username
	Groups          []string // bind-identity groups (will be sorted before hash)
	PerPage         int
	Page            int
	Extras          map[string]any

	// Stage is set ONLY for CacheEntryClass=="apistage" entries (Ship E,
	// 0.30.116). It carries the per-stage discriminator string —
	// stage id + O5 canonical filter-hash + a hash of the stage's
	// effective dict input (its dependsOn predecessor output). Empty
	// for "restactions"/"widgets" entries, so ComputeKey is
	// byte-identical to 0.30.115 for every non-apistage key (a
	// pre-existing entry's key does not shift). The api-stage resolver
	// builds the Stage value; ComputeKey only folds it into the hash.
	Stage string
}

// resolvedKeyVersion is folded into every key hash so a key-schema
// change forces a clean break across rolling pods. Bump on any change
// to ResolvedKeyInputs fields or the key-encoding logic.
//
// NOT bumped for Ship E's Stage field: ComputeKey folds Stage in only
// when it is non-empty (see ComputeKey), so every "restactions" /
// "widgets" key — Stage=="" — hashes byte-identically to v1. A version
// bump would needlessly rotate the whole key space on the 0.30.116
// rolling restart for zero correctness gain.
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
	hitTotal         atomic.Uint64
	missTotal        atomic.Uint64
	evictLRUTotal    atomic.Uint64
	evictTTLTotal    atomic.Uint64
	evictDeleteTotal atomic.Uint64 // 0.30.8: DELETE-event-driven evictions
	storeTotal       atomic.Uint64

	// Ship E (0.30.116) api-stage counters. apistageStoreTotal counts
	// Put()s of an "apistage"-kind entry; apistageEvictTotal counts
	// evictions (LRU/TTL/DELETE) of one. apistage_evict_pressure in the
	// summary line is the evict/store ratio — the O6 budget signal: a
	// high ratio means the maxEntries/maxBytes budget is too small for
	// the N-identities × M-stages cardinality and the api-stage entries
	// are churning rather than being reused. The store classifies via
	// entry.Inputs.CacheEntryClass, so the opaque key string never needs a
	// per-kind tag.
	apistageStoreTotal atomic.Uint64
	apistageEvictTotal atomic.Uint64
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

// ApistageL1Enabled reports whether the Ship E (0.30.116) per-api-stage
// L1 key-swap is opted in. THREE gates, all must hold:
//  1. CACHE_ENABLED=true        — the whole cache subsystem (Disabled()).
//  2. RESOLVED_CACHE_ENABLED!=false — the resolved-output L1 store +
//     refresher, which the api-stage entry reuses verbatim.
//  3. RESOLVED_CACHE_APISTAGE_ENABLED=="true" — the per-feature opt-in.
//
// Default OFF (gate 3 must be the explicit string "true", mirroring
// PrewarmEnabled). Flag-off the RESTAction resolver runs byte-identical
// to 0.30.115 — no per-stage Get/Put, no api-stage L1 key (AC-E1).
func ApistageL1Enabled() bool {
	if !ResolvedCacheEnabled() {
		return false
	}
	return os.Getenv(envResolvedCacheApistageEnabled) == "true"
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
		// 0.30.8: wire the cache into the dep tracker so OnDelete can
		// evict and so any eviction path (LRU/TTL/DELETE) calls
		// Deps().RemoveL1Key to keep dep records and L1 entries
		// in lock-step.
		Deps().SetStore(resolvedCacheInstance)
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
	h.Write([]byte(in.CacheEntryClass))
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

	// Identity (Username + Groups). Ship F1 (0.30.119): the api-stage
	// content layer is IDENTITY-FREE — an api-stage entry's resolved
	// content (a per-object GET / per-namespace LIST K8s call result) is
	// identity-invariant: K8s RBAC is a binary gate on (gvr, ns, [name])
	// units, it never filters items or shapes content, so the SAME
	// content unit is shared by every user the gate admits. Omitting the
	// identity fields makes the apistage key (gvr, ns, name-or-list,
	// filter-hash, stage-input-hash) shared across users. The per-user
	// narrowing moves to the SERVE-TIME RBAC gate (dispatcher path).
	//
	// This is a per-CLASS key shape, NOT a per-resource switch
	// (feedback_no_special_cases): the discriminant is the entry class,
	// uniform for every apistage entry of every GVR. "restactions" /
	// "widgets" keys hash Username+Groups exactly as before — byte-
	// identical, no key-space rotation. apistage is flag-off in prod
	// (RESOLVED_CACHE_APISTAGE_ENABLED default off), so this key change
	// rotates nothing live.
	if in.CacheEntryClass != CacheEntryClassApistage {
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
	}

	h.Write([]byte(strconv.Itoa(in.PerPage)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(in.Page)))
	h.Write([]byte{0})

	// Stage (Ship E, 0.30.116): folded in ONLY when non-empty. An empty
	// Stage writes nothing — so a "restactions"/"widgets" key (Stage=="")
	// hashes byte-identically to the pre-0.30.116 encoding and no
	// in-flight entry's key shifts. The non-empty branch writes a
	// sentinel byte (0x01) before the value so an api-stage key can
	// never collide with a hypothetical extras-only key that happened to
	// produce the same trailing bytes.
	if in.Stage != "" {
		h.Write([]byte{0x01})
		h.Write([]byte(in.Stage))
		h.Write([]byte{0})
	}

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
	bytes := entryBytes(entry)

	c.mu.Lock()
	defer c.mu.Unlock()

	apistage := isApistageEntry(entry)

	// Replace-in-place semantics if key already present.
	if el, ok := c.index[key]; ok {
		old := el.Value.(*lruItem)
		c.curBytes -= old.bytes
		old.entry = entry
		old.bytes = bytes
		c.curBytes += bytes
		c.order.MoveToFront(el)
		c.storeTotal.Add(1)
		if apistage {
			c.apistageStoreTotal.Add(1)
		}
		c.evictUntilUnderCapsLocked()
		return
	}

	item := &lruItem{key: key, entry: entry, bytes: bytes}
	el := c.order.PushFront(item)
	c.index[key] = el
	c.curBytes += bytes
	c.storeTotal.Add(1)
	if apistage {
		c.apistageStoreTotal.Add(1)
	}

	c.evictUntilUnderCapsLocked()
}

// isApistageEntry reports whether entry is a Ship E api-stage L1 entry —
// classified by its Inputs.CacheEntryClass. Nil-safe.
func isApistageEntry(entry *ResolvedEntry) bool {
	return entry != nil && entry.Inputs != nil &&
		entry.Inputs.CacheEntryClass == CacheEntryClassApistage
}

// itemsTreeOverheadFactor estimates the in-memory footprint of a parsed
// []*unstructured.Unstructured tree relative to the JSON text it was
// parsed from. A Go map[string]any / []any interface tree carries
// per-node header + boxing overhead well above the compact JSON byte
// length; 3x is a deliberately conservative floor so the LRU byte cap
// does not silently under-count the R3 pre-parsed Items (Ship 0.30.121).
const itemsTreeOverheadFactor = 3

// entryBytes is the LRU byte-accounting weight of an L1 entry — Ship
// 0.30.121 R3. It counts the pre-encoded RawJSON envelope AND, when the
// entry carries the R3 pre-parsed Items (an apistage LIST content
// entry), the estimated in-memory footprint of that parsed tree. Without
// the Items term the byte cap silently under-counts every content entry
// by roughly its own envelope size, letting curBytes drift far past
// maxBytes. Items is parsed from RawJSON, so its tree size is estimated
// as itemsTreeOverheadFactor * len(RawJSON) rather than re-serialising
// each item (which would re-introduce the very marshal R3 removes).
// A nil/empty Items contributes nothing — restactions/widgets entries
// and apistage GET entries are accounted exactly as pre-0.30.121.
func entryBytes(entry *ResolvedEntry) int64 {
	if entry == nil {
		return 0
	}
	b := int64(len(entry.RawJSON))
	if len(entry.Items) > 0 {
		b += int64(len(entry.RawJSON)) * itemsTreeOverheadFactor
	}
	return b
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
	Entries          int
	Bytes            int64
	MaxEntries       int
	MaxBytes         int64
	HitTotal         uint64
	MissTotal        uint64
	StoreTotal       uint64
	EvictLRUTotal    uint64
	EvictTTLTotal    uint64
	EvictDeleteTotal uint64 // 0.30.8: DELETE-event-driven evictions

	// Ship E (0.30.116) api-stage counters.
	ApistageStoreTotal uint64
	ApistageEvictTotal uint64
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
		Entries:            entries,
		Bytes:              bytes,
		MaxEntries:         c.maxEntries,
		MaxBytes:           c.maxBytes,
		HitTotal:           c.hitTotal.Load(),
		MissTotal:          c.missTotal.Load(),
		StoreTotal:         c.storeTotal.Load(),
		EvictLRUTotal:      c.evictLRUTotal.Load(),
		EvictTTLTotal:      c.evictTTLTotal.Load(),
		EvictDeleteTotal:   c.evictDeleteTotal.Load(),
		ApistageStoreTotal: c.apistageStoreTotal.Load(),
		ApistageEvictTotal: c.apistageEvictTotal.Load(),
	}
}

// ApistageEvictPressure is the Ship E (0.30.116) O6 budget signal: the
// ratio of api-stage entry evictions to api-stage entry stores. 0 means
// no api-stage churn (every stored stage entry is still resident or was
// never stored). A ratio approaching 1 means the maxEntries/maxBytes
// budget is too small for the N-identities × M-stages cardinality — the
// api-stage entries are being evicted as fast as they are written, so
// the key-swap buys nothing. The tester's 50K bench reads this to set
// the budget; the feature ships default-off until it is green.
func (s ResolvedCacheStats) ApistageEvictPressure() float64 {
	if s.ApistageStoreTotal == 0 {
		return 0
	}
	return float64(s.ApistageEvictTotal) / float64(s.ApistageStoreTotal)
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
//
// 0.30.8: also clears the dep-tracker reverse index for this key so
// dep records don't outlive the L1 entry. RemoveL1Key is itself
// lock-free (sync.Map ops) so calling it while holding c.mu is safe;
// the reverse path never re-enters the store.
func (c *ResolvedCacheStore) removeElementLocked(el *list.Element) {
	item := el.Value.(*lruItem)
	delete(c.index, item.key)
	c.order.Remove(el)
	c.curBytes -= item.bytes
	if c.curBytes < 0 {
		// Defensive — should never happen with non-negative bytes.
		c.curBytes = 0
	}
	// Ship E (0.30.116): count an api-stage eviction for the O6 pressure
	// metric. Classified off the dropped entry's CacheEntryClass.
	if isApistageEntry(item.entry) {
		c.apistageEvictTotal.Add(1)
	}
	// Dep-tracker cleanup. Safe even when L1 is the only consumer
	// (Deps() is always non-nil); a no-op when no edges were ever
	// recorded for this key.
	Deps().RemoveL1Key(item.key)
}

// deleteForDep removes the entry under key, returning true if a live
// entry was found and dropped. Increments the DELETE-eviction counter.
// Used by DepTracker.OnDelete; production code MUST NOT call this
// path directly (DELETE eviction must flow through the dep tracker so
// the dep-record cleanup runs alongside the L1 drop).
//
// Performs a separate lock acquisition from any in-flight Get/Put —
// holds c.mu only for the duration of the index lookup + LRU detach.
// The dep tracker calls RemoveL1Key AFTER deleteForDep returns; since
// the entry is already gone from index/order, the second cleanup pass
// is a cheap no-op on the L1 side and does the actual dep-record
// removal on the dep side.
func (c *ResolvedCacheStore) deleteForDep(key string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	el, ok := c.index[key]
	if !ok {
		c.mu.Unlock()
		return false
	}
	// removeElementLocked also calls Deps().RemoveL1Key — but in this
	// path the dep tracker is mid-iteration over the reverse index
	// for THIS key, and LoadAndDelete inside RemoveL1Key is a no-op
	// the second time. We accept the trivial double-call rather than
	// branching the eviction body.
	item := el.Value.(*lruItem)
	delete(c.index, item.key)
	c.order.Remove(el)
	c.curBytes -= item.bytes
	if c.curBytes < 0 {
		c.curBytes = 0
	}
	// Ship E (0.30.116): count an api-stage DELETE-eviction for the O6
	// pressure metric — same classification as removeElementLocked.
	apistage := isApistageEntry(item.entry)
	c.mu.Unlock()
	c.evictDeleteTotal.Add(1)
	if apistage {
		c.apistageEvictTotal.Add(1)
	}
	return true
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
			d := Deps().Stats()
			r := refresherStatsSnapshot()
			dw := DepWatchStatsSnapshot()
			// Falsifier shape per plan §"Code-path falsifier" (0.30.8):
			//   resolved_cache.summary entries=N bytes=B hit_rate=0.NN
			//   evict_lru=X evict_delete=Y refresh_enqueued=M refresh_completed=K
			//   dep_map_size=D
			slog.Info("resolved_cache.summary",
				slog.String("subsystem", "cache"),
				slog.Int("entries", s.Entries),
				slog.Int64("bytes", s.Bytes),
				slog.Float64("hit_rate", s.HitRate()),
				slog.Uint64("evict_lru", s.EvictLRUTotal),
				slog.Uint64("evict_ttl", s.EvictTTLTotal),
				slog.Uint64("evict_delete", s.EvictDeleteTotal),
				slog.Uint64("refresh_enqueued", d.EnqueueUpdateTotal),
				slog.Uint64("refresh_completed", r.completed),
				slog.Uint64("refresh_failed", r.failed),
				slog.Uint64("refresh_retried", r.retried),
				slog.Uint64("refresh_dropped", r.dropped),
				slog.Uint64("refresh_skipped_stage_error", r.skippedStageError),
				slog.Int64("dep_map_size", d.TotalRecords),
				slog.Uint64("dep_record_total", d.RecordTotal),
				slog.Uint64("dep_record_dropped_cap", d.RecordDroppedCap),
				slog.Uint64("dep_record_dropped_no_key", d.RecordDroppedNoKey),
				slog.Uint64("dep_dirty_mark_total", d.DirtyMarkTotal),
				slog.Uint64("dep_add_dropped_pre_sync", dw.AddDroppedPreSync),
				slog.Uint64("dep_add_propagated", dw.AddPropagated),
				slog.Uint64("hit_total", s.HitTotal),
				slog.Uint64("miss_total", s.MissTotal),
				slog.Uint64("store_total", s.StoreTotal),
				slog.Int("max_entries", s.MaxEntries),
				slog.Int64("max_bytes", s.MaxBytes),
				// Ship E (0.30.116) O6 budget signal — AC-E7.
				slog.Uint64("apistage_store_total", s.ApistageStoreTotal),
				slog.Uint64("apistage_evict_total", s.ApistageEvictTotal),
				slog.Float64("apistage_evict_pressure", s.ApistageEvictPressure()),
				slog.Bool("apistage_enabled", ApistageL1Enabled()),
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

// ResetResolvedCacheForTest is the exported variant for cross-package
// tests (e.g. internal/handlers/dispatchers' Ship C falsifier).
// Production code MUST NOT call it.
func ResetResolvedCacheForTest() {
	resetResolvedCacheForTest()
}

// DeleteForTest removes key from the resolved cache. Cross-package
// test-only seam — Ship C's resurrect-guard test emulates a DELETE-evict
// landing mid-refresh. Production eviction MUST flow through the dep
// tracker (deleteForDep) so dep records are cleaned alongside; this
// helper deliberately bypasses that and is therefore TEST-ONLY.
func (c *ResolvedCacheStore) DeleteForTest(key string) {
	if c == nil {
		return
	}
	c.deleteForDep(key)
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

// boolFromEnv parses an env var as a bool with a default fallback.
// Recognises the canonical false set ("false", "0", "no") and the
// canonical true set ("true", "1", "yes"); any unset or unrecognised
// value returns def. Used by R4's RESOLVER_COMPOSITION_STREAMING_LIST
// (default true) — env-knob misconfiguration is a deploy issue, so an
// unrecognised value falls back silently to the default.
func boolFromEnv(key string, def bool) bool {
	switch os.Getenv(key) {
	case "false", "0", "no":
		return false
	case "true", "1", "yes":
		return true
	default:
		return def
	}
}
