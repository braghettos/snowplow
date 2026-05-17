// deps_falsifier_test.go — Ship A (0.30.110) pre-flight falsifier triad.
//
// Team rule feedback_falsifier_first_before_ship: these tests are
// written BEFORE the production fix and MUST fail against the unfixed
// 0.30.109 dep tracker. The FAIL artifact is the pre-flight gate; only
// after capturing it does the implementation land.
//
//   F1 — ADD events reach nothing. RecordList a LIST-scope dep, fire an
//        ADD-equivalent, assert the dependent L1 key is dirty-marked
//        (enqueued into the refresh hook). FAILS today: OnAdd is a
//        no-op and AddFunc is unwired.
//
//   F2 — DELETE over-evicts. Build a widget L1 entry that GET-depends
//        on a DIFFERENT object (a RESTAction), DELETE that RESTAction,
//        assert the widget entry is DIRTY-MARKED, not evicted. FAILS
//        today: OnDelete evicts every collectMatches hit indiscriminately.
//
// (F3 lives in internal/objects — objects.Get must record a dep edge
// when invoked under a WithL1KeyContext ctx.)

package cache

import (
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// gvrRestActions is the templates.krateo.io/v1 RESTAction GVR — the
// "depended-upon" object in the F2 GET-dep scenario.
func gvrRestActions() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "templates.krateo.io",
		Version:  "v1",
		Resource: "restactions",
	}
}

// captureHook is a thread-safe refresh-hook recorder. Used by the
// falsifiers to assert which L1 keys got dirty-marked.
type captureHook struct {
	mu   sync.Mutex
	keys []string
}

func (h *captureHook) fn() func(string) {
	return func(k string) {
		h.mu.Lock()
		h.keys = append(h.keys, k)
		h.mu.Unlock()
	}
}

func (h *captureHook) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.keys))
	copy(out, h.keys)
	return out
}

func (h *captureHook) has(k string) bool {
	for _, x := range h.snapshot() {
		if x == k {
			return true
		}
	}
	return false
}

// --- F1 — ADD must dirty-mark LIST-scope dependents -------------------------

// TestFalsifierF1_AddDirtyMarksListDep records a LIST-scope dependency
// for an admin compositions-list L1 entry, then fires an ADD-equivalent
// for a brand-new object in that GVR + namespace. The dependent L1 key
// MUST be dirty-marked (enqueued into the refresh hook).
//
// FAILS on 0.30.109: OnAdd is a no-op — the enqueue list stays empty.
func TestFalsifierF1_AddDirtyMarksListDep(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	gvr := gvrCompositions()

	const adminL1 = "L1_admin_compositions"
	d.RecordList(adminL1, gvr, "bench-ns-07")

	hook := &captureHook{}
	d.SetRefreshHook(hook.fn())

	// Fire an ADD-equivalent for a freshly-created object in that scope.
	got := d.OnAdd(gvr, "bench-ns-07", "bench-app-07-new")

	if got != 1 {
		t.Fatalf("F1: OnAdd matched %d L1 keys, want 1 (LIST-dep)", got)
	}
	if !hook.has(adminL1) {
		t.Fatalf("F1: ADD did not dirty-mark the LIST-scope dependent %q; "+
			"enqueue list=%v — AddFunc unwired / OnAdd no-op", adminL1, hook.snapshot())
	}
}

// --- F2 — DELETE must NOT over-evict a dependent-GET entry -------------------

// TestFalsifierF2_DeleteDirtyMarksDependentGet builds a widget L1 entry
// (own object = widget W) that GET-depends on a SEPARATE RESTAction
// object R. A DELETE of R must DIRTY-MARK the widget entry (the widget
// itself still exists; only one of its inner-call dependencies went
// away) — it must NOT be evicted.
//
// FAILS on 0.30.109: OnDelete evicts every collectMatches hit, so the
// widget entry is dropped from the store even though it is not the
// self-representation of R.
func TestFalsifierF2_DeleteDirtyMarksDependentGet(t *testing.T) {
	d := newTestDepTracker(t, 1_000)
	store := newResolvedCache(100, 1<<20, time.Hour)
	d.SetStore(store)

	widgetGVR := schema.GroupVersionResource{
		Group:    "widgets.templates.krateo.io",
		Version:  "v1beta1",
		Resource: "buttons",
	}
	restGVR := gvrRestActions()

	// The widget L1 entry: its OWN object is widget "save-btn" in ns app.
	const widgetL1 = "L1_widget_save_btn"
	store.Put(widgetL1, &ResolvedEntry{
		RawJSON: []byte(`{"kind":"button"}`),
		Inputs: &ResolvedKeyInputs{
			CacheEntryClass: "widgets",
			Group:           widgetGVR.Group,
			Version:         widgetGVR.Version,
			Resource:        widgetGVR.Resource,
			Namespace:       "app",
			Name:            "save-btn",
		},
	})
	// The widget GET-depends on RESTAction "list-users" — a DIFFERENT
	// object. This is an exact (gvr, ns, name) dep, NOT the widget's own
	// self-representation.
	d.Record(widgetL1, restGVR, "app", "list-users")

	hook := &captureHook{}
	d.SetRefreshHook(hook.fn())

	// DELETE the RESTAction the widget depends on.
	evicted := d.OnDelete(restGVR, "app", "list-users")

	// The widget entry must SURVIVE in the store (dirty-marked, not evicted).
	if _, ok := store.Get(widgetL1); !ok {
		t.Fatalf("F2: DELETE of dependency RESTAction over-evicted the widget "+
			"L1 entry %q — OnDelete evicts every collectMatches hit "+
			"indiscriminately (evicted=%d)", widgetL1, evicted)
	}
	// And it must have been dirty-marked for stale-while-revalidate.
	if !hook.has(widgetL1) {
		t.Fatalf("F2: dependent-GET entry %q was not dirty-marked on DELETE; "+
			"enqueue list=%v", widgetL1, hook.snapshot())
	}
	// evictDeleteTotal counts self-evictions only — this DELETE hit no
	// self-representation, so it must stay 0.
	if got := d.evictDeleteTotal.Load(); got != 0 {
		t.Fatalf("F2: evictDeleteTotal=%d want 0 (no self-representation deleted)", got)
	}
}
