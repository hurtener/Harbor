// sloghandler.go — the *slog.Logger bridge over the canonical
// telemetry Logger (Wave C checkpoint audit; closes the 111f
// telemetry-threading gap). Long-lived subsystems constructed by the
// assembly (the notifications subscriber, the pause sweeper, the
// dispatch executor, the MCP attach loop, the search-cache warn path)
// accept a plain *slog.Logger; before this bridge they stayed on the
// bare boot logger for their whole lifetime, so their error paths
// bypassed the mandatory redactor and emitted no paired
// `runtime.error` bus event. Slog() lets the assembly hand them a
// *slog.Logger whose records flow through the full telemetry pipeline
// (identity stamping from ctx, redaction, bus-paired errors) without
// changing a single subsystem signature.
package telemetry

import (
	"context"
	"log/slog"
)

// Slog returns a *slog.Logger backed by this Logger: every record is
// routed through the canonical pipeline (ctx identity stamping,
// mandatory redaction, the bus-paired Error emit). Level filtering
// follows the Logger's configured log_level. Use it to thread the
// telemetry Logger into components that accept a plain *slog.Logger
// (CLAUDE.md §5 — one logger).
//
// Mapping: records at or above slog.LevelError route through
// Logger.Error (slog record + paired runtime.error bus event); Warn /
// Info / Debug route through the matching Logger method. The record's
// own timestamp and PC are dropped — the pipeline stamps its own
// time, exactly as a direct Logger call does.
//
// The returned *slog.Logger is safe for concurrent use (the Logger's
// D-025 contract carries over; the bridge holds no mutable state).
func (l *Logger) Slog() *slog.Logger {
	return slog.New(&slogBridgeHandler{base: l})
}

// slogBridgeHandler adapts the telemetry Logger to slog.Handler.
// Immutable after construction: WithAttrs / WithGroup return derived
// handlers (mirroring Logger.With's derive semantics).
type slogBridgeHandler struct {
	base *Logger
	// groups is the open WithGroup stack; attrs added or handled while
	// groups are open nest inside them (standard slog.Handler
	// semantics).
	groups []string
}

// Enabled delegates to the underlying configured slog handler so the
// bridge honours the Logger's log_level.
func (h *slogBridgeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.handler.Enabled(ctx, level)
}

// Handle routes the record through the telemetry pipeline at the
// matching severity.
func (h *slogBridgeHandler) Handle(ctx context.Context, rec slog.Record) error {
	attrs := make([]slog.Attr, 0, rec.NumAttrs())
	rec.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	attrs = h.wrapGroups(attrs)
	switch {
	case rec.Level >= slog.LevelError:
		h.base.Error(ctx, rec.Message, attrs...)
	case rec.Level >= slog.LevelWarn:
		h.base.Warn(ctx, rec.Message, attrs...)
	case rec.Level >= slog.LevelInfo:
		h.base.Info(ctx, rec.Message, attrs...)
	default:
		h.base.Debug(ctx, rec.Message, attrs...)
	}
	return nil
}

// WithAttrs returns a derived handler whose base Logger carries the
// (group-wrapped) attrs as bound attributes.
func (h *slogBridgeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &slogBridgeHandler{
		base:   h.base.With(h.wrapGroups(attrs)...),
		groups: h.groups,
	}
}

// WithGroup returns a derived handler that nests subsequent attrs
// under name. An empty name is the documented slog no-op.
func (h *slogBridgeHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	groups := make([]string, 0, len(h.groups)+1)
	groups = append(groups, h.groups...)
	groups = append(groups, name)
	return &slogBridgeHandler{base: h.base, groups: groups}
}

// wrapGroups nests attrs inside the open group stack, innermost last.
func (h *slogBridgeHandler) wrapGroups(attrs []slog.Attr) []slog.Attr {
	if len(h.groups) == 0 || len(attrs) == 0 {
		return attrs
	}
	for i := len(h.groups) - 1; i >= 0; i-- {
		attrs = []slog.Attr{{Key: h.groups[i], Value: slog.GroupValue(attrs...)}}
	}
	return attrs
}
