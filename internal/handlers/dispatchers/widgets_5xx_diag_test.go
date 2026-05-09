package dispatchers

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
	hpkg "github.com/krateoplatformops/snowplow/internal/handlers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Q-5XX-DIAG (0.25.324) — unit coverage for the diagnostic-only
// instrumentation surface added to the widget handler. Tests assert
// counter increments + label shape; no behavior change is verified
// here (the handler hot path is exercised by the existing UAF envtest).

// resetWidgetMetrics clears the four sync.Map counters + the two
// atomic totals so each test starts from a known-zero baseline. The
// sync.Map zero-clear is implemented via Range+Delete because sync.Map
// has no Reset; in unit tests this is bounded (a handful of keys).
func resetWidgetMetrics(t *testing.T) {
	t.Helper()
	cache.GlobalMetrics.UAFTouchingCount.Store(0)
	cache.GlobalMetrics.UAFNonTouchingCount.Store(0)
	clearSyncMap(&cache.GlobalMetrics.WidgetResponsesByResource)
	clearSyncMap(&cache.GlobalMetrics.WidgetErrorByClass)
	clearSyncMap(&cache.GlobalMetrics.UAFSkipped)
	clearSyncMap(&cache.GlobalMetrics.UAFTouchingByResource)
}

func clearSyncMap(m *sync.Map) {
	m.Range(func(k, _ any) bool {
		m.Delete(k)
		return true
	})
}

// TestStatusClass_BucketBoundaries asserts the 2xx/4xx/5xx classifier
// matches the /metrics/runtime contract. The class string is part of
// the canary scrape key — a future drift would silently bin every CR
// into "other" and the diagnostic would be blind to the failing-CR
// signal.
func TestStatusClass_BucketBoundaries(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{0, "no_header"},
		{100, "other"},
		{199, "other"},
		{200, "2xx"},
		{201, "2xx"},
		{299, "2xx"},
		{300, "other"},
		{399, "other"},
		{400, "4xx"},
		{403, "4xx"},
		{404, "4xx"},
		{499, "4xx"},
		{500, "5xx"},
		{502, "5xx"},
		{599, "5xx"},
		{600, "5xx"}, // monotone: anything ≥500 is 5xx
	}
	for _, tc := range cases {
		if got := statusClass(tc.code); got != tc.want {
			t.Errorf("statusClass(%d): got %q, want %q", tc.code, got, tc.want)
		}
	}
}

// TestReadReloadIdx_HeaderParsing covers the harness contract. Empty,
// non-numeric, and negative values fall back to -1; valid integers
// pass through verbatim. The architect's spec calls -1 the
// "production traffic" sentinel — this contract MUST not silently shift.
func TestReadReloadIdx_HeaderParsing(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"absent", "", -1},
		{"zero", "0", 0},
		{"one", "1", 1},
		{"two", "2", 2},
		{"three", "3", 3},
		{"large", "42", 42},
		{"negative_one", "-1", -1},
		{"non_numeric", "abc", -1},
		{"trailing_chars", "1abc", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/widgets", nil)
			if tc.header != "" {
				req.Header.Set("X-Reload-Idx", tc.header)
			}
			if got := readReloadIdx(req); got != tc.want {
				t.Errorf("readReloadIdx(%q): got %d, want %d", tc.header, got, tc.want)
			}
		})
	}
}

// TestClassifyResolveError_ClassMapping pins the error-class labels
// that the WidgetErrorByClass counter keys on. The label space is the
// architect's contract; a future renamer must fail this test, not
// silently break the canary dashboards.
func TestClassifyResolveError_ClassMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "x"}, "y", errors.New("denied")), "rbac_forbidden"},
		{"unauthorized", apierrors.NewUnauthorized("auth"), "rbac_forbidden"},
		{"marshal_failed_wrapped", fmt.Errorf("marshal_failed: %w", errors.New("bad")), "marshal_failed"},
		{"apiref_msg", errors.New("apiref resolution failed"), "apiref_resolve_failed"},
		{"unknown", errors.New("kaboom"), "restaction_dispatch_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyResolveError(tc.err); got != tc.want {
				t.Errorf("classifyResolveError(%v): got %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestBumpWidgetErrorClass_IncrementsCounter verifies that each class
// label produces a monotonic increment on the per-class counter. The
// log line is a side effect — this test asserts only the counter
// (the canary scrape contract). class="" is a no-op so callers can
// safely skip emission for non-classified errors.
func TestBumpWidgetErrorClass_IncrementsCounter(t *testing.T) {
	resetWidgetMetrics(t)
	t.Cleanup(func() { resetWidgetMetrics(t) })

	req := httptest.NewRequest(http.MethodGet, "/widgets", nil)
	classes := []string{
		"rbac_forbidden",
		"object_get_failed",
		"apiref_resolve_failed",
		"marshal_failed",
		"restaction_dispatch_failed",
	}
	for _, class := range classes {
		bumpWidgetErrorClass(req, class, 0, errors.New("test"))
		bumpWidgetErrorClass(req, class, 0, errors.New("test")) // second hit
	}
	bumpWidgetErrorClass(req, "", 0, errors.New("test")) // no-op

	snap := cache.SnapshotMap(&cache.GlobalMetrics.WidgetErrorByClass)
	for _, class := range classes {
		if got := snap[class]; got != 2 {
			t.Errorf("WidgetErrorByClass[%q]: got %d, want 2", class, got)
		}
	}
	if _, present := snap[""]; present {
		t.Errorf("WidgetErrorByClass[\"\"] must not be present (no-op)")
	}
}

// TestBumpUAFGateCounter_TouchingVsNonTouching is the smoking-gun test:
// asserts the UAFTouching/Non-touching atoms increment correctly AND
// the per-resource label key carries "{group}/{kind}/{reload_idx}/{true|false}".
// This is the H1' attribution surface — without it, observers cannot
// distinguish the failing CRs from siblings.
func TestBumpUAFGateCounter_TouchingVsNonTouching(t *testing.T) {
	resetWidgetMetrics(t)
	t.Cleanup(func() { resetWidgetMetrics(t) })

	uns := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "CompositionPanel",
		"metadata": map[string]any{
			"name":      "bench-app-05-906-composition-panel",
			"namespace": "krateo-system",
		},
	}}

	bumpUAFGateCounter(uns, true, 1)  // panel was UAF-touching during reload 1
	bumpUAFGateCounter(uns, true, 1)
	bumpUAFGateCounter(uns, false, 2) // panel was NOT UAF-touching during reload 2

	if got := cache.GlobalMetrics.UAFTouchingCount.Load(); got != 2 {
		t.Errorf("UAFTouchingCount: got %d, want 2", got)
	}
	if got := cache.GlobalMetrics.UAFNonTouchingCount.Load(); got != 1 {
		t.Errorf("UAFNonTouchingCount: got %d, want 1", got)
	}

	snap := cache.SnapshotMap(&cache.GlobalMetrics.UAFTouchingByResource)
	keyTrue1 := "widgets.templates.krateo.io/CompositionPanel/1/true"
	keyFalse2 := "widgets.templates.krateo.io/CompositionPanel/2/false"
	if got := snap[keyTrue1]; got != 2 {
		t.Errorf("UAFTouchingByResource[%q]: got %d, want 2", keyTrue1, got)
	}
	if got := snap[keyFalse2]; got != 1 {
		t.Errorf("UAFTouchingByResource[%q]: got %d, want 1", keyFalse2, got)
	}
}

// TestBumpUAFGateCounter_NilObject covers the L1-refresh corner where
// the resolver may be invoked with nil unstructured (defensive guard
// for mutated callers). The unlabelled atoms still bump but the per-
// resource key falls back to "unknown/unknown".
func TestBumpUAFGateCounter_NilObject(t *testing.T) {
	resetWidgetMetrics(t)
	t.Cleanup(func() { resetWidgetMetrics(t) })

	bumpUAFGateCounter(nil, true, -1)

	if got := cache.GlobalMetrics.UAFTouchingCount.Load(); got != 1 {
		t.Errorf("UAFTouchingCount: got %d, want 1", got)
	}
	snap := cache.SnapshotMap(&cache.GlobalMetrics.UAFTouchingByResource)
	if got := snap["unknown/unknown/-1/true"]; got != 1 {
		t.Errorf("UAFTouchingByResource fallback: got %d, want 1 (snap=%v)", got, snap)
	}
}

// TestLogWidgetDone_BumpsResponsesByResource exercises the deferred
// exit hook end-to-end: it must increment WidgetResponsesByResource
// keyed by "{group}/{resource}/{reload_idx}/{class}" using the
// recorder's HeaderStatus AND the request's GVR query parameters.
// The class is the 2xx/4xx/5xx bucket for the recorded status.
func TestLogWidgetDone_BumpsResponsesByResource(t *testing.T) {
	resetWidgetMetrics(t)
	t.Cleanup(func() { resetWidgetMetrics(t) })

	req := httptest.NewRequest(
		http.MethodGet,
		"/widgets?apiVersion=widgets.templates.krateo.io/v1beta1&resource=compositionpanels&namespace=ns&name=bench-app-05-906-composition-panel",
		nil,
	)
	rec := httptest.NewRecorder()
	sr := hpkg.NewStatusRecorder(rec)
	sr.WriteHeader(http.StatusInternalServerError)
	sr.Write([]byte(`{"err":"x"}`))

	logWidgetDone(req, sr, time.Now(), 1)

	snap := cache.SnapshotMap(&cache.GlobalMetrics.WidgetResponsesByResource)
	wantKey := "widgets.templates.krateo.io/compositionpanels/1/5xx"
	if got := snap[wantKey]; got != 1 {
		t.Errorf("WidgetResponsesByResource[%q]: got %d, want 1 (snap=%v)", wantKey, got, snap)
	}
}

// TestLogWidgetDone_2xxAnd4xxBuckets covers the non-error class
// labels so canary observers see ratio data, not just 5xx counts.
func TestLogWidgetDone_2xxAnd4xxBuckets(t *testing.T) {
	resetWidgetMetrics(t)
	t.Cleanup(func() { resetWidgetMetrics(t) })

	for _, code := range []int{http.StatusOK, http.StatusBadRequest, http.StatusForbidden} {
		req := httptest.NewRequest(
			http.MethodGet,
			"/widgets?apiVersion=widgets.templates.krateo.io/v1beta1&resource=compositionpanels&namespace=ns&name=p",
			nil,
		)
		rec := httptest.NewRecorder()
		sr := hpkg.NewStatusRecorder(rec)
		sr.WriteHeader(code)
		logWidgetDone(req, sr, time.Now(), 0)
	}

	snap := cache.SnapshotMap(&cache.GlobalMetrics.WidgetResponsesByResource)
	if got := snap["widgets.templates.krateo.io/compositionpanels/0/2xx"]; got != 1 {
		t.Errorf("2xx bucket: got %d, want 1 (snap=%v)", got, snap)
	}
	if got := snap["widgets.templates.krateo.io/compositionpanels/0/4xx"]; got != 2 {
		t.Errorf("4xx bucket: got %d, want 2 (snap=%v)", got, snap)
	}
	if got := snap["widgets.templates.krateo.io/compositionpanels/0/5xx"]; got != 0 {
		t.Errorf("5xx bucket: got %d, want 0", got)
	}
}

// TestRuntimeMetrics_ExposesWidgetsBlock guards the canary contract
// for Q-5XX-DIAG (0.25.324): /metrics/runtime MUST surface the new
// "widgets" block with the four counter shapes. Without this block
// the diagnostic instrumentation is invisible to the canary scraper.
func TestRuntimeMetrics_ExposesWidgetsBlock(t *testing.T) {
	resetWidgetMetrics(t)
	t.Cleanup(func() { resetWidgetMetrics(t) })

	cache.GlobalMetrics.UAFTouchingCount.Store(7)
	cache.GlobalMetrics.UAFNonTouchingCount.Store(3)
	cache.IncMapKey(&cache.GlobalMetrics.WidgetErrorByClass, "rbac_forbidden")
	cache.IncMapKey(&cache.GlobalMetrics.WidgetResponsesByResource, "g/r/1/5xx")
	cache.IncMapKey(&cache.GlobalMetrics.UAFSkipped, "api_error")
	cache.IncMapKey(&cache.GlobalMetrics.UAFTouchingByResource, "g/r/1/false")

	snap := cache.GlobalMetrics.Snapshot()
	if snap.UAFTouchingCount != 7 {
		t.Errorf("UAFTouchingCount: got %d, want 7", snap.UAFTouchingCount)
	}
	if snap.UAFNonTouchingCount != 3 {
		t.Errorf("UAFNonTouchingCount: got %d, want 3", snap.UAFNonTouchingCount)
	}
	if got := snap.WidgetErrorByClass["rbac_forbidden"]; got != 1 {
		t.Errorf("WidgetErrorByClass[rbac_forbidden]: got %d, want 1", got)
	}
	if got := snap.WidgetResponsesByResource["g/r/1/5xx"]; got != 1 {
		t.Errorf("WidgetResponsesByResource[g/r/1/5xx]: got %d, want 1", got)
	}
	if got := snap.UAFSkipped["api_error"]; got != 1 {
		t.Errorf("UAFSkipped[api_error]: got %d, want 1", got)
	}
	if got := snap.UAFTouchingByResource["g/r/1/false"]; got != 1 {
		t.Errorf("UAFTouchingByResource[g/r/1/false]: got %d, want 1", got)
	}
}

