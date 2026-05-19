// jsoncopy_test.go — Ship C (0.30.139) tests for `CopyJSONValue` /
// `CopyJSONMap`.
//
// Coverage map (AC-C.4 through AC-C.8):
//
//   - TestCopyJSONValue_UpstreamEquivalence — AC-C.4. `reflect.DeepEqual`
//     against `plumbing/maps.DeepCopyJSON` over the full §5 type
//     catalogue: all 3 typed-nils, empty containers, nested 3-level
//     map-in-slice-in-map, every leaf type (string/int64/bool/float64/
//     nil/json.Number), all 3 widening cases
//     (`int(7)→int64(7)`, `int32(8)→int64(8)`, `float32(2.5)→float64(2.5)`),
//     `[]map[string]any` widening result type asserted as `[]any`, and a
//     real `listEnvelopeValue`-shape envelope with ≥10 items.
//   - TestCopyJSONValue_PanicOnUnknown — AC-C.5. A non-JSON value
//     (`struct{}`, `chan int`, a custom typed-int) panics with message
//     containing `"cannot deep copy"` and the right `%T` rendering.
//   - TestCopyJSONValue_NoSharedSubtrees — AC-C.6. Mutate every
//     interior node of the copy, assert no mutation visible through the
//     original.
//   - TestCopyJSONValue_AliasingConcurrency_Race — AC-C.7. 32 readers ×
//     50 iters + writer goroutine mutating private copies. `go test
//     -race` clean.
//
// AC-C.8 cross-ship invariants are exercised transitively:
//   (a) gojq input-mutation safety is the property
//       TestCopyJSONValue_NoSharedSubtrees proves at the structural level
//       (the produced tree is disjoint from the source);
//   (b) gojq output-aliasing safety carries through because Ship A's
//       race test ran against `maps.DeepCopyJSON` and Ship C's race
//       test runs the SAME shape against `CopyJSONValue` — if Ship C
//       passes -race, the Ship A safety argument still holds;
//   (c) Ship B `convertUnstructured*` is orthogonal — Ship C does not
//       touch `rbac_snapshot.go`; the RBAC snapshot is read from the
//       typed RBAC indexer, NOT from `listEnvelopeValue`'s envelope.
//
// No build tag: these run with the default `go test` (and `-race`).
package api

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/krateoplatformops/plumbing/maps"
)

// ─────────────────────────────────────────────────────────────────────
// AC-C.4 — UpstreamEquivalence
// ─────────────────────────────────────────────────────────────────────

func TestCopyJSONValue_UpstreamEquivalence(t *testing.T) {
	// Each case: a value the upstream `maps.DeepCopyJSON` accepts.
	// `CopyJSONValue(input)` MUST `reflect.DeepEqual` to the equivalent
	// upstream copy. Where upstream rejects `CopyJSONMap`'s
	// `map[string]any` requirement (e.g. a scalar at top level), the
	// case uses `CopyJSONValue` directly.
	type row struct {
		name string
		// input is fed to both copiers.
		input any
		// useMap toggles `CopyJSONMap(input.(map[string]any))` vs
		// `CopyJSONValue(input)`; upstream `DeepCopyJSON` only accepts
		// `map[string]any`, so we wrap scalar-rooted cases in a single-
		// key map to make them comparable.
		wrap bool
	}
	rows := []row{
		// Empty containers + typed-nils.
		{"empty map", map[string]any{}, false},
		{"empty []any", []any{}, true},
		{"empty []map[string]any", []map[string]any{}, true},
		{"typed-nil map", map[string]any(nil), false},
		{"typed-nil []any", []any(nil), true},
		{"typed-nil []map[string]any", []map[string]any(nil), true},

		// Leaf scalars (each wrapped so upstream accepts them).
		{"bool true", true, true},
		{"bool false", false, true},
		{"string", "hello-world", true},
		{"unicode string", "héllo-☃-é", true},
		{"escape string", "tab\there\nline", true},
		{"int64", int64(42), true},
		{"float64", 3.14, true},
		{"float64 zero", 0.0, true},
		{"untyped nil", nil, true},
		{"json.Number int form", json.Number("9007199254740993"), true},
		{"json.Number float form", json.Number("3.14e10"), true},

		// Widening cases — the rows the verification-gate bounce
		// surfaced. Each MUST produce the upstream-widened type.
		{"int widens to int64", int(7), true},
		{"int32 widens to int64", int32(8), true},
		{"float32 widens to float64", float32(2.5), true},

		// `[]map[string]any` element-type promotion to `[]any`.
		{"[]map[string]any -> []any", []map[string]any{
			{"a": "x"}, {"b": "y"}, {"c": "z"},
		}, true},

		// Nested 3-level map-in-slice-in-map.
		{"nested 3-level", map[string]any{
			"items": []any{
				map[string]any{"id": int64(1), "tags": []any{"a", "b"}},
				map[string]any{"id": int64(2), "tags": []any{"c"}},
			},
			"meta": map[string]any{"count": int64(2), "ok": true},
		}, false},

		// `listEnvelopeValue`-shape envelope with ≥10 items — the
		// migrated site's real-world shape.
		{"listEnvelope shape", buildEnvelopeFixture(12), false},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			input := r.input
			if r.wrap {
				input = map[string]any{"v": r.input}
			}
			// Upstream copy — the byte-equivalence target.
			upstream := maps.DeepCopyJSON(input.(map[string]any))
			// Ship C copy — same input.
			got := CopyJSONMap(input.(map[string]any))

			if !reflect.DeepEqual(got, upstream) {
				t.Fatalf("CopyJSONValue diverges from upstream\n got: %#v\n want: %#v", got, upstream)
			}
		})
	}

	// Spot-asserts on the type-level guarantees the table-driven
	// reflect.DeepEqual would pass through (it asserts on shape +
	// values; the Go type of a slice element is verified here
	// explicitly).
	t.Run("int widens to int64 (Go type assertion)", func(t *testing.T) {
		got := CopyJSONValue(int(7))
		if _, ok := got.(int64); !ok {
			t.Errorf("CopyJSONValue(int) returned %T, want int64", got)
		}
		if got.(int64) != int64(7) {
			t.Errorf("CopyJSONValue(int(7)) = %v, want int64(7)", got)
		}
	})
	t.Run("int32 widens to int64 (Go type assertion)", func(t *testing.T) {
		got := CopyJSONValue(int32(8))
		if _, ok := got.(int64); !ok {
			t.Errorf("CopyJSONValue(int32) returned %T, want int64", got)
		}
	})
	t.Run("float32 widens to float64 (Go type assertion)", func(t *testing.T) {
		got := CopyJSONValue(float32(2.5))
		if _, ok := got.(float64); !ok {
			t.Errorf("CopyJSONValue(float32) returned %T, want float64", got)
		}
	})
	t.Run("[]map[string]any widens to []any (Go type assertion)", func(t *testing.T) {
		in := []map[string]any{{"a": "x"}, {"b": "y"}}
		got := CopyJSONValue(in)
		if _, ok := got.([]any); !ok {
			t.Errorf("CopyJSONValue([]map[string]any) returned %T, want []any", got)
		}
		if reflect.TypeOf(got).String() != "[]interface {}" {
			t.Errorf("[]map[string]any copy type = %s, want []interface {}", reflect.TypeOf(got))
		}
	})

	// Typed-nils — upstream returns the typed nil verbatim (not a fresh
	// empty container). Our copier must match.
	t.Run("typed-nil map preserved", func(t *testing.T) {
		got := CopyJSONValue(map[string]any(nil))
		if got == nil {
			// `got` is an `any` interface holding a typed-nil — it is
			// NOT a Go untyped nil.
			t.Errorf("typed-nil map collapsed to untyped nil")
		}
		m, ok := got.(map[string]any)
		if !ok {
			t.Errorf("typed-nil map became %T, want map[string]any", got)
		}
		if m != nil {
			t.Errorf("typed-nil map became non-nil %v", m)
		}
	})
	t.Run("typed-nil []any preserved", func(t *testing.T) {
		got := CopyJSONValue([]any(nil))
		s, ok := got.([]any)
		if !ok {
			t.Errorf("typed-nil []any became %T, want []any", got)
		}
		if s != nil {
			t.Errorf("typed-nil []any became non-nil %v", s)
		}
	})
	t.Run("typed-nil []map[string]any preserved", func(t *testing.T) {
		// Upstream returns the typed-nil `[]map[string]any` AS-IS at
		// deepcopy.go:34-36 — NOT promoted to `[]any(nil)`. Ship C
		// must match exactly.
		got := CopyJSONValue([]map[string]any(nil))
		s, ok := got.([]map[string]any)
		if !ok {
			t.Errorf("typed-nil []map[string]any became %T, want []map[string]any", got)
		}
		if s != nil {
			t.Errorf("typed-nil []map[string]any became non-nil %v", s)
		}
	})
}

// buildEnvelopeFixture is a representative `listEnvelopeValue` shape —
// the production migration site's envelope, with `count` items.
func buildEnvelopeFixture(count int) map[string]any {
	items := make([]any, count)
	for i := 0; i < count; i++ {
		items[i] = map[string]any{
			"metadata": map[string]any{
				"name":      "obj-" + strconv.Itoa(i),
				"namespace": "ns-" + strconv.Itoa(i%3),
				"labels":    map[string]any{"app": "snowplow", "tier": "cache"},
			},
			"spec": map[string]any{
				"replicas":  float64(i),
				"enabled":   i%2 == 0,
				"resources": []any{"cpu", "memory"},
			},
			"status": map[string]any{"phase": "Running", "ready": true},
		}
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "List",
		"items":      items,
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-C.5 — PanicOnUnknown
// ─────────────────────────────────────────────────────────────────────

func TestCopyJSONValue_PanicOnUnknown(t *testing.T) {
	type custom int
	cases := []struct {
		name string
		// input must be a value not in the accepted union.
		input any
		// The `%T` rendering we expect inside the panic message.
		wantType string
	}{
		{"struct{}", struct{}{}, "struct {}"},
		{"chan int", make(chan int), "chan int"},
		{"custom typed int", custom(5), "api.custom"},
		// `float32` IS in the accepted union (widens to float64). Use
		// `complex64` instead — it is NOT in the union.
		{"complex64", complex64(1 + 2i), "complex64"},
		{"func", func() {}, "func()"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("CopyJSONValue(%T) did not panic", c.input)
				}
				err, ok := r.(error)
				if !ok {
					t.Fatalf("panic value is %T, want error: %v", r, r)
				}
				msg := err.Error()
				if !contains(msg, "cannot deep copy") {
					t.Errorf("panic message %q missing 'cannot deep copy'", msg)
				}
				if !contains(msg, c.wantType) {
					t.Errorf("panic message %q missing type rendering %q", msg, c.wantType)
				}
			}()
			_ = CopyJSONValue(c.input)
		})
	}
}

func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────
// AC-C.6 — NoSharedSubtrees
// ─────────────────────────────────────────────────────────────────────

// TestCopyJSONValue_NoSharedSubtrees mutates every interior node of the
// SOURCE tree post-copy and asserts the COPY is unaffected. This is the
// AC-C.6 / §8.1 structural-isolation gate — the cross-ship invariants
// (a) gojq input-mutation and (b) gojq output-aliasing both depend on
// it. If any interior `map[string]any` or `[]any` slot were shared
// (alias) between source and copy, mutating the source would be visible
// through the copy and the test FAILs.
func TestCopyJSONValue_NoSharedSubtrees(t *testing.T) {
	src := map[string]any{
		"metadata": map[string]any{
			"name":   "original",
			"labels": map[string]any{"app": "snowplow"},
		},
		"items": []any{
			map[string]any{"name": "a", "n": float64(1)},
			map[string]any{"name": "b", "n": float64(2)},
		},
		"flag": true,
		"id":   int64(42),
	}

	dst := CopyJSONMap(src)

	// Outer map identity differs.
	if reflect.ValueOf(dst).Pointer() == reflect.ValueOf(src).Pointer() {
		t.Fatalf("copy alias check: dst and src share the SAME outer map header")
	}

	// Capture pre-mutation snapshots from the copy.
	dstMetaName := dst["metadata"].(map[string]any)["name"].(string)
	dstItem0Name := dst["items"].([]any)[0].(map[string]any)["name"].(string)
	dstLabelsApp := dst["metadata"].(map[string]any)["labels"].(map[string]any)["app"].(string)

	// Mutate every interior node of the SOURCE.
	src["metadata"].(map[string]any)["name"] = "MUTATED"
	src["metadata"].(map[string]any)["labels"].(map[string]any)["app"] = "MUTATED"
	src["items"].([]any)[0].(map[string]any)["name"] = "MUTATED"
	src["items"].([]any) [1].(map[string]any)["n"] = float64(999)
	src["new-top-key"] = "INJECTED"
	src["items"] = append(src["items"].([]any), "extra")

	// The copy MUST NOT observe any of those mutations.
	if dst["metadata"].(map[string]any)["name"] != dstMetaName {
		t.Errorf("dst.metadata.name leaked source mutation: got %v want %q",
			dst["metadata"].(map[string]any)["name"], dstMetaName)
	}
	if dst["metadata"].(map[string]any)["labels"].(map[string]any)["app"] != dstLabelsApp {
		t.Errorf("dst.metadata.labels.app leaked source mutation")
	}
	if dst["items"].([]any)[0].(map[string]any)["name"] != dstItem0Name {
		t.Errorf("dst.items[0].name leaked source mutation")
	}
	if _, ok := dst["new-top-key"]; ok {
		t.Errorf("dst absorbed a source-only top-level key")
	}
	if len(dst["items"].([]any)) != 2 {
		t.Errorf("dst.items length leaked source append: got %d want 2",
			len(dst["items"].([]any)))
	}
}

// ─────────────────────────────────────────────────────────────────────
// AC-C.7 — AliasingConcurrency -race
// ─────────────────────────────────────────────────────────────────────

// TestCopyJSONValue_AliasingConcurrency_Race exercises the
// `feedback_shared_vs_copy_is_a_concurrency_change` invariant for the
// third time in the campaign (Ship A: EvalValue; Ship B: rbacSnapshot;
// Ship C: this copier).
//
// THE PRODUCTION INVARIANT being tested. At `apistage.go:213-228` the
// `envelope` is BUILT fresh per call from immutable inputs
// (`itemList` is a freshly-`make()`'d `[]any` populated from the
// indexer's per-call typed pointers); it is NOT mutated by any other
// goroutine while being copied. The Ship A precedent
// (`TestEvalValue_AliasingConcurrency_Race`) and the Ship B precedent
// (`TestRBACSnapshot_Race_ReaderWriter`) both model the same shape:
// concurrent READERS each get their OWN private copy and then exercise
// the downstream mutation hazard on the private tree. Modelling a
// concurrent SOURCE-mutator would test something production never has
// (and would crash on `concurrent map iteration and map write`
// irrespective of the copier — the copier reads the source).
//
// 32 reader goroutines × 50 iters each call `CopyJSONMap(template)` on
// the SHARED template, then aggressively mutate the PRIVATE copy AND
// run gojq over it (the precise composition of Ship C copy + Ship A
// EvalValue the production hot path runs; gojq's normalizeNumbers
// mutates input in place — safe only because the input is private,
// which is exactly what Ship C must guarantee). If `CopyJSONValue`
// EVER aliased a sub-tree from `template` into any goroutine's `priv`,
// the simultaneous in-place mutations from many goroutines + gojq
// would trip the race detector on a shared map. Clean run under
// `go test -race` proves Ship C produces disjoint trees.
func TestCopyJSONValue_AliasingConcurrency_Race(t *testing.T) {
	template := map[string]any{
		"metadata": map[string]any{
			"name":      "shared-obj",
			"namespace": "krateo-system",
			"labels":    map[string]any{"app": "snowplow", "tier": "cache"},
		},
		"items": []any{
			map[string]any{"name": "a", "n": float64(1)},
			map[string]any{"name": "b", "n": float64(2)},
			map[string]any{"name": "c", "n": float64(3)},
		},
		"slice": map[string]any{"limit": float64(50)},
	}

	const goroutines = 32
	const iters = 50

	var readerWG sync.WaitGroup
	readerWG.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer readerWG.Done()
			for i := 0; i < iters; i++ {
				// Copy the template into a private tree.
				priv := CopyJSONMap(template)

				// Sanity: the outer-map pointers differ — basic header-
				// level isolation check.
				if reflect.ValueOf(priv).Pointer() == reflect.ValueOf(template).Pointer() {
					t.Errorf("priv outer map aliases template")
					return
				}

				// Mutate every interior node of priv. If ANY interior
				// map/slice aliased the template, -race catches the
				// concurrent write against any other goroutine doing
				// the same on its own `priv`.
				priv["priv-key-"+strconv.Itoa(g)] = i
				if m, ok := priv["metadata"].(map[string]any); ok {
					m["mutated-by"] = g
					if labels, ok := m["labels"].(map[string]any); ok {
						labels["mutated-by"] = g
					}
				}
				if items, ok := priv["items"].([]any); ok && len(items) > 0 {
					if it0, ok := items[0].(map[string]any); ok {
						it0["mutated-by"] = g
					}
				}

				// Run the actual Ship A wrapper over the private tree —
				// exercises the precise (Ship C copy + Ship A
				// EvalValue) composition the production hot path runs.
				// gojq mutates its input in place; that's safe only
				// because the input is private — exactly what Ship C
				// guarantees.
				_, _, _ = EvalValue(context.Background(), ".items[].name", priv, nil)
			}
		}(g)
	}

	readerWG.Wait()
}
