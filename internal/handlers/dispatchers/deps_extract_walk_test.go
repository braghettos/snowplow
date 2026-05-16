// deps_extract_walk_test.go — 0.30.105 unit coverage for the Phase 1
// recursive widget-tree walker's pure pieces: the /call?... endpoint
// parser, the resolved-status items extractor, and the load-bearing
// verb=="GET" navigation/safety filter.
//
// The recursion itself (phase1Walker.walk) is structurally cluster-
// coupled — it drives widgets.Resolve, which fetches CRs from an
// apiserver — so its end-to-end behavior is covered by the MANDATORY
// on-cluster Phase-1 boot-log validation, not a unit test. These tests
// pin the decision logic the walk relies on.

package dispatchers

import (
	"testing"
)

// TestParseCallPathToObjectRef covers the /call widget-endpoint decoder.
// A navigation child's `path` is itself a /call?... widget endpoint; the
// walker decodes it back into the ObjectReference objects.Get fetches.
func TestParseCallPathToObjectRef(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		wantOK   bool
		wantRes  string
		wantAPI  string
		wantName string
		wantNS   string
	}{
		{
			name:     "root-relative call endpoint",
			path:     "/call?resource=navmenus&apiVersion=widgets.templates.krateo.io/v1beta1&name=sidebar-nav-menu&namespace=krateo-system",
			wantOK:   true,
			wantRes:  "navmenus",
			wantAPI:  "widgets.templates.krateo.io/v1beta1",
			wantName: "sidebar-nav-menu",
			wantNS:   "krateo-system",
		},
		{
			name:     "host-qualified call endpoint",
			path:     "http://snowplow:8081/call?resource=routesloaders&apiVersion=widgets.templates.krateo.io/v1beta1&name=routes-loader&namespace=krateo-system",
			wantOK:   true,
			wantRes:  "routesloaders",
			wantAPI:  "widgets.templates.krateo.io/v1beta1",
			wantName: "routes-loader",
			wantNS:   "krateo-system",
		},
		{
			name:   "external link — not a /call endpoint",
			path:   "https://github.com/krateoplatformops/snowplow",
			wantOK: false,
		},
		{
			name:   "call endpoint missing resource",
			path:   "/call?apiVersion=widgets.templates.krateo.io/v1beta1&name=x&namespace=y",
			wantOK: false,
		},
		{
			name:   "call endpoint missing apiVersion",
			path:   "/call?resource=panels&name=x&namespace=y",
			wantOK: false,
		},
		{
			name:   "empty path",
			path:   "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, ok := parseCallPathToObjectRef(tc.path)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (ref=%+v)", ok, tc.wantOK, ref)
			}
			if !ok {
				return
			}
			if ref.Resource != tc.wantRes {
				t.Errorf("resource = %q, want %q", ref.Resource, tc.wantRes)
			}
			if ref.APIVersion != tc.wantAPI {
				t.Errorf("apiVersion = %q, want %q", ref.APIVersion, tc.wantAPI)
			}
			if ref.Name != tc.wantName {
				t.Errorf("name = %q, want %q", ref.Name, tc.wantName)
			}
			if ref.Namespace != tc.wantNS {
				t.Errorf("namespace = %q, want %q", ref.Namespace, tc.wantNS)
			}
		})
	}
}

// TestExtractResourcesRefsItems reads status.resourcesRefs.items[] from a
// resolved widget object — the same shape widgets.Resolve writes.
func TestExtractResourcesRefsItems(t *testing.T) {
	obj := map[string]any{
		"status": map[string]any{
			"resourcesRefs": map[string]any{
				"items": []any{
					map[string]any{
						"id":      "child-page",
						"path":    "/call?resource=widgets&apiVersion=widgets.templates.krateo.io/v1beta1&name=p&namespace=n",
						"verb":    "GET",
						"allowed": true,
					},
					map[string]any{
						"id":      "delete-action",
						"path":    "/call?resource=restactions&apiVersion=templates.krateo.io/v1&name=del&namespace=n",
						"verb":    "DELETE",
						"allowed": true,
					},
				},
			},
		},
	}
	got := extractResourcesRefsItems(obj)
	if len(got) != 2 {
		t.Fatalf("expected 2 items, got %d: %+v", len(got), got)
	}
	if got[0].ID != "child-page" || got[0].Verb != "GET" || !got[0].Allowed {
		t.Errorf("item 0 = %+v, want GET/allowed child-page", got[0])
	}
	if got[1].Verb != "DELETE" {
		t.Errorf("item 1 verb = %q, want DELETE", got[1].Verb)
	}

	// Absent status.resourcesRefs — must return nil, not panic.
	if got := extractResourcesRefsItems(map[string]any{}); got != nil {
		t.Errorf("absent resourcesRefs must yield nil; got %+v", got)
	}
}

// TestWalkFilter_GETOnly is the SAFETY falsifier for the recursive walk's
// child-recursion gate. The walk runs with the snowplow service account's
// privileged credentials; recursing into a non-GET resourcesRefs item
// would issue a DESTRUCTIVE apiserver mutation. This test asserts the
// exact predicate the walk applies before recursing into a child:
//
//	recurse  IFF  verb == "GET" (case-insensitive)  AND  path != ""
//
// verb == "GET" is the SOLE load-bearing read-only invariant. A
// regression that drops the verb filter — letting the SA walk follow a
// POST/PUT/PATCH/DELETE action ref — fails here.
//
// `allowed` is DELIBERATELY NOT a gate: it is snowplow's typed-RBAC
// evaluator (EvaluateRBAC) keyed on the REQUEST USER identity against the
// Krateo Role/RoleBinding CRs. The Phase 1 SA-walk context carries no
// Krateo RBAC CRs, so EvaluateRBAC default-denies and every child
// resolves allowed=false. Gating on it would prune the whole tree at the
// first Route and the composition informer would never register (the
// 0.30.104 failure 0.30.105 fixes). A GET ref with allowed=false MUST
// still be recursed: see walkShouldRecurse. A regression that re-adds the
// allowed gate fails the `{"GET", false, true}` case here.
func TestWalkFilter_GETOnly(t *testing.T) {
	cases := []struct {
		verb        string
		allowed     bool
		wantRecurse bool
	}{
		{"GET", true, true},
		{"get", true, true},  // case-insensitive
		{"GET", false, true}, // allowed=false GET MUST still recurse (SA walk carries no request-user identity; typed-RBAC default-denies)
		{"POST", true, false},
		{"POST", false, false},
		{"PUT", true, false},
		{"PATCH", true, false},
		{"DELETE", true, false},
		{"", true, false},
	}
	for _, tc := range cases {
		ref := navChildRef{Verb: tc.verb, Allowed: tc.allowed, Path: "/call?resource=r&apiVersion=g/v"}
		got := walkShouldRecurse(ref)
		if got != tc.wantRecurse {
			t.Errorf("verb=%q allowed=%v: shouldRecurse=%v, want %v "+
				"(verb==GET is the sole read-only invariant; allowed is NOT a gate)",
				tc.verb, tc.allowed, got, tc.wantRecurse)
		}
	}
	// An empty path is never recursable regardless of verb.
	if walkShouldRecurse(navChildRef{Verb: "GET", Path: ""}) {
		t.Error("a GET ref with empty path must NOT be recursed — nothing to fetch")
	}
}
