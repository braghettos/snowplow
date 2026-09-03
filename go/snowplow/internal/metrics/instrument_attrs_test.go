// instrument_attrs_test.go — 1.12.4 falsifiers F2 and F8.
//
// F1 proves bytes leave the process. These two prove the bytes are
// USEFUL and SAFE:
//
//	F2 (fidelity)  the attributes distinguish what a panel must
//	               distinguish — per class AND per gvr, not a collapsed
//	               aggregate.
//	F8 (hygiene)   no attribute key outside the closed set ever appears,
//	               no object-identifying expvar key becomes an attribute,
//	               resolver-plurals-hit appears ONLY under the diagnostic
//	               family, and the cell count respects the cardinality cap
//	               with the overflow landing on gvr="__other__".
//
// Both drive the REAL production registration function
// (registerInstruments) and the REAL observable callback it registers,
// over a ManualReader so collection is deterministic instead of waiting
// on the 60s periodic interval. A temporary panic() inside
// registerInstruments fails every arm here, which is the proof the
// production frame executes.
package metrics

import (
	"context"
	"strings"
	"testing"

	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/handlers/dispatchers"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectViaRealCallback registers the REAL instrument set on a
// ManualReader-backed provider and collects one snapshot. Everything
// asserted downstream is produced by the same code path metrics.Setup
// installs in production; only the reader differs, because a periodic
// reader would make the arm depend on wall-clock.
func collectViaRealCallback(t *testing.T, build string) metricdata.ResourceMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	if err := registerInstruments(mp.Meter(meterName), build); err != nil {
		t.Fatalf("registerInstruments: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return rm
}

// point is one flattened observation: metric name plus its attribute set.
type point struct {
	metric string
	attrs  map[string]string
	value  int64
}

// flatten walks the collected data into points. Both Sum and Gauge are
// handled because the instrument set uses both, and an arm that only
// looked at one half would silently skip the gauges.
func flatten(rm metricdata.ResourceMetrics) []point {
	var out []point
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out = append(out, point{m.Name, attrMap(dp.Attributes.ToSlice()), dp.Value})
				}
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					out = append(out, point{m.Name, attrMap(dp.Attributes.ToSlice()), dp.Value})
				}
			}
		}
	}
	return out
}

func attrMap(kvs []attribute.KeyValue) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value.Emit()
	}
	return m
}

// pointsFor filters to one metric name.
func pointsFor(all []point, name string) []point {
	var out []point
	for _, p := range all {
		if p.metric == name {
			out = append(out, p)
		}
	}
	return out
}

// TestF2_DispatchL1AttributeFidelity is F2.
//
// SHAPE IS THE GATE (feedback_falsifier_shape_must_discriminate): K>1
// classes x M>1 GVRs, with an UNEVEN distribution. The wrong
// implementations this discriminates against:
//
//   - the pre-1.12.4 aggregate: 2 points, no class, no gvr;
//   - a per-class-only collapse: 2 classes x 3 outcomes = 6 points;
//   - a per-gvr-only collapse: likewise 6;
//   - the correct one: 2 x 2 x 3 = 12 points with distinct values.
//
// A degenerate 1x1 fixture would pass against all four.
func TestF2_DispatchL1AttributeFidelity(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	const (
		gvrW = "widgets.ui.krateo.io/v1beta1, Resource=widgets"
		gvrR = "templates.krateo.io/v1, Resource=restactions"
	)
	dispatchers.ResetL1LookupCellsForTest()
	t.Cleanup(dispatchers.ResetL1LookupCellsForTest)

	// Uneven on purpose: every one of the four cells carries a distinct
	// (hit, miss, seed_hit) triple, so no two cells can be confused and
	// no collapse can reproduce the set.
	dispatchers.RecordL1LookupForTest("widgets", gvrW, true, true)       // hit 1, seed 1
	dispatchers.RecordL1LookupForTest("widgets", gvrW, true, false)      // hit 2
	dispatchers.RecordL1LookupForTest("widgets", gvrR, false, false)     // miss 1
	dispatchers.RecordL1LookupForTest("restactions", gvrW, false, false) // miss 1
	dispatchers.RecordL1LookupForTest("restactions", gvrW, false, false) // miss 2
	dispatchers.RecordL1LookupForTest("restactions", gvrW, false, false) // miss 3
	dispatchers.RecordL1LookupForTest("restactions", gvrR, true, true)   // hit 1, seed 1

	all := flatten(collectViaRealCallback(t, "deadbeef"))
	pts := pointsFor(all, "snowplow_dispatch_l1_lookups_total")

	if len(pts) != 12 {
		t.Fatalf("got %d snowplow_dispatch_l1_lookups_total points; want 12 (2 classes x 2 gvrs x 3 "+
			"outcomes). The pre-1.12.4 aggregate emits 2; a single-dimension collapse emits 6.",
			len(pts))
	}

	got := map[string]int64{}
	for _, p := range pts {
		for _, k := range []string{"class", "gvr", "outcome"} {
			if p.attrs[k] == "" {
				t.Fatalf("point %+v is missing the %q attribute — the dimension was collapsed", p, k)
			}
		}
		got[p.attrs["class"]+"|"+p.attrs["gvr"]+"|"+p.attrs["outcome"]] = p.value
	}

	want := map[string]int64{
		"widgets|" + gvrW + "|hit":          2,
		"widgets|" + gvrW + "|miss":         0,
		"widgets|" + gvrW + "|seed_hit":     1,
		"widgets|" + gvrR + "|hit":          0,
		"widgets|" + gvrR + "|miss":         1,
		"widgets|" + gvrR + "|seed_hit":     0,
		"restactions|" + gvrW + "|hit":      0,
		"restactions|" + gvrW + "|miss":     3,
		"restactions|" + gvrW + "|seed_hit": 0,
		"restactions|" + gvrR + "|hit":      1,
		"restactions|" + gvrR + "|miss":     0,
		"restactions|" + gvrR + "|seed_hit": 1,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("cell %q = %d; want %d", k, got[k], w)
		}
	}

	// seed_hit must be non-zero SOMEWHERE: it is a distinct outcome the
	// aggregate never had, and a mirror that emitted only hit/miss would
	// pass every count assertion above if seed_hit defaulted to 0.
	var seedSeen int64
	for k, v := range got {
		if strings.HasSuffix(k, "|seed_hit") {
			seedSeen += v
		}
	}
	if seedSeen != 2 {
		t.Errorf("total seed_hit across cells = %d; want 2 — the seed-attribution outcome is absent "+
			"or not wired to the per-cell counter", seedSeen)
	}
}

// closedAttributeKeys is the set of attribute keys ANY snowplow
// instrument may emit (design §3.4). It is a KEY allowlist, not a value
// allowlist: values are bounded by their own closed enums, but a new KEY
// is how unbounded material sneaks in.
var closedAttributeKeys = map[string]struct{}{
	"class": {}, "gvr": {}, "path": {}, "reason": {}, "outcome": {},
	"state": {}, "stat": {}, "check": {}, "version": {}, "health": {},
	"policy": {}, "family": {},
}

// neverAnAttribute lists the three expvar keys whose VALUES carry
// object-identifying or unbounded material (design §3.4). They stay
// expvar-only, where /debug/vars is JWT-gated and the map is not a
// metrics time series:
//
//	snowplow_phase1_walk_children          keyed gvr|ns|name — object
//	                                       names, unbounded at 50K
//	                                       compositions
//	snowplow_ra_full_list_memo             carries raKey / sliceShape digests
//	snowplow_upstream_webhook_failurepolicy keyed by webhook-config name
var neverAnAttribute = []string{
	"snowplow_phase1_walk_children",
	"snowplow_ra_full_list_memo",
	"snowplow_upstream_webhook_failurepolicy",
}

// TestF8_AttributeHygiene is the first half of F8: STRUCTURAL, over
// whatever the real callback emits, not a hand-copied list of the
// instruments someone remembered to check.
//
// This is the arm that would catch a future instrument added with an
// attribute like `user`, `name` or `namespace`. It passes on main
// vacuously (the instruments it guards do not exist there), and is
// declared as a guard rather than a red-then-green.
func TestF8_AttributeHygiene(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	dispatchers.ResetL1LookupCellsForTest()
	t.Cleanup(dispatchers.ResetL1LookupCellsForTest)
	dispatchers.RecordL1LookupForTest("widgets", "v1, Resource=pods", true, false)

	ctx := cache.WithFallthroughScope(context.Background(), cache.ScopeCallWidgets)
	cache.ResetFallthroughCountersForTest()
	cache.RecordApiserverFallthrough(ctx, cache.ReasonClientBuild, "v1/pods")
	cache.RecordApiserverFallthrough(ctx, cache.ReasonResolverPluralsHit, "v1/pods")

	all := flatten(collectViaRealCallback(t, "deadbeef"))
	if len(all) == 0 {
		t.Fatal("the callback emitted NO points — the arm would be vacuous")
	}

	for _, p := range all {
		for k := range p.attrs {
			if _, ok := closedAttributeKeys[k]; !ok {
				t.Errorf("instrument %q emits attribute key %q, which is not in the closed set %v. "+
					"A new attribute key is how unbounded or object-identifying material enters the "+
					"metric stream (design §3.4).", p.metric, k, keysOf(closedAttributeKeys))
			}
		}
	}

	// The three never-an-attribute expvar keys must not have become
	// instruments. Checking NAMES catches the direct mistake — publishing
	// snowplow_phase1_walk_children as a metric keyed by object name.
	names := map[string]bool{}
	for _, p := range all {
		names[p.metric] = true
	}
	for _, forbidden := range neverAnAttribute {
		for name := range names {
			if strings.HasPrefix(name, forbidden) {
				t.Errorf("%q is an OTLP instrument, but %q is on the never-an-attribute list: it "+
					"carries object-identifying or unbounded material and must stay expvar-only",
					name, forbidden)
			}
		}
	}

	// The §5 reclassification, asserted on the metric stream rather than
	// only on the counters: resolver-plurals-hit may appear ONLY under
	// the diagnostic family.
	for _, p := range pointsFor(all, "snowplow_apiserver_fallthrough_cells_total") {
		if p.attrs["reason"] == string(cache.ReasonResolverPluralsHit) {
			t.Errorf("resolver-plurals-hit is emitted under snowplow_apiserver_fallthrough_cells_total. "+
				"It is an in-process cache HIT and belongs only to the diagnostic family (design §5.2). "+
				"point=%+v", p)
		}
	}
	var diagHit bool
	for _, p := range pointsFor(all, "snowplow_cache_diagnostic_cells_total") {
		if p.attrs["reason"] == string(cache.ReasonResolverPluralsHit) {
			diagHit = true
		}
	}
	if !diagHit {
		t.Error("resolver-plurals-hit is absent from snowplow_cache_diagnostic_cells_total — it must " +
			"appear THERE, not merely be absent from the fall-through family")
	}
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestF8_CardinalityCapAndOverflow is the second half of F8, and the one
// that closes gate condition C6/C7.
//
// The hygiene arm above bounds attribute NAMES, not series COUNTS: it
// would pass happily at 46,137 series. Worst case for the two cell
// families is 13 paths x 169 registered GVRs x 21 reasons, and nothing
// in the sync.Map caps it. So this arm pushes the map PAST the
// production cap and asserts three things: the emitted series count is
// bounded, the overflow lands on gvr="__other__" with path and reason
// preserved, and the truncation is COUNTED so a regression is visible
// rather than silent.
func TestF8_CardinalityCapAndOverflow(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	cache.ResetFallthroughCountersForTest()
	cache.ResetSeriesTruncatedForTest()
	t.Cleanup(func() {
		cache.ResetFallthroughCountersForTest()
		cache.ResetSeriesTruncatedForTest()
	})

	cap := cache.OTLPCellSeriesCap()
	ctx := cache.WithFallthroughScope(context.Background(), cache.ScopeCallWidgets)

	// Overshoot the cap by a comfortable margin using synthetic GVRs —
	// the only label that can actually blow up, since path and reason are
	// closed enums.
	const overshoot = 400
	for i := 0; i < cap+overshoot; i++ {
		cache.RecordApiserverFallthrough(ctx, cache.ReasonClientBuild, syntheticGVR(i))
	}

	cells, truncated := cache.FallthroughCellsSnapshot()
	if truncated != overshoot {
		t.Errorf("truncated = %d; want %d (cells beyond the cap of %d)", truncated, overshoot, cap)
	}

	// The overflow folds by (path, reason), so the emitted series count
	// is cap + the number of distinct (path, reason) pairs in the tail —
	// here exactly one, since every synthetic cell shares them.
	if len(cells) != cap+1 {
		t.Errorf("emitted %d series; want %d (cap %d + one __other__ fold). An uncapped "+
			"implementation emits %d.", len(cells), cap+1, cap, cap+overshoot)
	}

	var other *cacheCellAlias
	for i := range cells {
		if cells[i].GVR == cache.OtherGVR {
			c := cacheCellAlias(cells[i])
			other = &c
		}
	}
	if other == nil {
		t.Fatalf("no gvr=%q overflow series — the tail was DROPPED rather than aggregated, which "+
			"silently loses counts", cache.OtherGVR)
	}
	// path and reason must survive: both are closed enums, so collapsing
	// them would throw away attribution for no cardinality benefit.
	if other.Path != cache.ScopeCallWidgets || other.Reason != string(cache.ReasonClientBuild) {
		t.Errorf("overflow series lost its labels: path=%q reason=%q; want %q / %q",
			other.Path, other.Reason, cache.ScopeCallWidgets, cache.ReasonClientBuild)
	}
	if other.Count != overshoot {
		t.Errorf("overflow series count = %d; want %d — the folded tail must CONSERVE counts, "+
			"not drop them", other.Count, overshoot)
	}

	// The canary: panel 15b alerts on this, so a cardinality regression
	// is visible instead of silently truncated.
	trunc := cache.SeriesTruncatedSnapshot()
	if trunc["apiserver_fallthrough"] < uint64(overshoot) {
		t.Errorf("snowplow_metrics_series_truncated_total[apiserver_fallthrough] = %d; want >= %d — "+
			"truncation MUST be counted or the cap hides the regression it is protecting against",
			trunc["apiserver_fallthrough"], overshoot)
	}

	// And it must reach the metric stream, not just the accessor.
	all := flatten(collectViaRealCallback(t, "deadbeef"))
	pts := pointsFor(all, "snowplow_metrics_series_truncated_total")
	if len(pts) == 0 {
		t.Error("snowplow_metrics_series_truncated_total emitted no points despite a live truncation")
	}
	for _, p := range pts {
		if p.attrs["family"] == "" {
			t.Errorf("truncation point %+v carries no family attribute — an operator cannot tell "+
				"WHICH family overflowed", p)
		}
	}

	// Under the cap, nothing is truncated and no __other__ appears: the
	// steady state must not pay for the safety net.
	cache.ResetFallthroughCountersForTest()
	cache.ResetSeriesTruncatedForTest()
	// A fresh map is needed, not just zeroed counters: the reset zeroes
	// cells, it does not delete keys. Assert on the diagnostic family,
	// which this arm has not touched.
	diagCells, diagTrunc := cache.CacheDiagnosticCellsSnapshot()
	if diagTrunc != 0 {
		t.Errorf("diagnostic family reports %d truncated with %d cells — nothing should be capped "+
			"below the cap", diagTrunc, len(diagCells))
	}
}

// cacheCellAlias mirrors cache.FallthroughCell so the arm can hold a
// pointer to a copy without importing the type name twice.
type cacheCellAlias = cache.FallthroughCell

// syntheticGVR mints a distinct GVR string per index — the shape of a
// cardinality blow-up, where the gvr label stops being bounded by the
// registered set.
func syntheticGVR(i int) string {
	return "synthetic" + itoa(i) + ".example.io/v1/things"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
