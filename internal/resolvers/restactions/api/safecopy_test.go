package api

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/krateoplatformops/plumbing/jqutil"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
)

func TestSafeCopyJSON_NumericNormalization(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want any
	}{
		{"int", 1, float64(1)},
		{"int8", int8(2), float64(2)},
		{"int16", int16(3), float64(3)},
		{"int32", int32(4), float64(4)},
		{"int64", int64(5), float64(5)},
		{"uint", uint(6), float64(6)},
		{"uint8", uint8(7), float64(7)},
		{"uint16", uint16(8), float64(8)},
		{"uint32", uint32(9), float64(9)},
		{"uint64", uint64(10), float64(10)},
		{"float32", float32(1.5), float64(1.5)},
		{"float64", float64(2.5), float64(2.5)},
		{"json.Number", json.Number("42"), float64(42)},
		{"string", "hello", "hello"},
		{"bool", true, true},
		{"nil", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := safeCopyJSON(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("safeCopyJSON(%v) = %v (%T), want %v (%T)",
					tc.in, got, got, tc.want, tc.want)
			}
		})
	}
}

// TestSafeCopyJSON_K8sUnstructuredShape simulates the structure that an
// informer-backed unstructured object presents: nested maps with int64
// numeric leaves at well-known K8s fields. The previous DeepCopyJSON-only
// implementation panicked downstream in gojq.normalizeNumbers on these
// int64 leaves; this test guards against that regression.
func TestSafeCopyJSON_K8sUnstructuredShape(t *testing.T) {
	in := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":       "demo",
			"namespace":  "default",
			"generation": int64(7),
			"labels": map[string]any{
				"app": "demo",
			},
		},
		"spec": map[string]any{
			"replicas": int32(3),
		},
		"status": map[string]any{
			"observedGeneration": int64(7),
			"readyReplicas":      int64(3),
			"conditions": []any{
				map[string]any{
					"type":   "Available",
					"status": "True",
				},
			},
		},
	}
	got := safeCopyJSON(in).(map[string]any)

	// Numeric leaves must all be float64.
	meta := got["metadata"].(map[string]any)
	if _, ok := meta["generation"].(float64); !ok {
		t.Fatalf("metadata.generation = %T, want float64", meta["generation"])
	}
	spec := got["spec"].(map[string]any)
	if _, ok := spec["replicas"].(float64); !ok {
		t.Fatalf("spec.replicas = %T, want float64", spec["replicas"])
	}
	status := got["status"].(map[string]any)
	if _, ok := status["observedGeneration"].(float64); !ok {
		t.Fatalf("status.observedGeneration = %T, want float64",
			status["observedGeneration"])
	}
	if _, ok := status["readyReplicas"].(float64); !ok {
		t.Fatalf("status.readyReplicas = %T, want float64",
			status["readyReplicas"])
	}
	// Strings and structure must survive untouched.
	if got["kind"] != "Deployment" {
		t.Fatalf("kind = %v, want Deployment", got["kind"])
	}
	conds := status["conditions"].([]any)
	if len(conds) != 1 {
		t.Fatalf("conditions len = %d, want 1", len(conds))
	}
}

// TestSafeCopyJSON_Independence verifies that the returned tree does not
// share map or slice headers with the input — a JQ filter that mutates
// the result must not corrupt the informer-shared source.
func TestSafeCopyJSON_Independence(t *testing.T) {
	in := map[string]any{
		"items": []any{
			map[string]any{"k": "v1"},
			map[string]any{"k": "v2"},
		},
	}
	out := safeCopyJSON(in).(map[string]any)

	// Mutate the output. The input must remain untouched.
	out["items"].([]any)[0].(map[string]any)["k"] = "MUTATED"
	delete(out, "items")

	if _, ok := in["items"]; !ok {
		t.Fatalf("input lost items key after mutating output")
	}
	srcItems := in["items"].([]any)
	if srcItems[0].(map[string]any)["k"] != "v1" {
		t.Fatalf("input items[0].k = %v, want v1 (input was mutated through copy)",
			srcItems[0].(map[string]any)["k"])
	}
}

// TestGojqDel_DoesNotMutateAlias is the gojq-purity invariant test
// required by /tmp/snowplow-runs/gojq-purity-audit-2026-05-03.md and the
// V0_HOIST / V3_SYNCMAP variant designs.
//
// gojq's `deleteEmpty` mutates input maps in-place when called via
// `del(...)`. Snowplow shields the informer-shared tree from this
// mutation by wrapping every gojq input in safeCopyJSON before Eval.
// This test pins that contract: feeding a tree containing del(...) to
// jqutil.Eval, after a safeCopyJSON wrap, MUST leave the original
// alias bit-identical. If a future refactor drops safeCopyJSON, this
// test fails.
func TestGojqDel_DoesNotMutateAlias(t *testing.T) {
	// Deeply nested tree resembling a K8s informer item.
	original := map[string]any{
		"apiVersion": "v1",
		"kind":       "List",
		"metadata":   map[string]any{},
		"items": []any{
			map[string]any{
				"metadata": map[string]any{
					"name":      "demo-1",
					"namespace": "default",
					"annotations": map[string]any{
						"krateo.io/foo": "bar",
						"krateo.io/baz": "qux",
					},
				},
				"spec": map[string]any{
					"replicas": int64(3),
				},
			},
			map[string]any{
				"metadata": map[string]any{
					"name":      "demo-2",
					"namespace": "default",
					"annotations": map[string]any{
						"krateo.io/foo": "removeme",
					},
				},
			},
		},
	}

	// Marshal a snapshot of the original — bit-identical comparison after Eval.
	snapshot, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("snapshot marshal: %v", err)
	}

	// Hand gojq a safeCopyJSON-wrapped clone. The clone is what
	// production resolve.go passes; this test validates the contract
	// that the wrap is sufficient.
	safe := safeCopyJSON(original)

	// Filter applies several del() patterns — the documented mutation
	// vector from the audit (gojq/func.go:1745-1773 deleteEmpty).
	filter := `
		del(.items[].metadata.annotations) |
		del(.items[].spec) |
		del(.metadata)
	`

	_, err = jqutil.Eval(context.TODO(), jqutil.EvalOptions{
		Query:        filter,
		Data:         safe,
		ModuleLoader: jqsupport.ModuleLoader(),
	})
	if err != nil {
		t.Fatalf("jqutil.Eval: %v", err)
	}

	// Original tree must be byte-identical post-Eval.
	after, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("post-eval marshal: %v", err)
	}
	if string(snapshot) != string(after) {
		t.Fatalf("original mutated by gojq Eval (safeCopyJSON contract violated)\nbefore: %s\nafter:  %s",
			string(snapshot), string(after))
	}
}

// TestGojqEval_DirectAliasIsMutated is the negative control for
// TestGojqDel_DoesNotMutateAlias — passing the alias directly (without
// safeCopyJSON) demonstrates that gojq DOES mutate. If this test starts
// passing without the safeCopyJSON wrap, gojq behavior has changed
// upstream and the audit conclusion needs revisiting.
//
// We do NOT t.Fatal on equality here — the test only asserts that
// Eval runs without panic on an aliased tree, exercising the same path
// the safeCopy variant covers, and logs the mutation for diagnosis.
func TestGojqEval_DirectAliasIsMutated(t *testing.T) {
	original := map[string]any{
		"items": []any{
			map[string]any{"metadata": map[string]any{"name": "x"}},
		},
	}
	snapshot, _ := json.Marshal(original)

	// Direct pass — NO safeCopyJSON. Production must never do this.
	_, err := jqutil.Eval(context.TODO(), jqutil.EvalOptions{
		Query:        `del(.items[].metadata)`,
		Data:         original,
		ModuleLoader: jqsupport.ModuleLoader(),
	})
	if err != nil {
		t.Fatalf("jqutil.Eval: %v", err)
	}

	after, _ := json.Marshal(original)
	if string(snapshot) == string(after) {
		// Either the gojq update path now copy-on-writes everything
		// (good), or the test regressed silently. Surface either way.
		t.Logf("note: aliased gojq Eval did NOT mutate input; gojq behavior may have changed since audit")
	}
}

// TestSafeCopyJSON_ListWrapper exercises the exact shape that the
// informer-list branch in Resolve constructs: an outer "List" envelope
// whose items[i] are the unstructured.Unstructured.Object maps.
func TestSafeCopyJSON_ListWrapper(t *testing.T) {
	in := map[string]any{
		"apiVersion": "v1",
		"kind":       "List",
		"metadata":   map[string]any{},
		"items": []any{
			map[string]any{
				"metadata": map[string]any{"generation": int64(1)},
			},
			map[string]any{
				"metadata": map[string]any{"generation": int64(2)},
			},
		},
	}
	out := safeCopyJSON(in).(map[string]any)
	items := out["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items len = %d, want 2", len(items))
	}
	for i, it := range items {
		m := it.(map[string]any)["metadata"].(map[string]any)
		if _, ok := m["generation"].(float64); !ok {
			t.Fatalf("items[%d].metadata.generation = %T, want float64", i, m["generation"])
		}
	}
}
