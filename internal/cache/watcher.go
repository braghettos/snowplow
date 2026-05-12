package cache

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	clientcache "k8s.io/client-go/tools/cache"
)

// listPageLimit is the chunk size used by every informer LIST call.
// Bounded paging keeps the apiserver from streaming an unbounded
// response — matches the policy from earlier ResourceWatcher iterations
// (Q-OOM-WARMER, ship/0.25.320).
const listPageLimit int64 = 500

// RBACResourceTypes is the eager-registered Role-Based Access Control
// resource-type set (0.30.4 binding, plan §"Tag 0.30.4 What's implemented"
// bullet 1). The four GVRs are eagerly informer-registered by
// NewResourceWatcher when CACHE_ENABLED=true so EvaluateRBAC can serve
// in-process Role-Based Access Control decisions without ever calling
// SubjectAccessReview against apiserver (Revision 1 binding).
//
// Per feedback_no_special_cases.md: NO per-resource policy lives in this
// set — it is the bare minimum required by EvaluateRBAC.
var RBACResourceTypes = []schema.GroupVersionResource{
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
}

// ResourceWatcher is the cluster-wide informer cache. At 0.30.4 the
// factory is instantiated AND started by NewResourceWatcher when
// CACHE_ENABLED=true; the four Role-Based Access Control GVRs are
// eagerly registered. At 0.30.6 the RestAction-derived inventory is
// also eager-registered post-construction via EagerRegisterAll.
//
// All methods are safe for concurrent use. AddResourceType registers an
// informer for a GVR; Start launches them; Get/List read from the
// in-memory store; Stop cancels the underlying context and waits for
// graceful shutdown.
//
// Per feedback_no_special_cases.md the type does not embed any
// per-resource policy — every consumer treats Disabled() uniformly.
type ResourceWatcher struct {
	dyn     dynamic.Interface
	factory dynamicinformer.DynamicSharedInformerFactory

	mu        sync.RWMutex
	informers map[schema.GroupVersionResource]informers.GenericInformer
	started   bool

	// eagerSet is the set of GVRs the caller passed to MarkEagerSet —
	// the post-startup expectation is that NO AddResourceType call
	// fires for a GVR in this set (because eager already registered
	// it). When one does, addResourceTypeLocked emits the WARN
	// "lazy-AddResourceType-unexpected" so the regression is visible.
	// nil eagerSet = "eager registration not yet completed" — no
	// WARNs fire (the constructor's own RBAC registrations are not
	// "lazy").
	eagerSet     map[schema.GroupVersionResource]struct{}
	eagerDone    bool

	stopCh chan struct{}
}

// NewResourceWatcher constructs a cluster-wide informer factory bound
// to dyn. When Disabled() is true the function returns (nil, nil) and
// NEVER instantiates the factory — guaranteeing zero goroutines and
// zero apiserver traffic in cache=off mode.
//
// Callers MUST nil-check the return value: when nil, every consumer
// takes the apiserver branch.
//
// At 0.30.4 (Revision 1 binding) cache=on mode eagerly registers the
// Role-Based Access Control GVRs (Role, RoleBinding, ClusterRole,
// ClusterRoleBinding) and starts the factory so EvaluateRBAC can serve
// in-process Role-Based Access Control decisions without ever calling
// SubjectAccessReview against apiserver.
func NewResourceWatcher(ctx context.Context, dyn dynamic.Interface) (*ResourceWatcher, error) {
	if Disabled() {
		slog.Info("cache.disabled=true",
			slog.String("subsystem", "cache"),
			slog.Bool("plumbing_present", true),
			slog.Bool("routed", false),
		)
		return nil, nil
	}

	if dyn == nil {
		return nil, fmt.Errorf("cache: NewResourceWatcher requires non-nil dynamic.Interface")
	}

	tweak := func(opts *metav1.ListOptions) {
		opts.Limit = listPageLimit
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dyn,
		0, // resync period 0 — pure event-driven; no periodic full re-list
		metav1.NamespaceAll,
		tweak,
	)

	rw := &ResourceWatcher{
		dyn:       dyn,
		factory:   factory,
		informers: map[schema.GroupVersionResource]informers.GenericInformer{},
		stopCh:    make(chan struct{}),
	}
	_ = ctx // reserved for future wiring (0.30.6 eager-registration caller may pass-through)

	// Revision 1 binding: register the four Role-Based Access Control
	// GVRs eagerly and start the factory. This is the single set of
	// types EvaluateRBAC reads from; without these we cannot meet the
	// "zero SubjectAccessReview in cache=on" rule.
	for _, gvr := range RBACResourceTypes {
		rw.addResourceTypeLocked(gvr)
	}

	// 0.30.5: install the SetTransform strip BEFORE factory.Start
	// (primer §4.7). The TransformFunc drops managedFields and the
	// last-applied-configuration annotation from every object before
	// it lands in the indexer. SetTransform returns an error only when
	// the informer has already started — at this point we have not
	// called Start yet, so the error path is unreachable. We log a
	// WARN if it ever happens to surface the regression rather than
	// failing the boot.
	for gvr, gi := range rw.informers {
		resourceType := gvrResourceTypeString(gvr)
		tf := StripBulkyFieldsForResourceType(resourceType, gvr)
		if err := gi.Informer().SetTransform(tf); err != nil {
			slog.Warn("cache.strip.set_transform_failed",
				slog.String("subsystem", "cache"),
				slog.String("resource_type", resourceType),
				slog.String("error", err.Error()),
			)
		}
	}

	rw.factory.Start(rw.stopCh)
	rw.started = true

	slog.Info("cache.plumbing_present=true cache.routed=true rbac.informer_started=true",
		slog.String("subsystem", "cache"),
		slog.Int64("list_page_limit", listPageLimit),
		slog.Int("resource_types_registered", len(rw.informers)),
		slog.String("rbac.evaluate_path", "in-process"),
		slog.String("subject_access_review_calls_in_cache_on_path", "banned"),
	)

	return rw, nil
}

// AddResourceType registers an informer for gvr. Idempotent: calling
// twice for the same GVR is a no-op. Safe for concurrent use.
//
// At 0.30.4 the constructor eagerly registers the four Role-Based
// Access Control GVRs. Future tags will register additional resource
// types lazily on first dispatch (0.30.6 eager registration covers
// RestAction inventory).
func (rw *ResourceWatcher) AddResourceType(gvr schema.GroupVersionResource) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	rw.addResourceTypeLocked(gvr)
}

// addResourceTypeLocked is the lock-held implementation of
// AddResourceType. Callers MUST hold rw.mu.Lock().
//
// 0.30.5: every newly-added informer also has the SetTransform strip
// installed — including informers added lazily after Start(). For
// post-Start registration SetTransform returns an error (cannot mutate
// a running informer); we log it as a WARN so the regression is
// observable but do NOT fail the registration (the cache still works,
// just at higher memory cost for that GVR).
//
// 0.30.6: post-eager-registration calls into AddResourceType are
// expected to be rare — the inventory walker is supposed to cover the
// full RestAction-derived GVR set. When a lazy registration fires AND
// eager registration has completed AND the GVR was in the eager
// inventory set, we emit `lazy-AddResourceType-unexpected` so the
// regression is loud. Lazy registration for a GVR NOT in the eager
// inventory is normal (e.g. customer-added RestAction post-startup).
func (rw *ResourceWatcher) addResourceTypeLocked(gvr schema.GroupVersionResource) {
	if _, exists := rw.informers[gvr]; exists {
		return
	}

	gi := rw.factory.ForResource(gvr)
	rw.informers[gvr] = gi

	resourceType := gvrResourceTypeString(gvr)
	tf := StripBulkyFieldsForResourceType(resourceType, gvr)
	if err := gi.Informer().SetTransform(tf); err != nil {
		slog.Warn("cache.strip.set_transform_failed",
			slog.String("subsystem", "cache"),
			slog.String("resource_type", resourceType),
			slog.String("error", err.Error()),
			slog.Bool("post_start", rw.started),
		)
	}

	if rw.started {
		// Late registration after Start(): kick the new informer.
		go gi.Informer().Run(rw.stopCh)

		// 0.30.6 falsifier (plan §"Code-path falsifier"). If eager
		// registration has already completed AND this GVR was in
		// the eager set, the inventory walker missed it OR an
		// upstream caller is double-registering — either way the
		// SRE wants to see it.
		if rw.eagerDone {
			if _, wasInEager := rw.eagerSet[gvr]; wasInEager {
				slog.Warn("lazy-AddResourceType-unexpected",
					slog.String("subsystem", "cache"),
					slog.String("resource_type", resourceType),
					slog.String("hint", "was in eager inventory but registered lazily"),
				)
			} else {
				slog.Info("lazy-AddResourceType",
					slog.String("subsystem", "cache"),
					slog.String("resource_type", resourceType),
					slog.String("hint", "not in eager inventory — likely post-startup RestAction"),
				)
			}
		}
	}
}

// MarkEagerSet records the set of GVRs that were registered via the
// eager-registration pathway (Tag 0.30.6). After this call, lazy
// AddResourceType for any GVR in `eagerSet` emits the
// `lazy-AddResourceType-unexpected` WARN — the gap is a falsifier the
// PM gate verifies via `kubectl logs`.
//
// Calling MarkEagerSet with a nil slice is permitted (resets the
// eager-done flag back to false — used by tests).
//
// Safe for concurrent use.
func (rw *ResourceWatcher) MarkEagerSet(eagerSet []schema.GroupVersionResource) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if eagerSet == nil {
		rw.eagerSet = nil
		rw.eagerDone = false
		return
	}
	m := make(map[schema.GroupVersionResource]struct{}, len(eagerSet))
	for _, gvr := range eagerSet {
		m[gvr] = struct{}{}
	}
	rw.eagerSet = m
	rw.eagerDone = true
}

// gvrResourceTypeString renders gvr as "group/version/Resource" for the
// strip.applied falsifier log line. The core group renders as
// "core/v1/Resource" (rather than "/v1/Resource") so log readers don't
// need to special-case the empty-group case.
func gvrResourceTypeString(gvr schema.GroupVersionResource) string {
	group := gvr.Group
	if group == "" {
		group = "core"
	}
	return group + "/" + gvr.Version + "/" + gvr.Resource
}

// Start launches every registered informer and begins serving from the
// in-memory cache. Idempotent.
//
// At 0.30.4 NewResourceWatcher invokes Start() automatically after
// eager RBAC registration — callers normally do not need to call this
// directly. Future tags may use it for lazy GVR registration scenarios.
func (rw *ResourceWatcher) Start() {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.started {
		return
	}
	rw.started = true
	rw.factory.Start(rw.stopCh)
}

// WaitForCacheSync blocks until every registered informer's local
// store is in sync with apiserver, or the timeout elapses. Returns nil
// on success, error on timeout or context cancellation.
func (rw *ResourceWatcher) WaitForCacheSync(ctx context.Context, timeout time.Duration) error {
	rw.mu.RLock()
	syncs := make([]clientcache.InformerSynced, 0, len(rw.informers))
	for _, gi := range rw.informers {
		syncs = append(syncs, gi.Informer().HasSynced)
	}
	rw.mu.RUnlock()

	if len(syncs) == 0 {
		return nil
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if !clientcache.WaitForCacheSync(cctx.Done(), syncs...) {
		return fmt.Errorf("cache: sync timeout after %s", timeout)
	}
	return nil
}

// GetObject returns the cached unstructured object for (gvr, namespace,
// name) or (nil, false) when missing. Reads are served from the
// informer indexer in O(1).
func (rw *ResourceWatcher) GetObject(gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, bool) {
	rw.mu.RLock()
	gi, ok := rw.informers[gvr]
	rw.mu.RUnlock()

	if !ok {
		return nil, false
	}

	key := name
	if namespace != "" {
		key = namespace + "/" + name
	}

	obj, exists, err := gi.Informer().GetIndexer().GetByKey(key)
	if err != nil || !exists {
		return nil, false
	}

	uns, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, false
	}
	return uns, true
}

// ListObjects returns every cached unstructured object for gvr scoped
// to namespace. Pass empty string for cluster-wide listing.
func (rw *ResourceWatcher) ListObjects(gvr schema.GroupVersionResource, namespace string) []*unstructured.Unstructured {
	rw.mu.RLock()
	gi, ok := rw.informers[gvr]
	rw.mu.RUnlock()

	if !ok {
		return nil
	}

	store := gi.Informer().GetIndexer()
	var items []interface{}
	if namespace == "" {
		items = store.List()
	} else {
		idx, err := store.ByIndex(clientcache.NamespaceIndex, namespace)
		if err != nil {
			items = filterByNamespace(store.List(), namespace)
		} else {
			items = idx
		}
	}

	out := make([]*unstructured.Unstructured, 0, len(items))
	for _, it := range items {
		if uns, ok := it.(*unstructured.Unstructured); ok {
			out = append(out, uns)
		}
	}
	return out
}

func filterByNamespace(items []interface{}, ns string) []interface{} {
	out := make([]interface{}, 0, len(items))
	for _, it := range items {
		uns, ok := it.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if uns.GetNamespace() == ns {
			out = append(out, it)
		}
	}
	return out
}

// MatchingObjects returns cached objects in namespace whose labels
// match selector. Use this for label-selected reads instead of
// post-filtering ListObjects, when the indexer can short-circuit.
func (rw *ResourceWatcher) MatchingObjects(gvr schema.GroupVersionResource, namespace string, selector labels.Selector) []*unstructured.Unstructured {
	all := rw.ListObjects(gvr, namespace)
	if selector == nil || selector.Empty() {
		return all
	}
	out := make([]*unstructured.Unstructured, 0, len(all))
	for _, uns := range all {
		if selector.Matches(labels.Set(uns.GetLabels())) {
			out = append(out, uns)
		}
	}
	return out
}

// Stop signals every informer goroutine to exit. Idempotent.
func (rw *ResourceWatcher) Stop() {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	select {
	case <-rw.stopCh:
		// Already closed.
	default:
		close(rw.stopCh)
	}
}

// global holds the cluster-wide ResourceWatcher singleton wired in
// main.go. Cache=on consumers read it via Global(); a nil return is the
// canonical cache=off branch signal.
//
// We accept a package-level singleton here because:
//   - the watcher is genuinely process-scoped (one factory per pod);
//   - threading it through every resolver call site would touch ~30
//     unrelated files for no behavioural gain;
//   - the cache=off branch is encoded as nil — there is no other
//     "disabled" state to model.
//
// Per feedback_no_special_cases.md the singleton holds no per-resource
// or per-user policy: it is a pointer or it is nil.
var (
	globalMu      sync.RWMutex
	globalWatcher *ResourceWatcher
)

// SetGlobal wires rw as the process-scoped ResourceWatcher. Called once
// from main.go after NewResourceWatcher succeeds. Passing nil clears
// the singleton — used by tests and by the cache=off path.
func SetGlobal(rw *ResourceWatcher) {
	globalMu.Lock()
	globalWatcher = rw
	globalMu.Unlock()
}

// Global returns the process-scoped ResourceWatcher or nil when the
// cache subsystem is disabled / not yet wired. Cache=on consumers MUST
// nil-check the return value.
func Global() *ResourceWatcher {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return globalWatcher
}
