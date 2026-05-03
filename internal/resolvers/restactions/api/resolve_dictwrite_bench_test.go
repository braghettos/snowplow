package api

import (
	"context"
	"sync"
	"testing"
)

// resolve_dictwrite_bench_test.go — Q-DEV-3 microbenchmark for the
// instrumentedDictWrite helper added in the dictMu OTel-span instrumentation
// (see /tmp/snowplow-runs/dictmu-otel-span-design-2026-05-03.md).
//
// Acceptance bar (per design §5 mitigation, §6 step 1):
//
//   - With the global no-op OTel tracer (OTEL_ENABLED=false equivalent —
//     this is the default in tests since no SDK provider is installed),
//     the wrapped helper must be at most ~1% slower per critical-section
//     entry than the raw `mu.Lock(); fn(); mu.Unlock()` baseline.
//
// Per design §5, the projected cost is ~15 ns/site for the no-op path
// (Start/End on the no-op span + IsRecording branch). The raw baseline is
// ~10–20 ns/Lock-Unlock pair on uncontended Mutex.
//
// The benchmark intentionally keeps fn empty: we want to measure the
// instrumentation overhead, not the cost of any real work the caller does
// inside the lock. Real workloads add JQ wall-time on top.
//
// Run: `go test -bench=BenchmarkInstrumentedDictWrite -benchmem -count=5
//
//	./internal/resolvers/restactions/api/...`
func BenchmarkInstrumentedDictWrite_Disabled(b *testing.B) {
	ctx := context.Background()
	var mu sync.Mutex
	dict := map[string]any{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		instrumentedDictWrite(ctx, &mu, dict, "snapshot", "read", nil, func() {})
	}
}

// BenchmarkRawLockUnlock measures the unwrapped baseline — what the code
// looked like before the helper was introduced. The instrumented helper
// (with no-op tracer) should be within ~1% of this.
func BenchmarkRawLockUnlock(b *testing.B) {
	var mu sync.Mutex
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mu.Lock()
		mu.Unlock()
	}
}

// BenchmarkInstrumentedDictWrite_DisabledWithExtras measures the helper
// when the caller passes an extra-attributes slice. With the no-op tracer
// the slice is constructed by the caller but never consumed (IsRecording
// returns false so no SetAttributes call). This bench captures the
// caller-side allocation cost which is the dominant residual when
// OTEL_ENABLED=false at sites that allocate extras unconditionally
// (informer_direct, call_l1, call_inline, http_fallback, err_write).
//
// If this is significantly more expensive than the no-extras variant,
// callers should defer the slice construction until inside fn — but per
// design §5 the cost is acceptable today.
func BenchmarkInstrumentedDictWrite_DisabledWithExtras(b *testing.B) {
	ctx := context.Background()
	var mu sync.Mutex
	dict := map[string]any{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		instrumentedDictWrite(ctx, &mu, dict, "informer_direct", "append",
			nil, func() {})
	}
}
