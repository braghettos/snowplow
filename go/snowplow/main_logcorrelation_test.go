package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// F6 — trace_id on EVERY record (1.12.4 design §4.3, gate condition C6).
//
// PRODUCTION FUNCTION UNDER TEST: buildLogHandler (log_handler.go), the
// SAME function main() calls — there is no second construction path
// (F10 pins that structurally). The decorator it installs,
// traceCorrelationHandler.Handle, is what must execute for these arms to
// mean anything; a panic probe placed in it fails both arms.
//
// RED on main (8de5295): buildLogHandler does not exist — the chain is
// inline in func main() and unreachable — and the only record that ever
// carried trace_id was the per_call_log.go INFO line. A record emitted
// from ANY other site had no trace_id.

// spanCtx returns a context carrying a real, valid, recording span from a
// real SDK TracerProvider (no exporter — nothing leaves the process), plus
// the ids the record must carry.
func spanCtx(t *testing.T) (context.Context, string, string) {
	t.Helper()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("f6").Start(context.Background(), "f6.span")
	t.Cleanup(func() { span.End() })
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		t.Fatal("harness: expected a valid span context")
	}
	return ctx, sc.TraceID().String(), sc.SpanID().String()
}

func decodeRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("record is not one JSON object: %v\n%s", err, buf.String())
	}
	return rec
}

// TestF6_TraceIDOnEveryContextRecord: a record emitted from a site that is
// NOT per_call_log.go carries trace_id/span_id equal to the active span's.
func TestF6_TraceIDOnEveryContextRecord(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(buildLogHandler(false, slog.LevelInfo, &buf))
	ctx, wantTrace, wantSpan := spanCtx(t)

	log.InfoContext(ctx, "arbitrary.event", slog.String("k", "v"))

	rec := decodeRecord(t, &buf)
	if got := rec["trace_id"]; got != wantTrace {
		t.Fatalf("trace_id: got %v want %s\nrecord: %s", got, wantTrace, buf.String())
	}
	if got := rec["span_id"]; got != wantSpan {
		t.Fatalf("span_id: got %v want %s\nrecord: %s", got, wantSpan, buf.String())
	}
	if rec["msg"] != "arbitrary.event" || rec["k"] != "v" {
		t.Fatalf("record body altered: %s", buf.String())
	}
}

// TestF6_DerivedLoggersKeepCorrelation: log.With(...) and log.WithGroup(...)
// derive a new handler; the decorator must survive derivation or every
// derived logger silently loses correlation.
func TestF6_DerivedLoggersKeepCorrelation(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(buildLogHandler(false, slog.LevelInfo, &buf))
	ctx, wantTrace, _ := spanCtx(t)

	base.With(slog.String("component", "f6")).InfoContext(ctx, "with.event")
	rec := decodeRecord(t, &buf)
	if rec["trace_id"] != wantTrace || rec["component"] != "f6" {
		t.Fatalf("With()-derived logger lost correlation or attrs: %s", buf.String())
	}

	buf.Reset()
	base.WithGroup("g").InfoContext(ctx, "group.event", slog.String("x", "y"))
	rec = decodeRecord(t, &buf)
	if rec["trace_id"] != wantTrace {
		t.Fatalf("WithGroup()-derived logger lost correlation: %s", buf.String())
	}
}

// TestF6_NoSpanRecordIsByteIdentical: with no active span the field is
// ABSENT and the record is byte-identical to what the bare JSON handler
// produces today — the off-path contract (tracing disabled, or a
// context-less slog call reaching the handler with context.Background()).
func TestF6_NoSpanRecordIsByteIdentical(t *testing.T) {
	// Pin the clock so the two emissions carry the same "time".
	opts := &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}
	var want bytes.Buffer
	slog.New(slog.NewJSONHandler(&want, opts)).Info("plain.event", slog.Int("n", 1))

	var got bytes.Buffer
	// Same options through the decorator: ReplaceAttr is part of the
	// base handler, so build it the same way and wrap it.
	log := slog.New(newTraceCorrelationHandler(slog.NewJSONHandler(&got, opts)))
	log.Info("plain.event", slog.Int("n", 1))                              // no ctx
	log.InfoContext(context.Background(), "plain.event", slog.Int("n", 1)) // ctx, no span

	lines := bytes.Split(bytes.TrimSpace(got.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected 2 records, got %d: %s", len(lines), got.String())
	}
	for i, l := range lines {
		if !bytes.Equal(l, bytes.TrimSpace(want.Bytes())) {
			t.Fatalf("record %d not byte-identical to the bare handler's\n got: %s\nwant: %s", i, l, want.String())
		}
		if bytes.Contains(l, []byte("trace_id")) {
			t.Fatalf("record %d carries trace_id with no active span: %s", i, l)
		}
	}
}

// TestF6_PrettyHandlerAlsoDecorated: the text (pretty) branch goes through
// the same decorator.
func TestF6_PrettyHandlerAlsoDecorated(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(buildLogHandler(true, slog.LevelInfo, &buf))
	ctx, wantTrace, _ := spanCtx(t)
	log.InfoContext(ctx, "pretty.event")
	if !bytes.Contains(buf.Bytes(), []byte("trace_id="+wantTrace)) {
		t.Fatalf("pretty record lacks trace_id: %s", buf.String())
	}
}
