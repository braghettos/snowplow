// Ship 1.5a (0.25.322) — pin the JSON shape contract for the new
// informer_event_dropped_total field exposed at /metrics/runtime under
// the watch_events block. Tester / canary scripts grep for this key by
// name; renaming or moving it would break the back-pressure dashboard
// the 1.5b A/B harness depends on.
package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

func TestRuntimeMetrics_ExposesInformerEventDropped(t *testing.T) {
	cache.GlobalMetrics.InformerEventDropped.Store(11)
	t.Cleanup(func() {
		cache.GlobalMetrics.InformerEventDropped.Store(0)
	})

	req := httptest.NewRequest(http.MethodGet, "/metrics/runtime", nil)
	rec := httptest.NewRecorder()
	RuntimeMetricsHandler(nil, nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}

	// Decode through the typed struct first to confirm the field is
	// reachable from Go callers (canaries that consume the JSON via
	// matching struct decoders).
	var got RuntimeMetrics
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode typed: %v body=%q", err, rec.Body.String())
	}
	if got.WatchEvents.Dropped != 11 {
		t.Errorf("WatchEvents.Dropped = %d, want 11", got.WatchEvents.Dropped)
	}

	// Decode through a generic map to confirm the JSON key shape — the
	// canary scripts grep this by string, not by struct type.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	we, ok := raw["watch_events"].(map[string]any)
	if !ok {
		t.Fatalf("watch_events block missing or wrong type: %T", raw["watch_events"])
	}
	v, ok := we["informer_event_dropped_total"]
	if !ok {
		t.Fatalf("watch_events.informer_event_dropped_total missing from JSON: %v", we)
	}
	// json decodes numbers to float64 by default.
	if f, ok := v.(float64); !ok || int64(f) != 11 {
		t.Errorf("watch_events.informer_event_dropped_total = %v, want 11", v)
	}
}
