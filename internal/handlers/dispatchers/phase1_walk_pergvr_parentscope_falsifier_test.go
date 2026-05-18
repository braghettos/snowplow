// phase1_walk_pergvr_parentscope_falsifier_test.go — Ship 0.30.126
// falsifier for the per-(parent,GVR) data-fan-out budget key.
//
// THE BUG (0.30.125): phase1Walker.gvrSamples was keyed on the BARE GVR
// (apiVersion|resource). The Compositions Page DataGrid's
// resourcesRefsTemplate floods the `panels` GVR with per-composition
// children; the walk samples phase1PerGVRSampleLimit (=4) and the shared
// `panels` budget saturates. The Dashboard page's STRUCTURAL `panels`
// widget — same GVR, DIFFERENT parent — is then skipped by the
// data-fan-out gate, so the walk never descends
// dashboard-page -> dashboard-compositions-panel -> row -> table and
// never reaches `spec.apiRef: compositions-list`. F2's apiRef harvester
// rides this walk, so `compositions-list` is never harvested and the
// content-prewarm pass never warms it — admin's first navigation then
// pays a cold composition-informer relist (~70 s wall). Measured on the
// 0.30.125 validation bench: admin page-load 30,630 ms, mix-weighted
// 5,155 ms — vs the <=1 s north-star.
//
// THE FIX: re-key gvrSamples to (parentEndpoint, GVR) via
// parentScopedGVRKey. Each parent widget's same-GVR children get an
// independent phase1PerGVRSampleLimit budget — the flood is still capped
// PER PARENT (the 49K-resolution storm stays bounded — C1), and a
// sibling parent's structural widgets are no longer starved.
//
// The real phase1Walker.walk drives widgets.Resolve + objects.Get, both
// of which need a live apiserver — so it cannot run hermetically (see
// phase1_walk_bounds_test.go). This falsifier therefore exercises the
// fix at the smallest faithful seam: it replicates walk's EXACT
// per-child decision (the gvrKey construction + the gvrSamples cap gate
// + harvestApiRef on each descended node) over a fixture nav tree, once
// with the pre-fix BARE-GVR key and once with the real post-fix
// parentScopedGVRKey, and asserts the harvested set diverges exactly as
// the bug predicts.

package dispatchers

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// --- fixture nav tree ----------------------------------------------------

// pgvrChild is one resourcesRefs child in the fixture: its GVR + the
// endpoint key it would be reached under + (for a leaf) the apiRef it
// carries.
type pgvrChild struct {
	parentEndpoint string // navWidgetEndpointKey of this child's parent
	endpoint       string // this child's own navWidgetEndpointKey
	apiVersion     string
	resource       string
	apiRefName     string // non-empty ⇒ this widget carries spec.apiRef
	apiRefNS       string
}

// pgvrFixtureTree models the architect's falsifier fixture, in the
// child-iteration ORDER the walk would see them — the flood-datagrid's
// children come FIRST (the resourcesRefsTemplate fan-out is iterated
// before the structural sibling), so the bare-GVR budget is already
// drained when the structural page's child is reached.
//
//	root
//	 ├── flood-datagrid              (parent: root)
//	 │     └── 10 × panels children  (parent: flood-datagrid)   ← floods `panels`
//	 └── structural-page             (parent: root)
//	       └── 1 × panels child       (parent: structural-page)
//	             → leaf carries spec.apiRef: target-restaction
//
// Both `flood-datagrid`'s children and `structural-page`'s child are the
// SAME GVR (widgets…/panels) but under DIFFERENT parents.
const (
	pgvrPanelsAPI  = "widgets.templates.krateo.io/v1beta1"
	pgvrPanelsRes  = "panels"
	pgvrFloodEP    = "widgets.templates.krateo.io/v1beta1|datagrids|krateo-system|flood-datagrid"
	pgvrStructEP   = "widgets.templates.krateo.io/v1beta1|pages|krateo-system|structural-page"
	pgvrTargetName = "target-restaction" // the apiRef the structural leaf carries (stands in for compositions-list)
	pgvrTargetNS   = "krateo-system"
)

// pgvrFixtureChildren returns the children the walk iterates, in order:
// the flood first (10 panels under flood-datagrid), then the structural
// page's single panel (which carries the target apiRef).
func pgvrFixtureChildren() []pgvrChild {
	var out []pgvrChild
	// The flood: 10 panels children under flood-datagrid.
	for i := 0; i < 10; i++ {
		out = append(out, pgvrChild{
			parentEndpoint: pgvrFloodEP,
			endpoint:       pgvrPanelsAPI + "|" + pgvrPanelsRes + "|krateo-system|flood-panel-" + string(rune('a'+i)),
			apiVersion:     pgvrPanelsAPI,
			resource:       pgvrPanelsRes,
		})
	}
	// The structural page's single panel — same GVR, different parent,
	// carries the target apiRef.
	out = append(out, pgvrChild{
		parentEndpoint: pgvrStructEP,
		endpoint:       pgvrPanelsAPI + "|" + pgvrPanelsRes + "|krateo-system|structural-panel",
		apiVersion:     pgvrPanelsAPI,
		resource:       pgvrPanelsRes,
		apiRefName:     pgvrTargetName,
		apiRefNS:       pgvrTargetNS,
	})
	return out
}

// pgvrLeafWidget builds the unstructured widget CR a descended child
// represents — carrying spec.apiRef when apiRefName is set, so
// harvestApiRef records it exactly as the real walk does.
func pgvrLeafWidget(c pgvrChild) *unstructured.Unstructured {
	spec := map[string]any{}
	if c.apiRefName != "" {
		apiRef := map[string]any{"name": c.apiRefName}
		if c.apiRefNS != "" {
			apiRef["namespace"] = c.apiRefNS
		}
		spec["apiRef"] = apiRef
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": c.apiVersion,
		"kind":       "Panel",
		"metadata":   map[string]any{"namespace": "krateo-system", "name": "panel"},
		"spec":       spec,
	}}
}

// runFixtureWalk replays the walk's per-child sample decision over the
// fixture, using keyFn to build the gvrSamples budget key. It returns
// the harvested apiRef set and, per parent endpoint, how many children
// of pgvrPanelsRes were "descended" (the C1 flood-cap measurement).
//
// keyFn is the ONLY difference between the pre-fix and post-fix runs:
//   - pre-fix  → bare GVR  (apiVersion|resource)
//   - post-fix → parentScopedGVRKey(parentEndpoint, apiVersion, resource)
// Everything else — the cap gate, the increment, harvestApiRef — is the
// EXACT logic phase1Walker.walk runs.
func runFixtureWalk(keyFn func(parentEP, apiVersion, resource string) string) (
	harvested map[string]bool, descendedPerParent map[string]int) {

	gvrSamples := map[string]int{}
	harvester := newContentPrewarmHarvester()
	descendedPerParent = map[string]int{}

	for _, c := range pgvrFixtureChildren() {
		gvrKey := keyFn(c.parentEndpoint, c.apiVersion, c.resource)
		// The data-fan-out gate — EXACTLY walk's `>= phase1PerGVRSampleLimit`.
		if gvrSamples[gvrKey] >= phase1PerGVRSampleLimit {
			continue // skipped — not descended, apiRef not harvested
		}
		gvrSamples[gvrKey]++
		descendedPerParent[c.parentEndpoint]++
		// Descended ⇒ the walk fetches the widget CR and harvestApiRef
		// records its spec.apiRef — the EXACT call walk makes at :645.
		harvester.harvestApiRef(pgvrLeafWidget(c))
	}

	harvested = map[string]bool{}
	for _, ref := range harvester.snapshot() {
		harvested[ref.Namespace+"/"+ref.Name] = true
	}
	return harvested, descendedPerParent
}

// bareGVRKey is the PRE-FIX (0.30.125) budget key — the bug under test.
func bareGVRKey(_ /*parentEP*/, apiVersion, resource string) string {
	return apiVersion + "|" + resource
}

// --- the falsifier -------------------------------------------------------

// TestFAL126_PreFix_BareGVRKeyStarvesStructuralSibling captures the
// pre-fix defect: with the bare-GVR budget key the flood-datagrid's 10
// panels children drain the single shared `panels` budget, so the
// structural page's panel — same GVR, different parent — is skipped and
// its `target-restaction` apiRef is NEVER harvested.
//
// This is the captured pre-fix FAIL artifact: it asserts the BUGGY
// outcome (target NOT harvested) and PASSES on bare-GVR keying — proving
// the defect is real and reproducible.
func TestFAL126_PreFix_BareGVRKeyStarvesStructuralSibling(t *testing.T) {
	harvested, descended := runFixtureWalk(bareGVRKey)

	// The bug: the structural sibling's apiRef is starved out.
	if harvested[pgvrTargetNS+"/"+pgvrTargetName] {
		t.Fatalf("pre-fix expectation broken: with the bare-GVR key the target "+
			"apiRef should be STARVED (not harvested) — got it harvested")
	}
	// The flood drained the single shared budget — only 4 descended total,
	// all under the flood parent; the structural parent got 0.
	if descended[pgvrStructEP] != 0 {
		t.Fatalf("pre-fix: structural parent should be fully starved (0 descended), "+
			"got %d", descended[pgvrStructEP])
	}
	if descended[pgvrFloodEP] != phase1PerGVRSampleLimit {
		t.Fatalf("pre-fix: flood parent should have drained the shared budget "+
			"(%d descended), got %d", phase1PerGVRSampleLimit, descended[pgvrFloodEP])
	}
}

// TestFAL126_PostFix_ParentScopedKeyReachesStructuralSibling is the
// post-fix proof: with the real parentScopedGVRKey the flood-datagrid's
// budget and the structural page's budget are INDEPENDENT, so the
// structural panel IS descended and `target-restaction` IS harvested.
func TestFAL126_PostFix_ParentScopedKeyReachesStructuralSibling(t *testing.T) {
	harvested, descended := runFixtureWalk(parentScopedGVRKey)

	// THE FIX: the target apiRef IS harvested — the structural sibling is
	// no longer starved by the flood.
	if !harvested[pgvrTargetNS+"/"+pgvrTargetName] {
		t.Fatalf("FAL-126 FIX FAILED: with the parent-scoped GVR key the structural "+
			"page's panel must be descended and its apiRef %q harvested — got it "+
			"MISSING. The flood still starves the sibling.", pgvrTargetName)
	}
	// The structural parent got its own independent budget — its 1 panel
	// child descended.
	if descended[pgvrStructEP] != 1 {
		t.Fatalf("FAL-126: structural parent must descend its 1 panel under its own "+
			"budget, got %d", descended[pgvrStructEP])
	}

	// C1 — PM HARD GATE: the per-parent flood cap MUST still hold. The
	// flood-datagrid offered 10 same-GVR children; the walk must descend
	// at most phase1PerGVRSampleLimit of them. A regression here
	// re-introduces the 49K-resolution storm.
	if descended[pgvrFloodEP] > phase1PerGVRSampleLimit {
		t.Fatalf("C1 VIOLATION (PM hard gate): the flood-datagrid descended %d of its "+
			"10 same-GVR children — the per-parent cap phase1PerGVRSampleLimit=%d MUST "+
			"still hold or the 49K-resolution storm returns", descended[pgvrFloodEP],
			phase1PerGVRSampleLimit)
	}
	if descended[pgvrFloodEP] != phase1PerGVRSampleLimit {
		t.Fatalf("C1: the flood-datagrid should descend exactly %d children (its full "+
			"per-parent budget), got %d", phase1PerGVRSampleLimit, descended[pgvrFloodEP])
	}
}

// TestFAL126_ParentScopedKeyConstruction pins the real parentScopedGVRKey
// helper: two different parents with the SAME GVR produce DIFFERENT keys
// (independent budgets); the same (parent,GVR) produces a stable key.
func TestFAL126_ParentScopedKeyConstruction(t *testing.T) {
	floodKey := parentScopedGVRKey(pgvrFloodEP, pgvrPanelsAPI, pgvrPanelsRes)
	structKey := parentScopedGVRKey(pgvrStructEP, pgvrPanelsAPI, pgvrPanelsRes)
	if floodKey == structKey {
		t.Fatalf("FAL-126: two parents with the same GVR must get DIFFERENT budget "+
			"keys — got identical %q (the bug: one shared budget)", floodKey)
	}
	// Stable for the same (parent, GVR).
	if parentScopedGVRKey(pgvrFloodEP, pgvrPanelsAPI, pgvrPanelsRes) != floodKey {
		t.Fatalf("FAL-126: parentScopedGVRKey must be deterministic for the same inputs")
	}
	// The bare-GVR key (pre-fix) collapses the two parents to one key —
	// the root cause, pinned for contrast.
	if bareGVRKey(pgvrFloodEP, pgvrPanelsAPI, pgvrPanelsRes) !=
		bareGVRKey(pgvrStructEP, pgvrPanelsAPI, pgvrPanelsRes) {
		t.Fatalf("contrast pin: the bare-GVR key should collapse both parents to one "+
			"key — that collapse is exactly the 0.30.125 bug")
	}
}
