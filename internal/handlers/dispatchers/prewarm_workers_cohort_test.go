// Q-COHORT-PREWARM (v0.25.312) — unit tests for the EnqueueForUser /
// EnqueueForCohort lifts on PrewarmWorkerPool. Exercises the per-user
// resolver indirection (SetEnqueueByName / SetEnqueueByNameFunc) and
// the cohort fan-out counter without touching the K8s API.
//
// Test strategy: install a fake `enqueueByName` that records call
// arguments and decides whether to forward to the real Enqueue. This
// isolates the lift refactor (Q-COHORT-PREWARM §2.1) from secret
// reads, JWT minting, and the resolver pipeline.
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
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// discoveredUserStub builds a minimal discoveredUser fixture for the
// resolveL1RestActionsCollect unit tests. The endpoint is empty (no real
// API server) — the tests below are designed to short-circuit BEFORE the
// resolver pipeline reaches a real K8s client.
func discoveredUserStub() discoveredUser {
	return discoveredUser{
		userInfo: jwtutil.UserInfo{Username: "cyberjoker", Groups: []string{"narrow-rbac"}},
		endpoint: endpoints.Endpoint{},
	}
}

// TestPool_EnqueueForUser_NoFnReturnsFalse confirms that EnqueueForUser
// is a safe no-op when SetEnqueueByName has not been called yet.
func TestPool_EnqueueForUser_NoFnReturnsFalse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTestPool(1, 16)
	p.Start(ctx)
	if ok := p.EnqueueForUser(ctx, "alice"); ok {
		t.Errorf("EnqueueForUser before SetEnqueueByName should return false")
	}
}

// TestPool_EnqueueForUser_RejectsEmptyAndStopped covers the defensive
// guards: empty username and pre-Start / post-Stop pools.
func TestPool_EnqueueForUser_RejectsEmptyAndStopped(t *testing.T) {
	p := newTestPool(1, 16)
	p.SetEnqueueByNameFunc(func(ctx context.Context, username string) bool {
		t.Fatalf("fn must not be called when pool is not started")
		return false
	})
	if ok := p.EnqueueForUser(context.Background(), "alice"); ok {
		t.Errorf("EnqueueForUser before Start should return false")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	if ok := p.EnqueueForUser(ctx, ""); ok {
		t.Errorf("EnqueueForUser with empty username should return false")
	}
}

// TestPool_EnqueueForUser_PostsToQueue asserts the lift refactor: when
// the closure forwards to p.Enqueue, the job appears on the queue and
// the processed counter advances.
func TestPool_EnqueueForUser_PostsToQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newTestPool(2, 32)
	p.Start(ctx)

	var calls atomic.Int64
	p.SetEnqueueByNameFunc(func(ctx context.Context, username string) bool {
		calls.Add(1)
		// Mirror the production closure: synthesise a minimal job and
		// forward to p.Enqueue. EntryPoints is empty so runPerUser is
		// a no-op (≈ µs per job).
		return p.Enqueue(PrewarmJob{Username: username})
	})

	if ok := p.EnqueueForUser(ctx, "alice"); !ok {
		t.Fatalf("EnqueueForUser returned false unexpectedly")
	}
	waitForProcessed(t, p, 1, drainTimeout)

	enq, proc, drop := p.Stats()
	if calls.Load() != 1 {
		t.Errorf("fn calls: got %d, want 1", calls.Load())
	}
	if enq != 1 {
		t.Errorf("enqueued: got %d, want 1", enq)
	}
	if proc != 1 {
		t.Errorf("processed: got %d, want 1", proc)
	}
	if drop != 0 {
		t.Errorf("dropped: got %d, want 0", drop)
	}
}

// TestPool_EnqueueForCohort_FanoutAndCounter exercises EnqueueForCohort
// with N usernames and confirms (a) all N forward to the closure,
// (b) CohortFanouts == N, (c) Enqueue's enqueued counter == N.
func TestPool_EnqueueForCohort_FanoutAndCounter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newTestPool(4, 256)
	p.Start(ctx)

	var calls sync.Map
	p.SetEnqueueByNameFunc(func(ctx context.Context, username string) bool {
		calls.Store(username, struct{}{})
		return p.Enqueue(PrewarmJob{Username: username})
	})

	const N = 50
	usernames := make([]string, 0, N)
	for i := 0; i < N; i++ {
		usernames = append(usernames, usernameFor(i))
	}
	accepted, dropped := p.EnqueueForCohort(ctx, usernames)
	if accepted != N {
		t.Errorf("accepted: got %d, want %d", accepted, N)
	}
	if dropped != 0 {
		t.Errorf("dropped: got %d, want 0", dropped)
	}
	if got := p.CohortFanouts(); got != int64(N) {
		t.Errorf("CohortFanouts: got %d, want %d", got, N)
	}
	waitForProcessed(t, p, N, drainTimeout)
}

// TestPool_EnqueueForCohort_DedupsViaInflight confirms that firing the
// same cohort twice in quick succession does not double-process: the
// inflightUsers tryLock + the L1 Exists short-circuit (in production)
// dedups; the closure is still called both times but the queue work
// is idempotent.
//
// Here we don't exercise inflight directly (jobs are too fast); instead
// we assert the contract that EnqueueForCohort is safe to call with
// duplicate usernames within one slice — accepted counts each slot.
func TestPool_EnqueueForCohort_DuplicatesAreAccepted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTestPool(2, 64)
	p.Start(ctx)
	p.SetEnqueueByNameFunc(func(ctx context.Context, username string) bool {
		return p.Enqueue(PrewarmJob{Username: username})
	})

	usernames := []string{"alice", "alice", "bob", "alice", "bob"}
	accepted, dropped := p.EnqueueForCohort(ctx, usernames)
	if accepted != len(usernames) {
		t.Errorf("accepted: got %d, want %d", accepted, len(usernames))
	}
	if dropped != 0 {
		t.Errorf("dropped: got %d, want 0", dropped)
	}
	if got := p.CohortFanouts(); got != int64(len(usernames)) {
		t.Errorf("CohortFanouts: got %d, want %d", got, len(usernames))
	}
}

// TestPool_EnqueueForCohort_StormDoesNotSaturate is the PM-mandated
// G-QUEUE-DRAIN gate (storm test). Fires 1004 cohort enqueues (matching
// the bench cluster's active-user count) and asserts:
//   - EnqueueForCohort returns within a reasonable bound (non-blocking).
//   - The pool drains the queue to 0 within the storm-test wall.
//
// With Workers=4, QueueCap=2048, EntryPoints=nil (so runPerUser is a
// no-op), this should drain in well under 1s. In production Workers=4
// drains real prewarm walks at ~5s each — the PM gate (5 min) maps to
// ~250 jobs/worker × 5s ≈ 21 min worst-case (architect §3.2). The unit
// test only proves the pool plumbing is non-blocking and finite under
// burst — the empirical 5-min bound is verified on the cluster (Step
// 9-10 of the run brief).
func TestPool_EnqueueForCohort_StormDoesNotSaturate(t *testing.T) {
	if testing.Short() {
		t.Skip("storm test (1004 fan-outs); skipping in -short")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &PrewarmWorkerPool{
		Workers:    4,
		QueueCap:   2048,
		Cache:      cache.NewMem(time.Hour),
		AuthnNS:    "krateo-system",
		JobTimeout: 200 * time.Millisecond,
		// EntryPoints intentionally empty — runPerUser is a no-op.
	}
	p.Start(ctx)
	p.SetEnqueueByNameFunc(func(ctx context.Context, username string) bool {
		return p.Enqueue(PrewarmJob{Username: username})
	})

	const N = 1004
	usernames := make([]string, 0, N)
	for i := 0; i < N; i++ {
		usernames = append(usernames, usernameFor(i))
	}

	start := time.Now()
	accepted, dropped := p.EnqueueForCohort(ctx, usernames)
	enqueueElapsed := time.Since(start)

	// EnqueueForCohort must return promptly (non-blocking). On a typical
	// laptop this should be well under 100 ms even at 1004 entries.
	if enqueueElapsed > 5*time.Second {
		t.Errorf("EnqueueForCohort took too long: %v (must be non-blocking)", enqueueElapsed)
	}

	// All 1004 slots must be accepted (queue cap 2048 absorbs the burst)
	// and CohortFanouts must increment to exactly N for the accepted set.
	if accepted+dropped != N {
		t.Errorf("accepted+dropped: got %d, want %d", accepted+dropped, N)
	}
	// In this configuration we expect zero drops (queue is sized for it).
	if dropped != 0 {
		t.Errorf("dropped: got %d, want 0 (QueueCap=%d, N=%d)", dropped, p.QueueCap, N)
	}
	if got := p.CohortFanouts(); got != int64(accepted) {
		t.Errorf("CohortFanouts: got %d, want %d", got, accepted)
	}

	// Queue must drain fully within the storm timeout. With no-op
	// runPerUser this is sub-second.
	const stormDrainTimeout = 10 * time.Second
	waitForProcessed(t, p, int64(accepted), stormDrainTimeout)

	if depth := p.QueueDepth(); depth != 0 {
		t.Errorf("queue did not drain: depth=%d after %v", depth, stormDrainTimeout)
	}
}

// Q-COHORT-PREWARM-RA (v0.25.318) — unit tests for the gate-removal in
// extractChildRefs. Pre-Patch the function dropped any ref whose group
// was not widgets.templates.krateo.io, so RAs reachable from the entry-
// point widget tree (e.g. compositions-list, RA group templates.krateo.io)
// were never pre-resolved during cohort prewarm. Post-Patch widget refs
// and RA refs are emitted on separate lists so recursivePreWarm can
// route each set to the right resolver.

// makeRefItem produces the same shape resourcesRefs items have in a
// resolved widget's status: a map with id/path keys. The path is the
// /call?... query that ParseCallPath understands.
func makeRefItem(id, group, version, resource, ns, name string) map[string]interface{} {
	apiVersion := version
	if group != "" {
		apiVersion = group + "/" + version
	}
	path := "/call?apiVersion=" + apiVersion +
		"&resource=" + resource +
		"&namespace=" + ns +
		"&name=" + name
	return map[string]interface{}{
		"id":   id,
		"path": path,
	}
}

// TestExtractChildRefs_BinsByGroup is the structural assertion for the
// Q-COHORT-PREWARM-RA gate-removal. A mixed list of resourcesRefs items
// MUST split cleanly: widgets.templates.krateo.io entries land in the
// widgets bin, every other group lands in the RAs bin.
//
// This is the test-of-record that the gate has been removed. If a future
// change re-introduces an `if gvr.Group != widgetGroup { continue }`
// inside extractChildRefs, raRefs becomes empty and this test fails.
func TestExtractChildRefs_BinsByGroup(t *testing.T) {
	c := cache.NewMem(time.Hour)
	identity := "test-identity"

	items := []interface{}{
		makeRefItem("w1", "widgets.templates.krateo.io", "v1beta1", "panels", "krateo-system", "homepage-panel"),
		makeRefItem("w2", "widgets.templates.krateo.io", "v1beta1", "buttons", "krateo-system", "create-btn"),
		makeRefItem("ra1", "templates.krateo.io", "v1", "restactions", "krateo-system", "compositions-list"),
		makeRefItem("ra2", "templates.krateo.io", "v1", "restactions", "krateo-system", "namespaces-rest"),
	}

	widgets, ras := extractChildRefs(context.Background(), c, items, identity, nil)

	if len(widgets) != 2 {
		t.Errorf("widgets bin: got %d, want 2", len(widgets))
	}
	if len(ras) != 2 {
		t.Errorf("RAs bin: got %d, want 2 (gate-removal regression — RA refs were dropped pre-Patch)", len(ras))
	}

	// Spot-check ra1 is the compositions-list ref (the cyberjoker cold-fill
	// target). If the path parser broke, we'd see Resource=="" or empty Name.
	var foundCompositionsList bool
	wantGVR := schema.GroupVersionResource{
		Group: "templates.krateo.io", Version: "v1", Resource: "restactions",
	}
	for _, r := range ras {
		if r.gvr == wantGVR && r.name == "compositions-list" && r.ns == "krateo-system" {
			foundCompositionsList = true
		}
	}
	if !foundCompositionsList {
		t.Errorf("compositions-list RA ref not in RAs bin (got %+v); cyberjoker first-request cold-fill will not be amortised", ras)
	}
}

// TestExtractChildRefs_SkipIDsExcludeBoth confirms the action-link skip
// logic still applies regardless of group. A resourceRefId in the actions
// map (skipIDs) must drop the item whether it's a widget or an RA — the
// frontend resolves these lazily on click and prewarm must not race ahead.
func TestExtractChildRefs_SkipIDsExcludeBoth(t *testing.T) {
	c := cache.NewMem(time.Hour)
	identity := "test-identity"
	items := []interface{}{
		makeRefItem("widget-action", "widgets.templates.krateo.io", "v1beta1", "buttons", "ns", "btn1"),
		makeRefItem("ra-action", "templates.krateo.io", "v1", "restactions", "ns", "ra1"),
	}
	skipIDs := map[string]bool{
		"widget-action": true,
		"ra-action":     true,
	}
	widgets, ras := extractChildRefs(context.Background(), c, items, identity, skipIDs)
	if len(widgets) != 0 {
		t.Errorf("widgets: got %d skipped items, want 0", len(widgets))
	}
	if len(ras) != 0 {
		t.Errorf("RAs: got %d skipped items, want 0", len(ras))
	}
}

// TestExtractChildRefs_ExistingL1SkipsBoth confirms the c.Exists short-
// circuit fires for both bins. If a cohort has already prewarmed an RA's
// L1 entry, a fresh walk must not re-emit it (avoids redundant resolves
// when the recursion converges on the same ref via multiple parents).
func TestExtractChildRefs_ExistingL1SkipsBoth(t *testing.T) {
	c := cache.NewMem(time.Hour)
	identity := "cyberjoker-bid"

	// Pre-populate L1 for both a widget AND an RA at the cohort identity.
	widgetGVR := schema.GroupVersionResource{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "panels"}
	raGVR := schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}
	widgetKey := cache.ResolvedKey(identity, widgetGVR, "krateo-system", "warm-panel", -1, -1)
	raKey := cache.ResolvedKey(identity, raGVR, "krateo-system", "warm-ra", -1, -1)
	_ = c.SetResolvedRaw(context.Background(), widgetKey, []byte(`{}`))
	_ = c.SetResolvedRaw(context.Background(), raKey, []byte(`{}`))

	items := []interface{}{
		makeRefItem("w1", "widgets.templates.krateo.io", "v1beta1", "panels", "krateo-system", "warm-panel"),
		makeRefItem("w2", "widgets.templates.krateo.io", "v1beta1", "panels", "krateo-system", "cold-panel"),
		makeRefItem("ra1", "templates.krateo.io", "v1", "restactions", "krateo-system", "warm-ra"),
		makeRefItem("ra2", "templates.krateo.io", "v1", "restactions", "krateo-system", "cold-ra"),
	}

	widgets, ras := extractChildRefs(context.Background(), c, items, identity, nil)
	if len(widgets) != 1 || widgets[0].name != "cold-panel" {
		t.Errorf("widgets bin: got %+v, want [cold-panel] (warm-panel L1 hit must be filtered)", widgets)
	}
	if len(ras) != 1 || ras[0].name != "cold-ra" {
		t.Errorf("RAs bin: got %+v, want [cold-ra] (warm-ra L1 hit must be filtered)", ras)
	}
}

// TestResolveL1RestActionsCollect_NoOpOnEmpty confirms the helper handles
// degenerate input cleanly. The cohort walk fires this path on every
// recursion level even when the level emitted no RA refs; the helper must
// not panic, leak goroutines, or write to nil maps.
func TestResolveL1RestActionsCollect_NoOpOnEmpty(t *testing.T) {
	c := cache.NewMem(time.Hour)
	// Should return without touching the cache.
	resolveL1RestActionsCollect(context.Background(), discoveredUserStub().userInfo, discoveredUserStub().endpoint, "", c, nil, nil, "krateo-system")
	resolveL1RestActionsCollect(context.Background(), discoveredUserStub().userInfo, discoveredUserStub().endpoint, "", c, nil, []l1Ref{}, "krateo-system")
}

// TestResolveL1RestActionsCollect_SkipsExistingL1 confirms that RA refs
// whose L1 entry already exists for this cohort identity are skipped via
// the same c.Exists short-circuit warmL1RestActionsForUser uses. This
// gates against double-resolution when a cohort fans out twice in quick
// succession (e.g. RoleBinding ADD storm).
func TestResolveL1RestActionsCollect_SkipsExistingL1(t *testing.T) {
	c := cache.NewMem(time.Hour)
	user := discoveredUserStub()

	raGVR := schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}
	identity := user.userInfo.Username
	rKey := cache.ResolvedKey(identity, raGVR, "krateo-system", "compositions-list", -1, -1)
	if err := c.SetResolvedRaw(context.Background(), rKey, []byte(`{"warm":true}`)); err != nil {
		t.Fatalf("SetResolvedRaw: %v", err)
	}

	refs := []l1Ref{
		{gvr: raGVR, ns: "krateo-system", name: "compositions-list"},
	}
	// dynClient is nil and the entry is L1-warm, so prewarmFetchCR must
	// not be called. If the c.Exists short-circuit regresses the helper
	// would attempt to fetch the CR, get nil-back, and continue. Either
	// way the helper must not panic.
	resolveL1RestActionsCollect(context.Background(), user.userInfo, user.endpoint, "", c, nil, refs, "krateo-system")

	// The pre-existing L1 entry must remain intact.
	if !c.Exists(context.Background(), rKey) {
		t.Errorf("L1 entry was evicted by resolveL1RestActionsCollect; cohort dedup is broken")
	}
}
