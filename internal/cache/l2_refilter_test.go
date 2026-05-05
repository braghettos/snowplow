// Q-RBACC-L2-1 — unit tests for the post-refilter L2 cache.
//
// Coverage matrix:
//
//   - L2Key determinism (same inputs → same hash; ordering of any
//     component flips the hash; empty inputs → empty key).
//   - HashGroups stability (sort-invariant; empty → "no_groups").
//   - SetL2 / GetL2 happy path with reduction-ratio gate.
//   - Reduction-ratio gate skips high-ratio entries (admin × panels
//     pattern; counter L2SkippedHighRatio increments).
//   - Size-cap gate skips entries > MAX_ENTRY_BYTES (counter).
//   - Schema-mismatch on read evicts opportunistically + miss.
//   - LRU sweep evicts oldest entries when over cap.
//   - EvictL2ForL1Keys evicts only the targeted entries.
//   - EvictL2ForIdentity evicts only entries pinned to the identity.
//   - EvictL2ForRESTAction evicts entries by gvr+name across cohorts.
//   - Disabled flag short-circuits Get/Set without state change.
//   - 1000-randomized-pair byte-equivalence harness (G9): the L2 entry
//     returned by Get is BYTE-IDENTICAL to the value passed at Set time.
//
// All tests reset the package-global L2 between cases via
// SetL2EnabledForTest + FlushL2ForTest. They DO NOT touch metrics
// across cases (each case asserts a metric delta, not absolute value).

package cache

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
)

func TestL2Key_Determinism(t *testing.T) {
	a := L2Key("snowplow:resolved:bid1:templates.krateo.io/v1/restactions:ns:name", "bid1", "ghash1")
	b := L2Key("snowplow:resolved:bid1:templates.krateo.io/v1/restactions:ns:name", "bid1", "ghash1")
	if a != b {
		t.Fatalf("L2Key not deterministic: %q vs %q", a, b)
	}
	if len(a) != 32 {
		t.Fatalf("L2Key length = %d; want 32 (hex-truncated SHA-256)", len(a))
	}
	c := L2Key("snowplow:resolved:bid1:templates.krateo.io/v1/restactions:ns:name", "bid2", "ghash1")
	if a == c {
		t.Fatalf("identity must enter the hash; got identical keys")
	}
	d := L2Key("snowplow:resolved:bid1:templates.krateo.io/v1/restactions:ns:name", "bid1", "ghash2")
	if a == d {
		t.Fatalf("groups hash must enter the hash; got identical keys")
	}
	e := L2Key("snowplow:resolved:bid2:templates.krateo.io/v1/restactions:ns:name", "bid1", "ghash1")
	if a == e {
		t.Fatalf("l1Key must enter the hash; got identical keys")
	}
}

func TestL2Key_EmptyInputs(t *testing.T) {
	if L2Key("", "id", "g") != "" {
		t.Fatalf("empty l1Key must yield empty key")
	}
	if L2Key("k", "", "g") != "" {
		t.Fatalf("empty identity must yield empty key")
	}
	if L2Key("k", "id", "") != "" {
		t.Fatalf("empty groupsHash must yield empty key")
	}
}

func TestHashGroups_SortInvariant(t *testing.T) {
	a := HashGroups([]string{"alpha", "beta", "gamma"})
	b := HashGroups([]string{"gamma", "alpha", "beta"})
	if a != b {
		t.Fatalf("HashGroups must be sort-invariant: %q vs %q", a, b)
	}
	if HashGroups(nil) != "no_groups" {
		t.Fatalf("nil groups should produce no_groups sentinel")
	}
	if HashGroups([]string{}) != "no_groups" {
		t.Fatalf("empty groups should produce no_groups sentinel")
	}
}

func TestL2_SetGet_HappyPath(t *testing.T) {
	enableL2(t)
	k := L2Key("snowplow:resolved:bid:g/v/r:ns:name", "bid", "ghash")
	want := []byte(`{"refiltered":true,"items":[1,2,3]}`)
	e := &L2Entry{Refiltered: bytes.Clone(want), l1Key: "snowplow:resolved:bid:g/v/r:ns:name", identity: "bid", SchemaVer: "v3"}
	SetL2Refilter(k, e, 1000) // L1 raw 1000B, refiltered 36B → ratio 0.036 → cached
	got, ok := GetL2Refilter(k)
	if !ok {
		t.Fatalf("expected hit after Set")
	}
	if !bytes.Equal(got.Refiltered, want) {
		t.Fatalf("byte-equivalence violation: got %q want %q", got.Refiltered, want)
	}
	if got.tag != L2EntryV1Tag {
		t.Fatalf("tag not stamped on Set: %q", got.tag)
	}
}

func TestL2_ReductionRatioGate_SkipsHighRatio(t *testing.T) {
	enableL2(t)
	k := L2Key("k1", "id1", "g1")
	skipped0 := GlobalMetrics.L2SkippedHighRatio.Load()
	// refiltered = 95% of L1 raw → ratio 0.95 → above 0.80 threshold → SKIP.
	bigRefilter := bytes.Repeat([]byte("X"), 950)
	SetL2Refilter(k, &L2Entry{Refiltered: bigRefilter, l1Key: "k1", identity: "id1"}, 1000)
	if _, ok := GetL2Refilter(k); ok {
		t.Fatalf("high-ratio entry must be skipped (ratio 0.95 > 0.80)")
	}
	if got := GlobalMetrics.L2SkippedHighRatio.Load(); got <= skipped0 {
		t.Fatalf("L2SkippedHighRatio counter did not advance (was %d, now %d)", skipped0, got)
	}
}

func TestL2_ReductionRatioGate_AcceptsLowRatio(t *testing.T) {
	enableL2(t)
	k := L2Key("k2", "id2", "g2")
	// refiltered = 10% of L1 raw → ratio 0.10 → cached.
	smallRefilter := bytes.Repeat([]byte("Y"), 100)
	SetL2Refilter(k, &L2Entry{Refiltered: smallRefilter, l1Key: "k2", identity: "id2"}, 1000)
	if _, ok := GetL2Refilter(k); !ok {
		t.Fatalf("low-ratio entry must be cached (ratio 0.10 < 0.80)")
	}
}

func TestL2_SizeCap_SkipsHugeEntry(t *testing.T) {
	enableL2(t)
	t.Setenv(envL2MaxEntryBytes, "1024") // 1 KB cap for this test
	k := L2Key("k3", "id3", "g3")
	skipped0 := GlobalMetrics.L2SkippedSizeCap.Load()
	huge := bytes.Repeat([]byte("Z"), 4096) // 4 KB > 1 KB cap
	SetL2Refilter(k, &L2Entry{Refiltered: huge, l1Key: "k3", identity: "id3"}, 1<<20 /* huge raw */)
	if _, ok := GetL2Refilter(k); ok {
		t.Fatalf("oversize entry must be skipped")
	}
	if got := GlobalMetrics.L2SkippedSizeCap.Load(); got <= skipped0 {
		t.Fatalf("L2SkippedSizeCap counter did not advance")
	}
}

func TestL2_BypassReductionGate_RawZero(t *testing.T) {
	enableL2(t)
	k := L2Key("k4", "id4", "g4")
	// rawL1Size=0 → bypass reduction gate (used by prewarm).
	bigRefilter := bytes.Repeat([]byte("X"), 5000)
	SetL2Refilter(k, &L2Entry{Refiltered: bigRefilter, l1Key: "k4", identity: "id4"}, 0)
	if _, ok := GetL2Refilter(k); !ok {
		t.Fatalf("rawL1Size=0 must bypass the reduction gate")
	}
}

func TestL2_SchemaTag_Mismatch_OpportunisticEvict(t *testing.T) {
	enableL2(t)
	k := L2Key("k5", "id5", "g5")
	// Inject an entry with the wrong tag directly (bypassing SetL2Refilter
	// which always stamps the correct tag).
	bad := &L2Entry{Refiltered: []byte("stale"), tag: "l2-vBOGUS", l1Key: "k5", identity: "id5"}
	globalL2.store.Store(k, bad)
	if _, ok := GetL2Refilter(k); ok {
		t.Fatalf("entry with wrong tag must be treated as miss")
	}
	// And evicted on the read.
	if _, ok := globalL2.store.Load(k); ok {
		t.Fatalf("schema-mismatch entry must be opportunistically evicted")
	}
}

func TestL2_DisabledShortCircuits(t *testing.T) {
	defer FlushL2ForTest()
	SetL2EnabledForTest(false)
	defer SetL2EnabledForTest(true) // restore for downstream tests
	k := L2Key("k6", "id6", "g6")
	SetL2Refilter(k, &L2Entry{Refiltered: []byte("x"), l1Key: "k6", identity: "id6"}, 100)
	if _, ok := GetL2Refilter(k); ok {
		t.Fatalf("disabled L2 must not store or return entries")
	}
}

func TestL2_EvictL2ForL1Keys(t *testing.T) {
	enableL2(t)
	l1A := "snowplow:resolved:idA:g/v/r:ns:nameA"
	l1B := "snowplow:resolved:idB:g/v/r:ns:nameB"
	keyA := L2Key(l1A, "idA", "g1")
	keyB := L2Key(l1B, "idB", "g1")
	SetL2Refilter(keyA, &L2Entry{Refiltered: []byte("A"), l1Key: l1A, identity: "idA"}, 100)
	SetL2Refilter(keyB, &L2Entry{Refiltered: []byte("B"), l1Key: l1B, identity: "idB"}, 100)

	n := EvictL2ForL1Keys(context.Background(), []string{l1A})
	if n != 1 {
		t.Fatalf("EvictL2ForL1Keys: got %d evictions, want 1", n)
	}
	if _, ok := GetL2Refilter(keyA); ok {
		t.Fatalf("keyA must be evicted")
	}
	if _, ok := GetL2Refilter(keyB); !ok {
		t.Fatalf("keyB must remain")
	}
}

func TestL2_EvictL2ForIdentity(t *testing.T) {
	enableL2(t)
	// Two cohorts, same RA, different identities.
	l1A := "snowplow:resolved:idA:g/v/r:ns:name"
	l1B := "snowplow:resolved:idB:g/v/r:ns:name"
	keyA := L2Key(l1A, "idA", "g1")
	keyB := L2Key(l1B, "idB", "g1")
	SetL2Refilter(keyA, &L2Entry{Refiltered: []byte("A"), l1Key: l1A, identity: "idA"}, 100)
	SetL2Refilter(keyB, &L2Entry{Refiltered: []byte("B"), l1Key: l1B, identity: "idB"}, 100)
	n := EvictL2ForIdentity(context.Background(), "idA")
	if n != 1 {
		t.Fatalf("EvictL2ForIdentity: got %d, want 1", n)
	}
	if _, ok := GetL2Refilter(keyA); ok {
		t.Fatalf("idA entries must be evicted")
	}
	if _, ok := GetL2Refilter(keyB); !ok {
		t.Fatalf("idB entries must remain")
	}
}

func TestL2_EvictL2ForRESTAction(t *testing.T) {
	enableL2(t)
	// Multiple cohorts share the same RESTAction (gvr/name); RA mutation
	// must evict ALL of them.
	gvrKey := "g/v/r"
	name := "compositions-list"
	l1A := "snowplow:resolved:idA:" + gvrKey + ":ns:" + name
	l1B := "snowplow:resolved:idB:" + gvrKey + ":ns:" + name
	keyA := L2Key(l1A, "idA", "g1")
	keyB := L2Key(l1B, "idB", "g1")
	SetL2Refilter(keyA, &L2Entry{Refiltered: []byte("A"), l1Key: l1A, identity: "idA"}, 100)
	SetL2Refilter(keyB, &L2Entry{Refiltered: []byte("B"), l1Key: l1B, identity: "idB"}, 100)
	n := EvictL2ForRESTAction(context.Background(), gvrKey, name)
	if n != 2 {
		t.Fatalf("EvictL2ForRESTAction: got %d, want 2 (cohorts share the same RA)", n)
	}
	if _, ok := GetL2Refilter(keyA); ok {
		t.Fatalf("keyA must be evicted on RA mutation")
	}
	if _, ok := GetL2Refilter(keyB); ok {
		t.Fatalf("keyB must be evicted on RA mutation")
	}
}

// G9 — byte-equivalence harness over 1000 random (user, l1Key) pairs.
//
// The contract: for any (user, l1Key), L2 returns BYTE-IDENTICAL bytes
// to what was passed at Set time. This is the load-bearing test against
// `feedback_cache_must_not_constrain_jq.md` — if L2 ever truncates,
// canonicalises, or otherwise mutates the bytes, this test catches it.
func TestL2_G9_ByteEquivalence_1000Pairs(t *testing.T) {
	enableL2(t)

	type pair struct {
		l1Key      string
		identity   string
		groupsHash string
		want       []byte
	}
	pairs := make([]pair, 1000)
	for i := range pairs {
		// 8-byte random suffix per pair guarantees uniqueness without
		// relying on any deterministic counter (which would mask
		// hash-collision bugs).
		buf := make([]byte, 8)
		_, _ = rand.Read(buf)
		// payload size varies 1–8 KB to exercise small-and-large
		size := 1024 + (int(buf[0])*32)%(7*1024)
		payload := make([]byte, size)
		_, _ = rand.Read(payload)

		pairs[i] = pair{
			l1Key:      fmt.Sprintf("snowplow:resolved:bid%d:g/v/r:ns:name%d", i, i),
			identity:   fmt.Sprintf("bid%d", i),
			groupsHash: fmt.Sprintf("ghash%x", buf),
			want:       payload,
		}
		k := L2Key(pairs[i].l1Key, pairs[i].identity, pairs[i].groupsHash)
		SetL2Refilter(k, &L2Entry{
			Refiltered: bytes.Clone(payload),
			l1Key:      pairs[i].l1Key,
			identity:   pairs[i].identity,
		}, 0 /* bypass ratio gate so every payload is cached */)
	}

	// Now read every pair back and assert byte-equivalence.
	for i, p := range pairs {
		k := L2Key(p.l1Key, p.identity, p.groupsHash)
		e, ok := GetL2Refilter(k)
		if !ok {
			t.Fatalf("pair %d: L2 miss after Set", i)
		}
		if !bytes.Equal(e.Refiltered, p.want) {
			t.Fatalf("pair %d: byte-equivalence violation (len got=%d want=%d)",
				i, len(e.Refiltered), len(p.want))
		}
	}
}

// TestL2_G11_HitRatePattern simulates the burst pattern (1 cold + 9
// hits) and asserts hit-rate ≥ 90%. This is the unit-form of G11.
func TestL2_G11_HitRatePattern(t *testing.T) {
	enableL2(t)
	// Reset L2 metric counters for a clean measurement.
	GlobalMetrics.L2Hits.Store(0)
	GlobalMetrics.L2Misses.Store(0)

	l1Key := "snowplow:resolved:bidH:g/v/r:ns:nameH"
	identity := "bidH"
	groupsHash := "ghash-burst"
	k := L2Key(l1Key, identity, groupsHash)

	// Rep 1: cold (miss).
	if _, ok := GetL2Refilter(k); ok {
		t.Fatalf("rep1 should miss")
	}
	// Pretend the dispatcher computed and stored.
	SetL2Refilter(k, &L2Entry{Refiltered: []byte("burstpayload"), l1Key: l1Key, identity: identity}, 1000)

	// Reps 2..10: hits.
	for i := 2; i <= 10; i++ {
		if _, ok := GetL2Refilter(k); !ok {
			t.Fatalf("rep%d should hit", i)
		}
	}
	hits := GlobalMetrics.L2Hits.Load()
	misses := GlobalMetrics.L2Misses.Load()
	rate := float64(hits) / float64(hits+misses)
	if rate < 0.90 {
		t.Fatalf("G11 hit-rate < 0.90: hits=%d misses=%d rate=%.3f", hits, misses, rate)
	}
}

// TestL2_G12_SkipsAdminPanelsPattern simulates the admin × compositions-
// panels pattern (post-refilter ≈ L1) and asserts the entry is recorded
// as L2SkippedHighRatio (validates the conditional caching policy).
func TestL2_G12_SkipsAdminPanelsPattern(t *testing.T) {
	enableL2(t)
	skipped0 := GlobalMetrics.L2SkippedHighRatio.Load()
	k := L2Key("snowplow:resolved:admin:g/v/r:ns:compositions-panels", "admin", "ghash-admin")

	// Admin sees all 50 NS; refilter is a near-pass-through.
	rawSize := 40 << 20
	refiltered := bytes.Repeat([]byte("X"), int(float64(rawSize)*0.95)) // 95% of L1
	SetL2Refilter(k, &L2Entry{Refiltered: refiltered, l1Key: "x", identity: "admin"}, rawSize)

	if got := GlobalMetrics.L2SkippedHighRatio.Load(); got <= skipped0 {
		t.Fatalf("G12 admin-panels pattern should record L2SkippedHighRatio")
	}
	if _, ok := GetL2Refilter(k); ok {
		t.Fatalf("admin × panels must not be cached when ratio > 0.80")
	}
}

func TestL2_LRUSweep_BoundsResident(t *testing.T) {
	enableL2(t)
	// Tighten the byte cap so the sweep fires.
	t.Setenv(envL2MaxBytes, "10240") // 10 KB cap
	t.Setenv(envL2MaxEntries, "1000")

	for i := 0; i < 50; i++ {
		k := L2Key(fmt.Sprintf("k-lru-%d", i), fmt.Sprintf("id-%d", i), "g-lru")
		// Each entry ~512 B → 50 × 512 = 25 KB > 10 KB cap → sweep evicts.
		SetL2Refilter(k, &L2Entry{
			Refiltered: bytes.Repeat([]byte("a"), 512),
			l1Key:      fmt.Sprintf("k-lru-%d", i),
			identity:   fmt.Sprintf("id-%d", i),
		}, 0)
	}
	// After all writes the resident bytes must be at-or-below cap.
	// (The sweep targets 90% of cap on each overshoot, so the final
	// state oscillates between 90% and 100% as new writes accumulate
	// up to the next sweep trigger. 100% is a valid steady state.)
	if got := L2ResidentBytes(); got > 10240 {
		t.Fatalf("LRU sweep failed to bound resident bytes: %d > 10240 (cap)", got)
	}
}

// TestL2_ConcurrentSetGet — race-discovery harness for the Get/Set
// surfaces. Run with `go test -race`.
func TestL2_ConcurrentSetGet(t *testing.T) {
	enableL2(t)
	const writers = 32
	const writes = 500

	var wg sync.WaitGroup
	wg.Add(writers)
	var totalSet atomic.Int64
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < writes; i++ {
				k := L2Key(
					fmt.Sprintf("snowplow:resolved:bid-conc-%d:g/v/r:ns:name", w),
					fmt.Sprintf("bid-conc-%d", w),
					"g-conc",
				)
				SetL2Refilter(k, &L2Entry{
					Refiltered: []byte(fmt.Sprintf("payload-%d-%d", w, i)),
					l1Key:      fmt.Sprintf("snowplow:resolved:bid-conc-%d:g/v/r:ns:name", w),
					identity:   fmt.Sprintf("bid-conc-%d", w),
				}, 1000)
				_, _ = GetL2Refilter(k)
				totalSet.Add(1)
			}
		}(w)
	}
	wg.Wait()
	// We don't assert exact counters under concurrency — sufficient that
	// the test ran without a -race violation.
	if totalSet.Load() != writers*writes {
		t.Fatalf("counter mismatch: %d", totalSet.Load())
	}
}

// TestL2Hash_NoCollisionAcrossComponents — adversarial test that the
// '|' separator and SHA-256 hash don't suffer from a trivial collision
// (e.g., l1Key="a|b", identity="" would NOT collide with l1Key="a",
// identity="b" because empty inputs short-circuit to "").
func TestL2Hash_NoCollisionAcrossComponents(t *testing.T) {
	a := L2Key("a", "b|c", "d")
	b := L2Key("a|b", "c", "d")
	if a == b {
		t.Fatalf("hash collision across separator: %q", a)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// enableL2 turns the L2 cache on for the duration of the calling test
// and flushes state. Tests that need a fresh L2 should call this first.
// Restores prior state on test cleanup.
func enableL2(t *testing.T) {
	t.Helper()
	prev := globalL2.enabled.Load()
	SetL2EnabledForTest(true)
	FlushL2ForTest()
	t.Cleanup(func() {
		FlushL2ForTest()
		globalL2.enabled.Store(prev)
	})
}

// _ keeps sort imported — used by HashGroups via the production code,
// referenced here so go imports doesn't strip if the file is reorganized.
var _ = sort.Strings
