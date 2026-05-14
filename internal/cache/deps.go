// deps.go — Tag 0.30.8: dependency-tracking layer for the L1 resolved-output cache.
//
// Per implementation plan §"Tag 0.30.8 — What's implemented" and binding
// memory rule feedback_l1_invalidation_delete_only.md:
//
//   - Records which L1 keys depend on which (gvr, namespace, name) tuples
//     (exact-object) and which (gvr, namespace, "*") tuples (list-scope).
//   - Four-bucket lookup on watcher events:
//       1. exact:        (gvr, ns,   name)
//       2. ns-list:      (gvr, ns,   "*")
//       3. cluster-name: (gvr, "",   name)
//       4. cluster-list: (gvr, "",   "*")
//     Union of dependent L1 keys is the action target.
//   - DELETE events evict each dependent L1 key from the resolved-output
//     cache (definite invalidation; the underlying object is gone).
//     DELETE is the ONLY authorised eviction trigger per the binding rule.
//   - UPDATE/PATCH events enqueue each dependent L1 key into the refresher
//     queue (stale-while-revalidate). NEVER evicts.
//   - ADD events are deliberately a no-op for the dep tracker. Pre-flight
//     falsifier on 0.30.7 (probe.log 2026-05-13) showed first nav after
//     namespace ADD already converges to 16/16 calls within 3 s — no
//     ADD-handler scope at this tag.
//
// Bounded: a single int cap (DEPS_MAX_RECORDS, default 1 000 000 forward
// records). Reaching the cap causes new Record calls to be silently
// dropped (cache stays correct via the time-to-live outer net) and the
// summary log emits a one-shot WARN. The cap is intentionally
// conservative — at ~100k L1 entries × ~10 inner-call edges each, the
// expected steady state sits at ~1M records.
//
// Concurrency: forward + reverse indexes are both sync.Map. Per-bucket
// L1-key sets are also sync.Map[l1Key]struct{}. No global mutex — every
// hot path is lock-free. Cleanup (RemoveL1Key) holds no global lock; it
// walks the reverse index for the dropped key and deletes from each
// referenced forward bucket independently.
//
// Why sync.Map (not map+mutex):
//   - hot path is "many readers (OnDelete/OnUpdate) + many writers
//     (Record on every resolve)" with disjoint keys. sync.Map's
//     space-time tradeoff fits this exactly.
//   - cleanup is rare (LRU evict / DELETE) and serial within an L1 key,
//     so the cost of sync.Map.Range is paid only at cleanup time.

package cache

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ctxKeyL1RecordType is the typed empty-struct context key used by
// WithL1KeyContext / L1KeyFromContext. Distinct unexported type so
// external packages cannot collide via raw string keys.
type ctxKeyL1RecordType struct{}

var ctxKeyL1Record = ctxKeyL1RecordType{}

// WithL1KeyContext returns a child context that carries l1Key as the
// resolved-output cache entry currently being populated. The resolver
// reads this via L1KeyFromContext during inner-call dispatch and records
// dep edges so DELETE events on the touched (gvr, ns, name) tuples evict
// the entry from L1.
//
// Empty l1Key is treated as "do not record" — the parent context is
// returned unchanged (saves an allocation and keeps the no-record
// invariant explicit at the call site).
//
// Per plan §0.30.94 / Revision 19 "Resolver-side dep recording threaded
// via context.Context". Threading via context.Value avoids adding a
// *RecordingDeps parameter to every signature in the resolver call
// chain (api.Resolve → restactions.Resolve → httpcall.Do).
func WithL1KeyContext(ctx context.Context, l1Key string) context.Context {
	if ctx == nil || l1Key == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyL1Record, l1Key)
}

// L1KeyFromContext returns the L1 key attached to ctx by
// WithL1KeyContext. Returns "" when no key was attached (the resolver
// must treat empty as "do not record").
func L1KeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ctxKeyL1Record).(string)
	return v
}

// Dependency env knobs.
const (
	envDepsMaxRecords = "DEPS_MAX_RECORDS"

	defaultDepsMaxRecords int64 = 1_000_000

	// listWildcard is the sentinel Name value indicating list-scope.
	// Picked as "*" to mirror the plan's prose ("name=*"). Real K8s
	// object names cannot contain "*" (validated by apiserver), so
	// there is no namespace collision.
	listWildcard = "*"
)

// DepKey identifies a (gvr, namespace, name) tuple in the dependency map.
// Name == "*" indicates the bucket is a list-scope dependency rather
// than an exact-object dependency.
type DepKey struct {
	GVR       schema.GroupVersionResource
	Namespace string
	Name      string
}

// keySet is the L1-key set stored under a forward DepKey bucket. A
// sync.Map plus an atomic counter (so we can prune empty buckets
// without scanning) keeps the cleanup path lock-free.
type keySet struct {
	keys  sync.Map // map[string]struct{}  (l1Key -> {})
	count atomic.Int64
}

// depSet is the DepKey set stored under a reverse l1Key index entry.
// Same shape as keySet — sync.Map + count.
type depSet struct {
	deps  sync.Map // map[DepKey]struct{}
	count atomic.Int64
}

// DepTracker is the package-private dependency map. The exported entry
// point is the package-level singleton accessed via Deps(); production
// code MUST NOT instantiate DepTracker directly so the eviction +
// refresher hooks share a single state.
type DepTracker struct {
	// forward: DepKey -> *keySet
	forward sync.Map
	// reverse: l1Key -> *depSet
	reverse sync.Map

	// totalRecords is the global record count — bounded by maxRecords.
	totalRecords atomic.Int64
	maxRecords   int64

	// Falsifier counters (atomic; safe to read without holding anything).
	recordTotal       atomic.Uint64
	recordDroppedCap  atomic.Uint64
	evictDeleteTotal  atomic.Uint64 // L1 evictions triggered by OnDelete
	enqueueUpdateTotal atomic.Uint64 // refresh enqueues triggered by OnUpdate
	removeL1Total     atomic.Uint64 // RemoveL1Key calls (LRU + DELETE cleanup)

	// One-shot WARN flag for cap reached. We only want to log once
	// (process lifetime) so the log file doesn't fill with the same
	// line every record under steady-state pressure.
	capWarned atomic.Bool

	// store is the L1 resolved-output cache instance OnDelete evicts
	// from. Wired by SetDepTrackerStore; nil-safe (lookups still
	// record, but OnDelete becomes a no-op until the store is wired,
	// which is fine for unit tests that exercise the dep tracker
	// alone).
	storeMu sync.RWMutex
	store   *ResolvedCacheStore

	// enqueueFn is the refresher hook OnUpdate calls. Wired by
	// SetRefreshHook; nil-safe (OnUpdate becomes a no-op).
	enqueueMu sync.RWMutex
	enqueueFn func(l1Key string)
}

// depsInstance is the singleton — lazily initialised on first call to
// Deps(). The ResolvedCache singleton wiring (resolved.go) installs the
// cache store as soon as it constructs the cache; the refresher startup
// installs the enqueue hook.
var (
	depsInstance *DepTracker
	depsOnce     sync.Once
)

// Deps returns the process-wide dependency tracker, lazily initialising
// it on first use. Always non-nil — the tracker is cheap to allocate
// even when L1 is disabled (it just never sees Record calls).
func Deps() *DepTracker {
	depsOnce.Do(func() {
		depsInstance = newDepTracker(
			int64FromEnv(envDepsMaxRecords, defaultDepsMaxRecords),
		)
	})
	return depsInstance
}

func newDepTracker(maxRecords int64) *DepTracker {
	if maxRecords <= 0 {
		maxRecords = defaultDepsMaxRecords
	}
	return &DepTracker{
		maxRecords: maxRecords,
	}
}

// SetStore wires the L1 resolved-output cache the tracker evicts from
// on DELETE events. Safe to call multiple times; later calls replace
// the earlier wiring (used by tests).
//
// Production wiring lives in ResolvedCache(): once the singleton is
// built, it calls Deps().SetStore(self). The cache then knows to call
// Deps().RemoveL1Key when LRU eviction drops an entry, so dep records
// don't outlive their L1 entry.
func (d *DepTracker) SetStore(s *ResolvedCacheStore) {
	d.storeMu.Lock()
	d.store = s
	d.storeMu.Unlock()
}

// SetRefreshHook wires the refresher enqueue function. Safe to call
// multiple times; later calls replace the earlier wiring.
//
// The hook is called with an L1 key string for each dependent entry
// matched by OnUpdate. The refresher is responsible for dedup, ordering,
// and the actual re-resolve.
func (d *DepTracker) SetRefreshHook(fn func(l1Key string)) {
	d.enqueueMu.Lock()
	d.enqueueFn = fn
	d.enqueueMu.Unlock()
}

// Record stores an exact-object dependency edge: l1Key depends on
// (gvr, namespace, name). Idempotent: repeated calls with the same
// arguments are no-ops after the first. Sub-microsecond hot-path cost
// (two sync.Map.LoadOrStore + two atomic.Add).
//
// When the global record cap is reached, the call is silently dropped
// (counter `record_dropped_cap_total` increments). The first cap-hit
// also emits a one-shot WARN log line.
func (d *DepTracker) Record(l1Key string, gvr schema.GroupVersionResource, namespace, name string) {
	if d == nil || l1Key == "" {
		return
	}
	if name == "" {
		// Empty name + non-empty namespace is meaningless — guard
		// against accidental "ns-only" records. Callers wanting
		// list-scope must use RecordList explicitly.
		return
	}
	d.recordInternal(l1Key, DepKey{GVR: gvr, Namespace: namespace, Name: name})
}

// RecordList stores a list-scope dependency edge: l1Key depends on
// every object of (gvr) in namespace (or cluster-wide when namespace is
// ""). Internally encodes the bucket as (gvr, namespace, "*").
func (d *DepTracker) RecordList(l1Key string, gvr schema.GroupVersionResource, namespace string) {
	if d == nil || l1Key == "" {
		return
	}
	d.recordInternal(l1Key, DepKey{GVR: gvr, Namespace: namespace, Name: listWildcard})
}

// recordInternal is the shared body of Record + RecordList. Idempotent;
// honours the global cap.
func (d *DepTracker) recordInternal(l1Key string, dk DepKey) {
	// Forward: DepKey -> *keySet[l1Key]
	ksI, _ := d.forward.LoadOrStore(dk, &keySet{})
	ks := ksI.(*keySet)
	if _, loaded := ks.keys.LoadOrStore(l1Key, struct{}{}); loaded {
		return // already recorded — idempotent no-op
	}
	// At this point we are committing a NEW edge. Bound-check first.
	if d.totalRecords.Load() >= d.maxRecords {
		// Cap reached — roll back the LoadOrStore on the forward side.
		// In the rare race where the cap moves between the load and the
		// add, we accept the off-by-one (worst case 1 extra record).
		ks.keys.Delete(l1Key)
		d.recordDroppedCap.Add(1)
		if d.capWarned.CompareAndSwap(false, true) {
			slog.Warn("deps.record.cap_reached",
				slog.String("subsystem", "cache"),
				slog.Int64("max_records", d.maxRecords),
				slog.String("hint", "subsequent records will be dropped silently — TTL purge keeps cache correct"),
			)
		}
		return
	}
	ks.count.Add(1)
	d.totalRecords.Add(1)
	d.recordTotal.Add(1)

	// Reverse: l1Key -> *depSet[DepKey]
	dsI, _ := d.reverse.LoadOrStore(l1Key, &depSet{})
	ds := dsI.(*depSet)
	if _, loaded := ds.deps.LoadOrStore(dk, struct{}{}); !loaded {
		ds.count.Add(1)
	}
}

// OnDelete is invoked by the watcher when an informer DELETE event
// arrives for (gvr, namespace, name). It evicts every L1 cache key that
// depends on this tuple OR on any of the three other bucket forms
// (ns-list, cluster-name, cluster-list).
//
// Returns the number of L1 keys evicted. >0 under DELETE churn is the
// falsifier signal per the plan.
//
// Per feedback_l1_invalidation_delete_only.md, DELETE is the ONLY
// authorised eviction trigger.
func (d *DepTracker) OnDelete(gvr schema.GroupVersionResource, namespace, name string) int {
	if d == nil {
		return 0
	}
	matched := d.collectMatches(gvr, namespace, name)
	if len(matched) == 0 {
		return 0
	}
	d.storeMu.RLock()
	store := d.store
	d.storeMu.RUnlock()

	evicted := 0
	for l1Key := range matched {
		if store != nil {
			if store.deleteForDep(l1Key) {
				evicted++
			}
		}
		d.RemoveL1Key(l1Key) // clear forward + reverse records
	}
	if evicted > 0 {
		d.evictDeleteTotal.Add(uint64(evicted))
	}
	slog.Info("cache_event.consumed",
		slog.String("subsystem", "cache"),
		slog.String("type", "DELETE"),
		slog.String("gvr", gvr.String()),
		slog.String("ns", namespace),
		slog.String("name", name),
		slog.String("action", "evict"),
		slog.Int("l1_keys", len(matched)),
		slog.Int("evicted", evicted),
	)
	return evicted
}

// OnUpdate is invoked by the watcher when an informer UPDATE/PATCH
// event arrives for (gvr, namespace, name). It enqueues every dependent
// L1 key into the refresher (stale-while-revalidate). Returns the
// number of L1 keys enqueued. NEVER evicts.
//
// Per feedback_l1_invalidation_delete_only.md, UPDATE/PATCH use
// stale-while-revalidate via the refresher; eviction would violate the
// rule.
func (d *DepTracker) OnUpdate(gvr schema.GroupVersionResource, namespace, name string) int {
	if d == nil {
		return 0
	}
	matched := d.collectMatches(gvr, namespace, name)
	if len(matched) == 0 {
		return 0
	}
	d.enqueueMu.RLock()
	enqueue := d.enqueueFn
	d.enqueueMu.RUnlock()

	enqueued := 0
	for l1Key := range matched {
		if enqueue != nil {
			enqueue(l1Key)
		}
		enqueued++
	}
	if enqueued > 0 {
		d.enqueueUpdateTotal.Add(uint64(enqueued))
	}
	slog.Info("cache_event.consumed",
		slog.String("subsystem", "cache"),
		slog.String("type", "UPDATE"),
		slog.String("gvr", gvr.String()),
		slog.String("ns", namespace),
		slog.String("name", name),
		slog.String("action", "refresh"),
		slog.Int("l1_keys", enqueued),
	)
	return enqueued
}

// collectMatches returns the union of dependent L1 keys across the
// four bucket forms.
func (d *DepTracker) collectMatches(gvr schema.GroupVersionResource, namespace, name string) map[string]struct{} {
	out := map[string]struct{}{}
	addAll := func(dk DepKey) {
		ksI, ok := d.forward.Load(dk)
		if !ok {
			return
		}
		ks := ksI.(*keySet)
		ks.keys.Range(func(k, _ any) bool {
			out[k.(string)] = struct{}{}
			return true
		})
	}
	addAll(DepKey{GVR: gvr, Namespace: namespace, Name: name})
	addAll(DepKey{GVR: gvr, Namespace: namespace, Name: listWildcard})
	if namespace != "" {
		addAll(DepKey{GVR: gvr, Namespace: "", Name: name})
		addAll(DepKey{GVR: gvr, Namespace: "", Name: listWildcard})
	}
	return out
}

// RemoveL1Key drops every dep record associated with l1Key. Invoked by
// the L1 store's LRU eviction (and TTL eviction, and DELETE-driven
// eviction inside OnDelete) so dep records don't outlive their L1
// entry.
//
// Cheap: O(deps-of-this-key) sync.Map.Delete operations. No global
// lock.
func (d *DepTracker) RemoveL1Key(l1Key string) {
	if d == nil || l1Key == "" {
		return
	}
	dsI, ok := d.reverse.LoadAndDelete(l1Key)
	if !ok {
		return
	}
	ds := dsI.(*depSet)
	ds.deps.Range(func(k, _ any) bool {
		dk := k.(DepKey)
		if ksI, ok := d.forward.Load(dk); ok {
			ks := ksI.(*keySet)
			if _, hit := ks.keys.LoadAndDelete(l1Key); hit {
				newCount := ks.count.Add(-1)
				d.totalRecords.Add(-1)
				// Prune empty bucket — keeps the forward map from
				// growing unboundedly under churn. The check-then-
				// delete race is benign: a concurrent Record that
				// hits the deleted bucket simply LoadOrStores a
				// fresh keySet.
				if newCount == 0 {
					d.forward.CompareAndDelete(dk, ks)
				}
			}
		}
		return true
	})
	d.removeL1Total.Add(1)
}

// DepStats is a snapshot of the falsifier counters. All numbers are
// atomic and may drift by a single call between fields.
type DepStats struct {
	TotalRecords      int64
	MaxRecords        int64
	RecordTotal       uint64
	RecordDroppedCap  uint64
	EvictDeleteTotal  uint64
	EnqueueUpdateTotal uint64
	RemoveL1Total     uint64
}

func (d *DepTracker) Stats() DepStats {
	if d == nil {
		return DepStats{}
	}
	return DepStats{
		TotalRecords:       d.totalRecords.Load(),
		MaxRecords:         d.maxRecords,
		RecordTotal:        d.recordTotal.Load(),
		RecordDroppedCap:   d.recordDroppedCap.Load(),
		EvictDeleteTotal:   d.evictDeleteTotal.Load(),
		EnqueueUpdateTotal: d.enqueueUpdateTotal.Load(),
		RemoveL1Total:      d.removeL1Total.Load(),
	}
}

// resetDepsForTest tears the singleton down so each test sees a clean
// tracker. Exported only via the *_test.go shim — production code MUST
// NOT call this.
func resetDepsForTest() {
	depsInstance = nil
	depsOnce = sync.Once{}
}

// ResetDepsForTest is the exported variant that lives outside _test.go
// so external packages (e.g., internal/handlers/dispatchers tests) can
// reset the singleton between cases. Production code MUST NOT call
// this; build tags would be cleaner but Go's module layout makes
// cross-package test helpers via _test.go awkward.
func ResetDepsForTest() {
	resetDepsForTest()
}

// CollectMatchesForTest exposes the package-private collectMatches for
// cross-package tests. Returns the union of dependent L1 keys across
// the four bucket forms. Production code MUST NOT call this.
func (d *DepTracker) CollectMatchesForTest(gvr schema.GroupVersionResource, namespace, name string) map[string]struct{} {
	if d == nil {
		return nil
	}
	return d.collectMatches(gvr, namespace, name)
}

// envInt64 is a typed helper that re-uses int64FromEnv from resolved.go.
// Kept here as a thin wrapper purely for readability of the constants
// block above.
var _ = strconv.ParseInt // touched by int64FromEnv via resolved.go
var _ = os.Getenv        // same — int parsing lives in resolved.go
