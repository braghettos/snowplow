// Q-RBAC-DECOUPLE C(d) v3 regression fix — sibling-iterator detection.
//
// Anti-regression for the parent→child→sibling iterator topology that the
// v3 child-ref sentinel breaks (#37b7bce). Architect-confirmed root cause:
// the call_l1 / call_inline branches write an opaque sentinel into
// dict[parentApiName] which has no `.status` field; a sibling api[] entry
// that does dependsOn.iterator = ".<parent>.status…" walks into a null
// value, throws "must return a JSON array", and emits zero requests.
//
// The fix detects the topology by scanning apiMap for any sibling whose
// dependsOn.name equals the resolving entry's id, and inlines a refiltered
// child shape (legacy CR with .status) instead of the sentinel.
//
// Detection lives in hasSiblingDependsOn (resolve.go). The unit tests here
// cover the three documented cases:
//   • sibling-with-dependsOn: detection returns true → refilter path
//   • no sibling: detection returns false → sentinel-deflection path
//   • multiple siblings depending on same id: detection returns true once
package api

import (
	"testing"

	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

// makeAPIMap is a tiny constructor that mirrors how Resolve builds apiMap.
// It accepts a slice and returns the keyed-by-Name map runOne sees.
func makeAPIMap(items []*templates.API) map[string]*templates.API {
	m := make(map[string]*templates.API, len(items))
	for _, el := range items {
		if el == nil {
			continue
		}
		m[el.Name] = el
	}
	return m
}

func TestHasSiblingDependsOn_NoSibling_FalseDetection(t *testing.T) {
	// One api entry with no siblings; nothing depends on it. Default
	// deflection path applies — sentinel is the correct write.
	items := []*templates.API{
		{Name: "lonely", Path: "/call?resource=foo"},
	}
	if got := hasSiblingDependsOn(makeAPIMap(items), "lonely"); got {
		t.Errorf("no sibling case: expected false, got true")
	}
}

func TestHasSiblingDependsOn_SiblingWithoutDependsOn_FalseDetection(t *testing.T) {
	// Sibling exists but does NOT depend on the resolving entry.
	// Detection must remain false — sentinel still safe.
	items := []*templates.API{
		{Name: "parent", Path: "/call?resource=foo"},
		{Name: "unrelated", Path: "/api/v1/configmaps"},
	}
	if got := hasSiblingDependsOn(makeAPIMap(items), "parent"); got {
		t.Errorf("sibling-without-dependsOn case: expected false, got true")
	}
}

func TestHasSiblingDependsOn_SiblingDependsOnDifferentName_FalseDetection(t *testing.T) {
	// Sibling depends on a different api entry — must NOT trigger detection.
	items := []*templates.API{
		{Name: "parent", Path: "/call?resource=foo"},
		{Name: "other", Path: "/call?resource=bar"},
		{
			Name: "consumer",
			Path: "/api/v1/x",
			DependsOn: &templates.Dependency{
				Name:     "other",
				Iterator: ptr.To(".other.status[]"),
			},
		},
	}
	if got := hasSiblingDependsOn(makeAPIMap(items), "parent"); got {
		t.Errorf("sibling-depends-on-other case: expected false, got true")
	}
}

func TestHasSiblingDependsOn_OneSibling_TrueDetection(t *testing.T) {
	// The headline case: sibling iterates parent's output. Detection must
	// fire so the call_l1 site refilters inline instead of writing a
	// sentinel that the iterator can't consume.
	items := []*templates.API{
		{Name: "allNamespacesAndCrds", Path: "/call?resource=ns-and-crd"},
		{
			Name: "allCompositions",
			Path: "/api/v1/compositions",
			DependsOn: &templates.Dependency{
				Name:     "allNamespacesAndCrds",
				Iterator: ptr.To(".allNamespacesAndCrds.status[]"),
			},
		},
	}
	if got := hasSiblingDependsOn(makeAPIMap(items), "allNamespacesAndCrds"); !got {
		t.Errorf("one-sibling case: expected true, got false")
	}
}

func TestHasSiblingDependsOn_DirectDependsOnNoIterator_TrueDetection(t *testing.T) {
	// Per the fix's design comment: a direct `.<id>` reference from a
	// dependent has the same null-status problem the iterator does.
	// Detection key is just the dependsOn.name match, NOT iterator presence.
	items := []*templates.API{
		{Name: "parent", Path: "/call?resource=x"},
		{
			Name: "consumer",
			Path: "/api/v1/y",
			DependsOn: &templates.Dependency{
				Name: "parent",
				// Iterator intentionally nil — direct payload reference.
			},
		},
	}
	if got := hasSiblingDependsOn(makeAPIMap(items), "parent"); !got {
		t.Errorf("direct-dependsOn-no-iterator case: expected true, got false")
	}
}

func TestHasSiblingDependsOn_MultipleSiblingsSameId_TrueDetection(t *testing.T) {
	// Several siblings depend on the same parent. Detection must fire
	// (single sibling is sufficient); the fix's refilter path runs once
	// regardless of sibling count because dict[id] is set once.
	items := []*templates.API{
		{Name: "parent", Path: "/call?resource=p"},
		{
			Name: "consumerA",
			Path: "/api/v1/a",
			DependsOn: &templates.Dependency{
				Name:     "parent",
				Iterator: ptr.To(".parent.status[]"),
			},
		},
		{
			Name: "consumerB",
			Path: "/api/v1/b",
			DependsOn: &templates.Dependency{
				Name:     "parent",
				Iterator: ptr.To(".parent.status[].nested"),
			},
		},
		{
			Name: "consumerC",
			Path: "/api/v1/c",
			DependsOn: &templates.Dependency{
				Name: "parent", // direct reference
			},
		},
	}
	if got := hasSiblingDependsOn(makeAPIMap(items), "parent"); !got {
		t.Errorf("multiple-siblings-same-id case: expected true, got false")
	}
}

func TestHasSiblingDependsOn_SelfReferenceIgnored(t *testing.T) {
	// Defensive: an api entry that mentions itself in dependsOn (which
	// topologicalLevels would reject anyway, but cheap to assert here)
	// must NOT trigger detection — self is excluded by the sibling loop.
	items := []*templates.API{
		{
			Name: "parent",
			Path: "/call?resource=p",
			DependsOn: &templates.Dependency{
				Name: "parent",
			},
		},
	}
	if got := hasSiblingDependsOn(makeAPIMap(items), "parent"); got {
		t.Errorf("self-reference case: expected false (self excluded), got true")
	}
}

func TestHasSiblingDependsOn_NilEntryInMapSkipped(t *testing.T) {
	// Defensive: a nil entry in the map (defensive against caller bugs)
	// must not panic and must not contribute to the result.
	m := map[string]*templates.API{
		"parent":      {Name: "parent", Path: "/call?resource=p"},
		"nilSibling":  nil,
		"realSibling": {Name: "realSibling", DependsOn: &templates.Dependency{Name: "parent"}},
	}
	if got := hasSiblingDependsOn(m, "parent"); !got {
		t.Errorf("nil-entry case: expected true (real sibling matches), got false")
	}
	// And the inverse: a map with only nil entries returns false.
	mEmpty := map[string]*templates.API{
		"parent":     {Name: "parent"},
		"nilSibling": nil,
	}
	if got := hasSiblingDependsOn(mEmpty, "parent"); got {
		t.Errorf("nil-only-siblings case: expected false, got true")
	}
}

func TestHasSiblingDependsOn_EmptyMap(t *testing.T) {
	// Defensive: empty map must return false without panic.
	if got := hasSiblingDependsOn(nil, "parent"); got {
		t.Errorf("nil map: expected false, got true")
	}
	if got := hasSiblingDependsOn(map[string]*templates.API{}, "parent"); got {
		t.Errorf("empty map: expected false, got true")
	}
}

// TestRefilterOutput_HasStatusForIterator asserts the byte-shape contract
// the sibling-iterator fix relies on: RefilterRESTAction's output, when
// unmarshaled, has a top-level `status` field. The runOne refilter path
// writes this output (after jsonHandlerCompute filtering) into dict[id],
// and a sibling iterator JQ of the form `.<id>.status…` MUST be able to
// drill into it.
//
// This is the contract that would silently break if a future change to
// RefilterRESTAction renamed or restructured the status field. It ALSO
// proves the fix's "inlined refilter shape supports iterator dependents"
// claim at the byte level without needing envtest.
func TestRefilterOutput_HasStatusForIterator(t *testing.T) {
	// Same fixture shape as the v3 anti-defect test: a CR with a single
	// api[] entry, a UAF on namespaces, an outer JQ that flips the unfiltered
	// list into `{namespaces: <slice>}`. The refilter output must be a
	// legacy CR whose `.status.namespaces` an iterator can walk.
	cr := makeNSCR()
	dict := map[string]any{"ns": []any{"ns-a", "ns-b", "ns-c"}}
	raw := mustMarshalCachedFor(t, cr, dict)

	rbac := &stubRBACEvaluator{allow: map[string]map[string]bool{
		"user-a": {"ns-a": true, "ns-b": true},
	}}
	out, err := RefilterRESTAction(ctxWith(t, "user-a", rbac), nil, raw)
	if err != nil {
		t.Fatalf("RefilterRESTAction: %v", err)
	}
	got := extractStatusNS(out)
	if len(got) == 0 {
		t.Fatalf("expected non-empty status.namespaces in refilter output, got empty (raw=%s)", string(out))
	}
	// Sibling-iterator topology: a dependent api[] would do
	// `.<id>.status.namespaces[]` against {<id>: out}. The fact that
	// extractStatusNS returns non-empty proves the JQ path resolves.
}
