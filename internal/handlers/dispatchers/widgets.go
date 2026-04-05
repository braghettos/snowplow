package dispatchers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/maps"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/profile"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func Widgets() http.Handler {
	return &widgetsHandler{
		authnNS: env.String("AUTHN_NAMESPACE", ""),
		verbose: env.True("DEBUG"),
	}
}

type widgetsHandler struct {
	authnNS string
	verbose bool
}

var _ http.Handler = (*widgetsHandler)(nil)

func (r *widgetsHandler) ServeHTTP(wri http.ResponseWriter, req *http.Request) {
	start := time.Now()
	log := xcontext.Logger(req.Context())

	extras, err := util.ParseExtras(req)
	if err != nil {
		response.BadRequest(wri, err)
		return
	}
	profile.Mark(req.Context(), "parse_extras")

	// ── Resolved-output cache ─────────────────────────────────────────────────
	// Cache the fully-resolved widget JSON keyed per user + resource.
	// This eliminates both the HTTP fan-out AND all JQ evaluations on repeated
	// requests. Only unpaginated requests without extras are cached.
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
				profile.Mark(req.Context(), "build_key")
				if raw, hit, _ := c.GetRaw(req.Context(), resolvedKey); hit {
				profile.Mark(req.Context(), "redis_get")
				cache.GlobalMetrics.Inc(&cache.GlobalMetrics.RawHits, "raw_hits")
				cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L1Hits, "l1_hits")
					log.Info("Widget resolved from cache",
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
					profile.Mark(req.Context(), "write")
					profile.End(req.Context(), "l1_hit")
					return
				}
		cache.GlobalMetrics.Inc(&cache.GlobalMetrics.RawMisses, "raw_misses")
		cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L1Misses, "l1_misses")
				log.Info("widget: L1 miss", slog.String("key", resolvedKey))
			}
		}
	}
	// ── End resolved-output cache ─────────────────────────────────────────────

	// Fetch the K8s object (needed for resolution). Done outside singleflight
	// because it requires the HTTP request to parse query parameters.
	got := fetchObject(req)
	if got.Err != nil {
		response.Encode(wri, got.Err)
		return
	}

	// ── Singleflight: deduplicate concurrent resolutions of the same key ──────
	if resolvedKey != "" {
		// Use context.WithoutCancel so that if the HTTP client disconnects,
		// the resolution still completes for other waiters.
		ctx := context.WithoutCancel(req.Context())
		result, resolveErr, _ := widgetFlight.Do(resolvedKey, func() (interface{}, error) {
			return resolveWidgetFromObject(ctx, c, got, resolvedKey, r.authnNS, perPage, page, extras)
		})
		if resolveErr != nil {
			writeWidgetError(wri, resolveErr)
			return
		}
		// Safe type assertion to avoid panic if singleflight returns
		// an unexpected type (Bug 14).
		res, ok := result.(*ResolveWidgetResult)
		if !ok || res == nil {
			writeWidgetError(wri, fmt.Errorf("singleflight returned unexpected type %T", result))
			return
		}
		log.Info("Widget successfully resolved (singleflight)",
			slog.String("key", resolvedKey),
			slog.String("duration", util.ETA(start)))
		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		_, _ = wri.Write(res.Raw)
		return
	}

	// Non-cacheable path (extras present): resolve inline without singleflight.
	res, resolveErr := resolveWidgetFromObject(req.Context(), c, got, "", r.authnNS, perPage, page, extras)
	if resolveErr != nil {
		writeWidgetError(wri, resolveErr)
		return
	}
	log.Info("Widget successfully resolved",
		slog.String("duration", util.ETA(start)))
	wri.Header().Set("Content-Type", "application/json")
	wri.WriteHeader(http.StatusOK)
	_, _ = wri.Write(res.Raw)
}

// resolveWidgetFromObject performs the full widget resolution: resolve → marshal
// → cache-set → pre-warm children. It is called both from the HTTP handler
// (via singleflight) and from L1 refresh (via ResolveWidgetDirect).
//
// Returns a *ResolveWidgetResult so singleflight callers can access both the
// raw JSON (for HTTP response) and the resolved unstructured (for child pre-warming).
func resolveWidgetFromObject(ctx context.Context, c *cache.RedisCache, got objects.Result, resolvedKey, authnNS string, perPage, page int, extras map[string]any) (*ResolveWidgetResult, error) {
	log := xcontext.Logger(ctx)

	log = log.With(
		slog.Group("widget",
			slog.String("name", widgets.GetName(got.Unstructured.Object)),
			slog.String("namespace", widgets.GetNamespace(got.Unstructured.Object)),
			slog.String("apiVersion", widgets.GetAPIVersion(got.Unstructured.Object)),
			slog.String("kind", widgets.GetKind(got.Unstructured.Object)),
		),
	)

	tracker := cache.NewDependencyTracker()
	tctx := cache.WithDependencyTracker(xcontext.BuildContext(ctx), tracker)

	res, err := widgets.Resolve(tctx, widgets.ResolveOptions{
		In:      got.Unstructured,
		AuthnNS: authnNS,
		PerPage: perPage,
		Page:    page,
		Extras:  extras,
	})
	if err != nil {
		log.Error("unable to resolve widget", slog.Any("err", err))
		return nil, err
	}

	traceId := xcontext.TraceId(tctx, false)
	if traceId != "" {
		if terr := maps.SetNestedField(res.Object, traceId, "status", "traceId"); terr != nil {
			log.Warn("unable to set traceId in status", slog.Any("err", terr))
		}
	}

	raw, merr := json.Marshal(res)
	if merr != nil {
		return nil, merr
	}

	// Populate the resolved cache and register GVR reverse indexes so that
	// informer events can do targeted invalidation instead of bulk deletes.
	if c != nil && resolvedKey != "" {
		_ = c.SetResolvedRaw(ctx, resolvedKey, raw)
		cache.RegisterL1Dependencies(ctx, c, tracker, resolvedKey)
		preWarmChildWidgets(ctx, c, res, authnNS)
	}

	return &ResolveWidgetResult{Raw: raw, Resolved: res}, nil
}

// ResolveWidgetDirect is the entry point for L1 refresh to resolve a widget
// using the same singleflight group as the HTTP handler. Returns both the raw
// JSON and the resolved unstructured for child pre-warming.
func ResolveWidgetDirect(ctx context.Context, c *cache.RedisCache, got objects.Result, resolvedKey, authnNS string, perPage, page int) (*ResolveWidgetResult, error) {
	result, err, _ := widgetFlight.Do(resolvedKey, func() (interface{}, error) {
		return resolveWidgetFromObject(ctx, c, got, resolvedKey, authnNS, perPage, page, nil)
	})
	if err != nil {
		return nil, err
	}
	return result.(*ResolveWidgetResult), nil
}

// ResolveWidgetBackground resolves a widget using a dedicated background
// singleflight group. This deduplicates concurrent background L1 refresh
// attempts for the same key without blocking HTTP requests (which use the
// separate widgetFlight group).
func ResolveWidgetBackground(ctx context.Context, c *cache.RedisCache, got objects.Result, resolvedKey, authnNS string, perPage, page int) (*ResolveWidgetResult, error) {
	if resolvedKey == "" {
		return resolveWidgetFromObject(ctx, c, got, resolvedKey, authnNS, perPage, page, nil)
	}
	// Use WithoutCancel so the resolution doesn't abort if the first caller's
	// context expires — other callers waiting in the singleflight group may
	// have longer deadlines.
	sfCtx := context.WithoutCancel(ctx)
	result, err, _ := widgetBgFlight.Do(resolvedKey, func() (interface{}, error) {
		return resolveWidgetFromObject(sfCtx, c, got, resolvedKey, authnNS, perPage, page, nil)
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	return result.(*ResolveWidgetResult), nil
}

func writeWidgetError(wri http.ResponseWriter, err error) {
	var statusErr *apierrors.StatusError
	if errors.As(err, &statusErr) {
		code := int(statusErr.Status().Code)
		msg := fmt.Errorf("%s", statusErr.Status().Message)
		response.Encode(wri, response.New(code, msg))
		return
	}
	response.InternalError(wri, err)
}

// ResolveWidgetResult holds the resolved unstructured widget and its serialized
// JSON. Used by L1 refresh to pass the result to preWarmChildWidgets.
type ResolveWidgetResult struct {
	Raw      []byte
	Resolved *unstructured.Unstructured
}
