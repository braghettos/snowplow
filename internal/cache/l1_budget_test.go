// Q-L1-BUDGET (0.25.319) — tests for the L1 byte-budget + LRU eviction.
//
// Covers (architect's #1 acceptance):
//   - byte-budget triggers LRU eviction of the oldest entries
//   - entry-count budget triggers LRU eviction at maxEntries+1
//   - GetRaw refreshes lastAccess so touched entries survive sweep
//   - TTL pass evicts independently of the byte budget
//   - residentBytes/entryCount counters track Set + Delete + TTL eviction
//
// Tests use small caps (CACHE_L1_MAX_BYTES / CACHE_L1_MAX_ENTRIES env) so
// they execute in milliseconds. The sweeper is invoked synchronously via
// sweepLRUIfOverBudget() — the StartEviction goroutine is NOT started so
// tests are deterministic.
package cache

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// fillBytes returns a slice of `n` filler bytes — useful for predictable
// byte-budget arithmetic.
func fillBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return b
}

// resetL1Counters reinitialises the global LRU/TTL eviction counters so
// tests can assert deltas without interference from other tests.
func resetL1Counters() {
	GlobalMetrics.L1EvictionsLRU.Store(0)
	GlobalMetrics.L1EvictionsTTL.Store(0)
}

// TestL1_ByteBudget_EvictsOldestWhenOverCap fills the cache past the byte
// cap and verifies the oldest (first-written) entries are evicted first
// until residentBytes <= 0.9 × cap.
func TestL1_ByteBudget_EvictsOldestWhenOverCap(t *testing.T) {
	resetL1Counters()
	// 10 KB cap with 1 KB-each entries → should hold ~9 after sweep.
	t.Setenv(envL1MaxBytes, "10240")
	t.Setenv(envL1MaxEntries, "100000") // disable entry-cap

	c := NewMem(time.Hour)
	ctx := context.Background()

	const entrySize = 1024
	const numEntries = 20

	for i := 0; i < numEntries; i++ {
		k := "key-" + strconv.Itoa(i)
		if err := c.SetRaw(ctx, k, fillBytes(entrySize)); err != nil {
			t.Fatalf("SetRaw: %v", err)
		}
		// Stagger lastAccess timestamps so the LRU ordering is stable.
		time.Sleep(time.Millisecond)
	}

	// Pre-sweep: residentBytes ≈ numEntries × entrySize. SetRaw stores
	// cloneBytes(val) so each entry's data is exactly entrySize.
	if got := c.L1ResidentBytes(); got < int64(numEntries*entrySize) {
		t.Errorf("pre-sweep residentBytes: got %d, want >= %d", got, numEntries*entrySize)
	}

	c.sweepLRUIfOverBudget()

	// Post-sweep: under target = 0.9 × cap = 9216 bytes.
	target := int64(float64(l1MaxBytes()) * 0.9)
	if got := c.L1ResidentBytes(); got > target {
		t.Errorf("post-sweep residentBytes: got %d, want <= %d", got, target)
	}

	// LRU evictions counter: should be > 0 (at least 11 evictions to bring
	// 20 KB down to <= 9 KB).
	evictedLRU := GlobalMetrics.L1EvictionsLRU.Load()
	if evictedLRU < 11 {
		t.Errorf("L1EvictionsLRU: got %d, want >= 11", evictedLRU)
	}

	// The newest entries (key-19, key-18, ...) MUST survive. The oldest
	// (key-0) MUST be evicted.
	if _, ok, _ := c.GetRaw(ctx, "key-0"); ok {
		t.Errorf("key-0 (oldest) survived sweep; want evicted")
	}
	if _, ok, _ := c.GetRaw(ctx, "key-19"); !ok {
		t.Errorf("key-19 (newest) evicted; want survived")
	}
}

// TestL1_EntryBudget_EvictsOldestAt200K simulates the entry-count cap
// trigger by setting a small entry cap and confirming that excess entries
// are LRU-evicted when residentBytes is well under the byte cap.
func TestL1_EntryBudget_EvictsOldestAt200K(t *testing.T) {
	resetL1Counters()
	t.Setenv(envL1MaxBytes, "10737418240") // 10 GiB — never trigger
	t.Setenv(envL1MaxEntries, "10")        // tight cap

	c := NewMem(time.Hour)
	ctx := context.Background()

	// 15 entries × 16 bytes each = 240 bytes (well under byte cap), but
	// 15 > 10 entry cap.
	for i := 0; i < 15; i++ {
		k := "k-" + strconv.Itoa(i)
		if err := c.SetRaw(ctx, k, fillBytes(16)); err != nil {
			t.Fatalf("SetRaw: %v", err)
		}
		time.Sleep(time.Millisecond)
	}

	if got := c.L1EntryCount(); got != 15 {
		t.Errorf("pre-sweep entryCount: got %d, want 15", got)
	}

	c.sweepLRUIfOverBudget()

	// Target = 0.9 × 10 = 9 entries.
	if got := c.L1EntryCount(); got > 9 {
		t.Errorf("post-sweep entryCount: got %d, want <= 9", got)
	}
	if got := GlobalMetrics.L1EvictionsLRU.Load(); got < 6 {
		t.Errorf("L1EvictionsLRU: got %d, want >= 6", got)
	}

	// k-14 (newest) survives; k-0 (oldest) evicted.
	if _, ok, _ := c.GetRaw(ctx, "k-14"); !ok {
		t.Errorf("k-14 evicted; want survived")
	}
	if _, ok, _ := c.GetRaw(ctx, "k-0"); ok {
		t.Errorf("k-0 survived; want evicted")
	}
}

// TestL1_LRUOrdering_GetRefreshesAccess writes 10 entries, then GetRaw on
// the OLDEST (which would normally be evicted first), then triggers a
// sweep. The previously-touched entry must survive while a non-touched
// older entry is evicted instead.
func TestL1_LRUOrdering_GetRefreshesAccess(t *testing.T) {
	resetL1Counters()
	// 1024 byte cap, 128-byte entries → 8 fit. Write 10, touch the
	// oldest, sweep.
	t.Setenv(envL1MaxBytes, "1024")
	t.Setenv(envL1MaxEntries, "100000")

	c := NewMem(time.Hour)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if err := c.SetRaw(ctx, "k-"+strconv.Itoa(i), fillBytes(128)); err != nil {
			t.Fatalf("SetRaw: %v", err)
		}
		time.Sleep(time.Millisecond)
	}

	// Touch k-0 (the oldest) — this should refresh its lastAccess.
	time.Sleep(time.Millisecond)
	if _, ok, _ := c.GetRaw(ctx, "k-0"); !ok {
		t.Fatalf("k-0 missing pre-touch")
	}

	c.sweepLRUIfOverBudget()

	// k-0 was just touched → must survive. k-1 should be the new oldest
	// and is the first eviction candidate.
	if _, ok, _ := c.GetRaw(ctx, "k-0"); !ok {
		t.Errorf("k-0 (touched) evicted; LRU touch did not refresh access")
	}
	if _, ok, _ := c.GetRaw(ctx, "k-1"); ok {
		t.Errorf("k-1 (oldest non-touched) survived; LRU should have evicted it before k-0")
	}
}

// TestL1_TTLAndLRU_BothFire confirms that TTL eviction works independently
// of byte budget — entries past TTL evict on the StartEviction tick (here
// invoked manually), and the L1EvictionsTTL counter increments.
func TestL1_TTLAndLRU_BothFire(t *testing.T) {
	resetL1Counters()
	t.Setenv(envL1MaxBytes, "10737418240")
	t.Setenv(envL1MaxEntries, "100000")

	c := NewMem(0)
	ctx := context.Background()

	// Write 5 entries with 50ms TTL.
	for i := 0; i < 5; i++ {
		if err := c.SetWithTTL(ctx, "ttl-"+strconv.Itoa(i), "value", 50*time.Millisecond); err != nil {
			t.Fatalf("SetWithTTL: %v", err)
		}
	}
	if got := c.L1EntryCount(); got != 5 {
		t.Errorf("pre-expiry entryCount: got %d, want 5", got)
	}

	// Wait for TTL to lapse, then read each — the lazy GetRaw expiry
	// path should evict and increment L1EvictionsTTL.
	time.Sleep(80 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if _, ok, _ := c.GetRaw(ctx, "ttl-"+strconv.Itoa(i)); ok {
			t.Errorf("ttl-%d not evicted past TTL", i)
		}
	}
	if got := GlobalMetrics.L1EvictionsTTL.Load(); got < 5 {
		t.Errorf("L1EvictionsTTL: got %d, want >= 5", got)
	}
	// All 5 should be gone — entry counter back to 0.
	if got := c.L1EntryCount(); got != 0 {
		t.Errorf("post-expiry entryCount: got %d, want 0", got)
	}
}

// TestL1_ResidentBytes_TracksDeletes verifies that explicit Delete() and
// LRU/TTL eviction all decrement residentBytes/entryCount correctly. A
// counter drift here would defeat the byte-budget sweep.
func TestL1_ResidentBytes_TracksDeletes(t *testing.T) {
	resetL1Counters()
	c := NewMem(time.Hour)
	ctx := context.Background()

	// Insert 3 known-size entries.
	if err := c.SetRaw(ctx, "a", fillBytes(100)); err != nil {
		t.Fatal(err)
	}
	if err := c.SetRaw(ctx, "b", fillBytes(200)); err != nil {
		t.Fatal(err)
	}
	if err := c.SetRaw(ctx, "c", fillBytes(300)); err != nil {
		t.Fatal(err)
	}

	if got := c.L1ResidentBytes(); got != 600 {
		t.Errorf("after 3 inserts: residentBytes=%d, want 600", got)
	}
	if got := c.L1EntryCount(); got != 3 {
		t.Errorf("after 3 inserts: entryCount=%d, want 3", got)
	}

	// Delete one — counter must shrink by exactly that entry's size.
	if err := c.Delete(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if got := c.L1ResidentBytes(); got != 400 {
		t.Errorf("after delete b: residentBytes=%d, want 400 (600-200)", got)
	}
	if got := c.L1EntryCount(); got != 2 {
		t.Errorf("after delete b: entryCount=%d, want 2", got)
	}

	// Replace 'a' with a smaller value — counter delta must reflect
	// new - old (100 → 50, delta -50).
	if err := c.SetRaw(ctx, "a", fillBytes(50)); err != nil {
		t.Fatal(err)
	}
	if got := c.L1ResidentBytes(); got != 350 {
		t.Errorf("after replace a (100→50): residentBytes=%d, want 350", got)
	}
	if got := c.L1EntryCount(); got != 2 {
		t.Errorf("after replace a: entryCount=%d, want 2 (no count change)", got)
	}

	// Delete remaining keys — counters must be 0.
	if err := c.Delete(ctx, "a", "c"); err != nil {
		t.Fatal(err)
	}
	if got := c.L1ResidentBytes(); got != 0 {
		t.Errorf("after delete a,c: residentBytes=%d, want 0", got)
	}
	if got := c.L1EntryCount(); got != 0 {
		t.Errorf("after delete a,c: entryCount=%d, want 0", got)
	}
}

// TestL1_Snapshot_ExposesL1Gauges confirms that MetricsSnapshot returns
// resident_bytes / entries / max_bytes / max_entries / evictions_lru /
// evictions_ttl after RegisterL1Sampler is wired. This pins the
// /metrics/runtime contract from the cache side (the handler-side test
// lives in internal/handlers/runtime_test.go).
func TestL1_Snapshot_ExposesL1Gauges(t *testing.T) {
	resetL1Counters()
	t.Setenv(envL1MaxBytes, "12345")
	t.Setenv(envL1MaxEntries, "678")

	c := NewMem(time.Hour)
	RegisterL1Sampler(func() (int64, int64) {
		return c.L1ResidentBytes(), c.L1EntryCount()
	})
	t.Cleanup(func() {
		// Restore a no-op sampler so other tests don't see this MemCache.
		RegisterL1Sampler(func() (int64, int64) { return 0, 0 })
	})

	if err := c.SetRaw(context.Background(), "x", fillBytes(42)); err != nil {
		t.Fatal(err)
	}

	snap := GlobalMetrics.Snapshot()
	if snap.L1MaxBytes != 12345 {
		t.Errorf("L1MaxBytes: got %d, want 12345", snap.L1MaxBytes)
	}
	if snap.L1MaxEntries != 678 {
		t.Errorf("L1MaxEntries: got %d, want 678", snap.L1MaxEntries)
	}
	if snap.L1ResidentBytes != 42 {
		t.Errorf("L1ResidentBytes: got %d, want 42", snap.L1ResidentBytes)
	}
	if snap.L1Entries != 1 {
		t.Errorf("L1Entries: got %d, want 1", snap.L1Entries)
	}
}
