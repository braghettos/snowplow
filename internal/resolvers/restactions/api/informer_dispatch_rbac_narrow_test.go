// informer_dispatch_rbac_narrow_test.go — Tag 0.30.100 falsifier.
//
// FALSIFIER (HARD GATE per feedback_falsifier_first_before_ship.md):
// the 0.30.99 resolver pivot over-exposes data. dispatchViaInformer's
// LIST branch returns the raw informer namespace partition with no
// per-user RBAC filter — a narrow-RBAC user (no grant for bench-ns-*)
// saw ALL 49,999 compositions via compositions-list.
//
// These tests assert PER-USER RBAC equivalence on the pivot-served
// LIST branch:
//
//   - TestDispatchViaInformer_RBACNarrowing_ListIsUserScoped
//     A narrow-RBAC user authorized for only SOME namespaces gets back
//     EXACTLY that authorized subset (item-exact, not "non-empty").
//     NEGATIVE CONTROL: against pre-Tag-A code this same scenario
//     returns ALL items — the over-exposure reproduced. See the
//     buildTag()=="baseline" branch.
//
//   - TestDispatchViaInformer_RBACFailClosed_NilIdentity
//     A served LIST with no UserInfo on the context serves NOTHING
//     (or falls through) — never the unfiltered partition.
//
// Build modes:
//   - default (Tag-A code present): assertions expect the filtered
//     subset. `go test ./internal/resolvers/restactions/api/...`
//   - `-tags rbac_narrow_baseline`: flips the LIST assertion to expect
//     the FULL unfiltered partition, proving the test catches the bug
//     when run against current (pre-fix) behaviour. Captured at the
//     preflight gate as the negative control.
//
// Constraint: uses the in-memory dynamic-fake watcher (newDispatchWatcher
// pattern) — NEVER the kind cluster / remote kubeconfig. The watcher's
// RBAC informers are eagerly registered by NewResourceWatcher, so RBAC
// objects seeded into the same dynamic fake reach EvaluateRBAC.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// rbacGVR* — the well-known RBAC GVRs the watcher eagerly registers.
var (
	rbacRoleBindingsGVR = schema.GroupVersionResource{
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings",
	}
)

// narrowUser is the narrow-RBAC principal: authorized to `list`
// restactions in only two of the seeded namespaces.
const narrowUser = "cyberjoker"

// authorizedNamespaces are the namespaces narrowUser may list. Every
// other seeded namespace must be invisible to them.
var authorizedNamespaces = []string{"team-a", "team-b"}

// deniedNamespaces are seeded namespaces narrowUser has NO RBAC grant
// for — the pre-Tag-A pivot leaked all of these.
var deniedNamespaces = []string{"bench-ns-1", "bench-ns-2", "bench-ns-3", "kube-system"}

// narrowingRoleBinding builds a RoleBinding in ns that grants narrowUser
// `list` on the dispatch test GVR's resource. One RoleBinding per
// authorized namespace = the realistic narrow-RBAC shape.
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
			Name:     "restaction-lister",
		},
	}
}

// listerClusterRole grants `list` on the dispatch test GVR's resource.
// Referenced (namespace-scoped) by each narrowingRoleBinding.
func listerClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "restaction-lister"},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{dispatchTestGVR.Group},
				Resources: []string{dispatchTestGVR.Resource},
				Verbs:     []string{"list"},
			},
		},
	}
}

// newRBACNarrowWatcher constructs a synced cache=on watcher seeded with:
//   - restactions across BOTH authorized and denied namespaces;
//   - a ClusterRole + per-authorized-namespace RoleBindings narrowing
//     narrowUser to authorizedNamespaces only.
//
// Returns the watcher and the set of object names narrowUser SHOULD see
// on a cluster-wide LIST (the authorized subset).
func newRBACNarrowWatcher(t *testing.T) (*cache.ResourceWatcher, map[string]bool) {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")

	var seed []runtime.Object
	authorized := map[string]bool{}

	// Two objects per authorized namespace — the visible subset.
	for _, ns := range authorizedNamespaces {
		for _, suffix := range []string{"x", "y"} {
			name := ns + "-" + suffix
			seed = append(seed, newTestRestActionRuntimeObject(ns, name, "marker"))
			authorized[name] = true
		}
	}
	// Two objects per denied namespace — the leak surface. NOT in the
	// authorized set.
	for _, ns := range deniedNamespaces {
		for _, suffix := range []string{"x", "y"} {
			seed = append(seed, newTestRestActionRuntimeObject(ns, ns+"-"+suffix, "marker"))
		}
	}

	// RBAC objects — the watcher eagerly registers RBAC informers, so
	// these reach EvaluateRBAC through the same dynamic fake.
	seed = append(seed, listerClusterRole())
	for _, ns := range authorizedNamespaces {
		seed = append(seed, narrowingRoleBinding(ns))
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

	// Register the dispatch GVR informer and wait for sync.
	added, syncCh := rw.EnsureResourceType(dispatchTestGVR)
	if !added {
		t.Fatalf("EnsureResourceType: want added=true; got false")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("EnsureResourceType: dispatch informer did not sync within 5s")
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
	return rw, authorized
}

// (ctxWithUser is defined in refilter_test.go — it attaches a
// jwtutil.UserInfo to the context, exactly the shape dispatchViaInformer's
// RBAC filter reads.)

// servedItemNames decodes a LIST envelope and returns the set of
// metadata.name values.
func servedItemNames(t *testing.T, raw []byte) map[string]bool {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("served LIST is not valid JSON: %v\nbytes: %s", err, string(raw))
	}
	items, _ := envelope["items"].([]any)
	return itemNameSet(items)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestDispatchViaInformer_RBACNarrowing_ListIsUserScoped is the headline
// falsifier. A cluster-wide LIST by a narrow-RBAC user MUST return
// exactly the user's authorized subset.
//
// NEGATIVE CONTROL: built with `-tags rbac_narrow_baseline` this test
// expects the FULL unfiltered partition instead — proving that against
// pre-Tag-A code the test reproduces the over-exposure. Captured at the
// preflight gate.
func TestDispatchViaInformer_RBACNarrowing_ListIsUserScoped(t *testing.T) {
	_, authorized := newRBACNarrowWatcher(t)

	totalSeeded := (len(authorizedNamespaces) + len(deniedNamespaces)) * 2

	// Cluster-wide LIST — the exact shape of the over-exposed
	// compositions-list call (namespace="").
	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/restactions")
	ctx := ctxWithUser(narrowUser)

	raw, served := dispatchViaInformer(ctx, call)
	if !served {
		t.Fatalf("cluster-wide LIST: expected served=true; got served=false")
	}
	got := servedItemNames(t, raw)

	if rbacNarrowBaseline {
		// NEGATIVE CONTROL (pre-Tag-A behaviour). The pivot LIST branch
		// returns the raw partition unfiltered → all seeded items.
		if len(got) != totalSeeded {
			t.Fatalf("BASELINE negative control: expected over-exposure of all %d seeded items; got %d (%v) — if this fails the bug is already fixed; rerun without -tags rbac_narrow_baseline",
				totalSeeded, len(got), sortedKeys(got))
		}
		t.Logf("BASELINE negative control reproduced over-exposure: narrow user %q saw all %d items including denied namespaces %v",
			narrowUser, len(got), deniedNamespaces)
		return
	}

	// Tag-A behaviour: served result == exactly the authorized subset.
	if len(got) != len(authorized) {
		t.Fatalf("RBAC-narrowed LIST: want %d items (authorized subset); got %d\n want: %v\n got:  %v",
			len(authorized), len(got), sortedKeys(authorized), sortedKeys(got))
	}
	for name := range authorized {
		if !got[name] {
			t.Errorf("RBAC-narrowed LIST: authorized item %q missing from served result", name)
		}
	}
	for name := range got {
		if !authorized[name] {
			t.Errorf("RBAC-narrowed LIST: LEAKED item %q — user %q has no RBAC grant for its namespace", name, narrowUser)
		}
	}
}

// TestDispatchViaInformer_RBACNarrowing_NamespacedListScoped covers the
// namespaced-LIST shape: a LIST scoped to a DENIED namespace must serve
// zero items for the narrow user.
func TestDispatchViaInformer_RBACNarrowing_NamespacedListScoped(t *testing.T) {
	newRBACNarrowWatcher(t)
	ctx := ctxWithUser(narrowUser)

	// Authorized namespace → 2 items.
	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/namespaces/team-a/restactions")
	raw, served := dispatchViaInformer(ctx, call)
	if !served {
		t.Fatalf("authorized-namespace LIST: expected served=true")
	}
	if got := servedItemNames(t, raw); len(got) != 2 {
		t.Fatalf("authorized-namespace LIST: want 2 items; got %d (%v)", len(got), sortedKeys(got))
	}

	// Denied namespace → 0 items (filtered) when Tag-A is present.
	call = buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/namespaces/bench-ns-1/restactions")
	raw, served = dispatchViaInformer(ctx, call)
	if !served {
		t.Fatalf("denied-namespace LIST: expected served=true")
	}
	got := servedItemNames(t, raw)
	if rbacNarrowBaseline {
		if len(got) != 2 {
			t.Fatalf("BASELINE negative control: expected denied-namespace LIST to leak all 2 items; got %d", len(got))
		}
		return
	}
	if len(got) != 0 {
		t.Fatalf("denied-namespace LIST: want 0 items (narrow user has no grant); got %d (%v) — LEAK", len(got), sortedKeys(got))
	}
}

// TestDispatchViaInformer_RBACFailClosed_NilIdentity asserts the
// fail-closed contract: a served LIST with no UserInfo on the context
// MUST NOT serve the unfiltered partition. Tag A either serves zero
// items or falls through to apiserver (served=false). It must never
// return the raw partition.
func TestDispatchViaInformer_RBACFailClosed_NilIdentity(t *testing.T) {
	newRBACNarrowWatcher(t)

	// No UserInfo attached — context.Background() only.
	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/restactions")
	raw, served := dispatchViaInformer(context.Background(), call)

	if rbacNarrowBaseline {
		// Pre-Tag-A: the pivot never reads identity → leaks everything.
		if !served {
			t.Fatalf("BASELINE negative control: expected served=true (unfiltered)")
		}
		totalSeeded := (len(authorizedNamespaces) + len(deniedNamespaces)) * 2
		if got := servedItemNames(t, raw); len(got) != totalSeeded {
			t.Fatalf("BASELINE negative control: expected leak of all %d items; got %d", totalSeeded, len(got))
		}
		return
	}

	// Tag-A fail-closed: either fell through (served=false) or served
	// an empty list. NEVER the unfiltered partition.
	if !served {
		if raw != nil {
			t.Fatalf("fail-closed fallthrough: want nil raw; got %d bytes", len(raw))
		}
		return // fell through to apiserver — acceptable fail-closed
	}
	if got := servedItemNames(t, raw); len(got) != 0 {
		t.Fatalf("fail-closed: nil identity served %d items (%v) — MUST serve zero", len(got), sortedKeys(got))
	}
}

// TestDispatchViaInformer_RBACNarrowing_GroupGrant confirms a group
// subject also narrows correctly — a user whose only grant is via a
// Group subject sees the authorized subset, not the full partition.
// (Guards against the filter only matching User subjects.)
func TestDispatchViaInformer_RBACNarrowing_GroupGrant(t *testing.T) {
	if rbacNarrowBaseline {
		t.Skip("group-grant assertion is Tag-A only")
	}
	t.Setenv("CACHE_ENABLED", "true")

	var seed []runtime.Object
	authorized := map[string]bool{}
	for _, ns := range []string{"team-a"} {
		seed = append(seed, newTestRestActionRuntimeObject(ns, ns+"-x", "m"))
		authorized[ns+"-x"] = true
	}
	for _, ns := range []string{"bench-ns-1"} {
		seed = append(seed, newTestRestActionRuntimeObject(ns, ns+"-x", "m"))
	}
	seed = append(seed, listerClusterRole())
	// RoleBinding grants the GROUP "devs", not the user directly.
	grpBinding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "grp-binding"},
		Subjects: []rbacv1.Subject{
			{Kind: rbacv1.GroupKind, APIGroup: "rbac.authorization.k8s.io", Name: "devs"},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "restaction-lister"},
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
	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/restactions")
	raw, served := dispatchViaInformer(ctxWithUser("alice", "devs"), call)
	if !served {
		t.Fatalf("group-grant LIST: expected served=true")
	}
	got := servedItemNames(t, raw)
	if len(got) != len(authorized) {
		t.Fatalf("group-grant LIST: want %d items; got %d (%v)", len(authorized), len(got), sortedKeys(got))
	}
	for name := range authorized {
		if !got[name] {
			t.Errorf("group-grant LIST: authorized item %q missing", name)
		}
	}
}

// guard: rbacRoleBindingsGVR is referenced so the var is not flagged
// unused if a future refactor drops its only use. Cheap, deterministic.
var _ = rbacRoleBindingsGVR
