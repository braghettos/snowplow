package dispatchers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/snowplow/internal/cache"
	idynamic "github.com/krateoplatformops/snowplow/internal/dynamic"
	"k8s.io/client-go/rest"
)

func All() map[string]http.Handler {
	return map[string]http.Handler{
		"restactions.templates.krateo.io": RESTAction(),
		"widgets.templates.krateo.io":     Widgets(),
	}
}

// RegisterRefreshHandlers wires the L1 resolved-output cache refresher
// callbacks for every refreshable handler kind — "restactions",
// "widgets", and (Ship 0.30.117) "apistage". MUST be called AFTER
// ResolvedCache() is built (so the cache singleton exists) and BEFORE
// cache.StartRefresher (so the worker pool sees populated handlers on
// first dequeue). A kind missing from this registry is silently
// TTL-only — the refresher counts skippedNoHandler and never re-resolves
// it (the 0.30.116 -> 0.30.117 AC-E3 defect).
//
// Ship C (0.30.112): the handlers are REAL — each delegates to the
// shared resolveAndPopulateL1, which re-resolves the entry from its own
// ResolvedKeyInputs under the entry's own identity and Put()s the fresh
// bytes back into L1. resolveAndPopulateL1 installs WithL1KeyContext
// internally so the resolver re-records dep edges (the inner object set
// may have changed since the original resolve). It only ever Put()s —
// never evicts — so the stale-while-revalidate contract
// (feedback_l1_invalidation_delete_only.md) holds.
//
// Ship 0.30.113 Part B: the refresh runs as a BACKGROUND task with no
// live per-user bearer token, so resolveAndPopulateL1 needs the snowplow
// ServiceAccount endpoint + *rest.Config as the re-resolve TRANSPORT
// (the widget resolver reads xcontext.UserConfig directly). saRC is the
// in-cluster *rest.Config main.go already built from rest.InClusterConfig;
// the SA endpoint is obtained here once via idynamic.ServiceAccountEndpoint
// (the SAME singleton Phase 1 uses) and captured in the closure — never
// rebuilt per refresh. The re-resolve still runs under the entry's OWN
// Username+Groups identity for RBAC correctness; the SA pair only carries
// the apiserver client-config — no per-user token is stored. When the SA
// endpoint is unavailable (unit test / outside-cluster) the refresh runs
// identity-only exactly as before — graceful degradation.
//
// A non-nil error from resolveAndPopulateL1 propagates to the refresher,
// which requeues the key with bounded exponential backoff (capped — see
// refresher.go Part A). A legacy nil-Inputs entry never reaches here —
// the refresher skips it before dispatching a handler.
func RegisterRefreshHandlers(saRC *rest.Config) {
	var saEP *endpoints.Endpoint
	if ep, err := idynamic.ServiceAccountEndpoint(); err != nil {
		slog.Warn("refresher.no_sa_transport",
			slog.String("subsystem", "cache"),
			slog.Any("err", err),
			slog.String("effect", "background refresh runs identity-only; widget-kind re-resolves that read xcontext.UserConfig will fail and drop to TTL after the requeue cap"),
		)
	} else {
		saEP = ep
	}
	if saRC == nil {
		slog.Warn("refresher.no_sa_rest_config",
			slog.String("subsystem", "cache"),
			slog.String("effect", "background refresh has no SA *rest.Config; object-fetch sites fall back to the per-user kubeconfig path which fails for a synthetic identity"),
		)
	}
	refreshFunc := func(ctx context.Context, _ string, in cache.ResolvedKeyInputs) error {
		// resolveAndPopulateL1 computes the canonical key from `in`,
		// installs WithL1KeyContext, builds the entry's own identity
		// context (plus the SA transport), re-resolves, re-checks
		// liveness, and Put()s. The `key` argument is redundant with
		// ComputeKey(in) — the shared path recomputes it so prewarm
		// (Ship F) can reuse the body without a pre-known key.
		return resolveAndPopulateL1(ctx, in, saEP, saRC)
	}
	cache.RegisterRefreshFunc("restactions", refreshFunc)
	cache.RegisterRefreshFunc("widgets", refreshFunc)
	// Ship 0.30.117 — AC-E3 fix. Ship E (0.30.116) added the "apistage"
	// L1 entry kind but RegisterRefreshHandlers never registered a
	// handler for it, so an apistage entry off the refresher queue hit a
	// nil handler -> skippedNoHandler -> silently TTL-only, never
	// refreshed (the dep-scoped refresh AC-E3 promised). The SAME
	// refreshFunc closure serves it: the resolve-once seam already routes
	// HandlerKindApistage (resolve_populate.go re-resolves the owning
	// RESTAction, whose in-loop key-swap re-Puts the stage entry).
	cache.RegisterRefreshFunc(cache.HandlerKindApistage, refreshFunc)
}
