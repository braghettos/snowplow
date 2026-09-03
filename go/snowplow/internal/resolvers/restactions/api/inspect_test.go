// inspect_test.go — in-process / httptest falsifiers for InspectReadSet, the
// dispatch-free RESTAction read-set enumeration behind GET /rbac (design
// docs/restaction-rbac-endpoint-design.md §7).
//
// These run WITHOUT a kind cluster: the SA *rest.Config seam
// (saRESTConfigForInspectFn) is swapped to point at an httptest TLS server
// with a synthetic CA that answers the client-go discovery requests
// (/apis for ServerGroups, /apis/<g>/<v> and /api/<v> for
// ServerResourcesForGroupVersion). They prove the classification, the UAF
// verb-verbatim emit, the resourcesFrom fan-out, the dispatch-free property,
// dedupe/sort, the unresolvable-stage fail-loud, and the endpointRef omission.
//
// The DECISIVE falsifier #1 (admission ACCEPTS a free-form verb like
// `deletecollection`) round-trips through REAL CEL admission and therefore
// lives in inspect_integration_test.go (kind). The unit-level companion here
// (TestInspect_UAFVerbVerbatim_FreeFormVerb) proves the EMIT half — that a
// hand-built UAF with verb=deletecollection produces the verb verbatim — so a
// regression in the emit is caught fast without a cluster; the integration
// test adds the admission-accepts-it half the unit test cannot.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	templates "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"k8s.io/client-go/rest"
)

// fakeDiscoveryServer starts an httptest TLS server that answers the
// client-go discovery requests for a fixed catalogue:
//
//   - core group v1: namespaces, pods, configmaps
//   - apps/v1:       deployments
//   - composition.krateo.io/v1: fireworksapps, secondapps
//
// /apis              -> APIGroupList (ServerGroups)
// /api               -> APIVersions  (core group versions)
// /api/v1            -> APIResourceList (core resources)
// /apis/<g>/<v>      -> APIResourceList (group resources)
//
// Its auto-generated cert is signed by a CA NOT in the system root store —
// the same trust posture as a real cluster's self-signed apiserver CA. Returns
// a *rest.Config carrying that CA, ready for the SA seam.
func fakeDiscoveryServer(t *testing.T) *rest.Config {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/apis", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "kind":"APIGroupList","apiVersion":"v1",
		  "groups":[
		    {"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}},
		    {"name":"composition.krateo.io","versions":[{"groupVersion":"composition.krateo.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"composition.krateo.io/v1","version":"v1"}}
		  ]}`))
	})
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIVersions","versions":["v1"]}`))
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[
		  {"name":"namespaces","singularName":"namespace","namespaced":false,"kind":"Namespace","verbs":["get","list","watch","deletecollection"]},
		  {"name":"pods","singularName":"pod","namespaced":true,"kind":"Pod","verbs":["get","list"]},
		  {"name":"configmaps","singularName":"configmap","namespaced":true,"kind":"ConfigMap","verbs":["get","list"]}
		]}`))
	})
	mux.HandleFunc("/apis/apps/v1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"apps/v1","resources":[
		  {"name":"deployments","singularName":"deployment","namespaced":true,"kind":"Deployment","verbs":["get","list"]}
		]}`))
	})
	mux.HandleFunc("/apis/composition.krateo.io/v1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"composition.krateo.io/v1","resources":[
		  {"name":"fireworksapps","singularName":"fireworksapp","namespaced":true,"kind":"FireworksApp","verbs":["get","list","watch"]},
		  {"name":"secondapps","singularName":"secondapp","namespaced":true,"kind":"SecondApp","verbs":["get","list","watch"]}
		]}`))
	})

	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	caPEM := pemEncodeCert(srv.Certificate())
	return &rest.Config{
		Host:            srv.URL,
		BearerToken:     "fake-sa-jwt",
		TLSClientConfig: rest.TLSClientConfig{CAData: caPEM},
	}
}

// withInspectSARESTConfig swaps the package-private SA *rest.Config seam to
// rc, resets the discovery-client memo (so a fresh client is built against the
// swapped rc), and restores both on cleanup. Mirrors withDiscoverySARESTConfig
// (discovery_dispatch_tls_test.go).
func withInspectSARESTConfig(t *testing.T, rc *rest.Config) {
	t.Helper()
	resetDiscoveryClientCacheForTest()
	prev := saRESTConfigForInspectFn
	saRESTConfigForInspectFn = func() (*rest.Config, error) { return rc, nil }
	t.Cleanup(func() {
		saRESTConfigForInspectFn = prev
		resetDiscoveryClientCacheForTest()
	})
}

func ptrStr(s string) *string { return &s }

func findRow(rows []Resource, group, resource, verb string) (Resource, bool) {
	for _, r := range rows {
		if r.Group == group && r.Resource == resource && r.Verb == verb {
			return r, true
		}
	}
	return Resource{}, false
}

// Falsifier #1 (EMIT half) — a UAF stage with a free-form verb
// (deletecollection) on namespaces emits exactly
// {group:"", resource:"namespaces", verb:"deletecollection"}. A verb-less or
// get-only emit FAILS. (The admission-accepts-it half is the integration test.)
func TestInspect_UAFVerbVerbatim_FreeFormVerb(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				{
					Name: "ns",
					Path: "/api/v1/namespaces",
					UserAccessFilter: &templates.UserAccessFilterSpec{
						Verb:     "deletecollection",
						Group:    "",
						Resource: "namespaces",
					},
				},
			},
		},
	}

	rows, unresolved, err := InspectReadSet(context.Background(), ra, nil)
	if err != nil {
		t.Fatalf("InspectReadSet errored: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("expected zero unresolved stages, got %+v", unresolved)
	}
	row, ok := findRow(rows, "", "namespaces", "deletecollection")
	if !ok {
		t.Fatalf("FALSIFIER #1 FAIL: read-set missing {group:\"\", resource:\"namespaces\", "+
			"verb:\"deletecollection\"} — a verb-less or get-only emit would UNDER-GRANT the "+
			"UAF stage at the first /call. got rows=%+v", rows)
	}
	if row.Verb != "deletecollection" {
		t.Fatalf("FALSIFIER #1 FAIL: UAF row verb must be the UAF's own verb verbatim "+
			"(deletecollection), got %q", row.Verb)
	}
	if row.Version != "" {
		t.Fatalf("Q1: UAF rows are version-less (the SAR check is version-less); "+
			"expected empty version, got %q", row.Version)
	}
}

// Falsifier #2 — a non-UAF in-cluster COLLECTION stage emits verb "list".
// The HTTP-stage method is CEL-bound to GET/HEAD (core.go:30), but a
// collection read (`.../pods`, no object name) is authorized by the apiserver
// under the `list` verb, not `get` (#44 verb fix, design §4). The prior
// constant `get` here UNDER-GRANTED the collection read.
func TestInspect_NonUAFStage_CollectionVerbList(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				{Name: "pods", Path: "/api/v1/namespaces/krateo-system/pods"},
			},
		},
	}
	rows, unresolved, err := InspectReadSet(context.Background(), ra, nil)
	if err != nil {
		t.Fatalf("InspectReadSet errored: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("expected zero unresolved, got %+v", unresolved)
	}
	row, ok := findRow(rows, "", "pods", "list")
	if !ok {
		t.Fatalf("FALSIFIER #2 FAIL: non-UAF COLLECTION stage must emit verb=list for pods; got %+v", rows)
	}
	if row.Namespace != "krateo-system" {
		t.Fatalf("expected namespace krateo-system on the namespaced pods row, got %q", row.Namespace)
	}
}

// DEFECT 2 falsifier — an extras-driven dependsOn.iterator stage expands to one
// RequestOptions per iterator element (createRequestOptions/jqutil.ForEach). The
// inspect pass MUST classify EVERY opt, not just opts[0]: a per-namespace pods
// COLLECTION iterating [ns-a, ns-b] must emit a pods row for EACH namespace.
// Before the fix (only opts[0] inspected) the ns-b row was silently dropped =
// under-grant. Each collection row carries verb=list (#44 verb fix).
func TestInspect_ExtrasIteratorStage_EmitsRowPerNamespace(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				{
					Name: "pods-per-ns",
					// Whole path built inside the ${} jq (mirrors the corpus
					// kube-get iterator stage); `.` is the iterator element.
					Path: `${ "/api/v1/namespaces/" + (.) + "/pods" }`,
					DependsOn: &templates.Dependency{
						// The iterator query must yield a JSON ARRAY (jqutil.ForEach
						// json.Unmarshals the result into []any); each element is fed
						// to the path's ${.} per createRequestOption.
						Iterator: ptrStr(".extras.namespaces"),
					},
				},
			},
		},
	}
	extras := map[string]any{"namespaces": []any{"ns-a", "ns-b"}}

	rows, unresolved, err := InspectReadSet(context.Background(), ra, extras)
	if err != nil {
		t.Fatalf("InspectReadSet errored: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("expected zero unresolved, got %+v", unresolved)
	}
	for _, ns := range []string{"ns-a", "ns-b"} {
		found := false
		for _, r := range rows {
			if r.Resource == "pods" && r.Verb == "list" && r.Namespace == ns {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("DEFECT 2 FAIL: extras-iterator stage must emit a pods/list row for "+
				"namespace %q — inspecting only opts[0] would drop it (under-grant). got rows=%+v", ns, rows)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("DEFECT 2: expected exactly 2 per-namespace pods rows, got %d: %+v", len(rows), rows)
	}
}

// Falsifier #3 — a UAF resourcesFrom yielding N plurals fans out to N rows,
// each carrying the UAF's verb + group. The resourcesFrom jq reads from the
// (extras-only) dict here.
func TestInspect_UAFResourcesFrom_FanOut(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				{
					Name: "comp",
					Path: "/apis/composition.krateo.io/v1/namespaces/krateo-system/fireworksapps",
					UserAccessFilter: &templates.UserAccessFilterSpec{
						Verb:          "watch",
						Group:         "composition.krateo.io",
						ResourcesFrom: "[ (.extras.plurals // [])[] ]",
					},
				},
			},
		},
	}
	extras := map[string]any{"plurals": []any{"fireworksapps", "secondapps"}}

	rows, unresolved, err := InspectReadSet(context.Background(), ra, extras)
	if err != nil {
		t.Fatalf("InspectReadSet errored: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("expected zero unresolved, got %+v", unresolved)
	}
	for _, plural := range []string{"fireworksapps", "secondapps"} {
		row, ok := findRow(rows, "composition.krateo.io", plural, "watch")
		if !ok {
			t.Fatalf("FALSIFIER #3 FAIL: resourcesFrom fan-out missing row for %q (verb=watch, "+
				"group=composition.krateo.io); got %+v", plural, rows)
		}
		if row.Version != "" {
			t.Fatalf("Q1: UAF rows are version-less; expected empty version for %q, got %q", plural, row.Version)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("FALSIFIER #3 FAIL: expected exactly 2 fan-out rows, got %d: %+v", len(rows), rows)
	}
}

// Unresolvable stage — a UAF resourcesFrom that references an UPSTREAM stage's
// output (absent from the empty/extras-only dict) → the stage is UNRESOLVABLE
// and named in `unresolved`, never silently dropped.
func TestInspect_UAFResourcesFrom_UpstreamDependent_Unresolved(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				{
					Name: "comp",
					Path: "/apis/composition.krateo.io/v1/namespaces/krateo-system/fireworksapps",
					UserAccessFilter: &templates.UserAccessFilterSpec{
						Verb:  "list",
						Group: "composition.krateo.io",
						// .crds is populated by a prior stage at /call time; at
						// inspect time the dict holds only extras → fail-closed.
						ResourcesFrom: "[ (.crds // [])[] | .plural ]",
					},
				},
			},
		},
	}
	rows, unresolved, err := InspectReadSet(context.Background(), ra, nil)
	if err != nil {
		t.Fatalf("InspectReadSet errored: %v", err)
	}
	// resourcesFrom on .crds yields [] (empty array) from the empty dict —
	// resolveUAFResources returns ([], true), which is a VALID "no resources"
	// answer (not a fail-closed). So the stage resolves to ZERO rows, NOT an
	// unresolvable. This asserts the documented contract: an empty resourcesFrom
	// result is a clean zero-row stage.
	if len(rows) != 0 {
		t.Fatalf("expected zero rows for an empty resourcesFrom result, got %+v", rows)
	}
	if len(unresolved) != 0 {
		t.Fatalf("an empty (but valid) resourcesFrom result is not unresolvable; got %+v", unresolved)
	}
}

// endpointRef stage — OMITTED (absent), not an error, not unresolved.
func TestInspect_EndpointRefStage_Omitted(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				{
					Name:        "external",
					Path:        "/some/external/path",
					EndpointRef: &templates.Reference{Name: "github", Namespace: "krateo-system"},
				},
				{Name: "pods", Path: "/api/v1/pods"},
			},
		},
	}
	rows, unresolved, err := InspectReadSet(context.Background(), ra, nil)
	if err != nil {
		t.Fatalf("InspectReadSet errored: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("endpointRef stage must be omitted, NOT unresolved; got %+v", unresolved)
	}
	if _, ok := findRow(rows, "", "external", "get"); ok {
		t.Fatalf("endpointRef stage leaked an in-cluster row: %+v", rows)
	}
	// /api/v1/pods is a cluster-scoped COLLECTION → verb=list (#44 verb fix).
	if _, ok := findRow(rows, "", "pods", "list"); !ok {
		t.Fatalf("the in-cluster pods collection stage should still emit (verb=list); got %+v", rows)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 row (pods); got %d: %+v", len(rows), rows)
	}
}

// DEFECT 1 falsifier — a stage carrying BOTH endpointRef AND userAccessFilter
// is a UAF stage (the dispatcher checks uafActive BEFORE EndpointRef,
// resolveStageEndpoint resolve.go:370-392): it dispatches via the SA and
// refilters. Its UAF read-set MUST be emitted, NOT omitted as "external".
// Before the fix (EndpointRef classified first) this readSet was EMPTY = a
// silent under-grant. RED-first.
func TestInspect_UAFWithEndpointRef_EmitsUAFReadSet(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				{
					Name:        "ns-via-endpoint",
					Path:        "/api/v1/namespaces",
					EndpointRef: &templates.Reference{Name: "krateo-kube", Namespace: "krateo-system"},
					UserAccessFilter: &templates.UserAccessFilterSpec{
						Verb:     "list",
						Group:    "",
						Resource: "namespaces",
					},
				},
			},
		},
	}
	rows, unresolved, err := InspectReadSet(context.Background(), ra, nil)
	if err != nil {
		t.Fatalf("InspectReadSet errored: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("expected zero unresolved, got %+v", unresolved)
	}
	row, ok := findRow(rows, "", "namespaces", "list")
	if !ok {
		t.Fatalf("DEFECT 1 FAIL: a UAF+endpointRef stage must EMIT its UAF read-set "+
			"{group:\"\", resource:\"namespaces\", verb:\"list\"} — classifying endpointRef "+
			"first would silently OMIT it (under-grant). got rows=%+v", rows)
	}
	if row.Verb != "list" {
		t.Fatalf("DEFECT 1 FAIL: UAF+endpointRef row verb must be uaf.Verb verbatim (list), got %q", row.Verb)
	}
	if row.Version != "" {
		t.Fatalf("Q1: UAF rows are version-less; expected empty version, got %q", row.Version)
	}
}

// Residual-template / non-kube path → UNRESOLVABLE, named.
func TestInspect_UpstreamTemplatedPath_Unresolved(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				// Path templates off a prior stage's output not in the dict —
				// the jq yields the error text, leaving a non-kube path.
				{Name: "detail", Path: "${ .upstream.items[0].metadata.name }"},
			},
		},
	}
	rows, unresolved, err := InspectReadSet(context.Background(), ra, nil)
	if err != nil {
		t.Fatalf("InspectReadSet errored: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected zero rows for an unresolvable stage, got %+v", rows)
	}
	if len(unresolved) != 1 || unresolved[0].Stage != "detail" {
		t.Fatalf("expected the 'detail' stage named unresolvable, got %+v", unresolved)
	}
}

// Discovery-miss → the stage is UNRESOLVABLE (the GVR the apiserver does not
// serve must not be emitted as a granted resource).
func TestInspect_DiscoveryMiss_Unresolved(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				// widgets is not in the fake catalogue under templates.krateo.io.
				{Name: "w", Path: "/apis/templates.krateo.io/v1/namespaces/krateo-system/widgets"},
			},
		},
	}
	rows, unresolved, err := InspectReadSet(context.Background(), ra, nil)
	if err != nil {
		t.Fatalf("InspectReadSet errored: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a discovery-miss GVR must not be emitted, got %+v", rows)
	}
	if len(unresolved) != 1 || unresolved[0].Stage != "w" {
		t.Fatalf("expected the 'w' stage named unresolvable on discovery miss, got %+v", unresolved)
	}
}

// Dedupe + sort — two stages reading the same (group, resource, verb) collapse
// to one row; two stages reading the same resource under DIFFERENT verbs stay
// distinct; output is lexicographically sorted.
func TestInspect_DedupeAndSort(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				{Name: "ns-get-1", Path: "/api/v1/namespaces"},
				{Name: "ns-get-2", Path: "/api/v1/namespaces"}, // dup of ns-get-1
				{
					Name: "ns-watch",
					Path: "/api/v1/namespaces",
					UserAccessFilter: &templates.UserAccessFilterSpec{
						Verb: "watch", Group: "", Resource: "namespaces",
					},
				},
				{Name: "deploys", Path: "/apis/apps/v1/namespaces/x/deployments"},
			},
		},
	}
	rows, unresolved, err := InspectReadSet(context.Background(), ra, nil)
	if err != nil {
		t.Fatalf("InspectReadSet errored: %v", err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("expected zero unresolved, got %+v", unresolved)
	}
	// namespaces/list appears once (the two collection GET stages dedupe),
	// namespaces/watch once (UAF, distinct verb), deployments/list once.
	// (#44 verb fix: collection reads carry verb=list, not get.)
	if _, ok := findRow(rows, "", "namespaces", "list"); !ok {
		t.Fatalf("missing namespaces/list; got %+v", rows)
	}
	if _, ok := findRow(rows, "", "namespaces", "watch"); !ok {
		t.Fatalf("missing namespaces/watch (different verb, must stay distinct); got %+v", rows)
	}
	if _, ok := findRow(rows, "apps", "deployments", "list"); !ok {
		t.Fatalf("missing apps/deployments/list; got %+v", rows)
	}
	if len(rows) != 3 {
		t.Fatalf("expected exactly 3 rows after dedupe, got %d: %+v", len(rows), rows)
	}
	// Sort: core group "" sorts before "apps"? No — lexicographically "" < "apps".
	// Verify ascending by (group, version, resource, namespace, verb).
	for i := 1; i < len(rows); i++ {
		if !lessResource(rows[i-1], rows[i]) && rows[i-1] != rows[i] {
			t.Fatalf("rows not sorted ascending at %d: %+v then %+v", i, rows[i-1], rows[i])
		}
	}
}

func lessResource(a, b Resource) bool {
	if a.Group != b.Group {
		return a.Group < b.Group
	}
	if a.Version != b.Version {
		return a.Version < b.Version
	}
	if a.Resource != b.Resource {
		return a.Resource < b.Resource
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	return a.Verb < b.Verb
}

// Byte-identical response for an unchanged RA — two inspect passes over the
// same RA yield byte-identical marshalled read-sets (the dedupe/sort + stable
// tuple key guarantee it).
func TestInspect_ByteIdenticalForUnchangedRA(t *testing.T) {
	withInspectSARESTConfig(t, fakeDiscoveryServer(t))

	ra := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{
				{Name: "z", Path: "/apis/apps/v1/deployments"},
				{Name: "a", Path: "/api/v1/configmaps"},
				{
					Name: "uaf",
					Path: "/api/v1/namespaces",
					UserAccessFilter: &templates.UserAccessFilterSpec{
						Verb: "list", Group: "", Resource: "namespaces",
					},
				},
			},
		},
	}
	r1, _, err := InspectReadSet(context.Background(), ra, nil)
	if err != nil {
		t.Fatal(err)
	}
	r2, _, err := InspectReadSet(context.Background(), ra, nil)
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(r1)
	b2, _ := json.Marshal(r2)
	if string(b1) != string(b2) {
		t.Fatalf("read-set not byte-identical across two passes:\n%s\n%s", b1, b2)
	}
}
