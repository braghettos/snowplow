package cache

import (
	"context"
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
}

var GlobalMetrics = &Metrics{}

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
	}
	s.GetHitRate = hitRate(s.GetHits, s.GetMisses)
	s.ListHitRate = hitRate(s.ListHits, s.ListMisses)
	s.RBACHitRate = hitRate(s.RBACHits, s.RBACMisses)
	s.L1HitRate = hitRate(s.L1Hits, s.L1Misses)
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
