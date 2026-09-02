// internal_dispatch_paged_test.go — 0.30.250 / Task #268 falsifier.
//
// Reproduces the Task #267 silent-widget failure mode (TRACED in
// docs/task-267-s6-admin-2-widget-silent-skip-trace-2026-06-09.md):
// at 50K composition CRs the pre-0.30.250 single-LIST call took 2.9-162 s
// and the browser cancelled the in-flight body read.
//
// The post-0.30.250 dispatcher is the continue-token paged walk
// (internalDispatchListPageLimit = 500 per page). This test stands up a
// httptest TLS server that answers a multi-page LIST — exactly the
// apiserver-paged-LIST wire shape — and asserts:
//
//   - (FALSIFIER-A) the dispatcher issues multiple paged requests (not
//     ONE call), each carrying opts.Limit and the prior page's
//     `continue` token;
//   - (FALSIFIER-B) the served bytes carry EVERY item across pages — no
//     accidental truncation when a later page has a different envelope
//     shape, no last-write-wins on resourceVersion;
//   - (FALSIFIER-C) the served envelope is byte-equivalent shape to the
//     pre-0.30.250 unpaginated path: `apiVersion`, `kind`, `metadata.*`,
//     `items: [...]` — and `metadata.continue` is EMPTY (the paged walk
//     accumulates pages; the served list is a fully-materialised list);
//   - (FALSIFIER-D) the WARN-level slog falsifier event fires with the
//     expected page_count / item_count;
//   - (FALSIFIER-E) the `internalDispatchListPageLimit` constant is
//     pinned at 500 — a future bench-driven retune is a single-site
//     change; this guards against silent edits.
//
// CRITICAL — like the 0.30.104 TLS test, this is a unit test against a
// httptest server with a synthetic CA. It exercises the paging contract
// at the client-go transport level but cannot exercise the real
// apiserver's continue-token behaviour. The on-cluster smoke check in
// the ship sequence is necessary.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	httpcall "github.com/krateo-platformops/plumbing/http/request"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"k8s.io/client-go/rest"
)

// pagedListFixture is the apiserver-paged-LIST test rig. It answers
// /apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages
// with N items spread across pages of `pageSize`. Each page emits a
// `metadata.continue` token equal to the next page's index (as a string);
// the last page emits an empty `continue`. This is the exact wire
// contract client-go's paged dynamic.List drives.
type pagedListFixture struct {
	server *httptest.Server
	pages  []int // index → number of items on that page
	calls  atomic.Int64

	// onFirstRequest, when non-nil, deterministically synchronises the
	// ctx-cancel test (Task #85). The handler invokes it ONCE — after it
	// has received the page-0 request but BEFORE it writes the response
	// body. The hook is expected to cancel the dispatch context and then
	// release the returned channel; the handler blocks on that channel
	// before responding, so the cancel is guaranteed to land WHILE the
	// page-0 round-trip is in flight (client-go then aborts the body read
	// with a `context canceled` error). nil for every other test, which
	// keeps their fast unsynchronised behaviour unchanged.
	onFirstRequest func() <-chan struct{}
}

// newPagedListFixture builds a TLS server that answers a fixed N-item
// LIST split into pages of pageSize.
func newPagedListFixture(t *testing.T, totalItems int, pageSize int) (*pagedListFixture, []byte) {
	t.Helper()
	if pageSize <= 0 {
		t.Fatalf("newPagedListFixture: pageSize must be > 0, got %d", pageSize)
	}
	fixture := &pagedListFixture{}

	// Pre-compute the page splits.
	rem := totalItems
	for rem > 0 {
		n := pageSize
		if n > rem {
			n = rem
		}
		fixture.pages = append(fixture.pages, n)
		rem -= n
	}
	if totalItems == 0 {
		fixture.pages = []int{0}
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.calls.Add(1)

		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		cont := q.Get("continue")

		// FALSIFIER-A1 — every request from the paged dispatcher MUST
		// carry a `limit` query parameter. The pre-0.30.250 code did
		// nri.List(ctx, ListOptions{}) with no Limit; client-go encodes
		// an empty Limit as the absence of the query param. The
		// post-0.30.250 dispatcher MUST set opts.Limit, which client-go
		// encodes as `?limit=500`.
		if limit != int(internalDispatchListPageLimit) {
			http.Error(w, fmt.Sprintf("FALSIFIER-A1 FAIL: expected ?limit=%d "+
				"(internalDispatchListPageLimit), got limit=%d (raw=%q). The "+
				"pre-0.30.250 unpaginated LIST omitted the limit query — this "+
				"server fails it. server-side: %s",
				internalDispatchListPageLimit, limit, q.Get("limit"), r.URL.String()),
				http.StatusBadRequest)
			return
		}

		// Determine which page index `continue` refers to. Empty
		// `continue` => page 0; otherwise the value IS the page index
		// (this fixture's convention).
		pageIdx := 0
		if cont != "" {
			n, err := strconv.Atoi(cont)
			if err != nil {
				http.Error(w, fmt.Sprintf("FALSIFIER-A2 FAIL: continue token "+
					"is not a fixture page index: %v", err), http.StatusBadRequest)
				return
			}
			pageIdx = n
		}

		if pageIdx >= len(fixture.pages) {
			http.Error(w, fmt.Sprintf("FALSIFIER-A3 FAIL: continue token %q "+
				"asked for page %d, fixture only has %d pages", cont, pageIdx,
				len(fixture.pages)), http.StatusBadRequest)
			return
		}

		// FALSIFIER-A4 — subsequent pages MUST NOT carry a
		// resourceVersion (pager.ListPager line 160-164; apiserver
		// returns "specifying resource version is not allowed when
		// using continue"). Our dispatcher clears opts.ResourceVersion
		// AND opts.ResourceVersionMatch on subsequent pages. Verify.
		if pageIdx > 0 {
			if rv := q.Get("resourceVersion"); rv != "" {
				http.Error(w, fmt.Sprintf("FALSIFIER-A4 FAIL: subsequent "+
					"page %d carries resourceVersion=%q — must be cleared "+
					"alongside the continue token", pageIdx, rv),
					http.StatusBadRequest)
				return
			}
			if rvm := q.Get("resourceVersionMatch"); rvm != "" {
				http.Error(w, fmt.Sprintf("FALSIFIER-A4 FAIL: subsequent "+
					"page %d carries resourceVersionMatch=%q — must be cleared",
					pageIdx, rvm), http.StatusBadRequest)
				return
			}
		}

		// Build this page's items. Items are numbered globally so the
		// FALSIFIER-B assertion can verify total-count and uniqueness.
		offset := 0
		for i := 0; i < pageIdx; i++ {
			offset += fixture.pages[i]
		}
		var itemsBuf bytes.Buffer
		for i := 0; i < fixture.pages[pageIdx]; i++ {
			if i > 0 {
				itemsBuf.WriteByte(',')
			}
			fmt.Fprintf(&itemsBuf, `{"apiVersion":"composition.krateo.io/v1-2-2",`+
				`"kind":"GitHubScaffoldingWithCompositionPages",`+
				`"metadata":{"name":"composition-%d","namespace":"krateo-system"}}`,
				offset+i)
		}

		nextContinue := ""
		if pageIdx+1 < len(fixture.pages) {
			nextContinue = strconv.Itoa(pageIdx + 1)
		}

		// Task #85 — deterministic ctx-cancel synchronisation. On the
		// FIRST (page-0) request, hand control to the test's hook which
		// cancels the dispatch context, then block on the channel it
		// returns before writing the body. By the time we write, the
		// client request context is cancelled, so client-go aborts and
		// nri.List(page 0) returns a `context canceled` error — landing
		// the cancel at a DEFINED point (mid-page-0) with no wall-clock
		// guard. Guard with `cont == ""` so it fires exactly once (page 0).
		if fixture.onFirstRequest != nil && cont == "" {
			<-fixture.onFirstRequest()
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w,
			`{"apiVersion":"composition.krateo.io/v1-2-2",`+
				`"kind":"GitHubScaffoldingWithCompositionPagesList",`+
				`"metadata":{"resourceVersion":"%d","continue":"%s"},`+
				`"items":[%s]}`,
			1000+pageIdx, nextContinue, itemsBuf.String())
	}))
	t.Cleanup(srv.Close)

	caPEM := pemEncodeCert(srv.Certificate())
	fixture.server = srv
	return fixture, caPEM
}

// TestInternalRESTConfigDispatch_PagedList_FullWalk is the
// FALSIFIER-A/B/C/E falsifier for Task #268. It stands up a 25-page
// LIST (5 pages × 5000 items at the listPageLimit of 500 — actually
// 25 × 500 = 12500 items so the server responds in ≤ a few hundred ms
// in CI; we choose item count to exercise multiple pages without
// blowing CI memory).
func TestInternalRESTConfigDispatch_PagedList_FullWalk(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	const totalItems = 1250 // 1250 / 500 = 2 full pages + 1 short page (250 items)
	const expectedPages = 3
	pageSize := int(internalDispatchListPageLimit)
	fixture, caPEM := newPagedListFixture(t, totalItems, pageSize)

	rc := &rest.Config{
		Host:        fixture.server.URL,
		BearerToken: "fake-sa-jwt",
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caPEM,
		},
	}

	// Capture the WARN-level slog falsifier event by attaching a
	// custom slog.Handler to the request context.
	var logBuf bytes.Buffer
	// LevelDebug: internal_dispatch.paged_list.completed is a DEBUG event
	// (#170) — capture it so the FALSIFIER-D assertion below still verifies
	// the paged path fired (the level dropped WARN→DEBUG, the event did not).
	logHandler := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(logHandler)

	ctx := cache.WithInternalRESTConfig(context.Background(), rc)
	ctx = withSlogLogger(ctx, logger)

	start := time.Now()
	raw, served, err := dispatchViaInternalRESTConfig(ctx, httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{
			Path: "/apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages",
		},
	})
	dur := time.Since(start)
	if err != nil {
		t.Fatalf("PAGED-LIST BROKEN: dispatch returned error: %v", err)
	}
	if !served {
		t.Fatal("PAGED-LIST BROKEN: expected served=true for the apiserver-shaped LIST path")
	}

	// FALSIFIER-A — server-side call count MUST be expectedPages, not 1.
	// The pre-0.30.250 code would have made a SINGLE unpaginated call;
	// if a future revert reintroduces nri.List(ctx, ListOptions{}) this
	// asserts a fail because the server's FALSIFIER-A1 limit check would
	// reject the unpaginated call AND because calls would be 1, not 3.
	gotCalls := fixture.calls.Load()
	if gotCalls != int64(expectedPages) {
		t.Fatalf("FALSIFIER-A FAIL: expected %d paged calls (continue-token "+
			"walk), got %d calls. The pre-0.30.250 unpaginated path makes 1 "+
			"call; this fix should drive %d paged round-trips.",
			expectedPages, gotCalls, expectedPages)
	}

	// FALSIFIER-B — every item across every page MUST be present in the
	// served envelope, with no duplication.
	var envelope struct {
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
	if uErr := json.Unmarshal(raw, &envelope); uErr != nil {
		t.Fatalf("FALSIFIER-C FAIL: served bytes are not valid JSON: %v", uErr)
	}
	if len(envelope.Items) != totalItems {
		t.Fatalf("FALSIFIER-B FAIL: served list carries %d items, expected %d. "+
			"The paged walk lost items across pages — check the for-loop "+
			"accumulation into resultList.Items.",
			len(envelope.Items), totalItems)
	}
	seen := make(map[string]struct{}, totalItems)
	for _, it := range envelope.Items {
		if _, dup := seen[it.Metadata.Name]; dup {
			t.Fatalf("FALSIFIER-B FAIL: duplicate item in served list: %q. "+
				"The paged walk is double-appending — check that page.Items "+
				"is consumed once per page.", it.Metadata.Name)
		}
		seen[it.Metadata.Name] = struct{}{}
	}
	for i := 0; i < totalItems; i++ {
		name := fmt.Sprintf("composition-%d", i)
		if _, ok := seen[name]; !ok {
			t.Fatalf("FALSIFIER-B FAIL: served list is missing item %q. "+
				"The paged walk dropped an item — check page-boundary "+
				"accumulation.", name)
		}
	}

	// FALSIFIER-C — envelope shape MUST be apiserver LIST. apiVersion +
	// kind from the GVR; resourceVersion from PAGE 1's metadata (NOT
	// last-write-wins). continue MUST be empty (we've accumulated all
	// pages; the served list is fully materialised).
	if envelope.APIVersion != "composition.krateo.io/v1-2-2" {
		t.Fatalf("FALSIFIER-C FAIL: served apiVersion=%q, expected the "+
			"GVR's apiVersion from page 1", envelope.APIVersion)
	}
	if envelope.Kind != "GitHubScaffoldingWithCompositionPagesList" {
		t.Fatalf("FALSIFIER-C FAIL: served kind=%q, expected the GVR's "+
			"list kind from page 1", envelope.Kind)
	}
	if envelope.Metadata.ResourceVersion != "1000" {
		t.Fatalf("FALSIFIER-C FAIL: served resourceVersion=%q, expected "+
			"\"1000\" (page 1's RV). A last-write-wins implementation "+
			"would report page %d's RV instead.",
			envelope.Metadata.ResourceVersion, expectedPages-1)
	}
	if envelope.Metadata.Continue != "" {
		// page 1 emits continue="1"; if we accidentally copy it onto
		// resultList.Object then the served envelope would also have
		// continue="1" — the JQ pipeline would misinterpret the list
		// as partial. Verify we omit it.
		t.Fatalf("FALSIFIER-C FAIL: served metadata.continue=%q is "+
			"non-empty. The paged walk should accumulate pages and emit "+
			"a fully-materialised list (continue=\"\"). Page 1's "+
			"continue token leaked into the served envelope.",
			envelope.Metadata.Continue)
	}

	// FALSIFIER-D — the DEBUG-level slog falsifier event MUST fire ONCE,
	// carrying the expected pages / items / page_limit / total_ms.
	logOut := logBuf.String()
	if !strings.Contains(logOut, `"msg":"internal_dispatch.paged_list.completed"`) {
		t.Fatalf("FALSIFIER-D FAIL: the DEBUG-level falsifier event "+
			"`internal_dispatch.paged_list.completed` did NOT fire. "+
			"A future code edit that drops the slog.Debug call would "+
			"silently regress the paged path's observability. Log "+
			"buffer:\n%s", logOut)
	}
	if got := strings.Count(logOut, `"msg":"internal_dispatch.paged_list.completed"`); got != 1 {
		t.Fatalf("FALSIFIER-D FAIL: the falsifier event fired %d times, "+
			"expected exactly 1 per served LIST. Log buffer:\n%s",
			got, logOut)
	}
	wantPagesField := fmt.Sprintf(`"pages":%d`, expectedPages)
	if !strings.Contains(logOut, wantPagesField) {
		t.Fatalf("FALSIFIER-D FAIL: the falsifier event omits %q. "+
			"Log buffer:\n%s", wantPagesField, logOut)
	}
	wantItemsField := fmt.Sprintf(`"items":%d`, totalItems)
	if !strings.Contains(logOut, wantItemsField) {
		t.Fatalf("FALSIFIER-D FAIL: the falsifier event omits %q. "+
			"Log buffer:\n%s", wantItemsField, logOut)
	}
	wantLimitField := fmt.Sprintf(`"page_limit":%d`, internalDispatchListPageLimit)
	if !strings.Contains(logOut, wantLimitField) {
		t.Fatalf("FALSIFIER-D FAIL: the falsifier event omits %q. "+
			"Log buffer:\n%s", wantLimitField, logOut)
	}

	t.Logf("paged-LIST falsifier passed — %d pages × %d items in %s "+
		"(server-side calls=%d). Falsifier event:\n%s",
		expectedPages, totalItems, dur, gotCalls, logOut)
}

// TestInternalDispatchListPageLimit_PinnedAt500 is FALSIFIER-E — a
// silent edit of the page-limit constant would change wire behaviour
// (round-trip count, per-page memory) and bench replays. Pin the value.
func TestInternalDispatchListPageLimit_PinnedAt500(t *testing.T) {
	if internalDispatchListPageLimit != 500 {
		t.Fatalf("FALSIFIER-E FAIL: internalDispatchListPageLimit=%d, "+
			"expected 500 (matches client-go defaultPageSize AND snowplow's "+
			"global listPageLimit in internal/cache/watcher.go:24-28). "+
			"A retune is intentional and requires updating this test AND "+
			"the bench's paging-aware assertions in the task-268 trace doc.",
			internalDispatchListPageLimit)
	}
}

// TestInternalRESTConfigDispatch_PagedList_ContextCancelDuringWalk
// proves the paged walk respects ctx cancellation mid-walk — the
// behaviour that closes the Task #267 failure mode (the browser
// cancelling mid-LIST should surface a clean `context canceled` error,
// not a hung dispatcher, and not a falsely-complete list).
//
// CONTRACT (TRACED — internal_dispatch.go:345-354 + the client-go
// transport): the paged walk has TWO cancel-observation points —
//   (1) the explicit `select { case <-ctx.Done(): return ctx.Err() }` at
//       the TOP of every loop iteration (between pages); and
//   (2) the in-flight nri.List(ctx, opts) call itself, which client-go
//       aborts when ctx cancels mid-request, returning an error whose
//       chain contains "context canceled" (typically a *url.Error).
// Either way the surfaced error chain contains "context canceled" and the
// walk does NOT complete all pages. The dispatcher returns (nil, false,
// err); resolve.go surfaces it as an HTTP 500 with the real error rather
// than hanging — exactly the Task #267 closure.
//
// Task #85 — DETERMINISTIC SYNCHRONISATION (no wall-clock guard). The
// pre-0.30.252 version slept 50 ms then cancelled, racing the walk: on a
// fast machine the whole 5-page walk finished in ~15-19 ms BEFORE the
// 50 ms cancel fired, so the walk returned nil and the test failed
// deterministically; under -race the instrumentation slowed the walk past
// 50 ms so the cancel sometimes landed mid-walk — a flaky 50 ms timing
// variant. We now drive the cancel off a fixture hook: the server hands
// control to us AFTER receiving the page-0 request but BEFORE writing its
// body; we cancel, then release the server. The cancel is therefore
// guaranteed to land while the page-0 round-trip is in flight, so
// nri.List(page 0) aborts with "context canceled" — point (2) above — at
// a defined synchronisation point, every run.
func TestInternalRESTConfigDispatch_PagedList_ContextCancelDuringWalk(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	const totalItems = 2500 // 5 pages — the walk would complete were it not cancelled
	pageSize := int(internalDispatchListPageLimit)
	fixture, caPEM := newPagedListFixture(t, totalItems, pageSize)

	rc := &rest.Config{
		Host:        fixture.server.URL,
		BearerToken: "fake-sa-jwt",
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caPEM,
		},
	}

	ctx, cancel := context.WithCancel(cache.WithInternalRESTConfig(context.Background(), rc))
	t.Cleanup(cancel)

	// Deterministic cancel: when the server has received page 0 and is
	// about to write the body, cancel the dispatch context, then unblock
	// the server. cancelLandedCh closes once cancel() has returned so the
	// server only proceeds AFTER the context is cancelled — guaranteeing
	// nri.List(page 0) observes the cancellation.
	cancelLandedCh := make(chan struct{})
	fixture.onFirstRequest = func() <-chan struct{} {
		cancel()
		close(cancelLandedCh)
		return cancelLandedCh
	}

	_, _, err := dispatchViaInternalRESTConfig(ctx, httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{
			Path: "/apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages",
		},
	})
	if err == nil {
		t.Fatal("FALSIFIER-CTX FAIL: expected a context-cancellation error " +
			"from the paged walk, got nil")
	}
	if err != context.Canceled && !strings.Contains(err.Error(), "context canceled") {
		// client-go may wrap the context error into a *url.Error or a
		// k8s.io API error envelope — either is acceptable as long as
		// the surface error chain includes `context canceled`.
		t.Fatalf("FALSIFIER-CTX FAIL: expected context.Canceled (or a "+
			"wrapper carrying \"context canceled\"), got %v", err)
	}
	// The walk MUST NOT have completed all pages. With the cancel landing
	// mid-page-0 the dispatcher bails before requesting page 1, so the
	// server saw strictly fewer than len(pages) requests.
	if calls := fixture.calls.Load(); calls >= int64(len(fixture.pages)) {
		t.Fatalf("FALSIFIER-CTX FAIL: paged walk issued all %d page requests "+
			"despite ctx cancellation — expected it to bail out mid-stream. "+
			"calls=%d", len(fixture.pages), calls)
	}
}

// withSlogLogger attaches a *slog.Logger to the request context using
// xcontext.BuildContext(...WithLogger), the same construction the
// resolver and dispatchers use (cluster_list.go:549, dispatchers/
// background_logger_level_test.go:48). xcontext.Logger(ctx) — which
// dispatchViaInternalRESTConfig invokes for the falsifier event — then
// resolves to this logger.
func withSlogLogger(ctx context.Context, l *slog.Logger) context.Context {
	return xcontext.BuildContext(ctx, xcontext.WithLogger(l))
}
