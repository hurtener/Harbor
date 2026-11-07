package telemetry

import (
	"context"
	"log/slog"
	"strings"

	"github.com/hurtener/Harbor/internal/audit"
)

// URLRedactingHandler protects URL-bearing diagnostics before the audit logger
// is available, and when a host injects a plain slog handler. It preserves the
// downstream handler's levels, groups, and record metadata. The handler and its
// derived handlers are immutable and safe for concurrent use.
func URLRedactingHandler(next slog.Handler) slog.Handler {
	if next == nil {
		panic("telemetry.URLRedactingHandler: nil handler")
	}
	return &urlRedactingHandler{next: next}
}

type urlRedactingHandler struct{ next slog.Handler }

func (h *urlRedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *urlRedactingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, audit.RedactURLText(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(redactURLAttr(attr))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h *urlRedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		clean[i] = redactURLAttr(attr)
	}
	return &urlRedactingHandler{next: h.next.WithAttrs(clean)}
}

func (h *urlRedactingHandler) WithGroup(name string) slog.Handler {
	return &urlRedactingHandler{next: h.next.WithGroup(name)}
}

func redactURLAttr(attr slog.Attr) slog.Attr {
	value := attr.Value.Resolve()
	switch value.Kind() {
	case slog.KindGroup:
		group := value.Group()
		clean := make([]slog.Attr, len(group))
		for i, sub := range group {
			clean[i] = redactURLAttr(sub)
		}
		attr.Value = slog.GroupValue(clean...)
	case slog.KindString:
		text := value.String()
		switch strings.ToLower(attr.Key) {
		case "url", "uri", "path", "request_uri", "requesturi", "http.target", "url.full":
			text = audit.RedactURL(text)
		default:
			text = audit.RedactURLText(text)
		}
		attr.Value = slog.StringValue(text)
	case slog.KindAny:
		clean, err := audit.RedactURLValue(value.Any())
		if err != nil {
			attr.Value = slog.StringValue(redactedSentinel)
		} else {
			attr.Value = slog.AnyValue(clean)
		}
	default:
		attr.Value = value
	}
	return attr
}
