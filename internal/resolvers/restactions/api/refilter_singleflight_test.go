// Q-RBAC-DECOUPLE C(d) v3 §2.6 — singleflight refilter dedup unit tests.
//
// The headline contract under test: 100 concurrent calls for the same
// (l1Key, user.Username, groups-hash) MUST result in exactly ONE inner
// RefilterRESTAction execution. 100 concurrent calls across N distinct
// users on the SAME l1Key MUST result in N inner executions (one per
// user) — the per-user trust boundary cannot collapse under load.
//
// The test injects a counting + blocking variant via the package-level
// `refilterFlightInner` seam. The blocker is a sync.WaitGroup the leader
// goroutine waits on so we can guarantee all followers have joined the
// singleflight slot BEFORE the leader returns. Without the barrier the
// real inner is microseconds-fast and frequently completes before
// follower goroutines reach the Do call, masking the dedup behaviour.
package api

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// withInnerStub installs a counting / barrier-aware refilterFlightInner
// for the duration of `fn`. Returns the call counter so the test can
// assert on it. Restores the production inner on exit.
//
// The stub blocks on `gate` (a sync.WaitGroup the test increments by N
// before issuing N concurrent callers). The leader goroutine inside the
// singleflight slot Wait()s on gate; the test unblocks it by Done()-ing
// once it has confirmed N followers are queued. This guarantees the
// dedup window stays open long enough to exercise the spec contract.
func withInnerStub(t *testing.T, gate *sync.WaitGroup, out []byte) (counter *atomic.Int64, restore func()) {
	t.Helper()
	prev := refilterFlightInner
	c := &atomic.Int64{}
	refilterFlightInner = func(ctx context.Context, _ cache.Cache, _ []byte) ([]byte, error) {
		c.Add(1)
		if gate != nil {
			gate.Wait()
		}
		return out, nil
	}
	return c, func() { refilterFlightInner = prev }
}

// rawForKey is a sentinel non-empty []byte the stubs return. The real
// bytes don't matter: the stub never decodes them and the singleflight
// just shares whatever the inner returns.
var rawForKey = []byte(`{"sentinel":"v3-singleflight-test"}`)

func TestRefilterRESTActionDeduped_100Concurrent_SameKey_ExactlyOneInner(t *testing.T) {
	const N = 100
	const l1Key = "snowplow:resolved:identity-x:t.k.io:v1:restactions::compositions-list:0:0"

	var gate sync.WaitGroup
	gate.Add(1) // leader blocks until released after followers have queued
	counter, restore := withInnerStub(t, &gate, rawForKey)
	defer restore()

	ctx := ctxWith(t, "user-burst", nil) // groups=["devs"] from helper

	var startedAll sync.WaitGroup
	startedAll.Add(N)
	var done sync.WaitGroup
	done.Add(N)
	results := make([][]byte, N)
	errs := make([]error, N)

	for i := 0; i < N; i++ {
		go func(i int) {
			defer done.Done()
			startedAll.Done() // mark this goroutine as "running"
			out, err := RefilterRESTActionDeduped(ctx, nil, rawForKey, l1Key)
			results[i] = out
			errs[i] = err
		}(i)
	}

	// Wait for every goroutine to be scheduled at least to the point
	// where it has called Done() on startedAll. They are now racing
	// into singleflight.Do; the leader is blocked on gate.
	startedAll.Wait()

	// Give followers ample time to enter Do() and queue behind the
	// leader. Without this, a fast leader could exit before stragglers
	// arrive and we'd see >1 inner call. The 100 ms window is generous;
	// the test still completes quickly.
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	// Release the leader. All followers immediately receive the shared
	// result.
	gate.Done()
	done.Wait()

	if got := counter.Load(); got != 1 {
		t.Errorf("expected exactly 1 inner refilter call for 100 concurrent same-key callers, got %d", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: unexpected err %v", i, err)
		}
		if string(results[i]) != string(rawForKey) {
			t.Errorf("goroutine %d: result mismatch, expected sentinel bytes", i)
		}
	}
}

func TestRefilterRESTActionDeduped_100Concurrent_DistinctUsers_OnePerUser(t *testing.T) {
	const N = 100 // 100 distinct users, ALL on the same L1 key
	const l1Key = "snowplow:resolved:shared-binding:t.k.io:v1:restactions::compositions-list:0:0"

	// No gate barrier: each user must get its OWN inner call (no dedup
	// collapse between users). We just count and let each one return
	// quickly.
	counter, restore := withInnerStub(t, nil, rawForKey)
	defer restore()

	var done sync.WaitGroup
	done.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer done.Done()
			user := userN(i)
			ctx := ctxWith(t, user, nil)
			_, _ = RefilterRESTActionDeduped(ctx, nil, rawForKey, l1Key)
		}(i)
	}
	done.Wait()

	if got := counter.Load(); got != int64(N) {
		t.Errorf("expected exactly %d inner refilter calls (one per distinct user) on the same L1 key, got %d",
			N, got)
	}
}

// userN returns a stable distinct username for index i — the test wants
// to ensure the singleflight key SEGREGATES per-user, so usernames must
// be different for every i.
func userN(i int) string {
	return "user-" + itoaPad(i)
}

// itoaPad zero-pads to 4 digits so the user names sort lexicographically
// and the stub-counter test always sees the SAME 100 distinct users.
// Avoids allocating fmt.Sprintf in a hot loop.
func itoaPad(i int) string {
	const digits = "0123456789"
	b := []byte("0000")
	for k := 3; k >= 0; k-- {
		b[k] = digits[i%10]
		i /= 10
	}
	return string(b)
}

// ── Key derivation contract: groups-hash splits the singleflight slot ──

func TestRefilterRESTActionDeduped_GroupsHashSplitsKey(t *testing.T) {
	// Same username, DIFFERENT groups → distinct singleflight keys → 2
	// inner calls. This is the OAuth-refresh-mid-burst scenario from
	// Q-RBACC-V3-IMPL-3: a user whose group claim flipped between
	// requests must NEVER share a refilter result with their previous
	// group set.
	const l1Key = "snowplow:resolved:groups-test:t.k.io:v1:restactions::compositions-list:0:0"

	counter, restore := withInnerStub(t, nil, rawForKey)
	defer restore()

	ctxOldGroups := ctxWithGroups(t, "user-x", []string{"old-group"})
	ctxNewGroups := ctxWithGroups(t, "user-x", []string{"new-group"})

	if _, err := RefilterRESTActionDeduped(ctxOldGroups, nil, rawForKey, l1Key); err != nil {
		t.Fatalf("old groups call: %v", err)
	}
	if _, err := RefilterRESTActionDeduped(ctxNewGroups, nil, rawForKey, l1Key); err != nil {
		t.Fatalf("new groups call: %v", err)
	}
	if got := counter.Load(); got != 2 {
		t.Errorf("expected 2 inner calls (distinct groups → distinct keys), got %d", got)
	}
}

// ctxWithGroups builds a test context with a specific groups list so the
// groups-hash component of the singleflight key can be exercised
// independently of the username. Mirrors ctxWith() in refilter_test.go
// but lets the caller override the Groups field directly (ctxWith hard-
// codes Groups=[]{"devs"}).
func ctxWithGroups(t *testing.T, username string, groups []string) context.Context {
	t.Helper()
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithLogger(slog.Default()),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: username, Groups: groups}),
	)
	return ctx
}

// ── hashGroups invariants ──────────────────────────────────────────────

func TestHashGroups_OrderIndependent(t *testing.T) {
	a := hashGroups([]string{"alpha", "beta", "gamma"})
	b := hashGroups([]string{"gamma", "alpha", "beta"})
	if a != b {
		t.Errorf("hashGroups not order-independent:\n a=%s\n b=%s", a, b)
	}
}

func TestHashGroups_DistinctSetsDistinctHashes(t *testing.T) {
	a := hashGroups([]string{"alpha", "beta"})
	b := hashGroups([]string{"alpha", "gamma"})
	if a == b {
		t.Errorf("hashGroups returned identical hash for distinct group sets: %s", a)
	}
}

func TestHashGroups_EmptyIsStable(t *testing.T) {
	a := hashGroups(nil)
	b := hashGroups([]string{})
	if a != b {
		t.Errorf("hashGroups: nil vs empty slice produced different hashes\n nil=%s\n []=%s", a, b)
	}
	if a == "" {
		t.Errorf("hashGroups: empty input produced empty string (expected stable sentinel)")
	}
}
