// crdwatch.go — 0.30.102 Tag B Part 2: CRD-watch for dynamic
// composition GVRs.
//
// Composition GVRs are produced by JQ-templated apiserver paths the
// RESTAction resolver dispatches against, e.g.
//
//   /apis/composition.krateo.io/${.spec.version}/namespaces/${.ns}/${.kind}
//
// The *group* component (`composition.krateo.io`) is STATIC — it is the
// second path segment and never carries a `${...}` template fragment.
// The version/namespace/resource segments ARE templated, so
// ParseAPIServerPathToGVR (which rejects any `${`) cannot derive the
// GVR from such a path. Phase 1's walk encounters these templated paths;
// ExtractAPIServerGroupFromTemplatedPath pulls the static group and
// feeds it into AddAutoDiscoverGroup.
//
// The CRD-watch then registers an informer for EVERY composition GVR
// whose CRD belongs to an auto-discovered group, AS the CRD appears (or
// at boot, for CRDs that already exist). matchesAutoDiscoverGroup is the
// single membership predicate — no per-resource switch.
//
// CRITICAL — feedback_no_special_cases.md: the auto-discover group set
// is NAVIGATION-DERIVED. It starts EMPTY. The only way a group enters it
// is Phase 1's resolution walk encountering a templated apiserver path
// in that group. `composition.krateo.io` is NOT a hardcoded constant
// anywhere in this file — it arrives purely from resolved RESTAction
// paths.

package cache

import (
	"context"
	"strings"
	"sync"

	"log/slog"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientcache "k8s.io/client-go/tools/cache"
)

// autoDiscoverGroups is the set of apiserver groups whose CRDs the
// CRD-watch auto-registers informers for. NAVIGATION-DERIVED — starts
// empty, populated only by AddAutoDiscoverGroup, which Phase 1's walk
// calls with the group it extracted from a resolved templated path.
//
// Guarded by autoDiscoverMu (its own mutex — not rw.mu — because the
// CRD informer's event handler goroutine and the Phase 1 walk goroutine
// both touch it, and neither holds rw.mu).
var (
	autoDiscoverMu     sync.RWMutex
	autoDiscoverGroups = map[string]struct{}{}
)

// AddAutoDiscoverGroup records group as a navigation-discovered
// composition group. Idempotent. Called by the Phase 1 walk for every
// static group it extracts from a templated apiserver path. A subsequent
// CRD-add (or the boot reconcile) for a CRD in this group triggers a
// per-GVR informer registration.
//
// The empty string is rejected — the core group ("") is never a
// composition group and admitting it would auto-register informers for
// every core resource on the cluster.
func AddAutoDiscoverGroup(group string) {
	if group == "" {
		return
	}
	autoDiscoverMu.Lock()
	_, existed := autoDiscoverGroups[group]
	autoDiscoverGroups[group] = struct{}{}
	autoDiscoverMu.Unlock()
	if !existed {
		slog.Info("cache.crdwatch.auto_discover_group_added",
			slog.String("subsystem", "cache"),
			slog.String("group", group),
			slog.String("note", "navigation-derived — extracted from a resolved templated apiserver path"),
		)
	}
}

// matchesAutoDiscoverGroup reports whether group is in the
// navigation-derived auto-discover set. The single membership predicate
// the CRD-watch consults — no per-resource carve-out.
func matchesAutoDiscoverGroup(group string) bool {
	autoDiscoverMu.RLock()
	defer autoDiscoverMu.RUnlock()
	_, ok := autoDiscoverGroups[group]
	return ok
}

// AutoDiscoverGroupsSnapshot returns a sorted-free copy of the current
// auto-discover group set. Test + observability helper.
func AutoDiscoverGroupsSnapshot() []string {
	autoDiscoverMu.RLock()
	defer autoDiscoverMu.RUnlock()
	out := make([]string, 0, len(autoDiscoverGroups))
	for g := range autoDiscoverGroups {
		out = append(out, g)
	}
	return out
}

// ResetAutoDiscoverGroupsForTest clears the auto-discover set. TEST-ONLY
// — the production lifecycle is append-only.
func ResetAutoDiscoverGroupsForTest() {
	autoDiscoverMu.Lock()
	autoDiscoverGroups = map[string]struct{}{}
	autoDiscoverMu.Unlock()
}

// ExtractAPIServerGroupFromTemplatedPath extracts the static apiserver
// GROUP from a (possibly JQ-templated) apiserver path. Unlike
// ParseAPIServerPathToGVR — which rejects any path containing `${` —
// this tolerates templated version/namespace/resource segments because
// the GROUP segment is always static:
//
//   /apis/<group>/<version>/...        -> <group>   (named group)
//   /apis/<group>/${.v}/namespaces/... -> <group>   (templated version OK)
//   /api/v1/...                        -> ""        (core group, ignored)
//
// Returns ("", false) for core-group paths, external endpoints, paths
// whose group segment itself is templated (`/apis/${...}/...`), or any
// non-apiserver shape. A templated GROUP segment is deliberately
// rejected — we cannot know the group statically, and admitting a
// `${...}` literal as a group would corrupt the auto-discover set.
func ExtractAPIServerGroupFromTemplatedPath(path string) (string, bool) {
	// Strip a leading `${ "..." }` wrapper if the whole path is one JQ
	// string expression — take the first quoted apiserver-looking
	// fragment. We only need the /apis/<group> prefix to survive.
	if i := strings.Index(path, "/apis/"); i >= 0 {
		rest := path[i+len("/apis/"):]
		// The group is everything up to the next '/'.
		slash := strings.IndexByte(rest, '/')
		if slash <= 0 {
			return "", false
		}
		group := rest[:slash]
		// Reject a templated group segment — cannot key the set on it.
		if group == "" || strings.Contains(group, "${") || strings.Contains(group, "\"") {
			return "", false
		}
		return group, true
	}
	return "", false
}

// StartCRDWatch registers a CRD informer (via the customresourcedefinitions
// meta-query seed) and wires an event handler that, on every CRD add /
// update, registers a per-GVR informer for the CRD's served version IFF
// the CRD's group is in the navigation-derived auto-discover set.
//
// This reuses EnsureResourceType for the per-GVR informer (the §0.30.93
// metadata-only routing applies — composition GVRs route to the
// PartialObjectMetadata informer when annotated / static-seeded). The
// new wiring is only: the group-membership gate + the CRD event handler.
//
// At boot, the CRD informer's initial LIST replays every existing CRD
// through AddFunc, so composition informers for already-present CRDs are
// registered as soon as their group is auto-discovered. Phase 1's final
// sync barrier (WaitAllInformersSynced) therefore includes the
// CRD-watch-spawned composition informers that exist at boot.
//
// Nil-receiver / passthrough are no-ops. Idempotent — guarded by
// crdWatchStarted so a duplicate call cannot double-register the handler.
//
// The CRD informer's run-loop and the per-GVR informers spawned by the
// handler are all bound by rw.stopCh (EnsureResourceType's late-register
// branch + the factory) so Stop() reaps them.
func (rw *ResourceWatcher) StartCRDWatch(ctx context.Context) {
	if rw == nil || rw.mode == modePassthrough {
		return
	}
	rw.mu.Lock()
	if rw.crdWatchStarted {
		rw.mu.Unlock()
		return
	}
	rw.crdWatchStarted = true
	rw.mu.Unlock()

	// Register the CRD informer through the standard path. EnsureResourceType
	// is idempotent — if RegisterMetaQuerySeeds already registered the CRD
	// GVR, this observes added=false and reuses the same informer.
	rw.EnsureResourceType(customResourceDefinitionGVR)

	rw.mu.RLock()
	gi, ok := rw.informers[customResourceDefinitionGVR]
	rw.mu.RUnlock()
	if !ok || gi == nil {
		slog.Warn("cache.crdwatch.no_crd_informer",
			slog.String("subsystem", "cache"),
			slog.String("hint", "EnsureResourceType did not register the CRD informer — CRD-watch inactive"),
		)
		return
	}

	if _, err := gi.Informer().AddEventHandler(clientcache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { rw.registerCRDObject(obj, "crd-event") },
		UpdateFunc: func(_, newObj interface{}) { rw.registerCRDObject(newObj, "crd-event") },
		// DeleteFunc intentionally omitted: a CRD deletion is rare and
		// the informer for its GVR going stale is harmless — the pivot's
		// servable() gate falls through on an unconfirmed resource type.
	}); err != nil {
		slog.Warn("cache.crdwatch.add_event_handler_failed",
			slog.String("subsystem", "cache"),
			slog.String("error", err.Error()),
		)
		return
	}

	slog.Info("cache.crdwatch.started",
		slog.String("subsystem", "cache"),
		slog.String("note", "CRD informer event handler installed — composition GVRs auto-register on CRD-add for navigation-discovered groups"),
	)
	_ = ctx // ctx reserved: the informer lifecycle is bound by rw.stopCh.
}

// registerCRDObject is the single per-CRD-object registration step:
// derive the CRD's served-version GVR and, IFF its group is in the
// navigation-derived auto-discover set, register a per-GVR informer for
// it. Idempotent — EnsureResourceType observes added=false for an
// already-registered GVR. Shared by the CRD informer's event handler
// (AddFunc/UpdateFunc) and the post-walk ReconcileAutoDiscoverCRDs scan.
//
// `via` is a log-only tag distinguishing the trigger (crd-event vs
// post-walk-reconcile).
func (rw *ResourceWatcher) registerCRDObject(obj interface{}, via string) {
	gvr, gvrOK := compositionGVRFromCRDObject(obj)
	if !gvrOK {
		return
	}
	if !matchesAutoDiscoverGroup(gvr.Group) {
		// CRD whose group is not navigation-discovered — ignored.
		return
	}
	added, _ := rw.EnsureResourceType(gvr)
	if added {
		slog.Info("cache.crdwatch.registered",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("via", via),
			slog.String("note", "composition informer spawned — group is navigation-discovered"),
		)
	}
}

// ReconcileAutoDiscoverCRDs re-scans the CRD informer's local store and
// re-applies registerCRDObject to every CRD currently present. It exists
// to close a boot ORDERING race:
//
//	StartCRDWatch installs the CRD informer's event handler, and the
//	informer's initial LIST replays every existing CRD through AddFunc
//	ONCE. The Phase 1 walk runs AFTER StartCRDWatch — so when the walk
//	discovers a composition group (AddAutoDiscoverGroup) the CRD informer
//	has very likely ALREADY replayed that group's CRD with
//	matchesAutoDiscoverGroup==false, dropping it permanently. AddFunc
//	never re-fires for a CRD that merely sat in etcd unchanged, so the
//	composition informer would never register.
//
// Calling this AFTER the Phase 1 walk has finished discovering all
// navigation groups deterministically closes the race: the
// auto-discover set is now complete, and a single store re-scan
// registers every composition informer whose CRD was replayed too
// early. EnsureResourceType is idempotent, so CRDs already registered by
// the live event handler are no-ops.
//
// Nil-receiver / passthrough / CRD-watch-not-started are no-ops.
func (rw *ResourceWatcher) ReconcileAutoDiscoverCRDs() int {
	if rw == nil || rw.mode == modePassthrough {
		return 0
	}
	rw.mu.RLock()
	gi, ok := rw.informers[customResourceDefinitionGVR]
	rw.mu.RUnlock()
	if !ok || gi == nil {
		return 0
	}

	before := rw.RegisteredCount()
	for _, obj := range gi.Informer().GetStore().List() {
		rw.registerCRDObject(obj, "post-walk-reconcile")
	}
	registered := rw.RegisteredCount() - before
	slog.Info("cache.crdwatch.reconcile_complete",
		slog.String("subsystem", "cache"),
		slog.Int("newly_registered", registered),
		slog.Any("auto_discover_groups", AutoDiscoverGroupsSnapshot()),
		slog.String("note", "post-walk CRD store re-scan — closes the boot replay-vs-discover ordering race"),
	)
	return registered
}

// compositionGVRFromCRDObject derives the GVR of a CRD's served (storage)
// version from a CustomResourceDefinition object. The object arrives
// from the dynamic informer as *unstructured.Unstructured (post-strip);
// it may also arrive typed if a future path converts it — both shapes
// are handled via runtime conversion.
//
// Returns ok=false when the object is not a CRD, has no served version,
// or is malformed. We pick the FIRST served version (the storage version
// when present); the CRD-watch only needs ONE informer per CRD and the
// served version is what the resolver's templated path targets.
func compositionGVRFromCRDObject(obj interface{}) (schema.GroupVersionResource, bool) {
	// Unwrap a tombstone if one slipped through.
	if tomb, ok := obj.(clientcache.DeletedFinalStateUnknown); ok {
		obj = tomb.Obj
	}

	var crd apiextensionsv1.CustomResourceDefinition
	switch o := obj.(type) {
	case *apiextensionsv1.CustomResourceDefinition:
		crd = *o
	default:
		// Unstructured (the dynamic informer's native shape) or any
		// runtime.Object — convert through the unstructured map.
		m, mapOK := toUnstructuredMap(obj)
		if !mapOK {
			return schema.GroupVersionResource{}, false
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &crd); err != nil {
			return schema.GroupVersionResource{}, false
		}
	}

	group := crd.Spec.Group
	resource := crd.Spec.Names.Plural
	if group == "" || resource == "" {
		return schema.GroupVersionResource{}, false
	}

	// Pick the served version: prefer the storage version, else the
	// first served one.
	version := ""
	for _, v := range crd.Spec.Versions {
		if !v.Served {
			continue
		}
		if v.Storage {
			version = v.Name
			break
		}
		if version == "" {
			version = v.Name
		}
	}
	if version == "" {
		return schema.GroupVersionResource{}, false
	}

	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}, true
}

// toUnstructuredMap pulls the map[string]any payload out of an
// unstructured-shaped object. Handles the *unstructured.Unstructured
// pointer (the informer's native type) and any runtime.Unstructured
// implementation.
func toUnstructuredMap(obj interface{}) (map[string]any, bool) {
	type unstructuredContent interface {
		UnstructuredContent() map[string]any
	}
	if u, ok := obj.(unstructuredContent); ok {
		return u.UnstructuredContent(), true
	}
	if m, ok := obj.(map[string]any); ok {
		return m, true
	}
	return nil, false
}
