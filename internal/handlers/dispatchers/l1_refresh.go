package dispatchers

import (
	"context"
	"log/slog"
	"sync"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

var l1RefreshTracer = otel.Tracer("snowplow/l1refresh")

const (
	refreshConcurrency         = 20 // warmup and HTTP-triggered refreshes
	refreshConcurrencyBackground = 8  // l3gen scanner refreshes (fewer keys, less RBAC pressure)
	restactionResource         = "restactions"
	templatesGroup             = "templates.krateo.io"
)

// MakeL1Refresher returns a cache.L1RefreshFunc that re-resolves L1 keys in
// the background instead of deleting them. Old values keep being served while
// the refresh runs (stale-while-revalidate).
func MakeL1Refresher(c *cache.RedisCache, rc *rest.Config, authnNS, signKey string) cache.L1RefreshFunc {
	return func(ctx context.Context, triggerGVR schema.GroupVersionResource, l1Keys []string) {
		ctx, span := l1RefreshTracer.Start(ctx, "l1.refresh",
			trace.WithAttributes(
				attribute.String("trigger", triggerGVR.String()),
				attribute.Int("keys", len(l1Keys)),
			),
		)
		defer span.End()

		log := slog.Default()

		// Use lower concurrency for background l3gen scans (empty triggerGVR)
		// to reduce RBAC API pressure. Warmup uses full concurrency.
		concurrency := refreshConcurrency
		if triggerGVR.Resource == "" {
			concurrency = refreshConcurrencyBackground
		}

		log.Info("L1 refresh: starting",
			slog.String("trigger", triggerGVR.String()),
			slog.Int("keys", len(l1Keys)),
			slog.Int("concurrency", concurrency))

		type userKeys struct {
			info cache.ResolvedKeyInfo
			raw  string
		}
		byUser := map[string][]userKeys{}
		for _, key := range l1Keys {
			info, ok := cache.ParseResolvedKey(key)
			if !ok {
				continue
			}
			byUser[info.Username] = append(byUser[info.Username], userKeys{info: info, raw: key})
		}

		// Only refresh L1 for users with a valid -clientconfig secret
		// (i.e., users who logged in while their certificate is still valid).
		// authn removes the secret when the cert expires, so the active-users
		// set tracks exactly who should get eager refresh.
		// Users without a secret are skipped — their L1 expires via TTL and
		// gets re-resolved on next login.
		activeUsers, _ := c.SMembers(ctx, cache.ActiveUsersKey)
		activeSet := make(map[string]bool, len(activeUsers))
		for _, u := range activeUsers {
			activeSet[u] = true
		}
		skippedUsers := 0
		for username := range byUser {
			if !activeSet[username] {
				delete(byUser, username)
				skippedUsers++
			}
		}
		if skippedUsers > 0 {
			log.Info("L1 refresh: skipped inactive users",
				slog.Int("skipped", skippedUsers),
				slog.Int("active", len(byUser)))
		}

		var totalRefreshed int64
		for username, keys := range byUser {
			ep, err := endpoints.FromSecret(ctx, rc, username+clientConfigSecretSuffix, authnNS)
			if err != nil {
				log.Warn("L1 refresh: cannot load user endpoint",
					slog.String("user", username), slog.Any("err", err))
				continue
			}
			groups := extractGroupsFromClientCert(ep.ClientCertificateData)
			user := jwtutil.UserInfo{Username: username, Groups: groups}
			accessToken := mintJWT(user, signKey)

			var (
				wg           sync.WaitGroup
				sem          = make(chan struct{}, concurrency)
				mu           sync.Mutex
				n            int64
				allCascade   []string
			)
			for _, k := range keys {
				wg.Add(1)
				sem <- struct{}{}
			go func(info cache.ResolvedKeyInfo, rawKey string) {
				defer wg.Done()
				defer func() { <-sem }()
				ok, cascade := refreshSingleL1(ctx, c, user, ep, accessToken, info, rawKey, authnNS)
				if ok {
						mu.Lock()
						n++
						allCascade = append(allCascade, cascade...)
						mu.Unlock()
					}
				}(k.info, k.raw)
			}
			wg.Wait()
			totalRefreshed += n

			// Cascading refresh: iteratively re-resolve L1 keys that depend
			// on refreshed RESTActions. Each round may discover more dependents
			// (e.g. CRD event → compositions-get-ns-and-crd → compositions-list
			// → piechart). Limit depth to avoid infinite loops.
			refreshed := make(map[string]bool, len(keys))
			for _, k := range keys {
				refreshed[k.raw] = true
			}
			const maxCascadeDepth = 5
			pending := allCascade
			for depth := 0; depth < maxCascadeDepth && len(pending) > 0; depth++ {
				var nextCascade []string
				for _, ck := range pending {
					if refreshed[ck] {
						continue
					}
					refreshed[ck] = true
					ci, cok := cache.ParseResolvedKey(ck)
					if !cok || ci.Username != username {
						continue
					}
					wg.Add(1)
					sem <- struct{}{}
					go func(cInfo cache.ResolvedKeyInfo, cRawKey string) {
						defer wg.Done()
						defer func() { <-sem }()
						ok, cascade := refreshSingleL1(ctx, c, user, ep, accessToken, cInfo, cRawKey, authnNS)
						if ok {
							mu.Lock()
							totalRefreshed++
							nextCascade = append(nextCascade, cascade...)
							mu.Unlock()
						}
					}(ci, ck)
				}
				wg.Wait()
				pending = nextCascade
			}

		}

		if ctx.Err() != nil {
			log.Warn("L1 refresh: context expired before completion",
				slog.String("trigger", triggerGVR.String()),
				slog.Int64("refreshed", totalRefreshed),
				slog.Int("total", len(l1Keys)),
				slog.Any("err", ctx.Err()))
		}
		log.Info("L1 refresh: done",
			slog.String("trigger", triggerGVR.String()),
			slog.Int64("refreshed", totalRefreshed),
			slog.Int("total", len(l1Keys)))
		// Use a fresh background context so the sentinel is always written,
		// even if the refresh context has expired.
		cache.MarkL1Ready(context.Background(), c)
	}
}

// refreshSingleL1 re-resolves one L1 entry and updates the cache in-place.
// For widget entries, it also calls preWarmChildWidgets so that child widgets
// discovered during resolution (e.g. composition-panels) are pre-warmed into L1.
//
// Returns (ok, cascadeKeys): ok indicates success, cascadeKeys contains L1 keys
// that depend on the refreshed resource and should be enqueued for refresh too
// (cascading invalidation for RESTAction → widget dependency chains).
func refreshSingleL1(ctx context.Context, c *cache.RedisCache, user jwtutil.UserInfo, ep endpoints.Endpoint, accessToken string, info cache.ResolvedKeyInfo, rawKey, authnNS string) (bool, []string) {
	rctx := xcontext.BuildContext(ctx,
		xcontext.WithUserConfig(ep),
		xcontext.WithUserInfo(user),
		xcontext.WithAccessToken(accessToken),
	)
	rctx = cache.WithCache(rctx, c)

	got := objects.Get(rctx, templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name: info.Name, Namespace: info.NS,
		},
		APIVersion: info.GVR.GroupVersion().String(),
		Resource:   info.GVR.Resource,
	})
	if got.Err != nil {
		return false, nil
	}

	switch {
	case info.GVR.Group == widgetGroup:
		// Use ResolveWidgetBackground to avoid blocking HTTP requests that
		// resolve the same key via singleflight.
		result, err := ResolveWidgetBackground(rctx, c, got, rawKey, authnNS, info.PerPage, info.Page)
		if err != nil {
			return false, nil
		}
		_ = result
		registerApiRefGVRDeps(rctx, c, got.Unstructured, rawKey, nil)
		return true, nil

	case info.GVR.Group == templatesGroup && info.GVR.Resource == restactionResource:
		// Use ResolveRESTActionBackground to avoid blocking HTTP requests.
		_, err := ResolveRESTActionBackground(rctx, c, got.Unstructured.Object, rawKey, authnNS, info.PerPage, info.Page)
		if err != nil {
			return false, nil
		}
		registerApiRefGVRDeps(rctx, c, got.Unstructured, rawKey, nil)

		// Cascading refresh: find L1 keys that depend on this RESTAction
		// as a resource (e.g. piechart depends on compositions-list).
		depKey := cache.L1ResourceDepKey(
			cache.GVRToKey(info.GVR), info.NS, info.Name,
		)
		cascade, _ := c.SMembers(rctx, depKey)
		return true, cascade

	default:
		return false, nil
	}
}
