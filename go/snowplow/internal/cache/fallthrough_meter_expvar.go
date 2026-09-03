// fallthrough_meter_expvar.go — Ship D (0.30.141). expvar exposure
// for `snowplow_apiserver_fallthrough_total` and the per-cell
// (path, gvr, reason) breakdown. Mirrors the existing snowplow
// metric-exposure pattern (informer_dispatch_metrics.go +
// resolved.go `startResolvedCacheSummary`).
//
// REGISTRATION TIME. The expvar handles are registered in `init()` so
// any process that imports this package picks them up. The registry
// keys are stable for log-aggregation and grep tooling:
//
//   - snowplow_apiserver_fallthrough_total — grand-total uint64.
//   - snowplow_apiserver_fallthrough_cells  — per-cell breakdown
//     as a map[string]uint64, key `"path|gvr|reason"`.
//   - snowplow_cache_diagnostic_total       — 1.12.4 companion
//     grand-total for the six reasons that reach no apiserver.
//   - snowplow_cache_diagnostic_cells       — its per-cell breakdown,
//     same `"path|gvr|reason"` key shape.
//
// The two families are published inside the SAME sync.Once, so a
// process either has all four keys or none — an operator never sees a
// half-migrated surface where the fallthrough number has dropped but
// the companion that explains the drop is missing.
//
// expvar is the existing pattern. No new dependency.
//
// CFG-1 (Ship 0.30.163) — cache-off compliance per project memory
// `project_cache_off_is_transparent_fallback`. Diego's 2026-05-22
// contract: "there is no cache with cache_enabled=false". Under
// CACHE_ENABLED=false the cache subsystem does not exist and these
// gauges MUST NOT be registered (so they don't appear at /debug/vars
// even with empty values). The gate is at init() time: Go runtime
// populates env vars BEFORE package init() runs, so Disabled() is
// authoritative here.
package cache

import (
	"expvar"
	"sync"
	"sync/atomic"
)

// fallthroughExpvarOnce guards registerFallthroughExpvar so the
// registration body runs at most once per process even if invoked from
// both init() (production) and RegisterExpvarForTest (in-process tests
// that boot with CACHE_ENABLED unset and later flip it via t.Setenv).
// expvar.Publish panics on duplicate key; sync.Once prevents that.
var fallthroughExpvarOnce sync.Once

func init() {
	// CFG-1: under CACHE_ENABLED=false, no cache subsystem exists →
	// gauges must not be registered. init() runs once per process so
	// this branch cannot be unit-tested in-process; falsifier is
	// HG-321 (4-env-value matrix process spawn, see
	// e2e/bench/cfg1_falsifier.sh).
	if Disabled() {
		return
	}
	registerFallthroughExpvar()
}

// registerFallthroughExpvar performs the three expvar.Publish calls
// for the fallthrough meter. Guarded by fallthroughExpvarOnce so it
// is safe to call from both init() and the test helper.
func registerFallthroughExpvar() {
	fallthroughExpvarOnce.Do(func() {
		expvar.Publish("snowplow_apiserver_fallthrough_total", expvar.Func(func() any {
			return fallthroughTotal.Load()
		}))
		expvar.Publish("snowplow_assertion_violations_total", expvar.Func(func() any {
			// Per-check map. Each invariant assertion registers its own
			// check= label here (Ship D: read_paths_scoped; hardening #1:
			// serve_requires_servable).
			return map[string]uint64{
				"read_paths_scoped":        assertionViolationsTotal.Load(),
				"serve_requires_servable":  serveRequiresServableViolations.Load(),
				"seed_aggregate_footprint": seedUnitFootprintViolations.Load(),
			}
		}))
		expvar.Publish("snowplow_apiserver_fallthrough_cells", expvar.Func(func() any {
			// Pipe-separated label tuple — none of the three label
			// values contains a pipe (path is a closed enum; gvr
			// uses `/`; reason is a closed enum).
			//
			// 1.12.4: this reads the same snapshot helper the OTLP
			// mirror reads, so the two surfaces cannot drift. expvar
			// gets the UNCAPPED map (the bench harness and /debug
			// diagnostics need every cell); the OTLP path applies the
			// cardinality cap on its own side.
			return flatFallthroughCells(&fallthroughCounters)
		}))
		// 1.12.4 (design §5.4) — the companion family. The six reasons in
		// diagnosticReasons reach no apiserver; on the krateo-057 corpus they
		// were 828,525 of the 936,320 that used to be reported as
		// fall-throughs. Published beside the pair above, in the same
		// sync.Once, so the number that explains the drop is always present
		// wherever the dropped number is.
		expvar.Publish("snowplow_cache_diagnostic_total", expvar.Func(func() any {
			return diagnosticTotal.Load()
		}))
		expvar.Publish("snowplow_cache_diagnostic_cells", expvar.Func(func() any {
			return flatFallthroughCells(&diagnosticCounters)
		}))
	})
}

// flatFallthroughCells flattens one cell map into the
// `"path|gvr|reason" -> count` shape both expvar keys publish. Shared by
// the two families so their serialisation cannot diverge, and reused by
// the OTLP snapshot accessors in otel_accessors.go.
func flatFallthroughCells(cells *sync.Map) map[string]uint64 {
	out := map[string]uint64{}
	cells.Range(func(k, v any) bool {
		key := k.(fallthroughKey)
		out[key.path+"|"+key.gvr+"|"+string(key.reason)] = v.(*atomic.Uint64).Load()
		return true
	})
	return out
}
