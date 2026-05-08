// Q-OOM-COMPLETION (v0.25.315) Patch 2 — unit tests for the inner
// prewarm concurrency cap. Pre-Patch the inner semaphore allowed
// runtime.GOMAXPROCS(0) concurrent inner resolves per outer worker
// (16 total at GOMAXPROCS=4), which produced transient 16 GiB peaks
// in the 16 GiB cgroup → OOM. Post-Patch the cap is max(2,
// GOMAXPROCS/2) by default and operator-overridable via env.
package dispatchers

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPrewarmInnerConcurrency_DefaultMatchesGomaxprocsHalf — the cap
// equals max(2, GOMAXPROCS/2) when no env override is set.
func TestPrewarmInnerConcurrency_DefaultMatchesGomaxprocsHalf(t *testing.T) {
	// Ensure no override is set for this test.
	t.Setenv(envPrewarmInnerConcurrency, "")

	got := prewarmInnerConcurrency()
	gp := runtime.GOMAXPROCS(0)
	want := gp / 2
	if want < 2 {
		want = 2
	}
	if got != want {
		t.Errorf("prewarmInnerConcurrency(): got %d, want %d (GOMAXPROCS=%d, half=%d, min=2)",
			got, want, gp, gp/2)
	}
}

// TestPrewarmInnerConcurrency_EnvOverride — operator-tuned override
// via env wins over the default.
func TestPrewarmInnerConcurrency_EnvOverride(t *testing.T) {
	tests := []struct {
		val  string
		want int
	}{
		{"1", 1},
		{"2", 2},
		{"8", 8},
		{"32", 32},
	}
	for _, tc := range tests {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv(envPrewarmInnerConcurrency, tc.val)
			if got := prewarmInnerConcurrency(); got != tc.want {
				t.Errorf("env=%q: got %d, want %d", tc.val, got, tc.want)
			}
		})
	}
}

// TestPrewarmInnerConcurrency_InvalidEnvFallsBack — bogus env values
// fall back to the GOMAXPROCS-derived default.
func TestPrewarmInnerConcurrency_InvalidEnvFallsBack(t *testing.T) {
	gp := runtime.GOMAXPROCS(0)
	def := gp / 2
	if def < 2 {
		def = 2
	}
	cases := []string{"abc", "0", "-5", ""}
	for _, c := range cases {
		t.Run("env="+c, func(t *testing.T) {
			t.Setenv(envPrewarmInnerConcurrency, c)
			if got := prewarmInnerConcurrency(); got != def {
				t.Errorf("env=%q: got %d, want default %d", c, got, def)
			}
		})
	}
}

// TestPrewarmInnerConcurrency_RespectsCap — drives a fan-out of fake
// "walks" through a sem sized at prewarmInnerConcurrency() and asserts
// peak observed concurrency never exceeds the cap. This mirrors the
// pattern used in resolveL1RefsCollect.
func TestPrewarmInnerConcurrency_RespectsCap(t *testing.T) {
	t.Setenv(envPrewarmInnerConcurrency, "3")
	cap := prewarmInnerConcurrency()
	if cap != 3 {
		t.Fatalf("cap: got %d, want 3", cap)
	}

	const fakeWalks = 32
	sem := make(chan struct{}, cap)

	var (
		wg          sync.WaitGroup
		inflight    atomic.Int64
		peak        atomic.Int64
		totalRan    atomic.Int64
	)

	// Mirror the production pattern: outer schedules N goroutines,
	// each acquires a sem slot, performs work, releases.
	for i := 0; i < fakeWalks; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			cur := inflight.Add(1)
			defer inflight.Add(-1)
			// Track peak observed concurrent in-flight.
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			// Simulate a brief decode/JSON-marshal step. Keep it
			// small so the test stays fast while still letting the
			// scheduler create real concurrency.
			time.Sleep(15 * time.Millisecond)
			totalRan.Add(1)
		}()
	}
	wg.Wait()

	if got := totalRan.Load(); got != int64(fakeWalks) {
		t.Errorf("total ran: got %d, want %d", got, fakeWalks)
	}
	pk := peak.Load()
	if pk > int64(cap) {
		t.Errorf("peak concurrent: got %d, want ≤%d (sem cap breached)", pk, cap)
	}
	// Sanity: the sem should actually have backpressured. With 32
	// walks queued and a cap of 3, peak concurrency should hit at
	// least 2 (else the test isn't actually exercising concurrency).
	if pk < 2 {
		t.Errorf("peak concurrent: got %d, expected ≥2 (test must actually overlap goroutines)", pk)
	}
}
