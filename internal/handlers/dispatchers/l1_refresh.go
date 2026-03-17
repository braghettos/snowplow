package dispatchers

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/apis"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/runtime"
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
	}
}

// refreshSingleL1 re-resolves one L1 entry and updates the cache in-place.
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

	tracker := cache.NewDependencyTracker()
	tctx := cache.WithDependencyTracker(rctx, tracker)

	var (
		raw []byte
		err error
	)

	switch {
	case info.GVR.Group == widgetGroup:
		res, resolveErr := widgets.Resolve(tctx, widgets.ResolveOptions{
			In:      got.Unstructured,
			AuthnNS: authnNS,
			PerPage: info.PerPage,
			Page:    info.Page,
		})
		if resolveErr != nil {
			return false
		}
		raw, err = json.Marshal(res)

	case info.GVR.Group == templatesGroup && info.GVR.Resource == restactionResource:
		scheme := runtime.NewScheme()
		_ = apis.AddToScheme(scheme)
		var cr templatesv1.RESTAction
		if convErr := runtime.DefaultUnstructuredConverter.FromUnstructured(got.Unstructured.Object, &cr); convErr != nil {
			return false
		}
		res, resolveErr := restactions.Resolve(tctx, restactions.ResolveOptions{
			In:      &cr,
			AuthnNS: authnNS,
			PerPage: info.PerPage,
			Page:    info.Page,
		})
		if resolveErr != nil {
			return false
		}
		raw, err = json.Marshal(res)

	default:
		return false
	}

	if err != nil {
		return false
	}

	_ = c.SetResolvedRaw(rctx, rawKey, raw)
	for _, gvrKey := range tracker.GVRKeys() {
		_ = c.SAddWithTTL(rctx, cache.L1GVRKey(gvrKey), rawKey, cache.DefaultResourceTTL)
	}
	registerApiRefGVRDeps(rctx, c, got.Unstructured, rawKey, tracker)
	return true
}
