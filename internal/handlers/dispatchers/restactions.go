package dispatchers

import (
	"context"
	"encoding/json"
	"fmt"
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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/runtime"
)

var restactionTracer = otel.Tracer("snowplow/dispatchers")

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
				lookupCtx, lookupSpan := restactionTracer.Start(req.Context(), "cache.lookup",
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
					wri.Header().Set("Cache-Control", "public, max-age=3, stale-while-revalidate=12")
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
		// Safe type assertion to avoid panic if singleflight returns
		// an unexpected type (Bug 14).
		raw, ok := result.([]byte)
		if !ok || raw == nil {
			response.InternalError(wri, fmt.Errorf("singleflight returned unexpected type %T", result))
			return
		}
		log.Info("RESTAction successfully resolved (singleflight)",
			slog.String("key", resolvedKey),
			slog.String("duration", util.ETA(start)))
		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		_, _ = wri.Write(raw)
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
		cache.RegisterL1Dependencies(ctx, c, tracker, resolvedKey)
		// Register group-level deps from the API request paths collected
		// during resolution. This ensures that when any resource in a K8s
		// API group changes, this RESTAction's L1 key is refreshed.
		cache.RegisterL1ApiDeps(ctx, c, resolvedKey, extractAPIRequests(raw))
	}

	return raw, nil
}

// extractAPIRequests extracts the "apiRequests" string array from the resolved
// RESTAction JSON output. Returns nil if the field is missing or not an array.
func extractAPIRequests(raw []byte) []string {
	// The apiRequests field is at the top level of the resolved output
	// (inside status.Raw which was marshaled from the api.Resolve dict).
	// However, the raw here is the full marshaled RESTAction, so we need
	// to look inside status.apiRequests.
	var wrapper struct {
		Status json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil || wrapper.Status == nil {
		return nil
	}
	var statusMap map[string]json.RawMessage
	if err := json.Unmarshal(wrapper.Status, &statusMap); err != nil {
		return nil
	}
	reqsRaw, ok := statusMap["apiRequests"]
	if !ok {
		return nil
	}
	var reqs []string
	if err := json.Unmarshal(reqsRaw, &reqs); err != nil {
		return nil
	}
	return reqs
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

// ResolveRESTActionBackground resolves a RESTAction using a dedicated background
// singleflight group. This deduplicates concurrent background L1 refresh
// attempts for the same key (e.g. l3gen scanner firing every 3s while a
// 10-15s compositions-list resolution is in progress) without blocking
// HTTP requests (which use the separate restactionFlight group).
func ResolveRESTActionBackground(ctx context.Context, c *cache.RedisCache, obj map[string]interface{}, resolvedKey, authnNS string, perPage, page int) ([]byte, error) {
	if resolvedKey == "" {
		return resolveRESTActionFromObject(ctx, c, obj, resolvedKey, authnNS, perPage, page, nil)
	}
	// Use WithoutCancel so the resolution doesn't abort if the first caller's
	// context expires — other callers waiting in the singleflight group may
	// have longer deadlines.
	sfCtx := context.WithoutCancel(ctx)
	result, err, _ := restactionBgFlight.Do(resolvedKey, func() (interface{}, error) {
		return resolveRESTActionFromObject(sfCtx, c, obj, resolvedKey, authnNS, perPage, page, nil)
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	return result.([]byte), nil
}
