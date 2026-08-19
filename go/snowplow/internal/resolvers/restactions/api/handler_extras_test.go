//go:build unit
// +build unit

// handler_extras_test.go — falsifiers for the step-filter `extras` sibling
// key feature. The per-stage RESTAction filter (spec.api[].filter) is
// evaluated against the wrapped envelope `pig`. Today `pig` carries only the
// stage response (under opts.key) plus a synthetic `slice`; this feature
// exposes the PURE request extras as a reserved sibling key `pig["extras"]`
// so the step filter can read `.extras.*` like every other jq surface.
//
// The five falsifiers below drive jsonHandlerCore directly (the post-decode
// core, behind every dispatch path):
//
//	(a) value present     — extras + `.extras.foo` filter → reads the value
//	(b) byte-identical     — no extras → pig has NO `extras` key (backward-compat)
//	(c) -race concurrent   — N goroutines share ONE extras map, COW-mutate +
//	                         read it; every goroutine sees the original
//	                         (proves the share-ref-not-deep-copy decision)
//	(d) cache-key distinct — two extras → distinct ComputeKey → distinct out
//	(e) stage-named-extras — a stage literally Named "extras" + request extras:
//	                         response WINS (collision Option A)
package api

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/krateo-platformops/plumbing/ptr"
	"github.com/krateo-platformops/snowplow/internal/cache"
)

// (a) value present: the step filter reads `.extras.foo` and gets the value.
func TestStepFilter_ExtrasValuePresent(t *testing.T) {
	ctx := context.Background()
	out := map[string]any{}
	opts := jsonHandlerOptions{
		key:    "stage",
		out:    out,
		extras: map[string]any{"foo": "bar"},
		filter: ptr.To(".extras.foo"),
	}
	if err := jsonHandlerCore(ctx, opts, map[string]any{"ignored": true}); err != nil {
		t.Fatalf("jsonHandlerCore: %v", err)
	}
	got := out["stage"]
	if got != "bar" {
		t.Fatalf("FALSIFIER (a) FAILED: step filter `.extras.foo` over extras {foo:bar} should yield %q, got %#v", "bar", got)
	}
}

// (b) byte-identical no-extras: with nil/empty extras, pig MUST NOT carry an
// `extras` key, and a non-`.extras` filter output is byte-identical to the
// pre-feature behaviour. We assert via a probe filter that yields the WHOLE
// pig envelope and check the absence of the `extras` key.
func TestStepFilter_NoExtras_NoExtrasKey(t *testing.T) {
	ctx := context.Background()

	// nil extras + a filter that returns the full envelope `pig` (a stage
	// named "stage" wrapping response {n:1}); the envelope must have exactly
	// the `stage` key — no `extras`, no `slice`.
	for _, tc := range []struct {
		name   string
		extras map[string]any
	}{
		{"nil", nil},
		{"empty", map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := map[string]any{}
			opts := jsonHandlerOptions{
				key:    "stage",
				out:    out,
				extras: tc.extras,
				filter: ptr.To("."), // identity → the whole pig envelope
			}
			if err := jsonHandlerCore(ctx, opts, map[string]any{"n": float64(1)}); err != nil {
				t.Fatalf("jsonHandlerCore: %v", err)
			}
			env, ok := out["stage"].(map[string]any)
			if !ok {
				t.Fatalf("expected map envelope, got %#v", out["stage"])
			}
			if _, hasExtras := env["extras"]; hasExtras {
				t.Fatalf("FALSIFIER (b) FAILED: no-extras request leaked an `extras` key into pig: %#v", env)
			}
			// byte-identical pre-feature shape: only the stage key present.
			if len(env) != 1 {
				t.Fatalf("FALSIFIER (b) FAILED: pig envelope should be byte-identical (only `stage`), got %#v", env)
			}
		})
	}
}

// (c) -race concurrent (MANDATORY): N goroutines run jsonHandlerCore against
// the SAME shared extras map. Each filter BOTH mutates a derived copy
// (`.extras + {seen:.x}`, `del(.extras.foo)`) AND reads `.extras.foo`. Every
// goroutine must observe the ORIGINAL extras.foo unchanged, and `go test
// -race` must be clean. This proves the COW share-reference decision: gojq is
// the krateo-platformops COW fork; input maps are never allocated() so mutating ops
// copy-on-write and reads can't mutate the shared input.
func TestStepFilter_ConcurrentSharedExtras_Race(t *testing.T) {
	ctx := context.Background()

	// ONE shared extras map — every goroutine's jsonHandlerCore aliases it.
	shared := map[string]any{
		"foo":    "bar",
		"nested": map[string]any{"k": "v"},
	}

	const goroutines = 16
	// Two filter shapes that BOTH derive-mutate and read .extras.foo:
	//  - `.extras + {seen: .stage.x}` builds a NEW object from extras → COW
	//  - `del(.extras.foo)` deletes the read key on a COW copy
	// Each appends `.extras.foo` read by returning it directly so we can
	// assert it's still "bar".
	filters := []string{
		`(.extras + {seen: .stage.x}) | .foo`,
		`(.extras | del(.foo)) as $d | .extras.foo`,
	}

	errs := make(chan string, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out := map[string]any{}
			opts := jsonHandlerOptions{
				key:    "stage",
				out:    out,
				extras: shared, // SHARED reference, not a copy
				filter: ptr.To(filters[i%len(filters)]),
			}
			if err := jsonHandlerCore(ctx, opts, map[string]any{"x": float64(i)}); err != nil {
				errs <- fmt.Sprintf("g%d: jsonHandlerCore err: %v", i, err)
				return
			}
			if got := out["stage"]; got != "bar" {
				errs <- fmt.Sprintf("g%d: expected .extras.foo == bar after COW mutate, got %#v", i, got)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	if bad, ok := <-errs; ok {
		t.Fatalf("FALSIFIER (c) FAILED: %s", bad)
	}

	// The shared map must be UNCHANGED after all concurrent COW mutations.
	if shared["foo"] != "bar" {
		t.Fatalf("FALSIFIER (c) FAILED: shared extras.foo was mutated cross-goroutine: %#v", shared["foo"])
	}
	if n, ok := shared["nested"].(map[string]any); !ok || n["k"] != "v" {
		t.Fatalf("FALSIFIER (c) FAILED: shared extras.nested was mutated: %#v", shared["nested"])
	}
}

// (d) cache-key distinctness: two different `extras` maps → different
// ComputeKey (mirrors internal/handlers/dispatchers/extras_cache_key_test.go),
// AND a `.extras`-reading step filter yields a different `out` per extras — so
// the two requests can't collide on one L1 cell. The key proof guards the L1
// keying; the out proof guards the served content.
func TestStepFilter_ExtrasCacheKeyDistinctness(t *testing.T) {
	ctx := context.Background()

	extrasA := map[string]any{"tenant": "acme"}
	extrasB := map[string]any{"tenant": "globex"}

	// L1 key half: distinct extras MUST fold to distinct ComputeKey. The
	// resolved-cache key folds the request extras (len>0) — see
	// cache.RAFullListKeyInputs / ResolvedKeyInputs.
	keyA := cache.ComputeKey(cache.ResolvedKeyInputs{Extras: extrasA})
	keyB := cache.ComputeKey(cache.ResolvedKeyInputs{Extras: extrasB})
	if keyA == keyB {
		t.Fatalf("FALSIFIER (d) FAILED: distinct extras produced the SAME ComputeKey %q — L1 collision risk", keyA)
	}

	// Served-content half: the same step filter `.extras.tenant` yields a
	// DIFFERENT out per extras, so two requests never serve each other's body.
	run := func(ex map[string]any) any {
		out := map[string]any{}
		opts := jsonHandlerOptions{
			key:    "stage",
			out:    out,
			extras: ex,
			filter: ptr.To(".extras.tenant"),
		}
		if err := jsonHandlerCore(ctx, opts, map[string]any{}); err != nil {
			t.Fatalf("jsonHandlerCore: %v", err)
		}
		return out["stage"]
	}
	outA, outB := run(extrasA), run(extrasB)
	if outA == outB {
		t.Fatalf("FALSIFIER (d) FAILED: distinct extras yielded the SAME filtered out %#v — content collision", outA)
	}
	if outA != "acme" || outB != "globex" {
		t.Fatalf("FALSIFIER (d) FAILED: filtered out should be acme/globex, got %#v / %#v", outA, outB)
	}
}

// (e) stage-named-`extras` collision: a stage literally Named "extras" with a
// non-empty request extras present → out["extras"] is the STAGE RESPONSE
// (response wins; collision Option A), and a `.extras` filter in that stage
// reads its OWN response, not the request extras. This locks in the write
// order: pig["extras"] (request) BEFORE pig[opts.key] (stage response).
func TestStepFilter_StageNamedExtras_ResponseWins(t *testing.T) {
	ctx := context.Background()
	out := map[string]any{}
	opts := jsonHandlerOptions{
		key:    "extras", // the stage is literally named "extras"
		out:    out,
		extras: map[string]any{"foo": "request-extras"},
		// the filter reads .extras.foo — with response-wins, .extras IS the
		// stage response {foo: stage-response}, so .extras.foo == stage value.
		filter: ptr.To(".extras.foo"),
	}
	if err := jsonHandlerCore(ctx, opts, map[string]any{"foo": "stage-response"}); err != nil {
		t.Fatalf("jsonHandlerCore: %v", err)
	}
	if got := out["extras"]; got != "stage-response" {
		t.Fatalf("FALSIFIER (e) FAILED: stage named `extras` should keep its OWN response (response wins), filter read %#v", got)
	}
}
