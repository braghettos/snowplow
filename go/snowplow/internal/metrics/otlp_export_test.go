// otlp_export_test.go — 1.12.4 falsifier F1, the primary arm.
//
// THE CLAIM UNDER TEST is the release's whole first symptom: "no snowplow
// telemetry exists in ClickStack". Every weaker way of testing that leaves
// the real failure mode open. Asserting the instrument was declared does
// not prove the pipeline runs. Asserting the callback fired does not prove
// bytes left the process. Asserting an HTTP request arrived does not prove
// the payload carries the metric, the value, or a resource the collector
// can associate to a pod.
//
// So this arm drives the ACTUAL production path end to end and reads the
// WIRE:
//
//	real metrics.Setup  ->  real otlpmetrichttp exporter  ->  real HTTP POST
//	  ->  protobuf-decoded ExportMetricsServiceRequest
//
// Nothing is stubbed except the collector itself, which is an httptest
// server standing in for the node-local daemonset agent's :4318. A temporary
// panic() in registerInstruments fails this test, which is the proof that
// the production registration frame executes rather than a test double.
//
// go.opentelemetry.io/proto/otlp is already in go.sum at v1.10.0 (indirect),
// so decoding the wire costs no new module and never enters the server binary.
package metrics

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/krateo-platformops/snowplow/internal/cache"

	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// capturedExport is one decoded OTLP/HTTP request body.
type capturedExport struct {
	req *collectormetricspb.ExportMetricsServiceRequest
}

// otlpReceiver stands in for the collector's OTLP/HTTP metrics receiver.
// It decodes each POST body as an ExportMetricsServiceRequest — the same
// protobuf the daemonset agent parses — so a payload that would not
// deserialise on the real collector fails here too.
type otlpReceiver struct {
	mu      sync.Mutex
	exports []capturedExport
	srv     *httptest.Server
}

func newOTLPReceiver(t *testing.T) *otlpReceiver {
	t.Helper()
	r := &otlpReceiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body io.Reader = req.Body
		// The exporter gzips by default; decode it the way the collector does.
		if req.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(req.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer gz.Close()
			body = gz
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		msg := &collectormetricspb.ExportMetricsServiceRequest{}
		if err := proto.Unmarshal(raw, msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.exports = append(r.exports, capturedExport{req: msg})
		r.mu.Unlock()

		// The collector answers with an empty ExportMetricsServiceResponse.
		resp, _ := proto.Marshal(&collectormetricspb.ExportMetricsServiceResponse{})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(resp)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *otlpReceiver) snapshot() []capturedExport {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]capturedExport, len(r.exports))
	copy(out, r.exports)
	return out
}

// findSum locates a Sum metric by name across every captured export and
// returns its first data point's int value plus the resource attributes
// it arrived under.
func findSum(exports []capturedExport, name string) (val int64, resAttrs map[string]string, found bool) {
	for _, e := range exports {
		for _, rm := range e.req.GetResourceMetrics() {
			attrs := map[string]string{}
			for _, kv := range rm.GetResource().GetAttributes() {
				attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
			}
			for _, sm := range rm.GetScopeMetrics() {
				for _, mt := range sm.GetMetrics() {
					if mt.GetName() != name {
						continue
					}
					sum, ok := mt.GetData().(*metricspb.Metric_Sum)
					if !ok || len(sum.Sum.GetDataPoints()) == 0 {
						continue
					}
					dp := sum.Sum.GetDataPoints()[0]
					return dp.GetAsInt(), attrs, true
				}
			}
		}
	}
	return 0, nil, false
}

// TestF1_OTLPMetricExportLeavesTheProcess is F1.
//
// RED ON MAIN, for two INDEPENDENT reasons — either alone is enough to
// make the release's symptom (i) real:
//
//	1. the pipeline is default-off, so no bytes leave at all; and
//	2. snowplow_widgets_uaf_put_declined_total is not one of the 41
//	   instruments — the three 1.12.3 UAF counters are expvar-only.
func TestF1_OTLPMetricExportLeavesTheProcess(t *testing.T) {
	rcv := newOTLPReceiver(t)

	// The REAL env contract, exactly as the chart sets it.
	t.Setenv("OTEL_ENABLED", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", rcv.srv.URL)
	// [C4] Without this, Disabled() is true, the UAF counters are never
	// even registered, and a green would mean "the cache was off".
	t.Setenv("CACHE_ENABLED", "true")

	ctx := context.Background()

	// The REAL Setup — same call main.go makes, same exporter, same
	// resource, same observable registration.
	shutdown, err := Setup(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("metrics.Setup: %v", err)
	}
	if shutdown == nil {
		t.Fatal("metrics.Setup returned a nil shutdown")
	}

	// The REAL counter, bumped through the REAL production incrementer.
	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	cache.BumpWidgetsUAFPutDeclined()
	if got := cache.WidgetsUAFPutDeclined(); got != 1 {
		t.Fatalf("setup: the counter itself did not move (%d) — the arm would test nothing", got)
	}

	// Shutdown force-flushes the periodic reader, so we do not depend on
	// the 60s collection interval.
	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("flush/shutdown: %v", err)
	}

	exports := rcv.snapshot()
	if len(exports) == 0 {
		t.Fatal("NOTHING was exported: no OTLP request reached the receiver. On main this is the " +
			"default-off pipeline; here it means Setup wired an exporter that does not send.")
	}

	val, resAttrs, found := findSum(exports, "snowplow_widgets_uaf_put_declined_total")
	if !found {
		t.Fatalf("snowplow_widgets_uaf_put_declined_total is absent from the exported payload. "+
			"Bytes left the process but this instrument is not among them. Exported metric names: %v",
			exportedNames(exports))
	}
	if val != 1 {
		t.Errorf("exported value = %d; want 1 — the observable callback is not reading the live counter", val)
	}

	// The resource is what the collector associates to a pod and what
	// every dashboard filters on. A metric with the right name under the
	// wrong resource is unqueryable.
	if got := resAttrs["service.name"]; got != "snowplow" {
		t.Errorf("resource service.name = %q; want \"snowplow\" — this is the otel_metrics.ServiceName "+
			"every panel filters on", got)
	}
	if got := resAttrs["service.version"]; got != "deadbeef" {
		t.Errorf("resource service.version = %q; want \"deadbeef\" (the build argument). This is the "+
			"hermetic half of F7a: it proves the SDK stamps the git commit, so a post-deploy "+
			"ServiceVersion of the chart appVersion would mean k8sattributes overwrote it", got)
	}
}

// exportedNames lists every metric name in the captured payloads, so a
// failure says what DID arrive rather than only what did not.
func exportedNames(exports []capturedExport) []string {
	var out []string
	for _, e := range exports {
		for _, rm := range e.req.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, mt := range sm.GetMetrics() {
					out = append(out, mt.GetName())
				}
			}
		}
	}
	return out
}

// TestF1_AllThreeUAFCountersExport extends F1 across the whole A-1
// family. Per feedback_leak_needs_acceptance_arm_per_carrier, a family
// with three carriers needs an arm per carrier: instrumenting one of the
// three and leaving the other two expvar-only would still leave two
// blind spots on the dashboard panel that is supposed to show all three.
func TestF1_AllThreeUAFCountersExport(t *testing.T) {
	rcv := newOTLPReceiver(t)
	t.Setenv("OTEL_ENABLED", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", rcv.srv.URL)
	t.Setenv("CACHE_ENABLED", "true")

	ctx := context.Background()
	shutdown, err := Setup(ctx, "deadbeef")
	if err != nil {
		t.Fatalf("metrics.Setup: %v", err)
	}

	cache.RegisterUAFPutDeclineMetricsForTest()
	cache.ResetUAFPutDeclineCountersForTest()
	// Distinct, non-equal values so a mis-wired observation that reads
	// the wrong counter cannot pass by coincidence.
	cache.BumpRestactionsUAFPutDeclined()
	cache.BumpWidgetsUAFPutDeclined()
	cache.BumpWidgetsUAFPutDeclined()
	cache.BumpRAFullListUAFBypass()
	cache.BumpRAFullListUAFBypass()
	cache.BumpRAFullListUAFBypass()

	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("flush/shutdown: %v", err)
	}
	exports := rcv.snapshot()

	for name, want := range map[string]int64{
		"snowplow_restactions_uaf_put_declined_total": 1,
		"snowplow_widgets_uaf_put_declined_total":     2,
		"snowplow_ra_full_list_uaf_bypass_total":      3,
	} {
		got, _, found := findSum(exports, name)
		if !found {
			t.Errorf("%s did not export", name)
			continue
		}
		if got != want {
			t.Errorf("%s exported %d; want %d — a cross-wired observation", name, got, want)
		}
	}
}
