// otel_accessors.go — exported, read-only accessors over the dispatchers
// package's existing prewarm / phase-1 / dispatch-L1 atomic counters, so
// the internal/metrics OTLP mirror (a separate package) can observe the
// SAME live snapshots the expvar `.Func` closures read.
//
// ADDITIVE: these are pure read accessors. They do NOT change any
// incrementer, populate path, or the expvar surface. Each reads the same
// package-private atomics / sync.Maps the existing expvar publishers in
// this package read (prewarm_engine_metrics.go, phase1_walk_pagination_metrics.go,
// phase1_walk_metrics.go, phase1_pip_metrics.go, l1_lookup_metrics.go).
//
// The metrics package reads these at OTLP collection time only (the
// observable-instrument callback), so there is zero per-/call cost — same
// "computed-on-read" semantics as expvar.

package dispatchers

import "strings"

// PrewarmEngineSnapshot returns the prewarm-engine worker counters,
// mirroring snowplow_prewarm_engine_{enqueued,processed,yield}_total and
// snowplow_prewarm_engine_pending_depth. pendingDepth is the workqueue's own
// Len() (F.4 / R1 — internally synchronized, race-free against Add/Get).
// Safe before the engine has started: prewarmEngineSingleton() lazily
// constructs the engine (queue included), so a pre-start read returns zeros
// from the freshly-allocated queue.
func PrewarmEngineSnapshot() (enqueued, processed, yield uint64, pendingDepth int64) {
	e := prewarmEngineSingleton()
	if e == nil {
		return 0, 0, 0, 0
	}
	pendingDepth = int64(e.queue.Len())
	return e.enqueuedTotal.Load(), e.processedTotal.Load(), e.yieldTotal.Load(), pendingDepth
}

// Phase1PaginationSnapshot returns the apiRef pagination-coverage grand
// totals, mirroring snowplow_phase1_units_planned / _units_seeded /
// _apiref_pages_total / _eligible_no_continue_total.
func Phase1PaginationSnapshot() (unitsPlanned, unitsSeeded, apiRefPages, eligibleNoContinue uint64) {
	return prewarmUnitsPlanned.Load(),
		prewarmUnitsSeeded.Load(),
		prewarmApiRefPagesTotal.Load(),
		prewarmEligibleNoContinueTotal.Load()
}

// Phase1WalkSnapshot returns the boot-walk fan-out totals, mirroring
// snowplow_phase1_walk_zero_children_total / _walk_observations_total. The
// per-root walk_children map is high-cardinality + diagnostic-only and is
// deliberately NOT mirrored to OTLP.
func Phase1WalkSnapshot() (zeroChildren, observations uint64) {
	return phase1WalkZeroChildrenTotal.Load(), phase1WalkObservationsTotal.Load()
}

// Phase1SeedSnapshot returns the per-target phase-1 seed outcome totals,
// mirroring snowplow_phase1_bindingset_seed_resolves_total /
// _bindingset_seed_failures_total / _seed_rbac_deny_total /
// _seed_operational_fail_total. The per-(cohort,target) failure maps and
// the per-cohort status map are high-cardinality + diagnostic-only and are
// deliberately NOT mirrored to OTLP.
func Phase1SeedSnapshot() (resolves, failures, rbacDeny, operationalFail uint64) {
	return pipBindingSetSeedResolvesTotal.Load(),
		pipBindingSetSeedFailuresTotal.Load(),
		pipSeedRBACDenyTotal.Load(),
		pipSeedOperationalFailTotal.Load()
}

// DispatchL1LookupTotals returns the cluster-wide aggregate hit/miss totals
// across every (handlerKind, gvr) dispatch-L1 cell, mirroring the
// snowplow_dispatch_l1_lookups expvar map collapsed to two grand totals.
// The per-(handlerKind|gvr) breakdown is intentionally aggregated here to
// keep OTLP cardinality bounded — the expvar map remains the per-cell
// drill-down surface.
func DispatchL1LookupTotals() (hit, miss uint64) {
	l1LookupCells.Range(func(_, v any) bool {
		cell, _ := v.(*l1LookupCell)
		if cell == nil {
			return true
		}
		hit += cell.hit.Load()
		miss += cell.miss.Load()
		return true
	})
	return hit, miss
}

// L1LookupCell is one (class, gvr) row of the dispatch-L1 lookup
// breakdown, as consumed by the OTLP mirror (1.12.4 §3.3).
//
// `Class` is the handlerKind half of the expvar key — one of
// restactions / widgets / widgetContent. It is called Class here because
// that is the attribute name the dashboard and the design's mapping
// table use, and because it lines up with the CacheEntryClass vocabulary
// the rest of the cache speaks.
type L1LookupCell struct {
	Class   string
	GVR     string
	Hit     uint64
	Miss    uint64
	SeedHit uint64
}

// DispatchL1LookupCells returns the per-(class, gvr) breakdown that
// DispatchL1LookupTotals collapses (1.12.4 §3.3, "widen").
//
// WHY THE AGGREGATE WAS NOT ENOUGH. Two process-wide numbers cannot
// answer the question the panel is for: "is the cache serving THIS
// class?" A widgets hit-rate of 99% and a widgetContent hit-rate of 0%
// average to a healthy-looking aggregate while the portal experience is
// broken. The per-cell data has existed at /debug/vars since Ship OBS-1;
// only the OTLP mirror was collapsing it.
//
// Cardinality is bounded by (3 classes x registered GVRs) — 169 GVRs on
// krateo-057, so ~507 worst case, and the GVR set does not grow with
// composition count. The observation path applies no cap here: unlike
// the fall-through cells this family cannot reach the 46K worst case.
//
// Returns cells in sync.Map iteration order; callers that need
// determinism must sort. A malformed key (no "|" separator) is skipped
// rather than emitted with an empty class, so a future key-shape change
// degrades to missing series instead of a mis-attributed one.
func DispatchL1LookupCells() []L1LookupCell {
	var out []L1LookupCell
	l1LookupCells.Range(func(k, v any) bool {
		ks, _ := k.(string)
		cell, _ := v.(*l1LookupCell)
		if cell == nil {
			return true
		}
		i := strings.Index(ks, "|")
		if i < 0 {
			return true
		}
		out = append(out, L1LookupCell{
			Class:   ks[:i],
			GVR:     ks[i+1:],
			Hit:     cell.hit.Load(),
			Miss:    cell.miss.Load(),
			SeedHit: cell.seedHit.Load(),
		})
		return true
	})
	return out
}

// HitsSeedAttributable returns the process-wide count of resolved-cache
// hits served from a boot-seeded cell, mirroring the
// snowplow_resolved_cache_hits_seed_attributable expvar (#130 F3). It is
// the one-number answer to "did the boot seed warm anything a browser
// actually hit".
func HitsSeedAttributable() uint64 {
	return hitsSeedAttributable.Load()
}

// ReadinessBackstopFired returns the number of boots whose /readyz
// flipped Ready via the C2 backstop rather than the firstNav-complete
// happy path — i.e. FAILED-but-serving boots (#131).
//
// [C11] This is the top boot SLI and it is LOG-ONLY today: the counter
// is an unexported expvar.Int in this package with no accessor, and the
// single ERROR log line that accompanies it is the only other trace. A
// healthy boot leaves it 0, so any non-zero value is alert-worthy — and
// nothing could alert on it, because nothing outside this package could
// read it. Note the counter lives under internal/handlers/dispatchers/,
// NOT internal/cache/ as the design's §3.3 table said.
func ReadinessBackstopFired() int64 {
	return readinessBackstopFired.Value()
}
