package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/krateo-platformops/snowplow/internal/logging"
	"github.com/krateo-platformops/snowplow/internal/metrics"
	"github.com/krateo-platformops/snowplow/internal/tracing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// F9 — off-path byte-identity (1.12.4 design §10, gate condition C8).
//
// DECLARED CONTRACT PIN: this passes on main (8de5295) by design. It is the
// FIRST test coverage internal/tracing, internal/metrics and
// internal/logging have ever had (all three carried zero _test.go files
// before 1.12.4), not a guard over existing tests.
//
// PRODUCTION FUNCTIONS UNDER TEST: the three real Setup entry points main()
// calls (main.go), driven with EVERY OTEL_* variable unset. Each must
// register nothing: the global TracerProvider / MeterProvider stay the
// SDK-free defaults, logging returns a nil provider, and the log handler
// built by buildLogHandler emits records byte-identical to the bare handler
// (no span → no trace_id). With OTEL_ENABLED unset the process must be
// indistinguishable from 1.12.3.
func TestF9_OffPathRegistersNothing(t *testing.T) {
	for _, kv := range os.Environ() {
		if k := strings.SplitN(kv, "=", 2)[0]; strings.HasPrefix(k, "OTEL_") {
			t.Setenv(k, "") // t.Setenv restores; "" reads as unset for env.Bool/env.String
			os.Unsetenv(k)
		}
	}

	ctx := context.Background()

	traceShutdown, err := tracing.Setup(ctx, "f9")
	if err != nil {
		t.Fatalf("tracing.Setup: %v", err)
	}
	t.Cleanup(func() { _ = traceShutdown(ctx) })
	if _, sdk := otel.GetTracerProvider().(*sdktrace.TracerProvider); sdk {
		t.Fatal("tracing.Setup installed an SDK TracerProvider with OTEL_* unset")
	}
	if tracing.Enabled() {
		t.Fatal("tracing.Enabled() true with OTEL_* unset")
	}

	metricShutdown, err := metrics.Setup(ctx, "f9")
	if err != nil {
		t.Fatalf("metrics.Setup: %v", err)
	}
	t.Cleanup(func() { _ = metricShutdown(ctx) })
	if _, sdk := otel.GetMeterProvider().(*sdkmetric.MeterProvider); sdk {
		t.Fatal("metrics.Setup installed an SDK MeterProvider with OTEL_* unset")
	}

	lp, logShutdown, err := logging.Setup(ctx, "f9")
	if err != nil {
		t.Fatalf("logging.Setup: %v", err)
	}
	t.Cleanup(func() { _ = logShutdown(ctx) })
	if lp != nil {
		t.Fatal("logging.Setup returned a LoggerProvider with OTEL_* unset")
	}

	// The log handler chain main installs: with no span on the context the
	// record is byte-identical to the bare JSON handler's (F6's second arm,
	// restated here as part of the off-path contract).
	opts := &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}
	var want, got bytes.Buffer
	slog.New(slog.NewJSONHandler(&want, opts)).InfoContext(ctx, "off.path", slog.String("k", "v"))
	slog.New(newTraceCorrelationHandler(slog.NewJSONHandler(&got, opts))).InfoContext(ctx, "off.path", slog.String("k", "v"))
	if !bytes.Equal(want.Bytes(), got.Bytes()) {
		t.Fatalf("off-path record differs from the bare handler's\n got: %s\nwant: %s", got.String(), want.String())
	}
}
