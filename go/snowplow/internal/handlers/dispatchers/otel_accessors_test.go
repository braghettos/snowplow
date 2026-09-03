// otel_accessors_test.go — 1.12.4 §3.3 acceptance for the two accessors
// added to this package: the per-(class, gvr) dispatch-L1 breakdown that
// the OTLP mirror was collapsing, and the readiness-backstop counter that
// had no accessor at all.
package dispatchers

import (
	"sort"
	"testing"
)

// resetL1LookupCellsForTest clears the package-level cell map so an arm
// sees only its own observations. The map is keyed by
// "<handlerKind>|<gvrString>" and grows monotonically in production; a
// test that inherited another arm's cells would assert on a moving set.
func resetL1LookupCellsForTest() {
	l1LookupCells.Range(func(k, _ any) bool {
		l1LookupCells.Delete(k)
		return true
	})
	hitsSeedAttributable.Store(0)
}

// TestDispatchL1LookupCells_PerClassPerGVR is the shape the aggregate
// could not express.
//
// K>1 x M>1 per feedback_falsifier_shape_must_discriminate: two classes
// across two GVRs, with a deliberately UNEVEN distribution. An
// implementation that collapsed either dimension — reporting per-class
// totals, or per-GVR totals, or the process-wide pair the previous
// accessor returned — cannot reproduce four distinct rows with these
// counts. A degenerate 1x1 fixture would pass against all three wrong
// implementations.
func TestDispatchL1LookupCells_PerClassPerGVR(t *testing.T) {
	resetL1LookupCellsForTest()
	t.Cleanup(resetL1LookupCellsForTest)

	const (
		gvrW = "widgets.ui.krateo.io/v1beta1, Resource=widgets"
		gvrR = "templates.krateo.io/v1, Resource=restactions"
	)

	// Drive the REAL recorder the dispatchers call, not the map directly.
	recordL1Lookup("widgets", gvrW, true, false)  // hit
	recordL1Lookup("widgets", gvrW, true, true)   // hit, seed-attributable
	recordL1Lookup("widgets", gvrR, false, false) // miss
	recordL1Lookup("restactions", gvrW, false, false)
	recordL1Lookup("restactions", gvrW, false, false)
	recordL1Lookup("restactions", gvrR, true, true) // hit, seed-attributable

	cells := DispatchL1LookupCells()
	if len(cells) != 4 {
		t.Fatalf("got %d cells; want 4 (2 classes x 2 GVRs). A collapsed dimension yields 2 or 1.",
			len(cells))
	}

	sort.Slice(cells, func(i, j int) bool {
		if cells[i].Class != cells[j].Class {
			return cells[i].Class < cells[j].Class
		}
		return cells[i].GVR < cells[j].GVR
	})

	want := []L1LookupCell{
		{Class: "restactions", GVR: gvrR, Hit: 1, Miss: 0, SeedHit: 1},
		{Class: "restactions", GVR: gvrW, Hit: 0, Miss: 2, SeedHit: 0},
		{Class: "widgets", GVR: gvrR, Hit: 0, Miss: 1, SeedHit: 0},
		{Class: "widgets", GVR: gvrW, Hit: 2, Miss: 0, SeedHit: 1},
	}
	for i := range want {
		if cells[i] != want[i] {
			t.Errorf("cell[%d] = %+v; want %+v", i, cells[i], want[i])
		}
	}

	// The class/gvr split must survive the key parse: a cell whose Class
	// still carries the pipe-joined key would silently double the
	// dashboard's class cardinality.
	for _, c := range cells {
		if c.Class != "widgets" && c.Class != "restactions" {
			t.Errorf("cell Class = %q — the '<handlerKind>|<gvr>' key was not split", c.Class)
		}
	}

	// The aggregate must remain consistent with the breakdown, since both
	// are published and a dashboard may divide one by the other.
	hit, miss := DispatchL1LookupTotals()
	var sumHit, sumMiss uint64
	for _, c := range cells {
		sumHit += c.Hit
		sumMiss += c.Miss
	}
	if hit != sumHit || miss != sumMiss {
		t.Errorf("aggregate (%d hit / %d miss) disagrees with the sum of cells (%d / %d)",
			hit, miss, sumHit, sumMiss)
	}
	if got := HitsSeedAttributable(); got != 2 {
		t.Errorf("HitsSeedAttributable() = %d; want 2", got)
	}
}

// TestReadinessBackstopFired_Accessor closes [C11]: the counter is the
// top boot SLI and was unreadable outside this package, so nothing could
// alert on it. A healthy boot leaves it 0, so the arm proves both the
// zero baseline and that a real backstop fire moves it.
func TestReadinessBackstopFired_Accessor(t *testing.T) {
	before := ReadinessBackstopFired()

	recordReadinessBackstop("phase1_timeout", 0, -1)

	if got := ReadinessBackstopFired(); got != before+1 {
		t.Errorf("ReadinessBackstopFired() %d -> %d across one recordReadinessBackstop; want +1",
			before, got)
	}
}
