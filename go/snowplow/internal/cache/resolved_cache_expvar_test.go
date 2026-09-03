// resolved_cache_expvar_test.go — 1.12.4 §7b acceptance.
//
// The load-bearing property here is NOT "the map has the right keys". It
// is that reading the telemetry surface does not CONSTRUCT the thing
// being measured. ResolvedCache() lazily builds the store, wires the dep
// tracker and starts the summary goroutine; if the expvar closure went
// through it, then scraping /debug/vars on a pod where L1 had never been
// touched would create an L1 cache and a background goroutine. That is a
// side effect no observability surface may have, and it is exactly the
// mistake the obvious implementation makes.
package cache

import (
	"testing"
)

// TestResolvedCacheStatsByStat_DoesNotConstructTheStore is the arm that
// matters. It tears the singleton down, reads the stats surface, and
// asserts the store is STILL not built.
//
// The observation is direct: resolvedCachePublished is written only
// inside resolvedCacheOnce.Do, so a nil Load after the read proves the
// Once never ran. An implementation that called ResolvedCache() would
// leave a non-nil pointer here and fail.
func TestResolvedCacheStatsByStat_DoesNotConstructTheStore(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")

	resetResolvedCacheForTest()
	t.Cleanup(resetResolvedCacheForTest)

	if got := resolvedCachePublished.Load(); got != nil {
		t.Fatalf("setup: store already published after reset (%p) — the arm would be vacuous", got)
	}

	stats := ResolvedCacheStatsByStat()

	if got := resolvedCachePublished.Load(); got != nil {
		t.Errorf("reading the stats surface CONSTRUCTED the resolved-cache store (%p). "+
			"The closure must read resolvedCachePublished, never ResolvedCache(), which also "+
			"wires the dep tracker and starts the summary goroutine.", got)
	}
	if len(stats) != 0 {
		t.Errorf("stats before the store exists = %v; want an EMPTY map, so a caller can tell "+
			"\"no cache\" from \"cache present and empty\"", stats)
	}
}

// TestResolvedCacheStatsByStat_ReportsLiveStore is the companion: once
// the store genuinely exists, the surface reports it, and the numbers
// track real Put activity rather than being a static shape.
func TestResolvedCacheStatsByStat_ReportsLiveStore(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")

	resetResolvedCacheForTest()
	t.Cleanup(resetResolvedCacheForTest)

	c := ResolvedCache()
	if c == nil {
		t.Fatal("setup: ResolvedCache() returned nil with both gates on")
	}

	stats := ResolvedCacheStatsByStat()
	if len(stats) == 0 {
		t.Fatal("stats map is empty with a live store — the closure is not seeing the published handle")
	}

	// Every documented stat must be present. A missing key is a silently
	// absent dashboard series, which is worse than a wrong number because
	// the panel renders as "no data" rather than as an alert.
	want := []string{
		"entries", "bytes", "max_entries", "max_bytes",
		"hit_total", "miss_total", "store_total",
		"evict_lru_total", "evict_ttl_total", "evict_delete_total",
		"resident_entries", "resident_bytes", "max_resident_bytes",
		"resident_pin_total", "resident_demote_total",
		"apistage_store_total", "apistage_evict_total",
		"widget_content_store_total", "widget_content_evict_total",
		"ra_full_list_store_total", "ra_full_list_evict_total",
	}
	for _, k := range want {
		if _, ok := stats[k]; !ok {
			t.Errorf("stat %q missing from snowplow_resolved_cache", k)
		}
	}
	if len(stats) != len(want) {
		t.Errorf("stat count = %d; want %d — an undocumented key was added without updating "+
			"this list or docs/architecture/observability.md", len(stats), len(want))
	}

	// The ceilings must be positive: a 0 max_bytes would mean the surface
	// is reporting a zero-value struct rather than the live store.
	if stats["max_bytes"] <= 0 || stats["max_entries"] <= 0 {
		t.Errorf("ceilings read as max_entries=%d max_bytes=%d; a live store always has positive "+
			"budgets, so this is a zero-value struct, not a real snapshot",
			stats["max_entries"], stats["max_bytes"])
	}

	// The surface must TRACK, not merely exist. A miss on an absent key
	// moves miss_total; a static-shape implementation would not.
	before := stats["miss_total"]
	if _, ok := c.Get("no-such-key-1124"); ok {
		t.Fatal("setup: a key that should not exist was found")
	}
	if after := ResolvedCacheStatsByStat()["miss_total"]; after != before+1 {
		t.Errorf("miss_total %d -> %d across one Get miss; want +1 — the surface is not reading "+
			"the live counters", before, after)
	}
}

// TestResolvedCacheExpvar_RegistrationIsIdempotent covers the CFG-1
// seam. init() returns early when CACHE_ENABLED is not truthy at process
// start, which is the case for every `go test` binary, so the helper is
// the only way the key ever gets published under test — and it must be
// safe to call twice, because expvar.Publish panics on a duplicate key.
func TestResolvedCacheExpvar_RegistrationIsIdempotent(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	RegisterResolvedCacheExpvarForTest()
	RegisterResolvedCacheExpvarForTest() // must not panic
}
