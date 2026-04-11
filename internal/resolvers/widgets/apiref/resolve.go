package apiref

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions"
	"golang.org/x/sync/singleflight"
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

// apiRefFlight dedups concurrent widget apiref resolutions for the same L1
// key. Without this, N concurrent widgets that share one RESTAction (e.g.
// dashboard piechart + table both reading compositions-list) each kick off
// the full 32 s L3→JQ aggregation, hammering memory and CPU.
//
// This singleflight is local to the apiref package and does NOT dedup with
// the RESTAction HTTP dispatcher's group (dispatchers/restactions.go) — the
// dispatcher path is rarely hit in practice since widgets never go through
// it, so cross-path dedup is not worth the import graph complexity.
var apiRefFlight singleflight.Group

func Resolve(ctx context.Context, opts ResolveOptions) (map[string]any, error) {
	if opts.ApiRef.Name == "" || opts.ApiRef.Namespace == "" {
		return map[string]any{}, nil
	}

	c := cache.FromContext(ctx)
	var l1Key string
	if c != nil {
		user, uerr := xcontext.UserInfo(ctx)
		if uerr == nil && user.Username != "" {
			l1Key = cache.ResolvedKey(user.Username, restActionGVR,
				opts.ApiRef.Namespace, opts.ApiRef.Name, opts.Page, opts.PerPage)
			// L1 fast path: when a widget depends on a RESTAction that has
			// already been resolved for this user, reading the cached output
			// is orders of magnitude faster than re-running the full
			// RESTAction pipeline. At 50K compositions this saves ~10-15s
			// per widget refresh.
			if status, ok := lookupL1(ctx, c, l1Key); ok {
				return status, nil
			}
		}
	}

	// ── L1 miss: go through singleflight so concurrent widgets for the
	// same RESTAction share one resolution. The singleflight key is the L1
	// key, so requests for different pages or different users don't collide.
	if l1Key != "" {
		sfCtx := context.WithoutCancel(ctx)
		result, err, _ := apiRefFlight.Do(l1Key, func() (interface{}, error) {
			return resolveAndCache(sfCtx, c, opts, l1Key)
		})
		if err != nil {
			return map[string]any{}, err
		}
		status, _ := result.(map[string]any)
		if status == nil {
			return map[string]any{}, nil
		}
		return status, nil
	}

	// No cache context: resolve inline without singleflight or L1 write.
	return resolveInline(ctx, opts)
}

// resolveAndCache runs the full RESTAction resolution and writes the result
// into L1 with dependency tracking. Called inside apiRefFlight so concurrent
// callers share one execution.
func resolveAndCache(ctx context.Context, c *cache.RedisCache, opts ResolveOptions, l1Key string) (map[string]any, error) {
	// Recheck L1 inside singleflight: another flight may have just finished
	// and populated it between our miss check and our flight acquire.
	if status, ok := lookupL1(ctx, c, l1Key); ok {
		return status, nil
	}

	res := objects.Get(ctx, opts.ApiRef)
	if res.Err != nil {
		return nil, fmt.Errorf("%s", res.Err.Message)
	}

	ra, err := convertToRESTAction(res.Unstructured.Object)
	if err != nil {
		return nil, err
	}

	// Wrap ctx with a dependency tracker so restactions.Resolve records
	// which L3 reads it made. We then register these as L1 deps so watch
	// events can invalidate this L1 key when any of its inputs change.
	tracker := cache.NewDependencyTracker()
	tctx := cache.WithDependencyTracker(ctx, tracker)

	if _, err = restactions.Resolve(tctx, restactions.ResolveOptions{
		In:      &ra,
		SArc:    opts.RC,
		AuthnNS: opts.AuthnNS,
		PerPage: opts.PerPage,
		Page:    opts.Page,
		Extras:  opts.Extras,
	}); err != nil {
		return nil, err
	}

	// Write the resolved RESTAction CR to L1, mirroring what the RESTAction
	// HTTP dispatcher does (dispatchers/restactions.go:resolveRESTActionFromObject).
	// Without this, every subsequent widget call for the same user+RESTAction
	// would re-run the full L3→JQ aggregation (N+1 problem across paginated
	// widget pages).
	if raw, merr := json.Marshal(&ra); merr == nil {
		raw = cache.StripBulkyAnnotations(raw)
		if serr := c.SetResolvedRaw(tctx, l1Key, raw); serr != nil {
			slog.Warn("apiref: SetResolvedRaw failed",
				slog.String("key", l1Key), slog.Any("err", serr))
		}
		cache.RegisterL1Dependencies(tctx, c, tracker, l1Key)
		cache.RegisterL1ApiDeps(tctx, c, l1Key, extractAPIRequests(raw))
	} else {
		slog.Warn("apiref: marshal RESTAction failed, skipping L1 write",
			slog.String("key", l1Key), slog.Any("err", merr))
	}

	return rawExtensionToMap(ra.Status)
}

// resolveInline is the non-cached fast path: no L1 lookup, no L1 write,
// no singleflight. Used when there is no cache context (tests, etc).
func resolveInline(ctx context.Context, opts ResolveOptions) (map[string]any, error) {
	res := objects.Get(ctx, opts.ApiRef)
	if res.Err != nil {
		return map[string]any{}, fmt.Errorf("%s", res.Err.Message)
	}

	ra, err := convertToRESTAction(res.Unstructured.Object)
	if err != nil {
		return map[string]any{}, err
	}

	if _, err = restactions.Resolve(ctx, restactions.ResolveOptions{
		In:      &ra,
		SArc:    opts.RC,
		AuthnNS: opts.AuthnNS,
		PerPage: opts.PerPage,
		Page:    opts.Page,
		Extras:  opts.Extras,
	}); err != nil {
		return map[string]any{}, err
	}

	return rawExtensionToMap(ra.Status)
}

// lookupL1 reads a RESTAction L1 entry and extracts its status map.
// Returns (status, true) on hit, (nil, false) on miss or decode failure.
func lookupL1(ctx context.Context, c *cache.RedisCache, l1Key string) (map[string]any, bool) {
	raw, hit, err := c.GetRaw(ctx, l1Key)
	if err != nil || !hit || len(raw) == 0 {
		return nil, false
	}
	var cached map[string]any
	if json.Unmarshal(raw, &cached) != nil {
		return nil, false
	}
	status, ok := cached["status"].(map[string]any)
	if !ok {
		return nil, false
	}
	return status, true
}

// extractAPIRequests extracts the "apiRequests" string array from the
// resolved RESTAction JSON output. Mirror of the helper in
// dispatchers/restactions.go — duplicated here to keep the import graph
// acyclic (resolvers must not import handlers). Returns nil if the field
// is missing or not an array.
func extractAPIRequests(raw []byte) []string {
	var wrapper struct {
		Status json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil || wrapper.Status == nil {
		return nil
	}
	var statusMap map[string]json.RawMessage
	if err := json.Unmarshal(wrapper.Status, &statusMap); err != nil {
		return nil
	}
	reqsRaw, ok := statusMap["apiRequests"]
	if !ok {
		return nil
	}
	var reqs []string
	if err := json.Unmarshal(reqsRaw, &reqs); err != nil {
		return nil
	}
	return reqs
}
