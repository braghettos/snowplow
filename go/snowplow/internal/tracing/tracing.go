// Package tracing wires snowplow's OpenTelemetry trace pipeline.
//
// COEXISTENCE CONTRACT (load-bearing): this package is ADDITIVE and must
// not interfere with the existing observability surfaces. Specifically:
//
//   - The shortid `X-Krateo-TraceId` correlation id (plumbing
//     xcontext.TraceId, surfaced as status.traceId, set as an outbound
//     header, and consumed by /refreshes) is UNTOUCHED. OTel spans carry a
//     separate W3C `traceparent`; the two coexist on the same request.
//
//   - The stdout -> otel-daemonset filelog -> ClickHouse `otel_logs` log
//     pipeline (slog JSON to os.Stdout) is UNTOUCHED. OTel adds
//     trace_id/span_id ATTRIBUTES to those log records for correlation but
//     does not reroute or replace the pipeline.
//
// GATING: Setup is a no-op unless tracing resolves to enabled. Tracing is
// gated by OTEL_TRACING_ENABLED, which DEFAULTS to the value of the
// OTEL_ENABLED master switch when unset — so OTEL_ENABLED=true turns
// tracing on, and a per-signal OTEL_TRACING_ENABLED still overrides. When
// the gate is off, Setup registers NOTHING — no TracerProvider, no
// propagator, no exporter — so the global otel.GetTracerProvider() stays
// the no-op default and every instrumentation site (otelhttp handler,
// otelhttp transport, span lookups) degrades to a zero-cost no-op. The
// off-path is therefore byte-identical to the pre-OTel binary.
//
// This matches the canonical platform OTel enablement contract (the
// chart-inspector internal/telemetry ConfigFromEnv and the runtimes):
//
//	OTEL_ENABLED            master switch                (default false)
//	OTEL_TRACING_ENABLED    gate tracing  (default: value of OTEL_ENABLED)
//	OTEL_METRICS_ENABLED    gate metrics  (default: value of OTEL_ENABLED)
//	OTEL_EXPORTER_OTLP_ENDPOINT   collector OTLP/HTTP endpoint
package tracing

import (
	"context"
	"net/http"
	"time"

	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/plumbing/kubeutil"
	"github.com/krateo-platformops/snowplow/internal/otelresource"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const (
	// EnvEnabled is the master OTel switch. Default false. When set, it
	// supplies the default for EnvTracingEnabled (and the metrics gate) so a
	// single flag turns the whole pipeline on.
	EnvEnabled = "OTEL_ENABLED"

	// EnvTracingEnabled gates the entire trace pipeline. It DEFAULTS to the
	// value of EnvEnabled (OTEL_ENABLED) when unset; a per-signal value
	// overrides the master. Default (with the master unset) is false.
	EnvTracingEnabled = "OTEL_TRACING_ENABLED"

	// EnvOTLPEndpoint is the OTLP/HTTP collector endpoint
	// (e.g. "otel-collector.krateo-system.svc:4318"). Consumed by the
	// standard OTEL_EXPORTER_OTLP_ENDPOINT env contract via
	// otlptracehttp's WithEndpointURL/env auto-config.
	EnvOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
)

// SAMPLING (1.12.4). This package installs NO sampler: NewTracerProvider
// below passes no WithSampler, so the SDK's samplerFromEnv reads
// OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG and those env vars are
// the live knob. The chart ships "traceidratio" at 0.05 — the ROOT-
// deciding family, deliberately NOT parentbased_traceidratio. On the
// 057 topology snowplow sits behind the agent gateway, whose tracing
// policy is attached with randomSampling "100", so it creates a SAMPLED
// span for every forwarded request and propagates sampled=1 upstream. A
// parent-based sampler would honour that flag and span 100% of customer
// /call traffic — precisely the traffic the ratio was meant to bound.
// traceidratio makes snowplow's own 5% decision regardless of any
// parent, which is also correct on the LoadBalancer topology where there
// is no parent at all. The documented trade is that snowplow's decision
// DIVERGES from the gateway's: 5% complete end-to-end traces plus 95%
// gateway-only traces, partial by design and bounded.
const (
	// serviceName is the span-scope name passed to otelhttp.NewHandler
	// in main. The resource service.name lives in internal/otelresource.
	serviceName = otelresource.ServiceName
)

// ShutdownFunc flushes and stops the trace pipeline. Always non-nil so the
// caller can `defer shutdown(ctx)` unconditionally; it is a no-op when
// tracing was not enabled.
type ShutdownFunc func(context.Context) error

// tracingEnabled resolves the trace gate from the canonical contract:
// OTEL_TRACING_ENABLED, defaulting to the value of the OTEL_ENABLED master
// when the per-signal var is unset. With both unset it is false, preserving
// the default-off byte-identical guarantee.
func tracingEnabled() bool {
	return env.Bool(EnvTracingEnabled, env.Bool(EnvEnabled, false))
}

// Setup wires the OTLP/HTTP trace exporter and a batch TracerProvider when
// tracing resolves to enabled (OTEL_TRACING_ENABLED, defaulting to
// OTEL_ENABLED), registers it as the global TracerProvider, and installs
// the composite W3C TraceContext + Baggage propagator.
//
// When the gate is off it returns a no-op ShutdownFunc and registers
// nothing (off-path byte-identical guarantee).
//
// build is the snowplow build string (main.build), recorded as the
// service.version resource attribute.
func Setup(ctx context.Context, build string) (ShutdownFunc, error) {
	noop := func(context.Context) error { return nil }

	if !tracingEnabled() {
		return noop, nil
	}

	// otlptracehttp reads OTEL_EXPORTER_OTLP_ENDPOINT (and the standard
	// OTLP env vars: headers, protocol, insecure) from the environment by
	// default, so the endpoint is configured via the env contract without
	// a hard-coded address. WithEndpointURL is applied explicitly when the
	// var is set so a bare host:port or a full URL both resolve.
	opts := []otlptracehttp.Option{}
	if ep := env.String(EnvOTLPEndpoint, ""); ep != "" {
		opts = append(opts, otlptracehttp.WithEndpointURL(ep))
	}

	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return noop, err
	}

	// 1.12.4 — ONE shared resource across traces/metrics/logs
	// (internal/otelresource). It carries service.name/.version plus the
	// downward-API pod identity that makes the collector's k8sattributes
	// pod_association deterministic instead of falling through to
	// source-IP matching over a SNAT-able hostPort hop. Partial-resource
	// handling moved into the helper unchanged. logging.go's own
	// doc-comment relies on the three resources being identical; that is
	// now structural rather than three copies of one literal.
	res, err := otelresource.Build(ctx, build, kubeutil.ServiceAccountNamespace)
	if err != nil {
		return noop, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

// Enabled reports whether tracing resolves to enabled (OTEL_TRACING_ENABLED,
// defaulting to the OTEL_ENABLED master). Instrumentation sites outside main
// (e.g. the outbound external-fetch transport) consult this so they only
// wrap when the trace pipeline is actually wired — keeping the off-path
// byte-identical.
func Enabled() bool {
	return tracingEnabled()
}

// WrapTransport wraps rt with otelhttp.NewTransport when tracing is
// enabled, so an outbound request created with a span-carrying context
// gets a client span and an injected W3C traceparent (via the global
// propagator). When tracing is disabled it returns rt unchanged — the
// outbound path is then byte-identical to the pre-OTel client. A nil rt is
// returned as-is (callers default to http.DefaultTransport downstream).
func WrapTransport(rt http.RoundTripper) http.RoundTripper {
	if !Enabled() || rt == nil {
		return rt
	}
	return otelhttp.NewTransport(rt)
}
