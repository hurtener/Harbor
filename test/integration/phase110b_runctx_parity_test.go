// Phase 110b cross-subsystem integration test per CLAUDE.md §17.
//
// Phase 110b promotes the RunContext-population helpers to
// `internal/runtime/runctx` and the two per-run event-emission
// closures to constructors on their owning packages
// (`events.IdentityStampingEmitter`, `llm.NewChunkPublisher`), and
// closes the devstack's missing parity: its RunSpec now wires
// Emit + OnChunk and its MarkComplete carries the 110a-exported
// `planner.AnswerEnvelope` instead of an empty TaskResult{}.
//
// What this test proves (the phase plan's devstack-parity gate):
//
//  1. A devstack-run task produces `planner.decision` AND
//     `llm.completion.chunk` events on the stack's bus — both were
//     silently dead on the kit before 110b (neither closure was
//     wired), so devstack validated weaker semantics than production.
//  2. Identity propagation: every delivered event carries the run's
//     full quadruple on the Event ENVELOPE (the 280-rejected-chunks
//     regression class) — asserted across N=10 concurrent runs with
//     no cross-run identity bleed (§17.3 concurrency stress).
//  3. The completed task's `Result.Value` parses as
//     `planner.AnswerEnvelope` with a non-empty answer (the audit's
//     empty-`TaskResult{}` drift is closed) — a `tasks.get`-shaped
//     read sees a real result.
//  4. Failure mode (§17.3 #3): a bus closed mid-run produces LOUD
//     Warns from the promoted publisher, never silent drops.
//
// Real drivers everywhere on the seam (§17.3): real audit redactor,
// real inmem EventBus, real StateStore, real inprocess TaskRegistry,
// real steering.RunLoop, real ReAct planner — assembled through
// `harbortest/devstack.Assemble`. The streaming recorder LLM is the
// only stub (it records + replays deterministic deltas so the chunk
// path is exercised hermetically).

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/tasks"
)

// phase110bStreamingLLM is a deterministic streaming recorder: when
// the planner wires the streaming hooks (req.Stream + OnContent), it
// replays a fixed delta sequence before returning a terminal finish.
// blockUntil (optional) gates the stream so the failure-mode test can
// close the bus BEFORE the deltas fire.
type phase110bStreamingLLM struct {
	mu         sync.Mutex
	calls      int
	answer     string
	started    chan struct{} // closed on first Complete entry (when non-nil)
	blockUntil chan struct{} // Complete waits on it before streaming (when non-nil)
	once       sync.Once
}

func (c *phase110bStreamingLLM) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.started != nil {
		c.once.Do(func() { close(c.started) })
	}
	if c.blockUntil != nil {
		select {
		case <-c.blockUntil:
		case <-ctx.Done():
			return llm.CompleteResponse{}, ctx.Err()
		}
	}
	if req.Stream && req.OnContent != nil {
		req.OnContent("streamed ", false)
		req.OnContent("delta", false)
		req.OnContent("", true)
	}
	// Under the native tool-calling path, a response with Content and
	// no ToolCalls is the model's terminal answer — the projector maps
	// it to Finish{Reason: goal, Payload: {"answer": Content}}.
	return llm.CompleteResponse{Content: c.answer}, nil
}

func (c *phase110bStreamingLLM) Close(_ context.Context) error { return nil }

func phase110bConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(context.Background(), writeDevConfig(t))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func phase110bLLMSnapshot(cfg *config.Config) *llm.ConfigSnapshot {
	return &llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: cfg.LLM.ContextWindowReserve,
		HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
		ModelProfiles: map[string]llm.ModelProfile{
			"anthropic/claude-sonnet-4": {
				ContextWindowTokens: 200000,
				TokenEstimator:      "chars_div_4",
			},
		},
	}
}

// TestE2E_Phase110b_DevstackEmitsPlannerAndChunkEvents_EnvelopeParity
// is the headline parity gate: N=10 concurrent devstack-run tasks
// each produce planner.decision + llm.completion.chunk events on the
// bus under their own run quadruple, and each completed task carries
// a parsed planner.AnswerEnvelope with a non-empty answer.
func TestE2E_Phase110b_DevstackEmitsPlannerAndChunkEvents_EnvelopeParity(t *testing.T) {
	t.Parallel()
	const taskCount = 10

	rec := &phase110bStreamingLLM{answer: "parity answer"}
	cfg := phase110bConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: phase110bLLMSnapshot(cfg),
		PlannerOverride:   react.New(rec),
	})
	defer stack.Close()

	devID := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// Subscribe BEFORE spawning so no event is missed. Triple-scoped
	// (not admin) — the events under test carry the dev triple, so a
	// plain session subscription seeing them IS the identity-
	// propagation assertion.
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	sub, err := stack.Bus.Subscribe(subCtx, events.Filter{
		Tenant:  devID.TenantID,
		User:    devID.UserID,
		Session: devID.SessionID,
		Types: []events.EventType{
			planner.EventTypePlannerDecision,
			llm.EventTypeCompletionChunk,
		},
	})
	if err != nil {
		t.Fatalf("Bus.Subscribe: %v", err)
	}

	// Spawn N concurrent foreground tasks.
	ids := make([]tasks.TaskID, 0, taskCount)
	for i := range taskCount {
		h, sErr := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
			Identity: identity.Quadruple{Identity: devID},
			Kind:     tasks.KindForeground,
			Query:    fmt.Sprintf("parity query %d", i),
		})
		if sErr != nil {
			t.Fatalf("Tasks.Spawn(%d): %v", i, sErr)
		}
		ids = append(ids, h.ID)
	}

	// Drain the subscription until every run produced ≥1 decision and
	// ≥3 chunks (the recorder's fixed delta count), bounded by a
	// real-time deadline (channel-driven — no sleep-as-sync).
	type runEvents struct{ decisions, chunks int }
	perRun := make(map[string]*runEvents, taskCount)
	for _, id := range ids {
		perRun[string(id)] = &runEvents{}
	}
	complete := func() bool {
		for _, re := range perRun {
			if re.decisions < 1 || re.chunks < 3 {
				return false
			}
		}
		return true
	}
	deadline := time.After(15 * time.Second)
	for !complete() {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before all events arrived")
			}
			re, known := perRun[ev.Identity.RunID]
			if !known {
				t.Fatalf("event carries unknown RunID %q — identity bleed or stamping gap", ev.Identity.RunID)
			}
			// Identity propagation: full quadruple on the ENVELOPE.
			if ev.Identity.TenantID != devID.TenantID ||
				ev.Identity.UserID != devID.UserID ||
				ev.Identity.SessionID != devID.SessionID {
				t.Fatalf("event envelope identity = %+v, want dev triple", ev.Identity)
			}
			switch ev.Type {
			case planner.EventTypePlannerDecision:
				re.decisions++
			case llm.EventTypeCompletionChunk:
				payload, ok := ev.Payload.(llm.CompletionChunkPayload)
				if !ok {
					t.Fatalf("chunk payload is %T, want llm.CompletionChunkPayload", ev.Payload)
				}
				// The TaskID doubles as the RunID at this layer — a
				// chunk whose payload names another task is bleed.
				if payload.TaskID != ev.Identity.RunID {
					t.Fatalf("cross-run bleed: chunk for task %q delivered under RunID %q",
						payload.TaskID, ev.Identity.RunID)
				}
				re.chunks++
			}
		case <-deadline:
			t.Fatalf("timed out waiting for parity events; got %+v", perRun)
		}
	}

	// Every task completes with the marshalled AnswerEnvelope — the
	// audit's empty-TaskResult{} drift is closed.
	for _, id := range ids {
		var task *tasks.Task
		waitDeadline := time.Now().Add(10 * time.Second)
		for {
			got, gErr := stack.Tasks.Get(idCtx, id)
			if gErr == nil && (got.Status == tasks.StatusComplete || got.Status == tasks.StatusFailed) {
				task = got
				break
			}
			if time.Now().After(waitDeadline) {
				t.Fatalf("task %s never reached a terminal status", id)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if task.Status != tasks.StatusComplete {
			t.Fatalf("task %s status = %s, want complete (err=%+v)", id, task.Status, task.Error)
		}
		if task.Result == nil || len(task.Result.Value) == 0 {
			t.Fatalf("task %s Result is empty — devstack MarkComplete envelope parity broken", id)
		}
		var envelope planner.AnswerEnvelope
		if uErr := json.Unmarshal(task.Result.Value, &envelope); uErr != nil {
			t.Fatalf("task %s Result.Value does not parse as planner.AnswerEnvelope: %v", id, uErr)
		}
		if envelope.Answer != "parity answer" {
			t.Errorf("task %s envelope.Answer = %q, want %q", id, envelope.Answer, "parity answer")
		}
		if envelope.FinishReason != string(planner.FinishGoal) {
			t.Errorf("task %s envelope.FinishReason = %q, want %q", id, envelope.FinishReason, planner.FinishGoal)
		}
	}
}

// syncLogBuffer is a mutex-guarded bytes.Buffer slog can write to
// from the driver's goroutines while the test reads it.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestE2E_Phase110b_ClosedBusMidRun_WarnsLoudly is the §17.3 failure
// mode: the bus is closed while a run is in flight; the promoted
// chunk publisher must Warn loudly on every failed publish — never a
// silent drop, never a panic, and the driver still drains cleanly.
func TestE2E_Phase110b_ClosedBusMidRun_WarnsLoudly(t *testing.T) {
	t.Parallel()

	logBuf := &syncLogBuffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	started := make(chan struct{})
	release := make(chan struct{})
	rec := &phase110bStreamingLLM{
		answer:     "never delivered",
		started:    started,
		blockUntil: release,
	}
	cfg := phase110bConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: phase110bLLMSnapshot(cfg),
		PlannerOverride:   react.New(rec),
		Logger:            logger,
	})
	defer stack.Close()

	devID := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: devID},
		Kind:     tasks.KindForeground,
		Query:    "doomed run",
	}); err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}

	// Wait for the run to reach the LLM (the planner is mid-step),
	// then close the bus underneath it and release the stream.
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("run never reached the LLM")
	}
	if err := stack.Bus.Close(context.Background()); err != nil {
		t.Fatalf("Bus.Close: %v", err)
	}
	close(release)

	// The publisher's Warn must surface (bounded poll on the log; the
	// warn is emitted synchronously inside the chunk callback, the
	// poll only covers goroutine scheduling).
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logBuf.String(), "completion-chunk publish failed") {
		if time.Now().After(deadline) {
			t.Fatalf("no loud Warn from the chunk publisher after bus close; log:\n%s", logBuf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
