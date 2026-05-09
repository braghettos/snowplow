package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/krateoplatformops/snowplow/internal/observability"
)

// clusterDepSampleEvery samples SCard once per N SAdds against cluster-wide
// dep keys. 128 chosen per design §3.2: ~40 samples/sec at 5K SAdds/sec.
const clusterDepSampleEvery = 128

type Metrics struct {
	GetHits         atomic.Int64
	GetMisses       atomic.Int64
	ListHits        atomic.Int64
	ListMisses      atomic.Int64
	RBACHits        atomic.Int64
	RBACMisses      atomic.Int64
	RawHits         atomic.Int64
	RawMisses       atomic.Int64
	L1Hits          atomic.Int64
	L1Misses        atomic.Int64
	CallHits        atomic.Int64
	CallMisses      atomic.Int64
	NegativeHits    atomic.Int64
	ExpiryRefreshes atomic.Int64

	// ── Cluster-wide dep instrumentation (Option A measurement) ────────
	// All counters are atomic.Int64; no locks. Sampled SCard runs on
	// every 128th SAdd via ClusterDepSAddSampler (no goroutine).
	ClusterDepSAddTotal            atomic.Int64
	ClusterDepSAddByResolve        atomic.Int64
	ClusterDepSAddByResolveNSList  atomic.Int64
	ClusterDepSAddByRegister       atomic.Int64
	ClusterDepSAddByRegisterNSList atomic.Int64
	ClusterDepSAddDeduped          atomic.Int64
	ClusterDepSetSizeMax           atomic.Int64
	ClusterDepSetSizeSumLast       atomic.Int64
	ClusterDepSampledKeys          atomic.Int64
	ClusterDepSMembersTotal        atomic.Int64
	ClusterDepSMembersBytes        atomic.Int64
	// ClusterDepSAddSampler is a free-running counter incremented on every
	// SAdd against a cluster-wide dep key. Modulo 128 selects the sample.
	ClusterDepSAddSampler atomic.Int64

	// ── Watch event delivery instrumentation (drift detection) ───────────
	// Counts every Add/Update/Delete that successfully reached handleEvent
	// after toUnstructured. WatchEventsDeleteTombstone counts the subset
	// of Deletes that were synthesized by the reflector's relist path
	// (k8scache.DeletedFinalStateUnknown) — i.e., the watch stream missed
	// the original Delete and the informer recovered it by re-LISTing.
	// A non-zero tombstone count is direct evidence of watch event loss.
	WatchEventsAdd             atomic.Int64
	WatchEventsUpdate          atomic.Int64
	WatchEventsDelete          atomic.Int64
	WatchEventsDeleteTombstone atomic.Int64
	// Q-OOM-FIX (Patch C, 2026-05-08) — incremented when handleEvent
	// short-circuits a no-op UPDATE because old.resourceVersion ==
	// new.resourceVersion. A growing delta vs WatchEventsUpdate is the
	// direct measure of how much wasted work the patch absorbs.
	WatchEventsNoopFiltered atomic.Int64

	// ── CRD auto-register convergence (Q-PREWARM-R5 fix) ─────────────────
	// CRDRegisterL1Evictions counts L1 resolved keys evicted because a CRD
	// for their dependency group was just auto-registered (the convergence
	// path that R5 prewarm shortened). Non-zero values mean the cache
	// self-healed L1 entries that were resolved before the CRD existed.
	CRDRegisterL1Evictions atomic.Int64

	// ── L1 byte-budget + LRU eviction (Q-L1-BUDGET, 0.25.319) ────────────
	// L1EvictionsLRU counts entries evicted by the byte/entry-count budget
	// sweep (least-recently-accessed first). L1EvictionsTTL counts entries
	// evicted by the existing TTL pass (was untracked before 0.25.319).
	L1EvictionsLRU atomic.Int64
	L1EvictionsTTL atomic.Int64
	// ── Synchronous L1 admission (0.25.327) ──────────────────────────────
	// L1SyncSweepCount bumps when a writer in kvStore trips the budget and
	// runs the LRU sweep inline. L1AsyncSweepCount bumps each time the
	// 30-s StartEviction tick runs the sweep. Together they reveal whether
	// admission control or the ticker is doing the work — pre-0.25.327 the
	// only path was the ticker, so SyncSweepCount must be 0 there.
	L1SyncSweepCount  atomic.Int64
	L1AsyncSweepCount atomic.Int64

	// ── L1 refresher singleflight coalesce (Q-REFRESH-COALESCE, 0.25.328) ─
	// RefresherInflightCoalesced increments once per FOLLOWER (not leader)
	// when concurrent refreshSingleL1 calls share the same L1-key
	// singleflight slot — i.e. the same logical key is already being
	// refreshed and the new caller piggybacks on the in-flight execution.
	// Surfaces at /metrics/runtime under cache.refresher_inflight_coalesced.
	// A non-zero counter is the falsifiable signal that the coalesce
	// window is collapsing duplicate work; zero means refresher fan-out
	// already had no duplicates to dedup.
	RefresherInflightCoalesced atomic.Int64

	// ── L2 post-refilter cache (Q-RBACC-L2-1) ────────────────────────────
	// Counters surface at /metrics/runtime under l2_* keys. Hit-rate is
	// computed by the snapshot consumer.
	L2Hits               atomic.Int64
	L2Misses             atomic.Int64
	L2Writes             atomic.Int64
	// Q-OOM-FIX (Patch F, 2026-05-08) — incremented when l1cache's
	// resolveAndCacheInner skips L2Put because the existing entry is
	// byte-equal to the new refiltered bytes. Surfaces the wasted-write
	// reduction at /metrics/runtime.
	L2WritesSkippedIdentical atomic.Int64
	L2SkippedHighRatio       atomic.Int64
	L2SkippedSizeCap         atomic.Int64
	L2EvictionsL1Delete  atomic.Int64
	L2EvictionsIdentity  atomic.Int64
	L2EvictionsRA        atomic.Int64
	L2EvictionsTotal     atomic.Int64

	// ── Causal-cost /call exit-edge counters (Q-CAUSAL-COST, 0.25.323) ──
	// Surface at /metrics/runtime under call_events. ClientGone bumps when
	// req.Context().Err() != nil at handler exit; the AfterWriteHeader
	// subset bumps when a status had already been sent. WriteError bumps
	// on any wri.Write returning a non-nil error.
	CallClientGone                 atomic.Int64
	CallClientGoneAfterWriteHeader atomic.Int64
	CallWriteError                 atomic.Int64

	// ── Widget 5xx attribution counters (Q-5XX-DIAG, 0.25.324) ──────────
	// Investigation-only instrumentation for the H1' cache-key-collision
	// hypothesis: widget L1 keys are per binding-identity (NOT per-user)
	// and rely on MarkUAFTouching() being called during apiref resolution
	// to gate the L1 write at widgets.go:263. The counters below let an
	// observer attribute 5xx widget responses to the failing CRs and
	// confirm whether `bench-app-05-{906,909,916}-composition-panel` are
	// systematically UAFTouching=false while siblings are UAFTouching=true.
	//
	// All maps are sync.Map of (string -> *atomic.Int64). Keys:
	//   WidgetResponsesByResource: "{group}/{resource}/{reload_idx}/{2xx|4xx|5xx}"
	//   WidgetErrorByClass: error class enum (rbac_forbidden, object_get_failed,
	//                       apiref_resolve_failed, marshal_failed, restaction_dispatch_failed)
	//   UAFSkipped: skip reason enum (api_error, ...)
	//   UAFTouchingByResource: "{group}/{resource}/{reload_idx}/{true|false}"
	WidgetResponsesByResource sync.Map // string -> *atomic.Int64
	WidgetErrorByClass        sync.Map // string -> *atomic.Int64
	UAFSkipped                sync.Map // string -> *atomic.Int64
	UAFTouchingByResource     sync.Map // string -> *atomic.Int64
	UAFTouchingCount          atomic.Int64
	UAFNonTouchingCount       atomic.Int64

	// ── Panel-probe counters (Q-PANEL-PROBE, 0.25.326) ─────────────────
	// Falsification gate for H_status_managed_empty: when the OUTER JQ
	// filter trips at restactions.go:118 with "unable to resolve filter",
	// the probe inspects the unfiltered dict for any value v whose
	// .status.managed shape predicts the failure. ManagedEmpty bumps
	// when an entry's .status.managed is nil/missing/empty-array;
	// ManagedPopulated bumps when it's a non-empty array (so the JQ trip
	// is NOT explained by managed-empty); PathMalformed bumps when
	// .status.managed is a non-array, non-nil value (string/object/etc).
	// All increments are observe-only — no behavior change.
	PanelProbeManagedEmpty     atomic.Int64
	PanelProbeManagedPopulated atomic.Int64
	PanelProbePathMalformed    atomic.Int64
}

// IncMapKey atomically increments the counter at key inside m, allocating
// a new *atomic.Int64 on first use. LoadOrStore guarantees only one
// counter exists per key under concurrent access. Returns the new value.
//
// Used by the Q-5XX-DIAG (0.25.324) per-string-key counters where the
// label space is open (group/resource/reload_idx/class) and a static
// struct field would not fit.
func IncMapKey(m *sync.Map, key string) int64 {
	v, _ := m.LoadOrStore(key, &atomic.Int64{})
	return v.(*atomic.Int64).Add(1)
}

// SnapshotMap copies a sync.Map of (string -> *atomic.Int64) to a plain
// map[string]int64. Safe under concurrent IncMapKey writes — each entry
// is loaded atomically. Returns nil when the map is empty so JSON output
// suppresses empty blocks (smaller /metrics/runtime payload).
func SnapshotMap(m *sync.Map) map[string]int64 {
	out := map[string]int64{}
	m.Range(func(k, v any) bool {
		ks, _ := k.(string)
		ai, _ := v.(*atomic.Int64)
		if ai != nil {
			out[ks] = ai.Load()
		}
		return true
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

// Inc atomically increments the given counter and updates the OTel metric.
func (m *Metrics) Inc(counter *atomic.Int64, field string) {
	counter.Add(1)
	observability.IncrementCacheMetric(field)
}

type MetricsSnapshot struct {
	GetHits         int64   `json:"get_hits"`
	GetMisses       int64   `json:"get_misses"`
	ListHits        int64   `json:"list_hits"`
	ListMisses      int64   `json:"list_misses"`
	RBACHits        int64   `json:"rbac_hits"`
	RBACMisses      int64   `json:"rbac_misses"`
	RawHits         int64   `json:"raw_hits"`
	RawMisses       int64   `json:"raw_misses"`
	L1Hits          int64   `json:"l1_hits"`
	L1Misses        int64   `json:"l1_misses"`
	CallHits        int64   `json:"call_hits"`
	CallMisses      int64   `json:"call_misses"`
	NegativeHits    int64   `json:"negative_hits"`
	ExpiryRefreshes int64   `json:"expiry_refreshes"`
	GetHitRate      float64 `json:"get_hit_rate"`
	ListHitRate     float64 `json:"list_hit_rate"`
	RBACHitRate     float64 `json:"rbac_hit_rate"`
	L1HitRate       float64 `json:"l1_hit_rate"`

	// Cluster-wide dep instrumentation (write-path measurement, no behavior).
	ClusterDepSAddTotal            int64   `json:"cluster_dep_sadd_total"`
	ClusterDepSAddByResolve        int64   `json:"cluster_dep_sadd_by_resolve"`
	ClusterDepSAddByResolveNSList  int64   `json:"cluster_dep_writes_per_namespace_list_resolve"`
	ClusterDepSAddByRegister       int64   `json:"cluster_dep_sadd_by_register"`
	ClusterDepSAddByRegisterNSList int64   `json:"cluster_dep_writes_per_namespace_list_register"`
	ClusterDepSAddDeduped          int64   `json:"cluster_dep_sadd_deduped"`
	ClusterDepSetSizeMax           int64   `json:"cluster_dep_set_size_max"`
	ClusterDepSetSizeAvg           float64 `json:"cluster_dep_set_size_avg"`
	ClusterDepSMembersTotal        int64   `json:"cluster_dep_smembers_total"`
	ClusterDepSMembersBytes        int64   `json:"cluster_dep_smembers_bytes"`

	// Watch event delivery instrumentation. WatchEventsDeleteTombstone is
	// the smoking-gun counter for watch-stream event loss: when non-zero,
	// the reflector recovered a Delete by relist instead of by watch.
	WatchEventsAdd             int64 `json:"watch_events_add"`
	WatchEventsUpdate          int64 `json:"watch_events_update"`
	WatchEventsDelete          int64 `json:"watch_events_delete"`
	WatchEventsDeleteTombstone int64 `json:"watch_events_delete_tombstone"`
	WatchEventsNoopFiltered    int64 `json:"watch_events_noop_filtered"`

	// CRD auto-register convergence (Q-PREWARM-R5 fix). Counts L1
	// resolved keys evicted because a CRD for their dependency group
	// was just auto-registered. See keys.go L1ResourceDepGroupKey.
	CRDRegisterL1Evictions int64 `json:"crd_register_l1_evictions"`

	// L1 byte-budget + LRU eviction (Q-L1-BUDGET, 0.25.319). Evictions
	// counters are monotonic; ResidentBytes/Entries are gauges sampled
	// once per snapshot from the live MemCache via the registered hook
	// (see RegisterL1Sampler below).
	L1EvictionsLRU int64 `json:"l1_evictions_lru"`
	L1EvictionsTTL int64 `json:"l1_evictions_ttl"`
	L1ResidentBytes int64 `json:"l1_resident_bytes"`
	L1Entries       int64 `json:"l1_entries"`
	L1MaxBytes      int64 `json:"l1_max_bytes"`
	L1MaxEntries    int64 `json:"l1_max_entries"`
	L1SyncSweepCount  int64 `json:"l1_sync_sweep_count"`
	L1AsyncSweepCount int64 `json:"l1_async_sweep_count"`

	// L1 refresher singleflight coalesce (Q-REFRESH-COALESCE, 0.25.328).
	// Mirrors the atomic counter at /metrics/runtime under
	// cache.refresher_inflight_coalesced.
	RefresherInflightCoalesced int64 `json:"refresher_inflight_coalesced"`

	// L2 post-refilter cache (Q-RBACC-L2-1). HitRate is computed in the
	// snapshot for parity with L1. ResidentBytes/Count are sampled once
	// per snapshot from the live counters.
	L2Hits                   int64 `json:"l2_hits"`
	L2Misses                 int64 `json:"l2_misses"`
	L2Writes                 int64 `json:"l2_writes"`
	L2WritesSkippedIdentical int64 `json:"l2_writes_skipped_identical"`
	L2SkippedHighRatio       int64 `json:"l2_skipped_high_ratio"`
	L2SkippedSizeCap         int64 `json:"l2_skipped_size_cap"`
	L2EvictionsL1Delete int64   `json:"l2_evictions_l1_delete"`
	L2EvictionsIdentity int64   `json:"l2_evictions_identity"`
	L2EvictionsRA       int64   `json:"l2_evictions_ra"`
	L2EvictionsTotal    int64   `json:"l2_evictions_total"`
	L2HitRate           float64 `json:"l2_hit_rate"`
	L2ResidentBytes     int64   `json:"l2_resident_bytes"`
	L2ResidentCount     int64   `json:"l2_resident_count"`

	// Causal-cost /call exit-edge counters (Q-CAUSAL-COST, 0.25.323).
	// Mirrored at /metrics/runtime under the call_events block.
	CallClientGone                 int64 `json:"call_client_gone"`
	CallClientGoneAfterWriteHeader int64 `json:"call_client_gone_after_write_header"`
	CallWriteError                 int64 `json:"call_write_error"`

	// Widget 5xx attribution (Q-5XX-DIAG, 0.25.324). Maps surface at
	// /metrics/runtime under the widgets block. nil when no entries.
	WidgetResponsesByResource map[string]int64 `json:"widget_responses_by_resource,omitempty"`
	WidgetErrorByClass        map[string]int64 `json:"widget_error_by_class,omitempty"`
	UAFSkipped                map[string]int64 `json:"uaf_skipped,omitempty"`
	UAFTouchingByResource     map[string]int64 `json:"uaf_touching_by_resource,omitempty"`
	UAFTouchingCount          int64            `json:"uaf_touching_count"`
	UAFNonTouchingCount       int64            `json:"uaf_non_touching_count"`

	// Panel-probe counters (Q-PANEL-PROBE, 0.25.326).
	PanelProbeManagedEmpty     int64 `json:"panel_probe_managed_empty"`
	PanelProbeManagedPopulated int64 `json:"panel_probe_managed_populated"`
	PanelProbePathMalformed    int64 `json:"panel_probe_path_malformed"`
}

var GlobalMetrics = &Metrics{}

// l1SamplerFn is registered by main.go so MetricsSnapshot can sample the
// live MemCache's residentBytes/entryCount without a hard dependency edge.
// Nil-safe: snapshots return 0 for the gauges when no sampler is registered
// (e.g. unit tests that build snapshots in isolation).
var l1SamplerFn atomic.Value // func() (residentBytes, entries int64)

// RegisterL1Sampler wires the MemCache's L1ResidentBytes / L1EntryCount into
// MetricsSnapshot. Called once at startup right after NewMem. Idempotent —
// re-registering replaces the previous sampler.
func RegisterL1Sampler(fn func() (int64, int64)) {
	if fn != nil {
		l1SamplerFn.Store(fn)
	}
}

// sampleL1 is called from snapshotFromAtomics. Returns 0,0 when no sampler
// is registered.
func sampleL1() (int64, int64) {
	if v := l1SamplerFn.Load(); v != nil {
		if fn, ok := v.(func() (int64, int64)); ok && fn != nil {
			return fn()
		}
	}
	return 0, 0
}

// DiagSnapshot bundles Q-DIAG-PPROF (0.25.321) diagnostic gauges sampled
// from RBACWatcher + MemCache.sets. Same nil-safe atomic-value pattern as
// l1SamplerFn — zero values surface when no sampler is registered.
type DiagSnapshot struct {
	IdentityCacheEntries        int64
	LastCohortBidForUserEntries int64
	EvalCacheEntries            int64
	ClusterDepSetCount          int64
	ClusterDepSetMemberTotal    int64
}

var diagSamplerFn atomic.Value // func() DiagSnapshot

func RegisterDiagSampler(fn func() DiagSnapshot) {
	if fn != nil {
		diagSamplerFn.Store(fn)
	}
}

func SampleDiag() DiagSnapshot {
	if v := diagSamplerFn.Load(); v != nil {
		if fn, ok := v.(func() DiagSnapshot); ok && fn != nil {
			return fn()
		}
	}
	return DiagSnapshot{}
}

// Snapshot returns the accumulated in-process metrics.
func (m *Metrics) Snapshot() MetricsSnapshot {
	return m.snapshotFromAtomics()
}

func (m *Metrics) snapshotFromAtomics() MetricsSnapshot {
	s := MetricsSnapshot{
		GetHits:         m.GetHits.Load(),
		GetMisses:       m.GetMisses.Load(),
		ListHits:        m.ListHits.Load(),
		ListMisses:      m.ListMisses.Load(),
		RBACHits:        m.RBACHits.Load(),
		RBACMisses:      m.RBACMisses.Load(),
		RawHits:         m.RawHits.Load(),
		RawMisses:       m.RawMisses.Load(),
		L1Hits:          m.L1Hits.Load(),
		L1Misses:        m.L1Misses.Load(),
		CallHits:        m.CallHits.Load(),
		CallMisses:      m.CallMisses.Load(),
		NegativeHits:    m.NegativeHits.Load(),
		ExpiryRefreshes: m.ExpiryRefreshes.Load(),

		ClusterDepSAddTotal:            m.ClusterDepSAddTotal.Load(),
		ClusterDepSAddByResolve:        m.ClusterDepSAddByResolve.Load(),
		ClusterDepSAddByResolveNSList:  m.ClusterDepSAddByResolveNSList.Load(),
		ClusterDepSAddByRegister:       m.ClusterDepSAddByRegister.Load(),
		ClusterDepSAddByRegisterNSList: m.ClusterDepSAddByRegisterNSList.Load(),
		ClusterDepSAddDeduped:          m.ClusterDepSAddDeduped.Load(),
		ClusterDepSetSizeMax:           m.ClusterDepSetSizeMax.Load(),
		ClusterDepSMembersTotal:        m.ClusterDepSMembersTotal.Load(),
		ClusterDepSMembersBytes:        m.ClusterDepSMembersBytes.Load(),

		WatchEventsAdd:             m.WatchEventsAdd.Load(),
		WatchEventsUpdate:          m.WatchEventsUpdate.Load(),
		WatchEventsDelete:          m.WatchEventsDelete.Load(),
		WatchEventsDeleteTombstone: m.WatchEventsDeleteTombstone.Load(),
		WatchEventsNoopFiltered:    m.WatchEventsNoopFiltered.Load(),

		CRDRegisterL1Evictions: m.CRDRegisterL1Evictions.Load(),

		L2Hits:                   m.L2Hits.Load(),
		L2Misses:                 m.L2Misses.Load(),
		L2Writes:                 m.L2Writes.Load(),
		L2WritesSkippedIdentical: m.L2WritesSkippedIdentical.Load(),
		L2SkippedHighRatio:       m.L2SkippedHighRatio.Load(),
		L2SkippedSizeCap:         m.L2SkippedSizeCap.Load(),
		L2EvictionsL1Delete: m.L2EvictionsL1Delete.Load(),
		L2EvictionsIdentity: m.L2EvictionsIdentity.Load(),
		L2EvictionsRA:       m.L2EvictionsRA.Load(),
		L2EvictionsTotal:    m.L2EvictionsTotal.Load(),
		L2ResidentBytes:     L2ResidentBytes(),
		L2ResidentCount:     L2ResidentCount(),

		L1EvictionsLRU:    m.L1EvictionsLRU.Load(),
		L1EvictionsTTL:    m.L1EvictionsTTL.Load(),
		L1SyncSweepCount:  m.L1SyncSweepCount.Load(),
		L1AsyncSweepCount: m.L1AsyncSweepCount.Load(),

		RefresherInflightCoalesced: m.RefresherInflightCoalesced.Load(),
		L1MaxBytes:        l1MaxBytes(),
		L1MaxEntries:      int64(l1MaxEntries()),

		CallClientGone:                 m.CallClientGone.Load(),
		CallClientGoneAfterWriteHeader: m.CallClientGoneAfterWriteHeader.Load(),
		CallWriteError:                 m.CallWriteError.Load(),

		WidgetResponsesByResource: SnapshotMap(&m.WidgetResponsesByResource),
		WidgetErrorByClass:        SnapshotMap(&m.WidgetErrorByClass),
		UAFSkipped:                SnapshotMap(&m.UAFSkipped),
		UAFTouchingByResource:     SnapshotMap(&m.UAFTouchingByResource),
		UAFTouchingCount:          m.UAFTouchingCount.Load(),
		UAFNonTouchingCount:       m.UAFNonTouchingCount.Load(),

		PanelProbeManagedEmpty:     m.PanelProbeManagedEmpty.Load(),
		PanelProbeManagedPopulated: m.PanelProbeManagedPopulated.Load(),
		PanelProbePathMalformed:    m.PanelProbePathMalformed.Load(),
	}
	s.L1ResidentBytes, s.L1Entries = sampleL1()
	s.GetHitRate = hitRate(s.GetHits, s.GetMisses)
	s.ListHitRate = hitRate(s.ListHits, s.ListMisses)
	s.RBACHitRate = hitRate(s.RBACHits, s.RBACMisses)
	s.L1HitRate = hitRate(s.L1Hits, s.L1Misses)
	s.L2HitRate = hitRate(s.L2Hits, s.L2Misses)
	if sampled := m.ClusterDepSampledKeys.Load(); sampled > 0 {
		s.ClusterDepSetSizeAvg = float64(m.ClusterDepSetSizeSumLast.Load()) / float64(sampled)
	}
	return s
}

func hitRate(hits, misses int64) float64 {
	if total := hits + misses; total > 0 {
		return float64(hits) / float64(total) * 100
	}
	return 0
}

// SAddClusterDepInstrumented performs a SAdd against a cluster-wide dep key
// and updates GlobalMetrics atomically: total counter, dedup counter (when
// the member already existed), and a sampled SCard every clusterDepSampleEvery
// calls (no goroutine — uses a free-running atomic sampler). Per-site
// counters (ByResolve / ByRegister and their *NSList variants) are
// incremented by the caller before invoking this helper.
//
// The function preserves the existing "discard error, do not block writer"
// semantics of the SAdd call sites — errors are intentionally ignored.
func SAddClusterDepInstrumented(ctx context.Context, c Cache, key, member string, ttl time.Duration) {
	if c == nil {
		return
	}
	added, _ := c.SAddWithTTLN(ctx, key, member, ttl)
	GlobalMetrics.ClusterDepSAddTotal.Add(1)
	if added == 0 {
		GlobalMetrics.ClusterDepSAddDeduped.Add(1)
	}
	// Sampled SCard: one in clusterDepSampleEvery writes pulls the size.
	if GlobalMetrics.ClusterDepSAddSampler.Add(1)%clusterDepSampleEvery == 0 {
		if size, err := c.SCard(ctx, key); err == nil {
			GlobalMetrics.ClusterDepSetSizeSumLast.Add(size)
			GlobalMetrics.ClusterDepSampledKeys.Add(1)
			// Update high-water mark with a CAS loop.
			for {
				cur := GlobalMetrics.ClusterDepSetSizeMax.Load()
				if size <= cur {
					break
				}
				if GlobalMetrics.ClusterDepSetSizeMax.CompareAndSwap(cur, size) {
					break
				}
			}
		}
	}
}
