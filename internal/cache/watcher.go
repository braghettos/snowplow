package cache

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sdynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	k8scache "k8s.io/client-go/tools/cache"
)

// L1RefreshFunc is invoked by the ResourceWatcher to proactively re-resolve
// L1 cache entries instead of deleting them. The function receives the GVR
// that triggered the refresh (for logging), the list of L1 keys to refresh,
// and a long-lived context. It must run synchronously; the caller invokes it
// in a goroutine.
type L1RefreshFunc func(ctx context.Context, triggerGVR schema.GroupVersionResource, l1Keys []string)

// expiryRefreshWorkers limits the number of concurrent goroutines handling
// Redis key expiry refresh to prevent unbounded goroutine creation under
// mass TTL expiry (e.g., after Redis restart or bulk warmup with identical TTLs).
const expiryRefreshWorkers = 10

// l1CoalescePeriod is the time to wait after the first event before triggering
// an L1 refresh. This allows burst events (e.g. 10 rapid DELETEs when a namespace
// is removed) to be coalesced into a single refresh cycle.
const l1CoalescePeriod = 3 * time.Second

// refreshQueue coalesces L1 refresh requests for a single GVR. Events arriving
// while a refresh is in-flight are queued for the next cycle — nothing is skipped.
type refreshQueue struct {
	mu        sync.Mutex
	pending   map[string]bool // deduplicated L1 keys waiting for next refresh
	timer     *time.Timer     // coalesce timer; nil when no events are pending
	running   bool            // true while a refresh goroutine is executing
	gvr       schema.GroupVersionResource
	gvrKey    string
	watcher   *ResourceWatcher
}

// enqueue adds L1 keys to the pending set and starts the coalesce timer if
// no refresh is currently in-flight. If a refresh IS running, the keys are
// queued and will be processed when the current refresh completes.
func (q *refreshQueue) enqueue(keys []string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, k := range keys {
		q.pending[k] = true
	}

	// If a refresh is running, the completion handler will pick up the
	// pending keys — no timer needed.
	if q.running {
		return
	}

	// Start or reset the coalesce timer.
	if q.timer == nil {
		q.timer = time.AfterFunc(l1CoalescePeriod, q.flush)
	}
	// Don't reset the timer — we want to fire l1CoalescePeriod after the
	// FIRST event, not the last. This prevents unbounded delays during
	// continuous streams (e.g. deploying 1200 compositions over 25 min).
}

// flush is called when the coalesce timer fires. It drains the pending set
// and starts a refresh goroutine.
func (q *refreshQueue) flush() {
	fn, ok := q.watcher.l1Refresh.Load().(L1RefreshFunc)
	if !ok || fn == nil {
		// No refresher registered — fall back to invalidation.
		q.mu.Lock()
		keys := q.drainLocked()
		q.mu.Unlock()
		if len(keys) > 0 {
			ctx := q.watcher.appCtx
			idxKey := L1GVRKey(q.gvrKey)
			_ = q.watcher.cache.Delete(ctx, append(keys, idxKey)...)
			slog.Debug("resource-watcher: L1 invalidated (no refresher, queued)",
				slog.String("gvr", q.gvr.String()), slog.Int("count", len(keys)))
		}
		return
	}

	q.mu.Lock()
	keys := q.drainLocked()
	if len(keys) == 0 {
		q.mu.Unlock()
		return
	}
	q.running = true
	q.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 4096)
				n := runtime.Stack(buf, false)
				slog.Error("resource-watcher: panic in queued L1 refresh",
					slog.Any("error", r),
					slog.String("gvr", q.gvr.String()),
					slog.String("stack", string(buf[:n])))
			}

			// Check if more events arrived while we were refreshing.
			q.mu.Lock()
			q.running = false
			hasPending := len(q.pending) > 0
			if hasPending {
				q.timer = time.AfterFunc(l1CoalescePeriod, q.flush)
			} else {
				q.timer = nil
			}
			q.mu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		fn(ctx, q.gvr, keys)
	}()
}

// drainLocked moves all pending keys out and returns them. Caller must hold q.mu.
func (q *refreshQueue) drainLocked() []string {
	if len(q.pending) == 0 {
		return nil
	}
	keys := make([]string, 0, len(q.pending))
	for k := range q.pending {
		keys = append(keys, k)
	}
	q.pending = make(map[string]bool)
	q.timer = nil
	return keys
}

// ResourceWatcher maintains dynamic informers for every GVR in the watched set.
type ResourceWatcher struct {
	cache     *RedisCache
	dynClient k8sdynamic.Interface
	factory   dynamicinformer.DynamicSharedInformerFactory
	mu        sync.Mutex
	watched   map[string]bool
	appCtx    context.Context // long-lived process context; set by Start()

	l1Refresh atomic.Value // stores L1RefreshFunc
	queues    sync.Map     // gvrKey → *refreshQueue
}

func NewResourceWatcher(c *RedisCache, rc *rest.Config) (*ResourceWatcher, error) {
	dynClient, err := k8sdynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return &ResourceWatcher{
		cache:     c,
		dynClient: dynClient,
		factory:   dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 0),
		watched:   make(map[string]bool),
	}, nil
}

func (rw *ResourceWatcher) Start(ctx context.Context) {
	rw.appCtx = ctx
	rw.syncNewGVRs(ctx)
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rw.syncNewGVRs(ctx)
			}
		}
	}()
}

// WaitForSync blocks until all started informers have completed their initial sync.
func (rw *ResourceWatcher) WaitForSync(ctx context.Context) bool {
	synced := rw.factory.WaitForCacheSync(ctx.Done())
	for _, ok := range synced {
		if !ok {
			return false
		}
	}
	return true
}

// StartExpiryRefresh subscribes to Redis expired-key events and proactively
// re-fetches resources so the cache never goes cold from TTL expiry alone.
// Uses a bounded worker pool to prevent goroutine storms under mass TTL expiry.
func (rw *ResourceWatcher) StartExpiryRefresh(ctx context.Context) {
	if err := rw.cache.EnableExpiryNotifications(ctx); err != nil {
		slog.Warn("resource-watcher: cannot enable Redis expiry notifications",
			slog.Any("err", err))
		return
	}
	ch := rw.cache.SubscribeExpired(ctx)
	sem := make(chan struct{}, expiryRefreshWorkers)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case key, ok := <-ch:
				if !ok {
					return
				}
				sem <- struct{}{}
				go func(k string) {
					defer func() { <-sem }()
					rw.handleExpiredKey(ctx, k)
				}(key)
			}
		}
	}()
	slog.Info("resource-watcher: proactive expiry refresh enabled",
		slog.Int("workers", expiryRefreshWorkers))
}

// SetL1Refresher registers a callback that will be used to proactively
// re-resolve L1 entries in the background instead of deleting them.
func (rw *ResourceWatcher) SetL1Refresher(fn L1RefreshFunc) {
	rw.l1Refresh.Store(fn)
}

func (rw *ResourceWatcher) AddGVR(_ context.Context, gvr schema.GroupVersionResource) {
	rw.startInformer(gvr)
}

func (rw *ResourceWatcher) syncNewGVRs(ctx context.Context) {
	members, err := rw.cache.SMembers(ctx, WatchedGVRsKey)
	if err != nil {
		slog.Warn("resource-watcher: failed to read watched GVR set", slog.Any("err", err))
		return
	}
	registered := false
	for _, key := range members {
		gvr := ParseGVRKey(key)
		if gvr.Resource == "" {
			continue
		}
		if rw.registerInformer(gvr) {
			registered = true
		}
	}
	if registered {
		rw.factory.Start(rw.appCtx.Done())
	}
}

// registerInformer creates the informer and adds event handlers without
// calling factory.Start. Returns true if a new informer was registered.
func (rw *ResourceWatcher) registerInformer(gvr schema.GroupVersionResource) bool {
	key := GVRToKey(gvr)
	rw.mu.Lock()
	if rw.watched[key] {
		rw.mu.Unlock()
		return false
	}
	rw.watched[key] = true
	rw.mu.Unlock()

	ctx := rw.appCtx
	informer := rw.factory.ForResource(gvr).Informer()
	if _, err := informer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { rw.handleEvent(ctx, gvr, nil, obj, "add") },
		UpdateFunc: func(old, obj any) { rw.handleEvent(ctx, gvr, old, obj, "update") },
		DeleteFunc: func(obj any) { rw.handleEvent(ctx, gvr, nil, obj, "delete") },
	}); err != nil {
		slog.Warn("resource-watcher: AddEventHandler failed",
			slog.String("gvr", gvr.String()), slog.Any("err", err))
		return false
	}
	slog.Info("resource-watcher: registered informer", slog.String("gvr", gvr.String()))
	return true
}

// startInformer registers the informer and immediately starts the factory.
// Used by AddGVR when a new GVR is discovered at request time.
func (rw *ResourceWatcher) startInformer(gvr schema.GroupVersionResource) {
	if rw.registerInformer(gvr) {
		rw.factory.Start(rw.appCtx.Done())
	}
}

// noisyConfigMapNamespaces are namespaces whose configmaps update very
// frequently (e.g. cluster-kubestore every 2s) but are never referenced
// by any widget. Skipping these avoids unnecessary L3 cache churn.
var noisyConfigMapNamespaces = map[string]bool{
	"kube-system": true,
	"gmp-system":  true,
}

func (rw *ResourceWatcher) handleEvent(ctx context.Context, gvr schema.GroupVersionResource, _, obj any, eventType string) {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			slog.Error("resource-watcher: panic recovered in handleEvent",
				slog.Any("error", r),
				slog.String("gvr", gvr.String()),
				slog.String("event", eventType),
				slog.String("stack", string(buf[:n])))
		}
	}()
	uns, ok := toUnstructured(obj)
	if !ok {
		return
	}
	ns, name := uns.GetNamespace(), uns.GetName()

	// Skip noisy configmap updates from system namespaces (e.g. cluster-kubestore
	// updates every 2s) that no widget depends on. These generate continuous
	// L3 cache churn with no benefit.
	if gvr.Resource == "configmaps" && noisyConfigMapNamespaces[ns] {
		return
	}

	slog.Debug("resource-watcher: event",
		slog.String("type", eventType),
		slog.String("gvr", gvr.String()),
		slog.String("ns", ns),
		slog.String("name", name))
	getKey := GetKey(gvr, ns, name)
	nsListKey := ListKey(gvr, ns)
	clusterListKey := ListKey(gvr, "")

	switch eventType {
	case "delete":
		_ = rw.cache.Delete(ctx, getKey)
		rw.patchListCache(ctx, gvr, nsListKey, uns, "delete")
		if ns != "" {
			rw.patchListCache(ctx, gvr, clusterListKey, uns, "delete")
		}
		// Invalidate L1 resolved outputs immediately so stale data
		// (containing the deleted resource) is not served.
		gvrKeyDel := GVRToKey(gvr)
		l1Idx := L1GVRKey(gvrKeyDel)
		l1KeysDel, serr := rw.cache.SMembers(ctx, l1Idx)
		if serr != nil || len(l1KeysDel) == 0 {
			return
		}
		_ = rw.cache.Delete(ctx, append(l1KeysDel, l1Idx)...)
		slog.Debug("resource-watcher: L1 invalidation (delete)",
			slog.String("index", l1Idx), slog.Int("count", len(l1KeysDel)))

		// Enqueue L1 refresh via the coalescing queue. Burst DELETEs
		// (e.g. 10 compositions in a namespace) are batched into a single
		// refresh cycle after the coalesce period.
		rw.enqueueL1Refresh(gvr, l1KeysDel)
		return

	case "add", "update":
		stripped := uns.DeepCopy()
		StripAnnotationsFromUnstructured(stripped)

		if serr := rw.cache.SetForGVR(ctx, gvr, getKey, stripped); serr != nil {
			slog.Warn("resource-watcher: failed to update GET cache",
				slog.String("key", getKey), slog.Any("err", serr))
		}
		rw.patchListCache(ctx, gvr, nsListKey, stripped, eventType)
		if ns != "" {
			rw.patchListCache(ctx, gvr, clusterListKey, stripped, eventType)
		}
	}

	// ── Proactive L1 refresh (stale-while-revalidate) ────────────────────────
	// Enqueue via the coalescing queue. Old L1 values keep being served while
	// the refresh runs. Burst events are coalesced — nothing is skipped.
	gvrKey := GVRToKey(gvr)
	l1IdxKey := L1GVRKey(gvrKey)
	l1Keys, serr := rw.cache.SMembers(ctx, l1IdxKey)
	if serr != nil || len(l1Keys) == 0 {
		return
	}
	rw.enqueueL1Refresh(gvr, l1Keys)
}

// enqueueL1Refresh adds L1 keys to the coalescing queue for the given GVR.
// The queue batches events and triggers a single refresh after the coalesce
// period, ensuring burst events (rapid ADD/UPDATE/DELETE) are handled efficiently.
func (rw *ResourceWatcher) enqueueL1Refresh(gvr schema.GroupVersionResource, l1Keys []string) {
	gvrKey := GVRToKey(gvr)
	raw, _ := rw.queues.LoadOrStore(gvrKey, &refreshQueue{
		pending: make(map[string]bool),
		gvr:     gvr,
		gvrKey:  gvrKey,
		watcher: rw,
	})
	q := raw.(*refreshQueue)
	q.enqueue(l1Keys)
}

// patchListCache atomically patches the cached list in-place using WATCH/MULTI/EXEC.
func (rw *ResourceWatcher) patchListCache(ctx context.Context, gvr schema.GroupVersionResource, listKey string, uns *unstructured.Unstructured, eventType string) {
	ttl := rw.cache.TTLForGVR(gvr)
	objNS, objName := uns.GetNamespace(), uns.GetName()

	err := rw.cache.AtomicUpdateJSON(ctx, listKey, func(raw []byte) ([]byte, error) {
		var list unstructured.UnstructuredList
		if raw != nil {
			if err := json.Unmarshal(raw, &list); err != nil {
				return nil, err
			}
		}
		if list.Object == nil {
			list.Object = map[string]interface{}{
				"kind":       uns.GetKind() + "List",
				"apiVersion": uns.GetAPIVersion(),
				"metadata":   map[string]interface{}{"resourceVersion": ""},
			}
		}
		idx := -1
		for i := range list.Items {
			if list.Items[i].GetNamespace() == objNS && list.Items[i].GetName() == objName {
				idx = i
				break
			}
		}
		switch eventType {
		case "add":
			if idx < 0 {
				list.Items = append(list.Items, *uns)
			} else {
				list.Items[idx] = *uns
			}
		case "update":
			if idx >= 0 {
				list.Items[idx] = *uns
			} else {
				list.Items = append(list.Items, *uns)
			}
		case "delete":
			if raw == nil {
				return nil, nil
			}
			if idx >= 0 {
				filtered := make([]unstructured.Unstructured, 0, len(list.Items)-1)
				for i := range list.Items {
					if i != idx {
						filtered = append(filtered, list.Items[i])
					}
				}
				list.Items = filtered
			}
		}
		return json.Marshal(&list)
	}, ttl)

	if err != nil {
		slog.Warn("resource-watcher: failed to patch list cache",
			slog.String("key", listKey), slog.Any("err", err))
	}
}

// ── Proactive expiry refresh ──────────────────────────────────────────────────

func (rw *ResourceWatcher) handleExpiredKey(ctx context.Context, key string) {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			slog.Error("resource-watcher: panic recovered in handleExpiredKey",
				slog.Any("error", r),
				slog.String("key", key),
				slog.String("stack", string(buf[:n])))
		}
	}()
	switch {
	case strings.HasPrefix(key, "snowplow:get:"):
		gvr, ns, name, ok := ParseGetKey(key)
		if ok && gvr.Resource != "" && name != "" {
			rw.refreshGetKey(ctx, gvr, ns, name)
		}
	case strings.HasPrefix(key, "snowplow:list:"):
		gvr, ns, ok := ParseListKey(key)
		if ok && gvr.Resource != "" {
			rw.refreshListKey(ctx, gvr, ns)
		}
	}
}

func (rw *ResourceWatcher) refreshGetKey(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) {
	var (
		obj *unstructured.Unstructured
		err error
	)
	if ns != "" {
		obj, err = rw.dynClient.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	} else {
		obj, err = rw.dynClient.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		if k8serrors.IsNotFound(err) {
			_ = rw.cache.SetNotFound(ctx, GetKey(gvr, ns, name))
		} else {
			slog.Warn("resource-watcher: failed to refresh expired GET key",
				slog.String("gvr", gvr.String()), slog.Any("err", err))
		}
		return
	}
	key := GetKey(gvr, ns, name)
	if serr := rw.cache.SetForGVR(ctx, gvr, key, obj); serr == nil {
		GlobalMetrics.Inc(&GlobalMetrics.ExpiryRefreshes, "expiry_refreshes")
	}
}

func (rw *ResourceWatcher) refreshListKey(ctx context.Context, gvr schema.GroupVersionResource, ns string) {
	var (
		list *unstructured.UnstructuredList
		err  error
	)
	if ns != "" {
		list, err = rw.dynClient.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
	} else {
		list, err = rw.dynClient.Resource(gvr).List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		slog.Warn("resource-watcher: failed to refresh expired LIST key",
			slog.String("gvr", gvr.String()), slog.Any("err", err))
		return
	}
	listKey := ListKey(gvr, ns)
	if serr := rw.cache.SetForGVR(ctx, gvr, listKey, list); serr == nil {
		for i := range list.Items {
			obj := &list.Items[i]
			_ = rw.cache.SetForGVR(ctx, gvr, GetKey(gvr, obj.GetNamespace(), obj.GetName()), obj)
		}
		GlobalMetrics.Inc(&GlobalMetrics.ExpiryRefreshes, "expiry_refreshes")
	}
}

func toUnstructured(obj any) (*unstructured.Unstructured, bool) {
	switch v := obj.(type) {
	case *unstructured.Unstructured:
		return v, true
	case k8scache.DeletedFinalStateUnknown:
		if u, ok := v.Obj.(*unstructured.Unstructured); ok {
			return u, true
		}
	}
	return nil, false
}

