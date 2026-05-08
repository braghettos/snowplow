// Q-OOM-COMPLETION (v0.25.315) Patch 3 — unit tests for the
// per-GVR narrow bulky-field strip.
//
// The strip is intentionally NARROW: only the three fields confirmed
// unused by every known widget JQ filter path are removed. Tests
// pin both the strip behaviour for composition CRs and the
// preservation of the load-bearing fields (uid, creationTimestamp,
// status.conditions, status.resourcesRefs.items) that production
// widgets read.
package cache

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// compositionGVR fabricates a GVR under composition.krateo.io.
func compositionGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "fireworksapp.composition.krateo.io",
		Version:  "v1alpha1",
		Resource: "fireworksapps",
	}
}

// widgetGVR fabricates a non-composition GVR (widget).
func widgetGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "widgets.templates.krateo.io",
		Version:  "v1beta1",
		Resource: "panels",
	}
}

// fixtureCompositionUns builds an unstructured composition CR with all
// the fields a strip path might touch.
func fixtureCompositionUns() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "fireworksapp.composition.krateo.io/v1alpha1",
		"kind":       "FireworksApp",
		"metadata": map[string]any{
			"name":              "demo-1",
			"namespace":         "ns-demo",
			"uid":               "uid-load-bearing",
			"creationTimestamp": "2026-05-07T08:00:00Z",
			"resourceVersion":   "12345",
			"generation":        int64(7),
			"annotations": map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": "{...heavy...}",
				"krateo.io/keep-me": "yes",
			},
			"managedFields": []any{map[string]any{"manager": "kubectl"}},
		},
		"spec": map[string]any{
			"foo": "bar",
		},
		"status": map[string]any{
			"observedGeneration": int64(7),
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True", "message": "load-bearing"},
			},
			"resourcesRefs": map[string]any{
				"items": []any{
					map[string]any{"name": "child-widget-1"},
				},
			},
		},
	}}
}

// TestStripBulkyFieldsForGVR_StripsResourceVersion — confirmed unused by
// widget JQ. Removed for compositions.
func TestStripBulkyFieldsForGVR_StripsResourceVersion(t *testing.T) {
	uns := fixtureCompositionUns()
	changed := StripBulkyFieldsForGVR(uns, compositionGVR())
	if !changed {
		t.Fatalf("StripBulkyFieldsForGVR: returned false; expected true (composition + bulky fields present)")
	}
	md := uns.Object["metadata"].(map[string]any)
	if _, exists := md["resourceVersion"]; exists {
		t.Errorf("metadata.resourceVersion: still present, want stripped")
	}
}

// TestStripBulkyFieldsForGVR_StripsGeneration — confirmed unused.
func TestStripBulkyFieldsForGVR_StripsGeneration(t *testing.T) {
	uns := fixtureCompositionUns()
	StripBulkyFieldsForGVR(uns, compositionGVR())
	md := uns.Object["metadata"].(map[string]any)
	if _, exists := md["generation"]; exists {
		t.Errorf("metadata.generation: still present, want stripped")
	}
}

// TestStripBulkyFieldsForGVR_StripsObservedGeneration — confirmed
// unused by widget JQ paths.
func TestStripBulkyFieldsForGVR_StripsObservedGeneration(t *testing.T) {
	uns := fixtureCompositionUns()
	StripBulkyFieldsForGVR(uns, compositionGVR())
	st, ok := uns.Object["status"].(map[string]any)
	if !ok {
		t.Fatalf("status missing post-strip")
	}
	if _, exists := st["observedGeneration"]; exists {
		t.Errorf("status.observedGeneration: still present, want stripped")
	}
}

// TestStripBulkyFieldsForGVR_PreservesUid — compositions-list filter
// (line 31 of restored-crs/restaction.compositions-list.yaml) reads
// `.metadata.uid`. Stripping would break the frontend.
func TestStripBulkyFieldsForGVR_PreservesUid(t *testing.T) {
	uns := fixtureCompositionUns()
	StripBulkyFieldsForGVR(uns, compositionGVR())
	md := uns.Object["metadata"].(map[string]any)
	uid, ok := md["uid"]
	if !ok {
		t.Errorf("metadata.uid: missing post-strip; compositions-list relies on this")
	}
	if uid != "uid-load-bearing" {
		t.Errorf("metadata.uid: corrupted post-strip: %v", uid)
	}
}

// TestStripBulkyFieldsForGVR_PreservesCreationTimestamp — sort_by in
// compositions-panels and blueprints-panels reads
// `.metadata.creationTimestamp`. compositions-list filter also
// references it (`ts: .metadata.creationTimestamp`).
func TestStripBulkyFieldsForGVR_PreservesCreationTimestamp(t *testing.T) {
	uns := fixtureCompositionUns()
	StripBulkyFieldsForGVR(uns, compositionGVR())
	md := uns.Object["metadata"].(map[string]any)
	ts, ok := md["creationTimestamp"]
	if !ok {
		t.Errorf("metadata.creationTimestamp: missing post-strip; sort_by widgets rely on this")
	}
	if ts != "2026-05-07T08:00:00Z" {
		t.Errorf("metadata.creationTimestamp: corrupted post-strip: %v", ts)
	}
}

// TestStripBulkyFieldsForGVR_PreservesStatusConditions —
// compositions-list filter reads `.status.conditions // []` and returns
// it to the frontend. Stripping would break the conditions chip.
func TestStripBulkyFieldsForGVR_PreservesStatusConditions(t *testing.T) {
	uns := fixtureCompositionUns()
	StripBulkyFieldsForGVR(uns, compositionGVR())
	st := uns.Object["status"].(map[string]any)
	conds, ok := st["conditions"]
	if !ok {
		t.Fatalf("status.conditions: missing post-strip; compositions-list relies on this")
	}
	condsList, _ := conds.([]any)
	if len(condsList) != 1 {
		t.Errorf("status.conditions: got %d entries, want 1 (preservation must be deep, not shallow)", len(condsList))
	}
}

// TestStripBulkyFieldsForGVR_PreservesStatusResourcesRefs — prewarm.go
// at line 219 (now 224 post-Patch-2) reads
// `status.resourcesRefs.items` to walk child widgets. Stripping
// status.resourcesRefs.items would break recursive prewarm.
func TestStripBulkyFieldsForGVR_PreservesStatusResourcesRefs(t *testing.T) {
	uns := fixtureCompositionUns()
	StripBulkyFieldsForGVR(uns, compositionGVR())
	st := uns.Object["status"].(map[string]any)
	rr, ok := st["resourcesRefs"].(map[string]any)
	if !ok {
		t.Fatalf("status.resourcesRefs: missing post-strip; prewarm.go relies on this for child-widget walk")
	}
	items, _ := rr["items"].([]any)
	if len(items) != 1 {
		t.Errorf("status.resourcesRefs.items: got %d, want 1 (deep preservation required)", len(items))
	}
}

// TestStripBulkyFieldsForGVR_PreservesNonCompositionGVR — for non-
// composition GVRs (e.g. widgets) the per-GVR strip is a no-op beyond
// the universal annotation/managedFields strip. resourceVersion and
// generation MUST stay on widget objects (some clients may rely on
// them; safer to leave the strip narrow to compositions).
func TestStripBulkyFieldsForGVR_PreservesNonCompositionGVR(t *testing.T) {
	uns := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Panel",
		"metadata": map[string]any{
			"name":            "panel-1",
			"namespace":       "ns-x",
			"resourceVersion": "999",
			"generation":      int64(3),
		},
		"status": map[string]any{
			"observedGeneration": int64(3),
		},
	}}
	StripBulkyFieldsForGVR(uns, widgetGVR())
	md := uns.Object["metadata"].(map[string]any)
	if _, exists := md["resourceVersion"]; !exists {
		t.Errorf("non-composition: metadata.resourceVersion missing; should be preserved")
	}
	if _, exists := md["generation"]; !exists {
		t.Errorf("non-composition: metadata.generation missing; should be preserved")
	}
	st := uns.Object["status"].(map[string]any)
	if _, exists := st["observedGeneration"]; !exists {
		t.Errorf("non-composition: status.observedGeneration missing; should be preserved")
	}
}

// TestStripBulkyFieldsForGVR_DisabledByEnv — operator can disable the
// per-GVR strip via CACHE_BULKY_STRIP_ENABLED=false. The universal
// annotation strip still runs (managedFields removed).
func TestStripBulkyFieldsForGVR_DisabledByEnv(t *testing.T) {
	t.Setenv(envCacheBulkyStripEnabled, "false")
	uns := fixtureCompositionUns()
	StripBulkyFieldsForGVR(uns, compositionGVR())

	// Per-GVR strip should NOT run.
	md := uns.Object["metadata"].(map[string]any)
	if _, exists := md["resourceVersion"]; !exists {
		t.Errorf("disabled: metadata.resourceVersion stripped; should be preserved when env=false")
	}
	st := uns.Object["status"].(map[string]any)
	if _, exists := st["observedGeneration"]; !exists {
		t.Errorf("disabled: status.observedGeneration stripped; should be preserved when env=false")
	}

	// Universal annotation strip should STILL run.
	if _, exists := md["managedFields"]; exists {
		t.Errorf("disabled: managedFields not stripped; universal strip must always run")
	}
	annPost, ok := md["annotations"].(map[string]any)
	if ok {
		if _, exists := annPost["kubectl.kubernetes.io/last-applied-configuration"]; exists {
			t.Errorf("disabled: last-applied-configuration not stripped; universal strip must always run")
		}
	}
}

// TestStripBulkyFieldsForGVR_Idempotent — calling twice returns the
// same result and produces the same object. No double-mutation, no
// panics.
func TestStripBulkyFieldsForGVR_Idempotent(t *testing.T) {
	uns := fixtureCompositionUns()
	first := StripBulkyFieldsForGVR(uns, compositionGVR())
	if !first {
		t.Fatalf("first call: changed=false, want true")
	}
	second := StripBulkyFieldsForGVR(uns, compositionGVR())
	if second {
		t.Errorf("second call: changed=true, want false (idempotent)")
	}
	// Sanity: load-bearing fields still present.
	md := uns.Object["metadata"].(map[string]any)
	if _, ok := md["uid"]; !ok {
		t.Errorf("idempotent: uid lost on second call")
	}
}

// TestIsCompositionGVR — the suffix match must accept all composition
// subgroups (e.g. fireworksapp.composition.krateo.io) and reject
// non-composition groups (widgets.templates.krateo.io,
// templates.krateo.io, core.krateo.io).
func TestIsCompositionGVR(t *testing.T) {
	cases := []struct {
		group string
		want  bool
	}{
		{"composition.krateo.io", true},
		{"fireworksapp.composition.krateo.io", true},
		{"some.weird.composition.krateo.io", true},
		{"widgets.templates.krateo.io", false},
		{"templates.krateo.io", false},
		{"core.krateo.io", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.group, func(t *testing.T) {
			gvr := schema.GroupVersionResource{Group: tc.group, Version: "v1", Resource: "x"}
			if got := IsCompositionGVR(gvr); got != tc.want {
				t.Errorf("IsCompositionGVR(%q): got %v, want %v", tc.group, got, tc.want)
			}
		})
	}
}
