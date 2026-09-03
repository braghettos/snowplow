// resolved_cache_expvar.go — 1.12.4 §7b. expvar exposure for the L1
// resolved-output store's occupancy and lifetime counters.
//
// WHY THIS FILE EXISTS. Every field below already exists on
// ResolvedCacheStats and is already computed by Stats(). Until now the
// ONLY place it went was the periodic `resolved_cache.summary` INFO line
// emitted by startResolvedCacheSummary — and the chart ships
// LOG_LEVEL=warn, which suppresses INFO entirely. So in production the
// numbers that say whether the L1 cache is full, thrashing, or evicting
// were computed every N seconds and then thrown away. This publishes
// them on the surface that is actually readable (/debug/vars, and via
// the OTLP mirror in internal/metrics).
//
// SHAPE. One expvar key, `snowplow_resolved_cache`, returning a
// map[string]int64 keyed by stat name — the same "one counter keyed by
// stat" idiom the OTLP mirror already uses for snowplow_refresher and
// snowplow_crd_discovery, so the two surfaces map 1:1 with no per-stat
// key sprawl at /debug/vars.
//
// NON-CONSTRUCTING. The closure reads resolvedCachePublished (an
// atomic.Pointer written inside resolvedCacheOnce), NOT ResolvedCache(),
// which would lazily build the store, wire the dep tracker and start the
// summary goroutine. A telemetry scrape must never create the thing it
// is measuring. Before the store is built the closure returns an empty
// map rather than zeros, so "not built yet" and "built and empty" are
// distinguishable.
//
// COST. Stats() takes the store's full exclusive mutex (resolved.go
// Stats), not an RLock, and the body is order.Len() plus a handful of
// field reads — microseconds. startResolvedCacheSummary already calls it
// on a timer, so this is not a new class of contention; it is the same
// call on two more read paths. Noted deliberately and NOT "fixed" in
// this release (design §7b / gate §C).
//
// CFG-1 (cache-off compliance, project_cache_off_is_transparent_fallback).
// Under CACHE_ENABLED=false there is no L1 store, so the key MUST NOT be
// registered at all — not registered-with-zeros. The gate is at init()
// time, matching fallthrough_meter_expvar.go, and
// RegisterResolvedCacheExpvarForTest is the sync.Once-guarded seam for
// tests that flip CACHE_ENABLED with t.Setenv after init() already ran.
package cache

import (
	"expvar"
	"sync"
)

// resolvedCacheExpvarOnce guards the Publish call against the
// duplicate-key panic when both init() and the test seam run.
var resolvedCacheExpvarOnce sync.Once

func init() {
	if Disabled() {
		return
	}
	registerResolvedCacheExpvar()
}

// registerResolvedCacheExpvar publishes snowplow_resolved_cache.
// Idempotent (sync.Once), so init() and the test seam can both call it.
func registerResolvedCacheExpvar() {
	resolvedCacheExpvarOnce.Do(func() {
		expvar.Publish("snowplow_resolved_cache", expvar.Func(func() any {
			return ResolvedCacheStatsByStat()
		}))
	})
}

// RegisterResolvedCacheExpvarForTest forces registration under tests
// that flip CACHE_ENABLED=true via t.Setenv after init() already ran
// with the var unset. Idempotent. Production callers MUST NOT use it —
// the init() gate is the authoritative CFG-1 mechanism.
func RegisterResolvedCacheExpvarForTest() {
	registerResolvedCacheExpvar()
}

// ResolvedCacheStatsByStat flattens the live ResolvedCacheStats into the
// `stat -> value` map both the expvar key and the OTLP observable
// callback publish, so the two surfaces cannot drift.
//
// Returns an EMPTY map (not a map of zeros) when the store has not been
// constructed, so a caller can tell "no cache" from "empty cache".
//
// The per-class members are limited to the store/evict totals that
// already exist. Per-class RESIDENT and EXPIRED accounting is only half
// available — ResidentEntries/ResidentBytes are global and EvictTTLTotal
// is global — and adding it means new counters on the Put/evict path,
// which is behaviour-adjacent. Deferred to 1.13.0 (design §7b scope note).
func ResolvedCacheStatsByStat() map[string]int64 {
	c := resolvedCachePublished.Load()
	if c == nil {
		return map[string]int64{}
	}
	s := c.Stats()
	return map[string]int64{
		// occupancy vs its two ceilings
		"entries":     int64(s.Entries),
		"bytes":       s.Bytes,
		"max_entries": int64(s.MaxEntries),
		"max_bytes":   s.MaxBytes,
		// lifetime effectiveness
		"hit_total":   int64(s.HitTotal),
		"miss_total":  int64(s.MissTotal),
		"store_total": int64(s.StoreTotal),
		// why entries leave — lru means the budget is the binding
		// constraint, ttl means staleness, delete means invalidation
		"evict_lru_total":    int64(s.EvictLRUTotal),
		"evict_ttl_total":    int64(s.EvictTTLTotal),
		"evict_delete_total": int64(s.EvictDeleteTotal),
		// Ship 4a resident region
		"resident_entries":      int64(s.ResidentEntries),
		"resident_bytes":        s.ResidentBytes,
		"max_resident_bytes":    s.MaxResidentBytes,
		"resident_pin_total":    int64(s.ResidentPinTotal),
		"resident_demote_total": int64(s.ResidentDemoteTotal),
		// per-class store/evict — the three classes that carry their own
		// counters today
		"apistage_store_total":       int64(s.ApistageStoreTotal),
		"apistage_evict_total":       int64(s.ApistageEvictTotal),
		"widget_content_store_total": int64(s.WidgetContentStoreTotal),
		"widget_content_evict_total": int64(s.WidgetContentEvictTotal),
		"ra_full_list_store_total":   int64(s.RAFullListStoreTotal),
		"ra_full_list_evict_total":   int64(s.RAFullListEvictTotal),
	}
}
