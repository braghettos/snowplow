// Q-REFRESH-COALESCE (0.25.328) — singleflight refresher coalesce unit tests.
//
// The headline contract under test: 100 concurrent calls for the same raw
// L1 key MUST result in exactly ONE inner refreshSingleL1 execution. 100
// concurrent calls across N distinct raw keys MUST result in N inner
// executions (one per key).
//
// The follower counter (cache.GlobalMetrics.RefresherInflightCoalesced)
// MUST increment by exactly N-1 when N callers share a slot — the leader
// is the goroutine whose increment took the inflight counter from 0 → 1
// and is NOT counted.
//
// Errors are propagated identically to all callers (singleflight semantic).
//
// The tests inject a counting + barrier-aware refreshSingleL1Inner via the
// package-level seam. The barrier is a sync.WaitGroup the leader goroutine
// waits on so we can guarantee all followers have joined the singleflight
// slot BEFORE the leader returns. Without the barrier the real inner can
// complete in microseconds and frequently exits before follower goroutines
// reach the Do call, masking the dedup behaviour.
package dispatchers

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// withRefresherInnerStub installs a counting / barrier-aware
// refreshSingleL1Inner for the duration of fn and returns the call counter
// so the test can assert on it. Restores the production inner on exit.
//
// The stub blocks on `gate` (a sync.WaitGroup the test increments by 1
// before issuing N concurrent callers). The leader goroutine inside the
// singleflight slot Wait()s on gate; the test unblocks it by Done()-ing
// once it has confirmed N followers are queued.
func withRefresherInnerStub(t *testing.T, gate *sync.WaitGroup, ok bool, cascade []string) (counter *atomic.Int64, restore func()) {
	t.Helper()
	prev := refreshSingleL1Inner
	c := &atomic.Int64{}
	refreshSingleL1Inner = func(
		ctx context.Context,
		_ cache.Cache,
		_ jwtutil.UserInfo,
		_ endpoints.Endpoint,
		_ string,
		_ cache.ResolvedKeyInfo,
		_ string,
		_ string,
		_ func() (*endpoints.Endpoint, error),
		_ dynamic.Client,
	) (bool, []string) {
		c.Add(1)
		if gate != nil {
			gate.Wait()
		}
		return ok, cascade
	}
	return c, func() { refreshSingleL1Inner = prev }
}

// resetCoalesceState clears the inflight map and the follower counter so
// each test starts from a clean slate. Tests SHOULD call this at the top
// because state is package-level.
func resetCoalesceState(t *testing.T) {
	t.Helper()
	refresherInflight.Range(func(k, _ any) bool {
		refresherInflight.Delete(k)
		return true
	})
	cache.GlobalMetrics.RefresherInflightCoalesced.Store(0)
}

// makeInfo builds a minimal ResolvedKeyInfo for tests. The inner stub
// ignores all fields so any non-empty values work.
func makeInfo(username, ns, name string) cache.ResolvedKeyInfo {
	return cache.ResolvedKeyInfo{
		Username: username,
		GVR:      schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"},
		NS:       ns,
		Name:     name,
		Page:     0,
		PerPage:  0,
	}
}

func TestRefreshSingleL1Coalesced_100Concurrent_SameKey_ExactlyOneInner(t *testing.T) {
	resetCoalesceState(t)
	const N = 100
	const rawKey = "snowplow:resolved:identity-x:templates.krateo.io/v1/restactions:ns-a:compositions-list:p0-pp0"

	var gate sync.WaitGroup
	gate.Add(1) // leader blocks until released after followers have queued
	counter, restore := withRefresherInnerStub(t, &gate, true, []string{"cascade-1"})
	defer restore()

	info := makeInfo("identity-x", "ns-a", "compositions-list")
	user := jwtutil.UserInfo{Username: "identity-x", Groups: []string{"system:masters"}}

	var startedAll sync.WaitGroup
	startedAll.Add(N)
	var done sync.WaitGroup
	done.Add(N)
	results := make([]bool, N)

	for i := 0; i < N; i++ {
		go func(i int) {
			defer done.Done()
			startedAll.Done()
			ok, _ := refreshSingleL1Coalesced(context.Background(), nil, user, endpoints.Endpoint{}, "tok", info, rawKey, "krateo-system", nil, nil)
			results[i] = ok
		}(i)
	}

	// All goroutines have at least reached startedAll.Done(). They are now
	// racing into singleflight.Do; the leader is blocked on gate.
	startedAll.Wait()

	// Give followers ample time to enter Do() and queue behind the leader.
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	// Release the leader. All followers immediately receive the shared result.
	gate.Done()
	done.Wait()

	if got := counter.Load(); got != 1 {
		t.Errorf("expected exactly 1 inner refresh call for %d concurrent same-key callers, got %d", N, got)
	}
	for i, ok := range results {
		if !ok {
			t.Errorf("goroutine %d: expected ok=true (leader's result), got false", i)
		}
	}
	// Follower counter MUST equal exactly N-1 — the leader is the goroutine
	// whose Add(1) took the inflight counter from 0 → 1 and is not counted.
	if got := cache.GlobalMetrics.RefresherInflightCoalesced.Load(); got != int64(N-1) {
		t.Errorf("expected RefresherInflightCoalesced == %d (followers only, leader excluded), got %d", N-1, got)
	}
}

func TestRefreshSingleL1Coalesced_100Concurrent_DistinctKeys_OnePerKey(t *testing.T) {
	resetCoalesceState(t)
	const N = 100

	// No gate barrier: each key must get its OWN inner call (no dedup
	// collapse). Just count and let each one return quickly.
	counter, restore := withRefresherInnerStub(t, nil, true, nil)
	defer restore()

	user := jwtutil.UserInfo{Username: "identity-x", Groups: []string{"system:masters"}}

	var done sync.WaitGroup
	done.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer done.Done()
			// Distinct rawKey per i (encode i into ns to vary the key).
			rawKey := "snowplow:resolved:identity-x:templates.krateo.io/v1/restactions:ns-" + itoaPad(i) + ":res:p0-pp0"
			info := makeInfo("identity-x", "ns-"+itoaPad(i), "res")
			_, _ = refreshSingleL1Coalesced(context.Background(), nil, user, endpoints.Endpoint{}, "tok", info, rawKey, "krateo-system", nil, nil)
		}(i)
	}
	done.Wait()

	if got := counter.Load(); got != int64(N) {
		t.Errorf("expected exactly %d inner refresh calls (one per distinct key), got %d", N, got)
	}
	// No coalesce: follower counter MUST stay at zero.
	if got := cache.GlobalMetrics.RefresherInflightCoalesced.Load(); got != 0 {
		t.Errorf("expected RefresherInflightCoalesced == 0 for fully-distinct keys, got %d", got)
	}
}

func TestRefreshSingleL1Coalesced_FollowerCounterIncrementsExactlyOncePerFollower(t *testing.T) {
	resetCoalesceState(t)
	const N = 8 // leader + 7 followers
	const rawKey = "snowplow:resolved:identity-counter:templates.krateo.io/v1/restactions:ns-c:res:p0-pp0"

	var gate sync.WaitGroup
	gate.Add(1)
	_, restore := withRefresherInnerStub(t, &gate, true, nil)
	defer restore()

	info := makeInfo("identity-counter", "ns-c", "res")
	user := jwtutil.UserInfo{Username: "identity-counter", Groups: []string{"system:masters"}}

	var startedAll sync.WaitGroup
	startedAll.Add(N)
	var done sync.WaitGroup
	done.Add(N)

	for i := 0; i < N; i++ {
		go func() {
			defer done.Done()
			startedAll.Done()
			_, _ = refreshSingleL1Coalesced(context.Background(), nil, user, endpoints.Endpoint{}, "tok", info, rawKey, "krateo-system", nil, nil)
		}()
	}
	startedAll.Wait()
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	// Snapshot BEFORE releasing the leader. The followers have all
	// incremented the inflight counter and bumped the metric; the leader
	// did not. So we expect exactly N-1 followers counted.
	got := cache.GlobalMetrics.RefresherInflightCoalesced.Load()
	gate.Done()
	done.Wait()

	if got != int64(N-1) {
		t.Errorf("expected RefresherInflightCoalesced == %d before leader release, got %d", N-1, got)
	}
}

// itoaPad zero-pads to 4 digits so test keys are stable and unique.
func itoaPad(i int) string {
	const digits = "0123456789"
	b := []byte("0000")
	for k := 3; k >= 0; k-- {
		b[k] = digits[i%10]
		i /= 10
	}
	return string(b)
}
