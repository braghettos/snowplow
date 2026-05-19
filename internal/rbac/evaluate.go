// Package rbac — EvaluateRBAC: in-process Role-Based Access Control
// evaluator (Tag 0.30.4, Revision 1 binding).
//
// In cache=on mode snowplow MUST satisfy every Role-Based Access Control
// check against the informer-cached RBAC types (Role, RoleBinding,
// ClusterRole, ClusterRoleBinding). ZERO SubjectAccessReview calls to
// apiserver in cache=on mode — that rule is hard-tested in
// evaluate_test.go and is the rollback trigger for this tag.
//
// In cache=off mode the helper falls through to SubjectAccessReview
// (correctness baseline) — preserves the CACHE_ENABLED toggle's
// removability contract per project_redis_removal.md.
package rbac

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/snowplow/internal/cache"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// evaluateRBACCallCount is a process-scoped counter of EvaluateRBAC
// invocations. It exists so tests can assert call-count properties (the
// 0.30.111 namespace-keyed memo falsifier asserts an N-namespace LIST
// makes exactly N EvaluateRBAC calls). One atomic add per call — the
// production cost is negligible and the counter is never read on a hot
// path. Mirrors the established api-package metrics-counter pattern
// (dispatchInformerRBACDropped).
var evaluateRBACCallCount atomic.Uint64

// EvaluateRBACCallCount returns the number of EvaluateRBAC calls since
// process start (or since the last ResetEvaluateRBACCallCount). Exported
// for test instrumentation; production code has no reason to read it.
func EvaluateRBACCallCount() uint64 {
	return evaluateRBACCallCount.Load()
}

// ResetEvaluateRBACCallCount zeroes the EvaluateRBAC call counter.
// TEST-ONLY — production code MUST NOT call it.
func ResetEvaluateRBACCallCount() {
	evaluateRBACCallCount.Store(0)
}

// EvaluateOptions captures every input the evaluator needs to make a
// permit/deny decision. Mirrors authorizationv1.ResourceAttributes so
// the cache=off fallback (SubjectAccessReview) is a one-to-one mapping.
type EvaluateOptions struct {
	// Username is the authenticated user (e.g. "cyberjoker").
	Username string
	// Groups are the user's group memberships (e.g. {"devs"}).
	Groups []string
	// Verb is the Kubernetes Role-Based Access Control verb (lowercase,
	// e.g. "get", "list", "watch", "create", "update", "patch",
	// "delete"). Wildcard "*" matches every verb.
	Verb string
	// Group is the API group ("" for the core group).
	Group string
	// Resource is the plural resource name (e.g. "secrets",
	// "restactions").
	Resource string
	// Namespace is the request namespace. Empty string = cluster-wide.
	Namespace string
	// Name is the request object's name. For name-specific verbs
	// ("get"/"update"/"patch"/"delete" and similar) it is the name of
	// the single object the request targets — e.g. for a per-item check
	// on a served LIST it is that item's metadata.name. Empty string
	// means "no single named object" (the request is a collection-verb
	// such as "list"/"watch"/"create"/"deletecollection", or the caller
	// did not thread a name).
	//
	// Name is consumed by Kubernetes `resourceNames` semantics
	// (ResourceNameMatches): a PolicyRule with a non-empty
	// rule.ResourceNames only matches a request whose Name is in that
	// list — and only for name-specific verbs. A resourceNames-scoped
	// rule NEVER grants a collection verb. See rulesPermit /
	// resourceNameMatches below.
	Name string
}

// Ship B (0.30.138): the rbac-package GVR vars are dead — the snapshot
// reads come from `*cache.RBACSnapshot` fields, and the writer side
// owns the GVR set via `rbacTypedGVRs` (strip.go:101-106). Removing
// the duplicate set here aligns with the single-source-of-truth rule
// recorded in the design's `feedback_no_special_cases` discussion.

// EvaluateRBAC returns true iff opts describes an action permitted by
// the cluster Role-Based Access Control rules, evaluated against the
// in-process informer cache.
//
// Semantics match Kubernetes apiserver:
//   - any matching rule permits (no deny rules in RBAC v1)
//   - "*" wildcards match every verb / resource / API group
//   - empty Username / Groups is treated as "no subject matches" → deny
//
// In cache=off mode (cache.Disabled() == true) the function falls
// through to SubjectAccessReview-via-UserCan with a synthesised
// UserCanOptions. The fallback exists so CACHE_ENABLED=false retains
// the upstream correctness baseline (project_redis_removal.md).
//
// Returns (true, nil) on permit, (false, nil) on deny, (false, err) on
// internal evaluator error (failed type assertion etc.).
func EvaluateRBAC(ctx context.Context, opts EvaluateOptions) (bool, error) {
	log := xcontext.Logger(ctx)
	evaluateRBACCallCount.Add(1)

	if cache.Disabled() {
		// Cache=off correctness baseline. UserCan reads the user's
		// endpoint from ctx and issues a SelfSubjectAccessReview.
		ok := UserCan(ctx, UserCanOptions{
			Verb: opts.Verb,
			GroupResource: schema.GroupResource{
				Group: opts.Group, Resource: opts.Resource,
			},
			Namespace: opts.Namespace,
		})
		return ok, nil
	}

	rw := cache.Global()
	if rw == nil {
		// Cache=on flagged but watcher not wired — defensive
		// degrade-to-deny. Without the informer we cannot honour the
		// "zero SubjectAccessReview in cache=on" rule, and we MUST NOT
		// silently fall back to apiserver (would violate Revision 1).
		log.Warn("rbac.evaluate: cache enabled but Global() is nil — denying",
			slog.String("user", opts.Username),
			slog.String("verb", opts.Verb),
			slog.String("group", opts.Group),
			slog.String("resource", opts.Resource),
			slog.String("namespace", opts.Namespace),
		)
		return false, fmt.Errorf("rbac: cache=on but ResourceWatcher not wired")
	}

	// Ship B (0.30.138, AC-B.3) — SINGLE rbacSnap.Load() at the top of
	// EvaluateRBAC. The resulting *cache.RBACSnapshot is threaded as an
	// explicit parameter into evaluateAgainstInformer AND roleRefPermits,
	// so every read inside one EvaluateRBAC call observes the SAME
	// snapshot version. A Load() per site would let two reads in the
	// same eval see two different snapshots (e.g. a CRB in the new
	// snapshot but its referenced ClusterRole only in the old) —
	// correctness regression. The architect-review gate rejects on this
	// alone.
	snap := rw.Snapshot()
	if snap == nil {
		// AC-B.8 — degrade-to-deny pre-readiness gate. Cache=on
		// activated but the 4 RBAC syncCh have not all closed AND the
		// initial rebuildRBACSnapshot has not yet published. Fail
		// closed; never fall through to UserCan (would violate
		// Revision 1).
		log.Warn("rbac.evaluate: typed-RBAC snapshot not yet published — denying",
			slog.String("user", opts.Username),
			slog.String("verb", opts.Verb),
			slog.String("group", opts.Group),
			slog.String("resource", opts.Resource),
			slog.String("namespace", opts.Namespace),
		)
		return false, fmt.Errorf("rbac: snapshot not yet built")
	}

	allowed, err := evaluateAgainstInformer(ctx, snap, opts)
	if err != nil {
		log.Error("rbac.evaluate: informer evaluation failed",
			slog.String("user", opts.Username), slog.Any("err", err))
		return false, err
	}

	log.Debug("rbac.evaluate",
		slog.String("path", "in-process"),
		slog.String("user", opts.Username),
		slog.String("verb", opts.Verb),
		slog.String("group", opts.Group),
		slog.String("resource", opts.Resource),
		slog.String("namespace", opts.Namespace),
		slog.Bool("allowed", allowed),
	)
	return allowed, nil
}

// evaluateAgainstInformer walks every ClusterRoleBinding and (when
// namespace is non-empty) RoleBinding in opts.Namespace looking for a
// Subject that matches opts.Username / opts.Groups / "system:authenticated".
// For every match the bound Role / ClusterRole is resolved and its
// rules walked. First permitting rule wins (RBAC semantics).
//
// Ship B (0.30.138) — reads typed *rbacv1.{ClusterRole,Role}Binding
// from a pre-built `*cache.RBACSnapshot` passed in by EvaluateRBAC. No
// per-call ListTypedObjects / GetTypedObject. AC-B.3: the snap pointer
// is captured ONCE at the top of EvaluateRBAC and threaded through
// every sub-read so one eval observes one coherent snapshot version.
//
// The pre-Ship-B defensive Unstructured fallback (`as{Kind}` helpers in
// this file) is no longer reachable from this hot path — the snapshot
// writer skips non-typed indexer entries upstream with its own WARN
// (mirroring the 0.30.6 fallback=true invariant). The as{Kind} helpers
// stay in this file for documentation and for any future caller that
// reads the indexer outside the snapshot path.
//
// 0.30.6's subject-prefilter ordering is preserved (subject match
// BEFORE the rule walk) so a no-subject CRB still costs only the
// subject scan, not the expensive PolicyRule walk.
func evaluateAgainstInformer(ctx context.Context, snap *cache.RBACSnapshot, opts EvaluateOptions) (bool, error) {
	log := xcontext.Logger(ctx)

	// 1) ClusterRoleBindings — apply cluster-wide. Cluster-wide
	//    permits override namespace scope. (Pre-Ship-B: evaluate.go:198
	//    ListTypedObjects(clusterRoleBindingsGVR, ""). Ship B: snapshot
	//    field read — no slice allocation per call.)
	for _, crb := range snap.ClusterRoleBindings {
		// Subject prefilter FIRST — skip the entire roleRefPermits
		// walk when no subject matches. Cheaper than the rule-walk.
		if !anySubjectMatches(crb.Subjects, opts) {
			continue
		}
		permits, err := roleRefPermits(snap, "", crb.RoleRef, opts, log)
		if err != nil {
			return false, err
		}
		if permits {
			return true, nil
		}
	}

	// 2) RoleBindings in opts.Namespace — only when namespace is set.
	//    A RoleBinding's permit is scoped to its own namespace; the
	//    RoleRef can point at a Role (same namespace) or a ClusterRole
	//    (cluster-wide) but the binding's effect is the namespace.
	//    (Pre-Ship-B: evaluate.go:223 ListTypedObjects(roleBindingsGVR,
	//    opts.Namespace). Ship B: snapshot map lookup.)
	if opts.Namespace != "" {
		for _, rb := range snap.RoleBindingsByNS[opts.Namespace] {
			if !anySubjectMatches(rb.Subjects, opts) {
				continue
			}
			permits, err := roleRefPermits(snap, opts.Namespace, rb.RoleRef, opts, log)
			if err != nil {
				return false, err
			}
			if permits {
				return true, nil
			}
		}
	}

	return false, nil
}

// roleRefPermits resolves ref (Role or ClusterRole) against the
// passed-in snapshot and walks its rules. namespace is the
// RoleBinding's namespace (used to resolve kind=Role); empty when ref
// came from a ClusterRoleBinding.
//
// Ship B (0.30.138) — map lookups on the snapshot replace the
// per-call GetTypedObject reads (pre-Ship-B: evaluate.go:254/:270). A
// missed lookup (`!ok`) is recorded via cache.RecordRBACSnapshotMiss
// (AC-B.10) and treated as a deny — same fail-closed posture as
// today's GetTypedObject !ok.
func roleRefPermits(snap *cache.RBACSnapshot, namespace string, ref rbacv1.RoleRef, opts EvaluateOptions, log *slog.Logger) (bool, error) {
	_ = log // reserved for future per-ref debug logging; kept in signature for parity
	switch ref.Kind {
	case "ClusterRole":
		cr, ok := snap.ClusterRolesByName[ref.Name]
		if !ok {
			cache.RecordRBACSnapshotMiss("ClusterRole", "", ref.Name)
			return false, nil
		}
		return rulesPermit(cr.Rules, opts), nil

	case "Role":
		if namespace == "" {
			// kind=Role in a ClusterRoleBinding is invalid per
			// Kubernetes — treat as deny.
			return false, nil
		}
		r, ok := snap.RolesByNSName[namespace+"/"+ref.Name]
		if !ok {
			cache.RecordRBACSnapshotMiss("Role", namespace, ref.Name)
			return false, nil
		}
		return rulesPermit(r.Rules, opts), nil

	default:
		return false, nil
	}
}

// rulesPermit returns true iff any PolicyRule in rules permits opts.
// Wildcard semantics match Kubernetes: "*" in Verbs, Resources or
// APIGroups matches everything.
//
// 0.30.109 (G1) — rule.ResourceNames is now honoured. A rule scoped to
// specific named objects (resourceNames: ["foo"]) must NOT be treated
// as granting every object of that GVR. resourceNameMatches implements
// the Kubernetes `ResourceNameMatches` semantics — see its doc comment.
// Before 0.30.109 this check was absent: a resourceNames-scoped rule
// over-exposed every object (cross-user leak in filterListByRBAC).
func rulesPermit(rules []rbacv1.PolicyRule, opts EvaluateOptions) bool {
	for _, rule := range rules {
		if !stringSliceMatches(rule.Verbs, opts.Verb) {
			continue
		}
		if !stringSliceMatches(rule.APIGroups, opts.Group) {
			continue
		}
		if !stringSliceMatches(rule.Resources, opts.Resource) {
			continue
		}
		if !resourceNameMatches(rule, opts) {
			continue
		}
		return true
	}
	return false
}

// nameSpecificVerbs is the set of RBAC verbs that act on a single named
// object — the only verbs for which a resourceNames-scoped rule can
// grant access. Mirrors the Kubernetes RBAC authorizer: the collection
// verbs ("list", "watch", "create", "deletecollection") have no single
// named object, so a rule with a non-empty ResourceNames must never
// match them. (rbac/v1's authorizer scopes resourceNames to exactly
// these verbs; "get"/"update"/"patch"/"delete" — and the "*" wildcard,
// handled separately — are name-specific.)
var nameSpecificVerbs = map[string]struct{}{
	"get":    {},
	"update": {},
	"patch":  {},
	"delete": {},
}

// resourceNameMatches implements Kubernetes `ResourceNameMatches`
// semantics for a single PolicyRule (0.30.109, G1):
//
//   - rule.ResourceNames empty  → matches all objects (unchanged
//     behaviour for unscoped rules).
//   - rule.ResourceNames non-empty → the rule is scoped to specific
//     named objects:
//   - It can only ever match a name-specific verb
//     ("get"/"update"/"patch"/"delete", or the verb wildcard "*").
//     For a collection verb ("list"/"watch"/"create"/
//     "deletecollection") it does NOT match — a resourceNames-scoped
//     rule never grants `list`.
//   - The request's object name (opts.Name) must appear in
//     rule.ResourceNames.
//
// opts.Verb is already known to satisfy the rule's Verbs list by the
// time this is called (rulesPermit checks Verbs first). We re-derive
// the name-specific predicate from opts.Verb itself rather than the
// rule's Verbs so that a wildcard-verb rule with resourceNames is still
// correctly denied for a collection-verb request.
func resourceNameMatches(rule rbacv1.PolicyRule, opts EvaluateOptions) bool {
	if len(rule.ResourceNames) == 0 {
		return true
	}
	// Non-empty ResourceNames: only name-specific verbs can match.
	if _, nameSpecific := nameSpecificVerbs[opts.Verb]; !nameSpecific {
		return false
	}
	// The targeted object's name must be in the list.
	for _, n := range rule.ResourceNames {
		if n == opts.Name {
			return true
		}
	}
	return false
}

// stringSliceMatches implements the RBAC wildcard rule: "*" matches
// every value; otherwise an exact match is required.
func stringSliceMatches(allowed []string, want string) bool {
	for _, a := range allowed {
		if a == "*" || a == want {
			return true
		}
	}
	return false
}

// anySubjectMatches returns true iff opts.Username, any of opts.Groups
// (as Kind="Group") or the system-authenticated group appears in subjects.
// ServiceAccount subjects are matched when opts.Username has the
// canonical "system:serviceaccount:<ns>:<name>" form.
//
// 0.30.109 (G3/G6) — ServiceAccount synthetic groups. Kubernetes
// implicitly places every ServiceAccount in two synthetic groups:
//   - "system:serviceaccounts"            (all ServiceAccounts)
//   - "system:serviceaccounts:<ns>"       (all SAs in the SA's namespace)
//
// A binding granting a Group subject of either name must therefore
// match a request whose Username is a canonical ServiceAccount in the
// matching namespace. effectiveGroups (below) computes the augmented
// group set; anySubjectMatches consults it for every Group subject.
// This mirrors k8s.io/apiserver's serviceaccount.UserInfo.
func anySubjectMatches(subjects []rbacv1.Subject, opts EvaluateOptions) bool {
	saNS, saName, isSA := parseServiceAccountUsername(opts.Username)
	groups := effectiveGroups(opts, isSA, saNS)

	for _, s := range subjects {
		switch s.Kind {
		case rbacv1.UserKind:
			if s.Name == opts.Username {
				return true
			}
		case rbacv1.GroupKind:
			for _, g := range groups {
				if s.Name == g {
					return true
				}
			}
			// Every authenticated request gains the
			// system:authenticated group implicitly (Kubernetes
			// auth chain).
			if s.Name == "system:authenticated" && opts.Username != "" {
				return true
			}
		case rbacv1.ServiceAccountKind:
			if isSA && s.Namespace == saNS && s.Name == saName {
				return true
			}
		}
	}
	return false
}

// well-known ServiceAccount synthetic group names (0.30.109, G3/G6).
// Mirrors k8s.io/apiserver/pkg/authentication/serviceaccount.
const (
	allServiceAccountsGroup     = "system:serviceaccounts"
	serviceAccountsNamespacePfx = "system:serviceaccounts:"
)

// effectiveGroups returns the group set used for Group-subject matching.
// It is opts.Groups plus, when the request identity is a canonical
// ServiceAccount, the two synthetic ServiceAccount groups Kubernetes
// adds implicitly:
//
//	system:serviceaccounts
//	system:serviceaccounts:<the-SA's-namespace>
//
// For a non-ServiceAccount identity the synthetic groups are not added
// and the function returns opts.Groups unchanged (no allocation when
// there is nothing to add).
func effectiveGroups(opts EvaluateOptions, isSA bool, saNS string) []string {
	if !isSA {
		return opts.Groups
	}
	groups := make([]string, 0, len(opts.Groups)+2)
	groups = append(groups, opts.Groups...)
	groups = append(groups,
		allServiceAccountsGroup,
		serviceAccountsNamespacePfx+saNS,
	)
	return groups
}

// parseServiceAccountUsername decodes the
// "system:serviceaccount:<ns>:<name>" form. Returns (ns, name, true) on
// success; ("", "", false) for non-ServiceAccount usernames.
func parseServiceAccountUsername(u string) (string, string, bool) {
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(u, prefix) {
		return "", "", false
	}
	rest := u[len(prefix):]
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// asClusterRoleBinding extracts a *rbacv1.ClusterRoleBinding from an
// indexer object. Happy path: the indexer already holds a typed
// pointer (cache/strip.go stripAndTypeClusterRoleBinding ran at the
// Add/Update event), the type assertion succeeds, and per-call
// FromUnstructured cost is zero. This is the 0.30.6 headline win.
//
// Defensive fallback path: the indexer entry is still
// *unstructured.Unstructured (e.g. transform missed it, test seeded
// without the transform pipeline, or a future code regression). In
// that case we convert once with the existing toClusterRoleBinding
// helper and log WARN with fallback=true so the regression is loud
// (plan §"Code-path falsifier"). Allow/deny result is bit-exact equal
// either way — the test suite asserts equivalence.
//
// Returns error only when the indexer object is neither typed nor
// convertible (the fallback FromUnstructured failed).
func asClusterRoleBinding(obj interface{}, log *slog.Logger) (*rbacv1.ClusterRoleBinding, error) {
	if crb, ok := obj.(*rbacv1.ClusterRoleBinding); ok {
		return crb, nil
	}
	uns, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("rbac.asClusterRoleBinding: indexer object is neither *rbacv1.ClusterRoleBinding nor *unstructured.Unstructured (%T)", obj)
	}
	log.Warn("rbac.indexer.read fallback=true",
		slog.String("subsystem", "rbac"),
		slog.String("kind", "ClusterRoleBinding"),
		slog.String("name", uns.GetName()),
		slog.String("hint", "indexer entry was Unstructured — typed transform did not fire on this object"),
	)
	return toClusterRoleBinding(uns)
}

// asRoleBinding is the RoleBinding analogue of asClusterRoleBinding.
func asRoleBinding(obj interface{}, log *slog.Logger) (*rbacv1.RoleBinding, error) {
	if rb, ok := obj.(*rbacv1.RoleBinding); ok {
		return rb, nil
	}
	uns, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("rbac.asRoleBinding: indexer object is neither *rbacv1.RoleBinding nor *unstructured.Unstructured (%T)", obj)
	}
	log.Warn("rbac.indexer.read fallback=true",
		slog.String("subsystem", "rbac"),
		slog.String("kind", "RoleBinding"),
		slog.String("name", uns.GetName()),
		slog.String("namespace", uns.GetNamespace()),
		slog.String("hint", "indexer entry was Unstructured — typed transform did not fire on this object"),
	)
	return toRoleBinding(uns)
}

// asClusterRole is the ClusterRole analogue of asClusterRoleBinding.
func asClusterRole(obj interface{}, log *slog.Logger) (*rbacv1.ClusterRole, error) {
	if cr, ok := obj.(*rbacv1.ClusterRole); ok {
		return cr, nil
	}
	uns, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("rbac.asClusterRole: indexer object is neither *rbacv1.ClusterRole nor *unstructured.Unstructured (%T)", obj)
	}
	log.Warn("rbac.indexer.read fallback=true",
		slog.String("subsystem", "rbac"),
		slog.String("kind", "ClusterRole"),
		slog.String("name", uns.GetName()),
		slog.String("hint", "indexer entry was Unstructured — typed transform did not fire on this object"),
	)
	return toClusterRole(uns)
}

// asRole is the Role analogue of asClusterRoleBinding.
func asRole(obj interface{}, log *slog.Logger) (*rbacv1.Role, error) {
	if r, ok := obj.(*rbacv1.Role); ok {
		return r, nil
	}
	uns, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("rbac.asRole: indexer object is neither *rbacv1.Role nor *unstructured.Unstructured (%T)", obj)
	}
	log.Warn("rbac.indexer.read fallback=true",
		slog.String("subsystem", "rbac"),
		slog.String("kind", "Role"),
		slog.String("name", uns.GetName()),
		slog.String("namespace", uns.GetNamespace()),
		slog.String("hint", "indexer entry was Unstructured — typed transform did not fire on this object"),
	)
	return toRole(uns)
}

// to{Kind} helpers (below) remain for the defensive Unstructured
// fallback path only. The cache=on happy path uses the as{Kind}
// helpers above with zero per-call conversion.

func toRoleBinding(uns *unstructured.Unstructured) (*rbacv1.RoleBinding, error) {
	out := &rbacv1.RoleBinding{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(uns.Object, out); err != nil {
		return nil, fmt.Errorf("rbac: convert RoleBinding %s/%s: %w", uns.GetNamespace(), uns.GetName(), err)
	}
	return out, nil
}

func toClusterRoleBinding(uns *unstructured.Unstructured) (*rbacv1.ClusterRoleBinding, error) {
	out := &rbacv1.ClusterRoleBinding{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(uns.Object, out); err != nil {
		return nil, fmt.Errorf("rbac: convert ClusterRoleBinding %s: %w", uns.GetName(), err)
	}
	return out, nil
}

func toRole(uns *unstructured.Unstructured) (*rbacv1.Role, error) {
	out := &rbacv1.Role{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(uns.Object, out); err != nil {
		return nil, fmt.Errorf("rbac: convert Role %s/%s: %w", uns.GetNamespace(), uns.GetName(), err)
	}
	return out, nil
}

func toClusterRole(uns *unstructured.Unstructured) (*rbacv1.ClusterRole, error) {
	out := &rbacv1.ClusterRole{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(uns.Object, out); err != nil {
		return nil, fmt.Errorf("rbac: convert ClusterRole %s: %w", uns.GetName(), err)
	}
	return out, nil
}
