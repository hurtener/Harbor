// Package integration — Phase 150 (D-280): the run-completion hook's §13
// consumer test. A run with a completion hook targeting a REAL in-proc
// fixture tool (the sink) receives the full ordered transcript INCLUDING a
// steering-injected mid-run USER_MESSAGE, with identity asserted AT THE
// RECEIVING TOOL. Real drivers on the seam (real RunLoop, the production
// dispatch.ToolExecutor, real inmem bus/state/artifacts, a deterministic
// planner, the real steering inbox). Covers both failure legs (hook tool
// error leaves the run outcome unchanged + emits run.hook_failed; a cancelled
// run still fires with outcome=cancelled under the detached ctx) and the
// N≥10 concurrent no-bleed guarantee. Runs under -race.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/deterministic"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

const (
	p150SinkTool = "run_transcript_sink"
	p150WorkTool = "p150_work"
)

// p150SinkCapture records what the receiving hook tool observed.
type p150SinkCapture struct {
	mu       sync.Mutex
	payloads map[string]steering.RunCompletionPayload // keyed by run id
	identity map[string]identity.Quadruple            // ctx identity AT the sink, keyed by run id
	failFor  map[string]bool                          // run ids whose sink Invoke should error
}

func newP150SinkCapture() *p150SinkCapture {
	return &p150SinkCapture{
		payloads: map[string]steering.RunCompletionPayload{},
		identity: map[string]identity.Quadruple{},
		failFor:  map[string]bool{},
	}
}

func (s *p150SinkCapture) get(runID string) (steering.RunCompletionPayload, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payloads[runID]
	return p, ok
}

func (s *p150SinkCapture) identityFor(runID string) identity.Quadruple {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identity[runID]
}

// p150Env is the shared real-driver harness.
type p150Env struct {
	bus      events.EventBus
	steerReg *steering.Registry
	runLoop  *steering.RunLoop
	executor steering.ToolExecutor
	sink     *p150SinkCapture
}

func newP150Env(t *testing.T) *p150Env {
	t.Helper()
	red := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 32, SubscriberBufferSize: 256,
		IdleTimeout: time.Minute, DropWindow: time.Second, ReplayBufferSize: 4096,
	}, red)
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	coord := pauseresume.New(pauseresume.WithBus(bus))
	steerReg := steering.NewRegistry()
	sink := newP150SinkCapture()

	cat := tools.NewCatalog()

	// The receiving hook tool (the sink): decodes the RunCompletionPayload
	// from its args and records it + the ctx identity AT THE RECEIVING TOOL.
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{
			Name: p150SinkTool, Description: "run-completion transcript sink (D-280 test)",
			Transport: tools.TransportInProcess, Source: "p150-test-source",
		},
		Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			var p steering.RunCompletionPayload
			if err := json.Unmarshal(args, &p); err != nil {
				return tools.ToolResult{}, fmt.Errorf("sink decode: %w", err)
			}
			q, _ := identity.QuadrupleFrom(ctx)
			sink.mu.Lock()
			sink.payloads[p.RunID] = p
			sink.identity[p.RunID] = q
			shouldFail := sink.failFor[p.RunID]
			sink.mu.Unlock()
			if shouldFail {
				return tools.ToolResult{}, errors.New("sink is deliberately down (D-280 failure leg)")
			}
			return tools.ToolResult{Value: map[string]any{"ok": true}}, nil
		},
	}); err != nil {
		t.Fatalf("register sink: %v", err)
	}

	// The mid-run "work" tool: on its FIRST invocation per run it injects a
	// steering USER_MESSAGE into the run's OWN inbox through the real
	// registry — the real steering path a Protocol edge would drive.
	injected := &sync.Map{} // run id -> struct{}
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{
			Name: p150WorkTool, Description: "no-op work tool that injects a mid-run USER_MESSAGE",
			Transport: tools.TransportInProcess, Source: "p150-test-source",
		},
		Invoke: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			q, ok := identity.QuadrupleFrom(ctx)
			if ok {
				if _, loaded := injected.LoadOrStore(q.RunID, struct{}{}); !loaded {
					if inbox, lerr := steerReg.Lookup(q); lerr == nil {
						_ = inbox.Enqueue(steering.ControlEvent{
							Type:         steering.ControlUserMessage,
							Identity:     q,
							CallerScope:  steering.ScopeSessionUser,
							CallerTenant: q.TenantID,
							Payload:      map[string]any{"message": "mid-run: also summarise the result"},
						})
					}
				}
			}
			return tools.ToolResult{Value: map[string]any{"did": "work"}}, nil
		},
	}); err != nil {
		t.Fatalf("register work: %v", err)
	}

	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}

	artStore, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = artStore.Close(context.Background()) })
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store: st, Bus: bus, Redactor: red, Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	t.Cleanup(func() { _ = taskReg.Close(context.Background()) })

	return &p150Env{
		bus:      bus,
		steerReg: steerReg,
		runLoop:  rl,
		executor: dispatch.NewToolExecutor(cat, artStore, taskReg),
		sink:     sink,
	}
}

// p150WorkThenFinishPlanner: step 0 → CallTool the work tool (which injects a
// mid-run USER_MESSAGE); once the trajectory carries that step, Finish goal.
func p150WorkThenFinishPlanner(t *testing.T) planner.Planner {
	t.Helper()
	p, err := deterministic.NewDeterministicPlanner(deterministic.WithSteps(
		&deterministic.CallToolStep{
			Tool: p150WorkTool,
			ArgsBuilder: func(planner.RunContext) (json.RawMessage, error) {
				return json.RawMessage(`{"do":"work"}`), nil
			},
			When: func(rc planner.RunContext) bool {
				return rc.Trajectory == nil || len(rc.Trajectory.Steps) == 0
			},
		},
		&deterministic.FinishStep{
			Reason:         planner.FinishGoal,
			PayloadBuilder: func(planner.RunContext) (any, error) { return "the final answer", nil },
		},
	))
	if err != nil {
		t.Fatalf("deterministic planner: %v", err)
	}
	return p
}

func p150Quad(i int) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  fmt.Sprintf("tenant-%d", i),
			UserID:    fmt.Sprintf("user-%d", i),
			SessionID: fmt.Sprintf("session-%d", i),
		},
		RunID: fmt.Sprintf("run-%d", i),
	}
}

// TestE2E_Phase150_HookReceivesOrderedTranscript is the §13 consumer test.
func TestE2E_Phase150_HookReceivesOrderedTranscript(t *testing.T) {
	env := newP150Env(t)
	q := p150Quad(1)
	q.RunID = "run-transcript"

	spec := steering.RunSpec{
		Planner:        p150WorkThenFinishPlanner(t),
		Base:           planner.RunContext{Quadruple: q, Goal: "summarise the input", Trajectory: &planner.Trajectory{Query: "summarise the input"}},
		MaxSteps:       8,
		ToolExecutor:   env.executor,
		CompletionHook: &steering.CompletionHookSpec{Tool: p150SinkTool, AgentID: "agent-e2e", Timeout: 5 * time.Second},
	}

	fin, err := env.runLoop.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Fatalf("Finish.Reason = %q, want goal", fin.Reason)
	}

	payload, ok := env.sink.get(q.RunID)
	if !ok {
		t.Fatal("the sink tool did not receive the transcript")
	}
	// Identity asserted AT THE RECEIVING TOOL.
	seen := env.sink.identityFor(q.RunID)
	if seen != q {
		t.Errorf("sink ctx identity = %+v, want %+v", seen, q)
	}
	if payload.Outcome != "goal" || payload.AgentID != "agent-e2e" {
		t.Errorf("payload outcome/agent = %q/%q, want goal/agent-e2e", payload.Outcome, payload.AgentID)
	}
	if payload.ToolInvocations != 1 {
		t.Errorf("payload ToolInvocations = %d, want 1 (the work tool only; the hook is not counted)", payload.ToolInvocations)
	}
	// The ordered transcript must contain the initial goal, the tool line, the
	// mid-run steering USER_MESSAGE, and the final answer — in order.
	kinds := transcriptKinds(payload)
	if !containsInOrder(kinds, []string{"goal", "tool", "user_message", "final_answer"}) {
		t.Fatalf("transcript kinds = %v, want [goal ... tool ... user_message ... final_answer] in order", kinds)
	}
	var sawInjected bool
	for _, e := range payload.Conversation {
		if e.Kind == "user_message" && e.Content == "mid-run: also summarise the result" {
			sawInjected = true
		}
	}
	if !sawInjected {
		t.Errorf("the steering-injected mid-run USER_MESSAGE is not in the transcript: %+v", payload.Conversation)
	}

	// A healthy hook emits run.hook_dispatched (checked via a fresh replay).
	if !busSawHookEvent(t, env.bus, q, steering.EventTypeRunHookDispatched) {
		t.Error("expected run.hook_dispatched on the bus")
	}
}

// TestE2E_Phase150_HookErrorLeavesOutcomeUnchanged: the sink tool errors; the
// run outcome is unchanged and run.hook_failed lands on the bus.
func TestE2E_Phase150_HookErrorLeavesOutcomeUnchanged(t *testing.T) {
	env := newP150Env(t)
	q := p150Quad(2)
	q.RunID = "run-hook-err"
	env.sink.mu.Lock()
	env.sink.failFor[q.RunID] = true
	env.sink.mu.Unlock()

	spec := steering.RunSpec{
		Planner:        p150WorkThenFinishPlanner(t),
		Base:           planner.RunContext{Quadruple: q, Goal: "do the thing", Trajectory: &planner.Trajectory{Query: "do the thing"}},
		MaxSteps:       8,
		ToolExecutor:   env.executor,
		CompletionHook: &steering.CompletionHookSpec{Tool: p150SinkTool, Timeout: 5 * time.Second},
	}

	fin, err := env.runLoop.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run err = %v, want nil (a hook failure must not fail the run)", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Finish.Reason = %q, want goal (hook failure must not alter the outcome)", fin.Reason)
	}
	if !busSawHookEvent(t, env.bus, q, steering.EventTypeRunHookFailed) {
		t.Error("expected run.hook_failed on the bus")
	}
}

// TestE2E_Phase150_CancelledRunFiresWithCancelledOutcome: a cancelled run
// still fires the hook (under the detached bounded ctx) with outcome=cancelled.
func TestE2E_Phase150_CancelledRunFiresWithCancelledOutcome(t *testing.T) {
	env := newP150Env(t)
	q := p150Quad(3)
	q.RunID = "run-cancelled"

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the run ctx is dead before the first boundary check

	spec := steering.RunSpec{
		Planner:        p150WorkThenFinishPlanner(t),
		Base:           planner.RunContext{Quadruple: q, Goal: "will be cancelled", Trajectory: &planner.Trajectory{Query: "will be cancelled"}},
		MaxSteps:       8,
		ToolExecutor:   env.executor,
		CompletionHook: &steering.CompletionHookSpec{Tool: p150SinkTool, Timeout: 5 * time.Second},
	}

	_, err := env.runLoop.Run(ctx, spec)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want a context.Canceled-wrapped error", err)
	}
	payload, ok := env.sink.get(q.RunID)
	if !ok {
		t.Fatal("the hook did not fire for a cancelled run (the detached ctx must survive cancellation)")
	}
	if payload.Outcome != "cancelled" {
		t.Errorf("payload outcome = %q, want cancelled", payload.Outcome)
	}
	// Identity still flows through WithoutCancel.
	seen := env.sink.identityFor(q.RunID)
	if seen != q {
		t.Errorf("sink ctx identity = %+v, want %+v (WithoutCancel must preserve identity)", seen, q)
	}
}

// TestE2E_Phase150_ConcurrentNoBleed: N≥10 concurrent hooked runs against ONE
// shared RunLoop + executor + sink under -race — no cross-run transcript bleed.
func TestE2E_Phase150_ConcurrentNoBleed(t *testing.T) {
	env := newP150Env(t)
	const n = 16
	var wg sync.WaitGroup
	var failures atomic.Int64
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			q := p150Quad(1000 + i)
			goal := fmt.Sprintf("sentinel-goal-%d", i)
			spec := steering.RunSpec{
				Planner:        p150WorkThenFinishPlanner(t),
				Base:           planner.RunContext{Quadruple: q, Goal: goal, Trajectory: &planner.Trajectory{Query: goal}},
				MaxSteps:       8,
				ToolExecutor:   env.executor,
				CompletionHook: &steering.CompletionHookSpec{Tool: p150SinkTool, Timeout: 5 * time.Second},
			}
			if _, err := env.runLoop.Run(context.Background(), spec); err != nil {
				t.Errorf("run %d: %v", i, err)
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if failures.Load() > 0 {
		t.Fatalf("%d concurrent runs failed", failures.Load())
	}
	for i := range n {
		q := p150Quad(1000 + i)
		payload, ok := env.sink.get(q.RunID)
		if !ok {
			t.Errorf("run %s: no transcript at the sink", q.RunID)
			continue
		}
		wantGoal := fmt.Sprintf("sentinel-goal-%d", i)
		var gotGoal string
		for _, e := range payload.Conversation {
			if e.Kind == "goal" {
				gotGoal = e.Content
			}
		}
		if gotGoal != wantGoal {
			t.Errorf("run %s: transcript goal = %q, want %q (cross-run bleed)", q.RunID, gotGoal, wantGoal)
		}
		if payload.RunID != q.RunID {
			t.Errorf("payload RunID %q != %q", payload.RunID, q.RunID)
		}
	}
}

// --- helpers ---

func transcriptKinds(p steering.RunCompletionPayload) []string {
	out := make([]string, 0, len(p.Conversation))
	for _, e := range p.Conversation {
		out = append(out, e.Kind)
	}
	return out
}

func containsInOrder(have, want []string) bool {
	i := 0
	for _, h := range have {
		if i < len(want) && h == want[i] {
			i++
		}
	}
	return i == len(want)
}

// busSawHookEvent subscribes to the run's identity-scoped stream and replays
// the retained events, reporting whether a hook event of the given type was
// emitted for the run.
func busSawHookEvent(t *testing.T, bus events.EventBus, q identity.Quadruple, want events.EventType) bool {
	t.Helper()
	replayer, ok := bus.(events.Replayer)
	if !ok {
		t.Fatal("inmem bus is not a Replayer")
	}
	evs, err := replayer.Replay(context.Background(), events.Cursor{SessionID: q.SessionID}, events.Filter{
		Tenant: q.TenantID, User: q.UserID, Session: q.SessionID, Run: q.RunID,
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	for _, ev := range evs {
		if ev.Type == want {
			return true
		}
	}
	return false
}
