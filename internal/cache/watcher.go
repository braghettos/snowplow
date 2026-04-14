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

// L1ChangeInfo describes a single L3 change that triggered an L1 refresh.
// Used by the incremental L1 patch path (C2) to avoid full re-resolution
// when only one item changed.
type L1ChangeInfo struct {
	GVR       schema.GroupVersionResource
	Namespace string
	Name      string
	Operation string // "add", "update", "delete"
}

// L1RefreshFunc is invoked by the ResourceWatcher to proactively re-resolve
// L1 cache entries instead of deleting them. The function receives the GVR
// that triggered the refresh (for logging), the list of L1 keys to refresh,
// recent L3 changes (for incremental patching), and a long-lived context.
// It must run synchronously; the caller invokes it in a goroutine.
type L1RefreshFunc func(ctx context.Context, triggerGVR schema.GroupVersionResource, l1Keys []string, changes []L1ChangeInfo)

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

	// recentChanges tracks recent L3 changes for incremental L1 patching (C2).
	// handleEvent appends; scanL3Gens/reconcileGVR drains. Protected by
	// recentChangesMu. No capacity cap — the drain cycle (every 1-30s) is
	// the natural bound. Even 50K entries between drains is ~5MB.
	recentChanges   []L1ChangeInfo
	recentChangesMu sync.Mutex

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
		factory: dynamicinformer.NewFilteredDynamicSharedInformerFactory(
			dynClient, 0, metav1.NamespaceAll,
			func(opts *metav1.ListOptions) {
				// Chunk the initial LIST to avoid a transient memory spike
				// that OOMKills the pod at 50K+ compositions. Without this,
				// the informer decodes the entire LIST response (~15 GB for
				// 50K unstructured objects) in one pass. With Limit=500, it
				// pages through 100 batches of ~150 MB each, keeping peak
				// transient allocation under 500 MB.
				opts.Limit = 500
			},
		),
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
	lastSeen := make(map[string]string)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rw.scanL3Gens(ctx, lastSeen)
		case <-rw.l3genScanNow:
			rw.scanL3Gens(ctx, lastSeen)
		}
	}
}

// scanL3Gens scans all snowplow:l3gen:* keys, detects changes since the last
// scan, and refreshes affected L1 keys via the dependency indexes.
func (rw *ResourceWatcher) scanL3Gens(ctx context.Context, lastSeen map[string]string) {
	ctx, span := watcherTracer.Start(ctx, "l3gen.scan")
	defer span.End()

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

	// L3 is updated per-item atomically by handleEvent (index SETs are
	// O(1)). The old two-tick stability check was designed for monolithic
	// blob writes where partial data was possible — those writes were
	// removed in v0.25.180. Process changes immediately.

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
	span.AddEvent("l3gen.scan.refresh", trace.WithAttributes(
		attribute.Int("changed_gvr_ns", len(changedGVRNS)),
		attribute.Int("l1_keys", len(keys)),
	))

	// Launch at most ONE async refresh. If a refresh is already running,
	// skip this tick — the running refresh is processing changes, and the
	// next tick will catch any new ones. This prevents both goroutine
	// accumulation (Bug 7, unbounded go func) and scanner blocking
	// (synchronous approach regressed S4/S5 convergence from 3s to 40s).
	if !rw.l1RefreshRunning.CompareAndSwap(false, true) {
		slog.Debug("resource-watcher: l3gen refresh skipped (already running)")
		span.AddEvent("l1.refresh.skipped", trace.WithAttributes(
			attribute.String("reason", "already_running"),
		))
		return
	}
	// Commit staged lastSeen values now that the refresh is launching.
	// If CAS had failed above, lastSeen would stay stale and the next tick
	// would re-detect the changes, ensuring no events are dropped.
	for gk, v := range pendingLastSeen {
		lastSeen[gk] = v
	}

	// Drain recent changes for incremental L1 patching (C2).
	rw.recentChangesMu.Lock()
	changes := rw.recentChanges
	rw.recentChanges = nil
	rw.recentChangesMu.Unlock()

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
		fn(refreshCtx, schema.GroupVersionResource{}, keys, changes)
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
	// Strip heavy metadata before objects enter the informer's in-memory store.
	// managedFields + last-applied-configuration are 30-50% of each object but
	// never used by widgets. Without this, the informer store at 50K objects
	// consumes ~3.5GB instead of ~2GB (confirmed via pprof heap-223810).
	if err := informer.SetTransform(func(obj any) (any, error) {
		if uns, ok := obj.(*unstructured.Unstructured); ok {
			StripAnnotationsFromUnstructured(uns)
		}
		return obj, nil
	}); err != nil {
		slog.Debug("resource-watcher: SetTransform not supported", slog.Any("err", err))
	}
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
	ttl := rw.cache.TTLForGVR(gvr)

	switch eventType {
	case "delete":
		_ = rw.cache.Delete(ctx, getKey)
		// Per-item index: remove from namespace and cluster-wide index SETs.
		// O(1) — no read-modify-write of a blob.
		nsIdxKey := ListIndexKey(gvr, ns)
		_ = rw.cache.SRemMembers(ctx, nsIdxKey, name)
		if ns != "" {
			clusterIdxKey := ListIndexKey(gvr, "")
			_ = rw.cache.SRemMembers(ctx, clusterIdxKey, ns+"/"+name)
		}

	case "add", "update":
		// Safe to use directly: SetTransform already stripped at ingestion,
		// and SetForGVR only reads (json.Marshal) without mutating.
		if serr := rw.cache.SetForGVR(ctx, gvr, getKey, uns); serr != nil {
			slog.Warn("resource-watcher: failed to update GET cache",
				slog.String("key", getKey), slog.Any("err", serr))
		}
		// Per-item index: add to namespace and cluster-wide index SETs.
		// O(1) — no read-modify-write of a blob. The GET key written above
		// holds the item data; the index SET just tracks membership.
		nsIdxKey := ListIndexKey(gvr, ns)
		_ = rw.cache.SAddWithTTL(ctx, nsIdxKey, name, ttl)
		if ns != "" {
			clusterIdxKey := ListIndexKey(gvr, "")
			_ = rw.cache.SAddWithTTL(ctx, clusterIdxKey, ns+"/"+name, ttl)
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
	// SKIP for dynamic GVRs: per-item index updates are fast and contention-free,
	// but the l3gen bump can still race with the debounced reconciliation.
	// For dynamic GVRs, reconcileGVR (debounced below) is the sole trigger:
	// it rebuilds the L3 index + blob from the informer's authoritative state,
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
	// Per-item index updates (SADD/SREM) are O(1) and contention-free, but
	// during rapid deployment bursts the index may temporarily have members
	// whose GET keys haven't been written yet. For dynamic GVRs, schedule a
	// debounced reconciliation that rebuilds all indexes from the informer's
	// authoritative store once the burst settles, then triggers L1 refresh.
	if isDynamic {
		rw.scheduleDynamicReconcile(gvr)
	}

	// ── Record change for incremental L1 patching (C2) ──────────────────────
	rw.recentChangesMu.Lock()
	rw.recentChanges = append(rw.recentChanges, L1ChangeInfo{
		GVR:       gvr,
		Namespace: ns,
		Name:      name,
		Operation: eventType,
	})
	rw.recentChangesMu.Unlock()

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

	// 2. Read current L3 from per-item index (authoritative source).
	var l3List unstructured.UnstructuredList
	if raw, hit, gerr := rw.cache.AssembleListFromIndex(ctx, gvr, ""); gerr != nil {
		slog.Warn("reconcile: failed to read L3 index",
			slog.String("gvr", gvr.String()), slog.Any("err", gerr))
		errs++
		return
	} else if hit && raw != nil {
		if uerr := json.Unmarshal(raw, &l3List); uerr != nil {
			slog.Warn("reconcile: failed to unmarshal L3 index",
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
	// Collect all writes and flush in pipelined batches to avoid 5055
	// sequential Redis round-trips at large scale (was ~7s per round at 50K).
	const batchSize = 500
	pendingWrites := make(map[string]any, batchSize)
	flushPending := func() {
		if len(pendingWrites) == 0 {
			return
		}
		if serr := rw.cache.SetMultiForGVR(ctx, gvr, pendingWrites); serr != nil {
			errs++
		}
		pendingWrites = make(map[string]any, batchSize)
	}
	for k, entry := range informerMap {
		l3RV, inL3 := l3Map[k]
		if !inL3 {
			added++
		} else if l3RV != entry.rv {
			updated++
		} else {
			continue // identical, skip
		}
		// Safe to use directly: SetTransform already stripped at ingestion,
		// and SetMultiForGVR only reads (json.Marshal) without mutating.
		getKey := GetKey(gvr, entry.uns.GetNamespace(), entry.uns.GetName())
		pendingWrites[getKey] = entry.uns
		changedNamespaces[entry.uns.GetNamespace()] = true
		if len(pendingWrites) >= batchSize {
			flushPending()
		}
	}
	flushPending()

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
		// Safe to reference directly: SetTransform already stripped at
		// ingestion, and rebuildListCaches only reads (GetNamespace,
		// GetName) without mutating. Eliminates ~3GB transient allocation
		// at 50K scale.
		allItems := make([]unstructured.Unstructured, 0, len(informerMap))
		for _, entry := range informerMap {
			allItems = append(allItems, *entry.uns)
		}
		rw.rebuildListCaches(ctx, gvr, allItems)
		span.AddEvent("l3.rebuilt", trace.WithAttributes(
			attribute.String("gvr", gvr.String()),
			attribute.Int("items", len(allItems)),
		))

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
				_ = rw.cache.Delete(ctx, ListKey(gvr, ns), ListIndexKey(gvr, ns))
				span.AddEvent("stale_ns_key.cleared", trace.WithAttributes(
					attribute.String("gvr", gvr.String()),
					attribute.String("namespace", ns),
				))
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
					// Drain recentChanges for this GVR. handleEvent populates
					// the buffer BEFORE updating L3, so it contains the actual
					// mutations (add/update/delete) even though L3 is already
					// in sync by the time reconcileGVR runs.
					//
					// Previous approach (synthesize from reconcile diff) failed
					// because handleEvent already synced L3 before the debounce
					// timer fired — l3Map matches informerMap, zero diff.
					rw.recentChangesMu.Lock()
					var matched, unmatched []L1ChangeInfo
					for _, ch := range rw.recentChanges {
						if ch.GVR == gvr {
							matched = append(matched, ch)
						} else {
							unmatched = append(unmatched, ch)
						}
					}
					rw.recentChanges = unmatched
					rw.recentChangesMu.Unlock()

					// If zero changes were found in the buffer AND the
					// reconcile diff shows no additions/removals/updates,
					// skip the L1 refresh entirely. The L3 is already in
					// sync (handleEvent updated it immediately) and L1 is
					// still valid. Without this guard, every debounced
					// reconcile launches a 10-15s full re-resolve at 50K
					// even when nothing actually changed — the root cause
					// of the S7 spike.
					if len(matched) == 0 && added+removed+updated == 0 {
						slog.Debug("reconcile: skipping L1 refresh (no changes, L3 in sync)")
						rw.l1RefreshRunning.Store(false)
					} else {
						go func() {
							defer rw.l1RefreshRunning.Store(false)
							refreshCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
							defer cancel()
							fn(refreshCtx, gvr, l1Keys, matched)
						}()
					}
				}
			} else {
				_ = rw.cache.Delete(ctx, l1Keys...)
			}
		}
	}

	return
}

// rebuildListCaches writes namespace-scoped and cluster-wide per-item index
// SETs from the given items. Used by Reconcile to ensure list caches match
// the informer's authoritative state. Readers consume them via
// AssembleListFromIndex.
func (rw *ResourceWatcher) rebuildListCaches(ctx context.Context, gvr schema.GroupVersionResource, items []unstructured.Unstructured) {
	ttl := rw.cache.TTLForGVR(gvr)

	byNamespace := make(map[string][]unstructured.Unstructured)
	for i := range items {
		ns := items[i].GetNamespace()
		byNamespace[ns] = append(byNamespace[ns], items[i])
	}

	// ── Per-item index SETs ──────────────────────────────────────────────────

	// Build cluster-wide index: members are "ns/name" for namespaced resources.
	var clusterMembers []string
	for i := range items {
		ns := items[i].GetNamespace()
		name := items[i].GetName()
		if ns != "" {
			clusterMembers = append(clusterMembers, ns+"/"+name)
		} else {
			clusterMembers = append(clusterMembers, name)
		}
	}
	if len(clusterMembers) > 0 {
		_ = rw.cache.ReplaceSetWithTTL(ctx, ListIndexKey(gvr, ""), clusterMembers, ttl)
	} else {
		_ = rw.cache.Delete(ctx, ListIndexKey(gvr, ""))
	}

	// Build per-namespace indexes: members are just "name".
	for ns, nsItems := range byNamespace {
		if ns == "" {
			continue
		}
		members := make([]string, len(nsItems))
		for i := range nsItems {
			members[i] = nsItems[i].GetName()
		}
		_ = rw.cache.ReplaceSetWithTTL(ctx, ListIndexKey(gvr, ns), members, ttl)
	}

	// Legacy monolithic blob writes (cluster-wide + per-namespace) REMOVED.
	// At 50K scale, these writes added ~6-9s per reconcile round and ~100MB
	// Redis memory. The per-item index SETs above (ListIndexKey) are
	// authoritative and readers consume them via AssembleListFromIndex.
	_ = byNamespace // silence unused if all paths return
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
	case strings.HasPrefix(key, "snowplow:list-idx:"):
		// Index key expired — refresh the list (which rebuilds both index + blob).
		gvr, ns, ok := ParseListIndexKey(key)
		if ok && gvr.Resource != "" {
			rw.refreshListKey(ctx, gvr, ns)
		}
	case strings.HasPrefix(key, "snowplow:list:"):
		// Legacy blob keys — no longer written. Ignore expiry.
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
	ttl := rw.cache.TTLForGVR(gvr)

	// Write per-item GET keys and build index members.
	var members []string
	for i := range list.Items {
		obj := &list.Items[i]
		_ = rw.cache.SetForGVR(ctx, gvr, GetKey(gvr, obj.GetNamespace(), obj.GetName()), obj)
		if ns != "" {
			members = append(members, obj.GetName())
		} else if obj.GetNamespace() != "" {
			members = append(members, obj.GetNamespace()+"/"+obj.GetName())
		} else {
			members = append(members, obj.GetName())
		}
	}

	// Write per-item index SET.
	if len(members) > 0 {
		_ = rw.cache.ReplaceSetWithTTL(ctx, ListIndexKey(gvr, ns), members, ttl)
	} else {
		_ = rw.cache.Delete(ctx, ListIndexKey(gvr, ns))
	}

	GlobalMetrics.Inc(&GlobalMetrics.ExpiryRefreshes, "expiry_refreshes")
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

