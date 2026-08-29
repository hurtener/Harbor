// internal/llm/chunk_publisher.go — the per-run completion-chunk
// publisher constructor.
//
// The planner's streaming seam is `planner.RunContext.OnChunk
// (func(delta string, done bool, kind planner.ChunkKind))`: the
// planner invokes it per provider token delta and the runtime
// translates each delta into an `llm.completion.chunk` bus event
// under the run's identity quadruple. Previously that
// translation lived as a per-run closure hand-written in
// `cmd/harbor`'s run loop (and absent from the devstack mirror,
// leaving streaming silently dead on the official test surface).

package llm

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// NewChunkPublisher returns the per-run OnChunk closure a run-loop
// driver adapts onto `planner.RunContext.OnChunk`.
// The closure builds a [CompletionChunkPayload] and publishes
// [EventTypeCompletionChunk] with the run's identity quadruple on the
// **Event envelope**, not just the payload — the event bus validates
// the envelope before fan-out (CLAUDE.md §6 rule 5). Completion chunks
// use the additive LivePublisher capability when the bus provides it: they
// are non-durable, non-replayable animation frames with Sequence == 0, while
// the terminal answer/task lifecycle remains authoritative through the
// durable Publish path. Legacy/custom EventBus implementations that do not
// opt into LivePublisher receive no completion-chunk event: the constructor
// logs one explicit per-run warning and disables animation rather than
// reintroducing per-chunk durable writes. This is the hard-won trap the
// constructor encodes: when the original closure
// stamped the payload only, live testing surfaced 280+ rejected
// chunks per task ("events: event identity missing one or more
// components: type=llm.completion.chunk"). Publish failures Warn
// loudly — never silent drops.
//
// `kind` is string-typed because `planner` imports `llm`, so this
// package cannot name `planner.ChunkKind`; the run loop adapts with a
// one-line wrapper:
//
//	pub := llm.NewChunkPublisher(bus, q, string(taskID), logger)
//	onChunk := func(d string, done bool, k planner.ChunkKind) { pub(d, done, string(k)) }
//
// The live-publish context is `context.Background()` — the documented
// bridge across an unmanaged async boundary (CLAUDE.md §5) for callers
// that genuinely have no lifetime ctx. Run-loop drivers are NOT that
// caller: they own a driver-lifetime ctx and MUST use
// [NewChunkPublisherContext] so publishes are bounded by it. The live lane
// never touches durable storage, but the context still bounds redaction and
// admission work and keeps the callback's cancellation contract explicit.
//
// Concurrent reuse: the constructor allocates no shared
// mutable state — each run constructs its own closure over the run's
// quadruple + task ID; N concurrent runs see N independent closures
// over one shared (concurrent-safe) bus.
//
// A nil logger defaults to slog.Default() so the failure path stays
// loud for every caller.
func NewChunkPublisher(bus events.EventBus, q identity.Quadruple, taskID string, logger *slog.Logger) func(delta string, done bool, kind string) {
	return NewChunkPublisherContext(context.Background(), bus, q, taskID, logger)
}

// NewChunkPublisherContext is [NewChunkPublisher] with a
// caller-supplied base context bounding every live publish. Run-loop drivers
// pass their driver-lifetime context (the earlier `d.subCtx` semantics), so
// cancellation stops late animation callbacks at driver teardown. Publish
// failures (including baseCtx cancellation) Warn loudly, never silently drop.
//
// A nil baseCtx falls back to context.Background() (the ctx-less
// constructor's documented bridge).
func NewChunkPublisherContext(baseCtx context.Context, bus events.EventBus, q identity.Quadruple, taskID string, logger *slog.Logger) func(delta string, done bool, kind string) {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	live, liveOK := bus.(events.LivePublisher)
	var disabledWarning sync.Once
	return func(delta string, done bool, kind string) {
		if !liveOK {
			disabledWarning.Do(func() {
				logger.Warn("llm: completion animation disabled",
					slog.String("task_id", taskID),
					slog.String("run_id", q.RunID),
					slog.String("reason", "events.EventBus does not implement events.LivePublisher"))
			})
			return
		}
		now := time.Now()
		payload := CompletionChunkPayload{
			Identity:   q,
			TaskID:     taskID,
			RunID:      q.RunID,
			Delta:      delta,
			Done:       done,
			Kind:       kind,
			OccurredAt: now,
		}
		if pubErr := live.PublishLive(baseCtx, events.Event{
			Type:       EventTypeCompletionChunk,
			Identity:   q,
			OccurredAt: now,
			Payload:    payload,
		}); pubErr != nil {
			logger.Warn("llm: completion-chunk publish failed",
				slog.String("task_id", taskID),
				slog.String("run_id", q.RunID),
				slog.String("err", pubErr.Error()))
		}
	}
}
