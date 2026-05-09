// Ship 1.5a (0.25.322) — sync.Pool round-trip tests for the JQ input
// scratch buffer used by jsonHandlerCompute.
//
// Pins:
//   1. acquire/release round-trips return a usable buffer.
//   2. release zeros the `tmp` field so the next acquire never observes
//      a stale tree reference (gojq purity invariant — see audit at
//      /tmp/snowplow-runs/gojq-purity-audit-2026-05-03.md).
//   3. jsonHandlerCompute still produces correct output through the pool
//      (no JQ — passthrough; with JQ — filter applied).
package api

import (
	"context"
	"testing"

	"github.com/krateoplatformops/plumbing/ptr"
)

// TestDecodeBufferPoolRoundTrip confirms the scratch holder round-trips
// through the pool and is reset on Put. Without the reset, the next Get
// would observe a stale tree reference that gojq may have aliased into
// its output — re-introducing the alias hazard the purity audit
// eliminated. The release helper sets tmp to nil before Put.
func TestDecodeBufferPoolRoundTrip(t *testing.T) {
	b := acquireDecodeBuffer()
	if b == nil {
		t.Fatalf("acquireDecodeBuffer returned nil")
	}
	// Stuff a sentinel value into the holder.
	b.tmp = map[string]any{"sentinel": "v3"}
	releaseDecodeBuffer(b)

	// The released holder MUST have its tmp field zeroed. We can't
	// reliably read back the same holder (sync.Pool may return a fresh
	// one), so we exercise the contract via the helper directly.
	if b.tmp != nil {
		t.Fatalf("releaseDecodeBuffer did not reset tmp: got %v", b.tmp)
	}

	// And a fresh acquire returns a buffer whose tmp is nil (either the
	// reset holder or a newly New'd one — both are nil).
	b2 := acquireDecodeBuffer()
	if b2.tmp != nil {
		t.Fatalf("fresh acquireDecodeBuffer returned non-nil tmp: %v", b2.tmp)
	}
	releaseDecodeBuffer(b2)
}

// TestDecodeBufferPoolNilSafe confirms releasing a nil buffer is a no-op
// (defensive: prevents panic if a caller passes nil from an error path).
func TestDecodeBufferPoolNilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("releaseDecodeBuffer(nil) panicked: %v", r)
		}
	}()
	releaseDecodeBuffer(nil)
}

// TestJsonHandlerComputePassthroughViaPool confirms jsonHandlerCompute
// still returns the unmarshaled tree when there is no JQ filter. Pool
// instrumentation MUST NOT change widget-prop output for valid input —
// feedback_cache_must_not_constrain_jq.md.
func TestJsonHandlerComputePassthroughViaPool(t *testing.T) {
	raw := []byte(`{"foo":"bar","n":42}`)
	opts := jsonHandlerOptions{key: "test", out: map[string]any{}}

	got, err := jsonHandlerCompute(context.Background(), opts, raw, nil)
	if err != nil {
		t.Fatalf("jsonHandlerCompute: %v", err)
	}
	gotMap, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", got)
	}
	if gotMap["foo"] != "bar" {
		t.Errorf("foo = %v, want bar", gotMap["foo"])
	}
}

// TestJsonHandlerComputeWithFilterViaPool confirms the JQ branch still
// runs correctly through the pooled scratch holder. Pool churn must not
// alter JQ output for any valid expression. The filter is keyed under
// opts.key because jsonHandlerCompute wraps the parsed input as
// {opts.key: parsed} before passing to gojq.
func TestJsonHandlerComputeWithFilterViaPool(t *testing.T) {
	raw := []byte(`{"foo":"bar","num":42}`)
	q := ".test.foo"
	opts := jsonHandlerOptions{
		key:    "test",
		out:    map[string]any{},
		filter: ptr.To(q),
	}

	got, err := jsonHandlerCompute(context.Background(), opts, raw, nil)
	if err != nil {
		t.Fatalf("jsonHandlerCompute: %v", err)
	}
	if got != "bar" {
		t.Errorf("filter output = %v, want \"bar\"", got)
	}
}

// TestJsonHandlerComputePoolReuseUnderContention drives the pool path
// concurrently to exercise the New-or-reuse branch. Each goroutine
// computes its own input independently; if pooling ever leaked a tree
// reference between goroutines (alias hazard), the assertions would
// catch the cross-talk.
func TestJsonHandlerComputePoolReuseUnderContention(t *testing.T) {
	const goroutines = 16
	done := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			raw := []byte(`{"id":` + itoa(idx) + `,"name":"row"}`)
			q := ".row.id"
			opts := jsonHandlerOptions{
				key:    "row",
				out:    map[string]any{},
				filter: ptr.To(q),
			}
			got, err := jsonHandlerCompute(context.Background(), opts, raw, nil)
			if err != nil {
				done <- err
				return
			}
			f, ok := got.(float64)
			if !ok {
				done <- &poolTestErr{got: got}
				return
			}
			if int(f) != idx {
				done <- &poolTestErr{got: int(f), want: idx}
				return
			}
			done <- nil
		}(i)
	}
	for i := 0; i < goroutines; i++ {
		if err := <-done; err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
}

type poolTestErr struct {
	got, want any
}

func (e *poolTestErr) Error() string { return "got != want under pool contention" }

// itoa is a tiny helper so the test does not depend on strconv import
// shape; keeps the test file self-contained.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
