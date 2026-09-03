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

	"github.com/itchyny/gojq"
	xcontext "github.com/krateo-platformops/plumbing/context"
	templates "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/rbac"
	jqsupport "github.com/krateo-platformops/snowplow/internal/support/jq"
)

// uafReadVerbs is the set of Kubernetes RBAC verbs that unambiguously express
// a READ intent — A-4 (1.12.3).
//
// Lower-case only, and that is a correctness point rather than a style one.
// rbac/evaluate.go stringSliceMatches tests the rule's "*" wildcard FIRST, so an
// upper-case "GET" checked against a wildcard grant is silently ALLOWED, and
// against every non-wildcard grant is silently DENIED. Either way the operator
// sees a filter that quietly does the wrong thing. Treating a non-lower-case
// verb as "not a read verb" is what surfaces the typo.
//
// Not a resource/path/user special-case (feedback_no_special_cases): a fixed,
// mechanism-level verb vocabulary, uniform across every userAccessFilter.
var uafReadVerbs = map[string]struct{}{
	"get":   {},
	"list":  {},
	"watch": {},
}

// uafVerbIsRead reports whether verb unambiguously expresses a read intent.
// Anything else — a write verb, a typo, an upper-case "GET", the empty string —
// is not read.
//
// WHAT THIS IS FOR, AND WHAT IT IS DELIBERATELY NOT. uaf.Verb is threaded
// verbatim into rbac.EvaluateRBAC (evalSingle below). The dangerous shape is a
// stage that returns objects of resource R filtered by a WRITE verb on R: it
// keeps the objects the caller may mutate rather than the ones they may read,
// so a user with a broad create grant and a narrow get grant is SHOWN objects
// they cannot read. That is a genuine scope inversion.
//
// But "non-read verb" alone does NOT identify it. Three LIVE portal RESTActions
// (testdata/portal_uaf_corpus.yaml, portal @ b2a558d) use `verb: create`
// legitimately: the api-step returns NAMESPACES and the filter checks create on
// a DIFFERENT resource, so it answers "which namespaces may I create X in" — a
// form picker that discloses nothing about objects the caller cannot read.
// A-4's first cut rejected exactly these at admission and dropped their items
// here; it would have failed the portal upgrade and silently emptied three
// namespace pickers. See core.go for why the CRD rule was removed too.
//
// So in 1.12.3 this predicate is WARN-ONLY. It never drops an item.
//
// THE 1.13.0 RULE, written down so the flip is mechanical and does not
// re-litigate this. Drop the items only when BOTH hold:
//
//	(a) !uafVerbIsRead(uaf.Verb), and
//	(b) the UAF's resource set intersects the resource the api-step RETURNS.
//
// (b) is the part that separates the portal's pickers (namespaces filtered by
// create on compositiondefinitions/alerts — disjoint, allowed) from the
// inversion (compositions filtered by create on compositions — equal, dropped).
// Deriving the returned resource means parsing apiCall.Path, and that path may
// be jq-templated ("${...}"): when it is, or when it cannot be parsed to a
// plural, (b) MUST evaluate FALSE — fail OPEN, serve the items. A fail-closed
// default on an underivable path recreates the very outage this warn-only
// stage exists to avoid. Resolve the UAF side with resolveUAFResources, which
// already handles the resourcesFrom jq fan-out.
func uafVerbIsRead(verb string) bool {
	_, ok := uafReadVerbs[verb]
	return ok
}

// warnUAFVerbNonRead emits the A-4 operator signal from BOTH refilter entry
// points (applyUserAccessFilterOnPig — the live jsonHandlerCore callsite — and
// its applyUserAccessFilter twin, kept symmetric so the twin cannot become a
// silent bypass if it is ever re-wired).
//
// It returns nothing and changes no items: 1.12.3 is observation only. It fires
// exactly ONCE per (RA stage) evaluation, never per item — a non-read verb is a
// single authoring choice in one CR, so per-item logging would flood the
// operator on a 500-item list and tell them nothing extra.
func warnUAFVerbNonRead(log *slog.Logger, stageName string, uaf *templates.UserAccessFilterSpec) {
	if log == nil || uafVerbIsRead(uaf.Verb) {
		return
	}
	resource := uaf.Resource
	if resource == "" {
		resource = uaf.ResourcesFrom
	}
	log.Warn("userAccessFilter: verb is not a read verb",
		slog.String("subsystem", "uaf"),
		slog.String("api", stageName),
		slog.String("verb", uaf.Verb),
		slog.String("uaf_resource", resource),
		slog.String("group", uaf.Group),
		slog.String("effect", "none in 1.12.3 — items are served unchanged; this is an observation-only signal"),
		slog.String("risk", "a non-read verb is only a scope inversion when it is checked against the SAME resource the stage returns; then the filter keeps what the caller may MUTATE, not what they may read"),
		slog.String("next", "1.13.0 drops items only for the same-resource case (failing open on jq-templated paths); a cross-resource picker stays served"),
	)
}

// refilterResult is the (kept, dropped, total) summary the resolver
// logs as the per-call falsifier per plan §"Code-path falsifier":
//
//	userAccessFilter.dispatch=service_account ... refilter_dropped=N refilter_kept=M evaluate_rbac_calls=K
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
//   - map[string]any with an "items" slice (typical K8s list response);
//   - []any (rare; some endpoints flatten the list).
//
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

	// A-1 (1.12.3) — EXECUTION-BASED UAF signal for the L1 Put-gate. Record that
	// the resolve under this ctx produced a userAccessFilter-NARROWED body, so
	// every Put site downstream declines to cache it: the cache key folds
	// BindingUID and RBACSubGen, neither of which separates requesters by their
	// per-object narrowing scope, so caching a refiltered body serves one
	// requester's rows to another.
	//
	// This is the second of the two bump sites cache/uaf_touched_sink.go
	// describes, and the one that is not declaration-blind. The apiref
	// chokepoint bump fires when the apiRef'd RESTAction DECLARES a UAF; it
	// cannot see a CHAIN (a parent RA declaring nothing whose inner step consumes
	// a UAF child). Only bumping at the refilter itself observes execution. No
	// live RA forms such a chain today, which closes it by corpus accident rather
	// than by code — one customer CR away from being false.
	//
	// Placed BEFORE any item is evaluated so the signal does not depend on the
	// filter keeping anything: a refilter that drops every item still narrowed
	// the body. No-op when no sink is installed on ctx (nil-receiver-safe), so
	// every non-Put path is byte-identical.
	//
	// Symmetric with the live entry deliberately (architect ruling): this twin is
	// currently dead, but if it is ever re-wired an unbumped path would silently
	// cache narrowed bytes.
	cache.BumpUAFTouched(ctx)

	// A-4 (1.12.3) — WARN-ONLY, once per stage, before any per-item work. Kept
	// symmetric with the live applyUserAccessFilterOnPig entry so this twin
	// cannot silently become a bypass if it is ever re-wired. Drops nothing;
	// 1.13.0 flips it to the same-resource rule (see uafVerbIsRead).
	warnUAFVerbNonRead(log, apiCall.Name, uaf)

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
			// map as the single object. #121 1b — compile the
			// NamespaceFrom expression once (single item, but shares the
			// hoisted signature). #123 — compile NameFrom too.
			nsExpr, nsCode := compileNamespaceFrom(uaf)
			nameExpr, nameCode := compileNameFrom(uaf)
			permitted := evalSingle(ctx, log, user.Username, user.Groups, uaf, resources, v, nsExpr, nsCode, nameExpr, nameCode)
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

	// #121 1b — compile the per-item NamespaceFrom expression ONCE for the
	// whole slice, not O(items) times. The expression is the SAME for every
	// item (it comes from uaf.NamespaceFrom / the Ship-S.1 default), so the
	// pre-1b path re-Parse+Compiled the identical query per item inside
	// EvalValue (jqvalue.go), paying N parse+compile cycles for one filter.
	// At the 50K composition boot walk an 8854-item RBAC filter paid ~0.3s of
	// pure recompile (docs/boot-walk-deadline-rootcause-2026-07-09.md §4).
	// Hoisting the compile here shaves that off every large filter. The
	// compiled *gojq.Code is reused read-only across items — gojq.Code is
	// immutable after Compile (mutable state lives in the per-Run iterator),
	// so this is safe even though evalSingle runs it per item.
	//
	// SECURITY / fail-closed is UNCHANGED: nsCode is threaded into evalSingle
	// → evalJQString, which applies the SAME (value, ok, err) contract as
	// before; a compile error here fails closed (nsCode==nil → evalSingle
	// treats every item as denied), exactly as the pre-1b inline EvalValue
	// compile-error branch did per item.
	nsExpr, nsCode := compileNamespaceFrom(uaf)

	// #123 — compile the per-item NameFrom expression ONCE for the whole
	// slice, symmetric with NamespaceFrom above. NameFrom derives each
	// object's NAME, threaded into EvaluateOptions.Name so that a
	// resourceNames-scoped RBAC grant (Role/ClusterRole rule with a
	// non-empty resourceNames, valid only for name-specific verbs) is
	// honoured. Pre-#123 the serve path passed no Name → every
	// resourceNames-scoped grant matched nothing → all named objects were
	// silently dropped (under-serve / fail-closed). Same hoist-once
	// mechanism and fail-closed contract as nsCode: a compile error yields
	// nameCode==nil → evalSingle denies the item.
	nameExpr, nameCode := compileNameFrom(uaf)

	for _, item := range items {
		switch item.(type) {
		case map[string]any, string:
			// Object OR bare scalar — both reach the evaluator. evalSingle
			// resolves NamespaceFrom + NameFrom against the item (jq handles a
			// string receiver fine) and calls EvaluateRBAC.
			permitted := evalSingle(ctx, log, username, groups, uaf, resources, item, nsExpr, nsCode, nameExpr, nameCode)
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
func evalSingle(ctx context.Context, log *slog.Logger, username string, groups []string, uaf *templates.UserAccessFilterSpec, resources []string, item any, nsExpr string, nsCode *gojq.Code, nameExpr string, nameCode *gojq.Code) bool {
	// #121 1b — nsExpr + nsCode are the ONCE-compiled NamespaceFrom
	// expression, hoisted out of the per-item loop by the caller
	// (compileNamespaceFrom). nsExpr is kept for error/log fidelity; nsCode
	// is the reusable compiled program. A nil nsCode means the expression
	// failed to compile — fail-closed (deny the item), matching the pre-1b
	// per-item EvalValue compile-error branch.
	if nsCode == nil {
		log.Warn("userAccessFilter: NamespaceFrom JQ compile failed; treating item as denied",
			slog.String("expr", nsExpr),
		)
		return false
	}
	ns, err := evalJQStringCompiled(ctx, nsCode, item)
	if err != nil {
		log.Warn("userAccessFilter: NamespaceFrom JQ eval failed; treating item as denied",
			slog.String("expr", nsExpr),
			slog.Any("err", err),
		)
		return false
	}
	namespace := ns

	// #123 — derive the per-object NAME so a resourceNames-scoped grant is
	// honoured. The name only ever matters for a NAME-SPECIFIC verb
	// (get/update/patch/delete): the RBAC evaluator scopes resourceNames to
	// exactly those verbs (rbac.IsNameSpecificVerb, the same predicate
	// resourceNameMatches uses), so for a COLLECTION verb (list/watch/…) the
	// Name is never consulted. We therefore evaluate NameFrom ONLY for
	// name-specific verbs; for collection verbs Name stays "" — byte-identical
	// to the pre-#123 serve path (which passed no Name), and, critically,
	// avoids fail-closing on a NameFrom JQ error that would be IRRELEVANT to a
	// list grant (e.g. the namespaces-stage bare-string shape where the item
	// is a name string, not an object with .metadata.name). This is not a
	// resource/path special-case — it is the evaluator's own verb contract.
	name := ""
	if rbac.IsNameSpecificVerb(uaf.Verb) {
		// nameExpr + nameCode are the ONCE-compiled NameFrom expression,
		// hoisted by the caller (compileNameFrom), symmetric with nsExpr/nsCode.
		// A nil nameCode means the expression failed to compile — fail-closed
		// (deny the item), identical to the NamespaceFrom compile-error branch.
		if nameCode == nil {
			log.Warn("userAccessFilter: NameFrom JQ compile failed; treating item as denied",
				slog.String("expr", nameExpr),
			)
			return false
		}
		n, err := evalJQStringCompiled(ctx, nameCode, item)
		if err != nil {
			log.Warn("userAccessFilter: NameFrom JQ eval failed; treating item as denied",
				slog.String("expr", nameExpr),
				slog.Any("err", err),
			)
			return false
		}
		name = n
	}

	// OR semantics: keep the item iff the user is permitted on ANY
	// resource in the set. An empty set yields no iterations -> false
	// (fail-closed — an unresolvable / empty resource set never permits).
	for _, resource := range resources {
		// Ship 0.30.242 H.c-layered Phase 2 step 2a — per-item caller
		// ignores matchedBindingUID return.
		allowed, _, err := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
			Username:  username,
			Groups:    groups,
			Verb:      uaf.Verb,
			Group:     uaf.Group,
			Resource:  resource,
			Namespace: namespace,
			// #123 — thread the per-object name so a resourceNames-scoped
			// grant (name-specific verb only) matches the named object.
			// Empty for collection verbs is harmless: the evaluator scopes
			// resourceNames to name-specific verbs, so a plain list grant is
			// evaluated identically whether or not Name is set (INERTNESS).
			Name: name,
			// Ship L1 (0.30.252): per-item caller discards
			// matchedBindingUID — skip the CRB/RB stable-sort.
			SkipBindingUID: true,
		})
		if err != nil {
			log.Warn("userAccessFilter: EvaluateRBAC error; treating resource as denied",
				slog.String("user", username),
				slog.String("verb", uaf.Verb),
				slog.String("group", uaf.Group),
				slog.String("resource", resource),
				slog.String("namespace", namespace),
				slog.String("name", name),
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

// compileNamespaceFrom resolves the effective NamespaceFrom expression for a
// UAF and compiles it ONCE (#121 1b), returning (expr, code). It centralises
// the Ship-S.1 default so the per-item loop and the single-object branches
// share one derivation — the expression is item-independent, so it is
// compiled here, before any item is evaluated, and reused across every item.
//
// Ship S.1 — an absent/empty NamespaceFrom defaults to ".metadata.namespace"
// (the dominant namespaced-object shape) rather than a cluster-scope
// (namespace=="") RBAC check. The cluster-scope default was a SEMANTICS BUG:
// it denied narrow devs who hold the grant only in their own namespace. The
// CRD-level +kubebuilder:default makes the apiserver fill this for stored
// RESTAction CRs; this in-code default is the belt-and-suspenders for
// pre-default CRs and any caller that passes a UAF with an empty
// NamespaceFrom directly. An explicit "." or ".metadata.name" still overrides
// verbatim.
//
// A compile error returns (expr, nil): evalSingle then fails closed (deny),
// identical to the pre-1b per-item EvalValue compile-error branch. The
// ModuleLoader matches the pre-1b evalJQString call (jqsupport.ModuleLoader()).
func compileNamespaceFrom(uaf *templates.UserAccessFilterSpec) (string, *gojq.Code) {
	nsExpr := uaf.NamespaceFrom
	if nsExpr == "" {
		nsExpr = ".metadata.namespace"
	}
	code, err := CompileJQ(nsExpr, jqsupport.ModuleLoader())
	if err != nil {
		return nsExpr, nil
	}
	return nsExpr, code
}

// compileNameFrom resolves the effective NameFrom expression for a UAF and
// compiles it ONCE (#123), returning (expr, code). Symmetric with
// compileNamespaceFrom: it centralises the default so the per-item loop and
// the single-object branches share one derivation — the expression is
// item-independent, so it is compiled here, before any item is evaluated,
// and reused across every item.
//
// #123 — an absent/empty NameFrom defaults to ".metadata.name" (the
// dominant K8s-object shape). The derived name is threaded into
// EvaluateOptions.Name so a resourceNames-scoped RBAC grant (a rule with a
// non-empty resourceNames, valid only for name-specific verbs —
// get/update/patch/delete) is honoured. Pre-#123 the serve path passed no
// Name → every resourceNames-scoped grant matched nothing → all named
// objects were silently dropped (under-serve / fail-closed). The CRD-level
// +kubebuilder:default fills this for stored RESTAction CRs; this in-code
// default is the belt-and-suspenders for pre-default CRs and any caller
// passing a UAF with an empty NameFrom directly. An explicit "." (the
// namespaces-stage bare-name-string shape) still overrides verbatim.
//
// A compile error returns (expr, nil): evalSingle then fails closed (deny),
// identical to the compileNamespaceFrom compile-error branch. The
// ModuleLoader matches the NamespaceFrom compile (jqsupport.ModuleLoader()).
func compileNameFrom(uaf *templates.UserAccessFilterSpec) (string, *gojq.Code) {
	nameExpr := uaf.NameFrom
	if nameExpr == "" {
		nameExpr = ".metadata.name"
	}
	code, err := CompileJQ(nameExpr, jqsupport.ModuleLoader())
	if err != nil {
		return nameExpr, nil
	}
	return nameExpr, code
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
	return jqValueToString(v, ok, err)
}

// evalJQStringCompiled is the #121 1b compiled-code twin of evalJQString: it
// runs an already-compiled *gojq.Code (via EvalValueCompiled) instead of
// re-Parse+Compiling `expr` per call. Byte-identical (value, ok, err)
// mapping to evalJQString — both delegate to jqValueToString — so a caller
// switching from evalJQString to this pays the compile once (CompileJQ) and
// gets the SAME string/error result per item, including the DELIBERATE
// multi-yield error surface (§3.4.5).
func evalJQStringCompiled(ctx context.Context, code *gojq.Code, data any) (string, error) {
	v, ok, err := EvalValueCompiled(ctx, code, data)
	return jqValueToString(v, ok, err)
}

// jqValueToString maps EvalValue's (value, ok, err) triple to the (string,
// error) shape evalJQString has always returned. Extracted for #121 1b so
// the recompiling path (evalJQString) and the compiled-code path
// (evalJQStringCompiled) apply IDENTICAL mapping — the anti-drift principle
// (a single body, not two copies that can diverge).
//
// DELIBERATE CHANGE (design §3.4.5, RATIFIED): a multi-yield expression
// pre-Ship-A returned the concatenated invalid-JSON string verbatim with
// err==nil — silent garbage flowing into a URL path / ResourceRef field /
// RBAC namespace. Ship A surfaces it as ErrMultiYield, so this returns
// ("", err) and the caller's existing error handling fires (fail-closed at
// both consumers — evalSingle's NamespaceFrom RBAC path treats it as denied).
func jqValueToString(v any, ok bool, err error) (string, error) {
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

// applyUserAccessFilterOnPig is the Ship-0.30.235 jsonHandlerCore-side
// shim. It runs the UAF refilter on the per-stage `pig` map (the
// {opts.key: rawEnvelope} jsonHandlerCore assembles BEFORE the stage
// filter applies its projection). Mutates pig[stageName] in place with
// the user-narrowed subset. Reuses refilterSlice / evalSingle /
// resolveUAFResources — identical fail-closed semantics to
// applyUserAccessFilter (refilter.go:71). The pre-0.30.235 post-g.Wait()
// applyUserAccessFilter call (resolve.go:879-888 in dbbea37) is DELETED;
// this shim is the new sole production callsite.
//
// Ship K / 0.30.245 — added the `dict` parameter. resolveUAFResources
// now evaluates `userAccessFilter.resourcesFrom` against the FULL
// resolver dict (where upstream stage outputs live, e.g. `.crds`
// populated by a prior stage), NOT the per-stage `pig` (which only
// contains {stageName: <thisStageRawEnvelope>}). Pre-Ship-K used `pig`,
// which never contained upstream stage keys → ResourcesFrom always
// returned [] → fail-closed dropped every item → 0-count piechart
// regression on the live multi-stage compositions-list RA.
// `dict` MAY be nil for callers that don't have an upstream dict
// (single-stage RAs); resolveUAFResources handles nil safely by
// returning empty resources, which is the existing fail-closed
// shape for an unresolvable ResourcesFrom — identical to pre-Ship-K
// behaviour for that case. Per-item refilter still uses `pig` items —
// that scope is correct for items.
func applyUserAccessFilterOnPig(ctx context.Context, pig map[string]any, dict map[string]any, stageName string, uaf *templates.UserAccessFilterSpec) refilterResult {
	log := xcontext.Logger(ctx)
	res := refilterResult{}

	if uaf == nil {
		return res
	}

	// A-1 (1.12.3) — EXECUTION-BASED UAF signal for the L1 Put-gate. Record that
	// the resolve under this ctx produced a userAccessFilter-NARROWED body, so
	// every Put site downstream declines to cache it: the cache key folds
	// BindingUID and RBACSubGen, neither of which separates requesters by their
	// per-object narrowing scope, so caching a refiltered body serves one
	// requester's rows to another.
	//
	// This is the second of the two bump sites cache/uaf_touched_sink.go
	// describes, and the one that is not declaration-blind. The apiref
	// chokepoint bump fires when the apiRef'd RESTAction DECLARES a UAF; it
	// cannot see a CHAIN (a parent RA declaring nothing whose inner step consumes
	// a UAF child). Only bumping at the refilter itself observes execution. No
	// live RA forms such a chain today, which closes it by corpus accident rather
	// than by code — one customer CR away from being false.
	//
	// Placed BEFORE any item is evaluated so the signal does not depend on the
	// filter keeping anything: a refilter that drops every item still narrowed
	// the body. No-op when no sink is installed on ctx (nil-receiver-safe), so
	// every non-Put path is byte-identical.
	//
	// THIS IS THE LIVE SITE (jsonHandlerCore) and a hard tag condition for
	// 1.12.3 per cache/uaf_touched_sink.go.
	cache.BumpUAFTouched(ctx)

	// A-4 (1.12.3) — WARN-ONLY, once per stage, before any per-item work. This
	// is the LIVE production callsite; the applyUserAccessFilter twin above
	// carries the identical call so neither entry point can drift. Drops
	// nothing; 1.13.0 flips it to the same-resource rule (see uafVerbIsRead).
	warnUAFVerbNonRead(log, stageName, uaf)

	user, err := xcontext.UserInfo(ctx)
	if err != nil {
		log.Error("userAccessFilter: cannot extract UserInfo; dropping all items (fail-closed)",
			slog.String("api", stageName),
			slog.Any("err", err),
		)
		_ = setRefilteredEmpty(pig, stageName)
		return res
	}

	// Ship 0.30.129 ResourcesFrom contract — fail-closed.
	// Ship K / 0.30.245 — pass `dict` (full resolver scope) instead of
	// the per-stage `pig`. `dict` carries upstream stage outputs that
	// resourcesFrom expressions reference (e.g. `.crds`).
	resources, resOK := resolveUAFResources(ctx, log, uaf, dict)
	if !resOK {
		log.Error("userAccessFilter: resource set unresolved; dropping all items (fail-closed)",
			slog.String("api", stageName),
		)
		_ = setRefilteredEmpty(pig, stageName)
		return res
	}

	raw, ok := pig[stageName]
	if !ok || raw == nil {
		return res
	}

	switch v := raw.(type) {
	case map[string]any:
		itemsRaw, hasItems := v["items"]
		if !hasItems {
			// Single-object shape (GET-by-name). #121 1b — compile the
			// NamespaceFrom expression once (shares the hoisted signature).
			// #123 — compile NameFrom too.
			nsExpr, nsCode := compileNamespaceFrom(uaf)
			nameExpr, nameCode := compileNameFrom(uaf)
			permitted := evalSingle(ctx, log, user.Username, user.Groups, uaf, resources, v, nsExpr, nsCode, nameExpr, nameCode)
			res.EvaluateRBACCalls++
			if permitted {
				res.Kept++
			} else {
				res.Dropped++
				pig[stageName] = map[string]any{}
			}
			return res
		}
		items, ok := itemsRaw.([]any)
		if !ok {
			log.Warn("userAccessFilter: items is not a slice; passing through unchanged",
				slog.String("api", stageName),
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
		pig[stageName] = kept
		res.Kept = len(kept)
		res.Dropped = dropped
		res.EvaluateRBACCalls = calls

	default:
		log.Warn("userAccessFilter: unrecognised result shape; passing through unchanged",
			slog.String("api", stageName),
			slog.Any("result_type", fmt.Sprintf("%T", raw)),
		)
	}

	return res
}

// emitRefilterFalsifierFromHandler emits the per-call refilter falsifier
// from jsonHandlerCore — Ship 0.30.235 sibling of emitRefilterFalsifier.
// Output shape identical (consumed by the bench's grep harness).
func emitRefilterFalsifierFromHandler(ctx context.Context, log *slog.Logger, apiCallName string, uaf *templates.UserAccessFilterSpec, res refilterResult) {
	if log == nil || uaf == nil {
		return
	}
	username := ""
	if u, err := xcontext.UserInfo(ctx); err == nil {
		username = u.Username
	}
	log.Debug("userAccessFilter",
		slog.String("subsystem", "uaf"),
		slog.String("dispatch", "service_account"),
		slog.String("user", username),
		slog.String("api", apiCallName),
		slog.String("verb", uaf.Verb),
		slog.String("group", uaf.Group),
		slog.String("resource", uaf.Resource),
		slog.Int("refilter_kept", res.Kept),
		slog.Int("refilter_dropped", res.Dropped),
		slog.Int("evaluate_rbac_calls", res.EvaluateRBACCalls),
	)
}

// emitRefilterFalsifier emits the per-call falsifier per plan
// §"Code-path falsifier" line:
//
//	userAccessFilter.dispatch=service_account user=X resource_type=...
//	refilter_dropped=N refilter_kept=M evaluate_rbac_calls=K
//
// Called by the resolver right after applyUserAccessFilter completes.
func emitRefilterFalsifier(log *slog.Logger, apiCall *templates.API, username string, res refilterResult) {
	if log == nil || apiCall == nil || apiCall.UserAccessFilter == nil {
		return
	}
	log.Debug("userAccessFilter",
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
