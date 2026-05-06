// Q-PREWARM-R2R5 PR-B (R5) — per-user L1 + L2 eviction on user-secret DELETE.
//
// Spec: /tmp/snowplow-runs/q-cold-1-cd-v6-phase6-50k-postPR2PR3-2026-05-05/
//       Q-PREWARM-R2R5-SPEC.md §2.3.
//
// Per `feedback_l1_invalidation_delete_only.md` — L1 deletes happen ONLY
// on resource DELETE events. The "resource" here is the user's
// -clientconfig Secret. When that secret disappears, the user is gone
// (de-provisioned, namespace nuked, etc.) and any cached widget/RA
// outputs keyed under that user are stranded. Evicting them is the
// correct semantics, not stale-while-revalidate.
//
// Per architect spec §7 OQ4: this crosses the feedback boundary because
// the feedback was originally written about CR-DELETE → L1-evict, but
// Secret-DELETE → user-L1-evict is the same pattern (resource gone →
// evict its cache footprint). The architect read this as compliant.
package cache

import (
	"context"
	"log/slog"
)

// EvictUserL1 removes every L1 (resolved) cache entry keyed under the
// given username. Includes the per-username resolved-index SET and any
// scan-discoverable resolved keys that may have leaked outside the
// index (e.g. legacy keys written before the SAdd index was added).
//
// Returns the number of keys deleted (best-effort; cache implementations
// that batch deletes may report counts approximately).
//
// Safe for the empty-string username — the username is part of the key
// prefix, so an empty value would scan an over-broad pattern; the
// function bails out early in that case.
func EvictUserL1(ctx context.Context, c Cache, username string) int {
	if c == nil || username == "" {
		return 0
	}

	total := 0

	// Primary: resolved-index SET membership (the authoritative list
	// of L1 keys for this user written by SetResolvedRaw).
	idxKey := UserResolvedIndexKey(username)
	keys, _ := c.SMembers(ctx, idxKey)

	if len(keys) > 0 {
		// Delete the indexed L1 keys themselves (kv-store entries).
		_ = c.Delete(ctx, keys...)
		total += len(keys)
		// Empty the SET so a subsequent re-add of the same user starts
		// fresh. SRemMembers + ReplaceSetWithTTL with empty members
		// drops the SET entirely (see MemCache.ReplaceSetWithTTL).
		_ = c.SRemMembers(ctx, idxKey, keys...)
	} else {
		// Fallback: the index drifted (e.g. legacy key written without
		// SAdd). Use prefix delete -- the resolved-key format embeds
		// `<group>/<version>/<resource>` literally, so glob-based
		// ScanKeys would match zero entries (path.Match refuses to
		// span `/`). The DeleteByPrefix fast path on *MemCache walks
		// the kv keyspace once and removes anything matching the
		// per-user prefix.
		prefix := "snowplow:resolved:" + username + ":"
		if pd, ok := c.(prefixDeleter); ok {
			n, _ := pd.DeleteByPrefix(ctx, prefix)
			total += n
		}
	}
	// Drop the index SET key entirely. ReplaceSetWithTTL with len==0
	// is the public delete-set primitive (cache.go interface).
	_ = c.ReplaceSetWithTTL(ctx, idxKey, nil, 0)
	return total
}

// EvictUserCohortL2 removes every L2 (post-refilter) entry whose
// binding identity matches `username` directly. When the user has been
// part of a shared binding-identity cohort, the cohort eviction is the
// caller's responsibility (typically RBACWatcher.purgeUserCacheData on
// CRB transitions). On user-secret DELETE we do not know the binding
// identity any more — the user is gone — so we evict the username-keyed
// fallback entries that exist when the RBAC watcher had not yet
// computed an identity for this user.
//
// Returns the number of L2 entries removed.
func EvictUserCohortL2(ctx context.Context, username string) int {
	if username == "" {
		return 0
	}
	return EvictL2ForIdentity(ctx, username)
}

// PurgeUserOnSecretDelete is the single coordinated entry point called
// by UserSecretWatcher.onDelete. It evicts in the architect-mandated
// order from rbac_watcher.go: L2 FIRST, then L1, then RBAC.
//
// Order rationale (mirrors purgeUserCacheData CONCERN-2):
//
//	apiref reads L2 BEFORE L1. If we deleted L1 first a microsecond
//	window would open where L1 is gone but L2 still serves stale
//	bytes against the now-vanished user. Evict L2 first so concurrent
//	reads see L2 miss → fall through → eventually L1 miss.
func PurgeUserOnSecretDelete(ctx context.Context, c Cache, username string) {
	if c == nil || username == "" {
		return
	}

	// Phase 1: evict L2 first (architect CONCERN-2).
	l2Removed := EvictUserCohortL2(ctx, username)

	// Phase 2: evict L1 resolved entries.
	l1Removed := EvictUserL1(ctx, c, username)

	// Phase 3: evict RBAC hash (already done by user_watcher.go, but
	// idempotent — left to the caller to keep responsibilities tidy).

	slog.Info("user-evict: purged caches on -clientconfig DELETE",
		slog.String("username", username),
		slog.Int("l1Removed", l1Removed),
		slog.Int("l2Removed", l2Removed),
	)
}
