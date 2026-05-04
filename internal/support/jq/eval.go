// Package jq adds a thin Eval helper on top of plumbing's jqutil.Eval that
// returns the JSON-decoded result instead of the marshaled string.
//
// applyUserAccessFilter (in resolvers/restactions/api) is the only caller
// today: it needs to take the value that a small jq expression (commonly
// just `.metadata.namespace` or `.`) produces against a single response
// item, and decide whether to keep, drop, or skip the item based on
// whether the value is a non-empty string.
//
// jqutil.Eval marshals iterator outputs back to JSON; this helper finishes
// the round-trip with a single Unmarshal so the caller can do a clean
// `v.(string)` type assertion.
package jq

import (
	"context"
	"encoding/json"

	"github.com/krateoplatformops/plumbing/jqutil"
)

// Eval runs `query` against `data` and returns the FIRST JSON-decoded
// result of the iterator. If the query produces no values the helper
// returns (nil, nil). A trailing array with multiple items is returned
// as a []any.
//
// The caller is responsible for type-asserting the result. Unmarshal
// errors propagate. Callers should not retain references to substructures
// of the returned value across goroutines without copying — the underlying
// objects are owned by gojq and may be mutated by subsequent evaluations.
func Eval(ctx context.Context, query string, data any) (any, error) {
	out, err := jqutil.Eval(ctx, jqutil.EvalOptions{
		Query:        query,
		Data:         data,
		ModuleLoader: ModuleLoader(),
	})
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, err
	}
	return v, nil
}
