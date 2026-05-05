package api

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// uafYieldEvery controls the cadence at which applyUserAccessFilter
// releases its P back to the runtime (Q-RBACC-L2-1, OQ-6). The 50K-item
// cyberjoker × compositions-panels refilter walks one tight loop with
// per-item gojq Eval + per-item EvaluateRBAC; without yield, that loop
// can hold a P through 30+ liveness probe windows on a freshly-restarted
// pod and trip the SIGKILL pattern that killed v6 phase-6.
//
// 1024 chosen as a power-of-2 tradeoff: at 50K items the loop yields ~50
// times (~2 µs per yield → ~100 µs total overhead, < 0.1% of refilter
// wall). Generic yield discipline; not a refilter-specific behaviour
// optimization. feedback_no_special_cases.md honoured: applies uniformly
// to every applyUserAccessFilter caller, no per-RA / per-user branches.
const uafYieldEvery = 1024

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

	// Audit counters captured by the deferred audit emission below. Declaring
	// them here (rather than at the loop site) lets every return path —
	// including FAIL-CLOSED early returns for missing UserInfo / missing
	// RBACEvaluator — fire exactly one audit log entry. Auditors aggregating
	// on the structured `audit=user_access_filter` event would otherwise
	// undercount denials when the helper bails out early (architect review
	// blocker, 2026-05-04).
	var (
		inCount  int
		outCount int
		denied   int
		jqErrors int
	)
	defer func() {
		auditUserAccessFilter(ctx, apiCall, inCount, outCount, denied, jqErrors, time.Since(start))
	}()

	items, ok := computed.([]any)
	if !ok {
		log.Warn("userAccessFilter: response not an array, skipping",
			slog.String("api", apiCall.Name),
			slog.String("type", fmt.Sprintf("%T", computed)))
		// inCount/outCount remain 0; audit reflects the skip. Returning
		// computed unchanged matches the documented contract — single-object
		// responses are still gated by rbac.UserCan upstream.
		return computed
	}
	inCount = len(items)

	user, err := xcontext.UserInfo(ctx)
	if err != nil {
		log.Error("userAccessFilter: cannot read user identity, returning empty",
			slog.String("api", apiCall.Name),
			slog.Any("err", err))
		// FAIL-CLOSED: every input item is effectively denied by the
		// fail-closed policy; reflect that in the audit counters so an
		// operator sees `denied == items_in` and can correlate the spike
		// with the absent UserInfo error log.
		denied = inCount
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
		// FAIL-CLOSED: same accounting as the missing-UserInfo path above.
		denied = inCount
		return []any{}
	}

	gr := schema.GroupResource{Group: f.Group, Resource: f.Resource}
	nsFrom := ptr.Deref(f.NamespaceFrom, "")

	out := make([]any, 0, len(items))
	for i, item := range items {
		// Q-RBACC-L2-1, OQ-6 — yield discipline. Release the P every
		// uafYieldEvery items so the liveness probe goroutine can run
		// during a long refilter pass over a 50K-item input. Without
		// this, the v6-phase-6 panels SIGKILL recurs on prewarm-time
		// admin × compositions-panels refilter (40 MB body, ~10 s wall).
		if i > 0 && i%uafYieldEvery == 0 {
			runtime.Gosched()
		}
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
	outCount = len(out)

	return out
}
