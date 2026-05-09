package handlers

import (
	"encoding/json"
	"net/http"
	"runtime"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// RuntimeMetrics holds the JSON structure returned by /metrics/runtime.
type RuntimeMetrics struct {
	HeapAllocMB    float64         `json:"heap_alloc_mb"`
	HeapSysMB      float64         `json:"heap_sys_mb"`
	GoroutineCount int             `json:"goroutine_count"`
	NumGC          uint32          `json:"num_gc"`
	ActiveUsers    int             `json:"active_users"`
	CacheKeyCount  int64           `json:"cache_key_count"`
	ClusterDep     ClusterDepInfo  `json:"cluster_dep"`
	WatchEvents    WatchEventsInfo `json:"watch_events"`
	WorkQueues     WorkQueuesInfo  `json:"work_queues"`
	L1             L1Info          `json:"l1"`
	L2             L2Info          `json:"l2"`
	Prewarm        PrewarmInfo     `json:"prewarm"`
	Diag           DiagInfo        `json:"diag"`
	CallEvents     CallEventsInfo  `json:"call_events"`
	Widgets        WidgetsInfo     `json:"widgets"`
}

// WidgetsInfo exposes Q-5XX-DIAG (0.25.324) widget-handler attribution
// counters. ResponsesByResource keys "{group}/{resource}/{reload_idx}/{class}"
// where class ∈ {2xx,4xx,5xx}. ErrorByClass keys the writeWidgetError site
// (rbac_forbidden, object_get_failed, apiref_resolve_failed, marshal_failed,
// restaction_dispatch_failed). UAFSkipped keys the
// auditUserAccessFilterSkipped reason (api_error, ...). UAFTouchingCount /
// UAFNonTouchingCount tally hits at the widgets.go gate (uafSkip = tracker
// .UAFTouching()) — UAFTouchingByResource breaks the same tally down by
// "{group}/{resource}/{reload_idx}/{true|false}" so observers can see if
// the failing CRs are systematically UAFTouching=false while siblings are
// UAFTouching=true (the H1' smoking gun).
//
// All maps surface only non-empty buckets to keep the /metrics/runtime
// payload bounded under cold cache.
type WidgetsInfo struct {
	// JSON tags pin the architect's Q-5XX-DIAG canonical contract names.
	// `responses_5xx_by_resource` carries 2xx/4xx/5xx labels via key suffix
	// (the per-class dimension lives inside the key, not the metric name);
	// `user_access_filter_skipped` mirrors the slog audit channel.
	ResponsesByResource   map[string]int64 `json:"responses_5xx_by_resource,omitempty"`
	ErrorByClass          map[string]int64 `json:"error_by_class,omitempty"`
	UAFSkipped            map[string]int64 `json:"user_access_filter_skipped,omitempty"`
	UAFTouchingByResource map[string]int64 `json:"uaf_touching_by_resource,omitempty"`
	UAFTouchingCount      int64            `json:"uaf_touching_count"`
	UAFNonTouchingCount   int64            `json:"uaf_non_touching_count"`
	PanelProbe            PanelProbeInfo   `json:"panel_probe"`
}

// PanelProbeInfo exposes Q-PANEL-PROBE (0.25.326) counters: when the OUTER
// JQ filter at restactions.go:118 trips, the probe inspects the unfiltered
// dict's .status.managed shape and bumps one of these. Falsification gate
// for H_status_managed_empty: managed_empty / (managed_empty + managed_populated)
// is the overlap ratio with failing-CR set.
type PanelProbeInfo struct {
	ManagedEmpty     int64 `json:"managed_empty"`
	ManagedPopulated int64 `json:"managed_populated"`
	PathMalformed    int64 `json:"path_malformed"`
}

// CallEventsInfo exposes Q-CAUSAL-COST (0.25.323) /call exit-edge counters.
// ClientGone: ctx.Err() != nil at handler exit (client closed or deadline);
// ClientGoneAfterWriteHeader: subset where a status was already sent (200
// then client bailed mid-body); WriteError: Write returned non-nil (broken
// pipe, TCP reset, gzip flush). All monotonic; sample successively for rates.
type CallEventsInfo struct {
	ClientGone                 int64 `json:"client_gone"`
	ClientGoneAfterWriteHeader int64 `json:"client_gone_after_write_header"`
	WriteError                 int64 `json:"write_error"`
}

// DiagInfo exposes Q-DIAG-PPROF (0.25.321) heap-shift diagnostic gauges:
// per-cache entry counts pprof inuse_space alone cannot disambiguate.
// RBACWatcher: identityCache, lastCohortBidForUser, evalCache.
// MemCache.sets: count + member total.
type DiagInfo struct {
	IdentityCacheEntries        int64 `json:"identity_cache_entries"`
	LastCohortBidForUserEntries int64 `json:"last_cohort_bid_for_user_entries"`
	EvalCacheEntries            int64 `json:"eval_cache_entries"`
	ClusterDepSetCount          int64 `json:"cluster_dep_set_count"`
	ClusterDepSetMemberTotal    int64 `json:"cluster_dep_set_member_total"`
}

// L1Info exposes the L1 byte-budget + LRU eviction telemetry (Q-L1-BUDGET,
// 0.25.319) at /metrics/runtime. ResidentBytes/Entries are gauge snapshots
// of the live MemCache; EvictionsLRU/EvictionsTTL are monotonic counters
// that distinguish budget-driven evictions from age-driven ones. MaxBytes
// /MaxEntries reflect the configured caps so canary observers can compute
// utilisation percentages without re-reading env.
//
// Hits/Misses/HitRate (Q-DIAG-PPROF, 0.25.321) folded in from /metrics/cache
// so canary observers can read L1 attribution without two endpoints.
type L1Info struct {
	ResidentBytes   int64   `json:"resident_bytes"`
	Entries         int64   `json:"entries"`
	MaxBytes        int64   `json:"max_bytes"`
	MaxEntries      int64   `json:"max_entries"`
	EvictionsLRU    int64   `json:"evictions_lru"`
	EvictionsTTL    int64   `json:"evictions_ttl"`
	Hits            int64   `json:"hits"`
	Misses          int64   `json:"misses"`
	HitRate         float64 `json:"hit_rate"`
	SyncSweepCount  int64   `json:"sync_sweep_count"`
	AsyncSweepCount int64   `json:"async_sweep_count"`
}

// WorkQueueLens is the read-side observability surface of the priority
// workqueue (HOT > WARM > COLD). Implemented by *cache.ResourceWatcher.
type WorkQueueLens interface {
	HotQueueLen() int
	WarmQueueLen() int
	ColdQueueLen() int
}

// PrewarmLens is the read-side observability surface of the
// PrewarmWorkerPool. Implemented by *dispatchers.PrewarmWorkerPool.
// Kept narrow so internal/handlers does not import dispatchers.
type PrewarmLens interface {
	Stats() (enqueued, processed, dropped int64)
	QueueDepth() int
	QueueCapacity() int
	CohortFanouts() int64
}

// PrewarmInfo exposes the PrewarmWorkerPool counters at /metrics/runtime
// so canary observers can verify (a) the cohort-prewarm hook fires on
// RBAC binding ADD (cohort_fanouts > 0), (b) the queue is draining
// (queue_depth low + processed advancing), (c) backpressure is healthy
// (dropped == 0 in steady state). Q-COHORT-PREWARM (v0.25.312) PM gate
// G-PREWARM-COUNT reads CohortFanouts.
type PrewarmInfo struct {
	Enqueued      int64 `json:"enqueued"`
	Processed     int64 `json:"processed"`
	Dropped       int64 `json:"dropped"`
	QueueDepth    int   `json:"queue_depth"`
	QueueCapacity int   `json:"queue_capacity"`
	CohortFanouts int64 `json:"cohort_fanouts"`
}

// WorkQueuesInfo exposes the current depth of the three L1 refresh
// priority queues. Non-zero hot_len under load indicates HOT-tier
// worker saturation (architect report 2026-05-02 §6/§7 #1).
type WorkQueuesInfo struct {
	HotLen  int `json:"hot_len"`
	WarmLen int `json:"warm_len"`
	ColdLen int `json:"cold_len"`
}

// WatchEventsInfo exposes informer event delivery counters. DeleteTombstone
// is the smoking-gun signal: when non-zero, the reflector synthesized a
// Delete via relist because the original watch event was lost.
type WatchEventsInfo struct {
	Add             int64 `json:"add"`
	Update          int64 `json:"update"`
	Delete          int64 `json:"delete"`
	DeleteTombstone int64 `json:"delete_tombstone"`
}

// L2Info exposes the L2 post-refilter cache (Q-RBACC-L2-1) counters at
// /metrics/runtime so canary observers can compute hit ratio + budget
// utilisation without needing /metrics/cache. All fields are sampled
// from cache.GlobalMetrics.Snapshot() (atomic loads) — safe under
// concurrent /metrics/runtime requests.
//
// Hits/Misses/Writes/Skipped/Evictions are monotonic counters (compute
// rate via successive samples). HitRate is the cumulative percentage
// (0–100) computed in the snapshot. ResidentBytes/Count are gauge
// snapshots of the current L2 budget consumption.
type L2Info struct {
	Hits                int64   `json:"hits"`
	Misses              int64   `json:"misses"`
	Writes              int64   `json:"writes"`
	SkippedHighRatio    int64   `json:"skipped_high_ratio"`
	SkippedSizeCap      int64   `json:"skipped_size_cap"`
	EvictionsL1Delete   int64   `json:"evictions_l1_delete"`
	EvictionsIdentity   int64   `json:"evictions_identity"`
	EvictionsRA         int64   `json:"evictions_ra"`
	EvictionsTotal      int64   `json:"evictions_total"`
	HitRate             float64 `json:"hit_rate"`
	ResidentBytes       int64   `json:"resident_bytes"`
	EntryCount          int64   `json:"entry_count"`
}

// ClusterDepInfo mirrors the cluster-wide dep instrumentation counters from
// MetricsSnapshot. The two *_per_namespace_list_* fields drive the Option A
// go/no-go signal (W_ns / W headline ratio per design doc §3.4).
type ClusterDepInfo struct {
	SAddTotal                 int64   `json:"sadd_total"`
	SAddByResolve             int64   `json:"sadd_by_resolve"`
	SAddByResolveNSList       int64   `json:"writes_per_namespace_list_resolve"`
	SAddByRegister            int64   `json:"sadd_by_register"`
	SAddByRegisterNSList      int64   `json:"writes_per_namespace_list_register"`
	SAddDeduped               int64   `json:"sadd_deduped"`
	SetSizeMax                int64   `json:"set_size_max"`
	SetSizeAvg                float64 `json:"set_size_avg"`
	SMembersTotal             int64   `json:"smembers_total"`
	SMembersBytes             int64   `json:"smembers_bytes"`
	WritesPerNamespaceListSum int64   `json:"writes_per_namespace_list"`
}

// RuntimeMetricsHandler returns an http.Handler that serves /metrics/runtime.
// It collects Go runtime stats, active user count, and total cache key count.
// queues may be nil before the ResourceWatcher is wired in
// startBackgroundServices. prewarm may be nil before the PrewarmWorkerPool
// is started (or when PREWARM_MODE=legacy).
func RuntimeMetricsHandler(c cache.Cache, queues WorkQueueLens, prewarm PrewarmLens) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)

		ctx := r.Context()

		activeUsers := 0
		if c != nil {
			members, err := c.SMembers(ctx, cache.ActiveUsersKey)
			if err == nil {
				activeUsers = len(members)
			}
		}

		var redisKeyCount int64
		if c != nil {
			redisKeyCount = c.DBSize(ctx)
		}

		var wqInfo WorkQueuesInfo
		if queues != nil {
			wqInfo = WorkQueuesInfo{
				HotLen:  queues.HotQueueLen(),
				WarmLen: queues.WarmQueueLen(),
				ColdLen: queues.ColdQueueLen(),
			}
		}

		var pwInfo PrewarmInfo
		if prewarm != nil {
			enq, proc, drop := prewarm.Stats()
			pwInfo = PrewarmInfo{
				Enqueued:      enq,
				Processed:     proc,
				Dropped:       drop,
				QueueDepth:    prewarm.QueueDepth(),
				QueueCapacity: prewarm.QueueCapacity(),
				CohortFanouts: prewarm.CohortFanouts(),
			}
		}

		snap := cache.GlobalMetrics.Snapshot()
		m := RuntimeMetrics{
			HeapAllocMB:    float64(ms.HeapAlloc) / (1024 * 1024),
			HeapSysMB:      float64(ms.HeapSys) / (1024 * 1024),
			GoroutineCount: runtime.NumGoroutine(),
			NumGC:          ms.NumGC,
			ActiveUsers:    activeUsers,
			CacheKeyCount:  redisKeyCount,
			ClusterDep: ClusterDepInfo{
				SAddTotal:                 snap.ClusterDepSAddTotal,
				SAddByResolve:             snap.ClusterDepSAddByResolve,
				SAddByResolveNSList:       snap.ClusterDepSAddByResolveNSList,
				SAddByRegister:            snap.ClusterDepSAddByRegister,
				SAddByRegisterNSList:      snap.ClusterDepSAddByRegisterNSList,
				SAddDeduped:               snap.ClusterDepSAddDeduped,
				SetSizeMax:                snap.ClusterDepSetSizeMax,
				SetSizeAvg:                snap.ClusterDepSetSizeAvg,
				SMembersTotal:             snap.ClusterDepSMembersTotal,
				SMembersBytes:             snap.ClusterDepSMembersBytes,
				WritesPerNamespaceListSum: snap.ClusterDepSAddByResolveNSList + snap.ClusterDepSAddByRegisterNSList,
			},
			WatchEvents: WatchEventsInfo{
				Add:             snap.WatchEventsAdd,
				Update:          snap.WatchEventsUpdate,
				Delete:          snap.WatchEventsDelete,
				DeleteTombstone: snap.WatchEventsDeleteTombstone,
			},
			WorkQueues: wqInfo,
			L1: L1Info{
				ResidentBytes:   snap.L1ResidentBytes,
				Entries:         snap.L1Entries,
				MaxBytes:        snap.L1MaxBytes,
				MaxEntries:      snap.L1MaxEntries,
				EvictionsLRU:    snap.L1EvictionsLRU,
				EvictionsTTL:    snap.L1EvictionsTTL,
				Hits:            snap.L1Hits,
				Misses:          snap.L1Misses,
				HitRate:         snap.L1HitRate,
				SyncSweepCount:  snap.L1SyncSweepCount,
				AsyncSweepCount: snap.L1AsyncSweepCount,
			},
			L2: L2Info{
				Hits:              snap.L2Hits,
				Misses:            snap.L2Misses,
				Writes:            snap.L2Writes,
				SkippedHighRatio:  snap.L2SkippedHighRatio,
				SkippedSizeCap:    snap.L2SkippedSizeCap,
				EvictionsL1Delete: snap.L2EvictionsL1Delete,
				EvictionsIdentity: snap.L2EvictionsIdentity,
				EvictionsRA:       snap.L2EvictionsRA,
				EvictionsTotal:    snap.L2EvictionsTotal,
				HitRate:           snap.L2HitRate,
				ResidentBytes:     snap.L2ResidentBytes,
				EntryCount:        snap.L2ResidentCount,
			},
			Prewarm: pwInfo,
		}

		// Q-DIAG-PPROF (0.25.321) — sampled gauges for heap-shift RCA;
		// zero values when no sampler registered (unit tests, cache-off).
		ds := cache.SampleDiag()
		m.Diag = DiagInfo(ds)

		// Q-CAUSAL-COST (0.25.323) — /call exit-edge counters.
		m.CallEvents = CallEventsInfo{
			ClientGone:                 snap.CallClientGone,
			ClientGoneAfterWriteHeader: snap.CallClientGoneAfterWriteHeader,
			WriteError:                 snap.CallWriteError,
		}

		// Q-5XX-DIAG (0.25.324) — widget handler 5xx attribution.
		// Q-PANEL-PROBE (0.25.326) — panel-probe nested under widgets.
		m.Widgets = WidgetsInfo{
			ResponsesByResource:   snap.WidgetResponsesByResource,
			ErrorByClass:          snap.WidgetErrorByClass,
			UAFSkipped:            snap.UAFSkipped,
			UAFTouchingByResource: snap.UAFTouchingByResource,
			UAFTouchingCount:      snap.UAFTouchingCount,
			UAFNonTouchingCount:   snap.UAFNonTouchingCount,
			PanelProbe: PanelProbeInfo{
				ManagedEmpty:     snap.PanelProbeManagedEmpty,
				ManagedPopulated: snap.PanelProbeManagedPopulated,
				PathMalformed:    snap.PanelProbePathMalformed,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(m)
	})
}
