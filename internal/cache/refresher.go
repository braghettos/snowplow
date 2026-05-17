// refresher.go — Ship C (0.30.112): the runtime L1 resolved-output
// cache refresher, rebuilt on a client-go workqueue.
//
// The dep tracker dirty-marks an L1 key (OnAdd/OnUpdate -> refreshHook)
// when an informer event invalidates the resolved output. The refresher
// is the worker pool that drains those dirty-marks and RE-RESOLVES the
// entry — never evicts (feedback_l1_invalidation_delete_only.md: UPDATE
// uses stale-while-revalidate).
//
// QUEUE FOUNDATION (Ship C — replaces the 0.30.8 hand-rolled
// enqueueCh + inFlight + dedupWindow):
//
//   * workqueue.NewTypedRateLimitingQueue[string] backed by a
//     NewTypedItemExponentialFailureRateLimiter[string] (base/max delay
//     from the env knobs). The queue gives us, for free, the three
//     properties the hand-rolled version lacked or got wrong:
//       - idempotent dedup: Add(key) of an already-queued key is a
//         no-op — M rapid dirty-marks of one key coalesce to one
//         processing;
//       - NEVER drops: the queue is unbounded; a burst past any buffer
//         is queued, not dropped (the F-drop falsifier);
//       - bounded exponential-backoff retry: AddRateLimited re-enqueues
//         a failed key after an exponentially-growing delay; Forget
//         stops the backoff once it succeeds (the F-backoff falsifier).
//
//   * N workers (RESOLVED_CACHE_REFRESHER_PARALLELISM) each run
//     wait.UntilWithContext(processNext): Get -> process -> Done; on
//     success Forget, on error AddRateLimited.
//
//   * ShutDown() on ctx-cancel drains the workers cleanly — Get returns
//     shutdown=true, the worker loop returns, no goroutine leak.
//
// Lifecycle: one pool launched at process start by StartRefresher
// (after ResolvedCache() is built and after the dispatchers register
// their RefreshFuncs). Exits on context cancellation or StopRefresher.

package cache

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
)

// Refresher env knobs. The base/max backoff delays drive the
// exponential-failure rate limiter; parallelism sizes the worker pool.
const (
	envRefresherParallelism = "RESOLVED_CACHE_REFRESHER_PARALLELISM"
	envRefresherBaseDelayMS = "RESOLVED_CACHE_REFRESHER_BASE_DELAY_MS"
	envRefresherMaxDelayMS  = "RESOLVED_CACHE_REFRESHER_MAX_DELAY_MS"

	defaultRefresherParallelism = 4
	// Exponential-failure backoff: first retry after baseDelay, doubling
	// each requeue, capped at maxDelay. 500ms -> 1s -> 2s -> ... -> 60s.
	defaultRefresherBaseDelayMS = 500
	defaultRefresherMaxDelayMS  = 60_000

	// maxRefreshRequeues caps how many times a single key may be
	// re-enqueued via AddRateLimited before the refresher gives up on it
	// (Ship 0.30.113 Part A — the poison-pill bound). This is the standard
	// client-go controller idiom: a key whose handler keeps failing is
	// almost always a DETERMINISTIC failure (a deleted CR, a malformed
	// spec, a missing dependency) — re-enqueuing it forever just spins the
	// worker pool with no chance of success. Once NumRequeues exceeds the
	// cap, the key is Forgotten and DROPPED: the entry falls back to its
	// TTL outer-net (no resurrection, no spin). The bound is GENERAL — it
	// is not specific to any one handler kind or failure; it protects
	// against ANY future deterministic refresh failure.
	maxRefreshRequeues = 5
)

// RefreshFunc is the callback the cache package invokes on a refresh.
// It MUST re-resolve the entry described by inputs and Put the fresh
// bytes back into the L1 store under the same cache key. The cache
// package supplies the matching key string for convenience.
//
// Implementations live in `internal/handlers/dispatchers` (one per
// handlerKind). A non-nil error makes the refresher requeue the key
// with exponential backoff; nil makes it Forget the key.
type RefreshFunc func(ctx context.Context, key string, inputs ResolvedKeyInputs) error

// refresher is the singleton worker pool. Constructed lazily by
// StartRefresher; one per process.
type refresher struct {
	parallelism int

	// queue is the rate-limiting workqueue. Add(key) is idempotent
	// dedup; AddRateLimited(key) re-enqueues with exponential backoff;
	// the queue is unbounded so a dirty-mark is NEVER dropped.
	queue workqueue.TypedRateLimitingInterface[string]

	handlersMu sync.RWMutex
	handlers   map[string]RefreshFunc

	// Falsifier counters (atomic).
	enqueueTotal        atomic.Uint64
	completedTotal      atomic.Uint64
	failedTotal         atomic.Uint64
	retriedTotal        atomic.Uint64 // keys re-enqueued via AddRateLimited
	droppedTotal        atomic.Uint64 // keys dropped after exceeding maxRefreshRequeues
	skippedNoEntryTotal atomic.Uint64
	skippedNoHandler    atomic.Uint64

	startedOnce sync.Once
	stopOnce    sync.Once
	// workersWG lets test cleanup block until every worker goroutine
	// has actually exited (Get returned shutdown).
	workersWG sync.WaitGroup
}

var (
	refresherInstance *refresher
	refresherInit     sync.Once
)

// refresherSingleton returns the process-wide refresher, constructing
// it lazily.
func refresherSingleton() *refresher {
	refresherInit.Do(func() {
		parallelism := intFromEnv(envRefresherParallelism, defaultRefresherParallelism)
		if parallelism <= 0 {
			parallelism = defaultRefresherParallelism
		}
		baseMS := intFromEnv(envRefresherBaseDelayMS, defaultRefresherBaseDelayMS)
		if baseMS <= 0 {
			baseMS = defaultRefresherBaseDelayMS
		}
		maxMS := intFromEnv(envRefresherMaxDelayMS, defaultRefresherMaxDelayMS)
		if maxMS <= 0 {
			maxMS = defaultRefresherMaxDelayMS
		}
		rl := workqueue.NewTypedItemExponentialFailureRateLimiter[string](
			time.Duration(baseMS)*time.Millisecond,
			time.Duration(maxMS)*time.Millisecond,
		)
		refresherInstance = &refresher{
			parallelism: parallelism,
			queue:       workqueue.NewTypedRateLimitingQueue[string](rl),
			handlers:    map[string]RefreshFunc{},
		}
	})
	return refresherInstance
}

// RegisterRefreshFunc wires a refresh handler for handlerKind ("restactions",
// "widgets"). Safe to call multiple times; later calls replace the
// earlier wiring (used by tests + by hot-reload scenarios).
//
// MUST be called BEFORE StartRefresher so the worker pool sees a fully
// populated handler map on its first dequeue.
func RegisterRefreshFunc(handlerKind string, fn RefreshFunc) {
	r := refresherSingleton()
	r.handlersMu.Lock()
	r.handlers[handlerKind] = fn
	r.handlersMu.Unlock()
}

// StartRefresher launches the worker pool. Idempotent — repeated calls
// are no-ops (the second StartRefresher does NOT spawn more workers).
// The pool exits cleanly when ctx is canceled OR when StopRefresher is
// called (both ShutDown the queue).
func StartRefresher(ctx context.Context) {
	if !ResolvedCacheEnabled() {
		return
	}
	r := refresherSingleton()
	r.startedOnce.Do(func() {
		// Wire the dep tracker's dirty-mark hook to the queue. Add is
		// idempotent — repeat marks of one key coalesce; the queue is
		// unbounded — a mark is never dropped.
		Deps().SetRefreshHook(func(l1Key string) {
			r.enqueue(l1Key)
		})

		for i := 0; i < r.parallelism; i++ {
			r.workersWG.Add(1)
			go func(id int) {
				defer r.workersWG.Done()
				// UntilWithContext re-invokes processNext until ctx is
				// done. processNext blocks in queue.Get, so the period
				// only matters after the queue ShutsDown (Get returns
				// immediately) — a small period keeps the post-shutdown
				// spin bounded; the loop exits on ctx.Done regardless.
				wait.UntilWithContext(ctx, func(c context.Context) {
					for r.processNext(c) {
					}
				}, time.Second)
			}(i)
		}

		// Shut the queue down when ctx is cancelled — Get unblocks,
		// every worker's processNext returns false, the loop ends.
		go func() {
			<-ctx.Done()
			r.queue.ShutDown()
		}()

		slog.Info("refresher.started",
			slog.String("subsystem", "cache"),
			slog.Int("parallelism", r.parallelism),
			slog.String("queue", "workqueue.RateLimiting"),
		)
	})
}

// StopRefresher shuts the queue down. Safe to call multiple times. Used
// by tests; production lets the context-cancel path drive shutdown.
func StopRefresher() {
	r := refresherSingleton()
	r.stopOnce.Do(func() {
		r.queue.ShutDown()
	})
}

// enqueue adds l1Key to the workqueue. Add is idempotent: a key already
// queued (or being processed) is coalesced — never duplicated, never
// dropped. The counter ticks on every accepted enqueue call; dedup is
// invisible to it by design (the queue owns coalescing).
func (r *refresher) enqueue(l1Key string) {
	if l1Key == "" {
		return
	}
	r.queue.Add(l1Key)
	r.enqueueTotal.Add(1)
}

// processNext pulls one key, processes it, and reports whether the
// worker loop should continue (false once the queue has ShutDown).
//
//   - success: Forget(key) — clear the backoff — then Done(key).
//   - error, under the requeue cap: AddRateLimited(key) — requeue after
//     exponential backoff — then Done(key). The key WILL be retried.
//   - error, requeue cap exceeded: Forget(key) and DROP the key — then
//     Done(key). The key is NOT retried; the entry falls back to its TTL
//     outer-net. This is the Ship 0.30.113 Part A poison-pill bound — a
//     deterministic failure (one that can never succeed on retry) must
//     not re-enqueue forever and spin the worker pool.
//
// Done(key) is always called (deferred) so the queue can release the
// key for re-add; Forget/AddRateLimited only touch the rate limiter.
func (r *refresher) processNext(ctx context.Context) bool {
	key, shutdown := r.queue.Get()
	if shutdown {
		return false
	}
	defer r.queue.Done(key)

	if err := r.processOne(ctx, key); err != nil {
		r.failedTotal.Add(1)
		// Poison-pill bound (Part A). NumRequeues is how many times this
		// exact key has already been AddRateLimited. Once it exceeds the
		// cap the failure is treated as deterministic: Forget the key
		// (clear the rate limiter so a FUTURE genuine dirty-mark of the
		// same key starts clean) and DROP it — no AddRateLimited. The
		// entry stays in L1, stale, until its TTL purges it; a later
		// informer event can re-enqueue it fresh.
		if r.queue.NumRequeues(key) >= maxRefreshRequeues {
			r.queue.Forget(key)
			r.droppedTotal.Add(1)
			slog.Warn("refresher.refresh_dropped",
				slog.String("subsystem", "cache"),
				slog.String("key_hash", key),
				slog.Int("requeues", maxRefreshRequeues),
				slog.String("effect", "deterministic refresh failure — key dropped to TTL outer-net, not retried"),
			)
			return true
		}
		r.retriedTotal.Add(1)
		// Bounded exponential-backoff retry. The key is NOT Forgotten,
		// so the rate limiter's NumRequeues climbs and the next delay
		// doubles (capped at maxDelay).
		r.queue.AddRateLimited(key)
		return true
	}
	// Success — stop the rate limiter tracking this key so a future
	// dirty-mark of the same key starts from a clean backoff.
	r.queue.Forget(key)
	r.completedTotal.Add(1)
	return true
}

// processOne handles a single refresh: load the entry from L1, dispatch
// the registered handler for its kind. Returns the handler's error
// (drives the requeue decision). A missing entry / missing handler /
// legacy nil-Inputs entry is a non-error skip (counted, not retried).
func (r *refresher) processOne(ctx context.Context, key string) error {
	c := ResolvedCache()
	if c == nil {
		r.skippedNoEntryTotal.Add(1)
		return nil
	}
	entry, ok := c.Get(key)
	if !ok || entry == nil {
		// L1 may have evicted between the dirty-mark and us picking up
		// the key (e.g. a DELETE raced the UPDATE). Stale-while-
		// revalidate degrades to next-cold-miss; not an error, not a
		// retry — the entry is gone.
		r.skippedNoEntryTotal.Add(1)
		return nil
	}
	if entry.Inputs == nil {
		// Legacy pre-0.30.8 entry — no Inputs to drive a re-resolve.
		// Skip silently; TTL will purge. Not an error, not a retry.
		r.skippedNoHandler.Add(1)
		return nil
	}
	r.handlersMu.RLock()
	fn := r.handlers[entry.Inputs.CacheEntryClass]
	r.handlersMu.RUnlock()
	if fn == nil {
		r.skippedNoHandler.Add(1)
		return nil
	}
	if err := fn(ctx, key, *entry.Inputs); err != nil {
		slog.Warn("refresher.refresh_failed",
			slog.String("subsystem", "cache"),
			slog.String("handler_kind", entry.Inputs.CacheEntryClass),
			slog.String("key_hash", key),
			slog.Int("requeues", r.queue.NumRequeues(key)),
			slog.Any("err", err),
		)
		return err
	}
	return nil
}

// refresherStats is the read-only snapshot the summary log consumes.
type refresherStats struct {
	enqueued         uint64
	completed        uint64
	failed           uint64
	retried          uint64
	dropped          uint64
	skippedNoEntry   uint64
	skippedNoHandler uint64
}

func refresherStatsSnapshot() refresherStats {
	r := refresherSingleton()
	if r == nil {
		return refresherStats{}
	}
	return refresherStats{
		enqueued:         r.enqueueTotal.Load(),
		completed:        r.completedTotal.Load(),
		failed:           r.failedTotal.Load(),
		retried:          r.retriedTotal.Load(),
		dropped:          r.droppedTotal.Load(),
		skippedNoEntry:   r.skippedNoEntryTotal.Load(),
		skippedNoHandler: r.skippedNoHandler.Load(),
	}
}

// resetRefresherForTest tears the singleton down so each test sees a
// clean refresher. Exported only via the *_test.go shim — production
// code MUST NOT call this.
//
// CRITICAL: ShutDown the queue then block until every worker goroutine
// has actually exited. Without this barrier, a worker mid-processOne
// can race with the next test's resetResolvedCacheForTest.
func resetRefresherForTest() {
	if refresherInstance != nil {
		refresherInstance.queue.ShutDown()
		// Wait for workers to drain + exit. Capped at 5s as a defensive
		// deadline that should never fire.
		done := make(chan struct{})
		go func() {
			refresherInstance.workersWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			// Worker stuck — defensive; the test will likely fail
			// downstream because of corruption.
		}
	}
	refresherInstance = nil
	refresherInit = sync.Once{}
}

// ResetRefresherForTest is the exported variant of resetRefresherForTest
// for cross-package tests (e.g. internal/handlers/dispatchers' Ship C
// falsifier). Production code MUST NOT call it.
func ResetRefresherForTest() {
	resetRefresherForTest()
}

// RefreshFuncForTest returns the RefreshFunc registered for handlerKind,
// or nil when none is registered. Exported for cross-package tests
// (internal/handlers/dispatchers' Ship C falsifier invokes the
// dispatcher-registered handler directly). Production code MUST NOT
// call it.
func RefreshFuncForTest(handlerKind string) RefreshFunc {
	r := refresherSingleton()
	r.handlersMu.RLock()
	defer r.handlersMu.RUnlock()
	return r.handlers[handlerKind]
}

// enqueueRefreshForTest pushes l1Key into the refresher's queue via the
// same enqueue path the dep-tracker refresh hook uses. TEST-ONLY — a
// stable seam so refresher tests (and the Ship C falsifiers) do not
// depend on the queue's internal shape. Production code MUST NOT call
// it.
func enqueueRefreshForTest(l1Key string) {
	refresherSingleton().enqueue(l1Key)
}
