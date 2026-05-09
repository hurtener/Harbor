// Package eventbus is the adapter that connects Phase 04's
// telemetry.BusEmitter seam to Phase 05's events.EventBus. The
// adapter lives in its own package — neither `internal/telemetry`
// nor `internal/events` import the other, so the dependency edge
// passes through the wiring layer where it belongs.
//
// Construction is straightforward; the canonical usage from
// `cmd/harbor` (or any future bootstrap site) is:
//
//	bus, _ := events.Open(ctx, cfg.Events, redactor)
//	logger, _ := telemetry.New(cfg.Telemetry, redactor,
//	    telemetry.WithBusEmitter(eventbus.New(bus)))
//
// After this wiring, `Logger.Error(ctx, msg, attrs...)` emits both
// the slog record AND a `runtime.error` event onto the bus.
package eventbus

import (
	"context"
	"log/slog"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// Adapter satisfies telemetry.BusEmitter by publishing
// runtime.error events to the wrapped events.EventBus.
type Adapter struct {
	bus events.EventBus
}

// New constructs an Adapter wrapping bus. nil bus returns a nil
// Adapter — callers that want a no-op emitter should omit
// telemetry.WithBusEmitter rather than passing nil through here.
func New(bus events.EventBus) *Adapter {
	if bus == nil {
		return nil
	}
	return &Adapter{bus: bus}
}

// EmitRuntimeError converts the Logger.Error payload to a
// runtime.error event and publishes it. Best-effort: a publish
// failure (closed bus, redactor error, missing identity) does NOT
// propagate back into Logger.Error — the slog record has already
// been written, so the operator still sees the error.
//
// Identity is recovered from ctx via identity.QuadrupleFrom (or
// identity.From if no Quadruple is present). The bus rejects
// publishes with a missing identity triple, so when ctx carries no
// identity at all the adapter quietly skips publishing — Logger
// already wrote the slog record.
func (a *Adapter) EmitRuntimeError(ctx context.Context, msg string, attrs []slog.Attr) {
	if a == nil || a.bus == nil {
		return
	}
	id, ok := quadrupleFrom(ctx)
	if !ok {
		return
	}
	fields := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		fields[attr.Key] = attr.Value.Any()
	}
	ev := events.Event{
		Type:     events.EventTypeRuntimeError,
		Identity: id,
		Payload:  events.RuntimeErrorPayload{Message: msg, Fields: fields},
	}
	// Best-effort. Don't propagate Publish errors back to Logger.
	_ = a.bus.Publish(ctx, ev)
}

// quadrupleFrom returns a fully-specified Quadruple from ctx, or
// (zero, false) when the triple is incomplete. Prefers the
// Quadruple-key form (carries RunID) but falls back to the Identity
// form when only the triple is present.
func quadrupleFrom(ctx context.Context) (identity.Quadruple, bool) {
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		if q.TenantID != "" && q.UserID != "" && q.SessionID != "" {
			return q, true
		}
	}
	if id, ok := identity.From(ctx); ok {
		if id.TenantID != "" && id.UserID != "" && id.SessionID != "" {
			return identity.Quadruple{Identity: id}, true
		}
	}
	return identity.Quadruple{}, false
}
