package api

import (
	"context"
	"log/slog"
	"net/http"

	httpcall "github.com/krateo-platformops/plumbing/http/request"
	"github.com/krateo-platformops/plumbing/jqutil"
	"github.com/krateo-platformops/plumbing/ptr"
	templates "github.com/krateo-platformops/snowplow/apis/templates/v1"
	jqsupport "github.com/krateo-platformops/snowplow/internal/support/jq"
)

// Ship 0.30.127: phase1IteratorCap (added 0.30.111) was DELETED. The cap
// truncated a `dependsOn.iterator` stage to its first 3 elements under a
// Phase-1 context — and that silently broke the Phase-1 navigation walk:
// the sidebar-nav-menu's apiRef RESTAction iterates a per-namespace
// navmenuitems LIST, and at bench scale the first 3 namespaces
// (bench-ns-01/02/03) hold ZERO navmenuitems — the real nav-menu-item-*
// CRs live in krateo-system, past the cap. The navmenu's
// resourcesRefsTemplate then expanded to zero children and the walk
// descended nothing past the roots (F2's warmed=2 defect, and a latent
// regression of #83). Iterator stages now expand FULLY; the storm guard
// for the expansion is the existing bounded errgroup
// (g.SetLimit(iterParallelism(ctx)), resolve.go) — no new mechanism.

func createRequestOptions(ctx context.Context, log *slog.Logger, in *templates.API, dict map[string]any) (all []httpcall.RequestOptions) {
	it := ""
	if in.DependsOn != nil {
		it = ptr.Deref(in.DependsOn.Iterator, "")
	}

	if len(it) == 0 {
		all = make([]httpcall.RequestOptions, 0, 1)
		el := createRequestOption(in, dict)
		all = append(all, el)
		return
	}

	all = []httpcall.RequestOptions{}

	action := func(sa any) error {
		el := createRequestOption(in, sa)
		all = append(all, el)
		return nil
	}

	err := jqutil.ForEach(ctx, jqutil.EvalOptions{Query: it, Unquote: true, Data: dict}, action)
	if err != nil {
		if jqsupport.IsBenignNilIteration(err) {
			// Iterator walked a null/absent upstream value → zero request
			// options; the stage continues exactly as the empty-iterator case
			// (C-3). Data-dependent and benign — DEBUG, not the ERROR that would
			// flood the WARN-floor firehose on a healthy cluster.
			log.Debug("iterator yielded no items (nil upstream)", slog.String("query", it), slog.Any("err", err))
		} else {
			log.Error("unable to execute iterator", slog.String("query", it), slog.Any("err", err))
		}
	}

	return all
}

func createRequestOption(in *templates.API, ds any) (out httpcall.RequestOptions) {
	out.ContinueOnError = ptr.Deref(in.ContinueOnError, false)
	out.ErrorKey = ptr.Deref(in.ErrorKey, "error")

	out.Path = evalJQ(in.Path, ds)
	out.Verb = ptr.To(ptr.Deref(in.Verb, http.MethodGet))

	if in.Payload != nil {
		out.Payload = ptr.To(evalJQ(*in.Payload, ds))
	}

	if in.Headers != nil {
		out.Headers = make([]string, 0, len(in.Headers))
		//copy(el.Headers, in.Headers)
		for _, h := range in.Headers {
			out.Headers = append(out.Headers, evalJQ(h, ds))
		}
	}

	return
}

func evalJQ(q string, ds any) string {
	q, ok := jqutil.MaybeQuery(q)
	if !ok {
		return q
	}

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
