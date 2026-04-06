package observability

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// TraceIDFromContext extracts the OTel trace ID and span ID from a context
// and returns them as slog attributes. Returns nil if the span context is
// invalid (no active span or OTel is disabled).
func TraceIDAttrs(ctx context.Context) []slog.Attr {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []slog.Attr{
		slog.String("trace_id", sc.TraceID().String()),
		slog.String("span_id", sc.SpanID().String()),
	}
}

// TraceIDHandler wraps a slog.Handler to automatically inject trace_id and
// span_id from the context into every log record. When OTel is disabled or
// no span is active, the handler passes through without modification.
type TraceIDHandler struct {
	inner slog.Handler
}

// NewTraceIDHandler wraps an existing slog.Handler with trace context injection.
func NewTraceIDHandler(inner slog.Handler) *TraceIDHandler {
	return &TraceIDHandler{inner: inner}
}

func (h *TraceIDHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *TraceIDHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, r)
}

func (h *TraceIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceIDHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *TraceIDHandler) WithGroup(name string) slog.Handler {
	return &TraceIDHandler{inner: h.inner.WithGroup(name)}
}
