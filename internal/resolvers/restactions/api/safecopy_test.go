package api

import (
	"encoding/json"
	"reflect"
	"testing"
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
