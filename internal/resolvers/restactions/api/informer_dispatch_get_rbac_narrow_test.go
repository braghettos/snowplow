// informer_dispatch_get_rbac_narrow_test.go — Tag 0.30.101 falsifier.
//
// FALSIFIER (HARD GATE per feedback_falsifier_first_before_ship.md):
// the resolver pivot over-exposes objects on the GET-by-name path — the
// GET-verb sibling of the 0.30.99 "Finding 1" LIST over-exposure that
// Tag A / 0.30.100 fixed. dispatchViaInformer's GET-by-name branch
// returned the raw informer object (rw.GetObject → json.Marshal) with
// ZERO RBAC check: a narrow-RBAC user GETting a known object name in a
// namespace they have no `get` grant for would receive the object.
//
// THE FIX (filterGetByRBAC, mirrors Tag A's filterListByRBAC): the
// informer-served GET runs through a `get`-verb EvaluateRBAC check
// against the in-process typed-RBAC indexer. Denied / no-identity /
// evaluator-error → NOT served → apiserver fallthrough (the apiserver
// applies the per-user token, which correctly 403s).
//
// These tests assert PER-USER RBAC equivalence on the pivot-served
// GET-by-name branch:
//
//   - TestDispatchViaInformer_RBACNarrowing_GetDeniedNamespace
//     A narrow-RBAC user GETting an object in a DENIED namespace gets
//     a fallthrough (served=false). An object in an AUTHORIZED
//     namespace IS served. NEGATIVE CONTROL via -tags rbac_narrow_baseline:
//     against pre-fix code the denied GET is served (over-exposure
//     reproduced).
//
//   - TestDispatchViaInformer_RBACFailClosed_GetNilIdentity
//     A pivot-served GET with no UserInfo on the context falls through —
//     never serves the raw informer object.
//
// Build modes mirror the Tag A LIST falsifier (rbac_narrow_flag.go):
//   - default: assertions expect the denied GET to fall through.
//   - `-tags rbac_narrow_baseline`: flips assertions to expect the
//     denied GET served — the negative control. Captured at the
//     preflight gate.
//
// Constraint: in-memory dynamic-fake watcher only — NEVER the kind
// cluster / remote kubeconfig.

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// getterClusterRole grants `get` (not `list`) on the dispatch test
// GVR's resource — the verb the GET-by-name pivot branch evaluates.
func getterClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "restaction-getter"},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{dispatchTestGVR.Group},
				Resources: []string{dispatchTestGVR.Resource},
				Verbs:     []string{"get"},
			},
		},
	}
}

// getNarrowingRoleBinding builds a RoleBinding in ns granting narrowUser
// `get` on the dispatch test GVR (via restaction-getter).
func getNarrowingRoleBinding(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "get-narrow-binding"},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.UserKind, APIGroup: "rbac.authorization.k8s.io", Name: narrowUser},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "restaction-getter",
		},
	}
}

// newRBACNarrowGetWatcher constructs a synced cache=on watcher seeded
// with restactions across BOTH authorized and denied namespaces, plus a
// ClusterRole + per-authorized-namespace RoleBindings granting narrowUser
// the `get` verb in authorizedNamespaces only. The GET-verb analogue of
// newRBACNarrowWatcher (which grants `list`).
func newRBACNarrowGetWatcher(t *testing.T) *cache.ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")

	var seed []runtime.Object
	for _, ns := range authorizedNamespaces {
		for _, suffix := range []string{"x", "y"} {
			seed = append(seed, newTestRestActionRuntimeObject(ns, ns+"-"+suffix, "marker"))
		}
	}
	for _, ns := range deniedNamespaces {
		for _, suffix := range []string{"x", "y"} {
			seed = append(seed, newTestRestActionRuntimeObject(ns, ns+"-"+suffix, "marker"))
		}
	}

	seed = append(seed, getterClusterRole())
	for _, ns := range authorizedNamespaces {
		seed = append(seed, getNarrowingRoleBinding(ns))
	}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		dispatchTestScheme(), dispatchTestListKinds(), seed...)

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher")
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})

	added, syncCh := rw.EnsureResourceType(dispatchTestGVR)
	if !added {
		t.Fatalf("EnsureResourceType: want added=true; got false")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("EnsureResourceType: dispatch informer did not sync within 5s")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(ctx, 5*time.Second); err != nil {
		t.Fatalf("WaitForCacheSync (RBAC informers): %v", err)
	}

	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })
	return rw
}

// getByNamePath builds the apiserver GET-by-name path for the dispatch
// test GVR: /apis/<group>/<version>/namespaces/<ns>/restactions/<name>.
func getByNamePath(ns, name string) string {
	return "/apis/templates.krateo.io/v1/namespaces/" + ns + "/restactions/" + name
}

// TestDispatchViaInformer_RBACNarrowing_GetDeniedNamespace is the
// headline GET-path falsifier. A narrow-RBAC user GETting an object in
// an AUTHORIZED namespace IS served from the informer; the SAME user
// GETting an object in a DENIED namespace falls through (served=false).
//
// NEGATIVE CONTROL: built with `-tags rbac_narrow_baseline` the denied
// GET is expected to be SERVED — proving the test reproduces the
// over-exposure against pre-fix code. Captured at the preflight gate.
func TestDispatchViaInformer_RBACNarrowing_GetDeniedNamespace(t *testing.T) {
	newRBACNarrowGetWatcher(t)
	ctx := ctxWithUser(narrowUser)

	// AUTHORIZED namespace — the narrow user has a `get` grant; the
	// pivot serves the object. (Both pre- and post-fix.)
	raw, served := dispatchViaInformer(ctx, buildCall(http.MethodGet, getByNamePath("team-a", "team-a-x")))
	if !served {
		t.Fatalf("authorized GET: expected served=true; got served=false")
	}
	if len(raw) == 0 {
		t.Fatalf("authorized GET: served=true but empty bytes")
	}

	// DENIED namespace — the narrow user has NO `get` grant.
	raw, served = dispatchViaInformer(ctx, buildCall(http.MethodGet, getByNamePath("bench-ns-1", "bench-ns-1-x")))

	if rbacNarrowBaseline {
		// NEGATIVE CONTROL (pre-fix behaviour). The pivot GET branch
		// returns the raw object with no RBAC check.
		if !served {
			t.Fatalf("BASELINE negative control: expected over-exposure (denied GET served); got served=false — if this fails the bug is already fixed; rerun without -tags rbac_narrow_baseline")
		}
		if len(raw) == 0 {
			t.Fatalf("BASELINE negative control: served=true but empty bytes")
		}
		t.Logf("BASELINE negative control reproduced over-exposure: narrow user %q was served object %q in denied namespace %q",
			narrowUser, "bench-ns-1-x", "bench-ns-1")
		return
	}

	// Tag-0.30.101 behaviour: the denied GET is NOT served — it falls
	// through to the apiserver (whose per-user token correctly 403s).
	if served {
		t.Fatalf("denied GET: LEAK — pivot served object %q in namespace %q to user %q who has no `get` grant",
			"bench-ns-1-x", "bench-ns-1", narrowUser)
	}
	if raw != nil {
		t.Fatalf("denied GET: fallthrough must return nil raw; got %d bytes", len(raw))
	}
}

// TestDispatchViaInformer_RBACFailClosed_GetNilIdentity asserts the
// fail-closed contract: a pivot-served GET with no UserInfo on the
// context MUST NOT serve the raw informer object — it falls through to
// the apiserver (whose per-user token gate is the authoritative answer).
func TestDispatchViaInformer_RBACFailClosed_GetNilIdentity(t *testing.T) {
	newRBACNarrowGetWatcher(t)

	// No UserInfo attached — context.Background() only. Use an object
	// in an AUTHORIZED namespace so the ONLY thing preventing a serve
	// is the missing identity.
	raw, served := dispatchViaInformer(context.Background(),
		buildCall(http.MethodGet, getByNamePath("team-a", "team-a-x")))

	if rbacNarrowBaseline {
		// Pre-fix: the pivot GET branch never reads identity → serves.
		if !served {
			t.Fatalf("BASELINE negative control: expected served=true (no identity check)")
		}
		if len(raw) == 0 {
			t.Fatalf("BASELINE negative control: served=true but empty bytes")
		}
		return
	}

	// Tag-0.30.101 fail-closed: no identity → not served → apiserver
	// fallthrough. NEVER the raw informer object.
	if served {
		t.Fatalf("fail-closed: nil identity was served the raw informer object — MUST fall through")
	}
	if raw != nil {
		t.Fatalf("fail-closed: fallthrough must return nil raw; got %d bytes", len(raw))
	}
}

// TestDispatchViaInformer_RBACNarrowing_GetGroupGrant confirms a group
// subject also narrows the GET path — a user whose only `get` grant is
// via a Group subject is served the authorized object.
func TestDispatchViaInformer_RBACNarrowing_GetGroupGrant(t *testing.T) {
	if rbacNarrowBaseline {
		t.Skip("group-grant assertion is Tag-0.30.101 only")
	}
	t.Setenv("CACHE_ENABLED", "true")

	var seed []runtime.Object
	seed = append(seed, newTestRestActionRuntimeObject("team-a", "team-a-x", "m"))
	seed = append(seed, getterClusterRole())
	grpBinding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "get-grp-binding"},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.GroupKind, APIGroup: "rbac.authorization.k8s.io", Name: "devs"},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "restaction-getter"},
	}
	seed = append(seed, grpBinding)

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		dispatchTestScheme(), dispatchTestListKinds(), seed...)
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
	added, syncCh := rw.EnsureResourceType(dispatchTestGVR)
	if !added {
		t.Fatalf("EnsureResourceType: want added=true")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("dispatch informer did not sync")
	}
	ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(ctx2, 5*time.Second); err != nil {
		t.Fatalf("WaitForCacheSync: %v", err)
	}
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	// User "alice" is in group "devs".
	raw, served := dispatchViaInformer(ctxWithUser("alice", "devs"),
		buildCall(http.MethodGet, getByNamePath("team-a", "team-a-x")))
	if !served {
		t.Fatalf("group-grant GET: expected served=true")
	}
	if len(raw) == 0 {
		t.Fatalf("group-grant GET: served=true but empty bytes")
	}
}
