// debug_refreshes_auth_test.go — #69: /debug/refreshes is auth-gated for prod.
// The aggregate diagnostic must 401 a bare (unauthenticated) request and serve
// the counters only to a valid cookie-or-header JWT — the SAME RefreshAuth gate
// /refreshes uses. The body stays aggregate-only (structurally leak-safe); the
// gate is defence-in-depth so the diagnostic is not world-readable in prod.
//
// Hermetic: httptest + the shared test signing key. NO apiserver.
package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krateo-platformops/snowplow/internal/handlers/middleware"
)

// debugRefreshesServer wires the PRODUCTION gate (RefreshAuth → DebugRefreshes)
// on a test server, mirroring the main.go mount exactly.
func debugRefreshesServer(t *testing.T) string {
	t.Helper()
	h := middleware.RefreshAuth(refreshTestKeys)(DebugRefreshes())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// #69 ARM 1 — bare unauthenticated GET → 401 (the prod hardening: not
// world-readable).
func TestDebugRefreshes_Auth_BareRequest401(t *testing.T) {
	base := debugRefreshesServer(t)
	resp, err := http.Get(base)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bare /debug/refreshes: status=%d want 401 — the endpoint must be auth-gated in prod "+
			"(was world-readable before #69)", resp.StatusCode)
	}
}

// #69 ARM 2 — header JWT → 200 + the aggregate body (the diagnostic still works
// for an authenticated operator).
func TestDebugRefreshes_Auth_HeaderToken200(t *testing.T) {
	base := debugRefreshesServer(t)
	req, _ := http.NewRequest(http.MethodGet, base, nil)
	req.Header.Set("Authorization", "Bearer "+mintToken(t, "userA"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authed /debug/refreshes: status=%d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("authed /debug/refreshes: Content-Type=%q want application/json", ct)
	}
}

// #69 ARM 3 — cookie JWT → 200 (the browser EventSource path also reaches the
// diagnostic).
func TestDebugRefreshes_Auth_CookieToken200(t *testing.T) {
	t.Setenv("REFRESH_SESSION_COOKIE", "krateo-session")
	base := debugRefreshesServer(t)
	req, _ := http.NewRequest(http.MethodGet, base, nil)
	req.AddCookie(&http.Cookie{Name: "krateo-session", Value: mintToken(t, "userA")})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie-authed /debug/refreshes: status=%d want 200", resp.StatusCode)
	}
}
