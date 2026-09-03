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
	"reflect"
	"strings"
	"testing"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/jwtutil"
	templates "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
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
	kept, logs, _ := a4RunStageCore(t, uaf, true)
	return kept, logs
}

// a4RunStageWithSink drives the stage WITH an A-1 UAFTouchedSink installed on
// the request ctx and reports how many times the refilter bumped it. Every L1
// Put site reads Count()>0 to decline caching a UAF-narrowed body, so a missing
// bump is a cross-requester cache leak, not a metrics gap.
func a4RunStageWithSink(t *testing.T, uaf *templates.UserAccessFilterSpec) (keptNamespaces []string, logs string, uafBumps int64) {
	t.Helper()
	return a4RunStageCore(t, uaf, true)
}

// a4RunStageNoSink drives the SAME stage with NO sink on the ctx — the shape
// every caller that does not Put an L1 entry presents. cache.BumpUAFTouched is
// nil-receiver-safe, so this must neither panic nor change the result.
//
// This exists because the obvious way to write the inertness arm is wrong: if it
// goes through a helper that installs a sink, it asserts nothing about the
// no-sink path while its name promises it does. That is the same
// assert-on-your-own-simulation shape that made the M1 chain arm a dud, so the
// helper takes the flag explicitly rather than leaving it implicit.
func a4RunStageNoSink(t *testing.T, uaf *templates.UserAccessFilterSpec) (keptNamespaces []string, logs string) {
	t.Helper()
	kept, logs, bumps := a4RunStageCore(t, uaf, false)
	if bumps != -1 {
		t.Fatalf("a4RunStageNoSink must report -1 bumps (no sink to count); got %d — the helper installed a sink and this arm is not testing the no-sink path", bumps)
	}
	return kept, logs
}

// a4RunStageCore drives the LIVE refilter path. installSink chooses whether the
// request ctx carries a UAFTouchedSink; with it false the returned bump count is
// -1, which is not a possible real count, so a caller that forgets which mode it
// asked for fails loudly instead of reading 0 as "no bump observed".
func a4RunStageCore(t *testing.T, uaf *templates.UserAccessFilterSpec, installSink bool) (keptNamespaces []string, logs string, uafBumps int64) {
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
	var uafSink *cache.UAFTouchedSink
	if installSink {
		ctx, uafSink = cache.WithUAFTouchedSink(ctx)
	}

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
	if !installSink {
		return out, logBuf.String(), -1
	}
	return out, logBuf.String(), uafSink.Count()
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

// TestA4_RefilterBumpsUAFTouchedSink — the A-1 hard tag condition, asserted from
// the file that owns the bump site.
//
// The L1 Put-gate declines to cache a userAccessFilter-narrowed body by reading
// UAFTouchedSink.Count()>0. The apiref chokepoint bump
// (internal/resolvers/widgets/apiref) is DECLARATION-based and therefore blind
// to a chain — a parent RA declaring no UAF whose inner step consumes a UAF
// child. Only the bump at the refilter itself observes execution. Without it the
// chain is closed by corpus accident (0 of 49 live RAs form one), which is one
// customer CR away from being false.
//
// RED without the bump: the sink reads 0 while the refilter demonstrably ran and
// narrowed the body, so a downstream Put site would cache requester-specific
// rows under a key that does not separate requesters.
func TestA4_RefilterBumpsUAFTouchedSink(t *testing.T) {
	a4SplitScopeRBAC(t)

	// A read verb: the ordinary, overwhelmingly common shape.
	kept, _, bumps := a4RunStageWithSink(t, a4UAF("list"))
	if len(kept) == 0 {
		t.Fatal("setup: expected the list filter to keep the ns-a items")
	}
	if bumps == 0 {
		t.Fatal("A-1: the live refilter must bump the UAFTouchedSink — every L1 Put site reads Count()>0 to decline caching a UAF-narrowed body. With no bump, a resolve whose rows were narrowed for THIS requester is cached under a key that folds only BindingUID/RBACSubGen and is served to a co-binding requester with different per-object grants")
	}

	// A non-read verb still narrows the body, so it must bump too — the signal
	// must not depend on the A-4 warn path's verdict.
	if _, _, wbumps := a4RunStageWithSink(t, a4UAF("create")); wbumps == 0 {
		t.Fatal("A-1: a non-read-verb UAF still runs the refilter and still narrows the body; the sink must bump regardless of the A-4 verb signal")
	}

	// The bump must be placed BEFORE per-item evaluation, so a filter that keeps
	// NOTHING still records that narrowing happened. A drop-everything result is
	// still a per-requester result and must not be cached.
	emptyUAF := &templates.UserAccessFilterSpec{
		Verb:          "list",
		Group:         "example.test",
		Resource:      "no-such-resource",
		NamespaceFrom: ".metadata.namespace",
	}
	keptNone, _, nbumps := a4RunStageWithSink(t, emptyUAF)
	if len(keptNone) != 0 {
		t.Fatalf("setup: a grant-less resource must keep nothing; got %v", keptNone)
	}
	if nbumps == 0 {
		t.Fatal("A-1: the bump must precede per-item evaluation — a refilter that drops EVERY item still narrowed the body for this requester, and an empty result cached under a shared key is still wrong")
	}
}

// TestA4_SinkBumpIsNoOpWithoutSink — the inertness arm. Every caller that
// installs no sink (the resolve paths that never Put an L1 entry, and every
// test) must be byte-identical to pre-1.12.3: cache.BumpUAFTouched is
// nil-receiver-safe, so it must neither panic nor change what the refilter
// returns.
//
// It drives the NO-SINK helper deliberately. An earlier version of this arm went
// through a4RunStage, which installs one — so it ran the with-sink path while its
// name promised the opposite, and would have stayed green with the no-sink path
// completely broken.
func TestA4_SinkBumpIsNoOpWithoutSink(t *testing.T) {
	a4SplitScopeRBAC(t)

	// No sink on ctx: must not panic, and must keep exactly what RBAC permits.
	kept, logs := a4RunStageNoSink(t, a4UAF("list"))
	if len(kept) != 2 || kept[0] != "ns-a" || kept[1] != "ns-a" {
		t.Fatalf("A-1 inertness: with NO sink installed the refilter must behave exactly as before the bump (the two ns-a items); got %v logs=%s", kept, logs)
	}

	// And identical to the with-sink run — the bump observes, it does not filter.
	keptWith, _, bumps := a4RunStageWithSink(t, a4UAF("list"))
	if bumps == 0 {
		t.Fatal("setup: the with-sink run must actually bump, otherwise this comparison proves nothing")
	}
	if !reflect.DeepEqual(kept, keptWith) {
		t.Fatalf("A-1 inertness: installing a sink changed the refilter result; no-sink=%v with-sink=%v — the bump must be observation-only", kept, keptWith)
	}

	// A non-read verb with no sink must also be inert: the A-4 warn path and the
	// A-1 bump are independent, and neither may panic without a sink.
	if keptWrite, _ := a4RunStageNoSink(t, a4UAF("create")); len(keptWrite) != 2 {
		t.Fatalf("A-1 inertness: a non-read verb with no sink must still serve the items RBAC permits (the two ns-b items); got %v", keptWrite)
	}
}
