// a1_uaf_widget_sites_test.go — R-1 per-site arms for the two remaining widget
// carriers: the BOOT SEED (seedOneWidget) and the identity-free WIDGETCONTENT
// populate (populateWidgetContentL1).
//
// The R-2 acceptance arm covers the customer dispatch. These two cover the paths
// that fill the same cells WITHOUT a customer request — and they matter more than
// the dispatch on a cold pod, because they warm a per-BINDING cell under a cohort
// REPRESENTATIVE identity (the seed) or an identity-FREE shared cell under the
// SA identity (the walker). Both are read by every co-bound user's first paint.
//
// Each arm pairs the refiltered case with a non-refiltered CONTROL that must
// still be cached, so a gate that simply disabled the widget seed or the content
// layer wholesale cannot pass.

package dispatchers

import (
	"context"
	"testing"

	"github.com/krateo-platformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestR1_WidgetContentPopulate_DeclinesOnRefilter_ControlPopulates drives the
// REAL populateWidgetContentL1 with and without a refilter marked on ctx.
//
// This cell is the worst destination for a narrowed body in the whole system: it
// carries NO identity fold at all (widgetContentL1Key leaves Username/Groups
// zero), so it is shared by every user, and its serve-time gate re-derives only
// status.resourcesRefs.items[].allowed — it NEVER narrows status.widgetData.
//
// RED on origin/main: the envelope is Put and every user reads it.
func TestR1_WidgetContentPopulate_DeclinesOnRefilter_ControlPopulates(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	t.Setenv("WIDGET_CONTENT_L1_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)
	cache.ResetDepsForTest()
	t.Cleanup(cache.ResetDepsForTest)
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)

	in := h1WidgetUnstructured(map[string]any{
		"apiRef": map[string]any{"name": a1RAName2, "namespace": h1NS},
	})
	res := h1WidgetUnstructured(map[string]any{})
	if err := unstructured.SetNestedField(res.Object, "narrowed-for-whoever-resolved", "status", "widgetData", "rows"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	key, _ := widgetContentL1Key(h1WidgetGVR, h1NS, h1WName, -1, -1)
	if key == "" {
		t.Fatalf("PRECONDITION: the widgetContent layer must be live (key derivation returned \"\") — otherwise this " +
			"arm passes vacuously because nothing would be Put either way")
	}
	c := cache.ResolvedCache()
	if c == nil {
		t.Fatal("PRECONDITION: expected a live resolved cache")
	}

	// --- REFILTERED: a sink with a touch on ctx → decline. ------------------
	ctx, sink := cache.WithUAFTouchedSink(context.Background())
	sink.Bump()
	populateWidgetContentL1(ctx, h1WidgetGVR, in, -1, -1, res)
	if entry, ok := c.Get(key); ok {
		t.Fatalf("R-1 RED (widgetContent carrier): a userAccessFilter-narrowed envelope was seeded into the "+
			"IDENTITY-FREE shared content cell at %q (%q). That cell has no identity fold whatsoever and its serve-time "+
			"gate never narrows status.widgetData, so EVERY user would be served the resolving identity's rows.",
			key, entry.RawJSON)
	}
	if got := cache.WidgetsUAFPutDeclined(); got != 1 {
		t.Fatalf("the widgetContent decline must tick the widgets-class counter once; got %d", got)
	}

	// --- CONTROL: no refilter observed → the layer still works. -------------
	ctlCtx, _ := cache.WithUAFTouchedSink(context.Background())
	populateWidgetContentL1(ctlCtx, h1WidgetGVR, in, -1, -1, res)
	if _, ok := c.Get(key); !ok {
		t.Fatalf("R-1 CONTROL BROKE: a widget whose resolve ran NO refilter was not seeded into the content cell at "+
			"%q — the gate is over-broad and has disabled the identity-free content layer wholesale, which would make "+
			"the assertion above vacuous", key)
	}
	if got := cache.WidgetsUAFPutDeclined(); got != 1 {
		t.Fatalf("control: a non-refiltered populate must not tick the decline counter; total went to %d", got)
	}
}

// TestR1_WidgetSeedGate_DeclinesOnRefilter_ControlSeeds covers the seedOneWidget
// leg at the level the gate actually operates: the shared widgets-class decision
// over (inputs, sink).
//
// WHY NOT AN END-TO-END seedOneWidget DRIVE like the restactions seed arm: that
// arm works because seedOneRestaction has a resolve+Put TAIL SEAM
// (seedRestactionResolveAndPutFn) to observe. seedOneWidget has no equivalent
// seam and calls widgets.Resolve directly, which dials the apiserver — so an
// end-to-end hermetic drive is not available without adding a production seam
// purely for the test. The wiring that the gate IS called there is pinned
// structurally instead, by TestA1_EveryResolvedEntryPutSiteIsUAFGatedOrWaived,
// which walks the AST and requires seedOneWidget's ResolvedEntry literal to sit
// in a function that consults the gate. Behaviour here, wiring there.
func TestR1_WidgetSeedGate_DeclinesOnRefilter_ControlSeeds(t *testing.T) {
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	t.Cleanup(cache.ResetUAFPutDeclineCountersForTest)

	// The seed's inputs never carry HasUAF for a widget — the declaration lives
	// on the apiRef'd RESTAction, frames below. So the OBSERVED limb is the only
	// one that can fire here, which is exactly the R-1 shape.
	seedInputs := &cache.ResolvedKeyInputs{
		CacheEntryClass:        "widgets",
		Namespace:              h1NS,
		Name:                   h1WName,
		BindingUID:             "uid-shared-portal-binding",
		RepresentativeUsername: a1Alice,
	}

	_, sink := cache.WithUAFTouchedSink(context.Background())
	sink.Bump() // the apiRef chokepoint marked the widget's resolve
	if !declineWidgetUAFPut(seedInputs, sink) {
		t.Fatal("R-1 RED (widget seed carrier): the boot seed would Put a refilter-narrowed widget envelope under a " +
			"per-BINDING key held by a cohort REPRESENTATIVE — every co-bound cohort member reads the representative's " +
			"rows on their first paint. The seed must decline.")
	}
	if got := cache.WidgetsUAFPutDeclined(); got != 1 {
		t.Fatalf("the widget seed decline must tick the widgets-class counter; got %d", got)
	}

	// CONTROL: an identical seed unit whose resolve ran no refilter still seeds.
	_, cleanSink := cache.WithUAFTouchedSink(context.Background())
	if declineWidgetUAFPut(seedInputs, cleanSink) {
		t.Fatal("R-1 CONTROL BROKE: a widget seed whose resolve ran NO refilter must still be Put — otherwise the " +
			"gate has disabled the widget boot seed wholesale and every cold paint regresses")
	}
	if got := cache.WidgetsUAFPutDeclined(); got != 1 {
		t.Fatalf("control: a non-refiltered seed must not tick the decline counter; total went to %d", got)
	}
}
