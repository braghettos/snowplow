// refilter.go — Tag 0.30.9 Sub-scope A: in-process per-object refilter
// for the userAccessFilter dispatch path.
//
// Contract:
//   - Input: the raw JSON-decoded response of a ServiceAccount-dispatched
//     K8s list call (typically {"kind":"...List","items":[...]} but
//     tolerates a bare slice too).
//   - The per-object NamespaceFrom JQ expression yields the namespace
//     used in the EvaluateRBAC call. When the expression is empty or
//     evaluates to "", the per-object namespace is "" (cluster-scoped
//     check).
//   - Output: same shape as input with non-permitted items dropped.
//     Result is RBAC-clean from the user's perspective.
//
// Bindings:
//   - feedback_restaction_no_widget_logic.md: this code lives in the
//     resolver layer (per-API stage), not in widget canonicalization.
//   - feedback_no_special_cases.md: no per-resource carve-outs.
//     refilter is uniform across every userAccessFilter usage.
//   - Revision 2: EvaluateRBAC fires per object. The refilter is the
//     authoritative gate; if it drops, the user does not see the item.
//   - feedback_l1_invalidation_delete_only.md: refilter runs BEFORE
//     the resolved-cache write (dispatchers/restactions.go path), so
//     the cached entry is already user-scoped.
//
// Performance: per-object JQ eval + per-object EvaluateRBAC. At the
// production refilter sites (cyberjoker's 6 namespace-scope filters)
// the result sets are ≤ 50 items, so the cost is ≤ 50 × (JQ eval + RBAC
// lookup). EvaluateRBAC is in-process (typed-RBAC informer cache);
// amortised to <1µs at Tag 0.30.10 (permission-check cache).

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
)

// refilterResult is the (kept, dropped, total) summary the resolver
// logs as the per-call falsifier per plan §"Code-path falsifier":
//   userAccessFilter.dispatch=service_account ... refilter_dropped=N refilter_kept=M evaluate_rbac_calls=K
type refilterResult struct {
	Kept              int
	Dropped           int
	EvaluateRBACCalls int
}

// applyUserAccessFilter is the entrypoint the resolver calls when an
// API stage declares userAccessFilter. It mutates dict[apiCall.Name]
// in place, replacing the SA-dispatched result with the refiltered
// subset.
//
// Shape detection: dict[apiCall.Name] is either:
//   * map[string]any with an "items" slice (typical K8s list response);
//   * []any (rare; some endpoints flatten the list).
// Other shapes are passed through unchanged with a WARN (we cannot
// safely refilter what we don't understand — the user sees the full
// SA-dispatched response, which is the conservative-deny choice for
// security but a leak by definition; the WARN is the operator alert).
//
// Errors during JQ eval or RBAC eval are treated as DENIES per object
// (fail-closed semantics) so a transient evaluator hiccup never
// permits a leak.
func applyUserAccessFilter(ctx context.Context, dict map[string]any, apiCall *templates.API) refilterResult {
	log := xcontext.Logger(ctx)
	res := refilterResult{}

	if apiCall == nil || apiCall.UserAccessFilter == nil {
		return res
	}
	uaf := apiCall.UserAccessFilter

	user, err := xcontext.UserInfo(ctx)
	if err != nil {
		log.Error("userAccessFilter: cannot extract UserInfo; dropping all items (fail-closed)",
			slog.String("api", apiCall.Name),
			slog.Any("err", err),
		)
		// Replace with empty result set. Conservative-deny per
		// security model.
		_ = setRefilteredEmpty(dict, apiCall.Name)
		return res
	}

	// Ship 0.30.129 — resolve the RBAC resource-plural set ONCE per
	// dispatch. With ResourcesFrom set, the set is jq-evaluated against
	// the full dict (runtime-discovered plurals); unset, it is the
	// single static uaf.Resource. resolveUAFResources fails closed: a
	// ResourcesFrom that errors or yields an empty set returns an empty
	// slice — and evalSingle with an empty resource set DENIES every
	// item (no resource to grant against), never allow-all.
	resources, resOK := resolveUAFResources(ctx, log, uaf, dict)
	if !resOK {
		log.Error("userAccessFilter: resource set unresolved; dropping all items (fail-closed)",
			slog.String("api", apiCall.Name),
		)
		_ = setRefilteredEmpty(dict, apiCall.Name)
		return res
	}

	raw, ok := dict[apiCall.Name]
	if !ok || raw == nil {
		return res
	}

	switch v := raw.(type) {
	case map[string]any:
		// Typical K8s list shape: {"items": [...]}
		itemsRaw, hasItems := v["items"]
		if !hasItems {
			// Some endpoints return a single object without an "items"
			// wrapper (cluster-scoped GET-by-name). Treat the whole
			// map as the single object.
			permitted := evalSingle(ctx, log, user.Username, user.Groups, uaf, resources, v)
			res.EvaluateRBACCalls++
			if permitted {
				res.Kept++
			} else {
				res.Dropped++
				dict[apiCall.Name] = map[string]any{}
			}
			return res
		}
		items, ok := itemsRaw.([]any)
		if !ok {
			log.Warn("userAccessFilter: items is not a slice; passing through unchanged",
				slog.String("api", apiCall.Name),
				slog.Any("items_type", fmt.Sprintf("%T", itemsRaw)),
			)
			return res
		}
		kept, dropped, calls := refilterSlice(ctx, log, user.Username, user.Groups, uaf, resources, items)
		v["items"] = kept
		res.Kept = len(kept)
		res.Dropped = dropped
		res.EvaluateRBACCalls = calls

	case []any:
		kept, dropped, calls := refilterSlice(ctx, log, user.Username, user.Groups, uaf, resources, v)
		dict[apiCall.Name] = kept
		res.Kept = len(kept)
		res.Dropped = dropped
		res.EvaluateRBACCalls = calls

	default:
		log.Warn("userAccessFilter: unrecognised result shape; passing through unchanged",
			slog.String("api", apiCall.Name),
			slog.Any("result_type", fmt.Sprintf("%T", raw)),
		)
	}

	return res
}

// refilterSlice walks items[] and returns (kept, droppedCount, calls).
// Idempotent on input — items slice is treated as immutable; a fresh
// kept slice is allocated.
//
// Item shapes (0.30.111 Part 1 fix):
//   - map[string]any — a K8s object. NamespaceFrom (e.g. ".metadata.name")
//     resolves against the object. The pre-0.30.111 path, unchanged.
//   - string — a bare scalar. This is the namespaces-stage shape: the
//     stage's own `filter: "[.namespaces.items[] | .metadata.name]"`
//     has already projected the namespace LIST down to a name-string
//     array, so refilterSlice sees ["ns-01","ns-02",…]. namespaceFrom:"."
//     resolves to the name itself (jq `.` on a string). WITHOUT this
//     branch the scalar failed the map type-assert → conservative-deny
//     → every namespace dropped → empty Compositions page for everyone.
//   - any other type (int, nil, nested array, …) — conservative-deny:
//     a shape the refilter cannot reason about must never be served.
//
// RBAC fail-closed is preserved on every branch: a NamespaceFrom JQ
// error denies the item; an EvaluateRBAC error denies the item; a
// scalar the user has no grant for is dropped by EvaluateRBAC.
func refilterSlice(ctx context.Context, log *slog.Logger, username string, groups []string, uaf *templates.UserAccessFilterSpec, resources []string, items []any) ([]any, int, int) {
	kept := make([]any, 0, len(items))
	dropped := 0
	calls := 0
	for _, item := range items {
		switch item.(type) {
		case map[string]any, string:
			// Object OR bare scalar — both reach the evaluator. evalSingle
			// resolves NamespaceFrom against the item (jq handles a string
			// receiver fine) and calls EvaluateRBAC.
			permitted := evalSingle(ctx, log, username, groups, uaf, resources, item)
			calls++
			if permitted {
				kept = append(kept, item)
			} else {
				dropped++
			}
		default:
			// Unhandleable item type (int, nil, nested array, …).
			// Conservative-deny — the refilter cannot vouch for a shape
			// it does not understand.
			dropped++
		}
	}
	return kept, dropped, calls
}

// evalSingle resolves NamespaceFrom against item and calls EvaluateRBAC.
// Returns true iff RBAC permits (verb, group, <resource>, ns) for
// (username, groups) for AT LEAST ONE resource in `resources` — the
// Ship 0.30.129 OR semantics: a namespace is kept iff the user can
// perform Verb on ANY of the resolved resource plurals there.
//
// `resources` is the resolved plural set (resolveUAFResources): a single
// element [uaf.Resource] when ResourcesFrom is unset (pre-0.30.129
// behaviour, byte-identical), or the jq-derived set when it is set.
//
// item is any (0.30.111 Part 1): a K8s object (map[string]any) OR a
// bare scalar (string — the namespaces-stage name-array shape). jq
// evaluates a string receiver fine, so namespaceFrom:"." on "ns-01"
// yields "ns-01".
//
// JQ-eval errors and RBAC errors both fail closed. An EMPTY `resources`
// set denies (no resource to grant against) — never allow-all.
func evalSingle(ctx context.Context, log *slog.Logger, username string, groups []string, uaf *templates.UserAccessFilterSpec, resources []string, item any) bool {
	namespace := ""
	if uaf.NamespaceFrom != "" {
		ns, err := evalJQString(ctx, uaf.NamespaceFrom, item)
		if err != nil {
			log.Warn("userAccessFilter: NamespaceFrom JQ eval failed; treating item as denied",
				slog.String("expr", uaf.NamespaceFrom),
				slog.Any("err", err),
			)
			return false
		}
		namespace = ns
	}

	// OR semantics: keep the item iff the user is permitted on ANY
	// resource in the set. An empty set yields no iterations -> false
	// (fail-closed — an unresolvable / empty resource set never permits).
	for _, resource := range resources {
		allowed, err := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
			Username:  username,
			Groups:    groups,
			Verb:      uaf.Verb,
			Group:     uaf.Group,
			Resource:  resource,
			Namespace: namespace,
		})
		if err != nil {
			log.Warn("userAccessFilter: EvaluateRBAC error; treating resource as denied",
				slog.String("user", username),
				slog.String("verb", uaf.Verb),
				slog.String("group", uaf.Group),
				slog.String("resource", resource),
				slog.String("namespace", namespace),
				slog.Any("err", err),
			)
			continue // a transient evaluator error on one resource must
			// not mask a genuine grant on another — but it also must not
			// permit: try the rest, default-deny if none grant.
		}
		if allowed {
			return true
		}
	}
	return false
}

// resolveUAFResources resolves the RBAC resource-plural set for a UAF
// dispatch — Ship 0.30.129.
//
//   - ResourcesFrom unset: the set is the single static uaf.Resource.
//     Returns ([uaf.Resource], true) — pre-0.30.129 behaviour,
//     byte-identical. (An empty static Resource yields [""] — and an old
//     resolver path; evalSingle then checks resource "" which RBAC never
//     grants, i.e. fail-closed. That is the AC-129.9 cross-repo
//     deploy-order safety.)
//   - ResourcesFrom set: the expression is jq-evaluated ONCE against the
//     full dict, expected to yield a JSON array of strings. A jq error,
//     a non-array result, or a non-string element FAILS CLOSED — returns
//     (nil, false); the caller drops all items. An empty array is a
//     valid "no resources" answer: returns ([], true), and evalSingle
//     then denies every item (no resource to grant against).
func resolveUAFResources(ctx context.Context, log *slog.Logger, uaf *templates.UserAccessFilterSpec, dict map[string]any) ([]string, bool) {
	if uaf.ResourcesFrom == "" {
		return []string{uaf.Resource}, true
	}
	// Ship A (0.30.137): EvalValue returns gojq's result value directly
	// (design §3.4.3). Fail-closed for every non-single-array outcome.
	v, ok, err := EvalValue(ctx, uaf.ResourcesFrom, dict, jqsupport.ModuleLoader())
	if err != nil {
		// Parse/compile/runtime error OR ErrMultiYield. Pre-Ship-A the
		// jqutil.Eval err branch logs "JQ eval failed"; the multi-yield
		// case surfaced as a json.Unmarshal error logged "not a JSON
		// array". Both fail-closed identically — one warn; the distinction
		// was never security-load-bearing.
		log.Warn("userAccessFilter: ResourcesFrom JQ eval failed; fail-closed",
			slog.String("expr", uaf.ResourcesFrom),
			slog.Any("err", err),
		)
		return nil, false
	}
	arr, isArr := v.([]any) // !ok (zero-yield) => v==nil => isArr false
	if !ok || !isArr {
		// Zero-yield, or a non-array result. Pre-Ship-A json.Unmarshal of
		// "" or of a non-array into []any errors -> "not a JSON array" ->
		// fail-closed.
		log.Warn("userAccessFilter: ResourcesFrom result is not a JSON array; fail-closed",
			slog.String("expr", uaf.ResourcesFrom),
		)
		return nil, false
	}
	out := make([]string, 0, len(arr))
	seen := map[string]struct{}{}
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			log.Warn("userAccessFilter: ResourcesFrom yielded a non-string element; fail-closed",
				slog.String("expr", uaf.ResourcesFrom),
			)
			return nil, false
		}
		if s == "" {
			continue // skip empty plurals — they grant nothing
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out, true
}

// evalJQString evaluates expr against data and returns a Go string.
//
// Ship A (0.30.137): EvalValue returns gojq's result value directly
// (design §3.4.4). A string result IS the Go string — no quote-stripping
// round-trip. A null result maps to "" (pre-Ship-A trimJSONString("null")).
// A non-string single value is JSON-serialised verbatim (cold branch).
//
// DELIBERATE CHANGE (design §3.4.5, RATIFIED): a multi-yield expression
// pre-Ship-A returned the concatenated invalid-JSON string verbatim with
// err==nil — silent garbage flowing into a URL path / ResourceRef field /
// RBAC namespace. Ship A surfaces it as ErrMultiYield, so this returns
// ("", err) and the caller's existing error handling fires (fail-closed at
// both consumers — evalSingle's NamespaceFrom RBAC path treats it as denied).
//
// data is any (0.30.111 Part 1): jq `.` on a string yields the string;
// a map receiver is unchanged.
func evalJQString(ctx context.Context, expr string, data any) (string, error) {
	v, ok, err := EvalValue(ctx, expr, data, jqsupport.ModuleLoader())
	if errors.Is(err, ErrMultiYield) {
		// DELIBERATE CHANGE — see §3.4.5. Pre-Ship-A a multi-yield expr
		// returned the concatenated invalid-JSON string verbatim (latent
		// garbage). Ship A surfaces it as an error.
		return "", err
	}
	if err != nil {
		return "", err
	}
	if !ok {
		// Zero-yield. Pre-Ship-A: trimJSONString("") == "".
		return "", nil
	}
	switch s := v.(type) {
	case string:
		return s, nil // pre-Ship-A: trimJSONString strips the quotes
	case nil:
		return "", nil // pre-Ship-A: trimJSONString("null") == ""
	default:
		// Non-string single value. Pre-Ship-A: trimJSONString of the JSON
		// serialisation == the serialisation verbatim. Cold branch.
		return encodeValueCompact(s), nil
	}
}

// setRefilteredEmpty replaces dict[apiName] with the canonical empty
// shape (map with empty items slice). Used by the fail-closed paths.
func setRefilteredEmpty(dict map[string]any, apiName string) error {
	dict[apiName] = map[string]any{"items": []any{}}
	return nil
}

// emitRefilterFalsifier emits the per-call falsifier per plan
// §"Code-path falsifier" line:
//
//   userAccessFilter.dispatch=service_account user=X resource_type=...
//   refilter_dropped=N refilter_kept=M evaluate_rbac_calls=K
//
// Called by the resolver right after applyUserAccessFilter completes.
func emitRefilterFalsifier(log *slog.Logger, apiCall *templates.API, username string, res refilterResult) {
	if log == nil || apiCall == nil || apiCall.UserAccessFilter == nil {
		return
	}
	log.Info("userAccessFilter",
		slog.String("subsystem", "uaf"),
		slog.String("dispatch", "service_account"),
		slog.String("user", username),
		slog.String("api", apiCall.Name),
		slog.String("verb", apiCall.UserAccessFilter.Verb),
		slog.String("group", apiCall.UserAccessFilter.Group),
		slog.String("resource", apiCall.UserAccessFilter.Resource),
		slog.Int("refilter_kept", res.Kept),
		slog.Int("refilter_dropped", res.Dropped),
		slog.Int("evaluate_rbac_calls", res.EvaluateRBACCalls),
	)
}
