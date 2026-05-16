// informer_serve_rbac_narrow_test.go — Tag 0.30.101 falsifier.
//
// FALSIFIER (HARD GATE per feedback_falsifier_first_before_ship.md):
// the resolver pivot over-exposes objects on the GET-by-name path — the
// same RBAC over-exposure class as 0.30.99 "Finding 1" (the LIST path,
// fixed by Tag A / 0.30.100). objects.Get's informer-served branch
// returns a raw informer object with ZERO RBAC check: a narrow-RBAC
// user GETting a known object name in a namespace they have no `get`
// grant for would receive the object.
//
// THE FIX (filterGetByRBAC, mirrors Tag A's filterListByRBAC): the
// informer-served GET runs through a `get`-verb EvaluateRBAC check
// against the in-process typed-RBAC indexer. Denied / no-identity /
// evaluator-error → NOT served → apiserver fallthrough (the apiserver
// applies the per-user token, which correctly 403s).
//
// This test asserts PER-USER RBAC equivalence on the objects.Get
// informer-served GET branch:
//
//   - TestGet_RBACNarrowing_DeniedNamespaceNotServed
//     A narrow-RBAC user GETting an object in a DENIED namespace gets
//     a fallthrough (informer does NOT serve). An object in an
//     AUTHORIZED namespace IS served from the informer.
//     NEGATIVE CONTROL: against pre-fix code the denied GET is served
//     (over-exposure reproduced). See the rbacNarrowBaseline branch.
//
//   - TestGet_RBACFailClosed_NilIdentity
//     An informer-served GET with no UserInfo on the context falls
//     through — never serves the raw informer object.
//
// Build modes:
//   - default (Tag-0.30.101 code present): assertions expect the
//     denied GET to fall through.
//   - `-tags rbac_narrow_baseline`: flips the assertion to expect the
//     denied GET to be SERVED, proving the test catches the bug when
//     run against pre-fix behaviour. Captured at the preflight gate.
//
// Constraint: uses the in-memory dynamic-fake watcher (newServeWatcher
// pattern) — NEVER the kind cluster / remote kubeconfig. The watcher's
// RBAC informers are eagerly registered by NewResourceWatcher, so RBAC
// objects seeded into the same dynamic fake reach EvaluateRBAC.

package objects

import (
	"context"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// narrowUser is the narrow-RBAC principal: authorized to `get`
// restactions in only the authorized namespace.
const narrowUser = "cyberjoker"

// authorizedNS is the namespace narrowUser may `get` objects in.
const authorizedNS = "team-a"

// deniedNS is a seeded namespace narrowUser has NO RBAC grant for —
// the pre-fix pivot leaked objects here.
const deniedNS = "bench-ns-1"

// ctxWithUser returns a context carrying UserInfo for username + groups.
// Mirrors the request-pipeline shape (handlers wire UserInfo via the
// auth middleware; tests wire it here directly).
func ctxWithUser(username string, groups ...string) context.Context {
	return xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: username,
			Groups:   groups,
		}),
	)
}

// getterClusterRole grants `get` on the serve test GVR's resource.
// Referenced (namespace-scoped) by the narrowing RoleBinding.
func getterClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "restaction-getter"},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{serveTestGVR.Group},
				Resources: []string{serveTestGVR.Resource},
				Verbs:     []string{"get"},
			},
		},
	}
}

// narrowingRoleBinding builds a RoleBinding in ns granting narrowUser
// `get` on the serve test GVR's resource (via the getter ClusterRole).
func narrowingRoleBinding(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "narrow-binding"},
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

// newRBACNarrowServeWatcher constructs a synced cache=on watcher seeded
// with restactions in BOTH the authorized and a denied namespace, plus
// a ClusterRole + a RoleBinding (in authorizedNS only) narrowing
// narrowUser to `get` in authorizedNS. Mirrors the api package's
// newRBACNarrowWatcher.
func newRBACNarrowServeWatcher(t *testing.T) *cache.ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")

	var seed []runtime.Object
	// One object in the authorized namespace, one in the denied one.
	seed = append(seed, newServeTestObject(authorizedNS, "obj-authorized", "marker-a"))
	seed = append(seed, newServeTestObject(deniedNS, "obj-denied", "marker-d"))

	// RBAC objects — the watcher eagerly registers RBAC informers, so
	// these reach EvaluateRBAC through the same dynamic fake.
	seed = append(seed, getterClusterRole())
	seed = append(seed, narrowingRoleBinding(authorizedNS))

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		serveTestScheme(), serveTestListKinds(), seed...)

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

	added, syncCh := rw.EnsureResourceType(serveTestGVR)
	if !added {
		t.Fatalf("EnsureResourceType: want added=true; got false")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("EnsureResourceType: serve informer did not sync within 5s")
	}

	// Wait for the eagerly-registered RBAC informers to sync — without
	// this EvaluateRBAC sees an empty RBAC store and denies everything.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(ctx, 5*time.Second); err != nil {
		t.Fatalf("WaitForCacheSync (RBAC informers): %v", err)
	}

	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })
	return rw
}

// TestGet_RBACNarrowing_DeniedNamespaceNotServed is the headline
// falsifier. A narrow-RBAC user GETting an object in an AUTHORIZED
// namespace IS served from the informer; the SAME user GETting an
// object in a DENIED namespace falls through to the apiserver — the
// informer must NOT serve an object the user has no `get` grant for.
//
// NEGATIVE CONTROL: built with `-tags rbac_narrow_baseline` the denied
// GET is expected to be SERVED instead — proving that against pre-fix
// code the test reproduces the over-exposure. Captured at the preflight
// gate.
func TestGet_RBACNarrowing_DeniedNamespaceNotServed(t *testing.T) {
	resetServeCounters()
	t.Setenv("RESOLVER_USE_INFORMER", "true")
	newRBACNarrowServeWatcher(t)

	ctx := ctxWithUser(narrowUser)

	// AUTHORIZED namespace — the narrow user has a `get` grant; the
	// informer serves the object. (Both pre- and post-fix: an
	// authorized GET is always served.)
	res := Get(ctx, serveTestRef(authorizedNS, "obj-authorized"))
	if res.Unstructured == nil {
		t.Fatalf("authorized GET: expected informer-served object; got nil (Err=%v)", res.Err)
	}
	if s := ObjectsGetStatsSnapshot(); s.InformerServed != 1 {
		t.Fatalf("authorized GET: InformerServed want 1; got %d", s.InformerServed)
	}

	resetServeCounters()

	// DENIED namespace — the narrow user has NO `get` grant.
	res = Get(ctx, serveTestRef(deniedNS, "obj-denied"))

	if rbacNarrowBaseline {
		// NEGATIVE CONTROL (pre-fix behaviour). objects.Get's informer
		// branch serves the raw object with no RBAC check → the denied
		// object reaches the user.
		if res.Unstructured == nil {
			t.Fatalf("BASELINE negative control: expected over-exposure (denied object served); got nil — if this fails the bug is already fixed; rerun without -tags rbac_narrow_baseline")
		}
		if s := ObjectsGetStatsSnapshot(); s.InformerServed != 1 {
			t.Fatalf("BASELINE negative control: expected InformerServed=1 (object leaked); got %d", s.InformerServed)
		}
		t.Logf("BASELINE negative control reproduced over-exposure: narrow user %q was served object %q in denied namespace %q",
			narrowUser, "obj-denied", deniedNS)
		return
	}

	// Tag-0.30.101 behaviour: the denied GET is NOT served from the
	// informer — it falls through to the apiserver (which the test
	// fake cannot satisfy without a user endpoint, so Unstructured is
	// nil and the fallthrough counter incremented).
	if res.Unstructured != nil {
		t.Fatalf("denied GET: LEAK — informer served object %q in namespace %q to user %q who has no `get` grant",
			"obj-denied", deniedNS, narrowUser)
	}
	s := ObjectsGetStatsSnapshot()
	if s.InformerServed != 0 {
		t.Fatalf("denied GET: InformerServed must stay 0 (RBAC fallthrough); got %d", s.InformerServed)
	}
	if s.ApiserverFallthrough != 1 {
		t.Fatalf("denied GET: ApiserverFallthrough want 1 (RBAC-denied → apiserver); got %d", s.ApiserverFallthrough)
	}
}

// TestGet_RBACFailClosed_NilIdentity asserts the fail-closed contract:
// an informer-served GET with no UserInfo on the context MUST NOT serve
// the raw informer object — it falls through to the apiserver (whose
// per-user token gate is the authoritative answer).
func TestGet_RBACFailClosed_NilIdentity(t *testing.T) {
	resetServeCounters()
	t.Setenv("RESOLVER_USE_INFORMER", "true")
	newRBACNarrowServeWatcher(t)

	// No UserInfo attached — context.Background() only. Use the
	// AUTHORIZED namespace's object so the ONLY thing preventing a
	// serve is the missing identity, not the namespace grant.
	res := Get(context.Background(), serveTestRef(authorizedNS, "obj-authorized"))

	if rbacNarrowBaseline {
		// Pre-fix: the informer branch never reads identity → serves.
		if res.Unstructured == nil {
			t.Fatalf("BASELINE negative control: expected informer serve (no identity check); got nil")
		}
		if s := ObjectsGetStatsSnapshot(); s.InformerServed != 1 {
			t.Fatalf("BASELINE negative control: expected InformerServed=1; got %d", s.InformerServed)
		}
		return
	}

	// Tag-0.30.101 fail-closed: no identity → not served → apiserver
	// fallthrough. NEVER the raw informer object.
	if res.Unstructured != nil {
		t.Fatalf("fail-closed: nil identity was served the raw informer object — MUST fall through")
	}
	s := ObjectsGetStatsSnapshot()
	if s.InformerServed != 0 {
		t.Fatalf("fail-closed: InformerServed must stay 0; got %d", s.InformerServed)
	}
	if s.ApiserverFallthrough != 1 {
		t.Fatalf("fail-closed: ApiserverFallthrough want 1; got %d", s.ApiserverFallthrough)
	}
}

// TestGet_RBACNarrowing_GroupGrant confirms a group subject also
// narrows correctly — a user whose only `get` grant is via a Group
// subject is served the authorized object. Guards against the filter
// only matching User subjects.
func TestGet_RBACNarrowing_GroupGrant(t *testing.T) {
	if rbacNarrowBaseline {
		t.Skip("group-grant assertion is Tag-0.30.101 only")
	}
	resetServeCounters()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVER_USE_INFORMER", "true")

	var seed []runtime.Object
	seed = append(seed, newServeTestObject(authorizedNS, "obj-authorized", "marker-a"))
	seed = append(seed, getterClusterRole())
	// RoleBinding grants the GROUP "devs", not the user directly.
	grpBinding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Namespace: authorizedNS, Name: "grp-binding"},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.GroupKind, APIGroup: "rbac.authorization.k8s.io", Name: "devs"},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "restaction-getter"},
	}
	seed = append(seed, grpBinding)

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		serveTestScheme(), serveTestListKinds(), seed...)
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
	added, syncCh := rw.EnsureResourceType(serveTestGVR)
	if !added {
		t.Fatalf("EnsureResourceType: want added=true")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("serve informer did not sync")
	}
	ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(ctx2, 5*time.Second); err != nil {
		t.Fatalf("WaitForCacheSync: %v", err)
	}
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	// User "alice" is in group "devs".
	res := Get(ctxWithUser("alice", "devs"), serveTestRef(authorizedNS, "obj-authorized"))
	if res.Unstructured == nil {
		t.Fatalf("group-grant GET: expected informer-served object; got nil (Err=%v)", res.Err)
	}
	if s := ObjectsGetStatsSnapshot(); s.InformerServed != 1 {
		t.Fatalf("group-grant GET: InformerServed want 1; got %d", s.InformerServed)
	}
}
