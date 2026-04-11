package schema

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/resolvers/crds"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

// schemaTracer emits observation spans inside ValidateObjectStatus so we can
// separate the cheap cache lookup path from the expensive validator walk.
var schemaTracer = otel.Tracer("snowplow/resolvers/crds/schema")

const (
	widgetDataKey = "widgetData"
)

// validatedSchema holds a compiled SchemaValidator ready to use.
// The compiled validator is expensive to build (~40ms + 8KB per call);
// caching eliminates 47% of all allocations at 50K scale (pprof verified:
// NewSchemaValidator was 176GB cumulative allocs in v0.25.161).
type validatedSchema struct {
	crv       *apiextensions.CustomResourceValidation
	validator validation.SchemaValidator
}

type validatorKey struct {
	resource string
	group    string
	version  string
}

var (
	validatorCacheMu sync.RWMutex
	validatorCache   = make(map[validatorKey]*validatedSchema)
)

// InvalidateValidatorCache clears the cached validators. Should be called
// when a CRD is updated to pick up schema changes.
func InvalidateValidatorCache() {
	validatorCacheMu.Lock()
	validatorCache = make(map[validatorKey]*validatedSchema)
	validatorCacheMu.Unlock()
}

func ValidateObjectStatus(ctx context.Context, rc *rest.Config, obj map[string]any) error {
	gv := dynamic.GroupVersion(obj)
	gvr, err := dynamic.ResourceFor(rc, gv.WithKind(dynamic.GetKind(obj)))
	if err != nil {
		return err
	}

	// Use NestedFieldNoCopy to avoid DeepCopying the widgetData subtree.
	// The validator does not mutate its input — saves ~236MB heap at 50K.
	widgetDataRaw, ok, err := unstructured.NestedFieldNoCopy(obj, "status", widgetDataKey)
	if err != nil {
		return err
	}
	if !ok {
		name := dynamic.GetName(obj)
		return &apierrors.StatusError{
			ErrStatus: metav1.Status{
				Status: metav1.StatusFailure,
				Code:   http.StatusNotFound,
				Reason: metav1.StatusReasonNotFound,
				Details: &metav1.StatusDetails{
					Group: gvr.Group,
					Kind:  gvr.Resource,
					Name:  name,
				},
				Message: fmt.Sprintf("status.widgetData not found in %s %q", gvr.String(), name),
			}}
	}
	widgetData, _ := widgetDataRaw.(map[string]any)

	// Cache lookup: reuse the compiled SchemaValidator if we have one for
	// this CRD version. The previous cache (v0.25.161) only cached the CRV
	// but still called NewSchemaValidator on every invocation — proven by
	// pprof to be 47% of all allocations. Now we cache the compiled
	// validator itself.
	cacheKey := validatorKey{
		resource: gvr.Resource,
		group:    gvr.Group,
		version:  gvr.Version,
	}
	_, lookupSpan := schemaTracer.Start(ctx, "schema.lookup_crd",
		trace.WithAttributes(
			attribute.String("schema.resource", gvr.Resource),
			attribute.String("schema.group", gvr.Group),
		))
	validatorCacheMu.RLock()
	entry, cached := validatorCache[cacheKey]
	validatorCacheMu.RUnlock()
	lookupSpan.SetAttributes(attribute.Bool("schema.validator_cache_hit", cached))

	if !cached {
		crd, err := crds.Get(ctx, crds.GetOptions{
			RC:      rc,
			Name:    fmt.Sprintf("%s.%s", gvr.Resource, gvr.Group),
			Version: gvr.Version,
		})
		if err != nil {
			lookupSpan.End()
			return err
		}

		crv, err := extractOpenAPISchemaFromCRD(crd, gvr.Version)
		if err != nil {
			lookupSpan.End()
			return err
		}

		// Compile the validator once and cache it.
		sv, _, svErr := validation.NewSchemaValidator(crv.OpenAPIV3Schema)
		if svErr != nil {
			lookupSpan.End()
			return svErr
		}

		entry = &validatedSchema{
			crv:       crv,
			validator: sv,
		}
		validatorCacheMu.Lock()
		validatorCache[cacheKey] = entry
		validatorCacheMu.Unlock()
	}
	lookupSpan.End()

	_, runSpan := schemaTracer.Start(ctx, "schema.validate_object",
		trace.WithAttributes(
			attribute.Int("schema.widgetdata.keys", len(widgetData)),
		))
	err = validateCustomResourceWithCachedValidator(entry.validator, widgetData)
	runSpan.End()
	return err
}
