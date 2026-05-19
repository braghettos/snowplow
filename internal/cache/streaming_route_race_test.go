// streaming_route_race_test.go — Ship 0.30.133 hermetic acceptance
// tests for the duplicate-informer registration-ordering race fix.
//
// THE BUG: addResourceTypeLocked's streaming-routing check used to be
// nested inside `if standalone { ... }`, and `standalone :=
// matchesAutoDiscoverGroup(gvr.Group)` is FALSE until Phase 1's
// navigation walk has called AddAutoDiscoverGroup. A composition
// EnsureResourceType landing BEFORE that walk fell to the factory
// branch — the stock shared-factory informer (the ~80%-of-heap
// NewFilteredDynamicInformer.func3 driver). The composition GVR's
// representation depended on registration timing — a race.
//
// THE FIX: the streaming-routing check is hoisted OUT of `if standalone`
// and evaluated first. matchesStreamingListGroup derives from
// bytesResourceOverrides (a write-once init-time set) — timing-
// independent — so the composition GVR routes to the streaming informer
// on its first registration regardless of Phase-1 state.
//
// Coverage maps to the PM-gate acceptance criteria:
//
//   - TestRouteRace_AC1_StreamingBeforePhase1     -> AC-1
//   - TestRouteRace_AC2_NoStockInformerEitherOrder-> AC-2
//   - TestRouteRace_AC3_NonStreamingRoutingUnchanged -> AC-3
//   - TestRouteRace_AC4_StreamingInformerRemovable-> AC-4
//   - TestRouteRace_AC5_CacheEnabledFallback      -> AC-5
package cache

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// routeRaceCompositionGVR is the composition-group GVR exercised by the
// race-fix ACs. Its group is in bytesResourceOverrides, so it is a
// streaming-list group; its resource segment is a representative
// dynamically-named composition CRD.
var routeRaceCompositionGVR = schema.GroupVersionResource{
	Group:    "composition.krateo.io",
	Version:  "v1",
	Resource: "githubscaffoldingwithcompositionpages",
}

// newRouteRaceWatcher builds a CACHE_ENABLED ResourceWatcher with a
// REST config wired (so newStreamingDynamicInformer can build its REST
// client — the informer is constructed, never Run, in these tests, so a
// stub host is sufficient). The fake dynamic client is seeded for the
// composition GVR and the synthetic GVRs.
func newRouteRaceWatcher(t *testing.T, withRESTConfig bool, gvrs ...schema.GroupVersionResource) *ResourceWatcher {
	t.Helper()
	rw := newSyntheticRemoveWatcher(t, gvrs...)
	if withRESTConfig {
		// A stub host — streamingRESTClient only BUILDS the REST client;
		// it does not connect until a LIST/WATCH is issued, and these
		// tests never Run the informer.
		rw.SetRESTConfig(&rest.Config{Host: "https://stub.invalid"})
	}
	return rw
}

// isStreamingInformer reports whether rw.informers[gvr] is a
// *streamingDynamicInformer (the H2a/R4 streaming-list informer).
func isStreamingInformer(rw *ResourceWatcher, gvr schema.GroupVersionResource) bool {
	rw.mu.RLock()
	defer rw.mu.RUnlock()
	gi, ok := rw.informers[gvr]
	if !ok {
		return false
	}
	_, streaming := gi.(*streamingDynamicInformer)
	return streaming
}

// TestRouteRace_AC1_StreamingBeforePhase1 — AC-1.
//
// A composition GVR registered BEFORE Phase 1's walk has run (i.e. with
// autoDiscoverGroups empty) must route to the streaming informer. This
// is the exact race the fix closes: pre-fix, the streaming check was
// gated behind `standalone == matchesAutoDiscoverGroup(...)`, which is
// false here, so the GVR fell to the stock factory informer.
func TestRouteRace_AC1_StreamingBeforePhase1(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv(envCompositionStreamingList, "true")
	ResetDepsForTest()
	t.Cleanup(ResetDepsForTest)
	ResetAutoDiscoverGroupsForTest()
	t.Cleanup(ResetAutoDiscoverGroupsForTest)

	// autoDiscoverGroups is empty — Phase 1 has NOT run. This is the
	// pre-Phase-1 registration order.
	if matchesAutoDiscoverGroup(routeRaceCompositionGVR.Group) {
		t.Fatal("precondition: autoDiscoverGroups must be empty for this test")
	}

	rw := newRouteRaceWatcher(t, true, routeRaceCompositionGVR)
	t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })

	rw.EnsureResourceType(routeRaceCompositionGVR)

	if !isStreamingInformer(rw, routeRaceCompositionGVR) {
		t.Fatal("AC-1 FAIL: composition GVR registered before Phase 1 did NOT route to " +
			"the streaming informer — the registration-ordering race is not fixed")
	}
}

// TestRouteRace_AC2_NoStockInformerEitherOrder — AC-2.
//
// The composition GVR must route to the streaming informer in BOTH
// registration orders — autoDiscoverGroups empty AND populated — and
// must reach NEITHER stock-informer constructor: not the factory
// (rw.factory.ForResource) and not the in-standalone fallback
// (dynamicinformer.NewFilteredDynamicInformer). The structural proof is
// that rw.informers[gvr] is a *streamingDynamicInformer both ways (a
// stock informer is never a *streamingDynamicInformer).
func TestRouteRace_AC2_NoStockInformerEitherOrder(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv(envCompositionStreamingList, "true")

	t.Run("autoDiscoverGroups empty (pre-Phase-1)", func(t *testing.T) {
		ResetDepsForTest()
		t.Cleanup(ResetDepsForTest)
		ResetAutoDiscoverGroupsForTest()
		t.Cleanup(ResetAutoDiscoverGroupsForTest)

		rw := newRouteRaceWatcher(t, true, routeRaceCompositionGVR)
		t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
		rw.EnsureResourceType(routeRaceCompositionGVR)

		if !isStreamingInformer(rw, routeRaceCompositionGVR) {
			t.Fatal("AC-2 FAIL: pre-Phase-1 composition GVR reached a stock informer")
		}
	})

	t.Run("autoDiscoverGroups populated (post-Phase-1)", func(t *testing.T) {
		ResetDepsForTest()
		t.Cleanup(ResetDepsForTest)
		ResetAutoDiscoverGroupsForTest()
		t.Cleanup(ResetAutoDiscoverGroupsForTest)

		// Simulate Phase 1 having run: the composition group is now in
		// the auto-discover set (standalone would be true).
		AddAutoDiscoverGroup(routeRaceCompositionGVR.Group)

		rw := newRouteRaceWatcher(t, true, routeRaceCompositionGVR)
		t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
		rw.EnsureResourceType(routeRaceCompositionGVR)

		if !isStreamingInformer(rw, routeRaceCompositionGVR) {
			t.Fatal("AC-2 FAIL: post-Phase-1 composition GVR reached a stock informer")
		}
	})
}

// TestRouteRace_AC3_NonStreamingRoutingUnchanged — AC-3.
//
// For non-composition GVRs the standalone/factory decision must be
// byte-identical to pre-fix:
//   - a non-composition group in autoDiscoverGroups -> standalone
//     informer (NewFilteredDynamicInformer), NOT streaming, NOT factory.
//   - a non-composition group NOT in autoDiscoverGroups -> factory
//     informer (rw.factory.ForResource), NOT streaming.
func TestRouteRace_AC3_NonStreamingRoutingUnchanged(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv(envCompositionStreamingList, "true")

	standaloneGVR := schema.GroupVersionResource{
		Group: "synthetic-ac3-standalone.krateo.io", Version: "v1", Resource: "things",
	}
	factoryGVR := schema.GroupVersionResource{
		Group: "synthetic-ac3-factory.krateo.io", Version: "v1", Resource: "things",
	}

	t.Run("non-composition standalone GVR", func(t *testing.T) {
		ResetDepsForTest()
		t.Cleanup(ResetDepsForTest)
		ResetAutoDiscoverGroupsForTest()
		t.Cleanup(ResetAutoDiscoverGroupsForTest)
		AddAutoDiscoverGroup(standaloneGVR.Group)

		rw := newRouteRaceWatcher(t, true, standaloneGVR)
		t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
		rw.EnsureResourceType(standaloneGVR)

		if isStreamingInformer(rw, standaloneGVR) {
			t.Fatal("AC-3 FAIL: a non-composition GVR routed to the streaming informer — " +
				"streaming routing must be scoped to bytesResourceOverrides groups only")
		}
		rw.mu.RLock()
		_, registered := rw.informers[standaloneGVR]
		rw.mu.RUnlock()
		if !registered {
			t.Fatal("AC-3 FAIL: non-composition standalone GVR not registered at all")
		}
	})

	t.Run("non-composition factory GVR", func(t *testing.T) {
		ResetDepsForTest()
		t.Cleanup(ResetDepsForTest)
		ResetAutoDiscoverGroupsForTest()
		t.Cleanup(ResetAutoDiscoverGroupsForTest)
		// factoryGVR's group is NOT auto-discovered -> factory branch.

		rw := newRouteRaceWatcher(t, true, factoryGVR)
		t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
		rw.EnsureResourceType(factoryGVR)

		if isStreamingInformer(rw, factoryGVR) {
			t.Fatal("AC-3 FAIL: a non-composition non-standalone GVR routed to streaming")
		}
		rw.mu.RLock()
		_, registered := rw.informers[factoryGVR]
		rw.mu.RUnlock()
		if !registered {
			t.Fatal("AC-3 FAIL: non-composition factory GVR not registered")
		}
	})
}

// TestRouteRace_AC4_StreamingInformerRemovable — AC-4 (R6 removability).
//
// A composition GVR registered via the hoisted streaming path must stay
// removable: RemoveResourceType tears it down, and a subsequent
// EnsureResourceType constructs a FRESH streaming informer (a distinct
// instance — not a resurrected stopped one). This confirms the reorder
// did not move the streaming construction past perGVRStopLocked: the
// per-GVR stop channel is still allocated after rw.informers[gvr] = gi.
func TestRouteRace_AC4_StreamingInformerRemovable(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv(envCompositionStreamingList, "true")
	ResetDepsForTest()
	t.Cleanup(ResetDepsForTest)
	ResetAutoDiscoverGroupsForTest()
	t.Cleanup(ResetAutoDiscoverGroupsForTest)

	rw := newRouteRaceWatcher(t, true, routeRaceCompositionGVR)
	t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })

	// First registration — pre-Phase-1 (the race-prone order). Must be
	// the streaming informer.
	rw.EnsureResourceType(routeRaceCompositionGVR)
	if !isStreamingInformer(rw, routeRaceCompositionGVR) {
		t.Fatal("AC-4 precondition FAIL: first registration did not route to streaming")
	}
	rw.mu.RLock()
	inf1 := rw.informers[routeRaceCompositionGVR].Informer()
	_, stop1Allocated := rw.informerStop[routeRaceCompositionGVR]
	rw.mu.RUnlock()
	if !stop1Allocated {
		t.Fatal("AC-4 FAIL: per-GVR stop channel not allocated for the streaming informer — " +
			"the reorder moved streaming construction past perGVRStopLocked")
	}

	// Tear it down.
	rw.RemoveResourceType(routeRaceCompositionGVR)
	rw.mu.RLock()
	_, stillRegistered := rw.informers[routeRaceCompositionGVR]
	_, stopStillPresent := rw.informerStop[routeRaceCompositionGVR]
	rw.mu.RUnlock()
	if stillRegistered {
		t.Fatal("AC-4 FAIL: RemoveResourceType did not purge the streaming informer")
	}
	if stopStillPresent {
		t.Fatal("AC-4 FAIL: RemoveResourceType did not purge the per-GVR stop channel")
	}

	// Re-register — must construct a FRESH streaming informer.
	rw.EnsureResourceType(routeRaceCompositionGVR)
	if !isStreamingInformer(rw, routeRaceCompositionGVR) {
		t.Fatal("AC-4 FAIL: re-registration did not route to a streaming informer")
	}
	rw.mu.RLock()
	inf2 := rw.informers[routeRaceCompositionGVR].Informer()
	rw.mu.RUnlock()
	if inf1 == inf2 {
		t.Fatal("AC-4 FAIL: re-added streaming informer is the SAME instance as the " +
			"torn-down one — a frozen/stopped informer was resurrected")
	}
}

// TestRouteRace_AC5_CacheEnabledFallback — AC-5.
//
// The `if gi == nil` fallback to the stock informer must still be
// reachable after the hoist. With the streaming path disabled — either
// RESOLVER_COMPOSITION_STREAMING_LIST=false OR no *rest.Config wired
// (newStreamingDynamicInformer returns ok=false) — the composition GVR
// must fall back to a stock informer, NOT a *streamingDynamicInformer.
// This proves the CACHE_ENABLED / AC-7 toggle is intact post-reorder.
func TestRouteRace_AC5_CacheEnabledFallback(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	t.Run("streaming flag off", func(t *testing.T) {
		t.Setenv(envCompositionStreamingList, "false")
		ResetDepsForTest()
		t.Cleanup(ResetDepsForTest)
		ResetAutoDiscoverGroupsForTest()
		t.Cleanup(ResetAutoDiscoverGroupsForTest)

		// REST config IS wired — proving the fallback is driven by the
		// flag, not by a missing config.
		rw := newRouteRaceWatcher(t, true, routeRaceCompositionGVR)
		t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
		rw.EnsureResourceType(routeRaceCompositionGVR)

		if isStreamingInformer(rw, routeRaceCompositionGVR) {
			t.Fatal("AC-5 FAIL: RESOLVER_COMPOSITION_STREAMING_LIST=false but the " +
				"composition GVR still got a streaming informer — toggle broken")
		}
		rw.mu.RLock()
		_, registered := rw.informers[routeRaceCompositionGVR]
		rw.mu.RUnlock()
		if !registered {
			t.Fatal("AC-5 FAIL: streaming-off composition GVR not registered at all — " +
				"the stock fallback (if gi == nil) is unreachable post-hoist")
		}
	})

	t.Run("no rest.Config wired", func(t *testing.T) {
		t.Setenv(envCompositionStreamingList, "true")
		ResetDepsForTest()
		t.Cleanup(ResetDepsForTest)
		ResetAutoDiscoverGroupsForTest()
		t.Cleanup(ResetAutoDiscoverGroupsForTest)

		// NO REST config — newStreamingDynamicInformer returns ok=false,
		// so gi stays nil and the stock fallback must run.
		rw := newRouteRaceWatcher(t, false, routeRaceCompositionGVR)
		t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
		rw.EnsureResourceType(routeRaceCompositionGVR)

		if isStreamingInformer(rw, routeRaceCompositionGVR) {
			t.Fatal("AC-5 FAIL: no *rest.Config wired but the composition GVR still got " +
				"a streaming informer")
		}
		rw.mu.RLock()
		_, registered := rw.informers[routeRaceCompositionGVR]
		rw.mu.RUnlock()
		if !registered {
			t.Fatal("AC-5 FAIL: no-rest-config composition GVR not registered — the " +
				"stock fallback (if gi == nil) is unreachable post-hoist")
		}
	})
}
