package telemetry

import (
	"context"
	"log/slog"
)

// BusEmitter is the seam Phase 05+ uses to make Logger.Error fire a
// paired runtime.error event. Phase 04 ships the noopEmitter default;
// the production emitter is wired via WithBusEmitter once the event
// bus lands.
//
// Implementations MUST NOT call back into the originating Logger
// (directly or transitively): Logger.Error guards against single-step
// recursion via a context sentinel, but a bus emitter that hands its
// payload to a *different* Logger that re-enters this one would
// bypass the guard. Keep emitter implementations simple.
type BusEmitter interface {
	EmitRuntimeError(ctx context.Context, msg string, attrs []slog.Attr)
}

// noopEmitter is the default BusEmitter — does nothing. Phase 04
// ships it so Logger.Error has a non-nil emitter pointer at all
// times; Phase 05 swaps in a real implementation via WithBusEmitter.
type noopEmitter struct{}

func (noopEmitter) EmitRuntimeError(_ context.Context, _ string, _ []slog.Attr) {}
