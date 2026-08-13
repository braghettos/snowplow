// refreshes_gzip_integration_test.go — Track 2 (transport gzip) C-1 PROD arm.
//
// The C-1 arms in internal/handlers/middleware/compression_test.go drive a
// self-contained idleConnectHandler (preamble is a test parameter) — they
// prove the WRAPPER behavior but never exercise refreshes.go, so they cannot
// guard the production `: connected` preamble against a future refactor that
// deletes it. This arm closes that gap: it drives the REAL Refreshes /
// serveIdleSSE handler THROUGH the production middleware.Gzip() wrapper on the
// warmup idle path (phase1 NOT done + a NotFound coord → armed==0 →
// serveIdleSSE), with a gzip-advertising client and a short request deadline,
// and asserts response headers COMMIT AT CONNECT (client.Do returns before the
// deadline) even though the idle stream never writes an event.
//
// RED proof (captured in /tmp/comp/): strip the `: connected` preamble from
// BOTH refreshes.go sites → this arm FAILS (headers withheld until the first
// heartbeat / deadline); restore byte-identical → GREEN. This is the arm that
// makes the production preamble load-bearing under test.
package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/handlers/middleware"
)

// refreshServerGzip wires the PRODUCTION chain (RefreshAuth -> Refreshes)
// THROUGH middleware.Gzip() — the exact wrapper main.go mounts on the mux —
// on a test server. Returns the server so the caller controls teardown (the
// idle SSE handler blocks streaming, so Close must follow a client-conn drop).
func refreshServerGzip(t *testing.T) *httptest.Server {
	t.Helper()
	h := middleware.RefreshAuth(refreshTestKeys)(Refreshes())
	srv := httptest.NewServer(middleware.Gzip(h))
	return srv
}

// TestRefreshes_Gzip_C1_WarmupIdleHeadersCommitAtConnect is the C-1 PROD GREEN
// arm. Under the production Gzip() wrapper, the REAL warmup idle path
// (serveIdleSSE via the #68 divert) commits response headers within 100ms even
// though no event is ever written — because refreshes.go emits the
// `: connected` preamble before its first flush.
func TestRefreshes_Gzip_C1_WarmupIdleHeadersCommitAtConnect(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("REFRESH_SSE_ENABLED", "")
	seedAuthTestWidget(t) // seeds the panel CR + RBAC (RBACGen>0)

	// REAL warmup predicate: phase1 NOT done → refreshWarmupIncomplete() true →
	// an armed==0 coord serves serveIdleSSE (ARM-A path) instead of a 400.
	cache.ResetPhase1DoneForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)
	if cache.IsPhase1Done() {
		t.Fatalf("C-1 PROD setup: phase1 must be NOT done to exercise the warmup idle path")
	}

	srv := refreshServerGzip(t)
	// Teardown order: the idle handler blocks on the heartbeat loop, so the
	// client connection stays active. Drop client conns, THEN Close, so
	// srv.Close does not wait on the still-streaming handler.
	defer func() {
		srv.CloseClientConnections()
		srv.Close()
	}()

	// A NotFound coord → objects.Get NotFound → fail-closed skip → armed==0;
	// combined with warmup-incomplete this drives serveIdleSSE.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?sub="+subParamNotFound(t), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+mintToken(t, "userA"))
	req.Header.Set("Accept-Encoding", "gzip") // gzip-advertising: engages the wrapper

	// A dedicated transport (no auto-gunzip) we can close at teardown.
	tr := &http.Transport{DisableCompression: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}

	resp, err := client.Do(req)
	if err != nil {
		// Headers did NOT arrive before the 100ms deadline → the preamble is
		// missing / not committing headers at connect (EventSource stuck in
		// CONNECTING). This is the RED signal when the preamble is stripped.
		t.Fatalf("C-1 PROD: /refreshes warmup idle headers did NOT commit within 100ms through the Gzip wrapper "+
			"(client.Do err=%v) — the `: connected` preamble in refreshes.go serveIdleSSE is missing or not forcing "+
			"header commit; a browser EventSource would sit in CONNECTING.", err)
	}
	// Cancel so the client abandons the still-open idle body promptly.
	cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("C-1 PROD: warmup idle status=%d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("C-1 PROD: warmup idle Content-Type=%q want text/event-stream", ct)
	}
	// The exclusion also keeps SSE uncompressed (T2-2) — belt-and-suspenders
	// on the real handler path.
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("C-1 PROD: SSE response Content-Encoding=%q want empty (text/event-stream must not be gzip-compressed)", ce)
	}
}
