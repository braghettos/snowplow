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

// l1Event represents an informer event that needs L1 refresh processing.
type l1Event struct {
	gvr             schema.GroupVersionResource
	gvrKey          string
	ns              string
	name            string
	eventType       string
	resourceVersion string // K8s resourceVersion of the object that triggered this event
}

// dirtyEntry tracks L3 generations for an L1 key. The ticker refreshes
// whenever ANY L3 gen has changed since the last refresh (progressive
// convergence during burst). After events stop, gens stabilize and the
// ticker skips (no redundant work).
type dirtyEntry struct {
	targetGens map[string]string // "gvrKey:ns" → latest resourceVersion seen
	lastSeen   map[string]string // "gvrKey:ns" → gen value at last refresh
}

// ResourceWatcher maintains dynamic informers for every GVR in the watched set.
type ResourceWatcher struct {
	cache     *RedisCache
	dynClient k8sdynamic.Interface
	factory   dynamicinformer.DynamicSharedInformerFactory
	mu        sync.Mutex
	watched   map[string]bool
	appCtx    context.Context // long-lived process context; set by Start()

	l1Refresh atomic.Value    // stores L1RefreshFunc
	eventCh   chan l1Event     // buffered channel for L1 refresh events
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
		eventCh:   make(chan l1Event, 10000),
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
	go rw.l1Worker(ctx)
}

// l1Worker processes L1 refresh events. All events (ADD/UPDATE/DELETE) mark
// affected L1 keys as dirty. A periodic 3s ticker re-resolves dirty keys,
// but only when L3 generation (resourceVersion) has changed since the last
// refresh — avoiding redundant work when L3 is stable.
//
// This gives:
// - Fast HTTP responses: L1 is never deleted, always serves from cache (~60ms)
// - Progressive convergence: dirty keys re-resolved every 3s during burst
// - Exact values: after events stop, L3 gens stabilize, final refresh is exact
// - No redundant work: ticker skips refresh when no L3 gen changed
func (rw *ResourceWatcher) l1Worker(ctx context.Context) {
	dirty := make(map[string]*dirtyEntry) // L1 key → target gens
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-rw.eventCh:
			if !ok {
				return
			}
			rw.processL1Event(ctx, evt, dirty)
		case <-ticker.C:
			if len(dirty) == 0 {
				continue
			}

			// For each dirty L1 key, check if ANY L3 gen changed since the
			// last refresh. This gives progressive convergence during burst
			// (refresh every 3s with latest data) and skips when stable.
			var ready []string
			for l1Key, entry := range dirty {
				anyChanged := false
				for gvrNS := range entry.targetGens {
					curRV, _ := rw.cache.client.Get(ctx, "snowplow:l3gen:"+gvrNS).Result()
					if curRV != entry.lastSeen[gvrNS] {
						anyChanged = true
						entry.lastSeen[gvrNS] = curRV
					}
				}
				if anyChanged {
					ready = append(ready, l1Key)
				}
			}
			// Remove dirty entries only when their gens haven't changed
			// (no more events flowing for this key).
			for l1Key, entry := range dirty {
				stale := false
				for gvrNS := range entry.targetGens {
					curRV, _ := rw.cache.client.Get(ctx, "snowplow:l3gen:"+gvrNS).Result()
					if curRV != entry.lastSeen[gvrNS] {
						stale = true
						break
					}
				}
				if !stale {
					// Check if this entry was just refreshed — if so, keep it
					// for one more tick to confirm stability.
					found := false
					for _, k := range ready {
						if k == l1Key {
							found = true
							break
						}
					}
					if !found {
						delete(dirty, l1Key)
					}
				}
			}

			if len(ready) == 0 {
				continue
			}

			fn, ok := rw.l1Refresh.Load().(L1RefreshFunc)
			if !ok || fn == nil {
				continue
			}
			slog.Info("resource-watcher: periodic dirty refresh",
				slog.Int("keys", len(ready)),
				slog.Int("remaining", len(dirty)))

			func() {
				defer func() {
					if r := recover(); r != nil {
						buf := make([]byte, 4096)
						n := runtime.Stack(buf, false)
						slog.Error("resource-watcher: panic in dirty refresh",
							slog.Any("error", r),
							slog.String("stack", string(buf[:n])))
					}
				}()
				refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				fn(refreshCtx, schema.GroupVersionResource{}, ready)
			}()
		}
	}
}

func (rw *ResourceWatcher) processL1Event(ctx context.Context, evt l1Event, dirty map[string]*dirtyEntry) {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			slog.Error("resource-watcher: panic in L1 worker",
				slog.Any("error", r),
				slog.String("gvr", evt.gvr.String()),
				slog.String("stack", string(buf[:n])))
		}
	}()

	l1Keys := rw.collectAffectedL1Keys(ctx, evt.gvrKey, evt.ns, evt.name)

	// CRD-based invalidation: when a CR is created/deleted, also find L1 keys
	// that depend on its CRD (e.g. compositions-get-ns-and-crd depends on the
	// CRD LIST). This handles the zero-state problem agnostically.
	crdGVRKey := GVRToKey(schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	})
	crdName := evt.gvr.Resource + "." + evt.gvr.Group
	crdL1Keys := rw.collectAffectedL1Keys(ctx, crdGVRKey, "", crdName)
	for _, k := range crdL1Keys {
		found := false
		for _, existing := range l1Keys {
			if existing == k {
				found = true
				break
			}
		}
		if !found {
			l1Keys = append(l1Keys, k)
		}
	}

	// Walk the dependency tree to find transitively-dependent L1 keys.
	// Example: CRD lookup finds compositions-get-ns-and-crd → expand finds
	// compositions-list → expand finds piechart, table.
	if len(l1Keys) > 0 {
		l1Keys = rw.expandDependents(ctx, l1Keys, 5)
	}

	if len(l1Keys) == 0 {
		return
	}

	// All events (ADD/UPDATE/DELETE): mark affected L1 keys as dirty.
	// Don't delete — serve stale. The periodic 3s ticker re-resolves them.
	// For DELETE: the per-resource dep index entry is cleaned up, but the
	// L1 key stays cached (stale for up to 3-6s) until the ticker refreshes it.
	if evt.eventType == "delete" {
		resDepKey := L1ResourceDepKey(evt.gvrKey, evt.ns, evt.name)
		_ = rw.cache.Delete(ctx, resDepKey)
	}

	gvrNS := evt.gvrKey + ":" + evt.ns
	for _, k := range l1Keys {
		entry := dirty[k]
		if entry == nil {
			entry = &dirtyEntry{
				targetGens: make(map[string]string),
				lastSeen:   make(map[string]string),
			}
			dirty[k] = entry
		}
		// Update target gen: always store the latest resourceVersion
		// for this GVR+ns so the ticker waits for L3 to catch up.
		if evt.resourceVersion > entry.targetGens[gvrNS] {
			entry.targetGens[gvrNS] = evt.resourceVersion
		}
	}

	slog.Debug("resource-watcher: L1 marked dirty",
		slog.String("event", evt.eventType),
		slog.String("gvr", evt.gvr.String()),
		slog.String("ns", evt.ns),
		slog.String("name", evt.name),
		slog.Int("dirty", len(l1Keys)))
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

	// ── Update L3 generation (resourceVersion) ──────────────────────────────
	// Store the latest resourceVersion for this GVR+ns so the dirty refresh
	// ticker can detect L3 changes without re-reading the full list.
	gvrKey := GVRToKey(gvr)
	l3GenKey := L3GenKey(gvrKey, ns)
	rv := uns.GetResourceVersion()
	if rv != "" {
		// Only update if new rv is greater (monotonic). Prevents out-of-order
		// events from lowering the generation, which would block the dirty ticker.
		luaMaxSet := `
			local cur = redis.call('GET', KEYS[1])
			if not cur or ARGV[1] > cur then
				redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
				return 1
			end
			return 0`
		ttlSec := int(rw.cache.TTLForGVR(gvr).Seconds())
		_ = rw.cache.client.Eval(ctx, luaMaxSet, []string{l3GenKey}, rv, ttlSec).Err()
	}

	// ── Enqueue L1 refresh event ─────────────────────────────────────────────
	// L3 was just patched above. Enqueue the event for the L1 worker to
	// process sequentially. The worker will read the latest L3 state.
	// Non-blocking: if the channel is full, drop the event (L3 is already
	// updated; the next event for this GVR will trigger a refresh).
	select {
	case rw.eventCh <- l1Event{
		gvr:             gvr,
		gvrKey:          gvrKey,
		ns:              ns,
		name:            name,
		eventType:       eventType,
		resourceVersion: rv,
	}:
	default:
		slog.Warn("resource-watcher: L1 event queue full, dropping event",
			slog.String("gvr", gvr.String()),
			slog.String("ns", ns),
			slog.String("name", name))
	}
}

// collectAffectedL1Keys returns the deduplicated set of L1 resolved keys that
// depend on the given K8s resource via per-resource dependency indexes:
//   - GET dependency:    L1 keys that fetched this specific resource
//   - LIST (namespaced): L1 keys that listed this GVR in this namespace
//   - LIST (cluster):    L1 keys that listed this GVR across all namespaces
func (rw *ResourceWatcher) collectAffectedL1Keys(ctx context.Context, gvrKey, ns, name string) []string {
	seen := map[string]bool{}
	collect := func(idxKey string) {
		if keys, err := rw.cache.SMembers(ctx, idxKey); err == nil {
			for _, k := range keys {
				seen[k] = true
			}
		}
	}

	// GET dependency: specific resource
	collect(L1ResourceDepKey(gvrKey, ns, name))
	// LIST dependency: namespaced
	if ns != "" {
		collect(L1ResourceDepKey(gvrKey, ns, ""))
	}
	// LIST dependency: cluster-wide
	collect(L1ResourceDepKey(gvrKey, "", ""))

	// API-level dependency: RESTActions that declared interest in this
	// GVR via their apiRequests. The GVR key format is "group/version/resource".
	collect(L1ApiDepKey(gvrKey))

	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// expandDependents walks the L1 dependency tree starting from the given keys.
// For each L1 key, it parses the key to extract GVR/ns/name, looks up
// L1ResourceDepKey to find L1 keys that depend on it, and adds them.
// Repeats up to maxDepth levels. Returns all keys (input + discovered).
//
// Example: compositions-get-ns-and-crd → compositions-list → piechart
func (rw *ResourceWatcher) expandDependents(ctx context.Context, l1Keys []string, maxDepth int) []string {
	seen := make(map[string]bool, len(l1Keys))
	for _, k := range l1Keys {
		seen[k] = true
	}

	pending := l1Keys
	for depth := 0; depth < maxDepth && len(pending) > 0; depth++ {
		var nextPending []string
		for _, l1Key := range pending {
			info, ok := ParseResolvedKey(l1Key)
			if !ok {
				continue
			}
			gvrKey := GVRToKey(info.GVR)
			depKey := L1ResourceDepKey(gvrKey, info.NS, info.Name)
			dependents, err := rw.cache.SMembers(ctx, depKey)
			if err != nil || len(dependents) == 0 {
				continue
			}
			for _, dep := range dependents {
				if !seen[dep] {
					seen[dep] = true
					nextPending = append(nextPending, dep)
				}
			}
		}
		pending = nextPending
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
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

