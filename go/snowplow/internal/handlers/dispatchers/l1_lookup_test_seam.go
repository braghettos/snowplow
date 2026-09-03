// l1_lookup_test_seam.go — cross-package test seams over the dispatch-L1
// cell recorder (1.12.4 §3.3).
//
// WHY THESE ARE HERE AND NOT IN A _test.go FILE. The OTLP mirror lives in
// internal/metrics, a different package, and its falsifier (F2) has to
// populate real cells before asserting the attributes the mirror emits
// for them. Go test files are not importable across packages, so the
// seam must live in a production file — the same reason
// expvar_test_helpers.go and ResetResolvedCacheForTest exist.
//
// WHAT IS AND IS NOT BEING FALSIFIED THROUGH THIS SEAM. RecordL1Lookup
// ForTest is the recorder, which sits UPSTREAM of the code F2 tests: the
// accessor (DispatchL1LookupCells) and the observable callback that turns
// cells into attributed data points. So this is a fixture, not a stub of
// the thing under test — nothing below the assertion is being simulated.
// It delegates to the SAME recordL1Lookup the production dispatchers
// call, so a change to the cell key shape or the seed-attribution rule
// propagates into the falsifier rather than being papered over.
//
// Production code MUST NOT call either function.
package dispatchers

// RecordL1LookupForTest records one dispatch-L1 lookup observation
// against the live cell map, delegating to the production recorder.
//
// handlerKind is one of restactions / widgets / widgetContent — the same
// closed set the dispatchers pass. seededAtBoot is meaningful only when
// hit is true (it reflects the hit entry's ResolvedEntry.SeededAtBoot)
// and is ignored on a miss, exactly as in production.
func RecordL1LookupForTest(handlerKind, gvrString string, hit, seededAtBoot bool) {
	recordL1Lookup(handlerKind, gvrString, hit, seededAtBoot)
}

// ResetL1LookupCellsForTest clears every cell and the process-wide
// seed-attributable aggregate, so an arm asserts over its own
// observations only. The map grows monotonically in production and is
// never pruned, so without this a cross-package arm would assert against
// a set another test had already populated.
func ResetL1LookupCellsForTest() {
	l1LookupCells.Range(func(k, _ any) bool {
		l1LookupCells.Delete(k)
		return true
	})
	hitsSeedAttributable.Store(0)
}
