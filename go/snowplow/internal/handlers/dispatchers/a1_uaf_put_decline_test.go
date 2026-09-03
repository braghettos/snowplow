// a1_uaf_put_decline_test.go — 1.12.3 A-1 falsifiers for the three restactions
// L1 Put sites.
//
// THE DEFECT: the restactions key folds BindingUID + RBACSubGen; a
// userAccessFilter (UAF) stage narrows the body PER REQUESTER PER OBJECT, a
// dependency the key never sees. Two users sharing the first-match binding for
// `get restactions` derive the SAME key and the hit path writes entry.RawJSON
// verbatim — one user's rows are served to the other. The 1.12.3 mitigation
// declines the L1 Put for a UAF-bearing RA at all three sites; the request is
// still served its own correctly-narrowed 200.
//
// THE ARMS HERE are the per-site WIRING falsifiers — each drives the REAL
// function through the REAL shared gate (declineUAFPut) with a UAF fixture and
// a non-UAF CONTROL, so a decline that is actually an "everything is broken,
// nothing is cached" regression cannot pass. The cross-user ACCEPTANCE arm (the
// one that proves the leak itself is closed, per
// feedback_seed_group_shared_cell_needs_resolve_output_arm) is
// TestA1_UAFCrossUser_NoSharedCellServe in a1_uaf_cross_user_isolation_test.go.
//
// RED on origin/main: every arm below fails there — main Puts the UAF entry at
// each of the three sites (it only stamps a shorter TTL on it).

package dispatchers

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	templatesv1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/objects"
	"github.com/krateo-platformops/snowplow/internal/resolvers/restactions"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// a1RAUnstructured builds the RESTAction CR the dispatch path FETCHES and then
// converts with the REAL runtime.DefaultUnstructuredConverter — so the UAF
// stanza travels the production route into the typed CR the gate reads, rather
// than being hand-set on a typed struct. `uaf` selects the ONLY difference
// between the two arms: an api-step carrying userAccessFilter vs not.
func a1RAUnstructured(uaf bool) *unstructured.Unstructured {
	step := map[string]any{"name": "list-namespaces", "path": "/api/v1/namespaces"}
	if uaf {
		step["userAccessFilter"] = map[string]any{
			"verb":          "get",
			"group":         "",
			"resource":      "configmaps",
			"namespaceFrom": ".metadata.name",
		}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": h1RAGVR.Group + "/" + h1RAGVR.Version,
		"kind":       "RESTAction",
		"metadata":   map[string]any{"name": h1RAName, "namespace": h1NS},
		"spec":       map[string]any{"api": []any{step}},
	}}
}

// ---------------------------------------------------------------------------
// Arm (a) — CUSTOMER DISPATCH Put site (restactions.go)
// ---------------------------------------------------------------------------

// TestA1_Dispatch_UAFDeclinesPut_ControlPuts drives the REAL restActionHandler
// (RESTAction().ServeHTTP) over the REAL key derivation and the REAL cache
// handle, twice under the SAME coordinates — so both arms derive the IDENTICAL
// key and the ONLY variable is the userAccessFilter stanza on the fetched CR.
//
//   - UAF arm: 200 with the resolved body, and NOTHING is Put under the derived
//     key (nor a dep edge recorded); the decline counter ticks exactly once.
//   - CONTROL arm: the same request shape without the UAF stanza still Puts and
//     still records its dep — proving the decline is UAF-driven and not a
//     caching regression.
//
// RED on origin/main: the UAF arm finds the entry Put (main caches it with a
// short TTL), and RestactionsUAFPutDeclined() is 0 (the counter does not exist).
func TestA1_Dispatch_UAFDeclinesPut_ControlPuts(t *testing.T) {
	h1BuildWatcher(t)
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)

	reqCtx := h1ReqCtx(h1User)
	key, handle, inputs := dispatchCacheLookupKey(reqCtx, "restactions",
		h1RAGVR.Group, h1RAGVR.Version, h1RAGVR.Resource, h1NS, h1RAName, -1, -1, nil)
	if handle == nil || key == "" || inputs == nil || inputs.BindingUID == "" {
		t.Fatalf("PRECONDITION: expected a live, cacheable key (non-empty BindingUID) — without it the Put is declined for the #95 reason and this arm would pass vacuously; key=%q handle=%v inputs=%+v",
			key, handle != nil, inputs)
	}
	if _, ok := handle.Get(key); ok {
		t.Fatalf("PRECONDITION: the derived key must be cold before the arms run")
	}

	resolved := &templatesv1.RESTAction{}
	resolved.SetName(h1RAName)
	resolved.SetNamespace(h1NS)

	// serve runs one full dispatch against a CR with/without the UAF stanza and
	// returns the response recorder.
	serve := func(t *testing.T, uaf bool) *httptest.ResponseRecorder {
		t.Helper()
		restore := installRAFakes(t, a1RAUnstructured(uaf), func() bool { return true },
			func(ctx context.Context, opts restactions.ResolveOptions) (*templatesv1.RESTAction, error) {
				return resolved, nil
			})
		defer restore()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/call", nil).WithContext(reqCtx)
		RESTAction().ServeHTTP(rec, req)
		return rec
	}

	// --- UAF arm: served, NOT cached. ---
	rec := serve(t, true)
	if rec.Code != 200 {
		t.Fatalf("arm (a): a UAF RESTAction must still be SERVED normally; got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(bytes.TrimSpace(rec.Body.Bytes())) == 0 {
		t.Fatalf("arm (a): the decline must skip only the cache write — the response body must still carry the resolved result")
	}
	if entry, ok := handle.Get(key); ok {
		t.Fatalf("arm (a) RED (A-1 LEAK): the dispatch Put'd a UAF-bearing RESTAction under key %q (body %q). "+
			"That cell is keyed on BindingUID+RBACSubGen only, so a co-bound user with a DIFFERENT per-object "+
			"narrowing derives the same key and is served these rows verbatim. The Put must be DECLINED.", key, entry.RawJSON)
	}
	if depRecordedFor(key, h1RAGVR, h1NS, h1RAName) {
		t.Fatalf("arm (a): a declined Put must not record a dep edge — there is no cell for a DELETE/UPDATE to invalidate")
	}
	if got := cache.RestactionsUAFPutDeclined(); got != 1 {
		t.Fatalf("arm (a): snowplow_restactions_uaf_put_declined_total must tick exactly once per declined Put; got %d", got)
	}

	// --- CONTROL arm: the same coordinates WITHOUT the UAF stanza still cache. ---
	rec = serve(t, false)
	if rec.Code != 200 {
		t.Fatalf("arm (a) control: expected 200; got %d body=%s", rec.Code, rec.Body.String())
	}
	entry, ok := handle.Get(key)
	if !ok {
		t.Fatalf("arm (a) CONTROL BROKE: a NON-UAF RESTAction was not Put under key %q — the decline is over-broad "+
			"(it would have disabled the restactions cache wholesale, and the UAF arm above would pass vacuously)", key)
	}
	if !bytes.Equal(entry.RawJSON, rec.Body.Bytes()) {
		t.Fatalf("arm (a) control: the Put'd bytes must equal the served bytes; put=%q served=%q", entry.RawJSON, rec.Body.Bytes())
	}
	if !depRecordedFor(key, h1RAGVR, h1NS, h1RAName) {
		t.Fatalf("arm (a) control: a genuine cold Put must still record the self-dep")
	}
	if got := cache.RestactionsUAFPutDeclined(); got != 1 {
		t.Fatalf("arm (a) control: a NON-UAF Put must not tick the decline counter; total went to %d", got)
	}
}

// ---------------------------------------------------------------------------
// Arm (b) — BOOT SEED Put site (phase1_pip_seed.go seedOneRestaction)
// ---------------------------------------------------------------------------

// a1SeedFetchedRestaction is the seed-side twin of a1RAUnstructured: the
// objects.Result seedObjectsGetFn hands back, so the REAL FromUnstructured
// conversion inside seedOneRestaction produces the typed CR the gate reads.
func a1SeedFetchedRestaction(uaf bool) objects.Result {
	step := map[string]any{
		"name":        "list-namespaces",
		"path":        "/api/v1/namespaces",
		"endpointRef": map[string]any{"name": "krateo-kube", "namespace": "krateo-system"},
	}
	if uaf {
		step["userAccessFilter"] = map[string]any{
			"verb":          "get",
			"group":         "",
			"resource":      "configmaps",
			"namespaceFrom": ".metadata.name",
		}
	}
	return objects.Result{
		GVR: restActionGVR,
		Unstructured: &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": restActionGVR.Group + "/" + restActionGVR.Version,
			"kind":       "RESTAction",
			"metadata":   map[string]any{"name": "uaf-seed-ra", "namespace": "krateo-system"},
			"spec":       map[string]any{"api": []any{step}},
		}},
	}
}

// TestA1_Seed_UAFSkipsResolveAndPut_ControlSeeds drives the REAL
// seedOneRestaction end-to-end (fetch → convert → gates → resolve+Put tail),
// with the fetch and the resolve+Put TAIL seamed exactly as the #113 M15 arm
// does. A UAF-bearing RA must never REACH the tail (the seed short-circuits
// before the resolve — its Put could never happen, so paying for the resolve is
// waste); a non-UAF RA must still reach it.
//
// Reaching the tail is the right observable: the tail is where the ONLY seed Put
// lives, so "tail not reached" is strictly stronger than "no Put happened".
//
// RED on origin/main: the UAF RA reaches the tail there (main seeds it and only
// stamps a short TTL) — the widest form of the leak, since the seed warms one
// cohort representative's narrowing for every co-bound member to read on their
// first /call.
func TestA1_Seed_UAFSkipsResolveAndPut_ControlSeeds(t *testing.T) {
	const user = "userGranted"
	buildGrantedRestactionWatcher(t, user)
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)

	ref := templatesv1.ObjectReference{
		Reference:  templatesv1.Reference{Name: "uaf-seed-ra", Namespace: "krateo-system"},
		APIVersion: restActionGVR.Group + "/" + restActionGVR.Version,
		Resource:   restActionGVR.Resource,
	}

	var tailReached bool
	origPut := seedRestactionResolveAndPutFn
	origGet := seedObjectsGetFn
	t.Cleanup(func() {
		seedRestactionResolveAndPutFn = origPut
		seedObjectsGetFn = origGet
	})
	seedRestactionResolveAndPutFn = func(
		_, _ context.Context, _ *templatesv1.RESTAction, _ templatesv1.ObjectReference,
		_, _ string, _ cacheHandle, _ *cache.ResolvedKeyInputs, _ objects.Result,
		_ *cache.StageErrorSink, _ *cache.ExternalTouchedSink,
	) error {
		tailReached = true
		return nil
	}

	run := func(t *testing.T, uaf bool) bool {
		t.Helper()
		tailReached = false
		seedObjectsGetFn = func(_ context.Context, _ templatesv1.ObjectReference) objects.Result {
			return a1SeedFetchedRestaction(uaf)
		}
		if err := seedOneRestaction(seedCohortCtx(user), "cohort-granted", ref, "krateo-system", seedModeBoot); err != nil {
			t.Fatalf("seedOneRestaction returned %v; want nil (both the skip and the success path return nil)", err)
		}
		return tailReached
	}

	if run(t, true) {
		t.Fatal("arm (b) RED (A-1 LEAK): seedOneRestaction REACHED the resolve+Put tail for a userAccessFilter-bearing " +
			"RESTAction. The boot seed warms a per-BINDING cell under a cohort REPRESENTATIVE identity, so every " +
			"co-bound cohort member would be served the representative's per-object narrowing on their first /call. " +
			"The seed must short-circuit before the resolve.")
	}
	if got := cache.RestactionsUAFPutDeclined(); got != 1 {
		t.Fatalf("arm (b): the seed skip must route through the SHARED declineUAFPut gate (which is what bumps the counter); got %d ticks, want 1", got)
	}

	if !run(t, false) {
		t.Fatal("arm (b) CONTROL BROKE: seedOneRestaction did NOT reach the tail for a NON-UAF RESTAction — the skip " +
			"is over-broad (it would have disabled the restactions boot seed wholesale, making the arm above vacuous).")
	}
	if got := cache.RestactionsUAFPutDeclined(); got != 1 {
		t.Fatalf("arm (b) control: a NON-UAF seed must not tick the decline counter; total went to %d", got)
	}
}

// ---------------------------------------------------------------------------
// Arm (c) — REFRESHER re-Put site (resolve_populate.go)
// ---------------------------------------------------------------------------

// TestA1_Refresher_DeclinesCarriedHasUAF_ControlRefreshes drives the REAL
// resolveAndPopulateL1 over the REAL store with the resolve seam stubbed. The
// refresher holds no RESTAction CR, so its ONLY signal is the HasUAF the
// original Put carried on Inputs — this arm proves that carried flag is
// load-bearing at the re-Put gate.
//
// This site is DEFENSIVE SYMMETRY: since 1.12.3 no UAF cell can be created, so
// the refresher should never see one. It is wired anyway because a residual cell
// (written by a pre-1.12.3 process image, or by a future path) must not be
// refreshed into a longer life — the same posture as the external-touched gate
// beside it.
//
// RED on origin/main: the entry is overwritten with the fresh bytes there.
func TestA1_Refresher_DeclinesCarriedHasUAF_ControlRefreshes(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)

	c := cache.ResolvedCache()
	restore := setResolveOnceForTest(func(context.Context, cache.ResolvedKeyInputs) ([]byte, error) {
		return []byte(`{"fresh":1}`), nil
	})
	t.Cleanup(restore)

	// refresh seeds a prior entry under inputs' key, runs one refresh cycle, and
	// returns the entry's bytes afterwards.
	refresh := func(t *testing.T, hasUAF bool, name string) string {
		t.Helper()
		inputs := cache.ResolvedKeyInputs{
			CacheEntryClass:        "restactions",
			Group:                  "templates.krateo.io",
			Version:                "v1",
			Resource:               "restactions",
			Namespace:              "krateo-system",
			Name:                   name,
			BindingUID:             "uid-shared-binding",
			RepresentativeUsername: "alice",
			HasUAF:                 hasUAF,
		}
		key := cache.ComputeKey(inputs)
		c.Put(key, &cache.ResolvedEntry{RawJSON: []byte(`{"prior":1}`), Inputs: &inputs})
		if err := resolveAndPopulateL1(context.Background(), inputs, nil, nil); err != nil {
			t.Fatalf("resolveAndPopulateL1(%s): %v", name, err)
		}
		e, ok := c.Get(key)
		if !ok {
			t.Fatalf("setup(%s): the entry vanished during the refresh", name)
		}
		return string(e.RawJSON)
	}

	if got := refresh(t, true, "uaf-ra"); got != `{"prior":1}` {
		t.Fatalf("arm (c) RED (A-1): the refresher re-Put a cell whose carried Inputs.HasUAF is TRUE — a UAF body is "+
			"per-requester-narrowed and the key does not separate co-bound users, so the refresher must DECLINE and "+
			"keep the prior entry (TTL is the outer net). Entry is now %s, want the untouched prior.", got)
	}
	if got := cache.RestactionsUAFPutDeclined(); got != 1 {
		t.Fatalf("arm (c): the refresher decline must route through the SHARED declineUAFPut gate; got %d ticks, want 1", got)
	}

	if got := refresh(t, false, "plain-ra"); got != `{"fresh":1}` {
		t.Fatalf("arm (c) CONTROL BROKE: a NON-UAF cell must still be refreshed; entry is %s, want the fresh bytes "+
			"(otherwise the decline is over-broad and has disabled the refresher wholesale)", got)
	}
	if got := cache.RestactionsUAFPutDeclined(); got != 1 {
		t.Fatalf("arm (c) control: a NON-UAF re-Put must not tick the decline counter; total went to %d", got)
	}
}

// ---------------------------------------------------------------------------
// Anti-shadow-drift: all three sites route through the ONE shared gate
// ---------------------------------------------------------------------------

// SUPERSEDED: the hand-listed three-file wiring guard that used to live here has
// been replaced by TestA1_EveryResolvedEntryPutSiteIsUAFGatedOrWaived
// (a1_uaf_site_enumeration_test.go), which enumerates ResolvedEntry Put sites
// STRUCTURALLY from the AST instead of from a list.
//
// The hand-list is worth remembering as a lesson rather than as code: it named
// restactions.go, resolve_populate.go and phase1_pip_seed.go, it passed, and it
// was guarding an open hot carrier the whole time. It could not have found the
// widgets class, because it was written from the same mistaken belief as the
// comment it was guarding. A guard built from the author's own enumeration can
// only confirm what the author already believed.

// TestA1_DeclineUAFPut_PredicateAndCounter pins the gate's own contract: it
// declines exactly when the entry is UAF-bearing, is nil-safe, and bumps the
// counter on (and only on) a decline — the property that makes
// snowplow_restactions_uaf_put_declined_total unable to drift from the gate.
func TestA1_DeclineUAFPut_PredicateAndCounter(t *testing.T) {
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)

	if declineUAFPut(nil, nil) {
		t.Fatal("declineUAFPut(nil, nil) must be false — no inputs, no sink, no decline")
	}
	if declineUAFPut(&cache.ResolvedKeyInputs{CacheEntryClass: "restactions"}, nil) {
		t.Fatal("a non-UAF entry must NOT be declined — every non-UAF cell caches byte-identically to 1.12.2")
	}
	if got := cache.RestactionsUAFPutDeclined(); got != 0 {
		t.Fatalf("no decline happened yet; counter must still be 0, got %d", got)
	}

	// LIMB 1 — the DECLARATION. Available pre-resolve; what lets the seed skip.
	if !declineUAFPut(&cache.ResolvedKeyInputs{CacheEntryClass: "restactions", HasUAF: true}, nil) {
		t.Fatal("a UAF-DECLARING entry MUST be declined even with no sink installed")
	}
	if got := cache.RestactionsUAFPutDeclined(); got != 1 {
		t.Fatalf("the counter must tick on the decline itself (one tick per skipped Put); got %d", got)
	}

	// LIMB 2 — the OBSERVED refilter (R-1), CONSUMER SIDE ONLY.
	//
	// SCOPE, stated precisely because the first version of this comment overstated
	// it (and overstated coverage is the R-1 failure class itself): the sink is
	// HAND-BUMPED here, so what this proves is exactly one implication —
	//
	//     Count() > 0  ⇒  the gate declines, even with HasUAF false
	//
	// That is the CONSUMER question. It says nothing about the PRODUCER question,
	// "does anything in production actually make Count() > 0 for a given shape?",
	// which is where the real coverage lives and where the limbs differ:
	//
	//   - WIDGETS CARRIER (R-1): producer coverage is real on this branch —
	//     apiref.Resolve's declaration bump fires because the apiRef'd RA itself
	//     declares the filter. Proven end-to-end by
	//     TestR2_UAFCrossUser_WidgetNoSharedCellServe.
	//   - NESTED-RA CHAIN (a non-UAF parent whose inner step consumes a UAF
	//     child): NOT covered on this branch. No production bump fires — the
	//     declaration bump inspects the PARENT, which declares nothing. Producer
	//     coverage arrives with the refilter bump on fix/1.12.3-authz-hardening.
	//     TestM1_NestedUAFChild_NoCellPut_RequiresRefilterBump is that arm; it is
	//     RED here BY DESIGN and green on the assembled tree.
	//
	// So: this asserts the gate consumes the sink. It does not assert the nested
	// chain is closed, and must not be cited as if it did.
	_, sink := cache.WithUAFTouchedSink(context.Background())
	sink.Bump()
	if !declineUAFPut(&cache.ResolvedKeyInputs{CacheEntryClass: "restactions" /* HasUAF false */}, sink) {
		t.Fatal("R-1 consumer side: an entry whose resolve OBSERVED a refilter MUST be declined even though its own " +
			"Inputs declare no UAF. This is the implication the widgets carrier depends on; whether a given shape " +
			"actually bumps the sink is the producer question, covered by TestR2_... (widgets, closed here) and " +
			"TestM1_... (nested chain, closed only once the authz refilter bump merges).")
	}
	if got := cache.RestactionsUAFPutDeclined(); got != 2 {
		t.Fatalf("the observed-refilter decline must tick the counter too; got %d", got)
	}

	// The two limbs are reported distinctly, and neither firing means proceed.
	if r := uafDeclineReason(&cache.ResolvedKeyInputs{HasUAF: true}, nil); r != uafDeclineDeclared {
		t.Fatalf("declaration limb must report %q; got %q", uafDeclineDeclared, r)
	}
	if r := uafDeclineReason(nil, sink); r != uafDeclineObserved {
		t.Fatalf("sink limb must report %q; got %q", uafDeclineObserved, r)
	}
	if r := uafDeclineReason(nil, nil); r != "" {
		t.Fatalf("neither limb firing must report \"\" (proceed); got %q", r)
	}

	// The widgets class routes through the SAME rule but its OWN counter — the
	// R-1 carrier must be countable separately from the restactions cold path.
	before := cache.WidgetsUAFPutDeclined()
	if !declineWidgetUAFPut(nil, sink) {
		t.Fatal("declineWidgetUAFPut must apply the same rule as declineUAFPut")
	}
	if got := cache.WidgetsUAFPutDeclined(); got != before+1 {
		t.Fatalf("the widgets decline must tick the WIDGETS counter; got %d want %d", got, before+1)
	}
	if got := cache.RestactionsUAFPutDeclined(); got != 2 {
		t.Fatalf("a widgets decline must NOT tick the restactions counter (they are separate classes); got %d", got)
	}

	// The gate reads HasUAF, which the CR-bearing sites stamp from the ONE
	// predicate on the API type. Pin that delegation here so a future inline
	// re-implementation in this package is caught.
	uaf := &templatesv1.RESTAction{Spec: templatesv1.RESTActionSpec{API: []*templatesv1.API{
		nil,
		{Name: "plain"},
		{Name: "refilter", UserAccessFilter: &templatesv1.UserAccessFilterSpec{Verb: "get", Resource: "configmaps"}},
	}}}
	if !restactionHasUAFStage(uaf) || !uaf.HasUserAccessFilterStage() {
		t.Fatal("restactionHasUAFStage must agree with RESTAction.HasUserAccessFilterStage — they are the same predicate")
	}
	plain := &templatesv1.RESTAction{Spec: templatesv1.RESTActionSpec{API: []*templatesv1.API{{Name: "plain"}}}}
	if restactionHasUAFStage(plain) || plain.HasUserAccessFilterStage() {
		t.Fatal("an RA with no userAccessFilter stage must not be reported UAF-bearing")
	}
	var nilRA *templatesv1.RESTAction
	if nilRA.HasUserAccessFilterStage() {
		t.Fatal("the predicate must be nil-receiver-safe (the apiref bypass calls it on a possibly-nil ra)")
	}
}
