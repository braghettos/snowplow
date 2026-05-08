package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestRuntimeMetrics_ExposesL2Block guards the canary-instrumentation
// contract for Q-RBACC-L2-1: /metrics/runtime MUST surface the L2 cache
// counters under the top-level "l2" key. Required-field set is the PM
// Gate A panel: hits, misses, writes, skipped_high_ratio, hit_rate,
// resident_bytes, entry_count.
//
// Without this block the L2 flag-on canary is blind — Diego's plan
// (.claude/analysis/dev-l2-flag-on-plan-2026-05-07.md §1.2) calls out
// the gap explicitly. This test fails loudly if a future refactor
// removes or renames the JSON keys (e.g. snake_case vs camelCase drift).
func TestRuntimeMetrics_ExposesL2Block(t *testing.T) {
	// Arm the L2 counters with deterministic values so we can assert
	// the snapshot path actually surfaces them.
	cache.GlobalMetrics.L2Hits.Store(7)
	cache.GlobalMetrics.L2Misses.Store(3)
	cache.GlobalMetrics.L2Writes.Store(11)
	cache.GlobalMetrics.L2SkippedHighRatio.Store(2)
	cache.GlobalMetrics.L2SkippedSizeCap.Store(1)
	cache.GlobalMetrics.L2EvictionsL1Delete.Store(5)
	cache.GlobalMetrics.L2EvictionsIdentity.Store(4)
	cache.GlobalMetrics.L2EvictionsRA.Store(6)
	cache.GlobalMetrics.L2EvictionsTotal.Store(15)
	t.Cleanup(func() {
		cache.GlobalMetrics.L2Hits.Store(0)
		cache.GlobalMetrics.L2Misses.Store(0)
		cache.GlobalMetrics.L2Writes.Store(0)
		cache.GlobalMetrics.L2SkippedHighRatio.Store(0)
		cache.GlobalMetrics.L2SkippedSizeCap.Store(0)
		cache.GlobalMetrics.L2EvictionsL1Delete.Store(0)
		cache.GlobalMetrics.L2EvictionsIdentity.Store(0)
		cache.GlobalMetrics.L2EvictionsRA.Store(0)
		cache.GlobalMetrics.L2EvictionsTotal.Store(0)
	})

	req := httptest.NewRequest(http.MethodGet, "/metrics/runtime", nil)
	rec := httptest.NewRecorder()

	// nil cache + nil queues — exercises the safe-defaults branches.
	handler := RuntimeMetricsHandler(nil, nil, nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", rec.Code, rec.Body.String())
	}

	var got RuntimeMetrics
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%q", err, rec.Body.String())
	}

	if got.L2.Hits != 7 || got.L2.Misses != 3 || got.L2.Writes != 11 {
		t.Errorf("L2 counters not surfaced: hits=%d misses=%d writes=%d (want 7/3/11)",
			got.L2.Hits, got.L2.Misses, got.L2.Writes)
	}
	if got.L2.SkippedHighRatio != 2 || got.L2.SkippedSizeCap != 1 {
		t.Errorf("L2 skip counters not surfaced: high_ratio=%d size_cap=%d (want 2/1)",
			got.L2.SkippedHighRatio, got.L2.SkippedSizeCap)
	}
	if got.L2.EvictionsL1Delete != 5 || got.L2.EvictionsIdentity != 4 ||
		got.L2.EvictionsRA != 6 || got.L2.EvictionsTotal != 15 {
		t.Errorf("L2 eviction counters not surfaced: l1del=%d id=%d ra=%d total=%d",
			got.L2.EvictionsL1Delete, got.L2.EvictionsIdentity,
			got.L2.EvictionsRA, got.L2.EvictionsTotal)
	}
	// Hit-rate is computed in the snapshot: 7/(7+3) = 70.0
	if got.L2.HitRate < 69.999 || got.L2.HitRate > 70.001 {
		t.Errorf("L2 hit_rate = %.3f, want 70.0", got.L2.HitRate)
	}

	// Verify the JSON shape exposes snake_case keys under "l2"
	// (the canary scripts grep for these; renaming would break them).
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("raw decode: %v", err)
	}
	l2, ok := raw["l2"].(map[string]any)
	if !ok {
		t.Fatalf("expected top-level 'l2' object, got %T", raw["l2"])
	}
	required := []string{
		"hits", "misses", "writes",
		"skipped_high_ratio", "skipped_size_cap",
		"evictions_l1_delete", "evictions_identity",
		"evictions_ra", "evictions_total",
		"hit_rate", "resident_bytes", "entry_count",
	}
	for _, k := range required {
		if _, ok := l2[k]; !ok {
			t.Errorf("missing required L2 field %q in JSON output: %v", k, l2)
		}
	}
}

// TestRuntimeMetrics_PreservesExistingShape guards against accidental
// removal of the pre-L2 fields (cluster_dep, watch_events, work_queues).
// Additive-only is the contract — the canary cannot land if an existing
// dashboard panel breaks.
func TestRuntimeMetrics_PreservesExistingShape(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/metrics/runtime", nil)
	rec := httptest.NewRecorder()
	RuntimeMetricsHandler(nil, nil, nil).ServeHTTP(rec, req)

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{
		"heap_alloc_mb", "heap_sys_mb", "goroutine_count", "num_gc",
		"active_users", "cache_key_count",
		"cluster_dep", "watch_events", "work_queues", "l1", "l2", "prewarm",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing top-level field %q in /metrics/runtime output", key)
		}
	}
}

// TestRuntimeMetrics_ExposesL1Block guards the canary-instrumentation
// contract for Q-L1-BUDGET (0.25.319): /metrics/runtime MUST surface the
// L1 byte-budget gauges + LRU/TTL eviction counters under top-level "l1".
//
// Required-field set is the architect's acceptance: resident_bytes,
// entries, max_bytes, max_entries, evictions_lru, evictions_ttl. Without
// these the byte-budget rollout is blind (PM cannot verify L1 evictions
// fire under load, cannot compute utilisation %, cannot diff between
// LRU-driven and TTL-driven evictions). Test fails loudly on JSON-key
// drift.
func TestRuntimeMetrics_ExposesL1Block(t *testing.T) {
	cache.GlobalMetrics.L1EvictionsLRU.Store(17)
	cache.GlobalMetrics.L1EvictionsTTL.Store(23)
	t.Cleanup(func() {
		cache.GlobalMetrics.L1EvictionsLRU.Store(0)
		cache.GlobalMetrics.L1EvictionsTTL.Store(0)
		cache.RegisterL1Sampler(func() (int64, int64) { return 0, 0 })
	})
	cache.RegisterL1Sampler(func() (int64, int64) { return 4096, 8 })

	req := httptest.NewRequest(http.MethodGet, "/metrics/runtime", nil)
	rec := httptest.NewRecorder()
	RuntimeMetricsHandler(nil, nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", rec.Code, rec.Body.String())
	}

	var got RuntimeMetrics
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%q", err, rec.Body.String())
	}

	if got.L1.ResidentBytes != 4096 {
		t.Errorf("L1.ResidentBytes: got %d, want 4096", got.L1.ResidentBytes)
	}
	if got.L1.Entries != 8 {
		t.Errorf("L1.Entries: got %d, want 8", got.L1.Entries)
	}
	if got.L1.EvictionsLRU != 17 {
		t.Errorf("L1.EvictionsLRU: got %d, want 17", got.L1.EvictionsLRU)
	}
	if got.L1.EvictionsTTL != 23 {
		t.Errorf("L1.EvictionsTTL: got %d, want 23", got.L1.EvictionsTTL)
	}
	if got.L1.MaxBytes <= 0 {
		t.Errorf("L1.MaxBytes: got %d, want > 0 (default 2GiB or env override)", got.L1.MaxBytes)
	}
	if got.L1.MaxEntries <= 0 {
		t.Errorf("L1.MaxEntries: got %d, want > 0 (default 200000 or env override)", got.L1.MaxEntries)
	}

	// Verify the JSON shape exposes snake_case keys under "l1".
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("raw decode: %v", err)
	}
	l1, ok := raw["l1"].(map[string]any)
	if !ok {
		t.Fatalf("expected top-level 'l1' object, got %T", raw["l1"])
	}
	required := []string{
		"resident_bytes", "entries", "max_bytes", "max_entries",
		"evictions_lru", "evictions_ttl",
	}
	for _, k := range required {
		if _, ok := l1[k]; !ok {
			t.Errorf("missing required L1 field %q in JSON output: %v", k, l1)
		}
	}
}
