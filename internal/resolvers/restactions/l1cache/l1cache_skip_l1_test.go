// Ship 1.5a (0.25.322) — unit test for item 4: skip identical L1 writes
// inside resolveAndCacheInner. Mirrors the Patch F contract for the L2
// path; signal is the L1WritesSkippedIdentical counter.
//
// Two back-to-back resolves of the same RESTAction produce byte-identical
// freshly-marshaled v3 wrapper bytes; the first call writes L1, the
// second SHOULD skip the SetResolvedRaw and tick the counter exactly once.

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

// TestResolveAndCache_SkipsIdenticalL1Write pins the Ship 1.5a item 4
// contract: when the freshly marshaled v3 bytes are byte-equal to the
// cached entry, SetResolvedRaw is skipped and L1WritesSkippedIdentical
// increments by exactly 1.
func TestResolveAndCache_SkipsIdenticalL1Write(t *testing.T) {
	mc := cache.NewMem(time.Hour)

	// Minimal CR shape — same as Patch F test. Empty Spec.API + nil
	// Spec.Filter so restactions.Resolve completes without a kube
	// apiserver and produces deterministic output.
	cr := &templates.RESTAction{}
	cr.SetName("ra-ship15a-l1skip")
	cr.SetNamespace("ns-ship15a-l1skip")

	uns, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cr)
	if err != nil {
		t.Fatalf("ToUnstructured: %v", err)
	}

	identity := "bid-ship15a-l1skip"
	username := "test-ship15a-user"
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithLogger(slog.Default()),
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: username,
			Groups:   []string{"devs"},
		}),
	)
	ctx = cache.WithCache(ctx, mc)
	ctx = cache.WithBindingIdentity(ctx, identity)

	gvr := schema.GroupVersionResource{
		Group: "templates.krateo.io", Version: "v1", Resource: "restactions",
	}
	l1Key := cache.ResolvedKey(identity, gvr, "ns-ship15a-l1skip", "ra-ship15a-l1skip", -1, -1)

	in := Input{
		Cache:       mc,
		Obj:         uns,
		ResolvedKey: l1Key,
		AuthnNS:     "krateo-system",
		PerPage:     -1,
		Page:        -1,
	}

	skippedBefore := cache.GlobalMetrics.L1WritesSkippedIdentical.Load()

	// First call — must write to L1 (no prior entry; bytes-equal check
	// fails, falls through to SetResolvedRaw).
	if _, err := ResolveAndCache(ctx, in); err != nil {
		t.Fatalf("first ResolveAndCache: %v", err)
	}

	skippedAfter1 := cache.GlobalMetrics.L1WritesSkippedIdentical.Load()
	if skippedAfter1-skippedBefore != 0 {
		t.Fatalf("first call: L1WritesSkippedIdentical delta = %d, want 0 (no prior entry to be byte-equal to)",
			skippedAfter1-skippedBefore)
	}

	// Confirm the L1 entry actually landed so the second call has
	// something to compare against.
	if _, hit, _ := mc.GetRaw(ctx, l1Key); !hit {
		t.Fatalf("first call: L1 entry not cached at %q (precondition for the skip test)", l1Key)
	}

	// Second call — same Input, deterministic output → byte-equal to
	// the cached entry. SetResolvedRaw MUST be skipped;
	// L1WritesSkippedIdentical MUST tick exactly once.
	if _, err := ResolveAndCache(ctx, in); err != nil {
		t.Fatalf("second ResolveAndCache: %v", err)
	}

	skippedAfter2 := cache.GlobalMetrics.L1WritesSkippedIdentical.Load()
	if got := skippedAfter2 - skippedAfter1; got != 1 {
		t.Fatalf("second call: L1WritesSkippedIdentical delta = %d, want 1 "+
			"(item 4 contract: skip identical L1 writes)", got)
	}

	t.Logf("Ship 1.5a item 4 verified: skipped total=%d across two identical calls",
		skippedAfter2-skippedBefore)
}
