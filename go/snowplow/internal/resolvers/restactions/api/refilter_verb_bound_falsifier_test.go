// refilter_verb_bound_falsifier_test.go — A-4 (1.12.3) RUNTIME arms for the
// userAccessFilter non-read-verb signal.
//
// WHAT A-4 SET OUT TO STOP. uaf.Verb is threaded verbatim into
// rbac.EvaluateRBAC (refilter.go evalSingle). A stage that returns objects of
// resource R filtered by a WRITE verb on R keeps the objects the caller may
// MUTATE rather than the ones they may read, so a user with a broad create
// grant and a narrow get grant is shown objects they cannot read.
//
// WHY 1.12.3 ONLY WARNS. "Non-read verb" does not identify that shape. Three
// LIVE portal RESTActions use `verb: create` legitimately, with the filter
// checking a DIFFERENT resource than the stage returns
// (testdata/portal_uaf_corpus.yaml, portal @ b2a558d). The first cut of A-4
// dropped their items and rejected them at admission, which would have failed
// the portal upgrade and silently emptied three namespace pickers. So the
// runtime check is observation-only here, and 1.13.0 enforces the narrower
// SAME-RESOURCE rule (spelled out at uafVerbIsRead).
//
// The arms, all driving the LIVE path jsonHandlerBytes → jsonHandlerCore →
// applyUserAccessFilterOnPig:
//
//	(1) SERVED, NOT DROPPED. A `create` UAF still returns the items ordinary
//	    RBAC permits, and emits exactly ONE warn naming the verb.
//	(2) ONE WARN PER STAGE, not per item. The envelope carries four items on
//	    purpose, so a per-item guard would emit four.
//	(3) READ VERBS SILENT. get/list/watch emit no warn and behave exactly as
//	    before A-4.
//	(4) PORTAL CORPUS SERVED. The three vendored live stanzas each keep their
//	    permitted items — the regression gate on the outage the first cut
//	    would have caused.
//
// Arms (1) and (4) are RED on the FIRST A-4 cut (the fail-closed guard), which
// is the version they exist to prevent coming back; they pass on origin/main
// (no guard at all) because origin/main also serves the items. The behaviour
// they pin is therefore "1.12.3 == origin/main for the corpus, plus a signal" —
// which is exactly the intent of a warn-only stage.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/jwtutil"
	templates "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/rbac"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// a4WarnMsg is the exact Warn message warnUAFVerbNonRead emits.
const a4WarnMsg = "verb is not a read verb"

// a4SplitScopeRBAC seeds the split-scope fixture: user1 may READ in ns-a and
// may only CREATE in ns-b. The asymmetry is what makes "served, not dropped"
// meaningful: under a `create` filter the ns-b items are the ones RBAC permits,
// so a fail-closed guard is instantly visible as an empty result.
func a4SplitScopeRBAC(t *testing.T) {
	t.Helper()

	readRole := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: "anyplural-reader", Namespace: "ns-a"},
		Rules: []rbacv1.PolicyRule{{
			Verbs:     []string{"get", "list", "watch"},
			APIGroups: []string{"example.test"},
			Resources: []string{"anyplural"},
		}},
	}
	readBinding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "anyplural-reader-binding", Namespace: "ns-a"},
		Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "user1"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "anyplural-reader"},
	}

	// ns-b: CREATE ONLY.
	writeRole := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: "anyplural-creator", Namespace: "ns-b"},
		Rules: []rbacv1.PolicyRule{{
			Verbs:     []string{"create"},
			APIGroups: []string{"example.test"},
			Resources: []string{"anyplural"},
		}},
	}
	writeBinding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "anyplural-creator-binding", Namespace: "ns-b"},
		Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "user1"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "anyplural-creator"},
	}

	newRefilterTestWatcher(t, readRole, readBinding, writeRole, writeBinding)
}

// a4Envelope is the SA-dispatched cluster-scope LIST: two items in ns-a
// (readable) and two in ns-b (create-only). Four items so the once-per-stage
// warn assertion is meaningful.
func a4Envelope(t *testing.T) []byte {
	t.Helper()
	item := func(name, ns string) any {
		return map[string]any{
			"kind":       "AnyPlural",
			"apiVersion": "example.test/v1",
			"metadata":   map[string]any{"uid": "uid-" + name, "name": name, "namespace": ns},
		}
	}
	raw, err := json.Marshal(map[string]any{
		"kind":       "AnyPluralList",
		"apiVersion": "example.test/v1",
		"items":      []any{item("a1", "ns-a"), item("a2", "ns-a"), item("b1", "ns-b"), item("b2", "ns-b")},
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

// a4RunStage drives the LIVE refilter path for one UAF spec and returns the
// namespaces of the kept items plus everything the stage logged.
func a4RunStage(t *testing.T, uaf *templates.UserAccessFilterSpec) (keptNamespaces []string, logs string) {
	t.Helper()

	apiCall := &templates.API{
		Name:             "anyplurals",
		Filter:           ptrToString("[.anyplurals.items[]? | {name: .metadata.name, ns: .metadata.namespace}]"),
		UserAccessFilter: uaf,
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "user1"}),
		xcontext.WithLogger(logger),
	)

	dict := make(map[string]any)
	handler := jsonHandlerBytes(ctx, jsonHandlerOptions{
		key:         apiCall.Name,
		out:         dict,
		filter:      apiCall.Filter,
		uaf:         apiCall.UserAccessFilter,
		apiCallName: apiCall.Name,
		dict:        dict,
	})
	if err := handler(a4Envelope(t)); err != nil {
		t.Fatalf("jsonHandlerBytes(verb=%q): %v", uaf.Verb, err)
	}

	out := []string{}
	switch v := dict[apiCall.Name].(type) {
	case []any:
		for _, it := range v {
			if m, ok := it.(map[string]any); ok {
				if ns, ok := m["ns"].(string); ok {
					out = append(out, ns)
				}
			}
		}
	case map[string]any:
		if items, ok := v["items"].([]any); ok && len(items) > 0 {
			t.Fatalf("verb=%q: unexpected wrapped items shape: %v", uaf.Verb, items)
		}
	}
	return out, logBuf.String()
}

func a4UAF(verb string) *templates.UserAccessFilterSpec {
	return &templates.UserAccessFilterSpec{
		Verb:          verb,
		Group:         "example.test",
		Resource:      "anyplural",
		NamespaceFrom: ".metadata.namespace",
	}
}

// TestA4_UAFVerbNonRead_WarnsButServes — the make-or-break runtime arm.
func TestA4_UAFVerbNonRead_WarnsButServes(t *testing.T) {
	a4SplitScopeRBAC(t)

	// (1) A write verb must STILL SERVE the items ordinary RBAC permits. Under
	// `create`, user1's grant is in ns-b, so the ns-b items are kept and the
	// ns-a items are dropped by RBAC — not by A-4.
	kept, logs := a4RunStage(t, a4UAF("create"))
	if len(kept) != 2 || kept[0] != "ns-b" || kept[1] != "ns-b" {
		t.Fatalf("A-4 (1) SERVED-NOT-DROPPED: verb=create must serve the items RBAC permits (the two ns-b items); got %v. A fail-closed guard here is the regression that would have emptied the three live portal namespace pickers (testdata/portal_uaf_corpus.yaml)", kept)
	}

	// (2) Exactly ONE warn for the stage, naming the verb — not one per item.
	if n := strings.Count(logs, a4WarnMsg); n != 1 {
		t.Fatalf("A-4 (2) WARN: expected exactly ONE %q warning per stage (4 items in the envelope, so a per-item signal would emit 4); got %d. logs:\n%s", a4WarnMsg, n, logs)
	}
	if !strings.Contains(logs, `"verb":"create"`) {
		t.Fatalf("A-4 (2) WARN: the warning must name the offending verb so the operator can find the CR; logs:\n%s", logs)
	}
	if !strings.Contains(logs, `"uaf_resource":"anyplural"`) {
		t.Fatalf("A-4 (2) WARN: the warning must name the UAF resource — it is what 1.13.0 compares against the returned resource; logs:\n%s", logs)
	}
	// The warn must not overstate what 1.12.3 does.
	if strings.Contains(logs, "dropping all items") {
		t.Fatalf("A-4 (2) WARN: the 1.12.3 signal must NOT claim it dropped anything — it serves the items; logs:\n%s", logs)
	}

	// (3) Read verbs are silent and behave exactly as before A-4: under `list`
	// user1's grant is in ns-a, so the ns-a items survive.
	keptRead, readLogs := a4RunStage(t, a4UAF("list"))
	if len(keptRead) != 2 || keptRead[0] != "ns-a" || keptRead[1] != "ns-a" {
		t.Fatalf("A-4 (3) NO-REGRESSION: verb=list must keep exactly the two ns-a items user1 can read; got %v", keptRead)
	}
	if strings.Contains(readLogs, a4WarnMsg) {
		t.Fatalf("A-4 (3): the signal must be SILENT for a legitimate read verb; logs:\n%s", readLogs)
	}
	for _, verb := range []string{"get", "watch"} {
		if _, l := a4RunStage(t, a4UAF(verb)); strings.Contains(l, a4WarnMsg) {
			t.Errorf("A-4 (3): verb=%q is a read verb and must not warn; logs:\n%s", verb, l)
		}
	}
}

// portalUAFCase mirrors one entry of testdata/portal_uaf_corpus.yaml. Kept in
// sync with the twin in apis/templates/v1/uaf_verb_cel_test.go, which asserts
// the admission half over the same file.
type portalUAFCase struct {
	Name             string `json:"name"`
	Source           string `json:"source"`
	APIStepName      string `json:"apiStepName"`
	APIStepPath      string `json:"apiStepPath"`
	APIStepVerb      string `json:"apiStepVerb"`
	UserAccessFilter struct {
		Verb          string `json:"verb"`
		Group         string `json:"group"`
		Resource      string `json:"resource"`
		ResourcesFrom string `json:"resourcesFrom"`
		NamespaceFrom string `json:"namespaceFrom"`
	} `json:"userAccessFilter"`
}

// TestA4_PortalCorpus_CreateVerbUAFs_AdmittedAndServed — the served half of the
// corpus gate (the admitted half is
// TestA4_PortalCorpus_CreateVerbUAFs_Admitted, apis/templates/v1). Each live
// portal stanza's `create` filter must still return the caller's permitted
// namespaces.
//
// The fixture mirrors the portal's real shape: a NAMESPACES list filtered by a
// create grant on a DIFFERENT resource. user1 may create example.test/anyplural
// in ns-b only, so the picker must offer exactly ns-b — which is the whole
// point of these RAs, and precisely what a fail-closed guard destroyed.
func TestA4_PortalCorpus_CreateVerbUAFs_AdmittedAndServed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "testdata", "portal_uaf_corpus.yaml"))
	if err != nil {
		t.Fatalf("read portal UAF corpus: %v", err)
	}
	var doc struct {
		Cases []portalUAFCase `json:"cases"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal portal UAF corpus: %v", err)
	}
	if len(doc.Cases) != 3 {
		t.Fatalf("portal UAF corpus must carry the 3 live stanzas; got %d", len(doc.Cases))
	}

	a4SplitScopeRBAC(t)

	for _, c := range doc.Cases {
		if c.UserAccessFilter.Verb == "" {
			t.Errorf("corpus case %q has no verb", c.Name)
			continue
		}
		// Re-point the vendored stanza at the fixture's own group/resource so it
		// runs against real RBAC; the VERB and the cross-resource SHAPE — the
		// only things A-4 reasons about — are the live ones.
		uaf := &templates.UserAccessFilterSpec{
			Verb:          c.UserAccessFilter.Verb,
			Group:         "example.test",
			Resource:      "anyplural",
			NamespaceFrom: ".metadata.namespace",
		}
		kept, logs := a4RunStage(t, uaf)
		if len(kept) == 0 {
			t.Errorf("A-4 PORTAL CORPUS (served): the live portal stanza %q (%s) uses verb %q and its "+
				"items were ALL DROPPED. In production this stage feeds a form's Namespace picker; "+
				"an empty result is a silent outage — the user gets an empty enum and cannot create "+
				"anything. 1.12.3 must WARN, never drop. logs:\n%s", c.Name, c.Source, c.UserAccessFilter.Verb, logs)
			continue
		}
		if n := strings.Count(logs, a4WarnMsg); n != 1 {
			t.Errorf("A-4 PORTAL CORPUS (%s): expected exactly one warn for the stage; got %d", c.Name, n)
		}
	}
}

// TestA4_SameResourceIsTheRealInversion documents, as an executable note, the
// distinction 1.13.0 will enforce — and proves the fixture can actually tell the
// two apart, so the 1.13.0 rule is implementable against it rather than
// aspirational.
//
// Under `create`, RBAC's answer for ns-b is ALLOW and for ns-a is DENY; under
// `list` it is the exact opposite. That divergence is what makes a same-resource
// write filter a scope inversion. The portal's cross-resource pickers are not
// affected by it, because there the verb is checked against a resource the stage
// never returns.
func TestA4_SameResourceIsTheRealInversion(t *testing.T) {
	a4SplitScopeRBAC(t)
	ctx := ctxWithUser("user1")

	ask := func(verb, ns string) bool {
		allowed, _, err := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
			Username:       "user1",
			Verb:           verb,
			Group:          "example.test",
			Resource:       "anyplural",
			Namespace:      ns,
			SkipBindingUID: true,
		})
		if err != nil {
			t.Fatalf("EvaluateRBAC(%s, %s): %v", verb, ns, err)
		}
		return allowed
	}

	if ask("list", "ns-b") {
		t.Fatal("A-4 fixture: user1 must NOT be able to list in ns-b — the fixture no longer separates read from write scope, so the arms above are vacuous")
	}
	if !ask("create", "ns-b") {
		t.Fatal("A-4 fixture: user1 must be able to create in ns-b — the fixture no longer separates read from write scope")
	}
	if ask("create", "ns-b") == ask("list", "ns-b") {
		t.Fatal("A-4: the create and list decisions for ns-b must DIFFER; without that a same-resource write filter could not invert the scope and the 1.13.0 rule would be pointless")
	}
}

// ptrToString is a local helper (the package's ptr import is not in scope here).
func ptrToString(s string) *string { return &s }
