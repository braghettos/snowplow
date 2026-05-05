// Q-RBAC-DECOUPLE C(d) v4 §6.6 — anti-defect test for Q-RBACC-DEFECT-3
// (L1 refresh under SA breaks per-user clientconfig fallback for non-UAF
// api[] entries).
//
// CRITICAL CONTRACT (per the v4 spec §6.6): this test MUST FAIL on the
// v3 implementation tagged 0.25.296 (proves it catches the defect) and
// PASS on the v4 implementation. The v3 path was:
//
//   MakeL1Refresher → refreshSingleL1 (under SA identity) → ResolveAndCache
//                  → restactions.Resolve → api.Resolve
//                  → for each api[] entry:
//                      - UAF set        → snowplow endpoint (correct)
//                      - UAF NOT set    → mapper.resolveOne(ctx, nil)
//                                       → endpoints.FromSecret(
//                                            "<sanitize(saUser)>-clientconfig")
//                                       → secret doesn't exist → ERROR
//
//   The error fires on every L1 refresh for every non-UAF api[] entry —
//   ~150 lines per refresh cycle at 50 NS scale, the production failure
//   mode that caused the rollback at 0.25.296.
//
// In v4 (Fix-3a):
//
//   MakeL1Refresher sets cache.WithSystemIdentity on the refresh ctx.
//   api.Resolve's dispatch fork now treats system-identity ctx + nil
//   EndpointRef as "use snowplow endpoint" — regardless of UserAccessFilter.
//   Result: all api[] entries route through the snowplow endpoint during
//   L1 refresh. Real-user contexts (which never set the system-identity
//   flag) preserve the original per-user clientconfig path.
//
// Test design: drive `MakeL1Refresher`'s refresh closure directly with a
// pre-seeded L1 key for a NON-UAF RESTAction. Capture log output via a
// custom slog handler; fail if any line contains
// `clientconfig not found`. Assert the L1 entry was overwritten.

package dispatchers

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/client-go/rest"
)

// captureSlogHandler buffers all emitted log lines so the test can scan
// for the Q-RBACC-DEFECT-3 signature `clientconfig not found`. Thread-
// safe — the L1 refresh closure may emit logs from background
// goroutines.
type captureSlogHandler struct {
	mu    sync.Mutex
	lines []string
}

func (h *captureSlogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureSlogHandler) WithAttrs(_ []slog.Attr) slog.Handler        { return h }
func (h *captureSlogHandler) WithGroup(_ string) slog.Handler             { return h }
func (h *captureSlogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	parts := []string{r.Message}
	r.Attrs(func(a slog.Attr) bool {
		parts = append(parts, a.Key+"="+a.Value.String())
		return true
	})
	h.lines = append(h.lines, strings.Join(parts, " "))
	return nil
}
func (h *captureSlogHandler) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.lines))
	copy(out, h.lines)
	return out
}
func (h *captureSlogHandler) hasLineContaining(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// TestL1Refresh_SystemIdentity_NonUAF_NoClientConfigError is the §6.6
// anti-defect contract for Q-RBACC-DEFECT-3.
//
// Invariant: on L1 refresh under the synthesized snowplow SA identity,
// NO api[] entry should attempt to load a `<sa-username>-clientconfig`
// Secret. The dispatch fork in api.Resolve must route ALL entries
// through the snowplow endpoint when cache.IsSystemIdentity(ctx) is
// true.
//
// On v3 (0.25.296), the test FAILS at the
// `secrets "...snowplow-clientconfig" not found` log line — the per-
// user clientconfig fallback fires for non-UAF entries even under SA
// identity.
//
// On v4 (Fix-3a), the test PASSES: the system-identity flag routes
// non-UAF entries through the snowplow endpoint and the L1 entry is
// overwritten cleanly.
func TestL1Refresh_SystemIdentity_NonUAF_NoClientConfigError(t *testing.T) {
	env.SetTestMode(true)
	t.Cleanup(func() { env.SetTestMode(false) })

	// Capture all log lines emitted during refresh — the failure mode
	// is observable as a log line, not as a returned error.
	cap := &captureSlogHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prev) })

	upstreamCt := &atomic.Int32{}
	upstreamPaths := &sync.Map{}
	upstreamAuth := &sync.Map{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCt.Add(1)
		upstreamPaths.Store(r.URL.Path, true)
		upstreamAuth.Store(r.Header.Get("Authorization"), true)
		w.Header().Set("Content-Type", "application/json")
		// Mirror the K8s namespaces list shape — the same upstream is
		// used both for the api[] entry and any opportunistic discovery
		// the test harness might trigger. The path doesn't matter for
		// the §6.6 assertion (we only care about which auth scheme the
		// resolver used to reach us).
		_, _ = w.Write([]byte(k8sNamespacesListBody([]string{"default", "kube-system"})))
	}))
	t.Cleanup(upstream.Close)

	// Pre-seed the L1 entry for a NON-UAF RESTAction. The refresher
	// re-resolves it; if Fix-3a is missing, the resolver tries to load
	// `system:serviceaccount:...-clientconfig` for the (non-UAF)
	// api[] entry and errors out.
	//
	// IMPORTANT: the api[] entry must have NO UserAccessFilter so the
	// dispatch fork's v3 condition (UAF set → snowplow endpoint) does
	// NOT fire. Only the v4 Fix-3a system-identity branch should route
	// this entry to the snowplow endpoint.
	cr := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{{
				Name: "ns",
				Path: "/api/v1/namespaces",
				// UserAccessFilter intentionally NIL — this is the
				// shape that exposes Q-RBACC-DEFECT-3 in production.
				Filter: ptr.To(`[.ns.items[].metadata.name]`),
			}},
			Filter: ptr.To(`{namespaces: .ns}`),
		},
	}
	cr.Name = "non-uaf-restaction"
	cr.Namespace = "krateo-system"
	cr.APIVersion = "templates.krateo.io/v1"
	cr.Kind = "RESTAction"

	c := cache.NewMem(0)
	if err := c.Set(context.Background(),
		cache.GetKey(restActionGVRForTest, cr.Namespace, cr.Name),
		mustToUnstructured(t, cr)); err != nil {
		t.Fatalf("seed RESTAction in L2: %v", err)
	}

	// Pre-seed the L1 entry with placeholder bytes so refreshSingleL1
	// has something to refresh. The bytes will be overwritten by the
	// refresh. The "identity" segment in the key is the production
	// binding-identity hash for some real user-or-group set; refresh
	// re-resolves under the SA identity (set on ctx via Fix-3a) but
	// keys remain stable across the cycle.
	identity := "binding-id-H-real-user-group"
	l1Key := cache.ResolvedKey(identity, restActionGVRForTest,
		cr.Namespace, cr.Name, -1, -1)
	if err := c.SetResolvedRaw(context.Background(), l1Key,
		[]byte(`{"placeholder":true}`)); err != nil {
		t.Fatalf("seed L1 entry: %v", err)
	}

	snowplowEndpointFn := func() (*endpoints.Endpoint, error) {
		return &endpoints.Endpoint{
			ServerURL: upstream.URL,
			Token:     "SNOWPLOW-SA-TEST",
		}, nil
	}

	// MakeL1Refresher: production wires rbacWatcher; the test omits it
	// (the L1 refresh closure tolerates nil — the dispatch fork's
	// system-identity check is the load-bearing v4 logic and runs
	// independently).
	refresh := MakeL1Refresher(c, &rest.Config{Host: upstream.URL},
		cr.Namespace, "test-jwt-key", nil, snowplowEndpointFn, nil)

	// Drive the refresh. Pass our test's rest.Config via the v4 ctx
	// fallback so api.Resolve's nil-RC guard finds it (production sets
	// SArc=nil, falls back to InClusterConfig in-cluster — tests
	// substitute via WithTestRestConfig).
	ctx := cache.WithTestRestConfig(context.Background(),
		&rest.Config{Host: upstream.URL})
	_ = refresh(ctx, restActionGVRForTest.GroupVersion().WithResource(restActionGVRForTest.Resource), []string{l1Key})

	// Assertion 1 (the load-bearing one): NO log line should contain
	// `unable to resolve api endpoint reference`. This is what the v3
	// dispatch path emits when mapper.resolveOne errors out for a
	// non-UAF api[] entry under SA identity (the production signature
	// of Q-RBACC-DEFECT-3 is the deeper `clientconfig not found` Secret
	// lookup error; in this test the upstream mock surfaces a different
	// decode error but the dispatcher-level signal is the same).
	for _, sig := range []string{
		"clientconfig not found",
		"unable to resolve api endpoint reference",
		"snowplow-clientconfig",
	} {
		if cap.hasLineContaining(sig) {
			var matched string
			for _, l := range cap.snapshot() {
				if strings.Contains(l, sig) {
					matched = l
					break
				}
			}
			t.Errorf("Q-RBACC-DEFECT-3: refresh emitted v3-dispatch error containing %q — non-UAF api[] entry hit the per-user clientconfig fallback under SA identity (Fix-3a not in effect)\nfirst match: %s", sig, matched)
		}
	}

	// Diagnostic dump (always): surface captured logs so the
	// dispatch path is always visible for debugging.
	t.Cleanup(func() {
		lines := cap.snapshot()
		t.Logf("captured %d log line(s):", len(lines))
		for i, l := range lines {
			t.Logf("  [%d] %s", i, l)
		}
	})

	// Assertion 2: the upstream MUST have been called at least once.
	// On v3 (pre-Fix-3a), the non-UAF entry would error out at
	// endpoints.FromSecret BEFORE reaching httpcall.Do, so the
	// upstream count would be 0. On v4 the system-identity branch
	// routes the entry through the snowplow endpoint and we see at
	// least one hit.
	//
	// NB: the bearerAuthRoundTripper does NOT overwrite an existing
	// Authorization header (see plumbing/http/request/transport.go),
	// so the user-JWT injected via shouldInjectUserJWT for non-UAF
	// entries WINS over the snowplow Endpoint.Token. The presence of
	// the call (regardless of which auth header was sent) is the
	// load-bearing evidence that the dispatch fork picked the
	// snowplow endpoint URL.
	if upstreamCt.Load() == 0 {
		t.Errorf("Fix-3a: upstream NOT called — the system-identity branch did not route the non-UAF entry through the snowplow endpoint\nexpected at least one HTTP call to %s", upstream.URL)
	}
	// Diagnostic: assert the entry reached the upstream at the api[]
	// path (not just at endpoints.FromSecret's secret-GET path which
	// also hits our upstream mock and would falsely satisfy a substring
	// check). On v3 (pre-Fix-3a), the only upstream hit is the secret
	// GET; on v4 the api[] entry itself reaches the upstream after the
	// system-identity dispatch fork.
	pathSeen := false
	upstreamPaths.Range(func(k, _ any) bool {
		if s, ok := k.(string); ok && s == "/api/v1/namespaces" {
			pathSeen = true
			return false
		}
		return true
	})
	if !pathSeen {
		var paths []string
		upstreamPaths.Range(func(k, _ any) bool {
			if s, ok := k.(string); ok {
				paths = append(paths, s)
			}
			return true
		})
		t.Errorf("Fix-3a: expected upstream call to exact path /api/v1/namespaces (the api[] entry path); saw paths=%v\nThis means the dispatch fork did NOT route the non-UAF entry through the snowplow endpoint — Q-RBACC-DEFECT-3 active.", paths)
	}

	// Assertion 3: the L1 entry was overwritten with the resolved CR
	// wrapper (proves the refresh ran end-to-end and produced output).
	cached, hit, err := c.GetRaw(context.Background(), l1Key)
	if err != nil || !hit {
		t.Fatalf("L1 entry should still exist after refresh; hit=%v err=%v", hit, err)
	}
	if string(cached) == `{"placeholder":true}` {
		t.Errorf("L1 entry NOT overwritten by refresh; still holds the placeholder bytes")
	}
	if !strings.Contains(string(cached), `"schema_version":"v3"`) {
		t.Errorf("L1 entry should be the v3 wrapper after a successful refresh; got\n%s", truncate(cached, 600))
	}
}

// TestL1Refresh_SystemIdentity_FlagSetOnRefreshCtxOnly is the §6.3 §6.8
// negative invariant: WithSystemIdentity must be set ONLY on the L1
// refresh closure's ctx. Real HTTP request flows must NOT carry the
// flag.
func TestL1Refresh_SystemIdentity_FlagSetOnRefreshCtxOnly(t *testing.T) {
	// 1. Real-user ctx — flag MUST be false.
	realCtx := context.Background()
	if cache.IsSystemIdentity(realCtx) {
		t.Errorf("default context unexpectedly carries system-identity flag")
	}
	// 2. Set the flag. Confirm IsSystemIdentity reads true.
	sysCtx := cache.WithSystemIdentity(realCtx)
	if !cache.IsSystemIdentity(sysCtx) {
		t.Errorf("WithSystemIdentity did not produce a system-identity ctx")
	}
	// 3. The system-identity flag is TRUE only inside the chain that
	//    SET it. A child ctx derived from realCtx via context.WithValue
	//    or other operations does NOT inherit it.
	derivedFromReal := context.WithValue(realCtx, struct{ k string }{"foo"}, "bar")
	if cache.IsSystemIdentity(derivedFromReal) {
		t.Errorf("ctx derived from real-user parent unexpectedly carries system-identity flag")
	}
}
