// otel_accessors.go — exported, read-only accessors over cache-package
// counters that already back expvar but whose snapshot helpers are
// package-private (refresherStatsSnapshot, controllerHealthSnap). The
// internal/metrics OTLP mirror (a separate package) reads these at OTLP
// collection time to observe the SAME live snapshots the expvar `.Func`
// closures read.
//
// ADDITIVE: pure read accessors. They change no incrementer, no populate
// path, and the expvar surface is untouched.

package cache

import (
	"sort"
	"sync"
	"sync/atomic"
)

// otlpCellSeriesCap bounds how many (path, gvr, reason) series ONE cell
// family may contribute to a single OTLP collection.
//
// WHY A CAP AT ALL, GIVEN CLOSED ENUMS. `path` is a closed set of 13
// scope constants and `reason` a closed enum of 21, so the space is
// bounded — but bounded is not small. `gvr` is bounded by REGISTERED
// GVRs, which is 169 on krateo-057 (the source comment's "~50" and the
// ~11,000-cell budget it implies are 3.4x stale). Worst case is
// therefore 13 x 169 x 21 = 46,137 series across the two families, and
// nothing in code caps it: the sync.Map grows monotonically with no
// ceiling. Observed on 057 is 218 cells, so the cap is ~23x headroom
// over reality and still an order of magnitude under the worst case.
//
// The cap is NOT a cardinality budget for ClickHouse to enforce; it is
// the thing that stops a cardinality regression from becoming an
// incident, and snowplow_metrics_series_truncated_total makes it VISIBLE
// rather than silent.
const otlpCellSeriesCap = 5000

// seriesTruncatedTotal counts series dropped into the __other__ bucket,
// per family. Exported through snowplow_metrics_series_truncated_total;
// dashboard panel 15b alerts on any non-zero value.
var seriesTruncatedTotal sync.Map // family string -> *atomic.Uint64

// OtherGVR is the overflow bucket label. Cells beyond otlpCellSeriesCap
// collapse into it, PRESERVING path and reason — both are closed enums,
// so only gvr can blow up, and keeping the other two means the overflow
// is still attributable to a scope and a cause.
const OtherGVR = "__other__"

// FallthroughCell is one (path, gvr, reason) series for the OTLP mirror.
type FallthroughCell struct {
	Path   string
	GVR    string
	Reason string
	Count  uint64
}

// FallthroughCellsSnapshot returns at most otlpCellSeriesCap series from
// the GENUINE apiserver-fall-through family, plus the number of cells
// that were folded into the overflow bucket.
//
// DETERMINISTIC TRUNCATION. Cells are ordered by DESCENDING count before
// truncation, so the largest contributors always keep their own series
// and only the tail aggregates. A cap that dropped an arbitrary
// map-iteration slice would make a dashboard flicker between collections
// as different cells won the race.
//
// APPLIED ON THE OTLP OBSERVATION PATH ONLY. /debug/vars keeps the full
// uncapped map (fallthrough_meter_expvar.go), so the bench harness and
// the /debug diagnostics are unaffected by this function's existence.
func FallthroughCellsSnapshot() (cells []FallthroughCell, truncated int) {
	return cappedCells(&fallthroughCounters, "apiserver_fallthrough")
}

// CacheDiagnosticCellsSnapshot is the same for the 1.12.4 diagnostic
// family (design §5.4). Same cap, same deterministic ordering, same
// __other__ overflow, counted under its own family label.
func CacheDiagnosticCellsSnapshot() (cells []FallthroughCell, truncated int) {
	return cappedCells(&diagnosticCounters, "cache_diagnostic")
}

// cappedCells is the shared body. Both families must truncate the same
// way or the two dashboard panels stop being comparable.
func cappedCells(m *sync.Map, family string) ([]FallthroughCell, int) {
	var all []FallthroughCell
	m.Range(func(k, v any) bool {
		key := k.(fallthroughKey)
		all = append(all, FallthroughCell{
			Path:   key.path,
			GVR:    key.gvr,
			Reason: string(key.reason),
			Count:  v.(*atomic.Uint64).Load(),
		})
		return true
	})
	if len(all) <= otlpCellSeriesCap {
		return all, 0
	}

	// Descending by count; ties broken on the label tuple so the result
	// is a total order and successive collections agree.
	sort.Slice(all, func(i, j int) bool {
		if all[i].Count != all[j].Count {
			return all[i].Count > all[j].Count
		}
		if all[i].Path != all[j].Path {
			return all[i].Path < all[j].Path
		}
		if all[i].Reason != all[j].Reason {
			return all[i].Reason < all[j].Reason
		}
		return all[i].GVR < all[j].GVR
	})

	kept := all[:otlpCellSeriesCap]
	tail := all[otlpCellSeriesCap:]

	// Fold the tail into one __other__ series per (path, reason). gvr is
	// the only unbounded label, so collapsing just it keeps the overflow
	// attributable while making the series count bounded by
	// cap + 13 paths x 21 reasons.
	type pr struct{ path, reason string }
	folded := map[pr]uint64{}
	for _, c := range tail {
		folded[pr{c.Path, c.Reason}] += c.Count
	}
	out := make([]FallthroughCell, 0, len(kept)+len(folded))
	out = append(out, kept...)
	for k, n := range folded {
		out = append(out, FallthroughCell{Path: k.path, GVR: OtherGVR, Reason: k.reason, Count: n})
	}

	bumpSeriesTruncated(family, uint64(len(tail)))
	return out, len(tail)
}

// bumpSeriesTruncated records overflow for a family. Monotonic across
// collections: it answers "has this family EVER overflowed", which is
// what an alert needs, rather than "is it overflowing right now", which
// a scrape could miss.
func bumpSeriesTruncated(family string, n uint64) {
	if n == 0 {
		return
	}
	c, ok := seriesTruncatedTotal.Load(family)
	if !ok {
		c, _ = seriesTruncatedTotal.LoadOrStore(family, new(atomic.Uint64))
	}
	c.(*atomic.Uint64).Add(n)
}

// SeriesTruncatedSnapshot returns family -> cumulative truncated-series
// count, for snowplow_metrics_series_truncated_total. Empty when nothing
// has ever overflowed, which is the expected steady state.
func SeriesTruncatedSnapshot() map[string]uint64 {
	out := map[string]uint64{}
	seriesTruncatedTotal.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Uint64).Load()
		return true
	})
	return out
}

// ResetSeriesTruncatedForTest zeroes the overflow counters. TEST-ONLY.
func ResetSeriesTruncatedForTest() {
	seriesTruncatedTotal.Range(func(k, v any) bool {
		v.(*atomic.Uint64).Store(0)
		return true
	})
}

// OTLPCellSeriesCap exposes the cap so the falsifier asserts against the
// production constant rather than a hand-copied literal that could drift
// from it.
func OTLPCellSeriesCap() int { return otlpCellSeriesCap }

// RefresherSnapshot returns the background re-resolve worker-pool counters,
// mirroring the snowplow_refresher_* expvar family. queueDepth is the live
// workqueue Len(). Safe before StartRefresher: refresherStatsSnapshot reads
// the singleton lazily and returns zeros when the pool is not yet built.
func RefresherSnapshot() (enqueued, completed, failed, retried, dropped,
	skippedNoEntry, skippedNoHandler, skippedStageError,
	yielded, capped, floored uint64, queueDepth int64) {

	s := refresherStatsSnapshot()
	r := refresherInstance
	if r != nil && r.queue != nil {
		queueDepth = int64(r.queue.Len())
	}
	return s.enqueued, s.completed, s.failed, s.retried, s.dropped,
		s.skippedNoEntry, s.skippedNoHandler, s.skippedStageError,
		s.yielded, s.capped, s.floored, queueDepth
}

// UpstreamHealthSnapshot collapses the per-controller controller-health
// snapshot into bounded aggregate gauges suitable for OTLP, mirroring the
// operationally-significant signal of snowplow_upstream_controller_health
// (every entry Healthy=1) and snowplow_upstream_webhook_failurepolicy (how
// many discovered webhooks carry a Fail policy). The per-name maps stay the
// expvar drill-down surface; OTLP gets the alarm-worthy counts so a
// dashboard can alert on "controllersUnhealthy > 0" or
// "webhooksFailPolicy > 0 on a degraded controller" without per-name
// cardinality.
//
// Returns all zeros when cache is off or no snapshot has been published yet.
func UpstreamHealthSnapshot() (controllersHealthy, controllersUnhealthy,
	webhooksTotal, webhooksFailPolicy int64) {

	if Disabled() {
		return 0, 0, 0, 0
	}
	s := controllerHealthSnap.Load()
	if s == nil {
		return 0, 0, 0, 0
	}
	for _, c := range s.Controllers {
		if c.Healthy == 1 {
			controllersHealthy++
		} else {
			controllersUnhealthy++
		}
	}
	for _, w := range s.Webhooks {
		webhooksTotal++
		if w.Policy == "Fail" {
			webhooksFailPolicy++
		}
	}
	return controllersHealthy, controllersUnhealthy, webhooksTotal, webhooksFailPolicy
}
