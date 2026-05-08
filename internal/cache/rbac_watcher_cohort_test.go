// Q-COHORT-PREWARM (v0.25.312) — unit tests for RBACWatcher's binding-
// ADD cohort fan-out.
//
// Test strategy: drive scheduleCohortPrewarmFromBinding directly with
// fabricated *rbacv1.RoleBinding objects and a mock RBACPrewarmer.
// This bypasses the K8s informer wiring while exercising:
//   - Subject extraction (User vs Group).
//   - Group expansion against the active-users set in the cache.
//   - Debounce coalescing (multiple ADDs collapse into one fan-out).
//   - Storm safety (1004 ADDs in a tight burst → one fan-out).
//   - Race cleanliness (concurrent calls to the scheduler).
package cache

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mockPrewarmer captures EnqueueForCohort calls. Thread-safe.
type mockPrewarmer struct {
	mu       sync.Mutex
	calls    int
	users    [][]string // recorded usernames per call
	delay    time.Duration
	onCall   func()
	accepted int
	dropped  int
}

func (m *mockPrewarmer) EnqueueForCohort(ctx context.Context, usernames []string) (accepted, dropped int) {
	m.mu.Lock()
	m.calls++
	cp := append([]string(nil), usernames...)
	m.users = append(m.users, cp)
	d := m.delay
	cb := m.onCall
	a := m.accepted
	dr := m.dropped
	m.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	if cb != nil {
		cb()
	}
	if a == 0 && dr == 0 {
		// default: accept all
		return len(usernames), 0
	}
	return a, dr
}

func (m *mockPrewarmer) snapshot() (calls int, lastUsers []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	calls = m.calls
	if len(m.users) > 0 {
		last := m.users[len(m.users)-1]
		lastUsers = append([]string(nil), last...)
	}
	return
}

// newTestRBACWatcher constructs a minimally-initialised RBACWatcher
// suitable for direct scheduler tests. The informer wiring is skipped;
// we drive scheduleCohortPrewarmFromBinding directly.
//
// Q-OOM-COMPLETION (v0.25.315) Patch 1 — installs a bidComputerOverride
// that assigns each username a unique BID. This preserves pre-Patch-1
// fan-out semantics (every user → a distinct cohort representative) so
// existing tests continue to assert their original expectations. Tests
// that exercise BID dedup explicitly install their own override.
func newTestRBACWatcher(c Cache) *RBACWatcher {
	rw := &RBACWatcher{cache: c}
	rw.bidComputerOverride = func(u string) string { return "bid-" + u }
	return rw
}

// fixtureRoleBinding builds a *rbacv1.RoleBinding with the given subjects.
func fixtureRoleBinding(name string, subjects ...rbacv1.Subject) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Subjects:   subjects,
	}
}

func userSubject(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.UserKind, Name: name}
}

func groupSubject(name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.GroupKind, Name: name}
}

// waitForCalls polls the mock for `want` calls or fails after timeout.
func waitForCalls(t *testing.T, m *mockPrewarmer, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, _ := m.snapshot()
		if c >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	c, _ := m.snapshot()
	t.Fatalf("waitForCalls: got %d, want %d after %v", c, want, timeout)
}

// TestRBACAdd_FiresPrewarmer_UserSubject — fundamental contract: a
// binding with one User subject fires the prewarmer once for that user
// after the debounce window elapses.
func TestRBACAdd_FiresPrewarmer_UserSubject(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := newTestRBACWatcher(NewMem(time.Hour))
	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	rb := fixtureRoleBinding("rb-1", userSubject("alice"))
	rw.scheduleCohortPrewarmFromBinding(ctx, rb)

	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)
	calls, users := mp.snapshot()
	if calls != 1 {
		t.Fatalf("calls: got %d, want 1", calls)
	}
	if len(users) != 1 || users[0] != "alice" {
		t.Errorf("users: got %v, want [alice]", users)
	}
}

// TestRBACAdd_DebounceCoalesces — many ADDs within rbacDebounceWindow
// produce ONE fan-out. This is the load-bearing storm-coalescing
// invariant: 50 RBs in <1s → 1 prewarmer call.
func TestRBACAdd_DebounceCoalesces(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := newTestRBACWatcher(NewMem(time.Hour))
	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	const N = 50
	for i := 0; i < N; i++ {
		rb := fixtureRoleBinding("rb-burst", userSubject("alice"))
		rw.scheduleCohortPrewarmFromBinding(ctx, rb)
	}

	// All ADDs should coalesce; wait for ONE call.
	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)

	// Sleep past the debounce window to ensure no second fire happens.
	time.Sleep(rbacDebounceWindow + 500*time.Millisecond)
	calls, users := mp.snapshot()
	if calls != 1 {
		t.Errorf("calls: got %d, want 1 (debounce should coalesce)", calls)
	}
	if len(users) != 1 || users[0] != "alice" {
		t.Errorf("users: got %v, want [alice]", users)
	}
}

// TestRBACAdd_DebounceCoalesces_Storm1004 is the PM-mandated G-QUEUE-
// DRAIN gate at the unit level: 1004 binding ADDs in a tight burst
// (matching the bench cluster's active-user count, simulating the
// initial-LIST storm + helm rollout) must coalesce into ONE fan-out
// covering the union of subjects. No saturation, no panic.
//
// Q-OOM-COMPLETION (v0.25.315) Patch 1 — post-dedup the fan-out is
// capped at cohortMaxFanoutDefault (32 representatives by default) even
// when the storm produces 1004 distinct BIDs. The newTestRBACWatcher
// stub assigns each user a unique BID, so the worst-case fan-out
// after dedup is exactly cohortMaxFanout(). Original test asserted 1004
// — that was the unbounded behaviour that triggered the OOM loop.
func TestRBACAdd_DebounceCoalesces_Storm1004(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := newTestRBACWatcher(NewMem(time.Hour))
	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	const N = 1004
	expected := make(map[string]struct{}, N)
	start := time.Now()
	for i := 0; i < N; i++ {
		u := usernameForBench(i)
		expected[u] = struct{}{}
		rb := fixtureRoleBinding("rb-storm", userSubject(u))
		rw.scheduleCohortPrewarmFromBinding(ctx, rb)
	}
	scheduleElapsed := time.Since(start)
	if scheduleElapsed > 5*time.Second {
		t.Errorf("schedule loop took too long: %v (must be non-blocking)", scheduleElapsed)
	}

	// Exactly one fan-out must fire after the debounce window.
	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)

	// Verify no spurious second fan-out within another window.
	time.Sleep(rbacDebounceWindow + 500*time.Millisecond)
	calls, users := mp.snapshot()
	if calls != 1 {
		t.Errorf("calls: got %d, want 1 (storm must coalesce)", calls)
	}
	cap := cohortMaxFanout()
	if len(users) > cap {
		t.Errorf("users: got %d unique, want ≤%d (Patch 1 cap)", len(users), cap)
	}
	if len(users) != cap {
		t.Errorf("users: got %d, want exactly %d (1004 unique BIDs vs cap=%d)", len(users), cap, cap)
	}
	for _, u := range users {
		if _, ok := expected[u]; !ok {
			t.Errorf("unexpected user in fan-out: %q", u)
			break
		}
	}
}

// TestRBACAdd_GroupSubjectExpansion — a binding with a Group subject
// fans out to ALL active users in the cache (Option A). Direct User
// subjects in the same burst are deduped against the active-users set.
func TestRBACAdd_GroupSubjectExpansion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := NewMem(time.Hour)
	// Seed 3 active users.
	for _, u := range []string{"alice", "bob", "carol"} {
		if err := c.SAddUser(ctx, u); err != nil {
			t.Fatalf("SAddUser(%q): %v", u, err)
		}
	}

	rw := newTestRBACWatcher(c)
	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	rb := fixtureRoleBinding("rb-devs", groupSubject("devs"))
	rw.scheduleCohortPrewarmFromBinding(ctx, rb)

	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)
	calls, users := mp.snapshot()
	if calls != 1 {
		t.Fatalf("calls: got %d, want 1", calls)
	}

	// Expansion must produce all 3 active users (Option A).
	got := make(map[string]bool, len(users))
	for _, u := range users {
		got[u] = true
	}
	for _, want := range []string{"alice", "bob", "carol"} {
		if !got[want] {
			t.Errorf("expected %q in expanded fan-out, got %v", want, users)
		}
	}
}

// TestRBACAdd_NoPrewarmerIsSafe — when SetPrewarmer was never called,
// the scheduler must not panic and must not schedule a no-op timer
// indefinitely.
func TestRBACAdd_NoPrewarmerIsSafe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := newTestRBACWatcher(NewMem(time.Hour))
	// No SetPrewarmer call.
	rb := fixtureRoleBinding("rb-1", userSubject("alice"))
	rw.scheduleCohortPrewarmFromBinding(ctx, rb)

	// Wait for the debounce timer to fire and discover the nil prewarmer.
	time.Sleep(rbacDebounceWindow + 500*time.Millisecond)
	// Subsequent ADDs should still be safe.
	rw.scheduleCohortPrewarmFromBinding(ctx, rb)
	time.Sleep(rbacDebounceWindow + 500*time.Millisecond)
	// If we got here without panic, the contract is satisfied.
}

// TestRBACAdd_RaceClean — concurrent ADDs from many goroutines must not
// corrupt the accumulator. Run under -race to flag map mutation
// races.
func TestRBACAdd_RaceClean(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := newTestRBACWatcher(NewMem(time.Hour))
	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	const G = 32
	const N = 100
	var wg sync.WaitGroup
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < N; i++ {
				u := usernameForBench(seed*N + i)
				rb := fixtureRoleBinding("rb-race", userSubject(u))
				rw.scheduleCohortPrewarmFromBinding(ctx, rb)
			}
		}(g)
	}
	wg.Wait()

	// Wait for the coalesced fan-out.
	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)
	calls, users := mp.snapshot()
	if calls != 1 {
		t.Errorf("calls: got %d, want 1 (storm + race)", calls)
	}
	// Q-OOM-COMPLETION (v0.25.315) Patch 1 — post-dedup the fan-out is
	// capped at cohortMaxFanout(). The newTestRBACWatcher stub assigns
	// each user a unique BID, so 3200 distinct BIDs collapse to the cap.
	// Original test asserted 3200 — that was the unbounded behaviour
	// that contributed to the OOM loop.
	cap := cohortMaxFanout()
	if len(users) > cap {
		t.Errorf("users: got %d unique, want ≤%d (Patch 1 cap)", len(users), cap)
	}
}

// TestRBACAdd_NonBindingObjectIgnored — extractSubjects refuses non-
// binding types; scheduler must skip without panic and without
// scheduling a fan-out.
func TestRBACAdd_NonBindingObjectIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := newTestRBACWatcher(NewMem(time.Hour))
	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	rw.scheduleCohortPrewarmFromBinding(ctx, "not a binding")
	rw.scheduleCohortPrewarmFromBinding(ctx, struct{}{})

	time.Sleep(rbacDebounceWindow + 500*time.Millisecond)
	calls, _ := mp.snapshot()
	if calls != 0 {
		t.Errorf("calls: got %d, want 0 (non-binding objects must be ignored)", calls)
	}
}

// TestRBACAdd_DropsArePropagated — when the prewarmer reports drops,
// the scheduler logs them but does not retry within the same window.
// This documents the contract: drops are absorbed by the next ADD or
// by the request-driven backstop (deferred per PM scope).
func TestRBACAdd_DropsArePropagated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := newTestRBACWatcher(NewMem(time.Hour))
	var dropCalls atomic.Int64
	mp := &mockPrewarmer{accepted: 0, dropped: 1, onCall: func() { dropCalls.Add(1) }}
	rw.SetPrewarmer(mp)

	rb := fixtureRoleBinding("rb-drop", userSubject("alice"))
	rw.scheduleCohortPrewarmFromBinding(ctx, rb)

	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)
	if dropCalls.Load() != 1 {
		t.Errorf("dropCalls: got %d, want 1", dropCalls.Load())
	}
}

// TestFireCohortPrewarm_DeduplicatesByBID — Q-OOM-COMPLETION Patch 1.
//
// 1000 candidates collapse to 4 distinct BIDs → exactly 4 enqueued.
// Without dedup the OOM bundle pre-Patch-1 enqueued 1000 jobs and
// peaked the cgroup at 16 GiB. With dedup we enqueue one representative
// per cohort transition.
func TestFireCohortPrewarm_DeduplicatesByBID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := NewMem(time.Hour)
	for i := 0; i < 1000; i++ {
		if err := c.SAddUser(ctx, usernameForBench(i)); err != nil {
			t.Fatalf("SAddUser: %v", err)
		}
	}

	rw := newTestRBACWatcher(c)
	// 4 distinct BIDs, hashed by index mod 4. Each unique BID has 250
	// candidate users.
	rw.bidComputerOverride = func(u string) string {
		// Lookup by name → bucket. Stable.
		var sum int
		for _, b := range []byte(u) {
			sum += int(b)
		}
		return "bid-bucket-" + strconv.Itoa(sum%4)
	}

	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	// Group subject expansion: all 1000 active users become candidates.
	rb := fixtureRoleBinding("rb-fanout", groupSubject("devs"))
	rw.scheduleCohortPrewarmFromBinding(ctx, rb)

	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)
	calls, users := mp.snapshot()
	if calls != 1 {
		t.Fatalf("calls: got %d, want 1", calls)
	}
	if len(users) != 4 {
		t.Errorf("dedup users: got %d, want 4 (one rep per BID)", len(users))
	}
	// Every dispatched username must map to a distinct BID.
	seenBids := make(map[string]bool, 4)
	for _, u := range users {
		bid := rw.bidComputerOverride(u)
		if seenBids[bid] {
			t.Errorf("duplicate BID dispatched: bid=%s user=%s users=%v", bid, u, users)
		}
		seenBids[bid] = true
	}
}

// TestFireCohortPrewarm_SkipsUnchangedBIDs — Q-OOM-COMPLETION Patch 1.
//
// First fire dispatches representatives. Second fire with the SAME BID
// per user dispatches NOTHING because all candidate BIDs match the
// memo from the first fire. This is the production-realistic case
// where a benign RB add doesn't actually expand any user's visibility.
func TestFireCohortPrewarm_SkipsUnchangedBIDs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := NewMem(time.Hour)
	for i := 0; i < 1000; i++ {
		if err := c.SAddUser(ctx, usernameForBench(i)); err != nil {
			t.Fatalf("SAddUser: %v", err)
		}
	}

	rw := newTestRBACWatcher(c)
	// One BID across all users — represents a no-op RB add (e.g. an RB
	// that doesn't bind to any User/Group subject the candidate users
	// match against).
	rw.bidComputerOverride = func(u string) string { return "bid-stable" }

	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	// First fire: 1 distinct BID across 1000 users → 1 representative.
	rb := fixtureRoleBinding("rb-1", groupSubject("devs"))
	rw.scheduleCohortPrewarmFromBinding(ctx, rb)
	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)
	calls, users := mp.snapshot()
	if calls != 1 || len(users) != 1 {
		t.Fatalf("first fire: calls=%d users=%d, want 1/1", calls, len(users))
	}

	// Wait past the debounce window so a SECOND fire is independent of
	// the first.
	time.Sleep(rbacDebounceWindow + 200*time.Millisecond)

	// Second fire: BID hasn't changed for any user → 0 enqueued.
	rb2 := fixtureRoleBinding("rb-2", groupSubject("devs"))
	rw.scheduleCohortPrewarmFromBinding(ctx, rb2)
	time.Sleep(rbacDebounceWindow + 500*time.Millisecond)

	calls2, _ := mp.snapshot()
	if calls2 != 1 {
		t.Errorf("second fire: total calls=%d, want 1 (the second fire must be a no-op because no BID changed)", calls2)
	}
}

// TestFireCohortPrewarm_RespectsMaxFanout — Q-OOM-COMPLETION Patch 1.
//
// 100 distinct BIDs with MAX_FANOUT=8 → exactly 8 enqueued. The cap
// protects against a runaway fan-out (e.g. helm rolling out 100 unique
// RoleBindings simultaneously) from overwhelming the worker pool.
func TestFireCohortPrewarm_RespectsMaxFanout(t *testing.T) {
	t.Setenv(envRBACCohortMaxFanout, "8")
	if cohortMaxFanout() != 8 {
		t.Fatalf("cohortMaxFanout(): got %d, want 8", cohortMaxFanout())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := NewMem(time.Hour)
	for i := 0; i < 100; i++ {
		if err := c.SAddUser(ctx, usernameForBench(i)); err != nil {
			t.Fatalf("SAddUser: %v", err)
		}
	}

	rw := newTestRBACWatcher(c)
	// Each user has a UNIQUE BID — worst-case fan-out.
	rw.bidComputerOverride = func(u string) string { return "bid-" + u }

	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	rb := fixtureRoleBinding("rb-explosion", groupSubject("devs"))
	rw.scheduleCohortPrewarmFromBinding(ctx, rb)

	waitForCalls(t, mp, 1, 3*rbacDebounceWindow)
	calls, users := mp.snapshot()
	if calls != 1 {
		t.Fatalf("calls: got %d, want 1", calls)
	}
	if len(users) > 8 {
		t.Errorf("max-fanout: got %d users, want ≤8", len(users))
	}
	if len(users) != 8 {
		t.Errorf("max-fanout: got %d users, want exactly 8 (cap == 8 with 100 distinct BIDs)", len(users))
	}
}

// TestFireCohortPrewarm_SyncedFalseIsNoop — when bidComputerOverride
// is nil and the watcher is not synced, ComputeBindingIdentity returns ""
// for all candidates → 0 enqueued. This is the pre-startup safety
// behaviour: don't dispatch fan-outs against an empty informer cache.
func TestFireCohortPrewarm_SyncedFalseIsNoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw := &RBACWatcher{cache: NewMem(time.Hour)} // no override, no listers
	mp := &mockPrewarmer{}
	rw.SetPrewarmer(mp)

	rb := fixtureRoleBinding("rb-presync", userSubject("alice"))
	rw.scheduleCohortPrewarmFromBinding(ctx, rb)

	time.Sleep(rbacDebounceWindow + 500*time.Millisecond)
	calls, _ := mp.snapshot()
	if calls != 0 {
		t.Errorf("pre-sync: got %d calls, want 0 (synced=false must skip dispatch)", calls)
	}
}

// usernameForBench yields a deterministic stable username for a given
// index. Avoids fmt.Sprintf in hot loops.
func usernameForBench(i int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz"
	return "u-" +
		string(alpha[i%len(alpha)]) +
		string(alpha[(i/len(alpha))%len(alpha)]) +
		string(alpha[(i/(len(alpha)*len(alpha)))%len(alpha)]) +
		string(alpha[(i/(len(alpha)*len(alpha)*len(alpha)))%len(alpha)])
}
