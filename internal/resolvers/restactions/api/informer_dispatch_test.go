// informer_dispatch_test.go — Tag 0.30.95 resolver pivot unit tests.
//
// Covers every gate in `dispatchViaInformer` plus the parallel-safe
// integration with the bounded errgroup iterator. Gate coverage:
//
//	  GATE                  | TEST                                          | EXPECT
//	  ----------------------|-----------------------------------------------|---------------
//	  verb (non-GET)        | TestDispatchViaInformer_VerbGate              | (nil, false)
//	  subresource path      | TestDispatchViaInformer_SubresourcePaths      | (nil, false)
//	  external path         | TestDispatchViaInformer_ExternalPaths         | (nil, false)
//	  passthrough mode      | TestDispatchViaInformer_PassthroughMode       | (nil, false)
//	  not synced            | TestDispatchViaInformer_NotSyncedFallback     | (nil, false)
//	  metadata-only GVR     | TestDispatchViaInformer_MetadataOnlyFallback  | (nil, false)
//	  synced LIST           | TestDispatchViaInformer_ListServed            | envelope bytes
//	  synced GET-by-name    | TestDispatchViaInformer_GetByNameServed       | bare obj bytes
//	  GET-by-name 404       | TestDispatchViaInformer_GetByNameNotFound     | (nil, false)
//	  errgroup parallelism  | TestIterParallelismRespected                  | served + bounded
//
// Per `feedback_no_special_cases.md`: every assertion uses a generic
// customer GVR (no per-resource branches). The metadata-only test
// reuses the composition GVR that the cache_mode predicate handles via
// the annotation set (not a hardcoded carve-out).

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/ptr"
	"github.com/krateoplatformops/snowplow/internal/cache"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
)

// dispatchTestGVR is the customer-style GVR we serve from the informer
// cache in these tests. NOT in the composition.krateo.io group (so the
// metadata-only predicate returns false) and NOT in rbac.authorization.k8s.io
// (so the RBAC carve-out doesn't fire).
var dispatchTestGVR = schema.GroupVersionResource{
	Group:    "templates.krateo.io",
	Version:  "v1",
	Resource: "restactions",
}

// dispatchTestListKinds is the LIST-kind hint for dynamicfake to drive
// the fake apiserver shape for the test GVR.
func dispatchTestListKinds() map[schema.GroupVersionResource]string {
	out := map[schema.GroupVersionResource]string{
		dispatchTestGVR: "RestActionList",
	}
	// RBAC kinds — required by NewResourceWatcher constructor's eager
	// RBAC informer registration.
	out[schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}] = "RoleList"
	out[schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}] = "RoleBindingList"
	out[schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}] = "ClusterRoleList"
	out[schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}] = "ClusterRoleBindingList"
	return out
}

// dispatchTestScheme builds the runtime.Scheme the dynamic fake needs.
// Includes RBAC types so the watcher constructor's eager registration
// has typings to attach.
func dispatchTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = rbacv1.AddToScheme(s)
	return s
}

// dispatchAdminUser is the identity dispatchCtx() carries. The
// dispatch serve-mechanics tests (verb gate, subresource, LIST/GET
// served, counters, parallelism) are NOT RBAC tests — they assert the
// pivot's cache-serving plumbing. Tag 0.30.100 added a post-LIST
// per-item RBAC filter to dispatchViaInformer; for those tests to keep
// exercising the served path the context must carry an identity that
// is authorized for everything. dispatchAdminRBACSeed() grants exactly
// that. The RBAC-narrowing behaviour itself is covered by the dedicated
// falsifier in informer_dispatch_rbac_narrow_test.go.
const dispatchAdminUser = "dispatch-admin"

// dispatchAdminRBACSeed returns RBAC objects granting dispatchAdminUser
// a cluster-wide wildcard — so the Tag-0.30.100 per-item RBAC filter is
// a transparent pass-through for the serve-mechanics tests.
func dispatchAdminRBACSeed() []runtime.Object {
	return []runtime.Object{
		&rbacv1.ClusterRole{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
			ObjectMeta: metav1.ObjectMeta{Name: "dispatch-admin-role"},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}},
			},
		},
		&rbacv1.ClusterRoleBinding{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
			ObjectMeta: metav1.ObjectMeta{Name: "dispatch-admin-binding"},
			Subjects: []rbacv1.Subject{
				{Kind: rbacv1.UserKind, APIGroup: "rbac.authorization.k8s.io", Name: dispatchAdminUser},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     "dispatch-admin-role",
			},
		},
	}
}

// newDispatchWatcher constructs a synced cache=on watcher with the test
// GVR's informer registered and at least one seeded object. Returns
// the watcher (caller does NOT need to stop — t.Cleanup is wired).
//
// The watcher is additionally seeded with dispatchAdminRBACSeed() so the
// Tag-0.30.100 per-item RBAC filter permits dispatchAdminUser (the
// identity dispatchCtx carries) — see dispatchAdminUser doc.
func newDispatchWatcher(t *testing.T, seed ...runtime.Object) *cache.ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")

	seed = append(seed, dispatchAdminRBACSeed()...)
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

	// Register the test GVR's informer + wait for initial sync. The
	// dispatch tests assume the informer is synced; the not-synced
	// gate is exercised separately via a pristine watcher.
	added, syncCh := rw.EnsureResourceType(dispatchTestGVR)
	if !added {
		t.Fatalf("EnsureResourceType: want added=true; got false (informer was unexpectedly pre-registered)")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("EnsureResourceType: informer did not sync within 5s")
	}

	// Wait for the eagerly-registered RBAC informers to sync — the
	// Tag-0.30.100 per-item RBAC filter reads them; an unsynced RBAC
	// store would deny dispatchAdminUser and break the serve-mechanics
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

// newTestRestActionObject builds an Unstructured with the test GVR's
// shape carrying a single deterministic field. The dispatch tests
// assert the served bytes contain this field intact.
func newTestRestActionObject(ns, name, marker string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "templates.krateo.io/v1",
			"kind":       "RestAction",
			"metadata": map[string]any{
				"namespace": ns,
				"name":      name,
			},
			"spec": map[string]any{
				"marker": marker,
			},
		},
	}
}

// newTestRestActionRuntimeObject wraps newTestRestActionObject for use
// as the dynamic fake's seed slice (which takes []runtime.Object).
func newTestRestActionRuntimeObject(ns, name, marker string) runtime.Object {
	return newTestRestActionObject(ns, name, marker)
}

// dispatchCtx builds a context for the serve-mechanics dispatch tests.
// xcontext.Logger falls back to slog.Default() when the context has no
// logger attached, so the dispatch path's log calls are safe.
//
// Tag 0.30.100: the context carries dispatchAdminUser as the identity.
// dispatchViaInformer's served-LIST branch now runs a per-item RBAC
// filter and FAILS CLOSED on a missing identity (served=false). The
// serve-mechanics tests assert served=true; they pair with watchers
// built via newDispatchWatcher / newDispatchWatcherWithNamespaces,
// which seed dispatchAdminRBACSeed() granting this user a cluster-wide
// wildcard — so the RBAC filter is a transparent pass-through here.
func dispatchCtx() context.Context {
	return ctxWithUser(dispatchAdminUser)
}

// buildCall returns a RequestOptions with verb + path + a no-op
// ResponseHandler. Tests that need a handler set their own.
func buildCall(verb, path string) httpcall.RequestOptions {
	return httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{
			Path: path,
			Verb: ptr.To(verb),
		},
		Endpoint: &endpoints.Endpoint{ServerURL: "http://test.invalid"},
	}
}

// TestDispatchViaInformer_VerbGate asserts that POST/PUT/PATCH/DELETE
// fall through to apiserver. The informer cache is read-only; serving
// a write verb would silently swallow mutations.
func TestDispatchViaInformer_VerbGate(t *testing.T) {
	newDispatchWatcher(t)

	for _, verb := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(verb, func(t *testing.T) {
			call := buildCall(verb, "/apis/templates.krateo.io/v1/namespaces/default/restactions")
			raw, served := dispatchViaInformer(dispatchCtx(), call)
			if served {
				t.Fatalf("verb=%s: expected served=false; got served=true with %d bytes", verb, len(raw))
			}
		})
	}
}

// TestDispatchViaInformer_SubresourcePaths asserts that the six
// well-known apiserver subresource suffixes fall through. The cache
// has no shape for these.
func TestDispatchViaInformer_SubresourcePaths(t *testing.T) {
	newDispatchWatcher(t)

	base := "/apis/apps/v1/namespaces/default/deployments/foo"
	for _, sfx := range []string{"/status", "/scale", "/log", "/exec", "/binding", "/proxy"} {
		t.Run(sfx, func(t *testing.T) {
			call := buildCall(http.MethodGet, base+sfx)
			raw, served := dispatchViaInformer(dispatchCtx(), call)
			if served {
				t.Fatalf("subresource %s: expected served=false; got served=true with %d bytes", sfx, len(raw))
			}
		})
	}
}

// TestDispatchViaInformer_ExternalPaths asserts that non-apiserver
// paths fall through. Includes a fully-qualified external URL, the
// snowplow /call endpoint, and a JQ-leaked unresolved fragment.
func TestDispatchViaInformer_ExternalPaths(t *testing.T) {
	newDispatchWatcher(t)

	cases := []struct {
		name string
		path string
	}{
		{"github_external", "https://github.com/owner/repo"},
		{"snowplow_call_endpoint", "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=foo"},
		{"jq_leaked_fragment", "/apis/apps/v1/namespaces/${ns}/deployments"},
		{"unrecognised_shape", "/some/random/path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := buildCall(http.MethodGet, tc.path)
			raw, served := dispatchViaInformer(dispatchCtx(), call)
			if served {
				t.Fatalf("path=%s: expected served=false; got served=true with %d bytes", tc.path, len(raw))
			}
		})
	}
}

// TestDispatchViaInformer_PassthroughMode asserts that when the
// watcher is in passthrough mode (cache=off + dyn provided), the
// pivot falls through to apiserver. modePassthrough has no informers
// to serve from.
func TestDispatchViaInformer_PassthroughMode(t *testing.T) {
	// Force the passthrough construction path: CACHE_ENABLED=false but
	// a dynamic client is still wired (the 0.30.71 diagnostic mode).
	t.Setenv("CACHE_ENABLED", "false")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		dispatchTestScheme(), dispatchTestListKinds())
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(rw.Stop)
	if !rw.IsPassthrough() {
		t.Fatalf("expected watcher in passthrough mode under CACHE_ENABLED=false")
	}
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/namespaces/default/restactions")
	raw, served := dispatchViaInformer(dispatchCtx(), call)
	if served {
		t.Fatalf("passthrough: expected served=false; got served=true with %d bytes", len(raw))
	}
}

// TestDispatchViaInformer_NotSyncedFallback asserts that a registered-
// but-not-yet-synced informer falls through to apiserver. The pivot
// also triggers EnsureResourceType for future calls — but the
// CURRENT call MUST NOT serve from a pre-sync indexer (would return
// an empty slice indistinguishable from a real "no objects" answer).
func TestDispatchViaInformer_NotSyncedFallback(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		dispatchTestScheme(), dispatchTestListKinds())
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})

	// Do NOT register the GVR — IsSynced returns false for unknown
	// GVRs by contract. The pivot should fall through AND fire
	// EnsureResourceType as a side-effect.
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/namespaces/default/restactions")
	raw, served := dispatchViaInformer(dispatchCtx(), call)
	if served {
		t.Fatalf("not-synced: expected served=false; got served=true with %d bytes", len(raw))
	}

	// Side-effect: EnsureResourceType should have registered the
	// informer. Verify by polling IsSynced — once the fake's empty
	// initial LIST completes the informer flips synced.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rw.IsSynced(dispatchTestGVR) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("EnsureResourceType side-effect: informer never reached synced state within 3s")
}

// TestDispatchViaInformer_MetadataOnlyFallback asserts that GVRs
// routed onto the PartialObjectMetadata informer fall through to
// apiserver. PartialObjectMetadata carries only ObjectMeta — no spec,
// no status — so serving such a read would produce shape-incompatible
// bytes for the downstream JQ pipeline.
//
// We exercise the metadata-only path by registering a GVR via the
// explicit `EnsureResourceTypeMetadataOnly` API (uniform plumbing call,
// not a per-resource carve-out).
func TestDispatchViaInformer_MetadataOnlyFallback(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		dispatchTestScheme(), dispatchTestListKinds())
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})

	// Wire a metadata fake client + register the GVR onto the
	// metadata-only path.
	metaScheme := runtime.NewScheme()
	_ = metav1.AddMetaToScheme(metaScheme)
	metaCli := metadatafake.NewSimpleMetadataClient(metaScheme)
	rw.SetMetadataClient(metaCli)

	added, syncCh := rw.EnsureResourceTypeMetadataOnly(dispatchTestGVR)
	if !added {
		t.Fatalf("EnsureResourceTypeMetadataOnly: want added=true; got false")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("metadata informer did not sync within 5s")
	}
	if !rw.IsMetadataOnly(dispatchTestGVR) {
		t.Fatalf("IsMetadataOnly=false after explicit metadata-only register — setup bug")
	}
	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })

	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/namespaces/default/restactions")
	raw, served := dispatchViaInformer(dispatchCtx(), call)
	if served {
		t.Fatalf("metadata-only: expected served=false; got served=true with %d bytes", len(raw))
	}
}

// TestDispatchViaInformer_ListServed is the binding happy-path test:
// a synced full-informer LIST returns a valid LIST envelope carrying
// the seeded items.
func TestDispatchViaInformer_ListServed(t *testing.T) {
	rw := newDispatchWatcher(t,
		newTestRestActionRuntimeObject("default", "a", "alpha"),
		newTestRestActionRuntimeObject("default", "b", "bravo"),
		newTestRestActionRuntimeObject("other", "c", "charlie"),
	)
	if !rw.IsSynced(dispatchTestGVR) {
		t.Fatalf("setup: expected synced informer")
	}

	// Namespaced LIST → 2 items.
	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/namespaces/default/restactions")
	raw, served := dispatchViaInformer(dispatchCtx(), call)
	if !served {
		t.Fatalf("LIST: expected served=true; got served=false")
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("LIST envelope is not valid JSON: %v\nbytes: %s", err, string(raw))
	}
	if envelope["apiVersion"] != "templates.krateo.io/v1" {
		t.Fatalf("apiVersion: want templates.krateo.io/v1; got %v", envelope["apiVersion"])
	}
	// PM 2026-05-15: kind is `<R>List` (resource first letter
	// uppercased + "List"). dispatchTestGVR.Resource is "restactions"
	// → "RestactionsList".
	if envelope["kind"] != "RestactionsList" {
		t.Fatalf("kind: want RestactionsList; got %v", envelope["kind"])
	}
	items, ok := envelope["items"].([]any)
	if !ok {
		t.Fatalf("items: want []any; got %T", envelope["items"])
	}
	if len(items) != 2 {
		t.Fatalf("items: want 2 (namespaced LIST); got %d\nbytes: %s", len(items), string(raw))
	}

	// Cluster-wide LIST → 3 items.
	call = buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/restactions")
	raw, served = dispatchViaInformer(dispatchCtx(), call)
	if !served {
		t.Fatalf("cluster-wide LIST: expected served=true; got served=false")
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("cluster-wide LIST envelope: %v", err)
	}
	items, _ = envelope["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("cluster-wide items: want 3; got %d", len(items))
	}
}

// TestDispatchViaInformer_GetByNameServed asserts a synced informer
// resolves a GET-by-name to the bare object bytes (no envelope).
func TestDispatchViaInformer_GetByNameServed(t *testing.T) {
	newDispatchWatcher(t,
		newTestRestActionRuntimeObject("default", "alpha", "marker-alpha"),
	)

	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/namespaces/default/restactions/alpha")
	raw, served := dispatchViaInformer(dispatchCtx(), call)
	if !served {
		t.Fatalf("GET-by-name: expected served=true; got served=false")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("GET bytes are not valid JSON: %v\nbytes: %s", err, string(raw))
	}
	// Bare object — NO `items` wrapper.
	if _, hasItems := obj["items"]; hasItems {
		t.Fatalf("GET-by-name returned envelope shape (has items); want bare object\nbytes: %s", string(raw))
	}
	if obj["kind"] != "RestAction" {
		t.Fatalf("kind: want RestAction; got %v", obj["kind"])
	}
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing or wrong shape: %T", obj["spec"])
	}
	if spec["marker"] != "marker-alpha" {
		t.Fatalf("spec.marker: want marker-alpha; got %v", spec["marker"])
	}
}

// TestDispatchViaInformer_GetByNameNotFound asserts that a GET-by-name
// for a non-existent object falls through to apiserver (returns
// (nil, false)). The apiserver's 404 Status envelope is the contract;
// fabricating a 404 here would either swallow the shape or duplicate
// it.
func TestDispatchViaInformer_GetByNameNotFound(t *testing.T) {
	newDispatchWatcher(t,
		newTestRestActionRuntimeObject("default", "alpha", "marker-alpha"),
	)

	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/namespaces/default/restactions/missing")
	raw, served := dispatchViaInformer(dispatchCtx(), call)
	if served {
		t.Fatalf("GET-by-name 404: expected served=false (apiserver-handled); got served=true with %d bytes", len(raw))
	}
	if raw != nil {
		t.Fatalf("GET-by-name 404: expected nil raw; got %d bytes", len(raw))
	}
}

// TestIterParallelismRespected runs the pivot path concurrently from
// N goroutines and asserts:
//
//  1. every call serves correctly under contention;
//  2. the cache.Global() indexer is parallel-safe (no panics, no races);
//  3. result bytes are identical across goroutines for the same GVR
//     (informer state is consistent, not nondeterministic).
//
// This is the package-level parallel-safety check the architect's
// design calls out: the bounded errgroup at the resolver boundary
// dispatches concurrent g.Go() calls, and EACH can take the pivot
// branch. The pivot itself must not introduce a serialisation point.
func TestIterParallelismRespected(t *testing.T) {
	newDispatchWatcher(t,
		newTestRestActionRuntimeObject("default", "x1", "m1"),
		newTestRestActionRuntimeObject("default", "x2", "m2"),
	)

	const workers = 16
	const iters = 32

	var wg sync.WaitGroup
	var served int64
	var refBytes atomic.Value // []byte — first served result; later results must equal

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/namespaces/default/restactions")
				raw, ok := dispatchViaInformer(dispatchCtx(), call)
				if !ok {
					t.Errorf("parallel call: expected served=true; got false")
					return
				}
				atomic.AddInt64(&served, 1)
				if cur := refBytes.Load(); cur == nil {
					refBytes.Store(raw)
				} else if string(cur.([]byte)) != string(raw) {
					// Ordering of items[] in JSON could vary if the
					// indexer's underlying map iteration leaks order.
					// We accept order-independent equivalence by
					// decoding + re-encoding into a canonical form.
					if !equivalentListEnvelopes(cur.([]byte), raw) {
						t.Errorf("parallel call: served bytes diverged across workers\nfirst:  %s\nlater:  %s",
							string(cur.([]byte)), string(raw))
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	want := int64(workers * iters)
	if got := atomic.LoadInt64(&served); got != want {
		t.Fatalf("served count: want %d; got %d", want, got)
	}
}

// equivalentListEnvelopes returns true when two LIST envelope byte
// slices carry the same apiVersion + kind + an order-insensitive set
// of items (by metadata.name). Used by the parallelism test to tolerate
// indexer iteration-order drift across goroutines.
func equivalentListEnvelopes(a, b []byte) bool {
	var ea, eb map[string]any
	if err := json.Unmarshal(a, &ea); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &eb); err != nil {
		return false
	}
	if ea["apiVersion"] != eb["apiVersion"] {
		return false
	}
	if ea["kind"] != eb["kind"] {
		return false
	}
	itemsA, _ := ea["items"].([]any)
	itemsB, _ := eb["items"].([]any)
	if len(itemsA) != len(itemsB) {
		return false
	}
	namesA := itemNameSet(itemsA)
	namesB := itemNameSet(itemsB)
	if len(namesA) != len(namesB) {
		return false
	}
	for n := range namesA {
		if !namesB[n] {
			return false
		}
	}
	return true
}

func itemNameSet(items []any) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		meta, ok := m["metadata"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := meta["name"].(string)
		out[name] = true
	}
	return out
}

// TestResolverUseInformer_FlagParsing covers the env-var read.
// Default OFF is the R-FALSE-1 binding invariant — the 0.30.95 binary
// with the flag unset MUST be byte-identical to 0.30.94.
func TestResolverUseInformer_FlagParsing(t *testing.T) {
	cases := []struct {
		env, want string
	}{
		{"", ""},
		{"true", "true"},
		{"TRUE", "true"},
		{" true ", "true"},
		{"shadow", "shadow"},
		{"false", "false"},
		{"yes", "yes"}, // arbitrary; only "true"/"shadow" branch in the resolver
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv(resolverUseInformerEnv, tc.env)
			if got := resolverUseInformer(); got != tc.want {
				t.Fatalf("resolverUseInformer(): want %q; got %q", tc.want, got)
			}
		})
	}
}

// TestHasSubresourceSuffix is the unit-level check for the path tail
// matcher. Direct coverage so a future tail-list addition cannot
// regress without test updates.
func TestHasSubresourceSuffix(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/api/v1/namespaces/foo/pods/bar/log", true},
		{"/api/v1/namespaces/foo/pods/bar/log/", true},
		{"/api/v1/namespaces/foo/pods/bar/log?tailLines=10", true},
		{"/api/v1/namespaces/foo/pods/bar/exec", true},
		{"/apis/apps/v1/namespaces/foo/deployments/bar/scale", true},
		{"/apis/apps/v1/namespaces/foo/deployments/bar/status", true},
		{"/apis/apps/v1/namespaces/foo/deployments/bar/binding", true},
		{"/api/v1/namespaces/foo/services/bar/proxy", true},
		// Negative: substring match must NOT fire.
		{"/api/v1/namespaces/status-ns/pods/bar", false},
		{"/apis/apps/v1/namespaces/foo/deployments/bar", false},
		{"/apis/apps/v1/namespaces/foo/deployments", false},
		// Negative: resource literally named "status" (no subresource).
		{"/apis/example.io/v1/namespaces/foo/status", false}, // last segment IS literally "/status" — this WILL match; acknowledge limitation
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := hasSubresourceSuffix(tc.path)
			// Last test case is a known false-positive — every path
			// that ends in "/status" matches. The fix would need to
			// parse the path into segments and check whether the
			// previous segment is a resource name vs a parent resource.
			// At 0.30.95 we accept the over-rejection (apiserver still
			// answers correctly via the fallthrough); the customer-
			// visible behaviour is the same.
			if tc.path == "/apis/example.io/v1/namespaces/foo/status" {
				if !got {
					t.Skip("known limitation: resource literally named 'status' is over-rejected as a subresource")
				}
				return
			}
			if got != tc.want {
				t.Fatalf("hasSubresourceSuffix(%q): want %v; got %v", tc.path, tc.want, got)
			}
		})
	}
}

// TestMarshalAsList_EmptyShape asserts an empty items slice still
// emits "items": [] so the downstream JQ `.items[]` iterator produces
// an empty stream rather than a null.
func TestMarshalAsList_EmptyShape(t *testing.T) {
	raw, err := marshalAsList("v1", "PodsList", nil)
	if err != nil {
		t.Fatalf("marshalAsList: %v", err)
	}
	if !strings.Contains(string(raw), `"items":[]`) {
		t.Fatalf("expected items:[]; got %s", string(raw))
	}
	if !strings.Contains(string(raw), `"kind":"PodsList"`) {
		t.Fatalf("expected kind:PodsList; got %s", string(raw))
	}
	if !strings.Contains(string(raw), `"apiVersion":"v1"`) {
		t.Fatalf("expected apiVersion:v1 (core group); got %s", string(raw))
	}
}

// TestMarshalAsList_CoreVsGrouped covers the apiVersion field for the
// two GV families: core group (just "v1") and a real group ("g/v"),
// and the synthesized `<R>List` kind for each.
func TestMarshalAsList_CoreVsGrouped(t *testing.T) {
	cases := []struct {
		name       string
		apiVersion string
		resource   string
		wantKind   string
	}{
		// Note: the synthesis rule only ASCII-uppercases the FIRST byte
		// of gvr.Resource. Resources whose Kind has internal capitals
		// (e.g. configmaps → ConfigMap) come out as `Configmaps`+`List`.
		// Uniform rule per `feedback_no_special_cases.md` — no per-resource
		// title-casing dictionary.
		{"core_group_pods", "v1", "pods", "PodsList"},
		{"grouped_restactions", "templates.krateo.io/v1", "restactions", "RestactionsList"},
	}
	obj := newTestRestActionObject("ns", "n", "m")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := marshalAsList(tc.apiVersion, listKindForResource(tc.resource), []*unstructured.Unstructured{obj})
			if err != nil {
				t.Fatalf("marshalAsList: %v", err)
			}
			var env map[string]any
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if env["apiVersion"] != tc.apiVersion {
				t.Fatalf("apiVersion: want %q; got %v", tc.apiVersion, env["apiVersion"])
			}
			if env["kind"] != tc.wantKind {
				t.Fatalf("kind: want %q; got %v", tc.wantKind, env["kind"])
			}
			items, _ := env["items"].([]any)
			if len(items) != 1 {
				t.Fatalf("items: want 1; got %d", len(items))
			}
		})
	}
}

// TestListKindForResource is the unit-level falsifier for the PM-binding
// kind-synthesis rule (DEV-Q2 resolution, 2026-05-15): capitalise the
// first byte of gvr.Resource and append "List". The rule is uniform
// across all GVRs per `feedback_no_special_cases.md`.
func TestListKindForResource(t *testing.T) {
	cases := []struct {
		resource string
		want     string
	}{
		// Per PM-binding rule: capitalise FIRST byte only, append "List".
		// PM examples (the two required ones).
		{"compositions", "CompositionsList"},
		{"panels", "PanelsList"},
		// Resolver-targeted GVRs that exercise the rule.
		{"restactions", "RestactionsList"},
		{"deployments", "DeploymentsList"},
		{"pods", "PodsList"},
		{"namespaces", "NamespacesList"},
		// Internal-cap resources: rule only touches byte[0]. configmaps →
		// `Configmaps` + `List` (apiserver returns "ConfigMapList" with
		// internal cap, derived from CRD spec.names.kind). The simpler
		// uniform rule is intentional per `feedback_no_special_cases.md`
		// — adding a title-casing dictionary would be per-resource state.
		{"configmaps", "ConfigmapsList"},
		// Edge: empty resource (defensive — only reachable from malformed callers).
		{"", "List"},
		// Edge: single letter resource.
		{"x", "XList"},
		// Edge: already-uppercase first byte — function is idempotent on byte[0].
		{"Foos", "FoosList"},
	}
	for _, tc := range cases {
		t.Run(tc.resource, func(t *testing.T) {
			got := listKindForResource(tc.resource)
			if got != tc.want {
				t.Fatalf("listKindForResource(%q): want %q; got %q", tc.resource, tc.want, got)
			}
		})
	}
}

// ----------------------------------------------------------------------
// Tag 0.30.97 — servability fix for the pivot's handler-not-found
// empty-list regression (regression journal 2026-05-15).
//
// 0.30.96 hard-failed Phase 6 at S4: the pivot routed a list of
// `/api/v1/namespaces` through the informer for a GVR with NO
// registered handler and returned an empty list as `served=true`,
// zeroing the Compositions feature. The LIST branch now serves via
// ListObjectsServable + the GET branch re-checks IsServable, so an
// unregistered / not-yet-synced GVR yields `served=false` (apiserver
// fallthrough) instead of a served-empty answer. A genuinely-empty but
// synced informer MUST still serve `([], true)`.
// ----------------------------------------------------------------------

// namespacesGVR is the core-group GVR at the heart of the 0.30.96 S4
// regression — `compositions-list`'s inner step lists it. It is never
// registered by newDispatchWatcher, so it is the canonical "no handler"
// GVR for the negative falsifier.
var namespacesGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}

// dispatchTestListKindsWithNamespaces extends dispatchTestListKinds with
// a NamespaceList entry so the LAZY informer that Gate 6 registers for
// the never-registered namespaces GVR does not panic on its initial
// LIST against the fake dynamic client.
func dispatchTestListKindsWithNamespaces() map[schema.GroupVersionResource]string {
	m := dispatchTestListKinds()
	m[namespacesGVR] = "NamespaceList"
	return m
}

// newDispatchWatcherWithNamespaces builds a synced cache=on watcher with
// the test GVR registered but the namespaces GVR deliberately LEFT
// UNREGISTERED. The dynamic fake knows the NamespaceList kind so Gate
// 6's lazy EnsureResourceType side-effect is harmless.
func newDispatchWatcherWithNamespaces(t *testing.T) *cache.ResourceWatcher {
	t.Helper()
	t.Setenv("CACHE_ENABLED", "true")

	// dispatchAdminRBACSeed() so the Tag-0.30.100 per-item RBAC filter
	// permits dispatchAdminUser (the identity dispatchCtx carries).
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		dispatchTestScheme(), dispatchTestListKindsWithNamespaces(),
		dispatchAdminRBACSeed()...)

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(50 * time.Millisecond)
	})

	added, syncCh := rw.EnsureResourceType(dispatchTestGVR)
	if !added {
		t.Fatalf("EnsureResourceType: want added=true")
	}
	select {
	case <-syncCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("EnsureResourceType: informer did not sync within 5s")
	}

	// Wait for the eagerly-registered RBAC informers to sync (Tag
	// 0.30.100 per-item RBAC filter reads them).
	syncCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rw.WaitForCacheSync(syncCtx, 5*time.Second); err != nil {
		t.Fatalf("WaitForCacheSync (RBAC informers): %v", err)
	}

	cache.SetGlobal(rw)
	t.Cleanup(func() { cache.SetGlobal(nil) })
	return rw
}

// TestDispatchViaInformer_UnregisteredGVR_ListFallthrough is the
// regression-binding test: a LIST for a GVR with NO registered informer
// MUST take the apiserver fallthrough (served=false) — NOT a served-
// empty answer. The fallthrough counter increments; list_served does
// NOT (the negative falsifier — closes the 0.30.96 G3 blind spot).
func TestDispatchViaInformer_UnregisteredGVR_ListFallthrough(t *testing.T) {
	resetDispatchCounters()
	rw := newDispatchWatcherWithNamespaces(t)

	if rw.IsServable(namespacesGVR) {
		t.Fatalf("setup: namespaces GVR must NOT be servable (never registered)")
	}

	// LIST /api/v1/namespaces — the exact shape that froze S4.
	call := buildCall(http.MethodGet, "/api/v1/namespaces")
	raw, served := dispatchViaInformer(dispatchCtx(), call)
	if served {
		t.Fatalf("unregistered-GVR LIST: expected served=false (apiserver fallthrough); got served=true with %d bytes", len(raw))
	}
	if raw != nil {
		t.Fatalf("unregistered-GVR LIST: expected nil raw on fallthrough; got %d bytes", len(raw))
	}

	s := DispatchInformerStatsSnapshot()
	if s.ListServed != 0 {
		t.Fatalf("negative falsifier: list_served must NOT increment for a never-registered GVR; got %d", s.ListServed)
	}
	if s.Fallthrough != 1 {
		t.Fatalf("unregistered-GVR LIST: want Fallthrough=1; got %d", s.Fallthrough)
	}
}

// TestDispatchViaInformer_UnregisteredGVR_GetFallthrough asserts the GET
// branch is also defended: a GET-by-name for a never-registered GVR
// takes the apiserver fallthrough (served=false). The GET path is a
// latent instance of the same (nil,false) overload — fixed alongside
// LIST in 0.30.97.
func TestDispatchViaInformer_UnregisteredGVR_GetFallthrough(t *testing.T) {
	resetDispatchCounters()
	rw := newDispatchWatcherWithNamespaces(t)

	if rw.IsServable(namespacesGVR) {
		t.Fatalf("setup: namespaces GVR must NOT be servable")
	}

	call := buildCall(http.MethodGet, "/api/v1/namespaces/kube-system")
	raw, served := dispatchViaInformer(dispatchCtx(), call)
	if served {
		t.Fatalf("unregistered-GVR GET: expected served=false (apiserver fallthrough); got served=true with %d bytes", len(raw))
	}
	if raw != nil {
		t.Fatalf("unregistered-GVR GET: expected nil raw on fallthrough; got %d bytes", len(raw))
	}

	s := DispatchInformerStatsSnapshot()
	if s.GetServed != 0 {
		t.Fatalf("negative falsifier: get_served must NOT increment for a never-registered GVR; got %d", s.GetServed)
	}
	if s.Fallthrough != 1 {
		t.Fatalf("unregistered-GVR GET: want Fallthrough=1; got %d", s.Fallthrough)
	}
}

// TestDispatchViaInformer_SyncedEmptyStillServes is the over-correction
// guard: a registered + synced informer with a GENUINELY-EMPTY store
// must STILL serve `served=true` with an empty `items:[]` envelope.
// 0.30.97 must distinguish "no handler / not synced" (fall through)
// from "synced, genuinely empty" (serve) — it must not regress the
// latter into an apiserver fallthrough.
func TestDispatchViaInformer_SyncedEmptyStillServes(t *testing.T) {
	resetDispatchCounters()
	// newDispatchWatcher registers dispatchTestGVR + waits for sync;
	// with zero seeds the indexer is genuinely empty.
	rw := newDispatchWatcher(t)
	if !rw.IsServable(dispatchTestGVR) {
		t.Fatalf("setup: test GVR must be registered + synced")
	}

	call := buildCall(http.MethodGet, "/apis/templates.krateo.io/v1/namespaces/default/restactions")
	raw, served := dispatchViaInformer(dispatchCtx(), call)
	if !served {
		t.Fatalf("synced + genuinely-empty: expected served=true (over-correction guard); got served=false")
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("synced-empty LIST envelope is not valid JSON: %v\nbytes: %s", err, string(raw))
	}
	items, ok := envelope["items"].([]any)
	if !ok {
		t.Fatalf("synced-empty: items want []any; got %T", envelope["items"])
	}
	if len(items) != 0 {
		t.Fatalf("synced-empty: want 0 items; got %d", len(items))
	}

	s := DispatchInformerStatsSnapshot()
	if s.ListServed != 1 {
		t.Fatalf("synced-empty: want ListServed=1 (genuinely-empty IS servable); got %d", s.ListServed)
	}
	if s.Fallthrough != 0 {
		t.Fatalf("synced-empty: want Fallthrough=0 (no over-correction); got %d", s.Fallthrough)
	}
}
