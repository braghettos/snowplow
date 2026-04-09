package dispatchers

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	// fullRefreshInterval controls how many consecutive incremental patches
	// are allowed before forcing a full re-resolve to correct any drift.
	fullRefreshInterval = 10
)

// patchRESTActionL1 attempts an incremental JSON patch on an existing L1 value
// for a RESTAction key. It handles DELETE operations by removing items from the
// cached JSON output without re-resolving the entire RESTAction.
//
// Returns true if the patch was applied successfully. Returns false if the
// patch is not applicable (not a RESTAction, no existing L1 value, unsupported
// operation, etc.) — in which case the caller should fall back to full resolution.
//
// The patch operates on the resolved RESTAction JSON structure:
//
//	{
//	  "status": {
//	    "items": [ {..., "metadata": {"namespace": "ns", "name": "foo"}, ...}, ... ],
//	    "total": 5000,
//	    ...
//	  }
//	}
//
// For DELETE: finds and removes items matching the change's namespace+name,
// decrements the total count, and writes back to L1.
func patchRESTActionL1(ctx context.Context, c *cache.RedisCache, l1Key string, changes []cache.L1ChangeInfo) bool {
	ctx, span := l1RefreshTracer.Start(ctx, "l1.patch.restaction",
		trace.WithAttributes(
			attribute.String("l1_key", l1Key),
			attribute.Int("changes", len(changes)),
		))
	defer span.End()

	start := time.Now()
	log := slog.Default()

	// Read existing L1 value.
	raw, hit, err := c.GetRaw(ctx, l1Key)
	if err != nil || !hit || len(raw) == 0 {
		log.Debug("L1 patch: skip - no existing L1", slog.String("key", l1Key))
		return false
	}

	// Parse the top-level JSON to extract the status field.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		log.Info("L1 patch: skip - envelope parse error", slog.String("key", l1Key), slog.Any("err", err))
		return false
	}
	statusRaw, ok := envelope["status"]
	if !ok || len(statusRaw) == 0 {
		// Log available top-level keys for debugging
		keys := make([]string, 0, len(envelope))
		for k := range envelope {
			keys = append(keys, k)
		}
		log.Info("L1 patch: skip - no status field", slog.String("key", l1Key), slog.Any("keys", keys))
		return false
	}

	// Parse status as a generic JSON value. It may be an object with "items"
	// (list-type RESTAction) or something else entirely.
	var status map[string]json.RawMessage
	if err := json.Unmarshal(statusRaw, &status); err != nil {
		log.Info("L1 patch: skip - status not object", slog.String("key", l1Key))
		return false
	}

	// Look for the array field — RESTActions use "items" or "list" depending
	// on the JQ filter output.
	itemsFieldName := "items"
	itemsRaw, hasItems := status["items"]
	if !hasItems || len(itemsRaw) == 0 {
		itemsRaw, hasItems = status["list"]
		itemsFieldName = "list"
	}
	if !hasItems || len(itemsRaw) == 0 {
		statusKeys := make([]string, 0, len(status))
		for k := range status {
			statusKeys = append(statusKeys, k)
		}
		log.Info("L1 patch: skip - no items in status", slog.String("key", l1Key), slog.Any("statusKeys", statusKeys))
		return false
	}

	// Parse items array. Each item is a K8s-like object with metadata.namespace
	// and metadata.name, OR a JQ-transformed object with "namespace" and "name"
	// at the top level or in a "metadata" sub-object.
	var items []json.RawMessage
	if err := json.Unmarshal(itemsRaw, &items); err != nil {
		return false
	}

	// Build a set of items to delete (namespace/name pairs).
	type nsName struct{ ns, name string }
	toDelete := make(map[nsName]bool)
	for _, ch := range changes {
		if ch.Operation == "delete" {
			toDelete[nsName{ch.Namespace, ch.Name}] = true
		}
	}
	if len(toDelete) == 0 {
		// No deletes — incremental patch not applicable for add/update.
		span.AddEvent("patch.skip", trace.WithAttributes(
			attribute.String("reason", "no_deletes"),
		))
		return false
	}

	// Filter items, removing deleted ones.
	originalLen := len(items)
	filtered := make([]json.RawMessage, 0, len(items))
	removed := 0
	for _, item := range items {
		ns, name := extractItemIdentity(item)
		if ns == "" && name == "" {
			// Can't identify this item — keep it.
			filtered = append(filtered, item)
			continue
		}
		if toDelete[nsName{ns, name}] {
			removed++
			continue
		}
		filtered = append(filtered, item)
	}

	if removed == 0 {
		// No items matched the delete — the change may not be relevant to this
		// RESTAction. Return false so full resolution runs and handles it.
		span.AddEvent("patch.skip", trace.WithAttributes(
			attribute.String("reason", "no_items_matched"),
		))
		return false
	}

	// Re-serialize items.
	newItemsRaw, err := json.Marshal(filtered)
	if err != nil {
		return false
	}
	status[itemsFieldName] = newItemsRaw

	// Update total count if present.
	if totalRaw, ok := status["total"]; ok {
		var total float64
		if err := json.Unmarshal(totalRaw, &total); err == nil {
			newTotal := int(total) - removed
			if newTotal < 0 {
				newTotal = 0
			}
			status["total"] = json.RawMessage(jsonInt(newTotal))
		}
	}

	// Re-serialize status.
	newStatusRaw, err := json.Marshal(status)
	if err != nil {
		return false
	}
	envelope["status"] = newStatusRaw

	// Re-serialize the entire response.
	newRaw, err := json.Marshal(envelope)
	if err != nil {
		return false
	}

	// Write back to L1.
	if err := c.SetResolvedRaw(ctx, l1Key, newRaw); err != nil {
		return false
	}

	log.Info("L1 incremental patch applied",
		slog.String("key", l1Key),
		slog.Int("removed", removed),
		slog.Int("original", originalLen),
		slog.Int("remaining", len(filtered)),
		slog.String("duration", time.Since(start).String()))
	span.AddEvent("patch.applied", trace.WithAttributes(
		attribute.Int("removed", removed),
		attribute.Int("original", originalLen),
		attribute.Int("remaining", len(filtered)),
	))

	return true
}

// extractItemIdentity extracts the namespace and name from a JSON item.
// Tries multiple formats:
//  1. metadata.namespace / metadata.name (K8s-style)
//  2. Top-level "namespace" / "name" (JQ-transformed)
//  3. Fallback: scan for "name" field containing "/" (ns/name format)
func extractItemIdentity(item json.RawMessage) (ns, name string) {
	// Fast path: try to extract from a minimal parse.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(item, &obj); err != nil {
		return "", ""
	}

	// Try metadata.namespace + metadata.name (K8s-style objects).
	if metaRaw, ok := obj["metadata"]; ok {
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(metaRaw, &meta); err == nil {
			ns = unquote(meta["namespace"])
			name = unquote(meta["name"])
			if name != "" {
				return ns, name
			}
		}
	}

	// Try top-level namespace + name (JQ-transformed flat objects).
	ns = unquote(obj["namespace"])
	name = unquote(obj["name"])
	if name != "" {
		return ns, name
	}

	// Try "id" field containing "namespace/name" (some JQ outputs).
	if idRaw, ok := obj["id"]; ok {
		id := unquote(idRaw)
		if parts := strings.SplitN(id, "/", 2); len(parts) == 2 {
			return parts[0], parts[1]
		}
	}

	return "", ""
}

// unquote extracts a string value from a JSON-encoded string (removes quotes).
func unquote(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// jsonInt returns a JSON-encoded integer.
func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
