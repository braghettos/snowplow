// refilter_verb_bound_falsifier_test.go — A-4 (1.12.3) RUNTIME falsifier for
// the read-verb bound on userAccessFilter.verb.
//
// THE DEFECT. uaf.Verb was unbounded and threaded verbatim into
// rbac.EvaluateRBAC (refilter.go evalSingle). A userAccessFilter narrows a READ
// path — it decides which items of a ServiceAccount-dispatched list the caller
// may SEE — so checking a WRITE verb inverts its scope: the filter starts
// keeping the items the caller may MUTATE. Those two sets are unrelated, so a
// user with a broad create grant and a narrow get grant is SHOWN objects they
// cannot read.
//
// WHY THESE ARMS DISCRIMINATE. A fail-closed guard is trivially satisfiable by
// a broken filter that drops everything always, so "verb=create drops all
// items" alone proves nothing. The fixture therefore splits the RBAC grants
// across TWO namespaces:
//
//	ns-a — user1 may LIST (and get/watch) anyplural. Readable.
//	ns-b — user1 may CREATE anyplural, and NOTHING else. Not readable.
//
// so the three arms pin three DIFFERENT outcomes on the SAME envelope:
//
//	(i)   verb "create" post-fix → 0 items + exactly one Warn (fail closed).
//	(ii)  verb "create" pre-fix  → the ns-B item, the one user1 may CREATE but
//	      MUST NOT SEE. TestA4_RED_UnboundedVerb_ServesWriteScopedItem asserts
//	      that inversion directly against rbac.EvaluateRBAC, so the leak arm (i)
//	      prevents is demonstrated, not merely asserted.
//	(iii) verb "list" → the ns-A item, unchanged. Proves the guard is scoped to
//	      non-read verbs and has not broken the production corpus.
//
// The Warn arm counts occurrences: the guard must log ONCE PER STAGE, never per
// item (the envelope carries several items on purpose), because a non-read verb
// is a single authoring mistake in one CR and per-item logging would flood the
// operator on a 500-item list.
//
// All arms drive the LIVE path — jsonHandlerBytes → jsonHandlerCore →
// applyUserAccessFilterOnPig (refilter.go), the sole production callsite — not
// the guard helper in isolation.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/jwtutil"
	templates "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/rbac"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// a4SplitScopeRBAC seeds the split-scope fixture: user1 may READ in ns-a and
// may only CREATE in ns-b. The asymmetry is the whole point — it makes
// "filtered by a read verb" and "filtered by a write verb" produce DIFFERENT,
// observable item sets.
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

	// ns-b: CREATE ONLY. user1 can write here but must never SEE these objects.
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
// (readable) and two in ns-b (create-only). Several items per namespace so the
// once-per-stage Warn assertion is meaningful — a per-item log would emit 4.
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

// a4RunStage drives the LIVE refilter path for one uaf.Verb and returns the
// namespaces of the kept items plus everything the stage logged.
func a4RunStage(t *testing.T, verb string) (keptNamespaces []string, logs string) {
	t.Helper()

	apiCall := &templates.API{
		Name:   "anyplurals",
		Filter: ptrToString("[.anyplurals.items[]? | {name: .metadata.name, ns: .metadata.namespace}]"),
		UserAccessFilter: &templates.UserAccessFilterSpec{
			Verb:          verb,
			Group:         "example.test",
			Resource:      "anyplural",
			NamespaceFrom: ".metadata.namespace",
		},
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
		t.Fatalf("jsonHandlerBytes(verb=%q): %v", verb, err)
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
		// The fail-closed shape setRefilteredEmpty writes: {"items": []}.
		if items, ok := v["items"].([]any); ok && len(items) > 0 {
			t.Fatalf("verb=%q: unexpected non-empty items in the fail-closed shape: %v", verb, items)
		}
	}
	return out, logBuf.String()
}

// TestA4_UAFVerbNonRead_FailsClosed — the make-or-break runtime arm.
func TestA4_UAFVerbNonRead_FailsClosed(t *testing.T) {
	a4SplitScopeRBAC(t)

	// (i) A WRITE verb must drop EVERY item. Pre-fix this returns the two ns-b
	// items — the ones user1 may CREATE but must never SEE.
	kept, logs := a4RunStage(t, "create")
	if len(kept) != 0 {
		t.Fatalf("A-4 (i) FAIL-CLOSED: userAccessFilter.verb=create must drop every item; the refilter KEPT %v. Those are the items user1 may CREATE, not read — the filter's scope inverted from \"what you may see\" to \"what you may write\"", kept)
	}

	// The operator must be told, exactly ONCE for the stage — not once per item.
	const warnMsg = "verb is not a read verb"
	if n := strings.Count(logs, warnMsg); n != 1 {
		t.Fatalf("A-4 (i) WARN: expected exactly ONE %q warning per stage (4 items in the envelope, so a per-item guard would emit 4); got %d. logs:\n%s", warnMsg, n, logs)
	}
	if !strings.Contains(logs, `"verb":"create"`) {
		t.Fatalf("A-4 (i) WARN: the warning must name the offending verb so the operator can find the CR; logs:\n%s", logs)
	}

	// (iii) A READ verb is UNCHANGED: the ns-a items survive, the ns-b items are
	// dropped by ordinary RBAC. This is what makes (i) a bound and not a break.
	keptRead, readLogs := a4RunStage(t, "list")
	if len(keptRead) != 2 || keptRead[0] != "ns-a" || keptRead[1] != "ns-a" {
		t.Fatalf("A-4 (iii) NO-REGRESSION: verb=list must keep exactly the two ns-a items user1 can read; got %v", keptRead)
	}
	if strings.Contains(readLogs, warnMsg) {
		t.Fatalf("A-4 (iii): the read-verb guard must be SILENT for a legitimate verb; logs:\n%s", readLogs)
	}

	// get and watch are the other two admitted verbs — neither may warn.
	for _, verb := range []string{"get", "watch"} {
		_, l := a4RunStage(t, verb)
		if strings.Contains(l, warnMsg) {
			t.Errorf("A-4 (iii): verb=%q is a read verb and must not trip the guard; logs:\n%s", verb, l)
		}
	}
}

// TestA4_RED_UnboundedVerb_ServesWriteScopedItem is the RED companion. It does
// NOT go through the refilter (the guard now stops that); it asks
// rbac.EvaluateRBAC the exact question the UNBOUNDED refilter asked, and asserts
// that the answer for the ns-b item is ALLOW under "create" and DENY under
// "list". That is the scope inversion in one assertion: with an unbounded verb
// the refilter's keep-decision for ns-b flips from deny to allow purely because
// the author typed a write verb, and user1 is served an object they cannot read.
//
// This arm is GREEN because it asserts the PROPERTY OF THE RBAC FIXTURE that
// makes the leak real. If it ever fails, the main arm's fail-closed assertion
// has become vacuous (the fixture no longer distinguishes read from write) and
// the whole file needs revisiting.
func TestA4_RED_UnboundedVerb_ServesWriteScopedItem(t *testing.T) {
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
		t.Fatal("A-4 RED setup: user1 must NOT be able to list in ns-b — the fixture no longer separates read from write scope")
	}
	if !ask("create", "ns-b") {
		t.Fatal("A-4 RED setup: user1 must be able to create in ns-b — the fixture no longer separates read from write scope")
	}
	// The inversion: the SAME item, the SAME user, opposite keep-decisions
	// depending only on the author-chosen verb. Pre-fix the refilter propagated
	// the create answer straight into the served response.
	if ask("create", "ns-b") == ask("list", "ns-b") {
		t.Fatal("A-4 RED: the create and list decisions for ns-b must DIFFER; without that difference an unbounded verb could not invert the filter's scope and the guard would be unfalsifiable")
	}
}

// ptrToString is a local helper (the package's ptr import is not in scope here).
func ptrToString(s string) *string { return &s }
