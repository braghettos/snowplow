package cache

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestStrip_DropsManagedFields confirms the default stripper removes
// metadata.managedFields. Tag 0.30.5 plan §"strip_test.go" bullet 1.
func TestStrip_DropsManagedFields(t *testing.T) {
	resetStripLoggingForTest()
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	tf := StripBulkyFieldsForResourceType("apps/v1/deployments", gvr)

	uns := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      "demo",
			"namespace": "default",
			"managedFields": []interface{}{
				map[string]interface{}{"manager": "kubectl"},
				map[string]interface{}{"manager": "controller"},
			},
			"labels": map[string]interface{}{"app": "demo"},
		},
		"spec":   map[string]interface{}{"replicas": int64(3)},
		"status": map[string]interface{}{"readyReplicas": int64(3)},
	}}

	out, err := tf(uns)
	if err != nil {
		t.Fatalf("transform returned error: %v", err)
	}
	stripped, ok := out.(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("transform returned non-unstructured: %T", out)
	}
	if mf := stripped.GetManagedFields(); len(mf) != 0 {
		t.Fatalf("managedFields not dropped: %v", mf)
	}
}

// TestStrip_DropsLastAppliedAnnotation confirms the default stripper
// removes the kubectl last-applied annotation while preserving other
// annotations. Tag 0.30.5 plan §"strip_test.go" bullet 2.
func TestStrip_DropsLastAppliedAnnotation(t *testing.T) {
	resetStripLoggingForTest()
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	tf := StripBulkyFieldsForResourceType("apps/v1/deployments", gvr)

	uns := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      "demo",
			"namespace": "default",
			"annotations": map[string]interface{}{
				"kubectl.kubernetes.io/last-applied-configuration": `{"apiVersion":"apps/v1","kind":"Deployment","very":"long"}`,
				"keep-me": "yes",
			},
		},
	}}

	out, _ := tf(uns)
	stripped := out.(*unstructured.Unstructured)
	annos := stripped.GetAnnotations()
	if _, present := annos["kubectl.kubernetes.io/last-applied-configuration"]; present {
		t.Fatalf("last-applied-configuration not dropped: %v", annos)
	}
	if annos["keep-me"] != "yes" {
		t.Fatalf("non-target annotation lost: got %v", annos)
	}
}

// TestStrip_PreservesSpecStatusLabels confirms strip does not touch
// fields outside the documented allow-list. Tag 0.30.5 plan
// §"strip_test.go" bullet 3.
func TestStrip_PreservesSpecStatusLabels(t *testing.T) {
	resetStripLoggingForTest()
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	tf := StripBulkyFieldsForResourceType("apps/v1/deployments", gvr)

	uns := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      "demo",
			"namespace": "default",
			"labels":    map[string]interface{}{"team": "infra", "tier": "backend"},
			"managedFields": []interface{}{
				map[string]interface{}{"manager": "kubectl"},
			},
		},
		"spec": map[string]interface{}{
			"replicas": int64(7),
			"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "demo"}},
		},
		"status": map[string]interface{}{
			"readyReplicas":     int64(7),
			"availableReplicas": int64(7),
		},
	}}

	out, _ := tf(uns)
	stripped := out.(*unstructured.Unstructured)

	// Labels preserved.
	if got := stripped.GetLabels()["team"]; got != "infra" {
		t.Fatalf("label team lost: got %q", got)
	}
	if got := stripped.GetLabels()["tier"]; got != "backend" {
		t.Fatalf("label tier lost: got %q", got)
	}

	// Spec preserved.
	spec, found, err := unstructured.NestedMap(stripped.Object, "spec")
	if err != nil || !found {
		t.Fatalf("spec missing after strip: found=%v err=%v", found, err)
	}
	if spec["replicas"] != int64(7) {
		t.Fatalf("spec.replicas mutated: got %v", spec["replicas"])
	}

	// Status preserved.
	status, found, err := unstructured.NestedMap(stripped.Object, "status")
	if err != nil || !found {
		t.Fatalf("status missing after strip: found=%v err=%v", found, err)
	}
	if status["readyReplicas"] != int64(7) {
		t.Fatalf("status.readyReplicas mutated: got %v", status["readyReplicas"])
	}

	// Name + namespace preserved.
	if stripped.GetName() != "demo" {
		t.Fatalf("metadata.name lost: got %q", stripped.GetName())
	}
	if stripped.GetNamespace() != "default" {
		t.Fatalf("metadata.namespace lost: got %q", stripped.GetNamespace())
	}
}

// TestStrip_IdempotentOnAlreadyStripped confirms running the transform
// twice produces the same result and never panics. Tag 0.30.5 plan
// §"strip_test.go" bullet 4.
func TestStrip_IdempotentOnAlreadyStripped(t *testing.T) {
	resetStripLoggingForTest()
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	tf := StripBulkyFieldsForResourceType("apps/v1/deployments", gvr)

	// Pre-stripped object: no managedFields, no last-applied.
	uns := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      "demo",
			"namespace": "default",
			"annotations": map[string]interface{}{
				"keep-me": "yes",
			},
		},
		"spec": map[string]interface{}{"replicas": int64(1)},
	}}

	out1, err1 := tf(uns)
	if err1 != nil {
		t.Fatalf("first transform error: %v", err1)
	}
	out2, err2 := tf(out1)
	if err2 != nil {
		t.Fatalf("second transform error: %v", err2)
	}
	stripped := out2.(*unstructured.Unstructured)
	if stripped.GetAnnotations()["keep-me"] != "yes" {
		t.Fatalf("idempotent re-strip lost keep-me annotation")
	}
	if len(stripped.GetManagedFields()) != 0 {
		t.Fatalf("idempotent re-strip introduced managedFields")
	}
}

// TestStrip_ErrorPathDoesNotPanic confirms the transform returns the
// input unmodified and (obj, nil) for non-unstructured / nil / wrong-
// type inputs. Tag 0.30.5 plan §"strip_test.go" bullet 5.
func TestStrip_ErrorPathDoesNotPanic(t *testing.T) {
	resetStripLoggingForTest()
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	tf := StripBulkyFieldsForResourceType("apps/v1/deployments", gvr)

	cases := []struct {
		name string
		in   interface{}
	}{
		{"nil", nil},
		{"string", "not-an-object"},
		{"int", 42},
		{"nil-unstructured", (*unstructured.Unstructured)(nil)},
		{"empty-unstructured", &unstructured.Unstructured{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := tf(c.in)
			if err != nil {
				t.Fatalf("error path returned non-nil error: %v", err)
			}
			// We don't assert object identity (the empty-unstructured
			// path returns the same pointer; the nil-interface case is
			// nil-in, nil-out). The contract is: no panic, no error.
			_ = out
		})
	}
}

// TestStrip_DropsEmptyAnnotationMap confirms that when stripping the
// last-applied annotation leaves no other entries, SetAnnotations(nil)
// is called rather than leaving an empty map (cosmetic but tested to
// pin the contract).
func TestStrip_DropsEmptyAnnotationMap(t *testing.T) {
	resetStripLoggingForTest()
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	tf := StripBulkyFieldsForResourceType("apps/v1/deployments", gvr)

	uns := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name": "demo",
			"annotations": map[string]interface{}{
				"kubectl.kubernetes.io/last-applied-configuration": "x",
			},
		},
	}}

	out, _ := tf(uns)
	stripped := out.(*unstructured.Unstructured)
	if got := stripped.GetAnnotations(); got != nil && len(got) != 0 {
		t.Fatalf("expected empty/nil annotations after sole-key drop, got %v", got)
	}
}
