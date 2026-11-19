package telemetry

import (
	"context"
	"log/slog"
)

type traceSlogHandler struct {
	next slog.Handler
}

// WrapSlogHandler wraps an slog.Handler so every log record emitted with a traced
// context.Context automatically includes "trace_id" and "span_id" attributes.
func WrapSlogHandler(next slog.Handler) slog.Handler {
	if next == nil {
		return nil
	}
	if _, already := next.(*traceSlogHandler); already {
		return next
	}
	return &traceSlogHandler{next: next}
}

func (h *traceSlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *traceSlogHandler) Handle(ctx context.Context, rec slog.Record) error {
	if sc, ok := SpanContextFromContext(ctx); ok {
		cloned := rec.Clone()
		cloned.AddAttrs(
			slog.String("trace_id", sc.TraceIDHex()),
			slog.String("span_id", sc.SpanIDHex()),
		)
		return h.next.Handle(ctx, cloned)
	}
	return h.next.Handle(ctx, rec)
}

func (h *traceSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceSlogHandler{next: h.next.WithAttrs(attrs)}
}

func (h *traceSlogHandler) WithGroup(name string) slog.Handler {
	return &traceSlogHandler{next: h.next.WithGroup(name)}
}
