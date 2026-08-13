// userconfig_test.go — Ship D.3 / 0.30.151. AC-D3.6 + AC-D3.14.
//
// AC-D3.6 — TestUserConfig_ByteEquivalence_CacheMiss is the §3.1 gate
// from docs/ship-d3-clientconfig-secret-cache-middleware-design.md.
// It pins the byte-equivalence claim between snowplow-local
// `middleware.UserConfig` and upstream `use.UserConfig` on every
// branch that exits BEFORE `rest.InClusterConfig()` (auth-header
// missing, invalid format, JWT-fail, JWT-expired) — i.e. the
// branches the cache-miss fallback has to preserve verbatim per the
// design §3 line-by-line comparison.
//
// The `rest.InClusterConfig`-and-after branches (InternalError on
// InClusterConfig fail, FromSecret success, FromSecret NotFound,
// FromSecret non-NotFound) cannot be exercised hermetically — they
// require a real in-cluster service account token mounted at the
// fixed path `/var/run/secrets/kubernetes.io/serviceaccount/token`.
// In CI / dev workstation, `rest.InClusterConfig()` returns
// `ErrNotInCluster` deterministically, which IS the branch we assert
// byte-identity for via TestUserConfig_ByteEquivalence_InClusterConfigFails.
//
// The cache-HIT branch (5.a) skips `rest.InClusterConfig()` entirely
// by design (refactor §2.2.b). TestUserConfig_CacheHit_BypassesApiserver
// proves the ctx values land in the next handler without any
// apiserver round-trip.
//
// AC-D3.14 — TestUserConfigMirror_PlumbingVersionPin fails if
// go.mod's pinned plumbing version drifts from
// `PinnedPlumbingVersion`, forcing a re-audit of upstream
// `server/use/userconfig.go` line-by-line before bump.
package middleware_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/krateo-platformops/plumbing/server/use"
	"k8s.io/client-go/kubernetes/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/handlers/middleware"
)

const (
	testKeyID   = "test-kid-for-snowplow-d3-byte-equivalence-2026-05-20"
	testAuthnNS = "krateo-system"
	testUsername = "alice"
)

// testPrivateKey / testKeys are the asymmetric keypair these tests sign/verify
// with (RS256, replacing the old HS256 shared secret). Generated once at package
// init; a failure here means the test binary itself is broken, so a panic is
// appropriate.
//
// testKeys is a StaticKeySource rather than the JWKSKeySource production uses:
// both satisfy jwtutil.KeySource, and the static one keeps the byte-equivalence
// comparison against upstream use.UserConfig hermetic (no HTTP server in the
// loop). The JWKS fetch/cache/rotation semantics are covered upstream in
// plumbing's jwtutil/jwks_test.go.
var (
	testPrivateKey *rsa.PrivateKey
	testKeys       jwtutil.KeySource
)

func init() {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	testPrivateKey = key
	testKeys = jwtutil.NewStaticKeySource(&key.PublicKey)
}

// helperMakeToken issues a valid JWT signed with the test private key.
func helperMakeToken(t *testing.T, username string, expires time.Duration) string {
	t.Helper()
	tok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
		Username:   username,
		Groups:     []string{"krateo:admins"},
		Duration:   expires,
		KeyID:      testKeyID,
		PrivateKey: testPrivateKey,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return tok
}

// readBody drains the body and returns it (response.Encode encodes
// JSON; we compare bodies byte-for-byte).
func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

// serveOnce drives a single request through the given middleware and
// returns the response status + body.
func serveOnce(t *testing.T, mw func(http.Handler) http.Handler, req *http.Request) (int, []byte) {
	t.Helper()
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	chain := use.NewChain(mw)
	wrapped := chain.Then(terminal)

	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// ─────────────────────────────────────────────────────────────────────
// AC-D3.6 — byte-equivalence on the pre-InClusterConfig branches
// ─────────────────────────────────────────────────────────────────────

// TestUserConfig_ByteEquivalence_CacheMiss compares snowplow-local
// `middleware.UserConfig` against upstream `use.UserConfig` on every
// branch of the upstream function that exits before
// `rest.InClusterConfig()` is reached. These branches are the
// observable "cache-miss fallback" surface — on any of these the
// upstream re-call is never issued, so cache-miss behaviour MUST be
// byte-identical to upstream. A divergence on any branch (different
// status code, different body bytes, different header set) fails
// this test and the Ship D.3 ship.
func TestUserConfig_ByteEquivalence_CacheMiss(t *testing.T) {
	// Cache disabled — guarantees `FromInformerSecret` returns
	// (zero, false, nil) for any input. We never reach the cache-hit
	// branch even if the request were well-formed enough to reach
	// the cache lookup.
	t.Setenv("CACHE_ENABLED", "false")

	upstream := use.UserConfig(testKeys, testAuthnNS)
	snowplow := middleware.UserConfig(testKeys, testAuthnNS)

	cases := []struct {
		name    string
		mkReq   func(t *testing.T) *http.Request
		// expectedStatus is the upstream status code; we cross-check
		// that snowplow returns the same.
		expectedStatus int
	}{
		{
			name: "missing_authorization_header",
			mkReq: func(t *testing.T) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/call?x=1", nil)
			},
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name: "invalid_authorization_format_no_bearer",
			mkReq: func(t *testing.T) *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/call?x=1", nil)
				r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
				return r
			},
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name: "invalid_authorization_format_no_space",
			mkReq: func(t *testing.T) *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/call?x=1", nil)
				r.Header.Set("Authorization", "BearerToken123")
				return r
			},
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name: "jwt_validate_garbage_token",
			mkReq: func(t *testing.T) *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/call?x=1", nil)
				r.Header.Set("Authorization", "Bearer not.a.real.jwt.token")
				return r
			},
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name: "jwt_validate_wrong_signing_key",
			mkReq: func(t *testing.T) *http.Request {
				// Token signed with a DIFFERENT key — Validate rejects.
				otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatalf("generating RSA key: %v", err)
				}
				wrongTok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
					Username:   "bob",
					Groups:     []string{"krateo:users"},
					Duration:   1 * time.Hour,
					KeyID:      testKeyID,
					PrivateKey: otherKey,
				})
				if err != nil {
					t.Fatalf("CreateToken: %v", err)
				}
				r := httptest.NewRequest(http.MethodGet, "/call?x=1", nil)
				r.Header.Set("Authorization", "Bearer "+wrongTok)
				return r
			},
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name: "jwt_validate_expired",
			mkReq: func(t *testing.T) *http.Request {
				// Negative duration ⇒ ExpiresAt is in the past ⇒
				// jwtutil.Validate returns ErrTokenExpired.
				expiredTok := helperMakeToken(t, "alice", -1*time.Hour)
				r := httptest.NewRequest(http.MethodGet, "/call?x=1", nil)
				r.Header.Set("Authorization", "Bearer "+expiredTok)
				return r
			},
			expectedStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upStatus, upBody := serveOnce(t, upstream, tc.mkReq(t))
			snStatus, snBody := serveOnce(t, snowplow, tc.mkReq(t))

			if upStatus != tc.expectedStatus {
				t.Fatalf("upstream status = %d; want %d (test pre-condition broken)",
					upStatus, tc.expectedStatus)
			}
			if snStatus != upStatus {
				t.Errorf("status divergence: snowplow=%d upstream=%d", snStatus, upStatus)
			}
			if !bytes.Equal(snBody, upBody) {
				t.Errorf("body divergence:\n  snowplow=%q\n  upstream=%q",
					string(snBody), string(upBody))
			}
		})
	}
}

// TestUserConfig_ByteEquivalence_InClusterConfigFails covers the
// upstream :44-48 branch. Outside a cluster `rest.InClusterConfig()`
// returns `ErrNotInCluster` deterministically; both middlewares must
// surface the same 500 + same body wrap "unable to create in cluster
// config: <ErrNotInCluster>". On the snowplow side this branch is
// reached ONLY on a cache MISS (5.b) — the cache=off env-flag here
// guarantees we hit the miss branch.
func TestUserConfig_ByteEquivalence_InClusterConfigFails(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	// Clear any inherited env (be defensive: if a developer's shell
	// has KUBERNETES_SERVICE_HOST set from an active kubeconfig,
	// InClusterConfig might progress to the token-file read instead
	// of returning ErrNotInCluster cleanly).
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	tok := helperMakeToken(t, testUsername, 1*time.Hour)

	mkReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/call?x=1", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		return r
	}

	upstream := use.UserConfig(testKeys, testAuthnNS)
	snowplow := middleware.UserConfig(testKeys, testAuthnNS)

	upStatus, upBody := serveOnce(t, upstream, mkReq())
	snStatus, snBody := serveOnce(t, snowplow, mkReq())

	if upStatus != http.StatusInternalServerError {
		t.Fatalf("upstream status = %d; want 500 (test precondition: outside-cluster)",
			upStatus)
	}
	if snStatus != upStatus {
		t.Errorf("status divergence: snowplow=%d upstream=%d", snStatus, upStatus)
	}
	if !bytes.Equal(snBody, upBody) {
		t.Errorf("body divergence:\n  snowplow=%q\n  upstream=%q",
			string(snBody), string(upBody))
	}
	// Sanity check on the body wrap text — both should include the
	// upstream "unable to create in cluster config" prefix verbatim.
	if !strings.Contains(string(snBody), "unable to create in cluster config") {
		t.Errorf("snowplow body missing upstream error wrap: %q", string(snBody))
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D3.3 / AC-D3.4 / AC-D3.5 / AC-D3.N — counter fires on cache-miss
// AND scope marker survives the new middleware
// ─────────────────────────────────────────────────────────────────────

// TestUserConfig_CacheMiss_RecordsFallthrough proves the cache-miss
// branch (5.b) increments `snowplow_apiserver_fallthrough_total`
// once per request on the (call-*, "", secret-get) cell. This is
// the AC-D3.3 + AC-D3.N empirical recovery rationale: post-D.3 the
// internal counter and apiserver-side `secrets,GET` rate have the
// same denominator.
//
// AC-D3.5 discharge: this test also covers the scope-marker survival
// invariant (cross-ship invariant (f) per design §6). Composition:
//
//   chain.Append(cache.FallthroughScopeMiddleware(ScopeCallGeneric),
//                middleware.UserConfig(...))
//      .Then(terminal)
//
// The assertion `FallthroughCount(ScopeCallGeneric, "", ReasonSecretGet) == 1`
// can only pass if the scope marker stamped by the first middleware
// is still present in the ctx that the cache-miss branch hands to
// `RecordApiserverFallthrough` — i.e. the new middleware does not
// strip the scope ctx value. A regression that re-creates the
// request via `context.Background()` (the canonical scope-marker
// drop) would land the increment in the empty-scope cell and this
// test would fail.
func TestUserConfig_CacheMiss_RecordsFallthrough(t *testing.T) {
	// Cache must be ENABLED for the counter to fire (the recorder
	// short-circuits when cache.Disabled()==true). We then force the
	// soft-miss branch via the namespace-mismatch guard in
	// `api.FromInformerSecret`: start the informer against a
	// different namespace, then ask the middleware (which is wired
	// for `testAuthnNS`) for the user's clientconfig — the cache
	// adapter returns (zero, false, nil) on the NS-mismatch path,
	// the middleware records the fallthrough, then tries
	// `rest.InClusterConfig()` (which fails outside the cluster →
	// 500).
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	cache.ResetFallthroughCountersForTest()
	cache.ResetSecretsSnapshotForTest()
	cache.ResetSecretsInformerForTest()
	t.Cleanup(func() {
		cache.ResetSecretsSnapshotForTest()
		cache.ResetSecretsInformerForTest()
	})

	// Informer wired for a DIFFERENT namespace — guarantees soft
	// miss inside `api.FromInformerSecret` (the NS-match guard at
	// endpoints_cache.go:110-117 returns served=false).
	cli := fake.NewSimpleClientset()
	restore := cache.SetSecretsClientForTest(cli)
	t.Cleanup(restore)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cache.StartSecretsInformer(ctx, nil, "different-ns"); err != nil {
		t.Fatalf("StartSecretsInformer(different-ns): %v", err)
	}

	tok := helperMakeToken(t, testUsername, 1*time.Hour)

	// Wrap our middleware with the scope marker, exactly as main.go
	// does for /call — the recorder reads the scope from ctx.
	mw := middleware.UserConfig(testKeys, testAuthnNS)
	scoped := cache.FallthroughScopeMiddleware(cache.ScopeCallGeneric)
	chain := use.NewChain(scoped, mw)
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := chain.Then(terminal)

	r := httptest.NewRequest(http.MethodGet, "/call?x=1", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, r)

	// We expect a 500 (InClusterConfig fails outside the cluster),
	// but the cache-miss counter MUST have fired BEFORE that — per
	// the middleware's order: RecordApiserverFallthrough is invoked
	// BEFORE rest.InClusterConfig.
	if rec.Code != http.StatusInternalServerError {
		t.Logf("(diagnostic) status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got, want := cache.FallthroughCount(cache.ScopeCallGeneric, "", cache.ReasonSecretGet), uint64(1); got != want {
		t.Errorf("(call-generic, secret-get) cell = %d; want %d (counter wired on cache-miss path)",
			got, want)
	}
	if got, want := cache.FallthroughTotal(), uint64(1); got != want {
		t.Errorf("FallthroughTotal = %d; want %d", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Cache-HIT success path — middleware-only (no upstream comparison
// because upstream has no cache equivalent)
// ─────────────────────────────────────────────────────────────────────

// TestUserConfig_CacheHit_BypassesApiserver verifies that on a cache
// HIT (5.a), the middleware:
//
//   (1) does NOT call rest.InClusterConfig() (the deferred-until-miss
//       refactor) — proven by the fact that no KUBERNETES_SERVICE_HOST
//       is set, yet the request succeeds with a 200 from the next
//       handler.
//   (2) does NOT fire the fallthrough counter (cache-miss-only firing
//       per AC-D3.4).
//   (3) DOES stamp xcontext.UserInfo + UserConfig + AccessToken on
//       the request ctx so downstream handlers see the same shape as
//       upstream's success path.
func TestUserConfig_CacheHit_BypassesApiserver(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	// TEST_MODE=true suppresses xcontext.UserConfig's non-test-mode
	// override of ServerURL to "https://kubernetes.default.svc" —
	// we want to read the cache's verbatim Endpoint.
	t.Setenv("TEST_MODE", "true")
	cache.ResetFallthroughCountersForTest()
	cache.ResetSecretsSnapshotForTest()
	cache.ResetSecretsInformerForTest()
	t.Cleanup(func() {
		cache.ResetSecretsSnapshotForTest()
		cache.ResetSecretsInformerForTest()
	})

	// Populate the secrets cache with a single `<user>-clientconfig`
	// secret for testUsername. The middleware's secret-name
	// computation is `kubeutil.MakeDNS1123Compatible(username)+"-clientconfig"`.
	// For "alice" the DNS1123 form is "alice" verbatim.
	seedSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testAuthnNS,
			Name:      testUsername + "-clientconfig",
		},
		Data: map[string][]byte{
			"server-url": []byte("https://alice.example/k8s"),
			"token":      []byte("alice-bearer-token-from-cache"),
		},
	}
	cli := fake.NewSimpleClientset(seedSecret)
	restore := cache.SetSecretsClientForTest(cli)
	t.Cleanup(restore)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cache.StartSecretsInformer(ctx, nil, testAuthnNS); err != nil {
		t.Fatalf("StartSecretsInformer: %v", err)
	}
	if !cache.SecretsCacheServable() {
		t.Fatalf("SecretsCacheServable=false after StartSecretsInformer; want true")
	}
	if cache.SecretsSnapshotLoad() == nil {
		t.Fatalf("SecretsSnapshotLoad returned nil; want populated snapshot")
	}

	// Capture the ctx values the next handler observes.
	var observedAccessToken string
	var observedUser jwtutil.UserInfo
	var observedEndpoint endpoints.Endpoint
	var observedEndpointErr error
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedAccessToken, _ = xcontext.AccessToken(r.Context())
		observedUser, _ = xcontext.UserInfo(r.Context())
		observedEndpoint, observedEndpointErr = xcontext.UserConfig(r.Context())
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	tok := helperMakeToken(t, testUsername, 1*time.Hour)
	chain := use.NewChain(middleware.UserConfig(testKeys, testAuthnNS))
	wrapped := chain.Then(terminal)

	r := httptest.NewRequest(http.MethodGet, "/call?x=1", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, r)

	// (1) The request succeeded — no rest.InClusterConfig() call was
	// made (we'd have gotten a 500 otherwise).
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (cache-HIT path must NOT call InClusterConfig). body=%q",
			rec.Code, rec.Body.String())
	}
	// (2) The fallthrough counter MUST be zero — cache hits don't
	// fire the recorder.
	if got, want := cache.FallthroughTotal(), uint64(0); got != want {
		t.Errorf("FallthroughTotal = %d; want %d (cache-HIT must not record fallthrough)",
			got, want)
	}
	// (3) Context values landed correctly.
	if observedAccessToken != tok {
		t.Errorf("observed access token mismatch")
	}
	if observedUser.Username != testUsername {
		t.Errorf("observed user.Username = %q; want %q", observedUser.Username, testUsername)
	}
	if observedEndpointErr != nil {
		t.Errorf("xcontext.UserConfig returned error: %v", observedEndpointErr)
	}
	if observedEndpoint.ServerURL != "https://alice.example/k8s" {
		t.Errorf("observed endpoint.ServerURL = %q; want %q (TEST_MODE=true should suppress override)",
			observedEndpoint.ServerURL, "https://alice.example/k8s")
	}
	if observedEndpoint.Token != "alice-bearer-token-from-cache" {
		t.Errorf("observed endpoint.Token mismatch: got %q", observedEndpoint.Token)
	}
}

// TestUserConfigMirror_PlumbingVersionPin — AC-D3.14.
//
// The constant `middleware.PinnedPlumbingVersion` records the
// upstream version this middleware was transcribed from. If `go.mod`
// pins a different `plumbing` version, this test fails — forcing the
// operator to re-audit upstream `server/use/userconfig.go`
// line-by-line and update the constant after verifying no behaviour
// drift.
func TestUserConfigMirror_PlumbingVersionPin(t *testing.T) {
	pinned := middleware.PinnedPlumbingVersion
	if pinned == "" {
		t.Fatal("PinnedPlumbingVersion is empty — set it to the upstream version this file was transcribed from")
	}

	goMod := findGoMod(t)
	data, err := os.ReadFile(goMod)
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	want := fmt.Sprintf("github.com/krateo-platformops/plumbing %s", pinned)
	if !bytes.Contains(data, []byte(want)) {
		t.Errorf("go.mod does not contain pinned %q.\nThe middleware was transcribed from plumbing %s; if you bump plumbing in go.mod, re-audit internal/handlers/middleware/userconfig.go against the new server/use/userconfig.go line-by-line and update PinnedPlumbingVersion.\nFirst 256 bytes of go.mod: %s",
			want, pinned, truncate(data, 256))
	}
}

// findGoMod walks up from CWD looking for go.mod. The test runs from
// the package directory; the repo root is 4 levels up.
func findGoMod(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := cwd
	for range 10 {
		candidate := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("go.mod not found walking up from %s", cwd)
	return ""
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// Compile-time check that the helper signatures wired above match
// the real package (catches a future signature change at upstream
// jwtutil during plumbing bumps before runtime).
var (
	_ = jwtutil.ErrTokenExpired
	_ = errors.Is
	_ = json.Marshal
)
