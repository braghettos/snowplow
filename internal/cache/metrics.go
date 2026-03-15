package cache

import "sync/atomic"

type Metrics struct {
	GetHits                  atomic.Int64
	GetMisses                atomic.Int64
	ListHits                 atomic.Int64
	ListMisses               atomic.Int64
	RBACHits                 atomic.Int64
	RBACMisses               atomic.Int64
	RawHits                  atomic.Int64
	RawMisses                atomic.Int64
	NegativeHits             atomic.Int64
	ExpiryRefreshes          atomic.Int64
	DebouncedInvalidations   atomic.Int64
}

type MetricsSnapshot struct {
	GetHits                  int64   `json:"get_hits"`
	GetMisses                int64   `json:"get_misses"`
	ListHits                 int64   `json:"list_hits"`
	ListMisses               int64   `json:"list_misses"`
	RBACHits                 int64   `json:"rbac_hits"`
	RBACMisses               int64   `json:"rbac_misses"`
	RawHits                  int64   `json:"raw_hits"`
	RawMisses                int64   `json:"raw_misses"`
	NegativeHits             int64   `json:"negative_hits"`
	ExpiryRefreshes          int64   `json:"expiry_refreshes"`
	DebouncedInvalidations   int64   `json:"debounced_invalidations"`
	GetHitRate               float64 `json:"get_hit_rate"`
	ListHitRate              float64 `json:"list_hit_rate"`
	RBACHitRate              float64 `json:"rbac_hit_rate"`
}

var GlobalMetrics = &Metrics{}

func (m *Metrics) Snapshot() MetricsSnapshot {
	s := MetricsSnapshot{
		GetHits:                m.GetHits.Load(),
		GetMisses:              m.GetMisses.Load(),
		ListHits:               m.ListHits.Load(),
		ListMisses:             m.ListMisses.Load(),
		RBACHits:               m.RBACHits.Load(),
		RBACMisses:             m.RBACMisses.Load(),
		RawHits:                m.RawHits.Load(),
		RawMisses:              m.RawMisses.Load(),
		NegativeHits:           m.NegativeHits.Load(),
		ExpiryRefreshes:        m.ExpiryRefreshes.Load(),
		DebouncedInvalidations: m.DebouncedInvalidations.Load(),
	}
	s.GetHitRate = hitRate(s.GetHits, s.GetMisses)
	s.ListHitRate = hitRate(s.ListHits, s.ListMisses)
	s.RBACHitRate = hitRate(s.RBACHits, s.RBACMisses)
	return s
}

func hitRate(hits, misses int64) float64 {
	if total := hits + misses; total > 0 {
		return float64(hits) / float64(total) * 100
	}
	return 0
}
