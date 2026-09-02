// fallthrough_meter.go — Ship D (0.30.141, architectural-consistency
// invariant). Layer A of the design (§3 of
// docs/ship-d-architectural-consistency-design.md).
//
// PURPOSE — invariant-lock, not wall-clock. Every `/call` read path in
// cache=on mode MUST be cache-served; this file provides the MECHANISM
// (a closed-enum-labelled counter + sampled WARN) that surfaces any
// future regression where a request takes the apiserver-fall-through
// lane. NO BEHAVIOUR CHANGE — `RecordApiserverFallthrough` is a
// telemetry-only call invoked by Layer B (the instrumented wrappers
// in fallthrough_wrappers.go); it never short-circuits, redirects, or
// modifies an upstream request.
//
// CARDINALITY DISCIPLINE — PM-tight. The labels are closed enums:
//
//   - `reason` — the 16 FallthroughReason* constants below. New reasons
//     MUST be added here as named constants; ad-hoc strings are
//     forbidden (defence-in-depth on Prometheus cardinality budget).
//   - `path`  — the scope name passed to FallthroughScopeMiddleware,
//     bounded by the dispatcher's route list (call-restactions,
//     call-widgets, call-generic, call-write-*, list, plurals,
//     nested-call, resolver-inner-call) — 7-10 values.
//   - `gvr`   — bounded by the cluster's GVR set (~50 at production
//     scale); empty string when the apiserver call has no GVR (e.g.
//     `endpoints.FromSecret` is a Secret read but the wrapper is
//     called at the resolver mapper stage where the resolver's
//     target GVR is unknown — use `""` to keep cardinality bounded).
//
// Worst-case cardinality: 10 × 50 × 16 = 8000 series. Well within
// Prometheus comfort. The counter is exposed via `expvar` in
// fallthrough_meter_expvar.go — same pattern as snowplow's other
// metric counters (informer_dispatch_metrics.go).
//
// CACHE-TOGGLE COMPLIANCE (project_caching_is_provisional + AC-D.1).
// `RecordApiserverFallthrough` short-circuits when `cache.Disabled() ==
// true` — in cache=off mode the apiserver hops are the documented
// upstream baseline (project_caching_is_provisional), the counter
// stays silent. The middleware-driven scope marker is also short-
// circuited (see fallthrough_ctx.go).
package cache

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

// FallthroughReason is the closed enum of `reason` label values for
// `snowplow_apiserver_fallthrough_total`. New reasons MUST land here as
// named constants; a new ad-hoc reason string is a code-level
// regression. The 16 values below cover every site catalogued in the
// design's §3.1 + §3.2 (architect's design + PM tightening).
//
// Grouped for documentation:
//
//   - Construction-site reasons (Layer B wrappers fire on client build):
//     ReasonClientBuild, ReasonSecretGet, ReasonCRDDiscover.
//   - Resolver-branch-5 sub-reasons (resolve.go:716 fall-throughs by
//     gate-miss cause; AC-D.3 §F-2 sub-classification):
//     ReasonInformerNotSynced, ReasonInformerNotServable,
//     ReasonInformerRBACDeny, ReasonInformerWriteVerb,
//     ReasonInformerSubresource, ReasonInformerExternalURL,
//     ReasonInformerUnparseable, ReasonInformerPassthrough,
//     ReasonInformerMetadataOnly.
//   - Apistage GET-by-name partial-shape guard (Ship D.4.2 / 0.30.149 —
//     gateGetEnvelope:281 Go-nil-check on apiVersion/kind; empirically
//     grounded at the 0.30.148 burst's site=13 evidence — 10/250
//     /v1,configmaps GET-by-name fires had key-absent shape. Returns
//     (nil, false) → fall-through to fresh apiserver GET-by-name).
//     ReasonApistageGetPartialShape.
//   - Allowed-fall-through bucket (mainly for visibility):
//     ReasonGetMissLetApiserver404.
type FallthroughReason string

// Construction-site reasons — fired by Layer B wrappers when a fresh
// apiserver client / discovery client / restmapper is built on a
// `/call` read path.
//
// Ship 2 (production-aim cleanup 2026-06-01) removed three reasons
// whose construction sites were deleted from the codebase:
//   - ReasonCRDGet — `crds.Get` deleted; inlined direct CRD GET in
//     internal/resolvers/crds/schema/schema.go uses
//     ReasonResolverPluralsHit/Miss via the GVRFor wrappers below.
//   - ReasonRestmapperKindFor — `dynamic.KindFor` deleted; the call
//     site in internal/resolvers/widgets/resourcesrefs/resolve.go
//     now uses cache.KindForGVR which records
//     ReasonResolverPluralsHit/Miss.
//   - ReasonRestmapperResourceFor — `dynamic.ResourceFor` deleted;
//     the call site in internal/resolvers/crds/schema/schema.go now
//     uses cache.GVRFor which records ReasonResolverPluralsHit/Miss.
const (
	ReasonClientBuild FallthroughReason = "client-build"
	ReasonSecretGet   FallthroughReason = "secret-get"
	ReasonCRDDiscover FallthroughReason = "crd-discover"
)

// Resolver-branch-5 sub-reasons — fired by the inner-call worker's
// fall-through arm at `resolve.go:716` so F-2 (design §F-2) can be
// sub-classified by gate-miss cause. PM tightening — these sub-
// reasons MUST be non-zero in the tester's tester-side multi-context
// validation (any zero count means the wiring missed a branch).
//
// Ship D.4 / 0.30.144 (HARD-REVERTED) introduced
// ReasonApistagePartialShape and a TypeMeta-based predicate at the
// apistage cache gates. The predicate fired on every core-group
// LIST item (apiserver elides per-item TypeMeta by k8s convention)
// → false positives across `namespaces`, `configmaps`, etc.
// The constant and both gates were removed in Ship D.4.1.
//
// Ship D.4.1 / 0.30.145 (HARD-REVERTED) introduced a per-stage
// "resolver-nil-merge" reason and a `case []any:` iterator-merge
// predicate in `handler.go`. The 0.30.146-debug and 0.30.148-debug
// burst evidence showed `tmp_is_nil=false` on every fire — the
// predicate was empirically inert (never matched). The constant +
// predicate were REMOVED in Ship D.4.3 / 0.30.150 alongside the
// associated diagnostic scaffold. Closed-enum count: 18 (D.4.2) − 1
// (D.4.3 removes the resolver-nil-merge constant) = 17.
//
// Ship D.4.2 / 0.30.149 — ReasonApistageGetPartialShape
// ("apistage-get-partial-shape"). EMPIRICALLY GROUNDED at the
// 0.30.148 burst's site=13 evidence: 10/250 served objects for
// `/v1, Resource=configmaps` had `obj["apiVersion"] == nil` (key
// ABSENT from the map). Fired by gateGetEnvelope:281's narrower
// Go-nil-check predicate (NOT D.4's TypeMeta string-zero-value
// check) on per-name GET-by-name cached envelopes whose decoded
// map lacks `apiVersion` or `kind`. The defect flows: apiserver
// elides per-item TypeMeta on core-group LIST responses (k8s
// convention) → streaming_list.go captures item bytes verbatim →
// bytesObject's b.raw lacks apiVersion → dispatchViaInformer's
// json.Marshal produces bytes without apiVersion → apistage Put
// stores them → apistage Get + gateGetEnvelope decodes back →
// obj["apiVersion"] is Go nil (untyped nil from absent map key).
// Returns (nil, false) → fall-through to fresh apiserver GET-by-
// name (the existing served=false arm). Distinct name from D.4's
// reverted ReasonApistagePartialShape — `-get-` suffix signals
// the narrower scope (GET-by-name only, NOT LIST), avoids bisect
// confusion across the campaign.
//
// Closed-enum count: 18 (D.4.2) − 1 (D.4.3 removes the
// resolver-nil-merge constant) = 17. Within budget
// (cardinality: 10 paths × 50 GVRs × 17 reasons = 8,500 cells).
//
// Ship D.5 / 0.30.152 — adds TWO new constants:
//   - ReasonClusterListDispatch — diagnostic counter that fires when
//     the new cluster-list-when-allowed iterator collapse selects the
//     single cluster-scope LIST path. NOT a fallthrough (the dispatch
//     SUCCEEDED); recorded through the same fall-through-meter cell
//     model so per-RA / per-GVR activation can be observed via the
//     existing FallthroughCount + /debug/vars surfaces. Closed-enum
//     count: 17 + 1 = 18.
//   - ReasonClusterListShapeFallback — fires when AC-D5.14's defensive
//     multi-element shape check rejects the cluster-scope response
//     envelope (missing list kind, empty/missing items, items lacking
//     apiVersion/kind). The dispatcher then falls back to the per-NS
//     iterator path. Closed-enum count: 18 + 1 = 19. Within budget
//     (cardinality: 10 paths × 50 GVRs × 19 reasons = 9,500 cells).
const (
	ReasonInformerNotSynced       FallthroughReason = "informer-fallthrough-not-synced"
	ReasonInformerNotServable     FallthroughReason = "informer-fallthrough-not-servable"
	ReasonInformerRBACDeny        FallthroughReason = "informer-fallthrough-rbac-deny"
	ReasonInformerWriteVerb       FallthroughReason = "informer-fallthrough-write-verb"
	ReasonInformerSubresource     FallthroughReason = "informer-fallthrough-subresource"
	ReasonInformerExternalURL     FallthroughReason = "informer-fallthrough-external-url"
	ReasonInformerUnparseable     FallthroughReason = "informer-fallthrough-unparseable"
	ReasonInformerPassthrough     FallthroughReason = "informer-fallthrough-passthrough"
	ReasonInformerMetadataOnly    FallthroughReason = "informer-fallthrough-metadata-only"
	ReasonApistageGetPartialShape FallthroughReason = "apistage-get-partial-shape"
	ReasonGetMissLetApiserver404  FallthroughReason = "get-miss-let-apiserver-404"

	// Ship D.5 / 0.30.152 — cluster-list-when-allowed iterator collapse.
	// ReasonClusterListDispatch is a diagnostic (NOT a fall-through) counter
	// recording that a stage's iterator fan-out was successfully collapsed
	// to a single cluster-scope LIST. ReasonClusterListShapeFallback fires
	// when the defensive shape check (AC-D5.14) rejects the cluster-scope
	// response envelope; the dispatcher then falls back to the per-NS
	// iterator path verbatim.
	ReasonClusterListDispatch      FallthroughReason = "cluster-list-dispatch"
	ReasonClusterListShapeFallback FallthroughReason = "cluster-list-shape-fallback"

	// Ship G / 0.30.16x — identity-free widget content L1 layer.
	// ReasonWidgetContentHit is a DIAGNOSTIC (NOT a fall-through) counter
	// recording that the Ship G content layer was consulted and Get-hit
	// — gateWidgetEnvelope runs over the cached envelope and overwrites
	// every status.resourcesRefs.items[].allowed flag per-request before
	// the body leaves the pod. ReasonWidgetContentMissPerUserFallback
	// fires when the content layer Gets a miss and the dispatcher falls
	// through to the existing per-user widget L1 — the expected path
	// when F2 has not warmed this (gvr, ns, name, perPage, page) tuple.
	// Closed-enum count: 19 (D.5) + 2 = 21. Within budget (cardinality:
	// 10 paths × 50 GVRs × 21 reasons = 10,500 cells).
	ReasonWidgetContentHit                 FallthroughReason = "widget-content-hit"
	ReasonWidgetContentMissPerUserFallback FallthroughReason = "widget-content-miss-per-user-fallback"

	// Ship 1 / 0.30.225 — plurals permanent store (v6 design §3.2
	// Layer 5). ReasonPluralsDiscoveryHop fires once per gvk per
	// process lifetime on the first PluralFor / KindForGVR miss
	// against the permanent sync.Map store. MONOTONICALLY rises to
	// a bounded ceiling equal to the number of unique CRD-backed
	// GVKs in the walker corpus, then stays. Built-in scheme GVKs
	// resolved by GVRFor / KindForGVR fast path NEVER fire this
	// counter (zero apiserver hop). PluralFor (handler path) DOES
	// fire it once per built-in GVK as well, since byte-identical
	// /api-info/names response shape requires the full Info
	// (Singular + Shorts) which only the apiserver discovery
	// response provides.
	// Closed-enum count: 21 (Ship G) + 1 = 22. Within budget
	// (cardinality: 10 paths × 50 GVRs × 22 reasons = 11,000 cells).
	ReasonPluralsDiscoveryHop FallthroughReason = "plurals-discovery-hop"

	// Ship 2 / production-aim cleanup 2026-06-01 — resolver-side
	// plurals/kind lookup. ReasonResolverPluralsHit fires when the
	// in-process cache (built-in fast path or permanent store)
	// serves the request without an apiserver hop;
	// ReasonResolverPluralsMiss fires when the resolver had to fall
	// through to discovery (already counted separately by
	// ReasonPluralsDiscoveryHop — these two cells let the tester
	// see hit/miss ratios on the resolver call sites without
	// untangling per-process counters).
	//
	// Replaces the deleted ReasonRestmapperKindFor /
	// ReasonRestmapperResourceFor / ReasonCRDGet construction-site
	// counters from Ship D (0.30.141). Net closed-enum count
	// (Ship G's 21 + Ship 1's +1 + Ship 2's +2 − Ship 2's −3) = 21.
	ReasonResolverPluralsHit  FallthroughReason = "resolver-plurals-hit"
	ReasonResolverPluralsMiss FallthroughReason = "resolver-plurals-miss"
)

// RecordResolverPluralsHit records that a resolver-side plurals/kind
// lookup was served entirely by the in-process cache (built-in fast
// path or permanent sync.Map store) — no apiserver discovery hop.
// Thin wrapper over RecordApiserverFallthrough so the call site stays
// one-line and the constant choice is centralised here.
func RecordResolverPluralsHit(ctx context.Context, gvr string) {
	RecordApiserverFallthrough(ctx, ReasonResolverPluralsHit, gvr)
}

// RecordResolverPluralsMiss records that a resolver-side plurals/kind
// lookup missed the in-process cache and fell through to discovery.
// The underlying discovery hop is ALSO counted by
// ReasonPluralsDiscoveryHop at the PluralFor / KindForGVR sites; this
// wrapper captures the miss at the resolver call site so tester
// dashboards can attribute traffic to the originating resolver.
func RecordResolverPluralsMiss(ctx context.Context, gvr string) {
	RecordApiserverFallthrough(ctx, ReasonResolverPluralsMiss, gvr)
}

// fallthroughKey is the composite label tuple for one counter cell.
// We key sync.Map by this struct (Go map key — string-equality) so
// every (path, gvr, reason) combination is one atomic counter.
type fallthroughKey struct {
	path   string
	gvr    string
	reason FallthroughReason
}

// fallthroughCounters carries one *atomic.Uint64 per (path, gvr,
// reason). sync.Map is the right primitive — writes are rare relative
// to reads (`expvar` collection); the key set grows monotonically and
// is bounded by the cardinality budget. (A plain map + sync.RWMutex
// would be simpler but ranges-while-collecting cost more lock time;
// the budget per the design is 8000 cells, so the sync.Map miss-path
// allocation is a one-time fixed cost.)
var fallthroughCounters sync.Map

// fallthroughTotal is the grand-total counter — every increment to any
// per-cell counter ALSO Add(1)'s this one. Used by tests (and by the
// AC-D.1 race test in particular) to assert "the wrapper fired" without
// having to enumerate the cell map.
var fallthroughTotal atomic.Uint64

// FallthroughTotal returns the cumulative count of apiserver
// fall-throughs observed by `RecordApiserverFallthrough` since process
// start. Exported for the AC-D.5 test gate.
func FallthroughTotal() uint64 {
	return fallthroughTotal.Load()
}

// FallthroughCount returns the per-cell count for a (path, gvr,
// reason) tuple, or 0 if the cell has never incremented. Used by tests
// to assert per-label-tuple cardinality (e.g. F-3 ratify: the
// `secret-get` reason cell is non-zero post-traffic).
func FallthroughCount(path, gvr string, reason FallthroughReason) uint64 {
	v, ok := fallthroughCounters.Load(fallthroughKey{path, gvr, reason})
	if !ok {
		return 0
	}
	c := v.(*atomic.Uint64)
	return c.Load()
}

// fallthroughWarnSampleCounter cycles 0..99; we WARN-log when it
// passes the modulo gate. Deterministic (mod 100) and allocation-free,
// per the task's "1% WARN sampling via atomic.Uint64 mod 100" spec.
// CompareAndSwap-free design: the counter is monotonically incremented,
// the WARN gate fires for every 100th increment. Two goroutines racing
// to log the same tick is a non-event — the labels are identical and
// the log is informational.
var fallthroughWarnSampleCounter atomic.Uint64

// fallthroughWarnSampleEvery is the sampling denominator: 1 WARN per
// 100 fall-throughs. Constant for the deterministic-sampling property.
const fallthroughWarnSampleEvery = 100

// RecordApiserverFallthrough is invoked by every Layer B wrapper (see
// fallthrough_wrappers.go) BEFORE the wrapper delegates to the
// upstream apiserver-client construction. The "before" ordering is
// load-bearing (PM tightening): if the upstream call panics, the
// counter must still record the fall-through occurred. A deferred
// call AFTER the upstream construction would miss panicking sites.
//
// Short-circuits to a no-op when:
//
//   - `cache.Disabled() == true` — cache=off baseline; the apiserver
//     fall-through is expected and counted nowhere.
//   - The ctx is not inside a `FallthroughScope` — i.e. the call is
//     not on a `/call`-class read path (e.g. Phase 1 walker, watcher
//     bootstrap, refresher). The middleware in fallthrough_ctx.go
//     stamps the scope ONLY on the `/call`-class routes.
//
// Both checks are cheap (one boolean read + one ctx.Value lookup);
// the no-op branch is taken on every non-`/call` apiserver
// construction, so the overhead must be minimal.
//
// gvr may be empty when the construction site does not know the
// target GVR (e.g. `endpoints.FromSecret` — the Secret being read is
// fixed per user, not per resolver target). Use `""` to keep label
// cardinality bounded; do NOT synthesize a placeholder string.
func RecordApiserverFallthrough(ctx context.Context, reason FallthroughReason, gvr string) {
	if Disabled() {
		return
	}
	scope := FallthroughScope(ctx)
	if scope == nil || !scope.Active {
		return
	}

	key := fallthroughKey{path: scope.Path, gvr: gvr, reason: reason}
	c, ok := fallthroughCounters.Load(key)
	if !ok {
		// LoadOrStore is the standard race-free init pattern for
		// sync.Map — if two goroutines race to create the cell, the
		// LoadOrStore call returns the same pointer to both and the
		// loser drops its fresh atomic. Per-cell counter alloc is
		// then a one-time cost per (path, gvr, reason) tuple — the
		// hot-path increment is purely an atomic.Add.
		c, _ = fallthroughCounters.LoadOrStore(key, new(atomic.Uint64))
	}
	c.(*atomic.Uint64).Add(1)
	fallthroughTotal.Add(1)

	// 1%-sampled DEBUG echo — deterministic via mod 100 on a monotonic
	// counter. Allocation-free: the counter is package-level atomic.
	// Two goroutines incrementing at the same tick both pass the gate
	// — the duplicate line is informational only (counter accuracy is
	// per-cell, sampling is loose by design).
	//
	// LEVEL — DEBUG (#170). The authoritative cache-effectiveness signal
	// is the snowplow_apiserver_fallthrough_total / _cells expvar counters
	// (updated unconditionally above); this sampled log is only a
	// convenience echo of an already-exported metric. Even 1%-sampled it
	// is ~14K/week at the WARN floor (#163), so it belongs at DEBUG — the
	// counters remain the source of truth, and the echo stays available at
	// LOG_LEVEL=debug. Consistent with the discovery soft-fail sibling
	// (resolve.go: "apiserver_fallthrough metrics. DEBUG, not WARN.").
	if fallthroughWarnSampleCounter.Add(1)%fallthroughWarnSampleEvery == 0 {
		slog.Debug("apiserver_fallthrough",
			slog.String("subsystem", "cache"),
			slog.String("path", scope.Path),
			slog.String("gvr", gvr),
			slog.String("reason", string(reason)),
			slog.String("hint", "a /call read path issued an apiserver-attributable request in cache=on mode "+
				"— see docs/ship-d-architectural-consistency-design.md §F-N for remediation"),
		)
	}
}

// ResetFallthroughCountersForTest zeros every per-cell counter and the
// grand-total. TEST-ONLY — production code MUST NOT call it.
// Mirrors the established ResetEvaluateRBACCallCount pattern at
// internal/rbac/evaluate.go:48.
func ResetFallthroughCountersForTest() {
	fallthroughCounters.Range(func(k, v any) bool {
		v.(*atomic.Uint64).Store(0)
		return true
	})
	fallthroughTotal.Store(0)
	fallthroughWarnSampleCounter.Store(0)
}
