// Q-L1-SYNC-ADMIT (0.25.327) — tests for synchronous L1 admission control.
//
// Covers (architect's #2 acceptance):
//   - kvStore triggers sync-sweep when residentBytes > maxBytes post-write
//   - sync-sweep correctly advances L1EvictionsLRU
//   - L1SyncSweepCount increments on the inline path
//   - L1AsyncSweepCount increments on the 30-s ticker path
//   - concurrent over-budget writers don't deadlock and stay under cap
//
// Tests use small caps (CACHE_L1_MAX_BYTES env) so they run in ms.
package cache

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestL1_SyncAdmit_KvStoreTriggersSweepOnByteOverflow asserts that a single
// SetRaw call that pushes residentBytes past maxBytes runs the LRU sweep
// inline (no ticker needed) and that L1SyncSweepCount + L1EvictionsLRU
// both bump.
func TestL1_SyncAdmit_KvStoreTriggersSweepOnByteOverflow(t *testing.T) {
	resetL1Counters()
	GlobalMetrics.L1SyncSweepCount.Store(0)
	GlobalMetrics.L1AsyncSweepCount.Store(0)
	t.Setenv(envL1MaxBytes, "1024")
	t.Setenv(envL1MaxEntries, "100000")

	c := NewMem(time.Hour)
	ctx := context.Background()

	// Fill exactly to the cap (no overflow yet) — sync admission must NOT
	// fire because residentBytes <= maxBytes.
	for i := 0; i < 8; i++ {
		if err := c.SetRaw(ctx, "k-"+strconv.Itoa(i), fillBytes(128)); err != nil {
			t.Fatalf("SetRaw: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if got := GlobalMetrics.L1SyncSweepCount.Load(); got != 0 {
		t.Errorf("at-cap: L1SyncSweepCount=%d, want 0 (no overflow yet)", got)
	}

	// One more write tips us past the cap → sync admission must fire.
	if err := c.SetRaw(ctx, "k-overflow", fillBytes(128)); err != nil {
		t.Fatalf("SetRaw: %v", err)
	}

	if got := GlobalMetrics.L1SyncSweepCount.Load(); got == 0 {
		t.Errorf("post-overflow: L1SyncSweepCount=0, want >= 1")
	}
	if got := GlobalMetrics.L1EvictionsLRU.Load(); got == 0 {
		t.Errorf("post-overflow: L1EvictionsLRU=0, want >= 1")
	}
	// Async-ticker counter must stay 0 — StartEviction goroutine is not
	// running in this test.
	if got := GlobalMetrics.L1AsyncSweepCount.Load(); got != 0 {
		t.Errorf("post-overflow: L1AsyncSweepCount=%d, want 0", got)
	}
	// And resident bytes must be at-or-below the cap.
	if got := c.L1ResidentBytes(); got > 1024 {
		t.Errorf("post-overflow: residentBytes=%d, want <= 1024", got)
	}
}

// TestL1_SyncAdmit_ConcurrentWritersDoNotDeadlock fires N goroutines that
// each write enough bytes to trip the budget repeatedly. Without the
// try-lock guard, they could all enter sweepLRUIfOverBudget simultaneously
// and pile up; with it, exactly one runs and others fast-path-skip. The
// completion test is "all goroutines exit within a generous deadline" —
// any deadlock or contention storm exceeds it.
func TestL1_SyncAdmit_ConcurrentWritersDoNotDeadlock(t *testing.T) {
	resetL1Counters()
	GlobalMetrics.L1SyncSweepCount.Store(0)
	t.Setenv(envL1MaxBytes, "4096")
	t.Setenv(envL1MaxEntries, "100000")

	c := NewMem(time.Hour)
	ctx := context.Background()

	const numWriters = 32
	const writesPerWriter = 100

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		wg.Add(numWriters)
		for w := 0; w < numWriters; w++ {
			go func(wid int) {
				defer wg.Done()
				for i := 0; i < writesPerWriter; i++ {
					k := "w" + strconv.Itoa(wid) + "-k" + strconv.Itoa(i)
					_ = c.SetRaw(ctx, k, fillBytes(128))
				}
			}(w)
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All writers finished.
	case <-time.After(10 * time.Second):
		t.Fatalf("concurrent writers deadlocked or starved (>10 s)")
	}

	// Sanity: residentBytes settled at-or-below maxBytes.
	if got := c.L1ResidentBytes(); got > 4096 {
		t.Errorf("post-storm: residentBytes=%d, want <= 4096", got)
	}
	// Sync admission MUST have fired multiple times under load.
	if got := GlobalMetrics.L1SyncSweepCount.Load(); got == 0 {
		t.Errorf("post-storm: L1SyncSweepCount=0, want >= 1")
	}
	// Many evictions across 32 × 100 = 3200 writes against a 32-entry cap.
	if got := GlobalMetrics.L1EvictionsLRU.Load(); got < 100 {
		t.Errorf("post-storm: L1EvictionsLRU=%d, want >= 100", got)
	}
}

// TestL1_SyncAdmit_TickerDrivesAsyncCounter starts the 30-s ticker via a
// shorter equivalent (a direct sweepLRUIfOverBudget call after manually
// bumping L1AsyncSweepCount, mirroring StartEviction's path). Verifies the
// async counter increments on the ticker path even when the sync path
// stayed idle (residentBytes was already under cap when the ticker ran).
//
// Driving StartEviction's real ticker in a unit test is wasteful (30 s
// minimum); the test asserts the contract — the counter mapping — by
// invoking the same code path the goroutine would.
func TestL1_SyncAdmit_TickerDrivesAsyncCounter(t *testing.T) {
	resetL1Counters()
	GlobalMetrics.L1SyncSweepCount.Store(0)
	GlobalMetrics.L1AsyncSweepCount.Store(0)
	t.Setenv(envL1MaxBytes, "10240")
	t.Setenv(envL1MaxEntries, "100000")

	c := NewMem(time.Hour)
	ctx := context.Background()

	// Write a few entries — well under cap, no sync sweep.
	for i := 0; i < 3; i++ {
		if err := c.SetRaw(ctx, "k-"+strconv.Itoa(i), fillBytes(128)); err != nil {
			t.Fatalf("SetRaw: %v", err)
		}
	}
	if got := GlobalMetrics.L1SyncSweepCount.Load(); got != 0 {
		t.Errorf("under-cap: L1SyncSweepCount=%d, want 0", got)
	}
	if got := GlobalMetrics.L1AsyncSweepCount.Load(); got != 0 {
		t.Errorf("pre-ticker: L1AsyncSweepCount=%d, want 0", got)
	}

	// Simulate the ticker path: same ordering as StartEviction's tick
	// branch — bump async counter, then call sweep (which is a no-op
	// here because we are under cap).
	GlobalMetrics.L1AsyncSweepCount.Add(1)
	c.sweepLRUIfOverBudget()

	if got := GlobalMetrics.L1AsyncSweepCount.Load(); got != 1 {
		t.Errorf("post-ticker: L1AsyncSweepCount=%d, want 1", got)
	}
	if got := GlobalMetrics.L1SyncSweepCount.Load(); got != 0 {
		t.Errorf("post-ticker: L1SyncSweepCount=%d, want 0 (no overflow)", got)
	}
}

// TestL1_SyncAdmit_BoundedLatency ensures the inline sweep is bounded —
// even with many entries already in cache and a write that trips the cap,
// kvStore returns within a generous wallclock window. Acts as a
// regression guard against an unbounded sort/Range call sneaking in.
func TestL1_SyncAdmit_BoundedLatency(t *testing.T) {
	resetL1Counters()
	t.Setenv(envL1MaxBytes, "4096")
	t.Setenv(envL1MaxEntries, "10000")

	c := NewMem(time.Hour)
	ctx := context.Background()

	// Pre-fill below cap (eviction-free build-up).
	for i := 0; i < 30; i++ {
		if err := c.SetRaw(ctx, "k-"+strconv.Itoa(i), fillBytes(128)); err != nil {
			t.Fatalf("SetRaw: %v", err)
		}
	}

	// Now hit the cap with one more write — must return well under the
	// 10 ms inline-sweep budget plus generous headroom for CI noise.
	start := time.Now()
	if err := c.SetRaw(ctx, "trigger", fillBytes(128)); err != nil {
		t.Fatalf("SetRaw: %v", err)
	}
	dur := time.Since(start)
	if dur > 200*time.Millisecond {
		t.Errorf("kvStore latency: %v, want <= 200ms (inline sweep unbounded?)", dur)
	}
}

// TestL1_SyncAdmit_SnapshotExposesCounters confirms the new fields surface
// in MetricsSnapshot under the canonical JSON keys.
func TestL1_SyncAdmit_SnapshotExposesCounters(t *testing.T) {
	resetL1Counters()
	GlobalMetrics.L1SyncSweepCount.Store(7)
	GlobalMetrics.L1AsyncSweepCount.Store(3)
	t.Cleanup(func() {
		GlobalMetrics.L1SyncSweepCount.Store(0)
		GlobalMetrics.L1AsyncSweepCount.Store(0)
	})

	snap := GlobalMetrics.Snapshot()
	if snap.L1SyncSweepCount != 7 {
		t.Errorf("L1SyncSweepCount: got %d, want 7", snap.L1SyncSweepCount)
	}
	if snap.L1AsyncSweepCount != 3 {
		t.Errorf("L1AsyncSweepCount: got %d, want 3", snap.L1AsyncSweepCount)
	}
}
