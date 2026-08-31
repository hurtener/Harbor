package steering

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

type chunkFlushPlanner struct {
	order *[]string
}

func (p *chunkFlushPlanner) Next(context.Context, planner.RunContext) (planner.Decision, error) {
	*p.order = append(*p.order, "next")
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

func TestRun_AfterPlannerStepRunsBeforeDecisionProgression(t *testing.T) {
	order := make([]string, 0, 2)
	rl, _, _ := newTestRunLoop(t)
	spec := runSpecFor(runA, &chunkFlushPlanner{order: &order})
	spec.Base.AfterPlannerStep = func(context.Context) error {
		order = append(order, "flush")
		return nil
	}
	if _, err := rl.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reflect.DeepEqual(order, []string{"next", "flush"}) {
		t.Fatalf("step order = %v, want [next flush]", order)
	}
}

func TestRun_AfterPlannerStepFailurePropagates(t *testing.T) {
	want := errors.New("chunk persistence failed")
	rl, _, _ := newTestRunLoop(t)
	spec := runSpecFor(runA, &chunkFlushPlanner{order: new([]string)})
	var calls int
	spec.Base.AfterPlannerStep = func(context.Context) error {
		calls++
		return want
	}
	if _, err := rl.Run(context.Background(), spec); err == nil || !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want wrapped chunk persistence failure", err)
	}
	if calls != 1 {
		t.Fatalf("AfterPlannerStep calls = %d, want 1", calls)
	}
}

// retainedOnChunkPlanner keeps the per-step callback after Next returns. A
// provider can legally finish its callback on another goroutine, so terminal
// sealing must define the admission boundary rather than assuming Next has
// joined every callback.
type retainedOnChunkPlanner struct {
	mu      sync.Mutex
	onChunk func(string, bool, planner.ChunkKind)
}

func (p *retainedOnChunkPlanner) Next(_ context.Context, rc planner.RunContext) (planner.Decision, error) {
	p.mu.Lock()
	p.onChunk = rc.OnChunk
	p.mu.Unlock()
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

func (p *retainedOnChunkPlanner) emit(delta string, done bool, kind planner.ChunkKind) {
	p.mu.Lock()
	onChunk := p.onChunk
	p.mu.Unlock()
	if onChunk != nil {
		onChunk(delta, done, kind)
	}
}

// terminalChunkBus is a narrow replay authority for the run-loop race tests.
// PersistBatch deliberately does not append to live: the production durable
// and inmem implementations use the same persist-only/no-fan-out contract.
type terminalChunkBus struct {
	mu         sync.Mutex
	live       []events.Event
	persisted  []events.Event
	persistErr error
}

func (b *terminalChunkBus) Publish(context.Context, events.Event) error { return nil }

func (b *terminalChunkBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, errors.New("terminalChunkBus: Subscribe unsupported")
}

func (b *terminalChunkBus) Close(context.Context) error { return nil }

func (b *terminalChunkBus) PublishLive(_ context.Context, ev events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ev.Sequence = 0
	b.live = append(b.live, ev)
	return nil
}

func (b *terminalChunkBus) PersistBatch(_ context.Context, batch []events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.persistErr != nil {
		return b.persistErr
	}
	for _, ev := range batch {
		ev.Sequence = uint64(len(b.persisted) + 1)
		b.persisted = append(b.persisted, ev)
	}
	return nil
}

func (b *terminalChunkBus) Replay(ctx context.Context, _ events.Cursor, _ events.Filter) ([]events.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]events.Event(nil), b.persisted...), nil
}

func (b *terminalChunkBus) snapshot() (live, persisted []events.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]events.Event(nil), b.live...), append([]events.Event(nil), b.persisted...)
}

func TestRun_TerminalChunkSealDrainsLateCallbackAndRejectsPostSeal(t *testing.T) {
	bus := &terminalChunkBus{}
	publisher := llm.NewBufferedChunkPublisher(bus, runA, "task-terminal-seal", nil)
	plannerUnderTest := &retainedOnChunkPlanner{}
	stepFlushed := make(chan struct{})
	releaseStep := make(chan struct{})
	callbackDone := make(chan struct{})

	rl, _, _ := newTestRunLoop(t)
	spec := runSpecFor(runA, plannerUnderTest)
	spec.Base.OnChunk = func(delta string, done bool, kind planner.ChunkKind) {
		publisher.OnChunk(delta, done, string(kind))
	}
	spec.Base.AfterPlannerStep = func(ctx context.Context) error {
		if err := publisher.Flush(ctx); err != nil {
			return err
		}
		close(stepFlushed)
		<-releaseStep
		return nil
	}
	spec.Base.SealCompletionChunks = publisher.Seal

	go func() {
		<-stepFlushed
		// This callback is deliberately concurrent with the terminal path,
		// but accepted before AfterPlannerStep releases the run to Seal.
		plannerUnderTest.emit("late", true, planner.ChunkContent)
		close(callbackDone)
		close(releaseStep)
	}()

	fin, err := rl.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Fatalf("Finish.Reason = %q, want %q", fin.Reason, planner.FinishGoal)
	}
	<-callbackDone

	live, persisted := bus.snapshot()
	if len(live) != 1 {
		t.Fatalf("live events = %d, want one callback accepted before seal", len(live))
	}
	if len(persisted) != len(live) || persisted[0].Sequence == 0 {
		t.Fatalf("persisted events = %+v, want the accepted live callback with durable sequence", persisted)
	}
	replayed, replayErr := bus.Replay(context.Background(), events.Cursor{SessionID: runA.SessionID}, events.Filter{Tenant: runA.TenantID, User: runA.UserID, Session: runA.SessionID})
	if replayErr != nil {
		t.Fatalf("Replay: %v", replayErr)
	}
	if len(replayed) != 1 || replayed[0].Sequence == 0 {
		t.Fatalf("replay = %+v, want accepted late callback", replayed)
	}

	// A callback after Run has returned observes the seal and cannot create a
	// new live frame or an unpersisted pending event.
	plannerUnderTest.emit("too-late", true, planner.ChunkContent)
	liveAfter, persistedAfter := bus.snapshot()
	if len(liveAfter) != len(live) || len(persistedAfter) != len(persisted) {
		t.Fatalf("post-seal callback changed lanes: live %d->%d persisted %d->%d", len(live), len(liveAfter), len(persisted), len(persistedAfter))
	}
}

func TestRun_TerminalChunkSealFailureDoesNotComplete(t *testing.T) {
	want := errors.New("terminal chunk persistence failed")
	bus := &terminalChunkBus{persistErr: want}
	publisher := llm.NewBufferedChunkPublisher(bus, runA, "task-terminal-seal-failure", nil)
	plannerUnderTest := &retainedOnChunkPlanner{}
	stepFlushed := make(chan struct{})
	releaseStep := make(chan struct{})

	rl, _, _ := newTestRunLoop(t)
	spec := runSpecFor(runA, plannerUnderTest)
	spec.Base.OnChunk = func(delta string, done bool, kind planner.ChunkKind) {
		publisher.OnChunk(delta, done, string(kind))
	}
	spec.Base.AfterPlannerStep = func(context.Context) error {
		close(stepFlushed)
		<-releaseStep
		return nil
	}
	spec.Base.SealCompletionChunks = publisher.Seal
	go func() {
		<-stepFlushed
		plannerUnderTest.emit("late", true, planner.ChunkContent)
		close(releaseStep)
	}()

	fin, err := rl.Run(context.Background(), spec)
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want terminal persistence error", err)
	}
	if fin.Reason != "" {
		t.Fatalf("Finish.Reason = %q, want empty on seal failure", fin.Reason)
	}
	live, persisted := bus.snapshot()
	if len(live) != 1 || len(persisted) != 0 {
		t.Fatalf("failure lanes = live:%d persisted:%d, want accepted live and no committed replay", len(live), len(persisted))
	}
}
