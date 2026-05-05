package jqlint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestLintFilter_TableDriven covers the small AST cases Option A is meant
// to catch + the defensive idioms we MUST allow without false-positives.
func TestLintFilter_TableDriven(t *testing.T) {
	type tc struct {
		name      string
		filter    string
		protected []string
		wantViol  bool
	}

	cases := []tc{
		{
			name:      "Unguarded_OuterIter_Crds",
			filter:    `[.crds[] as $crd | {plural: $crd.plural}]`,
			protected: []string{"crds"},
			wantViol:  true,
		},
		{
			name:      "Unguarded_TwoFields_BothFlagged",
			filter:    `[.crds[] as $crd | .namespaces[] as $ns | {plural: $crd.plural, namespace: $ns}]`,
			protected: []string{"crds", "namespaces"},
			wantViol:  true,
		},
		{
			name:      "Guarded_AltDefault_Crds",
			filter:    `[(.crds // [])[] as $crd | {plural: $crd.plural}]`,
			protected: []string{"crds"},
			wantViol:  false,
		},
		{
			name:      "Guarded_AltDefault_Both",
			filter:    `[(.crds // [])[] as $crd | (.namespaces // [])[] as $ns | {plural: $crd.plural, namespace: $ns}]`,
			protected: []string{"crds", "namespaces"},
			wantViol:  false,
		},
		{
			name:      "Guarded_OptionalSuffix",
			filter:    `[.crds[]?]`,
			protected: []string{"crds"},
			wantViol:  false,
		},
		{
			name: "Guarded_TypeDispatch_If",
			filter: `{r: (if (.routes | type) == "array" then [.routes[]?.items[]?] elif (.routes | type) == "object" then [.routes.items[]?] else [] end)}`,
			protected: []string{"routes"},
			wantViol:  false,
		},
		{
			name:      "NotProtected_NotFlagged",
			filter:    `[.somethingelse[]]`,
			protected: []string{"crds"},
			wantViol:  false,
		},
		{
			name:      "EmptyProtected_NotFlagged",
			filter:    `[.crds[]]`,
			protected: nil,
			wantViol:  false,
		},
		{
			name: "Guarded_AltDefault_Sort",
			// like blueprints-panels.
			filter:    `{p: ((.blueprintspanels // []) as $items | $items | length)}`,
			protected: []string{"blueprintspanels"},
			wantViol:  false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pf := make([]ProtectedField, 0, len(c.protected))
			for _, n := range c.protected {
				pf = append(pf, ProtectedField{Name: n})
			}
			viols, err := LintFilter(c.filter, pf)
			if err != nil {
				t.Fatalf("LintFilter parse error: %v", err)
			}
			got := len(viols) > 0
			if got != c.wantViol {
				t.Fatalf("violations=%d wantViol=%v\nfilter: %s\ngot: %v",
					len(viols), c.wantViol, c.filter, viols)
			}
		})
	}
}

// TestPortalRA_AllNullSafe_PostV5Fix loads every YAML under
// portal-cache-yaml-staging/ and asserts the outer filter is null-safe
// w.r.t. its api[] entries that have continueOnError: true.
//
// This is the §6.3 Option A regression test. PASSES on v5; the
// compositions-get-ns-and-crd case FAILED on v4 (asserted by the
// companion TestUnfixedYAML_Compositions_ShouldFail below using the
// embedded fixture).
func TestPortalRA_AllNullSafe_PostV5Fix(t *testing.T) {
	dir := portalYAMLDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}

	covered := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			rendered := renderHelmYAMLForLint(t, path)
			ra := parseRESTActionFilterAndAPIs(t, rendered)
			if ra.outerFilter == "" {
				t.Skip("no outer filter")
				return
			}
			pf := protectedFromAPIs(ra.apis)
			viols, err := LintFilter(ra.outerFilter, pf)
			if err != nil {
				t.Fatalf("lint parse err: %v", err)
			}
			if len(viols) > 0 {
				t.Fatalf("portal RA %s outer filter has unguarded iterations:\n  %s",
					e.Name(), strings.Join(viols, "\n  "))
			}
		})
		covered++
	}
	if covered == 0 {
		t.Fatalf("expected at least 1 portal YAML, found 0 in %s", dir)
	}
}

// TestUnfixedYAML_Compositions_ShouldFail loads the v4 (un-fixed) form
// of compositions-get-ns-and-crd.yaml from an in-test fixture and
// asserts the lint REJECTS it — proving the lint would have caught the
// D2 defect class.
//
// This is the negative-baseline that demonstrates the lint is not
// vacuous. It does NOT depend on git checkout state — the v4 filter
// body is embedded literally in this file.
func TestUnfixedYAML_Compositions_ShouldFail(t *testing.T) {
	v4Filter := `[.crds[] as $crd | .namespaces[] as $ns | {plural: $crd.plural, version: $crd.storedVersions, namespace: $ns}]`
	pf := []ProtectedField{
		{Name: "crds"},
		{Name: "namespaces"},
	}
	viols, err := LintFilter(v4Filter, pf)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(viols) == 0 {
		t.Fatalf("expected lint violations on v4 unfixed filter; got none")
	}
	// Ensure the violation actually mentions one of the offending fields.
	joined := strings.Join(viols, "\n")
	if !strings.Contains(joined, "crds") && !strings.Contains(joined, "namespaces") {
		t.Fatalf("violation messages missing field names: %s", joined)
	}
	t.Logf("v4 baseline lint violations (proves the catcher works):\n  %s",
		strings.Join(viols, "\n  "))
}

// ── helpers ────────────────────────────────────────────────────────────────

func portalYAMLDir(t *testing.T) string {
	t.Helper()
	// Walk up from the test file dir to the repo root.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	cur := wd
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(cur, "portal-cache-yaml-staging")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	t.Fatalf("portal-cache-yaml-staging not found upwards from %s", wd)
	return ""
}

// renderHelmYAMLForLint substitutes the small set of helm template
// placeholders used by the portal-cache YAMLs so they parse as valid
// YAML for sigs.k8s.io/yaml. We do NOT run helm; only the literal
// placeholders appearing in the chart are replaced.
func renderHelmYAMLForLint(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := string(b)
	repl := []struct{ k, v string }{
		{"{{ .Release.Namespace }}", "krateo-system"},
	}
	for _, r := range repl {
		s = strings.ReplaceAll(s, r.k, r.v)
	}
	return s
}

type extractedRA struct {
	outerFilter string
	apis        []apiEntry
}

type apiEntry struct {
	name             string
	continueOnError  bool
	userAccessFilter bool
}

// parseRESTActionFilterAndAPIs parses just the fields of a RESTAction
// CR that the lint cares about. We avoid pulling in the full apis/
// types to keep this lint test package decoupled from the rest of the
// snowplow codebase (the lint is a pure jq AST walk; binding it to the
// generated types would invert the dep direction).
func parseRESTActionFilterAndAPIs(t *testing.T, raw string) extractedRA {
	t.Helper()
	type apiYAML struct {
		Name             string `json:"name"`
		ContinueOnError  *bool  `json:"continueOnError,omitempty"`
		UserAccessFilter any    `json:"userAccessFilter,omitempty"`
	}
	type spec struct {
		API    []apiYAML `json:"api"`
		Filter string    `json:"filter"`
	}
	type doc struct {
		Spec spec `json:"spec"`
	}
	var d doc
	if err := yaml.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("yaml parse: %v\n--- raw ---\n%s", err, raw)
	}
	out := extractedRA{
		outerFilter: strings.TrimSpace(d.Spec.Filter),
	}
	for _, a := range d.Spec.API {
		coe := false
		if a.ContinueOnError != nil {
			coe = *a.ContinueOnError
		}
		out.apis = append(out.apis, apiEntry{
			name:             a.Name,
			continueOnError:  coe,
			userAccessFilter: a.UserAccessFilter != nil,
		})
	}
	return out
}

func protectedFromAPIs(apis []apiEntry) []ProtectedField {
	out := make([]ProtectedField, 0, len(apis))
	for _, a := range apis {
		if a.continueOnError && a.name != "" {
			out = append(out, ProtectedField{Name: a.name})
		}
	}
	return out
}
