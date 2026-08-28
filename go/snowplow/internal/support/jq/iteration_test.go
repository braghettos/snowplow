package jq

import (
	"context"
	"testing"

	"github.com/krateo-platformops/plumbing/jqutil"
)

// TestIsBenignNilIteration drives REAL gojq (via jqutil.ForEach, the same call
// the resolvers use) so the discriminator is validated against gojq's actual
// error strings, not a hand-written approximation. If a gojq upgrade changes
// the "cannot iterate over: null" phrasing, the nilIteration case reds here —
// which is exactly when the setup.go / resourcesrefstemplate ERROR→DEBUG gates
// would silently start mis-classifying, so this test is the tripwire.
func TestIsBenignNilIteration(t *testing.T) {
	ctx := context.Background()
	forEachErr := func(query string, data map[string]any) error {
		return jqutil.ForEach(ctx, jqutil.EvalOptions{Query: query, Unquote: true, Data: data}, func(any) error { return nil })
	}

	cases := []struct {
		name  string
		query string
		data  map[string]any
		want  bool
	}{
		{
			// The observed production case: `.found[]` where "found" is absent
			// → gojq "cannot iterate over: null". Benign (zero items), DEBUG.
			name:  "iterate over absent key (null)",
			query: ".found[]",
			data:  map[string]any{},
			want:  true,
		},
		{
			// Explicit null value, same benign nil-iteration.
			name:  "iterate over explicit null",
			query: ".found[]",
			data:  map[string]any{"found": nil},
			want:  true,
		},
		{
			// Iterating a non-null scalar is a genuine data-shape fault
			// ("cannot iterate over: number") — must stay ERROR.
			name:  "iterate over a number",
			query: ".n[]",
			data:  map[string]any{"n": 5},
			want:  false,
		},
		{
			// A malformed jq query is an authoring fault — must stay ERROR.
			name:  "malformed query",
			query: ".[",
			data:  map[string]any{},
			want:  false,
		},
		{
			// A query that yields a non-array (jqutil.ForEach requires an
			// array result) is a genuine fault — must stay ERROR.
			name:  "non-array result",
			query: ".n",
			data:  map[string]any{"n": 5},
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := forEachErr(tc.query, tc.data)
			if err == nil {
				t.Fatalf("query %q: expected a ForEach error to classify, got nil", tc.query)
			}
			if got := IsBenignNilIteration(err); got != tc.want {
				t.Errorf("IsBenignNilIteration(%v) = %v, want %v (query %q)", err, got, tc.want, tc.query)
			}
		})
	}

	// Nil error is never a benign-nil-iteration (no error to classify).
	if IsBenignNilIteration(nil) {
		t.Error("IsBenignNilIteration(nil) = true, want false")
	}
}
