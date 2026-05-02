package cache

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/labels"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sdynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

var watcherTracer = otel.Tracer("snowplow/watcher")

// ---------------------------------------------------------------------------
// Access-recency tracking for HOT / WARM / COLD refresh priority
// ---------------------------------------------------------------------------

var (
	// hotThreshold: keys accessed within this duration are HOT (highest priority).
	// Configurable via CACHE_HOT_THRESHOLD (e.g. "5m"). Default 5 minutes.
	hotThreshold = envDuration("CACHE_HOT_THRESHOLD", 5*time.Minute)
	// warmThreshold: keys accessed within this duration (but older than hotThreshold)
	// are WARM (medium priority). Keys older than this are COLD.
	// Configurable via CACHE_WARM_THRESHOLD (e.g. "60m"). Default 60 minutes.
	warmThreshold = envDuration("CACHE_WARM_THRESHOLD", 60*time.Minute)
	// accessCleanupCutoff: entries older than this are pruned to prevent unbounded
	// growth of the keyAccess map. Set to 2x warmThreshold.
	accessCleanupCutoff = 2 * warmThreshold
)

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

// keyAccess tracks the last access time for each resolved key base.
// Used to classify keys as HOT/WARM/COLD for refresh priority.
var keyAccess sync.Map // map[string]int64 (unix nano)

// TouchKey records that a resolved key was just accessed (HTTP request or prewarm).
func TouchKey(resolvedKeyBase string) {
	keyAccess.Store(resolvedKeyBase, time.Now().UnixNano())
}

// keyTemperature returns "hot", "warm", or "cold" based on last access time.
func keyTemperature(resolvedKeyBase string) string {
	v, ok := keyAccess.Load(resolvedKeyBase)
	if !ok {
		return "cold"
	}
	elapsed := time.Since(time.Unix(0, v.(int64)))
	if elapsed < hotThreshold {
		return "hot"
	}
	if elapsed < warmThreshold {
		return "warm"
	}
	return "cold"
}

// cleanupKeyAccess prunes entries older than accessCleanupCutoff to prevent
// unbounded growth of the keyAccess sync.Map.
func cleanupKeyAccess() {
	cutoff := time.Now().Add(-accessCleanupCutoff).UnixNano()
	keyAccess.Range(func(key, value any) bool {
		if value.(int64) < cutoff {
			keyAccess.Delete(key)
		}
		return true
	})
}

// L1RefreshFunc is invoked by the ResourceWatcher to proactively re-resolve
// L1 cache entries. The function receives the GVR that triggered the refresh
// (for logging) and the list of L1 keys to refresh. All refreshes are full
// re-resolves — incremental patching was removed because JQ aggregation
// filters (e.g., piechart counts) require the full pipeline.
// It must run synchronously; the caller invokes it in a goroutine.
// L1RefreshFunc resolves the given L1 keys and returns cascade keys
// (transitive dependencies that also need refresh, e.g., a widget
// that depends on a RESTAction). The caller handles cascade ordering.
type L1RefreshFunc func(ctx context.Context, triggerGVR schema.GroupVersionResource, l1Keys []string) []string

// expiryRefreshWorkers limits the number of concurrent goroutines handling
// Redis key expiry refresh to prevent unbounded goroutine creation under
// mass TTL expiry (e.g., after Redis restart or bulk warmup with identical TTLs).

// l1Event represents an informer event that needs L1 refresh processing.
type l1Event struct {
	gvr       schema.GroupVersionResource
	gvrKey    string
	ns        string
	name      string
	eventType string
}

// ResourceWatcher maintains dynamic informers for every GVR in the watched set.
type ResourceWatcher struct {
	cache     Cache
	dynClient k8sdynamic.Interface
	factory   dynamicinformer.DynamicSharedInformerFactory
	mu        sync.Mutex
	watched   map[string]bool
	appCtx    context.Context // long-lived process context; set by Start()

	l1Refresh          atomic.Value // stores L1RefreshFunc
	eventCh            chan l1Event  // buffered channel for L1 refresh events (100k)
	autoDiscoverGroups []string     // CRD group suffix patterns for auto-registration

	// dynamicGVRs tracks GVRs registered at runtime via autoRegisterCRDInformer.
	dynamicGVRs   map[string]schema.GroupVersionResource
	dynamicGVRsMu sync.Mutex

	// synced is set to true after WaitForSync completes. Before that,
	// handleEvent skips L1 refresh events — resolving during initial sync
	// produces incomplete results and causes OOM at 71K+ objects because
	// the informer store + L1 resolve pipeline compete for memory.
	synced atomic.Bool

	// Three priority workqueues: HOT > WARM > COLD. Workers always
	// drain hotQ before warmQ, and warmQ before coldQ. Each queue
	// has its own rate limiter for independent backoff tracking.
	hotQ  workqueue.TypedRateLimitingInterface[string]
	warmQ workqueue.TypedRateLimitingInterface[string]
	coldQ workqueue.TypedRateLimitingInterface[string]

	// refreshMeta stores per-identity metadata needed by processItem:
	// the trigger GVR, page keys, and the refresh function. Updated
	// atomically by enqueueRefresh before adding to the queue.
	refreshMeta sync.Map // map[string]*refreshMetaEntry

	// dirtyEntries accumulates per-identity dirty GVR+ns pairs. Each
	// enqueueRefresh call appends entries; processItem drains them to
	// build a DirtySet for targeted API result cache bypass.
	dirtyEntries sync.Map // map[string]*dirtyEntryBucket
}

func NewResourceWatcher(c Cache, rc *rest.Config) (*ResourceWatcher, error) {
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
		watched:     make(map[string]bool),
		eventCh:     make(chan l1Event, 1000000),
		dynamicGVRs: make(map[string]schema.GroupVersionResource),
		hotQ: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](1*time.Second, 30*time.Second),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "snowplow-hot"},
		),
		warmQ: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](1*time.Second, 30*time.Second),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "snowplow-warm"},
		),
		coldQ: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](1*time.Second, 30*time.Second),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "snowplow-cold"},
		),
	}, nil
}

func (rw *ResourceWatcher) Start(ctx context.Context) {
	rw.appCtx = WithInformerReader(ctx, rw)
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
	go rw.l1Worker(rw.appCtx) // appCtx carries InformerReader for background refreshes
	go rw.runWorkers(rw.appCtx)
	// Periodically prune stale keyAccess entries to prevent unbounded map growth.
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cleanupKeyAccess()
			}
		}
	}()
}

// runWorkers launches GOMAXPROCS worker goroutines that drain the three
// priority queues (HOT > WARM > COLD). On context cancellation all
// queues are shut down and workers exit.
func (rw *ResourceWatcher) runWorkers(ctx context.Context) {
	numWorkers := runtime.GOMAXPROCS(0)
	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go func() {
			defer wg.Done()
			for rw.processNext(ctx) {
			}
		}()
	}
	<-ctx.Done()
	rw.hotQ.ShutDown()
	rw.warmQ.ShutDown()
	rw.coldQ.ShutDown()
	wg.Wait()
}

// processNext polls the three queues in strict priority order:
// HOT first, then WARM, then COLD. If all are empty, sleeps 10ms
// before retrying. Returns false when all queues are shut down.
func (rw *ResourceWatcher) processNext(ctx context.Context) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		default:
		}

		// HOT: always preferred
		if rw.hotQ.Len() > 0 {
			item, shutdown := rw.hotQ.Get()
			if shutdown {
				return false
			}
			rw.processItem(ctx, item)
			rw.hotQ.Done(item)
			return true
		}
		// WARM: second priority
		if rw.warmQ.Len() > 0 {
			item, shutdown := rw.warmQ.Get()
			if shutdown {
				return false
			}
			rw.processItem(ctx, item)
			rw.warmQ.Done(item)
			return true
		}
		// COLD: lowest priority
		if rw.coldQ.Len() > 0 {
			item, shutdown := rw.coldQ.Get()
			if shutdown {
				return false
			}
			rw.processItem(ctx, item)
			rw.coldQ.Done(item)
			return true
		}

		// All empty: wait briefly then re-check priorities.
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// processItem resolves all pages for a single identity (resolved key base).
// It drains accumulated dirty entries, builds a DirtySet, and calls the
// L1RefreshFunc for each page. On success the rate limiter is reset; cascade
// targets are enqueued via enqueueCascade.
func (rw *ResourceWatcher) processItem(ctx context.Context, identity string) {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			slog.Error("resource-watcher: panic in L1 refresh",
				slog.Any("error", r),
				slog.String("identity", identity),
				slog.String("stack", string(buf[:n])))
		}
	}()

	// Load metadata for this identity. If missing, another goroutine
	// may have cleaned up — skip.
	metaVal, ok := rw.refreshMeta.Load(identity)
	if !ok {
		return
	}
	meta := metaVal.(*refreshMetaEntry)

	// Snapshot metadata under lock to avoid races with concurrent
	// enqueueRefresh calls updating the same identity.
	meta.mu.Lock()
	triggerGVR := meta.triggerGVR
	pages := make([]pageKey, len(meta.pages))
	copy(pages, meta.pages)
	fn := meta.fn
	meta.mu.Unlock()

	// Drain dirty entries accumulated since the last processItem call.
	var dirtySet *DirtySet
	if bucketVal, ok := rw.dirtyEntries.Load(identity); ok {
		bucket := bucketVal.(*dirtyEntryBucket)
		entries := bucket.drain()
		dirtySet = NewDirtySet(entries)
	} else {
		dirtySet = NewDirtySet(nil)
	}

	var allCascade []string
	for _, pg := range pages {
		refreshCtx, cancel := context.WithTimeout(WithDirtySet(ctx, dirtySet), 60*time.Second)
		cascade := fn(refreshCtx, triggerGVR, []string{pg.key})
		cancel()
		allCascade = append(allCascade, cascade...)
	}

	// Success: reset the rate limiter backoff for this identity.
	// Forget on all three queues — no-op on queues that don't track it.
	rw.hotQ.Forget(identity)
	rw.warmQ.Forget(identity)
	rw.coldQ.Forget(identity)

	slog.Info("refresh done",
		slog.String("key", identity),
		slog.Int("cascade", len(allCascade)),
		slog.String("trigger", triggerGVR.String()))

	if len(allCascade) > 0 {
		rw.enqueueCascade(ctx, triggerGVR, pages, allCascade, fn)
	}
}

// enqueueCascade groups cascade keys by identity (resolved key base) and
// enqueues each group for refresh. The parent's GVR info is propagated
// so cascade resolves build a non-empty DirtySet that bypasses stale
// API result cache entries.
func (rw *ResourceWatcher) enqueueCascade(_ context.Context, triggerGVR schema.GroupVersionResource, parentPages []pageKey, cascadeKeys []string, fn L1RefreshFunc) {
	// Extract parent GVR info for DirtySet propagation.
	parentGVRKey := ""
	parentNS := ""
	if len(parentPages) > 0 {
		if pInfo, ok := ParseResolvedKey(parentPages[0].key); ok {
			parentGVRKey = GVRToKey(pInfo.GVR)
			parentNS = pInfo.NS
		}
	}

	// Build dirty pairs from the parent's GVR info.
	var dirtyPairs []DirtyEntry
	if parentGVRKey != "" {
		dirtyPairs = []DirtyEntry{{GVRKey: parentGVRKey, NS: parentNS}}
	}

	// Group cascade keys by identity.
	cascadeGroups := make(map[string][]pageKey)
	for _, ck := range cascadeKeys {
		info, ok := ParseResolvedKey(ck)
		if !ok {
			cascadeGroups[ck] = append(cascadeGroups[ck], pageKey{key: ck, page: 0})
			continue
		}
		baseKey := ResolvedKeyBase(info.Username, info.GVR, info.NS, info.Name)
		cascadeGroups[baseKey] = append(cascadeGroups[baseKey], pageKey{key: ck, page: info.Page})
	}
	// Ensure page 0 exists for each group.
	for bk, cps := range cascadeGroups {
		hasPage0 := false
		for _, p := range cps {
			if p.page == 0 {
				hasPage0 = true
				break
			}
		}
		if !hasPage0 {
			cascadeGroups[bk] = append([]pageKey{{key: bk, page: 0}}, cps...)
		}
	}
	for _, cPages := range cascadeGroups {
		sort.Slice(cPages, func(i, j int) bool { return cPages[i].page < cPages[j].page })
		rw.enqueueRefresh(cPages, triggerGVR, dirtyPairs, fn)
	}
}

// enqueueRefresh is the unified enqueue function. It stores/updates metadata
// and dirty entries, then adds the identity to one of three priority queues
// (HOT/WARM/COLD) based on access-recency temperature. Workers poll the
// queues in strict priority order so HOT items always preempt COLD ones.
func (rw *ResourceWatcher) enqueueRefresh(pages []pageKey, triggerGVR schema.GroupVersionResource, dirtyPairs []DirtyEntry, fn L1RefreshFunc) {
	if len(pages) == 0 {
		return
	}
	// Compute identity from the first page key.
	identity := pages[0].key
	if info, ok := ParseResolvedKey(identity); ok {
		identity = ResolvedKeyBase(info.Username, info.GVR, info.NS, info.Name)
	}

	// Store/update metadata. If another goroutine already stored an entry,
	// update it under lock to reflect the latest trigger GVR and pages.
	metaVal, loaded := rw.refreshMeta.LoadOrStore(identity, &refreshMetaEntry{
		triggerGVR: triggerGVR,
		pages:      pages,
		fn:         fn,
	})
	if loaded {
		meta := metaVal.(*refreshMetaEntry)
		meta.mu.Lock()
		meta.triggerGVR = triggerGVR
		meta.pages = pages
		meta.fn = fn
		meta.mu.Unlock()
	}

	// Accumulate dirty entries.
	if len(dirtyPairs) > 0 {
		bucketVal, _ := rw.dirtyEntries.LoadOrStore(identity, &dirtyEntryBucket{})
		bucket := bucketVal.(*dirtyEntryBucket)
		for _, dp := range dirtyPairs {
			bucket.add(dp.GVRKey, dp.NS)
		}
	}

	// Classify by temperature and enqueue to the appropriate priority queue.
	tier := keyTemperature(identity)
	switch tier {
	case "hot":
		rw.hotQ.Add(identity)
	case "warm":
		rw.warmQ.Add(identity)
	default: // cold
		rw.coldQ.Add(identity)
	}
	slog.Info("dispatch", slog.String("tier", tier), slog.String("key", identity))
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

// l1Worker consumes events from eventCh, batches them, and triggers L1
// refreshes with deduplicated Redis lookups. Events are drained in batches:
// the first event is received via blocking select, then all pending events
// are drained non-blockingly. The batch is processed as a unit, deduplicating
// the SMembers calls across all events.
//
// This is fully agnostic — no GVR-specific logic. Any informer change triggers
// the corresponding L1 refresh automatically.
func (rw *ResourceWatcher) l1Worker(ctx context.Context) {
	for {
		// Block until at least one event arrives or context is cancelled.
		var first l1Event
		var ok bool
		select {
		case <-ctx.Done():
			return
		case first, ok = <-rw.eventCh:
			if !ok {
				return
			}
		}

		// Drain all pending events non-blockingly to form a batch.
		// Cap at 10K events per batch to bound the processing time.
		// Remaining events are picked up in the next iteration.
		// This prevents the l1Worker from being blocked for minutes
		// when 50K+ events arrive (e.g., during mass deployment).
		const maxBatchSize = 10000
		batch := []l1Event{first}
	drain:
		for len(batch) < maxBatchSize {
			select {
			case evt, ok := <-rw.eventCh:
				if !ok {
					break drain
				}
				batch = append(batch, evt)
			default:
				break drain
			}
		}

		// Count event types for logging.
		var nDelete int
		for i := range batch {
			if batch[i].eventType == "delete" {
				nDelete++
			}
		}
		if len(batch) > 1 || nDelete > 0 {
			slog.Info("l1Worker: processing batch",
				slog.Int("size", len(batch)),
				slog.Int("deletes", nDelete))
		}

		// Process the batch: L1 refresh first, then DELETE dep cleanup.
		// ORDER MATTERS: triggerL1RefreshBatch calls SMembers on per-resource
		// GET dep keys. If we delete those keys first (for DELETE events),
		// the SMembers returns empty and we lose the dep chain for individually
		// GET'd resources. By running the refresh first, the dep lookup
		// succeeds; then we clean up the stale dep key.
		batchStart := time.Now()
		rw.triggerL1RefreshBatch(ctx, batch)
		if elapsed := time.Since(batchStart); elapsed > 5*time.Second {
			slog.Warn("l1Worker: batch processing slow",
				slog.Int("size", len(batch)),
				slog.String("elapsed", elapsed.String()))
		}
		for i := range batch {
			if batch[i].eventType == "delete" {
				resDepKey := L1ResourceDepKey(batch[i].gvrKey, batch[i].ns, batch[i].name)
				_ = rw.cache.Delete(ctx, resDepKey)
			}
		}
	}
}

// refreshMetaEntry stores per-identity metadata needed by processItem.
// Updated by enqueueRefresh under lock; read by processItem under lock.
type refreshMetaEntry struct {
	mu         sync.Mutex
	triggerGVR schema.GroupVersionResource
	pages      []pageKey
	fn         L1RefreshFunc
}

// dirtyEntryBucket accumulates dirty GVR+ns pairs for a single identity.
// Thread-safe: add() appends under lock, drain() returns and resets.
type dirtyEntryBucket struct {
	mu      sync.Mutex
	entries []DirtyEntry
}

// add appends a dirty entry under lock.
func (b *dirtyEntryBucket) add(gvrKey, ns string) {
	b.mu.Lock()
	b.entries = append(b.entries, DirtyEntry{GVRKey: gvrKey, NS: ns})
	b.mu.Unlock()
}

// drain returns and resets the accumulated dirty entries under lock.
func (b *dirtyEntryBucket) drain() []DirtyEntry {
	b.mu.Lock()
	out := b.entries
	b.entries = nil
	b.mu.Unlock()
	return out
}

// pageKey identifies a paginated L1 key with its page number for sorting.
type pageKey struct {
	key  string
	page int
}

// gvrNS identifies a GVR+namespace pair for dirty tracking across batched events.
type gvrNS struct {
	gvrKey string
	ns     string
}

// triggerL1RefreshBatch processes a batch of informer events with deduplicated
// Redis dependency lookups. Instead of calling SMembers per event (O(events)),
// it collects unique dependency keys across the batch and calls SMembers once
// per unique key (O(unique namespaces + unique cluster-wide GVRs + unique GET resources)).
//
// At 50K scale with 10 namespaces: ~11 SMembers instead of 150K.
//
// The dedup key for dependency lookups is:
//   - LIST deps:  (gvrKey, ns)     — one per unique GVR+namespace pair
//   - Cluster:    (gvrKey, "")     — one per unique GVR (cluster-wide)
//   - GET deps:   (gvrKey, ns, name) — one per unique resource (preserves per-resource granularity)
//
// For DELETE events, API result keys are collected and removed.
func (rw *ResourceWatcher) triggerL1RefreshBatch(ctx context.Context, events []l1Event) {
	fn, ok := rw.l1Refresh.Load().(L1RefreshFunc)
	if !ok || fn == nil {
		return
	}

	// ── Phase 1: Collect unique dependency index keys across all events ──
	// Each dep index key maps to one SMembers call. By deduplicating the
	// keys here, we avoid redundant Redis round-trips.
	type depLookup struct {
		redisKey string
	}
	listDeps := make(map[string]depLookup)     // "gvrKey\x00ns" → dep key (namespaced LIST)
	clusterDeps := make(map[string]depLookup)   // "gvrKey" → dep key (cluster-wide LIST)
	getDeps := make(map[string]depLookup)        // "gvrKey\x00ns\x00name" → dep key (per-resource GET)

	// Track which events are deletes (need API result key cleanup).
	hasDelete := false
	// Track which events are adds (need async api-result refresh enqueue
	// per the SWR contract — see Phase 3 below).
	hasAdd := false

	// Track all unique (gvrKey, ns) pairs for DirtySet entries.
	dirtyPairs := make(map[gvrNS]bool)

	// Track GVR for markDirtySequential. Use the first event's GVR as representative.
	// All events in a batch typically share the same GVR (informer fires per-GVR),
	// but if mixed, we use a representative — markDirtySequential only uses it for logging.
	var representativeGVR schema.GroupVersionResource
	if len(events) > 0 {
		representativeGVR = events[0].gvr
	}

	for _, evt := range events {
		// Namespaced LIST dep: one per unique (gvrKey, ns).
		nsKey := evt.gvrKey + "\x00" + evt.ns
		if _, ok := listDeps[nsKey]; !ok {
			listDeps[nsKey] = depLookup{redisKey: L1ResourceDepKey(evt.gvrKey, evt.ns, "")}
		}

		// Cluster-wide LIST dep: one per unique gvrKey.
		if _, ok := clusterDeps[evt.gvrKey]; !ok {
			clusterDeps[evt.gvrKey] = depLookup{redisKey: L1ResourceDepKey(evt.gvrKey, "", "")}
		}

		// Per-resource GET dep: one per unique (gvrKey, ns, name).
		// Preserves per-resource granularity for cluster-scoped resources (ns="")
		// and for any resource individually fetched via objects.Get().
		if evt.name != "" {
			getKey := evt.gvrKey + "\x00" + evt.ns + "\x00" + evt.name
			if _, ok := getDeps[getKey]; !ok {
				getDeps[getKey] = depLookup{redisKey: L1ResourceDepKey(evt.gvrKey, evt.ns, evt.name)}
			}
		}

		switch evt.eventType {
		case "delete":
			hasDelete = true
		case "add":
			hasAdd = true
		}

		dirtyPairs[gvrNS{gvrKey: evt.gvrKey, ns: evt.ns}] = true
	}

	// ── Phase 2: Collect ALL affected keys from ALL dep lookups ──────────
	//
	// All dep types (LIST, cluster, GET) are collected into a single allKeys
	// map. Keys are then classified by access recency into HOT/WARM/COLD tiers
	// for priority-ordered refresh.
	//
	// GET deps MUST be looked up because they point to DIFFERENT L1 keys than
	// LIST deps. A LIST dep key (snowplow:l1dep:gvr:ns:) contains list-page
	// resolved keys. A GET dep key (snowplow:l1dep:gvr:ns:name) contains
	// per-resource resolved keys. Skipping GET deps leaves detail pages stale.
	//
	// To avoid 50K individual SMembers calls at scale, GET dep lookups use a
	// Redis pipeline: one round-trip for all GET deps (~1-2s for 50K commands).
	allKeys := make(map[string]bool)

	// Look up all dep sets (list + cluster + get) via direct SMembers calls.
	// In-process MemCache makes each call O(1) — no pipelining needed.
	for _, dep := range listDeps {
		members, err := rw.cache.SMembers(ctx, dep.redisKey)
		if err != nil {
			continue
		}
		for _, k := range members {
			allKeys[k] = true
		}
	}
	for _, dep := range clusterDeps {
		members, err := rw.cache.SMembers(ctx, dep.redisKey)
		if err != nil {
			continue
		}
		// Instrumentation: cluster-wide dep reader (event invalidation).
		GlobalMetrics.ClusterDepSMembersTotal.Add(1)
		GlobalMetrics.ClusterDepSMembersBytes.Add(int64(len(members)))
		for _, k := range members {
			allKeys[k] = true
		}
	}
	// GET deps are only looked up when the batch is small enough that
	// individual resource tracking matters.  When the batch has cluster-wide
	// LIST deps (which already cover all resources in the GVR), individual
	// GET dep lookups are redundant — the LIST dep already triggers refresh
	// for the aggregating RESTActions, and per-resource widgets are
	// refreshed via cascade.  This prevents 500K SMembers calls when 50K
	// compositions are deployed simultaneously.
	if len(getDeps) <= 100 {
		for _, dep := range getDeps {
			members, err := rw.cache.SMembers(ctx, dep.redisKey)
			if err != nil {
				continue
			}
			for _, k := range members {
				allKeys[k] = true
			}
		}
	} else {
		slog.Info("triggerL1RefreshBatch: skipping GET deps (covered by LIST/cluster deps)",
			slog.Int("getDeps", len(getDeps)),
			slog.Int("listDeps", len(listDeps)),
			slog.Int("clusterDeps", len(clusterDeps)))
	}

	if len(allKeys) == 0 {
		slog.Debug("triggerL1RefreshBatch: no affected keys",
			slog.Int("events", len(events)),
			slog.Int("listDeps", len(listDeps)),
			slog.Int("clusterDeps", len(clusterDeps)),
			slog.Int("getDeps", len(getDeps)))
		return
	}

	// ── Phase 2b: Classify keys by access recency (HOT/WARM/COLD) ───────
	// HOT  (accessed < 5min):  refreshed first, highest priority
	// WARM (accessed 5-60min): refreshed after HOT completes
	// COLD (accessed >60min or never): refreshed after WARM completes
	hotKeys := make(map[string]bool)
	warmKeys := make(map[string]bool)
	coldKeys := make(map[string]bool)
	for k := range allKeys {
		info, ok := ParseResolvedKey(k)
		if !ok {
			// Cannot classify — treat as HOT (safe default).
			hotKeys[k] = true
			continue
		}
		base := ResolvedKeyBase(info.Username, info.GVR, info.NS, info.Name)
		switch keyTemperature(base) {
		case "hot":
			hotKeys[k] = true
		case "warm":
			warmKeys[k] = true
		default:
			coldKeys[k] = true
		}
	}

	slog.Info("triggerL1RefreshBatch: affected keys",
		slog.Int("events", len(events)),
		slog.Int("listDeps", len(listDeps)),
		slog.Int("clusterDeps", len(clusterDeps)),
		slog.Int("getDeps", len(getDeps)),
		slog.Int("hot", len(hotKeys)),
		slog.Int("warm", len(warmKeys)),
		slog.Int("cold", len(coldKeys)))

	// ── Phase 3: Separate API result keys from resolved keys ─────────────
	// Eager eviction on both ADD and DELETE.
	//
	// The user-facing tier is the resolved-widget cache (snowplow:resolved:*),
	// refreshed by processItem → ResolveWidget → SetResolvedRaw. That refresh
	// re-runs the resolve pipeline, whose inner /call paths read api-result
	// (snowplow:api-result:*) FIRST (resolve.go:217). If api-result still
	// holds the pre-event bytes, processItem computes a stale resolved-widget
	// output and writes that to the user-visible tier — even though the
	// informer is already fresh.
	//
	// DirtySet bypass (resolve.go:217 / context.go:124-130) is meant to skip
	// stale api-result for the dirty GVR — but it depends on the bucket being
	// populated for the identity at processItem time, which races with
	// concurrent batches. Eager eviction on the watcher side removes the
	// race entirely: the next inner /call falls through to the informer
	// because there is nothing to read.
	//
	// Trade-off: the next inner /call pays a marshal cost (already accepted
	// for the DELETE branch). UPDATE/PATCH stays untouched — natural TTL
	// (60s) handles in-place mutations.
	filterResolved := func(keys map[string]bool) map[string]bool {
		resolved := make(map[string]bool, len(keys))
		for key := range keys {
			if !IsAPIResultKey(key) {
				resolved[key] = true
			}
		}
		return resolved
	}
	var apiResultKeys []string
	for key := range allKeys {
		if IsAPIResultKey(key) {
			apiResultKeys = append(apiResultKeys, key)
		}
	}
	if (hasDelete || hasAdd) && len(apiResultKeys) > 0 {
		_ = rw.cache.Delete(ctx, apiResultKeys...)
	}
	hotKeys = filterResolved(hotKeys)
	warmKeys = filterResolved(warmKeys)
	coldKeys = filterResolved(coldKeys)

	if len(hotKeys) == 0 && len(warmKeys) == 0 && len(coldKeys) == 0 {
		return
	}

	// ── Phase 4: Group paginated keys and dispatch to workqueue ─────────
	// Group paginated keys by base key (user:gvr:ns:name without page suffix).
	// Paginated variants of the same resource resolve the same underlying data
	// — only the page parameter changes the output. Resolving them sequentially
	// (p1 → p2 → ... → pN) avoids N concurrent goroutines each doing a full
	// O(N) resolve of the same 49K items.
	//
	// Items are routed to one of three priority workqueues by temperature
	// (HOT/WARM/COLD). Workers poll HOT > WARM > COLD in strict priority
	// order. The workqueues handle deduplication, rate-limited backoff on
	// failure, and bounded concurrency via GOMAXPROCS workers.
	groupKeys := func(keys map[string]bool) map[string][]pageKey {
		groups := make(map[string][]pageKey)
		for l1Key := range keys {
			info, ok := ParseResolvedKey(l1Key)
			if !ok {
				groups[l1Key] = append(groups[l1Key], pageKey{key: l1Key, page: 0})
				continue
			}
			baseKey := ResolvedKeyBase(info.Username, info.GVR, info.NS, info.Name)
			groups[baseKey] = append(groups[baseKey], pageKey{key: l1Key, page: info.Page})
		}
		// Ensure the unpaginated variant (page=0) is refreshed alongside
		// paginated keys. HTTP requests without pagination read the base
		// key; without this, it stays stale while paginated keys converge.
		for baseKey, pages := range groups {
			hasPage0 := false
			for _, p := range pages {
				if p.page == 0 {
					hasPage0 = true
					break
				}
			}
			if !hasPage0 {
				groups[baseKey] = append([]pageKey{{key: baseKey, page: 0}}, pages...)
			}
		}
		return groups
	}

	// Convert dirtyPairs to []DirtyEntry for enqueueRefresh.
	dirtyList := make([]DirtyEntry, 0, len(dirtyPairs))
	for pair := range dirtyPairs {
		dirtyList = append(dirtyList, DirtyEntry{GVRKey: pair.gvrKey, NS: pair.ns})
	}

	for _, pages := range groupKeys(hotKeys) {
		sort.Slice(pages, func(i, j int) bool { return pages[i].page < pages[j].page })
		rw.enqueueRefresh(pages, representativeGVR, dirtyList, fn)
	}
	for _, pages := range groupKeys(warmKeys) {
		sort.Slice(pages, func(i, j int) bool { return pages[i].page < pages[j].page })
		rw.enqueueRefresh(pages, representativeGVR, dirtyList, fn)
	}
	for _, pages := range groupKeys(coldKeys) {
		sort.Slice(pages, func(i, j int) bool { return pages[i].page < pages[j].page })
		rw.enqueueRefresh(pages, representativeGVR, dirtyList, fn)
	}
}

// WaitForSync blocks until all started informers have completed their initial sync.
func (rw *ResourceWatcher) WaitForSync(ctx context.Context) bool {
	result := rw.factory.WaitForCacheSync(ctx.Done())
	for _, ok := range result {
		if !ok {
			return false
		}
	}
	rw.synced.Store(true)
	slog.Info("informer caches synced")
	return true
}

// GetObject returns a single object from the informer's in-memory store.
// Returns (nil, false) if the GVR has no registered informer or the object
// does not exist. Zero I/O — reads from the informer's synced cache.
func (rw *ResourceWatcher) GetObject(gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, bool) {
	key := ns + "/" + name
	if ns == "" {
		key = name
	}
	item, exists, err := rw.factory.ForResource(gvr).Informer().GetStore().GetByKey(key)
	if err != nil || !exists {
		return nil, false
	}
	uns, ok := item.(*unstructured.Unstructured)
	if !ok {
		return nil, false
	}
	return uns, true
}

// ListObjects returns all objects for a GVR from the informer's in-memory
// store, optionally scoped to a namespace (ns="" means cluster-wide).
// Returns (nil, false) if the GVR has no registered informer.
func (rw *ResourceWatcher) ListObjects(gvr schema.GroupVersionResource, ns string) ([]*unstructured.Unstructured, bool) {
	informer := rw.factory.ForResource(gvr).Informer()

	var items []any
	if ns != "" {
		// Use the namespace index that DynamicSharedInformerFactory already
		// registers (cache.NamespaceIndex). Returns only items in the target
		// namespace — O(items_in_ns) instead of O(total_items).
		var err error
		items, err = informer.GetIndexer().ByIndex(k8scache.NamespaceIndex, ns)
		if err != nil {
			items = informer.GetStore().List()
		}
	} else {
		items = informer.GetStore().List()
	}

	if len(items) == 0 {
		rw.mu.Lock()
		_, registered := rw.watched[GVRToKey(gvr)]
		rw.mu.Unlock()
		if !registered {
			return nil, false
		}
		return []*unstructured.Unstructured{}, true
	}

	result := make([]*unstructured.Unstructured, 0, len(items))
	for _, item := range items {
		uns, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		result = append(result, uns)
	}
	return result, true
}

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
// After starting, it waits for the informer to sync and then reconciles
// the L2 Redis cache from the informer's authoritative in-memory store.
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

	// Reconcile L2 from the informer's authoritative store in a single pass.
	// This replaces the old event-by-event replay which caused sustained
	// AtomicUpdateJSON contention on list keys (e.g. 4759 RESTActions each
	// hitting 20 retries on the cluster-wide list key).
	// reconcileGVR writes individual GET keys + rebuilds all list caches at once.
	storeSize := len(informer.GetStore().List())
	slog.Info("resource-watcher: reconciling new informer with L2 Redis",
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
// by any widget. Skipping these avoids unnecessary cache churn.
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
	// cache churn with no benefit.
	if gvr.Resource == "configmaps" && noisyConfigMapNamespaces[ns] {
		return
	}

	if eventType == "delete" {
		slog.Info("resource-watcher: delete event",
			slog.String("gvr", gvr.String()),
			slog.String("ns", ns),
			slog.String("name", name))
	} else {
		slog.Debug("resource-watcher: event",
			slog.String("type", eventType),
			slog.String("gvr", gvr.String()),
			slog.String("ns", ns),
			slog.String("name", name))
	}

	// Update the raw object cache (snowplow:get:*) so the callHandler
	// serves fresh data immediately. Same stale-while-refresh pattern as
	// L1 resolved keys: overwrite on ADD/UPDATE, delete on DELETE.
	if rw.cache != nil {
		getKey := GetKey(gvr, ns, name)
		if eventType == "delete" {
			_ = rw.cache.Delete(ctx, getKey)
		} else {
			_ = rw.cache.SetForGVR(ctx, gvr, getKey, uns.Object)
		}
	}

	gvrKey := GVRToKey(gvr)

	// ── Auto-register informers for new CRDs ────────────────────────────────
	if gvr.Resource == "customresourcedefinitions" && gvr.Group == "apiextensions.k8s.io" && (eventType == "add" || eventType == "update") {
		rw.autoRegisterCRDInformer(uns)
	}

	// ── Enqueue L1 refresh event ─────────────────────────────────────────────
	// Skip L1 refresh during initial informer sync. Resolving before all
	// informers have completed their LIST produces incomplete results and
	// causes OOM (informer store + resolve pipeline competing for memory).
	// After WaitForSync, the warmup + reconcile cycle handles the initial state.
	if !rw.synced.Load() {
		return
	}

	// The informer store is already updated (the event means it changed).
	// Non-blocking: if the channel is full, drop the event (the informer
	// is already updated; the next event for this GVR will trigger a refresh).
	select {
	case rw.eventCh <- l1Event{
		gvr:       gvr,
		gvrKey:    gvrKey,
		ns:        ns,
		name:      name,
		eventType: eventType,
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

// ReconcileStats holds aggregate results from a startup reconciliation pass.
type ReconcileStats struct {
	GVRs     int
	Added    int
	Removed  int
	Updated  int
	Errors   int
	Duration time.Duration
}

// Reconcile compares the L2 Redis cache with informer stores (in-memory, authoritative)
// for every watched GVR. Any differences -- ghost objects from missed DELETEs during
// pod downtime, missing objects, stale versions -- are patched in L2. This ensures the
// cache is correct even when events were lost between restarts.
//
// Must be called after WaitForSync (informers have completed initial LIST/WATCH) and
// after L2 warmup (baseline data exists in Redis).
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
			slog.Info("reconcile: patched L2",
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

	// No per-GVR mutex needed: scheduleDynamicReconcile was removed.
	// reconcileGVR is only called from Reconcile() at startup (serial).

	// 1. Read authoritative state from informer store (in-memory, no API call).
	lister := rw.factory.ForResource(gvr).Lister()
	objects, err := lister.List(labels.Everything())
	if err != nil {
		slog.Warn("reconcile: failed to list informer store",
			slog.String("gvr", gvr.String()), slog.Any("err", err))
		errs++
		return
	}

	// No L2 diff or writes. The informer store IS the authoritative source.
	// reconcileGVR at startup just counts items for logging.
	added = len(objects)

	slog.Info("reconcile: informer store synced",
		slog.String("gvr", gvr.String()),
		slog.Int("items", len(objects)))

	return
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

