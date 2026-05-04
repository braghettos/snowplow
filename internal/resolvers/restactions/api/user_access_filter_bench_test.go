package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// BenchmarkApplyUserAccessFilter_50Items — §9.6 gate (ns/op ≤ 5 ms).
//
// Realistic workload: 50 namespace items shaped like the response of
// `[ .namespaces.items[] | { metadata: .metadata } ]` (list-of-objects with
// a metadata.namespace field). Half are RBAC-allowed; the rest are denied.
//
// On a recent laptop (M-series) the helper completes in single-digit µs per
// invocation — orders of magnitude under the 5 ms SLO. The gate exists to
// catch a regression where the per-item jq path silently goes off the
// hot-cache (e.g. unbounded module loader, allocator regression).
func BenchmarkApplyUserAccessFilter_50Items(b *testing.B) {
	items := make([]any, 50)
	for i := range items {
		items[i] = map[string]any{
			"metadata": map[string]any{
				"namespace": fmt.Sprintf("tenant-%02d", i),
			},
		}
	}

	fr := newFakeRBAC()
	for i := 0; i < 50; i += 2 {
		fr.Allow("list", "core.krateo.io", "compositions", fmt.Sprintf("tenant-%02d", i))
	}

	// Discard logs — the audit log is cheap but emits per-call so we
	// don't want it bloating the benchmark.
	silent := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithLogger(silent),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "cyberjoker", Groups: []string{"devs"}}),
	)
	ctx = cache.WithRBACEvaluator(ctx, fr)

	api := &templates.API{
		Name: "compositions",
		UserAccessFilter: &templates.UserAccessFilter{
			Verb: "list", Group: "core.krateo.io", Resource: "compositions",
			NamespaceFrom: ptr.To(".metadata.namespace"),
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := applyUserAccessFilter(ctx, api, items)
		if got := out.([]any); len(got) != 25 {
			b.Fatalf("expected 25 kept, got %d", len(got))
		}
	}
}
