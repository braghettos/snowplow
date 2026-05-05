package cache

import (
	"context"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
)

// emitBindingIdentityTransition logs a per-event audit line when the
// binding identity for a username changes (Q-RBAC-DECOUPLE C(d) v5 —
// D3b, audit 2026-05-05).
//
// Sources of legitimate change:
//
//	(a) operator added/removed a CRB referencing the user's groups;
//	(b) operator added/removed a CRB referencing the user directly;
//	(c) the user's group membership changed (token re-mint).
//
// Cardinality control: per-event in v5 by Diego decision (Q-RBACC-V5-OQ-1
// recommendation accepted). Identity changes are operator-driven (CRB
// CUD events) — minutes-to-hours apart in steady state, possibly bursty
// during a chart re-deploy. If observed rate exceeds 1/sec/pod sustained,
// a counter complement is a 3-LOC follow-up.
func emitBindingIdentityTransition(ctx context.Context, username, oldID, newID string) {
	xcontext.Logger(ctx).Info("cache.binding_identity_transition",
		slog.String("audit", "binding_identity_transition"),
		slog.String("user", username),
		slog.String("old_id", oldID),
		slog.String("new_id", newID),
	)
}
