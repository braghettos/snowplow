package apiref

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions/l1cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

type ResolveOptions struct {
	RC      *rest.Config
	ApiRef  templatesv1.ObjectReference
	AuthnNS string
	PerPage int
	Page    int
	Extras  map[string]any
}

// restActionGVR is the GVR under which RESTAction L1 entries are stored.
// Matches the key written by the HTTP dispatcher (via l1cache).
var restActionGVR = schema.GroupVersionResource{
	Group:    "templates.krateo.io",
	Version:  "v1",
	Resource: "restactions",
}

// Resolve fetches (and on L1 miss, resolves + caches) the RESTAction
// referenced by opts.ApiRef for the current user, returning its status
// subtree.
//
// L1 miss path delegates to the shared l1cache.ResolveAndCache helper,
// which owns both the write path AND a process-wide foreground
// singleflight.Group. The group is shared with the RESTAction HTTP
// dispatcher, so concurrent widget + direct /call requests for the
// same key dedup against each other — the previous per-package group
// allowed N parallel resolutions of the same 32 s aggregation.
func Resolve(ctx context.Context, opts ResolveOptions) (map[string]any, error) {
	if opts.ApiRef.Name == "" || opts.ApiRef.Namespace == "" {
		return map[string]any{}, nil
	}

	c := cache.FromContext(ctx)
	var l1Key string
	if c != nil {
		user, uerr := xcontext.UserInfo(ctx)
		if uerr == nil && user.Username != "" {
			identity := cache.CacheIdentity(ctx, user.Username)
			l1Key = cache.ResolvedKey(identity, restActionGVR,
				opts.ApiRef.Namespace, opts.ApiRef.Name, opts.Page, opts.PerPage)
			// L1 fast path: when a widget depends on a RESTAction that
			// has already been resolved for this user, reading the
			// cached output is orders of magnitude faster than
			// re-running the full RESTAction pipeline. At 50K
			// compositions this saves ~10-15 s per widget refresh.
			if status, ok := lookupL1(ctx, c, l1Key); ok {
				// Record the restaction GVR in the dependency tracker
				// even on L1 hit. The widget dispatcher uses the
				// tracker to register deps (widgets.go:267). Without
				// this, the widget L1 key is not connected to the
				// restaction in the dep graph and never gets refreshed
				// when the restaction's data changes.
				if tracker := cache.TrackerFromContext(ctx); tracker != nil {
					tracker.AddGVR(restActionGVR)
					tracker.AddResource(restActionGVR, opts.ApiRef.Namespace, opts.ApiRef.Name)
					slog.Debug("apiref: L1 hit, added restaction dep to tracker",
						slog.String("l1Key", l1Key),
						slog.String("apiRef", opts.ApiRef.Name),
						slog.Int("page", opts.Page),
						slog.Int("perPage", opts.PerPage))
				} else {
					slog.Warn("apiref: L1 hit but NO tracker in context",
						slog.String("l1Key", l1Key))
				}
				return status, nil
			}
		}
	}

	// L1 miss: fall through to shared resolver.
	if l1Key != "" {
		status, err := resolveViaL1Cache(ctx, c, opts, l1Key)
		// Register the restaction dep in the CALLER's tracker even on
		// L1 miss. l1cache.ResolveAndCache creates its own tracker, so
		// the widget's tracker doesn't get the restaction ref automatically.
		// Without this, the widget → RESTAction cascade dep is never
		// written on cold start (first resolve).
		if err == nil {
			if tracker := cache.TrackerFromContext(ctx); tracker != nil {
				tracker.AddGVR(restActionGVR)
				tracker.AddResource(restActionGVR, opts.ApiRef.Namespace, opts.ApiRef.Name)
			}
		}
		return status, err
	}

	// No cache context (tests, etc.): inline resolve with no L1 write.
	return resolveInline(ctx, opts)
}

// resolveViaL1Cache fetches the RESTAction CR and hands it off to
// l1cache.ResolveAndCache, which runs inside the shared foreground
// singleflight.Group and writes the L1 entry.
func resolveViaL1Cache(ctx context.Context, c *cache.RedisCache, opts ResolveOptions, l1Key string) (map[string]any, error) {
	res := objects.Get(ctx, opts.ApiRef)
	if res.Err != nil {
		return map[string]any{}, fmt.Errorf("%s", res.Err.Message)
	}

	result, err := l1cache.ResolveAndCache(ctx, l1cache.Input{
		Cache:       c,
		Obj:         res.Unstructured.Object,
		ResolvedKey: l1Key,
		AuthnNS:     opts.AuthnNS,
		SArc:        opts.RC,
		PerPage:     opts.PerPage,
		Page:        opts.Page,
		Extras:      opts.Extras,
	})
	if err != nil {
		return map[string]any{}, err
	}
	if result == nil || result.Status == nil {
		return map[string]any{}, nil
	}
	return result.Status, nil
}

// resolveInline is the no-cache fast path: no L1 lookup, no L1 write,
// no singleflight. Used when there is no cache context.
func resolveInline(ctx context.Context, opts ResolveOptions) (map[string]any, error) {
	res := objects.Get(ctx, opts.ApiRef)
	if res.Err != nil {
		return map[string]any{}, fmt.Errorf("%s", res.Err.Message)
	}

	ra, err := convertToRESTAction(res.Unstructured.Object)
	if err != nil {
		return map[string]any{}, err
	}

	if _, err = restactions.Resolve(ctx, restactions.ResolveOptions{
		In:      &ra,
		SArc:    opts.RC,
		AuthnNS: opts.AuthnNS,
		PerPage: opts.PerPage,
		Page:    opts.Page,
		Extras:  opts.Extras,
	}); err != nil {
		return map[string]any{}, err
	}

	return rawExtensionToMap(ra.Status)
}

// lookupL1 reads a RESTAction L1 entry and extracts its status map.
// Returns (status, true) on hit, (nil, false) on miss or decode failure.
func lookupL1(ctx context.Context, c *cache.RedisCache, l1Key string) (map[string]any, bool) {
	raw, hit, err := c.GetRaw(ctx, l1Key)
	if err != nil || !hit || len(raw) == 0 {
		return nil, false
	}
	var cached map[string]any
	if json.Unmarshal(raw, &cached) != nil {
		return nil, false
	}
	status, ok := cached["status"].(map[string]any)
	if !ok {
		return nil, false
	}
	return status, true
}
