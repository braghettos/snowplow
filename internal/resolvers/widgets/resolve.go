package widgets

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"

	xcontext "github.com/krateoplatformops/plumbing/context"
	xenv "github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/maps"
	v1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	crdschema "github.com/krateoplatformops/snowplow/internal/resolvers/crds/schema"

	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets/apiref"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets/resourcesrefs"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets/resourcesrefstemplate"

	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets/widgetdatatemplate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

type Widget = unstructured.Unstructured

type ResolveOptions struct {
	In      *Widget
	RC      *rest.Config
	AuthnNS string
	PerPage int
	Page    int
	Extras  map[string]any
}

func Resolve(ctx context.Context, opts ResolveOptions) (*Widget, error) {
	log := xcontext.Logger(ctx).With(loggerAttr(opts.In.Object))

	ds, err := resolveApiRef(ctx, opts)
	if err != nil {
		log.Error("unable to resolve api reference", slog.Any("err", err))
		maps.SetNestedField(opts.In.Object, err.Error(), "status", "error")
		return opts.In, err
	}

	widgetData, err := resolveWidgetData(ctx, opts.In, ds)
	if err != nil {
		log.Error("unable to resolve widget data", slog.Any("err", err))
		maps.SetNestedField(opts.In.Object, err.Error(), "status", "error")
		return opts.In, err
	}

	err = maps.SetNestedField(opts.In.Object, widgetData, "status", widgetDataKey)
	if err != nil {
		log.Error("unable to set status as unstructured.NestedMap",
			slog.Any("err", err))
		return opts.In, err
	}

	resourcesRefsResults, err := resolveResourceRefs(ctx, opts.In, ds)
	if err != nil {
		maps.SetNestedField(opts.In.Object, err.Error(), "status", "error")
		return opts.In, err
	}

	if tot := len(resourcesRefsResults); tot > 0 {
		tmp, err := maps.StructSliceToMapSlice(resourcesRefsResults)
		if err != nil {
			return opts.In, err
		}

		pig := map[string]any{
			"items": tmp,
		}
		if opts.PerPage > 0 && opts.Page > 0 {
			hasNext := (tot >= opts.PerPage)
			page := opts.Page
			if hasNext {
				page = page + 1
			}
			pig["slice"] = map[string]any{
				"perPage":  opts.PerPage,
				"page":     page,
				"continue": hasNext,
			}
		}

		err = maps.SetNestedField(opts.In.Object, pig, "status", resourcesRefsKey)
		if err != nil {
			return opts.In, err
		}
	}

	if xenv.TestMode() {
		err = crdschema.ValidateObjectStatus(ctx, opts.RC, opts.In.Object)
	} else {
		err = crdschema.ValidateObjectStatus(ctx, nil, opts.In.Object)
	}
	if err != nil {
		maps.SetNestedField(opts.In.Object, err.Error(), "status", "error")
		return opts.In, &apierrors.StatusError{
			ErrStatus: metav1.Status{
				Status:  metav1.StatusFailure,
				Code:    http.StatusBadRequest,
				Reason:  metav1.StatusReasonBadRequest,
				Message: err.Error(),
			}}
	}

	return opts.In, nil
}

func resolveApiRef(ctx context.Context, opts ResolveOptions) (map[string]any, error) {
	apiRef, err := GetApiRef(opts.In.Object)
	if err != nil {
		return nil, err
	}

	return apiref.Resolve(ctx, apiref.ResolveOptions{
		RC:      opts.RC,
		ApiRef:  apiRef,
		AuthnNS: opts.AuthnNS,
		PerPage: opts.PerPage,
		Page:    opts.Page,
	})
}

func resolveWidgetData(ctx context.Context, obj *Widget, ds map[string]any) (map[string]any, error) {
	log := xcontext.Logger(ctx)

	src := GetWidgetData(obj.Object)

	wdt, err := GetWidgetDataTemplate(obj.Object)
	if err != nil {
		log.Warn("unable to get widgetDataTemplate", slog.Any("err", err))
		return src, nil
	}

	evals, err := widgetdatatemplate.Resolve(ctx, widgetdatatemplate.ResolveOptions{
		Items:      wdt,
		DataSource: ds,
	})
	if err != nil {
		log.Error("unable to resolve widgetDataTemplate", slog.Any("err", err))
		return src, err
	}
	log.Debug("widgetDataTemplate JQ evaluation complete", slog.Int("count", len(evals)))

	for _, el := range evals {
		fields := maps.ParsePath(el.Path)
		if len(fields) == 0 {
			continue
		}

		log.Debug("widgetDataTemplate setting nested value",
			slog.String("path", el.Path),
			slog.Any("type", reflect.TypeOf(el.Value)),
		)

		err = maps.SetNestedValue(src, fields, el.Value)
		if err != nil {
			log.Error("unable to set nested value",
				slog.Any("fields", fields),
				slog.Any("value", el.Value),
				slog.Any("valueType", reflect.TypeOf(el.Value)),
				slog.Any("err", err))
			return src, err
		}
	}

	return src, nil
}

func resolveResourceRefs(ctx context.Context, obj *Widget, ds map[string]any) ([]v1.ResourceRefResult, error) {
	log := xcontext.Logger(ctx)

	all := []v1.ResourceRef{}

	resrefs, err := GetResourcesRefs(obj.Object)
	if err != nil {
		log.Warn("unable to get resources references", slog.Any("err", err))
	} else {
		all = append(all, resrefs...)
	}

	resrefstpl, err := GetResourcesRefsTemplate(obj.Object)
	if err != nil {
		log.Warn("unable to get resource references template", slog.Any("err", err))
	}
	if len(resrefstpl) > 0 {
		resrefsExtra, err := resourcesrefstemplate.Resolve(ctx, resrefstpl, ds)
		if err != nil {
			return nil, err
		} else {
			all = append(all, resrefsExtra...)
		}
	}

	return resourcesrefs.Resolve(ctx, all)
}
