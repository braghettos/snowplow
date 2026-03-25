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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
)

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

// ResolveResult carries the resolved RESTAction along with the raw API
// request paths collected during resolution (before the JQ filter strips them).
type ResolveResult struct {
	Action      *templates.RESTAction
	APIRequests []string
}

func Resolve(ctx context.Context, opts ResolveOptions) (*ResolveResult, error) {
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

	log := xcontext.Logger(ctx)
	log.Debug("resolved api", slog.Any("dict", dict))

	// Extract apiRequests BEFORE the JQ filter strips them.
	var apiRequests []string
	if reqs, ok := dict["apiRequests"]; ok {
		if arr, ok := reqs.([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					apiRequests = append(apiRequests, s)
				}
			}
		}
	}

	var raw []byte
	if opts.In.Spec.Filter != nil {
		q := ptr.Deref(opts.In.Spec.Filter, "")
		s, err := jqutil.Eval(context.TODO(), jqutil.EvalOptions{
			Query: q, Data: dict,
			ModuleLoader: jqsupport.ModuleLoader(),
		})
		if err != nil {
			return &ResolveResult{Action: opts.In}, fmt.Errorf("unable to resolve filter: %w", err)
		}

		raw = []byte(s)
	} else {
		var err error
		raw, err = json.Marshal(dict)
		if err != nil {
			return &ResolveResult{Action: opts.In}, err
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

	return &ResolveResult{Action: opts.In, APIRequests: apiRequests}, nil
}

// IsVerbose returns true if the object has the AnnotationKeyConnectorVerbose
// annotation set to `true`.
func isVerbose(o metav1.Object) bool {
	return o.GetAnnotations()[annotationKeyVerboseAPI] == "true"
}
