// debug_routes.go — O-D3 (1.12.3): the whole /debug/* surface behind the same
// JWT gate /debug/refreshes already uses.
//
// WHY THIS FILE EXISTS
//
// The debug surface (`/debug/pprof/*`, `/debug/vars`, `/debug/servable`,
// `/debug/apistage`, `/debug/refreshes`) is mounted on the SAME mux and the
// SAME port the chart publishes through a `service.type: LoadBalancer`
// (helm/snowplow/values.yaml). Before 1.12.3 only `/debug/refreshes` was gated
// (#69); the other four were world-readable on that LB:
//
//   - `/debug/pprof/*` returns heap/CPU/goroutine profiles — goroutine dumps
//     carry stack frames and `cmdline` carries the process argv;
//   - `/debug/vars` returns the whole expvar registry, which includes
//     per-cell maps keyed by `path|gvr|reason` (fall-through cells,
//     `snowplow_dispatch_l1_lookups`) — a free map of the tenant's GVRs;
//   - `/debug/servable` and `/debug/apistage` return per-GVR / per-entry
//     cache METADATA (never resolved bodies — that structural guard stays),
//     which is still an unauthenticated inventory of the cluster's shape.
//
// `/debug/profile` and `/debug/trace` are additionally a trivial availability
// lever: an anonymous caller can pin a CPU for the profile duration.
//
// THE GATE IS THE EXISTING ONE. registerDebugRoutes wraps every debug route in
// `middleware.RefreshAuth(jwtKeys)` — the cookie-or-header stateless RS256
// validation `/debug/refreshes` has used since #69, chosen deliberately over
// `middleware.UserConfig` because it performs ZERO apiserver reads (no
// `<user>-clientconfig` Secret GET), so a diagnostic pull never perturbs the
// cache-respecting invariant it is being used to diagnose. Any valid Krateo JWT
// passes; missing/expired/invalid → 401, unfetchable JWKS → 503.
//
// NOT GATED (deliberate): `/health` and `/readyz` stay anonymous — they are the
// kubelet probe targets (howto/operating.md, "the chart ⇄ binary probe
// contract") and the kubelet presents no JWT. `/swagger/` is unchanged.
//
// TESTABILITY: the registration is a function rather than a straight-line block
// in main() so main_debug_auth_test.go can build the real mux and drive real
// requests through it — the same "extracted, named constructor" shape
// snowplowCORSOptions uses for the CORS contract (main_cors_test.go).

package main

import (
	"expvar"
	"net/http"
	"net/http/pprof"

	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/krateo-platformops/plumbing/server/use"
	"github.com/krateo-platformops/snowplow/internal/handlers"
	"github.com/krateo-platformops/snowplow/internal/handlers/middleware"
)

// debugRoutePatterns is the exact set of mux patterns registerDebugRoutes
// mounts, in registration order. Exported to the test only (same package) so
// the falsifier enumerates the production set rather than a hand-copied list:
// a future debug route added to registerDebugRoutes but NOT to this slice
// fails the "every registered pattern is gated" arm.
var debugRoutePatterns = []string{
	"GET /debug/pprof/",
	"GET /debug/pprof/cmdline",
	"GET /debug/pprof/profile",
	"GET /debug/pprof/symbol",
	"GET /debug/pprof/trace",
	"GET /debug/vars",
	"GET /debug/servable",
	"GET /debug/apistage",
	"GET /debug/refreshes",
}

// debugMux is the minimal registration surface registerDebugRoutes needs.
// *http.ServeMux satisfies it; the falsifier passes a recording wrapper so it
// can assert the EXACT set of patterns production registers (drift between
// registerDebugRoutes and debugRoutePatterns fails the test).
type debugMux interface {
	Handle(pattern string, handler http.Handler)
}

// registerDebugRoutes mounts the whole /debug/* surface on mux, each route
// wrapped in chain + middleware.RefreshAuth(jwtKeys).
//
// Behaviour past the gate is byte-identical to the pre-1.12.3 handlers: the
// same pprof.Index/Cmdline/Profile/Symbol/Trace funcs, the same
// expvar.Handler(), the same handlers.DebugServable/DebugApistage/DebugRefreshes.
// The ONLY change is that an unauthenticated caller now gets 401 instead of the
// body.
//
// Callers MUST have registered every expvar publisher (cache.RegisterRBACSnapshotExpvar,
// rbac.RegisterAuthzMemoExpvar, …) BEFORE calling this — the mount accepts
// scrapes as soon as the server starts.
func registerDebugRoutes(mux debugMux, chain use.Chain, jwtKeys jwtutil.KeySource) {
	gated := chain.Append(middleware.RefreshAuth(jwtKeys))

	// /debug/pprof/* — registered on the custom mux (server does NOT use
	// http.DefaultServeMux). Exposes goroutine, heap, profile, allocs, mutex,
	// block, cmdline, symbol, threadcreate, trace. pprof's handlers are plain
	// http.HandlerFunc values, so they wrap exactly like any other handler.
	mux.Handle("GET /debug/pprof/", gated.Then(http.HandlerFunc(pprof.Index)))
	mux.Handle("GET /debug/pprof/cmdline", gated.Then(http.HandlerFunc(pprof.Cmdline)))
	mux.Handle("GET /debug/pprof/profile", gated.Then(http.HandlerFunc(pprof.Profile)))
	mux.Handle("GET /debug/pprof/symbol", gated.Then(http.HandlerFunc(pprof.Symbol)))
	mux.Handle("GET /debug/pprof/trace", gated.Then(http.HandlerFunc(pprof.Trace)))

	// Ship D.1 (0.30.142) — expvar.Handler() carries Ship D's counters
	// (snowplow_apiserver_fallthrough_total / _cells,
	// snowplow_assertion_violations_total), the RBAC publish-seq
	// (cache.RegisterRBACSnapshotExpvar) and the Ship L2 snapshot authz-memo
	// counters (rbac.RegisterAuthzMemoExpvar). All of those are registered by
	// main() BEFORE this mount.
	mux.Handle("GET /debug/vars", gated.Then(expvar.Handler()))

	// Fix #1 / stale-delete diagnostic — read-only per-GVR servability
	// snapshot {HasSynced, watchBroken, confirmed, servable}
	// (docs/rca-stale-delete-compositiondefinitions-informer-2026-06-25.md).
	// Mutates no state; available in both cache-on and cache-off so the
	// stale-delete latch (registered-but-unconfirmed / watch-broken GVR) is
	// diagnosable without a kubectl exec.
	mux.Handle("GET /debug/servable", gated.Then(handlers.DebugServable()))

	// R1 diagnostic — read-only METADATA-ONLY snapshot of resolved-output
	// cache entries (class/path/gvr/age/ttl/items_count), for diagnosing a
	// degraded apistage entry (stale getComposition / cluster-scoped
	// allCompositionResources) without a kubectl exec. NEVER returns resolved
	// bodies (per-identity RBAC-sensitive) — the structural leak guard is
	// cache.ResolvedEntryMeta's type shape
	// (docs/design-r1-allcompositionresources-invalidation-2026-06-26.md §6).
	mux.Handle("GET /debug/apistage", gated.Then(handlers.DebugApistage()))

	// #61 diagnostic — read-only AGGREGATE-ONLY refresh-broadcaster counters
	// (published/delivered/dropped/coalesced + subscriber count), the
	// on-cluster instrument for verifying live-refresh delivery
	// (refreshDeliveredTotal>0 for an armed key under churn) without a kubectl
	// exec. NO per-subscription-key/identity enumeration — totals only (a
	// per-key dump would be a cross-user signal)
	// (docs/rca-refreshes-zero-delivery-2026-06-26.md §5).
	//
	// #69 gated this route first; 1.12.3 (O-D3) extends the SAME gate to its
	// four unauthenticated siblings above, so this line is now the general
	// case rather than the exception.
	mux.Handle("GET /debug/refreshes", gated.Then(handlers.DebugRefreshes()))
}
