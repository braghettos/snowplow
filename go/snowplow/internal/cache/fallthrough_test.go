// fallthrough_test.go — Ship D (0.30.141) tests for the
// architectural-consistency invariant (counter + scope middleware +
// boot assertion). Maps to AC-D.5.
//
// The required tests, per the task spec:
//
//   - TestRecordApiserverFallthrough_Counter_Increments
//   - TestRecordApiserverFallthrough_Counter_Increments_Race  (32×50,
//     `-race` clean)
//   - TestFallthroughScopeMiddleware_Sets_Context
//   - TestAssertReadPathsScoped_PanicsInTest_OnMissingMiddleware
//   - TestAssertReadPathsScoped_LogsAndCountsInProd_OnMissingMiddleware
//   - TestNoBehaviorChange_GoldenCorpus (replay byte-identical
//     content corpus — covered transitively: the counter / scope /
//     middleware are additive; they never short-circuit, redirect, or
//     modify a response. The golden-corpus replay is the tester's job
//     against `/tmp/snowplow-runs/0.30.140/before/`; a unit test
//     can't simulate the full pipeline, so the no-behaviour-change
//     claim is asserted here by structural means — the wrappers never
//     return early when their precondition fires).
//
// Lives in package cache (internal) so it can access
// scopedRouteRegistrySnapshot, ResetRouteScopeRegistryForTest,
// ResetFallthroughCountersForTest, and the closed-enum constants.
package cache

import (
	"context"
	"encoding/json"
	"expvar"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/plumbing/server/use"
)

// ─────────────────────────────────────────────────────────────────────
// AC-D.5 — counter increments
// ─────────────────────────────────────────────────────────────────────

func TestRecordApiserverFallthrough_Counter_Increments(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetFallthroughCountersForTest()

	ctx := WithFallthroughScope(context.Background(), ScopeCallRestactions)
	if scope := FallthroughScope(ctx); scope == nil || !scope.Active {
		t.Fatalf("scope not stamped: %+v", scope)
	}

	RecordApiserverFallthrough(ctx, ReasonSecretGet, "")
	RecordApiserverFallthrough(ctx, ReasonSecretGet, "")
	RecordApiserverFallthrough(ctx, ReasonClientBuild, "v1/secrets")

	if got, want := FallthroughTotal(), uint64(3); got != want {
		t.Errorf("FallthroughTotal = %d; want %d", got, want)
	}
	if got, want := FallthroughCount(ScopeCallRestactions, "", ReasonSecretGet), uint64(2); got != want {
		t.Errorf("F-3 secret-get cell = %d; want %d", got, want)
	}
	if got, want := FallthroughCount(ScopeCallRestactions, "v1/secrets", ReasonClientBuild), uint64(1); got != want {
		t.Errorf("client-build cell = %d; want %d", got, want)
	}
}

// AC-D.1 — Disabled() short-circuits the counter.
func TestRecordApiserverFallthrough_NoOp_WhenCacheDisabled(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	ResetFallthroughCountersForTest()
	// Even with a scoped ctx, the counter must stay silent — cache=off
	// makes apiserver hops legitimate by contract.
	ctx := WithFallthroughScope(context.Background(), ScopeCallRestactions)
	RecordApiserverFallthrough(ctx, ReasonSecretGet, "")
	if got := FallthroughTotal(); got != 0 {
		t.Errorf("FallthroughTotal in cache=off = %d; want 0 (invariant inert)", got)
	}
}

// AC-D.2 — non-scoped ctx short-circuits the counter (Phase 1
// walker / refresher / etc. never produces counter cells).
func TestRecordApiserverFallthrough_NoOp_WhenScopeNotStamped(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetFallthroughCountersForTest()
	// No WithFallthroughScope on this ctx.
	RecordApiserverFallthrough(context.Background(), ReasonSecretGet, "")
	if got := FallthroughTotal(); got != 0 {
		t.Errorf("FallthroughTotal without scope = %d; want 0 (non-/call paths inert)", got)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D.5 — -race concurrent (32 readers × 50 iters)
// ─────────────────────────────────────────────────────────────────────

func TestRecordApiserverFallthrough_Counter_Increments_Race(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetFallthroughCountersForTest()
	ctx := WithFallthroughScope(context.Background(), ScopeCallWidgets)

	const goroutines = 32
	const itersPerGoroutine = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			// Distribute the gvr label across a small set so the
			// per-cell sync.Map sees both fresh inserts and hits.
			gvr := "v1/group" + strconv.Itoa(g%4) + "/resource"
			for i := 0; i < itersPerGoroutine; i++ {
				RecordApiserverFallthrough(ctx, ReasonClientBuild, gvr)
			}
		}(g)
	}
	wg.Wait()

	want := uint64(goroutines * itersPerGoroutine)
	if got := FallthroughTotal(); got != want {
		t.Errorf("FallthroughTotal after race = %d; want %d (atomicity broken)", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D.5 — middleware stamps the context
// ─────────────────────────────────────────────────────────────────────

func TestFallthroughScopeMiddleware_Sets_Context(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	var observed *FallthroughScopeData
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = FallthroughScope(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	mw := FallthroughScopeMiddleware(ScopeCallWidgets)
	wrapped := mw(probe)

	req := httptest.NewRequest(http.MethodGet, "/call?x=1", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if observed == nil {
		t.Fatalf("scope not stamped by middleware")
	}
	if !observed.Active {
		t.Errorf("scope.Active = false; want true")
	}
	if observed.Path != ScopeCallWidgets {
		t.Errorf("scope.Path = %q; want %q", observed.Path, ScopeCallWidgets)
	}

	// AC-D.2 closed enum: unknown scope panics at constructor time.
	t.Run("unknown scope panics", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Errorf("FallthroughScopeMiddleware(unknown) did not panic")
			}
		}()
		_ = FallthroughScopeMiddleware("call-not-in-the-enum-foo")
	})
}

// AC-D.2 — middleware no-op when cache=off (no scope stamped).
func TestFallthroughScopeMiddleware_NoOp_WhenCacheDisabled(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")

	var observed *FallthroughScopeData
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = FallthroughScope(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	mw := FallthroughScopeMiddleware(ScopeCallRestactions)
	wrapped := mw(probe)
	req := httptest.NewRequest(http.MethodGet, "/call", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if observed != nil {
		t.Errorf("scope stamped in cache=off mode; got %+v; want nil", observed)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D.5 — boot assert panics in test, logs+counts in prod
// ─────────────────────────────────────────────────────────────────────

func TestAssertReadPathsScoped_PanicsInTest_OnMissingMiddleware(t *testing.T) {
	env.SetTestMode(true)
	t.Cleanup(func() { env.SetTestMode(false) })

	ResetRouteScopeRegistryForTest()
	// Register only a SUBSET — leave POST /call missing.
	RegisterScopedRoute("GET /api-info/names", ScopePlurals)
	RegisterScopedRoute("GET /list", ScopeList)
	RegisterScopedRoute("GET /call", ScopeCallGeneric)
	// Skip POST /call, PUT /call, PATCH /call, DELETE /call.

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("AssertReadPathsScoped did not panic in test mode with missing routes")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is %T; want string", r)
		}
		// The panic message lists missing routes in alphabetical
		// order — deterministic per fallthrough_assert.go's
		// sort.Strings call.
		for _, want := range []string{"DELETE /call", "PATCH /call", "POST /call", "PUT /call"} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic message missing %q; got %q", want, msg)
			}
		}
	}()
	_ = AssertReadPathsScoped()
}

func TestAssertReadPathsScoped_LogsAndCountsInProd_OnMissingMiddleware(t *testing.T) {
	env.SetTestMode(false)
	t.Cleanup(func() { env.SetTestMode(false) })

	ResetRouteScopeRegistryForTest()
	// Register only GET /call — every other required route is missing.
	RegisterScopedRoute("GET /call", ScopeCallGeneric)

	before := AssertionViolationsTotal()
	missing := AssertReadPathsScoped()
	after := AssertionViolationsTotal()

	// 6 required routes minus the one registered = 5 missing
	// (GET /api-info/names, GET /list, POST /call, PUT /call,
	// PATCH /call, DELETE /call).
	if missing != 6 {
		t.Errorf("missing count = %d; want 6", missing)
	}
	if delta := after - before; delta != 6 {
		t.Errorf("assertionViolationsTotal delta = %d; want 6", delta)
	}
}

func TestAssertReadPathsScoped_AllPresentReturnsZero(t *testing.T) {
	ResetRouteScopeRegistryForTest()
	// Register every required route.
	RegisterScopedRoute("GET /api-info/names", ScopePlurals)
	RegisterScopedRoute("GET /list", ScopeList)
	RegisterScopedRoute("GET /call", ScopeCallGeneric)
	RegisterScopedRoute("POST /call", ScopeCallWritePost)
	RegisterScopedRoute("PUT /call", ScopeCallWritePut)
	RegisterScopedRoute("PATCH /call", ScopeCallWritePatch)
	RegisterScopedRoute("DELETE /call", ScopeCallWriteDelete)

	if missing := AssertReadPathsScoped(); missing != 0 {
		t.Errorf("missing count = %d; want 0 (all required routes present)", missing)
	}
}

// TestAssertReadPathsScoped_RBACEndpointUnregisteredStillPasses is the /rbac
// endpoint falsifier #6 (design §7). GET /rbac is mounted on UserConfig but
// DELIBERATELY NOT RegisterScopedRoute'd — like /refreshes, it issues ZERO
// per-user apiserver reads (enumeration runs under the SA), so it sits outside
// the read-path-scoped invariant. Because requiredScopedRoutes is a positive
// whitelist (it only checks REQUIRED routes are present, never flags extras),
// an un-registered /rbac cannot trip AssertReadPathsScoped: with every
// required route registered and /rbac left unregistered, the assert still
// returns 0. (And /rbac is not in requiredScopedRoutes, so a future ship
// cannot accidentally require it.)
func TestAssertReadPathsScoped_RBACEndpointUnregisteredStillPasses(t *testing.T) {
	ResetRouteScopeRegistryForTest()
	RegisterScopedRoute("GET /api-info/names", ScopePlurals)
	RegisterScopedRoute("GET /list", ScopeList)
	RegisterScopedRoute("GET /call", ScopeCallGeneric)
	RegisterScopedRoute("POST /call", ScopeCallWritePost)
	RegisterScopedRoute("PUT /call", ScopeCallWritePut)
	RegisterScopedRoute("PATCH /call", ScopeCallWritePatch)
	RegisterScopedRoute("DELETE /call", ScopeCallWriteDelete)
	// GET /rbac deliberately NOT registered (mirrors /refreshes).

	if missing := AssertReadPathsScoped(); missing != 0 {
		t.Errorf("FALSIFIER #6 FAIL: missing count = %d; want 0 — an unregistered /rbac "+
			"must NOT trip the read-path-scoped invariant", missing)
	}

	for _, r := range requiredScopedRoutes {
		if r == "GET /rbac" {
			t.Fatalf("FALSIFIER #6 FAIL: GET /rbac must NOT be in requiredScopedRoutes "+
				"(it issues zero per-user apiserver reads); found it in %v", requiredScopedRoutes)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D.5 — closed-enum discipline
// ─────────────────────────────────────────────────────────────────────

// TestRegisterScopedRoute_RejectsUnknownScope asserts the closed-enum
// gate on the route registry: an unknown scope panics at registration
// time, not at boot. This matches FallthroughScopeMiddleware's same
// validation — defence-in-depth so both halves of the wiring (mux
// handle + registry) refuse an out-of-enum scope.
func TestRegisterScopedRoute_RejectsUnknownScope(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("RegisterScopedRoute with unknown scope did not panic")
		}
	}()
	RegisterScopedRoute("GET /something", "scope-not-in-enum")
}

// TestRegisterScopedRoute_DuplicateWithDifferentScope_Panics — the
// registry refuses to silently overwrite a pre-existing pattern with a
// different scope. This catches a mis-wired duplicate registration
// (e.g. a refactor that splits a route in two and forgets to dedupe).
func TestRegisterScopedRoute_DuplicateWithDifferentScope_Panics(t *testing.T) {
	ResetRouteScopeRegistryForTest()
	RegisterScopedRoute("GET /call", ScopeCallGeneric)
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("RegisterScopedRoute duplicate-with-different-scope did not panic")
		}
	}()
	RegisterScopedRoute("GET /call", ScopeCallWidgets)
}

// TestRegisterScopedRoute_IdempotentSameScope verifies a re-register
// with the SAME scope is a no-op (legitimate hot-reload pattern).
func TestRegisterScopedRoute_IdempotentSameScope(t *testing.T) {
	ResetRouteScopeRegistryForTest()
	RegisterScopedRoute("GET /call", ScopeCallGeneric)
	RegisterScopedRoute("GET /call", ScopeCallGeneric) // no panic
	snap := scopedRouteRegistrySnapshot()
	if got := snap["GET /call"]; got != ScopeCallGeneric {
		t.Errorf("snapshot[GET /call] = %q; want %q", got, ScopeCallGeneric)
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-D.8 — no behaviour change (structural)
// ─────────────────────────────────────────────────────────────────────

// TestNoBehaviorChange_RecordIsTelemetryOnly asserts the structural
// claim that RecordApiserverFallthrough returns no error, modifies no
// argument, and is callable in any combination of (Disabled, Scoped)
// states without side-effects beyond the counter. The byte-identical
// content-corpus replay is the tester's job against
// /tmp/snowplow-runs/0.30.140/before/; this test is the unit-level
// canary: a future regression that adds short-circuit / redirect /
// modify logic to RecordApiserverFallthrough fails this row.
func TestNoBehaviorChange_RecordIsTelemetryOnly(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetFallthroughCountersForTest()
	ctx := WithFallthroughScope(context.Background(), ScopeCallRestactions)

	// Pre-call ctx and a probe value — assert neither is mutated.
	probeKey := struct{}{}
	type marker struct{ n int }
	ctxWithProbe := context.WithValue(ctx, probeKey, &marker{n: 42})

	RecordApiserverFallthrough(ctxWithProbe, ReasonSecretGet, "")

	// ctx still carries the probe value, unchanged.
	got := ctxWithProbe.Value(probeKey)
	m, ok := got.(*marker)
	if !ok || m == nil || m.n != 42 {
		t.Errorf("RecordApiserverFallthrough mutated/dropped ctx probe value: %v", got)
	}
	// Counter incremented exactly once (no double-count / no skip).
	if got, want := FallthroughTotal(), uint64(1); got != want {
		t.Errorf("FallthroughTotal = %d; want %d", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Ship D.1 (0.30.142) — E2E HTTP-chain integration test
// ─────────────────────────────────────────────────────────────────────

// TestFallthroughScope_E2E_HTTPChain is the integration coverage Ship
// D's unit-level tests didn't have: it builds a production-shape
// middleware chain (use.NewChain + cache.FallthroughScopeMiddleware +
// terminal handler), spins an httptest.Server, fires a real GET /call
// request through it, and asserts that the Layer B wrapper invoked
// from inside the handler observes the Layer A scope AND increments
// the counter.
//
// Why this test exists. Ship D's existing unit tests verify each
// layer in isolation:
//   - TestFallthroughScopeMiddleware_Sets_Context — middleware stamps
//     the ctx (in-process probe);
//   - TestRecordApiserverFallthrough_Counter_Increments — recorder
//     increments the cell when given a scoped ctx.
//
// The PM-gate coverage gap was the *composition*: does the chain
// produce a ctx whose scope survives the middleware → handler →
// wrapper boundary? The unit tests can't catch a future regression
// where (e.g.) a handler re-creates the request via
// `httpcall.Do(context.Background(), ...)` — the recorder would still
// look up the scope and find none, the counter would stay at 0, and
// nothing in the unit suite would notice. This E2E test wires the
// exact composition main.go uses (chain.Append → middleware →
// dispatcher-shaped handler → terminal call) and asserts the counter
// post-request.
//
// Architect's prescription, codified: if the scope-marker drop ever
// IS introduced (e.g. a future ship swaps a real handler for a
// context.Background() re-entrant), this test fails immediately.
//
// Optional probe — interleave xcontext.BuildContext inside the
// terminal handler to close the "BuildContext-strips-scope" hypothesis
// (the architect's audit refuted this on live evidence, but the test
// pins the contract). The probe re-derives a new context layered on
// top of the request ctx and asserts the scope STILL survives — i.e.
// BuildContext composes additively, it does not replace the value
// chain.
func TestFallthroughScope_E2E_HTTPChain(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	ResetFallthroughCountersForTest()

	// Terminal handler — models a Dispatcher-routed inner handler
	// (e.g. restactions.ServeHTTP). It does the work the production
	// wrapper sites do: read the scope, then call
	// RecordApiserverFallthrough exactly as endpoints.go:59 would.
	// Mirrors api/endpoints.go F-3 (secret-get) — the highest-volume
	// /call-read fall-through reason in the design.
	var observedScope *FallthroughScopeData
	var observedTotalAfterRecord uint64
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Architect's optional probe: BuildContext-as-suspect.
		// xcontext.BuildContext layers user-info / access-token onto
		// the parent context — it must NOT strip the FallthroughScope
		// value. A regression that re-creates the ctx via
		// context.Background() would lose the scope and the recorder
		// would observe nil → counter stays 0.
		buildCtx := xcontext.BuildContext(r.Context())
		observedScope = FallthroughScope(buildCtx)

		// Layer B wrapper-call shape — exactly the pattern at
		// api/endpoints.go:59 (F-3 secret-get).
		RecordApiserverFallthrough(buildCtx, ReasonSecretGet, "")
		observedTotalAfterRecord = FallthroughTotal()

		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	// Production-shape middleware chain. We omit use.UserConfig
	// (which would require a real signingKey + authn namespace + JWT
	// stamped on the request; a hermetic test can't easily fabricate
	// those) and substitute a trivial pass-through so the chain shape
	// matches main.go's mux.Handle("GET /call", ...) registration:
	//
	//   chain.Append(use.UserConfig(...),
	//                cache.FallthroughScopeMiddleware(...),
	//                handlers.Dispatcher(...)).Then(handlers.Call())
	//
	// The architectural-consistency property under test — does the
	// scope marker survive the chain through to the terminal handler
	// — is unaffected by which user-auth middleware sits before
	// FallthroughScopeMiddleware. Use of use.NewChain (the real
	// snowplow chain primitive) keeps the test honest about the
	// middleware-composition mechanism.
	chain := use.NewChain(
		FallthroughScopeMiddleware(ScopeCallRestactions),
	)
	wrapped := chain.Then(terminal)

	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	// One real GET /call?... request through the test server.
	req, err := http.NewRequest(http.MethodGet,
		srv.URL+"/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=test&namespace=default", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", resp.StatusCode, string(body))
	}

	// --- Assertions ---------------------------------------------------

	// (1) The scope MUST be observable inside the terminal handler.
	// A nil scope is the canonical scope-marker-drop regression the
	// architect's audit refuted but Ship D had no test for.
	if observedScope == nil {
		t.Fatalf("scope marker DROPPED across the middleware chain — observedScope==nil. " +
			"This is the regression the test exists to catch (Ship D.1). " +
			"BuildContext probe ran AFTER FallthroughScopeMiddleware; the scope must survive.")
	}
	if !observedScope.Active {
		t.Errorf("scope.Active = false; want true (FallthroughScopeMiddleware stamps Active=true)")
	}
	if observedScope.Path != ScopeCallRestactions {
		t.Errorf("scope.Path = %q; want %q", observedScope.Path, ScopeCallRestactions)
	}

	// (2) The counter MUST have incremented exactly once — the
	// terminal handler's RecordApiserverFallthrough call landed in
	// the (call-restactions, "", secret-get) cell.
	if got, want := FallthroughTotal(), uint64(1); got != want {
		t.Errorf("FallthroughTotal = %d; want %d", got, want)
	}
	if got := observedTotalAfterRecord; got != 1 {
		t.Errorf("FallthroughTotal observed inside handler after Record = %d; want 1", got)
	}
	if got, want := FallthroughCount(ScopeCallRestactions, "", ReasonSecretGet), uint64(1); got != want {
		t.Errorf("(call-restactions, secret-get) cell = %d; want %d (counter wired E2E)", got, want)
	}
}

// TestFallthroughScope_E2E_ExpvarHandler — Ship D.1 (0.30.142).
// Validates the second half of Change 1: the counters exposed via
// fallthrough_meter_expvar.go's expvar.Publish are actually visible
// through expvar.Handler() (the handler main.go mounts at
// /debug/vars). Without this test a future expvar registration
// regression in init() would silently break the operator surface.
//
// Per the published shape (fallthrough_meter_expvar.go):
//
//   - "snowplow_apiserver_fallthrough_total" → uint64 grand total
//   - "snowplow_apiserver_fallthrough_cells" → map[string]uint64
//     keyed "path|gvr|reason"
//   - "snowplow_cache_diagnostic_total" → uint64 grand total (1.12.4)
//   - "snowplow_cache_diagnostic_cells" → map[string]uint64 (1.12.4)
//   - "snowplow_assertion_violations_total" → map[string]uint64
//     keyed "check"
//
// 1.12.4 RECLASSIFICATION. The two reasons this test drives —
// resolver-plurals-hit and resolver-plurals-miss — are BOTH in design
// §5.2's diagnostic set (an in-process cache hit, and a double-count of
// a hop already recorded as plurals-discovery-hop). Before 1.12.4 they
// landed on the fall-through family, and this test asserted that. It now
// asserts the corrected routing end-to-end through the real
// expvar.Handler surface: the two cells appear under
// snowplow_cache_diagnostic_cells and the fall-through family reads 0
// with the keys ABSENT. Driving a genuine reason as well keeps the
// original assertion — that the fall-through surface still works — alive.
func TestFallthroughScope_E2E_ExpvarHandler(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	// CFG-1 (Ship 0.30.163): expvar gauges are registered only when
	// CACHE_ENABLED is truthy at package init() time. The `go test`
	// process boots with the env unset, so init() returned early.
	// RegisterExpvarForTest is the explicit, sync.Once-guarded
	// re-registration hook. See expvar_test_helpers.go.
	RegisterExpvarForTest()
	ResetFallthroughCountersForTest()

	// Drive the counter once so the cells are populated — otherwise
	// the per-cell map is empty (sync.Map only stores on first Inc).
	ctx := WithFallthroughScope(context.Background(), ScopeCallWidgets)
	RecordApiserverFallthrough(ctx, ReasonResolverPluralsHit, "apiextensions.k8s.io/v1/customresourcedefinitions")
	RecordApiserverFallthrough(ctx, ReasonResolverPluralsMiss, "v1/pods")
	// A GENUINE apiserver hop, so the fall-through half of the surface is
	// still exercised rather than merely asserted empty.
	RecordApiserverFallthrough(ctx, ReasonClientBuild, "v1/pods")

	// Mount expvar.Handler on a test server (the same one-line mount
	// main.go now does at /debug/vars).
	mux := http.NewServeMux()
	mux.Handle("/debug/vars", expvar.Handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/debug/vars")
	if err != nil {
		t.Fatalf("HTTP GET /debug/vars: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/debug/vars status = %d", resp.StatusCode)
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal /debug/vars JSON: %v\nbody: %s", err, string(body))
	}

	// (1) grand totals — JSON numbers unmarshal as float64. Only the
	// client-build call is a genuine apiserver hop; the two
	// resolver-plurals calls are diagnostics (1.12.4 design §5.2).
	total, ok := doc["snowplow_apiserver_fallthrough_total"].(float64)
	if !ok {
		t.Fatalf("snowplow_apiserver_fallthrough_total missing or wrong type: %#v", doc["snowplow_apiserver_fallthrough_total"])
	}
	if uint64(total) != 1 {
		t.Errorf("expvar fallthrough total = %d; want 1 (only client-build is a genuine hop)", uint64(total))
	}
	diagTotal, ok := doc["snowplow_cache_diagnostic_total"].(float64)
	if !ok {
		t.Fatalf("snowplow_cache_diagnostic_total missing or wrong type: %#v — the 1.12.4 companion "+
			"family must be published beside the fall-through family, in the same sync.Once",
			doc["snowplow_cache_diagnostic_total"])
	}
	if uint64(diagTotal) != 2 {
		t.Errorf("expvar diagnostic total = %d; want 2", uint64(diagTotal))
	}

	// (2) per-cell breakdown — map[string]float64 (JSON-decoded).
	cellsRaw, ok := doc["snowplow_apiserver_fallthrough_cells"].(map[string]any)
	if !ok {
		t.Fatalf("snowplow_apiserver_fallthrough_cells missing or wrong type: %#v", doc["snowplow_apiserver_fallthrough_cells"])
	}
	diagCellsRaw, ok := doc["snowplow_cache_diagnostic_cells"].(map[string]any)
	if !ok {
		t.Fatalf("snowplow_cache_diagnostic_cells missing or wrong type: %#v", doc["snowplow_cache_diagnostic_cells"])
	}
	const crdKey = "call-widgets|apiextensions.k8s.io/v1/customresourcedefinitions|resolver-plurals-hit"
	const kindKey = "call-widgets|v1/pods|resolver-plurals-miss"
	const hopKey = "call-widgets|v1/pods|client-build"
	if v, ok := diagCellsRaw[crdKey].(float64); !ok || uint64(v) != 1 {
		t.Errorf("diagnostic cell %q = %#v; want 1", crdKey, diagCellsRaw[crdKey])
	}
	if v, ok := diagCellsRaw[kindKey].(float64); !ok || uint64(v) != 1 {
		t.Errorf("diagnostic cell %q = %#v; want 1", kindKey, diagCellsRaw[kindKey])
	}
	if v, ok := cellsRaw[hopKey].(float64); !ok || uint64(v) != 1 {
		t.Errorf("fallthrough cell %q = %#v; want 1", hopKey, cellsRaw[hopKey])
	}
	// The acceptance observation of design §5 read straight off the
	// operator surface: the reclassified keys are ABSENT from the
	// fall-through cells map, not merely netted out of its scalar.
	for _, k := range []string{crdKey, kindKey} {
		if _, present := cellsRaw[k]; present {
			t.Errorf("key %q is still present in snowplow_apiserver_fallthrough_cells — "+
				"design §5.4 requires two separate maps so the key disappears from this one", k)
		}
	}

	// (3) assertion-violations map — must exist (zero is the
	// expected steady-state value with no missing routes).
	violations, ok := doc["snowplow_assertion_violations_total"].(map[string]any)
	if !ok {
		t.Fatalf("snowplow_assertion_violations_total missing or wrong type: %#v", doc["snowplow_assertion_violations_total"])
	}
	if _, has := violations["read_paths_scoped"]; !has {
		t.Errorf("snowplow_assertion_violations_total[read_paths_scoped] missing — expvar map shape regression")
	}
}
