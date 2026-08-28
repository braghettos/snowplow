package jq

import "strings"

// gojqNilIterationMarker is the exact message gojq renders when a jq iterator
// (`.[]`, `.foo[]`) is applied to a null value — e.g. `.found[]` when the
// upstream data dictionary has no "found" key. gojq's iteratorError formats it
// as "cannot iterate over: " + typeErrorPreview(v), and typeErrorPreview(nil)
// is "null" (itchyny/gojq error.go:35,373-375; pinned by gojq's own
// query_test.go:221). A non-null non-iterable renders a different preview
// ("cannot iterate over: number (5)", …), so this marker matches ONLY the
// nil case and never a genuine data-shape mismatch.
const gojqNilIterationMarker = "cannot iterate over: null"

// IsBenignNilIteration reports whether err is the benign "iterator over a null
// value" case: an upstream stage produced no data for the key the iterator
// walks, so the fan-out yields zero items. This is semantically identical to an
// empty iterator (zero request options / zero refs; the stage simply
// continues) and is NOT an authoring fault — callers log it at DEBUG so it does
// not flood the WARN-floor firehose on a healthy cluster.
//
// Any OTHER jqutil.ForEach error — a malformed jq query, iterating over a
// non-null scalar, or a non-array result — is a genuine fault the caller MUST
// keep at ERROR so it is not buried.
func IsBenignNilIteration(err error) bool {
	return err != nil && strings.Contains(err.Error(), gojqNilIterationMarker)
}
