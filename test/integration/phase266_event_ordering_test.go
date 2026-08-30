// Phase 266 integration gates for the durable publication boundary.
//
// These tests deliberately exercise only the stable EventBus and Replayer
// interfaces. The baseline does not expose a batch-publication method, so a
// "batch" here is the ordered set of canonical events a runtime would hand to
// such a method. Keeping the assertion on individual Event records makes the
// gate useful before and after an additive batch API lands: every record must
// retain its own sequence and the replay cursor must never flatten or reorder
// the set.
package integration_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// phase266CountingStore keeps the durable driver real while exposing the
// transaction count needed by the optional PublishBatch gate.
type phase266CountingStore struct {
	state.StateStore
	batchCalls atomic.Int64
}

func (s *phase266CountingStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	s.batchCalls.Add(1)
	return s.StateStore.SaveBatchIf(ctx, expectations, writes)
}

// phase266BatchPublisher is intentionally structural: the baseline has no
// BatchPublisher symbol yet, but a candidate that adds PublishBatch satisfies
// this interface without forcing the gate to invent a parallel API.
type phase266BatchPublisher interface {
	PublishBatch(context.Context, []events.Event) error
}

func phase266EventsConfig() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 32,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Hour,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
		LegacyWritersDrained:     true,
	}
}

func phase266Quadruple() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "phase266-tenant",
			UserID:    "phase266-user",
			SessionID: "phase266-session",
		},
		RunID: "phase266-run",
	}
}

func phase266Filter(id identity.Quadruple) events.Filter {
	return events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Run:     id.RunID,
	}
}

func phase266OrderedBatch(id identity.Quadruple) []events.Event {
	base := time.Unix(1_754_000_000, 0)
	return []events.Event{
		{
			Type:       tools.EventTypeToolInvoked,
			Identity:   id,
			OccurredAt: base,
			Payload: tools.ToolInvokedPayload{
				Identity:  id,
				ToolName:  "phase266-search",
				Transport: tools.TransportInProcess,
				StartedAt: base,
			},
		},
		{
			Type:       llm.EventTypeCostRecorded,
			Identity:   id,
			OccurredAt: base.Add(time.Millisecond),
			Payload: llm.CostRecordedPayload{
				Identity:            id,
				Model:               "phase266-model",
				Cost:                llm.Cost{TotalCost: 0.01, Currency: "USD"},
				Usage:               llm.Usage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20},
				ContextWindowTokens: 4096,
				OccurredAt:          base.Add(time.Millisecond),
			},
		},
		{
			Type:       tools.EventTypeToolCompleted,
			Identity:   id,
			OccurredAt: base.Add(2 * time.Millisecond),
			Payload: tools.ToolCompletedPayload{
				Identity:   id,
				ToolName:   "phase266-search",
				Transport:  tools.TransportInProcess,
				Attempts:   1,
				DurationMS: 7,
			},
		},
		{
			Type:       tasks.EventTypeTaskCompleted,
			Identity:   id,
			OccurredAt: base.Add(3 * time.Millisecond),
			Payload: tasks.TaskCompletedPayload{
				TaskID: tasks.TaskID("phase266-task"),
			},
		},
	}
}

func phase266Replay(t *testing.T, rp events.Replayer, id identity.Quadruple, cursor uint64) []events.Event {
	t.Helper()
	got, err := rp.Replay(context.Background(), events.Cursor{
		SessionID: id.SessionID,
		Sequence:  cursor,
	}, phase266Filter(id))
	if err != nil {
		t.Fatalf("Replay(cursor=%d): %v", cursor, err)
	}
	return got
}

func assertPhase266Sequence(t *testing.T, got []events.Event, want []events.Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("replay returned %d events, want %d: %#v", len(got), len(want), got)
	}
	for i, ev := range got {
		if ev.Sequence != uint64(i+1) {
			t.Errorf("event %d has sequence %d, want %d", i, ev.Sequence, i+1)
		}
		if ev.Type != want[i].Type {
			t.Errorf("event %d has type %q, want %q", i, ev.Type, want[i].Type)
		}
		if ev.Identity != want[i].Identity {
			t.Errorf("event %d identity = %+v, want %+v", i, ev.Identity, want[i].Identity)
		}
	}
}

// phase266PublishBatch exercises the additive PublishBatch capability when
// a candidate exposes it. Baseline drivers retain the stable Publish fallback.
// A batch-capable durable driver must commit all records in one real
// StateStore.SaveBatchIf transaction.
func phase266PublishBatch(t *testing.T, bus events.EventBus, store *phase266CountingStore, batch []events.Event) {
	t.Helper()
	if bp, ok := bus.(phase266BatchPublisher); ok {
		before := store.batchCalls.Load()
		if err := bp.PublishBatch(context.Background(), batch); err != nil {
			t.Fatalf("PublishBatch: %v", err)
		}
		if calls := store.batchCalls.Load() - before; calls != 1 {
			t.Fatalf("PublishBatch used %d durable transactions, want exactly 1", calls)
		}
		return
	}
	for i, ev := range batch {
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatalf("Publish fallback event %d (%s): %v", i, ev.Type, err)
		}
	}
}

// TestE2E_Phase266_DurableIndividualSequenceReplay proves the integrated
// real-driver path preserves ordering and per-record sequence identity across
// a restart. The same assertions cover a future additive PublishBatch path:
// a batch is still replayed as its exact individual canonical records.
func TestE2E_Phase266_DurableIndividualSequenceReplay(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	countingStore := &phase266CountingStore{StateStore: store}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	id := phase266Quadruple()
	want := phase266OrderedBatch(id)

	bus1, err := durable.New(context.Background(), phase266EventsConfig(), auditpatterns.New(), countingStore)
	if err != nil {
		t.Fatalf("durable.New (first runtime): %v", err)
	}
	phase266PublishBatch(t, bus1, countingStore, want)
	firstReplay := phase266Replay(t, bus1.(events.Replayer), id, 0)
	assertPhase266Sequence(t, firstReplay, want)
	if err := bus1.Close(context.Background()); err != nil {
		t.Fatalf("first runtime Close: %v", err)
	}

	// Rebuilding the bus over the same real StateStore is the process-loss /
	// restart boundary. No in-memory subscriber or sequence state is reused.
	bus2, err := durable.New(context.Background(), phase266EventsConfig(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New (rebuilt runtime): %v", err)
	}
	t.Cleanup(func() { _ = bus2.Close(context.Background()) })
	rp2 := bus2.(events.Replayer)
	fullReplay := phase266Replay(t, rp2, id, 0)
	assertPhase266Sequence(t, fullReplay, want)

	// A reconnect cursor must return exact individual records, not a single
	// flattened batch envelope and not a duplicated boundary event.
	tail := phase266Replay(t, rp2, id, 1)
	if len(tail) != len(want)-1 {
		t.Fatalf("replay after cursor 1 returned %d events, want %d", len(tail), len(want)-1)
	}
	for i, ev := range tail {
		wantSeq := uint64(i + 2)
		if ev.Sequence != wantSeq || ev.Type != want[i+1].Type {
			t.Errorf("tail event %d = (%d,%q), want (%d,%q)", i, ev.Sequence, ev.Type, wantSeq, want[i+1].Type)
		}
	}
}

// TestE2E_Phase266_RestartDoesNotInventTerminalState documents the honest
// process-loss boundary: rebuilding a durable bus replays only committed
// records. It must not infer a task completion for work that had no terminal
// event before the process stopped.
func TestE2E_Phase266_RestartDoesNotInventTerminalState(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	id := phase266Quadruple()
	first := phase266OrderedBatch(id)[0]

	bus1, err := durable.New(context.Background(), phase266EventsConfig(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New (first runtime): %v", err)
	}
	if err := bus1.Publish(context.Background(), first); err != nil {
		t.Fatalf("Publish committed prefix: %v", err)
	}
	if err := bus1.Close(context.Background()); err != nil {
		t.Fatalf("first runtime Close: %v", err)
	}

	bus2, err := durable.New(context.Background(), phase266EventsConfig(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New (rebuilt runtime): %v", err)
	}
	t.Cleanup(func() { _ = bus2.Close(context.Background()) })
	got := phase266Replay(t, bus2.(events.Replayer), id, 0)
	if len(got) != 1 || got[0].Type != tools.EventTypeToolInvoked || got[0].Sequence != 1 {
		t.Fatalf("replay after process-loss boundary = %#v, want only committed tool.invoked seq=1", got)
	}
	for _, ev := range got {
		if ev.Type == tasks.EventTypeTaskCompleted {
			t.Fatal("replay invented task.completed after process-loss boundary")
		}
	}
}
