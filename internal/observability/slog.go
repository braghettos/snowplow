package observability

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
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

// NewOTelSlogHandler creates a multi-handler that writes logs to BOTH the
// original handler (stderr JSON) AND the OTel LoggerProvider (OTLP export).
// The OTel bridge automatically includes trace_id/span_id from context.
func NewOTelSlogHandler(original slog.Handler) slog.Handler {
	otelHandler := otelslog.NewHandler("snowplow")
	return &multiHandler{handlers: []slog.Handler{original, otelHandler}}
}

// multiHandler fans out log records to multiple slog.Handlers.
type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r)
		}
	}
	return nil
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: handlers}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: handlers}
}
