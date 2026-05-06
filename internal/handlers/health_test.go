package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

func mockNSGetter() (string, error) {
	return "test-namespace", nil
}

// TestHealthCheck_PureInProcess verifies Q-PREWARM-R2.2: /health
// must NOT touch the kube API server, NOT call rest.InClusterConfig,
// and must respond synchronously regardless of test mode flags.
//
// The post-R2 handler is a 5-byte write — anything else is a
// regression that re-introduces the death-spiral risk.
func TestHealthCheck_PureInProcess(t *testing.T) {
	// Sanity — even outside test-mode the handler must work, because
	// production probes never set TestMode and must still get 200.
	if err := os.Unsetenv("KUBERNETES_SERVICE_HOST"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	handler := HealthCheck("test-service", "v1.0.0", mockNSGetter)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Fatalf("expected body 'ok', got %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("expected text/plain content-type, got %q", ct)
	}
}

// TestInfoHandler_RichJSON verifies the rich service-info payload
// (formerly served at /health) is now served at /info.
func TestInfoHandler_RichJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/info", nil)
	rec := httptest.NewRecorder()

	handler := InfoHandler("test-service", "v1.0.0", mockNSGetter)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp serviceInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}
	want := serviceInfo{Name: "test-service", Build: "v1.0.0", Namespace: "test-namespace"}
	if resp != want {
		t.Errorf("expected %+v, got %+v", want, resp)
	}
}

// TestReadinessCheck_GatesOnInformerReady verifies Q-PREWARM-R2.1:
// /ready returns 503 until cache.MarkInformersReady() is called,
// 200 thereafter — independent of the legacy
// dispatchers.IsPreWarmComplete() flag (which used to be the gate).
func TestReadinessCheck_GatesOnInformerReady(t *testing.T) {
	// Make sure we're testing the cache-enabled path.
	if err := os.Unsetenv("CACHE_ENABLED"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Reset the package-level atomic so the test is order-
	// independent. The test helper lives in the cache package.
	cache.ResetInformersReadyForTest()

	handler := ReadinessCheck()

	// Before MarkInformersReady — 503.
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 before informer-ready, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "warming up") {
		t.Errorf("expected 'warming up' in body, got %q", rec.Body.String())
	}

	// After MarkInformersReady — 200.
	cache.MarkInformersReady()
	req2 := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 after informer-ready, got %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "ready") {
		t.Errorf("expected 'ready' in body, got %q", rec2.Body.String())
	}

	// Cleanup so subsequent tests start from a known state.
	cache.ResetInformersReadyForTest()
}

// TestReadinessCheck_CacheDisabledShortcircuits verifies that with
// CACHE_ENABLED=false the /ready handler returns 200 immediately —
// the informer subsystem is bypassed entirely so there is no
// readiness gate to wait on.
func TestReadinessCheck_CacheDisabledShortcircuits(t *testing.T) {
	cache.ResetInformersReadyForTest()
	t.Setenv("CACHE_ENABLED", "false")

	handler := ReadinessCheck()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with cache disabled, got %d", rec.Code)
	}
}

// TestReadinessCheck_NoPrewarmDependency is the core regression
// guard for the death-spiral fix. /ready MUST flip to 200 once
// MarkInformersReady is called even if no prewarm has ever run.
//
// Pre-R2 this test would fail because the gate also checked
// dispatchers.IsPreWarmComplete(); post-R2 it must pass.
func TestReadinessCheck_NoPrewarmDependency(t *testing.T) {
	cache.ResetInformersReadyForTest()
	if err := os.Unsetenv("CACHE_ENABLED"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Simulate: informers have synced, prewarm has NOT.
	cache.MarkInformersReady()
	defer cache.ResetInformersReadyForTest()

	handler := ReadinessCheck()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (informers ready, prewarm not run): got %d body=%q",
			rec.Code, rec.Body.String())
	}
}
