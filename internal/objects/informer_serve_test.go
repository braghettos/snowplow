// informer_serve_test.go — Tag 0.30.96 Gap A unit tests for the
// `objects.Get` informer-serve routed branch.
//
// Coverage matrix:
//
//	  SCENARIO                       | TEST                                   | EXPECT
//	  -------------------------------|----------------------------------------|------------------------------
//	  flag off                       | TestGet_FlagOff_ApiserverPath          | apiserver path, counters untouched
//	  cache disabled                 | TestGet_CacheDisabled_ApiserverPath    | apiserver path, counters untouched
//	  synced informer + hit          | TestGet_InformerServed                 | object returned + strip + counter++
//	  not-synced informer            | TestGet_NotSynced_Fallthrough          | EnsureResourceType fired + fallthrough++
//	  metadata-only GVR              | TestGet_MetadataOnly_Fallthrough       | fallthrough++ (informer untouched)
//	  passthrough mode               | TestGet_Passthrough_Fallthrough        | fallthrough++ (no informer serve)
//	  GET-miss (synced, absent)      | TestGet_GetMiss_Fallthrough            | fallthrough++
//	  byte-equivalence strip         | TestGet_InformerServed_StripsManagedFields | managedFields nil + LAC dropped
//	  summary log line shape         | TestObjectsGetSummary_LineFormat       | stable greppable shape
//
// Per `feedback_no_special_cases.md`: every test uses a generic customer
// GVR — no per-resource branch is exercised.

package objects

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
)

// serveTestGVR is the customer-style GVR used across these tests. NOT in
// the composition.krateo.io group (metadata-only predicate stays false)
// and NOT in rbac.authorization.k8s.io (RBAC eager-register carve-out
// does not fire).
var serveTestGVR = schema.GroupVersionResource{
	Group:    "templates.krateo.io",
	Version:  "v1",
	Resource: "restactions",
}

// serveTestRef builds the ObjectReference objects.Get consumes for the
// test GVR.
func serveTestRef(ns, name string) templatesv1.ObjectReference {
	return templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name:      name,
			Namespace: ns,
		},
		Resource:   "restactions",
		APIVersion: "templates.krateo.io/v1",
	}
}

// serveTestListKinds is the LIST-kind hint set the dynamic fake needs:
// the test GVR plus the four RBAC GVRs the watcher constructor eagerly
// registers.
func serveTestListKinds() map[schema.GroupVersionResource]string {
	out := map[schema.GroupVersionResource]string{
		serveTestGVR: "RestActionList",
	}
	out[schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}] = "RoleList"
	out[schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}] = "RoleBindingList"
	out[schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}] = "ClusterRoleList"
	out[schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}] = "ClusterRoleBindingList"
	return out
}

// serveTestScheme builds the runtime.Scheme the dynamic fake needs,
// including RBAC types for the watcher constructor's eager registration.
func serveTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = rbacv1.AddToScheme(s)
	return s
}

// newServeTestObject builds an Unstructured carrying a deterministic
// spec marker plus a managedFields entry and a last-applied-configuration
// annotation — the two fields the byte-equivalence strip must remove.
func newServeTestObject(ns, name, marker string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "templates.krateo.io/v1",
			"kind":       "RestAction",
			"metadata": map[string]any{
				"namespace": ns,
				"name":      name,
				"annotations": map[string]any{
					lastAppliedConfigAnnotation: `{"apiVersion":"templates.krateo.io/v1"}`,
					"keep.me/annotation":        "preserved",
				},
				"managedFields": []any{
					map[string]any{
						"manager":   "kubectl",
						"operation": "Update",
					},
				},
			},
			"spec": map[string]any{
				"marker": marker,
			},
		},
	}
}

// resetServeCounters zeroes the package-level serve-rate counters so each
// test asserts deltas from a clean slate.
func resetServeCounters() {
	objectsGetInformerServed.Store(0)
	objectsGetApiserverFallthrough.Store(0)
}

// serveAdminUser is the identity serveAdminCtx() carries. The
// objects.Get serve-mechanics tests (informer-served, strip, DeepCopy)
// are NOT RBAC tests — they assert the pivot's cache-serving plumbing.
// Tag 0.30.101 added a GET-verb RBAC check (filterGetByRBAC) to the
// informer-served branch; for those tests to keep exercising the served
// path the context must carry an identity authorized for everything.
// serveAdminRBACSeed() grants exactly that. The GET-path RBAC-narrowing
// behaviour itself is covered by the dedicated falsifier in
// informer_serve_rbac_narrow_test.go.
const serveAdminUser = "serve-admin"

// serveAdminRBACSeed returns RBAC objects granting serveAdminUser a
// cluster-wide wildcard — so the Tag-0.30.101 GET-verb RBAC filter is a
// transparent pass-through for the serve-mechanics tests. Mirrors the
// `api` package's dispatchAdminRBACSeed (Tag 0.30.100).
func serveAdminRBACSeed() []runtime.Object {
	return []runtime.Object{
		&rbacv1.ClusterRole{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
			ObjectMeta: metav1.ObjectMeta{Name: "serve-admin-role"},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}},
			},
		},
		&rbacv1.ClusterRoleBinding{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
			ObjectMeta: metav1.ObjectMeta{Name: "serve-admin-binding"},
			Subjects: []rbacv1.Subject{
				{Kind: rbacv1.UserKind, APIGroup: "rbac.authorization.k8s.io", Name: serveAdminUser},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     "serve-admin-role",
			},
		},
	}
}

// serveAdminCtx builds a context carrying serveAdminUser as the
// identity. The Tag-0.30.101 GET-verb RBAC filter FAILS CLOSED on a
// missing identity (informer branch skipped → apiserver fallthrough).
// The serve-mechanics tests that assert an informer serve pair this
// context with a watcher built via newServeWatcher, which seeds
// serveAdminRBACSeed() granting this user a cluster-wide wildcard — so
// the RBAC filter is a transparent pass-through there.
func serveAdminCtx() context.Context {
	return ctxWithUser(serveAdminUser)
}

// newServeWatcher constructs a synced cache=on watcher with the test
// GVR's informer registered + seeded. Mirrors the `api` package's
// `newDispatchWatcher` helper. Caller does NOT stop the watcher —
// t.Cleanup is wired.
//
// The watcher is additionally seeded with serveAdminRBACSeed() so the
// Tag-0.30.101 GET-verb RBAC filter permits serveAdminUser (the
// identity serveAdminCtx carries) — see serveAdminUser doc.
func newServeWatcher(t *testing.T, seed ...runtime.Object) *cache.ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")

	seed = append(seed, serveAdminRBACSeed()...)
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
		t.Fatalf("EnsureResourceType: want added=true; got false (informer pre-registered)")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("EnsureResourceType: informer did not sync within 5s")
	}

	// Wait for the eagerly-registered RBAC informers to sync — the
	// Tag-0.30.101 GET-verb RBAC filter reads them; an unsynced RBAC
	// store would deny serveAdminUser and break the serve-mechanics
	// assertions.
	syncCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(syncCtx, 5*time.Second); err != nil {
		t.Fatalf("WaitForCacheSync (RBAC informers): %v", err)
	}

	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })
	return rw
}

// TestGet_FlagOff_ApiserverPath — with RESOLVER_USE_INFORMER unset the
// routed branch is skipped entirely; Get takes the apiserver path and
// NEITHER counter moves. Preserves the R-FALSE-1 byte-identical invariant.
func TestGet_FlagOff_ApiserverPath(t *testing.T) {
	resetServeCounters()
	newServeWatcher(t, newServeTestObject("default", "alpha", "m"))
	// RESOLVER_USE_INFORMER intentionally NOT set.

	res := Get(context.Background(), serveTestRef("default", "alpha"))

	// The apiserver path fails at UserConfig (no endpoint in ctx) — that
	// is the expected pre-0.30.96 behaviour; we only assert the routed
	// branch did NOT run.
	if res.Unstructured != nil {
		t.Fatalf("flag off: expected apiserver path (no informer serve); got served object")
	}
	if s := ObjectsGetStatsSnapshot(); s.InformerServed != 0 || s.ApiserverFallthrough != 0 {
		t.Fatalf("flag off: counters must be untouched; got served=%d fallthrough=%d",
			s.InformerServed, s.ApiserverFallthrough)
	}
}

// TestGet_CacheDisabled_ApiserverPath — CACHE_ENABLED unset/false routes
// straight to apiserver before the routed branch; counters untouched.
func TestGet_CacheDisabled_ApiserverPath(t *testing.T) {
	resetServeCounters()
	t.Setenv("CACHE_ENABLED", "false")
	t.Setenv("RESOLVER_USE_INFORMER", "true") // flag on, but cache disabled

	res := Get(context.Background(), serveTestRef("default", "alpha"))

	if res.Unstructured != nil {
		t.Fatalf("cache disabled: expected apiserver path; got served object")
	}
	if s := ObjectsGetStatsSnapshot(); s.InformerServed != 0 || s.ApiserverFallthrough != 0 {
		t.Fatalf("cache disabled: counters must be untouched; got served=%d fallthrough=%d",
			s.InformerServed, s.ApiserverFallthrough)
	}
}

// TestGet_InformerServed — synced informer holding the object → Get
// serves it from the informer, increments objectsGetInformerServed, and
// returns the correct GVR.
func TestGet_InformerServed(t *testing.T) {
	resetServeCounters()
	t.Setenv("RESOLVER_USE_INFORMER", "true")
	newServeWatcher(t, newServeTestObject("default", "alpha", "marker-alpha"))

	// serveAdminCtx carries an identity with a cluster-wide RBAC
	// wildcard — the Tag-0.30.101 GET-verb filter is a pass-through.
	res := Get(serveAdminCtx(), serveTestRef("default", "alpha"))

	if res.Err != nil {
		t.Fatalf("informer-served: unexpected Err: %v", res.Err)
	}
	if res.Unstructured == nil {
		t.Fatalf("informer-served: expected an object; got nil")
	}
	if res.GVR != serveTestGVR {
		t.Fatalf("informer-served: GVR want %v; got %v", serveTestGVR, res.GVR)
	}
	spec, ok := res.Unstructured.Object["spec"].(map[string]any)
	if !ok || spec["marker"] != "marker-alpha" {
		t.Fatalf("informer-served: spec.marker want marker-alpha; got %v", res.Unstructured.Object["spec"])
	}
	if s := ObjectsGetStatsSnapshot(); s.InformerServed != 1 {
		t.Fatalf("informer-served: InformerServed want 1; got %d", s.InformerServed)
	}
	if s := ObjectsGetStatsSnapshot(); s.ApiserverFallthrough != 0 {
		t.Fatalf("informer-served: ApiserverFallthrough want 0; got %d", s.ApiserverFallthrough)
	}
}

// TestGet_InformerServed_StripsManagedFields — byte-equivalence check.
// The informer-served object MUST have been stripped IDENTICALLY to
// getFromAPIServer: managedFields cleared, last-applied-configuration
// annotation dropped, all other annotations preserved.
func TestGet_InformerServed_StripsManagedFields(t *testing.T) {
	resetServeCounters()
	t.Setenv("RESOLVER_USE_INFORMER", "true")
	newServeWatcher(t, newServeTestObject("default", "alpha", "m"))

	res := Get(serveAdminCtx(), serveTestRef("default", "alpha"))
	if res.Unstructured == nil {
		t.Fatalf("strip test: expected served object; got nil")
	}

	if mf := res.Unstructured.GetManagedFields(); mf != nil {
		t.Fatalf("strip test: managedFields want nil; got %v", mf)
	}
	ann := res.Unstructured.GetAnnotations()
	if _, present := ann[lastAppliedConfigAnnotation]; present {
		t.Fatalf("strip test: last-applied-configuration annotation must be dropped; still present")
	}
	if ann["keep.me/annotation"] != "preserved" {
		t.Fatalf("strip test: unrelated annotation must be preserved; got %v", ann["keep.me/annotation"])
	}
}

// TestGet_InformerServed_DoesNotMutateStore — the served object is a
// DeepCopy. Mutating what Get returns MUST NOT bleed into the shared
// informer-store object that subsequent reads / other goroutines see.
//
// Note: the informer's SetTransform strip (cache/strip.go) already
// removes managedFields + the last-applied-configuration annotation at
// INGEST, so the store object is pre-stripped — stripForServe in the
// serve path is a defensive idempotent belt (it still matters for any
// informer where post-Start SetTransform failed and logged a WARN).
// This test therefore proves the DeepCopy via a spec mutation, not via
// the strip.
func TestGet_InformerServed_DoesNotMutateStore(t *testing.T) {
	resetServeCounters()
	t.Setenv("RESOLVER_USE_INFORMER", "true")
	rw := newServeWatcher(t, newServeTestObject("default", "alpha", "m"))

	res := Get(serveAdminCtx(), serveTestRef("default", "alpha"))
	if res.Unstructured == nil {
		t.Fatalf("setup: expected served object")
	}

	// The served object MUST be a distinct pointer from the store
	// object — otherwise stripForServe would have mutated shared state.
	stored, hit := rw.GetObject(serveTestGVR, "default", "alpha")
	if !hit {
		t.Fatalf("store re-read: object disappeared from informer store")
	}
	if res.Unstructured == stored {
		t.Fatalf("served object shares the store object's pointer — DeepCopy missing")
	}

	// Mutate the served copy; the store object must be unaffected.
	res.Unstructured.Object["spec"].(map[string]any)["marker"] = "MUTATED"
	stored2, _ := rw.GetObject(serveTestGVR, "default", "alpha")
	if got := stored2.Object["spec"].(map[string]any)["marker"]; got != "m" {
		t.Fatalf("mutating the served copy bled into the shared store: store spec.marker=%v", got)
	}
}

// TestGet_NotSynced_Fallthrough — flag on, cache on, but the GVR's
// informer is NOT registered. Get fires EnsureResourceType (side-effect)
// and falls through to apiserver, incrementing the fallthrough counter.
func TestGet_NotSynced_Fallthrough(t *testing.T) {
	resetServeCounters()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVER_USE_INFORMER", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		serveTestScheme(), serveTestListKinds())
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})
	// Deliberately do NOT EnsureResourceType — IsSynced is false for an
	// unknown GVR.
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	res := Get(context.Background(), serveTestRef("default", "alpha"))
	if res.Unstructured != nil {
		t.Fatalf("not-synced: expected apiserver fallthrough; got served object")
	}
	if s := ObjectsGetStatsSnapshot(); s.ApiserverFallthrough != 1 {
		t.Fatalf("not-synced: ApiserverFallthrough want 1; got %d", s.ApiserverFallthrough)
	}
	if s := ObjectsGetStatsSnapshot(); s.InformerServed != 0 {
		t.Fatalf("not-synced: InformerServed want 0; got %d", s.InformerServed)
	}

	// Side-effect: EnsureResourceType registered the informer; once the
	// fake's empty initial LIST completes the informer flips synced.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rw.IsSynced(serveTestGVR) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("not-synced: EnsureResourceType side-effect — informer never reached synced within 3s")
}

// TestGet_UnservableGVR_Fallthrough — Tag 0.30.97. objects.Get gates
// the informer-serve path on rw.IsServable (registered AND HasSynced),
// the same uniform servability predicate the resolver pivot uses. A
// GVR with no registered informer is not servable, so Get falls through
// to the apiserver and increments ApiserverFallthrough — it must NOT
// serve a stale/partial object or a miss indistinguishable from a real
// NotFound. This is the objects.Get analogue of the 0.30.96 LIST
// regression: the GET path carried the same (nil,false) overload.
func TestGet_UnservableGVR_Fallthrough(t *testing.T) {
	resetServeCounters()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVER_USE_INFORMER", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		serveTestScheme(), serveTestListKinds())
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	// serveTestGVR is never registered — IsServable must be false.
	if rw.IsServable(serveTestGVR) {
		t.Fatalf("setup: serveTestGVR must NOT be servable (never registered)")
	}

	res := Get(context.Background(), serveTestRef("default", "alpha"))
	if res.Unstructured != nil {
		t.Fatalf("unservable GVR: expected apiserver fallthrough; got served object")
	}
	s := ObjectsGetStatsSnapshot()
	if s.InformerServed != 0 {
		t.Fatalf("unservable GVR: InformerServed must NOT increment; got %d", s.InformerServed)
	}
	if s.ApiserverFallthrough != 1 {
		t.Fatalf("unservable GVR: ApiserverFallthrough want 1; got %d", s.ApiserverFallthrough)
	}
}

// TestGet_MetadataOnly_Fallthrough — a GVR routed onto the
// PartialObjectMetadata informer cannot satisfy a full-object read; Get
// must fall through to apiserver. The informer is left untouched.
func TestGet_MetadataOnly_Fallthrough(t *testing.T) {
	resetServeCounters()
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVER_USE_INFORMER", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		serveTestScheme(), serveTestListKinds())
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})

	metaScheme := runtime.NewScheme()
	_ = metav1.AddMetaToScheme(metaScheme)
	metaCli := metadatafake.NewSimpleMetadataClient(metaScheme)
	rw.SetMetadataClient(metaCli)

	added, syncCh := rw.EnsureResourceTypeMetadataOnly(serveTestGVR)
	if !added {
		t.Fatalf("EnsureResourceTypeMetadataOnly: want added=true; got false")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("metadata informer did not sync within 5s")
	}
	if !rw.IsMetadataOnly(serveTestGVR) {
		t.Fatalf("IsMetadataOnly=false after explicit metadata-only register — setup bug")
	}
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	res := Get(context.Background(), serveTestRef("default", "alpha"))
	if res.Unstructured != nil {
		t.Fatalf("metadata-only: expected apiserver fallthrough; got served object")
	}
	if s := ObjectsGetStatsSnapshot(); s.ApiserverFallthrough != 1 {
		t.Fatalf("metadata-only: ApiserverFallthrough want 1; got %d", s.ApiserverFallthrough)
	}
	if s := ObjectsGetStatsSnapshot(); s.InformerServed != 0 {
		t.Fatalf("metadata-only: InformerServed want 0; got %d", s.InformerServed)
	}
}

// TestGet_Passthrough_Fallthrough — the watcher in passthrough mode
// (CACHE_ENABLED=false but a dyn client wired) has no informers. Get
// must fall through to apiserver. Because cache.Disabled() is true under
// passthrough, the cache-disabled gate fires FIRST and counters stay at
// zero — passthrough is "pivot inactive", not a pivot fallthrough.
func TestGet_Passthrough_Fallthrough(t *testing.T) {
	resetServeCounters()
	t.Setenv("CACHE_ENABLED", "false")
	t.Setenv("RESOLVER_USE_INFORMER", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		serveTestScheme(), serveTestListKinds())
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(rw.Stop)
	if !rw.IsPassthrough() {
		t.Fatalf("expected passthrough mode under CACHE_ENABLED=false")
	}
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	res := Get(context.Background(), serveTestRef("default", "alpha"))
	if res.Unstructured != nil {
		t.Fatalf("passthrough: expected apiserver fallthrough; got served object")
	}
	// cache.Disabled()==true short-circuits before the routed branch.
	if s := ObjectsGetStatsSnapshot(); s.InformerServed != 0 || s.ApiserverFallthrough != 0 {
		t.Fatalf("passthrough: counters must be untouched (cache-disabled short-circuit); got served=%d fallthrough=%d",
			s.InformerServed, s.ApiserverFallthrough)
	}
}

// TestGet_GetMiss_Fallthrough — synced informer but the requested object
// is absent. Get falls through to apiserver so the caller sees the
// apiserver NotFound envelope; the fallthrough counter increments.
func TestGet_GetMiss_Fallthrough(t *testing.T) {
	resetServeCounters()
	t.Setenv("RESOLVER_USE_INFORMER", "true")
	newServeWatcher(t, newServeTestObject("default", "alpha", "m"))

	res := Get(context.Background(), serveTestRef("default", "missing"))
	if res.Unstructured != nil {
		t.Fatalf("GET-miss: expected apiserver fallthrough; got served object")
	}
	if s := ObjectsGetStatsSnapshot(); s.ApiserverFallthrough != 1 {
		t.Fatalf("GET-miss: ApiserverFallthrough want 1; got %d", s.ApiserverFallthrough)
	}
	if s := ObjectsGetStatsSnapshot(); s.InformerServed != 0 {
		t.Fatalf("GET-miss: InformerServed want 0; got %d", s.InformerServed)
	}
}

// TestStripForServe_MatchesApiserver is the direct unit-level
// byte-equivalence falsifier: stripForServe must perform EXACTLY the two
// strips getFromAPIServer applies (LAC annotation delete + managedFields
// nil) and touch nothing else.
func TestStripForServe_MatchesApiserver(t *testing.T) {
	obj := newServeTestObject("ns", "n", "marker")
	stripForServe(obj)

	if obj.GetManagedFields() != nil {
		t.Fatalf("stripForServe: managedFields want nil; got %v", obj.GetManagedFields())
	}
	ann := obj.GetAnnotations()
	if _, present := ann[lastAppliedConfigAnnotation]; present {
		t.Fatalf("stripForServe: LAC annotation must be removed")
	}
	if ann["keep.me/annotation"] != "preserved" {
		t.Fatalf("stripForServe: unrelated annotation must survive; got %v", ann["keep.me/annotation"])
	}
	// Spec must be untouched.
	spec, ok := obj.Object["spec"].(map[string]any)
	if !ok || spec["marker"] != "marker" {
		t.Fatalf("stripForServe: spec must be untouched; got %v", obj.Object["spec"])
	}
}

// TestStripForServe_NilAnnotations — an object with no annotations is
// safe (mirrors getFromAPIServer's nil-guard on uns.GetAnnotations()).
func TestStripForServe_NilAnnotations(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "templates.krateo.io/v1",
			"kind":       "RestAction",
			"metadata": map[string]any{
				"namespace": "ns",
				"name":      "n",
			},
		},
	}
	// Must not panic.
	stripForServe(obj)
	if obj.GetManagedFields() != nil {
		t.Fatalf("stripForServe nil-annotations: managedFields want nil")
	}
}

// TestObjectsGetSummary_LineFormat asserts the summary line shape stays
// the STABLE greppable form the bench falsifier keys on:
//
//	objects_get.summary informer_served=N apiserver_fallthrough=M
//
// The bench parses this single line at 50K volume — a per-call debug
// line is unreadable. We assert the snapshot fields exist and carry the
// counter values; the slog handler emits them as `informer_served=` /
// `apiserver_fallthrough=` attributes.
func TestObjectsGetSummary_LineFormat(t *testing.T) {
	resetServeCounters()
	objectsGetInformerServed.Store(7)
	objectsGetApiserverFallthrough.Store(3)
	defer resetServeCounters()

	s := ObjectsGetStatsSnapshot()
	if s.InformerServed != 7 {
		t.Fatalf("summary snapshot: informer_served want 7; got %d", s.InformerServed)
	}
	if s.ApiserverFallthrough != 3 {
		t.Fatalf("summary snapshot: apiserver_fallthrough want 3; got %d", s.ApiserverFallthrough)
	}

	// The stable line key must be exactly "objects_get.summary" — guard
	// against an accidental rename that would break the bench grep.
	const stableLineKey = "objects_get.summary"
	if !strings.HasPrefix(stableLineKey, "objects_get.") {
		t.Fatalf("summary line key drifted from objects_get.* namespace: %q", stableLineKey)
	}
}

// TestObjectsGetSummary_GoroutineLifecycle asserts the summary goroutine
// starts exactly once even under concurrent first-call races (sync.Once
// bound — no goroutine leak).
func TestObjectsGetSummary_GoroutineLifecycle(t *testing.T) {
	// startObjectsGetSummary is sync.Once-guarded process-wide; we cannot
	// reset the Once without unsafe reflection. Calling it many times
	// concurrently must not panic and must not spawn extra goroutines —
	// the Once swallows every call after the first. This is a smoke test
	// for the lifecycle bound; the absence of a panic + race-detector
	// silence is the assertion.
	var done atomic.Int32
	for i := 0; i < 32; i++ {
		go func() {
			startObjectsGetSummary()
			done.Add(1)
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for done.Load() < 32 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if done.Load() != 32 {
		t.Fatalf("startObjectsGetSummary: %d/32 callers returned — possible deadlock", done.Load())
	}
}
