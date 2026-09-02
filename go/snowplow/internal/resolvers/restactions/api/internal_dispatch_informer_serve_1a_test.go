// internal_dispatch_informer_serve_1a_test.go — #121 1a falsifiers.
//
// 1a adds an INFORMER-SERVE branch to dispatchViaInternalRESTConfig's
// cluster/namespace-wide LIST path: when a serve-watcher is attached to ctx
// (the prewarm walk/seed context, via cache.WithServeWatcher) AND the target
// GVR is servable, the LIST is served from the synced informer indexer
// (cache.ListServableEnvelopeJSON) instead of a live paged apiserver LIST.
// This kills the boot-walk deadline-cut root cause: the 27.5s/60K live LIST
// that ran ~24s after the composition informer had already synced
// (docs/boot-walk-deadline-rootcause-2026-07-09.md).
//
// Arms in this file:
//
//   WHOLE-DESIGN GREEN (informer-served) — a servable, populated GVR + a
//     ctx serve-watcher → dispatch serves from the informer with ZERO live
//     apiserver calls, and the `internal_dispatch.list.informer_served` event
//     fires. RED: revert the branch → the live paged LIST runs (calls>0), the
//     boot-walk symptom persists.
//
//   C1 "NEVER WORSE" fall-through — a registered-but-NEVER-synced GVR (the
//     informer never reaches HasSynced within the bounded wait) → the bounded
//     WaitForGVRSync expires, IsServable is false, and the dispatch FALLS
//     THROUGH to the live paged LIST which COMPLETES normally (served=true,
//     all items, paged_list.completed fires). The bounded wait consumed ≤
//     internalDispatchServeSyncWait+slack, so it cannot re-create the seed
//     deadline-cut. RED: an unbounded wait would hang the test.
//
//   BYTE-PARITY golden — the informer-served envelope vs the live-SA-LIST
//     envelope: items-equal (per-item bytes identical) AND envelope-shape-
//     equal ({apiVersion, kind:<Kind>List, items}), MODULO metadata.
//     resourceVersion + metadata.continue (a cached snapshot has no single
//     live collection RV; the live path itself clears continue). This is the
//     honest realized parity — strict byte-identity is impossible because a
//     fresh live LIST pins a live RV a cached read cannot reproduce; the
//     composition-list JQ reads only .items[], so the difference is
//     provably consumer-invariant.
//
//   NO-SERVE-WATCHER byte-identity — WITHOUT a ctx serve-watcher (the
//     per-user /call path) the dispatch is byte-identical to pre-1a: it runs
//     the live paged LIST, calls>0, no informer_served event. Proves the
//     scope gate (customer path unchanged).

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	httpcall "github.com/krateo-platformops/plumbing/http/request"
	"github.com/krateo-platformops/snowplow/internal/cache"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"

	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// serve1aLiveFixture is a minimal TLS apiserver serving a paged BenchAppList
// — the LIVE LIST the informer-serve branch replaces (and the byte-parity
// comparison arm). Item shape mirrors serve1aItem so parity is like-for-like.
// Kept local (not newPagedListFixture) so the kind (BenchApp/BenchAppList)
// matches serve1aGVR's clean plural guess (benchapps).
type serve1aLiveFixture struct {
	server *httptest.Server
	calls  atomic.Int64
	pages  []int // page index → item count
}

// newServe1aLiveFixture emits per-item resourceVersion 2000+gi — MATCHING the
// informer items (serve1aItem uses 2000+idx) for the main parity + RED arms.
func newServe1aLiveFixture(t *testing.T, totalItems, pageSize int) (*serve1aLiveFixture, []byte) {
	return newServe1aLiveFixtureRVOffset(t, totalItems, pageSize, 2000)
}

// newServe1aLiveFixtureRVOffset parametrises the per-item resourceVersion base
// (rvBase+gi). The RV-differs guardrail arm passes a DIFFERENT base (9000) than
// the informer items (2000) so the two serves differ ONLY on per-item RV —
// proving the golden is RV-modulo (does not false-RED a faithful serve).
func newServe1aLiveFixtureRVOffset(t *testing.T, totalItems, pageSize, rvBase int) (*serve1aLiveFixture, []byte) {
	t.Helper()
	f := &serve1aLiveFixture{}
	rem := totalItems
	for rem > 0 {
		n := pageSize
		if n > rem {
			n = rem
		}
		f.pages = append(f.pages, n)
		rem -= n
	}
	if totalItems == 0 {
		f.pages = []int{0}
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		q := r.URL.Query()
		cont := q.Get("continue")
		pageIdx := 0
		if cont != "" {
			pageIdx, _ = strconv.Atoi(cont)
		}
		offset := 0
		for i := 0; i < pageIdx; i++ {
			offset += f.pages[i]
		}
		var itemsBuf bytes.Buffer
		for i := 0; i < f.pages[pageIdx]; i++ {
			if i > 0 {
				itemsBuf.WriteByte(',')
			}
			// Emit the SAME per-item content as serve1aItem(gi,...) — full field
			// set (metadata name/namespace/resourceVersion/labels/annotations +
			// spec + status) so the content byte-parity arm compares like-for-like
			// across every ASSERTED field (per-item RV asserted EQUAL, arch FINAL).
			//
			// PLUS, on the LIVE arm ONLY: metadata.managedFields + the kubectl
			// last-applied annotation — the two fields the informer store STRIPS at
			// Put (stripItemJSON). The informer-served arm will NOT carry them, so
			// this real informer-vs-live asymmetry EXERCISES the golden's exclusion
			// (proves the golden STAYS GREEN despite the divergence, not that the
			// fixture avoids it). The content annotation (app.krateo.io/note)
			// survives the strip on both arms → asserted equal.
			gi := offset + i
			fmt.Fprintf(&itemsBuf,
				`{"apiVersion":%q,"kind":%q,`+
					`"metadata":{"name":"composition-%d","namespace":"bench-ns-%d","resourceVersion":"%d",`+
					`"uid":"uid-%d","creationTimestamp":"2026-07-09T0%d:00:00Z",`+
					`"managedFields":[{"manager":"kubectl","operation":"Update","apiVersion":%q}],`+
					`"labels":{"app.krateo.io/tier":"t%d"},`+
					`"annotations":{"app.krateo.io/note":"note-%d","kubectl.kubernetes.io/last-applied-configuration":"{\"apiVersion\":\"%s\"}"}},`+
					`"spec":{"replicas":%d,"image":"img-%d"},`+
					`"status":{"ready":%t,"phase":"Running"}}`,
				serve1aAPIVersion, serve1aItemKind, gi, gi, rvBase+gi, gi, gi%10, serve1aAPIVersion, gi%3, gi, serve1aAPIVersion, gi, gi, gi%2 == 0)
		}
		nextContinue := ""
		if pageIdx+1 < len(f.pages) {
			nextContinue = strconv.Itoa(pageIdx + 1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w,
			`{"apiVersion":%q,"kind":%q,"metadata":{"resourceVersion":"%d","continue":"%s"},"items":[%s]}`,
			serve1aAPIVersion, serve1aListKind, 1000+pageIdx, nextContinue, itemsBuf.String())
	}))
	t.Cleanup(srv.Close)
	f.server = srv
	return f, pemEncodeCert(srv.Certificate())
}

// serve1aGVR is the composition GVR the 1a arms LIST. Kind "BenchApp"
// guesses (meta.UnsafeGuessKindToResource) to resource "benchapps", so the
// fake dynamic client's tracker keys the seeded items on serve1aGVR and the
// informer's initial LIST actually returns them (a kind whose guessed plural
// != the GVR resource would silently list empty — the gotcha this avoids).
var serve1aGVR = schema.GroupVersionResource{
	Group:    "composition.krateo.io",
	Version:  "v1-2-2",
	Resource: "benchapps",
}

const (
	serve1aListPath   = "/apis/composition.krateo.io/v1-2-2/benchapps"
	serve1aAPIVersion = "composition.krateo.io/v1-2-2"
	serve1aItemKind   = "BenchApp"
	serve1aListKind   = "BenchAppList"
)

// serve1aItem builds one composition item. The informer path and the live
// fixture (serve1aLiveFixture) emit the SAME per-item shape so the content
// byte-parity arm compares like-for-like. TL DECISION (A): carries the full
// content-load-bearing field set — labels + annotations + spec + status (the
// widget JQ renders spec/status; the RBAC refilter reads namespace) — plus a
// per-item resourceVersion (asserted EQUAL: the informer preserves the item's
// own RV verbatim at Put (bytesobject.go SetResourceVersion), so for a static
// object it matches a fresh live-LIST item RV — RED_PerItemRVMismatch pins it.
// Only the LIST-LEVEL RV is modulo, since streaming_list synthesizes it).
func serve1aItem(idx int, name, ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": serve1aAPIVersion,
		"kind":       serve1aItemKind,
		"metadata": map[string]any{
			"name":            name,
			"namespace":       ns,
			"resourceVersion": itoa(2000 + idx), // asserted EQUAL (informer preserves per-item RV verbatim)
			// uid + creationTimestamp: IMMUTABLE + consumer-read (compositions-panel
			// row-table renders .metadata.uid; compositions-panels RA sorts on
			// .metadata.creationTimestamp) → the negative projection asserts them
			// equal, and RED_PerItemUIDDrop/CreationTimestampCorrupt pin the coverage.
			// Both arms emit the SAME values so the assert-equal holds.
			"uid":               "uid-" + itoa(idx),
			"creationTimestamp": "2026-07-09T0" + itoa(idx%10) + ":00:00Z",
			"labels":            map[string]any{"app.krateo.io/tier": "t" + itoa(idx%3)},
			"annotations":       map[string]any{"app.krateo.io/note": "note-" + itoa(idx)},
		},
		"spec":   map[string]any{"replicas": int64(idx), "image": "img-" + itoa(idx)},
		"status": map[string]any{"ready": idx%2 == 0, "phase": "Running"},
	}}
}

// newServe1aWatcher builds a cache=on watcher seeded with `items` for
// serve1aGVR, registers + syncs its informer, and (when servable=true) leaves
// it servable (no disco client wired → conjunct 4 is degraded-true, so
// IsServable holds on HasSynced). Returns the watcher.
//
// When sync=false the informer is NOT registered/synced (never-synced arm):
// the GVR stays not-servable so the dispatch must fall through.
func newServe1aWatcher(t *testing.T, sync bool, items ...*unstructured.Unstructured) *cache.ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")

	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)
	listKinds := map[schema.GroupVersionResource]string{
		serve1aGVR: serve1aListKind,
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:                "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:         "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:         "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
	}
	var seed []runtime.Object
	for _, it := range items {
		seed = append(seed, it)
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, seed...)

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher under CACHE_ENABLED=true")
	}
	t.Cleanup(rw.Stop)

	if sync {
		added, syncCh := rw.EnsureResourceType(serve1aGVR)
		if !added {
			t.Fatalf("EnsureResourceType(%s): want added=true", serve1aGVR)
		}
		select {
		case <-syncCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s informer did not sync within 5s", serve1aGVR)
		}
		if !rw.IsServable(serve1aGVR) {
			t.Fatalf("precondition: %s must be servable after sync", serve1aGVR)
		}
	}
	return rw
}

// TestServe1a_InformerServed_WholeDesignGreen is the whole-design GREEN arm:
// a servable, populated GVR + a ctx serve-watcher → the dispatch serves from
// the informer with ZERO live apiserver round-trips.
func TestServe1a_InformerServed_WholeDesignGreen(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	const n = 20
	items := make([]*unstructured.Unstructured, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, serve1aItem(i, "composition-"+itoa(i), "krateo-system"))
	}
	rw := newServe1aWatcher(t, true, items...)

	// A live fixture whose call-count we assert stays ZERO — if the informer
	// branch is reverted, the dispatch would hit this server (calls>0).
	fixture, caPEM := newServe1aLiveFixture(t, n, int(internalDispatchListPageLimit))
	rc := &rest.Config{
		Host:            fixture.server.URL,
		BearerToken:     "fake-sa-jwt",
		TLSClientConfig: rest.TLSClientConfig{CAData: caPEM},
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx := cache.WithInternalRESTConfig(context.Background(), rc)
	ctx = cache.WithServeWatcher(ctx, rw)
	ctx = withSlogLogger(ctx, logger)

	raw, served, err := dispatchViaInternalRESTConfig(ctx, httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{Path: serve1aListPath},
	})
	if err != nil {
		t.Fatalf("informer-serve dispatch returned error: %v", err)
	}
	if !served {
		t.Fatal("expected served=true from the informer-serve branch")
	}
	// ZERO live apiserver calls — the whole point of 1a.
	if calls := fixture.calls.Load(); calls != 0 {
		t.Fatalf("WHOLE-DESIGN RED: dispatch made %d LIVE apiserver calls; want 0 "+
			"(the informer-serve branch must replace the live paged LIST — the 27.5s/60K "+
			"boot-walk LIST is exactly this)", calls)
	}
	// informer_served event fired.
	if !strings.Contains(logBuf.String(), `"msg":"internal_dispatch.list.informer_served"`) {
		t.Fatalf("expected the informer_served falsifier event; log:\n%s", logBuf.String())
	}
	// All items present.
	items0 := serve1aItemsFromRaw(t, raw)
	if len(items0) != n {
		t.Fatalf("informer-served list carries %d items, want %d", len(items0), n)
	}
}

// TestServe1a_C1_NeverWorse_FallsThroughToLiveList is the C1 arm: a
// registered-but-NEVER-synced GVR → bounded WaitForGVRSync expires → the
// dispatch FALLS THROUGH to the live paged LIST which COMPLETES. Never worse.
func TestServe1a_C1_NeverWorse_FallsThroughToLiveList(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	// Watcher WITHOUT the GVR synced — the never-synced race. IsServable is
	// false, so the informer branch must fall through.
	rw := newServe1aWatcher(t, false)
	if rw.IsServable(serve1aGVR) {
		t.Fatal("precondition: an unregistered/never-synced GVR must not be servable")
	}

	const n = 750 // 2 pages @500 — the live walk completes were it reached
	fixture, caPEM := newServe1aLiveFixture(t, n, int(internalDispatchListPageLimit))
	rc := &rest.Config{
		Host:            fixture.server.URL,
		BearerToken:     "fake-sa-jwt",
		TLSClientConfig: rest.TLSClientConfig{CAData: caPEM},
	}

	var logBuf bytes.Buffer
	// LevelDebug: internal_dispatch.paged_list.completed is a DEBUG event
	// (#170) — capture it so the fall-through assertion below still verifies
	// the live paged LIST completed (the level dropped WARN→DEBUG).
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx := cache.WithInternalRESTConfig(context.Background(), rc)
	ctx = cache.WithServeWatcher(ctx, rw)
	ctx = withSlogLogger(ctx, logger)

	start := time.Now()
	raw, served, err := dispatchViaInternalRESTConfig(ctx, httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{Path: serve1aListPath},
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("C1 FAIL: fall-through dispatch returned error (should complete the live LIST): %v", err)
	}
	if !served {
		t.Fatal("C1 FAIL: expected served=true from the live-LIST fall-through")
	}
	// It ran the LIVE paged LIST (fell through) — calls>0 + the paged event.
	if calls := fixture.calls.Load(); calls == 0 {
		t.Fatal("C1 FAIL: expected the dispatch to FALL THROUGH to the live paged LIST " +
			"(calls>0) when the GVR is unsynced; got 0 live calls")
	}
	if !strings.Contains(logBuf.String(), `"msg":"internal_dispatch.paged_list.completed"`) {
		t.Fatalf("C1 FAIL: expected the live paged_list.completed event on fall-through; log:\n%s", logBuf.String())
	}
	// All items present (the fall-through completed the whole list).
	if got := len(serve1aItemsFromRaw(t, raw)); got != n {
		t.Fatalf("C1 FAIL: fall-through served %d items, want %d (the live LIST must complete)", got, n)
	}
	// C1(b) bound proportionality — the unregistered GVR returns from
	// WaitForGVRSync immediately (not even registered), so the wait cost here
	// is ~0; assert well under the bound+the live-LIST time, proving the wait
	// cannot itself blow the budget. Generous ceiling for CI.
	if elapsed > internalDispatchServeSyncWait+10*time.Second {
		t.Fatalf("C1 FAIL: total dispatch took %v — the bounded sync-wait must not "+
			"dominate (bound=%v). An unbounded wait would hang.", elapsed, internalDispatchServeSyncWait)
	}
}

// TestServe1a_ByteParity_InformerVsLiveList is the byte-parity golden.
//
// PM REWORK (feedback_byte_parity_golden_must_assert_peritem_bytes): the arm
// asserts PER-ITEM BYTE equality — every served item marshalled through the
// REAL serve path (ListServableEnvelopeJSON) is byte-identical to the live-SA-
// LIST item of the same name, over the RBAC-LOAD-BEARING fields the
// userAccessFilter refilter reads (.metadata.namespace, .metadata.name). A
// name-SET-only assertion (the prior weak arm) would pass a serve path that
// silently dropped .metadata.namespace or injected a spoof field per item —
// exactly the per-item content transform that would BREAK downstream RBAC
// (NamespaceFrom reads .metadata.namespace). The mutation probe below proves
// the strengthened arm DISCRIMINATES that class (RED on a per-item transform),
// not just a dropped/added item.
//
// Envelope-level modulo (documented, arch-confirmed consumer-safe): the
// LIST-level metadata.resourceVersion may differ (a cached snapshot has no
// single live collection RV; the composition-list JQ reads only .items[], never
// the list-level RV) and metadata.continue is empty on both. Parity is asserted
// on items + {apiVersion,kind} shape, MODULO those two list-level fields.
func TestServe1a_ByteParity_InformerVsLiveList(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	const n = 12
	items := make([]*unstructured.Unstructured, 0, n)
	for i := 0; i < n; i++ {
		// Distinct namespace per item so a namespace-drop/spoof is a REAL
		// per-item content change the RBAC-field assertion can catch (not
		// masked by a uniform namespace).
		items = append(items, serve1aItem(i, "composition-"+itoa(i), "bench-ns-"+itoa(i)))
	}

	// (1) LIVE bytes — no serve-watcher, hit the fixture.
	fixture, caPEM := newServe1aLiveFixture(t, n, int(internalDispatchListPageLimit))
	rc := &rest.Config{
		Host:            fixture.server.URL,
		BearerToken:     "fake-sa-jwt",
		TLSClientConfig: rest.TLSClientConfig{CAData: caPEM},
	}
	liveCtx := cache.WithInternalRESTConfig(context.Background(), rc)
	liveRaw, liveServed, err := dispatchViaInternalRESTConfig(liveCtx, httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{Path: serve1aListPath},
	})
	if err != nil || !liveServed {
		t.Fatalf("live LIST arm: served=%v err=%v", liveServed, err)
	}

	// (2) INFORMER bytes — serve-watcher, GVR populated with the SAME items,
	// served through the REAL path (ListServableEnvelopeJSON via the dispatch).
	rw := newServe1aWatcher(t, true, items...)
	infCtx := cache.WithInternalRESTConfig(context.Background(), rc)
	infCtx = cache.WithServeWatcher(infCtx, rw)
	infRaw, infServed, err := dispatchViaInternalRESTConfig(infCtx, httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{Path: serve1aListPath},
	})
	if err != nil || !infServed {
		t.Fatalf("informer LIST arm: served=%v err=%v", infServed, err)
	}

	liveEnv := decodeListEnvelope(t, liveRaw)
	infEnv := decodeListEnvelope(t, infRaw)

	// Envelope-shape: apiVersion + kind identical.
	if liveEnv.APIVersion != infEnv.APIVersion {
		t.Errorf("BYTE-PARITY FAIL: apiVersion live=%q inf=%q", liveEnv.APIVersion, infEnv.APIVersion)
	}
	if liveEnv.Kind != infEnv.Kind {
		t.Errorf("BYTE-PARITY FAIL: kind live=%q inf=%q (expected <Kind>List parity)", liveEnv.Kind, infEnv.Kind)
	}
	if liveEnv.Metadata.Continue != "" || infEnv.Metadata.Continue != "" {
		t.Errorf("BYTE-PARITY FAIL: continue must be empty on both; live=%q inf=%q",
			liveEnv.Metadata.Continue, infEnv.Metadata.Continue)
	}

	// PER-ITEM CONTENT byte equality over the full content-load-bearing field
	// set (TL DECISION A): metadata.{name,namespace,labels,annotations} + spec +
	// status, MODULO per-item resourceVersion. Any per-item drop/spoof/corruption
	// of a consumer-read field (RBAC namespace OR widget spec/status/labels/
	// annotations) diverges here.
	liveItems := indexContentItemsByName(t, liveRaw)
	infItems := indexContentItemsByName(t, infRaw)
	if diff := contentItemsDiff(liveItems, infItems); diff != "" {
		t.Fatalf("BYTE-PARITY FAIL: per-item content bytes differ:\n%s", diff)
	}

	// Built-in self-guard (non-discrimination): the drop-namespace+spoof probe
	// on the SERVED items must diverge. If it ever passes, the golden regressed
	// to non-discriminating.
	if diff := contentItemsDiff(liveItems, mutateContentDropNamespaceInjectSpoof(infItems)); diff == "" {
		t.Fatal("SELF-GUARD FAIL: drop-namespace + spoof-inject did NOT break per-item content " +
			"equality — the golden is non-discriminating.")
	}
}

// TestServe1a_ByteParity_RED_PerItemSpecStatus is the content-fidelity RED arm
// (TL DECISION A's NEW discrimination — the one the name+namespace floor stayed
// green under): mutate per-item spec + status (metadata untouched) → the golden
// MUST FAIL. The widget JQ renders spec/status; a serve path corrupting them is
// a real correctness bug beyond RBAC. A metadata-only golden would stay green.
func TestServe1a_ByteParity_RED_PerItemSpecStatus(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	const n = 12
	items := make([]*unstructured.Unstructured, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, serve1aItem(i, "composition-"+itoa(i), "bench-ns-"+itoa(i)))
	}
	liveRaw, infRaw := serve1aLiveAndInformerRaw(t, n, items)
	liveItems := indexContentItemsByName(t, liveRaw)
	infItems := indexContentItemsByName(t, infRaw)
	if diff := contentItemsDiff(liveItems, infItems); diff != "" {
		t.Fatalf("precondition FAIL: unmutated items already differ:\n%s", diff)
	}
	if diff := contentItemsDiff(liveItems, mutateContentSpecStatus(infItems)); diff == "" {
		t.Fatal("RED ARM FAIL: mutating per-item spec.replicas + status.phase did NOT break " +
			"content equality — the golden does not assert spec/status fidelity, so a serve " +
			"path that corrupted rendered content would pass (the floor's blind spot).")
	}
}

// TestServe1a_ByteParity_RED_PerItemUIDDrop is the completeness RED arm for
// metadata.uid: uid is IMMUTABLE + consumer-read (compositions-panel row-table
// renders .metadata.uid), so the negative projection MUST compare it. The
// fixture emits uid on BOTH arms; corrupting it MUST break the golden. Without
// the fixture-emit + this arm the negative-projection uid coverage is a no-op
// (the 1b9729f-lineage fixture-blindspot PM caught).
func TestServe1a_ByteParity_RED_PerItemUIDDrop(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	const n = 12
	items := make([]*unstructured.Unstructured, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, serve1aItem(i, "composition-"+itoa(i), "bench-ns-"+itoa(i)))
	}
	liveRaw, infRaw := serve1aLiveAndInformerRaw(t, n, items)
	liveItems := indexContentItemsByName(t, liveRaw)
	infItems := indexContentItemsByName(t, infRaw)
	if diff := contentItemsDiff(liveItems, infItems); diff != "" {
		t.Fatalf("precondition FAIL: unmutated items already differ:\n%s", diff)
	}
	if diff := contentItemsDiff(liveItems, mutateContentUID(infItems)); diff == "" {
		t.Fatal("RED ARM FAIL: mutating per-item metadata.uid did NOT break content " +
			"equality — uid is NOT asserted (fixture may not emit it, or projection drops it). " +
			"uid is immutable + consumer-read → MUST be in the compared set.")
	}
}

// TestServe1a_ByteParity_RED_PerItemCreationTimestampCorrupt is the completeness
// RED arm for metadata.creationTimestamp (immutable + consumer-read: the
// compositions-panels RA sort_by(.metadata.creationTimestamp)). Corrupting it
// MUST break the golden.
func TestServe1a_ByteParity_RED_PerItemCreationTimestampCorrupt(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	const n = 12
	items := make([]*unstructured.Unstructured, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, serve1aItem(i, "composition-"+itoa(i), "bench-ns-"+itoa(i)))
	}
	liveRaw, infRaw := serve1aLiveAndInformerRaw(t, n, items)
	liveItems := indexContentItemsByName(t, liveRaw)
	infItems := indexContentItemsByName(t, infRaw)
	if diff := contentItemsDiff(liveItems, infItems); diff != "" {
		t.Fatalf("precondition FAIL: unmutated items already differ:\n%s", diff)
	}
	if diff := contentItemsDiff(liveItems, mutateContentCreationTimestamp(infItems)); diff == "" {
		t.Fatal("RED ARM FAIL: mutating per-item metadata.creationTimestamp did NOT break " +
			"content equality — creationTimestamp is NOT asserted. It is immutable + " +
			"consumer-read (sort key) → MUST be in the compared set.")
	}
}

// TestServe1a_ByteParity_StripSetExcludedStaysGreen is arch FINAL arm #3: the
// managedFields + last-applied EXCLUSION is EXERCISED against a REAL asymmetry.
// The live fixture arm CARRIES metadata.managedFields + the kubectl last-applied
// annotation; the informer-served arm has them STRIPPED (stripItemJSON). This
// arm PROVES the asymmetry is real (live carries, informer strips) AND that the
// golden STAYS GREEN (the exclusion works) — not that the fixture avoids the
// fields. The content annotation (app.krateo.io/note) survives on BOTH → still
// asserted equal (exclusion scoped to last-applied only, not all annotations).
func TestServe1a_ByteParity_StripSetExcludedStaysGreen(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	const n = 12
	items := make([]*unstructured.Unstructured, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, serve1aItem(i, "composition-"+itoa(i), "bench-ns-"+itoa(i)))
	}
	liveRaw, infRaw := serve1aLiveAndInformerRaw(t, n, items)
	liveS, infS := string(liveRaw), string(infRaw)

	// PROVE the asymmetry is REAL (not fixture-avoided).
	if !strings.Contains(liveS, "managedFields") {
		t.Fatal("FIXTURE-CARRY FAIL: the LIVE arm must CARRY metadata.managedFields so the " +
			"exclusion is EXERCISED — it does not.")
	}
	if !strings.Contains(liveS, "last-applied-configuration") {
		t.Fatal("FIXTURE-CARRY FAIL: the LIVE arm must CARRY the kubectl last-applied annotation.")
	}
	if strings.Contains(infS, "managedFields") {
		t.Fatal("STRIP FAIL: the informer-served arm must have managedFields STRIPPED (SetTransform) — present.")
	}
	if strings.Contains(infS, "last-applied-configuration") {
		t.Fatal("STRIP FAIL: the informer-served arm must have last-applied STRIPPED — present.")
	}
	// The CONTENT annotation must survive on BOTH (exclusion scoped to last-applied only).
	if !strings.Contains(liveS, "app.krateo.io/note") || !strings.Contains(infS, "app.krateo.io/note") {
		t.Fatal("CONTENT-ANNO FAIL: app.krateo.io/note must be present on BOTH arms.")
	}
	// GIVEN that real asymmetry, the golden STAYS GREEN (exclusion works).
	if diff := contentItemsDiff(indexContentItemsByName(t, liveRaw), indexContentItemsByName(t, infRaw)); diff != "" {
		t.Fatalf("STRIP-SET EXCLUSION FAIL: golden went RED despite {managedFields,last-applied} "+
			"being the ONLY per-item difference — the exclusion is not handling the real "+
			"informer-vs-live asymmetry. diff:\n%s", diff)
	}
}

// TestServe1a_ByteParity_RED_PerItemRVMismatch is arch FINAL arm #4 — the arm
// that PINS the reconciliation (per-item RV is ASSERTED EQUAL, not excluded).
// It mutates per-item metadata.resourceVersion on the served items → the golden
// MUST FAIL. Without this arm, "assert per-item RV equal" is claimed but never
// tested. (Contrast the arch's fixture-static note: real-object #124 may relax
// per-item RV to modulo; for THIS static-fixture ship, per-item RV is asserted.)
func TestServe1a_ByteParity_RED_PerItemRVMismatch(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	const n = 12
	items := make([]*unstructured.Unstructured, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, serve1aItem(i, "composition-"+itoa(i), "bench-ns-"+itoa(i)))
	}
	liveRaw, infRaw := serve1aLiveAndInformerRaw(t, n, items)
	liveItems := indexContentItemsByName(t, liveRaw)
	infItems := indexContentItemsByName(t, infRaw)
	if diff := contentItemsDiff(liveItems, infItems); diff != "" {
		t.Fatalf("precondition FAIL: unmutated items already differ:\n%s", diff)
	}
	if diff := contentItemsDiff(liveItems, mutateContentPerItemRV(infItems)); diff == "" {
		t.Fatal("RED ARM FAIL: mutating per-item metadata.resourceVersion did NOT break " +
			"content equality — per-item RV is NOT asserted (it's excluded). arch FINAL requires " +
			"per-item RV ASSERTED EQUAL for the static fixture.")
	}
}

// TestServe1a_ByteParity_ListLevelRVDiffStaysGreen is arch FINAL arm #5: the
// two serves' LIST-LEVEL metadata.resourceVersion legitimately differ (a cached
// snapshot has no single collection RV; nobody reads it) → the golden STAYS
// GREEN. This is the ONLY RV exclusion (list-level, not per-item). Proves the
// golden does not false-RED on the collection cursor.
func TestServe1a_ByteParity_ListLevelRVDiffStaysGreen(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	const n = 12
	items := make([]*unstructured.Unstructured, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, serve1aItem(i, "composition-"+itoa(i), "bench-ns-"+itoa(i)))
	}
	// Live fixture uses list-level RV base 1000+pageIdx; the informer-served
	// envelope carries no list-level RV — so the LIST-LEVEL RVs differ, while
	// per-item content (incl per-item RV 2000+i, equal both arms) matches.
	liveRaw, infRaw := serve1aLiveAndInformerRaw(t, n, items)
	liveEnv := decodeListEnvelope(t, liveRaw)
	infEnv := decodeListEnvelope(t, infRaw)
	// Confirm the list-level RVs REALLY differ (else the arm is vacuous).
	if liveEnv.Metadata.ResourceVersion == infEnv.Metadata.ResourceVersion {
		t.Skipf("list-level RVs coincidentally equal (live=%q inf=%q) — arm needs them to differ to be meaningful",
			liveEnv.Metadata.ResourceVersion, infEnv.Metadata.ResourceVersion)
	}
	// Per-item content golden STAYS GREEN despite the list-level RV difference.
	if diff := contentItemsDiff(indexContentItemsByName(t, liveRaw), indexContentItemsByName(t, infRaw)); diff != "" {
		t.Fatalf("FALSE-RED: list-level RV difference broke the per-item content golden — "+
			"list-level RV must be excluded (only per-item content is asserted). diff:\n%s", diff)
	}
}

// TestServe1a_NoServeWatcher_ByteIdenticalToPre1a proves the scope gate: with
// NO serve-watcher (the per-user /call path) the dispatch runs the live paged
// LIST exactly as pre-1a — calls>0, no informer_served event.
func TestServe1a_NoServeWatcher_ByteIdenticalToPre1a(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	const n = 10
	fixture, caPEM := newServe1aLiveFixture(t, n, int(internalDispatchListPageLimit))
	rc := &rest.Config{
		Host:            fixture.server.URL,
		BearerToken:     "fake-sa-jwt",
		TLSClientConfig: rest.TLSClientConfig{CAData: caPEM},
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// NO WithServeWatcher — the customer path.
	ctx := cache.WithInternalRESTConfig(context.Background(), rc)
	ctx = withSlogLogger(ctx, logger)

	_, served, err := dispatchViaInternalRESTConfig(ctx, httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{Path: serve1aListPath},
	})
	if err != nil || !served {
		t.Fatalf("no-serve-watcher arm: served=%v err=%v", served, err)
	}
	if calls := fixture.calls.Load(); calls == 0 {
		t.Fatal("SCOPE FAIL: without a serve-watcher the dispatch MUST hit the live " +
			"apiserver (customer /call path unchanged); got 0 calls")
	}
	if strings.Contains(logBuf.String(), `"msg":"internal_dispatch.list.informer_served"`) {
		t.Fatal("SCOPE FAIL: informer_served fired WITHOUT a serve-watcher — the branch " +
			"must be gated on the ctx watcher (customer path must not serve from informer)")
	}
}

// TestServe1a_CustomerPath_Gate1EarlyReturn_NoInformerServe is the HARD scope
// arm: it proves the informer-serve branch is unreachable on the customer path
// even when a fully-servable watcher is GLOBALLY reachable (cache.Global()).
// This is strictly stronger than NoServeWatcher_ByteIdenticalToPre1a (which
// proves "no watcher in ctx → no serve"): here the watcher IS published as
// cache.Global() and servable, so a branch that read the global (instead of
// ctx) OR that ran before Gate 1 WOULD serve — the arm proves neither happens,
// so the scoping is independent of handle-plumbing. (Grafted from a80cd96; the
// serve branch here reads ServeWatcherFromContext ctx-only, so this pins that
// the ctx-only contract holds AND that Gate 1 short-circuits the customer path.)
func TestServe1a_CustomerPath_Gate1EarlyReturn_NoInformerServe(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	// Publish a fully-servable GVR as cache.Global() — if the branch were
	// customer-reachable it WOULD serve from it; the arm proves it does NOT
	// because Gate 1 short-circuits first. SetGlobal is REQUIRED here (unlike
	// the ctx-watcher arms which attach rw via ctx): without it cache.Global()
	// is nil and the leak-probe RED would be vacuous (a leaked branch reading
	// cache.Global() would find nil and not serve, hiding the leak). With the
	// global published + servable, a leak past Gate 1 WOULD serve → the arm's
	// served=false assertion fires. Verified discriminating: injecting an
	// informer-serve above Gate 1 makes this arm FAIL.
	const n = 8
	items := make([]*unstructured.Unstructured, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, serve1aItem(i, "composition-"+itoa(i), "bench-ns-"+itoa(i)))
	}
	rw := newServe1aWatcher(t, true, items...)
	if !rw.IsServable(serve1aGVR) {
		t.Fatalf("precondition: %s must be servable so the leak-probe RED is non-vacuous", serve1aGVR)
	}
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// CUSTOMER-SHAPED ctx: NO WithInternalRESTConfig (the per-user /call path).
	ctx := withSlogLogger(context.Background(), logger)

	raw, served, err := dispatchViaInternalRESTConfig(ctx, httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{Path: serve1aListPath},
	})
	// Gate 1 early-return: (nil,false,nil).
	if err != nil {
		t.Fatalf("customer-path arm: expected nil err at Gate 1 early-return, got %v", err)
	}
	if served {
		t.Fatal("SCOPE FAIL: dispatch served a customer-shaped ctx (no InternalRESTConfig) — " +
			"the function must early-return at Gate 1 so the informer-serve branch is unreachable")
	}
	if raw != nil {
		t.Fatalf("SCOPE FAIL: expected nil bytes at Gate 1 early-return, got %d bytes", len(raw))
	}
	if strings.Contains(logBuf.String(), `"msg":"internal_dispatch.list.informer_served"`) {
		t.Fatal("SCOPE FAIL: informer_served fired for a customer-shaped ctx — the branch " +
			"leaked past Gate 1 (customer path MUST NOT serve from informer)")
	}
}

// --- helpers --------------------------------------------------------------

type serve1aEnvelope struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		ResourceVersion string `json:"resourceVersion"`
		Continue        string `json:"continue"`
	} `json:"metadata"`
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	} `json:"items"`
}

func (e serve1aEnvelope) itemNames() []string {
	out := make([]string, 0, len(e.Items))
	for _, it := range e.Items {
		out = append(out, it.Metadata.Name)
	}
	return out
}

func decodeListEnvelope(t *testing.T, raw []byte) serve1aEnvelope {
	t.Helper()
	var e serve1aEnvelope
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("served bytes not valid JSON: %v\nraw=%s", err, string(raw))
	}
	return e
}

func serve1aItemsFromRaw(t *testing.T, raw []byte) []string {
	return decodeListEnvelope(t, raw).itemNames()
}

// serve1aLiveAndInformerRaw runs both arms (WithServeWatcher wiring) and returns
// their served LIST bytes: LIVE (no serve-watcher → fixture apiserver) and
// INFORMER (serve-watcher attached → real ListServableEnvelopeJSON). Both use
// the SAME items so the per-item comparison is like-for-like.
func serve1aLiveAndInformerRaw(t *testing.T, n int, items []*unstructured.Unstructured) (liveRaw, infRaw []byte) {
	t.Helper()
	fixture, caPEM := newServe1aLiveFixture(t, n, int(internalDispatchListPageLimit))
	rc := &rest.Config{
		Host:            fixture.server.URL,
		BearerToken:     "fake-sa-jwt",
		TLSClientConfig: rest.TLSClientConfig{CAData: caPEM},
	}
	liveCtx := cache.WithInternalRESTConfig(context.Background(), rc)
	var liveServed bool
	var err error
	liveRaw, liveServed, err = dispatchViaInternalRESTConfig(liveCtx, httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{Path: serve1aListPath},
	})
	if err != nil || !liveServed {
		t.Fatalf("live LIST arm: served=%v err=%v", liveServed, err)
	}
	rw := newServe1aWatcher(t, true, items...)
	infCtx := cache.WithInternalRESTConfig(context.Background(), rc)
	infCtx = cache.WithServeWatcher(infCtx, rw)
	var infServed bool
	infRaw, infServed, err = dispatchViaInternalRESTConfig(infCtx, httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{Path: serve1aListPath},
	})
	if err != nil || !infServed {
		t.Fatalf("informer LIST arm: served=%v err=%v", infServed, err)
	}
	return liveRaw, infRaw
}

// indexContentItemsByName parses the served LIST bytes and projects each item
// to the FULL per-item content — disc-architect's FINAL (A) contract:
//
//	ASSERT EQUAL (kept): kind, apiVersion, metadata.{name, namespace,
//	  resourceVersion (per-item — the object's OWN version, equal for the
//	  static fixture), labels, all annotations EXCEPT last-applied} + spec +
//	  status + any other per-item field (incl an injected __spoof__).
//	EXCLUDE (stripped before compare, both sides): per-item
//	  metadata.managedFields + metadata.annotations[last-applied] (the informer
//	  store's Put-time strip set — the informer-vs-live divergence set, verified
//	  in-code = exactly these two). List-level metadata.resourceVersion +
//	  continue are handled at the envelope level, not here (this is per-item).
//
// FIXTURE-STATIC NOTE (arch): asserting per-item resourceVersion EQUAL holds
// because the fixture objects are static + seeded identically into both arms.
// It is NOT a universal invariant — a real object mutated between the informer's
// last watch event and a fresh live LIST would legitimately differ; a future
// real-object/mutation arm (#124) may relax per-item RV to modulo. Do not let
// #124 inherit "per-item RV always equal."
//
// This is FULL-object-minus-strip-set (not a positive allow-list): it copies
// every per-item field and removes ONLY the two arch-blessed strip fields, so a
// corruption of ANY other field (including one not enumerated here) diverges.
func indexContentItemsByName(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	var env struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("served bytes not valid JSON: %v\nraw=%s", err, string(raw))
	}
	out := make(map[string]string, len(env.Items))
	for _, itRaw := range env.Items {
		var it map[string]any
		if err := json.Unmarshal(itRaw, &it); err != nil {
			t.Fatalf("item not valid JSON: %v", err)
		}
		name := ""
		// Strip ONLY the arch-blessed exclusion set from the WHOLE item
		// (full-object-minus-strip-set), leaving every other field asserted.
		if md, ok := it["metadata"].(map[string]any); ok {
			if s, ok := md["name"].(string); ok {
				name = s
			}
			delete(md, "managedFields") // EXCLUDE: informer strips at Put
			if annos, ok := md["annotations"].(map[string]any); ok {
				delete(annos, "kubectl.kubernetes.io/last-applied-configuration") // EXCLUDE
				if len(annos) == 0 {
					delete(md, "annotations") // informer drops an emptied annotations map
				}
			}
			// per-item resourceVersion is KEPT (asserted equal, fixture-static).
		}
		b, err := json.Marshal(it) // json.Marshal sorts map keys → order-stable
		if err != nil {
			t.Fatalf("re-marshal item %q: %v", name, err)
		}
		out[name] = string(b)
	}
	return out
}

// contentItemsDiff returns "" when the two name→canonical-content maps are
// byte-identical item-for-item, else a human-readable first divergence.
func contentItemsDiff(a, b map[string]string) string {
	if len(a) != len(b) {
		return "item count differs: live=" + itoa(len(a)) + " inf=" + itoa(len(b))
	}
	for name, av := range a {
		bv, ok := b[name]
		if !ok {
			return "item " + name + " present in live, missing in inf"
		}
		if av != bv {
			return "item " + name + " content differs:\n  live=" + av + "\n  inf =" + bv
		}
	}
	return ""
}

// mutateContentDropNamespaceInjectSpoof applies the drop-namespace + spoof probe
// to the per-item canonical content (the RBAC RED arm).
func mutateContentDropNamespaceInjectSpoof(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for name, raw := range in {
		var obj map[string]any
		_ = json.Unmarshal([]byte(raw), &obj)
		if md, ok := obj["metadata"].(map[string]any); ok {
			delete(md, "namespace")
		}
		obj["__spoof__"] = "spoofed"
		b, _ := json.Marshal(obj)
		out[name] = string(b)
	}
	return out
}

// mutateContentSpecStatus mutates a per-item spec + status field (the CONTENT-
// FIDELITY RED arm — the dimension a name+namespace-only golden was blind to;
// the widget JQ renders spec/status, so a serve path corrupting them is a real
// correctness bug the golden must catch). metadata untouched.
func mutateContentSpecStatus(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for name, raw := range in {
		var obj map[string]any
		_ = json.Unmarshal([]byte(raw), &obj)
		if sp, ok := obj["spec"].(map[string]any); ok {
			sp["replicas"] = int64(9999)
		}
		if st, ok := obj["status"].(map[string]any); ok {
			st["phase"] = "Corrupted"
		}
		b, _ := json.Marshal(obj)
		out[name] = string(b)
	}
	return out
}

// mutateContentPerItemRV mutates each item's metadata.resourceVersion (the arm
// #4 probe): proves per-item RV is ASSERTED EQUAL (arch FINAL), not excluded —
// the golden must diverge on a per-item RV change.
func mutateContentPerItemRV(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for name, raw := range in {
		var obj map[string]any
		_ = json.Unmarshal([]byte(raw), &obj)
		if md, ok := obj["metadata"].(map[string]any); ok {
			md["resourceVersion"] = "999999" // a value neither arm emits
		}
		b, _ := json.Marshal(obj)
		out[name] = string(b)
	}
	return out
}

// mutateContentUID corrupts each item's metadata.uid — PRESENT-ONLY (guarded)
// so if the projection ever dropped uid this becomes a no-op and the RED arm
// correctly FAILS (proving uid must be IN the compared set). uid is immutable +
// consumer-read (compositions-panel row-table renders .metadata.uid).
func mutateContentUID(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for name, raw := range in {
		var obj map[string]any
		_ = json.Unmarshal([]byte(raw), &obj)
		if md, ok := obj["metadata"].(map[string]any); ok {
			if _, has := md["uid"]; has {
				md["uid"] = "uid-CORRUPTED"
			}
		}
		b, _ := json.Marshal(obj)
		out[name] = string(b)
	}
	return out
}

// mutateContentCreationTimestamp corrupts each item's metadata.creationTimestamp
// — PRESENT-ONLY guarded (no-op if the projection dropped it → RED arm FAILS).
// Immutable + consumer-read (compositions-panels RA sort_by(.metadata.creationTimestamp)).
func mutateContentCreationTimestamp(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for name, raw := range in {
		var obj map[string]any
		_ = json.Unmarshal([]byte(raw), &obj)
		if md, ok := obj["metadata"].(map[string]any); ok {
			if _, has := md["creationTimestamp"]; has {
				md["creationTimestamp"] = "1999-01-01T00:00:00Z"
			}
		}
		b, _ := json.Marshal(obj)
		out[name] = string(b)
	}
	return out
}

// serve1aRawPerItemRVDiffers reports whether the two served envelopes carry at
// least one item whose metadata.resourceVersion differs (matched by name).
// Retained as a fixture-sanity helper.
func serve1aRawPerItemRVDiffers(t *testing.T, aRaw, bRaw []byte) bool {
	t.Helper()
	rv := func(raw []byte) map[string]string {
		var env struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("served bytes not valid JSON: %v", err)
		}
		out := map[string]string{}
		for _, it := range env.Items {
			md, _ := it["metadata"].(map[string]any)
			if md == nil {
				continue
			}
			nm, _ := md["name"].(string)
			r, _ := md["resourceVersion"].(string)
			out[nm] = r
		}
		return out
	}
	a, b := rv(aRaw), rv(bRaw)
	for name, ar := range a {
		if br, ok := b[name]; ok && br != ar {
			return true
		}
	}
	return false
}

// itoa is a tiny local int->string (avoid importing strconv just for this).
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
