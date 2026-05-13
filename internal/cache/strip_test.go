package cache

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
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

// TestStrip_TypedRBACOverridesRegistered confirms init() populated
// typedResourceOverrides for all four RBAC GVRs. 0.30.6 plan §Risks
// bullet 1 startup assertion is built on this; a regression here
// would panic NewResourceWatcher rather than ship silently.
func TestStrip_TypedRBACOverridesRegistered(t *testing.T) {
	for _, gvr := range rbacTypedGVRs {
		if _, ok := typedResourceOverrides[gvr]; !ok {
			t.Fatalf("typedResourceOverrides missing entry for %s", gvr)
		}
	}
	// AssertRBACTypedOverridesRegistered must NOT panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("AssertRBACTypedOverridesRegistered panicked: %v", r)
		}
	}()
	AssertRBACTypedOverridesRegistered()
}

// TestStrip_AssertRBACPanicsOnMissingOverride confirms the startup
// assertion DOES panic when an RBAC GVR is missing its override.
// Saves/restores the override map so other tests are unaffected.
func TestStrip_AssertRBACPanicsOnMissingOverride(t *testing.T) {
	gvr := rbacTypedGVRs[0]
	saved := typedResourceOverrides[gvr]
	delete(typedResourceOverrides, gvr)
	defer func() {
		typedResourceOverrides[gvr] = saved
		if r := recover(); r == nil {
			t.Fatalf("expected panic, got none")
		}
	}()
	AssertRBACTypedOverridesRegistered()
	t.Fatalf("AssertRBACTypedOverridesRegistered did not panic (unreachable)")
}

// TestStrip_TypedTransform_ProducesTypedPointer is the load-bearing
// 0.30.6 assertion: when the typed-converting transform fires on an
// RBAC GVR, the indexer-bound output is a typed pointer (e.g.
// *rbacv1.ClusterRoleBinding), NOT *unstructured.Unstructured. This
// is the contract internal/rbac/evaluate.go relies on for zero
// per-call FromUnstructured cost.
func TestStrip_TypedTransform_ProducesTypedPointer(t *testing.T) {
	resetStripLoggingForTest()
	cases := []struct {
		name     string
		gvr      schema.GroupVersionResource
		uns      *unstructured.Unstructured
		wantType string
	}{
		{
			name: "ClusterRoleBinding",
			gvr:  rbacTypedGVRs[3], // clusterrolebindings
			uns: &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "rbac.authorization.k8s.io/v1",
				"kind":       "ClusterRoleBinding",
				"metadata":   map[string]interface{}{"name": "demo-crb"},
				"subjects": []interface{}{
					map[string]interface{}{"kind": "User", "name": "alice", "apiGroup": "rbac.authorization.k8s.io"},
				},
				"roleRef": map[string]interface{}{
					"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "admin",
				},
			}},
			wantType: "*v1.ClusterRoleBinding",
		},
		{
			name: "RoleBinding",
			gvr:  rbacTypedGVRs[1], // rolebindings
			uns: &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "rbac.authorization.k8s.io/v1",
				"kind":       "RoleBinding",
				"metadata":   map[string]interface{}{"name": "demo-rb", "namespace": "ns-a"},
				"subjects": []interface{}{
					map[string]interface{}{"kind": "Group", "name": "devs", "apiGroup": "rbac.authorization.k8s.io"},
				},
				"roleRef": map[string]interface{}{
					"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "viewer",
				},
			}},
			wantType: "*v1.RoleBinding",
		},
		{
			name: "ClusterRole",
			gvr:  rbacTypedGVRs[2], // clusterroles
			uns: &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "rbac.authorization.k8s.io/v1",
				"kind":       "ClusterRole",
				"metadata":   map[string]interface{}{"name": "admin"},
				"rules": []interface{}{
					map[string]interface{}{
						"apiGroups": []interface{}{"*"},
						"resources": []interface{}{"*"},
						"verbs":     []interface{}{"*"},
					},
				},
			}},
			wantType: "*v1.ClusterRole",
		},
		{
			name: "Role",
			gvr:  rbacTypedGVRs[0], // roles
			uns: &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "rbac.authorization.k8s.io/v1",
				"kind":       "Role",
				"metadata":   map[string]interface{}{"name": "viewer", "namespace": "ns-a"},
				"rules": []interface{}{
					map[string]interface{}{
						"apiGroups": []interface{}{""},
						"resources": []interface{}{"secrets"},
						"verbs":     []interface{}{"get"},
					},
				},
			}},
			wantType: "*v1.Role",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetStripLoggingForTest()
			tf := StripBulkyFieldsForResourceType(gvrResourceTypeString(c.gvr), c.gvr)
			out, err := tf(c.uns)
			if err != nil {
				t.Fatalf("transform error: %v", err)
			}
			// Must be typed pointer, NOT Unstructured.
			if _, isUns := out.(*unstructured.Unstructured); isUns {
				t.Fatalf("transform returned *unstructured.Unstructured; want typed pointer (%s)", c.wantType)
			}
			// Type-specific assertion.
			switch c.gvr.Resource {
			case "clusterrolebindings":
				crb, ok := out.(*rbacv1.ClusterRoleBinding)
				if !ok {
					t.Fatalf("want *rbacv1.ClusterRoleBinding, got %T", out)
				}
				if len(crb.Subjects) != 1 || crb.Subjects[0].Name != "alice" {
					t.Fatalf("subject lost in conversion: %+v", crb.Subjects)
				}
				if crb.RoleRef.Name != "admin" {
					t.Fatalf("roleRef lost: %+v", crb.RoleRef)
				}
			case "rolebindings":
				rb, ok := out.(*rbacv1.RoleBinding)
				if !ok {
					t.Fatalf("want *rbacv1.RoleBinding, got %T", out)
				}
				if len(rb.Subjects) != 1 || rb.Subjects[0].Name != "devs" {
					t.Fatalf("subject lost: %+v", rb.Subjects)
				}
			case "clusterroles":
				cr, ok := out.(*rbacv1.ClusterRole)
				if !ok {
					t.Fatalf("want *rbacv1.ClusterRole, got %T", out)
				}
				if len(cr.Rules) != 1 || cr.Rules[0].Verbs[0] != "*" {
					t.Fatalf("rules lost: %+v", cr.Rules)
				}
			case "roles":
				r, ok := out.(*rbacv1.Role)
				if !ok {
					t.Fatalf("want *rbacv1.Role, got %T", out)
				}
				if len(r.Rules) != 1 || r.Rules[0].Resources[0] != "secrets" {
					t.Fatalf("rules lost: %+v", r.Rules)
				}
			}
		})
	}
}

// TestStrip_TypedTransform_MalformedFallsBack confirms that a
// malformed RBAC Unstructured (broken roleRef type) cannot panic and
// returns the original Unstructured for the downstream evaluate.go
// fallback path. The informer never sees an error. Plan §Risks bullet
// 2.
func TestStrip_TypedTransform_MalformedFallsBack(t *testing.T) {
	resetStripLoggingForTest()
	gvr := rbacTypedGVRs[3] // clusterrolebindings
	tf := StripBulkyFieldsForResourceType(gvrResourceTypeString(gvr), gvr)

	// roleRef has wrong type — FromUnstructured will fail.
	uns := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRoleBinding",
		"metadata":   map[string]interface{}{"name": "broken"},
		"roleRef":    "this-should-be-a-map-not-a-string",
	}}

	out, err := tf(uns)
	if err != nil {
		t.Fatalf("transform must never propagate error to informer: %v", err)
	}
	// Fallback contract: returns Unstructured (not nil, not typed).
	if _, isUns := out.(*unstructured.Unstructured); !isUns {
		t.Fatalf("malformed fallback expected *unstructured.Unstructured, got %T", out)
	}
}

// TestStrip_TypedTransform_StripsBeforeConverting confirms that the
// typed transform applies the default strip (managedFields +
// last-applied) BEFORE conversion — the typed object inherits the
// strip. This matters because the typed pointer in the indexer is the
// only copy; if strip didn't run, the typed object would carry the
// bulk fields forward.
func TestStrip_TypedTransform_StripsBeforeConverting(t *testing.T) {
	resetStripLoggingForTest()
	gvr := rbacTypedGVRs[3] // clusterrolebindings
	tf := StripBulkyFieldsForResourceType(gvrResourceTypeString(gvr), gvr)

	uns := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRoleBinding",
		"metadata": map[string]interface{}{
			"name": "demo-crb",
			"annotations": map[string]interface{}{
				"kubectl.kubernetes.io/last-applied-configuration": "should-be-dropped",
				"keep-me": "yes",
			},
			"managedFields": []interface{}{
				map[string]interface{}{"manager": "kubectl"},
			},
		},
		"roleRef": map[string]interface{}{
			"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "admin",
		},
	}}

	out, err := tf(uns)
	if err != nil {
		t.Fatalf("transform error: %v", err)
	}
	crb, ok := out.(*rbacv1.ClusterRoleBinding)
	if !ok {
		t.Fatalf("want *rbacv1.ClusterRoleBinding, got %T", out)
	}
	if _, present := crb.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; present {
		t.Fatalf("last-applied not dropped on typed path: %v", crb.Annotations)
	}
	if crb.Annotations["keep-me"] != "yes" {
		t.Fatalf("non-target annotation lost on typed path: %v", crb.Annotations)
	}
	if len(crb.ManagedFields) != 0 {
		t.Fatalf("managedFields not dropped on typed path: %v", crb.ManagedFields)
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
