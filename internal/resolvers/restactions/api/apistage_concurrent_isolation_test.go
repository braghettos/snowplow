// apistage_concurrent_isolation_test.go — Ship 0.30.130 pre-flight
// falsifier for the corrected P-CORE-2 (per-call value isolation).
//
// THE BUG (P-CORE-2 as shipped in 0.30.128, un-reverted, still on the
// branch under 0.30.129): the api-stage content L1 stores a LIST entry
// with R3 pre-parsed Items ([]*unstructured.Unstructured). On every
// content-Get-HIT gateListItems -> listEnvelopeValue assembles the
// served envelope by appending each it.Object DIRECTLY — it.Object is
// the SHARED map[string]any owned by the cached entry.Items. Every
// concurrent Get-hit therefore receives a value tree that ALIASES the
// cached entry's maps.
//
// jsonHandlerCore feeds that aliased tree into jqutil.Eval (Data: pig).
// gojq's normalizeNumbers iterates every map[string]any in place
// (normalize.go:76 — `for k := range v { v[k] = ... }`) and deleteEmpty
// /delpaths delete+write them in place (func.go:1742). Two Resolve
// errgroup workers (resolve.go:591) Get-hitting the SAME content entry
// run gojq over the SAME item maps concurrently ->
//   fatal error: concurrent map iteration and map write
// — the crash that took down the 0.30.128 and 0.30.129 deploys.
//
// THE FIX (0.30.130): listEnvelopeValue deep-copies the assembled
// envelope via maps.DeepCopyJSON so every caller gets a private value
// tree — no map shared with entry.Items. Restores the per-call
// isolation the removed marshal+unmarshal used to provide, without the
// redundant round-trip. P-CORE-1 untouched.
//
// FALSIFIER — MANDATORY HARD GATE: this test MUST be run with -race.
//   - Pre-fix: FAILS — `go test -race` reports `concurrent map iteration
//     and map write` (the fatal error aborts the test binary).
//   - Post-fix: PASSES — each Get-hit gates a private deep copy; gojq
//     mutates only that copy; no shared map, no race.
//
// It drives the REAL api.Resolve stage loop (no stub) — N goroutines
// concurrently resolve the SAME one-stage RESTAction whose single
// widgets LIST is a content-Get-HIT after a cold warm-up resolve, with
// a stage filter that forces gojq to walk + normalize every item map.

package api

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

// f1ListStageWithFilter is f1ListStage with an explicit stage filter —
// the filter forces gojq to recurse into + rewrite every item map, the
// in-place map mutation the P-CORE-2 race needs.
func f1ListStageWithFilter(id, filter string) *templates.API {
	return &templates.API{
		Name:   id,
		Path:   "/apis/widgets.krateo.io/v1/widgets",
		Verb:   ptr.To(http.MethodGet),
		Filter: ptr.To(filter),
	}
}

// f1ResolveAsConcurrent is f1ResolveAs's goroutine-safe twin: it drives
// the REAL api.Resolve stage loop as `username` but reports failures via
// t.Errorf (safe to call from any goroutine) rather than t.Fatalf
// (which must run on the test goroutine). The P-CORE-2 falsifier fans
// resolves across N goroutines, so the per-goroutine resolve helper must
// not call Fatalf.
func f1ResolveAsConcurrent(t *testing.T, username string, stage *templates.API) map[string]any {
	t.Helper()
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: username}),
	)
	ctx = cache.WithInternalEndpoint(ctx, &endpoints.Endpoint{ServerURL: "http://test.invalid"})
	dict := Resolve(ctx, ResolveOptions{
		RC:                  &rest.Config{},
		Items:               []*templates.API{stage},
		RESTActionNamespace: "default",
		RESTActionName:      "pcore2-widget-restaction",
	})
	if dict == nil {
		t.Errorf("concurrent resolve as %q returned nil dict", username)
	}
	return dict
}

// TestFalsifierPCORE2_ConcurrentContentHitIsolation is the 0.30.130 hard
// gate. A cold resolve warms the identity-free content entry for the
// cluster-wide widgets LIST (R3 pre-parsed Items attached). Then N
// goroutines concurrently resolve the SAME stage — every one is a
// content-Get-HIT, so (pre-fix) every one's listEnvelopeValue hands back
// item maps aliased to the single shared entry.Items. The stage filter
// makes gojq walk + normalize + project every item map; concurrent gojq
// over shared maps triggers the concurrent-map-access fatal error.
//
// Run with -race. Pre-fix this FAILS (fatal: concurrent map iteration
// and map write). Post-fix (listEnvelopeValue deep-copies) it PASSES.
func TestFalsifierPCORE2_ConcurrentContentHitIsolation(t *testing.T) {
	newF1Watcher(t)

	// A stage whose filter forces gojq to recurse into and rewrite every
	// item map: normalizeNumbers walks the whole input tree and the
	// object-constructor projection reads each item's nested metadata
	// maps — the item map[string]any are genuinely traversed.
	stage := f1ListStageWithFilter("widgets",
		"[.widgets.items[] | {n: .metadata.name, ns: .metadata.namespace}]")

	// Warm-up: one cold resolve populates the identity-free content
	// entry (un-gated dispatch + Put, R3 Items attached). Every
	// subsequent resolve of this stage is a content-Get-HIT.
	_ = f1ResolveAs(t, f1BroadUser, stage)

	// N concurrent resolves of the SAME stage. Pre-fix each Get-hit's
	// listEnvelopeValue returns item maps aliased to the one shared
	// entry.Items; jqutil.Eval mutates them in place; concurrent workers
	// race on the shared maps. -race makes the data race a hard failure;
	// the bare concurrent-map-access also fatals the test binary.
	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		// Alternate broad/narrow so the RBAC gate keeps a non-empty item
		// set for both — a non-empty kept slice is what reaches gojq.
		user := f1BroadUser
		if i%2 == 1 {
			user = f1NarrowUser
		}
		go func(u string) {
			defer wg.Done()
			d := f1ResolveAsConcurrent(t, u, stage)
			// Touch the output so the resolve is not optimized away and
			// the result tree is fully realized.
			if items, ok := d["widgets"].([]any); ok {
				_ = len(items)
			}
		}(user)
	}
	wg.Wait()

	// Reaching here without a fatal `concurrent map iteration and map
	// write` (and with -race clean) is the post-fix pass: every Get-hit
	// gated a private deep copy.
}

// pcore2BenchItems builds n unstructured composition-shaped items with a
// realistic nested metadata + spec + status tree — the per-item cost
// listEnvelopeValue's deep copy pays on every content-Get-hit.
func pcore2BenchItems(n int) []*unstructured.Unstructured {
	items := make([]*unstructured.Unstructured, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "composition.krateo.io/v1",
			"kind":       "Composition",
			"metadata": map[string]any{
				"name":      "comp-" + strconv.Itoa(i),
				"namespace": "bench-ns-" + strconv.Itoa(i%40),
				"labels":    map[string]any{"app": "bench", "idx": strconv.Itoa(i)},
				"annotations": map[string]any{
					"krateo.io/last-applied": "2026-05-19T00:00:00Z",
				},
			},
			"spec": map[string]any{
				"replicas": int64(3),
				"values":   map[string]any{"region": "eu", "tier": "gold"},
			},
			"status": map[string]any{
				"ready":      true,
				"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
			},
		}})
	}
	return items
}

// BenchmarkListEnvelopeValue_DeepCopy is INFERRED-item I4: the isolated
// per-call copy cost of the 0.30.130 fix. It measures listEnvelopeValue
// over a 1000-item composition LIST (the cyberjoker-lane shape) so the
// allocs/op + bytes/op delta of the maps.DeepCopyJSON isolation is
// attributable on its own, not buried in a full Resolve. Compare against
// the pre-0.30.128 marshal+unmarshal round-trip it replaces: the deep
// copy never re-serialises, so it is structurally cheaper than the
// round-trip while providing the same per-call isolation.
func BenchmarkListEnvelopeValue_DeepCopy(b *testing.B) {
	items := pcore2BenchItems(1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = listEnvelopeValue("composition.krateo.io/v1", "CompositionList", items)
	}
}
