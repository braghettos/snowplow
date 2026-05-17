// refresher_noop_falsifier_test.go — Ship C (0.30.112) pre-flight
// falsifier F-noop.
//
// Team rule feedback_falsifier_first_before_ship: written BEFORE the
// RefreshFunc rewrite; F-noop MUST fail against the no-op dispatcher
// handlers (dispatchers.go RegisterRefreshHandlers — both bodies
// `return nil` without re-resolving).
//
//   F-noop — registering the refresh handlers then invoking a registered
//        RefreshFunc must produce a REAL re-resolve: the entry is
//        re-resolved and Put() back into L1 (observable as a
//        ResolvedCacheStore StoreTotal increment). FAILS today: the
//        handler returns nil with no resolve and no Put.
//
// The re-resolve is exercised through the resolveOnceFn seam so the
// falsifier is deterministic without a live cluster — the seam is
// stubbed to return fixed encoded bytes; the falsifier asserts the
// RefreshFunc drove the resolve-and-store path (the Put), which is the
// load-bearing "real re-resolve" behaviour.

package dispatchers

import (
	"context"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestFalsifierFNoop_RefreshFuncReallyReResolves registers the Ship C
// refresh handlers, seeds an L1 entry, then invokes the registered
// "widgets" RefreshFunc and asserts it re-resolved + Put the entry.
//
// FAILS on the unfixed dispatchers.go: the registered handler is a
// no-op `return nil` — StoreTotal never increments past the seed Put.
func TestFalsifierFNoop_RefreshFuncReallyReResolves(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetDepsForTest()
	cache.ResetResolvedCacheForTest()
	cache.ResetRefresherForTest()
	t.Cleanup(func() {
		cache.ResetRefresherForTest()
		cache.ResetResolvedCacheForTest()
		cache.ResetDepsForTest()
	})

	// Deterministic resolve seam — no live cluster. The fresh bytes
	// differ from the seed so the re-resolve is observable in content
	// too, not just in the StoreTotal counter.
	freshBytes := []byte(`{"refreshed":true}`)
	restoreSeam := setResolveOnceForTest(func(_ context.Context, _ cache.ResolvedKeyInputs) ([]byte, error) {
		return freshBytes, nil
	})
	t.Cleanup(restoreSeam)

	c := cache.ResolvedCache()
	if c == nil {
		t.Fatalf("ResolvedCache nil — CACHE_ENABLED test setup wrong")
	}
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: "widgets",
		Group:           "widgets.templates.krateo.io",
		Version:         "v1beta1",
		Resource:        "buttons",
		Namespace:       "demo",
		Name:            "save-btn",
		Username:        "cyberjoker",
		Groups:          []string{"devs"},
	}
	key := cache.ComputeKey(inputs)
	c.Put(key, &cache.ResolvedEntry{RawJSON: []byte(`{"stale":true}`), Inputs: &inputs})

	seededStores := c.Stats().StoreTotal // == 1

	// Register the Ship C handlers and retrieve the "widgets" one.
	// saRC nil: outside-cluster, SA transport unavailable — the stubbed
	// resolveOnceFn does not need it (the falsifier exercises the
	// resolve-and-store plumbing, not a live resolver).
	RegisterRefreshHandlers(nil)
	fn := cache.RefreshFuncForTest("widgets")
	if fn == nil {
		t.Fatalf("F-noop: no RefreshFunc registered for kind=widgets")
	}

	// Invoke it exactly as the refresher worker would.
	if err := fn(context.Background(), key, inputs); err != nil {
		t.Fatalf("F-noop: RefreshFunc returned error: %v", err)
	}

	// A real re-resolve ends in an L1 Put — StoreTotal must climb.
	if got := c.Stats().StoreTotal; got != seededStores+1 {
		t.Fatalf("F-noop: invoking the registered widgets RefreshFunc did not "+
			"re-resolve + Put — StoreTotal %d -> %d (want +1). The handler is a "+
			"no-op that returns nil without re-resolving.", seededStores, got)
	}
	// And the entry content must now be the refreshed bytes.
	entry, ok := c.Get(key)
	if !ok {
		t.Fatalf("F-noop: entry gone after refresh")
	}
	if string(entry.RawJSON) != string(freshBytes) {
		t.Fatalf("F-noop: entry not updated by refresh — RawJSON=%q want %q",
			entry.RawJSON, freshBytes)
	}
}
