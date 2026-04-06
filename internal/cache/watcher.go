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

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sdynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	k8scache "k8s.io/client-go/tools/cache"
)

var watcherTracer = otel.Tracer("snowplow/watcher")

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

// dynamicReconcileDebounce is how long to wait after the last informer event
// for a dynamic GVR before running reconcileGVR. This collapses rapid events
// into fewer reconciliation passes.
const dynamicReconcileDebounce = 2 * time.Second

// maxReconcileDelay caps how long reconciliation can be deferred during a
// sustained burst. Even if events keep arriving, reconcileGVR fires at least
// every maxReconcileDelay to keep L3 list caches reasonably fresh.
const maxReconcileDelay = 30 * time.Second


// l1Event represents an informer event that needs L1 refresh processing.
type l1Event struct {
	gvr             schema.GroupVersionResource
	gvrKey          string
	ns              string
	name            string
	eventType       string
	resourceVersion string // K8s resourceVersion of the object that triggered this event
}

// ResourceWatcher maintains dynamic informers for every GVR in the watched set.
type ResourceWatcher struct {
	cache     *RedisCache
	dynClient k8sdynamic.Interface
	factory   dynamicinformer.DynamicSharedInformerFactory
	mu        sync.Mutex
	watched   map[string]bool
	appCtx    context.Context // long-lived process context; set by Start()

	l1Refresh          atomic.Value // stores L1RefreshFunc
	eventCh            chan l1Event  // buffered channel for L1 refresh events (100k)
	autoDiscoverGroups []string     // CRD group suffix patterns for auto-registration

	// dynamicGVRs tracks GVRs registered at runtime via autoRegisterCRDInformer.
	// When events fire for these GVRs, a debounced reconciliation is scheduled
	// so that rapid deployment bursts collapse into a single reconcileGVR call.
	dynamicGVRs   map[string]schema.GroupVersionResource
	dynamicGVRsMu sync.Mutex

	// dynamicReconcileTimers holds per-GVR debounce state. When an event fires
	// for a dynamic GVR, the timer is reset — but only up to maxReconcileDelay
	// from the first event. This ensures reconciliation fires at least every
	// maxReconcileDelay even during sustained bursts.
	dynamicReconcileTimers    map[string]*time.Timer
	dynamicReconcileDeadlines map[string]time.Time

	// reconcileMu serializes reconcileGVR calls per GVR. Without this,
	// startInformer and scheduleDynamicReconcile can run reconcileGVR
	// concurrently for the same GVR, causing TOCTOU on L3 list writes (Bug 4).
	reconcileMu   sync.Map // map[string]*sync.Mutex — keyed by GVR string

	// l1RefreshRunning is a non-blocking mutex for L1 refresh. At most one
	// refresh goroutine runs at a time. If a refresh is already running,
	// new requests are skipped (the next l3gen tick will retry).
	// This prevents both goroutine accumulation (Bug 7) and scanner
	// blocking (synchronous approach regressed S4/S5 convergence).
	l1RefreshRunning atomic.Bool

	// l3genScanNow is a signal channel to trigger an immediate l3gen scan.
	// Used by scheduleDynamicReconcile after reconcileGVR updates L3, so
	// the l3gen scanner detects the changes instantly instead of waiting
	// for the next 3s tick.
	l3genScanNow chan struct{}

}

func NewResourceWatcher(c *RedisCache, rc *rest.Config) (*ResourceWatcher, error) {
	// Create a dedicated rest.Config for informers with its own rate limiter.
	// This prevents L1 refresh API calls (RBAC, objects.Get) from starving
	// informer WATCH streams. Without this, the shared rate limiter at QPS=100
	// is consumed by refresh bursts, causing informers to miss events.
	informerRC := rest.CopyConfig(rc)
	informerRC.QPS = 50
	informerRC.Burst = 100

	dynClient, err := k8sdynamic.NewForConfig(informerRC)
	if err != nil {
		return nil, err
	}
	return &ResourceWatcher{
		cache:                  c,
		dynClient:              dynClient,
		factory:                dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 0),
		watched:                make(map[string]bool),
		eventCh:                make(chan l1Event, 100000),
		dynamicGVRs:               make(map[string]schema.GroupVersionResource),
		dynamicReconcileTimers:    make(map[string]*time.Timer),
		dynamicReconcileDeadlines: make(map[string]time.Time),
		l3genScanNow:              make(chan struct{}, 1),
	}, nil
}

func (rw *ResourceWatcher) Start(ctx context.Context) {
	rw.appCtx = ctx
	// Register all known informers from Redis BEFORE starting the factory.
	// This ensures event handlers are attached before the initial LIST/WATCH,
	// so existing objects trigger ADD callbacks during the first sync.
	rw.registerFromRedis(ctx)
	// Start the factory ONCE — all registered informers begin their initial sync.
	rw.factory.Start(ctx.Done())
	// Periodically check for new GVRs added to Redis by other components.
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

// registerFromRedis reads the watched-gvrs set from Redis and registers
// informers for each GVR. Does NOT call factory.Start — the caller does
// that after all registrations are complete.
func (rw *ResourceWatcher) registerFromRedis(ctx context.Context) {
	members, err := rw.cache.SMembers(ctx, WatchedGVRsKey)
	if err != nil {
		slog.Warn("resource-watcher: failed to read watched GVR set", slog.Any("err", err))
		return
	}
	for _, key := range members {
		gvr := ParseGVRKey(key)
		if gvr.Resource == "" {
			continue
		}
		rw.registerInformer(gvr)
	}
}

// l1Worker has two responsibilities:
//
//  1. Consume events from eventCh so handleEvent doesn't block. Events are
//     currently only used for delete-related L1 dep cleanup.
//
//  2. Every 3s, scan all snowplow:l3gen:* keys in Redis. If any generation
//     changed since the last scan, find ALL L1 keys that depend on that
//     GVR+ns via the reverse dependency indexes and refresh them.
//
// This is fully agnostic — no GVR-specific logic. Any L3 change triggers
// the corresponding L1 refresh automatically.
func (rw *ResourceWatcher) l1Worker(ctx context.Context) {
	// Event consumer goroutine: handles DELETE dep index cleanup.
	// Runs independently so the event channel doesn't starve the ticker.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-rw.eventCh:
				if !ok {
					return
				}
				if evt.eventType == "delete" {
					resDepKey := L1ResourceDepKey(evt.gvrKey, evt.ns, evt.name)
					_ = rw.cache.Delete(ctx, resDepKey)
				}
			}
		}
	}()

	// L3gen scanner: runs every 1s independently of event volume.
	// Detects L3 changes via generation keys and refreshes affected L1 keys.
	// prevObserved tracks l3gen values from the previous tick — we only
	// launch a refresh when values are stable (no change from prev tick).
	// This avoids refreshing with partial L3 data during event bursts.
	lastSeen := make(map[string]string)
	prevObserved := make(map[string]string)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rw.scanL3Gens(ctx, lastSeen, prevObserved)
		case <-rw.l3genScanNow:
			rw.scanL3Gens(ctx, lastSeen, prevObserved)
		}
	}
}

// scanL3Gens scans all snowplow:l3gen:* keys, detects changes since the last
// scan, and refreshes affected L1 keys via the dependency indexes.
func (rw *ResourceWatcher) scanL3Gens(ctx context.Context, lastSeen, prevObserved map[string]string) {
	fn, ok := rw.l1Refresh.Load().(L1RefreshFunc)
	if !ok || fn == nil {
		// Don't update lastSeen — accumulate changes until fn is available.
		return
	}

	// Scan all l3gen keys using SCAN (non-blocking) instead of KEYS (blocks Redis).
	genKeys, err := rw.cache.ScanKeys(ctx, "snowplow:l3gen:*")
	if err != nil || len(genKeys) == 0 {
		return
	}

	// Read current values in a pipeline for efficiency.
	pipe := rw.cache.client.Pipeline()
	cmds := make(map[string]*redis.StringCmd, len(genKeys))
	for _, gk := range genKeys {
		cmds[gk] = pipe.Get(ctx, gk)
	}
	_, _ = pipe.Exec(ctx)

	// Prune expired keys from lastSeen to prevent unbounded memory growth.
	currentKeys := make(map[string]bool, len(genKeys))
	for _, gk := range genKeys {
		currentKeys[gk] = true
	}
	for gk := range lastSeen {
		if !currentKeys[gk] {
			delete(lastSeen, gk)
		}
	}

	// Snapshot current l3gen values for stability comparison.
	currentObserved := make(map[string]string, len(cmds))
	for gk, cmd := range cmds {
		if curVal, _ := cmd.Result(); curVal != "" {
			currentObserved[gk] = curVal
		}
	}

	// Stable-tick debounce: only launch a refresh if l3gen values match
	// what we saw last tick. If values changed since prev tick, events
	// are still arriving — wait for quiescence to avoid refreshing with
	// partial L3 data during bursts.
	stable := true
	if len(currentObserved) != len(prevObserved) {
		stable = false
	} else {
		for gk, v := range currentObserved {
			if prevObserved[gk] != v {
				stable = false
				break
			}
		}
	}
	// Update prevObserved for next tick (swap contents in place).
	for gk := range prevObserved {
		delete(prevObserved, gk)
	}
	for gk, v := range currentObserved {
		prevObserved[gk] = v
	}
	if !stable {
		slog.Debug("resource-watcher: l3gen unstable — waiting for next tick",
			slog.Int("observed", len(currentObserved)))
		return
	}

	// Find which GVR+ns changed. Stage lastSeen updates — only commit them
	// when the refresh actually launches (CAS succeeds). If CAS fails,
	// lastSeen stays stale so the next tick re-detects the changes.
	changedGVRNS := make(map[string]bool)
	pendingLastSeen := make(map[string]string)
	for gk, curVal := range currentObserved {
		if lastSeen[gk] != curVal {
			pendingLastSeen[gk] = curVal
			// Extract GVR+ns from key: "snowplow:l3gen:{gvrKey}:{ns}"
			// The gvrKey is "group/version/resource", ns is the namespace.
			suffix := strings.TrimPrefix(gk, "snowplow:l3gen:")
			changedGVRNS[suffix] = true
		}
	}

	if len(changedGVRNS) == 0 {
		slog.Debug("resource-watcher: l3gen scan — no changes",
			slog.Int("total_keys", len(genKeys)))
		return
	}

	// For each changed GVR+ns, find affected L1 keys via dependency indexes.
	affected := make(map[string]bool)
	for gvrNS := range changedGVRNS {
		// Parse "group/version/resource:namespace" into gvrKey and ns.
		// The gvrKey contains slashes, ns is after the last colon.
		lastColon := strings.LastIndex(gvrNS, ":")
		if lastColon < 0 {
			continue
		}
		gvrKey := gvrNS[:lastColon]
		ns := gvrNS[lastColon+1:]

		// Collect L1 keys that depend on this GVR+ns via precise indexes only.
		// Deliberately excludes L1GVRKey (too broad — 5000+ keys for popular GVRs).
		collect := func(idxKey string) {
			if keys, err := rw.cache.SMembers(ctx, idxKey); err == nil {
				for _, k := range keys {
					affected[k] = true
				}
			}
		}
		collect(L1ResourceDepKey(gvrKey, ns, ""))   // namespaced LIST
		collect(L1ResourceDepKey(gvrKey, "", ""))    // cluster-wide LIST
		collect(L1ApiDepKey(gvrKey))                 // API-level dep

		// CRD-based chain: for any GVR, also look up L1 keys that depend on
		// the cluster-wide CRD LIST. This handles zero-state agnostically —
		// e.g. composition ADD → CRD LIST dep → compositions-get-ns-and-crd.
		//
		// Use the cluster-wide LIST dep (typically 1-2 keys: compositions-get-ns-and-crd
		// per user). Do NOT use the specific CRD GET dep (40+ form widgets that
		// check CRD schema for validation — they don't need refresh on data changes).
		gvr := ParseGVRKey(gvrKey)
		if gvr.Group != "" && gvr.Resource != "" {
			crdGVRKey := GVRToKey(schema.GroupVersionResource{
				Group:    "apiextensions.k8s.io",
				Version:  "v1",
				Resource: "customresourcedefinitions",
			})
			collect(L1ResourceDepKey(crdGVRKey, "", ""))
		}
	}

	if len(affected) == 0 {
		return
	}

	// Walk dependency tree to find transitively affected L1 keys.
	// Starting from ~2-6 keys (e.g. compositions-get-ns-and-crd),
	// expand to compositions-list → piechart → table (~10 keys total).
	rw.expandDependents(ctx, affected, 5)

	keys := make([]string, 0, len(affected))
	for k := range affected {
		keys = append(keys, k)
	}

	slog.Info("resource-watcher: l3gen scan refresh",
		slog.Int("changed_gvr_ns", len(changedGVRNS)),
		slog.Int("l1_keys", len(keys)))

	// Launch at most ONE async refresh. If a refresh is already running,
	// skip this tick — the running refresh is processing changes, and the
	// next tick will catch any new ones. This prevents both goroutine
	// accumulation (Bug 7, unbounded go func) and scanner blocking
	// (synchronous approach regressed S4/S5 convergence from 3s to 40s).
	if !rw.l1RefreshRunning.CompareAndSwap(false, true) {
		slog.Debug("resource-watcher: l3gen refresh skipped (already running)")
		return
	}
	// Commit staged lastSeen values now that the refresh is launching.
	// If CAS had failed above, lastSeen would stay stale and the next tick
	// would re-detect the changes, ensuring no events are dropped.
	for gk, v := range pendingLastSeen {
		lastSeen[gk] = v
	}
	go func() {
		defer rw.l1RefreshRunning.Store(false)
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 4096)
				n := runtime.Stack(buf, false)
				slog.Error("resource-watcher: panic in l3gen refresh",
					slog.Any("error", r),
					slog.String("stack", string(buf[:n])))
			}
		}()
		refreshCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		fn(refreshCtx, schema.GroupVersionResource{}, keys)
	}()
}

// expandDependents walks the L1 dependency tree and adds all transitively
// dependent keys to the affected set. For example, refreshing compositions-list
// also needs to refresh piechart which depends on it.
func (rw *ResourceWatcher) expandDependents(ctx context.Context, affected map[string]bool, maxDepth int) {
	pending := make([]string, 0, len(affected))
	for k := range affected {
		pending = append(pending, k)
	}

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
				if !affected[dep] {
					affected[dep] = true
					nextPending = append(nextPending, dep)
				}
			}
		}
		pending = nextPending
	}
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
// SetAutoDiscoverGroups configures which CRD groups trigger automatic informer
// registration. Patterns use suffix matching: "*.krateo.io" matches any group
// ending in ".krateo.io".
func (rw *ResourceWatcher) SetAutoDiscoverGroups(patterns []string) {
	rw.autoDiscoverGroups = patterns
}

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
// After starting, it waits for the informer to sync and then patches L3
// from the informer's cache to ensure all existing objects are in L3.
func (rw *ResourceWatcher) startInformer(gvr schema.GroupVersionResource) {
	if !rw.registerInformer(gvr) {
		return
	}
	rw.factory.Start(rw.appCtx.Done())

	// Wait for the new informer to sync (initial LIST complete).
	// Use a short timeout — if it doesn't sync, the l3gen scanner will
	// catch up later via periodic scans.
	syncCtx, cancel := context.WithTimeout(rw.appCtx, 30*time.Second)
	defer cancel()
	informer := rw.factory.ForResource(gvr).Informer()
	k8scache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced)

	if !informer.HasSynced() {
		slog.Warn("resource-watcher: new informer did not sync in time",
			slog.String("gvr", gvr.String()))
		return
	}

	// Reconcile L3 from the informer's authoritative store in a single pass.
	// This replaces the old event-by-event replay which caused sustained
	// AtomicUpdateJSON contention on list keys (e.g. 4759 RESTActions each
	// hitting 20 retries on the cluster-wide list key).
	// reconcileGVR writes individual GET keys + rebuilds all list caches at once.
	storeSize := len(informer.GetStore().List())
	slog.Info("resource-watcher: reconciling new informer with L3",
		slog.String("gvr", gvr.String()),
		slog.Int("storeSize", storeSize))
	added, removed, updated, errs := rw.reconcileGVR(rw.appCtx, gvr)
	if added+removed+updated > 0 || errs > 0 {
		slog.Info("resource-watcher: initial reconciliation done",
			slog.String("gvr", gvr.String()),
			slog.Int("added", added),
			slog.Int("removed", removed),
			slog.Int("updated", updated),
			slog.Int("errors", errs))
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

	gvrKey := GVRToKey(gvr)

	// Check if GVR is dynamic once for both l3gen bump and reconcile decisions.
	rw.dynamicGVRsMu.Lock()
	_, isDynamic := rw.dynamicGVRs[gvrKey]
	rw.dynamicGVRsMu.Unlock()

	// ── Update L3 generation (resourceVersion) ──────────────────────────────
	// Store the latest resourceVersion for this GVR+ns so the l3gen scanner
	// can detect L3 changes without re-reading the full list.
	//
	// SKIP for dynamic GVRs: their L3 list updates via patchListCache are
	// under WATCH/MULTI contention and may lag the l3gen bump. If the
	// scanner fires based on the l3gen bump while the list is still being
	// patched, it refreshes L1 with stale/ghost items (S8 tail bug).
	// For dynamic GVRs, reconcileGVR (debounced below) is the sole trigger:
	// it first rebuilds the L3 list from the informer's authoritative state,
	// then triggers the L1 refresh from consistent data.
	rv := uns.GetResourceVersion()
	if !isDynamic && rv != "" {
		l3GenKey := L3GenKey(gvrKey, ns)
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

	// ── Auto-register informers for new CRDs ────────────────────────────────
	// When the CRD informer fires an ADD event for a new CustomResourceDefinition,
	// extract the GVR from the CRD spec and register an informer for it.
	// This ensures snowplow watches new CR types immediately when the CRD
	// appears — e.g. when a CompositionDefinition creates a new CRD.
	if gvr.Resource == "customresourcedefinitions" && gvr.Group == "apiextensions.k8s.io" && (eventType == "add" || eventType == "update") {
		rw.autoRegisterCRDInformer(uns)
	}

	// ── Debounced reconciliation for dynamic GVRs ────────────────────────────
	// patchListCache updates L3 list keys one-by-one via AtomicUpdateJSON.
	// Under rapid deployment bursts this can lag behind due to Redis transaction
	// contention. For dynamic GVRs, schedule a debounced reconciliation that
	// rebuilds all list caches from the informer's authoritative store once
	// the burst settles, then triggers L1 refresh from consistent data.
	if isDynamic {
		rw.scheduleDynamicReconcile(gvr)
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

// autoRegisterCRDInformer extracts the GVR from a CustomResourceDefinition
// unstructured object and registers an informer for it. This ensures snowplow
// watches new CR types as soon as their CRD appears.
func (rw *ResourceWatcher) autoRegisterCRDInformer(uns *unstructured.Unstructured) {
	// Extract group from spec.group
	group, _, _ := unstructured.NestedString(uns.Object, "spec", "group")
	if group == "" {
		slog.Debug("resource-watcher: CRD auto-register: empty group", slog.String("crd", uns.GetName()))
		return
	}
	// Check if the group matches any configured autoDiscoverGroups pattern.
	if !rw.matchesAutoDiscoverGroup(group) {
		slog.Debug("resource-watcher: CRD auto-register: group not in autoDiscoverGroups",
			slog.String("crd", uns.GetName()), slog.String("group", group),
			slog.Int("patterns", len(rw.autoDiscoverGroups)))
		return
	}
	// Extract resource (plural) from spec.names.plural
	plural, _, _ := unstructured.NestedString(uns.Object, "spec", "names", "plural")
	if plural == "" {
		return
	}
	// Extract the served version from spec.versions
	versions, found, _ := unstructured.NestedSlice(uns.Object, "spec", "versions")
	if !found || len(versions) == 0 {
		return
	}
	var version string
	for _, v := range versions {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		served, _, _ := unstructured.NestedBool(vm, "served")
		if served {
			version, _, _ = unstructured.NestedString(vm, "name")
			break
		}
	}
	if version == "" {
		return
	}

	newGVR := schema.GroupVersionResource{Group: group, Version: version, Resource: plural}
	rw.startInformer(newGVR)
	slog.Info("resource-watcher: auto-registered informer for new CRD",
		slog.String("crd", uns.GetName()),
		slog.String("gvr", newGVR.String()))
	// Also register in Redis so warmup can discover it next restart.
	if rw.cache != nil {
		_ = rw.cache.SAddGVR(context.Background(), newGVR)
	}
	// Track as dynamic GVR for periodic reconciliation.
	rw.dynamicGVRsMu.Lock()
	rw.dynamicGVRs[GVRToKey(newGVR)] = newGVR
	rw.dynamicGVRsMu.Unlock()
}

// matchesAutoDiscoverGroup checks if a CRD group matches any configured
// autoDiscoverGroups pattern. Patterns support suffix matching with "*." prefix.
func (rw *ResourceWatcher) matchesAutoDiscoverGroup(group string) bool {
	if len(rw.autoDiscoverGroups) == 0 {
		return false
	}
	for _, pattern := range rw.autoDiscoverGroups {
		if strings.HasPrefix(pattern, "*.") {
			suffix := pattern[1:] // e.g. ".krateo.io"
			if strings.HasSuffix(group, suffix) {
				return true
			}
		} else if group == pattern {
			return true
		}
	}
	return false
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

// scheduleDynamicReconcile debounces reconciliation for a dynamic GVR.
// Each call resets the timer, but only up to maxReconcileDelay from the first
// event. This ensures:
//   - Quiet periods: reconciliation fires 5s after the last event
//   - Sustained bursts: reconciliation fires every 30s regardless
func (rw *ResourceWatcher) scheduleDynamicReconcile(gvr schema.GroupVersionResource) {
	key := GVRToKey(gvr)

	rw.dynamicGVRsMu.Lock()
	defer rw.dynamicGVRsMu.Unlock()

	deadline, hasDeadline := rw.dynamicReconcileDeadlines[key]
	now := time.Now()

	if t, ok := rw.dynamicReconcileTimers[key]; ok {
		// Timer exists — reset it, but cap at the deadline.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// Deadline already passed — let the timer fire immediately.
			return
		}
		delay := dynamicReconcileDebounce
		if delay > remaining {
			delay = remaining
		}
		t.Reset(delay)
		return
	}

	// First event — set the deadline and start the timer.
	if !hasDeadline {
		rw.dynamicReconcileDeadlines[key] = now.Add(maxReconcileDelay)
	}

	rw.dynamicReconcileTimers[key] = time.AfterFunc(dynamicReconcileDebounce, func() {
		// Clean up BEFORE running reconcileGVR so that new events
		// arriving during reconciliation create a fresh timer instead
		// of calling Reset() on this fired AfterFunc timer (Bug 3:
		// Reset on a fired AfterFunc is undefined per Go docs and can
		// silently drop events or schedule duplicate callbacks).
		rw.dynamicGVRsMu.Lock()
		delete(rw.dynamicReconcileTimers, key)
		delete(rw.dynamicReconcileDeadlines, key)
		rw.dynamicGVRsMu.Unlock()

		ctx := rw.appCtx
		added, removed, updated, errs := rw.reconcileGVR(ctx, gvr)
		if added+removed+updated > 0 {
			slog.Info("reconcile-dynamic: patched L3",
				slog.String("gvr", gvr.String()),
				slog.Int("added", added),
				slog.Int("removed", removed),
				slog.Int("updated", updated),
				slog.Int("errors", errs))
		}

		// Note: we do NOT signal l3genScanNow here. reconcileGVR
		// already triggers its own L1 refresh (lines 1066+), so
		// also waking the l3gen scanner would cause a redundant
		// second L1 refresh for the same L3 changes (Bug 11).
		// The scanner will still detect the l3gen bump on its
		// next 3s tick, but l1RefreshRunning prevents duplicate work.
	})
}

// ReconcileStats holds aggregate results from a startup reconciliation pass.
type ReconcileStats struct {
	GVRs     int
	Added    int
	Removed  int
	Updated  int
	Errors   int
	Duration time.Duration
}

// Reconcile compares L3 cache (Redis) with informer stores (in-memory, authoritative)
// for every watched GVR. Any differences — ghost objects from missed DELETEs during
// pod downtime, missing objects, stale versions — are patched in L3. This ensures the
// cache is correct even when events were lost between restarts.
//
// Must be called after WaitForSync (informers have completed initial LIST/WATCH) and
// after L3 warmup (baseline data exists in Redis).
func (rw *ResourceWatcher) Reconcile(ctx context.Context) ReconcileStats {
	start := time.Now()
	var stats ReconcileStats

	// Snapshot watched GVRs.
	rw.mu.Lock()
	gvrKeys := make([]string, 0, len(rw.watched))
	for k := range rw.watched {
		gvrKeys = append(gvrKeys, k)
	}
	rw.mu.Unlock()

	for _, key := range gvrKeys {
		gvr := ParseGVRKey(key)
		if gvr.Resource == "" {
			continue
		}
		added, removed, updated, errs := rw.reconcileGVR(ctx, gvr)
		stats.GVRs++
		stats.Added += added
		stats.Removed += removed
		stats.Updated += updated
		stats.Errors += errs

		if added+removed+updated > 0 {
			slog.Info("reconcile: patched L3",
				slog.String("gvr", gvr.String()),
				slog.Int("added", added),
				slog.Int("removed", removed),
				slog.Int("updated", updated))
		}
	}

	stats.Duration = time.Since(start)
	return stats
}

func (rw *ResourceWatcher) reconcileGVR(ctx context.Context, gvr schema.GroupVersionResource) (added, removed, updated, errs int) {
	ctx, span := watcherTracer.Start(ctx, "reconcile.gvr",
		trace.WithAttributes(
			attribute.String("gvr", gvr.String()),
		))
	defer func() {
		if span.IsRecording() {
			span.SetAttributes(
				attribute.Int("reconcile.added", added),
				attribute.Int("reconcile.removed", removed),
				attribute.Int("reconcile.updated", updated),
				attribute.Int("reconcile.errors", errs),
			)
		}
		span.End()
	}()

	// Per-GVR mutex: prevent concurrent reconciliation for the same GVR (Bug 4).
	// startInformer and scheduleDynamicReconcile can both call reconcileGVR
	// simultaneously, causing TOCTOU on L3 list cache writes.
	muKey := GVRToKey(gvr)
	muVal, _ := rw.reconcileMu.LoadOrStore(muKey, &sync.Mutex{})
	mu := muVal.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	// 1. Read authoritative state from informer store (in-memory, no API call).
	lister := rw.factory.ForResource(gvr).Lister()
	objects, err := lister.List(labels.Everything())
	if err != nil {
		slog.Warn("reconcile: failed to list informer store",
			slog.String("gvr", gvr.String()), slog.Any("err", err))
		errs++
		return
	}

	// Build map of informer objects keyed by ns/name.
	type objEntry struct {
		uns *unstructured.Unstructured
		rv  string
	}
	informerMap := make(map[string]objEntry, len(objects))
	for _, obj := range objects {
		uns, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		k := uns.GetNamespace() + "/" + uns.GetName()
		informerMap[k] = objEntry{uns: uns, rv: uns.GetResourceVersion()}
	}

	// 2. Read current L3 cluster-wide list from Redis.
	var l3List unstructured.UnstructuredList
	clusterListKey := ListKey(gvr, "")
	if raw, hit, gerr := rw.cache.GetRaw(ctx, clusterListKey); gerr != nil {
		slog.Warn("reconcile: failed to read L3 list",
			slog.String("gvr", gvr.String()), slog.Any("err", gerr))
		errs++
		return
	} else if hit && raw != nil {
		if uerr := json.Unmarshal(raw, &l3List); uerr != nil {
			slog.Warn("reconcile: failed to unmarshal L3 list",
				slog.String("gvr", gvr.String()), slog.Any("err", uerr))
			errs++
			return
		}
	}

	// Build map of L3 objects.
	l3Map := make(map[string]string, len(l3List.Items)) // ns/name → resourceVersion
	for i := range l3List.Items {
		item := &l3List.Items[i]
		k := item.GetNamespace() + "/" + item.GetName()
		l3Map[k] = item.GetResourceVersion()
	}

	// 3. Diff: find additions, removals, and updates.
	// Track which namespaces have changes for precise L1 refresh.
	var keysToDelete []string
	changedNamespaces := make(map[string]bool)

	// Objects in L3 but NOT in informer → ghost objects (missed DELETEs).
	for k := range l3Map {
		if _, exists := informerMap[k]; !exists {
			parts := strings.SplitN(k, "/", 2)
			ns, name := parts[0], parts[1]
			keysToDelete = append(keysToDelete, GetKey(gvr, ns, name))
			changedNamespaces[ns] = true
			removed++
		}
	}

	// Objects in informer but NOT in L3, or with different resourceVersion.
	for k, entry := range informerMap {
		l3RV, inL3 := l3Map[k]
		if !inL3 {
			added++
		} else if l3RV != entry.rv {
			updated++
		} else {
			continue // identical, skip
		}
		stripped := entry.uns.DeepCopy()
		StripAnnotationsFromUnstructured(stripped)
		getKey := GetKey(gvr, stripped.GetNamespace(), stripped.GetName())
		if serr := rw.cache.SetForGVR(ctx, gvr, getKey, stripped); serr != nil {
			errs++
		}
		changedNamespaces[stripped.GetNamespace()] = true
	}

	// 4. Delete orphaned GET keys.
	if len(keysToDelete) > 0 {
		if derr := rw.cache.Delete(ctx, keysToDelete...); derr != nil {
			slog.Warn("reconcile: failed to delete orphaned GET keys",
				slog.String("gvr", gvr.String()), slog.Any("err", derr))
			errs++
		}
	}

	// 5. Rebuild list caches from informer state if anything changed.
	if added+removed+updated > 0 {
		var allItems []unstructured.Unstructured
		for _, entry := range informerMap {
			stripped := entry.uns.DeepCopy()
			StripAnnotationsFromUnstructured(stripped)
			allItems = append(allItems, *stripped)
		}
		rw.rebuildListCaches(ctx, gvr, allItems)

		// Clear per-namespace list keys for namespaces that had changes but
		// now have 0 items in the informer (e.g. namespace deleted with all
		// its resources). rebuildListCaches only WRITES lists for namespaces
		// WITH items, so stale per-ns list keys with ghost entries would
		// otherwise persist until TTL expiry (causing the S8 21s tail bug).
		nsWithItems := make(map[string]bool)
		for _, it := range allItems {
			if ns := it.GetNamespace(); ns != "" {
				nsWithItems[ns] = true
			}
		}
		for ns := range changedNamespaces {
			if ns != "" && !nsWithItems[ns] {
				_ = rw.cache.Delete(ctx, ListKey(gvr, ns))
				slog.Debug("reconcile: cleared stale empty-ns list key",
					slog.String("gvr", gvr.String()),
					slog.String("ns", ns))
			}
		}

		// Trigger L1 refresh using PRECISE dependency indexes (same approach
		// as the l3gen scanner). The broad L1GVRKey index can yield 2000+
		// keys for popular GVRs (e.g. every per-composition widget/button),
		// causing the refresh to time out. The precise indexes yield only
		// the RESTAction + widget keys that aggregate compositions
		// (typically 6-10 keys), which complete in <1s.
		gvrKey := GVRToKey(gvr)
		affected := make(map[string]bool)
		collect := func(idxKey string) {
			if keys, serr := rw.cache.SMembers(ctx, idxKey); serr == nil {
				for _, k := range keys {
					affected[k] = true
				}
			}
		}
		// Namespace-scoped list deps — only for namespaces that actually changed.
		// For S7 (delete 1 composition from bench-ns-01), this collects deps from
		// bench-ns-01 only (~6 keys) instead of all 120 namespaces.
		for ns := range changedNamespaces {
			collect(L1ResourceDepKey(gvrKey, ns, ""))
		}
		collect(L1ResourceDepKey(gvrKey, "", "")) // cluster-wide LIST dep
		collect(L1ApiDepKey(gvrKey))              // API-level dep
		// CRD-based chain (same as l3gen scanner).
		crdGVRKey := GVRToKey(schema.GroupVersionResource{
			Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
		})
		collect(L1ResourceDepKey(crdGVRKey, "", ""))

		// Expand transitive dependencies (e.g. compositions-list → piechart).
		rw.expandDependents(ctx, affected, 5)

		if len(affected) > 0 {
			l1Keys := make([]string, 0, len(affected))
			for k := range affected {
				l1Keys = append(l1Keys, k)
			}
			if fn, ok := rw.l1Refresh.Load().(L1RefreshFunc); ok && fn != nil {
				// Bounded async: at most one refresh at a time via
				// l1RefreshRunning. If the l3gen scanner is already
				// refreshing, skip — it will pick up our changes.
				if rw.l1RefreshRunning.CompareAndSwap(false, true) {
					go func() {
						defer rw.l1RefreshRunning.Store(false)
						refreshCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
						defer cancel()
						fn(refreshCtx, gvr, l1Keys)
					}()
				}
			} else {
				_ = rw.cache.Delete(ctx, l1Keys...)
			}
		}
	}

	return
}

// rebuildListCaches writes namespace-scoped and cluster-wide LIST keys from
// the given items. Used by Reconcile to ensure list caches match the
// informer's authoritative state.
func (rw *ResourceWatcher) rebuildListCaches(ctx context.Context, gvr schema.GroupVersionResource, items []unstructured.Unstructured) {
	byNamespace := make(map[string][]unstructured.Unstructured)
	for i := range items {
		ns := items[i].GetNamespace()
		byNamespace[ns] = append(byNamespace[ns], items[i])
	}

	// Write cluster-wide list. Skip if empty: we don't know the Kind for this
	// GVR without at least one item, and writing a malformed list (missing
	// Kind/APIVersion) would cause patchListCache to fail on subsequent events.
	// The list will be created correctly when the first ADD event arrives.
	if len(items) > 0 {
		clusterList := &unstructured.UnstructuredList{Items: items}
		clusterList.SetAPIVersion(items[0].GetAPIVersion())
		clusterList.SetKind(items[0].GetKind() + "List")
		_ = rw.cache.SetForGVR(ctx, gvr, ListKey(gvr, ""), clusterList)
	}

	// Write per-namespace lists.
	for ns, nsItems := range byNamespace {
		if ns == "" {
			continue
		}
		if len(nsItems) == 0 {
			continue
		}
		nsList := &unstructured.UnstructuredList{Items: nsItems}
		nsList.SetAPIVersion(nsItems[0].GetAPIVersion())
		nsList.SetKind(nsItems[0].GetKind() + "List")
		_ = rw.cache.SetForGVR(ctx, gvr, ListKey(gvr, ns), nsList)
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
		// Heal malformed lists that were previously stored without Kind/APIVersion
		// (e.g. empty lists written by rebuildListCaches before this fix).
		if list.GetKind() == "" {
			list.SetKind(uns.GetKind() + "List")
		}
		if list.GetAPIVersion() == "" {
			list.SetAPIVersion(uns.GetAPIVersion())
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

