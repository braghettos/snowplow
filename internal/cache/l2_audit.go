// Q-RBACC-L2-1 — sampled L2 hit audit.
//
// L2 hits SKIP applyUserAccessFilter (the function that emits
// audit=user_access_filter). The decision the L2 hit reflects was made
// the FIRST time the L2 entry was filled (and audited then). For
// operational visibility, we still want a low-volume signal that an L2
// hit occurred for a (user, identity, restaction) triple — defaulting
// to 1% sample (CACHE_L2_REFILTER_HIT_AUDIT_SAMPLE).
//
// SecOps may want a 100% rate post-canary; that's why the sample is env-
// configurable (default 0.01). The "user_access_filter_l2_hit" string is
// intentionally distinct from "user_access_filter" so log-aggregation
// pipelines can grep them separately without aliasing.
//
// Counter `cache.GlobalMetrics.L2Hits` records every hit (100%); the
// audit-emit is the post-hoc-grep complement of that counter.

package cache

import (
	"context"
	"hash/fnv"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
)

// EmitL2HitAudit logs a sampled audit=user_access_filter_l2_hit line.
// Caller passes the (l1Key, identity, restAction, username) tuple. The
// sampling decision is keyed on a fast hash of (l1Key + identity) so
// the same (l1Key, identity) consistently emits or doesn't — this
// preserves "rare is rare; common is common" without random-walk noise.
//
// SAFETY: never mutates input; cheap (one fnv64 + one float compare).
// Returns nothing — safe to call from any hot path under enabled L2.
func EmitL2HitAudit(ctx context.Context, l1Key, identity, restAction, username string) {
	rate := l2HitAuditSample()
	if rate <= 0 {
		return
	}
	if rate < 1.0 {
		h := fnv.New64a()
		h.Write([]byte(l1Key))
		h.Write([]byte{0})
		h.Write([]byte(identity))
		// Map the 64-bit hash into [0, 1) via a uint conversion. This is
		// a deterministic sampler — consistent across calls for the same
		// pair, so a single (key, identity) is either always-sampled or
		// never-sampled at a given rate.
		bucket := float64(h.Sum64()&0x7FFFFFFFFFFFFFFF) / float64(1<<63)
		if bucket >= rate {
			return
		}
	}
	xcontext.Logger(ctx).Info("cache.user_access_filter_l2_hit",
		slog.String("audit", "user_access_filter_l2_hit"),
		slog.String("user", username),
		slog.String("identity", identity),
		slog.String("restaction", restAction),
		slog.String("l1_key", l1Key),
	)
}
