// a1_uaf_seed_ctx_propagation_test.go — turns the widget-seed leg's WEAK RED
// into a real one (adv-cache-isolation).
//
// WHAT WAS WEAK. The earlier seed arm exercised declineWidgetUAFPut over
// (inputs, sink) directly. That proves the RULE, not the WIRING, and it passes on
// origin/main because the gate helper is scaffolding. The structural AST guard
// proves seedOneWidget CONSULTS the gate — but neither can see the failure mode
// in between: the resolve running under a DIFFERENT ctx than the one carrying
// the sink the gate reads. Under that bug the gate is present, the helper is
// correct, both tests are green, and every refilter-narrowed widget is still
// seeded, because the sink the refilter bumps is not the sink the gate loads.
//
// seedOneWidget now resolves through the widgetsResolveFn seam (the same one the
// dispatcher uses), which makes that gap observable: the seam receives the exact
// ctx the resolve runs under, so a falsifier can mark THAT ctx and then watch
// whether the gate — reading its own sink — actually declines.
//
// SAMENESS IS PROVEN BEHAVIOURALLY, NOT BY COMPARING POINTERS. The gate's sink is
// a local inside seedOneWidget and is not reachable from here, but it does not
// need to be: bumping the sink the RESOLVE saw and observing that the PUT DID NOT
// HAPPEN is strictly stronger than a pointer comparison. It proves the two are
// the same object AND that the gate reads it AND that reading it suppresses the
// write. Two distinct sinks would both be non-nil and both report sane counts,
// and the cell would still be Put — which is exactly what the assertion below
// catches.

package dispatchers

import (
	"context"
	"testing"

	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestR1_SeedOneWidget_ResolveCtxCarriesTheGatesSink drives the REAL
// seedOneWidget end-to-end with only the resolver seamed.
func TestR1_SeedOneWidget_ResolveCtxCarriesTheGatesSink(t *testing.T) {
	a1BuildTwoTenantWatcher(t)
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)

	entry := navWidgetEntry{
		W:          h1WidgetUnstructured(map[string]any{"apiRef": map[string]any{"name": a1RAName2, "namespace": h1NS}}),
		GVR:        h1WidgetGVR,
		PerPage:    -1,
		Page:       -1,
		KeyPerPage: -1,
		KeyPage:    -1,
	}

	// The cohort ctx must present the GROUP the shared CRB grants (a1Group),
	// exactly as a real cohort representative does — a username-only cohort
	// derives an empty first-match BindingUID and seedOneWidget short-circuits
	// at the #95 guard before ever reaching the resolve.
	seedCtx := withCohortSeedContext(context.Background(),
		seedTarget{Username: a1Alice, Groups: []string{a1Group}}, endpoints.Endpoint{}, nil)
	key, handle, inputs := dispatchCacheLookupKey(seedCtx, "widgets",
		h1WidgetGVR.Group, h1WidgetGVR.Version, h1WidgetGVR.Resource,
		h1NS, h1WName, -1, -1, effectiveKeyExtras(seedCtx, entry.W.Object, nil))
	if handle == nil || key == "" || inputs == nil || inputs.BindingUID == "" {
		t.Fatalf("PRECONDITION: the seed cohort must derive a live, non-empty-BindingUID widgets key — otherwise "+
			"seedOneWidget short-circuits for the #95 reason and this arm proves nothing; key=%q handle=%v inputs=%+v",
			key, handle != nil, inputs)
	}

	orig := widgetsResolveFn
	t.Cleanup(func() { widgetsResolveFn = orig })

	// sinkSeenByResolver is the sink the RESOLVE actually ran under. The gate
	// below reads whatever seedOneWidget installed; if those are different
	// objects, the bump lands on one and the gate reads the other.
	var sinkSeenByResolver *cache.UAFTouchedSink
	var resolverRan bool
	widgetsResolveFn = func(ctx context.Context, _ widgets.ResolveOptions) (*widgets.Widget, error) {
		resolverRan = true
		sinkSeenByResolver = cache.UAFTouchedSinkFromContext(ctx)
		// Stand in for the apiRef'd UAF RESTAction's refilter: mark THIS ctx.
		cache.BumpUAFTouched(ctx)
		out := h1WidgetUnstructured(map[string]any{})
		if err := unstructured.SetNestedField(out.Object, "representative-only", "status", "widgetData", "rows"); err != nil {
			t.Fatalf("seam: %v", err)
		}
		return out, nil
	}

	if err := seedOneWidget(seedCtx, entry, h1NS, seedModeBoot); err != nil {
		t.Fatalf("seedOneWidget returned %v; want nil (both the decline and the success path return nil)", err)
	}

	if !resolverRan {
		t.Fatal("PRECONDITION: seedOneWidget never reached the resolve — it short-circuited earlier, so nothing " +
			"about the sink or the gate was exercised and this arm would pass vacuously")
	}
	if sinkSeenByResolver == nil {
		t.Fatal("R-1 CTX-PROPAGATION RED: seedOneWidget ran the widget resolve under a ctx carrying NO UAFTouchedSink. " +
			"The refilter's bump would land on a nil receiver, the gate would read Count()==0, and every " +
			"refilter-narrowed widget would be seeded into the shared per-binding cell under the cohort " +
			"representative's narrowing — with the gate present and the AST guard green.")
	}
	if got := sinkSeenByResolver.Count(); got != 1 {
		t.Fatalf("the sink on the resolve ctx did not record the bump the resolver made (count=%d, want 1) — it is "+
			"not a live sink, so nothing downstream can read the mark", got)
	}

	// THE ASSERTION. The resolve marked the sink; if the gate read the SAME
	// object, no cell exists. A second, distinct sink would leave the cell Put.
	if entry, ok := handle.Get(key); ok {
		t.Fatalf("R-1 CTX-PROPAGATION RED: the widget seed Put a refilter-marked envelope under %q (%q). The bump the "+
			"resolve made did not reach the sink the gate consults — they are different objects, so the gate is "+
			"wired but INERT on this path.", key, entry.RawJSON)
	}
	if got := cache.WidgetsUAFPutDeclined(); got != 1 {
		t.Fatalf("the seed decline must route through the widgets-class gate exactly once; counter=%d want 1", got)
	}

	// --- CONTROL: same drive, resolve marks NOTHING → the seed still Puts. ---
	cache.ResetUAFPutDeclineCountersForTest()
	widgetsResolveFn = func(ctx context.Context, _ widgets.ResolveOptions) (*widgets.Widget, error) {
		if cache.UAFTouchedSinkFromContext(ctx) == nil {
			t.Fatal("control: the sink must still be installed on the resolve ctx even when nothing bumps it")
		}
		out := h1WidgetUnstructured(map[string]any{})
		if err := unstructured.SetNestedField(out.Object, "same-for-everyone", "status", "widgetData", "rows"); err != nil {
			t.Fatalf("control seam: %v", err)
		}
		return out, nil
	}
	if err := seedOneWidget(seedCtx, entry, h1NS, seedModeBoot); err != nil {
		t.Fatalf("control: seedOneWidget returned %v", err)
	}
	if _, ok := handle.Get(key); !ok {
		t.Fatalf("R-1 CONTROL BROKE: a widget seed whose resolve marked NO refilter was not Put under %q — the gate "+
			"is over-broad and has disabled the widget boot seed wholesale, which would make the assertion above "+
			"vacuous", key)
	}
	if got := cache.WidgetsUAFPutDeclined(); got != 0 {
		t.Fatalf("control: a non-refiltered widget seed must not tick the decline counter; got %d", got)
	}
}
