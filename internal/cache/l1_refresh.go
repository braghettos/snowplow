package cache

import (
	"context"
	"encoding/json"
	"log/slog"
)

// apiResultRefreshPrefix is a sentinel prepended to workqueue items that
// represent an api-result raw-bytes refresh job (as opposed to the default
// resolved-key refresh job handled by processItem). The leading NUL byte
// ensures it cannot collide with a legitimate resolved-key string, which
// always starts with "snowplow:resolved:".
const apiResultRefreshPrefix = "\x00ar:"

// enqueueAPIResultRefresh schedules background refresh of api-result keys
// after an ADD event. The job runs in the existing HOT priority queue and
// overwrites the cached bytes in place via SetAPIResultRaw — readers see
// the previous (stale) bytes immediately and the next (fresh) bytes after
// the worker completes. This is the SWR contract for ADD events.
//
// Coalescing: workqueue.Add is idempotent. A storm of N events affecting
// the same api-result key collapses to ~2 jobs (in-flight + latest queued).
func (rw *ResourceWatcher) enqueueAPIResultRefresh(keys []string) {
	for _, k := range keys {
		// Route to HOT — api-result refresh is fast (single marshal) and
		// freshness-critical. WARM/COLD routing only makes sense for
		// resolved-widget keys where we can read user temperature; for
		// raw bytes there is no associated user activity record.
		rw.hotQ.Add(apiResultRefreshPrefix + k)
	}
}

// refreshSingleAPIResult re-marshals the informer state for the GVR/ns/name
// encoded in apiKey and overwrites the api-result cache entry in place.
// This is the SWR-compliant background path for ADD events (see
// enqueueAPIResultRefresh).
//
// Cold-identity guard: if the api-result key is absent (TTL-evicted), we
// short-circuit. The next user read will lazy-load via the normal resolve
// path; we do not proactively warm cold identities.
func (rw *ResourceWatcher) refreshSingleAPIResult(ctx context.Context, apiKey string) {
	if rw == nil || rw.cache == nil {
		return
	}

	// 1. Defensive: skip if TTL-evicted (cold identity). Lazy-load on next read.
	if _, hit, _ := rw.cache.GetRaw(ctx, apiKey); !hit {
		return
	}

	// 2. Parse identity/GVR/ns/name from the key.
	_, gvr, ns, name, ok := ParseAPIResultKey(apiKey)
	if !ok {
		slog.Debug("refreshSingleAPIResult: unparseable key", slog.String("key", apiKey))
		return
	}

	// 3. Read from informer. Prefer the InformerReader from ctx (set by
	// Start via WithInformerReader) so tests can inject a stub; fall back
	// to the ResourceWatcher itself, which implements InformerReader.
	ir := InformerReaderFromContext(ctx)
	if ir == nil {
		ir = rw
	}
	var data any
	if name == "" {
		items, ok := ir.ListObjects(gvr, ns)
		if !ok {
			return
		}
		// Mirror resolve.go's wrapping: items must be []any of obj.Object.
		itemsList := make([]any, len(items))
		for i, item := range items {
			itemsList[i] = item.Object
		}
		data = map[string]any{
			"apiVersion": "v1",
			"kind":       "List",
			"metadata":   map[string]any{},
			"items":      itemsList,
		}
	} else {
		obj, ok := ir.GetObject(gvr, ns, name)
		if !ok {
			return
		}
		data = obj.Object
	}

	// 4. Marshal + atomic overwrite. SetAPIResultRaw is a sync.Map.Store —
	// readers between event-time and now see old bytes; readers after see
	// fresh bytes. No reader blocks on this work.
	raw, err := json.Marshal(data)
	if err != nil {
		slog.Warn("refreshSingleAPIResult: marshal failed",
			slog.String("key", apiKey),
			slog.Any("err", err))
		return
	}
	_ = rw.cache.SetAPIResultRaw(ctx, apiKey, raw)
}
