// informer_dispatch_rbac.go — Tag 0.30.100: post-LIST per-item RBAC
// filter for the resolver pivot's served LIST branch.
//
// THE BUG (0.30.99 Phase-6 bench "Finding 1"):
//   dispatchViaInformer's LIST branch returned the raw informer
//   namespace partition (rw.ListObjectsServable) with NO per-user
//   filter. dispatchViaInformer never reads call.Endpoint, so it
//   bypasses the per-user `<username>-clientconfig` bearer token
//   entirely. For an apiserver-routed call WITHOUT a userAccessFilter
//   stanza (e.g. compositions-list) that per-user token was the ONLY
//   RBAC gate — the pivot bypassed it and a narrow-RBAC user
//   (`cyberjoker`) saw ALL 49,999 compositions.
//
// THE FIX:
//   filterListByRBAC runs every pivot-served LIST through a per-item
//   EvaluateRBAC check — the same in-process, tokenless typed-RBAC
//   indexer the userAccessFilter refilter uses (internal/rbac/
//   evaluate.go). It generalises the UAF refilter (refilter.go) to fire
//   on EVERY pivot-served LIST, not just stages that declare a
//   userAccessFilter stanza.
//
// FAIL-CLOSED CONTRACT (binding — security):
//   - No identity on the context (xcontext.UserInfo error) → the whole
//     LIST is not served (servable=false → apiserver fallthrough). The
//     apiserver path narrows via the per-user token; falling through is
//     correct and never leaks.
//   - EvaluateRBAC returns an error for an item → that item is DROPPED.
//   - A nil/empty username after a successful UserInfo read → treated
//     as "no subject matches" by EvaluateRBAC → every item dropped.
//   Under no path does an unfiltered partition reach the caller.
//
// PERFORMANCE:
//   EvaluateRBAC is an indexed read against the typed-RBAC informer
//   cache (ListTypedObjects + GetTypedObject — O(bindings) per call,
//   not a per-call apiserver round-trip). The existing UAF refilter
//   already does per-item EvaluateRBAC at production scale, so per-item
//   eval on a large LIST is proven-acceptable. At ~50K items the cost
//   is 50K × (typed-indexer walk) — bounded, in-process, no I/O. The
//   0.30.10 permission-check cache amortises repeat (user, ns) tuples.
//
// BINDINGS:
//   - feedback_no_special_cases.md: the filter is uniform over every
//     GVR — no per-resource carve-out. Verb is hardcoded "list"
//     because this is the served-LIST branch (the call IS a list).
//   - feedback_restaction_no_widget_logic.md: lives in the resolver
//     layer, not widget canonicalization.
//   - feedback_l1_invalidation_delete_only.md: filtering happens before
//     the bytes reach call.ResponseHandler / the resolved-cache write,
//     so any cached entry is already user-scoped.

package api

import (
	"context"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// filterListByRBAC returns the subset of items the context's user is
// authorized to `list`, evaluated per-item against the in-process
// typed-RBAC indexer.
//
// Return contract:
//   - (subset, true)  — identity present; subset is the RBAC-permitted
//     items (possibly empty). Caller serves the subset.
//   - (nil, false)    — NO identity on the context. Caller MUST NOT
//     serve; fall through to the apiserver (whose per-user token gate
//     narrows correctly). This is the fail-closed path: an absent
//     identity is never permitted to yield an unfiltered partition.
//
// gvr supplies the API group + resource for the EvaluateRBAC tuple. The
// verb is fixed "list" — this is the served-LIST branch.
//
// Per-item EvaluateRBAC errors fail closed (item dropped) — see file
// header. namespace for each item is the item's own metadata.namespace
// (cluster-scoped items, namespace=="", evaluate against cluster-wide
// RBAC, which is the correct apiserver-equivalent semantics).
func filterListByRBAC(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	items []*unstructured.Unstructured,
) ([]*unstructured.Unstructured, bool) {
	log := xcontext.Logger(ctx)

	user, err := xcontext.UserInfo(ctx)
	if err != nil {
		// FAIL-CLOSED: no identity → cannot vouch for ANY item. Do not
		// serve; fall through to the apiserver. Returning an empty
		// served list here would also be safe, but fallthrough keeps
		// the apiserver's per-user-token narrowing as the authoritative
		// answer and avoids masking a genuine "no objects" result.
		log.Warn("informer_dispatch.rbac_filter.no_identity",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.Any("err", err),
			slog.String("action", "fallthrough_to_apiserver"),
		)
		return nil, false
	}

	kept := make([]*unstructured.Unstructured, 0, len(items))
	dropped := 0
	for _, it := range items {
		if it == nil {
			// Defensive: a nil entry cannot be vouched for. Drop it
			// (fail-closed) — listFromIndexer never emits nil, but the
			// per-item loop must not trust that invariant blindly.
			dropped++
			continue
		}
		allowed, err := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
			Username:  user.Username,
			Groups:    user.Groups,
			Verb:      "list",
			Group:     gvr.Group,
			Resource:  gvr.Resource,
			Namespace: it.GetNamespace(),
		})
		if err != nil {
			// FAIL-CLOSED: an evaluator hiccup never permits a leak.
			log.Warn("informer_dispatch.rbac_filter.evaluate_error",
				slog.String("subsystem", "cache"),
				slog.String("user", user.Username),
				slog.String("gvr", gvr.String()),
				slog.String("namespace", it.GetNamespace()),
				slog.Any("err", err),
				slog.String("action", "drop_item"),
			)
			dropped++
			continue
		}
		if allowed {
			kept = append(kept, it)
		} else {
			dropped++
		}
	}

	if dropped > 0 {
		dispatchInformerRBACDropped.Add(uint64(dropped))
	}
	log.Debug("informer_dispatch.rbac_filter",
		slog.String("subsystem", "cache"),
		slog.String("user", user.Username),
		slog.String("gvr", gvr.String()),
		slog.Int("served", len(items)),
		slog.Int("kept", len(kept)),
		slog.Int("dropped", dropped),
	)
	return kept, true
}
