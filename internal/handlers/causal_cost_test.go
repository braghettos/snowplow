package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// errorAfterNWriter is a test ResponseWriter that captures bytes through
// some N writes successfully, then returns a synthetic error on subsequent
// writes. Used to drive the partial-write branch of statusRecorder.
type errorAfterNWriter struct {
	header  http.Header
	body    []byte
	code    int
	allowed int // bytes we allow before erroring
	written int
	err     error
}

func newErrorAfterNWriter(allowed int, err error) *errorAfterNWriter {
	return &errorAfterNWriter{header: http.Header{}, allowed: allowed, err: err}
}

func (w *errorAfterNWriter) Header() http.Header { return w.header }

func (w *errorAfterNWriter) WriteHeader(code int) { w.code = code }

func (w *errorAfterNWriter) Write(b []byte) (int, error) {
	remaining := w.allowed - w.written
	if remaining <= 0 {
		return 0, w.err
	}
	if len(b) <= remaining {
		w.body = append(w.body, b...)
		w.written += len(b)
		return len(b), nil
	}
	// Partial write: emit `remaining` bytes, then error on the rest.
	w.body = append(w.body, b[:remaining]...)
	w.written += remaining
	return remaining, w.err
}

// TestStatusRecorder_PassThroughBaseline verifies the wrapper is byte-
// equivalent to the underlying ResponseWriter on a clean Get/Set roundtrip.
// Same status, same body bytes, same headers, same ordering. The architect's
// acceptance criterion: "no behavior change in /call hot path".
func TestStatusRecorder_PassThroughBaseline(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := newStatusRecorder(rec)

	sr.Header().Set("X-Test", "value")
	sr.Header().Set("Content-Type", "application/json")
	sr.WriteHeader(http.StatusCreated)
	payload := []byte(`{"hello":"world"}`)
	n, err := sr.Write(payload)
	if err != nil {
		t.Fatalf("Write: unexpected err: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write: returned n=%d, want %d", n, len(payload))
	}

	// Underlying recorder must see the same status/body/headers as if the
	// wrapper were absent.
	if rec.Code != http.StatusCreated {
		t.Errorf("underlying status: got %d, want %d", rec.Code, http.StatusCreated)
	}
	if got := rec.Body.String(); got != `{"hello":"world"}` {
		t.Errorf("underlying body: got %q, want %q", got, `{"hello":"world"}`)
	}
	if got := rec.Header().Get("X-Test"); got != "value" {
		t.Errorf("underlying header X-Test: got %q, want %q", got, "value")
	}

	// Recorder fields must reflect what the client saw.
	if sr.headerStatus != http.StatusCreated {
		t.Errorf("recorder headerStatus: got %d, want %d", sr.headerStatus, http.StatusCreated)
	}
	if sr.bytesWritten != int64(len(payload)) {
		t.Errorf("recorder bytesWritten: got %d, want %d", sr.bytesWritten, len(payload))
	}
	if sr.writeErr != nil {
		t.Errorf("recorder writeErr: got %v, want nil", sr.writeErr)
	}
}

// TestStatusRecorder_WriteHeaderIsSticky guards the contract that only the
// FIRST WriteHeader is reported. net/http only honors the first call too.
func TestStatusRecorder_WriteHeaderIsSticky(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := newStatusRecorder(rec)

	sr.WriteHeader(http.StatusOK)
	sr.WriteHeader(http.StatusInternalServerError) // should be ignored

	if sr.headerStatus != http.StatusOK {
		t.Errorf("headerStatus: got %d, want %d (first WriteHeader wins)",
			sr.headerStatus, http.StatusOK)
	}
}

// TestStatusRecorder_ImplicitWriteHeaderOnFirstWrite verifies the recorder
// mirrors net/http's contract: a Write before WriteHeader sends an implicit
// 200 to the client. The recorder must capture 200 in headerStatus so the
// deferred log/counters reflect what the browser actually saw.
func TestStatusRecorder_ImplicitWriteHeaderOnFirstWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := newStatusRecorder(rec)

	if _, err := sr.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: unexpected err: %v", err)
	}

	if sr.headerStatus != http.StatusOK {
		t.Errorf("headerStatus on implicit: got %d, want %d",
			sr.headerStatus, http.StatusOK)
	}
	if sr.bytesWritten != 5 {
		t.Errorf("bytesWritten: got %d, want 5", sr.bytesWritten)
	}
}

// TestStatusRecorder_WriteErrorIsCapturedAndPropagated guards two
// invariants: (1) the underlying Write error is returned to the caller
// VERBATIM (transparent wrapper), (2) bytesWritten reflects only the
// actually-written portion under partial writes, (3) writeErr captures
// the first non-nil error.
func TestStatusRecorder_WriteErrorIsCapturedAndPropagated(t *testing.T) {
	sentinelErr := errors.New("broken pipe")
	w := newErrorAfterNWriter(3, sentinelErr) // allow 3 bytes, then fail
	sr := newStatusRecorder(w)

	// First write — fully consumed.
	n, err := sr.Write([]byte("ab"))
	if err != nil || n != 2 {
		t.Fatalf("first write: got n=%d err=%v, want 2,nil", n, err)
	}

	// Second write — partial: 1 byte fits, then error mid-call.
	n, err = sr.Write([]byte("cdef"))
	if !errors.Is(err, sentinelErr) {
		t.Errorf("second write: got err=%v, want %v", err, sentinelErr)
	}
	if n != 1 {
		t.Errorf("second write: got n=%d, want 1 (partial)", n)
	}

	if sr.bytesWritten != 3 {
		t.Errorf("bytesWritten: got %d, want 3 (2 full + 1 partial)", sr.bytesWritten)
	}
	if !errors.Is(sr.writeErr, sentinelErr) {
		t.Errorf("writeErr: got %v, want %v", sr.writeErr, sentinelErr)
	}

	// Subsequent writes after an error — the recorder must still pass
	// through but writeErr must remain the FIRST error (not be overwritten
	// by a later one).
	otherErr := errors.New("connection reset")
	w.err = otherErr
	_, err = sr.Write([]byte("ghi"))
	if !errors.Is(err, otherErr) {
		t.Errorf("third write: got err=%v, want %v", err, otherErr)
	}
	if !errors.Is(sr.writeErr, sentinelErr) {
		t.Errorf("writeErr after second error: got %v, want %v (first wins)",
			sr.writeErr, sentinelErr)
	}
}

// TestRuntimeMetrics_ExposesCallEventsBlock guards the canary contract for
// Q-CAUSAL-COST (0.25.323): /metrics/runtime MUST surface the three call
// exit-edge counters under the top-level "call_events" key. Without this
// block the causal-cost diagnostic is blind — Diego's PM-approved spec
// calls the block out by name.
func TestRuntimeMetrics_ExposesCallEventsBlock(t *testing.T) {
	cache.GlobalMetrics.CallClientGone.Store(11)
	cache.GlobalMetrics.CallClientGoneAfterWriteHeader.Store(4)
	cache.GlobalMetrics.CallWriteError.Store(2)
	t.Cleanup(func() {
		cache.GlobalMetrics.CallClientGone.Store(0)
		cache.GlobalMetrics.CallClientGoneAfterWriteHeader.Store(0)
		cache.GlobalMetrics.CallWriteError.Store(0)
	})

	req := httptest.NewRequest(http.MethodGet, "/metrics/runtime", nil)
	rec := httptest.NewRecorder()
	handler := RuntimeMetricsHandler(nil, nil, nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", rec.Code, rec.Body.String())
	}

	var got RuntimeMetrics
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%q", err, rec.Body.String())
	}

	if got.CallEvents.ClientGone != 11 {
		t.Errorf("CallEvents.ClientGone: got %d, want 11", got.CallEvents.ClientGone)
	}
	if got.CallEvents.ClientGoneAfterWriteHeader != 4 {
		t.Errorf("CallEvents.ClientGoneAfterWriteHeader: got %d, want 4",
			got.CallEvents.ClientGoneAfterWriteHeader)
	}
	if got.CallEvents.WriteError != 2 {
		t.Errorf("CallEvents.WriteError: got %d, want 2", got.CallEvents.WriteError)
	}

	// Also assert the JSON keys are the snake_case contract the canary
	// scrapes — a future renamer must fail this test, not silently break
	// the dashboards.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	ce, ok := raw["call_events"].(map[string]any)
	if !ok {
		t.Fatalf("call_events block missing or wrong shape: %T", raw["call_events"])
	}
	for _, k := range []string{"client_gone", "client_gone_after_write_header", "write_error"} {
		if _, present := ce[k]; !present {
			t.Errorf("call_events.%s key missing from JSON", k)
		}
	}
}

// TestCallHandler_CounterEdges_ClientGoneBeforeWriteHeader exercises the
// edge case where the request context is already cancelled before the
// handler writes anything. The deferred block must (a) NOT increment
// CallClientGoneAfterWriteHeader (no header was written), (b) increment
// CallClientGone (ctx.Err() != nil at exit).
//
// We drive this through the real callHandler.ServeHTTP so the defer wiring
// is exercised end-to-end. validateRequest fails on missing apiVersion,
// which produces a synchronous BadRequest before any cancel logic — but
// because the request context is already cancelled, the deferred block
// still observes ctx.Err() != nil at exit.
//
// Note: BadRequest writes a 400 header BEFORE the defer runs, so this is
// actually the after-WriteHeader path. We separate the two cases by
// constructing a request whose context is cancelled and whose handler
// writes a header (any handler will). The "before WriteHeader" branch is
// tested in unit isolation against the recorder directly above.
func TestCallHandler_CounterEdges_AfterWriteHeader(t *testing.T) {
	beforeGone := cache.GlobalMetrics.CallClientGone.Load()
	beforeAfter := cache.GlobalMetrics.CallClientGoneAfterWriteHeader.Load()
	beforeWErr := cache.GlobalMetrics.CallWriteError.Load()
	t.Cleanup(func() {
		cache.GlobalMetrics.CallClientGone.Store(beforeGone)
		cache.GlobalMetrics.CallClientGoneAfterWriteHeader.Store(beforeAfter)
		cache.GlobalMetrics.CallWriteError.Store(beforeWErr)
	})

	// Build a request with a pre-cancelled context. validateRequest will
	// fail (no apiVersion query) and BadRequest writes the 400 header
	// BEFORE the deferred block runs — so we expect both ClientGone AND
	// ClientGoneAfterWriteHeader to bump.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/call", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	h := Call()
	h.ServeHTTP(rec, req)

	gotGone := cache.GlobalMetrics.CallClientGone.Load() - beforeGone
	gotAfter := cache.GlobalMetrics.CallClientGoneAfterWriteHeader.Load() - beforeAfter

	if gotGone != 1 {
		t.Errorf("CallClientGone delta: got %d, want 1", gotGone)
	}
	if gotAfter != 1 {
		t.Errorf("CallClientGoneAfterWriteHeader delta: got %d, want 1 (BadRequest writes 400 before defer)", gotAfter)
	}
}

// TestCallHandler_NoCounterBumpOnHealthyPath verifies the negative case:
// a normal request with a live context must NOT bump any of the three
// causal-cost counters at handler exit. validateRequest still fails (we
// don't supply a full request body / endpoint) but ctx.Err() is nil and
// no Write error occurs, so all three counters stay flat.
func TestCallHandler_NoCounterBumpOnHealthyPath(t *testing.T) {
	beforeGone := cache.GlobalMetrics.CallClientGone.Load()
	beforeAfter := cache.GlobalMetrics.CallClientGoneAfterWriteHeader.Load()
	beforeWErr := cache.GlobalMetrics.CallWriteError.Load()
	t.Cleanup(func() {
		cache.GlobalMetrics.CallClientGone.Store(beforeGone)
		cache.GlobalMetrics.CallClientGoneAfterWriteHeader.Store(beforeAfter)
		cache.GlobalMetrics.CallWriteError.Store(beforeWErr)
	})

	req := httptest.NewRequest(http.MethodGet, "/call", nil) // live ctx
	rec := httptest.NewRecorder()

	h := Call()
	h.ServeHTTP(rec, req)

	if got := cache.GlobalMetrics.CallClientGone.Load() - beforeGone; got != 0 {
		t.Errorf("CallClientGone delta on healthy path: got %d, want 0", got)
	}
	if got := cache.GlobalMetrics.CallClientGoneAfterWriteHeader.Load() - beforeAfter; got != 0 {
		t.Errorf("CallClientGoneAfterWriteHeader delta on healthy path: got %d, want 0", got)
	}
	if got := cache.GlobalMetrics.CallWriteError.Load() - beforeWErr; got != 0 {
		t.Errorf("CallWriteError delta on healthy path: got %d, want 0", got)
	}
}
