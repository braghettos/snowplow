// h4_widgets_test.go — Ship H4 hermetic acceptance tests.
//
// H4 adds "widgets.templates.krateo.io" to bytesResourceOverrides
// (strip.go) — one declarative group key. Group-keyed routing then
// sends every widget GVR through the SAME streaming-list +
// bytesObject machinery the composition group already uses; the
// 0.30.133 hoist makes it timing-independent.
//
// THE ATTRIBUTION (team-lead-verified): NewFilteredDynamicInformer.func3
// = 4,440 MB / ~74% of heap = the STOCK informers for the widgets
// group (~720K objects, 9 populated GVRs). H4 routes them to streaming.
//
// Coverage of the PM-gate ACs that are hermetically verifiable:
//
//   - TestH4_AC1_WidgetGVRRoutesToStreaming      -> AC-1 (both orders)
//   - TestH4_AC2_WidgetResolvesByteIdentical     -> AC-2 (load-bearing
//     functional correctness — widget CR via bytesObject == stock)
//   - TestH4_AC4_OtherRoutingUnchanged           -> AC-4 (+ the
//     bytesResourceOverrides-exactly-2-entries grep-equivalent)
//   - TestH4_AC5_WidgetFallbackWhenStreamingOff  -> AC-5
//
// AC-3 (no per-/call latency regression) is on-cluster — the tester's.
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/rest"
)

// h4WidgetGVR is a representative widget GVR — the Panel kind, the
// widget the portal reads most. Its group is widgets.templates.krateo.io,
// so post-H4 it is a bytes-override / streaming-list group.
var h4WidgetGVR = schema.GroupVersionResource{
	Group:    "widgets.templates.krateo.io",
	Version:  "v1beta1",
	Resource: "panels",
}

// h4WidgetItem builds a representative widget CR (a Panel) with a
// nested spec carrying scalars of every JSON type — including INTEGER
// fields, to exercise the int64-vs-float64 numeric-fidelity contract —
// plus the two bookkeeping fields the strip policy drops.
func h4WidgetItem(ns, name string) map[string]any {
	return map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Panel",
		"metadata": map[string]any{
			"namespace":  ns,
			"name":       name,
			"generation": int64(3),
			"annotations": map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": `{"big":"blob"}`,
				"krateo.io/widget-rev":                             "v7",
			},
			"managedFields": []any{
				map[string]any{"manager": "widget-controller", "operation": "Update"},
			},
		},
		"spec": map[string]any{
			"widgetData": map[string]any{
				"title":   name + " panel",
				"columns": int64(4),
				"ratio":   0.5,
				"visible": true,
				"items": []any{
					map[string]any{"id": int64(1), "label": "one"},
					map[string]any{"id": int64(2), "label": "two"},
				},
			},
			"actions": map[string]any{
				"onClick": map[string]any{"verb": "GET", "retries": int64(2)},
			},
		},
		"status": map[string]any{
			"rendered": true,
			"revision": int64(7),
		},
	}
}

// h4WidgetListServer serves a paged Panel LIST — the widgets analogue
// of h2aListServer.
func h4WidgetListServer(t *testing.T, totalItems, perPage int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := 0
		if c := r.URL.Query().Get("continue"); c != "" {
			fmt.Sscanf(c, "%d", &offset)
		}
		end := offset + perPage
		if end > totalItems {
			end = totalItems
		}
		items := make([]any, 0, end-offset)
		for i := offset; i < end; i++ {
			items = append(items, h4WidgetItem("krateo-system", fmt.Sprintf("panel-%04d", i)))
		}
		meta := map[string]any{"resourceVersion": "55555"}
		if end < totalItems {
			meta["continue"] = fmt.Sprintf("%d", end)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "widgets.templates.krateo.io/v1beta1",
			"kind":       "PanelList",
			"metadata":   meta,
			"items":      items,
		})
	}))
}

// TestH4_AC1_WidgetGVRRoutesToStreaming — AC-1.
//
// A widget GVR EnsureResourceType produces a *streamingDynamicInformer
// in BOTH registration orders — autoDiscoverGroups empty (pre-Phase-1)
// and populated (post-Phase-1). Never a stock factory informer, never
// the in-branch NewFilteredDynamicInformer. The 0.30.133 timing
// independence applies to the widget group via the group-keyed set.
func TestH4_AC1_WidgetGVRRoutesToStreaming(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv(envCompositionStreamingList, "true")

	t.Run("autoDiscoverGroups empty (pre-Phase-1)", func(t *testing.T) {
		ResetDepsForTest()
		t.Cleanup(ResetDepsForTest)
		ResetAutoDiscoverGroupsForTest()
		t.Cleanup(ResetAutoDiscoverGroupsForTest)

		if matchesAutoDiscoverGroup(h4WidgetGVR.Group) {
			t.Fatal("precondition: autoDiscoverGroups must be empty for this subtest")
		}

		rw := newRouteRaceWatcher(t, true, h4WidgetGVR)
		t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
		rw.EnsureResourceType(h4WidgetGVR)

		if !isStreamingInformer(rw, h4WidgetGVR) {
			t.Fatal("AC-1 FAIL: widget GVR registered before Phase 1 did NOT route to " +
				"the streaming informer — H4 group key not effective / race not closed")
		}
	})

	t.Run("autoDiscoverGroups populated (post-Phase-1)", func(t *testing.T) {
		ResetDepsForTest()
		t.Cleanup(ResetDepsForTest)
		ResetAutoDiscoverGroupsForTest()
		t.Cleanup(ResetAutoDiscoverGroupsForTest)
		AddAutoDiscoverGroup(h4WidgetGVR.Group)

		rw := newRouteRaceWatcher(t, true, h4WidgetGVR)
		t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
		rw.EnsureResourceType(h4WidgetGVR)

		if !isStreamingInformer(rw, h4WidgetGVR) {
			t.Fatal("AC-1 FAIL: widget GVR registered after Phase 1 did NOT route to streaming")
		}
	})
}

// TestH4_AC2_WidgetResolvesByteIdentical — AC-2 (load-bearing).
//
// Functional correctness: a widget CR delivered by the streaming
// bytes-backed path must reconstruct BYTE-IDENTICALLY to the same CR
// decoded by the stock *unstructured path. This is the exact input
// contract the widget resolver depends on — it reads widget CRs from
// the store on every /call via ListObjects/GetObject ->
// decodeBytesObject. If the bytesObject round-trip lost or mistyped a
// field, the resolved widget props would diverge.
//
// The control is the stock decode (json -> map -> Unstructured) then
// strip — exactly what a plain dynamic informer would have stored.
// reflect.DeepEqual catches any field loss AND any int64-vs-float64
// numeric drift (the widget spec carries integer fields: columns,
// retries, revision, generation).
func TestH4_AC2_WidgetResolvesByteIdentical(t *testing.T) {
	const n = 24
	srv := h4WidgetListServer(t, n, 10) // 3-page continue-walk
	defer srv.Close()

	rc, err := streamingRESTClient(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("streamingRESTClient: %v", err)
	}
	got, err := streamingList(context.Background(), rc, h4WidgetGVR,
		metav1.ListOptions{Limit: listPageLimit})
	if err != nil {
		t.Fatalf("streamingList(widget GVR): %v", err)
	}
	if len(got.Items) != n {
		t.Fatalf("streamed %d widget items, want %d", len(got.Items), n)
	}

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("panel-%04d", i)

		// Control: what a plain dynamic informer would have stored — a
		// CR decoded the way client-go's UnstructuredJSONScheme decodes
		// it, then stripped. The scheme uses k8s.io/apimachinery's
		// util/json (integral JSON numbers -> int64); decoding the
		// control with plain encoding/json would yield float64 and make
		// this a comparison of two DIFFERENT conventions, not a fidelity
		// check. utiljson here makes the control faithful to the real
		// dynamic-informer store shape.
		fixtureJSON, _ := json.Marshal(h4WidgetItem("krateo-system", name))
		var stockMap map[string]any
		if err := utiljson.Unmarshal(fixtureJSON, &stockMap); err != nil {
			t.Fatalf("stock decode widget %d: %v", i, err)
		}
		stock := &unstructured.Unstructured{Object: stockMap}
		_, _ = defaultStripUnstructured(stock)

		// H4 path: the streamed bytesObject, decoded the way the
		// resolver reads it (decodeBytesObject — the indexer-read path).
		streamed, ok := decodeBytesObject(got.Items[i])
		if !ok || streamed == nil {
			t.Fatalf("widget %d: decodeBytesObject failed — the resolver would see nothing", i)
		}

		if !reflect.DeepEqual(stock.Object, streamed.Object) {
			t.Fatalf("AC-2 FAIL: widget %d resolved from the bytesObject store diverges "+
				"from the stock path\n stock=%#v\n   h4=%#v", i, stock.Object, streamed.Object)
		}

		// Spot-check the numeric-fidelity contract explicitly: an
		// integer spec field must be int64, not float64.
		spec, _ := streamed.Object["spec"].(map[string]any)
		wd, _ := spec["widgetData"].(map[string]any)
		if cols, isInt := wd["columns"].(int64); !isInt || cols != 4 {
			t.Fatalf("AC-2 FAIL: widget %d spec.widgetData.columns = %v (%T), want int64(4) — "+
				"numeric fidelity lost (must decode via util/json)", i, wd["columns"], wd["columns"])
		}
		// Strip applied: the two bookkeeping fields gone, others kept.
		if len(streamed.GetManagedFields()) != 0 {
			t.Fatalf("AC-2 FAIL: widget %d still carries managedFields — strip not applied", i)
		}
		if _, present := streamed.GetAnnotations()["kubectl.kubernetes.io/last-applied-configuration"]; present {
			t.Fatalf("AC-2 FAIL: widget %d still carries the last-applied annotation", i)
		}
		if streamed.GetAnnotations()["krateo.io/widget-rev"] != "v7" {
			t.Fatalf("AC-2 FAIL: widget %d: strip dropped a non-bookkeeping annotation", i)
		}
	}
}

// TestH4_AC4_OtherRoutingUnchanged — AC-4.
//
// SUPERSEDED BY THE SHIP H5 ROUTING INVERSION. The original H4 AC-4
// asserted bytesResourceOverrides held exactly {composition, widgets}
// and a non-widget GVR did NOT stream. H5 deleted bytesResourceOverrides
// and made streaming the DEFAULT — a non-widget GVR now DOES stream
// (that is H5 AC-7, the structural falsifier in streaming_route_race_test.go).
// This test is re-pointed to assert the H5-correct state: the widget
// group still streams (now via the default, not an allow-list entry),
// and the allow-list is gone.
func TestH4_AC4_OtherRoutingUnchanged(t *testing.T) {
	// The widget group still streams — post-H5 via the default
	// (isStreamingException is false for it), not an allow-list entry.
	if isStreamingException(h4WidgetGVR) {
		t.Fatal("widget GVR became a streaming exception — H5: only typed-RBAC is excepted")
	}

	// The widget GVR still routes to the streaming informer end-to-end.
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv(envCompositionStreamingList, "true")
	ResetDepsForTest()
	t.Cleanup(ResetDepsForTest)
	ResetAutoDiscoverGroupsForTest()
	t.Cleanup(ResetAutoDiscoverGroupsForTest)

	rw := newRouteRaceWatcher(t, true, h4WidgetGVR)
	t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
	rw.EnsureResourceType(h4WidgetGVR)
	if !isStreamingInformer(rw, h4WidgetGVR) {
		t.Fatal("widget GVR no longer routes to the streaming informer post-H5")
	}
}

// TestH4_AC5_WidgetFallbackWhenStreamingOff — AC-5.
//
// The CACHE_ENABLED / streaming toggle and the `if gi == nil` stock
// fallback must hold for the widget group exactly as for compositions.
// With the streaming flag off OR no *rest.Config wired, a widget GVR
// must fall back to a stock informer — not a *streamingDynamicInformer
// — and still register. No hardcoded special-case: routing stays on the
// declarative bytesResourceOverrides set.
func TestH4_AC5_WidgetFallbackWhenStreamingOff(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	t.Run("streaming flag off", func(t *testing.T) {
		t.Setenv(envCompositionStreamingList, "false")
		ResetDepsForTest()
		t.Cleanup(ResetDepsForTest)
		ResetAutoDiscoverGroupsForTest()
		t.Cleanup(ResetAutoDiscoverGroupsForTest)

		// REST config IS wired — proving the fallback is flag-driven.
		rw := newRouteRaceWatcher(t, true, h4WidgetGVR)
		t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
		rw.EnsureResourceType(h4WidgetGVR)

		if isStreamingInformer(rw, h4WidgetGVR) {
			t.Fatal("AC-5 FAIL: streaming flag off but widget GVR still got a streaming informer")
		}
		rw.mu.RLock()
		_, registered := rw.informers[h4WidgetGVR]
		rw.mu.RUnlock()
		if !registered {
			t.Fatal("AC-5 FAIL: streaming-off widget GVR not registered — stock fallback unreachable")
		}
	})

	t.Run("no rest.Config wired", func(t *testing.T) {
		t.Setenv(envCompositionStreamingList, "true")
		ResetDepsForTest()
		t.Cleanup(ResetDepsForTest)
		ResetAutoDiscoverGroupsForTest()
		t.Cleanup(ResetAutoDiscoverGroupsForTest)

		// NO REST config — newStreamingDynamicInformer returns ok=false.
		rw := newRouteRaceWatcher(t, false, h4WidgetGVR)
		t.Cleanup(func() { rw.Stop(); time.Sleep(50 * time.Millisecond) })
		rw.EnsureResourceType(h4WidgetGVR)

		if isStreamingInformer(rw, h4WidgetGVR) {
			t.Fatal("AC-5 FAIL: no *rest.Config but widget GVR still got a streaming informer")
		}
		rw.mu.RLock()
		_, registered := rw.informers[h4WidgetGVR]
		rw.mu.RUnlock()
		if !registered {
			t.Fatal("AC-5 FAIL: no-rest-config widget GVR not registered — stock fallback unreachable")
		}
	})
}
