package cache

import "sync/atomic"

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
	L2Hits          atomic.Int64
	L2Misses        atomic.Int64
	CallHits        atomic.Int64
	CallMisses      atomic.Int64
	L3Promotions    atomic.Int64
	NegativeHits    atomic.Int64
	ExpiryRefreshes atomic.Int64
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
	L2Hits          int64   `json:"l2_hits"`
	L2Misses        int64   `json:"l2_misses"`
	CallHits        int64   `json:"call_hits"`
	CallMisses      int64   `json:"call_misses"`
	L3Promotions    int64   `json:"l3_promotions"`
	NegativeHits    int64   `json:"negative_hits"`
	ExpiryRefreshes int64   `json:"expiry_refreshes"`
	GetHitRate      float64 `json:"get_hit_rate"`
	ListHitRate     float64 `json:"list_hit_rate"`
	RBACHitRate     float64 `json:"rbac_hit_rate"`
	L1HitRate       float64 `json:"l1_hit_rate"`
	L2HitRate       float64 `json:"l2_hit_rate"`
}

var GlobalMetrics = &Metrics{}

func (m *Metrics) Snapshot() MetricsSnapshot {
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
		L2Hits:          m.L2Hits.Load(),
		L2Misses:        m.L2Misses.Load(),
		CallHits:        m.CallHits.Load(),
		CallMisses:      m.CallMisses.Load(),
		L3Promotions:    m.L3Promotions.Load(),
		NegativeHits:    m.NegativeHits.Load(),
		ExpiryRefreshes: m.ExpiryRefreshes.Load(),
	}
	s.GetHitRate = hitRate(s.GetHits, s.GetMisses)
	s.ListHitRate = hitRate(s.ListHits, s.ListMisses)
	s.RBACHitRate = hitRate(s.RBACHits, s.RBACMisses)
	s.L1HitRate = hitRate(s.L1Hits, s.L1Misses)
	s.L2HitRate = hitRate(s.L2Hits, s.L2Misses)
	return s
}

func hitRate(hits, misses int64) float64 {
	if total := hits + misses; total > 0 {
		return float64(hits) / float64(total) * 100
	}
	return 0
}
