// refresher_falsifier_test.go — Ship C (0.30.112) pre-flight falsifiers
// for the runtime L1 refresher's queue foundation.
//
// Team rule feedback_falsifier_first_before_ship: written BEFORE the
// workqueue rebuild; F-backoff and F-drop MUST fail against the
// hand-rolled refresher.go (enqueueCh + inFlight + dedupWindow).
//
//   F-backoff — a RefreshFunc that errors must be retried with bounded
//        exponential backoff. FAILS today: the hand-rolled processOne
//        just counts failedTotal and drops the key — no requeue, no
//        retry at all.
//
//   F-drop   — dirty-marking parallelism*64 + 1 keys faster than the
//        workers drain must drop NONE. FAILS today: enqueueCh is a
//        buffered channel of cap parallelism*64; the parallelism*64+1-th
//        enqueue hits the `default` branch and is dropped
//        (skippedFullTotal++). This is the load-bearing justification
//        for the workqueue rebuild.
//
// (F-noop — the registered RefreshFunc body actually re-resolving —
// lives in internal/handlers/dispatchers, where the handler bodies are.)

package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- F-backoff — error path must retry with bounded backoff -----------------

// TestFalsifierFBackoff_ErrorRetriesWithBoundedBackoff registers a
// RefreshFunc that fails the first few attempts then succeeds, and
// asserts the refresher retries it (the key is re-processed) and that
// the retries are bounded (it eventually succeeds, not infinite churn).
//
// FAILS today: the hand-rolled refresher never requeues a failed key —
// the handler is invoked exactly once, failedTotal ticks, done.
func TestFalsifierFBackoff_ErrorRetriesWithBoundedBackoff(t *testing.T) {
	cleanup := withCleanRefresher(t, 1, 1)
	defer cleanup()

	c := ResolvedCache()
	inputs := ResolvedKeyInputs{CacheEntryClass: "widgets", Name: "backoff"}
	key := ComputeKey(inputs)
	c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})

	var attempts atomic.Int32
	const failFirst = 3
	RegisterRefreshFunc("widgets", func(context.Context, string, ResolvedKeyInputs) error {
		n := attempts.Add(1)
		if n <= failFirst {
			return errors.New("simulated transient resolver failure")
		}
		return nil // succeeds on attempt failFirst+1
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	enqueueRefreshForTest(key)

	// A retrying refresher re-processes the key until it succeeds:
	// attempts must climb past 1 and reach failFirst+1.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if attempts.Load() >= failFirst+1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := attempts.Load()
	if got < failFirst+1 {
		t.Fatalf("F-backoff: handler invoked %d time(s); want >= %d "+
			"(an erroring RefreshFunc must be retried with backoff until success) "+
			"— the refresher does not retry failed keys", got, failFirst+1)
	}
	// Bounded: once it succeeds the key must stop being re-processed.
	settled := attempts.Load()
	time.Sleep(500 * time.Millisecond)
	if attempts.Load() != settled {
		t.Fatalf("F-backoff: handler kept firing after success (%d -> %d) — "+
			"retry is not bounded; Forget after success is missing",
			settled, attempts.Load())
	}
}

// --- F-drop — a burst past the buffer must drop nothing ---------------------

// TestFalsifierFDrop_BurstDropsNothing dirty-marks parallelism*64 + 1
// distinct keys far faster than the (deliberately blocked) workers can
// drain, then unblocks the workers and asserts EVERY key was processed
// — none dropped.
//
// FAILS today: the hand-rolled enqueueCh is a buffered channel of cap
// parallelism*defaultRefresherQueueBufferMultiplier; the (cap+1)-th
// enqueue takes the `default` branch and is dropped (skippedFullTotal++).
func TestFalsifierFDrop_BurstDropsNothing(t *testing.T) {
	const parallelism = 2
	cleanup := withCleanRefresher(t, parallelism, 1)
	defer cleanup()

	c := ResolvedCache()

	// Block every worker on `release` so the queue cannot drain while we
	// enqueue the burst — this forces the buffer-full condition.
	release := make(chan struct{})
	var processed sync.Map // key -> struct{}
	var processedCount atomic.Int64
	RegisterRefreshFunc("widgets", func(_ context.Context, k string, _ ResolvedKeyInputs) error {
		<-release
		if _, dup := processed.LoadOrStore(k, struct{}{}); !dup {
			processedCount.Add(1)
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRefresher(ctx)

	// Burst comfortably past the old hand-rolled capacity. The old
	// refresher's enqueueCh held parallelism*64 buffered plus one
	// in-flight per worker — it dropped everything past that on the
	// `default` branch. 180 keys with parallelism=2 is well past
	// 2*64+2=130, so the old code dropped ~50; the workqueue rebuild
	// is unbounded and processes every key once `release` opens.
	const burst = 180
	keys := make([]string, 0, burst)
	for i := 0; i < burst; i++ {
		inputs := ResolvedKeyInputs{CacheEntryClass: "widgets", Name: "drop" + itoa(i)}
		key := ComputeKey(inputs)
		c.Put(key, &ResolvedEntry{RawJSON: []byte(`{}`), Inputs: &inputs})
		keys = append(keys, key)
	}
	for _, k := range keys {
		enqueueRefreshForTest(k)
	}

	// Unblock the workers and let the queue fully drain.
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if processedCount.Load() >= int64(burst) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := processedCount.Load(); got != int64(burst) {
		t.Fatalf("F-drop: %d/%d burst keys processed — %d dropped. The refresher "+
			"queue must never drop a dirty-mark; a bounded channel that "+
			"`default`-drops on full does.", got, burst, int64(burst)-got)
	}
}
