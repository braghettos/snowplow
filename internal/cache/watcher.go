package cache

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
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

// ResourceWatcher maintains dynamic informers for every GVR in the watched set.
type ResourceWatcher struct {
	cache     *RedisCache
	dynClient k8sdynamic.Interface
	factory   dynamicinformer.DynamicSharedInformerFactory
	mu        sync.Mutex
	watched   map[string]bool
	appCtx    context.Context // long-lived process context; set by Start()

	l1Refresh  L1RefreshFunc
	refreshing sync.Map // gvrKey → bool; prevents concurrent refreshes for the same GVR
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
func (rw *ResourceWatcher) StartExpiryRefresh(ctx context.Context) {
	if err := rw.cache.EnableExpiryNotifications(ctx); err != nil {
		slog.Warn("resource-watcher: cannot enable Redis expiry notifications",
			slog.Any("err", err))
		return
	}
	ch := rw.cache.SubscribeExpired(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case key, ok := <-ch:
				if !ok {
					return
				}
				go rw.handleExpiredKey(ctx, key)
			}
		}
	}()
	slog.Info("resource-watcher: proactive expiry refresh enabled")
}

// SetL1Refresher registers a callback that will be used to proactively
// re-resolve L1 entries in the background instead of deleting them.
func (rw *ResourceWatcher) SetL1Refresher(fn L1RefreshFunc) {
	rw.l1Refresh = fn
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

func (rw *ResourceWatcher) handleEvent(ctx context.Context, gvr schema.GroupVersionResource, _, obj any, eventType string) {
	uns, ok := toUnstructured(obj)
	if !ok {
		return
	}
	ns, name := uns.GetNamespace(), uns.GetName()
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
		// Full L1 + per-resource L2 + GVR-wide L2 invalidation: the resource is gone.
		gvrKey := GVRToKey(gvr)
		l1Idx := L1GVRKey(gvrKey)
		if keys, serr := rw.cache.SMembers(ctx, l1Idx); serr == nil && len(keys) > 0 {
			_ = rw.cache.Delete(ctx, append(keys, l1Idx)...)
			slog.Debug("resource-watcher: L1 invalidation (delete)",
				slog.String("index", l1Idx), slog.Int("count", len(keys)))
		}
		// Delete per-resource L2 entries for this specific resource
		resIdx := L2ResourceKey(gvrKey, ns, name)
		if keys, serr := rw.cache.SMembers(ctx, resIdx); serr == nil && len(keys) > 0 {
			_ = rw.cache.Delete(ctx, append(keys, resIdx)...)
			slog.Debug("resource-watcher: L2 resource invalidation (delete)",
				slog.String("index", resIdx), slog.Int("count", len(keys)))
		}
		// Delete GVR-wide L2 entries (LIST caches — stored as HASH)
		l2Idx := L2GVRKey(gvrKey)
		if mapping, serr := rw.cache.HGetAll(ctx, l2Idx); serr == nil && len(mapping) > 0 {
			keysToDelete := make([]string, 0, len(mapping)+1)
			for l2Key := range mapping {
				keysToDelete = append(keysToDelete, l2Key)
			}
			keysToDelete = append(keysToDelete, l2Idx)
			_ = rw.cache.Delete(ctx, keysToDelete...)
			slog.Debug("resource-watcher: L2 GVR invalidation (delete)",
				slog.String("index", l2Idx), slog.Int("count", len(mapping)))
		}
		return

	case "add", "update":
		if serr := rw.cache.SetForGVR(ctx, gvr, getKey, uns); serr != nil {
			slog.Warn("resource-watcher: failed to update GET cache",
				slog.String("key", getKey), slog.Any("err", serr))
		}
		rw.patchListCache(ctx, gvr, nsListKey, uns, eventType)
		if ns != "" {
			rw.patchListCache(ctx, gvr, clusterListKey, uns, eventType)
		}
	}

	// ── Proactive L2 + L1 refresh (stale-while-revalidate) ───────────────────
	// Instead of deleting L2 keys (which causes cache misses during L1 refresh),
	// update them in-place from the fresh L3 data. L3 was already updated above
	// (SetForGVR + patchListCache), so the data is authoritative.
	gvrKey := GVRToKey(gvr)

	// L2 GET: refresh per-resource entries from the fresh L3 GET key.
	gr := gvr.GroupResource()
	resIdxKey := L2ResourceKey(gvrKey, ns, name)
	if resKeys, serr := rw.cache.SMembers(ctx, resIdxKey); serr == nil && len(resKeys) > 0 {
		l3GetKey := GetKey(gvr, ns, name)
		if fresh, hit, _ := rw.cache.GetRaw(ctx, l3GetKey); hit && !IsNotFoundRaw(fresh) {
			for _, l2Key := range resKeys {
				if u, ok := ParseHTTPUserKey(l2Key); ok {
					allowed, cached := rw.cache.IsRBACAllowed(ctx, u, "get", gr, ns)
					if cached && !allowed {
						_ = rw.cache.Delete(ctx, l2Key)
						continue
					}
				}
				_ = rw.cache.SetHTTPRaw(ctx, l2Key, fresh)
			}
			slog.Debug("resource-watcher: L2 resource refreshed",
				slog.String("resource", ns+"/"+name),
				slog.Int("count", len(resKeys)))
		} else {
			_ = rw.cache.Delete(ctx, append(resKeys, resIdxKey)...)
			slog.Debug("resource-watcher: L2 resource deleted (L3 miss)",
				slog.String("resource", ns+"/"+name))
		}
	}
	// L2 LIST: refresh each entry from its recorded L3 key (stored in HASH).
	l2IdxKey := L2GVRKey(gvrKey)
	if mapping, serr := rw.cache.HGetAll(ctx, l2IdxKey); serr == nil && len(mapping) > 0 {
		refreshed := 0
		for l2Key, l3Key := range mapping {
			if u, ok := ParseHTTPUserKey(l2Key); ok {
				listGVR, listNS, lok := ParseListKey(l3Key)
				if lok {
					allowed, cached := rw.cache.IsRBACAllowed(ctx, u, "list", listGVR.GroupResource(), listNS)
					if cached && !allowed {
						_ = rw.cache.Delete(ctx, l2Key)
						continue
					}
				}
			}
			if fresh, hit, _ := rw.cache.GetRaw(ctx, l3Key); hit && !IsNotFoundRaw(fresh) {
				_ = rw.cache.SetHTTPRaw(ctx, l2Key, fresh)
				refreshed++
			} else {
				_ = rw.cache.Delete(ctx, l2Key)
			}
		}
		slog.Debug("resource-watcher: L2 LIST refreshed",
			slog.String("gvr", gvr.String()),
			slog.Int("refreshed", refreshed),
			slog.Int("total", len(mapping)))
	}

	// L1: proactively refresh instead of deleting.
	l1IdxKey := L1GVRKey(gvrKey)
	l1Keys, serr := rw.cache.SMembers(ctx, l1IdxKey)
	if serr != nil || len(l1Keys) == 0 {
		return
	}

	if rw.l1Refresh != nil {
		// Guard: skip if a refresh for this GVR is already in-flight.
		if _, alreadyRunning := rw.refreshing.LoadOrStore(gvrKey, true); alreadyRunning {
			slog.Debug("resource-watcher: L1 refresh already in-flight, skipping",
				slog.String("gvr", gvr.String()))
			return
		}
		go func() {
			defer rw.refreshing.Delete(gvrKey)
			refreshCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			rw.l1Refresh(refreshCtx, gvr, l1Keys)
		}()
	} else {
		// No refresher configured; fall back to deletion.
		_ = rw.cache.Delete(ctx, append(l1Keys, l1IdxKey)...)
		slog.Debug("resource-watcher: L1 invalidated (no refresher)",
			slog.String("gvr", gvr.String()),
			slog.Int("count", len(l1Keys)))
	}
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
		GlobalMetrics.ExpiryRefreshes.Add(1)
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
		GlobalMetrics.ExpiryRefreshes.Add(1)
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

