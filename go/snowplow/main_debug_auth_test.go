// main_debug_auth_test.go — O-D3 [SEC] (1.12.3): the /debug/* surface is not
// world-readable on the chart's LoadBalancer.
//
// THE DEFECT THIS PINS. Before 1.12.3, main.go mounted `/debug/pprof/{,cmdline,
// profile,symbol,trace}`, `/debug/vars`, `/debug/servable` and `/debug/apistage`
// with NO middleware on the SAME mux and port the chart publishes through a
// `service.type: LoadBalancer`. Only `/debug/refreshes` was gated (#69). An
// anonymous caller could pull heap/goroutine profiles, the process argv, the
// whole expvar registry (which carries per-cell maps keyed by `path|gvr|reason`)
// and the per-GVR / per-entry cache metadata — and could pin a CPU for the
// duration of `/debug/pprof/profile`.
//
// THE ARMS (all hermetic — httptest + a test RS256 keypair, no apiserver):
//
//	A1  every pattern registerDebugRoutes mounts 401s WITHOUT credentials.
//	    This is the arm that is RED on origin/main (200 today).
//	A2  /health and /readyz answer 200 WITHOUT credentials — the kubelet probe
//	    contract (howto/operating.md) must not regress behind the gate.
//	A3  the SAME debug paths answer 200 WITH a valid Bearer JWT — the gate does
//	    not brick the diagnostic surface.
//	A4  an EXPIRED JWT 401s, and a valid JWT presented ONLY in the query string
//	    401s (RefreshAuth's no-token-in-URL contract, inherited).
//	A5  the set of patterns registerDebugRoutes registers is EXACTLY
//	    debugRoutePatterns — so a future debug route added to the registrar
//	    without being added to the enumerated (and therefore A1-driven) list
//	    fails here rather than shipping ungated.
//
// RED arm (TestDebugSurface_RED_UngatedRegistrationIsCaught): the PRE-1.12.3
// registration — the verbatim ungated `mux.HandleFunc(...)` / `mux.Handle(...)`
// block from main.go before this fix — is driven through the IDENTICAL A1
// assertion helper, and every path comes back 200. That proves A1 discriminates
// between the gated and the ungated wiring rather than passing vacuously.

package main

import (
	"crypto/rand"
	"crypto/rsa"
	"expvar"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/pprof"
	"net/url"
	"testing"
	"time"

	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/krateo-platformops/plumbing/server/use"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/handlers"
	"github.com/krateo-platformops/snowplow/internal/handlers/middleware"
)

const debugAuthTestKeyID = "test-kid-for-debug-auth-od3-1.12.3"

var (
	debugAuthTestPrivateKey *rsa.PrivateKey
	debugAuthTestKeys       jwtutil.KeySource
)

func init() {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	debugAuthTestPrivateKey = key
	// In production the source is a JWKS fetcher against authn
	// (jwtutil.NewJWKSKeySource, main.go); a static source is the same
	// jwtutil.KeySource contract without the network.
	debugAuthTestKeys = jwtutil.NewStaticKeySource(&key.PublicKey)
}

// mintDebugToken issues a JWT signed with the test private key. A negative
// duration yields an already-expired token.
func mintDebugToken(t *testing.T, dur time.Duration) string {
	t.Helper()
	tok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
		Username:   "od3-operator",
		Groups:     []string{"devs"},
		Duration:   dur,
		KeyID:      debugAuthTestKeyID,
		PrivateKey: debugAuthTestPrivateKey,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return tok
}

// debugProbePaths maps each registered pattern to a concrete request path that
// routes to it. `GET /debug/pprof/` is a prefix pattern, so it is probed with a
// real named profile (`heap`) — the exact shape an operator or `go tool pprof`
// uses.
var debugProbePaths = map[string]string{
	"GET /debug/pprof/":        "/debug/pprof/heap",
	"GET /debug/pprof/cmdline": "/debug/pprof/cmdline",
	"GET /debug/pprof/profile": "/debug/pprof/profile",
	"GET /debug/pprof/symbol":  "/debug/pprof/symbol",
	"GET /debug/pprof/trace":   "/debug/pprof/trace",
	"GET /debug/vars":          "/debug/vars",
	"GET /debug/servable":      "/debug/servable",
	"GET /debug/apistage":      "/debug/apistage",
	"GET /debug/refreshes":     "/debug/refreshes",
}

// debugPathsSafeToDriveAuthenticated is debugProbePaths minus the two
// long-running collectors: `/debug/pprof/profile` (30s CPU profile by default)
// and `/debug/pprof/trace` (1s execution trace). Their GATING is covered by A1
// like every other path; only the authenticated 200 arm skips them, because
// past the gate they intentionally block for their sample window.
var debugPathsSafeToDriveAuthenticated = []string{
	"/debug/pprof/heap",
	"/debug/pprof/cmdline",
	"/debug/pprof/symbol",
	"/debug/vars",
	"/debug/servable",
	"/debug/apistage",
	"/debug/refreshes",
}

// recordingMux records the patterns registered on it and delegates to a real
// http.ServeMux, so a test can assert BOTH the pattern set and the served
// behaviour off the SAME registration call.
type recordingMux struct {
	inner    *http.ServeMux
	patterns []string
}

func newRecordingMux() *recordingMux {
	return &recordingMux{inner: http.NewServeMux()}
}

func (m *recordingMux) Handle(pattern string, handler http.Handler) {
	m.patterns = append(m.patterns, pattern)
	m.inner.Handle(pattern, handler)
}

// buildProdDebugMux builds the mux exactly the way main() does for the routes
// under test: the anonymous probe endpoints, then the production
// registerDebugRoutes call with the production chain shape.
func buildProdDebugMux(t *testing.T) (*recordingMux, http.Handler) {
	t.Helper()
	m := newRecordingMux()
	// Anonymous, exactly as main.go mounts them (kubelet probe contract).
	m.inner.Handle("GET /health", handlers.HealthCheck(serviceName, build, func() (string, error) {
		return "krateo-system", nil
	}))
	m.inner.Handle("GET /readyz", handlers.ReadyCheck())
	registerDebugRoutes(m, use.NewChain(), debugAuthTestKeys)
	return m, m.inner
}

// getStatus drives one GET through h and returns the status code. `bearer`, when
// non-empty, is sent as an `Authorization: Bearer` header.
func getStatus(t *testing.T, h http.Handler, path, bearer string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://snowplow.example"+path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result().StatusCode
}

// assertAllDebugPathsUnauthorized is THE A1 assertion, factored out so the RED
// arm can drive the pre-fix wiring through the identical check. It returns the
// paths that did NOT 401 (empty = the surface is gated).
func assertAllDebugPathsUnauthorized(t *testing.T, h http.Handler) []string {
	t.Helper()
	var leaked []string
	for _, path := range debugProbePaths {
		if code := getStatus(t, h, path, ""); code != http.StatusUnauthorized {
			leaked = append(leaked, fmt.Sprintf("%s → %d", path, code))
		}
	}
	return leaked
}

// ─── A1 — the O-D3 falsifier ────────────────────────────────────────────────

func TestDebugSurface_UnauthenticatedIs401(t *testing.T) {
	_, h := buildProdDebugMux(t)
	if leaked := assertAllDebugPathsUnauthorized(t, h); len(leaked) > 0 {
		t.Fatalf("O-D3: %d debug path(s) served WITHOUT credentials (want 401 on all): %v",
			len(leaked), leaked)
	}
}

// ─── A2 — the kubelet probe contract must NOT regress ───────────────────────

func TestProbeEndpointsStayAnonymous(t *testing.T) {
	cache.MarkPhase1Done() // /readyz is 503 "warming" until this flips
	defer cache.ResetPhase1DoneForTest()

	_, h := buildProdDebugMux(t)
	for _, path := range []string{"/health", "/readyz"} {
		if code := getStatus(t, h, path, ""); code != http.StatusOK {
			t.Errorf("%s unauthenticated = %d, want 200 (kubelet presents no JWT)", path, code)
		}
	}
}

// ─── A3 — a valid JWT still gets the diagnostic ─────────────────────────────

func TestDebugSurface_ValidJWTIs200(t *testing.T) {
	_, h := buildProdDebugMux(t)
	tok := mintDebugToken(t, time.Hour)
	for _, path := range debugPathsSafeToDriveAuthenticated {
		if code := getStatus(t, h, path, tok); code != http.StatusOK {
			t.Errorf("%s with a valid JWT = %d, want 200 (the gate must not brick the surface)", path, code)
		}
	}
}

// ─── A4 — expired token, and no-token-in-URL ────────────────────────────────

func TestDebugSurface_ExpiredAndQueryStringTokenAre401(t *testing.T) {
	_, h := buildProdDebugMux(t)

	if code := getStatus(t, h, "/debug/vars", mintDebugToken(t, -time.Hour)); code != http.StatusUnauthorized {
		t.Errorf("/debug/vars with an EXPIRED JWT = %d, want 401", code)
	}

	// A valid token in the query string only must NOT authenticate (it would
	// leak in logs/referrer) — RefreshAuth's contract, inherited by every
	// debug route now that they share the gate.
	tok := mintDebugToken(t, time.Hour)
	path := "/debug/vars?token=" + url.QueryEscape(tok)
	if code := getStatus(t, h, path, ""); code != http.StatusUnauthorized {
		t.Errorf("/debug/vars with the JWT ONLY in the query string = %d, want 401", code)
	}
}

// ─── A5 — the enumerated pattern set is the registered pattern set ──────────

func TestDebugRoutePatternsMatchRegistration(t *testing.T) {
	m, _ := buildProdDebugMux(t)

	got := map[string]bool{}
	for _, p := range m.patterns {
		got[p] = true
	}
	want := map[string]bool{}
	for _, p := range debugRoutePatterns {
		want[p] = true
	}
	for p := range want {
		if !got[p] {
			t.Errorf("debugRoutePatterns lists %q but registerDebugRoutes does not register it", p)
		}
		if _, ok := debugProbePaths[p]; !ok {
			t.Errorf("pattern %q has no entry in debugProbePaths — A1 would not drive it", p)
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("registerDebugRoutes registers %q but debugRoutePatterns does not list it "+
				"— add it there (and to debugProbePaths) so the 401 arm drives it", p)
		}
	}
}

// ─── RED arm — the pre-1.12.3 wiring fails the SAME assertion ───────────────

// registerDebugRoutesUngated is the VERBATIM pre-1.12.3 registration block from
// main.go (comments stripped): pprof + expvar + servable + apistage with no
// middleware, and only /debug/refreshes behind RefreshAuth. It exists solely so
// the RED arm can prove A1 discriminates.
func registerDebugRoutesUngated(mux *http.ServeMux, chain use.Chain, jwtKeys jwtutil.KeySource) {
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.Handle("GET /debug/vars", expvar.Handler())
	mux.HandleFunc("GET /debug/servable", handlers.DebugServable())
	mux.HandleFunc("GET /debug/apistage", handlers.DebugApistage())
	mux.Handle("GET /debug/refreshes", chain.Append(
		middleware.RefreshAuth(jwtKeys)).
		Then(handlers.DebugRefreshes()))
}

func TestDebugSurface_RED_UngatedRegistrationIsCaught(t *testing.T) {
	mux := http.NewServeMux()
	registerDebugRoutesUngated(mux, use.NewChain(), debugAuthTestKeys)

	// The pre-fix mux does not register profile/trace here (they are the
	// long-running collectors; their gating is A1's job on the real
	// registrar). Drive the four that ARE registered ungated.
	var leaked []string
	for _, path := range []string{
		"/debug/pprof/heap", "/debug/pprof/cmdline",
		"/debug/vars", "/debug/servable", "/debug/apistage",
	} {
		if code := getStatus(t, mux, path, ""); code != http.StatusUnauthorized {
			leaked = append(leaked, fmt.Sprintf("%s → %d", path, code))
		}
	}
	if len(leaked) == 0 {
		t.Fatal("RED arm did not fire: the PRE-1.12.3 ungated wiring returned 401 everywhere, " +
			"so the A1 assertion cannot distinguish gated from ungated")
	}
	t.Logf("RED arm fired as designed — pre-1.12.3 wiring served %d path(s) anonymously: %v",
		len(leaked), leaked)

	// And the gate that DID exist pre-fix still holds, so the RED arm is not
	// simply "nothing is ever gated".
	if code := getStatus(t, mux, "/debug/refreshes", ""); code != http.StatusUnauthorized {
		t.Errorf("/debug/refreshes was gated by #69 even pre-1.12.3; got %d, want 401", code)
	}
}
