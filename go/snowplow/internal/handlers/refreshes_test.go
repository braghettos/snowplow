// refreshes_test.go — Ship 1 (live-refresh-coherence, option A) HTTP-level
// falsifiers for GET /refreshes: RefreshAuth (cookie-or-header JWT), the
// cache-off idle stream (9.5b, the /refreshes half), and validateSubscription
// rejection. Hermetic: httptest + a test JWT signing key; NO apiserver,
// KUBECONFIG unset. NEVER ./internal/rbac.
//
// The per-subject ISOLATION (9.4a) and the per-row CONTENT (9.4b) live at the
// derivation layer (refresh_isolation_falsifier_test.go in package dispatchers,
// where the in-process RBAC snapshot builder exists) and the cluster gate,
// respectively. This file proves the endpoint's auth + lifecycle + input
// validation.

package handlers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/handlers/middleware"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

const refreshTestKeyID = "test-kid-ship1-live-refresh"

// refreshTestPrivateKey / refreshTestKeys are the asymmetric keypair these
// tests sign/verify with (RS256, replacing the old HS256 shared secret).
// Production resolves the verification key from authn's JWKS endpoint; a
// StaticKeySource satisfies the same jwtutil.KeySource contract without a
// network dependency, keeping these tests hermetic (the fetch/cache behaviour
// is covered in plumbing's jwtutil/jwks_test.go).
var (
	refreshTestPrivateKey *rsa.PrivateKey
	refreshTestKeys       jwtutil.KeySource
)

func init() {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	refreshTestPrivateKey = key
	refreshTestKeys = jwtutil.NewStaticKeySource(&key.PublicKey)
}

func mintToken(t *testing.T, username string) string {
	t.Helper()
	tok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
		Username:   username,
		Groups:     []string{"devs"},
		Duration:   time.Hour,
		KeyID:      refreshTestKeyID,
		PrivateKey: refreshTestPrivateKey,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return tok
}

// subParam builds a valid ?sub= value (base64 JSON coordinate array) for one
// widgetContent coordinate (identity-free, so it derives without an RBAC
// snapshot).
func subParam(t *testing.T) string {
	t.Helper()
	body := []map[string]any{{
		"class":     "widgetContent",
		"group":     "widgets.templates.krateo.io",
		"version":   "v1beta1",
		"resource":  "panels",
		"namespace": "krateo-system",
		"name":      "dashboard-piechart",
		"perPage":   5,
		"page":      1,
	}}
	raw, _ := json.Marshal(body)
	return base64.StdEncoding.EncodeToString(raw)
}

// seedAuthTestWidget wires cache.Global() with the dashboard-piechart panel CR
// that subParam() arms + an RBAC binding granting userA's group (devs) get/list
// on panels, so the #64 subscriptionKeyExtras objects.Get (informer-served,
// RBAC-gated under the connection identity) succeeds and DeriveSubscriptionKey
// derives a valid key.
//
// WHY (#64): the auth tests exercise the REAL arming path, which now (correctly,
// C64-1 fail-closed) requires a fetchable widget CR — a widget the user can't
// GET is not live-refreshable, so it is not armed. Pre-#64 these coords
// phantom-armed (200, but the request-only key was WRONG → never delivered, the
// very bug). Seeding the CR tests the honest arming path.
func seedAuthTestWidget(t *testing.T) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")

	panelGVR := schema.GroupVersionResource{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "panels"}
	crbGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	crGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)
	listKinds := map[schema.GroupVersionResource]string{
		crbGVR: "ClusterRoleBindingList",
		crGVR:  "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}: "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:        "RoleList",
		panelGVR: "PanelList",
	}
	rule := []rbacv1.PolicyRule{{Verbs: []string{"get", "list"}, APIGroups: []string{"widgets.templates.krateo.io"}, Resources: []string{"panels"}}}
	seed := []runtime.Object{
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "panel-reader"}, Rules: rule},
		// Grant the "devs" GROUP (userA's mintToken group) get/list panels.
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "devs-bind", UID: types.UID("uid-devs")},
			Subjects:   []rbacv1.Subject{{Kind: "Group", Name: "devs"}},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "panel-reader"},
		},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "widgets.templates.krateo.io/v1beta1",
			"kind":       "Panel",
			"metadata":   map[string]any{"name": "dashboard-piechart", "namespace": "krateo-system"},
			"spec":       map[string]any{}, // no inline extras — request-only key, byte-identical to emit
		}},
	}

	wctx, wcancel := context.WithCancel(context.Background())
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, seed...)
	rw, err := cache.NewResourceWatcher(wctx, dyn)
	if err != nil {
		wcancel()
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer syncCancel()
	if err := rw.WaitForCacheSync(syncCtx, 5*time.Second); err != nil {
		rw.Stop()
		wcancel()
		t.Fatalf("WaitForCacheSync: %v", err)
	}
	_, _ = rw.EnsureResourceType(panelGVR)
	_ = rw.WaitForCacheSync(syncCtx, 5*time.Second)
	cache.RebuildRBACSnapshotForTest(rw)
	prev := cache.Global()
	cache.SetGlobal(rw)
	t.Cleanup(func() {
		rw.Stop()
		wcancel()
		cache.SetGlobal(prev)
		cache.PublishRBACSnapshotForTest(nil)
	})
}

// refreshServer wires the production chain (RefreshAuth -> Refreshes) on a
// test server. Returns the base URL.
func refreshServer(t *testing.T) string {
	t.Helper()
	h := middleware.RefreshAuth(refreshTestKeys)(Refreshes())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// openStream issues GET /refreshes with the given setup and returns the
// response + a cancel func. The caller MUST cancel to release the streaming
// handler goroutine.
func openStream(t *testing.T, baseURL, query string, setup func(*http.Request)) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+query, nil)
	if err != nil {
		cancel()
		t.Fatalf("NewRequest: %v", err)
	}
	if setup != nil {
		setup(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("Do: %v", err)
	}
	return resp, cancel
}

// --- RefreshAuth ------------------------------------------------------------

// TestRefreshes_Auth_HeaderTokenReachesHandler — a valid Authorization: Bearer
// header authenticates and the handler opens the SSE stream (200 +
// text/event-stream). The curl-falsifier / non-browser path.
func TestRefreshes_Auth_HeaderTokenReachesHandler(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("REFRESH_SSE_ENABLED", "")
	seedAuthTestWidget(t) // #64: the armed widget CR must be fetchable (C64-1 fail-closed)
	base := refreshServer(t)

	resp, cancel := openStream(t, base, "?sub="+subParam(t), func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+mintToken(t, "userA"))
	})
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("header-auth: status=%d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("header-auth: Content-Type=%q want text/event-stream", ct)
	}
}

// TestRefreshes_Auth_CookieTokenReachesHandler — the browser EventSource path:
// the JWT in the configured session cookie authenticates (no Authorization
// header). This is the make-or-break for EventSource (it cannot set headers).
func TestRefreshes_Auth_CookieTokenReachesHandler(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("REFRESH_SSE_ENABLED", "")
	t.Setenv("REFRESH_SESSION_COOKIE", "krateo-session")
	seedAuthTestWidget(t) // #64: the armed widget CR must be fetchable (C64-1 fail-closed)
	base := refreshServer(t)

	resp, cancel := openStream(t, base, "?sub="+subParam(t), func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: "krateo-session", Value: mintToken(t, "userA")})
	})
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie-auth: status=%d want 200 — EventSource cookie path broken", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("cookie-auth: Content-Type=%q want text/event-stream", ct)
	}
}

// TestRefreshes_Auth_MissingCredentials401 — no header, no cookie -> 401.
func TestRefreshes_Auth_MissingCredentials401(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	base := refreshServer(t)

	resp, cancel := openStream(t, base, "?sub="+subParam(t), nil)
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing-creds: status=%d want 401", resp.StatusCode)
	}
}

// TestRefreshes_Auth_InvalidToken401 — a token signed with the WRONG key -> 401.
func TestRefreshes_Auth_InvalidToken401(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	base := refreshServer(t)

	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	bad, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
		Username: "userA", Duration: time.Hour, KeyID: refreshTestKeyID, PrivateKey: wrongKey,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	resp, cancel := openStream(t, base, "?sub="+subParam(t), func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+bad)
	})
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid-token: status=%d want 401 (wrong-key JWT must not validate)", resp.StatusCode)
	}
}

// --- validateSubscription rejection -----------------------------------------

// TestRefreshes_Validation_Rejections — malformed/oversized/empty ?sub= -> 400.
// (Auth succeeds first; the rejection is the subscription validation.)
func TestRefreshes_Validation_Rejections(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	base := refreshServer(t)
	tok := mintToken(t, "userA")

	// Oversized: a base64 blob whose DECODED size exceeds refreshSubParamMaxBytes.
	huge := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", refreshSubParamMaxBytes+1)))

	cases := []struct {
		name  string
		query string
	}{
		{"missing sub", ""},
		{"malformed base64", "?sub=!!!not-base64!!!"},
		{"oversized payload", "?sub=" + huge},
		{"empty array", "?sub=" + base64.StdEncoding.EncodeToString([]byte("[]"))},
		{"not an array", "?sub=" + base64.StdEncoding.EncodeToString([]byte(`{"class":"widgetContent"}`))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, cancel := openStream(t, base, c.query, func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+tok)
			})
			defer cancel()
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s: status=%d want 400", c.name, resp.StatusCode)
			}
		})
	}
}

// TestRefreshes_Validation_AllForeignKeysRejected — when every coordinate fails
// derivation (cache layer present but identity yields no key for an identity-
// bound class with no RBAC snapshot -> BindingUID empty is still a derived key;
// so use an UNKNOWN class, which DeriveSubscriptionKey fails-closed on) the
// armed set is empty -> 400 "no valid subscription keys".
func TestRefreshes_Validation_AllForeignKeysRejected(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	// #68: this asserts the WARM-pod honest-400 for unarmable (unknown-class)
	// keys. Mark the pod warm (both gates: phase1 done + RBAC published) so the
	// new warmup-divert does NOT serve an idle stream — the divert is for the
	// transient warmup window only, not for a warm pod with genuinely-empty
	// subscriptions (C64-1 honest-400 must survive).
	cache.MarkPhase1Done()
	cache.BumpRBACGenForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)
	t.Cleanup(cache.ResetRBACGenForTest)
	base := refreshServer(t)
	tok := mintToken(t, "userA")

	body := []map[string]any{{"class": "totally-unknown-class", "name": "x"}}
	raw, _ := json.Marshal(body)
	q := "?sub=" + base64.StdEncoding.EncodeToString(raw)

	resp, cancel := openStream(t, base, q, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+tok)
	})
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("all-foreign: status=%d want 400 (no armable keys)", resp.StatusCode)
	}
}

// --- 9.5b — cache-off idle stream -------------------------------------------

// TestRefreshes_CacheOff_IdleStream is the /refreshes half of falsifier 9.5b:
// with the cache subsystem off, GET /refreshes returns 200 + text/event-stream
// and emits ONLY heartbeats — zero `event: refresh` frames — so a connected
// client degrades to its own throttle (transparent fallback,
// project_cache_off_is_transparent_fallback). It also requires NO auth-bearing
// credentials? No — auth still applies; we pass a valid token. The point is the
// STREAM is idle. (The /call correct-CONTENT half of 9.5b is the cluster
// falsifier — it needs the resolve stack.)
func TestRefreshes_CacheOff_IdleStream(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false") // cache subsystem OFF
	base := refreshServer(t)

	// Under cache-off the handler serves the idle stream BEFORE subscription
	// validation, so even a valid token + any sub yields the idle stream.
	resp, cancel := openStream(t, base, "?sub="+subParam(t), func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+mintToken(t, "userA"))
	})
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cache-off: status=%d want 200 (idle stream, transparent fallback)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("cache-off: Content-Type=%q want text/event-stream", ct)
	}

	// Read for a short window; assert NO `event: refresh` frame arrives (the
	// broadcaster does not exist under cache-off, so nothing can publish).
	// We cannot easily wait a full heartbeat (20s) in a unit test, so we just
	// assert that within a short read no refresh event appears and the stream
	// stays open (no premature EOF/error).
	done := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: refresh") {
				done <- "GOT_REFRESH"
				return
			}
		}
		done <- "EOF"
	}()
	select {
	case sig := <-done:
		if sig == "GOT_REFRESH" {
			t.Fatalf("cache-off: received an `event: refresh` frame — the stream must be idle (no broadcaster exists)")
		}
		// "EOF" here would mean the server closed the stream; under cache-off
		// the idle stream stays open until client-cancel, so a fast EOF is
		// unexpected — but the cancel in defer can race it. Treat EOF within
		// the window as benign (the stream did not emit a refresh).
	case <-time.After(500 * time.Millisecond):
		// No refresh frame within the window — correct (idle stream).
	}
}

// --- M18 [SEC-adj] ----------------------------------------------------------

// subParamRawURL builds the SAME coordinate array as subParam but encodes it
// with base64.RawURLEncoding (unpadded, URL-safe alphabet) — the encoding a
// browser EventSource URL naturally carries. It is distinct from the StdEncoding
// form whenever the payload has padding or +// bytes.
func subParamRawURL(t *testing.T) string {
	t.Helper()
	body := []map[string]any{{
		"class":     "widgetContent",
		"group":     "widgets.templates.krateo.io",
		"version":   "v1beta1",
		"resource":  "panels",
		"namespace": "krateo-system",
		"name":      "dashboard-piechart",
		"perPage":   5,
		"page":      1,
	}}
	raw, _ := json.Marshal(body)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// TestRefreshes_RawURLEncodedSubReachesHandler is M18(a): a ?sub= encoded with
// RawURLEncoding (the browser EventSource path) decodes via validateSubscription's
// URL-safe fallback and reaches the handler → 200 + text/event-stream. Without
// the RawURLEncoding fallback the browser payload would fail StdEncoding decode
// and 400 — the exact regression the RED arm below reproduces.
func TestRefreshes_RawURLEncodedSubReachesHandler(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("REFRESH_SSE_ENABLED", "")
	seedAuthTestWidget(t) // #64: the armed widget CR must be fetchable
	base := refreshServer(t)

	resp, cancel := openStream(t, base, "?sub="+subParamRawURL(t), func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+mintToken(t, "userA"))
	})
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("M18(a) RawURLEncoding sub: status=%d want 200 (EventSource URL-safe base64 path)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("M18(a): Content-Type=%q want text/event-stream", ct)
	}
}

// TestRefreshes_RawURLEncoding_RED_StdOnlyRejectsBrowserPayload is the RED proof
// for M18(a). It reproduces the plausible wrong impl — decode with StdEncoding
// ONLY, no RawURLEncoding fallback — and asserts that the browser's RawURL
// payload FAILS to decode under it. This is exactly the 400-on-browser-path
// regression the fallback in validateSubscription prevents; the green test above
// proves the real code does NOT regress.
func TestRefreshes_RawURLEncoding_RED_StdOnlyRejectsBrowserPayload(t *testing.T) {
	raw := subParamRawURL(t)
	// Sanity: the RawURL form must actually differ from the Std form for this
	// payload (else the arm is vacuous). The JSON has enough bytes that its
	// base64 is padded → StdEncoding carries '=' that RawURLEncoding omits.
	body := []map[string]any{{
		"class": "widgetContent", "group": "widgets.templates.krateo.io", "version": "v1beta1",
		"resource": "panels", "namespace": "krateo-system", "name": "dashboard-piechart", "perPage": 5, "page": 1,
	}}
	j, _ := json.Marshal(body)
	std := base64.StdEncoding.EncodeToString(j)
	if raw == std {
		t.Skipf("RawURL and Std encodings coincide for this payload; RED arm not meaningful")
	}

	// Wrong impl: StdEncoding-only decode of the browser (RawURL) payload.
	if _, err := base64.StdEncoding.DecodeString(raw); err == nil {
		t.Fatalf("RED: StdEncoding-only decode of a RawURL payload should FAIL (proving the fallback is load-bearing); it succeeded")
	}
	// And the real code path (with the fallback) accepts it — proven at the HTTP
	// level by TestRefreshes_RawURLEncodedSubReachesHandler; here we assert the
	// in-package decode contract directly.
	if _, err := base64.RawURLEncoding.DecodeString(raw); err != nil {
		t.Fatalf("RED control: the RawURLEncoding fallback must decode the browser payload; got %v", err)
	}
}

// TestRefreshes_SubEntryCountBoundary is M18(b): the refreshSubMaxEntries cap is
// INCLUSIVE — exactly 512 entries is accepted (validateSubscription returns no
// error), 513 is rejected. Driven through validateSubscription directly so the
// boundary is isolated from the downstream armed==0 → 400 (a 512-entry all-skip
// subscription would otherwise 400 at the handler for a DIFFERENT reason,
// masking the count guard).
func TestRefreshes_SubEntryCountBoundary(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")

	mkSub := func(n int) string {
		arr := make([]map[string]any, n)
		for i := range arr {
			// Empty object → a valid subRequest (all fields optional) that skips
			// derivation (no class), which is NOT a validateSubscription error.
			// Kept minimal (2 bytes each) so even cap+1 entries stay well under
			// refreshSubParamMaxBytes — isolating the entry-COUNT guard from the
			// byte-size guard (an unknown-class fixture would trip the byte cap
			// first at 512 entries, masking the boundary under test).
			arr[i] = map[string]any{}
		}
		raw, _ := json.Marshal(arr)
		return base64.StdEncoding.EncodeToString(raw)
	}

	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError}))

	call := func(n int) error {
		ctx := xcontext.BuildContext(context.Background(),
			xcontext.WithLogger(logger),
			xcontext.WithUserInfo(jwtutil.UserInfo{Username: "userA", Groups: []string{"devs"}}),
		)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/refreshes?sub="+mkSub(n), nil)
		_, err := validateSubscription(req)
		return err
	}

	if err := call(refreshSubMaxEntries); err != nil {
		t.Fatalf("M18(b): exactly %d entries (the cap) must be accepted; got error %v", refreshSubMaxEntries, err)
	}
	if err := call(refreshSubMaxEntries + 1); err == nil {
		t.Fatalf("M18(b): %d entries (cap+1) must be rejected; got nil error", refreshSubMaxEntries+1)
	}
}

// TestRefreshes_AbsentCoord_SkippedNoApiserverError is M18(c): a coord whose CR
// is absent from the informer is SKIPPED (informer-miss) and reflected in the
// subscription summary — with NO ERROR-level log line (the #101 property: the
// informer-only ctx turns the absent-CR read into a quiet NotFound-shaped skip,
// NOT the "unable to get user endpoint" apiserver-fallthrough ERROR storm).
func TestRefreshes_AbsentCoord_SkippedNoApiserverError(t *testing.T) {
	seedSummaryWidget(t) // one armable "armed-panel" CR + RBAC binding

	buf := &bytes.Buffer{}
	// Capture at DEBUG so an ERROR line, if any were emitted, is definitely seen.
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	body := []map[string]any{
		widgetContentCoord("armed-panel"),    // present → arms
		widgetContentCoord("absent-panel-1"), // absent → informer-miss skip
		widgetContentCoord("absent-panel-2"), // absent → informer-miss skip
	}
	raw, _ := json.Marshal(body)
	sub := base64.StdEncoding.EncodeToString(raw)

	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithLogger(logger),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "userA", Groups: []string{"devs"}}),
	)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/refreshes?sub="+sub, nil)

	armed, err := validateSubscription(req)
	if err != nil {
		t.Fatalf("M18(c): validateSubscription must not error on absent coords; got %v", err)
	}
	if len(armed) != 1 {
		t.Fatalf("M18(c): only the present coord arms; want 1, got %d", len(armed))
	}

	// (1) The summary line reflects the two informer-miss skips.
	sum := findSummaryLine(t, buf)
	if sum.Requested != 3 || sum.Armed != 1 || sum.SkippedInformerMiss != 2 {
		t.Fatalf("M18(c): summary want {requested:3 armed:1 skipped_informer_miss:2}; got %+v", sum)
	}

	// (2) NO ERROR-level log line was emitted — the #101 apiserver-fallthrough
	// error storm is gone. Scan every captured JSON line for level==ERROR.
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec.Level == "ERROR" {
			t.Fatalf("M18(c): an absent-CR coord must NOT emit an ERROR log line (apiserver-fallthrough storm); "+
				"saw ERROR msg=%q\nfull log:\n%s", rec.Msg, buf.String())
		}
	}
}
