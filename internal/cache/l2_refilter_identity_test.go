// Q-COMP-LIST-IDENTITY — L2 reduction-ratio gate bypass tests.
//
// The reduction-ratio gate (default 0.20) refuses entries whose post-
// refilter size is too close to the L1 raw size, on the grounds that
// caching them adds memory pressure without saving CPU. For identity
// wrappers (no UAF, no childRefs), refilter is a structural no-op:
// post-refilter ≈ L1 is the EXPECTED outcome, and caching the wire
// bytes IS the win we want. The bypass makes that possible.
//
// PM acceptance gates covered here:
//
//   - Test_L2_BypassReductionRatio_WhenFlagTrue
//     identity-flagged entries with ratio > 0.80 ARE cached + counter
//     L2WritesIdentityBypass increments
//   - Test_L2_StillRejectsSizeCap_WhenFlagTrue
//     identity-flagged entries that exceed l2MaxEntryBytes are STILL
//     rejected (size cap is a hard memory-safety limit unrelated to
//     the ratio policy)
//   - Test_L2_NonIdentity_StillGated
//     non-identity entries (isRefilterIdentity=false) behave exactly
//     like L2Put — the bypass is gated on the wrapper flag, not the
//     user
//   - Test_L2_HitCounter_IdentityCohort
//     L2Get against an entry written via the bypass increments
//     L2HitsIdentityCohort, surfacing the steady-state win on
//     /metrics/runtime
package cache

import (
	"bytes"
	"testing"
)

func TestL2_BypassReductionRatio_WhenFlagTrue(t *testing.T) {
	enableL2(t)

	preBypass := GlobalMetrics.L2WritesIdentityBypass.Load()
	preWrites := GlobalMetrics.L2Writes.Load()
	preSkipped := GlobalMetrics.L2SkippedHighRatio.Load()

	l1Key := "snowplow:resolved:idA:g/v/r:ns:compositions-list"
	identity := "idA"
	groupsHash := "g1"

	// Refilter output ≈ 95% of L1 raw size — the standard gate would
	// SKIP this (ratio 0.95 > 0.80). The identity bypass MUST cache it.
	bigRefilter := bytes.Repeat([]byte("X"), 950)
	rawL1Size := 1000

	L2PutWithIdentityHint(l1Key, identity, groupsHash, bigRefilter, nil, false, "v3", rawL1Size, true)

	if e, ok := L2Get(l1Key, identity, groupsHash); !ok {
		t.Fatalf("identity-flagged entry must be cached even at ratio 0.95")
	} else if !bytes.Equal(e.Refiltered, bigRefilter) {
		t.Fatalf("cached bytes diverge from input: got %q, want %q", e.Refiltered, bigRefilter)
	}

	if got := GlobalMetrics.L2WritesIdentityBypass.Load(); got != preBypass+1 {
		t.Fatalf("L2WritesIdentityBypass: got %d, want %d", got, preBypass+1)
	}
	if got := GlobalMetrics.L2Writes.Load(); got != preWrites+1 {
		t.Fatalf("L2Writes: got %d, want %d", got, preWrites+1)
	}
	if got := GlobalMetrics.L2SkippedHighRatio.Load(); got != preSkipped {
		t.Fatalf("L2SkippedHighRatio MUST NOT advance on bypass: got %d, want %d", got, preSkipped)
	}
}

func TestL2_StillRejectsSizeCap_WhenFlagTrue(t *testing.T) {
	enableL2(t)
	t.Setenv(envL2MaxEntryBytes, "1024") // 1 KB cap for this test

	preBypass := GlobalMetrics.L2WritesIdentityBypass.Load()
	preSkipCap := GlobalMetrics.L2SkippedSizeCap.Load()
	preWrites := GlobalMetrics.L2Writes.Load()

	l1Key := "snowplow:resolved:idA:g/v/r:ns:compositions-list"
	identity := "idA"
	groupsHash := "g1"

	huge := bytes.Repeat([]byte("Z"), 4096) // 4 KB > 1 KB cap

	L2PutWithIdentityHint(l1Key, identity, groupsHash, huge, nil, false, "v3", 1<<20, true)

	if _, ok := L2Get(l1Key, identity, groupsHash); ok {
		t.Fatalf("oversize entry must STILL be rejected even with identity bypass — size cap is a hard memory-safety limit")
	}
	if got := GlobalMetrics.L2SkippedSizeCap.Load(); got != preSkipCap+1 {
		t.Fatalf("L2SkippedSizeCap MUST advance on size-cap rejection: got %d, want %d", got, preSkipCap+1)
	}
	if got := GlobalMetrics.L2WritesIdentityBypass.Load(); got != preBypass {
		t.Fatalf("L2WritesIdentityBypass MUST NOT advance when the size cap rejects the entry: got %d, want %d", got, preBypass)
	}
	if got := GlobalMetrics.L2Writes.Load(); got != preWrites {
		t.Fatalf("L2Writes MUST NOT advance on size-cap rejection: got %d, want %d", got, preWrites)
	}
}

func TestL2_NonIdentity_StillGated(t *testing.T) {
	enableL2(t)

	preSkipped := GlobalMetrics.L2SkippedHighRatio.Load()
	preBypass := GlobalMetrics.L2WritesIdentityBypass.Load()

	l1Key := "snowplow:resolved:idA:g/v/r:ns:foo"
	identity := "idA"
	groupsHash := "g1"

	// Same high-ratio entry as above, but isRefilterIdentity=false.
	// The reduction-ratio gate MUST run and reject the entry — this is
	// the policy preserved for non-identity wrappers.
	bigRefilter := bytes.Repeat([]byte("Y"), 950)
	L2PutWithIdentityHint(l1Key, identity, groupsHash, bigRefilter, nil, false, "v3", 1000, false)

	if _, ok := L2Get(l1Key, identity, groupsHash); ok {
		t.Fatalf("non-identity high-ratio entry must be REJECTED by the standard gate")
	}
	if got := GlobalMetrics.L2SkippedHighRatio.Load(); got != preSkipped+1 {
		t.Fatalf("L2SkippedHighRatio MUST advance for non-identity high-ratio entry: got %d, want %d", got, preSkipped+1)
	}
	if got := GlobalMetrics.L2WritesIdentityBypass.Load(); got != preBypass {
		t.Fatalf("L2WritesIdentityBypass MUST NOT advance for non-identity entries: got %d, want %d", got, preBypass)
	}
}

func TestL2_HitCounter_IdentityCohort(t *testing.T) {
	enableL2(t)

	l1Key := "snowplow:resolved:idA:g/v/r:ns:compositions-list"
	identity := "idA"
	groupsHash := "g1"

	// Write via the bypass.
	payload := []byte(`{"status":{"list":[]}}`)
	L2PutWithIdentityHint(l1Key, identity, groupsHash, payload, nil, false, "v3", 100, true)

	preIdHits := GlobalMetrics.L2HitsIdentityCohort.Load()
	if _, ok := L2Get(l1Key, identity, groupsHash); !ok {
		t.Fatalf("expected hit on identity-bypass entry")
	}
	if got := GlobalMetrics.L2HitsIdentityCohort.Load(); got != preIdHits+1 {
		t.Fatalf("L2HitsIdentityCohort: got %d, want %d", got, preIdHits+1)
	}

	// Sanity: a non-identity entry should NOT advance the identity-cohort
	// hit counter.
	otherKey := "snowplow:resolved:idB:g/v/r:ns:foo"
	otherID := "idB"
	L2PutWithIdentityHint(otherKey, otherID, groupsHash, []byte("small"), nil, false, "v3", 1000, false)

	preIdHits = GlobalMetrics.L2HitsIdentityCohort.Load()
	if _, ok := L2Get(otherKey, otherID, groupsHash); !ok {
		t.Fatalf("expected hit on non-identity entry (low ratio)")
	}
	if got := GlobalMetrics.L2HitsIdentityCohort.Load(); got != preIdHits {
		t.Fatalf("L2HitsIdentityCohort MUST NOT advance for non-identity entries: got %d, want %d", got, preIdHits)
	}
}

func TestL2_PutWithIdentityHint_DisabledShortCircuits(t *testing.T) {
	defer FlushL2ForTest()
	SetL2EnabledForTest(false)
	defer SetL2EnabledForTest(true) // restore for downstream tests

	preBypass := GlobalMetrics.L2WritesIdentityBypass.Load()
	preWrites := GlobalMetrics.L2Writes.Load()

	L2PutWithIdentityHint("k", "id", "g", []byte("x"), nil, false, "v3", 100, true)

	if got := GlobalMetrics.L2WritesIdentityBypass.Load(); got != preBypass {
		t.Fatalf("disabled L2 must not increment counters: bypass got %d", got)
	}
	if got := GlobalMetrics.L2Writes.Load(); got != preWrites {
		t.Fatalf("disabled L2 must not increment counters: writes got %d", got)
	}
}
