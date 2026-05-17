// apistage_refresh_selfhit_falsifier_test.go — Ship 0.30.118 pre-flight
// falsifier for the api-stage refresh SELF-HIT defect.
//
// THE DEFECT (Ship E api-stage L1, found validating 0.30.117):
//
//   The refresher's apistage re-resolve routes through the WHOLE-RESTAction
//   stage loop (resolveOnceProd's CacheEntryClassApistage branch ->
//   restactions.Resolve -> api.Resolve). That loop, before each stage
//   dispatches, does a per-stage apistageStore.Get(stageKey) on the
//   resolved-output L1 — the SAME L1 the entry being refreshed lives in.
//   ResolvedCacheStore.Get honors only TTL, NOT the dirty flag, so the
//   dirty-but-resident entry the refresh is trying to refresh SELF-HITS:
//   the stage's K8s call is skipped, dict[id] is served from the stale L1
//   entry, the key-swap Put never fires, and resolveOnceProd returns
//   (nil,nil). The apistage L1 entry converges by TTL ONLY under a
//   dep-change — exactly the TTL-only behaviour Ship E was meant to end.
//
// THE GAP that hid it: Ship 0.30.117's FA2 stubbed resolveOnceFn, so it
// never ran the real stage loop and could not see the self-hit. This
// falsifier drives the REAL resolveOnceProd -> restactions.Resolve stage
// loop against a fake apiserver — no stub of the resolve seam — so the
// self-hit path is genuinely exercised.
//
// THE FIX (0.30.118): a WithRefreshBypass context marker the refresher
// sets for an apistage re-resolve; the stage loop skips the Get for
// EXACTLY the stage key being refreshed (RefreshBypassFromContext &&
// stageKey == L1KeyFromContext). The target stage recomputes from K8s and
// the key-swap Put writes the fresh value.
//
// FAIL->PASS capture: fix (A) is already in the working tree. To capture
// the FAIL run, `git stash` the 3 fix files (deps.go,
// resolve_populate.go, resolve.go) — without the bypass the seeded stale
// apistage entry stays stale (self-hit) and the assertions fail. `git
// stash pop` restores the fix and the assertions pass.
//
// Tests:
//
//   TestFalsifier118_ApistageRefreshSelfHit_SingleStage — a single-stage
//     RESTAction. Seed a stale apistage entry, mutate the backing K8s
//     object, dirty-mark via Deps().OnUpdate, run the real refresher,
//     assert the entry re-Puts fresh (apistage_store_total advances; the
//     entry's content reflects the K8s mutation) — AC-118.2.
//
//   TestFalsifier118_ApistageRefreshSelfHit_TwoStage — AC-118.6: a
//     predecessor LIST stage (stage1) feeding a dependent iterator stage
//     (stage2). Asserts BOTH stages converge their OWN entry for their
//     OWN backing object's change: stage2's backing object changes ->
//     stage2 converges; the predecessor's backing object changes ->
//     stage1 converges.
//
// Hermetic: a single httptest server stands in for the kube-apiserver —
// it answers discovery (for objects.Get's RESTAction re-fetch) plus the
// RESTAction GET and the stage LIST(s). No cluster, no CRD.

package dispatchers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/jwtutil"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// --- fake apiserver -------------------------------------------------------

// fakeAPIServer is a minimal kube-apiserver stand-in. It serves the
// discovery routes objects.Get's dynamic client needs (to re-fetch the
// RESTAction CR), the RESTAction GET itself, and one or more namespaced
// LIST endpoints whose response bodies are mutable so a test can "change
// the backing K8s object" between resolves.
type fakeAPIServer struct {
	srv *httptest.Server

	mu sync.Mutex
	// listBodies maps a namespaced LIST path
	// (/apis/<g>/<v>/namespaces/<ns>/<resource>) to its current JSON body.
	// Swapping a body simulates a backing-object change.
	listBodies map[string]string
	// restActionBody is the JSON object body for the RESTAction GET.
	restActionBody string
	// restActionPath is the GET path for the RESTAction CR.
	restActionPath string
}

func (f *fakeAPIServer) URL() string { return f.srv.URL }

// setListBody swaps the JSON body served for a namespaced LIST path.
func (f *fakeAPIServer) setListBody(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listBodies[path] = body
}

func (f *fakeAPIServer) getListBody(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.listBodies[path]
	return b, ok
}

// newFakeAPIServer starts the fake apiserver. groups maps an apiserver
// group/version path suffix (e.g. "templates.krateo.io/v1") to its
// APIResourceList JSON; discovery is assembled from it.
func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	f := &fakeAPIServer{listBodies: map[string]string{}}

	// Discovery: /apis lists the two groups; the per-group/version
	// endpoints list the resources the dynamic client's RESTMapper needs.
	const apiGroupList = `{"kind":"APIGroupList","apiVersion":"v1","groups":[` +
		`{"name":"templates.krateo.io","versions":[{"groupVersion":"templates.krateo.io/v1","version":"v1"}],` +
		`"preferredVersion":{"groupVersion":"templates.krateo.io/v1","version":"v1"}},` +
		`{"name":"composition.krateo.io","versions":[{"groupVersion":"composition.krateo.io/v1","version":"v1"}],` +
		`"preferredVersion":{"groupVersion":"composition.krateo.io/v1","version":"v1"}}]}`
	const templatesResList = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"templates.krateo.io/v1",` +
		`"resources":[{"name":"restactions","singularName":"restaction","namespaced":true,"kind":"RESTAction",` +
		`"verbs":["get","list","watch"]}]}`
	const compositionResList = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"composition.krateo.io/v1",` +
		`"resources":[` +
		`{"name":"widgets","singularName":"widget","namespaced":true,"kind":"Widget","verbs":["get","list","watch"]},` +
		`{"name":"panels","singularName":"panel","namespaced":true,"kind":"Panel","verbs":["get","list","watch"]}]}`
	const coreAPIVersions = `{"kind":"APIVersions","versions":["v1"]}`
	const coreResList = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[]}`

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case path == "/api":
			_, _ = w.Write([]byte(coreAPIVersions))
		case path == "/api/v1":
			_, _ = w.Write([]byte(coreResList))
		case path == "/apis":
			_, _ = w.Write([]byte(apiGroupList))
		case path == "/apis/templates.krateo.io/v1":
			_, _ = w.Write([]byte(templatesResList))
		case path == "/apis/composition.krateo.io/v1":
			_, _ = w.Write([]byte(compositionResList))
		case path == f.restActionPath:
			f.mu.Lock()
			body := f.restActionBody
			f.mu.Unlock()
			_, _ = w.Write([]byte(body))
		default:
			if body, ok := f.getListBody(path); ok {
				_, _ = w.Write([]byte(body))
				return
			}
			http.Error(w, fmt.Sprintf(`{"kind":"Status","apiVersion":"v1","status":"Failure",`+
				`"message":"no fake route for %s","code":404}`, path), http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// --- fixtures -------------------------------------------------------------

const selfhitNS = "bench-ns-01"

// widgetListBody builds a Widget LIST envelope with the named items —
// the mutable "backing K8s object" the falsifier changes between resolves.
func widgetListBody(rv string, names ...string) string {
	var items []string
	for _, n := range names {
		items = append(items, `{"apiVersion":"composition.krateo.io/v1","kind":"Widget",`+
			`"metadata":{"name":"`+n+`","namespace":"`+selfhitNS+`"}}`)
	}
	return `{"apiVersion":"composition.krateo.io/v1","kind":"WidgetList",` +
		`"metadata":{"resourceVersion":"` + rv + `"},"items":[` + strings.Join(items, ",") + `]}`
}

// restActionObjectBody builds the JSON object body served for the
// RESTAction GET. apiSpec is the rendered `spec.api` JSON array.
func restActionObjectBody(name, apiSpec string) string {
	return `{"apiVersion":"templates.krateo.io/v1","kind":"RESTAction",` +
		`"metadata":{"name":"` + name + `","namespace":"` + selfhitNS + `","resourceVersion":"1"},` +
		`"spec":{"api":` + apiSpec + `}}`
}

// selfhitIdentity is the fixed bind-identity the falsifier resolves under;
// per-user stage keys are stable across the cold resolve and the refresh.
var selfhitIdentity = jwtutil.UserInfo{Username: "cyberjoker", Groups: []string{"devs"}}

// resolveCtx builds the resolve context used by both the cold resolve and
// the refresher closure: the bind identity, plus the fake apiserver as the
// SA-transport endpoint (WithUserConfig for objects.Get's UserConfig
// read, WithInternalEndpoint for the stage's nil-EndpointRef dispatch,
// WithInternalRESTConfig so objects.Get + the stage dispatch route through
// a client built from a *rest.Config pointed at the fake server).
func resolveCtx(fakeURL string, rc *rest.Config) context.Context {
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(selfhitIdentity),
		xcontext.WithUserConfig(endpoints.Endpoint{ServerURL: fakeURL}),
	)
	ctx = cache.WithInternalEndpoint(ctx, &endpoints.Endpoint{ServerURL: fakeURL})
	ctx = cache.WithInternalRESTConfig(ctx, rc)
	return ctx
}

// enableApistageCache turns on CACHE_ENABLED + RESOLVED_CACHE_ENABLED +
// RESOLVED_CACHE_APISTAGE_ENABLED and resets every cache singleton so the
// test starts clean. RESOLVER_USE_INFORMER stays OFF: objects.Get then
// takes its apiserver branch (getFromAPIServer) and the stage dispatch
// routes through dispatchViaInternalRESTConfig — both against the fake
// server, no informer needed.
func enableApistageCache(t *testing.T) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_APISTAGE_ENABLED", "true")
	cache.ResetDepsForTest()
	cache.ResetResolvedCacheForTest()
	cache.ResetRefresherForTest()
	t.Cleanup(func() {
		cache.ResetRefresherForTest()
		cache.ResetResolvedCacheForTest()
		cache.ResetDepsForTest()
	})
}

// apistageResolveSeam is a FAITHFUL re-implementation of resolveOnceProd's
// apistage branch — objects.Get the owning RESTAction off the fake
// apiserver, then run the REAL restactions.Resolve -> api.Resolve stage
// loop, then return (nil,nil) on success exactly as resolveOnceProd does.
//
// It builds restactions.ResolveOptions with EXACTLY the fields
// resolveOnceProd's apistage branch (resolveRestActionForRefresh) builds —
// AuthnNS from env.String("AUTHN_NAMESPACE",""), PerPage/Page/Extras from
// the inputs — so the seam exercises the same stage-key shape (PerPage and
// Page feed stageInputHash). The SOLE divergence from resolveOnceProd is
// the threaded SArc: in production resolveRestActionForRefresh leaves SArc
// unset and api.Resolve sources its *rest.Config from
// rest.InClusterConfig(), which succeeds in-cluster but cannot be
// satisfied hermetically in a unit test — so the seam threads the fake
// server's *rest.Config there. The stage loop itself — including the
// per-stage apistageStore.Get(stageKey) where the SELF-HIT lives — runs
// entirely REAL. This is NOT the FA2-style stub (which stubbed the whole
// stage loop away and returned canned bytes); the self-hit code path is
// genuinely exercised (the FAIL-with-Get-skip-disabled run is dispositive).
//
// rcForSeam is the package-level *rest.Config the seam threads. A package
// var (not a closure capture) so the seam can be installed once by
// setResolveOnceForTest while the per-test fake server still varies.
var rcForSeam *rest.Config

func apistageResolveSeam(ctx context.Context, inputs cache.ResolvedKeyInputs) ([]byte, error) {
	ref := templatesv1.ObjectReference{
		Reference:  templatesv1.Reference{Name: inputs.Name, Namespace: inputs.Namespace},
		APIVersion: inputs.Group + "/" + inputs.Version,
		Resource:   inputs.Resource,
	}
	got := objects.Get(ctx, ref)
	if got.Err != nil {
		return nil, fmt.Errorf("seam re-fetch %s/%s: %s", inputs.Resource, inputs.Name, got.Err.Message)
	}
	if got.Unstructured == nil {
		return nil, fmt.Errorf("seam re-fetch %s/%s: nil object", inputs.Resource, inputs.Name)
	}
	var cr templatesv1.RESTAction
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(got.Unstructured.Object, &cr); err != nil {
		return nil, fmt.Errorf("seam unstructured -> RESTAction: %w", err)
	}
	// The REAL restactions.Resolve -> api.Resolve stage loop. Every field
	// matches resolveOnceProd's apistage branch (resolveRestActionForRefresh)
	// — AuthnNS/PerPage/Page/Extras — so the stage-key shape (PerPage/Page
	// feed stageInputHash) is exercised faithfully. SArc is the SOLE
	// divergence: the irreducible hermeticity seam (see the doc comment).
	res, err := restactions.Resolve(ctx, restactions.ResolveOptions{
		In:      &cr,
		SArc:    rcForSeam,
		AuthnNS: env.String("AUTHN_NAMESPACE", ""),
		PerPage: inputs.PerPage,
		Page:    inputs.Page,
		Extras:  inputs.Extras,
	})
	if err != nil {
		return nil, fmt.Errorf("seam resolve RESTAction: %w", err)
	}
	switch inputs.CacheEntryClass {
	case cache.CacheEntryClassApistage:
		// resolveOnceProd's apistage branch returns (nil,nil) on success —
		// the stage loop already re-Put each stage entry under its stage
		// key. Returning bytes would make resolveAndPopulateL1 Put the
		// whole-RESTAction output under the stage key.
		return nil, nil
	default:
		// restactions/widgets-class: hand back the encoded output, as
		// resolveOnceProd does for those kinds.
		return encodeResolvedJSON(res)
	}
}

// registerRealApistageRefresh wires the apistage refresh handler to the
// REAL resolveAndPopulateL1 path AND installs apistageResolveSeam as
// resolveOnceFn for the test's duration. The full
// resolveAndPopulateL1 -> resolveOnceFn chain is exercised — including
// the 0.30.118 WithRefreshBypass marker resolveAndPopulateL1 sets and the
// real api.Resolve stage loop the seam runs. The fake server is supplied
// as the SA transport so the background re-resolve — which has no
// per-user token — can reach it, exactly as RegisterRefreshHandlers
// supplies the real SA endpoint in production. restactions/widgets are
// registered too so the registry is production-faithful.
func registerRealApistageRefresh(t *testing.T, fakeURL string, rc *rest.Config) {
	t.Helper()
	rcForSeam = rc
	restore := setResolveOnceForTest(apistageResolveSeam)
	t.Cleanup(func() { restore(); rcForSeam = nil })

	saEP := &endpoints.Endpoint{ServerURL: fakeURL}
	refreshFunc := func(ctx context.Context, _ string, in cache.ResolvedKeyInputs) error {
		return resolveAndPopulateL1(ctx, in, saEP, rc)
	}
	cache.RegisterRefreshFunc("restactions", refreshFunc)
	cache.RegisterRefreshFunc("widgets", refreshFunc)
	cache.RegisterRefreshFunc(cache.CacheEntryClassApistage, refreshFunc)
}

// --- single-stage falsifier (AC-118.2) ------------------------------------

// TestFalsifier118_ApistageRefreshSelfHit_SingleStage drives the real
// resolveOnceProd -> restactions.Resolve stage loop for a single-stage
// RESTAction. After a cold resolve seeds the apistage L1 entry, the
// backing Widget LIST is mutated and the entry is dirty-marked; the real
// refresher must re-resolve from K8s and re-Put the FRESH value.
//
// FAILS without the 0.30.118 fix: the refresher's whole-RESTAction
// re-resolve self-hits the dirty-but-resident apistage entry (Get honors
// TTL only), skips the K8s LIST, never re-Puts — apistage_store_total
// does not advance past the cold-resolve count and the entry stays stale.
func TestFalsifier118_ApistageRefreshSelfHit_SingleStage(t *testing.T) {
	enableApistageCache(t)

	const raName = "selfhit-single-restaction"
	fake := newFakeAPIServer(t)
	fake.restActionPath = "/apis/templates.krateo.io/v1/namespaces/" + selfhitNS + "/restactions/" + raName

	// Single stage: LIST composition.krateo.io/v1 widgets in selfhitNS.
	// The stage `filter` is `.<stageID>.items` because the resolver's
	// jsonHandler wraps each stage's raw response under the stage id
	// before applying the filter (pig = {<stageID>: <response>}); a bare
	// `.items` would evaluate against that wrapper and resolve to null.
	const stageID = "compositions"
	listPath := "/apis/composition.krateo.io/v1/namespaces/" + selfhitNS + "/widgets"
	apiSpec := `[{"name":"` + stageID + `",` +
		`"path":"` + listPath + `",` +
		`"verb":"GET","filter":".` + stageID + `.items"}]`
	fake.restActionBody = restActionObjectBody(raName, apiSpec)
	// STALE backing object — one widget.
	fake.setListBody(listPath, widgetListBody("1", "w-stale"))

	rc := &rest.Config{Host: fake.URL()}

	// --- cold resolve: drives the real stage loop, Puts the apistage entry.
	coldDict := apistageColdResolve(t, fake.URL(), rc, raName, apiSpec)
	if !apistageDictHasWidget(coldDict, stageID, "w-stale") {
		t.Fatalf("cold resolve dict missing the seeded widget: %#v", coldDict)
	}

	store := cache.ResolvedCache()
	if store == nil {
		t.Fatal("resolved cache nil under RESOLVED_CACHE_ENABLED=true")
	}
	coldStoreTotal := store.Stats().ApistageStoreTotal
	if coldStoreTotal == 0 {
		t.Fatal("cold resolve Put no apistage entry — the key-swap never ran")
	}

	// The cold stage resolve recorded a LIST-dep for the widgets GVR under
	// the stage key. CollectMatchesForTest recovers that stage key without
	// needing a store key-enumerator.
	widgetsGVR := schema.GroupVersionResource{
		Group: "composition.krateo.io", Version: "v1", Resource: "widgets",
	}
	stageKey := singleMatch(t, widgetsGVR, selfhitNS, "stage entry")

	// --- mutate the backing K8s object: the widget LIST now returns a
	// DIFFERENT object set.
	fake.setListBody(listPath, widgetListBody("2", "w-fresh"))

	// --- wire the REAL refresh path and start the worker pool.
	registerRealApistageRefresh(t, fake.URL(), rc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache.StartRefresher(ctx)
	t.Cleanup(cache.StopRefresher)

	// --- dirty-mark the stage entry the way an informer UPDATE does.
	if marked := cache.Deps().OnUpdate(widgetsGVR, selfhitNS, "w-fresh"); marked < 1 {
		t.Fatalf("OnUpdate dirty-marked %d keys, want >=1 (the apistage stage entry)", marked)
	}

	// --- assert convergence: the apistage entry re-Puts the FRESH value.
	waitForApistageConverge(t, store, stageKey, "w-fresh", coldStoreTotal)
}

// --- two-stage falsifier (AC-118.6) ---------------------------------------

// TestFalsifier118_ApistageRefreshSelfHit_TwoStage is the AC-118.6
// two-stage case: a predecessor LIST stage (stage1) whose output an
// iterator stage (stage2) consumes. It asserts BOTH stages converge their
// OWN entry for their OWN backing object's change:
//
//   - stage2's backing object (panels in the iterated namespace) changes
//     -> stage2's entry converges fresh;
//   - the predecessor's backing object (the namespaces stage1 LISTs)
//     changes -> stage1's entry converges fresh.
//
// Each stage records its own LIST-dep under its own stage key, so an
// informer UPDATE of either GVR dirty-marks the matching stage entry and
// the refresher re-resolves it. Without the 0.30.118 bypass the
// whole-RESTAction re-resolve self-hits whichever stage entry is being
// refreshed and it stays stale.
func TestFalsifier118_ApistageRefreshSelfHit_TwoStage(t *testing.T) {
	enableApistageCache(t)

	const raName = "selfhit-twostage-restaction"
	fake := newFakeAPIServer(t)
	fake.restActionPath = "/apis/templates.krateo.io/v1/namespaces/" + selfhitNS + "/restactions/" + raName

	// stage1: LIST composition.krateo.io/v1 widgets — the predecessor.
	// stage2: dependsOn stage1, iterate its items, LIST panels per item.
	//
	// The stage `filter` is `.<stageID>.items` (the jsonHandler wraps the
	// raw response under the stage id before filtering — a bare `.items`
	// would resolve to null). stage1's filter therefore makes
	// dict["predecessor"] the widgets `items` array; stage2's iterator
	// `.predecessor` IS that array (jqutil.ForEach requires the iterator
	// query to RETURN a JSON array — a bare `.predecessor[]` stream
	// errors "must return a JSON array").
	const stage1ID = "predecessor"
	const stage2ID = "dependent"
	widgetsPath := "/apis/composition.krateo.io/v1/namespaces/" + selfhitNS + "/widgets"
	// stage2's path is a fixed panels LIST. With the predecessor seeded to
	// a single stable anchor widget the iterator expands to exactly one
	// panels LIST against a stable path, so the stage2 key is
	// deterministic.
	panelsPath := "/apis/composition.krateo.io/v1/namespaces/" + selfhitNS + "/panels"
	apiSpec := `[` +
		`{"name":"` + stage1ID + `","path":"` + widgetsPath + `","verb":"GET","filter":".` + stage1ID + `.items"},` +
		`{"name":"` + stage2ID + `","path":"` + panelsPath + `","verb":"GET","filter":".` + stage2ID + `.items",` +
		`"dependsOn":{"name":"` + stage1ID + `","iterator":".` + stage1ID + `"}}` +
		`]`
	fake.restActionBody = restActionObjectBody(raName, apiSpec)
	// Predecessor seeded with a single STABLE anchor widget so stage2's
	// iterator fan-out (and hence its stage key) is deterministic. The
	// per-stage convergence assertions below mutate the *content* the
	// stages serve, not the anchor.
	fake.setListBody(widgetsPath, widgetListBody("1", "anchor"))
	fake.setListBody(panelsPath, widgetListBody("1", "p-stale"))

	rc := &rest.Config{Host: fake.URL()}

	// --- cold resolve: drives the real two-stage loop.
	coldDict := apistageColdResolve(t, fake.URL(), rc, raName, apiSpec)
	if !apistageDictHasWidget(coldDict, stage1ID, "anchor") {
		t.Fatalf("cold resolve: predecessor stage missing its widget: %#v", coldDict)
	}
	if !apistageDictHasWidget(coldDict, stage2ID, "p-stale") {
		t.Fatalf("cold resolve: dependent stage missing its panel: %#v", coldDict)
	}

	store := cache.ResolvedCache()
	if store == nil {
		t.Fatal("resolved cache nil")
	}
	if store.Stats().ApistageStoreTotal < 2 {
		t.Fatalf("cold resolve: want >=2 apistage entries (stage1+stage2), got %d",
			store.Stats().ApistageStoreTotal)
	}

	widgetsGVR := schema.GroupVersionResource{
		Group: "composition.krateo.io", Version: "v1", Resource: "widgets",
	}
	panelsGVR := schema.GroupVersionResource{
		Group: "composition.krateo.io", Version: "v1", Resource: "panels",
	}
	// stage1 recorded a widgets LIST-dep; stage2 recorded a panels LIST-dep.
	stage1Key := singleMatch(t, widgetsGVR, selfhitNS, "stage1 (predecessor) entry")
	stage2Key := singleMatch(t, panelsGVR, selfhitNS, "stage2 (dependent) entry")
	if stage1Key == stage2Key {
		t.Fatal("stage1 and stage2 resolved to the SAME stage key — the two-stage " +
			"fixture is degenerate")
	}

	registerRealApistageRefresh(t, fake.URL(), rc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache.StartRefresher(ctx)
	t.Cleanup(cache.StopRefresher)

	// --- AC-118.6 case 1: stage2's backing object changes -> stage2
	//     converges. The dependent stage's panels LIST now returns a fresh
	//     object set; an informer UPDATE of the panels GVR dirty-marks
	//     stage2's entry.
	store2Total := store.Stats().ApistageStoreTotal
	fake.setListBody(panelsPath, widgetListBody("2", "p-fresh"))
	if marked := cache.Deps().OnUpdate(panelsGVR, selfhitNS, "p-fresh"); marked < 1 {
		t.Fatalf("OnUpdate(panels) dirty-marked %d keys, want >=1 (stage2 entry)", marked)
	}
	waitForApistageConverge(t, store, stage2Key, "p-fresh", store2Total)

	// --- AC-118.6 case 2: the predecessor's backing object changes ->
	//     stage1 converges. The predecessor's widgets LIST now returns a
	//     fresh object set ALONGSIDE the stable anchor (so stage2's
	//     iterator fan-out — and stage2's key — does not shift); an
	//     informer UPDATE of the widgets GVR dirty-marks stage1's entry.
	store1Total := store.Stats().ApistageStoreTotal
	fake.setListBody(widgetsPath, widgetListBody("3", "anchor", "w-fresh"))
	if marked := cache.Deps().OnUpdate(widgetsGVR, selfhitNS, "w-fresh"); marked < 1 {
		t.Fatalf("OnUpdate(widgets) dirty-marked %d keys, want >=1 (stage1 entry)", marked)
	}
	waitForApistageConverge(t, store, stage1Key, "w-fresh", store1Total)
}

// --- helpers --------------------------------------------------------------

// apistageInputs builds the apistage-class ResolvedKeyInputs for the
// owning RESTAction — the input resolveOnceProd's CacheEntryClassApistage
// branch (and the falsifier's faithful apistageResolveSeam) re-resolve
// from.
func apistageInputs(raName string) cache.ResolvedKeyInputs {
	return cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassApistage,
		Group:           "templates.krateo.io",
		Version:         "v1",
		Resource:        "restactions",
		Namespace:       selfhitNS,
		Name:            raName,
		Username:        selfhitIdentity.Username,
		Groups:          selfhitIdentity.Groups,
	}
}

// apistageColdResolve drives a cold (first) resolve through the faithful
// apistageResolveSeam: it re-fetches the RESTAction CR off the fake
// apiserver and runs the REAL restactions.Resolve -> api.Resolve stage
// loop, which Puts each per-stage apistage L1 entry and records its dep
// edges. Returns the resolved stage dict so the caller can assert the
// cold content.
//
// The cold resolve and the refresher both go through apistageResolveSeam
// — one code path, not two. The seam runs the SAME real stage loop the
// refresher's resolveAndPopulateL1 reaches.
func apistageColdResolve(t *testing.T, fakeURL string, rc *rest.Config, raName, _ string) map[string]any {
	t.Helper()
	rcForSeam = rc // the seam threads this into restactions.ResolveOptions.SArc
	ctx := resolveCtx(fakeURL, rc)
	inputs := apistageInputs(raName)
	rctx := cache.WithL1KeyContext(ctx, cache.ComputeKey(inputs))
	if _, err := apistageResolveSeam(rctx, inputs); err != nil {
		t.Fatalf("cold apistage resolve failed: %v", err)
	}
	// The apistage seam returns (nil,nil) — the stage loop Put the entries
	// but did not hand back the dict. Re-resolve under a restactions-class
	// input to capture the dict for content assertions: the seam returns
	// the encoded RESTAction whose `status` is the resolved dict. This
	// second resolve serves the just-Put stage entries from L1 and yields
	// the identical dict.
	return apistageResolveDict(t, rctx, raName)
}

// apistageResolveDict drives the faithful seam under a restactions-class
// input and returns the resolved stage dict (the RESTAction's `status`).
func apistageResolveDict(t *testing.T, ctx context.Context, raName string) map[string]any {
	t.Helper()
	encoded, err := apistageResolveSeam(ctx, cache.ResolvedKeyInputs{
		CacheEntryClass: "restactions",
		Group:           "templates.krateo.io",
		Version:         "v1",
		Resource:        "restactions",
		Namespace:       selfhitNS,
		Name:            raName,
		Username:        selfhitIdentity.Username,
		Groups:          selfhitIdentity.Groups,
	})
	if err != nil {
		t.Fatalf("resolve RESTAction for dict failed: %v", err)
	}
	return decodeStageDict(t, encoded)
}

// waitForApistageConverge polls the apistage L1 entry under key until its
// content carries wantToken AND apistage_store_total has advanced past
// baselineStoreTotal (the entry was genuinely re-Put). Fails after 5s.
func waitForApistageConverge(t *testing.T, store *cache.ResolvedCacheStore, key, wantToken string, baselineStoreTotal uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entry, ok := store.Get(key)
		if ok && entry != nil &&
			strings.Contains(string(entry.RawJSON), wantToken) &&
			store.Stats().ApistageStoreTotal > baselineStoreTotal {
			return // converged — PASS
		}
		if time.Now().After(deadline) {
			cur := "<gone>"
			if e, k := store.Get(key); k && e != nil {
				cur = string(e.RawJSON)
			}
			t.Fatalf("apistage L1 entry %s did NOT converge within 5s — "+
				"content=%s want token %q; apistage_store_total=%d baseline=%d. "+
				"The refresher's whole-RESTAction re-resolve self-hit the "+
				"dirty-but-resident stage entry (Get honors TTL only), skipped "+
				"the K8s call, and never re-Put — the 0.30.118 self-hit defect.",
				key[:12], cur, wantToken, store.Stats().ApistageStoreTotal, baselineStoreTotal)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// singleMatch returns the single L1 key whose LIST-dep matches (gvr, ns).
// Fails if zero or more than one key matched — the stage-entry recovery
// must be unambiguous.
func singleMatch(t *testing.T, gvr schema.GroupVersionResource, ns, what string) string {
	t.Helper()
	matches := cache.Deps().CollectMatchesForTest(gvr, ns, "any")
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 L1 key matching the %s dep (%s/%s), got %d: %v",
			what, gvr.Resource, ns, len(matches), matches)
	}
	for k := range matches {
		return k
	}
	return ""
}

// apistageDictHasWidget reports whether dict[stageID] (a resolver stage
// output) JSON-encodes to a value containing the given object name.
func apistageDictHasWidget(dict map[string]any, stageID, name string) bool {
	v, ok := dict[stageID]
	if !ok {
		return false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), `"`+name+`"`)
}

// decodeStageDict extracts the resolved stage dict from an encoded
// RESTAction. restactions.Resolve writes the resolver dict into the
// RESTAction's `status` field (status.Raw == json(dict)); the encoded CR
// therefore carries the dict under the top-level `status` key.
func decodeStageDict(t *testing.T, encoded []byte) map[string]any {
	t.Helper()
	var cr struct {
		Status map[string]any `json:"status"`
	}
	if err := json.Unmarshal(encoded, &cr); err != nil {
		t.Fatalf("decode RESTAction status: %v (raw=%s)", err, string(encoded))
	}
	return cr.Status
}
