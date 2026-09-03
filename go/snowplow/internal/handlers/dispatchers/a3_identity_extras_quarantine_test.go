// a3_identity_extras_quarantine_test.go — A-3 (1.12.3) falsifier for
// shared-cell body poisoning via UNDECLARED client-supplied identity extras.
//
// THE DEFECT. requestExtrasFullyDeclared exempted username/groups/displayName
// from the F6 Put-quarantine UNCONDITIONALLY, reasoning that the identity
// dimension already partitions the key. It does — but the KEY is not the only
// dimension. For a widget declaring NEITHER spec.keyExtras NOR
// spec.identityContext, server-side identity injection never fires
// (widgets.Resolve injects only for a DECLARING widget), so the RAW extras
// reach the resolver and mergeExtras places them in the jq dict. A request
// sending ?extras={"username":"evil"} therefore SHAPES the rendered body; the
// identity keys are dropped from the key, so that body is written into the
// per-BindingUID cell EVERY co-cohort member reads.
//
// THE FIX. sanitizeUndeclaredIdentityExtras strips undeclared identity keys at
// the dispatcher entry, before the key fold, the resolver and the Put-guard.
// requestExtrasFullyDeclared additionally refuses to exempt an undeclared
// identity key, so an unsanitised caller falls closed. See helpers.go for why
// stripping the INPUT is the right remedy rather than declining the Put: a
// decline still serves the poisoned body, and it would permanently uncache the
// legacy-wire corpus, since displayName can never be declared
// (GetIdentityContext enum-filters to {username, groups}).
//
// WHY THESE ARMS DISCRIMINATE. A key-side assertion is blind here — the key is
// IDENTICAL before and after the fix, which is precisely what makes the leak
// possible (feedback_spoof_quarantine_needs_both_key_and_resolved_output_arms).
// Every arm therefore drives the REAL Widgets().ServeHTTP over a widget whose
// widgetDataTemplate ECHOES .username, and asserts on the RESOLVE OUTPUT on
// BOTH surfaces that matter: the body served to the requester and the bytes
// PERSISTED in the shared cell. Arm (i) fails on origin/main because "evil"
// appears in both.
//
// The arms:
//
//	(i)   undeclared widget + {"username":"evil"} → 200, and neither the served
//	      body nor the persisted cell carries "evil".
//	(ii)  widget declaring identityContext:[username] + the SAME spoof → the Put
//	      is ALLOWED and the persisted body carries the JWT username, not "evil"
//	      (injection-wins survives; the fix did not over-strip).
//	(iii) control: a non-identity {"foo":"evil"} is STILL quarantined — no Put —
//	      proving the fix did not weaken the F6 self-quarantine.
//	(iv)  1.7.11 NO-REGRESSION: the west4 wire shape (declared keyExtras +
//	      username + displayName) still CACHES, at the same extras_len=2 key.
//	(v)   key-inertness: sanitising cannot move any cache key.

package dispatchers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// a3EchoSpec is a widget spec whose widgetDataTemplate echoes BOTH .username
// and .foo into the rendered body. It has NO apiRef, so widgets.Resolve is
// hermetic (no apiserver). `declare` adds spec.identityContext / spec.keyExtras
// entries so one spec builder covers every arm.
func a3EchoSpec(identityContext []string, keyExtras []string) map[string]any {
	spec := map[string]any{
		"widgetData": map[string]any{},
		"widgetDataTemplate": []any{
			map[string]any{"forPath": ".echoedUser", "expression": "${ .username }"},
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
// The resolve seam wraps — it does not replace — the REAL widgets.Resolve, so
// the whole A-3 chain under test runs for real: the dispatcher's extras handling
// → resolve.go's DeclaredIdentity injection merge → mergeExtras into the jq dict
// → the widgetDataTemplate. Only the resolver's TAIL crdschema.ValidateObjectStatus
// error is swallowed; it needs an apiserver and is unrelated to A-3, and
// status.widgetData is already rendered by then (the same tolerance
// resolveRenderedWidgetData documents). The seam also records the extras the
// DISPATCHER handed the resolver, which is the sanitiser's direct observable.
func a3Serve(t *testing.T, spec map[string]any, extras map[string]any) (served []byte, persisted []byte, handedToResolver map[string]any) {
	t.Helper()

	// Every arm asserts the COLD-path Put DECISION, so each drive must start
	// from an empty L1. Without this the arms alias each other: sanitising
	// {"username":"evil"} down to {} lands arm (i) on the SAME empty-extras key
	// as the {"foo":"evil"} control, and the control would serve arm (i)'s warm
	// cell instead of resolving and reaching its own Put-gate.
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

	// The key the dispatcher will derive for THIS request, computed the same
	// production way, so we can read back whatever it persisted.
	keyExtras := effectiveKeyExtras(reqCtx, cr.Object, extras)
	key, handle, _ := dispatchCacheLookupKey(reqCtx, "widgets",
		h1WidgetGVR.Group, h1WidgetGVR.Version, h1WidgetGVR.Resource, h1NS, h1WName, -1, -1, keyExtras)
	if handle == nil || key == "" {
		t.Fatalf("A-3 setup: expected a live cache handle + derived key; key=%q handle=%v", key, handle != nil)
	}

	target := "/call"
	if len(extras) > 0 {
		target += "?extras=" + url.QueryEscape(string(mustJSON(t, extras)))
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

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal extras: %v", err)
	}
	return b
}

// TestA3_UndeclaredIdentityExtras_Quarantined — the make-or-break arm.
func TestA3_UndeclaredIdentityExtras_Quarantined(t *testing.T) {
	h1BuildWatcher(t)

	// (i) A widget declaring NEITHER field. The client spoofs an identity extra.
	served, persisted, handed := a3Serve(t, a3EchoSpec(nil, nil), map[string]any{"username": "evil"})

	// Assert the observable HARM before the mechanism, so a RED run reports the
	// leak itself rather than the internal call that causes it.
	if bytes.Contains(persisted, []byte("evil")) {
		t.Fatalf("A-3 (i) SHARED-CELL POISONING: the spoofed body was PERSISTED into the per-BindingUID cell that every co-cohort member reads. The identity dimension partitions the KEY, not the BODY — every user in this binding would be served %q. persisted=%s", "evil", persisted)
	}
	if bytes.Contains(served, []byte("evil")) {
		t.Fatalf("A-3 (i) SERVED BODY: the client-supplied identity extra shaped the rendered body; the widget declares no identityContext, so no server injection overwrites it. body=%s", served)
	}
	if _, present := handed["username"]; present {
		t.Fatalf("A-3 (i) SOURCE: the dispatcher must NOT hand an undeclared client-supplied username to widgets.Resolve; it passed %#v. That value lands in the jq dict (mergeExtras) and shapes the rendered body", handed)
	}

	// (ii) DECLARED identityContext + the SAME spoof: the Put is ALLOWED and the
	// persisted body carries the JWT username. This is the no-over-fix arm — the
	// sanitiser must not strip a key the server is about to overwrite anyway.
	served2, persisted2, handed2 := a3Serve(t, a3EchoSpec([]string{"username"}, nil), map[string]any{"username": "evil"})
	if len(persisted2) == 0 {
		t.Fatal("A-3 (ii): a widget DECLARING identityContext:[username] must still be CACHED — its identity folds into the key, so its cell is per-identity and safe; the Put was declined")
	}
	if bytes.Contains(persisted2, []byte("evil")) {
		t.Fatalf("A-3 (ii) INJECTION-WINS: the declared widget's persisted body carries the client spoof instead of the JWT username; DeclaredIdentity must overwrite it. persisted=%s", persisted2)
	}
	if !bytes.Contains(persisted2, []byte(h1User)) {
		t.Fatalf("A-3 (ii) RESOLVE OUTPUT: the declared widget's persisted body must carry the JWT username %q (the server-injected value reached the jq dict); persisted=%s", h1User, persisted2)
	}
	if handed2["username"] != "evil" {
		t.Fatalf("A-3 (ii) SETUP: a DECLARED identity key must survive sanitisation and reach the resolver, where injection-wins overwrites it; the dispatcher handed %#v", handed2)
	}
	if bytes.Contains(served2, []byte("evil")) {
		t.Fatalf("A-3 (ii): the declared widget's SERVED body must carry the JWT username, not the spoof; body=%s", served2)
	}

	// (iii) CONTROL — a genuinely-undeclared NON-identity key is still quarantined.
	// This is the F6 mechanism the exemption was never supposed to weaken.
	served3, persisted3, handed3 := a3Serve(t, a3EchoSpec(nil, nil), map[string]any{"foo": "evil"})
	if handed3["foo"] != "evil" {
		t.Fatalf("A-3 (iii) CONTROL: a non-identity extra must STILL reach the resolve input (the deliberate KEY-ONLY split); the dispatcher handed %#v", handed3)
	}
	if !bytes.Contains(served3, []byte("evil")) {
		t.Fatalf("A-3 (iii) CONTROL: the requester's OWN body must still be shaped by their own non-identity extra; body=%s", served3)
	}
	if len(persisted3) != 0 {
		t.Fatalf("A-3 (iii) CONTROL REGRESSION: an undeclared non-identity extra must still DECLINE the Put (F6 self-quarantine); the dispatcher persisted %s", persisted3)
	}
}

// TestA3_LegacyIdentityWire_StillCaches — the 1.7.11 no-regression arm. The
// west4 wire shape (a widget declaring keyExtras:[name,namespace] and NO
// identityContext, receiving username + displayName because the frontend's
// buildExtrasParam sends them when identity injection is off) must STILL be
// cached, at the SAME extras_len=2 key.
//
// This is the arm that rules out the alternative remedy of declining the Put on
// an undeclared identity extra: that remedy reintroduces the 0/14-hit west4
// revisit-miss, and unfixably, because displayName can never be declared.
func TestA3_LegacyIdentityWire_StillCaches(t *testing.T) {
	h1BuildWatcher(t)

	spec := a3EchoSpec(nil, []string{"name", "namespace"})
	wire := map[string]any{
		"namespace":   "team-a",
		"name":        "demo-1",
		"username":    "cyberjoker",
		"displayName": "Cyber Joker",
	}

	served, persisted, handed := a3Serve(t, spec, wire)

	if len(persisted) == 0 {
		t.Fatal("A-3 1.7.11 NO-REGRESSION: a widget declaring keyExtras:[name,namespace] carrying the legacy identity wire must STILL be cached; the Put was declined — this is the west4 revisit-MISS regression (0/14 hit), and it is unfixable by authoring because displayName can never be declared (GetIdentityContext enum-filters to {username, groups})")
	}
	// The declared route params must survive; only the identity keys are stripped.
	if handed["name"] != "demo-1" || handed["namespace"] != "team-a" {
		t.Fatalf("A-3: the DECLARED keyExtras must reach the resolver untouched; handed %#v", handed)
	}
	if _, present := handed["username"]; present {
		t.Fatalf("A-3: the undeclared identity key must be stripped even alongside declared keys; handed %#v", handed)
	}
	if _, present := handed["displayName"]; present {
		t.Fatalf("A-3: displayName is never declarable and must always be stripped; handed %#v", handed)
	}
	if bytes.Contains(served, []byte("cyberjoker")) {
		t.Fatalf("A-3: the client-supplied username must not reach the body of a widget that declares no identityContext; body=%s", served)
	}

	// The key is UNMOVED: extras_len=2 over the declared [name,namespace].
	keyExtras := effectiveKeyExtras(h1ReqCtx(h1User), h1WidgetUnstructured(spec).Object, wire)
	if len(keyExtras) != 2 || keyExtras["name"] != "demo-1" || keyExtras["namespace"] != "team-a" {
		t.Fatalf("A-3: the key must still fold exactly the declared [name,namespace]; got %#v", keyExtras)
	}
}

// TestA3_SanitizerIsKeyInert — the key-inertness arm. Sanitising the extras
// cannot move ANY cache key, because effectiveKeyExtras folds request extras
// through filterDeclaredKeyExtras (spec.keyExtras-declared names only) and every
// key the sanitiser removes is by construction outside that set. If this ever
// fails, the fix has silently re-keyed the corpus and every warm cell is orphaned.
func TestA3_SanitizerIsKeyInert(t *testing.T) {
	ctx := ctxAsIdentity(h1User, "devs")

	cases := []struct {
		name string
		spec map[string]any
		wire map[string]any
	}{
		{"undeclared widget, identity wire", a3EchoSpec(nil, nil),
			map[string]any{"username": "evil", "displayName": "E V"}},
		{"declared keyExtras, identity wire", a3EchoSpec(nil, []string{"name", "namespace"}),
			map[string]any{"name": "demo-1", "namespace": "team-a", "username": "cyberjoker", "displayName": "C J"}},
		{"declared identityContext", a3EchoSpec([]string{"username"}, nil),
			map[string]any{"username": "evil"}},
		{"username declared as a keyExtra", a3EchoSpec(nil, []string{"username"}),
			map[string]any{"username": "route-value"}},
		{"non-identity undeclared", a3EchoSpec(nil, nil),
			map[string]any{"foo": "evil"}},
	}

	for _, tc := range cases {
		cr := h1WidgetUnstructured(tc.spec).Object
		raw := effectiveKeyExtras(ctx, cr, tc.wire)
		sanitised := effectiveKeyExtras(ctx, cr, sanitizeUndeclaredIdentityExtras(cr, tc.wire))
		if !reflect.DeepEqual(raw, sanitised) {
			t.Errorf("A-3 KEY-INERTNESS (%s): sanitising moved the key material; raw=%#v sanitised=%#v — every warm cell for this corpus would be orphaned", tc.name, raw, sanitised)
		}
		if cache.ComputeKey(cache.ResolvedKeyInputs{CacheEntryClass: "widgets", Extras: raw}) !=
			cache.ComputeKey(cache.ResolvedKeyInputs{CacheEntryClass: "widgets", Extras: sanitised}) {
			t.Errorf("A-3 KEY-INERTNESS (%s): the computed key digest differs", tc.name)
		}
	}
}
