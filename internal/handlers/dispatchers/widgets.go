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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var widgetTracer = otel.Tracer("snowplow/dispatchers")

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
				lookupCtx, lookupSpan := widgetTracer.Start(req.Context(), "cache.lookup",
					trace.WithAttributes(
						attribute.String("cache.layer", "l1"),
						attribute.String("cache.key", resolvedKey),
					))
				raw, hit, _ := c.GetRaw(lookupCtx, resolvedKey)
				if lookupSpan.IsRecording() {
					lookupSpan.SetAttributes(attribute.Bool("cache.hit", hit))
				}
				lookupSpan.End()
				if hit {
				if httpSpan := trace.SpanFromContext(req.Context()); httpSpan.IsRecording() {
					httpSpan.AddEvent("cache.hit", trace.WithAttributes(
						attribute.String("cache.key", resolvedKey),
						attribute.String("cache.layer", "l1"),
					))
				}
				profile.Mark(req.Context(), "redis_get")
				cache.GlobalMetrics.Inc(&cache.GlobalMetrics.RawHits, "raw_hits")
				cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L1Hits, "l1_hits")
				profile.Mark(req.Context(), "metrics")
					log.Info("Widget resolved from cache",
						slog.String("key", resolvedKey),
						slog.String("user", user.Username),
						slog.String("resource", gvr.Resource),
						slog.String("name", nsn.Name),
						slog.String("namespace", nsn.Namespace),
						slog.String("source", "L1-cache"),
						slog.String("duration", util.ETA(start)))
					profile.Mark(req.Context(), "log_info")
					wri.Header().Set("Content-Type", "application/json")
					wri.Header().Set("Cache-Control", "public, max-age=3, stale-while-revalidate=12")
					wri.WriteHeader(http.StatusOK)
					profile.Mark(req.Context(), "headers")
					_, writeSpan := widgetTracer.Start(req.Context(), "http.write",
						trace.WithAttributes(
							attribute.Bool("cache.hit", true),
							attribute.Int("http.response.body.size", len(raw)),
						))
					_, _ = wri.Write(raw)
					writeSpan.End()
					profile.Mark(req.Context(), "body_write")
					profile.End(req.Context(), "l1_hit")
					return
				}
		if httpSpan := trace.SpanFromContext(req.Context()); httpSpan.IsRecording() {
			httpSpan.AddEvent("cache.miss", trace.WithAttributes(
				attribute.String("cache.key", resolvedKey),
				attribute.String("cache.layer", "l1"),
			))
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
	_, fetchSpan := widgetTracer.Start(req.Context(), "widget.fetch_object")
	got := fetchObject(req)
	fetchSpan.End()
	if got.Err != nil {
		response.Encode(wri, got.Err)
		return
	}

	// Resolve the widget. L1 keys are per-user so there is no
	// thundering herd — each user resolves their own key.
	res, resolveErr := resolveWidgetFromObject(req.Context(), c, got, resolvedKey, r.authnNS, perPage, page, extras)
	if resolveErr != nil {
		writeWidgetError(wri, resolveErr)
		return
	}
	log.Info("Widget successfully resolved",
		slog.String("duration", util.ETA(start)))
	wri.Header().Set("Content-Type", "application/json")
	wri.WriteHeader(http.StatusOK)
	_, writeSpan := widgetTracer.Start(req.Context(), "http.write",
		trace.WithAttributes(
			attribute.Bool("cache.hit", false),
			attribute.String("path", "inline"),
			attribute.Int("http.response.body.size", len(res.Raw)),
		))
	_, _ = wri.Write(res.Raw)
	writeSpan.End()
}

// resolveWidgetFromObject performs the full widget resolution: resolve → marshal
// → cache-set → pre-warm children. It is called both from the HTTP handler
// (via singleflight) and from L1 refresh (via ResolveWidgetDirect).
//
// Returns a *ResolveWidgetResult so singleflight callers can access both the
// raw JSON (for HTTP response) and the resolved unstructured (for child pre-warming).
func resolveWidgetFromObject(ctx context.Context, c *cache.RedisCache, got objects.Result, resolvedKey, authnNS string, perPage, page int, extras map[string]any) (*ResolveWidgetResult, error) {
	ctx, span := widgetTracer.Start(ctx, "widget.resolve",
		trace.WithAttributes(
			attribute.String("widget.kind", widgets.GetKind(got.Unstructured.Object)),
			attribute.String("widget.name", widgets.GetName(got.Unstructured.Object)),
			attribute.String("widget.namespace", widgets.GetNamespace(got.Unstructured.Object)),
		))
	defer span.End()

	if span.IsRecording() {
		span.AddEvent("widget.resolution.started", trace.WithAttributes(
			attribute.String("widget.kind", widgets.GetKind(got.Unstructured.Object)),
			attribute.String("widget.name", widgets.GetName(got.Unstructured.Object)),
		))
	}

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
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	traceId := xcontext.TraceId(tctx, false)
	if traceId != "" {
		if terr := maps.SetNestedField(res.Object, traceId, "status", "traceId"); terr != nil {
			log.Warn("unable to set traceId in status", slog.Any("err", terr))
		}
	}

	_, marshalSpan := widgetTracer.Start(ctx, "http.marshal")
	raw, merr := json.Marshal(res)
	if merr == nil {
		marshalSpan.SetAttributes(attribute.Int("http.response.body.size", len(raw)))
	}
	marshalSpan.End()
	if merr != nil {
		return nil, merr
	}

	// Write resolved output to L1. Register cascade deps so that:
	// 1. The widget appears in the RESTAction's per-resource dep index
	//    (for cascade: compositions-list → piechart)
	// 2. The widget appears in the composition GVR dep index
	//    (for triggerL1Refresh to find compositions-list)
	//
	// Only write deps from the tracker's ResourceRefs that are RESTAction
	// refs (from apiRef resolution). Container widgets without apiRef
	// have no RESTAction refs in the tracker, so nothing is registered.
	if c != nil && resolvedKey != "" {
		_ = c.SetResolvedRaw(ctx, resolvedKey, raw)
		// Register per-resource cascade dep (widget → specific RESTAction).
		// The tracker records AddResource(restactionGVR, ns, name) during
		// apiref.Resolve. This puts the widget L1 key in the RESTAction's
		// dep index so refreshSingleL1's cascade finds it.
		refs := tracker.ResourceRefs()
		if len(refs) > 0 {
			pipe := c.Pipeline(ctx)
			if pipe != nil {
				for _, ref := range refs {
					key := cache.L1ResourceDepKey(ref.GVRKey, ref.NS, ref.Name)
					pipe.SAdd(ctx, key, resolvedKey)
					pipe.Expire(ctx, key, cache.ReverseIndexTTL)
				}
				_, _ = pipe.Exec(ctx)
			}
		}
		_, preWarmSpan := widgetTracer.Start(ctx, "widget.prewarm_children")
		preWarmChildWidgets(ctx, c, res, authnNS)
		preWarmSpan.End()
	}

	return &ResolveWidgetResult{Raw: raw, Resolved: res}, nil
}

// ResolveWidgetDirect resolves a widget and writes L1. Used by prewarm.
func ResolveWidgetDirect(ctx context.Context, c *cache.RedisCache, got objects.Result, resolvedKey, authnNS string, perPage, page int) (*ResolveWidgetResult, error) {
	return resolveWidgetFromObject(ctx, c, got, resolvedKey, authnNS, perPage, page, nil)
}

// ResolveWidgetBackground resolves a widget for the background L1 refresh path.
func ResolveWidgetBackground(ctx context.Context, c *cache.RedisCache, got objects.Result, resolvedKey, authnNS string, perPage, page int) (*ResolveWidgetResult, error) {
	return resolveWidgetFromObject(ctx, c, got, resolvedKey, authnNS, perPage, page, nil)
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
