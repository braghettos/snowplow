package restactions

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jqutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
)

// restactionResolveTracer emits observation spans for the RESTAction resolve
// pipeline: API-fetch phase and outer-filter JQ eval phase.
var restactionResolveTracer = otel.Tracer("snowplow/resolvers/restactions")

const (
	annotationKeyLastAppliedConfiguration = "kubectl.kubernetes.io/last-applied-configuration"
	annotationKeyVerboseAPI               = "krateo.io/verbose"
)

type ResolveOptions struct {
	In      *templates.RESTAction
	SArc    *rest.Config
	AuthnNS string
	PerPage int
	Page    int
	Extras  map[string]any
}

func Resolve(ctx context.Context, opts ResolveOptions) (*templates.RESTAction, error) {
	_, apiSpan := restactionResolveTracer.Start(ctx, "restaction.api_resolve")
	dict := api.Resolve(ctx, api.ResolveOptions{
		RC:      opts.SArc,
		AuthnNS: opts.AuthnNS,
		Verbose: isVerbose(opts.In),
		Items:   opts.In.Spec.API,
		PerPage: opts.PerPage,
		Page:    opts.Page,
		Extras:  opts.Extras,
	})
	if dict == nil {
		dict = map[string]any{}
	}
	apiSpan.SetAttributes(attribute.Int("restaction.dict_keys", len(dict)))
	apiSpan.End()

	log := xcontext.Logger(ctx)
	log.Debug("resolved api", slog.Int("dict_keys", len(dict)))

	var raw []byte
	if opts.In.Spec.Filter != nil {
		q := ptr.Deref(opts.In.Spec.Filter, "")
		_, jqSpan := restactionResolveTracer.Start(ctx, "restaction.jq.eval")
		jqSpan.SetAttributes(attribute.Int("restaction.filter_len", len(q)))
		s, err := jqutil.Eval(context.TODO(), jqutil.EvalOptions{
			Query: q, Data: dict,
			ModuleLoader: jqsupport.ModuleLoader(),
		})
		if err == nil {
			jqSpan.SetAttributes(attribute.Int("restaction.output_bytes", len(s)))
		}
		jqSpan.End()
		if err != nil {
			return opts.In, fmt.Errorf("unable to resolve filter: %w", err)
		}

		raw = []byte(s)
	} else {
		var err error
		raw, err = json.Marshal(dict)
		if err != nil {
			return opts.In, err
		}
	}

	opts.In.Status = &runtime.RawExtension{
		Raw: raw,
	}

	if opts.In.Annotations != nil {
		delete(opts.In.Annotations, annotationKeyLastAppliedConfiguration)
	}
	if opts.In.ManagedFields != nil {
		opts.In.ManagedFields = nil
	}

	return opts.In, nil
}

// IsVerbose returns true if the object has the AnnotationKeyConnectorVerbose
// annotation set to `true`.
func isVerbose(o metav1.Object) bool {
	return o.GetAnnotations()[annotationKeyVerboseAPI] == "true"
}
