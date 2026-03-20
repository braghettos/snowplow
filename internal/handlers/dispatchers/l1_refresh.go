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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

const (
	refreshConcurrency = 20
	restactionResource = "restactions"
	templatesGroup     = "templates.krateo.io"
)

// MakeL1Refresher returns a cache.L1RefreshFunc that re-resolves L1 keys in
// the background instead of deleting them. Old values keep being served while
// the refresh runs (stale-while-revalidate).
func MakeL1Refresher(c *cache.RedisCache, rc *rest.Config, authnNS, signKey string) cache.L1RefreshFunc {
	return func(ctx context.Context, triggerGVR schema.GroupVersionResource, l1Keys []string) {
		log := slog.Default()
		log.Info("L1 refresh: starting",
			slog.String("trigger", triggerGVR.String()),
			slog.Int("keys", len(l1Keys)))

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
				wg  sync.WaitGroup
				sem = make(chan struct{}, refreshConcurrency)
				mu  sync.Mutex
				n   int64
			)
			for _, k := range keys {
				wg.Add(1)
				sem <- struct{}{}
			go func(info cache.ResolvedKeyInfo, rawKey string) {
				defer wg.Done()
				defer func() { <-sem }()
				if refreshSingleL1(ctx, c, user, ep, accessToken, info, rawKey, authnNS) {
						mu.Lock()
						n++
						mu.Unlock()
					}
				}(k.info, k.raw)
			}
			wg.Wait()
			totalRefreshed += n
		}

		log.Info("L1 refresh: done",
			slog.String("trigger", triggerGVR.String()),
			slog.Int64("refreshed", totalRefreshed),
			slog.Int("total", len(l1Keys)))
		cache.MarkL1Ready(ctx, c)
	}
}

// refreshSingleL1 re-resolves one L1 entry and updates the cache in-place.
// For widget entries, it also calls preWarmChildWidgets so that child widgets
// discovered during resolution (e.g. composition-panels) are pre-warmed into L1.
func refreshSingleL1(ctx context.Context, c *cache.RedisCache, user jwtutil.UserInfo, ep endpoints.Endpoint, accessToken string, info cache.ResolvedKeyInfo, rawKey, authnNS string) bool {
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
		return false
	}

	switch {
	case info.GVR.Group == widgetGroup:
		// Use ResolveWidgetDirect which shares the singleflight group with
		// the HTTP handler — concurrent resolutions are deduplicated.
		result, err := ResolveWidgetDirect(rctx, c, got, rawKey, authnNS, info.PerPage, info.Page)
		if err != nil {
			return false
		}
		// preWarmChildWidgets is already called inside resolveWidgetFromObject
		// (via ResolveWidgetDirect), so newly-discovered child widgets
		// (e.g. composition-panels) are pre-warmed automatically.
		_ = result
		registerApiRefGVRDeps(rctx, c, got.Unstructured, rawKey, nil)
		return true

	case info.GVR.Group == templatesGroup && info.GVR.Resource == restactionResource:
		// Use ResolveRESTActionDirect which shares the singleflight group
		// with the HTTP handler.
		_, err := ResolveRESTActionDirect(rctx, c, got.Unstructured.Object, rawKey, authnNS, info.PerPage, info.Page)
		if err != nil {
			return false
		}
		registerApiRefGVRDeps(rctx, c, got.Unstructured, rawKey, nil)
		return true

	default:
		return false
	}
}
