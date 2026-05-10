package dynamic

import (
	"context"
	"log/slog"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/snowplow/internal/cache"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ListObjects is the cache-gated wrapper around Client.List. At 0.30.1
// cache.Disabled() defaults to true so every call falls through to the
// apiserver branch. The gate is in place so the 0.30.2 ship can flip
// the routed branch on without touching every call site.
//
// Per feedback_no_special_cases.md the gate is identical for every GVR.
func ListObjects(ctx context.Context, cli Client, opts Options) (*unstructured.UnstructuredList, error) {
	log := xcontext.Logger(ctx)

	if cache.Disabled() {
		return cli.List(ctx, opts)
	}

	// Routed branch — placeholder for 0.30.2 wiring. Falls through to
	// the apiserver branch at 0.30.1 because the cache subsystem is
	// dormant and no ResourceWatcher is wired into this call site yet.
	log.Debug("dynamic.ListObjects: cache routed but no watcher injected; falling back to apiserver",
		slog.String("gvr", opts.GVR.String()),
		slog.String("namespace", opts.Namespace))
	return cli.List(ctx, opts)
}
