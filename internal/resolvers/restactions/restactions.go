package restactions

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jqutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
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
	// SnowplowEndpoint is the elevated-call provider used by api.Resolve
	// when a UserAccessFilter is present on an api[] entry. Threaded
	// through l1cache.Input from the dispatcher constructors.
	// See api.ResolveOptions.SnowplowEndpoint for the contract.
	SnowplowEndpoint func() (*endpoints.Endpoint, error)

	// SnowplowK8sClient is the in-cluster dynamic K8s client used by the
	// SA-elevated dispatch path (Q-RBAC-DECOUPLE C(d) v6 — Path B).
	// Replaces httpcall.Do for SA dispatch, structurally avoiding
	// plumbing's tlsConfigFor bug. nil-safe: api.Resolve falls back to
	// cache.SnowplowK8sFromContext(ctx) before erroring.
	SnowplowK8sClient dynamic.Client
}

// Resolve runs the per-api fan-out (api.Resolve) and the outer JQ filter,
// populating opts.In.Status.Raw with the final wire-shape body.
//
// Q-RBAC-DECOUPLE C(d) v3 — additionally returns the unfiltered
// per-api intermediate `dict` (the ProtectedDict in spec terms) so
// l1cache.ResolveAndCache can stash it inside a CachedRESTAction wrapper
// for per-user refiltering at HTTP-time. Callers that don't need it (the
// inline / no-cache fast path in apiref) ignore the third return.
//
// SECURITY: opts.In.Status.Raw is the OUTER-JQ output computed against
// the UNFILTERED dict (no applyUserAccessFilter on protected slots —
// those wraps moved to api.RefilterRESTAction). Anything that serves
// opts.In.Status.Raw as a wire body MUST go through RefilterRESTAction
// first or it leaks the unfiltered shape. Production wiring satisfies
// this via the dispatcher refilter at internal/handlers/dispatchers/
// restactions.go and internal/resolvers/widgets/apiref/resolve.go.
func Resolve(ctx context.Context, opts ResolveOptions) (*templates.RESTAction, map[string]any, error) {
	// Attach RESTAction name to ctx for audit/observability helpers in
	// the api package (e.g. applyUserAccessFilter audit log via
	// cache.RESTActionNameFromContext). Per Q-RBACC-IMPL-2.
	ctx = cache.WithRESTActionName(ctx, opts.In.GetName())

	_, apiSpan := restactionResolveTracer.Start(ctx, "restaction.api_resolve")
	dict := api.Resolve(ctx, api.ResolveOptions{
		RC:                opts.SArc,
		AuthnNS:           opts.AuthnNS,
		Verbose:           isVerbose(opts.In),
		Items:             opts.In.Spec.API,
		PerPage:           opts.PerPage,
		Page:              opts.Page,
		Extras:            opts.Extras,
		SnowplowEndpoint:  opts.SnowplowEndpoint,
		SnowplowK8sClient: opts.SnowplowK8sClient,
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
		// gojq-purity-required: `dict` is the resolved-API output map
		// owned by this goroutine. Every informer-sourced value inside
		// it has already been safeCopyJSON'd by api.Resolve before
		// landing here (see internal/resolvers/restactions/api/resolve.go
		// at the informer_direct site). gojq is free to mutate.
		s, err := jqutil.Eval(context.TODO(), jqutil.EvalOptions{
			Query: q, Data: dict,
			ModuleLoader: jqsupport.ModuleLoader(),
		})
		if err == nil {
			jqSpan.SetAttributes(attribute.Int("restaction.output_bytes", len(s)))
		}
		jqSpan.End()
		if err != nil {
			return opts.In, dict, fmt.Errorf("unable to resolve filter: %w", err)
		}

		raw = []byte(s)
	} else {
		var err error
		raw, err = json.Marshal(dict)
		if err != nil {
			return opts.In, dict, err
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

	return opts.In, dict, nil
}

// IsVerbose returns true if the object has the AnnotationKeyConnectorVerbose
// annotation set to `true`.
func isVerbose(o metav1.Object) bool {
	return o.GetAnnotations()[annotationKeyVerboseAPI] == "true"
}
