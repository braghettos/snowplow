package schema

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/resolvers/crds"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

const (
	widgetDataKey = "widgetData"
)

// validatorCache memoises compiled OpenAPI validators per (gvr, version).
// The CRD schema changes rarely compared to widget resolutions, so re-parsing
// the schema on every L1 refresh wastes ~490MB of heap at 50K scale (pprof
// verified on v0.25.160: ValidateObjectStatus cum = 490MB).
type validatorKey struct {
	resource string
	group    string
	version  string
}

var (
	validatorCacheMu sync.RWMutex
	validatorCache   = make(map[validatorKey]*apiextensions.CustomResourceValidation)
)

// InvalidateValidatorCache clears the cached validators. Should be called
// when a CRD is updated (e.g. from an informer event on the CRD GVR).
func InvalidateValidatorCache() {
	validatorCacheMu.Lock()
	validatorCache = make(map[validatorKey]*apiextensions.CustomResourceValidation)
	validatorCacheMu.Unlock()
}

func ValidateObjectStatus(ctx context.Context, rc *rest.Config, obj map[string]any) error {
	gv := dynamic.GroupVersion(obj)
	gvr, err := dynamic.ResourceFor(rc, gv.WithKind(dynamic.GetKind(obj)))
	if err != nil {
		return err
	}

	// Use NestedFieldNoCopy to avoid DeepCopying the widgetData subtree.
	// The validator does not mutate its input — this saves ~236MB heap at
	// 50K scale (pprof verified: unstructured.NestedMap 236MB cum).
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

	// Cache lookup: if we already compiled the validator for this CRD
	// version, reuse it. This is the biggest single hotspot at 50K
	// (490MB cum on refreshSingleL1 path).
	cacheKey := validatorKey{
		resource: gvr.Resource,
		group:    gvr.Group,
		version:  gvr.Version,
	}
	validatorCacheMu.RLock()
	crv, cached := validatorCache[cacheKey]
	validatorCacheMu.RUnlock()

	if !cached {
		crd, err := crds.Get(ctx, crds.GetOptions{
			RC:      rc,
			Name:    fmt.Sprintf("%s.%s", gvr.Resource, gvr.Group),
			Version: gvr.Version,
		})
		if err != nil {
			return err
		}

		crv, err = extractOpenAPISchemaFromCRD(crd, gvr.Version)
		if err != nil {
			return err
		}

		validatorCacheMu.Lock()
		validatorCache[cacheKey] = crv
		validatorCacheMu.Unlock()
	}

	return validateCustomResource(crv, widgetData)
}
