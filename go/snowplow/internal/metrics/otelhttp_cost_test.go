// otelhttp_cost_test.go — 1.12.4 falsifier F11.
//
// DECLARED AS A CHARACTERIZATION PIN, NOT A RED-THEN-GREEN. It passes on
// main (8de5295) and it passes here. The behaviour is inherent to
// otelhttp and predates 1.12.4 entirely. Its job is to make a cost that
// the design ORIGINALLY GOT WRONG explicit in code, so it is understood
// rather than discovered in production.
//
// # What rev 3 of the design claimed, and why it was false
//
// "Metrics on: effectively zero on the hot path. Every instrument is
// Observable/async." That is true of SNOWPLOW's instruments and false of
// the PROCESS, which is what the serving path actually experiences:
//
//   - otelhttp captures the GLOBAL MeterProvider at NewHandler time, not
//     per request;
//   - metrics.Setup calls otel.SetMeterProvider BEFORE main constructs
//     the handler, so the REAL provider is captured, not the no-op;
//   - every non-filtered request then performs THREE SYNCHRONOUS
//     histogram Records with a freshly built attribute set.
//
// And metrics do not honour the trace sampler, so every request pays
// this even at 5% trace sampling. OTEL_METRICS_ENABLED="false" is the
// rollback and needs no binary change.
//
// The compensating win, which this arm also pins: those histograms
// include http.server.request.duration — a per-route, UNSAMPLED,
// server-side latency distribution for /call, the first snowplow has
// ever exported.
//
// The REAL acceptance for the cost is not this arm. It is the Phase-6
// bench at SCALE=50000 comparing p50/p95 /call latency and RSS with
// OTEL_METRICS_ENABLED on versus off. That is the arm that can actually
// fail; this one only pins the contract, and reds if a future otelhttp
// bump or a WithMeterProvider(noop) silently changes it.
package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// the three synchronous server histograms otelhttp records per request.
var otelhttpServerHistograms = []string{
	"http.server.request.duration",
	"http.server.request.body.size",
	"http.server.response.body.size",
}

// TestF11_OtelHTTPRecordsThreeSynchronousHistogramsPerRequest reproduces
// main.go's exact ordering — SetMeterProvider, THEN NewHandler — and
// counts the data points N requests actually produce.
func TestF11_OtelHTTPRecordsThreeSynchronousHistogramsPerRequest(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	// ORDER IS THE WHOLE POINT. otelhttp's newConfig defaults its
	// MeterProvider to otel.GetMeterProvider() at CONSTRUCTION time, so
	// installing the real provider first is what makes the handler
	// record. Reversing these two lines is what "metrics are free" would
	// have required, and main.go does NOT reverse them (metrics.Setup at
	// :308, otelhttp.NewHandler at :1169).
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	h := otelhttp.NewHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("ok"))
		}),
		"snowplow",
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	const n = 7 // not 1: a per-request cost must be shown to scale with requests
	for i := 0; i < n; i++ {
		resp, err := http.Get(srv.URL + "/call")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_ = resp.Body.Close()
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	found := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			for _, want := range otelhttpServerHistograms {
				if m.Name != want {
					continue
				}
				found[want] = true

				// SYNCHRONOUS HISTOGRAM, not an observable gauge. This is
				// the type assertion that makes the cost claim concrete:
				// an async instrument would be read once per collection,
				// a histogram is Recorded once per request.
				hist, ok := m.Data.(metricdata.Histogram[int64])
				if !ok {
					// duration is float64-valued; body sizes are int64.
					fh, okf := m.Data.(metricdata.Histogram[float64])
					if !okf {
						t.Errorf("%s is %T, not a Histogram — otelhttp's server instruments are "+
							"SYNCHRONOUS histograms; if this changed, the per-request cost claim "+
							"in the package doc needs revisiting", m.Name, m.Data)
						continue
					}
					var count uint64
					for _, dp := range fh.DataPoints {
						count += dp.Count
					}
					if count != n {
						t.Errorf("%s recorded %d data points across %d requests; want exactly %d — "+
							"the cost is per-request, unsampled", m.Name, count, n, n)
					}
					continue
				}
				var count uint64
				for _, dp := range hist.DataPoints {
					count += dp.Count
				}
				if count != n {
					t.Errorf("%s recorded %d data points across %d requests; want exactly %d",
						m.Name, count, n, n)
				}
			}
		}
	}

	for _, want := range otelhttpServerHistograms {
		if !found[want] {
			t.Errorf("%s was NOT recorded. Either otelhttp stopped emitting it, or the handler did "+
				"not capture the real MeterProvider — in which case main.go's Setup-before-NewHandler "+
				"ordering has been broken and metrics silently stopped working.", want)
		}
	}

	// The claim worth making in the release notes, pinned: this is a
	// per-route server-side latency distribution, and it is the reason
	// dashboard panel 16 exists.
	if !found["http.server.request.duration"] {
		t.Error("http.server.request.duration is absent — the free per-route latency SLI, the first " +
			"server-side latency distribution snowplow exports, is not actually there")
	}
}

// TestF11_NoOpMeterProviderRecordsNothing is the other half of the
// contract and the off-path guarantee: with the global provider left at
// the SDK default no-op — which is what OTEL_METRICS_ENABLED=false
// leaves it as, since Setup returns before SetMeterProvider — the same
// handler records nothing at all.
//
// This is what makes OTEL_METRICS_ENABLED="false" a genuine rollback
// rather than a partial one.
func TestF11_NoOpMeterProviderRecordsNothing(t *testing.T) {
	// Construct the handler with NO real provider installed. A reader
	// attached afterwards must see no http.server.* series, because the
	// handler captured the no-op at construction.
	h := otelhttp.NewHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }),
		"snowplow",
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	for i := 0; i < 3; i++ {
		resp, err := http.Get(srv.URL + "/call")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_ = resp.Body.Close()
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			for _, name := range otelhttpServerHistograms {
				if m.Name == name {
					t.Errorf("%s was recorded into a provider the handler never captured — the "+
						"capture-at-construction contract this whole cost analysis rests on does "+
						"not hold", name)
				}
			}
		}
	}
}
