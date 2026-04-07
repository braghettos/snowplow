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

// MakeL1Refresher returns a cache.L1RefreshFunc that marks affected L1 keys
// as stale instead of deleting them. The stale data continues to be served
// instantly by the HTTP handlers, which trigger a background re-resolution
// on the next request for that key (stale-while-revalidate).
//
// This scales to 1000+ users: a composition change marks N L1 keys stale in
// a single Redis pipeline SET instead of spawning N expensive background
// resolutions (~5s each with RBAC + JWT + widget/restaction resolution).
func MakeL1Refresher(c *cache.RedisCache, rc *rest.Config, authnNS, signKey string) cache.L1RefreshFunc {
	// rc, authnNS, signKey are kept in the signature for API compatibility
	// but are no longer used — stale marking needs only the cache.
	_ = rc
	_ = authnNS
	_ = signKey

	return func(ctx context.Context, triggerGVR schema.GroupVersionResource, l1Keys []string) {
		if len(l1Keys) == 0 {
			cache.MarkL1Ready(context.Background(), c)
			return
		}

		ctx, span := l1RefreshTracer.Start(ctx, "l1.mark_stale",
			trace.WithAttributes(
				attribute.String("trigger", triggerGVR.String()),
				attribute.Int("keys", len(l1Keys)),
			),
		)
		defer span.End()

		log := slog.Default()
		log.Info("L1 invalidate: marking keys stale (stale-while-revalidate)",
			slog.String("trigger", triggerGVR.String()),
			slog.Int("keys", len(l1Keys)))

		// Also collect cascade dependents: for RESTAction keys, any L1 keys
		// that depend on them (e.g. widget depends on compositions-list) must
		// also be marked stale.
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

		// Deduplicate keys before marking stale.
		seen := make(map[string]struct{}, len(allKeys))
		deduped := allKeys[:0]
		for _, k := range allKeys {
			if _, ok := seen[k]; !ok {
				seen[k] = struct{}{}
				deduped = append(deduped, k)
			}
		}

		if err := c.MarkStale(ctx, deduped...); err != nil {
			log.Warn("L1 invalidate: failed to mark keys stale",
				slog.String("trigger", triggerGVR.String()),
				slog.Int("keys", len(deduped)),
				slog.Any("err", err))
		} else {
			log.Info("L1 invalidate: marked stale",
				slog.String("trigger", triggerGVR.String()),
				slog.Int("marked", len(deduped)))
		}

		// Use a fresh background context so the sentinel is always written,
		// even if the refresh context has expired.
		cache.MarkL1Ready(context.Background(), c)
	}
}
