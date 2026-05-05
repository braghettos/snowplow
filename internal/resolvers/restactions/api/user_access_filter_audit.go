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

// auditUserAccessFilterSkipped is the negative-evidence companion to
// auditUserAccessFilter (Q-RBAC-DECOUPLE C(d) v5 — D3 fix, audit 2026-05-05).
//
// It fires when an api[] entry has UserAccessFilter set but
// applyUserAccessFilter was never invoked because the upstream call failed
// (TLS handshake error, RBAC 403, network timeout, etc.). Operators
// correlating "did the filter run?" with "what data did the user see?"
// need both signals — pre-v5, the resolver-error path was completely
// silent on the audit channel and an outside observer could not
// distinguish (a) "filter denied everything" from (b) "filter never ran".
//
// Field shape mirrors auditUserAccessFilter for parser parity, with the
// counters set to 0 and `reason` + `error` substituting for the count
// fields:
//
//	audit=user_access_filter_skipped restaction api_call user groups
//	filter_verb filter_group filter_resource filter_namespace_from
//	reason error
//
// Note: api_call is the resolved name (post-template), matching the
// auditUserAccessFilter field. Reason is a small enum: today only
// "api_error" is emitted; reserved for future codes.
func auditUserAccessFilterSkipped(
	ctx context.Context,
	apiCall *templates.API,
	reason string,
	errMsg string,
) {
	if apiCall == nil || apiCall.UserAccessFilter == nil {
		return
	}
	log := xcontext.Logger(ctx)
	user, _ := xcontext.UserInfo(ctx)
	raName := cache.RESTActionNameFromContext(ctx)

	f := apiCall.UserAccessFilter
	log.Info("restaction.user_access_filter_skipped",
		slog.String("audit", "user_access_filter_skipped"),
		slog.String("restaction", raName),
		slog.String("api_call", apiCall.Name),
		slog.String("user", user.Username),
		slog.Any("groups", user.Groups),
		slog.String("filter_verb", f.Verb),
		slog.String("filter_group", f.Group),
		slog.String("filter_resource", f.Resource),
		slog.String("filter_namespace_from", ptr.Deref(f.NamespaceFrom, "")),
		slog.String("reason", reason),
		slog.String("error", errMsg),
	)
}

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
