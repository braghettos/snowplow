package dispatchers

import (
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
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
				if raw, hit, _ := c.GetRaw(req.Context(), resolvedKey); hit {
					cache.GlobalMetrics.RawHits.Add(1)
					cache.GlobalMetrics.L1Hits.Add(1)
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
					return
				}
			cache.GlobalMetrics.RawMisses.Add(1)
			cache.GlobalMetrics.L1Misses.Add(1)
				log.Info("widget: L1 miss", slog.String("key", resolvedKey))
			}
		}
	}
	// ── End resolved-output cache ─────────────────────────────────────────────

	got := fetchObject(req)
	if got.Err != nil {
		response.Encode(wri, got.Err)
		return
	}

	log = log.With(
		slog.Group("widget",
			slog.String("name", widgets.GetName(got.Unstructured.Object)),
			slog.String("namespace", widgets.GetNamespace(got.Unstructured.Object)),
			slog.String("apiVersion", widgets.GetAPIVersion(got.Unstructured.Object)),
			slog.String("kind", widgets.GetKind(got.Unstructured.Object)),
		),
	)

	tracker := cache.NewDependencyTracker()
	ctx := cache.WithDependencyTracker(xcontext.BuildContext(req.Context()), tracker)

	res, err := widgets.Resolve(ctx, widgets.ResolveOptions{
		In:      got.Unstructured,
		AuthnNS: r.authnNS,
		PerPage: perPage,
		Page:    page,
		Extras:  extras,
	})
	if err != nil {
		log.Error("unable to resolve widget", slog.Any("err", err))
		var statusErr *apierrors.StatusError
		if errors.As(err, &statusErr) {
			code := int(statusErr.Status().Code)
			msg := fmt.Errorf("%s", statusErr.Status().Message)
			response.Encode(wri, response.New(code, msg))
			return
		}
		response.InternalError(wri, err)
		return
	}

	traceId := xcontext.TraceId(ctx, false)
	if traceId != "" {
		if terr := maps.SetNestedField(res.Object, traceId, "status", "traceId"); terr != nil {
			log.Warn("unable to set traceId in status", slog.Any("err", terr))
		}
	}

	log.Info("Widget successfully resolved",
		slog.String("duration", util.ETA(start)),
	)

	raw, merr := json.Marshal(res)
	if merr != nil {
		response.InternalError(wri, merr)
		return
	}

	// Populate the resolved cache and register GVR reverse indexes so that
	// informer events can do targeted invalidation instead of bulk deletes.
	if c != nil && resolvedKey != "" {
		_ = c.SetResolvedRaw(req.Context(), resolvedKey, raw)
		for _, gvrKey := range tracker.GVRKeys() {
			_ = c.SAddWithTTL(req.Context(), cache.L1GVRKey(gvrKey), resolvedKey, cache.ReverseIndexTTL)
		}

		preWarmChildWidgets(req.Context(), c, res, r.authnNS)
	}

	wri.Header().Set("Content-Type", "application/json")
	wri.WriteHeader(http.StatusOK)
	_, _ = wri.Write(raw)
}
