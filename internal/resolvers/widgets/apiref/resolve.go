package apiref

import (
	"context"
	"encoding/json"
	"fmt"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	xcontext "github.com/krateoplatformops/plumbing/context"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

type ResolveOptions struct {
	RC      *rest.Config
	ApiRef  templatesv1.ObjectReference
	AuthnNS string
	PerPage int
	Page    int
	Extras  map[string]any
}

// restActionGVR is the GVR under which RESTAction L1 entries are stored.
// Matches the key written by internal/handlers/dispatchers/restactions.go.
var restActionGVR = schema.GroupVersionResource{
	Group:    "templates.krateo.io",
	Version:  "v1",
	Resource: "restactions",
}

func Resolve(ctx context.Context, opts ResolveOptions) (map[string]any, error) {
	if opts.ApiRef.Name == "" || opts.ApiRef.Namespace == "" {
		return map[string]any{}, nil
	}

	// Try L1 cache first. When a widget depends on a RESTAction that has
	// already been resolved for this user (very common: the L1 refresher
	// resolves RESTActions BEFORE dependent widgets in the same refresh
	// cycle), reading the cached output is orders of magnitude faster than
	// re-running the full RESTAction pipeline (which reads all L3 items and
	// re-runs JQ from scratch). At 50K compositions this saves ~10-15s per
	// widget refresh.
	if c := cache.FromContext(ctx); c != nil {
		user, uerr := xcontext.UserInfo(ctx)
		if uerr == nil && user.Username != "" {
			l1Key := cache.ResolvedKey(user.Username, restActionGVR,
				opts.ApiRef.Namespace, opts.ApiRef.Name, opts.Page, opts.PerPage)
			if raw, hit, err := c.GetRaw(ctx, l1Key); err == nil && hit && len(raw) > 0 {
				// The cached value is the full RESTAction CR (marshalled).
				// Extract the status field and return its contents.
				var cached map[string]any
				if json.Unmarshal(raw, &cached) == nil {
					if status, ok := cached["status"].(map[string]any); ok {
						return status, nil
					}
				}
				// If unmarshal or status extraction fails, fall through to
				// the normal resolution path.
			}
		}
	}

	res := objects.Get(ctx, opts.ApiRef)
	if res.Err != nil {
		return map[string]any{}, fmt.Errorf("%s", res.Err.Message)
	}

	ra, err := convertToRESTAction(res.Unstructured.Object)
	if res.Err != nil {
		return map[string]any{}, err
	}

	raopts := restactions.ResolveOptions{
		In:      &ra,
		SArc:    opts.RC,
		AuthnNS: opts.AuthnNS,
		PerPage: opts.PerPage,
		Page:    opts.Page,
		Extras:  opts.Extras,
	}

	if _, err = restactions.Resolve(ctx, raopts); err != nil {
		return map[string]any{}, err
	}

	return rawExtensionToMap(ra.Status)
}
