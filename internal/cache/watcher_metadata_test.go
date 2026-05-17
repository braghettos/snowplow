// watcher_metadata_test.go — §0.30.93 (Revision 18) metadata-only
// routing tests for the ResourceWatcher EnsureResourceType path.
//
// These tests assert:
//
//   * TestEnsureResourceTypeMetadataOnly_DepTrackerFires
//     The metadata-only informer wired by EnsureResourceTypeMetadataOnly
//     correctly forwards DELETE events to cache.Deps().OnDelete with the
//     (gvr, ns, name) tuple extracted from *metav1.PartialObjectMetadata.
//     This is the binding correctness property per
//     `feedback_l1_invalidation_delete_only.md` — Option D viability
//     rests entirely on this preservation.
//
//   * TestEnsureResourceType_RoutesCompositionToMetadataOnly
//     The predicate-driven dispatch in EnsureResourceType picks the
//     metadata-only path for a composition.krateo.io GVR when the
//     metadata client is wired.
//
//   * TestEnsureResourceType_RoutesNonSeedToFullInformer
//     The same dispatcher picks the full-informer path for a non-seed
//     non-annotated GVR.
//
//   * TestEnsureResourceTypeMetadataOnly_NoClientLogsAndNoOps
//     Without SetMetadataClient, the explicit metadata-only call
//     returns (false, nil) and does NOT register an informer (caller
//     should fall back via EnsureResourceType).
//
// Per `feedback_no_special_cases.md`: tests verify that the public
// signature `EnsureResourceType(gvr) (bool, <-chan struct{})` is the
// same for both routing paths — callers do not see the discriminator.

package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
)

// compositionGVR is a representative member of the Krateo Composition
// family — the static seed routes it to metadata-only.
var compositionGVR = schema.GroupVersionResource{
	Group:    "composition.krateo.io",
	Version:  "v1-2-2",
	Resource: "githubscaffoldingwithcompositionpages",
}

// nonSeedGVR is a representative customer GVR that should take the
// full-informer path (default).
var nonSeedGVR = schema.GroupVersionResource{
	Group:    "templates.krateo.io",
	Version:  "v1",
	Resource: "restactions",
}

// newMetaScheme prepares a runtime.Scheme suitable for the metadata
// fake client. The fake registers a synthetic List kind itself; we
// just need a base scheme. metav1.AddMetaToScheme installs the
// PartialObjectMetadata typing so the informer's decode path works.
func newMetaScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = metav1.AddMetaToScheme(s)
	return s
}

// newPartialMetadata constructs a *metav1.PartialObjectMetadata at the
// shape the metadata client returns from LIST. Used by tests that need
// to seed the fake client.
func newPartialMetadata(apiVersion, kind, ns, name string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiVersion,
			Kind:       kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		},
	}
}

// TestEnsureResourceTypeMetadataOnly_DepTrackerFires is the binding
// correctness check for Option D. A DELETE on a PartialObjectMetadata
// object passing through the metadata-only informer MUST trigger the
// DepTracker's OnDelete with the correct (gvr, ns, name) tuple.
//
// We seed the fake metadata client with one object, register the
// metadata-only informer, install a probe DepTracker record so we can
// observe eviction, then issue Delete via the metadata client. The
// informer's processor goroutine fires DeleteFunc → metaNSName →
// Deps().OnDelete; we assert the dep edge was evicted within 3 s.
func TestEnsureResourceTypeMetadataOnly_DepTrackerFires(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_TTL_SECONDS", "3600")

	// Build the watcher with the dynamic fake (used only for RBAC
	// constructor-registered informers — none of those are
	// metadata-only).
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), rbacListKinds())
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher")
	}
	t.Cleanup(func() {
		rw.Stop()
		// Allow informer goroutines bounded by stopCh to exit BEFORE
		// the next test in the package samples runtime.NumGoroutine
		// (TestNewResourceWatcher_DormantWhenCacheDisabled). 100 ms
		// exceeds the 50 ms waitInformerSync poll tick + observed
		// metadata fake watch teardown latency.
		time.Sleep(100 * time.Millisecond)
	})

	// Wire the metadata client. Pre-seed with one composition object.
	scheme := newMetaScheme()
	seed := newPartialMetadata("composition.krateo.io/v1-2-2", "Githubscaffoldingwithcompositionpage", "bench-ns-01", "victim")
	metaCli := metadatafake.NewSimpleMetadataClient(scheme, seed)
	rw.SetMetadataClient(metaCli)

	// L1 cache must exist so DepTracker.OnDelete actually evicts.
	// Put a marker entry whose OWN dispatched object IS the victim — a
	// self-representation, so the R2/R7 (0.30.110) three-way OnDelete
	// classifies the DELETE as an eviction (not a dirty-mark). This
	// keeps the test exercising the metadata-only DeleteFunc → OnDelete
	// plumbing while conforming to the post-0.30.110 contract.
	rc := cache.ResolvedCache()
	if rc == nil {
		t.Fatalf("ResolvedCache nil — CACHE_ENABLED test setup broken")
	}
	const l1Key = "test:composition-evict"
	rc.Put(l1Key, &cache.ResolvedEntry{
		RawJSON: []byte(`{"items":["before"]}`),
		Inputs: &cache.ResolvedKeyInputs{
			CacheEntryClass: "restactions",
			Group:           compositionGVR.Group,
			Version:         compositionGVR.Version,
			Resource:        compositionGVR.Resource,
			Namespace:       "bench-ns-01",
			Name:            "victim",
		},
	})
	cache.Deps().Record(l1Key, compositionGVR, "bench-ns-01", "victim")

	// Trigger metadata-only registration. We use the explicit entry
	// point so the test asserts THIS path even if predicate behaviour
	// changes in a future refactor.
	added, sync := rw.EnsureResourceTypeMetadataOnly(compositionGVR)
	if !added {
		t.Fatalf("first EnsureResourceTypeMetadataOnly: want added=true; got false")
	}
	if sync == nil {
		t.Fatalf("sync channel must be non-nil")
	}
	if !rw.IsMetadataOnly(compositionGVR) {
		t.Fatalf("IsMetadataOnly(%v) = false; want true", compositionGVR)
	}
	// Wait for the informer to sync the seeded object.
	select {
	case <-sync:
	case <-time.After(3 * time.Second):
		t.Fatalf("metadata informer did not sync within 3s")
	}

	// Sanity: L1 entry is present before delete.
	if _, ok := rc.Get(l1Key); !ok {
		t.Fatalf("pre-delete: L1 entry %s missing", l1Key)
	}

	// DELETE the object via the metadata client. The informer's
	// watch should pick it up and fire DeleteFunc → OnDelete →
	// resolvedCache.Delete.
	if err := metaCli.Resource(compositionGVR).Namespace("bench-ns-01").Delete(context.Background(), "victim", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("metadata DELETE: %v", err)
	}

	// Poll for L1 eviction. 3s budget — fake client's watch is in-
	// process and should propagate well within that.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := rc.Get(l1Key); !ok {
			// Evicted. Pass.
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("DepTracker.OnDelete did not evict L1 entry %s within 3s after metadata-only DELETE", l1Key)
}

// TestEnsureResourceType_RoutesCompositionToFullInformerByDefault asserts
// the inverse of the prior 0.30.93 behavior: with the static seed now
// empty (per 2026-05-15 directive), composition.krateo.io GVRs default
// to the full Unstructured informer. Metadata-only routing is opt-in
// via the `krateo.io/cache-mode: metadata` CRD annotation only.
func TestEnsureResourceType_RoutesCompositionToFullInformerByDefault(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), rbacListKinds())
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher")
	}
	t.Cleanup(func() {
		rw.Stop()
		time.Sleep(100 * time.Millisecond)
	})

	scheme := newMetaScheme()
	metaCli := metadatafake.NewSimpleMetadataClient(scheme)
	rw.SetMetadataClient(metaCli)

	added, _ := rw.EnsureResourceType(compositionGVR)
	if !added {
		t.Fatalf("first EnsureResourceType: want added=true; got false")
	}
	if rw.IsMetadataOnly(compositionGVR) {
		t.Fatalf("composition GVR MUST default to full Unstructured informer (seed empty, no annotation); IsMetadataOnly=true")
	}
}

// Annotation-driven metadata-only routing is covered by
// TestShouldUseMetadataOnly_AnnotationDiscovery in cache_mode_test.go
// (internal package, can manipulate annotatedGVRs directly). The
// watcher-level dispatch wiring is covered by
// TestEnsureResourceType_RoutesCompositionToFullInformerByDefault
// above (default path) and TestEnsureResourceType_RoutesNonSeedToFullInformer
// below.

// TestEnsureResourceType_RoutesNonSeedToFullInformer asserts that a
// non-Composition, non-annotated GVR takes the dynamic full-informer
// path (default).
func TestEnsureResourceType_RoutesNonSeedToFullInformer(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	listKinds := rbacListKinds()
	listKinds[nonSeedGVR] = "RestActionList"
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), listKinds)
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher")
	}
	t.Cleanup(func() {
		rw.Stop()
		// Allow informer goroutines bounded by stopCh to exit BEFORE
		// the next test in the package samples runtime.NumGoroutine
		// (TestNewResourceWatcher_DormantWhenCacheDisabled). 100 ms
		// exceeds the 50 ms waitInformerSync poll tick + observed
		// metadata fake watch teardown latency.
		time.Sleep(100 * time.Millisecond)
	})

	// Wire the metadata client too — the predicate should STILL pick
	// full-informer for a non-seed, non-annotated GVR.
	rw.SetMetadataClient(metadatafake.NewSimpleMetadataClient(newMetaScheme()))

	added, _ := rw.EnsureResourceType(nonSeedGVR)
	if !added {
		t.Fatalf("first EnsureResourceType: want added=true; got false")
	}
	if rw.IsMetadataOnly(nonSeedGVR) {
		t.Fatalf("non-seed GVR MUST take full-informer path; IsMetadataOnly=true (predicate regression)")
	}
}

// TestEnsureResourceTypeMetadataOnly_NoClientLogsAndNoOps asserts that
// calling the explicit metadata-only entry point WITHOUT
// SetMetadataClient is a no-op (returns (false, nil)). The watcher
// MUST NOT auto-fall-back to the full informer in this case — the
// caller asked for metadata-only and the absence of a client is a
// setup error worth surfacing (the WARN log).
func TestEnsureResourceTypeMetadataOnly_NoClientLogsAndNoOps(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), rbacListKinds())
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher")
	}
	t.Cleanup(func() {
		rw.Stop()
		// Allow informer goroutines bounded by stopCh to exit BEFORE
		// the next test in the package samples runtime.NumGoroutine
		// (TestNewResourceWatcher_DormantWhenCacheDisabled). 100 ms
		// exceeds the 50 ms waitInformerSync poll tick + observed
		// metadata fake watch teardown latency.
		time.Sleep(100 * time.Millisecond)
	})
	// NB: NO SetMetadataClient call.

	added, ch := rw.EnsureResourceTypeMetadataOnly(compositionGVR)
	if added {
		t.Fatalf("EnsureResourceTypeMetadataOnly without client: want added=false; got true")
	}
	if ch != nil {
		t.Fatalf("EnsureResourceTypeMetadataOnly without client: want nil channel; got %v", ch)
	}
	if rw.IsMetadataOnly(compositionGVR) {
		t.Fatalf("no informer should have been registered; IsMetadataOnly=true")
	}
}

// TestEnsureResourceType_FallsBackWhenMetaClientMissing asserts the
// predicate-routed path takes the full-informer fallback when the
// metadata client is missing — this is the OOM-risk path that
// `cache.lazy_register.metadata_only_unwired` flags via WARN. Tests
// the soft-fail semantics (no crash; observable in logs).
func TestEnsureResourceType_FallsBackWhenMetaClientMissing(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	// Add the composition list kind so the dynamic fake can serve
	// the (fallback) full informer's LIST.
	listKinds := rbacListKinds()
	listKinds[compositionGVR] = "GithubscaffoldingwithcompositionpageList"
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newTestScheme(), listKinds)
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher")
	}
	t.Cleanup(func() {
		rw.Stop()
		// Allow informer goroutines bounded by stopCh to exit BEFORE
		// the next test in the package samples runtime.NumGoroutine
		// (TestNewResourceWatcher_DormantWhenCacheDisabled). 100 ms
		// exceeds the 50 ms waitInformerSync poll tick + observed
		// metadata fake watch teardown latency.
		time.Sleep(100 * time.Millisecond)
	})
	// NB: NO SetMetadataClient call — the fallback branch must fire.

	added, _ := rw.EnsureResourceType(compositionGVR)
	if !added {
		t.Fatalf("EnsureResourceType: want added=true on miss; got false")
	}
	if rw.IsMetadataOnly(compositionGVR) {
		t.Fatalf("composition GVR with no metaClient MUST fall back to full informer; IsMetadataOnly=true")
	}
}
