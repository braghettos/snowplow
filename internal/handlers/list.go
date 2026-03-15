package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/kubeconfig"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// @Summary List resources by category in a specified namespace.
// @Description Resources List
// @ID list
// @Param  category         query   string  true  "Resource category"
// @Param  ns               query   string  false  "Namespace"
// @Produce  json
// @Success 200 {object} map[string]any
// @Failure 400 {object} response.Status
// @Failure 401 {object} response.Status
// @Failure 404 {object} response.Status
// @Failure 500 {object} response.Status
// @Router /list [get]
func List() http.HandlerFunc {
	return func(wri http.ResponseWriter, req *http.Request) {
		cat := req.URL.Query().Get("category")
		ns := req.URL.Query().Get("ns")

		if len(cat) == 0 {
			response.BadRequest(wri, fmt.Errorf("missing 'category' params"))
			return
		}

		log := xcontext.Logger(req.Context())
		start := time.Now()

		ep, err := xcontext.UserConfig(req.Context())
		if err != nil {
			log.Error("unable to get user endpoint", slog.Any("err", err))
			response.Unauthorized(wri, err)
			return
		}

		log.Debug("user config succesfully loaded", slog.Any("endpoint", ep))

		rc, err := kubeconfig.NewClientConfig(req.Context(), ep)
		if err != nil {
			log.Error("unable to create user client config", slog.Any("err", err))
			response.InternalError(wri, err)
			return
		}

		cli, err := dynamic.NewClient(rc)
		if err != nil {
			log.Error("cannot create dynamic client", slog.Any("err", err))
			response.InternalError(wri, err)
			return
		}

		c := cache.FromContext(req.Context())

		// Try discovery from cache first.
		var gvrs []schema.GroupVersionResource
		discoveryCacheKey := cache.DiscoveryKey(cat)
		if c != nil {
			if hit, rerr := c.Get(req.Context(), discoveryCacheKey, &gvrs); hit && rerr == nil {
				log.Debug("discovery cache hit", slog.String("category", cat))
			} else {
				gvrs = nil
			}
		}

		if len(gvrs) == 0 {
			log.Debug("performing discovery", slog.String("category", cat))
			discovered, err := cli.Discover(req.Context(), cat)
			if err != nil {
				log.Error("discovery failed", slog.Any("err", err))
				response.InternalError(wri, err)
				return
			}
			gvrs = discovered
			if c != nil && len(gvrs) > 0 {
				_ = c.Set(req.Context(), discoveryCacheKey, gvrs)
			}
		}

		log.Debug(fmt.Sprintf("discovery terminated (found: %d)", len(gvrs)))

		rt := []unstructured.Unstructured{}

		for _, gvr := range gvrs {
			listCacheKey := cache.ListKey(gvr, ns)

			// Register the GVR for dynamic informer watching early.
			if c != nil {
				_ = c.SAddGVR(req.Context(), gvr)
			}

			// Try list from shared cache.
			if c != nil {
				var cached unstructured.UnstructuredList
				if hit, rerr := c.Get(req.Context(), listCacheKey, &cached); hit && rerr == nil {
					cache.GlobalMetrics.ListHits.Add(1)
					log.Debug("list cache hit", slog.String("gvr", gvr.String()))
					for _, x := range cached.Items {
						unstructured.RemoveNestedField(x.UnstructuredContent(), "metadata", "managedFields")
						rt = append(rt, x)
					}
					continue
				}
				cache.GlobalMetrics.ListMisses.Add(1)
			}

			opts := dynamic.Options{
				Namespace: ns,
				GVR:       gvr,
			}

			obj, err := cli.List(req.Context(), opts)
			if err != nil {
				log.Error("cannot list resources",
					slog.String("gvr", gvr.String()), slog.Any("err", err))
				if apierrors.IsForbidden(err) {
					response.Forbidden(wri, err)
					return
				}
				continue
			}

			if c != nil && !c.Exists(req.Context(), listCacheKey) {
				_ = c.SetForGVR(req.Context(), gvr, listCacheKey, obj)
			}

			for _, x := range obj.Items {
				unstructured.RemoveNestedField(x.UnstructuredContent(), "metadata", "managedFields")
				rt = append(rt, x)
			}
		}

		log.Info("resources successfully listed",
			slog.String("category", cat),
			slog.String("namespace", ns),
			slog.String("duration", util.ETA(start)),
		)

		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(wri)
		enc.SetIndent("", "  ")
		enc.Encode(rt)
	}
}
