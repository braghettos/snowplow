// deps_watch.go — Ship A (0.30.110): the informer→DepTracker event
// bridge.
//
// Pre-0.30.110 every registration path (addResourceTypeLocked and
// addResourceTypeMetadataOnlyLocked) inlined a near-identical
// ResourceEventHandlerFuncs literal that wired UpdateFunc + DeleteFunc
// and deliberately LEFT AddFunc unwired. Ship A:
//
//   R1 — wires AddFunc on BOTH paths via the single shared builder
//        depEventHandlers. ADD is gated by the initial-replay gate:
//        propagate only once the GVR's syncCh is closed (post-sync);
//        drop (counter addDroppedPreSync) during the informer's initial
//        LIST replay, where every existing object arrives as an ADD and
//        re-dirty-marking the whole world would be pointless churn.
//
//   R3 — the DELETE self-eviction burst must not run inline on the
//        informer processor goroutine. DeleteFunc hands DepTracker.
//        OnDelete to a single bounded worker goroutine draining
//        deleteEvictCh; the processor goroutine returns immediately.
//
//   O14 — a nil syncCh at AddFunc time is a fail-safe "drop": one-shot
//        WARN, treat as pre-sync (never propagate, never block).

package cache

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/runtime/schema"
	clientcache "k8s.io/client-go/tools/cache"
)

// deleteEvictQueueDepth bounds the R3 DELETE-event handoff channel. Each
// buffered slot is one DELETE event; 1024 is ample headroom for a
// DELETE storm without unbounded memory. A full channel falls back to
// inline OnDelete (correctness over the R3 off-processor guarantee).
const deleteEvictQueueDepth = 1024

// depWatchCounters holds the Ship A informer-bridge falsifier counters.
// Process-scoped (the bridge wires one DepTracker singleton); atomic so
// they are readable without a lock.
type depWatchCounters struct {
	addDroppedPreSync atomic.Uint64 // ADD events dropped during initial replay
	addPropagated     atomic.Uint64 // ADD events propagated post-sync
	addNilSyncCh      atomic.Uint64 // ADD events seen with a nil syncCh (O14)
	deleteQueueFull   atomic.Uint64 // DELETE events run inline on a full queue
}

// depWatch is the process-scoped informer→DepTracker bridge state: the
// counters plus the R3 DELETE-eviction worker.
type depWatch struct {
	counters depWatchCounters

	// deleteEvictCh carries DELETE events to the worker goroutine.
	deleteEvictCh chan depDeleteEvent

	startOnce sync.Once
	stopCh    chan struct{}
	workerWG  sync.WaitGroup

	// nilSyncWarned ensures the O14 nil-syncCh WARN fires at most once.
	nilSyncWarned atomic.Bool
}

// depDeleteEvent is one queued DELETE handed to the eviction worker.
type depDeleteEvent struct {
	gvr       schema.GroupVersionResource
	namespace string
	name      string
}

var (
	depWatchInstance *depWatch
	depWatchOnce     sync.Once
)

// depWatchSingleton returns the process-scoped bridge, lazily building
// it. Always non-nil.
func depWatchSingleton() *depWatch {
	depWatchOnce.Do(func() {
		depWatchInstance = &depWatch{
			deleteEvictCh: make(chan depDeleteEvent, deleteEvictQueueDepth),
			stopCh:        make(chan struct{}),
		}
	})
	return depWatchInstance
}

// startDeleteWorker spawns the single DELETE-eviction worker goroutine
// exactly once (sync.Once-bounded). The worker drains deleteEvictCh and
// runs Deps().OnDelete OFF the informer processor goroutine. It exits on
// stopCh close (test cleanup); production never stops it — its lifetime
// is the process lifetime.
func (w *depWatch) startDeleteWorker() {
	w.startOnce.Do(func() {
		w.workerWG.Add(1)
		go func() {
			defer w.workerWG.Done()
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("deps.delete_worker.panic",
						slog.String("subsystem", "cache"),
						slog.Any("panic", rec),
					)
				}
			}()
			for {
				select {
				case <-w.stopCh:
					// Drain queued events before exit so test teardown
					// is deterministic.
					for {
						select {
						case ev := <-w.deleteEvictCh:
							Deps().OnDelete(ev.gvr, ev.namespace, ev.name)
						default:
							return
						}
					}
				case ev := <-w.deleteEvictCh:
					Deps().OnDelete(ev.gvr, ev.namespace, ev.name)
				}
			}
		}()
	})
}

// submitDeleteEvent hands a DELETE event to the worker goroutine (R3).
// On a full queue it falls back to running OnDelete inline — correctness
// over the off-processor guarantee — and counts deleteQueueFull.
func (w *depWatch) submitDeleteEvent(ev depDeleteEvent) {
	w.startDeleteWorker()
	select {
	case w.deleteEvictCh <- ev:
	default:
		w.counters.deleteQueueFull.Add(1)
		slog.Warn("deps.delete_queue_full",
			slog.String("subsystem", "cache"),
			slog.String("gvr", ev.gvr.String()),
			slog.String("hint", "DELETE storm outran the eviction worker — running OnDelete inline"),
		)
		Deps().OnDelete(ev.gvr, ev.namespace, ev.name)
	}
}

// stopDeleteWorker closes the worker stop channel and blocks until the
// worker goroutine has exited (and drained pending events). Used by the
// _test.go shim; production code MUST NOT call it.
func (w *depWatch) stopDeleteWorker() {
	select {
	case <-w.stopCh:
		// already stopped
	default:
		close(w.stopCh)
	}
	w.workerWG.Wait()
}

// depEventHandlers builds the shared informer event-handler set wired by
// BOTH addResourceTypeLocked (dynamic full-Unstructured) and
// addResourceTypeMetadataOnlyLocked (PartialObjectMetadata). The handler
// bodies are byte-identical across the two paths — metaNSName extracts
// (ns, name) via the nsNameAccessor interface that both shapes satisfy.
//
// Ship H5 — *bytesObject-SAFE BY CONSTRUCTION. Post the H5 routing
// inversion the streaming informers deliver *bytesObject to these
// handlers. That is safe WITHOUT change here: every handler reads ONLY
// (namespace, name) via metaNSName, and *bytesObject embeds ObjectMeta
// so it satisfies metaNSName's nsNameAccessor interface exactly as
// *unstructured.Unstructured and *PartialObjectMetadata do. The
// dep-tracker never reads object CONTENT — so it needs no decode-on-
// access. WARNING for a future editor: a content-assert added here
// (obj.(*unstructured.Unstructured), reading spec/status, etc.) would
// NOT be *bytesObject-safe and would silently drop every streamed
// object — it must decode via decodeBytesObject first.
//
// R1 — AddFunc IS wired here (pre-0.30.110 it was deliberately omitted).
// The initial-replay gate consults rw.syncCh[gvr]:
//
//   - syncCh closed (informer post-sync) → propagate the ADD to
//     Deps().OnAdd (dirty-mark LIST-scope dependents of the new object).
//   - syncCh open (initial LIST replay in flight) → DROP. Every existing
//     object arrives as an ADD during the initial replay; dirty-marking
//     each one is pointless churn and would storm the refresher at
//     startup. addDroppedPreSync counts the drops.
//   - syncCh nil (O14) → fail-safe DROP + one-shot WARN. A nil channel
//     means a registration path bypassed the syncCh allocation; we never
//     propagate (could storm) and never block (could deadlock).
func (rw *ResourceWatcher) depEventHandlers(gvr schema.GroupVersionResource) clientcache.ResourceEventHandlerFuncs {
	w := depWatchSingleton()
	return clientcache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if !rw.addEventPostSync(gvr, w) {
				w.counters.addDroppedPreSync.Add(1)
				return
			}
			ns, name := metaNSName(obj)
			w.counters.addPropagated.Add(1)
			Deps().OnAdd(gvr, ns, name)
		},
		UpdateFunc: func(_, newObj interface{}) {
			ns, name := metaNSName(newObj)
			Deps().OnUpdate(gvr, ns, name)
		},
		DeleteFunc: func(obj interface{}) {
			// DeletedFinalStateUnknown wraps the last-known object when
			// the watcher missed the explicit DELETE. Unwrap so we still
			// get the (ns, name) tuple.
			if tomb, ok := obj.(clientcache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			ns, name := metaNSName(obj)
			// R3: hand off to the worker — never run the eviction burst
			// inline on this informer processor goroutine.
			w.submitDeleteEvent(depDeleteEvent{gvr: gvr, namespace: ns, name: name})
		},
	}
}

// addEventPostSync implements the R1 initial-replay gate + the O14 nil-
// syncCh fail-safe. Returns true iff an ADD for gvr should propagate to
// the DepTracker.
//
// The syncCh read is the lock-free
//
//	select { case <-syncCh: <closed→post-sync> default: <open→pre-sync> }
//
// idiom — a closed channel always selects, an open one never does.
func (rw *ResourceWatcher) addEventPostSync(gvr schema.GroupVersionResource, w *depWatch) bool {
	rw.mu.RLock()
	ch := rw.syncCh[gvr]
	rw.mu.RUnlock()

	if ch == nil {
		// O14: nil syncCh — a registration path skipped the allocation.
		// Fail safe: drop (never propagate, never block). One-shot WARN.
		w.counters.addNilSyncCh.Add(1)
		if w.nilSyncWarned.CompareAndSwap(false, true) {
			slog.Warn("deps.add_nil_syncch",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
				slog.String("hint", "AddFunc fired with a nil syncCh — treating ADD as pre-sync (drop). "+
					"A registration path bypassed the syncCh allocation; dep dirty-marks for this GVR are degraded to TTL."),
			)
		}
		return false
	}

	select {
	case <-ch:
		return true // closed → informer post-sync → propagate
	default:
		return false // open → initial LIST replay → drop
	}
}

// DepWatchStats is a read-only snapshot of the Ship A informer-bridge
// counters. Consumed by the resolved_cache.summary log + the AC tests.
type DepWatchStats struct {
	AddDroppedPreSync uint64
	AddPropagated     uint64
	AddNilSyncCh      uint64
	DeleteQueueFull   uint64
}

// DepWatchStatsSnapshot returns the current bridge counters.
func DepWatchStatsSnapshot() DepWatchStats {
	w := depWatchSingleton()
	return DepWatchStats{
		AddDroppedPreSync: w.counters.addDroppedPreSync.Load(),
		AddPropagated:     w.counters.addPropagated.Load(),
		AddNilSyncCh:      w.counters.addNilSyncCh.Load(),
		DeleteQueueFull:   w.counters.deleteQueueFull.Load(),
	}
}

// resetDepWatchForTest tears the bridge singleton down so each test sees
// a clean bridge (counters zeroed, worker stopped). Exported via the
// _test.go shim; production code MUST NOT call it.
func resetDepWatchForTest() {
	if depWatchInstance != nil {
		depWatchInstance.stopDeleteWorker()
	}
	depWatchInstance = nil
	depWatchOnce = sync.Once{}
}
