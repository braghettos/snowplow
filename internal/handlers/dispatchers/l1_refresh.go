package dispatchers

import (
	"context"
	"log/slog"

	"github.com/krateoplatformops/snowplow/internal/cache"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

var l1RefreshTracer = otel.Tracer("snowplow/l1refresh")

const (
	restactionResource = "restactions"
	templatesGroup     = "templates.krateo.io"
)

// MakeL1Refresher returns a cache.L1RefreshFunc that invalidates (deletes)
// affected L1 keys instead of eagerly re-resolving them. The next HTTP
// request for each key will miss L1 and trigger a fresh resolution via the
// singleflight path (lazy / stale-while-revalidate).
//
// This scales to 1000+ users: a composition change deletes N L1 keys in a
// single Redis batch DEL instead of spawning N expensive background
// resolutions (~5s each with RBAC + JWT + widget/restaction resolution).
func MakeL1Refresher(c *cache.RedisCache, rc *rest.Config, authnNS, signKey string) cache.L1RefreshFunc {
	// rc, authnNS, signKey are kept in the signature for API compatibility
	// but are no longer used — lazy invalidation needs only the cache.
	_ = rc
	_ = authnNS
	_ = signKey

	return func(ctx context.Context, triggerGVR schema.GroupVersionResource, l1Keys []string) {
		if len(l1Keys) == 0 {
			cache.MarkL1Ready(context.Background(), c)
			return
		}

		ctx, span := l1RefreshTracer.Start(ctx, "l1.invalidate",
			trace.WithAttributes(
				attribute.String("trigger", triggerGVR.String()),
				attribute.Int("keys", len(l1Keys)),
			),
		)
		defer span.End()

		log := slog.Default()
		log.Info("L1 invalidate: deleting stale keys (lazy refresh)",
			slog.String("trigger", triggerGVR.String()),
			slog.Int("keys", len(l1Keys)))

		// Also collect cascade dependents: for RESTAction keys, any L1 keys
		// that depend on them (e.g. widget depends on compositions-list) must
		// also be invalidated.
		allKeys := make([]string, 0, len(l1Keys)*2)
		allKeys = append(allKeys, l1Keys...)

		for _, key := range l1Keys {
			info, ok := cache.ParseResolvedKey(key)
			if !ok {
				continue
			}
			if info.GVR.Group == templatesGroup && info.GVR.Resource == restactionResource {
				depKey := cache.L1ResourceDepKey(
					cache.GVRToKey(info.GVR), info.NS, info.Name,
				)
				cascade, err := c.SMembers(ctx, depKey)
				if err == nil && len(cascade) > 0 {
					allKeys = append(allKeys, cascade...)
				}
			}
		}

		// Deduplicate keys before deletion.
		seen := make(map[string]struct{}, len(allKeys))
		deduped := allKeys[:0]
		for _, k := range allKeys {
			if _, ok := seen[k]; !ok {
				seen[k] = struct{}{}
				deduped = append(deduped, k)
			}
		}

		if err := c.Delete(ctx, deduped...); err != nil {
			log.Warn("L1 invalidate: failed to delete keys",
				slog.String("trigger", triggerGVR.String()),
				slog.Int("keys", len(deduped)),
				slog.Any("err", err))
		} else {
			log.Info("L1 invalidate: deleted",
				slog.String("trigger", triggerGVR.String()),
				slog.Int("deleted", len(deduped)))
		}

		// Use a fresh background context so the sentinel is always written,
		// even if the refresh context has expired.
		cache.MarkL1Ready(context.Background(), c)
	}
}
