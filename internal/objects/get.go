package objects

import (
	"context"
	"log/slog"
	"net/http"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/kubeconfig"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	lastAppliedConfigAnnotation = "kubectl.kubernetes.io/last-applied-configuration"
)

type Result struct {
	GVR          schema.GroupVersionResource
	Unstructured *unstructured.Unstructured
	Err          *response.Status
}

func Get(ctx context.Context, ref templatesv1.ObjectReference) (res Result) {
	log := xcontext.Logger(ctx)

	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		log.Error("unable to parse group version", slog.Any("reference", ref), slog.Any("err", err))
		res.Err = response.New(http.StatusBadRequest, err)
		return
	}
	res.GVR = gv.WithResource(ref.Resource)

	if tracker := cache.TrackerFromContext(ctx); tracker != nil {
		tracker.AddGVR(res.GVR)
	}

	ep, err := xcontext.UserConfig(ctx)
	if err != nil {
		log.Error("unable to get user endpoint", slog.Any("err", err))
		res.Err = response.New(http.StatusUnauthorized, err)
		return
	}

	c := cache.FromContext(ctx)
	cacheKey := cache.GetKey(res.GVR, ref.Namespace, ref.Name)

	// Register the GVR for dynamic informer watching as early as possible so
	// the informer is started before the K8s API call returns.
	if c != nil {
		_ = c.SAddGVR(ctx, res.GVR)
	}

	// Negative cache check.
	if c != nil && c.GetNotFound(ctx, cacheKey) {
		cache.GlobalMetrics.Inc(&cache.GlobalMetrics.NegativeHits, "negative_hits")
		log.Debug("object not-found cache hit", slog.String("key", cacheKey))
		res.Err = response.New(http.StatusNotFound, apierrors.NewNotFound(schema.GroupResource{
			Group: res.GVR.Group, Resource: res.GVR.Resource,
		}, ref.Name))
		return
	}

	// Positive cache check.
	if c != nil {
		var cached unstructured.Unstructured
		if hit, rerr := c.Get(ctx, cacheKey, &cached); hit && rerr == nil {
			cache.GlobalMetrics.Inc(&cache.GlobalMetrics.GetHits, "get_hits")
			log.Debug("object cache hit", slog.String("key", cacheKey))
			res.Unstructured = &cached
			return
		}
		cache.GlobalMetrics.Inc(&cache.GlobalMetrics.GetMisses, "get_misses")
	}

	rc, err := kubeconfig.NewClientConfig(ctx, ep)
	if err != nil {
		log.Error("unable to create kubernetes client config", slog.Any("err", err))
		res.Err = response.New(http.StatusInternalServerError, err)
		return
	}

	cli, err := dynamic.NewClient(rc)
	if err != nil {
		log.Error("unable to create kubernetes dynamic client", slog.Any("err", err))
		res.Err = response.New(http.StatusInternalServerError, err)
		return
	}

	uns, err := cli.Get(ctx, ref.Name, dynamic.Options{
		Namespace: ref.Namespace,
		GVR:       res.GVR,
	})
	if err != nil {
		log.Error("unable to get resource",
			slog.String("name", ref.Name), slog.String("namespace", ref.Namespace),
			slog.String("gvr", res.GVR.String()), slog.Any("err", err))

		res.Err = response.New(http.StatusInternalServerError, err)
		if apierrors.IsForbidden(err) {
			res.Err = response.New(http.StatusForbidden, err)
		} else if apierrors.IsNotFound(err) {
			res.Err = response.New(http.StatusNotFound, err)
			if c != nil {
				_ = c.SetNotFound(ctx, cacheKey)
			}
		}

		return
	}

	annotations := uns.GetAnnotations()
	if annotations != nil {
		delete(annotations, lastAppliedConfigAnnotation)
		uns.SetAnnotations(annotations)
	}
	uns.SetManagedFields(nil)

	if c != nil {
		// Only store the K8s API response if the key is absent. The
		// ResourceWatcher may have already stored a fresher version while
		// this request was in-flight (e.g., a mutation event arrived).
		if !c.Exists(ctx, cacheKey) {
			_ = c.SetForGVR(ctx, res.GVR, cacheKey, uns)
		}
	}

	res.Unstructured = uns
	res.Err = nil
	return
}
