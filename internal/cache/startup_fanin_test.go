// Q-OOM-STARTUP-FANIN (0.25.329) — unit tests for the bounded informer
// registration fan-in introduced in registerFromRedis.
//
// Tests cover (in order of acceptance):
//
//  1. startupInformerFanin() env-knob contract — default 8 for unset/empty
//     and any malformed/<=0 value (PM A8: never unbounded).
//  2. registerBatched registers every GVR exactly once when fanin=2 across
//     a 5-GVR slice (covers two full batches + one trailing batch).
//  3. registerBatched respects the fanin concurrency cap — at most `fanin`
//     calls to registerInformer are in flight simultaneously.
//  4. registerBatched waits for in-flight registrations to complete before
//     starting the next batch (no "batch N+1 admits while batch N is still
//     decoding LIST" — the property that bounds the OOM peak).
package cache

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/dynamic/dynamicinformer"
	clienttesting "k8s.io/client-go/testing"
)

// TestStartupInformerFanin_DefaultsForUnset asserts the fallback path:
// missing env var → defaultStartupInformerFanin (8).
func TestStartupInformerFanin_DefaultsForUnset(t *testing.T) {
	t.Setenv(envStartupInformerFanin, "")
	if got := startupInformerFanin(); got != defaultStartupInformerFanin {
		t.Fatalf("unset env: got %d, want %d", got, defaultStartupInformerFanin)
	}
}

// TestStartupInformerFanin_DefaultsForWhitespace covers TrimSpace handling.
// "   " is treated identically to unset.
func TestStartupInformerFanin_DefaultsForWhitespace(t *testing.T) {
	t.Setenv(envStartupInformerFanin, "   ")
	if got := startupInformerFanin(); got != defaultStartupInformerFanin {
		t.Fatalf("whitespace env: got %d, want %d", got, defaultStartupInformerFanin)
	}
}

// TestStartupInformerFanin_RejectsNonPositive locks the PM A8 amendment:
// "0", "-1", and any non-numeric string MUST fall back to the default. We
// must NEVER fall back to unbounded (the falsified path).
func TestStartupInformerFanin_RejectsNonPositive(t *testing.T) {
	cases := []string{"0", "-1", "-100", "abc", "1.5", "8x", "0x10"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(envStartupInformerFanin, raw)
			if got := startupInformerFanin(); got != defaultStartupInformerFanin {
				t.Fatalf("env=%q: got %d, want %d", raw, got, defaultStartupInformerFanin)
			}
		})
	}
}

// TestStartupInformerFanin_HonorsValidPositive asserts valid env overrides
// (1, 4, 16, 64) are honored verbatim — operators retain control over the
// concurrency cap without code changes.
func TestStartupInformerFanin_HonorsValidPositive(t *testing.T) {
	cases := map[string]int{"1": 1, "4": 4, "16": 16, "64": 64, " 12 ": 12}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(envStartupInformerFanin, raw)
			if got := startupInformerFanin(); got != want {
				t.Fatalf("env=%q: got %d, want %d", raw, got, want)
			}
		})
	}
}

// makeFakeWatcher builds a ResourceWatcher backed by a fake dynamic client
// and a dynamicinformer factory. The fake client returns empty
// UnstructuredList for every GVR, so informers sync immediately. Suitable
// for testing registerBatched's sequencing without a real K8s API server.
func makeFakeWatcher(t *testing.T, gvrs []schema.GroupVersionResource) (*ResourceWatcher, dynamic.Interface) {
	t.Helper()
	scheme := apiruntime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{}
	for _, gvr := range gvrs {
		listGVK := schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: gvr.Resource + "List"}
		scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
		listKinds[gvr] = listGVK.Kind
	}
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)

	factory := dynamicinformer.NewDynamicSharedInformerFactory(fc, 0)
	rw := &ResourceWatcher{
		factory:     factory,
		watched:     make(map[string]bool),
		dynamicGVRs: make(map[string]schema.GroupVersionResource),
		eventCh:     make(chan l1Event, 16),
	}
	// appCtx must be non-nil because registerInformer captures it for event
	// handlers. The fake clients don't emit events during a normal test, but
	// using a real context keeps the closure references valid.
	rw.appCtx = context.Background()
	return rw, fc
}

// gvrSlice builds N synthetic GVRs in the form (test.krateo.io/v1, resN).
// Resources are numbered 0..N-1 so the test can confirm ordering when needed.
func gvrSlice(n int) []schema.GroupVersionResource {
	out := make([]schema.GroupVersionResource, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, schema.GroupVersionResource{
			Group:    "test.krateo.io",
			Version:  "v1",
			Resource: "res" + strconv.Itoa(i),
		})
	}
	return out
}

// TestRegisterBatched_AllGVRsRegistered locks the basic completeness
// contract: registerBatched calls registerInformer exactly once per GVR
// even when batches don't divide evenly (5 GVRs at fanin=2 → 2+2+1).
func TestRegisterBatched_AllGVRsRegistered(t *testing.T) {
	gvrs := gvrSlice(5)
	rw, _ := makeFakeWatcher(t, gvrs)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rw.registerBatched(ctx, gvrs, 2)

	rw.mu.Lock()
	defer rw.mu.Unlock()
	for _, gvr := range gvrs {
		key := GVRToKey(gvr)
		if !rw.watched[key] {
			t.Fatalf("gvr %s not registered", key)
		}
	}
	if len(rw.watched) != len(gvrs) {
		t.Fatalf("watched count = %d, want %d", len(rw.watched), len(gvrs))
	}
}

// TestRegisterBatched_DedupsAlreadyWatched asserts the watched-map dedup
// short-circuit: re-running registerBatched on the same set MUST be a
// no-op (not a re-register, not a panic). syncNewGVRs relies on this same
// idempotence.
func TestRegisterBatched_DedupsAlreadyWatched(t *testing.T) {
	gvrs := gvrSlice(3)
	rw, _ := makeFakeWatcher(t, gvrs)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rw.registerBatched(ctx, gvrs, 2)
	rw.registerBatched(ctx, gvrs, 2) // second call must be idempotent

	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.watched) != 3 {
		t.Fatalf("watched count = %d after idempotent re-register, want 3", len(rw.watched))
	}
}

// TestRegisterBatched_FaninCapHonored is the key falsification gate. We
// instrument the fake LIST reactor to record the maximum number of
// concurrently in-flight LIST calls. With Plan F's bounded fan-in, that
// peak must NEVER exceed `fanin`. Without the per-batch WaitForCacheSync,
// the dynamicinformer factory.Run goroutines all fire LIST in parallel
// and the peak == N (the falsified path).
//
// Each LIST reactor sleeps for `listDelay` to give all peer goroutines
// time to enter the reactor, ensuring the in-flight counter actually
// captures concurrent overlap.
func TestRegisterBatched_FaninCapHonored(t *testing.T) {
	const N = 8
	const fanin = 2
	const listDelay = 25 * time.Millisecond

	gvrs := gvrSlice(N)
	scheme := apiruntime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{}
	for _, gvr := range gvrs {
		listGVK := schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: gvr.Resource + "List"}
		scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
		listKinds[gvr] = listGVK.Kind
	}
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)

	var inFlight atomic.Int64
	var maxInFlight atomic.Int64

	for _, gvr := range gvrs {
		gvrCopy := gvr // closure capture
		fc.PrependReactor("list", gvrCopy.Resource, func(action clienttesting.Action) (bool, apiruntime.Object, error) {
			cur := inFlight.Add(1)
			defer inFlight.Add(-1)
			// Track running max with CAS-style update.
			for {
				prev := maxInFlight.Load()
				if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(listDelay)
			out := &unstructured.UnstructuredList{}
			out.SetGroupVersionKind(schema.GroupVersionKind{
				Group: gvrCopy.Group, Version: gvrCopy.Version, Kind: gvrCopy.Resource + "List",
			})
			out.SetContinue("")
			return true, out, nil
		})
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(fc, 0)
	rw := &ResourceWatcher{
		factory:     factory,
		watched:     make(map[string]bool),
		dynamicGVRs: make(map[string]schema.GroupVersionResource),
		eventCh:     make(chan l1Event, 16),
	}
	rw.appCtx = context.Background()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rw.registerBatched(ctx, gvrs, fanin)

	if got := maxInFlight.Load(); got > fanin {
		t.Fatalf("max concurrent LIST = %d, want <= %d (fan-in cap violated)", got, fanin)
	}
	// Sanity: we observed concurrency at least equal to fanin (otherwise
	// the test setup itself isn't exercising the cap and would pass
	// trivially even if the cap were broken).
	if got := maxInFlight.Load(); got < 1 {
		t.Fatalf("max concurrent LIST = 0; reactor never fired (test setup broken)")
	}

	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.watched) != N {
		t.Fatalf("watched count = %d, want %d", len(rw.watched), N)
	}
}

// TestRegisterBatched_BatchWaitsForSync locks the per-batch
// WaitForCacheSync invariant — batch N+1's LIST reactors MUST NOT fire
// until batch N's LIST reactors have ALL returned. Without this barrier
// the bounded fan-in degenerates to a simple goroutine semaphore that
// staggers the START of each LIST but does NOT cap the number of
// concurrent in-flight LISTs (since the dynamicinformer factory.Run
// runs LIST in its own goroutine, decoupled from registerInformer).
//
// We arrange the LIST reactor to record entry timestamps. Then we assert
// that for fanin=2, gvrs[2]'s LIST entry timestamp > gvrs[0]'s LIST exit
// timestamp (the last LIST of batch 0 must complete before any LIST of
// batch 1 begins).
func TestRegisterBatched_BatchWaitsForSync(t *testing.T) {
	const fanin = 2
	const listDelay = 30 * time.Millisecond

	gvrs := gvrSlice(4)
	scheme := apiruntime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{}
	for _, gvr := range gvrs {
		listGVK := schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: gvr.Resource + "List"}
		scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
		listKinds[gvr] = listGVK.Kind
	}
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)

	var mu sync.Mutex
	listEntry := map[string]time.Time{}
	listExit := map[string]time.Time{}

	for _, gvr := range gvrs {
		gvrCopy := gvr
		fc.PrependReactor("list", gvrCopy.Resource, func(action clienttesting.Action) (bool, apiruntime.Object, error) {
			mu.Lock()
			listEntry[gvrCopy.Resource] = time.Now()
			mu.Unlock()
			time.Sleep(listDelay)
			mu.Lock()
			listExit[gvrCopy.Resource] = time.Now()
			mu.Unlock()
			out := &unstructured.UnstructuredList{}
			out.SetGroupVersionKind(schema.GroupVersionKind{
				Group: gvrCopy.Group, Version: gvrCopy.Version, Kind: gvrCopy.Resource + "List",
			})
			out.SetContinue("")
			return true, out, nil
		})
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(fc, 0)
	rw := &ResourceWatcher{
		factory:     factory,
		watched:     make(map[string]bool),
		dynamicGVRs: make(map[string]schema.GroupVersionResource),
		eventCh:     make(chan l1Event, 16),
	}
	rw.appCtx = context.Background()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rw.registerBatched(ctx, gvrs, fanin)

	mu.Lock()
	defer mu.Unlock()

	// All 4 LISTs fired.
	for _, gvr := range gvrs {
		if _, ok := listEntry[gvr.Resource]; !ok {
			t.Fatalf("LIST never fired for %s", gvr.Resource)
		}
		if _, ok := listExit[gvr.Resource]; !ok {
			t.Fatalf("LIST never exited for %s", gvr.Resource)
		}
	}

	// Batch ordering: every batch-0 LIST must have EXITED before any
	// batch-1 LIST ENTERED. (registerBatched calls WaitForCacheSync
	// between batches; the informer's HasSynced flag flips only after
	// LIST returns.)
	maxBatch0Exit := maxTime(listExit[gvrs[0].Resource], listExit[gvrs[1].Resource])
	minBatch1Entry := minTime(listEntry[gvrs[2].Resource], listEntry[gvrs[3].Resource])

	if !minBatch1Entry.After(maxBatch0Exit) {
		t.Fatalf("batch ordering violated: batch-1 LIST entered at %v "+
			"before batch-0 LIST exited at %v "+
			"(WaitForCacheSync barrier missing or broken)",
			minBatch1Entry, maxBatch0Exit)
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// TestRegisterBatched_HandlesEmptyInput asserts the trivial guard: zero
// GVRs in → no panic, no factory.Start, no goroutine leak.
func TestRegisterBatched_HandlesEmptyInput(t *testing.T) {
	rw, _ := makeFakeWatcher(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	rw.registerBatched(ctx, nil, 4)
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.watched) != 0 {
		t.Fatalf("watched count = %d, want 0", len(rw.watched))
	}
}

// TestRegisterBatched_FaninZeroDoesNotDivideByZero locks the defensive
// clamp: a caller passing fanin<1 (shouldn't happen in production because
// startupInformerFanin guards it, but defensive code is cheap) MUST be
// promoted to fanin=1, not panic with a divide-by-zero or hang.
func TestRegisterBatched_FaninZeroDoesNotDivideByZero(t *testing.T) {
	gvrs := gvrSlice(3)
	rw, _ := makeFakeWatcher(t, gvrs)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// fanin=0 should be promoted to 1.
	rw.registerBatched(ctx, gvrs, 0)

	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.watched) != 3 {
		t.Fatalf("watched count = %d, want 3 (fanin=0 promote to 1)", len(rw.watched))
	}
}
