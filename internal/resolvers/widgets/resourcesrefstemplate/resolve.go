package resourcesrefstemplate

import (
	"context"
	"errors"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jqutil"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
	"k8s.io/utils/ptr"
)

type ResolveOptions struct {
	Items []templatesv1.ResourceRefTemplate
	Dict  map[string]any
}

func Resolve(ctx context.Context, items []templatesv1.ResourceRefTemplate, ds map[string]any) ([]templatesv1.ResourceRef, error) {
	all := []templatesv1.ResourceRef{}

	var errs []error
	for _, el := range items {
		tmp, err := createResourceReferencesFromTemplate(ctx, &el, ds)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(tmp) > 0 {
			all = append(all, tmp...)
		}
	}

	return all, errors.Join(errs...)
}

func createResourceReferencesFromTemplate(ctx context.Context, in *templatesv1.ResourceRefTemplate, ds map[string]any) (all []templatesv1.ResourceRef, err error) {
	it := ptr.Deref(in.Iterator, "")
	q, ok := jqutil.MaybeQuery(it)
	if !ok || q == "" {
		log := xcontext.Logger(ctx)
		log.Warn("bad or empty iterator", slog.String("iterator", it))

		all = make([]templatesv1.ResourceRef, 0, 1)
		el := createResourceRef(in, ds)
		all = append(all, el)
		return
	}

	all = []templatesv1.ResourceRef{}

	action := func(sa any) error {
		el := createResourceRef(in, sa)
		all = append(all, el)
		return nil
	}

	err = jqutil.ForEach(context.TODO(), jqutil.EvalOptions{
		Query: q, Unquote: true, Data: ds,
	}, action)
	if err != nil {
		log := xcontext.Logger(ctx)
		log.Error("unable to execute iterator", slog.String("iterator", it), slog.Any("err", err))
	}

	return
}

func createResourceRef(in *templatesv1.ResourceRefTemplate, ds any) (out templatesv1.ResourceRef) {
	out.ID = evalJQ(in.Template.ID, ds)
	out.Verb = in.Template.Verb

	out.APIVersion = evalJQ(in.Template.APIVersion, ds)
	out.Name = evalJQ(in.Template.Name, ds)
	out.Namespace = evalJQ(in.Template.Namespace, ds)
	out.Resource = evalJQ(in.Template.Resource, ds)

	return
}

func evalJQ(q string, ds any) string {
	q, ok := jqutil.MaybeQuery(q)
	if !ok {
		return q
	}

	// gojq-purity-required: `ds` is the per-call resourceRef template
	// data (caller-owned struct, not informer-aliased). gojq is free to
	// mutate.
	out, err := jqutil.Eval(context.TODO(),
		jqutil.EvalOptions{
			Query:        q,
			Unquote:      true,
			Data:         ds,
			ModuleLoader: jqsupport.ModuleLoader(),
		})
	if err != nil {
		out = err.Error()
	}

	return out
}
