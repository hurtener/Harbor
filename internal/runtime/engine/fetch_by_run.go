package engine

import (
	"context"
	"errors"

	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// FetchByRun reads from the dispatcher's per-run subqueue. Blocks
// until an envelope arrives, ctx cancels, the run is cancelled, or
// the engine stops.
//
// Concurrent-fetcher contract per brief 01 §5: a single per-run
// subqueue means concurrent FetchByRun calls would race for ordering.
// The API forbids the contention rather than serializing under the
// hood — a second concurrent fetch returns ErrConcurrentFetchByRun.
//
// Failure modes:
//   - ErrEngineStopped: Stop was called.
//   - ErrRunCancelled: Cancel(runID) closed the subqueue.
//   - ErrConcurrentFetchByRun: another goroutine is mid-fetch on this run.
//   - ctx.Err(): the caller's ctx cancelled while waiting.
//
// Identity propagation: the returned envelope carries the run's
// quadruple (the dispatcher demuxes by RunID; the per-run subqueue
// only sees envelopes whose RunID matches the request).
func (e *engine) FetchByRun(ctx context.Context, runID string) (messages.Envelope, error) {
	if e.stopped.Load() {
		return messages.Envelope{}, ErrEngineStopped
	}
	if runID == "" {
		return messages.Envelope{}, errors.New("engine: FetchByRun requires a non-empty runID")
	}
	if rc, cancelled := e.runIsCancelled(runID); cancelled && rc != nil {
		return messages.Envelope{}, ErrRunCancelled
	}
	return e.dispatcher.fetchByRun(ctx, runID)
}
