// Q-RBACC-L2-1 architect-review CONCERN-2 (2026-05-05) — purge ordering.
//
// purgeUserCacheData MUST evict L2 before any L1 cache.Delete fires.
// apiref reads L2 before L1, so L1-then-L2 ordering exposes a stale-L2
// window where the user observes refiltered bytes against the OLD
// binding even though L1 was already cleared.
//
// This test wraps a MemCache with an orderingCache decorator that
// records (operation, time-of-call). For each cache.Delete invocation,
// the decorator records a snapshot of L2ResidentBytes for the test
// identity. The invariant: at every Delete event, the L2 entries for
// the test identity are already evicted.

package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// orderingCache wraps a MemCache and records the L2 resident-count
// snapshot at every Delete invocation. Used to assert the ordering
// invariant: L2 evict precedes L1 delete in purgeUserCacheData.
type orderingCache struct {
	*MemCache
	deletes []deleteSnapshot
}

type deleteSnapshot struct {
	t              time.Time
	keys           []string
	l2ResidentByID int64 // L2 byIdentity bucket size for the test identity
}

func (oc *orderingCache) Delete(ctx context.Context, keys ...string) error {
	cnt := byIdentitySize(testIdentityForOrdering)
	oc.deletes = append(oc.deletes, deleteSnapshot{
		t:              time.Now(),
		keys:           append([]string(nil), keys...),
		l2ResidentByID: cnt,
	})
	return oc.MemCache.Delete(ctx, keys...)
}

func (oc *orderingCache) DeleteUserRBAC(ctx context.Context, username string) error {
	cnt := byIdentitySize(testIdentityForOrdering)
	oc.deletes = append(oc.deletes, deleteSnapshot{
		t:              time.Now(),
		keys:           []string{"<DeleteUserRBAC:" + username + ">"},
		l2ResidentByID: cnt,
	})
	return oc.MemCache.DeleteUserRBAC(ctx, username)
}

const testIdentityForOrdering = "test-bid-ordering"

// byIdentitySize returns the count of L2 entries pinned to identity.
// Reads the byIdentity reverse-index; returns 0 for unknown identities.
func byIdentitySize(identity string) int64 {
	v, ok := globalL2.byIdentity.Load(identity)
	if !ok {
		return 0
	}
	bucket, ok := v.(*sync.Map)
	if !ok {
		return 0
	}
	var n int64
	bucket.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// TestPurgeUserCacheData_L2EvictsBeforeL1 — Q-RBACC-L2-1 architect-
// review CONCERN-2 (2026-05-05). Asserts that purgeUserCacheData
// evicts L2 entries pinned to the user's binding identity BEFORE
// issuing any L1 cache.Delete (or DeleteUserRBAC).
//
// We exercise the ordering invariant directly by:
//
//  1. Pre-seeding an L2 entry for the test identity.
//  2. Pre-seeding an L1 resolved-index member so purgeUserCacheData
//     attempts to delete a L1 key.
//  3. Calling purgeUserCacheData under an orderingCache wrapper.
//  4. Asserting: at the time of the FIRST Delete or DeleteUserRBAC
//     event, the L2 byIdentity bucket for the test identity is empty.
//
// This is a direct race-free assertion of the architect's CONCERN-2
// fix (no goroutine synchronisation; the ordering is sequential
// inside purgeUserCacheData).
func TestPurgeUserCacheData_L2EvictsBeforeL1(t *testing.T) {
	enableL2(t)
	mc := NewMem(time.Hour)
	oc := &orderingCache{MemCache: mc}

	// Pre-seed: one L2 entry pinned to the test identity (so the L2
	// reverse-index has at least one entry to evict).
	identity := testIdentityForOrdering
	l1Key := "snowplow:resolved:" + identity + ":g/v/r:ns:testra"
	hk := L2Key(l1Key, identity, "ghash-x")
	SetL2Refilter(hk, &L2Entry{
		Refiltered: []byte("payload"),
		Status:     map[string]any{"k": "v"},
		l1Key:      l1Key,
		identity:   identity,
	}, 1000)

	// Sanity: pre-call, the byIdentity bucket has 1 entry.
	if got := byIdentitySize(identity); got != 1 {
		t.Fatalf("pre-seed: byIdentity=%d, want 1", got)
	}

	// Construct an RBACWatcher with the wrapped cache. Informers are
	// not started — ComputeBindingIdentity will return "" (synced=false),
	// which is acceptable here: we exercise the (oldBid, newBid="")
	// ordering. The L2 evict still runs for oldBid via the
	// identityCache pre-seed below.
	rw := &RBACWatcher{cache: oc}
	// Pre-seed the identityCache so purgeUserCacheData sees oldBid =
	// testIdentityForOrdering.
	username := "test-user"
	rw.identityCache.Store(username, identity)

	// Run the purge. With the architect CONCERN-2 fix, L2 evict
	// happens FIRST (Phase 2), then L1 + DeleteUserRBAC (Phase 3).
	rw.purgeUserCacheData(context.Background(), username)

	if len(oc.deletes) == 0 {
		t.Fatalf("expected at least one cache.Delete or DeleteUserRBAC during purge")
	}

	// THE LOAD-BEARING ASSERTION: at the FIRST L1-touching call, the
	// L2 byIdentity bucket for `identity` MUST already be empty.
	first := oc.deletes[0]
	if first.l2ResidentByID != 0 {
		t.Fatalf(
			"CONCERN-2: at first L1 op, L2 byIdentity[%s] = %d (must be 0). "+
				"Order detected: %v",
			identity, first.l2ResidentByID, sumarizeDeletes(oc.deletes),
		)
	}

	// Defense in depth: every subsequent L1 op should also see the L2
	// byIdentity bucket already drained (we never re-populate during a
	// single purge call).
	for i, d := range oc.deletes {
		if d.l2ResidentByID != 0 {
			t.Fatalf("delete[%d] observed L2 byIdentity=%d (expected 0)", i, d.l2ResidentByID)
		}
	}
}

func sumarizeDeletes(ds []deleteSnapshot) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = fmt.Sprintf("[t+%dus l2=%d keys=%v]",
			d.t.UnixNano()/1000%1_000_000_000, d.l2ResidentByID, d.keys)
	}
	return out
}
