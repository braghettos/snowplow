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

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/snowplow/internal/cache"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

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
}

// well-known RBAC GVRs — must match cache.RBACResourceTypes.
var (
	rolesGVR = schema.GroupVersionResource{
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles",
	}
	roleBindingsGVR = schema.GroupVersionResource{
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings",
	}
	clusterRolesGVR = schema.GroupVersionResource{
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles",
	}
	clusterRoleBindingsGVR = schema.GroupVersionResource{
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings",
	}
)

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

	allowed, err := evaluateAgainstInformer(ctx, rw, opts)
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
// 0.30.6 — reads typed *rbacv1.{ClusterRole,Role}Binding directly from
// the indexer (typed transform fires once at Add/Update event time per
// strip.go stripAndType*). Per-call FromUnstructured cost is zero.
// Defensive Unstructured fallback is retained for safety: if for any
// reason an indexer entry is still an Unstructured (e.g. test seeding
// path, transform conversion failure logged WARN at write time), the
// per-call conversion runs and logs WARN at read time so the regression
// is loud (plan §"Code-path falsifier" — fallback=true rate MUST stay
// below 1%).
//
// 0.30.6 also reorders subject-prefilter to run BEFORE the typed read
// in the inner loop where the typed object is already in hand. The
// outer ListTypedObjects walk picks subjects from the typed object
// directly; the prefilter still short-circuits the rule-walk (the
// expensive part) when no subject matches.
func evaluateAgainstInformer(ctx context.Context, rw *cache.ResourceWatcher, opts EvaluateOptions) (bool, error) {
	log := xcontext.Logger(ctx)

	// 1) ClusterRoleBindings — apply cluster-wide. Cluster-wide
	//    permits override namespace scope.
	crbs := rw.ListTypedObjects(clusterRoleBindingsGVR, "")
	for _, obj := range crbs {
		crb, err := asClusterRoleBinding(obj, log)
		if err != nil {
			return false, err
		}
		// Subject prefilter FIRST — skip the entire roleRefPermits
		// walk when no subject matches. Cheaper than the rule-walk.
		if !anySubjectMatches(crb.Subjects, opts) {
			continue
		}
		permits, err := roleRefPermits(rw, "", crb.RoleRef, opts, log)
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
	if opts.Namespace != "" {
		rbs := rw.ListTypedObjects(roleBindingsGVR, opts.Namespace)
		for _, obj := range rbs {
			rb, err := asRoleBinding(obj, log)
			if err != nil {
				return false, err
			}
			if !anySubjectMatches(rb.Subjects, opts) {
				continue
			}
			permits, err := roleRefPermits(rw, opts.Namespace, rb.RoleRef, opts, log)
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

// roleRefPermits resolves ref (Role or ClusterRole) and walks its
// rules. namespace is the RoleBinding's namespace (used to resolve
// kind=Role); empty when ref came from a ClusterRoleBinding.
//
// 0.30.6 — reads typed *rbacv1.{ClusterRole,Role} directly via
// GetTypedObject; defensive Unstructured fallback logs WARN.
func roleRefPermits(rw *cache.ResourceWatcher, namespace string, ref rbacv1.RoleRef, opts EvaluateOptions, log *slog.Logger) (bool, error) {
	switch ref.Kind {
	case "ClusterRole":
		obj, ok := rw.GetTypedObject(clusterRolesGVR, "", ref.Name)
		if !ok {
			return false, nil
		}
		cr, err := asClusterRole(obj, log)
		if err != nil {
			return false, err
		}
		return rulesPermit(cr.Rules, opts), nil

	case "Role":
		if namespace == "" {
			// kind=Role in a ClusterRoleBinding is invalid per
			// Kubernetes — treat as deny.
			return false, nil
		}
		obj, ok := rw.GetTypedObject(rolesGVR, namespace, ref.Name)
		if !ok {
			return false, nil
		}
		r, err := asRole(obj, log)
		if err != nil {
			return false, err
		}
		return rulesPermit(r.Rules, opts), nil

	default:
		return false, nil
	}
}

// rulesPermit returns true iff any PolicyRule in rules permits opts.
// Wildcard semantics match Kubernetes: "*" in Verbs, Resources or
// APIGroups matches everything.
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
		return true
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
func anySubjectMatches(subjects []rbacv1.Subject, opts EvaluateOptions) bool {
	saNS, saName, isSA := parseServiceAccountUsername(opts.Username)

	for _, s := range subjects {
		switch s.Kind {
		case rbacv1.UserKind:
			if s.Name == opts.Username {
				return true
			}
		case rbacv1.GroupKind:
			for _, g := range opts.Groups {
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
