package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// Q-5XX-DIAG (0.25.324) — /metrics/runtime exposes a "widgets" block
// with the four counter shapes. This test pins the JSON contract that
// the canary scraper depends on. A future renamer must fail this test,
// not silently break the dashboard.

func clearRuntimeWidgetMetrics() {
	cache.GlobalMetrics.UAFTouchingCount.Store(0)
	cache.GlobalMetrics.UAFNonTouchingCount.Store(0)
	for _, m := range []*sync.Map{
		&cache.GlobalMetrics.WidgetResponsesByResource,
		&cache.GlobalMetrics.WidgetErrorByClass,
		&cache.GlobalMetrics.UAFSkipped,
		&cache.GlobalMetrics.UAFTouchingByResource,
	} {
		m.Range(func(k, _ any) bool {
			m.Delete(k)
			return true
		})
	}
}

func TestRuntimeMetrics_WidgetsBlockShape(t *testing.T) {
	clearRuntimeWidgetMetrics()
	t.Cleanup(clearRuntimeWidgetMetrics)

	cache.GlobalMetrics.UAFTouchingCount.Store(11)
	cache.GlobalMetrics.UAFNonTouchingCount.Store(2)
	cache.IncMapKey(&cache.GlobalMetrics.WidgetResponsesByResource, "widgets.templates.krateo.io/compositionpanels/1/5xx")
	cache.IncMapKey(&cache.GlobalMetrics.WidgetErrorByClass, "rbac_forbidden")
	cache.IncMapKey(&cache.GlobalMetrics.UAFSkipped, "api_error")
	cache.IncMapKey(&cache.GlobalMetrics.UAFTouchingByResource, "widgets.templates.krateo.io/compositionpanels/1/false")

	req := httptest.NewRequest(http.MethodGet, "/metrics/runtime", nil)
	rec := httptest.NewRecorder()
	RuntimeMetricsHandler(nil, nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200, body=%q", rec.Code, rec.Body.String())
	}

	var typed RuntimeMetrics
	if err := json.Unmarshal(rec.Body.Bytes(), &typed); err != nil {
		t.Fatalf("decode typed: %v", err)
	}
	if typed.Widgets.UAFTouchingCount != 11 {
		t.Errorf("UAFTouchingCount: got %d, want 11", typed.Widgets.UAFTouchingCount)
	}
	if typed.Widgets.UAFNonTouchingCount != 2 {
		t.Errorf("UAFNonTouchingCount: got %d, want 2", typed.Widgets.UAFNonTouchingCount)
	}
	if got := typed.Widgets.ErrorByClass["rbac_forbidden"]; got != 1 {
		t.Errorf("ErrorByClass[rbac_forbidden]: got %d, want 1", got)
	}
	if got := typed.Widgets.UAFSkipped["api_error"]; got != 1 {
		t.Errorf("UAFSkipped[api_error]: got %d, want 1", got)
	}

	// Snake_case JSON contract — pin keys the canary parses.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	w, ok := raw["widgets"].(map[string]any)
	if !ok {
		t.Fatalf("widgets block missing or wrong shape: %T", raw["widgets"])
	}
	// Canary contract: JSON keys MUST match the architect's Q-5XX-DIAG
	// spec names. Past dev-shortened variants (`responses_by_resource`,
	// `uaf_skipped`) drifted from the tester's grep targets and produced
	// false-positive "metric missing" defects (Q-5XX-DIAG-FIXES, 0.25.325).
	for _, k := range []string{
		"uaf_touching_count",
		"uaf_non_touching_count",
		"responses_5xx_by_resource",
		"error_by_class",
		"user_access_filter_skipped",
		"uaf_touching_by_resource",
	} {
		if _, present := w[k]; !present {
			t.Errorf("widgets.%s key missing from JSON (raw=%v)", k, w)
		}
	}
}

// TestRuntimeMetrics_WidgetsBlockEmpty verifies that under a fresh
// cache (no widget traffic yet) the maps come back nil — the
// `omitempty` tags suppress them so /metrics/runtime stays small at
// startup. The atomic totals always render (canary cares about a
// monotone zero baseline).
func TestRuntimeMetrics_WidgetsBlockEmpty(t *testing.T) {
	clearRuntimeWidgetMetrics()
	t.Cleanup(clearRuntimeWidgetMetrics)

	req := httptest.NewRequest(http.MethodGet, "/metrics/runtime", nil)
	rec := httptest.NewRecorder()
	RuntimeMetricsHandler(nil, nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	w, _ := raw["widgets"].(map[string]any)
	if _, present := w["responses_5xx_by_resource"]; present {
		t.Errorf("responses_5xx_by_resource must be omitted when empty (got %v)", w)
	}
	if _, present := w["error_by_class"]; present {
		t.Errorf("error_by_class must be omitted when empty (got %v)", w)
	}
	if _, present := w["user_access_filter_skipped"]; present {
		t.Errorf("user_access_filter_skipped must be omitted when empty (got %v)", w)
	}
	if _, present := w["uaf_touching_by_resource"]; present {
		t.Errorf("uaf_touching_by_resource must be omitted when empty (got %v)", w)
	}
	// Atomic totals render zero (no omitempty).
	if got, ok := w["uaf_touching_count"]; !ok || got.(float64) != 0 {
		t.Errorf("uaf_touching_count must render 0, got %v", got)
	}
}
