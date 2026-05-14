// inventory_test.go — Tag 0.30.92: unit coverage for the exported
// ParseAPIServerPathToGVR. The function is the load-bearing input to
// the resolver-side lazy-register hook (restactions/api/resolve.go);
// any parser regression silently disables informer registration for
// the affected GVR, which re-introduces the 0.30.91 evict_delete=0
// failure mode.

package cache

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestParseAPIServerPathToGVR(t *testing.T) {
	cases := []struct {
		name string
		path string
		want schema.GroupVersionResource
		ok   bool
	}{
		// /api/v1 shapes.
		{
			name: "core_namespaced_pods",
			path: "/api/v1/namespaces/demo/pods",
			want: schema.GroupVersionResource{Version: "v1", Resource: "pods"},
			ok:   true,
		},
		{
			name: "core_cluster_namespaces",
			path: "/api/v1/namespaces",
			want: schema.GroupVersionResource{Version: "v1", Resource: "namespaces"},
			ok:   true,
		},
		{
			name: "core_namespaced_single",
			path: "/api/v1/namespaces/demo/pods/foo",
			want: schema.GroupVersionResource{Version: "v1", Resource: "pods"},
			ok:   true,
		},
		// /apis/<group>/<version> shapes.
		{
			name: "apps_namespaced_deployments",
			path: "/apis/apps/v1/namespaces/demo/deployments",
			want: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			ok:   true,
		},
		{
			name: "rbac_cluster_clusterroles",
			path: "/apis/rbac.authorization.k8s.io/v1/clusterroles",
			want: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
			ok:   true,
		},
		{
			name: "composition_namespaced",
			path: "/apis/composition.krateo.io/v1/namespaces/bench/githubscaffoldingwithcompositionpages",
			want: schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: "githubscaffoldingwithcompositionpages"},
			ok:   true,
		},
		{
			name: "trailing_slash_stripped",
			path: "/api/v1/namespaces/demo/pods/",
			want: schema.GroupVersionResource{Version: "v1", Resource: "pods"},
			ok:   true,
		},
		{
			name: "query_string_stripped",
			path: "/apis/apps/v1/deployments?labelSelector=foo%3Dbar",
			want: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			ok:   true,
		},
		// Non-apiserver / malformed paths.
		{
			name: "external_https",
			path: "https://api.github.com/repos/foo/bar",
			ok:   false,
		},
		{
			name: "jq_template_leak",
			path: `${ "/api/v1/namespaces/" + (.) + "/pods" }`,
			ok:   false,
		},
		{
			name: "empty",
			path: "",
			ok:   false,
		},
		{
			name: "root_slash",
			path: "/",
			ok:   false,
		},
		{
			name: "apis_no_resource",
			path: "/apis/apps/v1",
			ok:   false,
		},
		{
			name: "api_no_resource",
			path: "/api/v1",
			ok:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseAPIServerPathToGVR(c.path)
			if ok != c.ok {
				t.Fatalf("ok=%v want %v (path=%q got=%v)", ok, c.ok, c.path, got)
			}
			if !ok {
				return
			}
			if got != c.want {
				t.Fatalf("gvr mismatch: got %v want %v (path=%q)", got, c.want, c.path)
			}
		})
	}
}

// TestParseAPIServerPathToDep covers the 8 canonical apiserver path
// shapes (4 cluster-scoped + 4 namespaced × list/named) Edge type 3
// records dep edges against. Per plan §0.30.94 "Concrete file:line
// changes — internal/cache/inventory.go" + feedback_no_special_cases.md
// (parser is uniform; no per-resource carve-outs).
func TestParseAPIServerPathToDep(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wantGVR schema.GroupVersionResource
		wantNs  string
		wantNm  string
		ok      bool
	}{
		// /api/v1 cluster-scoped LIST.
		{
			name:    "core_cluster_list",
			path:    "/api/v1/namespaces",
			wantGVR: schema.GroupVersionResource{Version: "v1", Resource: "namespaces"},
			wantNs:  "",
			wantNm:  "",
			ok:      true,
		},
		// /api/v1 cluster-scoped NAMED.
		{
			name:    "core_cluster_named",
			path:    "/api/v1/namespaces/demo",
			wantGVR: schema.GroupVersionResource{Version: "v1", Resource: "namespaces"},
			wantNs:  "",
			wantNm:  "demo",
			ok:      true,
		},
		// /api/v1 namespaced LIST.
		{
			name:    "core_namespaced_list_pods",
			path:    "/api/v1/namespaces/demo/pods",
			wantGVR: schema.GroupVersionResource{Version: "v1", Resource: "pods"},
			wantNs:  "demo",
			wantNm:  "",
			ok:      true,
		},
		// /api/v1 namespaced NAMED.
		{
			name:    "core_namespaced_named_pod",
			path:    "/api/v1/namespaces/demo/pods/foo",
			wantGVR: schema.GroupVersionResource{Version: "v1", Resource: "pods"},
			wantNs:  "demo",
			wantNm:  "foo",
			ok:      true,
		},
		// /apis/<group>/<version> cluster-scoped LIST.
		{
			name:    "apis_cluster_list",
			path:    "/apis/rbac.authorization.k8s.io/v1/clusterroles",
			wantGVR: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
			wantNs:  "",
			wantNm:  "",
			ok:      true,
		},
		// /apis/<group>/<version> cluster-scoped NAMED.
		{
			name:    "apis_cluster_named",
			path:    "/apis/rbac.authorization.k8s.io/v1/clusterroles/admin",
			wantGVR: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
			wantNs:  "",
			wantNm:  "admin",
			ok:      true,
		},
		// /apis/<group>/<version> namespaced LIST (the load-bearing
		// admin-compositions-list shape — 49 iterator entries × this
		// shape per dispatch).
		{
			name:    "apis_namespaced_list_compositions",
			path:    "/apis/composition.krateo.io/v1/namespaces/bench-ns-02/githubscaffoldingwithcompositionpages",
			wantGVR: schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: "githubscaffoldingwithcompositionpages"},
			wantNs:  "bench-ns-02",
			wantNm:  "",
			ok:      true,
		},
		// /apis/<group>/<version> namespaced NAMED.
		{
			name:    "apis_namespaced_named_composition",
			path:    "/apis/composition.krateo.io/v1/namespaces/bench-ns-02/githubscaffoldingwithcompositionpages/bench-app-02-06",
			wantGVR: schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: "githubscaffoldingwithcompositionpages"},
			wantNs:  "bench-ns-02",
			wantNm:  "bench-app-02-06",
			ok:      true,
		},
		// Subresource: recorded against the parent resource + parent
		// name. Per plan: subresource changes count as parent UPDATE.
		{
			name:    "apis_namespaced_subresource_status",
			path:    "/apis/apps/v1/namespaces/demo/deployments/d1/status",
			wantGVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			wantNs:  "demo",
			wantNm:  "d1",
			ok:      true,
		},
		{
			name:    "apis_namespaced_subresource_scale",
			path:    "/apis/apps/v1/namespaces/demo/deployments/d1/scale",
			wantGVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			wantNs:  "demo",
			wantNm:  "d1",
			ok:      true,
		},
		{
			name:    "core_subresource_pod_exec",
			path:    "/api/v1/namespaces/demo/pods/foo/exec",
			wantGVR: schema.GroupVersionResource{Version: "v1", Resource: "pods"},
			wantNs:  "demo",
			wantNm:  "foo",
			ok:      true,
		},
		// Query string + trailing slash stripped before parse.
		{
			name:    "query_string_stripped",
			path:    "/apis/apps/v1/deployments?labelSelector=foo%3Dbar",
			wantGVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			wantNs:  "",
			wantNm:  "",
			ok:      true,
		},
		{
			name:    "trailing_slash_stripped",
			path:    "/api/v1/namespaces/demo/pods/",
			wantGVR: schema.GroupVersionResource{Version: "v1", Resource: "pods"},
			wantNs:  "demo",
			wantNm:  "",
			ok:      true,
		},
		// Rejection cases — non-apiserver / malformed / JQ leakage.
		{name: "external_https", path: "https://api.github.com/repos/foo/bar", ok: false},
		{name: "jq_template_leak", path: `${ "/api/v1/namespaces/" + (.) + "/pods" }`, ok: false},
		{name: "empty", path: "", ok: false},
		{name: "root_slash", path: "/", ok: false},
		{name: "apis_no_resource", path: "/apis/apps/v1", ok: false},
		{name: "api_no_resource", path: "/api/v1", ok: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gvr, ns, name, ok := ParseAPIServerPathToDep(c.path)
			if ok != c.ok {
				t.Fatalf("ok=%v want %v (path=%q got gvr=%v ns=%q name=%q)",
					ok, c.ok, c.path, gvr, ns, name)
			}
			if !ok {
				return
			}
			if gvr != c.wantGVR {
				t.Errorf("gvr mismatch: got %v want %v (path=%q)", gvr, c.wantGVR, c.path)
			}
			if ns != c.wantNs {
				t.Errorf("ns mismatch: got %q want %q (path=%q)", ns, c.wantNs, c.path)
			}
			if name != c.wantNm {
				t.Errorf("name mismatch: got %q want %q (path=%q)", name, c.wantNm, c.path)
			}
		})
	}
}
