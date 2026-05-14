package dispatchers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

func All() map[string]http.Handler {
	return map[string]http.Handler{
		"restactions.templates.krateo.io": RESTAction(),
		"widgets.templates.krateo.io":     Widgets(),
	}
}

// RegisterRefreshHandlers wires the 0.30.8 L1 resolved-output cache
// refresher callbacks for the two dispatcher kinds. MUST be called
// AFTER ResolvedCache() is built (so the cache singleton exists) and
// BEFORE cache.StartRefresher (so the worker pool sees populated
// handlers on first dequeue).
//
// **Current state (0.30.8):** the registered handlers are deliberately
// no-ops that return nil. Re-resolving an L1 entry requires per-user
// Kubernetes credentials (the user's `<username>-clientconfig` Secret +
// JWT) which are NOT stored on the cache entry — the original request
// context carried them in-band. A future sub-ship will stash a
// minimum-creds refresh token on the entry; until then the refresher
// faithfully implements queue + dedup + worker-pool semantics but does
// not actually re-resolve. UPDATE-driven freshness is provided by:
//
//   1. The TTL outer net (default 60s at 0.30.8 per plan chart values).
//   2. Per-request misses on the next user read after the dep-tracker
//      records a refresh-enqueue (entry stays in L1 but the next
//      mutation can race-evict it via DELETE if it occurs).
//
// Per feedback_l1_invalidation_delete_only.md, UPDATE/PATCH MUST NOT
// evict. The no-op handler complies — it neither evicts nor mutates the
// entry; the counter `refresh_completed` increments so the falsifier
// signal in the summary log fires when UPDATE events are flowing.
//
// Returning nil on every call also gives us a clean extension point:
// the closure body is where future credential-stashing work plugs in.
func RegisterRefreshHandlers() {
	cache.RegisterRefreshFunc("restactions", func(ctx context.Context, key string, in cache.ResolvedKeyInputs) error {
		// 0.30.94 Edge type 3 refresher integration: attach the L1 key
		// to the refresh ctx so when the no-op handler is flipped to
		// real re-resolve in a future tag, the resolver's inner-call
		// recording site sees a non-empty L1 key and re-records updated
		// dep edges (object set may have changed between original
		// resolve and refresh). At 0.30.94 the handler stays no-op per
		// 0.30.8 design — this is a no-cost pre-wire so subsequent
		// tags need not touch dispatchers.go again.
		_ = cache.WithL1KeyContext(ctx, key)

		// 0.30.8: no-op. Counter ticks via refresher.completedTotal so
		// downstream telemetry can verify the queue is draining.
		slog.Debug("refresh.restactions.noop",
			slog.String("subsystem", "cache"),
			slog.String("key_hash", key),
			slog.String("user", in.Username),
		)
		return nil
	})
	cache.RegisterRefreshFunc("widgets", func(ctx context.Context, key string, in cache.ResolvedKeyInputs) error {
		_ = cache.WithL1KeyContext(ctx, key)
		slog.Debug("refresh.widgets.noop",
			slog.String("subsystem", "cache"),
			slog.String("key_hash", key),
			slog.String("user", in.Username),
		)
		return nil
	})
}
