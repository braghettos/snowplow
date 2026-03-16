package dispatchers

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	preWarmConcurrency = 10
	preWarmTimeout     = 30 * time.Second
	widgetGroup        = "widgets.templates.krateo.io"
)

// preWarmChildWidgets resolves child widget references discovered during parent
// widget resolution and stores them in L1 cache. This eliminates the cold-start
// fan-out where the frontend issues N individual requests (e.g. 42 Routes after
// a RoutesLoader) that would each be an L1 miss.
func preWarmChildWidgets(parentCtx context.Context, c *cache.RedisCache, resolved *unstructured.Unstructured, authnNS string) {
	items, found, err := unstructured.NestedSlice(resolved.Object, "status", "resourcesRefs", "items")
	if err != nil || !found || len(items) == 0 {
		return
	}

	user, uerr := xcontext.UserInfo(parentCtx)
	if uerr != nil {
		return
	}
	ep, eerr := xcontext.UserConfig(parentCtx)
	if eerr != nil {
		return
	}
	accessToken, _ := xcontext.AccessToken(parentCtx)

	type childRef struct {
		gvr  schema.GroupVersionResource
		ns   string
		name string
	}

	var refs []childRef
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		path, _ := m["path"].(string)
		if path == "" || !strings.Contains(path, "/call?") {
			continue
		}
		gvr, ns, name := cache.ParseCallPath(path)
		if gvr.Resource == "" || name == "" {
			continue
		}
		if gvr.Group != widgetGroup {
			continue
		}
		key := cache.ResolvedKey(user.Username, gvr, ns, name, -1, -1)
		if c.Exists(parentCtx, key) {
			continue
		}
		refs = append(refs, childRef{gvr: gvr, ns: ns, name: name})
	}

	if len(refs) == 0 {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), preWarmTimeout)
		defer cancel()

		ctx = xcontext.BuildContext(ctx,
			xcontext.WithUserConfig(ep),
			xcontext.WithUserInfo(user),
			xcontext.WithAccessToken(accessToken),
		)
		ctx = cache.WithCache(ctx, c)

		log := slog.Default()

		var (
			wg      sync.WaitGroup
			sem     = make(chan struct{}, preWarmConcurrency)
			warmed  int64
			mu      sync.Mutex
		)

		for _, ref := range refs {
			wg.Add(1)
			sem <- struct{}{}
			go func(r childRef) {
				defer wg.Done()
				defer func() { <-sem }()

				rctx := xcontext.BuildContext(ctx,
					xcontext.WithUserConfig(ep),
					xcontext.WithUserInfo(user),
					xcontext.WithAccessToken(accessToken),
				)
				rctx = cache.WithCache(rctx, c)

				got := objects.Get(rctx, templatesv1.ObjectReference{
					Reference: templatesv1.Reference{
						Name: r.name, Namespace: r.ns,
					},
					APIVersion: r.gvr.GroupVersion().String(),
					Resource:   r.gvr.Resource,
				})
				if got.Err != nil {
					return
				}

				tracker := cache.NewDependencyTracker()
				tctx := cache.WithDependencyTracker(rctx, tracker)

				res, err := widgets.Resolve(tctx, widgets.ResolveOptions{
					In:      got.Unstructured,
					AuthnNS: authnNS,
					PerPage: -1,
					Page:    -1,
				})
				if err != nil {
					return
				}

				raw, merr := json.MarshalIndent(res, "", "  ")
				if merr != nil {
					return
				}

				rKey := cache.ResolvedKey(user.Username, r.gvr, r.ns, r.name, -1, -1)
				_ = c.SetResolvedRaw(rctx, rKey, raw)
				for _, gvrKey := range tracker.GVRKeys() {
					_ = c.SAddWithTTL(rctx, cache.L1GVRKey(gvrKey), rKey, cache.DefaultResourceTTL)
				}

				mu.Lock()
				warmed++
				mu.Unlock()
			}(ref)
		}

		wg.Wait()
		log.Info("L1 pre-warm completed",
			slog.String("user", user.Username),
			slog.Int("candidates", len(refs)),
			slog.Int64("warmed", warmed),
		)
	}()
}
