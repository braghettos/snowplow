// P1.2 Phase 1 — event-driven singleflight L1 refresh.
//
// Replaces the tick/debounce/cap pattern with a purely reactive coalescer:
// at most one refresh goroutine per GVR; events that arrive while a refresh
// is running are stashed into a per-GVR pending set that the running
// goroutine drains on its next iteration. Zero timers, zero idle cost.
//
// Currently wired alongside the legacy scheduleDynamicReconcile + scanL3Gens
// paths behind env flag SNOWPLOW_SINGLEFLIGHT_L1=1 so it can be enabled on a
// canary without deleting the old paths. Phases 2-4 retire the legacy paths.
package cache

import (
	"context"
	"os"
	"runtime"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"log/slog"
)

// singleflightEnabled reflects SNOWPLOW_SINGLEFLIGHT_L1 at process start.
// Read once to avoid env lookups on every event.
var singleflightEnabled = os.Getenv("SNOWPLOW_SINGLEFLIGHT_L1") == "1"

// l1SingleflightState holds the per-GVR coalescer state.
//
// Invariant: running transitions false→true only inside triggerL1RefreshSingleflight
// under mu, and true→false only inside runRefreshLoop under mu. Events that see
// running==true deposit their work into pending under mu; the running goroutine
// drains pending under mu at each iteration. This guarantees no lost events.
type l1SingleflightState struct {
	mu             sync.Mutex
	running        bool
	pending        map[string]bool
	pendingChanges []L1ChangeInfo
}

// getOrCreateSingleflightState returns (and lazily creates) the per-GVR state.
func (rw *ResourceWatcher) getOrCreateSingleflightState(gvrKey string) *l1SingleflightState {
	if v, ok := rw.singleflightStates.Load(gvrKey); ok {
		return v.(*l1SingleflightState)
	}
	st := &l1SingleflightState{pending: make(map[string]bool)}
	actual, _ := rw.singleflightStates.LoadOrStore(gvrKey, st)
	return actual.(*l1SingleflightState)
}

// triggerL1RefreshSingleflight is the entry point called from handleEvent after
// the per-item L3 write completes. Events arriving during an in-flight refresh
// are deposited into the pending set and return in <1µs.
func (rw *ResourceWatcher) triggerL1RefreshSingleflight(ctx context.Context, gvr schema.GroupVersionResource, ns, name, op string) {
	if !singleflightEnabled {
		return
	}
	gvrKey := GVRToKey(gvr)

	// Collect affected L1 keys for this change. Uses the precise dep indexes
	// (same approach as reconcileGVR and scanL3Gens, deliberately excluding
	// the broad L1GVRKey index).
	keys := rw.collectAffectedL1KeysForChange(ctx, gvr, gvrKey, ns)
	if len(keys) == 0 {
		return
	}

	change := L1ChangeInfo{GVR: gvr, Namespace: ns, Name: name, Operation: op}
	st := rw.getOrCreateSingleflightState(gvrKey)

	st.mu.Lock()
	if st.running {
		// A refresh is in flight — deposit our work and return. The running
		// goroutine will pick this up in its next drain iteration.
		for k := range keys {
			st.pending[k] = true
		}
		st.pendingChanges = append(st.pendingChanges, change)
		st.mu.Unlock()
		return
	}
	// No refresh running — we own the slot. Transition to running under the
	// lock so no concurrent caller can also grab the slot.
	st.running = true
	initialKeys := keys
	initialChanges := []L1ChangeInfo{change}
	st.mu.Unlock()

	go rw.runRefreshLoop(gvr, st, initialKeys, initialChanges)
}

// collectAffectedL1KeysForChange resolves a changed (gvr, ns) to the set of
// L1 keys that depend on it via the precise dep indexes, then expands the
// transitive dependency tree. Same semantics as the collect() closure in
// reconcileGVR / scanL3Gens. Distinct from the legacy collectAffectedL1Keys
// in watcher.go: that one is LIST+GET for a specific (ns,name) returning a
// slice and without tree expansion; this one is LIST-level for bursts,
// includes the CRD chain, and returns a set for merging into pending.
func (rw *ResourceWatcher) collectAffectedL1KeysForChange(ctx context.Context, gvr schema.GroupVersionResource, gvrKey, ns string) map[string]bool {
	affected := make(map[string]bool)
	collect := func(idxKey string) {
		if members, err := rw.cache.SMembers(ctx, idxKey); err == nil {
			for _, m := range members {
				affected[m] = true
			}
		}
	}
	// Namespace-scoped LIST dep — only for the namespace that changed.
	if ns != "" {
		collect(L1ResourceDepKey(gvrKey, ns, ""))
	}
	// Cluster-wide LIST dep.
	collect(L1ResourceDepKey(gvrKey, "", ""))
	// API-level dep.
	collect(L1ApiDepKey(gvrKey))
	// CRD-based chain: widgets that depend on the CRD LIST (typically
	// compositions-get-ns-and-crd). Matches reconcileGVR's behavior.
	if gvr.Group != "" && gvr.Resource != "" {
		crdGVRKey := GVRToKey(schema.GroupVersionResource{
			Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
		})
		collect(L1ResourceDepKey(crdGVRKey, "", ""))
	}
	if len(affected) == 0 {
		return affected
	}
	// Expand transitive dependents (compositions-list → piechart → table).
	rw.expandDependents(ctx, affected, 5)
	return affected
}

// runRefreshLoop is the per-GVR refresh goroutine. It processes the initial
// work unit and then drains pending under the lock until empty. Only one of
// these exists per GVR at any time (enforced by the running flag).
func (rw *ResourceWatcher) runRefreshLoop(gvr schema.GroupVersionResource, st *l1SingleflightState, keys map[string]bool, changes []L1ChangeInfo) {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			slog.Error("resource-watcher: panic in singleflight refresh loop",
				slog.String("gvr", gvr.String()),
				slog.Any("error", r),
				slog.String("stack", string(buf[:n])))
			// Release the slot so future events can restart the loop.
			st.mu.Lock()
			st.running = false
			st.pending = make(map[string]bool)
			st.pendingChanges = nil
			st.mu.Unlock()
		}
	}()

	for {
		fn, ok := rw.l1Refresh.Load().(L1RefreshFunc)
		if !ok || fn == nil {
			// L1 refresh not wired yet — drop the work; subsequent events
			// will re-trigger once fn is available.
			st.mu.Lock()
			st.running = false
			st.pending = make(map[string]bool)
			st.pendingChanges = nil
			st.mu.Unlock()
			return
		}

		keyList := make([]string, 0, len(keys))
		for k := range keys {
			keyList = append(keyList, k)
		}

		ctx, span := watcherTracer.Start(context.Background(), "singleflight.refresh",
			trace.WithAttributes(
				attribute.String("gvr", gvr.String()),
				attribute.Int("keys", len(keyList)),
				attribute.Int("changes", len(changes)),
			))
		refreshCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		fn(refreshCtx, gvr, keyList, changes)
		cancel()
		span.End()

		// Drain pending under lock. Atomic: either we hand off to the next
		// iteration or we release the slot. No in-between state.
		st.mu.Lock()
		if len(st.pending) == 0 {
			st.running = false
			st.pendingChanges = nil
			st.mu.Unlock()
			return
		}
		keys = st.pending
		changes = st.pendingChanges
		st.pending = make(map[string]bool)
		st.pendingChanges = nil
		st.mu.Unlock()
	}
}
