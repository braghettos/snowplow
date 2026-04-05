package cache

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"
)

const metricsRedisKey = "snowplow:metrics"

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
	L3Promotions    atomic.Int64
	NegativeHits    atomic.Int64
	ExpiryRefreshes atomic.Int64

	rc atomic.Pointer[RedisCache]
}

// SetRedis enables persistent metrics via Redis HINCRBY. Safe to call with nil.
func (m *Metrics) SetRedis(c *RedisCache) {
	if c != nil {
		m.rc.Store(c)
	}
}

// Inc atomically increments the given counter. The in-memory atomic is
// always updated; Redis persistence happens asynchronously via a background
// flusher (see StartMetricsFlusher) so the hot path stays ~nanoseconds.
func (m *Metrics) Inc(counter *atomic.Int64, field string) {
	counter.Add(1)
}

// StartMetricsFlusher starts a background goroutine that flushes the
// in-memory counters to Redis every `interval`. Call once at startup.
// The flush writes the TOTAL current count (HSET), not a delta — so
// concurrent Inc calls and multi-pod deployments are both safe.
func (m *Metrics) StartMetricsFlusher(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c := m.rc.Load()
				if c == nil {
					continue
				}
				s := m.snapshotFromAtomics()
				c.client.HSet(ctx, metricsRedisKey,
					"get_hits", s.GetHits, "get_misses", s.GetMisses,
					"list_hits", s.ListHits, "list_misses", s.ListMisses,
					"rbac_hits", s.RBACHits, "rbac_misses", s.RBACMisses,
					"raw_hits", s.RawHits, "raw_misses", s.RawMisses,
					"l1_hits", s.L1Hits, "l1_misses", s.L1Misses,
					"call_hits", s.CallHits, "call_misses", s.CallMisses,
					"l3_promotions", s.L3Promotions, "negative_hits", s.NegativeHits,
					"expiry_refreshes", s.ExpiryRefreshes)
			}
		}
	}()
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
	L3Promotions    int64   `json:"l3_promotions"`
	NegativeHits    int64   `json:"negative_hits"`
	ExpiryRefreshes int64   `json:"expiry_refreshes"`
	GetHitRate      float64 `json:"get_hit_rate"`
	ListHitRate     float64 `json:"list_hit_rate"`
	RBACHitRate     float64 `json:"rbac_hit_rate"`
	L1HitRate       float64 `json:"l1_hit_rate"`
}

var GlobalMetrics = &Metrics{}

// Snapshot returns the accumulated metrics. When Redis is available the
// counters survive pod restarts; otherwise in-process atomics are used.
func (m *Metrics) Snapshot() MetricsSnapshot {
	if c := m.rc.Load(); c != nil {
		if s, ok := m.snapshotFromRedis(c); ok {
			return s
		}
	}
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
		L3Promotions:    m.L3Promotions.Load(),
		NegativeHits:    m.NegativeHits.Load(),
		ExpiryRefreshes: m.ExpiryRefreshes.Load(),
	}
	s.GetHitRate = hitRate(s.GetHits, s.GetMisses)
	s.ListHitRate = hitRate(s.ListHits, s.ListMisses)
	s.RBACHitRate = hitRate(s.RBACHits, s.RBACMisses)
	s.L1HitRate = hitRate(s.L1Hits, s.L1Misses)
	return s
}

func (m *Metrics) snapshotFromRedis(c *RedisCache) (MetricsSnapshot, bool) {
	vals, err := c.client.HGetAll(context.Background(), metricsRedisKey).Result()
	if err != nil || len(vals) == 0 {
		return MetricsSnapshot{}, false
	}
	r := func(key string) int64 {
		v, _ := strconv.ParseInt(vals[key], 10, 64)
		return v
	}
	s := MetricsSnapshot{
		GetHits:         r("get_hits"),
		GetMisses:       r("get_misses"),
		ListHits:        r("list_hits"),
		ListMisses:      r("list_misses"),
		RBACHits:        r("rbac_hits"),
		RBACMisses:      r("rbac_misses"),
		RawHits:         r("raw_hits"),
		RawMisses:       r("raw_misses"),
		L1Hits:          r("l1_hits"),
		L1Misses:        r("l1_misses"),
		CallHits:        r("call_hits"),
		CallMisses:      r("call_misses"),
		L3Promotions:    r("l3_promotions"),
		NegativeHits:    r("negative_hits"),
		ExpiryRefreshes: r("expiry_refreshes"),
	}
	s.GetHitRate = hitRate(s.GetHits, s.GetMisses)
	s.ListHitRate = hitRate(s.ListHits, s.ListMisses)
	s.RBACHitRate = hitRate(s.RBACHits, s.RBACMisses)
	s.L1HitRate = hitRate(s.L1Hits, s.L1Misses)
	return s, true
}

func hitRate(hits, misses int64) float64 {
	if total := hits + misses; total > 0 {
		return float64(hits) / float64(total) * 100
	}
	return 0
}
