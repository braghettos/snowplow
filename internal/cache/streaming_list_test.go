// streaming_list_test.go — Ship 0.30.122 R4 Lever 1 hermetic falsifier,
// updated for Ship H2a (the LIST-decode re-design).
//
// The streaming ListFunc MUST produce a *bytesObjectList equivalent to
// the standard dynamic-client path for the same apiserver input — same
// items, same identity, same post-strip content (Diego's hard
// constraint: spec + status intact). Since H2a, the list's Items are
// *bytesObject values; the equivalence assertions decode each
// bytesObject (the field-fidelity path) and compare. These tests drive
// streamingList against an httptest apiserver stub serving a paged LIST
// fixture; no live cluster.

package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// r4CompositionGVR is the streaming-list fixture GVR — a composition
// group resource so matchesStreamingListGroup returns true.
var r4CompositionGVR = schema.GroupVersionResource{
	Group:    "composition.krateo.io",
	Version:  "v1",
	Resource: "githubscaffoldingwithcompositionpages",
}

// r4CompositionItem builds a fixture composition object with a non-empty
// spec AND status (the bytes R4 must preserve) plus the two bookkeeping
// fields defaultStripUnstructured drops (managedFields + last-applied).
func r4CompositionItem(ns, name string) map[string]any {
	return map[string]any{
		"apiVersion": "composition.krateo.io/v1",
		"kind":       "GithubScaffoldingWithCompositionPages",
		"metadata": map[string]any{
			"namespace": ns,
			"name":      name,
			"annotations": map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": `{"big":"blob"}`,
				"krateo.io/external-create-succeeded":              "2026-05-16T01:42:36Z",
			},
			"managedFields": []any{
				map[string]any{"manager": "controller", "operation": "Update"},
			},
		},
		"spec": map[string]any{
			"repoName": name + "-repo",
			"org":      "krateo",
		},
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True"},
			},
			"digest": name + "-digest",
		},
	}
}

// r4ListFixtureServer is an httptest apiserver stub that serves the
// composition collection LIST in `pages` pages of `perPage` items each,
// honouring the `continue` query parameter exactly like a real
// apiserver. totalItems items are served across the pages.
func r4ListFixtureServer(t *testing.T, totalItems, perPage int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Page offset comes from the continue token (we encode it as a
		// plain integer-string; a real apiserver uses an opaque token but
		// the streaming walk only round-trips it).
		offset := 0
		if c := r.URL.Query().Get("continue"); c != "" {
			fmt.Sscanf(c, "%d", &offset)
		}
		end := offset + perPage
		if end > totalItems {
			end = totalItems
		}
		items := make([]any, 0, end-offset)
		for i := offset; i < end; i++ {
			items = append(items, r4CompositionItem("bench-ns", fmt.Sprintf("comp-%04d", i)))
		}
		meta := map[string]any{"resourceVersion": "12345"}
		if end < totalItems {
			meta["continue"] = fmt.Sprintf("%d", end)
		}
		envelope := map[string]any{
			"apiVersion": "composition.krateo.io/v1",
			"kind":       "GithubScaffoldingWithCompositionPagesList",
			"metadata":   meta,
			"items":      items,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(envelope)
	}))
}

// --- Test 1 — streaming LIST equivalence + strip correctness --------------

// TestR4_StreamingListEquivalentToStandard drives streamingList across a
// multi-page fixture and asserts: every item is returned, in order, with
// the bookkeeping fields stripped and — critically — spec + status fully
// intact (Diego's hard constraint).
func TestR4_StreamingListEquivalentToStandard(t *testing.T) {
	const totalItems = 250
	// perPage is an explicit, independent fixture value — NOT coupled to
	// the production listPageLimit. 250 items / 100-per-page exercises a
	// genuine 3-page continue-walk (100 + 100 + 50).
	const perPage = 100

	srv := r4ListFixtureServer(t, totalItems, perPage)
	defer srv.Close()

	rc, err := streamingRESTClient(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("streamingRESTClient: %v", err)
	}

	got, err := streamingList(context.Background(), rc, r4CompositionGVR,
		metav1.ListOptions{Limit: listPageLimit})
	if err != nil {
		t.Fatalf("streamingList: %v", err)
	}

	// All items returned, paged correctly across 3 pages (100+100+50).
	if len(got.Items) != totalItems {
		t.Fatalf("streamingList returned %d items, want %d (paged continue-walk "+
			"must traverse every page)", len(got.Items), totalItems)
	}
	// Envelope identity carried onto the result list. bytesObjectList
	// embeds TypeMeta (APIVersion plain field) + ListMeta (RV accessor).
	if got.APIVersion != "composition.krateo.io/v1" {
		t.Fatalf("result apiVersion=%q, want composition.krateo.io/v1", got.APIVersion)
	}
	if got.GetResourceVersion() != "12345" {
		t.Fatalf("result resourceVersion=%q, want 12345", got.GetResourceVersion())
	}

	for i := range got.Items {
		wantName := fmt.Sprintf("comp-%04d", i)

		// H2a — each item is a *bytesObject. The embedded ObjectMeta is
		// the indexer-key surface; Decode() is the field-fidelity path.
		bo, ok := got.Items[i].(*bytesObject)
		if !ok {
			t.Fatalf("item %d is %T, want *bytesObject (H2a streaming path)", i, got.Items[i])
		}
		if bo.GetName() != wantName {
			t.Fatalf("item %d name=%q, want %q (streaming decode must preserve order)",
				i, bo.GetName(), wantName)
		}

		// STRIP (AC-H2a.2) — verified on the DECODED object: the two
		// bookkeeping fields must be gone from `raw`.
		item, derr := bo.Decode()
		if derr != nil {
			t.Fatalf("item %d Decode: %v", i, derr)
		}
		if mf := item.GetManagedFields(); len(mf) != 0 {
			t.Fatalf("item %d still carries %d managedFields entries — strip not applied",
				i, len(mf))
		}
		annos := item.GetAnnotations()
		if _, present := annos[lastAppliedAnnotation]; present {
			t.Fatalf("item %d still carries the last-applied annotation — strip not applied", i)
		}
		// A non-bookkeeping annotation survives.
		if annos["krateo.io/external-create-succeeded"] == "" {
			t.Fatalf("item %d: strip dropped a non-bookkeeping annotation", i)
		}

		// HARD CONSTRAINT — spec + status intact (decoded from `raw`).
		spec, specOK := item.Object["spec"].(map[string]any)
		if !specOK || spec["repoName"] != wantName+"-repo" || spec["org"] != "krateo" {
			t.Fatalf("item %d: spec corrupted/missing after strip — want repoName=%q org=krateo, got %v",
				i, wantName+"-repo", item.Object["spec"])
		}
		status, statusOK := item.Object["status"].(map[string]any)
		if !statusOK || status["digest"] != wantName+"-digest" {
			t.Fatalf("item %d: status corrupted/missing after strip — want digest=%q, got %v",
				i, wantName+"-digest", item.Object["status"])
		}
		conds, _ := status["conditions"].([]any)
		if len(conds) != 1 {
			t.Fatalf("item %d: status.conditions lost after strip — got %v", i, status["conditions"])
		}
	}
}

// --- Test 2 — single-page (continue empty on page 1) ----------------------

// TestR4_StreamingListSinglePage exercises the no-continue path: the
// fixture fits in one page so the walk must terminate after exactly one
// request.
func TestR4_StreamingListSinglePage(t *testing.T) {
	srv := r4ListFixtureServer(t, 10, 100)
	defer srv.Close()

	rc, err := streamingRESTClient(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("streamingRESTClient: %v", err)
	}
	got, err := streamingList(context.Background(), rc, r4CompositionGVR,
		metav1.ListOptions{Limit: listPageLimit})
	if err != nil {
		t.Fatalf("streamingList: %v", err)
	}
	if len(got.Items) != 10 {
		t.Fatalf("single-page streamingList returned %d items, want 10", len(got.Items))
	}
}

// --- Test 3 — empty collection --------------------------------------------

// TestR4_StreamingListEmpty asserts a zero-item collection streams to an
// empty (non-nil) list with no error — a genuine "no compositions"
// answer the informer can vouch for.
func TestR4_StreamingListEmpty(t *testing.T) {
	srv := r4ListFixtureServer(t, 0, 100)
	defer srv.Close()

	rc, err := streamingRESTClient(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("streamingRESTClient: %v", err)
	}
	got, err := streamingList(context.Background(), rc, r4CompositionGVR,
		metav1.ListOptions{Limit: listPageLimit})
	if err != nil {
		t.Fatalf("streamingList empty: %v", err)
	}
	if got == nil {
		t.Fatalf("streamingList returned nil list for an empty collection")
	}
	if len(got.Items) != 0 {
		t.Fatalf("empty collection streamed %d items, want 0", len(got.Items))
	}
}

// --- Test 4 — streaming routing predicate (updated for Ship H5) -----------

// TestR4_StreamingListGroupRouting asserts the routing predicate. Pre-H5
// this checked a positive allow-list (composition matches, others do
// not). H5 inverted routing: streaming is the default, and the predicate
// is isStreamingException — true ONLY for the typed-RBAC GVRs. So the
// composition group AND an arbitrary other group both stream (neither is
// an exception); the typed-RBAC GVRs do not.
func TestR4_StreamingListGroupRouting(t *testing.T) {
	if isStreamingException(r4CompositionGVR) {
		t.Fatalf("composition GVR must NOT be a streaming exception — it streams by default")
	}
	other := schema.GroupVersionResource{Group: "widgets.krateo.io", Version: "v1", Resource: "panels"}
	if isStreamingException(other) {
		t.Fatalf("an arbitrary GVR must NOT be a streaming exception — H5: streaming is the default")
	}
	// The typed-RBAC GVRs ARE the exception.
	for _, gvr := range rbacTypedGVRs {
		if !isStreamingException(gvr) {
			t.Fatalf("typed-RBAC GVR %q must be a streaming exception", gvr)
		}
	}
}

// --- Test 5 — flag default + toggle ---------------------------------------

// TestR4_StreamingListFlagDefault asserts RESOLVER_COMPOSITION_STREAMING_LIST
// defaults ON and the AC-7 toggle to "false" disables it.
func TestR4_StreamingListFlagDefault(t *testing.T) {
	if !compositionStreamingListEnabled() {
		t.Fatalf("RESOLVER_COMPOSITION_STREAMING_LIST must default ON")
	}
	t.Setenv(envCompositionStreamingList, "false")
	if compositionStreamingListEnabled() {
		t.Fatalf("RESOLVER_COMPOSITION_STREAMING_LIST=false must disable the streaming path (AC-7)")
	}
	t.Setenv(envCompositionStreamingList, "true")
	if !compositionStreamingListEnabled() {
		t.Fatalf("RESOLVER_COMPOSITION_STREAMING_LIST=true must enable the streaming path")
	}
}

// --- Test 6 — nil rest.Config falls back ----------------------------------

// TestR4_StreamingInformerNilConfigFallback asserts newStreamingDynamicInformer
// returns ok=false when no *rest.Config is wired — the caller then falls
// back to the standard NewFilteredDynamicInformer.
func TestR4_StreamingInformerNilConfigFallback(t *testing.T) {
	_, ok := newStreamingDynamicInformer(nil, nil, r4CompositionGVR, nil, listOptionsTweak)
	if ok {
		t.Fatalf("newStreamingDynamicInformer with nil *rest.Config must return ok=false "+
			"so the caller falls back to the standard informer")
	}
}

// --- Test 7 (BLOCKER 1 regression) — resourceVersion is first-page's ------

// TestR4_StreamingListResourceVersionIsFirstPage is the B1 regression
// test. It serves a fixture whose page 1 reports resourceVersion "100"
// and page 2 reports a DIFFERENT "999". For a paged collection LIST the
// apiserver pins a snapshot RV at the FIRST page; the result list's RV
// MUST be page 1's "100". The pre-fix code did last-write-wins
// (SetResourceVersion on every page) and would end with "999" — the
// wrong RV, which would make the informer's subsequent WATCH start from
// a bad RV and silently drift. This test FAILS on the pre-fix code.
func TestR4_StreamingListResourceVersionIsFirstPage(t *testing.T) {
	const totalItems = 150
	const perPage = 100 // page 1 = items 0..99, page 2 = items 100..149

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := 0
		if c := r.URL.Query().Get("continue"); c != "" {
			fmt.Sscanf(c, "%d", &offset)
		}
		end := offset + perPage
		if end > totalItems {
			end = totalItems
		}
		items := make([]any, 0, end-offset)
		for i := offset; i < end; i++ {
			items = append(items, r4CompositionItem("bench-ns", fmt.Sprintf("comp-%04d", i)))
		}
		// THE B1 TRAP: page 1 reports RV "100", every later page "999".
		rv := "999"
		if offset == 0 {
			rv = "100"
		}
		meta := map[string]any{"resourceVersion": rv}
		if end < totalItems {
			meta["continue"] = fmt.Sprintf("%d", end)
		}
		envelope := map[string]any{
			"apiVersion": "composition.krateo.io/v1",
			"kind":       "GithubScaffoldingWithCompositionPagesList",
			"metadata":   meta,
			"items":      items,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	defer srv.Close()

	rc, err := streamingRESTClient(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("streamingRESTClient: %v", err)
	}
	got, err := streamingList(context.Background(), rc, r4CompositionGVR,
		metav1.ListOptions{Limit: listPageLimit})
	if err != nil {
		t.Fatalf("streamingList: %v", err)
	}
	if got.GetResourceVersion() != "100" {
		t.Fatalf("B1 regression: result resourceVersion=%q, want page 1's %q. "+
			"The paged LIST's RV must be the FIRST page's snapshot RV — "+
			"last-write-wins would leave the wrong RV and drift the informer WATCH.",
			got.GetResourceVersion(), "100")
	}
}

// --- Test 8 (BLOCKER 2 regression) — Status envelope mid-walk errors ------

// TestR4_StreamingListStatusEnvelopeFailsWholeList is the B2 regression
// test. The fixture serves a valid LIST page 1 (with a continue token),
// then a metav1 `kind: Status` envelope on page 2 — exactly what an
// expired-continue-token 410 Gone looks like if it ever arrives with a
// 200 status (a proxy, a watch-cache edge). The pre-fix code parsed the
// Status envelope, found no `items`, left continueToken=="", broke the
// page loop, and returned (out, nil) — a TRUNCATED list served as
// authoritative (the S4 trap). The fix verifies kind != "Status" and
// fails the WHOLE list. This test FAILS on the pre-fix code (which
// returned a nil error + a partial 100-item list).
func TestR4_StreamingListStatusEnvelopeFailsWholeList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("continue") == "" {
			// Page 1 — a valid LIST page WITH a continue token, so the
			// walk proceeds to page 2.
			items := make([]any, 0, 100)
			for i := 0; i < 100; i++ {
				items = append(items, r4CompositionItem("bench-ns", fmt.Sprintf("comp-%04d", i)))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apiVersion": "composition.krateo.io/v1",
				"kind":       "GithubScaffoldingWithCompositionPagesList",
				"metadata":   map[string]any{"resourceVersion": "100", "continue": "100"},
				"items":      items,
			})
			return
		}
		// Page 2 — a Status error envelope (what an expired continue
		// token / 410 Gone decodes to). No `items` field.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "v1",
			"kind":       "Status",
			"status":     "Failure",
			"message":    "The provided continue parameter is too old to display a consistent list result",
			"reason":     "Expired",
			"code":       410,
		})
	}))
	defer srv.Close()

	rc, err := streamingRESTClient(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("streamingRESTClient: %v", err)
	}
	got, err := streamingList(context.Background(), rc, r4CompositionGVR,
		metav1.ListOptions{Limit: listPageLimit})
	if err == nil {
		gotItems := 0
		if got != nil {
			gotItems = len(got.Items)
		}
		t.Fatalf("B2 regression: streamingList returned a nil error with %d items after "+
			"a Status envelope on page 2. A Status mid-walk MUST fail the WHOLE list — "+
			"returning a truncated list as success is the S4 'partial looks complete' trap.",
			gotItems)
	}
	if got != nil {
		t.Fatalf("B2 regression: streamingList returned a non-nil list alongside the "+
			"error — a failed list must return (nil, err), never a partial list")
	}
}
