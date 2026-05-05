// Package cache — Q-RBACC-L2-1: post-refilter L2 cache.
//
// OVERVIEW
//
// L1 stores the v3 wrapper `cachedRESTAction{ CR, ProtectedDict, "v3" }`
// keyed per binding identity. On every HTTP /call the dispatcher walks
// that wrapper through `RefilterRESTAction` (per-item gojq for nsFrom +
// per-item RBAC eval + outer JQ), turning a 1.9 KB wire output into a
// 1.0–1.4 s wall on the cyberjoker hot path. For the 40 MB
// `compositions-panels` page the refilter cost saturated CPU long enough
// to miss 30 consecutive 1 s liveness probes (v6 phase-6 SIGKILL).
//
// L2 is a SECOND, post-refilter cache. Key tuple (l1Key, binding identity,
// groups hash) — collapsing per-user keying down to per-binding-identity ×
// groups (so 1000 users → ~10 entries in the hot path). Value is the
// wire-bytes the dispatcher writes to `wri.Write` plus a pre-decoded
// status map for the apiref consumer. Hits skip refilter entirely.
//
// INVARIANTS PRESERVED
//
//  - feedback_l1_invalidation_delete_only.md  — L2 evicts ONLY on K8s
//    DELETE, CRB transition, or RESTAction-CR change. UPDATE/PATCH stays
//    SWR (matches L1).
//  - feedback_no_special_cases.md             — `MIN_REDUCTION_RATIO` is
//    a generic property of post-refilter / L1 ratio. No per-user / per-RA
//    branches anywhere.
//  - feedback_cache_must_not_constrain_jq.md  — bytes are byte-identical
//    to a fresh refilter; widget canonicalisation is unconstrained.
//  - project_redis_removal.md                 — `CACHE_ENABLED=false`
//    short-circuits L2 (cache stays removable).
//
// ROLLBACK
//
// Behind env flag `CACHE_L2_REFILTER_ENABLED` (default false). Flipping
// to false makes every Get/Set return zero-value/no-op without disturbing
// the L1 hit path (the dispatcher falls through to RefilterRESTActionDeduped).

package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ── Tunables (env-overridable) ────────────────────────────────────────────────

const (
	envL2Enabled        = "CACHE_L2_REFILTER_ENABLED"
	envL2MaxBytes       = "CACHE_L2_REFILTER_MAX_BYTES"
	envL2MaxEntryBytes  = "CACHE_L2_REFILTER_MAX_ENTRY_BYTES"
	envL2MaxEntries     = "CACHE_L2_REFILTER_MAX_ENTRIES"
	envL2MinReduction   = "CACHE_L2_REFILTER_MIN_REDUCTION_RATIO"
	envL2HitAuditSample = "CACHE_L2_REFILTER_HIT_AUDIT_SAMPLE"

	defaultL2MaxBytes        int64   = 512 << 20 // 512 MB
	defaultL2MaxEntryBytes   int64   = 50 << 20  // 50 MB
	defaultL2MaxEntries      int     = 100_000
	defaultL2MinReduction    float64 = 0.20
	defaultL2HitAuditSample  float64 = 0.01 // 1% of L2 hits emit audit
)

// L2EntryV1Tag identifies the wire shape of an L2 entry written by this
// version of the binary. Bumping this string forces all readers to treat
// older entries as a miss + opportunistic evict (defends against schema
// migrations on the L1 wrapper that happen out-of-band).
const L2EntryV1Tag = "l2-v1"

// L2Entry is the value stored in the L2 cache. The Refiltered byte slice
// is the EXACT wire payload the dispatcher writes to `wri.Write`; the
// Status map mirrors what apiref consumers read (`json.Unmarshal(refiltered)["status"]`)
// — pre-decoded so the read path stays O(1).
//
// CONTRACT (load-bearing): downstream readers MUST treat Status as
// READ-ONLY. The value is shared across goroutines via pointer; mutating
// it would corrupt cached state. The widget pipeline already passes the
// status map to gojq read-only; the contract matches existing code.
type L2Entry struct {
	Refiltered []byte         // exact bytes the dispatcher writes to wri.Write
	Status     map[string]any // pre-decoded "status" object for apiref consumers
	HasUAF     bool           // cached unstructuredHasUAF result (apiref needs it)
	SizeBytes  int            // == len(Refiltered) — fast budget accounting
	StoredAt   int64          // unix nano — LRU eviction timestamp
	SchemaVer  string         // wrapper schema_version snapshot — defends L1 schema bump
	tag        string         // L2EntryV1Tag — refuse on mismatch
	// Reverse-index components, used by EvictL2For* helpers without
	// re-parsing the cache key on every eviction. Filled at SetL2 time.
	l1Key       string
	identity    string
	gvrName     string // ParseResolvedKey-derived "{group}/{version}/{resource}/{name}"; "" when unparseable
}

// l2Index is the in-process L2 cache. sync.Map for the hash table; an
// inline LRU list maintained under a fine-grained mutex for eviction
// ordering. Reverse indexes (by l1Key, by identity, by gvrName) are kept
// as sync.Map<string, *sync.Map[hashKey]struct{}> so that targeted
// eviction is O(N) over the affected subset, not O(N) over the entire
// cache.
type l2Index struct {
	enabled        atomic.Bool
	store          sync.Map // hashKey (string) -> *L2Entry
	residentBytes  atomic.Int64
	residentCount  atomic.Int64

	// Reverse indexes for targeted eviction.
	byL1Key    sync.Map // l1Key -> *sync.Map[hashKey]struct{}
	byIdentity sync.Map // bindingIdentity -> *sync.Map[hashKey]struct{}
	byGVRName  sync.Map // "{gvrKey}/{name}" -> *sync.Map[hashKey]struct{}

	// LRU access list. Every Set/Get under enabled appends a tick; the
	// sweeper runs on the same 30s ticker as MemCache.StartEviction and
	// trims entries when residentBytes or residentCount exceeds the cap.
	// We do NOT maintain a doubly-linked list (sync.Map cannot be ordered
	// without a separate slice); instead we rely on StoredAt timestamps
	// and a periodic sweep. For the 50-100 identity scale, a single
	// 100K-entry sweep at 30s cadence is cheap (~1 ms wall).
	mu        sync.Mutex
	gen       int64 // monotonic generation tag for last sweep
}

// globalL2 is the package-level L2 cache. Allocated on first call to
// initL2 (lazy). Tests may use NewL2IndexForTest to avoid touching the
// global.
var globalL2 = &l2Index{}
var globalL2Once sync.Once

// initL2 reads env defaults once and caches the enabled flag. Subsequent
// SetL2 / GetL2 calls are zero-allocation if disabled.
func initL2() {
	globalL2Once.Do(func() {
		if os.Getenv(envL2Enabled) == "true" {
			globalL2.enabled.Store(true)
		}
	})
}

// L2Enabled returns true iff the env flag is on AND the master CACHE_ENABLED
// flag is not set to false. Cheap; safe in hot paths.
func L2Enabled() bool {
	if Disabled() {
		return false
	}
	initL2()
	return globalL2.enabled.Load()
}

// SetL2EnabledForTest forces the L2 enabled state without consulting env.
// TEST-ONLY: production code MUST go through the env path.
func SetL2EnabledForTest(v bool) {
	initL2()
	globalL2.enabled.Store(v)
}

// l2MaxBytes / l2MaxEntryBytes / l2MaxEntries / l2MinReduction read
// env vars on each call (cheap; called at write time only). We don't
// cache them because operators may set them via downward-API at pod
// start; once set they don't change. The repeated env lookup costs
// ~50 ns per write — orders below the cost we save by caching the bytes.
func l2MaxBytes() int64        { return envInt64(envL2MaxBytes, defaultL2MaxBytes) }
func l2MaxEntryBytes() int64   { return envInt64(envL2MaxEntryBytes, defaultL2MaxEntryBytes) }
func l2MaxEntries() int        { return int(envInt64(envL2MaxEntries, int64(defaultL2MaxEntries))) }
func l2MinReduction() float64  { return envFloat(envL2MinReduction, defaultL2MinReduction) }
func l2HitAuditSample() float64 {
	return envFloat(envL2HitAuditSample, defaultL2HitAuditSample)
}

func envInt64(name string, def int64) int64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func envFloat(name string, def float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f > 1 {
		return def
	}
	return f
}

// ── Key helpers ───────────────────────────────────────────────────────────────

// L2Key derives the L2 cache key. To avoid `|`-injection collisions
// between components, we prefix each component with its 8-byte big-endian
// length before hashing. Adversarial inputs that put `|` characters
// inside a component cannot collide with a different distribution of
// the same total bytes across components.
//
// Empty inputs yield an empty key; callers MUST treat "" as "skip L2".
func L2Key(l1Key, bindingIdentity, groupsHash string) string {
	if l1Key == "" || bindingIdentity == "" || groupsHash == "" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte("L2"))
	writeLP(h, l1Key)
	writeLP(h, bindingIdentity)
	writeLP(h, groupsHash)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// writeLP writes an 8-byte big-endian length prefix followed by the
// component bytes. Domain-separates each component so the hash is
// unambiguous regardless of the bytes inside any component.
func writeLP(h interface{ Write([]byte) (int, error) }, s string) {
	var lp [8]byte
	n := uint64(len(s))
	lp[0] = byte(n >> 56)
	lp[1] = byte(n >> 48)
	lp[2] = byte(n >> 40)
	lp[3] = byte(n >> 32)
	lp[4] = byte(n >> 24)
	lp[5] = byte(n >> 16)
	lp[6] = byte(n >> 8)
	lp[7] = byte(n)
	_, _ = h.Write(lp[:])
	_, _ = h.Write([]byte(s))
}

// HashGroups returns a stable hex-encoded sha256 of the sorted, joined
// group list. Mirrors `refilterFlightKey`'s hash so the L2 key reuses
// the same cohort fingerprint as the singleflight key. Exported so
// dispatcher and apiref callers stay in lockstep on the hash function.
//
// Empty groups → "no_groups". This matches the singleflight contract
// exactly so callers don't have to remember the convention.
func HashGroups(groups []string) string {
	if len(groups) == 0 {
		return "no_groups"
	}
	cp := make([]string, len(groups))
	copy(cp, groups)
	sort.Strings(cp)
	h := sha256.Sum256([]byte(strings.Join(cp, "\x00")))
	return hex.EncodeToString(h[:])
}

// gvrNameFromL1Key extracts the "{gvrKey}/{name}" tag used by
// EvictL2ForRESTAction. Returns "" on parse failure (caller skips the
// reverse-index registration; eviction by RA is a no-op for that entry,
// matching pre-L2 behaviour where unaffected entries decay via L1 TTL).
func gvrNameFromL1Key(l1Key string) string {
	info, ok := ParseResolvedKey(l1Key)
	if !ok {
		return ""
	}
	return GVRToKey(info.GVR) + "/" + info.Name
}

// ── Public API ────────────────────────────────────────────────────────────────

// GetL2Refilter returns the cached entry for `key`. (nil, false) on miss
// or when L2 is disabled. Schema mismatch is treated as a miss and the
// stale entry is evicted opportunistically.
func GetL2Refilter(key string) (*L2Entry, bool) {
	if !L2Enabled() || key == "" {
		return nil, false
	}
	v, ok := globalL2.store.Load(key)
	if !ok {
		GlobalMetrics.L2Misses.Add(1)
		return nil, false
	}
	e, ok := v.(*L2Entry)
	if !ok || e == nil {
		GlobalMetrics.L2Misses.Add(1)
		return nil, false
	}
	if e.tag != L2EntryV1Tag {
		// Wire-shape mismatch — opportunistic evict + miss.
		globalL2.evictKey(key, e)
		GlobalMetrics.L2Misses.Add(1)
		return nil, false
	}
	GlobalMetrics.L2Hits.Add(1)
	return e, true
}

// SetL2Refilter writes an entry IFF (a) L2 is enabled, (b) the entry
// passes the size cap, and (c) the entry passes the reduction-ratio
// gate (post-refilter ≤ (1 - MIN_REDUCTION) × L1 raw size). The reduction
// gate is the §1.3 generic policy that prevents admin × compositions-panels
// (post-refilter ≈ L1) from consuming the L2 budget.
//
// Returns the resident-bytes delta (0 if the entry was rejected). The
// `rawL1Size` parameter is the size of the L1 wrapper bytes the refilter
// was computed from; pass 0 to bypass the reduction gate (e.g. for
// prewarm where the L1 size is not handy — accept the entry on size cap
// only).
func SetL2Refilter(key string, e *L2Entry, rawL1Size int) {
	if !L2Enabled() || key == "" || e == nil || len(e.Refiltered) == 0 {
		return
	}
	maxEntry := l2MaxEntryBytes()
	if int64(len(e.Refiltered)) > maxEntry {
		GlobalMetrics.L2SkippedSizeCap.Add(1)
		return
	}
	if rawL1Size > 0 {
		ratio := float64(len(e.Refiltered)) / float64(rawL1Size)
		if ratio > (1.0 - l2MinReduction()) {
			GlobalMetrics.L2SkippedHighRatio.Add(1)
			return
		}
	}

	// Stamp internal fields so eviction does not need to re-parse.
	e.tag = L2EntryV1Tag
	e.SizeBytes = len(e.Refiltered)
	e.StoredAt = time.Now().UnixNano()
	if e.gvrName == "" {
		e.gvrName = gvrNameFromL1Key(e.l1Key)
	}

	prev, loaded := globalL2.store.LoadOrStore(key, e)
	if loaded {
		// Slot existed — replace and adjust counters. We use Store, not
		// CompareAndSwap, because LoadOrStore's loser path is the rare
		// case (singleflight already deduplicates concurrent writers).
		if pe, ok := prev.(*L2Entry); ok && pe != nil {
			globalL2.residentBytes.Add(-int64(pe.SizeBytes))
			globalL2.residentCount.Add(-1)
			// Reverse-index removal handled by registerReverseIndex below;
			// since we're about to re-add, just skip the explicit detach.
		}
		globalL2.store.Store(key, e)
	}
	globalL2.residentBytes.Add(int64(e.SizeBytes))
	globalL2.residentCount.Add(1)
	globalL2.registerReverseIndex(key, e)
	GlobalMetrics.L2Writes.Add(1)

	// Cheap, opportunistic budget enforcement: if we're over the cap
	// after this write, kick a sweep (best-effort; no goroutine fan-out).
	if globalL2.residentBytes.Load() > l2MaxBytes() ||
		int(globalL2.residentCount.Load()) > l2MaxEntries() {
		globalL2.sweepLocked(time.Now().UnixNano())
	}
}

// EvictL2ForL1Keys removes every L2 entry whose l1Key is in the input
// set. Called from the watcher's DELETE-event path immediately after
// L1 deletion. Cluster-restart-DELETE storms (10K+ events) are
// debounced upstream by triggerL1RefreshBatch; this function does NOT
// add additional batching — the upstream batch shape is the right
// granularity.
//
// O(K) where K is the number of L2 entries pinned to those L1 keys
// (typically ≤ 50 per L1 key — one per active binding identity ×
// groups cohort).
func EvictL2ForL1Keys(_ context.Context, l1Keys []string) int {
	if !L2Enabled() || len(l1Keys) == 0 {
		return 0
	}
	total := 0
	for _, k := range l1Keys {
		v, ok := globalL2.byL1Key.Load(k)
		if !ok {
			continue
		}
		bucket, ok := v.(*sync.Map)
		if !ok {
			continue
		}
		bucket.Range(func(hashKey, _ any) bool {
			hk, _ := hashKey.(string)
			if e, hit := globalL2.store.Load(hk); hit {
				if pe, ok := e.(*L2Entry); ok {
					globalL2.evictKey(hk, pe)
					total++
				}
			}
			bucket.Delete(hashKey)
			return true
		})
		globalL2.byL1Key.Delete(k)
	}
	if total > 0 {
		GlobalMetrics.L2EvictionsL1Delete.Add(int64(total))
	}
	return total
}

// EvictL2ForIdentity removes every L2 entry whose binding identity
// matches `identity`. Called from purgeUserCacheData on CRB transitions
// so the FIRST request post-transition computes refilter against the
// NEW binding (G10). identity == "" is a no-op.
func EvictL2ForIdentity(_ context.Context, identity string) int {
	if !L2Enabled() || identity == "" {
		return 0
	}
	v, ok := globalL2.byIdentity.Load(identity)
	if !ok {
		return 0
	}
	bucket, ok := v.(*sync.Map)
	if !ok {
		return 0
	}
	total := 0
	bucket.Range(func(hashKey, _ any) bool {
		hk, _ := hashKey.(string)
		if e, hit := globalL2.store.Load(hk); hit {
			if pe, ok := e.(*L2Entry); ok {
				globalL2.evictKey(hk, pe)
				total++
			}
		}
		bucket.Delete(hashKey)
		return true
	})
	globalL2.byIdentity.Delete(identity)
	if total > 0 {
		GlobalMetrics.L2EvictionsIdentity.Add(int64(total))
	}
	return total
}

// EvictL2ForRESTAction removes every L2 entry whose l1Key references
// the given (gvrKey, name) tuple — one (gvrKey, name) maps to many L1
// keys (one per binding identity × namespace × pagination). Called from
// the RESTAction-CR-change path (filter or api[] mutation) since those
// changes invalidate every cohort's refilter output for that resource.
//
// Uses the byGVRName reverse index so eviction is O(K) where K is the
// number of L2 entries for that resource — typically ≤ 200 (10
// identities × 1 ns × 1 name × ~20 paginated variants).
func EvictL2ForRESTAction(_ context.Context, gvrKey, name string) int {
	if !L2Enabled() || gvrKey == "" || name == "" {
		return 0
	}
	tag := gvrKey + "/" + name
	v, ok := globalL2.byGVRName.Load(tag)
	if !ok {
		return 0
	}
	bucket, ok := v.(*sync.Map)
	if !ok {
		return 0
	}
	total := 0
	bucket.Range(func(hashKey, _ any) bool {
		hk, _ := hashKey.(string)
		if e, hit := globalL2.store.Load(hk); hit {
			if pe, ok := e.(*L2Entry); ok {
				globalL2.evictKey(hk, pe)
				total++
			}
		}
		bucket.Delete(hashKey)
		return true
	})
	globalL2.byGVRName.Delete(tag)
	if total > 0 {
		GlobalMetrics.L2EvictionsRA.Add(int64(total))
	}
	return total
}

// L2ResidentBytes returns the current resident byte count (best-effort,
// atomic). Exposed for /metrics/runtime.
func L2ResidentBytes() int64 { return globalL2.residentBytes.Load() }

// L2ResidentCount returns the current entry count.
func L2ResidentCount() int64 { return globalL2.residentCount.Load() }

// FlushL2ForTest clears the L2 cache. TEST-ONLY.
func FlushL2ForTest() {
	globalL2.store.Range(func(k, _ any) bool {
		globalL2.store.Delete(k)
		return true
	})
	globalL2.byL1Key.Range(func(k, _ any) bool {
		globalL2.byL1Key.Delete(k)
		return true
	})
	globalL2.byIdentity.Range(func(k, _ any) bool {
		globalL2.byIdentity.Delete(k)
		return true
	})
	globalL2.byGVRName.Range(func(k, _ any) bool {
		globalL2.byGVRName.Delete(k)
		return true
	})
	globalL2.residentBytes.Store(0)
	globalL2.residentCount.Store(0)
}

// ── Internal: reverse-index + sweep ───────────────────────────────────────────

// registerReverseIndex populates byL1Key, byIdentity, byGVRName so the
// EvictL2For* helpers can reach every entry in O(K). Each bucket is
// itself a sync.Map keyed by the L2 hash key — concurrent writers safe.
func (idx *l2Index) registerReverseIndex(key string, e *L2Entry) {
	if e.l1Key != "" {
		bucket, _ := idx.byL1Key.LoadOrStore(e.l1Key, &sync.Map{})
		bucket.(*sync.Map).Store(key, struct{}{})
	}
	if e.identity != "" {
		bucket, _ := idx.byIdentity.LoadOrStore(e.identity, &sync.Map{})
		bucket.(*sync.Map).Store(key, struct{}{})
	}
	if e.gvrName != "" {
		bucket, _ := idx.byGVRName.LoadOrStore(e.gvrName, &sync.Map{})
		bucket.(*sync.Map).Store(key, struct{}{})
	}
}

// evictKey removes a single entry from the store and adjusts counters.
// The reverse-index entries are NOT removed eagerly here — the caller
// (which iterates the reverse index) is responsible. Background sweep
// uses store.Delete which leaves dangling reverse-index pointers; those
// resolve to a store-miss on the next eviction-by-reverse-index call
// and are removed there. Net effect: bounded staleness, zero
// correctness risk.
func (idx *l2Index) evictKey(key string, e *L2Entry) {
	if e == nil {
		return
	}
	idx.store.Delete(key)
	idx.residentBytes.Add(-int64(e.SizeBytes))
	idx.residentCount.Add(-1)
	GlobalMetrics.L2EvictionsTotal.Add(1)
}

// sweepLocked is the LRU sweep. Called inline when SetL2Refilter pushes
// the cache over its caps; also called periodically from
// StartL2EvictionLoop (set up by main.go alongside MemCache.StartEviction).
//
// Strategy: collect all entries' StoredAt + key, sort ascending, evict
// from the head until residentBytes ≤ 0.9 × cap (10% slack so we don't
// thrash on repeated writes). For 100K entries this is ~5 ms — well
// under the 30s tick we run on.
func (idx *l2Index) sweepLocked(_ int64) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	maxBytes := l2MaxBytes()
	maxEntries := l2MaxEntries()
	curBytes := idx.residentBytes.Load()
	curCount := int(idx.residentCount.Load())

	// Fast path: under both caps — nothing to do.
	if curBytes <= maxBytes && curCount <= maxEntries {
		return
	}

	type item struct {
		key string
		ts  int64
		sz  int
	}
	items := make([]item, 0, curCount)
	idx.store.Range(func(k, v any) bool {
		ks, _ := k.(string)
		e, ok := v.(*L2Entry)
		if !ok || e == nil {
			return true
		}
		items = append(items, item{key: ks, ts: e.StoredAt, sz: e.SizeBytes})
		return true
	})
	sort.Slice(items, func(i, j int) bool { return items[i].ts < items[j].ts })

	targetBytes := int64(float64(maxBytes) * 0.9)
	targetCount := int(float64(maxEntries) * 0.9)

	for _, it := range items {
		if idx.residentBytes.Load() <= targetBytes && int(idx.residentCount.Load()) <= targetCount {
			break
		}
		v, ok := idx.store.Load(it.key)
		if !ok {
			continue
		}
		e, _ := v.(*L2Entry)
		if e == nil {
			continue
		}
		idx.evictKey(it.key, e)
		// Reverse-index entries become dangling; cleaned lazily on next
		// targeted eviction (see comment in evictKey).
	}
}

// StartL2EvictionLoop launches a background goroutine that runs the LRU
// sweep every 30s (matches MemCache.StartEviction cadence). Cancellable
// via ctx. Called once at startup from main.go after the L2 enabled
// flag is honoured.
func StartL2EvictionLoop(ctx context.Context) {
	if !L2Enabled() {
		return
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				globalL2.sweepLocked(time.Now().UnixNano())
			}
		}
	}()
}

// ── Convenience setter: build entry from refilter output + reverse-index
//    fields, then write. Used by dispatcher + apiref so call sites stay
//    one-liners.

// L2Put builds an L2Entry from the refilter output and stores it. Returns
// the entry (so the apiref consumer's caller can synchronously read the
// status without a follow-up Get on writes that survived the gate).
//
// `refiltered` is the wire payload; `status` is the pre-decoded status
// map (apiref pre-decode). Pass nil for status if the dispatcher path
// computed only the bytes — apiref will lazily decode on its first L2
// hit and re-Set with status populated (next-write upgrades the entry
// in place).
//
// `rawL1Size` is the size of the L1 wrapper bytes (used by the reduction
// gate). Pass 0 to bypass the gate (e.g. on prewarm).
func L2Put(l1Key, identity, groupsHash string, refiltered []byte, status map[string]any, hasUAF bool, schemaVer string, rawL1Size int) {
	if !L2Enabled() {
		return
	}
	hk := L2Key(l1Key, identity, groupsHash)
	if hk == "" || len(refiltered) == 0 {
		return
	}
	e := &L2Entry{
		Refiltered: append([]byte(nil), refiltered...), // defensive copy
		Status:     status,
		HasUAF:     hasUAF,
		SchemaVer:  schemaVer,
		l1Key:      l1Key,
		identity:   identity,
	}
	SetL2Refilter(hk, e, rawL1Size)
}

// L2Get is the convenience reader. Mirrors GetL2Refilter but accepts
// the (l1Key, identity, groupsHash) tuple so call sites avoid threading
// the hash key through their own machinery.
func L2Get(l1Key, identity, groupsHash string) (*L2Entry, bool) {
	hk := L2Key(l1Key, identity, groupsHash)
	if hk == "" {
		return nil, false
	}
	return GetL2Refilter(hk)
}
