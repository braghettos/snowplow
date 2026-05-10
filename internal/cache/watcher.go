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

// ResourceWatcher is the cluster-wide informer cache. At 0.30.1 it is
// constructed but NOT started by any consumer (the factory is only
// instantiated when CACHE_ENABLED is true). Routing flips on at 0.30.2.
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

	stopCh chan struct{}
}

// NewResourceWatcher constructs a cluster-wide informer factory bound
// to dyn. When Disabled() is true the function returns (nil, nil) and
// NEVER instantiates the factory — guaranteeing zero goroutines and
// zero apiserver traffic in cache=off mode.
//
// Callers MUST nil-check the return value: when nil, every consumer
// takes the apiserver branch.
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
	_ = ctx // reserved for future wiring (0.30.2 caller may pass-through)

	// We never call factory.Start() here — that's deliberately deferred
	// to 0.30.2 when consumer routing flips on. At 0.30.1 the factory
	// stays passive (no informers registered, no goroutines spawned by
	// the constructor — Stop() is the only shutdown path).

	slog.Info("cache.plumbing_present=true cache.routed=false",
		slog.String("subsystem", "cache"),
		slog.Int64("list_page_limit", listPageLimit),
	)

	return rw, nil
}

// AddResourceType registers an informer for gvr. Idempotent: calling
// twice for the same GVR is a no-op. Safe for concurrent use.
//
// At 0.30.1 no consumer calls this method — it lands wired but unused.
// Coverage for the 0.30.2 activation gate.
func (rw *ResourceWatcher) AddResourceType(gvr schema.GroupVersionResource) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if _, exists := rw.informers[gvr]; exists {
		return
	}

	gi := rw.factory.ForResource(gvr)
	rw.informers[gvr] = gi

	if rw.started {
		// Late registration after Start(): kick the new informer.
		go gi.Informer().Run(rw.stopCh)
	}
}

// Start launches every registered informer and begins serving from the
// in-memory cache. Idempotent. Must be called after every needed GVR
// has been added via AddResourceType.
//
// At 0.30.1 this is unused (consumers stay on apiserver branch). The
// dormancy unit test asserts Start() is NOT invoked from
// NewResourceWatcher itself.
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
