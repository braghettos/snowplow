// a3_identity_extras_quarantine_test.go — A-3 (1.12.3) END-TO-END arms for
// shared-cell body poisoning via UNDECLARED client-supplied identity extras.
//
// THE DEFECT. Identity keys are dropped from the widget CACHE KEY unless
// declared, which is correct — but nothing stopped them reaching the RESOLVE
// INPUT. For a widget declaring neither spec.identityContext nor
// spec.keyExtras, server-side injection never fires (DeclaredIdentity returns
// nil), so a request sending ?extras={"username":"evil"} had its own value
// merged into the jq dict by mergeExtras and rendered into the body. Because the
// key ignores identity, that body was then written into the per-BindingUID cell
// EVERY co-cohort member reads.
//
// THE FIX lives in widgets.Resolve (sanitizeUndeclaredIdentityExtras,
// internal/resolvers/widgets/resolve.go), NOT at this dispatcher — so the
// /call path, the refresher's re-resolve, the boot seed and nested resolves all
// get the same contract. The unit-level strip/keep matrix is in that package;
// THIS file drives the whole HTTP path and asserts on the two surfaces that
// actually matter to a user.
//
// WHY THESE ARMS DISCRIMINATE. A key-side assertion is blind here — the key is
// IDENTICAL before and after the fix, which is precisely what makes the leak
// possible (feedback_spoof_quarantine_needs_both_key_and_resolved_output_arms).
// Every arm therefore drives the REAL Widgets().ServeHTTP over a widget whose
// widgetDataTemplate ECHOES .username, and asserts on the RESOLVE OUTPUT on both
// the body served to the requester and the bytes PERSISTED in the shared cell.

package dispatchers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// a3EchoSpec is a widget spec whose widgetDataTemplate echoes .username,
// .displayName and .foo into the rendered body. No apiRef, so widgets.Resolve is
// hermetic (no apiserver).
func a3EchoSpec(identityContext []string, keyExtras []string) map[string]any {
	spec := map[string]any{
		"widgetData": map[string]any{},
		"widgetDataTemplate": []any{
			map[string]any{"forPath": ".echoedUser", "expression": "${ .username }"},
			map[string]any{"forPath": ".echoedDisplay", "expression": "${ .displayName }"},
			map[string]any{"forPath": ".echoedFoo", "expression": "${ .foo }"},
		},
	}
	toAny := func(ss []string) []any {
		out := make([]any, len(ss))
		for i, s := range ss {
			out[i] = s
		}
		return out
	}
	if len(identityContext) > 0 {
		spec["identityContext"] = toAny(identityContext)
	}
	if len(keyExtras) > 0 {
		spec["keyExtras"] = toAny(keyExtras)
	}
	return spec
}

// a3Serve drives the REAL Widgets().ServeHTTP over the given widget CR with the
// given ?extras=, and returns the served body plus the bytes PERSISTED under the
// request's own derived key ("" when the dispatcher declined the Put).
//
// The resolve seam WRAPS — it does not replace — the real widgets.Resolve, so
// the whole A-3 chain under test runs for real: the dispatcher's raw extras →
// Resolve's sanitiser → the DeclaredIdentity injection merge → mergeExtras into
// the jq dict → the widgetDataTemplate. Only the resolver's TAIL
// crdschema.ValidateObjectStatus error is swallowed; it needs an apiserver, is
// unrelated to A-3, and status.widgetData is already rendered by then (the same
// tolerance resolveRenderedWidgetData documents). handedToResolver records what
// the DISPATCHER passed in — deliberately the RAW map, since the sanitiser now
// lives one layer down.
func a3Serve(t *testing.T, spec map[string]any, extras map[string]any) (served []byte, persisted []byte, handedToResolver map[string]any) {
	t.Helper()

	// Every arm asserts the COLD-path Put DECISION, so each drive must start from
	// an empty L1. Without this the arms alias each other: undeclared identity
	// extras never fold into the key, so several arms derive the SAME key and a
	// later one would serve an earlier one's warm cell instead of resolving.
	cache.ResetResolvedCacheForTest()

	cr := h1WidgetUnstructured(spec)
	restore := installWidgetFakes(t, cr, func() bool { return true },
		func(ctx context.Context, opts widgets.ResolveOptions) (*widgets.Widget, error) {
			handedToResolver = opts.Extras
			out, _ := widgets.Resolve(ctx, opts) // REAL resolve; tail validate error benign
			if out == nil {
				out = &unstructured.Unstructured{Object: map[string]any{}}
			}
			return out, nil
		})
	defer restore()

	reqCtx := h1ReqCtx(h1User)

	keyExtras := effectiveKeyExtras(reqCtx, cr.Object, extras)
	key, handle, _ := dispatchCacheLookupKey(reqCtx, "widgets",
		h1WidgetGVR.Group, h1WidgetGVR.Version, h1WidgetGVR.Resource, h1NS, h1WName, -1, -1, keyExtras)
	if handle == nil || key == "" {
		t.Fatalf("A-3 setup: expected a live cache handle + derived key; key=%q handle=%v", key, handle != nil)
	}

	target := "/call"
	if len(extras) > 0 {
		b, err := json.Marshal(extras)
		if err != nil {
			t.Fatalf("marshal extras: %v", err)
		}
		target += "?extras=" + url.QueryEscape(string(b))
	}
	rec := httptest.NewRecorder()
	Widgets().ServeHTTP(rec, httptest.NewRequest("GET", target, nil).WithContext(reqCtx))

	if rec.Code != 200 {
		t.Fatalf("A-3: the request must still be SERVED 200 (the fix sanitises the input, it does not reject the request); got %d body=%s", rec.Code, rec.Body.String())
	}

	if entry, hit := handle.Get(key); hit {
		persisted = entry.RawJSON
	}
	return rec.Body.Bytes(), persisted, handedToResolver
}

// TestA3_UndeclaredIdentityExtras_Quarantined — the make-or-break arm.
func TestA3_UndeclaredIdentityExtras_Quarantined(t *testing.T) {
	h1BuildWatcher(t)

	// (i) A widget declaring NEITHER field. The client spoofs an identity extra.
	// Assert the observable HARM before the mechanism, so a RED run reports the
	// leak itself rather than the internal call that causes it.
	served, persisted, handed := a3Serve(t, a3EchoSpec(nil, nil), map[string]any{"username": "evil"})

	if bytes.Contains(persisted, []byte("evil")) {
		t.Fatalf("A-3 (i) SHARED-CELL POISONING: the spoofed body was PERSISTED into the per-BindingUID cell that every co-cohort member reads. The identity dimension partitions the KEY, not the BODY — every user in this binding would be served %q. persisted=%s", "evil", persisted)
	}
	if bytes.Contains(served, []byte("evil")) {
		t.Fatalf("A-3 (i) SERVED BODY: the client-supplied identity extra shaped the rendered body; the widget declares no identityContext, so no server injection overwrites it. body=%s", served)
	}
	// The dispatcher deliberately hands the RAW map down — the sanitiser lives in
	// widgets.Resolve so every caller gets it, not just this one.
	if handed["username"] != "evil" {
		t.Fatalf("A-3 (i) PLACEMENT: the dispatcher must pass the RAW extras to widgets.Resolve (the sanitiser is inside Resolve, so the refresher, seed and nested resolves are covered too); it passed %#v", handed)
	}

	// (ii) DECLARED identityContext + the SAME spoof: the Put is ALLOWED and the
	// persisted body carries the JWT username. The no-over-fix arm — the sanitiser
	// must not strip a key the server is about to overwrite anyway.
	served2, persisted2, _ := a3Serve(t, a3EchoSpec([]string{"username"}, nil), map[string]any{"username": "evil"})
	if len(persisted2) == 0 {
		t.Fatal("A-3 (ii): a widget DECLARING identityContext:[username] must still be CACHED — its identity folds into the key, so its cell is per-identity and safe; the Put was declined")
	}
	if bytes.Contains(persisted2, []byte("evil")) {
		t.Fatalf("A-3 (ii) INJECTION-WINS: the declared widget's persisted body carries the client spoof instead of the JWT username; DeclaredIdentity must overwrite it. persisted=%s", persisted2)
	}
	if !bytes.Contains(persisted2, []byte(h1User)) {
		t.Fatalf("A-3 (ii) RESOLVE OUTPUT: the declared widget's persisted body must carry the JWT username %q (the server-injected value reached the jq dict); persisted=%s", h1User, persisted2)
	}
	if bytes.Contains(served2, []byte("evil")) {
		t.Fatalf("A-3 (ii): the declared widget's SERVED body must carry the JWT username, not the spoof; body=%s", served2)
	}

	// (iii) CONTROL — a genuinely-undeclared NON-identity key is untouched by the
	// sanitiser, still reaches the resolve input, and is still quarantined at the
	// Put. This is the F6 mechanism A-3 must not weaken.
	served3, persisted3, _ := a3Serve(t, a3EchoSpec(nil, nil), map[string]any{"foo": "evil"})
	if !bytes.Contains(served3, []byte("evil")) {
		t.Fatalf("A-3 (iii) CONTROL: the requester's OWN body must still be shaped by their own non-identity extra (the deliberate KEY-ONLY split); body=%s", served3)
	}
	if len(persisted3) != 0 {
		t.Fatalf("A-3 (iii) CONTROL REGRESSION: an undeclared non-identity extra must still DECLINE the Put (F6 self-quarantine); the dispatcher persisted %s", persisted3)
	}
}

// TestA3_IdentityVocabularyIsInSync pins the THREE identity-dimension keys
// end-to-end, and with them the fact that the two vocabularies — this package's
// identityDimensionKeys (the F6 guard's exemption set) and the twin inside
// internal/resolvers/widgets (the sanitiser's strip set) — cover the same keys.
// The packages cannot import each other, so behaviour is the only place they can
// be checked against one another: if the resolver's set ever loses a key this
// package still exempts, that key reaches the jq dict unstripped AND is exempt
// from the Put quarantine — the A-3 hole, reopened silently.
func TestA3_IdentityVocabularyIsInSync(t *testing.T) {
	h1BuildWatcher(t)

	if len(identityDimensionKeys) != 3 {
		t.Fatalf("A-3: the F6 guard's identity vocabulary must be exactly {username, groups, displayName}; got %#v", identityDimensionKeys)
	}
	for _, k := range []string{"username", "groups", "displayName"} {
		if _, ok := identityDimensionKeys[k]; !ok {
			t.Fatalf("A-3: %q missing from the F6 guard's identity vocabulary", k)
		}
	}

	// Drive all three through the real path on an undeclared widget: none may
	// shape the body, and the request must still be served and cached.
	served, persisted, _ := a3Serve(t, a3EchoSpec(nil, nil), map[string]any{
		"username":    "evil-user",
		"displayName": "evil-display",
		"groups":      []any{"evil-group"},
	})
	for _, spoof := range []string{"evil-user", "evil-display", "evil-group"} {
		if bytes.Contains(served, []byte(spoof)) {
			t.Errorf("A-3 vocabulary: %q reached the SERVED body of an undeclared widget — the resolver's strip set is missing this key; body=%s", spoof, served)
		}
		if bytes.Contains(persisted, []byte(spoof)) {
			t.Errorf("A-3 vocabulary: %q reached the PERSISTED shared cell of an undeclared widget; persisted=%s", spoof, persisted)
		}
	}
}

// TestA3_LegacyIdentityWire_StillCaches — the 1.7.11 no-regression arm at the
// whole-path level. The west4 wire shape (a widget declaring
// keyExtras:[name,namespace] and NO identityContext, receiving username +
// displayName because buildExtrasParam sends them when identity injection is
// off) must STILL be cached, at the SAME extras_len=2 key.
//
// This is the arm that rules out the alternative remedy of declining the Put on
// an undeclared identity extra: that reintroduces the 0/14-hit west4
// revisit-miss, and unfixably, because GetIdentityContext is enum-filtered to
// {username, groups} so displayName can never be declared.
func TestA3_LegacyIdentityWire_StillCaches(t *testing.T) {
	h1BuildWatcher(t)

	spec := a3EchoSpec(nil, []string{"name", "namespace"})
	wire := map[string]any{
		"namespace":   "team-a",
		"name":        "demo-1",
		"username":    "cyberjoker",
		"displayName": "Cyber Joker",
	}

	served, persisted, _ := a3Serve(t, spec, wire)

	if len(persisted) == 0 {
		t.Fatal("A-3 1.7.11 NO-REGRESSION: a widget declaring keyExtras:[name,namespace] carrying the legacy identity wire must STILL be cached; the Put was declined — this is the west4 revisit-MISS regression (0/14 hit), and it is unfixable by authoring because displayName can never be declared in identityContext")
	}
	if bytes.Contains(served, []byte("cyberjoker")) || bytes.Contains(served, []byte("Cyber Joker")) {
		t.Fatalf("A-3: client-supplied identity must not reach the body of a widget that declares no identityContext; body=%s", served)
	}

	// The key is UNMOVED: extras_len=2 over the declared [name,namespace].
	keyExtras := effectiveKeyExtras(h1ReqCtx(h1User), h1WidgetUnstructured(spec).Object, wire)
	if len(keyExtras) != 2 || keyExtras["name"] != "demo-1" || keyExtras["namespace"] != "team-a" {
		t.Fatalf("A-3: the key must still fold exactly the declared [name,namespace]; got %#v", keyExtras)
	}
}

// TestA3_DisplayNameInKeyExtras_SurvivesAndPartitions — the hidden-dependency
// arm between the two declaration accessors.
//
// GetIdentityContext is ENUM-FILTERED to {username, groups}, so displayName can
// never be declared there. GetKeyExtras is NOT enum-filtered, so a widget CAN
// declare displayName as a key extra. The sanitiser must honour that: a
// keyExtras-declared identity key SURVIVES, because it PARTITIONS the key and
// therefore self-quarantines — a spoofed value lands in a cell only that same
// value can reach, so no co-cohort user is exposed to it.
//
// If someone "simplifies" the sanitiser to consult only spec.identityContext,
// this arm goes RED: the value would be stripped from the body while still
// folding into the key, giving a cell keyed on a value its own content no longer
// reflects.
func TestA3_DisplayNameInKeyExtras_SurvivesAndPartitions(t *testing.T) {
	h1BuildWatcher(t)

	spec := a3EchoSpec(nil, []string{"displayName"})
	ctx := h1ReqCtx(h1User)
	cr := h1WidgetUnstructured(spec).Object

	// KEY side: a keyExtras-declared displayName FOLDS, so two values partition.
	k1 := effectiveKeyExtras(ctx, cr, map[string]any{"displayName": "Ada"})
	k2 := effectiveKeyExtras(ctx, cr, map[string]any{"displayName": "Grace"})
	if k1["displayName"] != "Ada" || k2["displayName"] != "Grace" {
		t.Fatalf("A-3: a keyExtras-declared displayName must fold into the key material; got %#v / %#v", k1, k2)
	}
	inputs := func(e map[string]any) cache.ResolvedKeyInputs {
		return cache.ResolvedKeyInputs{CacheEntryClass: "widgets", Extras: e}
	}
	if cache.ComputeKey(inputs(k1)) == cache.ComputeKey(inputs(k2)) {
		t.Fatal("A-3: two displayName values must derive DIFFERENT keys — that partition is what makes keeping the key self-quarantining")
	}

	// BODY side: it SURVIVES the sanitiser and reaches the rendered body.
	served, persisted, _ := a3Serve(t, spec, map[string]any{"displayName": "Ada"})
	if !bytes.Contains(served, []byte("Ada")) {
		t.Fatalf("A-3 HIDDEN DEPENDENCY: a keyExtras-declared displayName must SURVIVE sanitisation and reach the resolve input — GetKeyExtras is NOT enum-filtered, unlike GetIdentityContext. A sanitiser consulting only spec.identityContext strips it here while it still folds into the key. body=%s", served)
	}
	if !bytes.Contains(persisted, []byte("Ada")) {
		t.Fatalf("A-3: the declared-partitioned body must be cached under its own key; persisted=%s", persisted)
	}
}
