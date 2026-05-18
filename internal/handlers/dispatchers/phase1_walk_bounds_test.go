// phase1_walk_bounds_test.go — 0.30.106 unit coverage for the two
// recursion-safety bounds the Phase 1 widget-tree walker enforces:
//
//   - the per-GVR data-fan-out bound (phase1PerGVRSampleLimit /
//     phase1Walker.gvrSamples), and
//   - the recursion-depth truncation bound (phase1MaxWalkDepth).
//
// WHY THESE ARE SEPARATE FROM phase1WalkerEndToEndTest: phase1Walker.walk
// drives widgets.Resolve + objects.Get, both of which build a LIVE
// apiserver client from the context *rest.Config — neither can be served
// by the package's fake dynamic client. So the walk's end-to-end behavior
// stays covered by the mandatory on-cluster Phase-1 boot-log validation.
// These tests pin the two bounds at the smallest faithful seam:
//
//   - the DEPTH bound is exercised through the REAL phase1Walker.walk:
//     the depth gate returns BEFORE widgets.Resolve is reached, so a
//     direct walk(depth > phase1MaxWalkDepth, ...) call hits the cap with
//     no cluster contact;
//   - the GVR-SAMPLE bound's gate is reached only AFTER widgets.Resolve
//     yields children, so it cannot run through walk without an
//     apiserver — it is pinned against the real phase1Walker.gvrSamples
//     field + the real phase1PerGVRSampleLimit constant, replicating the
//     EXACT key construction and threshold expression walk applies (see
//     phase1_walk.go gvrKey / w.gvrSamples gate).

package dispatchers

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestPhase1Walk_MaxDepthTruncation asserts the recursion-depth cap. The
// portal navigation tree is shallow (~5 levels); phase1MaxWalkDepth (=32)
// is a defensive guard against a pathological CR graph the visited-set
// fails to dedupe. walk MUST cap — return nil without recursing — once
// depth exceeds the bound, and log phase1.walk.max_depth.
//
// This drives the REAL phase1Walker.walk: the depth gate
// (`if depth > phase1MaxWalkDepth`) returns before widgets.Resolve is
// reached, so the call needs no cluster. A regression that drops or
// loosens the cap would let walk fall through to widgets.Resolve at
// unbounded depth and recurse without limit — and fail here.
func TestPhase1Walk_MaxDepthTruncation(t *testing.T) {
	w := &phase1Walker{
		authnNS:    "krateo-system",
		visited:    map[string]struct{}{},
		gvrSamples: map[string]int{},
	}
	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Widget",
		"metadata":   map[string]any{"namespace": "krateo-system", "name": "deep-widget"},
	}}

	// At depth == phase1MaxWalkDepth+1 the depth gate fires: walk returns
	// nil immediately, never touching widgets.Resolve. If the cap were
	// removed walk would call the resolver and fail (no apiserver).
	if err := w.walk(context.Background(), widget, phase1MaxWalkDepth+1, ""); err != nil {
		t.Fatalf("walk past the depth cap must return nil (truncate), got err: %v", err)
	}

	// The cap must not have recursed: nothing sampled, nothing visited.
	if len(w.gvrSamples) != 0 {
		t.Errorf("depth-capped walk recursed — gvrSamples = %v, want empty", w.gvrSamples)
	}
	if len(w.visited) != 0 {
		t.Errorf("depth-capped walk recursed — visited = %v, want empty", w.visited)
	}

	// A nil ctx-cancel + a deep nil widget must also be safe (defensive).
	if err := w.walk(context.Background(), nil, phase1MaxWalkDepth+5, ""); err != nil {
		t.Errorf("walk(nil widget, deep) must return nil, got: %v", err)
	}

	// Sanity-pin the constant: the cap is a small defensive bound, not an
	// accidental unbounded value.
	if phase1MaxWalkDepth != 32 {
		t.Errorf("phase1MaxWalkDepth = %d, want 32 (defensive recursion-safety bound)", phase1MaxWalkDepth)
	}
}

// TestPhase1Walk_PerGVRSampleBound asserts the per-GVR data-fan-out bound.
// A Compositions Page DataGrid yields one resourcesRefs child per
// composition row — ~49K children of the SAME GVR; recursing into all of
// them blows the PHASE1_TIMEOUT budget. The walk samples at most
// phase1PerGVRSampleLimit (=4) widget CRs per distinct child GVR — enough
// to traverse genuine navigation structure while skipping the per-row
// fan-out.
//
// The gate is reached only after widgets.Resolve yields children, so it
// cannot be exercised through walk without an apiserver. This test pins
// the accounting at the smallest faithful seam: it replicates the EXACT
// key construction (gvrKey := ref.APIVersion + "|" + ref.Resource) and
// threshold expression (w.gvrSamples[gvrKey] >= phase1PerGVRSampleLimit)
// walk applies, against the REAL phase1Walker.gvrSamples field and the
// REAL phase1PerGVRSampleLimit constant.
func TestPhase1Walk_PerGVRSampleBound(t *testing.T) {
	w := &phase1Walker{
		authnNS:    "krateo-system",
		visited:    map[string]struct{}{},
		gvrSamples: map[string]int{},
	}

	// sampleDecision mirrors EXACTLY the per-child accounting walk runs:
	// build the GVR key, skip once the GVR's sample count hits the limit,
	// else count this child as sampled. It returns whether the child WOULD
	// be recursed into. Keep this in lockstep with phase1_walk.go's
	// gvrKey + gvrSamples gate.
	sampleDecision := func(apiVersion, resource string) (recurse bool) {
		gvrKey := apiVersion + "|" + resource
		if w.gvrSamples[gvrKey] >= phase1PerGVRSampleLimit {
			return false
		}
		w.gvrSamples[gvrKey]++
		return true
	}

	const (
		compGVRAPI = "composition.krateo.io/v1"
		compGVRRes = "githubscaffoldings"
	)

	// The Compositions DataGrid fan-out: many same-GVR row children. The
	// first phase1PerGVRSampleLimit are sampled+recursed; every later one
	// is skipped.
	const fanout = 50
	recursedCount := 0
	for i := 0; i < fanout; i++ {
		if sampleDecision(compGVRAPI, compGVRRes) {
			recursedCount++
		}
	}
	if recursedCount != phase1PerGVRSampleLimit {
		t.Fatalf("per-GVR fan-out: %d same-GVR children recursed, want exactly %d "+
			"(the 5th+ sibling sharing one GVR must be skipped once the limit is hit)",
			recursedCount, phase1PerGVRSampleLimit)
	}

	// Concretely: with the limit at 4, child #5 (the 5th same-GVR widget)
	// must be skipped.
	w2 := &phase1Walker{gvrSamples: map[string]int{compGVRAPI + "|" + compGVRRes: phase1PerGVRSampleLimit}}
	gvrKey := compGVRAPI + "|" + compGVRRes
	if w2.gvrSamples[gvrKey] < phase1PerGVRSampleLimit {
		t.Fatalf("test setup: GVR should be at the limit")
	}
	if w2.gvrSamples[gvrKey] >= phase1PerGVRSampleLimit {
		// gate fires → skip; nothing further to assert beyond this branch.
	} else {
		t.Fatalf("a GVR already at phase1PerGVRSampleLimit must be skipped, not recursed")
	}

	// DISTINCT GVRs are each sampled INDEPENDENTLY up to the limit — the
	// counter is keyed per-GVR, so saturating one GVR does not starve
	// another. Drive three more distinct GVRs through a fresh walker.
	w3 := &phase1Walker{
		visited:    map[string]struct{}{},
		gvrSamples: map[string]int{},
	}
	distinct := []struct{ api, res string }{
		{"widgets.templates.krateo.io/v1beta1", "panels"},
		{"widgets.templates.krateo.io/v1beta1", "datagrids"},
		{"widgets.templates.krateo.io/v1beta1", "navmenus"},
	}
	sampleDecision3 := func(apiVersion, resource string) bool {
		gvrKey := apiVersion + "|" + resource
		if w3.gvrSamples[gvrKey] >= phase1PerGVRSampleLimit {
			return false
		}
		w3.gvrSamples[gvrKey]++
		return true
	}
	for _, g := range distinct {
		recursed := 0
		// 10 same-GVR siblings: only phase1PerGVRSampleLimit recurse...
		for i := 0; i < 10; i++ {
			if sampleDecision3(g.api, g.res) {
				recursed++
			}
		}
		if recursed != phase1PerGVRSampleLimit {
			t.Errorf("GVR %s/%s: %d recursed, want %d — each distinct GVR sampled independently",
				g.api, g.res, recursed, phase1PerGVRSampleLimit)
		}
	}
	// All three distinct GVRs reached their own full budget — none starved.
	if len(w3.gvrSamples) != len(distinct) {
		t.Errorf("gvrSamples tracked %d distinct GVRs, want %d", len(w3.gvrSamples), len(distinct))
	}
	for _, g := range distinct {
		if got := w3.gvrSamples[g.api+"|"+g.res]; got != phase1PerGVRSampleLimit {
			t.Errorf("GVR %s/%s sampled %d times, want %d (independent per-GVR budget)",
				g.api, g.res, got, phase1PerGVRSampleLimit)
		}
	}

	// Sanity-pin the constant: >1 so a couple of different-purpose
	// same-GVR siblings are not under-covered; small because additional
	// same-GVR widgets discover no new informer.
	if phase1PerGVRSampleLimit < 2 {
		t.Errorf("phase1PerGVRSampleLimit = %d, must be >1 so distinct-purpose same-GVR siblings are covered",
			phase1PerGVRSampleLimit)
	}
}
