// scope_test.go — Issue #156 hermetic unit for ScopeForGVR (the two-step
// GVR→KindFor→RESTMapping→Scope). Uses a fake scopeRESTMapper — no real
// discovery client, no kind cluster.

package dynamic

import (
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeMapper implements scopeRESTMapper. It maps a single GVR→GVK and a
// single GroupKind→scope, returning errors otherwise so the miss path is
// exercised.
type fakeMapper struct {
	gvr        schema.GroupVersionResource
	gvk        schema.GroupVersionKind
	scope      meta.RESTScope
	kindForErr error
	mappingErr error
}

func (f fakeMapper) KindFor(gvr schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	if f.kindForErr != nil {
		return schema.GroupVersionKind{}, f.kindForErr
	}
	if gvr != f.gvr {
		return schema.GroupVersionKind{}, fmt.Errorf("no matches for %s", gvr.String())
	}
	return f.gvk, nil
}

func (f fakeMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if f.mappingErr != nil {
		return nil, f.mappingErr
	}
	if gk != f.gvk.GroupKind() {
		return nil, fmt.Errorf("no mapping for %s", gk.String())
	}
	return &meta.RESTMapping{
		Resource:         f.gvr,
		GroupVersionKind: f.gvk,
		Scope:            f.scope,
	}, nil
}

func TestScopeForGVR_ClusterScoped(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	m := fakeMapper{
		gvr:   gvr,
		gvk:   schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"},
		scope: meta.RESTScopeRoot,
	}
	namespaced, err := ScopeForGVR(m, gvr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if namespaced {
		t.Fatalf("clusterroles resolved namespaced=true, want false (cluster scope)")
	}
}

func TestScopeForGVR_Namespaced(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
	m := fakeMapper{
		gvr:   gvr,
		gvk:   schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"},
		scope: meta.RESTScopeNamespace,
	}
	namespaced, err := ScopeForGVR(m, gvr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !namespaced {
		t.Fatalf("roles resolved namespaced=false, want true (namespace scope)")
	}
}

func TestScopeForGVR_KindForMiss(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}
	m := fakeMapper{kindForErr: fmt.Errorf("no matches for kind")}
	_, err := ScopeForGVR(m, gvr)
	if err == nil {
		t.Fatalf("KindFor miss must return an error (fail-closed), got nil")
	}
}

func TestScopeForGVR_RESTMappingMiss(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}
	m := fakeMapper{
		gvr:        gvr,
		gvk:        schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"},
		mappingErr: fmt.Errorf("no mapping"),
	}
	_, err := ScopeForGVR(m, gvr)
	if err == nil {
		t.Fatalf("RESTMapping miss must return an error (fail-closed), got nil")
	}
}

func TestScopeForGVR_NilMapper(t *testing.T) {
	_, err := ScopeForGVR(nil, schema.GroupVersionResource{Resource: "clusterroles"})
	if err == nil {
		t.Fatalf("nil mapper must return an error, got nil")
	}
}
