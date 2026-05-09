// Ship 1.5a (0.25.322) — decode-concurrency back-pressure tests.
//
// Pins:
//   1. handleEventSem cap = min(GOMAXPROCS, 8)
//   2. processItemSem cap = min(GOMAXPROCS, 4)
//   3. Both semaphores actually serialize concurrent acquirers (no goroutine
//      leak past the configured cap).
//   4. InformerEventDropped counter exposed at the snapshot level so the
//      runtime handler / canary can read back-pressure incidents.
package cache

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHandleEventSemCap confirms the decode-path semaphore is sized to
// min(GOMAXPROCS, 8). Architects spec'd this in the 1.5a plan; if a
// future refactor changes the cap silently we want a loud failure.
func TestHandleEventSemCap(t *testing.T) {
	got := HandleEventSemCap()
	want := runtime.GOMAXPROCS(0)
	if want > 8 {
		want = 8
	}
	if got != want {
		t.Fatalf("HandleEventSemCap = %d, want %d (min(GOMAXPROCS=%d, 8))",
			got, want, runtime.GOMAXPROCS(0))
	}
}

// TestProcessItemSemCap confirms the L1-refresh-path semaphore is sized to
// min(GOMAXPROCS, 4). Tighter cap than handleEvent because each
// processItem call fans out to multiple downstream resolves.
func TestProcessItemSemCap(t *testing.T) {
	got := ProcessItemSemCap()
	want := runtime.GOMAXPROCS(0)
	if want > 4 {
		want = 4
	}
	if got != want {
		t.Fatalf("ProcessItemSemCap = %d, want %d (min(GOMAXPROCS=%d, 4))",
			got, want, runtime.GOMAXPROCS(0))
	}
}

// TestHandleEventSemEnforcesCap confirms the semaphore actually limits
// concurrent holders to its cap, even under contention. We launch 4×cap
// goroutines that each acquire-hold-release; a peak-counter probe asserts
// the in-flight count never exceeds cap.
//
// Direct probe of the semaphore channel — handleEvent itself requires an
// informer fixture to drive, but the contract under test (back-pressure
// via channel-cap) is identical regardless of caller, so the unit-level
// probe is the right granularity.
func TestHandleEventSemEnforcesCap(t *testing.T) {
	ensureSemaphores()
	cap := HandleEventSemCap()
	if cap < 1 {
		t.Fatalf("invalid cap %d", cap)
	}

	var inFlight, peak atomic.Int64
	var wg sync.WaitGroup
	const goroutines = 32
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			handleEventSem <- struct{}{}
			n := inFlight.Add(1)
			// Track peak with a CAS loop so the test is race-clean.
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			// Hold briefly so concurrent goroutines pile up on the
			// channel send, exercising the cap.
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
			<-handleEventSem
		}()
	}
	wg.Wait()

	if peak.Load() > int64(cap) {
		t.Fatalf("handleEventSem peak in-flight = %d, exceeds cap %d", peak.Load(), cap)
	}
	if peak.Load() == 0 {
		t.Fatalf("handleEventSem never held — sanity check failed")
	}
}

// TestProcessItemSemEnforcesCap mirrors the above for the L1-refresh-path
// semaphore. Same contract, tighter cap.
func TestProcessItemSemEnforcesCap(t *testing.T) {
	ensureSemaphores()
	cap := ProcessItemSemCap()
	if cap < 1 {
		t.Fatalf("invalid cap %d", cap)
	}

	var inFlight, peak atomic.Int64
	var wg sync.WaitGroup
	const goroutines = 32
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			processItemSem <- struct{}{}
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
			<-processItemSem
		}()
	}
	wg.Wait()

	if peak.Load() > int64(cap) {
		t.Fatalf("processItemSem peak in-flight = %d, exceeds cap %d", peak.Load(), cap)
	}
	if peak.Load() == 0 {
		t.Fatalf("processItemSem never held — sanity check failed")
	}
}

// TestInformerEventDroppedCounter pins the back-pressure scaffold for
// Ship 1.5b. Increment the counter directly and assert the snapshot
// surfaces it. In 1.5a eventCh stays at 1M so this counter is expected
// to remain at 0 in production; the unit test exercises the surface only.
func TestInformerEventDroppedCounter(t *testing.T) {
	before := GlobalMetrics.InformerEventDropped.Load()
	GlobalMetrics.InformerEventDropped.Add(3)
	t.Cleanup(func() {
		GlobalMetrics.InformerEventDropped.Store(before)
	})

	snap := GlobalMetrics.Snapshot()
	got := snap.InformerEventDropped
	if got != before+3 {
		t.Fatalf("snapshot.InformerEventDropped = %d, want %d", got, before+3)
	}
}

// TestL1WritesSkippedIdenticalCounter pins the surface for the L1 no-op
// dampener counter (Ship 1.5a item 4 — defer/skip MarshalCached when
// bytes unchanged).
func TestL1WritesSkippedIdenticalCounter(t *testing.T) {
	before := GlobalMetrics.L1WritesSkippedIdentical.Load()
	GlobalMetrics.L1WritesSkippedIdentical.Add(7)
	t.Cleanup(func() {
		GlobalMetrics.L1WritesSkippedIdentical.Store(before)
	})

	snap := GlobalMetrics.Snapshot()
	got := snap.L1WritesSkippedIdentical
	if got != before+7 {
		t.Fatalf("snapshot.L1WritesSkippedIdentical = %d, want %d", got, before+7)
	}
}
