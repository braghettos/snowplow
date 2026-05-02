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
}

// WorkQueueLens is the read-side observability surface of the priority
// workqueue (HOT > WARM > COLD). Implemented by *cache.ResourceWatcher.
type WorkQueueLens interface {
	HotQueueLen() int
	WarmQueueLen() int
	ColdQueueLen() int
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
// queues may be nil before the ResourceWatcher is wired in startBackgroundServices.
func RuntimeMetricsHandler(c cache.Cache, queues WorkQueueLens) http.Handler {
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
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(m)
	})
}
