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
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// Completion chunk publishing builds a [CompletionChunkPayload] and publishes
// [EventTypeCompletionChunk] with the run's identity quadruple on the
// **Event envelope**, not just the payload — the event bus validates
// the envelope before fan-out (CLAUDE.md §6 rule 5). The immediate
// LivePublisher frame is non-durable animation with Sequence == 0. The same
// event is retained in a per-run memory buffer and becomes replayable only
// when the runtime calls ChunkPublisher.Flush at an LLM-step boundary.
//
// Legacy/custom EventBus implementations that do not opt into LivePublisher
// receive no animation event: the constructor logs one explicit per-run
// warning. A bus without the additive persist-only capability fails loudly at
// Flush rather than falling back to Publish and duplicating fan-out. This is
// the hard-won trap the constructor encodes: when the original closure
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
// A nil logger defaults to slog.Default() so the failure path stays loud.
//
// ChunkPublisher is the per-run completion-chunk bridge. OnChunk delivers a
// transient Sequence=0 frame immediately through LivePublisher and retains
// the same event in memory until Flush persists it through the additive
// persist-only lane. It owns no storage and performs no storage I/O while a
// provider callback is running.
type ChunkPublisher struct {
	baseCtx context.Context
	live    events.LivePublisher
	persist events.PersistBatchPublisher
	q       identity.Quadruple
	taskID  string
	logger  *slog.Logger

	mu              sync.Mutex
	flushMu         sync.Mutex
	pending         []events.Event
	disabledWarning sync.Once
}

// NewChunkPublisher returns the per-run OnChunk closure. Run-loop drivers that
// need durable replay should use NewBufferedChunkPublisherContext and install
// its Flush method at planner-step boundaries.
func NewChunkPublisher(bus events.EventBus, q identity.Quadruple, taskID string, logger *slog.Logger) func(delta string, done bool, kind string) {
	return NewBufferedChunkPublisherContext(context.Background(), bus, q, taskID, logger).OnChunk
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
	return NewBufferedChunkPublisherContext(baseCtx, bus, q, taskID, logger).OnChunk
}

// NewBufferedChunkPublisher constructs the per-run completion-chunk bridge
// with a background context. Callers must invoke Flush after each planner
// step to make buffered chunks replayable.
func NewBufferedChunkPublisher(bus events.EventBus, q identity.Quadruple, taskID string, logger *slog.Logger) *ChunkPublisher {
	return NewBufferedChunkPublisherContext(context.Background(), bus, q, taskID, logger)
}

// NewBufferedChunkPublisherContext is NewBufferedChunkPublisher with a
// caller-supplied base context bounding every live publish. PersistBatch is
// called only by the explicit Flush method.
func NewBufferedChunkPublisherContext(baseCtx context.Context, bus events.EventBus, q identity.Quadruple, taskID string, logger *slog.Logger) *ChunkPublisher {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	live, _ := bus.(events.LivePublisher)
	persist, _ := bus.(events.PersistBatchPublisher)
	return &ChunkPublisher{
		baseCtx: baseCtx,
		live:    live,
		persist: persist,
		q:       q,
		taskID:  taskID,
		logger:  logger,
	}
}

// OnChunk publishes one live frame immediately and appends the event to this
// run's in-memory ordered buffer. Every chunk kind, including done and
// reasoning chunks, follows the same path.
func (p *ChunkPublisher) OnChunk(delta string, done bool, kind string) {
	now := time.Now()
	payload := CompletionChunkPayload{
		Identity:   p.q,
		TaskID:     p.taskID,
		RunID:      p.q.RunID,
		Delta:      delta,
		Done:       done,
		Kind:       kind,
		OccurredAt: now,
	}
	ev := events.Event{
		Type:       EventTypeCompletionChunk,
		Identity:   p.q,
		OccurredAt: now,
		Payload:    payload,
	}

	// Serialize callback processing so the buffer order is the order observed
	// by this bridge. The live call remains storage-free.
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.live == nil {
		p.disabledWarning.Do(func() {
			p.logger.Warn("llm: completion animation disabled",
				slog.String("task_id", p.taskID),
				slog.String("run_id", p.q.RunID),
				slog.String("reason", "events.EventBus does not implement events.LivePublisher"))
		})
	} else if pubErr := p.live.PublishLive(p.baseCtx, ev); pubErr != nil {
		p.logger.Warn("llm: completion-chunk publish failed",
			slog.String("task_id", p.taskID),
			slog.String("run_id", p.q.RunID),
			slog.String("err", pubErr.Error()))
	}
	p.pending = append(p.pending, ev)
}

// Flush durably commits all chunks accepted by OnChunk, in exact order and in
// batches of at most events.DefaultPublishBatchSize. A legacy bus fails
// loudly instead of falling back to Publish and duplicating fan-out.
func (p *ChunkPublisher) Flush(ctx context.Context) error {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()

	p.mu.Lock()
	if len(p.pending) == 0 {
		p.mu.Unlock()
		return nil
	}
	if p.persist == nil {
		p.mu.Unlock()
		return fmt.Errorf("llm: persist completion chunks: %w", events.ErrPersistBatchUnsupported)
	}
	pending := append([]events.Event(nil), p.pending...)
	p.pending = p.pending[:0]
	p.mu.Unlock()

	if ctx == nil {
		ctx = p.baseCtx
	}
	if ctx == nil {
		p.restorePending(pending)
		return fmt.Errorf("llm: persist completion chunks: nil context")
	}
	for start := 0; start < len(pending); start += events.DefaultPublishBatchSize {
		end := start + events.DefaultPublishBatchSize
		if end > len(pending) {
			end = len(pending)
		}
		if err := ctx.Err(); err != nil {
			p.restorePending(pending[start:])
			return fmt.Errorf("llm: persist completion chunks: %w", err)
		}
		if err := p.persist.PersistBatch(ctx, pending[start:end]); err != nil {
			p.restorePending(pending[start:])
			return fmt.Errorf("llm: persist completion chunks batch [%d:%d]: %w", start, end, err)
		}
	}
	return nil
}

func (p *ChunkPublisher) restorePending(eventsToRestore []events.Event) {
	if len(eventsToRestore) == 0 {
		return
	}
	p.mu.Lock()
	p.pending = append(append([]events.Event(nil), eventsToRestore...), p.pending...)
	p.mu.Unlock()
}
