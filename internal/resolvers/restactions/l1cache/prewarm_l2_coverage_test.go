// Q-RBACC-L2-1 PR review N3 (PM strategic, 2026-05-05) — prewarm-L2
// coverage proof.
//
// CLAIM (architect deviation #2): the L2 cache is the single L2 writer
// and lives inside l1cache.ResolveAndCache. Therefore prewarm — which
// goes through:
//
//   WarmL1FromEntryPoints → recursivePreWarm → resolveL1RefsCollect
//     → widgets.Resolve → resolveApiRef → apiref.Resolve
//     → resolveViaL1Cache → l1cache.ResolveAndCache → cache.L2Put
//
//   WarmL1ForAllUsers → ... → warmL1RestActionsForUser
//     → l1cache.ResolveAndCache → cache.L2Put
//
// — populates L2 entries "for free" alongside L1.
//
// The PM concern: this is plausible-but-unverified. This test EMPIRICALLY
// verifies the load-bearing leg: l1cache.ResolveAndCache writes to L2
// when (Cache != nil, ResolvedKey != "", L2Enabled=true).
//
// Test shape:
//   1. Snapshot L2Writes counter.
//   2. Build a minimal RESTAction CR (no api[] entries, no filter) so
//      restactions.Resolve completes without needing a kube apiserver.
//   3. Call l1cache.ResolveAndCache with a populated ResolvedKey + a
//      MemCache + UserInfo on ctx.
//   4. Assert L2Writes counter incremented by 1 (or, if the reduction
//      gate skipped the entry, L2SkippedHighRatio incremented by 1).
//   5. Assert the L2 store has the entry under the expected key.
//
// Both branches (Writes incremented OR SkippedHighRatio incremented)
// PROVE that the L2 write site executed for the prewarm-shaped caller.
// A test that observes NEITHER counter advancing would prove the L2
// write site was bypassed entirely (the bug PM N3 was guarding against).

package l1cache

import (
	"context"
	"log/slog"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestL1Cache_ResolveAndCache_WritesL2_PrewarmShape — Q-RBACC-L2-1 PM
// review N3. Empirical proof that the prewarm-shaped caller exercises
// the L2 write path inside resolveAndCacheInner.
func TestL1Cache_ResolveAndCache_WritesL2_PrewarmShape(t *testing.T) {
	prevEnabled := envEnabledForTest()
	cache.SetL2EnabledForTest(true)
	defer func() { cache.SetL2EnabledForTest(prevEnabled) }()
	cache.FlushL2ForTest()

	mc := cache.NewMem(time.Hour)

	// Minimal RESTAction CR: no api[] entries, no filter. restactions.
	// Resolve handles empty Spec.API (returns empty dict) and empty
	// Spec.Filter (marshals the empty dict directly), so the resolve
	// chain completes without needing a kube apiserver or rest.Config.
	cr := &templates.RESTAction{}
	cr.SetName("ra-prewarm-test")
	cr.SetNamespace("ns-prewarm-test")

	// Convert to unstructured via the standard converter (matches what
	// the prewarm path passes — `cached.Object`).
	uns, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		t.Fatalf("ToUnstructured: %v", err)
	}

	// Build a context shaped like the prewarm context: cache, logger,
	// UserInfo (so RefilterRESTAction inside ResolveAndCache doesn't
	// fail-closed), and binding identity (so CacheIdentity returns the
	// shared identity instead of the username).
	identity := "bid-prewarm-test"
	username := "test-prewarm-user"
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithLogger(slog.Default()),
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: username,
			Groups:   []string{"devs"},
		}),
	)
	ctx = cache.WithCache(ctx, mc)
	ctx = cache.WithBindingIdentity(ctx, identity)

	// L1 key shape mirrors prewarm.go:614 / 757.
	gvr := schema.GroupVersionResource{
		Group: "templates.krateo.io", Version: "v1", Resource: "restactions",
	}
	l1Key := cache.ResolvedKey(identity, gvr, "ns-prewarm-test", "ra-prewarm-test", -1, -1)

	// Snapshot counters BEFORE the call so we measure the delta caused
	// by THIS invocation only.
	writesBefore := cache.GlobalMetrics.L2Writes.Load()
	skippedHighBefore := cache.GlobalMetrics.L2SkippedHighRatio.Load()
	skippedSizeBefore := cache.GlobalMetrics.L2SkippedSizeCap.Load()

	result, err := ResolveAndCache(ctx, Input{
		Cache:       mc,
		Obj:         uns,
		ResolvedKey: l1Key,
		AuthnNS:     "krateo-system",
		PerPage:     -1,
		Page:        -1,
	})
	if err != nil {
		t.Fatalf("ResolveAndCache: %v", err)
	}
	if result == nil {
		t.Fatalf("ResolveAndCache returned nil result")
	}

	writesDelta := cache.GlobalMetrics.L2Writes.Load() - writesBefore
	skippedHighDelta := cache.GlobalMetrics.L2SkippedHighRatio.Load() - skippedHighBefore
	skippedSizeDelta := cache.GlobalMetrics.L2SkippedSizeCap.Load() - skippedSizeBefore

	// THE LOAD-BEARING ASSERTION: the L2 write site executed at least
	// once. Either Writes (entry stored) or SkippedHighRatio /
	// SkippedSizeCap (gates fired) is acceptable evidence — both prove
	// the call reached cache.L2Put. The bug PM N3 was guarding against
	// would manifest as ALL THREE counters at zero (write site
	// bypassed entirely).
	if writesDelta == 0 && skippedHighDelta == 0 && skippedSizeDelta == 0 {
		t.Fatalf(
			"PM N3 regression: l1cache.ResolveAndCache did NOT call cache.L2Put. "+
				"L2Writes Δ=%d, L2SkippedHighRatio Δ=%d, L2SkippedSizeCap Δ=%d. "+
				"Prewarm path would NOT populate L2.",
			writesDelta, skippedHighDelta, skippedSizeDelta,
		)
	}

	// On the happy path (small payload, no skip), assert the write
	// landed and the L2 entry is retrievable. The empty-dict CR
	// produces a tiny refilter output (~80 bytes for `{"status":{}}` +
	// CR metadata), well under any size cap; no L1 raw bytes to compare
	// for the reduction gate.
	if writesDelta > 0 {
		// Verify the entry is materialised under the expected L2 key
		// (using the same key derivation as the writer).
		groupsHash := cache.HashGroups([]string{"devs"})
		// In l1cache.ResolveAndCache, identity comes from
		// ParseResolvedKey(in.ResolvedKey).Username — which is `identity`
		// here because `cache.ResolvedKey(identity, ...)` puts `identity`
		// in that slot.
		if e, ok := cache.L2Get(l1Key, identity, groupsHash); !ok {
			t.Fatalf("L2 entry not retrievable at expected key (l1Key=%q identity=%q)",
				l1Key, identity)
		} else if len(e.Refiltered) == 0 {
			t.Fatalf("L2 entry has zero-length Refiltered bytes")
		}
	}

	t.Logf("PM N3 verified: L2 write site reached. Δwrites=%d Δskipped_high=%d Δskipped_size=%d",
		writesDelta, skippedHighDelta, skippedSizeDelta)
}

// envEnabledForTest reads the current L2 enabled state via a Get-after-
// SetL2EnabledForTest cycle so the deferred restore doesn't accidentally
// disable L2 for downstream tests in the same package.
func envEnabledForTest() bool {
	// Force-init by querying L2Enabled; if it returns false but a Set
	// would set it true, that's the right baseline. This is best-effort:
	// the test is the only L2-toucher in this package.
	return cache.L2Enabled()
}
