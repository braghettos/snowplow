// refresher_stage_error_falsifier_test.go — Ship 0.30.120 falsifier for
// the two-layer L1-poison fix.
//
// THE DEFECT (pre-existing since 0.30.113): the SA-transport L1
// refresher re-resolves the whole compositions-list RESTAction with no
// per-user JWT. Its allNamespacesAndCrds stage has exportJwt:true — a
// nested snowplow /call loopback needing the user's bearer token. The
// background refresher has none, so the loopback 401s; the stage's
// continueOnError swallows the 401 as a structurally-valid empty result;
// the refresher Puts a ~1.9 KB empty envelope over the user's correct
// ~26 MB entry. Uniform under-serve.
//
// THREE HERMETIC TESTS (the existing setResolveOnceForTest seam +
// cache.ResetResolvedCacheForTest — no live cluster):
//
//   Test 1  TestRefresher_StageErrorDoesNotOverwriteGoodEntry — the
//           poison reproducer / falsifier. COMMIT 1 asserted the BUGGY
//           behaviour (empty overwrites good) — the captured pre-flight
//           artifact. COMMIT 2 (this state) INVERTS the assertion: with
//           the error-aware Put-gate landed, the good entry survives.
//           The commit-1 -> commit-2 inversion diff is the falsifier
//           proof.
//
//   Test 2  TestRefresher_LegitimateEmptyIsStored — the discriminator.
//           An empty result with NO stage error IS stored — the gate
//           keys on stage-error PRESENCE, never on result emptiness, so
//           a user who legitimately has 0 compositions still gets their
//           empty result.
//
//   Test 3  TestRefresher_ExportJwtRESTActionSkippedToTTL — layer (a),
//           added in COMMIT 2. A RESTAction CR with one exportJwt:true
//           stage drives resolveRestActionForRefresh to the (nil,nil)
//           skip-to-TTL sentinel before restactions.Resolve is ever
//           reached; a sibling CR with exportJwt unset does NOT
//           short-circuit via layer (a).

package dispatchers

import (
	"context"
	"testing"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// goodEntry is the non-trivial "correct" L1 payload the refresher must
// not clobber. emptyEntry is the under-served result the poisoned
// re-resolve produces.
var (
	stageErrGoodEntry  = []byte(`{"list":[{"uid":"real"}]}`)
	stageErrEmptyEntry = []byte(`{"list":[]}`)
)

// --- Test 1 — poison reproducer / falsifier --------------------------------

// TestRefresher_StageErrorDoesNotOverwriteGoodEntry seeds L1 with a good
// entry, then runs resolveAndPopulateL1 with a stub that emulates the
// poisoned re-resolve: it bumps the stage-error sink (the api resolver
// does exactly this when it writes dict[call.ErrorKey] on a swallowed
// continueOnError'd inner-call failure) and returns an empty result.
//
// COMMIT 2 (this state, post-fix): the assertion is INVERTED from
// commit 1's pre-fix artifact. With the error-aware Put-gate landed,
// resolveAndPopulateL1 observes stageErrSink.Load() > 0 and DECLINES the
// Put — the user's correct entry survives. resolveAndPopulateL1 still
// returns nil (a deterministic stage failure must NOT drive
// AddRateLimited / burn the retry budget); the prior good entry stays
// and TTL is the outer net.
func TestRefresher_StageErrorDoesNotOverwriteGoodEntry(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)

	c := cache.ResolvedCache()
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: "restactions",
		Group:           "templates.krateo.io",
		Version:         "v1",
		Resource:        "restactions",
		Namespace:       "krateo-system",
		Name:            "compositions-list",
		Username:        "cyberjoker",
		Groups:          []string{"devs"},
	}
	key := cache.ComputeKey(inputs)
	c.Put(key, &cache.ResolvedEntry{RawJSON: stageErrGoodEntry, Inputs: &inputs})

	// Emulate the poisoned re-resolve: touch the stage-error sink that
	// resolveAndPopulateL1 installed on ctx (this is exactly what the api
	// resolver does at each dict[call.ErrorKey] write), then return the
	// under-served empty result.
	restore := setResolveOnceForTest(func(ctx context.Context, _ cache.ResolvedKeyInputs) ([]byte, error) {
		if sink := cache.StageErrorSinkFromContext(ctx); sink != nil {
			sink.Add(1)
		}
		return stageErrEmptyEntry, nil
	})
	t.Cleanup(restore)

	skippedBefore := cache.RefresherSkippedStageError()

	if err := resolveAndPopulateL1(context.Background(), inputs, nil, nil); err != nil {
		t.Fatalf("resolveAndPopulateL1 must return nil even when it declines the Put "+
			"(a deterministic stage failure must NOT drive AddRateLimited); got %v", err)
	}

	got, ok := c.Get(key)
	if !ok {
		t.Fatalf("entry vanished — expected it to still be present")
	}
	// COMMIT 2 (post-fix, INVERTED from commit 1): the error-aware
	// Put-gate declined the overwrite — the user's correct entry survives.
	if string(got.RawJSON) != string(stageErrGoodEntry) {
		t.Fatalf("stage-error poison: L1 entry = %q; want the good entry %q kept "+
			"(the error-aware Put-gate must decline the under-served overwrite)",
			got.RawJSON, stageErrGoodEntry)
	}
	// The declined-Put counter must have ticked.
	if after := cache.RefresherSkippedStageError(); after != skippedBefore+1 {
		t.Fatalf("refresh_skipped_stage_error counter %d -> %d; want +1 "+
			"(the gate must record every declined Put)", skippedBefore, after)
	}
}

// --- Test 2 — discriminator / no false positive ----------------------------

// TestRefresher_LegitimateEmptyIsStored proves the gate keys on
// stage-error PRESENCE, never on result emptiness. The stub returns the
// SAME empty result as Test 1 but does NOT touch the sink — modelling a
// user who legitimately has zero compositions. The empty result MUST be
// stored: the gate must not false-positive on a legit empty.
func TestRefresher_LegitimateEmptyIsStored(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)

	c := cache.ResolvedCache()
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: "restactions",
		Resource:        "restactions",
		Namespace:       "krateo-system",
		Name:            "compositions-list",
		Username:        "loner",
	}
	key := cache.ComputeKey(inputs)
	c.Put(key, &cache.ResolvedEntry{RawJSON: stageErrGoodEntry, Inputs: &inputs})

	// Empty result, NO sink touch — a legitimate "0 compositions" outcome.
	restore := setResolveOnceForTest(func(_ context.Context, _ cache.ResolvedKeyInputs) ([]byte, error) {
		return stageErrEmptyEntry, nil
	})
	t.Cleanup(restore)

	skippedBefore := cache.RefresherSkippedStageError()

	if err := resolveAndPopulateL1(context.Background(), inputs, nil, nil); err != nil {
		t.Fatalf("resolveAndPopulateL1 error: %v", err)
	}

	got, ok := c.Get(key)
	if !ok {
		t.Fatalf("entry vanished — expected the legit empty result stored")
	}
	if string(got.RawJSON) != string(stageErrEmptyEntry) {
		t.Fatalf("legit empty NOT stored: L1 entry = %q; want %q "+
			"(no stage error => the gate must NOT fire; a legit empty IS stored)",
			got.RawJSON, stageErrEmptyEntry)
	}
	// The gate must NOT have fired — no stage error was observed.
	if after := cache.RefresherSkippedStageError(); after != skippedBefore {
		t.Fatalf("refresh_skipped_stage_error counter advanced %d -> %d on a legit empty; "+
			"the gate must key on stage-error PRESENCE, never on result emptiness",
			skippedBefore, after)
	}
}

// NOTE — the former Test 3 (TestRefresher_ExportJwtRESTActionSkippedToTTL)
// and its restActionUnstructured helper were REMOVED at Ship 0.30.123
// (#155). They validated the Ship 0.30.120 layer-(a) exportJwt
// skip-to-TTL net, which 0.30.123 deletes: in-process nested /call now
// resolves an exportJwt loopback stage correctly, so the refresher no
// longer skips those RESTActions. The layer-(b) error-aware Put-gate
// (Tests 1 and 2 above) STAYS and remains covered.
