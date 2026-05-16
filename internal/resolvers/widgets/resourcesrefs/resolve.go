package resourcesrefs

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	xcontext "github.com/krateoplatformops/plumbing/context"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/rbac"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

func Resolve(ctx context.Context, items []templatesv1.ResourceRef) ([]templatesv1.ResourceRefResult, error) {
	ep, err := xcontext.UserConfig(ctx)
	if err != nil {
		return nil, err
	}

	// 0.30.103: ClientConfigFor returns the context-injected
	// internal-dispatch *rest.Config when an internal/startup driver
	// (Phase 1's SA-credentialed walk) is in flight, else delegates to
	// the unchanged kubeconfig.NewClientConfig per-user path — see
	// cache.WithInternalRESTConfig.
	rc, err := cache.ClientConfigFor(ctx, ep)
	if err != nil {
		return nil, err
	}

	results := []templatesv1.ResourceRefResult{}
	for _, el := range items {
		res, err2 := resolveOne(ctx, rc, &el)
		if err2 != nil {
			err = errors.Join(err, err2)
			continue
		}

		results = append(results, res...)
	}

	return results, nil
}

func resolveOne(ctx context.Context, rc *rest.Config, in *templatesv1.ResourceRef) ([]templatesv1.ResourceRefResult, error) {
	all := []templatesv1.ResourceRefResult{}
	if in == nil {
		return all, nil
	}

	log := xcontext.Logger(ctx)

	gv, err := schema.ParseGroupVersion(in.APIVersion)
	if err != nil {
		return all, err
	}
	gvr := gv.WithResource(in.Resource)

	gvk, err := dynamic.KindFor(rc, gvr)
	if err != nil {
		return all, err
	}

	log.Info("resolving resource ref",
		slog.String("id", in.ID),
		slog.String("group", gvr.Group),
		slog.String("name", in.Name),
		slog.String("namespace", in.Namespace),
	)

	verbs := mapVerbs(in.Verb)
	for _, verb := range verbs {
		el := templatesv1.ResourceRefResult{
			ID:   in.ID,
			Verb: kubeToREST[verb],
		}

		el.Allowed = rbac.UserCan(ctx, rbac.UserCanOptions{
			Verb:          verb,
			GroupResource: gvr.GroupResource(),
			Namespace:     in.Namespace,
		})
		if !el.Allowed {
			log.Warn("resource ref action not allowed",
				slog.String("id", in.ID),
				slog.String("verb", verb),
				slog.String("group", gvr.Group),
				slog.String("resource", gvr.Resource),
				slog.String("namespace", in.Namespace))
		}

		el.Path = buildPath(gvr, in)

		if el.Verb == http.MethodPost || el.Verb == http.MethodPut || el.Verb == http.MethodPatch {
			el.Payload = &templatesv1.ResourceRefPayload{
				Kind:       gvk.Kind,
				APIVersion: in.APIVersion,
				MetaData: &templatesv1.Reference{
					Name:      in.Name,
					Namespace: in.Namespace,
				},
			}
		}

		all = append(all, el)

		log.Info("resource ref successfully resolved",
			slog.String("id", in.ID),
			slog.String("group", gvr.Group),
			slog.String("name", in.Name),
			slog.String("namespace", in.Namespace),
			slog.String("verb", verb),
			slog.String("path", el.Path),
			slog.Bool("allowed", el.Allowed),
		)
	}

	return all, nil
}

func buildPath(gvr schema.GroupVersionResource, in *templatesv1.ResourceRef) string {
	u := url.URL{
		Path: "/call",
	}

	q := url.Values{}
	q.Set("resource", gvr.Resource)
	q.Set("apiVersion", gvr.GroupVersion().String())
	q.Set("namespace", in.Namespace)

	if in.Name != "" {
		q.Set("name", in.Name)
	}

	if slice := in.Slice; slice != nil {
		q.Set("page", strconv.Itoa(slice.Page))
		q.Set("perPage", strconv.Itoa(slice.PerPage))
	}

	u.RawQuery = q.Encode()
	return u.String()
}
