package cache

import (
	"sync"
	"testing"
)

// Q-5XX-DIAG (0.25.324) — IncMapKey + SnapshotMap drive the open-label
// counters added for widget 5xx attribution. These tests pin the
// LoadOrStore-once semantics, the snapshot empty-suppression contract,
// and concurrency safety under the race detector.

func TestIncMapKey_AllocatesAndIncrements(t *testing.T) {
	var m sync.Map

	if got := IncMapKey(&m, "a"); got != 1 {
		t.Errorf("first inc: got %d, want 1", got)
	}
	if got := IncMapKey(&m, "a"); got != 2 {
		t.Errorf("second inc: got %d, want 2", got)
	}
	if got := IncMapKey(&m, "b"); got != 1 {
		t.Errorf("new key: got %d, want 1", got)
	}
}

func TestSnapshotMap_EmptyReturnsNil(t *testing.T) {
	var m sync.Map
	if got := SnapshotMap(&m); got != nil {
		t.Errorf("empty snapshot: got %v, want nil (omitempty contract)", got)
	}
}

func TestSnapshotMap_CopiesValues(t *testing.T) {
	var m sync.Map
	IncMapKey(&m, "a")
	IncMapKey(&m, "a")
	IncMapKey(&m, "b")

	snap := SnapshotMap(&m)
	if snap == nil {
		t.Fatalf("snap nil with entries")
	}
	if snap["a"] != 2 {
		t.Errorf("snap[a]: got %d, want 2", snap["a"])
	}
	if snap["b"] != 1 {
		t.Errorf("snap[b]: got %d, want 1", snap["b"])
	}

	// Snapshot is a copy: further increments do NOT mutate it.
	IncMapKey(&m, "a")
	if snap["a"] != 2 {
		t.Errorf("snap[a] mutated after fresh inc: got %d, want 2 (copy semantics)", snap["a"])
	}
}

// TestIncMapKey_ConcurrentSafe drives N goroutines against the same
// key. LoadOrStore must allocate exactly one *atomic.Int64 so the
// final count is exactly N. Race detector catches the dual-allocation
// regression that would otherwise lose increments.
func TestIncMapKey_ConcurrentSafe(t *testing.T) {
	var m sync.Map
	const N = 500

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			IncMapKey(&m, "shared")
		}()
	}
	wg.Wait()

	snap := SnapshotMap(&m)
	if got := snap["shared"]; got != N {
		t.Errorf("concurrent inc: got %d, want %d", got, N)
	}
}
