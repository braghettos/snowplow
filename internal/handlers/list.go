package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/kubeconfig"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

		log.Debug("performing discovery", slog.String("category", cat))
		res, err := cli.Discover(context.Background(), cat)
		if err != nil {
			log.Error("discovery failed", slog.Any("err", err))
			response.InternalError(wri, err)
			return
		}
		log.Debug(fmt.Sprintf("discovery terminated (found: %d)", len(res)))

		rt := []unstructured.Unstructured{}

		for _, gvr := range res {
			opts := dynamic.Options{
				Namespace: ns,
				GVR:       gvr,
			}

			obj, err := dynamic.ListObjects(req.Context(), cli, opts)
			if err != nil {
				log.Error("cannot list resources",
					slog.String("gvr", gvr.String()), slog.Any("err", err))
				if apierrors.IsForbidden(err) {
					response.Forbidden(wri, err)
					return
				}
				continue
			}

			for _, x := range obj.Items {
				unstructured.RemoveNestedField(
					x.UnstructuredContent(), "metadata", "managedFields")
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
