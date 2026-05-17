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
// O15 (0.30.110): an empty l1Key is also a loud-fail signal. A caller
// reaching WithL1KeyContext with "" usually means the L1 key was never
// threaded through — a silent dep-recording regression. In production
// this WARNs and bumps recordDroppedNoKey; in test mode it panics so the
// regression cannot ship. The parent context is still returned unchanged
// either way so the no-record invariant downstream is preserved.
//
// Per plan §0.30.94 / Revision 19 "Resolver-side dep recording threaded
// via context.Context". Threading via context.Value avoids adding a
// *RecordingDeps parameter to every signature in the resolver call
// chain (api.Resolve → restactions.Resolve → httpcall.Do).
func WithL1KeyContext(ctx context.Context, l1Key string) context.Context {
	if l1Key == "" {
		loudFailEmptyL1Key("WithL1KeyContext")
		return ctx
	}
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyL1Record, l1Key)
}

// loudFailEmptyL1Key implements the O15 empty-l1Key contract: panic in
// test mode (so a missing-key regression cannot ship), WARN + counter in
// production (the TTL outer net keeps the cache correct). The counter
// lives on the Deps() singleton; Deps() is always non-nil.
func loudFailEmptyL1Key(callsite string) {
	Deps().recordDroppedNoKey.Add(1)
	if depsTestMode.Load() {
		panic("cache.deps: " + callsite + " called with empty l1Key — " +
			"the L1 key was not threaded through (O15 loud-fail, test mode)")
	}
	slog.Warn("deps.empty_l1_key",
		slog.String("subsystem", "cache"),
		slog.String("callsite", callsite),
		slog.String("hint", "dep edge dropped — L1 key was not threaded through; "+
			"TTL purge keeps cache correct but stale-while-revalidate is degraded for this entry"),
	)
}

// SetDepsTestMode flips the O15 test-mode toggle. Exported for the
// cross-package _test.go shim; production code MUST NOT call it. When
// true, an empty-l1Key Record/RecordList/WithL1KeyContext panics instead
// of WARNing. The toggle is a Go variable, never an env var, so a
// customer deployment can never enable process-killing behaviour.
func SetDepsTestMode(on bool) {
	depsTestMode.Store(on)
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

// ctxKeyRefreshBypassType is the typed empty-struct context key for the
// Ship 0.30.118 refresh-bypass marker. Distinct unexported type — same
// collision-safety as ctxKeyL1RecordType.
type ctxKeyRefreshBypassType struct{}

var ctxKeyRefreshBypass = ctxKeyRefreshBypassType{}

// WithRefreshBypass returns a child context marked as a REFRESH-driven
// re-resolve (Ship 0.30.118 — the api-stage self-hit fix). The refresher
// sets it on the re-resolve context for an apistage entry; the api-stage
// resolver consults it via RefreshBypassFromContext.
//
// THE INVARIANT it restores: the resolve that feeds a refresh must not
// consult the entry being refreshed. The apistage refresh routes through
// the whole-RESTAction stage loop, whose per-stage Get(stageKey) reads
// the resolved-output L1 — so without this marker the refresh re-resolve
// self-hits the dirty-but-resident entry it is refreshing, skips the K8s
// call, and never re-Puts. With the marker the stage loop SKIPS the Get
// for exactly the stage key being refreshed (L1KeyFromContext) — that
// stage recomputes from K8s and the key-swap Put writes the fresh value.
// Sibling stages still Get-hit normally (they are not being refreshed).
//
// Mirrors WithL1KeyContext: a context.Value flag, no parameter threaded
// through the resolver call chain. A nil ctx is returned unchanged.
func WithRefreshBypass(ctx context.Context) context.Context {
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyRefreshBypass, true)
}

// RefreshBypassFromContext reports whether ctx was marked by
// WithRefreshBypass — i.e. whether this resolve is a refresh-driven
// re-resolve. The api-stage stage loop combines this with a stage-key
// equality check (stageKey == L1KeyFromContext(ctx)) so the bypass
// applies ONLY to the exact stage entry being refreshed.
func RefreshBypassFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(ctxKeyRefreshBypass).(bool)
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

// depsTestMode, when true, makes the empty-l1Key loud-fail (O15) panic
// instead of WARN+counter. It is a package-private Go variable flipped
// ONLY by the *_test.go shim SetDepsTestMode — never read from an env
// var, so a customer can never accidentally turn process-killing
// behaviour on in production. Default false (production semantics).
var depsTestMode atomic.Bool

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
	recordTotal        atomic.Uint64
	recordDroppedCap   atomic.Uint64
	recordDroppedNoKey atomic.Uint64 // O15: Record*/WithL1KeyContext with empty l1Key
	evictDeleteTotal   atomic.Uint64 // L1 self-representation evictions (OnDelete)
	dirtyMarkTotal     atomic.Uint64 // dirty-marks (OnAdd/OnUpdate + OnDelete non-self)
	enqueueUpdateTotal atomic.Uint64 // refresh enqueues triggered by OnUpdate
	removeL1Total      atomic.Uint64 // RemoveL1Key calls (LRU + DELETE cleanup)

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
	if d == nil {
		return
	}
	if l1Key == "" {
		// O15: a Record call with no L1 key is an unambiguous bug — a
		// DepKey with nowhere to attach it. Loud-fail.
		loudFailEmptyL1Key("Record")
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
	if d == nil {
		return
	}
	if l1Key == "" {
		// O15: same loud-fail as Record — a list-scope edge with no L1
		// key cannot be attached anywhere.
		loudFailEmptyL1Key("RecordList")
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

// OnAdd is invoked by the watcher when an informer ADD event arrives
// for (gvr, namespace, name) — gated post-sync by the watcher's
// AddFunc (initial-replay ADDs are dropped before they reach here).
//
// R1 (0.30.110): ADD == UPDATE. A freshly-created object can satisfy a
// LIST-scope dependency that resolved to an empty/partial result before
// the object existed; the dependent L1 entry is now stale and must be
// dirty-marked (enqueued into the refresher). ADD NEVER evicts —
// per feedback_l1_invalidation_delete_only.md eviction is DELETE-only.
//
// Returns the number of dependent L1 keys dirty-marked.
func (d *DepTracker) OnAdd(gvr schema.GroupVersionResource, namespace, name string) int {
	return d.onChange("ADD", gvr, namespace, name)
}

// OnUpdate is invoked by the watcher when an informer UPDATE/PATCH
// event arrives for (gvr, namespace, name). It dirty-marks every
// dependent L1 key into the refresher (stale-while-revalidate). Returns
// the number of L1 keys dirty-marked. NEVER evicts.
//
// Per feedback_l1_invalidation_delete_only.md, UPDATE/PATCH use
// stale-while-revalidate via the refresher; eviction would violate the
// rule.
func (d *DepTracker) OnUpdate(gvr schema.GroupVersionResource, namespace, name string) int {
	return d.onChange("UPDATE", gvr, namespace, name)
}

// onChange is the shared body of OnAdd + OnUpdate (R1: ADD == UPDATE).
// It dirty-marks every dependent L1 key — both exact-object deps and
// LIST-scope deps — by enqueuing them into the refresher. It NEVER
// evicts.
func (d *DepTracker) onChange(eventType string, gvr schema.GroupVersionResource, namespace, name string) int {
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

	marked := 0
	for l1Key := range matched {
		if enqueue != nil {
			enqueue(l1Key)
		}
		marked++
	}
	if marked > 0 {
		d.dirtyMarkTotal.Add(uint64(marked))
		// enqueueUpdateTotal is retained as the pre-0.30.110 falsifier
		// name the resolved_cache.summary log + existing tests read.
		d.enqueueUpdateTotal.Add(uint64(marked))
	}
	slog.Info("cache_event.consumed",
		slog.String("subsystem", "cache"),
		slog.String("type", eventType),
		slog.String("gvr", gvr.String()),
		slog.String("ns", namespace),
		slog.String("name", name),
		slog.String("action", "refresh"),
		slog.Int("l1_keys", marked),
	)
	return marked
}

// OnDelete is invoked by the watcher when an informer DELETE event
// arrives for (gvr, namespace, name).
//
// R2/R7 (0.30.110) — three-way classification. Every matched L1 entry
// falls into exactly one of three buckets:
//
//   1. self-representation — the entry's OWN dispatched object is the
//      deleted object. The cached output is the resolution of an object
//      that no longer exists → EVICT (the only authorised eviction
//      trigger, per feedback_l1_invalidation_delete_only.md).
//
//   2. LIST-dep — the entry matched via the (gvr, ns, "*") wildcard
//      bucket. The entry's own object still exists; one member of a list
//      it depends on went away → DIRTY-MARK (stale-while-revalidate).
//
//   3. dependent-GET-dep — the entry matched via an exact (gvr, ns,
//      name) bucket but its own object is a DIFFERENT object (e.g. a
//      widget that GET-depends on a deleted RESTAction) → DIRTY-MARK.
//
// Returns the number of L1 keys EVICTED (self-representations only).
// dirtyMarkTotal counts buckets 2 + 3.
//
// OnDelete itself is synchronous — classification + eviction both run on
// the calling goroutine. R3 (the "never on the informer processor
// goroutine" requirement) is satisfied at the watcher boundary: the
// informer DeleteFunc hands OnDelete to the deps eviction worker (see
// watcher.go submitDeleteEvent), so the eviction burst never blocks
// event delivery. Unit tests call OnDelete directly and observe a
// deterministic synchronous result.
func (d *DepTracker) OnDelete(gvr schema.GroupVersionResource, namespace, name string) int {
	if d == nil {
		return 0
	}
	matched := d.collectMatchesWithDep(gvr, namespace, name)
	if len(matched) == 0 {
		return 0
	}
	d.storeMu.RLock()
	store := d.store
	d.storeMu.RUnlock()
	d.enqueueMu.RLock()
	enqueue := d.enqueueFn
	d.enqueueMu.RUnlock()

	deleted := DepKey{GVR: gvr, Namespace: namespace, Name: name}

	// matched is map[l1Key]DepKey. The DepKey (LIST wildcard vs exact)
	// distinguishes bucket 2 from bucket 3, but the ACTION for both
	// non-self buckets is identical — dirty-mark — so OnDelete only needs
	// the self-vs-non-self split, computed from the entry's own Inputs.
	var toEvict []string
	dirtyMarked := 0
	for l1Key := range matched {
		if d.isSelfRepresentation(store, l1Key, deleted) {
			// Bucket 1: self-representation → evict.
			toEvict = append(toEvict, l1Key)
			continue
		}
		// Bucket 2 (LIST-dep) or bucket 3 (dependent-GET-dep) → dirty-mark.
		if enqueue != nil {
			enqueue(l1Key)
		}
		dirtyMarked++
	}
	if dirtyMarked > 0 {
		d.dirtyMarkTotal.Add(uint64(dirtyMarked))
	}

	evictCount := len(toEvict)
	if evictCount > 0 {
		d.runEvictionBatch(toEvict)
	}

	slog.Info("cache_event.consumed",
		slog.String("subsystem", "cache"),
		slog.String("type", "DELETE"),
		slog.String("gvr", gvr.String()),
		slog.String("ns", namespace),
		slog.String("name", name),
		slog.String("action", "evict+refresh"),
		slog.Int("l1_keys", len(matched)),
		slog.Int("evicted", evictCount),
		slog.Int("dirty_marked", dirtyMarked),
	)
	return evictCount
}

// isSelfRepresentation reports whether the L1 entry under l1Key is the
// resolved output of the `deleted` object itself (bucket 1). It reads
// the entry's Inputs from the store and compares (Group/Version/
// Resource, Namespace, Name).
//
// When the store is nil or the entry / its Inputs are unavailable, it
// returns false — the conservative direction: a non-self classification
// dirty-marks (stale-while-revalidate) rather than evicts. Missing an
// eviction merely leaves a stale entry until TTL; an over-eviction is
// the regression F2 catches.
func (d *DepTracker) isSelfRepresentation(store *ResolvedCacheStore, l1Key string, deleted DepKey) bool {
	if store == nil {
		return false
	}
	entry, ok := store.Get(l1Key)
	if !ok || entry == nil || entry.Inputs == nil {
		return false
	}
	in := entry.Inputs
	return in.Group == deleted.GVR.Group &&
		in.Version == deleted.GVR.Version &&
		in.Resource == deleted.GVR.Resource &&
		in.Namespace == deleted.Namespace &&
		in.Name == deleted.Name
}

// OnResourceTypeAvailable is invoked by the CRD-watch when a CRD newly
// appears at runtime (EnsureResourceType returned added==true for a
// genuinely-new GVR). D1 (Ship D, 0.30.114).
//
// A compositions-list resolve that ran BEFORE the CRD existed records a
// LIST-scope dep and caches `0 items`; once the CRD appears the cached
// result is stale-negative. This scans the forward index for every
// LIST-scope bucket matching gvr (every namespace AND the cluster-wide
// "" namespace) and dirty-marks the dependent L1 keys via the same
// refreshHook onChange uses.
//
// It deliberately ignores EXACT-object GET-dep buckets: an exact GET-dep
// for a named object that did not resolve before the CRD existed is not
// a stale-negative LIST and is left to OnAdd when the object itself
// arrives. Dirty-mark only — NEVER evicts.
//
// Returns the number of dependent L1 keys dirty-marked. AC-D4: a no-op
// (and idempotent) when no LIST-dep matches gvr.
func (d *DepTracker) OnResourceTypeAvailable(gvr schema.GroupVersionResource) int {
	if d == nil {
		return 0
	}
	matched := d.collectTypeMatches(gvr, true /* listOnly */)
	return d.dirtyMarkResourceType("CRD_ADD", gvr, matched)
}

// OnResourceTypeRemoved is invoked by the CRD-watch when a CRD is removed
// at runtime. D2 (Ship D, 0.30.114).
//
// Unlike OnDelete (a single object's DELETE), a CRD removal is a
// TYPE-removal — every L1 entry that LIST-depends on the GVR, OR
// GET-depends on any named object of the GVR, is now stale. This scans
// every forward bucket whose DepKey.GVR == gvr (LIST wildcard AND exact
// GET buckets, all namespaces) and dirty-marks the dependent L1 keys.
//
// Dirty-mark only — NEVER evicts, even a self-representation entry:
// feedback_l1_invalidation_delete_only.md authorises eviction ONLY for a
// single object's DELETE. A CRD removal mirrors OnDelete's non-self
// dependent-bucket handling (stale-while-revalidate).
//
// Returns the number of dependent L1 keys dirty-marked. AC-D4: a no-op
// (and idempotent) when no dep matches gvr.
func (d *DepTracker) OnResourceTypeRemoved(gvr schema.GroupVersionResource) int {
	if d == nil {
		return 0
	}
	matched := d.collectTypeMatches(gvr, false /* listOnly */)
	return d.dirtyMarkResourceType("CRD_DELETE", gvr, matched)
}

// dirtyMarkResourceType dirty-marks every L1 key in matched via the
// refreshHook — the shared body of OnResourceTypeAvailable +
// OnResourceTypeRemoved. NEVER evicts. Returns the number marked.
func (d *DepTracker) dirtyMarkResourceType(eventType string, gvr schema.GroupVersionResource, matched map[string]struct{}) int {
	if len(matched) == 0 {
		return 0
	}
	d.enqueueMu.RLock()
	enqueue := d.enqueueFn
	d.enqueueMu.RUnlock()

	marked := 0
	for l1Key := range matched {
		if enqueue != nil {
			enqueue(l1Key)
		}
		marked++
	}
	if marked > 0 {
		d.dirtyMarkTotal.Add(uint64(marked))
	}
	slog.Info("cache_event.consumed",
		slog.String("subsystem", "cache"),
		slog.String("type", eventType),
		slog.String("gvr", gvr.String()),
		slog.String("action", "refresh"),
		slog.Int("l1_keys", marked),
	)
	return marked
}

// collectTypeMatches scans the forward index for every bucket whose
// GVR == gvr and returns the union of dependent L1 keys. The CRD-watch
// lifecycle scan — it matches by GVR alone (every namespace), unlike
// collectMatches which point-looks-up a specific (gvr, ns, name) tuple.
//
//   - listOnly == true (D1, CRD-add): only LIST-scope buckets
//     (Name == listWildcard) — a stale-negative LIST is the only entry
//     a CRD-add can invalidate.
//   - listOnly == false (D2, CRD-delete): every bucket — LIST wildcard
//     AND exact GET — since a type-removal invalidates both.
//
// A forward-index Range is O(distinct DepKeys); CRD-add/delete is a rare
// event so the scan cost is paid only at CRD-lifecycle time, never on a
// resolver hot path.
func (d *DepTracker) collectTypeMatches(gvr schema.GroupVersionResource, listOnly bool) map[string]struct{} {
	out := map[string]struct{}{}
	d.forward.Range(func(k, v any) bool {
		dk := k.(DepKey)
		if dk.GVR != gvr {
			return true
		}
		if listOnly && dk.Name != listWildcard {
			return true
		}
		v.(*keySet).keys.Range(func(kk, _ any) bool {
			out[kk.(string)] = struct{}{}
			return true
		})
		return true
	})
	return out
}

// collectMatches returns the union of dependent L1 keys across the four
// bucket forms. Retained as the bare-set form for onChange (ADD/UPDATE),
// which dirty-marks every match uniformly and has no need for the
// matching DepKey.
func (d *DepTracker) collectMatches(gvr schema.GroupVersionResource, namespace, name string) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range d.collectMatchesWithDep(gvr, namespace, name) {
		out[k] = struct{}{}
	}
	return out
}

// collectMatchesWithDep returns the union of dependent L1 keys across
// the four bucket forms, each paired with the DepKey it matched
// through. When an L1 key matches more than one bucket, an EXACT-object
// bucket takes precedence over a LIST wildcard bucket — so OnDelete's
// classification sees the most specific dependency form.
func (d *DepTracker) collectMatchesWithDep(gvr schema.GroupVersionResource, namespace, name string) map[string]DepKey {
	out := map[string]DepKey{}
	addAll := func(dk DepKey) {
		ksI, ok := d.forward.Load(dk)
		if !ok {
			return
		}
		ks := ksI.(*keySet)
		ks.keys.Range(func(k, _ any) bool {
			l1 := k.(string)
			prev, seen := out[l1]
			// Exact (Name != listWildcard) beats wildcard; once an
			// exact match is recorded it is never downgraded.
			if !seen || (prev.Name == listWildcard && dk.Name != listWildcard) {
				out[l1] = dk
			}
			return true
		})
	}
	// Exact buckets first so they win the precedence check; wildcard
	// buckets only fill in keys not already matched exactly.
	addAll(DepKey{GVR: gvr, Namespace: namespace, Name: name})
	if namespace != "" {
		addAll(DepKey{GVR: gvr, Namespace: "", Name: name})
	}
	addAll(DepKey{GVR: gvr, Namespace: namespace, Name: listWildcard})
	if namespace != "" {
		addAll(DepKey{GVR: gvr, Namespace: "", Name: listWildcard})
	}
	return out
}

// runEvictionBatch drops each self-representation L1 key from the store
// and clears its dep records. Counts evictDeleteTotal — self-evictions
// only, per the R2/R7 counter contract.
func (d *DepTracker) runEvictionBatch(keys []string) {
	d.storeMu.RLock()
	store := d.store
	d.storeMu.RUnlock()

	evicted := 0
	for _, l1Key := range keys {
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
	TotalRecords       int64
	MaxRecords         int64
	RecordTotal        uint64
	RecordDroppedCap   uint64
	RecordDroppedNoKey uint64 // O15: empty-l1Key Record*/WithL1KeyContext
	EvictDeleteTotal   uint64 // self-representation evictions only
	DirtyMarkTotal     uint64 // dirty-marks (ADD/UPDATE + DELETE non-self)
	EnqueueUpdateTotal uint64
	RemoveL1Total      uint64
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
		RecordDroppedNoKey: d.recordDroppedNoKey.Load(),
		EvictDeleteTotal:   d.evictDeleteTotal.Load(),
		DirtyMarkTotal:     d.dirtyMarkTotal.Load(),
		EnqueueUpdateTotal: d.enqueueUpdateTotal.Load(),
		RemoveL1Total:      d.removeL1Total.Load(),
	}
}

// resetDepsForTest tears the singleton down so each test sees a clean
// tracker. Exported only via the *_test.go shim — production code MUST
// NOT call this. Also clears the O15 test-mode toggle so a test that
// forgot to reset it cannot leak panic-on-empty-key into the next test.
func resetDepsForTest() {
	depsInstance = nil
	depsOnce = sync.Once{}
	depsTestMode.Store(false)
}

// ResetDepsForTest is the exported variant that lives outside _test.go
// so external packages (e.g., internal/handlers/dispatchers tests) can
// reset the singleton between cases. Production code MUST NOT call
// this; build tags would be cleaner but Go's module layout makes
// cross-package test helpers via _test.go awkward.
//
// Also tears down the informer→DepTracker bridge (0.30.110) so a
// cross-package test cannot leak the DELETE-eviction worker goroutine
// or stale bridge counters into the next case.
func ResetDepsForTest() {
	resetDepsForTest()
	resetDepWatchForTest()
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
