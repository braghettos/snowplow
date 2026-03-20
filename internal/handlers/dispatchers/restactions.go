package dispatchers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/snowplow/apis"
	v1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"k8s.io/apimachinery/pkg/runtime"
)

func RESTAction() http.Handler {
	return &restActionHandler{
		authnNS: env.String("AUTHN_NAMESPACE", ""),
		verbose: env.True("DEBUG"),
	}
}

type restActionHandler struct {
	authnNS string
	verbose bool
}

var _ http.Handler = (*restActionHandler)(nil)

func (r *restActionHandler) ServeHTTP(wri http.ResponseWriter, req *http.Request) {
	log := xcontext.Logger(req.Context())
	start := time.Now()

	extras, err := util.ParseExtras(req)
	if err != nil {
		response.BadRequest(wri, err)
		return
	}

	// ── Resolved-output cache ─────────────────────────────────────────────────
	// Cache the fully-resolved RESTAction JSON keyed per user + resource.
	// This eliminates both the HTTP fan-out AND all JQ evaluations on repeated
	// requests. Only unpaginated requests are cached (extras excluded for now).
	perPage, page := paginationInfo(log, req)
	c := cache.FromContext(req.Context())

	var resolvedKey string
	if c != nil && len(extras) == 0 {
		gvr, gerr := util.ParseGVR(req)
		nsn, nerr := util.ParseNamespacedName(req)
		if gerr == nil && nerr == nil {
			user, uerr := xcontext.UserInfo(req.Context())
			if uerr == nil {
				resolvedKey = cache.ResolvedKey(user.Username, gvr, nsn.Namespace, nsn.Name, page, perPage)
				if raw, hit, _ := c.GetRaw(req.Context(), resolvedKey); hit {
				cache.GlobalMetrics.Inc(&cache.GlobalMetrics.RawHits, "raw_hits")
				cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L1Hits, "l1_hits")
					log.Info("RESTAction resolved from cache",
						slog.String("key", resolvedKey),
						slog.String("user", user.Username),
						slog.String("resource", gvr.Resource),
						slog.String("name", nsn.Name),
						slog.String("namespace", nsn.Namespace),
						slog.String("source", "L1-cache"),
						slog.String("duration", util.ETA(start)))
					wri.Header().Set("Content-Type", "application/json")
					wri.Header().Set("Cache-Control", "public, max-age=15")
					wri.WriteHeader(http.StatusOK)
					_, _ = wri.Write(raw)
					return
				}
		cache.GlobalMetrics.Inc(&cache.GlobalMetrics.RawMisses, "raw_misses")
		cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L1Misses, "l1_misses")
				log.Info("restaction: L1 miss", slog.String("key", resolvedKey))
			}
		}
	}
	// ── End resolved-output cache ─────────────────────────────────────────────

	// Fetch the K8s object (needs HTTP request for query params).
	got := fetchObject(req)
	if got.Err != nil {
		response.Encode(wri, got.Err)
		return
	}

	// ── Singleflight: deduplicate concurrent resolutions of the same key ──────
	if resolvedKey != "" {
		ctx := context.WithoutCancel(req.Context())
		result, resolveErr, _ := restactionFlight.Do(resolvedKey, func() (interface{}, error) {
			return resolveRESTActionFromObject(ctx, c, got.Unstructured.Object, resolvedKey, r.authnNS, perPage, page, extras)
		})
		if resolveErr != nil {
			response.InternalError(wri, resolveErr)
			return
		}
		log.Info("RESTAction successfully resolved (singleflight)",
			slog.String("key", resolvedKey),
			slog.String("duration", util.ETA(start)))
		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		_, _ = wri.Write(result.([]byte))
		return
	}

	// Non-cacheable path (extras present): resolve inline without singleflight.
	raw, resolveErr := resolveRESTActionFromObject(req.Context(), c, got.Unstructured.Object, "", r.authnNS, perPage, page, extras)
	if resolveErr != nil {
		response.InternalError(wri, resolveErr)
		return
	}
	log.Info("RESTAction successfully resolved",
		slog.String("duration", util.ETA(start)))
	wri.Header().Set("Content-Type", "application/json")
	wri.WriteHeader(http.StatusOK)
	_, _ = wri.Write(raw)
}

// resolveRESTActionFromObject performs the full RESTAction resolution: convert →
// resolve → marshal → strip annotations → cache-set. Called from the HTTP
// handler (via singleflight) and from L1 refresh (via ResolveRESTActionDirect).
func resolveRESTActionFromObject(ctx context.Context, c *cache.RedisCache, obj map[string]interface{}, resolvedKey, authnNS string, perPage, page int, extras map[string]any) ([]byte, error) {
	log := xcontext.Logger(ctx)

	scheme := runtime.NewScheme()
	if err := apis.AddToScheme(scheme); err != nil {
		return nil, err
	}

	var cr v1.RESTAction
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &cr); err != nil {
		log.Error("unable to convert unstructured to typed rest action",
			slog.Any("err", err))
		return nil, err
	}

	tracker := cache.NewDependencyTracker()
	tctx := cache.WithDependencyTracker(xcontext.BuildContext(ctx), tracker)
	res, err := restactions.Resolve(tctx, restactions.ResolveOptions{
		In:      &cr,
		AuthnNS: authnNS,
		PerPage: perPage,
		Page:    page,
		Extras:  extras,
	})
	if err != nil {
		log.Error("unable to resolve rest action",
			slog.String("name", cr.GetName()),
			slog.String("namespace", cr.GetNamespace()),
			slog.Any("err", err))
		return nil, err
	}

	log.Info("RESTAction resolved",
		slog.String("name", cr.Name),
		slog.String("namespace", cr.Namespace))

	raw, merr := json.Marshal(res)
	if merr != nil {
		return nil, merr
	}

	raw = cache.StripBulkyAnnotations(raw)

	if c != nil && resolvedKey != "" {
		_ = c.SetResolvedRaw(ctx, resolvedKey, raw)
		for _, gvrKey := range tracker.GVRKeys() {
			_ = c.SAddWithTTL(ctx, cache.L1GVRKey(gvrKey), resolvedKey, cache.ReverseIndexTTL)
		}
	}

	return raw, nil
}

// ResolveRESTActionDirect is the entry point for L1 refresh to resolve a
// RESTAction using the same singleflight group as the HTTP handler.
func ResolveRESTActionDirect(ctx context.Context, c *cache.RedisCache, obj map[string]interface{}, resolvedKey, authnNS string, perPage, page int) ([]byte, error) {
	result, err, _ := restactionFlight.Do(resolvedKey, func() (interface{}, error) {
		return resolveRESTActionFromObject(ctx, c, obj, resolvedKey, authnNS, perPage, page, nil)
	})
	if err != nil {
		return nil, err
	}
	return result.([]byte), nil
}
