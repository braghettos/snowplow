package api

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// applyUserAccessFilter drops items from `computed` for which the requesting
// user lacks the RBAC permission declared in apiCall.UserAccessFilter.
//
// Contract (load-bearing):
//   - apiCall.UserAccessFilter == nil → returns computed unchanged (no-op for
//     old YAMLs; safe to wrap unconditionally at every hookup site).
//   - computed not a []any → returns computed unchanged with WARN log.
//     Single-object responses are still gated by the existing rbac.UserCan
//     check at resolve.go:410 — this helper only filters list-shaped
//     responses where per-item per-namespace decisions matter.
//   - RBACWatcher missing from ctx → returns []any{} with ERROR log
//     (FAIL-CLOSED). Avoids leaking unfiltered items when the in-memory
//     evaluator is not wired (e.g. cache disabled, tests forgetting to
//     inject one).
//   - UserInfo missing from ctx → returns []any{} with ERROR log
//     (FAIL-CLOSED).
//   - JQ-eval error / non-string output on a single item → drops that item
//     (silently per-item, counted in jq_errors for the audit log).
//   - EvaluateRBAC returns false → drops that item, increments denied.
//
// On every invocation the audit log helper is called once with the in/out
// counts, denied count, jq error count, and elapsed time so the post-hoc
// observability story (per-trace, per-restaction, per-user) is consistent
// across all six hookup sites.
func applyUserAccessFilter(ctx context.Context, apiCall *templates.API, computed any) any {
	if apiCall == nil || apiCall.UserAccessFilter == nil {
		return computed
	}
	f := apiCall.UserAccessFilter
	log := xcontext.Logger(ctx)
	start := time.Now()

	items, ok := computed.([]any)
	if !ok {
		log.Warn("userAccessFilter: response not an array, skipping",
			slog.String("api", apiCall.Name),
			slog.String("type", fmt.Sprintf("%T", computed)))
		return computed
	}

	user, err := xcontext.UserInfo(ctx)
	if err != nil {
		log.Error("userAccessFilter: cannot read user identity, returning empty",
			slog.String("api", apiCall.Name),
			slog.Any("err", err))
		return []any{}
	}

	// Production wires *cache.RBACWatcher (which implements RBACEvaluator).
	// Tests may install a mock evaluator via WithRBACEvaluator; the mock
	// wins when both are present (Q-RBACC-IMPL-1).
	var ev cache.RBACEvaluator
	if mock := cache.RBACEvaluatorFromContext(ctx); mock != nil {
		ev = mock
	} else if rw := cache.RBACWatcherFromContext(ctx); rw != nil {
		ev = rw
	}
	if ev == nil {
		log.Error("userAccessFilter: RBACWatcher not in context, returning empty",
			slog.String("api", apiCall.Name))
		return []any{}
	}

	gr := schema.GroupResource{Group: f.Group, Resource: f.Resource}
	nsFrom := ptr.Deref(f.NamespaceFrom, "")

	out := make([]any, 0, len(items))
	var jqErrors, denied int
	for _, item := range items {
		ns := ""
		if nsFrom != "" {
			// gojq-purity-required: item came from JQ filter output via
			// jsonHandlerCompute / jsonHandlerDirectCompute (already
			// passed through gojq, owned by this goroutine).
			v, jqErr := jqsupport.Eval(ctx, nsFrom, item)
			if jqErr != nil {
				jqErrors++
				continue
			}
			s, isStr := v.(string)
			if !isStr {
				jqErrors++
				continue
			}
			ns = s
		}

		// RBACEvaluator interface (Q-RBACC-IMPL-1) is satisfied by
		// *cache.RBACWatcher; the indirection lets unit tests swap in a
		// mock via WithRBACEvaluator without standing up an informer
		// factory.
		if !ev.EvaluateRBAC(user.Username, user.Groups, f.Verb, gr, ns) {
			denied++
			continue
		}
		out = append(out, item)
	}

	auditUserAccessFilter(ctx, apiCall, len(items), len(out), denied, jqErrors, time.Since(start))
	return out
}
