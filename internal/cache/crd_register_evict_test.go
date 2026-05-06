// Q-PREWARM-R5 convergence fix — unit tests for the per-group L1
// reverse index and EvictL1KeysForGroup. The bug being fixed:
//
//   1. R5 prewarm fires when admin's secret ADD event arrives.
//   2. If a customer CRD doesn't yet exist, the iterator-based RESTAction
//      returns 0 namespaces, registers ZERO cluster-deps, and writes an
//      L1 entry keyed on a (now-incorrect) empty result.
//   3. When the CRD later appears via autoRegisterCRDInformer, watch
//      events for new CRs find an empty L1ResourceDepKey set and never
//      refresh the L1 entry — it stays stale until ResolvedCacheTTL
//      expires (1 hour).
//
// The fix: register every L1 resolved key in a per-group SET keyed on
// L1ResourceDepGroupKey(group). When autoRegisterCRDInformer runs, it
// evicts every L1 key in the new CRD's group via EvictL1KeysForGroup.
// Subsequent HTTP requests re-resolve at the correct cluster state and
// re-register cluster-deps; watch events then converge normally.
package cache

import (
	"context"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestL1ResourceDepGroupKey_NormalizesEmptyGroup verifies that the empty
// API group ("") is encoded as "core" — matching the GVRToKey convention.
func TestL1ResourceDepGroupKey_NormalizesEmptyGroup(t *testing.T) {
	if got := L1ResourceDepGroupKey(""); got != "snowplow:l1dep-group:core" {
		t.Errorf("empty group: got %q, want snowplow:l1dep-group:core", got)
	}
	if got := L1ResourceDepGroupKey("templates.krateo.io"); got != "snowplow:l1dep-group:templates.krateo.io" {
		t.Errorf("namespaced group: got %q, want snowplow:l1dep-group:templates.krateo.io", got)
	}
}

// TestEvictL1KeysForGroup_NilCache covers the defensive nil branch.
func TestEvictL1KeysForGroup_NilCache(t *testing.T) {
	if n := EvictL1KeysForGroup(context.Background(), nil, "templates.krateo.io"); n != 0 {
		t.Errorf("nil cache eviction should return 0, got %d", n)
	}
}

// TestEvictL1KeysForGroup_EmptySet verifies that an absent / empty SET
// for the group is a no-op. This is the no-op branch the architect
// asked for under "Empty group SET → no-op".
func TestEvictL1KeysForGroup_EmptySet(t *testing.T) {
	c := NewMem(0)
	ctx := context.Background()
	if n := EvictL1KeysForGroup(ctx, c, "templates.krateo.io"); n != 0 {
		t.Errorf("empty group set: got %d, want 0", n)
	}
}

// TestEvictL1KeysForGroup_EvictsAllRegisteredKeys covers the primary
// happy path. Register N L1 keys via RegisterL1Dependencies for the same
// group, then evict — all N must be gone, the per-group SET must be
// gone, and unrelated-group keys must be preserved.
func TestEvictL1KeysForGroup_EvictsAllRegisteredKeys(t *testing.T) {
	c := NewMem(0)
	ctx := context.Background()

	templatesGVR := schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}
	widgetsGVR := schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "widgets"}
	coreGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

	// Register 3 L1 keys touching templates.krateo.io GVRs.
	templateKeys := []string{
		ResolvedKey("admin", templatesGVR, "ns-01", "compositions-list", 0, 0),
		ResolvedKey("admin", templatesGVR, "ns-01", "piechart", 0, 0),
		// A widget L1 entry that depends on a templates.krateo.io GVR
		// via an apiref — same group, different resource.
		ResolvedKey("user-1", widgetsGVR, "ns-02", "my-widget", 0, 0),
	}
	for i, k := range templateKeys {
		if err := c.SetResolvedRaw(ctx, k, []byte(`{"items":[]}`)); err != nil {
			t.Fatalf("seed[%d]: %v", i, err)
		}
		tr := NewDependencyTracker()
		if i < 2 {
			tr.AddGVR(templatesGVR)
			tr.AddResource(templatesGVR, "ns-01", "")
		} else {
			tr.AddGVR(widgetsGVR)
			tr.AddResource(widgetsGVR, "ns-02", "my-widget")
		}
		RegisterL1Dependencies(ctx, c, tr, k)
	}

	// Register one L1 key in the core group — must NOT be evicted.
	coreKey := ResolvedKey("admin", coreGVR, "krateo-system", "admin-clientconfig", 0, 0)
	if err := c.SetResolvedRaw(ctx, coreKey, []byte(`{}`)); err != nil {
		t.Fatalf("seed core: %v", err)
	}
	tr := NewDependencyTracker()
	tr.AddGVR(coreGVR)
	tr.AddResource(coreGVR, "krateo-system", "admin-clientconfig")
	RegisterL1Dependencies(ctx, c, tr, coreKey)

	// Sanity: per-group SET is populated for templates.krateo.io.
	members, err := c.SMembers(ctx, L1ResourceDepGroupKey("templates.krateo.io"))
	if err != nil {
		t.Fatalf("smembers templates: %v", err)
	}
	if len(members) != len(templateKeys) {
		t.Errorf("expected %d members in templates set, got %d (members=%v)", len(templateKeys), len(members), members)
	}

	// Evict.
	evicted := EvictL1KeysForGroup(ctx, c, "templates.krateo.io")
	if evicted != len(templateKeys) {
		t.Errorf("eviction count: got %d, want %d", evicted, len(templateKeys))
	}

	// All template L1 keys must be gone.
	for _, k := range templateKeys {
		if _, hit, _ := c.GetRaw(ctx, k); hit {
			t.Errorf("template L1 key %q should be evicted, still present", k)
		}
	}
	// Core L1 key must be preserved.
	if _, hit, _ := c.GetRaw(ctx, coreKey); !hit {
		t.Errorf("core L1 key %q must NOT be evicted (different group)", coreKey)
	}
	// Per-group SET itself must be gone.
	postMembers, _ := c.SMembers(ctx, L1ResourceDepGroupKey("templates.krateo.io"))
	if len(postMembers) != 0 {
		t.Errorf("post-eviction templates group set must be empty, got %v", postMembers)
	}
	// Core per-group SET must still hold its member.
	coreMembers, _ := c.SMembers(ctx, L1ResourceDepGroupKey(""))
	if len(coreMembers) != 1 || coreMembers[0] != coreKey {
		t.Errorf("core group set: got %v, want [%s]", coreMembers, coreKey)
	}
}

// TestEvictL1KeysForGroup_PaginatedBaseKeyAlsoEvicted verifies that the
// unpaginated base key (registered alongside the paginated variant) is
// also evicted. This is the same pattern the per-resource and cluster
// deps already follow.
func TestEvictL1KeysForGroup_PaginatedBaseKeyAlsoEvicted(t *testing.T) {
	c := NewMem(0)
	ctx := context.Background()
	gvr := schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}

	pagedKey := ResolvedKey("admin", gvr, "ns-01", "compositions-list", 1, 10)
	baseKey := ResolvedKeyBase("admin", gvr, "ns-01", "compositions-list")

	if err := c.SetResolvedRaw(ctx, pagedKey, []byte(`{"items":[],"page":1}`)); err != nil {
		t.Fatalf("seed paged: %v", err)
	}
	if err := c.SetResolvedRaw(ctx, baseKey, []byte(`{"items":[]}`)); err != nil {
		t.Fatalf("seed base: %v", err)
	}

	tr := NewDependencyTracker()
	tr.AddGVR(gvr)
	tr.AddResource(gvr, "ns-01", "")
	RegisterL1Dependencies(ctx, c, tr, pagedKey)

	evicted := EvictL1KeysForGroup(ctx, c, "templates.krateo.io")
	if evicted != 2 {
		t.Errorf("expected 2 keys evicted (paged + base), got %d", evicted)
	}
	if _, hit, _ := c.GetRaw(ctx, pagedKey); hit {
		t.Errorf("paginated key %q must be evicted", pagedKey)
	}
	if _, hit, _ := c.GetRaw(ctx, baseKey); hit {
		t.Errorf("base key %q must be evicted (paginated variant registered it)", baseKey)
	}
}

// TestEvictL1KeysForGroup_DedupAcrossGVRsInGroup verifies that an L1
// key touching MULTIPLE GVRs in the same group is registered ONCE in
// the per-group SET (not once per ref/GVR). EvictL1KeysForGroup must
// still report a single eviction for that L1 key.
func TestEvictL1KeysForGroup_DedupAcrossGVRsInGroup(t *testing.T) {
	c := NewMem(0)
	ctx := context.Background()
	raGVR := schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}
	wGVR := schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "widgets"}

	l1 := ResolvedKey("admin", wGVR, "ns-01", "my-widget", 0, 0)
	if err := c.SetResolvedRaw(ctx, l1, []byte("{}")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tr := NewDependencyTracker()
	tr.AddGVR(raGVR)
	tr.AddGVR(wGVR)
	tr.AddResource(raGVR, "ns-01", "compositions-list")
	tr.AddResource(wGVR, "ns-01", "my-widget")
	RegisterL1Dependencies(ctx, c, tr, l1)

	members, _ := c.SMembers(ctx, L1ResourceDepGroupKey("templates.krateo.io"))
	if len(members) != 1 {
		t.Errorf("expected 1 member after dedup, got %d (members=%v)", len(members), members)
	}

	evicted := EvictL1KeysForGroup(ctx, c, "templates.krateo.io")
	if evicted != 1 {
		t.Errorf("expected 1 eviction after dedup, got %d", evicted)
	}
}

// TestRegisterL1Dependencies_Race covers the architect's "Race: register
// L1 dep + autoRegisterCRDInformer in parallel → eventual consistency"
// requirement. Two goroutines hammer the per-group SET: one registers
// new L1 keys for the group while the other repeatedly evicts. The
// invariant we check is liveness — no panic, no deadlock, and the
// FINAL eviction empties whatever was last written. There is an
// inherent TOCTOU window between SMembers and Delete that may cause
// some keys registered AFTER the SMembers snapshot to survive the
// eviction; that is acceptable per EvictL1KeysForGroup's documented
// semantics: such keys were resolved AFTER the CRD became visible
// to the writer.
func TestEvictL1KeysForGroup_ConcurrentRegisterAndEvict(t *testing.T) {
	c := NewMem(0)
	ctx := context.Background()
	gvr := schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}

	const N = 200
	keys := make([]string, N)
	for i := 0; i < N; i++ {
		k := ResolvedKey("u", gvr, "ns", "ra-"+itoa(i), 0, 0)
		keys[i] = k
		_ = c.SetResolvedRaw(ctx, k, []byte("{}"))
	}

	var wg sync.WaitGroup
	wg.Add(2)
	// Writer goroutine: register L1 deps for all N keys.
	go func() {
		defer wg.Done()
		for _, k := range keys {
			tr := NewDependencyTracker()
			tr.AddGVR(gvr)
			tr.AddResource(gvr, "ns", "")
			RegisterL1Dependencies(ctx, c, tr, k)
		}
	}()
	// Evictor goroutine: spin evictions in parallel.
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = EvictL1KeysForGroup(ctx, c, "templates.krateo.io")
		}
	}()
	wg.Wait()

	// Drain to a quiescent state — eventual consistency.
	_ = EvictL1KeysForGroup(ctx, c, "templates.krateo.io")
	final, _ := c.SMembers(ctx, L1ResourceDepGroupKey("templates.krateo.io"))
	if len(final) != 0 {
		t.Errorf("after final drain, group set must be empty, got %d members", len(final))
	}
}

// TestGvrKeyGroup_Forms verifies the gvr-key parser used by
// RegisterL1Dependencies to populate the per-group SET.
func TestGvrKeyGroup_Forms(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"core/v1/secrets", "core"},
		{"templates.krateo.io/v1/restactions", "templates.krateo.io"},
		{"", ""},
		{"malformed", ""},
		{"/v1/foo", ""}, // empty group, idx==0 → ""
	}
	for _, c := range cases {
		if got := gvrKeyGroup(c.in); got != c.want {
			t.Errorf("gvrKeyGroup(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// itoa is a tiny strconv-free helper to avoid importing strconv solely
// for tests; keeps the test file dependency-free.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
