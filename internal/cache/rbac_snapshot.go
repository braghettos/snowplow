// rbac_snapshot.go — Ship B (resolver-path rebuild, 0.30.138).
//
// The four RBAC GVRs (rbac.authorization.k8s.io/v1
// ClusterRoleBindings / RoleBindings / ClusterRoles / Roles) are
// read by EvaluateRBAC on the hot resolver path. Pre-Ship-B each
// `evaluateAgainstInformer` call materialised
//
//   - `cache.(*ResourceWatcher).ListTypedObjects`  — 614 MB / 60-call
//     (`make([]runtime.Object, 0, len(items))` + per-item append in
//     watcher.go:1826-1834)
//   - `client-go/tools/cache.(*threadSafeMap).List` — 574 MB / 60-call
//     (a fresh `[]interface{}` of every indexer key, watcher.go:1816)
//
// — i.e. ~1.2 GB / 60 calls of pure slice rebuild on each /call's RBAC
// fan-out (verdict 0.30.136, design ship-b-typed-rbac-snapshot-design.md
// §0). Ship B replaces those per-call rebuilds with a single typed-RBAC
// snapshot kept up to date by the informer event handlers and atomically
// published. Readers in evaluate.go take **one** `rbacSnap.Load()` per
// EvaluateRBAC call and thread the resulting pointer through every
// sub-read so a single eval observes a coherent snapshot
// (AC-B.3 — correctness-load-bearing, see also §3 of the design).
//
// Ship B EXTENDS the 0.30.6 typed-RBAC indexer (commit d0b3baf) — it does
// NOT introduce a parallel mechanism. The typed transforms
// (`stripAndType*` in strip.go) and the informer routing stay exactly as
// they are; Ship B adds one writer (`scheduleRBACRebuild`) wired to the
// four RBAC informers' ADD/UPDATE/DELETE events and one reader-side
// `Snapshot()` getter. `ListTypedObjects` / `GetTypedObject` remain on
// the public API for non-RBAC callers — they are simply no longer
// reached by the RBAC hot path.
//
// Concurrency (design §3, AC-B.5):
//
//   - Snapshot is IMMUTABLE post-publish. The writer builds a brand-new
//     *RBACSnapshot (fresh maps, fresh slices) and Store()s it. No field
//     of a previously-published snapshot is ever mutated by any
//     goroutine. Readers iterate the maps/slices and read pointer
//     fields; they never write.
//   - `atomic.Pointer[RBACSnapshot]` provides the single
//     memory-ordered Store/Load — no torn read, no half-built snapshot
//     visible to a reader.
//   - One writer at a time via `rbacRebuildLock atomic.Bool` (tryLock),
//     same pattern as the L1 refresh's "Bounded async L1 refresh" lineage
//     (watcher.go:1028). A dirty bit (`rbacRebuildDirty`) absorbs bursts
//     so multiple events during an in-flight rebuild collapse into ONE
//     follow-up rebuild — k8s informer event handlers must NOT block.
//   - The pointed-to typed `*rbacv1.{...}` objects are owned by the
//     client-go indexer (its `List()` returns a slice of pointers; the
//     pointed-to objects are read-only by client-go contract). Ship B
//     follows the same rule — the writer puts pointers into the snapshot
//     and never mutates the pointed-to objects.
//
// Cache toggle (AC-B.7): the snapshot is constructed only in cache=on
// mode. cache=off (`Disabled() == true`) never registers the 4 RBAC
// informers with `stripAndType*` and EvaluateRBAC early-returns to
// UserCan/SubjectAccessReview at evaluate.go:124 unchanged.
package cache

import (
	"log/slog"
	"runtime/debug"
	"sync/atomic"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientcache "k8s.io/client-go/tools/cache"
)

// RBACSnapshot is an immutable, atomically-published view of the four
// RBAC GVRs — built by an informer event handler, read lock-free by
// `evaluateAgainstInformer` via `Snapshot()`.
//
// Immutable post-publish: none of these fields is ever mutated after
// `rbacSnap.Store(s)` completes. Readers MUST treat every field as
// read-only — including the pointed-to `*rbacv1.{...}` objects (the
// client-go indexer owns those; the snapshot just keeps pointers).
//
// Future co-evolution: if a new GVR is added to `rbacTypedGVRs`
// (strip.go:101-106) the snapshot's fields + `rebuildRBACSnapshot` +
// the migrated reader sites in internal/rbac/evaluate.go must all be
// extended in lockstep. AC-B.10's snapshot-miss canary will surface a
// half-done migration loudly.
type RBACSnapshot struct {
	// Cluster-wide ClusterRoleBindings — read at evaluate.go:198.
	ClusterRoleBindings []*rbacv1.ClusterRoleBinding

	// Namespace-keyed RoleBindings — read at evaluate.go:223. A
	// namespace absent from the map yields a nil slice; readers range
	// over it with zero iterations.
	RoleBindingsByNS map[string][]*rbacv1.RoleBinding

	// ClusterRoles indexed by name — read at evaluate.go:254 (one map
	// lookup per matched binding's `roleRef`).
	ClusterRolesByName map[string]*rbacv1.ClusterRole

	// Roles indexed by `ns + "/" + name` — read at evaluate.go:270.
	RolesByNSName map[string]*rbacv1.Role
}

// rbacSnap is the sole publish container — a single-writer / many-reader
// `atomic.Pointer[RBACSnapshot]`. Load() returns nil before the initial
// rebuild publishes; reader-side code MUST treat nil as "degrade to
// deny" (AC-B.8).
var rbacSnap atomic.Pointer[RBACSnapshot]

// rbacRebuildLock + rbacRebuildDirty: single-writer atomic.Bool tryLock
// with a dirty-flag re-rebuild. Multiple events during an in-flight
// rebuild collapse into ONE follow-up rebuild — k8s informer event
// handlers must NOT block. Same pattern as the L1 refresh
// (watcher.go:1028 lineage; "Bounded async L1 refresh" — Bug 7).
//
// Goroutine accounting: at most ONE in-flight rebuild goroutine at any
// moment (the tryLock acquirer); a queued rebuild rides as the
// next-loop iteration inside that same goroutine, NOT a second
// goroutine — so max concurrent rebuild goroutines is exactly 1
// regardless of event burst rate (AC-B.5 invariant).
var (
	rbacRebuildLock  atomic.Bool
	rbacRebuildDirty atomic.Bool
)

// rbacSnapshotMissCount counts every roleRef map-lookup miss in
// `roleRefPermits` (AC-B.10 — mirrors the 0.30.6 fallback=true
// invariant at evaluate.go:185-186). Production target: miss-count /
// EvaluateRBAC-count MUST stay below 1% over any 1-minute window. The
// steady-state >0% baseline is the rebuild-lag eventual-consistency
// window described in the design §4.2.
var rbacSnapshotMissCount atomic.Uint64

// RBACSnapshotMissCount returns the cumulative count of `roleRefPermits`
// snapshot misses since process start. Exported for the tester's
// AC-B.10 ratio probe; production code has no reason to read it.
func RBACSnapshotMissCount() uint64 {
	return rbacSnapshotMissCount.Load()
}

// RecordRBACSnapshotMiss is called by the RBAC `roleRefPermits` code
// when a roleRef name is not in the snapshot's CR/R map even though the
// binding still references it. The miss is WARN-logged and a counter
// incremented; the caller treats the miss as a deny (same fail-closed
// posture as today's `GetTypedObject !ok`).
//
// The expected steady-state miss rate is the bounded rebuild lag from
// §4.2 (a binding whose target CR/R was just deleted; the snapshot
// rebuild has not landed yet). >1% over any 1-minute window indicates a
// genuine half-done migration / registration bug — the tester polls the
// ratio for the AC-B.10 gate.
func RecordRBACSnapshotMiss(kind, namespace, name string) {
	rbacSnapshotMissCount.Add(1)
	// Snapshot-version sequence number — bumped on every publish —
	// lets ops correlate a miss-burst with a specific snapshot version
	// in the logs. Cheaper than a pointer-identity hash.
	snapSeq := rbacSnapshotPublishSeq.Load()
	slog.Warn("rbac.evaluate.snapshot.miss",
		slog.String("subsystem", "rbac"),
		slog.String("event", "snapshot.miss"),
		slog.String("kind", kind),
		slog.String("namespace", namespace),
		slog.String("name", name),
		slog.Uint64("snap_seq", snapSeq),
		slog.String("hint", "roleRef references a target absent from the snapshot — "+
			"either rebuild-lag (self-healing) or a genuine indexer/snapshot inconsistency"),
	)
}

// rbacSnapshotPublishSeq is bumped on every successful publish. Used
// only for log correlation (`snap_seq` field in
// `rbac.evaluate.snapshot.miss` WARN); not load-bearing for correctness.
var rbacSnapshotPublishSeq atomic.Uint64

// Snapshot returns the current published RBAC snapshot, or nil if no
// snapshot has been published yet (pre-readiness / cache=off). Readers
// MUST call this ONCE per EvaluateRBAC call and thread the returned
// pointer through every sub-read in that eval — see AC-B.3 in the
// design (single-snapshot-per-evaluation invariant).
//
// Lock-free: a single atomic load.
func (rw *ResourceWatcher) Snapshot() *RBACSnapshot {
	_ = rw // signature is per-watcher for future multi-watcher symmetry; today there is one global rw
	return rbacSnap.Load()
}

// RBACSnapshotForTest returns the package-level snapshot for tests in
// this package and in evaltest. Production code uses ResourceWatcher
// .Snapshot(); this getter exists so a test can assert publish
// observability without going through the watcher.
func RBACSnapshotForTest() *RBACSnapshot {
	return rbacSnap.Load()
}

// scheduleRBACRebuild flips the dirty bit; if no rebuild is in flight,
// spawns ONE goroutine that drains the bit by looping: rebuild → check
// dirty → if dirty rebuild again. Exits cleanly when dirty is false on
// the post-rebuild re-check.
//
// Bounded goroutines: at most one in-flight rebuild goroutine.
// `feedback_l1_invalidation_delete_only`-aligned pattern — same shape
// as the L1 refresh atomic.Bool tryLock (Bug 7 fix, watcher.go:1028).
//
// `feedback_no_role_scope_stalls`-aligned: handler bodies are
// non-blocking. The dirty flip + tryLock + maybe-spawn is a few atomics
// — handler returns immediately. The actual indexer walk runs on the
// detached rebuild goroutine.
func scheduleRBACRebuild(rw *ResourceWatcher) {
	// Mark dirty FIRST. If a rebuild is already in flight, it will see
	// this bit on its post-rebuild re-check and loop again.
	rbacRebuildDirty.Store(true)

	// Try to take the writer slot. CompareAndSwap from false→true
	// succeeds for exactly one goroutine; everyone else returns
	// immediately, leaving their dirty flip for the in-flight rebuild
	// to absorb.
	if !rbacRebuildLock.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer rbacRebuildLock.Store(false)
		// Panic-recovering: a rebuild that crashes must not poison the
		// writer slot for the rest of the process. Recover, log loudly,
		// and re-mark dirty so the NEXT event re-acquires and retries.
		defer func() {
			if r := recover(); r != nil {
				rbacRebuildDirty.Store(true)
				slog.Error("cache.rbac.snapshot.rebuild_panic",
					slog.String("subsystem", "cache"),
					slog.Any("recovered", r),
					slog.String("stack", string(debug.Stack())),
				)
			}
		}()

		// Loop until dirty is false on a fresh check AFTER a rebuild.
		// Each iteration clears dirty BEFORE rebuilding so events arriving
		// MID-rebuild re-flip it and force another iteration. (Clearing
		// AFTER would lose those events.)
		for {
			rbacRebuildDirty.Store(false)
			rebuildRBACSnapshot(rw)
			if !rbacRebuildDirty.Load() {
				return
			}
		}
	}()
}

// rebuildRBACSnapshot walks the 4 RBAC indexers once each and publishes
// a fresh `*RBACSnapshot`. The previous snapshot is replaced
// atomically; existing readers holding the old pointer continue using
// it safely (immutability §3.1 invariant 1).
//
// Cost (INFERRED, design §4.2 — recorded NOT as a hard timeout but as
// a comment for cross-ship verification): on the live cluster
// (N₁=31797 CRBs, N₂=63316 RBs, N₃=31806 CRs, N₄=63314 Rs) this walks
// ~190K pointer copies + ~95K map inserts ≈ 5–10 ms per rebuild on
// commodity hardware. AC-B.12 empirically gates the resulting
// end-to-end mutation propagation under 1 s (100× headroom).
//
// Failure modes:
//   - Indexer item is not a typed pointer (e.g. transform conversion
//     failed and the entry is still *unstructured.Unstructured): the
//     defensive `as{Kind}` fallback at evaluate.go:486-547 will catch it
//     downstream — Ship B's writer skips the entry and continues (a
//     loud WARN, mirroring the 0.30.6 fallback=true invariant).
//   - Indexer for a GVR not yet registered (cache=off race / shutdown):
//     skip the GVR; the missing field in the snapshot will produce
//     denies until the indexer is wired and the next event triggers a
//     rebuild.
func rebuildRBACSnapshot(rw *ResourceWatcher) {
	if rw == nil || rw.mode == modePassthrough {
		return
	}

	snap := &RBACSnapshot{
		RoleBindingsByNS:   map[string][]*rbacv1.RoleBinding{},
		ClusterRolesByName: map[string]*rbacv1.ClusterRole{},
		RolesByNSName:      map[string]*rbacv1.Role{},
	}

	// Reserve a sensible initial capacity for the CRB slice. The
	// indexer's List() does its own allocation; the snapshot's slice is
	// fresh.
	//
	// Defensive Unstructured fallback (0.30.6 contract preserved): when
	// the indexer entry is *unstructured.Unstructured rather than the
	// expected typed pointer (typed override disabled for a test, or
	// transform conversion previously failed and was logged WARN at
	// write time), the writer one-shot-converts via FromUnstructured
	// and includes the typed result in the snapshot. WARN-logged once
	// per kind+(ns,name) — the same loud signal the pre-Ship-B
	// as{Kind} helpers emitted at read time. Without this fallback the
	// 0.30.6 fallback equivalence test breaks: a CRB stored as
	// Unstructured would be invisible to EvaluateRBAC.
	if items := indexerList(rw, clusterRoleBindingsTypedGVR); items != nil {
		snap.ClusterRoleBindings = make([]*rbacv1.ClusterRoleBinding, 0, len(items))
		for _, it := range items {
			if crb, ok := it.(*rbacv1.ClusterRoleBinding); ok {
				snap.ClusterRoleBindings = append(snap.ClusterRoleBindings, crb)
				continue
			}
			if crb, ok := convertUnstructuredCRB(it); ok {
				snap.ClusterRoleBindings = append(snap.ClusterRoleBindings, crb)
				continue
			}
			rebuildSkipNonTyped("ClusterRoleBinding", it)
		}
	}

	if items := indexerList(rw, roleBindingsTypedGVR); items != nil {
		for _, it := range items {
			rb, ok := it.(*rbacv1.RoleBinding)
			if !ok {
				if conv, cok := convertUnstructuredRB(it); cok {
					rb = conv
				} else {
					rebuildSkipNonTyped("RoleBinding", it)
					continue
				}
			}
			ns := rb.Namespace
			snap.RoleBindingsByNS[ns] = append(snap.RoleBindingsByNS[ns], rb)
		}
	}

	if items := indexerList(rw, clusterRolesTypedGVR); items != nil {
		for _, it := range items {
			cr, ok := it.(*rbacv1.ClusterRole)
			if !ok {
				if conv, cok := convertUnstructuredCR(it); cok {
					cr = conv
				} else {
					rebuildSkipNonTyped("ClusterRole", it)
					continue
				}
			}
			snap.ClusterRolesByName[cr.Name] = cr
		}
	}

	if items := indexerList(rw, rolesTypedGVR); items != nil {
		for _, it := range items {
			r, ok := it.(*rbacv1.Role)
			if !ok {
				if conv, cok := convertUnstructuredR(it); cok {
					r = conv
				} else {
					rebuildSkipNonTyped("Role", it)
					continue
				}
			}
			key := r.Namespace + "/" + r.Name
			snap.RolesByNSName[key] = r
		}
	}

	rbacSnap.Store(snap)
	rbacSnapshotPublishSeq.Add(1)

	slog.Debug("cache.rbac.snapshot.published",
		slog.String("subsystem", "cache"),
		slog.Int("crbs", len(snap.ClusterRoleBindings)),
		slog.Int("rb_namespaces", len(snap.RoleBindingsByNS)),
		slog.Int("crs", len(snap.ClusterRolesByName)),
		slog.Int("rs", len(snap.RolesByNSName)),
	)
}

// indexerList returns the typed-RBAC indexer's List() for gvr, or nil
// when the GVR is not registered. The caller iterates and type-asserts
// to the expected `*rbacv1.{...}` pointer (typed transform guarantees
// this on the happy path; the defensive `rebuildSkipNonTyped` covers
// transform-conversion failure).
func indexerList(rw *ResourceWatcher, gvr schema.GroupVersionResource) []interface{} {
	rw.mu.RLock()
	gi, ok := rw.informers[gvr]
	rw.mu.RUnlock()
	if !ok {
		return nil
	}
	return gi.Informer().GetIndexer().List()
}

// fallbackUnstructuredFromIndexer decodes an indexer entry that is NOT
// the expected typed RBAC pointer into an *unstructured.Unstructured.
// Handles both *bytesObject (the H5 routing-inversion path, reached when
// the typed override has been disabled for a test) and a bare
// *unstructured.Unstructured (test-seeding path). Returns (nil, false)
// for any other shape — the caller logs `rebuildSkipNonTyped`.
func fallbackUnstructuredFromIndexer(obj interface{}) (*unstructured.Unstructured, bool) {
	return decodeBytesObject(obj)
}

// convertUnstructuredCRB attempts the defensive Unstructured→typed
// fallback (0.30.6 contract). Returns (typed, true) on success; logs
// WARN and returns (nil, false) on conversion failure. The WARN
// matches the pre-Ship-B as{Kind} WARN's "loud" semantics so a
// transform-conversion regression remains visible at write time.
//
// Accepts the raw indexer entry (which may be *unstructured.Unstructured
// OR *bytesObject when the typed override is absent and the H5 bytes
// path applied) — fallbackUnstructuredFromIndexer normalises both.
func convertUnstructuredCRB(obj interface{}) (*rbacv1.ClusterRoleBinding, bool) {
	uns, ok := fallbackUnstructuredFromIndexer(obj)
	if !ok || uns == nil {
		return nil, false
	}
	out := &rbacv1.ClusterRoleBinding{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(uns.Object, out); err != nil {
		slog.Warn("cache.rbac.snapshot.unstructured_convert_failed",
			slog.String("subsystem", "cache"),
			slog.String("kind", "ClusterRoleBinding"),
			slog.String("name", uns.GetName()),
			slog.String("error", err.Error()),
		)
		return nil, false
	}
	slog.Warn("cache.rbac.snapshot.unstructured_fallback",
		slog.String("subsystem", "cache"),
		slog.String("kind", "ClusterRoleBinding"),
		slog.String("name", uns.GetName()),
		slog.String("hint", "indexer entry was Unstructured/bytesObject — typed transform did not fire; "+
			"converted at snapshot-rebuild time (mirrors 0.30.6 fallback=true)"),
	)
	return out, true
}

func convertUnstructuredRB(obj interface{}) (*rbacv1.RoleBinding, bool) {
	uns, ok := fallbackUnstructuredFromIndexer(obj)
	if !ok || uns == nil {
		return nil, false
	}
	out := &rbacv1.RoleBinding{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(uns.Object, out); err != nil {
		slog.Warn("cache.rbac.snapshot.unstructured_convert_failed",
			slog.String("subsystem", "cache"),
			slog.String("kind", "RoleBinding"),
			slog.String("name", uns.GetName()),
			slog.String("namespace", uns.GetNamespace()),
			slog.String("error", err.Error()),
		)
		return nil, false
	}
	slog.Warn("cache.rbac.snapshot.unstructured_fallback",
		slog.String("subsystem", "cache"),
		slog.String("kind", "RoleBinding"),
		slog.String("name", uns.GetName()),
		slog.String("namespace", uns.GetNamespace()),
		slog.String("hint", "indexer entry was Unstructured/bytesObject — typed transform did not fire; "+
			"converted at snapshot-rebuild time"),
	)
	return out, true
}

func convertUnstructuredCR(obj interface{}) (*rbacv1.ClusterRole, bool) {
	uns, ok := fallbackUnstructuredFromIndexer(obj)
	if !ok || uns == nil {
		return nil, false
	}
	out := &rbacv1.ClusterRole{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(uns.Object, out); err != nil {
		slog.Warn("cache.rbac.snapshot.unstructured_convert_failed",
			slog.String("subsystem", "cache"),
			slog.String("kind", "ClusterRole"),
			slog.String("name", uns.GetName()),
			slog.String("error", err.Error()),
		)
		return nil, false
	}
	slog.Warn("cache.rbac.snapshot.unstructured_fallback",
		slog.String("subsystem", "cache"),
		slog.String("kind", "ClusterRole"),
		slog.String("name", uns.GetName()),
		slog.String("hint", "indexer entry was Unstructured/bytesObject — typed transform did not fire; "+
			"converted at snapshot-rebuild time"),
	)
	return out, true
}

func convertUnstructuredR(obj interface{}) (*rbacv1.Role, bool) {
	uns, ok := fallbackUnstructuredFromIndexer(obj)
	if !ok || uns == nil {
		return nil, false
	}
	out := &rbacv1.Role{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(uns.Object, out); err != nil {
		slog.Warn("cache.rbac.snapshot.unstructured_convert_failed",
			slog.String("subsystem", "cache"),
			slog.String("kind", "Role"),
			slog.String("name", uns.GetName()),
			slog.String("namespace", uns.GetNamespace()),
			slog.String("error", err.Error()),
		)
		return nil, false
	}
	slog.Warn("cache.rbac.snapshot.unstructured_fallback",
		slog.String("subsystem", "cache"),
		slog.String("kind", "Role"),
		slog.String("name", uns.GetName()),
		slog.String("namespace", uns.GetNamespace()),
		slog.String("hint", "indexer entry was Unstructured/bytesObject — typed transform did not fire; "+
			"converted at snapshot-rebuild time"),
	)
	return out, true
}

// rebuildSkipNonTyped logs a WARN when the writer encounters an indexer
// entry that is neither a typed pointer NOR an *unstructured.Unstructured
// it can fall back to (e.g. *bytesObject — bytes routing is excepted
// for RBAC GVRs, but defense-in-depth). Mirrors the 0.30.6
// `fallback=true` invariant. The writer skips the entry; the downstream
// path cannot serve it.
func rebuildSkipNonTyped(kind string, obj interface{}) {
	name, namespace := "", ""
	if uns, ok := obj.(*unstructured.Unstructured); ok {
		name = uns.GetName()
		namespace = uns.GetNamespace()
	}
	slog.Warn("cache.rbac.snapshot.skip_non_typed",
		slog.String("subsystem", "cache"),
		slog.String("kind", kind),
		slog.String("name", name),
		slog.String("namespace", namespace),
		slog.String("got_type", goTypeOf(obj)),
		slog.String("hint", "indexer entry was not a typed *rbacv1.* pointer — "+
			"typed transform did not fire on this object (mirrors 0.30.6 fallback=true)"),
	)
}

// goTypeOf is the no-reflect equivalent of fmt.Sprintf("%T", obj) for a
// short class of expected types — used by the rebuild's WARN log. Falls
// back to "<unknown>" rather than reflect because the WARN runs on a
// processor goroutine and we keep it allocation-cheap.
func goTypeOf(obj interface{}) string {
	switch obj.(type) {
	case *rbacv1.ClusterRoleBinding:
		return "*rbacv1.ClusterRoleBinding"
	case *rbacv1.RoleBinding:
		return "*rbacv1.RoleBinding"
	case *rbacv1.ClusterRole:
		return "*rbacv1.ClusterRole"
	case *rbacv1.Role:
		return "*rbacv1.Role"
	case *unstructured.Unstructured:
		return "*unstructured.Unstructured"
	case nil:
		return "<nil>"
	default:
		return "<other>"
	}
}

// well-known typed RBAC GVRs — must match the writer-side population in
// strip.go's `rbacTypedGVRs` (strip.go:101-106). These are private
// duplicates because internal/rbac's own GVR vars are in a different
// package; Ship B's writer lives in cache. If `rbacTypedGVRs` ever
// grows, these vars + RBACSnapshot's fields + the rbac/evaluate.go
// reader sites must all be extended in lockstep (see AC-B.10).
//
// `feedback_no_special_cases`-compliant: these are the same GVRs the
// typed-RBAC indexer already discriminates on (strip.go:101-106); Ship B
// reads from the same set, not a new list.
var (
	clusterRoleBindingsTypedGVR = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	roleBindingsTypedGVR        = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
	clusterRolesTypedGVR        = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	rolesTypedGVR               = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
)

// isTypedRBACGVR reports whether gvr is one of the four RBAC GVRs the
// snapshot tracks. The watcher's addResourceTypeLocked uses this to
// decide whether to attach the snapshot-rebuild event handler.
//
// Driven by the existing `rbacTypedGVRs` set in strip.go — Ship B does
// NOT introduce a new GVR list. Per `feedback_no_special_cases` the
// participating GVRs come from the same source of truth as the typed
// transforms.
func isTypedRBACGVR(gvr schema.GroupVersionResource) bool {
	for _, g := range rbacTypedGVRs {
		if g == gvr {
			return true
		}
	}
	return false
}

// rbacSnapshotEventHandlers builds the informer event-handler set that
// schedules a snapshot rebuild on ADD/UPDATE/DELETE. Wired by
// addResourceTypeLocked for each of the 4 typed-RBAC GVRs alongside the
// existing `depEventHandlers` (deps_watch.go).
//
// All three callbacks call scheduleRBACRebuild(rw); they do not need
// to read the event object's content. The new indexer state is read
// inside the detached rebuild goroutine — the handler bodies stay
// O(1) atomics and never block the informer's processor goroutine.
func (rw *ResourceWatcher) rbacSnapshotEventHandlers() clientcache.ResourceEventHandlerFuncs {
	return clientcache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ interface{}) { scheduleRBACRebuild(rw) },
		UpdateFunc: func(_, _ interface{}) { scheduleRBACRebuild(rw) },
		DeleteFunc: func(_ interface{}) { scheduleRBACRebuild(rw) },
	}
}

// rbacSnapshotWired records whether the watcher has at least one of
// the typed-RBAC informers wired with the snapshot event handler. Set
// at NewResourceWatcher time; AssertRBACSnapshotWired panics at boot if
// it ever stays false despite the 4 RBAC informers being registered.
var rbacSnapshotHandlerWired atomic.Bool

// markRBACSnapshotWired flips the wired flag. Called by
// addResourceTypeLocked once per typed-RBAC GVR after attaching the
// snapshot event handler.
func markRBACSnapshotWired() {
	rbacSnapshotHandlerWired.Store(true)
}

// rbacSnapshotAssertionDisabled, when true, makes AssertRBACSnapshotWired
// a no-op. Set only by `DisableRBACSnapshotForTest` so a test that
// deliberately exercises the no-snapshot pre-readiness path doesn't
// trip the startup assertion.
var rbacSnapshotAssertionDisabled atomic.Bool

// AssertRBACSnapshotWired panics if the 4 RBAC informers are registered
// (typed overrides registered → addResourceTypeLocked must have been
// called for each) but the snapshot event handler was never attached.
// Analogous to AssertRBACTypedOverridesRegistered (strip.go:173); makes
// a snapshot-wiring regression loud at boot, not silent at request time
// (design AC-B.8).
//
// Called by NewResourceWatcher after the constructor's eager RBAC
// registration loop runs and the initial snapshot has been published.
// Tests that need to exercise the no-snapshot path (degrade-to-deny)
// call DisableRBACSnapshotForTest first.
func AssertRBACSnapshotWired() {
	if rbacSnapshotAssertionDisabled.Load() {
		return
	}
	if !rbacSnapshotHandlerWired.Load() {
		panic("cache: RBAC snapshot event handler not wired — " +
			"addResourceTypeLocked must call markRBACSnapshotWired() for every typed-RBAC GVR " +
			"(regression: Ship B writer wiring missing)")
	}
}

// DisableRBACSnapshotForTest disables the snapshot-wired startup
// assertion AND clears any already-published snapshot. Returns a
// restore function that re-enables the assertion (test responsibility:
// invoke via t.Cleanup). The cleared snapshot is NOT automatically
// restored — tests that need the previous snapshot must capture and
// re-publish it explicitly.
//
// Production callers MUST NOT call this. WARN-logged on every
// invocation so accidental use in non-test code is loud.
func DisableRBACSnapshotForTest() func() {
	slog.Warn("cache.DisableRBACSnapshotForTest invoked — production code MUST NOT call this",
		slog.String("subsystem", "cache"),
	)
	prev := rbacSnapshotAssertionDisabled.Swap(true)
	saved := rbacSnap.Load()
	rbacSnap.Store(nil)
	return func() {
		rbacSnapshotAssertionDisabled.Store(prev)
		// Do NOT auto-restore `saved` — tests that want it re-published
		// must do so explicitly via the public Snapshot() observers, so
		// we don't surprise them with a stale snapshot.
		_ = saved
	}
}

// PublishRBACSnapshotForTest installs `s` as the current snapshot. Used
// only by tests that build snapshots manually (e.g. the
// TestRBACSnapshot_Equivalence + TestRBACSnapshot_DegradeToDeny suites).
// Production code MUST NOT call this — production publishes go through
// `scheduleRBACRebuild` → `rebuildRBACSnapshot` only.
func PublishRBACSnapshotForTest(s *RBACSnapshot) {
	rbacSnap.Store(s)
}

// RebuildRBACSnapshotForTest publicly exposes a synchronous snapshot
// rebuild for tests. Production code uses `scheduleRBACRebuild` (which
// is asynchronous, bounded, and dirty-flag-coalesced) — never this.
func RebuildRBACSnapshotForTest(rw *ResourceWatcher) {
	rebuildRBACSnapshot(rw)
}

// waitAndPublishInitialRBACSnapshot is the initial-publish goroutine
// spawned by NewResourceWatcher (Ship B / AC-B.9). It blocks until all
// 4 RBAC syncCh channels close — the "Servable" signal that the
// informer's initial LIST has reconciled — then runs
// rebuildRBACSnapshot synchronously to publish the first snapshot.
//
// Before this goroutine completes, rbacSnap.Load() returns nil and
// EvaluateRBAC's AC-B.8 degrade-to-deny fires for every request. After
// the publish, every subsequent ADD/UPDATE/DELETE flows through the
// event handlers → scheduleRBACRebuild for incremental updates.
//
// Exits early (no publish) if rw.stopCh closes mid-wait — process
// shutdown.
func waitAndPublishInitialRBACSnapshot(rw *ResourceWatcher) {
	// Snapshot the 4 RBAC sync channels under the watcher lock. The
	// channels are allocated by addResourceTypeLocked and never
	// re-allocated for the same GVR, so capturing the pointers here is
	// safe.
	rw.mu.RLock()
	channels := make([]chan struct{}, 0, len(rbacTypedGVRs))
	for _, gvr := range rbacTypedGVRs {
		if ch, ok := rw.syncCh[gvr]; ok && ch != nil {
			channels = append(channels, ch)
		}
	}
	rw.mu.RUnlock()

	if len(channels) == 0 {
		slog.Warn("cache.rbac.snapshot.initial_publish_skipped",
			slog.String("subsystem", "cache"),
			slog.String("hint", "no RBAC syncCh channels at NewResourceWatcher end — "+
				"snapshot will publish on the first ADD event instead (degrade-to-deny "+
				"covers the gap)"),
		)
		return
	}

	for _, ch := range channels {
		select {
		case <-ch:
			// closed → informer synced
		case <-rw.stopCh:
			slog.Info("cache.rbac.snapshot.initial_publish_aborted",
				slog.String("subsystem", "cache"),
				slog.String("hint", "stopCh closed before initial RBAC sync — process shutdown"),
			)
			return
		}
	}

	// All 4 informers are synced — publish the initial snapshot
	// synchronously so the very next EvaluateRBAC call sees it.
	rebuildRBACSnapshot(rw)
	slog.Info("cache.rbac.snapshot.initial_publish_done",
		slog.String("subsystem", "cache"),
		slog.String("hint", "first typed-RBAC snapshot is live; "+
			"subsequent updates ride scheduleRBACRebuild"),
	)
}
