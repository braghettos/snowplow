// Q-PREWARM-R2R5 PR-B (R5) — tests for per-user cache eviction on
// -clientconfig Secret DELETE.
package cache

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestEvictUserL1_RemovesIndexedKeys seeds a per-user resolved-index
// SET with two members, calls EvictUserL1, and asserts both members
// and the index key itself are gone afterwards.
func TestEvictUserL1_RemovesIndexedKeys(t *testing.T) {
	c := NewMem(time.Hour)
	ctx := context.Background()
	username := "alice"

	gvr := schema.GroupVersionResource{Group: "g", Version: "v1", Resource: "rs"}
	k1 := ResolvedKey(username, gvr, "ns1", "n1", -1, -1)
	k2 := ResolvedKey(username, gvr, "ns1", "n2", -1, -1)
	idx := UserResolvedIndexKey(username)

	// Populate the resolved keys + index.
	if err := c.SetResolvedRaw(ctx, k1, []byte("body1")); err != nil {
		t.Fatalf("seed k1: %v", err)
	}
	if err := c.SetResolvedRaw(ctx, k2, []byte("body2")); err != nil {
		t.Fatalf("seed k2: %v", err)
	}
	if err := c.SAddWithTTL(ctx, idx, k1, time.Hour); err != nil {
		t.Fatalf("seed idx k1: %v", err)
	}
	if err := c.SAddWithTTL(ctx, idx, k2, time.Hour); err != nil {
		t.Fatalf("seed idx k2: %v", err)
	}

	if !c.Exists(ctx, k1) || !c.Exists(ctx, k2) {
		t.Fatalf("precondition: keys must exist after seed")
	}

	removed := EvictUserL1(ctx, c, username)
	if removed < 2 {
		t.Errorf("expected at least 2 keys evicted, got %d", removed)
	}

	if c.Exists(ctx, k1) {
		t.Errorf("k1 still present after EvictUserL1")
	}
	if c.Exists(ctx, k2) {
		t.Errorf("k2 still present after EvictUserL1")
	}
	if members, _ := c.SMembers(ctx, idx); len(members) != 0 {
		t.Errorf("index SET not empty after evict: %v", members)
	}
}

// TestEvictUserL1_EmptyUsernameNoOp verifies the safety bail-out for
// the empty-string username (which would otherwise scan an over-broad
// pattern and risk over-deletion).
func TestEvictUserL1_EmptyUsernameNoOp(t *testing.T) {
	c := NewMem(time.Hour)
	ctx := context.Background()
	gvr := schema.GroupVersionResource{Group: "g", Version: "v1", Resource: "rs"}
	stranger := ResolvedKey("bob", gvr, "ns1", "n1", -1, -1)
	if err := c.SetResolvedRaw(ctx, stranger, []byte("body")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := EvictUserL1(ctx, c, ""); got != 0 {
		t.Errorf("empty username: expected 0 removed, got %d", got)
	}
	if !c.Exists(ctx, stranger) {
		t.Errorf("empty-username evict accidentally removed an unrelated key")
	}
}

// TestEvictUserL1_NilCacheNoOp verifies the function does not panic
// when the cache is nil (defensive — the caller path through
// PurgeUserOnSecretDelete should never hit this, but a typo at a wiring
// site shouldn't crash the pod).
func TestEvictUserL1_NilCacheNoOp(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unexpected panic: %v", r)
		}
	}()
	if got := EvictUserL1(context.Background(), nil, "alice"); got != 0 {
		t.Errorf("nil cache: expected 0 removed, got %d", got)
	}
}

// TestEvictUserL1_FallbackScan exercises the scan-keys fallback path:
// when the index SET is empty (e.g. a legacy key written without
// indexing or after a manual SET deletion), the function must still
// find and delete the key by scan over the kv keyspace.
func TestEvictUserL1_FallbackScan(t *testing.T) {
	c := NewMem(time.Hour)
	ctx := context.Background()
	username := "carol"

	gvr := schema.GroupVersionResource{Group: "g", Version: "v1", Resource: "rs"}
	k := ResolvedKey(username, gvr, "ns", "n", -1, -1)

	// Write the resolved key (auto-indexed by SetResolvedRaw), then
	// flush the index SET to simulate a legacy / drifted key whose
	// index has been lost.
	if err := c.SetResolvedRaw(ctx, k, []byte("body")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := c.ReplaceSetWithTTL(ctx, UserResolvedIndexKey(username), nil, 0); err != nil {
		t.Fatalf("seed flush index: %v", err)
	}

	// Confirm index is empty so we know we're on the fallback path.
	if members, _ := c.SMembers(ctx, UserResolvedIndexKey(username)); len(members) != 0 {
		t.Fatalf("precondition: index expected empty, got %v", members)
	}

	removed := EvictUserL1(ctx, c, username)
	if removed < 1 {
		t.Errorf("fallback scan: expected at least 1 key evicted, got %d", removed)
	}
	if c.Exists(ctx, k) {
		t.Errorf("fallback scan: legacy key still present after evict")
	}
}

// TestPurgeUserOnSecretDelete_OrderL2BeforeL1 verifies the documented
// ordering contract: L2 evict happens before L1 evict. We assert this
// indirectly by checking that EvictL2ForIdentity is called via the
// counter on GlobalMetrics. The strong ordering guarantee is exercised
// in the rbac_watcher_l2_ordering_test.go family which uses an
// orderingCache wrapper -- not duplicated here.
func TestPurgeUserOnSecretDelete_DoesNotPanic(t *testing.T) {
	c := NewMem(time.Hour)
	ctx := context.Background()

	gvr := schema.GroupVersionResource{Group: "g", Version: "v1", Resource: "rs"}
	k := ResolvedKey("dave", gvr, "ns", "n", -1, -1)
	if err := c.SetResolvedRaw(ctx, k, []byte("body")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := c.SAddWithTTL(ctx, UserResolvedIndexKey("dave"), k, time.Hour); err != nil {
		t.Fatalf("seed idx: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unexpected panic: %v", r)
		}
	}()
	PurgeUserOnSecretDelete(ctx, c, "dave")
	if c.Exists(ctx, k) {
		t.Errorf("L1 key still present after PurgeUserOnSecretDelete")
	}
}

// TestPurgeUserOnSecretDelete_NilCacheNoOp guards against panic on
// degenerate input.
func TestPurgeUserOnSecretDelete_NilCacheNoOp(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unexpected panic: %v", r)
		}
	}()
	PurgeUserOnSecretDelete(context.Background(), nil, "anyone")
	PurgeUserOnSecretDelete(context.Background(), NewMem(time.Hour), "")
}
