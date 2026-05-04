package api

import (
	"context"
	"log/slog"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// auditUserAccessFilter writes the structured audit log entry for one
// applyUserAccessFilter invocation.
//
// Per Q-RBACC-IMPL-4 (architect 2026-05-04) the entry intentionally does
// NOT include an items_out_sample field — keeping per-trace correlation
// (via trace_id, auto-injected by observability.NewTraceIDHandler)
// instead of large free-text payloads in slog.
//
// The 14 fields below are the developer's contract:
//   audit, restaction, api_call, user, groups,
//   filter_verb, filter_group, filter_resource, filter_namespace_from,
//   items_in, items_out, denied, jq_errors, duration.
func auditUserAccessFilter(
	ctx context.Context,
	apiCall *templates.API,
	inCount, outCount, denied, jqErrors int,
	duration time.Duration,
) {
	log := xcontext.Logger(ctx)
	user, _ := xcontext.UserInfo(ctx)
	raName := cache.RESTActionNameFromContext(ctx)

	f := apiCall.UserAccessFilter
	log.Info("restaction.user_access_filter",
		slog.String("audit", "user_access_filter"),
		slog.String("restaction", raName),
		slog.String("api_call", apiCall.Name),
		slog.String("user", user.Username),
		slog.Any("groups", user.Groups),
		slog.String("filter_verb", f.Verb),
		slog.String("filter_group", f.Group),
		slog.String("filter_resource", f.Resource),
		slog.String("filter_namespace_from", ptr.Deref(f.NamespaceFrom, "")),
		slog.Int("items_in", inCount),
		slog.Int("items_out", outCount),
		slog.Int("denied", denied),
		slog.Int("jq_errors", jqErrors),
		slog.Duration("duration", duration),
	)
}
