// Package observability provides OpenTelemetry SDK initialization and
// metric instruments for the snowplow cache service.
//
// All functionality is gated behind OTEL_ENABLED=true. When disabled
// (the default), Init returns a no-op shutdown and no exporters or
// providers are created — zero runtime overhead.
package observability

import (
	"context"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const (
	defaultEndpoint    = "krateo-clickstack-otel-collector.clickhouse-system:4317"
	defaultServiceName = "snowplow"
)

// Init initializes the OTel SDK with OTLP/gRPC exporters for both traces
// and metrics. It returns a shutdown function that flushes pending data.
//
// Environment variables (standard OTel + custom):
//
//	OTEL_ENABLED              — "true" to enable (default: "false")
//	OTEL_EXPORTER_OTLP_ENDPOINT — collector address (default: krateo-clickstack-otel-collector.clickhouse-system:4317)
//	OTEL_SERVICE_NAME         — service name resource attribute (default: snowplow)
//	OTEL_TRACES_SAMPLER       — sampler type (handled by SDK env var support)
//	OTEL_TRACES_SAMPLER_ARG   — sampler argument (handled by SDK env var support)
//
// Resource attributes set: service.name, service.version, service.namespace,
// k8s.pod.name (from HOSTNAME), k8s.namespace.name (from POD_NAMESPACE).
func Init(ctx context.Context, version string) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }

	if !strings.EqualFold(os.Getenv("OTEL_ENABLED"), "true") {
		return noop, nil
	}

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}

	svcName := os.Getenv("OTEL_SERVICE_NAME")
	if svcName == "" {
		svcName = defaultServiceName
	}

	// Build resource with service and k8s attributes.
	attrs := []attribute.KeyValue{
		semconv.ServiceName(svcName),
		semconv.ServiceVersion(version),
		semconv.ServiceNamespace("krateo"),
	}
	if hostname := os.Getenv("HOSTNAME"); hostname != "" {
		attrs = append(attrs, semconv.K8SPodName(hostname))
	}
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		attrs = append(attrs, semconv.K8SNamespaceName(ns))
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(attrs...),
		resource.WithProcessRuntimeDescription(),
		resource.WithHost(),
	)
	if err != nil {
		return noop, err
	}

	// --- Trace exporter ---
	traceExporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return noop, err
	}

	tp := trace.NewTracerProvider(
		trace.WithBatcher(traceExporter, trace.WithBatchTimeout(5*time.Second)),
		trace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// --- Metric exporter ---
	metricExporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		// Clean up trace provider before returning.
		_ = tp.Shutdown(ctx)
		return noop, err
	}

	mp := metric.NewMeterProvider(
		metric.WithReader(metric.NewPeriodicReader(metricExporter, metric.WithInterval(15*time.Second))),
		metric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	// Initialize metric instruments now that the MeterProvider is set.
	meter := mp.Meter(svcName)
	if err := InitMetrics(meter); err != nil {
		_ = tp.Shutdown(ctx)
		_ = mp.Shutdown(ctx)
		return noop, err
	}

	// --- Log exporter ---
	logExporter, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(endpoint),
		otlploggrpc.WithInsecure(),
	)
	if err != nil {
		_ = tp.Shutdown(ctx)
		_ = mp.Shutdown(ctx)
		return noop, err
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter, sdklog.WithExportTimeout(5*time.Second))),
		sdklog.WithResource(res),
	)
	otellog.SetLoggerProvider(lp)

	// Composite shutdown flushes all three providers.
	shutdown = func(ctx context.Context) error {
		var firstErr error
		if e := tp.Shutdown(ctx); e != nil {
			firstErr = e
		}
		if e := mp.Shutdown(ctx); e != nil && firstErr == nil {
			firstErr = e
		}
		if e := lp.Shutdown(ctx); e != nil && firstErr == nil {
			firstErr = e
		}
		return firstErr
	}
	return shutdown, nil
}
