// Phase 47 integration test — wires the new ReAct emission paths
// end-to-end with the runtime parallel executor + real TaskRegistry.
// Proves four things in one PR (RFC §6.2 + master plan Phase 47 +
// D-056 + §13 primitive-with-consumer rule):
//
//  1. ReAct emits `_spawn_task` → runtime spawns a real
//     `tasks.SpawnRequest` into the TaskRegistry; group resolves
//     when the spawned task transitions to Complete; the planner
//     re-enters Next with the resolved MemberOutcome surfaced
//     through `RunContext.Trajectory.Background`.
//  2. ReAct emits `_await_task` → the planner produces a typed
//     planner.AwaitTask Decision carrying the task ID.
//  3. ReAct emits `CallParallel` (multi-action salvage) → the
//     runtime parallel executor consumes the shape, atomic setup
//     validation runs against the real ToolCatalog descriptors,
//     branches dispatch concurrently, JoinAll returns N results in
//     branch-index order.
//  4. The parallel executor's atomic-setup-validation contract
//     holds against a real catalog: any one branch's invalid args
//     fails the whole call BEFORE any branch executes.
//
// The test exercises the §13 "primitive must ship with consumer"
// rule for all three Phase 47 primitives in one wave:
//
//   - CallParallel executor (new at Phase 47) ← consumer: ReAct
//     emission + repair-loop multi-action.
//   - SpawnTask emission (Phase 42 shape, Phase 47 consumer) ← consumer:
//     runtime spawns a real task.
//   - AwaitTask emission (Phase 42 shape, Phase 47 consumer) ← consumer:
//     the planner's typed Decision.
package integration_test

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
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns47 "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/parallel"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	"github.com/hurtener/Harbor/internal/tools"
)

// phase47Deps bundles the subsystems the Phase 47 integration tests
// wire up. Mirrors the wave6 helper shape so the conformance is
// uniform across waves.
type phase47Deps struct {
	store    state.StateStore
	bus      events.EventBus
	artStore artifacts.ArtifactStore
	reg      tasks.TaskRegistry
}

func openPhase47(t *testing.T) (*phase47Deps, func()) {
	t.Helper()
	cfg := &config.Config{
		State: config.StateConfig{Driver: "inmem"},
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     64,
			IdleTimeout:              5 * time.Minute,
			DropWindow:               time.Second,
			ReplayBufferSize:         1024,
		},
		Artifacts: config.ArtifactsConfig{Driver: "inmem"},
		Tasks:     config.TasksConfig{Driver: "inprocess"},
	}
	red := auditpatterns47.New()
	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		_ = store.Close(context.Background())
		t.Fatalf("events.Open: %v", err)
	}
	artStore, err := artifacts.Open(context.Background(), cfg.Artifacts)
	if err != nil {
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
		t.Fatalf("artifacts.Open: %v", err)
	}
	reg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: red,
		Cfg:      cfg.Tasks,
	})
	if err != nil {
		_ = artStore.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}
	closed := false
	cleanup := func() {
		if closed {
			return
		}
		closed = true
		_ = reg.Close(context.Background())
		_ = artStore.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
	}
	return &phase47Deps{
		store:    store,
		bus:      bus,
		artStore: artStore,
		reg:      reg,
	}, cleanup
}

// scriptedLLM is a tiny llm.LLMClient that emits a scripted sequence
// of CompleteResponse contents. Re-used across the Phase 47 tests.
type scriptedLLM struct {
	mu       sync.Mutex
	contents []string
	cursor   int
	seen     []string // raw user-prompts (best-effort, last-message-content)
}

func (s *scriptedLLM) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(req.Messages) > 0 {
		last := req.Messages[len(req.Messages)-1]
		if last.Content.Text != nil {
			s.seen = append(s.seen, *last.Content.Text)
		}
	}
	if s.cursor >= len(s.contents) {
		return llm.CompleteResponse{Content: s.contents[len(s.contents)-1]}, nil
	}
	out := s.contents[s.cursor]
	s.cursor++
	return llm.CompleteResponse{Content: out}, nil
}

func (s *scriptedLLM) Close(_ context.Context) error { return nil }

// fixedQ47 builds a populated identity quadruple for the Phase 47
// integration tests.
func fixedQ47(_ *testing.T, runID string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "T47", UserID: "U47", SessionID: "S47"},
		RunID:    runID,
	}
}

// TestE2E_Phase47_ReactSpawnTaskWakeRoundTrip is the load-bearing
// round-trip: ReAct emits `_spawn_task` → runtime spawns the real
// task into the registry, opens a `tasks.WatchGroup` on the resulting
// group, transitions the spawned task to Complete, observes the
// `GroupCompletion` payload, populates RunContext.Trajectory.Background
// with the resolved MemberOutcome, re-invokes Next. The planner
// emits `_finish` once it sees the resolved background.
//
// This proves the §13 primitive-with-consumer rule for the
// SpawnTask emission shape (Phase 42 ship without consumer; Phase 47
// closes the gap).
func TestE2E_Phase47_ReactSpawnTaskWakeRoundTrip(t *testing.T) {
	deps, cleanup := openPhase47(t)
	defer cleanup()

	q := fixedQ47(t, "r-spawn-wake")
	ctx, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	// Step 1 LLM response: emit `_spawn_task`.
	// Step 2 LLM response (after group resolves): emit `_finish`.
	client := &scriptedLLM{contents: []string{
		`{"tool":"_spawn_task","args":{"kind":"background","spec":{"description":"summarise document X","query":"summarise X","priority":0,"retain_turn":false}},"reasoning":"need a side channel"}`,
		`{"tool":"_finish","args":{"answer":"all done, saw background result"},"reasoning":"wake observed"}`,
	}}
	p := react.New(client)

	// First step — expect a SpawnTask Decision.
	traj := &planner.Trajectory{}
	rc := planner.RunContext{
		Quadruple:  q,
		Goal:       "summarise X via background spawn",
		Trajectory: traj,
	}
	dec, err := p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next #1: %v", err)
	}
	spawn, ok := dec.(planner.SpawnTask)
	if !ok {
		t.Fatalf("Next #1 returned %T, want planner.SpawnTask", dec)
	}
	if spawn.Spec.RetainTurn {
		t.Errorf("Spec.RetainTurn = true, want false (non-retain-turn drives push wake)")
	}

	// Runtime side — spawn the real task in a fresh group; WatchGroup
	// before transitioning to Complete. The runtime executor that
	// lives in a later phase will do this in production; here we
	// inline the wiring so the integration test can assert the
	// end-to-end round-trip.
	group, err := deps.reg.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID:   q.Identity,
		Description: "phase 47 spawn-wake test",
	})
	if err != nil {
		t.Fatalf("ResolveOrCreateGroup: %v", err)
	}
	handle, err := deps.reg.Spawn(ctx, tasks.SpawnRequest{
		Identity:    q,
		Kind:        spawn.Kind,
		Description: spawn.Spec.Description,
		Query:       spawn.Spec.Query,
		Priority:    spawn.Spec.Priority,
		GroupID:     group.ID,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Seal after membership is set; sealed groups resolve when the
	// last member transitions to terminal.
	if err := deps.reg.SealGroup(ctx, group.ID); err != nil {
		t.Fatalf("SealGroup: %v", err)
	}
	completionCh, cancelWatch, err := deps.reg.WatchGroup(q.Identity, group.ID)
	if err != nil {
		t.Fatalf("WatchGroup: %v", err)
	}
	defer cancelWatch()

	// Drive the task to Complete (runtime executor would do this when
	// the spawned tool finishes; the test drives it explicitly).
	if err := deps.reg.MarkRunning(ctx, handle.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	resultBytes := json.RawMessage(`{"summary":"document X summarised offline"}`)
	if err := deps.reg.MarkComplete(ctx, handle.ID, tasks.TaskResult{Value: resultBytes}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	// Wait for the WatchGroup payload — the production runtime engine
	// uses the same wait loop in the planner step adapter at Phase 60+.
	var completion tasks.GroupCompletion
	select {
	case completion = <-completionCh:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchGroup did not deliver GroupCompletion within 2s")
	}
	if completion.FinalStatus != tasks.GroupCompleted {
		t.Errorf("FinalStatus = %q, want %q", completion.FinalStatus, tasks.GroupCompleted)
	}
	if len(completion.Members) != 1 {
		t.Fatalf("len(Members) = %d, want 1", len(completion.Members))
	}

	// Surface the MemberOutcome through RunContext.Trajectory.Background
	// — the planner reads this on the next step (the production runtime
	// engine wires the same mapping at Phase 60+ inside its planner
	// step adapter).
	bg := trajectory.BackgroundResult{
		GroupID:    string(group.ID),
		Status:     string(completion.FinalStatus),
		ResolvedAt: completion.ResolvedAt,
		Members: []trajectory.BackgroundMemberOutcome{
			{
				TaskID: string(completion.Members[0].TaskID),
				Status: string(completion.Members[0].Status),
			},
		},
	}
	traj.Background = map[string]trajectory.BackgroundResult{
		string(group.ID): bg,
	}

	// Second step — expect a Finish Decision.
	dec2, err := p.Next(ctx, rc)
	if err != nil {
		t.Fatalf("Next #2: %v", err)
	}
	fin, ok := dec2.(planner.Finish)
	if !ok {
		t.Fatalf("Next #2 returned %T, want planner.Finish", dec2)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishGoal)
	}
}

// TestE2E_Phase47_ReactAwaitTaskEmits asserts the planner emits
// AwaitTask when the LLM uses `_await_task`. The runtime-side
// consumption (block the turn on the named task) is the planner-step
// adapter's job (Phase 60+); the Phase 47 contract is the emission.
func TestE2E_Phase47_ReactAwaitTaskEmits(t *testing.T) {
	deps, cleanup := openPhase47(t)
	defer cleanup()
	_ = deps

	q := fixedQ47(t, "r-await")
	ctx, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	client := &scriptedLLM{contents: []string{
		`{"tool":"_await_task","args":{"task_id":"task-77"},"reasoning":"block until done"}`,
	}}
	p := react.New(client)
	dec, err := p.Next(ctx, planner.RunContext{Quadruple: q, Goal: "block on task-77"})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	aw, ok := dec.(planner.AwaitTask)
	if !ok {
		t.Fatalf("decision = %T, want planner.AwaitTask", dec)
	}
	if string(aw.TaskID) != "task-77" {
		t.Errorf("TaskID = %q, want %q", aw.TaskID, "task-77")
	}
}

// TestE2E_Phase47_ReactCallParallelEndToEnd is the load-bearing
// CallParallel acceptance test: the LLM emits a 3-way multi-action
// JSON array → Phase 44 repair loop produces CallParallel → ReAct
// passes through → the runtime parallel executor dispatches via the
// real ToolCatalog → JoinAll returns 3 results in branch-index order.
//
// Proves the §13 primitive-with-consumer rule for CallParallel
// execution: the new internal/runtime/parallel package ships its
// first consumer in the same PR.
func TestE2E_Phase47_ReactCallParallelEndToEnd(t *testing.T) {
	deps, cleanup := openPhase47(t)
	defer cleanup()
	_ = deps

	q := fixedQ47(t, "r-parallel-e2e")
	ctx, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	// Build a real catalog with three echo tools (`a`, `b`, `c`). The
	// in-memory catalog ships at Phase 26; we use it as the production
	// dispatch surface.
	catalog := tools.NewCatalog()
	for _, name := range []string{"a", "b", "c"} {
		n := name
		if err := catalog.Register(tools.ToolDescriptor{
			Tool: tools.Tool{
				Name:       n,
				ArgsSchema: json.RawMessage(`{"type":"object"}`),
				Transport:  tools.TransportInProcess,
				Loading:    tools.LoadingAlways,
			},
			Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: map[string]any{"tool": n, "args": string(args)}}, nil
			},
			Validate: func(_ json.RawMessage) error { return nil },
		}); err != nil {
			t.Fatalf("catalog.Register(%q): %v", n, err)
		}
	}

	client := &scriptedLLM{contents: []string{
		`[{"tool":"a","args":{"x":1}},{"tool":"b","args":{"x":2}},{"tool":"c","args":{"x":3}}]`,
	}}
	p := react.New(client)
	dec, err := p.Next(ctx, planner.RunContext{Quadruple: q, Goal: "fan-out across a/b/c"})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	par, ok := dec.(planner.CallParallel)
	if !ok {
		t.Fatalf("decision = %T, want planner.CallParallel (Phase 47 pass-through)", dec)
	}
	if got := len(par.Branches); got != 3 {
		t.Fatalf("len(Branches) = %d, want 3", got)
	}

	// Dispatch through the real parallel executor.
	exec := parallel.New(catalog)
	results, err := exec.Execute(ctx, par)
	if err != nil {
		t.Fatalf("parallel.Execute: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	wantNames := []string{"a", "b", "c"}
	for i, r := range results {
		if r.Index != i {
			t.Errorf("results[%d].Index = %d, want %d (deterministic merge key)", i, r.Index, i)
		}
		if r.Tool != wantNames[i] {
			t.Errorf("results[%d].Tool = %q, want %q", i, r.Tool, wantNames[i])
		}
		if r.Err != nil {
			t.Errorf("results[%d].Err = %v, want nil", i, r.Err)
		}
	}
}

// TestE2E_Phase47_ParallelExecutorAtomicSetup asserts that a bad
// branch (validator rejection) fails the whole call BEFORE any
// branch dispatches against the REAL catalog (no mocks at the seam).
func TestE2E_Phase47_ParallelExecutorAtomicSetup(t *testing.T) {
	deps, cleanup := openPhase47(t)
	defer cleanup()
	_ = deps

	q := fixedQ47(t, "r-atomic-e2e")
	ctx, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	var goodInvoked atomic.Int64
	catalog := tools.NewCatalog()
	_ = catalog.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "good", Transport: tools.TransportInProcess, Loading: tools.LoadingAlways},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			goodInvoked.Add(1)
			return tools.ToolResult{}, nil
		},
		Validate: func(_ json.RawMessage) error { return nil },
	})
	_ = catalog.Register(tools.ToolDescriptor{
		Tool:     tools.Tool{Name: "picky", Transport: tools.TransportInProcess, Loading: tools.LoadingAlways},
		Invoke:   func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil },
		Validate: func(_ json.RawMessage) error { return fmt.Errorf("picky-validator-says-no") },
	})

	exec := parallel.New(catalog)
	_, err = exec.Execute(ctx, planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "good", Args: json.RawMessage(`{}`)},
			{Tool: "picky", Args: json.RawMessage(`{}`)},
		},
		Join: &planner.JoinSpec{Kind: planner.JoinAll},
	})
	if err == nil {
		t.Fatal("Execute returned nil err, want ErrParallelBranchInvalidArgs")
	}
	if !errors.Is(err, planner.ErrParallelBranchInvalidArgs) {
		t.Errorf("err = %v, want errors.Is ErrParallelBranchInvalidArgs", err)
	}
	if goodInvoked.Load() != 0 {
		t.Errorf("goodInvoked = %d, want 0 (atomic-setup must reject BEFORE any dispatch)", goodInvoked.Load())
	}
}

// TestE2E_Phase47_ParallelExecutorAbsoluteMaxParallel asserts the
// system cap against a 51-branch input.
func TestE2E_Phase47_ParallelExecutorAbsoluteMaxParallel(t *testing.T) {
	deps, cleanup := openPhase47(t)
	defer cleanup()
	_ = deps

	q := fixedQ47(t, "r-cap-e2e")
	ctx, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	catalog := tools.NewCatalog()
	_ = catalog.Register(tools.ToolDescriptor{
		Tool:     tools.Tool{Name: "any", Transport: tools.TransportInProcess, Loading: tools.LoadingAlways},
		Invoke:   func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil },
		Validate: func(_ json.RawMessage) error { return nil },
	})
	branches := make([]planner.CallTool, planner.AbsoluteMaxParallel+1)
	for i := range branches {
		branches[i] = planner.CallTool{Tool: "any", Args: json.RawMessage(`{}`)}
	}
	exec := parallel.New(catalog)
	_, err = exec.Execute(ctx, planner.CallParallel{
		Branches: branches,
		Join:     &planner.JoinSpec{Kind: planner.JoinAll},
	})
	if !errors.Is(err, planner.ErrParallelCapExceeded) {
		t.Errorf("err = %v, want errors.Is ErrParallelCapExceeded", err)
	}
}
