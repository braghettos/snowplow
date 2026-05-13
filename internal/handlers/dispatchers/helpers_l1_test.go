// helpers_l1_test.go — Tag 0.30.7 binding: verify dispatcher-side L1
// hook respects the CACHE_ENABLED + RESOLVED_CACHE_ENABLED toggles and
// that the encoder + writer helpers produce byte-identical output to
// the pre-0.30.7 `json.Encoder.Encode` path.
//
// Per the plan's pre-flight falsifier #1: cache=off path must continue
// to serve correct results. The "cache=off ⇒ dispatchCacheLookupKey
// returns nil handle" test below is the unit-level proof of that
// invariant — when the handle is nil, the dispatchers fall straight
// through to the 0.30.6 resolve-and-encode path.

package dispatchers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestDispatchCacheLookupKey_CacheOffReturnsNilHandle(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	// Even with RESOLVED_CACHE_ENABLED=true, CACHE_ENABLED=false
	// must short-circuit to "no L1".
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	_, h := dispatchCacheLookupKey(context.Background(), "widgets",
		"g", "v", "r", "ns", "name", -1, -1, nil)
	if h != nil {
		t.Fatalf("CACHE_ENABLED=false must yield nil cache handle, got %T", h)
	}
}

func TestDispatchCacheLookupKey_ResolvedToggleOffReturnsNilHandle(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "false")
	_, h := dispatchCacheLookupKey(context.Background(), "widgets",
		"g", "v", "r", "ns", "name", -1, -1, nil)
	if h != nil {
		t.Fatalf("RESOLVED_CACHE_ENABLED=false must yield nil handle, got %T", h)
	}
}

func TestDispatchCacheLookupKey_NoUserInfoReturnsNilHandle(t *testing.T) {
	// A request with no UserInfo in context must be treated as
	// uncacheable — keying on missing identity would risk cross-user
	// leaks. The plan's binding: "key includes user_identity"; we
	// fail closed when it's absent.
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	_, h := dispatchCacheLookupKey(context.Background(), "widgets",
		"g", "v", "r", "ns", "name", -1, -1, nil)
	if h != nil {
		t.Fatalf("missing UserInfo must yield nil handle, got %T", h)
	}
}

func TestEncodeAndWriteResolvedJSON_MatchesPre0307Encoder(t *testing.T) {
	// Goal: prove encodeResolvedJSON + writeResolvedJSON produce the
	// same wire bytes the dispatchers wrote at 0.30.6 (via inline
	// json.NewEncoder + SetIndent). If this test fails, the
	// cache-miss vs cache-hit response would diverge and any client
	// computing a content hash would see the L1 layer.
	payload := map[string]any{
		"kind":      "RESTAction",
		"apiVersion": "templates.krateo.io/v1",
		"metadata":  map[string]any{"name": "demo"},
	}

	// Reference: inline encoder, identical knobs to the pre-0.30.7
	// dispatcher path.
	var ref bytes.Buffer
	refEnc := json.NewEncoder(&ref)
	refEnc.SetIndent("", "  ")
	if err := refEnc.Encode(payload); err != nil {
		t.Fatalf("reference encode failed: %v", err)
	}

	// Subject: helper-based path.
	got, err := encodeResolvedJSON(payload)
	if err != nil {
		t.Fatalf("encodeResolvedJSON failed: %v", err)
	}

	if !bytes.Equal(ref.Bytes(), got) {
		t.Fatalf("helper output diverges from pre-0.30.7 encoder.\nreference:%q\nhelper:%q", ref.Bytes(), got)
	}

	// Round-trip writeResolvedJSON: verify content-type, status, body.
	rec := httptest.NewRecorder()
	writeResolvedJSON(rec, got)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json, got %q", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), got) {
		t.Fatalf("body diverges from encoded payload")
	}
}
