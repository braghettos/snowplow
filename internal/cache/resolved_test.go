// resolved_test.go — Tag 0.30.7 binding: unit coverage for the
// resolved-output cache scaffold. Plan §"What's implemented" calls for
// hit/miss accounting, bounded-cache LRU eviction at cap, byte-budget
// eviction at cap, and basic Get/Put.
//
// We additionally cover:
//   - ResolvedCacheEnabled() obeys CACHE_ENABLED + RESOLVED_CACHE_ENABLED
//   - ComputeKey is stable across calls and sensitive to every input
//   - TTL expiry behaves like a miss + drops the entry
//   - Concurrent Get/Put is race-detector clean
//
// Each test resets the package singleton + env vars to avoid order
// dependencies.

package cache

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResolvedCacheEnabled_CacheDisabledMeansL1Off(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "false")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	if ResolvedCacheEnabled() {
		t.Fatalf("ResolvedCacheEnabled() should be false when CACHE_ENABLED=false (cache subsystem off)")
	}
	if ResolvedCache() != nil {
		t.Fatalf("ResolvedCache() should return nil when CACHE_ENABLED=false")
	}
}

func TestResolvedCacheEnabled_PerFeatureToggleOff(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "false")
	if ResolvedCacheEnabled() {
		t.Fatalf("RESOLVED_CACHE_ENABLED=false must disable L1 even when CACHE_ENABLED=true")
	}
}

func TestResolvedCacheEnabled_DefaultsOnWhenCacheEnabledOn(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "")
	if !ResolvedCacheEnabled() {
		t.Fatalf("default for RESOLVED_CACHE_ENABLED should be ON when CACHE_ENABLED=true")
	}
}

func TestComputeKey_StableAcrossCalls(t *testing.T) {
	in := ResolvedKeyInputs{
		HandlerKind: "widgets",
		Group:       "widgets.templates.krateo.io",
		Version:     "v1beta1",
		Resource:    "compositionsgrids",
		Namespace:   "demo",
		Name:        "main",
		Username:    "alice",
		Groups:      []string{"users", "admins"},
		PerPage:     20,
		Page:        1,
		Extras:      map[string]any{"foo": "bar", "n": float64(7)},
	}
	a := ComputeKey(in)
	b := ComputeKey(in)
	if a != b {
		t.Fatalf("ComputeKey not stable: %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("ComputeKey should be 64 hex chars (sha256), got %d", len(a))
	}
}

func TestComputeKey_GroupOrderInvariant(t *testing.T) {
	in1 := ResolvedKeyInputs{Username: "alice", Groups: []string{"a", "b", "c"}}
	in2 := ResolvedKeyInputs{Username: "alice", Groups: []string{"c", "a", "b"}}
	if ComputeKey(in1) != ComputeKey(in2) {
		t.Fatalf("ComputeKey should be invariant under group order; got divergent keys")
	}
}

func TestComputeKey_SensitiveToEveryField(t *testing.T) {
	base := ResolvedKeyInputs{
		HandlerKind: "widgets", Group: "g", Version: "v", Resource: "r",
		Namespace: "ns", Name: "n", Username: "u", Groups: []string{"x"},
		PerPage: 1, Page: 1, Extras: map[string]any{"k": "v"},
	}
	mutators := []struct {
		name string
		fn   func(*ResolvedKeyInputs)
	}{
		{"HandlerKind", func(in *ResolvedKeyInputs) { in.HandlerKind = "restactions" }},
		{"Group", func(in *ResolvedKeyInputs) { in.Group = "g2" }},
		{"Version", func(in *ResolvedKeyInputs) { in.Version = "v2" }},
		{"Resource", func(in *ResolvedKeyInputs) { in.Resource = "r2" }},
		{"Namespace", func(in *ResolvedKeyInputs) { in.Namespace = "ns2" }},
		{"Name", func(in *ResolvedKeyInputs) { in.Name = "n2" }},
		{"Username", func(in *ResolvedKeyInputs) { in.Username = "u2" }},
		{"Groups", func(in *ResolvedKeyInputs) { in.Groups = []string{"y"} }},
		{"PerPage", func(in *ResolvedKeyInputs) { in.PerPage = 2 }},
		{"Page", func(in *ResolvedKeyInputs) { in.Page = 2 }},
		{"Extras", func(in *ResolvedKeyInputs) { in.Extras = map[string]any{"k": "w"} }},
		{"Stage", func(in *ResolvedKeyInputs) { in.Stage = "stage\x1fcompositions\x1f\x1f" }},
	}
	baseKey := ComputeKey(base)
	for _, m := range mutators {
		t.Run(m.name, func(t *testing.T) {
			mutated := base
			// deep-copy slices/maps that mutators rebind
			mutated.Groups = append([]string(nil), base.Groups...)
			mutated.Extras = map[string]any{}
			for k, v := range base.Extras {
				mutated.Extras[k] = v
			}
			m.fn(&mutated)
			if ComputeKey(mutated) == baseKey {
				t.Fatalf("changing %s did not change the key — coalesced inputs", m.name)
			}
		})
	}
}

func TestResolvedCache_BasicGetPut(t *testing.T) {
	c := newResolvedCache(10, 1<<20, time.Hour)
	if _, ok := c.Get("nope"); ok {
		t.Fatalf("Get on empty cache must miss")
	}
	c.Put("k", &ResolvedEntry{RawJSON: []byte(`{"a":1}`)})
	got, ok := c.Get("k")
	if !ok {
		t.Fatalf("Get after Put should hit")
	}
	if string(got.RawJSON) != `{"a":1}` {
		t.Fatalf("RawJSON round-trip wrong: %q", got.RawJSON)
	}
	s := c.Stats()
	if s.HitTotal != 1 || s.MissTotal != 1 || s.StoreTotal != 1 {
		t.Fatalf("counters wrong: %+v", s)
	}
}

func TestResolvedCache_LRUEvictionAtEntryCap(t *testing.T) {
	c := newResolvedCache(3, 1<<30, time.Hour) // 3 entries, generous bytes
	// Insert 3 entries.
	for i := 0; i < 3; i++ {
		c.Put(fmt.Sprintf("k%d", i), &ResolvedEntry{RawJSON: []byte("x")})
	}
	if c.Len() != 3 {
		t.Fatalf("expected 3 entries, got %d", c.Len())
	}
	// Touch k0 to make it MRU.
	c.Get("k0")
	// Add a fourth — k1 (now LRU) should evict.
	c.Put("k3", &ResolvedEntry{RawJSON: []byte("x")})
	if _, ok := c.Get("k1"); ok {
		t.Fatalf("k1 should have been LRU-evicted")
	}
	if _, ok := c.Get("k0"); !ok {
		t.Fatalf("k0 was touched so should survive")
	}
	if c.Stats().EvictLRUTotal != 1 {
		t.Fatalf("expected 1 LRU eviction, got %d", c.Stats().EvictLRUTotal)
	}
}

func TestResolvedCache_ByteBudgetEviction(t *testing.T) {
	// Entries are ~100 bytes; budget = 250 bytes -> at most 2 fit.
	c := newResolvedCache(1000, 250, time.Hour)
	payload := strings.Repeat("a", 100)
	c.Put("k1", &ResolvedEntry{RawJSON: []byte(payload)})
	c.Put("k2", &ResolvedEntry{RawJSON: []byte(payload)})
	c.Put("k3", &ResolvedEntry{RawJSON: []byte(payload)})
	if got := c.Bytes(); got > 250 {
		t.Fatalf("byte budget violated: %d > 250", got)
	}
	if _, ok := c.Get("k1"); ok {
		t.Fatalf("k1 should have been evicted by byte-budget pressure")
	}
}

func TestResolvedCache_ReplaceInPlace(t *testing.T) {
	c := newResolvedCache(10, 1<<20, time.Hour)
	c.Put("k", &ResolvedEntry{RawJSON: []byte("aaa")})
	c.Put("k", &ResolvedEntry{RawJSON: []byte("bbbbb")})
	if c.Len() != 1 {
		t.Fatalf("expected 1 entry after replace, got %d", c.Len())
	}
	if got := c.Bytes(); got != 5 {
		t.Fatalf("expected 5 bytes after replace, got %d", got)
	}
	got, ok := c.Get("k")
	if !ok || string(got.RawJSON) != "bbbbb" {
		t.Fatalf("replace-in-place semantics broken: got=%+v ok=%v", got, ok)
	}
}

func TestResolvedCache_TTLExpiry(t *testing.T) {
	c := newResolvedCache(10, 1<<20, 10*time.Millisecond)
	c.Put("k", &ResolvedEntry{RawJSON: []byte("x")})
	if _, ok := c.Get("k"); !ok {
		t.Fatalf("immediate Get should hit")
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Fatalf("Get after TTL should miss")
	}
	if c.Stats().EvictTTLTotal != 1 {
		t.Fatalf("expected 1 TTL eviction, got %d", c.Stats().EvictTTLTotal)
	}
}

func TestResolvedCache_StatsHitRate(t *testing.T) {
	c := newResolvedCache(10, 1<<20, time.Hour)
	c.Put("k", &ResolvedEntry{RawJSON: []byte("x")})
	c.Get("k") // hit
	c.Get("k") // hit
	c.Get("k") // hit
	c.Get("y") // miss
	s := c.Stats()
	if s.HitTotal != 3 || s.MissTotal != 1 {
		t.Fatalf("counters wrong: %+v", s)
	}
	if hr := s.HitRate(); hr <= 0.74 || hr >= 0.76 {
		t.Fatalf("hit_rate should be ~0.75, got %f", hr)
	}
}

func TestResolvedCache_ConcurrentSafe(t *testing.T) {
	// Race-detector-clean concurrent Get/Put against the same cache.
	c := newResolvedCache(100, 1<<20, time.Hour)

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := fmt.Sprintf("k%d-%d", w, i%17)
				if i%2 == 0 {
					c.Put(k, &ResolvedEntry{RawJSON: []byte("x")})
				} else {
					c.Get(k)
				}
			}
		}()
	}
	wg.Wait()
}

func TestResolvedCache_EmptyTreatedAsMiss(t *testing.T) {
	// A nil receiver must not panic — defensive coding for callers
	// that take the L1 disabled path.
	var c *ResolvedCacheStore
	if _, ok := c.Get("x"); ok {
		t.Fatalf("nil cache must miss, not hit")
	}
	c.Put("x", &ResolvedEntry{RawJSON: []byte("y")}) // must not panic
	if c.Len() != 0 || c.Bytes() != 0 {
		t.Fatalf("nil cache should report zero metrics")
	}
}

// --- Ship E (0.30.116) — api-stage L1 ---------------------------------------

// TestComputeKey_EmptyStageByteIdenticalToPreShipE asserts AC-E1 at the
// key layer: a ResolvedKeyInputs with Stage=="" (every "restactions" /
// "widgets" entry) hashes byte-identically whether the Stage field
// exists or not — ComputeKey folds Stage in ONLY when non-empty, so no
// pre-0.30.116 entry's key shifts on the rolling restart.
func TestComputeKey_EmptyStageByteIdenticalToPreShipE(t *testing.T) {
	in := ResolvedKeyInputs{
		HandlerKind: "restactions", Group: "g", Version: "v", Resource: "r",
		Namespace: "ns", Name: "n", Username: "u", Groups: []string{"x"},
		PerPage: 1, Page: 1,
	}
	withEmptyStage := in
	withEmptyStage.Stage = ""
	if ComputeKey(in) != ComputeKey(withEmptyStage) {
		t.Fatalf("AC-E1: an empty Stage shifted the key — a non-apistage entry " +
			"must hash identically to pre-0.30.116")
	}
	// A non-empty Stage MUST shift the key (it is the apistage discriminator).
	withStage := in
	withStage.Stage = "stage\x1fcompositions\x1fabc\x1fdef"
	if ComputeKey(in) == ComputeKey(withStage) {
		t.Fatalf("a non-empty Stage did not change the key — apistage entries would collide")
	}
}

// TestApistageEvictPressure asserts AC-E7: the O6 budget signal is the
// evict/store ratio, 0 when no api-stage entry was ever stored.
func TestApistageEvictPressure(t *testing.T) {
	var s ResolvedCacheStats
	if got := s.ApistageEvictPressure(); got != 0 {
		t.Fatalf("ApistageEvictPressure with zero stores = %v, want 0", got)
	}
	s.ApistageStoreTotal = 10
	s.ApistageEvictTotal = 3
	if got := s.ApistageEvictPressure(); got != 0.3 {
		t.Fatalf("ApistageEvictPressure = %v, want 0.3", got)
	}
}

// TestApistageCounters_ClassifiedByHandlerKind asserts the store counts
// api-stage Put()s + evictions via the entry's HandlerKind — a non-
// apistage entry never moves the api-stage counters (AC-E7).
func TestApistageCounters_ClassifiedByHandlerKind(t *testing.T) {
	c := newResolvedCache(1, 1<<20, time.Hour) // entry cap 1 → next Put evicts

	// A non-apistage entry: api-stage counters stay 0.
	c.Put("plain", &ResolvedEntry{
		RawJSON: []byte(`{}`),
		Inputs:  &ResolvedKeyInputs{HandlerKind: "restactions"},
	})
	if s := c.Stats(); s.ApistageStoreTotal != 0 {
		t.Fatalf("non-apistage Put bumped apistage_store_total to %d", s.ApistageStoreTotal)
	}

	// An api-stage entry: store counter ticks; the cap-1 store evicts the
	// "plain" entry (non-apistage → apistage_evict stays 0).
	c.Put("stageA", &ResolvedEntry{
		RawJSON: []byte(`{"value":1}`),
		Inputs:  &ResolvedKeyInputs{HandlerKind: HandlerKindApistage, Stage: "s1"},
	})
	if s := c.Stats(); s.ApistageStoreTotal != 1 {
		t.Fatalf("apistage Put: apistage_store_total=%d want 1", s.ApistageStoreTotal)
	}
	if s := c.Stats(); s.ApistageEvictTotal != 0 {
		t.Fatalf("evicting a non-apistage entry bumped apistage_evict_total to %d", s.ApistageEvictTotal)
	}

	// A second api-stage Put evicts the first api-stage entry → apistage
	// evict counter ticks.
	c.Put("stageB", &ResolvedEntry{
		RawJSON: []byte(`{"value":2}`),
		Inputs:  &ResolvedKeyInputs{HandlerKind: HandlerKindApistage, Stage: "s2"},
	})
	if s := c.Stats(); s.ApistageEvictTotal != 1 {
		t.Fatalf("evicting an apistage entry: apistage_evict_total=%d want 1", s.ApistageEvictTotal)
	}
}

// TestApistageL1Enabled_DefaultOffAndGates asserts AC-E6: the feature is
// default-off and gated under CACHE_ENABLED + RESOLVED_CACHE_ENABLED.
func TestApistageL1Enabled_DefaultOffAndGates(t *testing.T) {
	// CACHE off → apistage off regardless of its own flag.
	t.Setenv("CACHE_ENABLED", "false")
	t.Setenv("RESOLVED_CACHE_APISTAGE_ENABLED", "true")
	if ApistageL1Enabled() {
		t.Fatalf("AC-E6: apistage L1 active with CACHE_ENABLED=false")
	}
	// CACHE on, RESOLVED_CACHE off → apistage off.
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "false")
	if ApistageL1Enabled() {
		t.Fatalf("AC-E6: apistage L1 active with RESOLVED_CACHE_ENABLED=false")
	}
	// All gates open but the apistage flag unset → default OFF.
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_APISTAGE_ENABLED", "")
	if ApistageL1Enabled() {
		t.Fatalf("AC-E6: apistage L1 must default OFF when its flag is unset")
	}
	// Explicit opt-in → on.
	t.Setenv("RESOLVED_CACHE_APISTAGE_ENABLED", "true")
	if !ApistageL1Enabled() {
		t.Fatalf("AC-E6: apistage L1 must be on when all three gates are open")
	}
}
