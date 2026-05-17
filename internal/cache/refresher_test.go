// refresher_test.go — Ship C (0.30.112) unit tests for the L1
// resolved-output cache refresher, rebuilt on a client-go workqueue.
//
// Coverage:
//   - dirty-mark -> handler invoked exactly once;
//   - M rapid marks of one key coalesce to one processing (idempotent
//     workqueue dedup);
//   - a burst far past any buffer drops NOTHING (unbounded queue);
//   - handler error -> bounded exponential-backoff retry (NumRequeues
//     climbs, then Forget on success);
//   - handler error never evicts the entry (stale-while-revalidate);
//   - missing handler / legacy nil-Inputs entry -> skip, no panic;
//   - clean ShutDown on ctx-cancel — workers exit, no leak;
//   - concurrent enqueue is race-free (run under -race);
//   - dep tracker OnUpdate -> refresher path fires end to end.

package cache

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withCleanRefresher gives each test a fresh refresher singleton with
// the supplied environment. The third arg is retained for call-site
// compatibility with the Ship C falsifier file but is unused — the
// workqueue rebuild has no dedup-window knob. Returns the cleanup
// function the test MUST defer.
func withCleanRefresher(t *testing.T, parallelism, _ int) func() {
	t.Helper()
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv(envRefresherParallelism, strconv.Itoa(parallelism))
	resetResolvedCacheForTest()
	resetDepsForTest()
	resetRefresherForTest()
	return func() {
		resetRefresherForTest()
		resetDepsForTest()
		resetResolvedCacheForTest()
	}
}

// waitFor polls cond until true or the deadline; fails the test on timeout.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", d, what)
}

// --- AC-C1 — dirty-mark -> re-resolved exactly once -------------------------

func TestRefresher_HandlerInvokedOnEnqueue(t *testing.T) {
	cleanup := withCleanRefresher(t, 2, 0)
	defer cleanup()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{CacheEntryClass: "widgets", Username: "u"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{"x":1}`), Inputs: &inputs})

	var called atomic.Int32
	RegisterRefreshFunc("widgets", func(_ context.Context, k string, in ResolvedKeyInputs) error {
		if k != key {
			t.Errorf("handler got key %q want %q", k, key)
		}
		if in.CacheEntryClass != "widgets" {
			t.Errorf("handler got CacheEntryClass %q want widgets", in.CacheEntryClass)
		}
		called.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	enqueueRefreshForTest(key)
	waitFor(t, 2*time.Second, "handler invoked", func() bool { return called.Load() == 1 })
	if got := refresherSingleton().completedTotal.Load(); got != 1 {
		t.Errorf("completedTotal=%d want 1", got)
	}
}

// AC-C1 — M rapid marks of one key coalesce. The workqueue dedups: a key
// already queued is not re-queued; a key being processed and re-Added
// is processed at most once more.
func TestRefresher_RapidMarksCoalesce(t *testing.T) {
	cleanup := withCleanRefresher(t, 1, 0)
	defer cleanup()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{CacheEntryClass: "widgets", Name: "coalesce"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

	// Gate the single worker so all M marks land while the key sits
	// queued — the workqueue must coalesce them to ONE processing.
	gate := make(chan struct{})
	var calls atomic.Int32
	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		<-gate
		calls.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	// First mark gets pulled by the worker (now blocked on gate). The
	// next marks all coalesce onto the dirty bit while it processes.
	for i := 0; i < 20; i++ {
		enqueueRefreshForTest(key)
		time.Sleep(time.Millisecond)
	}
	close(gate)

	// At most 2 processings total: the one in-flight + one coalesced
	// re-add. Never 20.
	time.Sleep(300 * time.Millisecond)
	if got := calls.Load(); got > 2 {
		t.Fatalf("rapid marks did not coalesce: handler ran %d times (want <= 2)", got)
	}
	if got := calls.Load(); got < 1 {
		t.Fatalf("handler never ran")
	}
}

// --- AC-C3 — error -> bounded exponential-backoff retry ---------------------

func TestRefresher_ErrorRetriesThenForgets(t *testing.T) {
	cleanup := withCleanRefresher(t, 1, 0)
	defer cleanup()
	// Tight backoff so the test is fast.
	t.Setenv(envRefresherBaseDelayMS, "5")
	t.Setenv(envRefresherMaxDelayMS, "50")
	resetRefresherForTest()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{CacheEntryClass: "widgets", Name: "retry"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

	var attempts atomic.Int32
	const failFirst = 3
	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		if attempts.Add(1) <= failFirst {
			return errors.New("transient failure")
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	enqueueRefreshForTest(key)
	waitFor(t, 5*time.Second, "handler retried to success",
		func() bool { return attempts.Load() >= failFirst+1 })

	// Bounded: once it succeeds the key is Forgotten — no more firing.
	settled := attempts.Load()
	time.Sleep(300 * time.Millisecond)
	if attempts.Load() != settled {
		t.Fatalf("retry not bounded: attempts kept climbing %d -> %d after success",
			settled, attempts.Load())
	}
	r := refresherSingleton()
	if r.completedTotal.Load() != 1 {
		t.Errorf("completedTotal=%d want 1", r.completedTotal.Load())
	}
	if r.failedTotal.Load() != failFirst {
		t.Errorf("failedTotal=%d want %d", r.failedTotal.Load(), failFirst)
	}
}

// --- Ship 0.30.113 Part A — poison-pill bounded retry -----------------------

// TestRefresher_PoisonPillDroppedAfterCap asserts the Part A bound: a
// DETERMINISTIC failure (a handler that always errors) is re-enqueued at
// most maxRefreshRequeues times, then the key is Forgotten and DROPPED —
// the retry loop stops, droppedTotal ticks, and the entry stays in L1
// (TTL outer-net) rather than being resurrected or evicted.
func TestRefresher_PoisonPillDroppedAfterCap(t *testing.T) {
	cleanup := withCleanRefresher(t, 1, 0)
	defer cleanup()
	// Tight backoff so the cap is reached fast.
	t.Setenv(envRefresherBaseDelayMS, "5")
	t.Setenv(envRefresherMaxDelayMS, "20")
	resetRefresherForTest()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{CacheEntryClass: "widgets", Name: "poison"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

	var attempts atomic.Int32
	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		attempts.Add(1)
		return errors.New("deterministic failure — never succeeds")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	enqueueRefreshForTest(key)

	// The key is dropped once it has been AddRateLimited maxRefreshRequeues
	// times. attempts == initial processing + maxRefreshRequeues retries.
	waitFor(t, 5*time.Second, "key dropped after requeue cap",
		func() bool { return refresherSingleton().droppedTotal.Load() == 1 })

	// Bounded — attempts must STOP climbing once dropped (no unbounded spin).
	settled := attempts.Load()
	time.Sleep(400 * time.Millisecond)
	if got := attempts.Load(); got != settled {
		t.Fatalf("poison-pill NOT bounded: attempts kept climbing %d -> %d after drop",
			settled, got)
	}
	// Total attempts == 1 initial + maxRefreshRequeues retries.
	if int(settled) != 1+maxRefreshRequeues {
		t.Fatalf("attempts=%d; want %d (1 initial + %d capped retries)",
			settled, 1+maxRefreshRequeues, maxRefreshRequeues)
	}
	r := refresherSingleton()
	if r.failedTotal.Load() != uint64(1+maxRefreshRequeues) {
		t.Errorf("failedTotal=%d want %d", r.failedTotal.Load(), 1+maxRefreshRequeues)
	}
	if r.retriedTotal.Load() != uint64(maxRefreshRequeues) {
		t.Errorf("retriedTotal=%d want %d", r.retriedTotal.Load(), maxRefreshRequeues)
	}
	if r.droppedTotal.Load() != 1 {
		t.Errorf("droppedTotal=%d want 1", r.droppedTotal.Load())
	}
	if r.completedTotal.Load() != 0 {
		t.Errorf("completedTotal=%d want 0 (poison pill never completes)", r.completedTotal.Load())
	}
	// The entry must still be in L1 — dropped to TTL, NOT evicted.
	if _, ok := c.Get(key); !ok {
		t.Fatalf("poison-pill drop evicted the entry — must fall back to TTL, not be removed")
	}
}

// TestRefresher_TransientUnderCapStillSucceeds guards the Part A bound
// against over-reach: a transient failure that recovers WITHIN the
// requeue cap must still succeed normally and NOT be dropped.
func TestRefresher_TransientUnderCapStillSucceeds(t *testing.T) {
	cleanup := withCleanRefresher(t, 1, 0)
	defer cleanup()
	t.Setenv(envRefresherBaseDelayMS, "5")
	t.Setenv(envRefresherMaxDelayMS, "20")
	resetRefresherForTest()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{CacheEntryClass: "widgets", Name: "transient"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

	var attempts atomic.Int32
	// Fail maxRefreshRequeues-1 times (strictly under the cap), then OK.
	failTimes := int32(maxRefreshRequeues - 1)
	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		if attempts.Add(1) <= failTimes {
			return errors.New("transient failure")
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	enqueueRefreshForTest(key)
	waitFor(t, 5*time.Second, "transient failure recovered",
		func() bool { return refresherSingleton().completedTotal.Load() == 1 })

	r := refresherSingleton()
	if r.droppedTotal.Load() != 0 {
		t.Fatalf("droppedTotal=%d want 0 — a sub-cap transient failure must NOT be dropped",
			r.droppedTotal.Load())
	}
}

// AC-C3 — a failing refresh must NOT evict the entry.
func TestRefresher_HandlerErrorDoesNotEvict(t *testing.T) {
	cleanup := withCleanRefresher(t, 1, 0)
	defer cleanup()
	t.Setenv(envRefresherBaseDelayMS, "10")
	t.Setenv(envRefresherMaxDelayMS, "10000")
	resetRefresherForTest()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{CacheEntryClass: "widgets", Name: "noevict"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		return errors.New("permanent failure")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	enqueueRefreshForTest(key)
	waitFor(t, 2*time.Second, "at least one failed attempt",
		func() bool { return refresherSingleton().failedTotal.Load() >= 1 })

	if _, ok := c.Get(key); !ok {
		t.Fatalf("entry evicted after refresh failure — violates stale-while-revalidate")
	}
}

// --- AC-C5 — legacy nil-Inputs entry skips, no panic ------------------------

func TestRefresher_NilInputsEntrySkips(t *testing.T) {
	cleanup := withCleanRefresher(t, 1, 0)
	defer cleanup()

	c := ResolvedCache()
	// Synthesise a key + a legacy entry with nil Inputs under it.
	key := ComputeKey(ResolvedKeyInputs{CacheEntryClass: "widgets", Name: "legacy"})
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: nil})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	enqueueRefreshForTest(key)
	waitFor(t, 2*time.Second, "legacy entry skipped",
		func() bool { return refresherSingleton().skippedNoHandler.Load() == 1 })
	// No panic, entry untouched.
	if _, ok := c.Get(key); !ok {
		t.Fatalf("legacy nil-Inputs entry should be left for TTL, not removed")
	}
}

func TestRefresher_NoHandlerForKind(t *testing.T) {
	cleanup := withCleanRefresher(t, 1, 0)
	defer cleanup()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{CacheEntryClass: "unregistered"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	enqueueRefreshForTest(key)
	waitFor(t, 2*time.Second, "no-handler skip counted",
		func() bool { return refresherSingleton().skippedNoHandler.Load() == 1 })
}

// --- AC-C4 — clean ShutDown on ctx-cancel -----------------------------------

func TestRefresher_CleanShutdownOnCancel(t *testing.T) {
	cleanup := withCleanRefresher(t, 4, 0)
	defer cleanup()

	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	StartRefresher(ctx)

	// Cancel and assert every worker goroutine exits within the bound.
	cancel()
	done := make(chan struct{})
	go func() {
		refresherSingleton().workersWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		// clean exit — no leak
	case <-time.After(5 * time.Second):
		t.Fatalf("workers did not exit within 5s of ctx-cancel — goroutine leak")
	}
}

// --- Concurrency ------------------------------------------------------------

func TestRefresher_ConcurrentEnqueueRaceFree(t *testing.T) {
	cleanup := withCleanRefresher(t, 4, 0)
	defer cleanup()

	c := ResolvedCache()
	var seen atomic.Int64
	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		seen.Add(1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	const N = 200
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		inputs := ResolvedKeyInputs{CacheEntryClass: "widgets", Name: "n" + itoa(i)}
		key := ComputeKey(inputs)
		c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			enqueueRefreshForTest(k)
		}(key)
	}
	wg.Wait()

	// Every distinct key must be processed exactly once — none dropped.
	waitFor(t, 5*time.Second, "all N keys processed",
		func() bool { return seen.Load() >= N })
	if got := seen.Load(); got != N {
		t.Fatalf("processed %d distinct keys want %d", got, N)
	}
}

// --- AC-C2 — re-resolved entry reflects new object state --------------------

// TestRefresher_OnUpdateRefreshesContent drives the full chain — an
// OnUpdate dirty-mark -> refresher -> RefreshFunc that writes NEW bytes
// -> Get returns the new content.
func TestRefresher_OnUpdateRefreshesContent(t *testing.T) {
	cleanup := withCleanRefresher(t, 2, 0)
	defer cleanup()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{CacheEntryClass: "widgets", Name: "content"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{"v":"stale"}`), Inputs: &inputs})

	gvr := gvrCompositions()
	Deps().Record(key, gvr, "ns", "n")

	fresh := []byte(`{"v":"fresh"}`)
	RegisterRefreshFunc("widgets", func(_ context.Context, k string, _ ResolvedKeyInputs) error {
		// A real RefreshFunc re-resolves and Put()s; emulate that here.
		c.Put(k, &ResolvedEntry{RawJSON: fresh, Inputs: &inputs})
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	Deps().OnUpdate(gvr, "ns", "n")
	waitFor(t, 2*time.Second, "entry content refreshed", func() bool {
		e, ok := c.Get(key)
		return ok && string(e.RawJSON) == string(fresh)
	})
}

// "refresh landing after an evict must not resurrect" — a refresh that
// completes after the entry was DELETE-evicted must NOT re-create it.
// The refresher's processOne skips a key whose entry is already gone
// (Get miss -> skippedNoEntry, no handler call).
func TestRefresher_RefreshAfterEvictDoesNotResurrect(t *testing.T) {
	cleanup := withCleanRefresher(t, 1, 0)
	defer cleanup()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{CacheEntryClass: "widgets", Name: "evicted"}
	key := ComputeKey(inputs)
	// Entry is NOT in the store — emulate "evicted before the worker
	// picked up the dirty-mark".

	var handlerCalls atomic.Int32
	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		handlerCalls.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	enqueueRefreshForTest(key)
	waitFor(t, 2*time.Second, "evicted-key skip counted",
		func() bool { return refresherSingleton().skippedNoEntryTotal.Load() == 1 })
	// The handler must NOT have run — a gone entry is not re-resolved.
	if got := handlerCalls.Load(); got != 0 {
		t.Fatalf("RefreshFunc ran %d time(s) for an evicted key — must not resurrect", got)
	}
	if _, ok := c.Get(key); ok {
		t.Fatalf("evicted entry was resurrected by the refresh")
	}
}

// --- AC-C6 — CACHE_ENABLED=false: refresher never starts --------------------

func TestRefresher_CacheDisabledNeverStarts(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true") // irrelevant — CACHE off wins
	resetResolvedCacheForTest()
	resetDepsForTest()
	resetRefresherForTest()
	defer func() {
		resetRefresherForTest()
		resetDepsForTest()
		resetResolvedCacheForTest()
	}()

	var calls atomic.Int32
	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		calls.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx) // must be a no-op when cache is disabled

	// No workers started — even an enqueue must not produce a call.
	enqueueRefreshForTest("any-key")
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("CACHE_ENABLED=false: RefreshFunc invoked %d time(s); refresher must never start", got)
	}
	// No worker goroutines spawned.
	r := refresherSingleton()
	done := make(chan struct{})
	go func() { r.workersWG.Wait(); close(done) }()
	select {
	case <-done:
		// WaitGroup at zero — no workers — correct.
	case <-time.After(time.Second):
		t.Fatalf("CACHE_ENABLED=false: worker goroutines exist; refresher started")
	}
}

// --- Dep tracker -> refresher hook wiring -----------------------------------

func TestRefresher_DepTrackerOnUpdateEnqueues(t *testing.T) {
	cleanup := withCleanRefresher(t, 2, 0)
	defer cleanup()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{CacheEntryClass: "widgets"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

	gvr := gvrCompositions()
	Deps().Record(key, gvr, "ns", "n")

	var fired atomic.Int32
	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		fired.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	Deps().OnUpdate(gvr, "ns", "n")
	waitFor(t, 2*time.Second, "dep-tracker -> refresher fired",
		func() bool { return fired.Load() == 1 })

	// UPDATE does NOT evict.
	if _, ok := c.Get(key); !ok {
		t.Fatalf("OnUpdate evicted entry — violates feedback_l1_invalidation_delete_only.md")
	}
}
