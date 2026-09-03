package main

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// buildLogHandler returns the PRODUCTION slog handler chain: the base
// text/JSON handler wrapped in the OTel trace-correlation decorator.
//
// 1.12.4 (design §4.3, gate condition C6). Before this release the chain
// was built inline in func main() and no test could reach it, so the one
// site that attached trace_id/span_id to a log record
// (per_call_log.go, INFO, suppressed by the production LOG_LEVEL=warn)
// was the whole of snowplow's trace/log correlation. main() and the F6
// falsifier both call THIS function — there is no second construction
// path, and the F10 AST guard reds the day someone reconstructs the
// handler inline and bypasses the decorator.
//
// prettyLog selects the text handler (operator terminals) over the JSON
// handler (the stdout -> filelog -> ClickHouse otel_logs pipeline). w is
// the destination stream; main passes os.Stderr for pretty and os.Stdout
// for JSON, preserving the pre-1.12.4 stream choice exactly.
func buildLogHandler(prettyLog bool, level slog.Level, w io.Writer) slog.Handler {
	var base slog.Handler
	if prettyLog {
		base = slog.NewTextHandler(w, &slog.HandlerOptions{
			Level:     level,
			AddSource: false,
		})
	} else {
		base = slog.NewJSONHandler(w, &slog.HandlerOptions{
			Level:     level,
			AddSource: false,
		})
	}
	return newTraceCorrelationHandler(base)
}

// traceCorrelationHandler decorates a slog.Handler so that every record
// emitted with a context carrying a VALID OTel span context gains two
// TOP-LEVEL attributes:
//
//	trace_id  the W3C trace id (hex), joinable to otel_traces.TraceId
//	span_id   the W3C span id (hex)
//
// NAMING — read this before touching either field. Every snowplow record
// ALREADY carries "traceId" (camelCase): the Krateo plumbing shortid
// correlation id (xcontext.TraceId, surfaced as status.traceId), which is
// unrelated to W3C. The decorator adds "trace_id" (snake_case), the OTel
// id. Two near-identical keys on one record is an operator trap the docs
// and the dashboard name explicitly; the HyperDX source expression for
// correlation is JSONExtractString(Body, 'trace_id').
//
// TOP-LEVEL, EVEN ON DERIVED LOGGERS. A logger derived via
// log.With(...) / log.WithGroup(...) derives its handler; if the
// decorator simply wrapped the derived base and appended the two attrs at
// Handle time, a WithGroup("g") logger would nest them as g.trace_id and
// the ClickHouse expression above would miss them. So the decorator
// keeps the ROOT base plus the ordered derivation chain, and on a
// correlated record applies the trace attrs to the root FIRST and then
// replays the chain — the ids always land at the top of the record. The
// cached derived handler serves the uncorrelated (no span) path with no
// per-record allocation, so a context-less or tracing-off record costs
// exactly what it cost before 1.12.4.
//
// NO-OP WHEN TRACING IS OFF. With the OTel pipeline disabled (or for any
// slog call made without a request context — log.Info rather than
// log.InfoContext — which reaches the handler with context.Background())
// SpanContextFromContext returns an invalid context, nothing is appended,
// and the record is byte-identical to the pre-1.12.4 emission. The F6
// byte-identity arm pins that. Note that a span context is VALID (and
// the ids are stamped) even when the sampler dropped the span: at
// traceidratio 0.05, 100% of request records still carry the request's
// trace_id — which is what lets an operator group one request's log
// lines together whether or not its trace was exported.
//
// WHAT THIS DOES NOT DO. The trace_id is only PRESENT on the record; the
// platform's filelog pipeline has no json_parser, so otel_logs.TraceId
// stays empty until the krateo-observability collector fix lands. That
// half is out of this release (design §4.2, C9).
type traceCorrelationHandler struct {
	root    slog.Handler                      // the undecorated base, before any With*/WithGroup
	ops     []func(slog.Handler) slog.Handler // derivation chain, in order
	derived slog.Handler                      // ops applied to root, cached for the no-span path
}

func newTraceCorrelationHandler(base slog.Handler) slog.Handler {
	return &traceCorrelationHandler{root: base, derived: base}
}

// Enabled defers to the derived handler so level filtering is unchanged.
func (h *traceCorrelationHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.derived.Enabled(ctx, l)
}

// Handle stamps trace_id/span_id at the top level when a valid span is on
// ctx, then delegates; without a span it delegates to the cached derived
// handler untouched.
func (h *traceCorrelationHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return h.derived.Handle(ctx, r)
	}
	target := h.root.WithAttrs([]slog.Attr{
		slog.String("trace_id", sc.TraceID().String()),
		slog.String("span_id", sc.SpanID().String()),
	})
	for _, op := range h.ops {
		target = op(target)
	}
	return target.Handle(ctx, r)
}

// WithAttrs / WithGroup extend the derivation chain (and the cached
// derived handler) so a logger created via log.With(...) or
// log.WithGroup(...) keeps the decorator. Without these two, every
// derived logger would silently lose correlation — the exact failure the
// F6 derived-logger arm exists to catch.
func (h *traceCorrelationHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	cp := append([]slog.Attr(nil), attrs...)
	return h.extend(func(b slog.Handler) slog.Handler { return b.WithAttrs(cp) })
}

func (h *traceCorrelationHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.extend(func(b slog.Handler) slog.Handler { return b.WithGroup(name) })
}

func (h *traceCorrelationHandler) extend(op func(slog.Handler) slog.Handler) slog.Handler {
	ops := make([]func(slog.Handler) slog.Handler, len(h.ops), len(h.ops)+1)
	copy(ops, h.ops)
	ops = append(ops, op)
	return &traceCorrelationHandler{root: h.root, ops: ops, derived: op(h.derived)}
}
