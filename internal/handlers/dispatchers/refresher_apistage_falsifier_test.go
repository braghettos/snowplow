// refresher_apistage_falsifier_test.go — Ship 0.30.117 pre-flight
// falsifier for the AC-E3 defect found validating Ship E (0.30.116).
//
// THE DEFECT: RegisterRefreshHandlers (dispatchers.go) registered the
// refresh handler only for "restactions" and "widgets" — the Ship E
// "apistage" handler kind was NEVER registered. An apistage-kind L1
// entry pulled off the refresher queue therefore hits a nil handler,
// the refresher counts skippedNoHandler, and the entry is silently
// TTL-only — never refreshed. The tester proved it on-cluster:
// apistage_store_total frozen at 8 while refresh_enqueued climbed
// 254 -> 622. AC-E3 ("the refresher keeps apistage entries fresh") FAIL.
//
// THE GAP that let it through: Ship E's falsifiers exercised only the
// DIRECT restactions.Resolve path; the defect lives in the REFRESHER
// path. These two falsifiers drive the refresher path:
//
//   FA1 (registry) — after RegisterRefreshHandlers, the handler registry
//        MUST carry an entry for cache.HandlerKindApistage. FAILS pre-fix:
//        RefreshFuncForTest(apistage) returns nil.
//
//   FA2 (end-to-end) — a dirty-marked apistage L1 key, drained by the
//        refresher worker pool, MUST be re-resolved (its content
//        refreshed). FAILS pre-fix: the worker finds no apistage handler,
//        counts skippedNoHandler, and the entry stays stale.
//
// Both are deterministic — the resolve is exercised through the
// resolveOnceFn seam, no live cluster.

package dispatchers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// apistageRefreshInputs is the canonical ResolvedKeyInputs for an
// api-stage L1 entry — HandlerKind=="apistage", Stage set, the owning
// RESTAction's GVR/namespace/name + a per-user identity.
func apistageRefreshInputs() cache.ResolvedKeyInputs {
	return cache.ResolvedKeyInputs{
		HandlerKind: cache.HandlerKindApistage,
		Group:       "templates.krateo.io",
		Version:     "v1",
		Resource:    "restactions",
		Namespace:   "bench-ns-01",
		Name:        "shared-compositions-restaction",
		Username:    "cyberjoker",
		Groups:      []string{"devs"},
		Stage:       "stage\x1fcompositions\x1fabc\x1fdef",
	}
}

// TestFalsifierFA1_ApistageHandlerRegistered asserts the registry-level
// defect directly: RegisterRefreshHandlers MUST register a RefreshFunc
// for cache.HandlerKindApistage alongside "restactions" and "widgets".
//
// FAILS pre-fix: RegisterRefreshHandlers omits the apistage kind, so
// RefreshFuncForTest(apistage) returns nil — the refresher would count
// skippedNoHandler for every apistage key.
func TestFalsifierFA1_ApistageHandlerRegistered(t *testing.T) {
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

	RegisterRefreshHandlers(nil)

	// The two pre-Ship-E kinds must still be registered (no regression).
	if cache.RefreshFuncForTest("restactions") == nil {
		t.Fatalf("FA1: restactions RefreshFunc unregistered — regression")
	}
	if cache.RefreshFuncForTest("widgets") == nil {
		t.Fatalf("FA1: widgets RefreshFunc unregistered — regression")
	}
	// THE DEFECT: the apistage kind must be registered.
	if cache.RefreshFuncForTest(cache.HandlerKindApistage) == nil {
		t.Fatalf("FA1: no RefreshFunc registered for kind=%q — an apistage L1 "+
			"entry off the refresher queue hits a nil handler -> skippedNoHandler "+
			"-> never refreshed (AC-E3 defect)", cache.HandlerKindApistage)
	}
}

// TestFalsifierFA2_ApistageEntryRefreshedEndToEnd drives the full
// refresher path: a dirty-marked apistage L1 key is drained by the
// worker pool and MUST be re-resolved (content refreshed).
//
// FAILS pre-fix: the worker dequeues the apistage key, finds no
// apistage handler, counts skippedNoHandler, returns — the entry's
// content stays stale.
func TestFalsifierFA2_ApistageEntryRefreshedEndToEnd(t *testing.T) {
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

	// Deterministic resolve seam — the refresh re-resolve returns fresh
	// bytes distinct from the seeded stale bytes, so a real refresh is
	// observable in content.
	freshBytes := []byte(`{"value":"refreshed-apistage"}`)
	restoreSeam := setResolveOnceForTest(func(_ context.Context, in cache.ResolvedKeyInputs) ([]byte, error) {
		if in.HandlerKind != cache.HandlerKindApistage {
			t.Errorf("FA2: resolve seam invoked for unexpected kind %q", in.HandlerKind)
		}
		return freshBytes, nil
	})
	t.Cleanup(restoreSeam)

	c := cache.ResolvedCache()
	if c == nil {
		t.Fatalf("FA2: ResolvedCache nil — test setup wrong")
	}

	inputs := apistageRefreshInputs()
	key := cache.ComputeKey(inputs)
	// Seed the apistage L1 entry with STALE content.
	c.Put(key, &cache.ResolvedEntry{
		RawJSON: []byte(`{"value":"stale-apistage"}`),
		Inputs:  &inputs,
	})
	// Record a dep edge so an informer event for the GVR dirty-marks
	// this stage entry — the exact O4 path Ship E wires.
	depGVR := schema.GroupVersionResource{
		Group: "composition.krateo.io", Version: "v1", Resource: "widgets",
	}
	cache.Deps().RecordList(key, depGVR, "bench-ns-01")

	// Register the handlers and start the worker pool.
	RegisterRefreshHandlers(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache.StartRefresher(ctx)
	t.Cleanup(cache.StopRefresher)

	// Dirty-mark the stage entry the way an informer UPDATE does: the
	// dep-tracker matches the LIST-scope dep and enqueues `key` into the
	// refresher queue via the hook StartRefresher wired.
	if marked := cache.Deps().OnUpdate(depGVR, "bench-ns-01", "any-widget"); marked != 1 {
		t.Fatalf("FA2: OnUpdate dirty-marked %d keys, want 1 (the apistage entry)", marked)
	}

	// Poll until the refresher worker has re-resolved the entry. A
	// working handler swaps the content to freshBytes; a missing handler
	// leaves it stale forever.
	deadline := time.Now().Add(5 * time.Second)
	for {
		entry, ok := c.Get(key)
		if ok && entry != nil && string(entry.RawJSON) == string(freshBytes) {
			break // refreshed — PASS
		}
		if time.Now().After(deadline) {
			cur := "<gone>"
			if e, k := c.Get(key); k && e != nil {
				cur = string(e.RawJSON)
			}
			t.Fatalf("FA2: apistage L1 entry was NOT refreshed within 5s — "+
				"content still %s (want %s). The refresher has no apistage "+
				"handler, so the dequeued key is skippedNoHandler and the "+
				"entry stays stale (AC-E3 defect).", cur, freshBytes)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Architect test-hardening: the refreshed re-Put value must decode as
	// the apistage value shape (encodeStageValue's apistageHitValue:
	// a top-level `value` key), NOT whole-RESTAction JSON. This pins the
	// load-bearing (nil,nil)-on-success contract in resolve_populate.go's
	// apistage branch — if a future seam refactor returned RESTAction
	// bytes there, resolveAndPopulateL1 would Put those bytes under the
	// stage key and silently corrupt the entry. apistageHitValue is
	// unexported in the api package; we assert the shape structurally:
	// the decoded object has a `value` key and no RESTAction markers.
	entry, ok := c.Get(key)
	if !ok || entry == nil {
		t.Fatalf("FA2: refreshed entry gone")
	}
	var decoded map[string]any
	if err := json.Unmarshal(entry.RawJSON, &decoded); err != nil {
		t.Fatalf("FA2: refreshed entry is not valid JSON: %v", err)
	}
	if _, hasValue := decoded["value"]; !hasValue {
		t.Fatalf("FA2: refreshed entry lacks the apistage `value` key — got %v. "+
			"The re-Put value is not the apistage shape; the resolve-once "+
			"seam's (nil,nil)-on-success contract is broken.", decoded)
	}
	for _, raMarker := range []string{"apiVersion", "kind", "spec", "status"} {
		if _, leaked := decoded[raMarker]; leaked {
			t.Fatalf("FA2: refreshed entry carries whole-RESTAction marker %q — "+
				"the stage entry was overwritten with RESTAction JSON instead "+
				"of the apistage value shape", raMarker)
		}
	}
}
