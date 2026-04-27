package dispatchers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions/l1cache"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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
				identity := cache.CacheIdentity(req.Context(), user.Username)
				resolvedKey = cache.ResolvedKey(identity, gvr, nsn.Namespace, nsn.Name, page, perPage)
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
					if httpSpan := trace.SpanFromContext(req.Context()); httpSpan.IsRecording() {
						httpSpan.AddEvent("cache.hit", trace.WithAttributes(
							attribute.String("cache.key", resolvedKey),
							attribute.String("cache.layer", "l1"),
						))
					}
					cache.GlobalMetrics.Inc(&cache.GlobalMetrics.RawHits, "raw_hits")
					cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L1Hits, "l1_hits")
					cache.TouchKey(cache.ResolvedKeyBase(identity, gvr, nsn.Namespace, nsn.Name))
					log.Info("RESTAction resolved from cache",
						slog.String("key", resolvedKey),
						slog.String("user", user.Username),
						slog.String("resource", gvr.Resource),
						slog.String("name", nsn.Name),
						slog.String("namespace", nsn.Namespace),
						slog.String("source", "L1-cache"),
						slog.String("duration", util.ETA(start)))
					wri.Header().Set("Content-Type", "application/json")
					wri.WriteHeader(http.StatusOK)
					_, writeSpan := restactionTracer.Start(req.Context(), "http.write",
						trace.WithAttributes(
							attribute.Bool("cache.hit", true),
							attribute.Int("http.response.body.size", len(raw)),
						))
					_, _ = wri.Write(raw)
					writeSpan.End()
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
				log.Info("restaction: L1 miss", slog.String("key", resolvedKey))
			}
		}
	}
	// ── End resolved-output cache ─────────────────────────────────────────────

	// Fetch the K8s object (needs HTTP request for query params).
	_, fetchSpan := restactionTracer.Start(req.Context(), "restaction.fetch_object")
	got := fetchObject(req)
	fetchSpan.End()
	if got.Err != nil {
		response.Encode(wri, got.Err)
		return
	}

	// ── Resolve via the shared l1cache package ─────────────────────────────
	// The shared foreground singleflight.Group in l1cache dedups against
	// widget.apiref callers too, killing the cross-path thundering herd
	// when a dashboard Row spawns 2+ parallel widget requests that all
	// chase the same compositions-list restaction.
	//
	// Non-cacheable path (extras present) passes ResolvedKey="" which
	// bypasses both singleflight and L1 write.
	sfCtx := req.Context()
	if resolvedKey != "" {
		sfCtx = context.WithoutCancel(req.Context())
	}
	result, resolveErr := l1cache.ResolveAndCache(sfCtx, l1cache.Input{
		Cache:       c,
		Obj:         got.Unstructured.Object,
		ResolvedKey: resolvedKey,
		AuthnNS:     r.authnNS,
		PerPage:     perPage,
		Page:        page,
		Extras:      extras,
	})
	if resolveErr != nil {
		response.InternalError(wri, resolveErr)
		return
	}
	if result == nil || result.Raw == nil {
		response.InternalError(wri, errEmptyResolveResult)
		return
	}
	raw := result.Raw

	// Touch the key so it starts HOT for refresh priority.
	if resolvedKey != "" {
		if rki, ok := cache.ParseResolvedKey(resolvedKey); ok {
			cache.TouchKey(cache.ResolvedKeyBase(rki.Username, rki.GVR, rki.NS, rki.Name))
		}
	}

	pathAttr := "inline"
	if resolvedKey != "" {
		pathAttr = "singleflight"
	}
	log.Info("RESTAction successfully resolved",
		slog.String("key", resolvedKey),
		slog.String("path", pathAttr),
		slog.String("duration", util.ETA(start)))
	wri.Header().Set("Content-Type", "application/json")
	wri.WriteHeader(http.StatusOK)
	_, writeSpan := restactionTracer.Start(req.Context(), "http.write",
		trace.WithAttributes(
			attribute.Bool("cache.hit", false),
			attribute.String("path", pathAttr),
			attribute.Int("http.response.body.size", len(raw)),
		))
	_, _ = wri.Write(raw)
	writeSpan.End()
}

// errEmptyResolveResult indicates the shared l1cache helper returned
// (nil, nil) — should not happen in practice, kept as a typed error so
// the HTTP path returns 500 cleanly instead of panicking on a nil
// dereference downstream.
var errEmptyResolveResult = errEmptyResolve{}

type errEmptyResolve struct{}

func (errEmptyResolve) Error() string {
	return "l1cache.ResolveAndCache returned empty result with no error"
}

// ResolveRESTActionBackground is the entry point for the L1 refresh
// loop. It forwards to l1cache.ResolveAndCache.
//
// Kept here as a thin shim because internal/handlers/dispatchers/
// l1_refresh.go already imports this package and calls this function;
// moving the call site to l1cache directly would churn l1_refresh
// without any correctness or performance benefit.
func ResolveRESTActionBackground(ctx context.Context, c cache.Cache, obj map[string]interface{}, resolvedKey, authnNS string, perPage, page int) ([]byte, error) {
	result, err := l1cache.ResolveAndCache(ctx, l1cache.Input{
		Cache:       c,
		Obj:         obj,
		ResolvedKey: resolvedKey,
		AuthnNS:     authnNS,
		PerPage:     perPage,
		Page:        page,
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	return result.Raw, nil
}
