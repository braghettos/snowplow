// fallthrough_meter.go — Ship D (0.30.141, architectural-consistency
// invariant). Layer A of the design (§3 of
// docs/ship-d-architectural-consistency-design.md).
//
// PURPOSE — invariant-lock, not wall-clock. Every `/call` read path in
// cache=on mode MUST be cache-served; this file provides the MECHANISM
// (a closed-enum-labelled counter + sampled WARN) that surfaces any
// future regression where a request takes the apiserver-fall-through
// lane. NO BEHAVIOUR CHANGE — `RecordApiserverFallthrough` is a
// telemetry-only call invoked at the construction sites listed in the
// 1.12.4 design §5.2; it never short-circuits, redirects, or modifies
// an upstream request.
//
// 1.12.4 — TWO FAMILIES, TWO MAPS (design §5). The counter's own
// definition is "a `/call`-class read that caused snowplow to issue a
// request to the live Kubernetes apiserver". Six of the 21 reasons
// contradict that in their own doc-comments (resolver-plurals-hit is
// an in-process cache HIT; resolver-plurals-miss double-counts the hop
// already counted by plurals-discovery-hop; widget-content-hit and
// widget-content-miss-per-user-fallback are L1 serves;
// cluster-list-dispatch records a SUCCESSFUL collapse). On the krateo-057
// corpus they were 828,525 of the 936,320 headline count — 88.5% of the
// number an operator reads as "the cache is not serving".
//
// Those six now land on a SEPARATE counter and a SEPARATE cell map
// (`snowplow_cache_diagnostic_total` / `_cells`). Two maps, not one map
// with two scalar totals: the acceptance requirement is that
// `resolver-plurals-hit` is ABSENT from the fallthrough cells map, which
// a shared map cannot satisfy. Membership of `diagnosticReasons` is the
// ONLY thing that routes a Record* call, so a call site cannot mis-route
// a reason.
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

// diagnosticCounters / diagnosticTotal are the 1.12.4 second family
// (design §5.4). Structurally identical to the pair above — a separate
// sync.Map keyed by the SAME fallthroughKey struct plus its own
// grand-total — so a diagnostic reason's cell is absent from
// fallthroughCounters entirely rather than merely subtracted from a
// shared scalar.
var (
	diagnosticCounters sync.Map
	diagnosticTotal    atomic.Uint64
)

// diagnosticReasons is the closed set of reasons that are NOT apiserver
// fall-throughs. Membership here is the ONLY thing that decides which
// counter AND WHICH CELL MAP a Record* call lands on, so a new reason
// cannot be mis-routed by a call site.
//
// Each entry is justified in design §5.2 by the reason's OWN
// doc-comment above:
//
//   - ReasonResolverPluralsHit — an in-process cache HIT (built-in fast
//     path or the permanent sync.Map store). Zero apiserver traffic.
//   - ReasonResolverPluralsMiss — the underlying hop is ALREADY counted
//     at plurals_resolver.go by ReasonPluralsDiscoveryHop. Counting it
//     here too double-counts; the 057 corpus proves it (both cells read
//     exactly 29).
//   - ReasonWidgetContentHit — the Ship G content layer Get-HIT, served
//     from L1.
//   - ReasonWidgetContentMissPerUserFallback — falls through to the
//     per-user L1 lookup, not to the apiserver.
//   - ReasonClusterListDispatch — the collapse SUCCEEDED; its own
//     comment says "NOT a fallthrough (the dispatch SUCCEEDED)".
//   - ReasonClusterListShapeFallback — a judgement call, stated as such
//     in design §5.2: the dispatcher reverts to the per-NS iterator
//     path, which is itself informer-eligible. A degradation, not a
//     proven apiserver hop. Impact on the 057 corpus is exactly 1 count.
//
// ReasonPluralsDiscoveryHop deliberately STAYS on the fallthrough
// family: it is the actual discovery request.
var diagnosticReasons = map[FallthroughReason]struct{}{
	ReasonResolverPluralsHit:               {},
	ReasonResolverPluralsMiss:              {},
	ReasonWidgetContentHit:                 {},
	ReasonWidgetContentMissPerUserFallback: {},
	ReasonClusterListDispatch:              {},
	ReasonClusterListShapeFallback:         {},
}

// IsDiagnosticReason reports whether reason is classified as a cache
// DIAGNOSTIC (design §5.2) rather than a genuine apiserver
// fall-through. Exported so the OTLP mirror and the structural
// attribute-hygiene falsifier read the same single source of truth the
// router reads, instead of a hand-copied list.
func IsDiagnosticReason(reason FallthroughReason) bool {
	_, ok := diagnosticReasons[reason]
	return ok
}

// FallthroughTotal returns the cumulative count of GENUINE apiserver
// fall-throughs observed by `RecordApiserverFallthrough` since process
// start. Exported for the AC-D.5 test gate.
//
// 1.12.4: the six diagnostic reasons (see diagnosticReasons) no longer
// contribute here; they are counted by DiagnosticTotal. On a corpus
// comparable to krateo-057 this value drops ~8.7x. That drop is the
// fix, not a cache regression — see howto/operating.md.
func FallthroughTotal() uint64 {
	return fallthroughTotal.Load()
}

// DiagnosticTotal returns the cumulative count of cache-DIAGNOSTIC
// observations — the six reasons that reach no apiserver (design §5.2).
// Mirrors FallthroughTotal for the second family.
func DiagnosticTotal() uint64 {
	return diagnosticTotal.Load()
}

// FallthroughCount returns the per-cell count for a (path, gvr,
// reason) tuple in the GENUINE fall-through map, or 0 if the cell has
// never incremented. Used by tests to assert per-label-tuple
// cardinality (e.g. F-3 ratify: the `secret-get` reason cell is
// non-zero post-traffic).
//
// A diagnostic reason ALWAYS returns 0 here — that absence is the
// acceptance assertion of design §5.4 / F4.
func FallthroughCount(path, gvr string, reason FallthroughReason) uint64 {
	v, ok := fallthroughCounters.Load(fallthroughKey{path, gvr, reason})
	if !ok {
		return 0
	}
	c := v.(*atomic.Uint64)
	return c.Load()
}

// CacheDiagnosticCount returns the per-cell count for a (path, gvr,
// reason) tuple in the DIAGNOSTIC map, or 0 if the cell has never
// incremented. Mirrors FallthroughCount for the second family.
func CacheDiagnosticCount(path, gvr string, reason FallthroughReason) uint64 {
	v, ok := diagnosticCounters.Load(fallthroughKey{path, gvr, reason})
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

// RecordApiserverFallthrough is invoked at each construction site
// (design §5.2) BEFORE the site delegates to the
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
	// 1.12.4 (design §5.4) — the ONLY routing decision. Everything
	// below recordCell is the pre-1.12.4 body verbatim; only the pair
	// of destinations differs. Call sites are unchanged and cannot
	// influence the choice.
	if _, diag := diagnosticReasons[reason]; diag {
		recordCell(ctx, reason, gvr, &diagnosticCounters, &diagnosticTotal)
		return
	}
	recordCell(ctx, reason, gvr, &fallthroughCounters, &fallthroughTotal)
}

// recordCell is the pre-1.12.4 body of RecordApiserverFallthrough,
// extracted verbatim and parameterised by its destination map + total.
// Same Disabled() + scope short-circuit, same LoadOrStore cell-init,
// same 1%-sampled DEBUG echo — so the split changes routing only, and
// both branches remain an atomic add over a sync.Map cell.
func recordCell(ctx context.Context, reason FallthroughReason, gvr string,
	cells *sync.Map, total *atomic.Uint64) {

	if Disabled() {
		return
	}
	scope := FallthroughScope(ctx)
	if scope == nil || !scope.Active {
		return
	}

	key := fallthroughKey{path: scope.Path, gvr: gvr, reason: reason}
	c, ok := cells.Load(key)
	if !ok {
		// LoadOrStore is the standard race-free init pattern for
		// sync.Map — if two goroutines race to create the cell, the
		// LoadOrStore call returns the same pointer to both and the
		// loser drops its fresh atomic. Per-cell counter alloc is
		// then a one-time cost per (path, gvr, reason) tuple — the
		// hot-path increment is purely an atomic.Add.
		c, _ = cells.LoadOrStore(key, new(atomic.Uint64))
	}
	c.(*atomic.Uint64).Add(1)
	total.Add(1)

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
// grand-total, in BOTH families. TEST-ONLY — production code MUST NOT
// call it. Mirrors the established ResetEvaluateRBACCallCount pattern
// at internal/rbac/evaluate.go:48.
//
// 1.12.4 (design §5.4, gate condition C5): the diagnostic map and total
// MUST be zeroed here too. Without it, F5's exact-conservation arm
// (FallthroughTotal()+DiagnosticTotal() == N) inherits counts from an
// earlier arm in the same test binary and flakes.
func ResetFallthroughCountersForTest() {
	fallthroughCounters.Range(func(k, v any) bool {
		v.(*atomic.Uint64).Store(0)
		return true
	})
	fallthroughTotal.Store(0)
	diagnosticCounters.Range(func(k, v any) bool {
		v.(*atomic.Uint64).Store(0)
		return true
	})
	diagnosticTotal.Store(0)
	fallthroughWarnSampleCounter.Store(0)
}
