// Q-RBAC-DECOUPLE C(d) v3 §6.6 — refilter benchmarks.
//
// These benches gate G7 acceptance for the v3 deploy:
//
//	BenchmarkRefilter_Cyberjoker_50NS              ≤  50 ms/op  (G1 cyberjoker shape)
//	BenchmarkRefilter_Admin_50NS                   ≤ 300 ms/op  (G3 admin shape; outer JQ dominates)
//	BenchmarkRefilter_Cyberjoker_compositionsList_5K ≤ 100 ms/op (cyberjoker on 5K-item list)
//	BenchmarkRefilter_apiref_PathSpecific          measure JSON round-trip overhead on top of refilter
//
// Each bench reports ns/op + B/op + allocs/op so we can correlate against
// the architect's concern that the apiref path does an extra json.Unmarshal
// on top of refilter's own re-marshal.
//
// Run: go test -bench=BenchmarkRefilter -benchmem ./internal/resolvers/restactions/api/
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// silentCtx is a context with a sink-discarded logger, a UserInfo, and
// an RBAC evaluator installed. The audit-log slogs that fire inside
// applyUserAccessFilter would otherwise dominate the bench (one log
// per item denied) — discarding them isolates the cost of refilter
// itself, not the logging path.
func silentCtx(username string, ev cache.RBACEvaluator) context.Context {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithLogger(logger),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: username, Groups: []string{"devs"}}),
	)
	if ev != nil {
		ctx = cache.WithRBACEvaluator(ctx, ev)
	}
	return ctx
}

// makeNSItems returns N namespace items in the unstructured-list shape
// (each {"metadata": {"name": "ns-i", "namespace": "ns-i"}}). The
// NamespaceFrom JQ on the production "namespaces" api[] entry uses "."
// (the bare list element is the NS name), but for compositions-list
// shape we use ".metadata.namespace" — both shapes produce the same
// per-item RBAC decision, so this helper keeps the shape closer to the
// real K8s response and a downstream test could swap NamespaceFrom
// without rewriting items.
func makeNSItems(n int) []any {
	out := make([]any, n)
	for i := 0; i < n; i++ {
		name := "ns-" + strconv.Itoa(i)
		out[i] = name
	}
	return out
}

// makeCompositionItems returns total composition items, distributed
// round-robin across nsCount distinct namespaces. Each composition is
// shaped like a real Composition CR (metadata.namespace, metadata.name,
// metadata.labels, status, spec) so the JSON encoding cost is realistic.
func makeCompositionItems(total, nsCount int) []any {
	out := make([]any, total)
	for i := 0; i < total; i++ {
		ns := "ns-" + strconv.Itoa(i%nsCount)
		out[i] = map[string]any{
			"apiVersion": "composition.krateo.io/v1-1-0",
			"kind":       "MyComposition",
			"metadata": map[string]any{
				"name":      "comp-" + strconv.Itoa(i),
				"namespace": ns,
				"labels": map[string]any{
					"krateo.io/composition-id": "comp-" + strconv.Itoa(i),
					"krateo.io/composition-name": "MyComposition",
				},
				"creationTimestamp": "2026-05-04T12:00:00Z",
			},
			"spec": map[string]any{
				"replicas": 3,
				"image":    "nginx:1.27",
			},
			"status": map[string]any{
				"phase":      "Ready",
				"conditions": []any{},
			},
		}
	}
	return out
}

// makeNSCROnly is a single-api[]-entry RESTAction with UserAccessFilter
// on `.namespaces` and an outer JQ filter that selects the resulting
// names. Mirrors the production "namespaces" / "all-routes" shape (the
// ones where a 50-NS list is the entire payload).
func makeNSCROnly() *templates.RESTAction {
	return &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{{
				Name: "namespaces",
				Path: "/api/v1/namespaces",
				UserAccessFilter: &templates.UserAccessFilter{
					Verb:          "list",
					Resource:      "namespaces",
					NamespaceFrom: ptr.To("."),
				},
			}},
			// Outer filter: pass through the namespaces list verbatim
			// under a top-level key. Cheap O(N).
			Filter: ptr.To(`{namespaces: .namespaces}`),
		},
	}
}

// makeCompositionsListCR mirrors the real compositions-panels shape
// (NS list + compositions list with a sort + pagination). The outer
// JQ filter is intentionally heavier so the bench reflects the
// "outer JQ dominates" cost on the admin pass-through case.
func makeCompositionsListCR() *templates.RESTAction {
	return &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{{
				Name: "compositions",
				Path: "/api/v1/compositions",
				UserAccessFilter: &templates.UserAccessFilter{
					Verb:          "list",
					Resource:      "compositions",
					NamespaceFrom: ptr.To(".metadata.namespace"),
				},
			}},
			Filter: ptr.To(`{compositions: [.compositions[] | {name: .metadata.name, namespace: .metadata.namespace, phase: .status.phase}]}`),
		},
	}
}

// We reuse stubRBACEvaluator from refilter_test.go (same package, same
// test binary) — it uses the k8s schema.GroupResource type and matches
// the production cache.RBACEvaluator interface exactly. The two helpers
// below build per-scenario stubs.

// adminStub returns a stubRBACEvaluator that allows the given user
// across nsCount namespaces (admin / pass-through scenario).
func adminStub(user string, nsCount int) *stubRBACEvaluator {
	allow := make(map[string]bool, nsCount)
	for i := 0; i < nsCount; i++ {
		allow["ns-"+strconv.Itoa(i)] = true
	}
	return &stubRBACEvaluator{
		allow: map[string]map[string]bool{user: allow},
	}
}

// cyberjokerStub returns a stubRBACEvaluator that allows the given
// user only on a single NS (filter-down-to-1 scenario).
func cyberjokerStub(user, allowedNS string) *stubRBACEvaluator {
	return &stubRBACEvaluator{
		allow: map[string]map[string]bool{
			user: {allowedNS: true},
		},
	}
}

// ── Bench 1: 50-item NS list, cyberjoker filtering down to 1 NS ────────
//
// Spec target: ≤ 50 ms/op. Models the "G1 cyberjoker shape" — a small
// payload (50 namespaces) where every item is RBAC-checked and only one
// survives. Dominant cost: 50 EvaluateRBAC calls + 1 outer JQ on a tiny
// dict.
func BenchmarkRefilter_Cyberjoker_50NS(b *testing.B) {
	cr := makeNSCROnly()
	dict := map[string]any{"namespaces": makeNSItems(50)}
	raw, err := MarshalCached(cr, dict)
	if err != nil {
		b.Fatalf("MarshalCached: %v", err)
	}
	rbac := cyberjokerStub("cyberjoker", "ns-7")
	ctx := silentCtx("cyberjoker", rbac)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := RefilterRESTAction(ctx, nil, raw)
		if err != nil {
			b.Fatalf("iter %d: %v", i, err)
		}
		if len(out) == 0 {
			b.Fatalf("iter %d: empty out", i)
		}
	}
}

// ── Bench 2: 50-item NS list, admin (full passthrough) ────────────────
//
// Spec target: ≤ 300 ms/op (admin slower because all 50 items walk all
// the way through to outer JQ; cyberjoker drops 49 early). Models the
// "G3 admin shape" — outer JQ dominates because every item is encoded
// into the result.
func BenchmarkRefilter_Admin_50NS(b *testing.B) {
	cr := makeNSCROnly()
	dict := map[string]any{"namespaces": makeNSItems(50)}
	raw, err := MarshalCached(cr, dict)
	if err != nil {
		b.Fatalf("MarshalCached: %v", err)
	}
	rbac := adminStub("admin", 50)
	ctx := silentCtx("admin", rbac)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := RefilterRESTAction(ctx, nil, raw)
		if err != nil {
			b.Fatalf("iter %d: %v", i, err)
		}
		if len(out) == 0 {
			b.Fatalf("iter %d: empty out", i)
		}
	}
}

// ── Bench 3: 5K compositions across 50 NS, cyberjoker filtering ───────
//
// Spec target: ≤ 100 ms/op (per spec §6.6 "≤ 100 ms/op cyberjoker view of
// compositions-list"). Models the production compositions-panels shape
// at the "5K compositions" mid-scale point. Dominant cost: unmarshal of
// the wrapper + per-item RBAC + outer JQ on (1 NS / 50)*5000 = 100
// surviving items.
//
// The 5K scale is deliberately chosen as a meaningful intermediate
// between the 50-item NS bench and the customer's 50K-composition north
// star — Diego's task call-out for the deliverable specifies 5K. At
// 50K we'd push the bench memory footprint into the GB range and bench
// runtime past 30 s, neither of which is appropriate for go test
// -bench in CI.
func BenchmarkRefilter_Cyberjoker_compositionsList_5K(b *testing.B) {
	cr := makeCompositionsListCR()
	dict := map[string]any{"compositions": makeCompositionItems(5000, 50)}
	raw, err := MarshalCached(cr, dict)
	if err != nil {
		b.Fatalf("MarshalCached: %v", err)
	}
	rbac := cyberjokerStub("cyberjoker", "ns-7")
	ctx := silentCtx("cyberjoker", rbac)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := RefilterRESTAction(ctx, nil, raw)
		if err != nil {
			b.Fatalf("iter %d: %v", i, err)
		}
		if len(out) == 0 {
			b.Fatalf("iter %d: empty out", i)
		}
	}
}

// ── Bench 4: apiref-path-specific (lookupL1Refiltered shape) ─────────
//
// Architect's concern #4: the widget apiref path layers
// json.Unmarshal(refiltered_bytes) + status extraction ON TOP OF
// refilter's own re-marshal. So the steady-state cost on a widget L1
// hit is: refilter (json.Unmarshal of the wrapper + walk + remarshal)
// + extra json.Unmarshal of the wire bytes + map lookup of "status".
//
// This bench mirrors the apiref code path from
// internal/resolvers/widgets/apiref/resolve.go::lookupL1Refiltered to
// quantify the JSON-round-trip overhead independently of refilter
// itself. Run under the ADMIN scenario specifically — admin pass-through
// keeps all 5K items in the refiltered status, exposing the worst-case
// JSON round-trip cost on top of refilter. The cyberjoker variant (next
// bench) shows the steady-state cost where 99 % of items drop early.
// The delta between this admin bench and BenchmarkRefilter_Admin_5K
// (informational, included as a sub-call below) is the apiref-specific
// json.Unmarshal+map-lookup overhead.
func BenchmarkRefilter_apiref_PathSpecific(b *testing.B) {
	cr := makeCompositionsListCR()
	dict := map[string]any{"compositions": makeCompositionItems(5000, 50)}
	raw, err := MarshalCached(cr, dict)
	if err != nil {
		b.Fatalf("MarshalCached: %v", err)
	}
	rbac := adminStub("admin", 50)
	ctx := silentCtx("admin", rbac)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Step 1 — refilter.
		refiltered, err := RefilterRESTAction(ctx, nil, raw)
		if err != nil {
			b.Fatalf("iter %d refilter: %v", i, err)
		}
		// Step 2 — apiref-specific JSON round-trip + status
		// extraction. Mirrors apiref/resolve.go::lookupL1Refiltered
		// lines 217-225.
		var cached map[string]any
		if err := json.Unmarshal(refiltered, &cached); err != nil {
			b.Fatalf("iter %d unmarshal: %v", i, err)
		}
		status, ok := cached["status"].(map[string]any)
		if !ok {
			b.Fatalf("iter %d: status not a map: %T", i, cached["status"])
		}
		if len(status) == 0 {
			b.Fatalf("iter %d: empty status", i)
		}
	}
}

// BenchmarkRefilter_Admin_5K is the apiref bench's "refilter only"
// counterpart — same shape as bench 4 but WITHOUT the apiref JSON
// round-trip. Subtract this from bench 4 to isolate the apiref cost.
// Not part of the §6.6 G7 acceptance gates; included as informational
// reference for architect concern #4.
func BenchmarkRefilter_Admin_5K(b *testing.B) {
	cr := makeCompositionsListCR()
	dict := map[string]any{"compositions": makeCompositionItems(5000, 50)}
	raw, err := MarshalCached(cr, dict)
	if err != nil {
		b.Fatalf("MarshalCached: %v", err)
	}
	rbac := adminStub("admin", 50)
	ctx := silentCtx("admin", rbac)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := RefilterRESTAction(ctx, nil, raw)
		if err != nil {
			b.Fatalf("iter %d refilter: %v", i, err)
		}
		if len(out) == 0 {
			b.Fatalf("iter %d: empty out", i)
		}
	}
}

// ── helper: print human-readable bench targets in a sub-bench ────────
//
// Not a real benchmark (suffix _Targets is a no-op iteration). Documents
// the spec gates inside the test binary so a developer running
// `go test -bench=. -v` sees the thresholds without grepping the spec.
func BenchmarkRefilter_Targets(b *testing.B) {
	if testing.Short() {
		b.Skip("documentation bench, no-op")
	}
	b.Logf("§6.6 G7 acceptance gates:")
	b.Logf("  Cyberjoker_50NS                    target ≤  50 ms/op  (G1 small body)")
	b.Logf("  Admin_50NS                         target ≤ 300 ms/op  (G3 admin pass-through)")
	b.Logf("  Cyberjoker_compositionsList_5K     target ≤ 100 ms/op  (compositions-list cyberjoker)")
	b.Logf("  apiref_PathSpecific                informational; delta vs bench 3 = JSON round-trip cost")
	// Consume b.N so the bench framework reports something sensible.
	for i := 0; i < b.N; i++ {
		_ = fmt.Sprintf("noop %d", i)
	}
}
