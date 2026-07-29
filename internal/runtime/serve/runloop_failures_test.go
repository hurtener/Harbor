package serve

// runloop_failures_test.go — fail-loud coverage for runOne's per-projection
// error branches. Each test injects a failure at ONE collaborator seam (an
// erroring memory store, an agent-config registry that errors on the Nth
// Active read, an invalid output schema) and asserts the run fails LOUD with
// the branch's terminal code + message — never a silent skip (CLAUDE.md §13).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/artifacts"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

var errInjected = errors.New("runloop failure-injection sentinel")

// failingMemoryStore wraps a real MemoryStore and errors on GetLLMContext
// (the run-start memory fetch) — every other method delegates to the real
// driver.
type failingMemoryStore struct {
	memory.MemoryStore
}

func (failingMemoryStore) GetLLMContext(context.Context, identity.Quadruple) (memory.LLMContextPatch, error) {
	return memory.LLMContextPatch{}, errInjected
}

// countingFailRegistry is an agentcfg.Registry whose Active errors on the
// FailAt-th call (1-based) and reports "no active revision" otherwise — the
// per-projection failure injector (each run-start projection reads Active
// once, so FailAt selects which projection dies).
type countingFailRegistry struct {
	calls  atomic.Int32
	failAt int32
}

func (r *countingFailRegistry) Active(context.Context, identity.Quadruple, string, agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	if r.calls.Add(1) == r.failAt {
		return agentcfg.Revision{}, false, errInjected
	}
	return agentcfg.Revision{}, false, nil
}

func (r *countingFailRegistry) SetRevision(context.Context, identity.Quadruple, string, agentcfg.ConfigScope, agentcfg.ConfigPayload) (agentcfg.Revision, error) {
	return agentcfg.Revision{}, errInjected
}

func (r *countingFailRegistry) Get(context.Context, identity.Quadruple, string, string, agentcfg.ConfigScope) (agentcfg.Revision, error) {
	return agentcfg.Revision{}, errInjected
}

func (r *countingFailRegistry) ListRevisions(context.Context, identity.Quadruple, string, agentcfg.ConfigScope, int) ([]agentcfg.Revision, error) {
	return nil, errInjected
}

func (r *countingFailRegistry) Rollback(context.Context, identity.Quadruple, string, string, agentcfg.ConfigScope) (agentcfg.Revision, error) {
	return agentcfg.Revision{}, errInjected
}

func (r *countingFailRegistry) Diff(context.Context, identity.Quadruple, string, string, string, agentcfg.ConfigScope) (agentcfg.Diff, error) {
	return agentcfg.Diff{}, errInjected
}

func (r *countingFailRegistry) Close(context.Context) error { return nil }

// failDriverEnv bundles the minimal real-driver env the failure tests share.
type failDriverEnv struct {
	bus events.EventBus
	reg tasks.TaskRegistry
	rl  *steering.RunLoop
}

func newFailDriverEnv(t *testing.T) failDriverEnv {
	t.Helper()
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	rl := newTestRunLoop(t, steerReg, bus)
	return failDriverEnv{bus: bus, reg: reg, rl: rl}
}

// startFailDriver constructs + starts a driver with the given extra options
// applied over the mandatory core.
func startFailDriver(t *testing.T, env failDriverEnv, mutate func(*RunLoopDriverOptions)) *RunLoopDriver {
	t.Helper()
	opts := RunLoopDriverOptions{
		Bus:     env.bus,
		RunLoop: env.rl,
		Planner: &driverTestPlanner{finishGoalImmediately: true},
		Tasks:   env.reg,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&opts)
	}
	d, err := NewRunLoopDriver(opts)
	if err != nil {
		t.Fatalf("NewRunLoopDriver: %v", err)
	}
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	return d
}

// spawnAndAwaitFailure spawns a task (optionally with an output schema) and
// asserts it reaches StatusFailed with the wanted code + message fragment.
func spawnAndAwaitFailure(t *testing.T, reg tasks.TaskRegistry, schema json.RawMessage, wantCode, wantMsg string) {
	t.Helper()
	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity:     identity.Quadruple{Identity: runLoopDriverTestID},
		Kind:         tasks.KindForeground,
		Query:        "failure-injection goal",
		OutputSchema: schema,
	})
	if err != nil {
		t.Fatalf("reg.Spawn: %v", err)
	}
	if status := waitForTaskStatus(t, reg, h.ID, tasks.StatusFailed, 5*time.Second); status != tasks.StatusFailed {
		t.Fatalf("task status = %q, want %q", status, tasks.StatusFailed)
	}
	got, err := reg.Get(ctx, h.ID)
	if err != nil {
		t.Fatalf("reg.Get: %v", err)
	}
	if got.Error == nil {
		t.Fatal("failed task carries no TaskError")
	}
	if wantCode != "" && got.Error.Code != wantCode {
		t.Errorf("TaskError.Code = %q, want %q (message: %s)", got.Error.Code, wantCode, got.Error.Message)
	}
	if wantMsg != "" && !strings.Contains(got.Error.Message, wantMsg) {
		t.Errorf("TaskError.Message = %q, want it to contain %q", got.Error.Message, wantMsg)
	}
}

// TestRunOne_OutputSchemaCompileError_MarksOutputInvalid — a task carrying a
// malformed output schema fails LOUD with the output_invalid terminal code;
// the planner is never called.
func TestRunOne_OutputSchemaCompileError_MarksOutputInvalid(t *testing.T) {
	env := newFailDriverEnv(t)
	startFailDriver(t, env, nil)
	spawnAndAwaitFailure(t, env.reg,
		json.RawMessage(`{"type": 42}`), // type must be a string — compile fails
		planner.TaskErrorCodeOutputInvalid, "output-schema compile failed")
}

// TestRunOne_MemoryFetchError_MarksRuntimeFetchError — a memory store whose
// GetLLMContext errors fails the run LOUD (runtime_fetch_error), never a
// silent no-memory degradation.
func TestRunOne_MemoryFetchError_MarksRuntimeFetchError(t *testing.T) {
	env := newFailDriverEnv(t)
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	realMem, err := memoryOpen(t, env.bus, st)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.Memory = failingMemoryStore{MemoryStore: realMem}
	})
	spawnAndAwaitFailure(t, env.reg, nil, "runtime_fetch_error", "FetchMemoryBlocks")
}

// TestRunOne_SkillsProjectionError_FailsRun — an agent-config registry that
// errors on the skills-projection read fails the run LOUD.
func TestRunOne_SkillsProjectionError_FailsRun(t *testing.T) {
	env := newFailDriverEnv(t)
	// A real (empty) skills directory so the skills block runs; the failing
	// registry dies on its FIRST Active read (the skills projection).
	skillStore, err := skills.Open(context.Background(), skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"}, skills.Deps{Bus: env.bus})
	if err != nil {
		t.Fatalf("skills.Open: %v", err)
	}
	skillsDir, err := skills.NewDirectory(skillStore, skills.Deps{Bus: env.bus},
		skills.DirectoryFromConfig(config.SkillsConfig{}, 5))
	if err != nil {
		t.Fatalf("skills.NewDirectory: %v", err)
	}
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.SkillsDirectory = skillsDir
		o.AgentConfig = &countingFailRegistry{failAt: 1}
		o.AgentConfigID = "fail-agent"
	})
	spawnAndAwaitFailure(t, env.reg, nil, "runtime_fetch_error", "agent-config skills projection")
}

// TestRunOne_CatalogProjectionError_FailsRun — an agent-config registry that
// errors on the tool-exposure read fails the run LOUD.
func TestRunOne_CatalogProjectionError_FailsRun(t *testing.T) {
	env := newFailDriverEnv(t)
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.Catalog = tools.NewCatalog()
		o.AgentConfig = &countingFailRegistry{failAt: 1}
		o.AgentConfigID = "fail-agent"
	})
	spawnAndAwaitFailure(t, env.reg, nil, "runtime_fetch_error", "agent-config tool-exposure projection")
}

// TestRunOne_PromptLayersProjectionError_FailsRun — the prompt-layer read
// (the second Active read on the no-skills/no-catalog path, after the
// LLM-overrides read) fails the run LOUD.
func TestRunOne_PromptLayersProjectionError_FailsRun(t *testing.T) {
	env := newFailDriverEnv(t)
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.AgentConfig = &countingFailRegistry{failAt: 2}
		o.AgentConfigID = "fail-agent"
	})
	spawnAndAwaitFailure(t, env.reg, nil, planner.TaskErrorCodeRunLoopError, "prompt-layer projection failed")
}

// TestRunOne_HookProjectionError_FailsRun — the run-completion-hook read (the
// fourth Active read: LLM overrides, then the prompt-layer projection's agent
// + user scopes, then the hook) fails the run LOUD.
func TestRunOne_HookProjectionError_FailsRun(t *testing.T) {
	env := newFailDriverEnv(t)
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.AgentConfig = &countingFailRegistry{failAt: 4}
		o.AgentConfigID = "fail-agent"
	})
	spawnAndAwaitFailure(t, env.reg, nil, planner.TaskErrorCodeRunLoopError, "run-completion-hook projection failed")
}

// TestRunOne_NamingProjectionError_FailsRun — the naming-policy read (the
// fifth Active read, reached only when the naming deps are wired) fails the
// run LOUD.
func TestRunOne_NamingProjectionError_FailsRun(t *testing.T) {
	env := newFailDriverEnv(t)
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	sessReg, err := sessions.New(st, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: 15 * time.Minute,
	}, env.bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = sessReg.CloseRegistry(context.Background()) })
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.AgentConfig = &countingFailRegistry{failAt: 5}
		o.AgentConfigID = "fail-agent"
		o.SessionTitler = sessReg
		o.NamingLLM = namingCompleterFunc(func(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
			return llm.CompleteResponse{}, nil
		})
	})
	spawnAndAwaitFailure(t, env.reg, nil, planner.TaskErrorCodeRunLoopError, "naming-policy projection failed")
}

// TestRunOne_FinishNoPath_MarksFailed — a Finish with a non-goal terminal
// reason (NoPath) transitions the task to Failed (the FSM has no
// "no-path-but-not-failed" status).
func TestRunOne_FinishNoPath_MarksFailed(t *testing.T) {
	env := newFailDriverEnv(t)
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.Planner = &noPathPlanner{}
	})
	spawnAndAwaitFailure(t, env.reg, nil, "", "")
}

// noPathPlanner immediately finishes with FinishNoPath.
type noPathPlanner struct{}

func (p *noPathPlanner) Next(_ context.Context, _ planner.RunContext) (planner.Decision, error) {
	return planner.Finish{Reason: planner.FinishNoPath}, nil
}

// TestRunOne_MarkRunningFails_SkipsRun — runOne against a task the registry
// does not know bails at MarkRunning without running the planner.
func TestRunOne_MarkRunningFails_SkipsRun(t *testing.T) {
	env := newFailDriverEnv(t)
	d := startFailDriver(t, env, nil)
	q := identity.Quadruple{Identity: runLoopDriverTestID, RunID: "ghost"}
	// Direct call: the task does not exist, MarkRunning fails, runOne bails.
	d.runOne(q, tasks.TaskID("ghost-task-never-spawned"))
}

// TestRunLoopDriver_Close_BeforeStart_NoOp — Close on a never-started driver
// is a clean no-op.
func TestRunLoopDriver_Close_BeforeStart_NoOp(t *testing.T) {
	d, err := NewRunLoopDriver(RunLoopDriverOptions{
		Bus:     mkDriverTestBus(t, auditpatterns.New()),
		RunLoop: newTestRunLoop(t, steering.NewRegistry(), mkDriverTestBus(t, auditpatterns.New())),
		Planner: &driverTestPlanner{},
		Tasks:   mkDriverTestTaskRegistry(t, mkDriverTestBus(t, auditpatterns.New()), auditpatterns.New()),
	})
	if err != nil {
		t.Fatalf("NewRunLoopDriver: %v", err)
	}
	if cErr := d.Close(context.Background()); cErr != nil {
		t.Fatalf("Close before Start: %v", cErr)
	}
}

// failSubscribeBus wraps a real bus and errors on Subscribe — the driver
// Start failure branch.
type failSubscribeBus struct{ events.EventBus }

func (failSubscribeBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, errInjected
}

// TestRunLoopDriver_Start_SubscribeError_FailsLoud — a bus whose Subscribe
// errors fails Start loud (and cancels the sub ctx).
func TestRunLoopDriver_Start_SubscribeError_FailsLoud(t *testing.T) {
	env := newFailDriverEnv(t)
	d, err := NewRunLoopDriver(RunLoopDriverOptions{
		Bus:     failSubscribeBus{EventBus: env.bus},
		RunLoop: env.rl,
		Planner: &driverTestPlanner{},
		Tasks:   env.reg,
	})
	if err != nil {
		t.Fatalf("NewRunLoopDriver: %v", err)
	}
	if sErr := d.Start(context.Background()); !errors.Is(sErr, errInjected) {
		t.Fatalf("Start with a failing Subscribe = %v, want the injected error", sErr)
	}
}

// failingTaskRegistry wraps a real registry with per-method error toggles —
// the post-MarkRunning failure branches' injector.
type failingTaskRegistry struct {
	tasks.TaskRegistry
	failGet          bool
	failMarkFailed   bool
	failMarkComplete bool
}

func (r failingTaskRegistry) Get(ctx context.Context, id tasks.TaskID) (*tasks.Task, error) {
	if r.failGet {
		return nil, errInjected
	}
	return r.TaskRegistry.Get(ctx, id)
}

func (r failingTaskRegistry) MarkFailed(ctx context.Context, id tasks.TaskID, te tasks.TaskError) error {
	if r.failMarkFailed {
		return errInjected
	}
	return r.TaskRegistry.MarkFailed(ctx, id, te)
}

func (r failingTaskRegistry) MarkComplete(ctx context.Context, id tasks.TaskID, res tasks.TaskResult) error {
	if r.failMarkComplete {
		return errInjected
	}
	return r.TaskRegistry.MarkComplete(ctx, id, res)
}

// spawnOn spawns a foreground task on the given registry and returns its id.
func spawnOn(t *testing.T, reg tasks.TaskRegistry, schema json.RawMessage) tasks.TaskID {
	t.Helper()
	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity:     identity.Quadruple{Identity: runLoopDriverTestID},
		Kind:         tasks.KindForeground,
		Query:        "wrapper-registry goal",
		OutputSchema: schema,
	})
	if err != nil {
		t.Fatalf("reg.Spawn: %v", err)
	}
	return h.ID
}

// TestRunOne_TaskGetError_MarksRuntimeFetchError — tasks.Get failing after
// MarkRunning fails the run loud with runtime_fetch_error.
func TestRunOne_TaskGetError_MarksRuntimeFetchError(t *testing.T) {
	env := newFailDriverEnv(t)
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.Tasks = failingTaskRegistry{TaskRegistry: env.reg, failGet: true}
	})
	id := spawnOn(t, env.reg, nil)
	if status := waitForTaskStatus(t, env.reg, id, tasks.StatusFailed, 5*time.Second); status != tasks.StatusFailed {
		t.Fatalf("task status = %q, want failed (Get error path)", status)
	}
}

// TestRunOne_MarkFailedAlsoFails_WarnsAndContinues — the inner
// MarkFailed-failed warns (schema-compile + memory-fetch + non-goal finish
// variants): the driver logs loud and keeps serving, never panics.
func TestRunOne_MarkFailedAlsoFails_WarnsAndContinues(t *testing.T) {
	t.Run("schema_compile", func(t *testing.T) {
		env := newFailDriverEnv(t)
		startFailDriver(t, env, func(o *RunLoopDriverOptions) {
			o.Tasks = failingTaskRegistry{TaskRegistry: env.reg, failMarkFailed: true}
		})
		id := spawnOn(t, env.reg, json.RawMessage(`{"type": 42}`))
		// MarkFailed is suppressed, so the task stays Running; the assertion
		// is the driver survived (no panic) and the task never completed.
		if status := waitForTaskStatus(t, env.reg, id, tasks.StatusRunning, 3*time.Second); status == tasks.StatusComplete {
			t.Fatalf("schema-compile failure must never complete the task, got %q", status)
		}
	})
	t.Run("non_goal_finish", func(t *testing.T) {
		env := newFailDriverEnv(t)
		startFailDriver(t, env, func(o *RunLoopDriverOptions) {
			o.Planner = &noPathPlanner{}
			o.Tasks = failingTaskRegistry{TaskRegistry: env.reg, failMarkFailed: true}
		})
		id := spawnOn(t, env.reg, nil)
		if status := waitForTaskStatus(t, env.reg, id, tasks.StatusRunning, 3*time.Second); status == tasks.StatusComplete {
			t.Fatalf("non-goal finish must never complete the task, got %q", status)
		}
	})
}

// TestRunOne_MarkCompleteError_WarnsLoud — MarkComplete failing after a
// successful run logs loud (the run result is lost to the FSM but the driver
// survives).
func TestRunOne_MarkCompleteError_WarnsLoud(t *testing.T) {
	env := newFailDriverEnv(t)
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.Tasks = failingTaskRegistry{TaskRegistry: env.reg, failMarkComplete: true}
	})
	id := spawnOn(t, env.reg, nil)
	if status := waitForTaskStatus(t, env.reg, id, tasks.StatusRunning, 3*time.Second); status == tasks.StatusComplete {
		t.Fatalf("MarkComplete was injected to fail; task must not read complete, got %q", status)
	}
}

// TestRunOne_SchemaRetryExhausted_MarksOutputInvalid — a schema-constrained
// run whose Run error wraps the LLM retry-exhausted sentinel fails with the
// output_invalid terminal code (never a schemaless success).
func TestRunOne_SchemaRetryExhausted_MarksOutputInvalid(t *testing.T) {
	env := newFailDriverEnv(t)
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.Planner = &driverTestPlanner{errOnNext: fmt.Errorf("scripted: %w", llm.ErrRetryExhausted)}
	})
	spawnAndAwaitFailure(t, env.reg,
		json.RawMessage(`{"type":"object"}`),
		planner.TaskErrorCodeOutputInvalid, "")
}

// TestRunOne_TerminalSchemaValidationFails_MarksOutputInvalid — a run that
// finishes GOAL but whose payload violates the compiled schema fails loud
// with output_invalid (the terminal-validation half).
func TestRunOne_TerminalSchemaValidationFails_MarksOutputInvalid(t *testing.T) {
	env := newFailDriverEnv(t)
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.Planner = &driverTestPlanner{finishGoalImmediately: true, finishPayload: map[string]any{"answer": "x"}}
	})
	spawnAndAwaitFailure(t, env.reg,
		json.RawMessage(`{"type":"object","required":["must_have_field"]}`),
		planner.TaskErrorCodeOutputInvalid, "terminal output failed schema validation")
}

// failingAddTurnStore wraps a real MemoryStore and errors on AddTurn only —
// the best-effort memory-writeback warn (the run still completes).
type failingAddTurnStore struct{ memory.MemoryStore }

func (failingAddTurnStore) AddTurn(context.Context, identity.Quadruple, memory.ConversationTurn) error {
	return errInjected
}

// TestRunOne_MemoryWritebackError_RunStillCompletes — an AddTurn failure is
// best-effort: logged loud, run still Complete.
func TestRunOne_MemoryWritebackError_RunStillCompletes(t *testing.T) {
	env := newFailDriverEnv(t)
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	realMem, err := memoryOpen(t, env.bus, st)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.Memory = failingAddTurnStore{MemoryStore: realMem}
	})
	id := spawnOn(t, env.reg, nil)
	if status := waitForTaskStatus(t, env.reg, id, tasks.StatusComplete, 5*time.Second); status != tasks.StatusComplete {
		t.Fatalf("AddTurn failure must not downgrade the run, got %q", status)
	}
}

// staleSourceDetacher enumerates one stale attached source; Detach records it.
type staleSourceDetacher struct{ detached atomic.Int32 }

func (d *staleSourceDetacher) AttachedSources(context.Context, toolauth.Owner) []string {
	return []string{"stale"}
}
func (d *staleSourceDetacher) Detach(context.Context, string, toolauth.Owner) error {
	d.detached.Add(1)
	return nil
}

// TestRunLoopDriver_Reconcile_DetachesStaleAndLogsErrors — the reconcile's
// detach leg fires for a source no active revision declares, and a registry
// read error is logged loud without failing the run.
func TestRunLoopDriver_Reconcile_DetachesStaleAndLogsErrors(t *testing.T) {
	q := identity.Quadruple{Identity: runLoopDriverTestID, RunID: "r"}
	ctx, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// Detach leg: one stale source, nothing declared (a registry with no
	// active revision) → detached once. Reconcile requires a non-nil registry
	// + agent id; the never-failing counting fake reports "no revision".
	det := &staleSourceDetacher{}
	d := &RunLoopDriver{
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		connectionDetacher: det,
		agentConfig:        &countingFailRegistry{failAt: 0}, // 0 = never fails
		agentConfigID:      "reconcile-agent",
	}
	d.reconcileConnections(ctx, d.agentConfigID, q)
	if det.detached.Load() != 1 {
		t.Fatalf("stale source detached %d times, want 1", det.detached.Load())
	}

	// Error leg: the registry read errors → logged loud, no panic.
	dErr := &RunLoopDriver{
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		connectionDetacher: det,
		agentConfig:        &countingFailRegistry{failAt: 1},
		agentConfigID:      "fail-agent",
	}
	dErr.reconcileConnections(ctx, dErr.agentConfigID, q)
}

// TestRunOne_InvalidIdentity_BailsBeforeRun — a quadruple whose identity is
// incomplete fails identity.With before the run starts.
func TestRunOne_InvalidIdentity_BailsBeforeRun(t *testing.T) {
	env := newFailDriverEnv(t)
	d := startFailDriver(t, env, nil)
	d.runOne(identity.Quadruple{Identity: identity.Identity{TenantID: "t"}}, tasks.TaskID("x"))
}

// TestRunLoopDriver_ProjectNaming_ModelFallback — the naming model falls back
// to the run's effective model override when the policy names none.
func TestRunLoopDriver_ProjectNaming_ModelFallback(t *testing.T) {
	env := newFailDriverEnv(t)
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	sessReg, err := sessions.New(st, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: 15 * time.Minute,
	}, env.bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = sessReg.CloseRegistry(context.Background()) })

	q := identity.Quadruple{Identity: runLoopDriverTestID, RunID: "r"}
	ctx, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	d := &RunLoopDriver{
		namingDefault: config.RuntimeNamingConfig{Auto: true, AfterTurns: 1},
		sessionTitler: sessReg,
		namingLLM: namingCompleterFunc(func(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
			return llm.CompleteResponse{}, nil
		}),
	}
	model := "override-model"
	spec, err := d.projectNaming(ctx, d.agentConfigID, q, &planner.LLMOverrides{Model: &model})
	if err != nil {
		t.Fatalf("projectNaming: %v", err)
	}
	if spec == nil || spec.Model != "override-model" {
		t.Fatalf("naming model = %+v, want the run's override model", spec)
	}
}

// callToolThenFinishPlanner emits ONE CallTool decision (to a registered
// in-proc tool) and then finishes — the tool-dispatch hook's driver.
type callToolThenFinishPlanner struct {
	mu     sync.Mutex
	called bool
}

func (p *callToolThenFinishPlanner) Next(_ context.Context, _ planner.RunContext) (planner.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.called {
		p.called = true
		return planner.CallTool{Tool: "echo-coverage", Args: json.RawMessage(`{}`)}, nil
	}
	return planner.Finish{Reason: planner.FinishGoal, Payload: map[string]any{"answer": "done"}}, nil
}

// TestRunOne_ToolDispatch_IncrementsToolCount — a run that dispatches one
// tool call advances the task's ToolCount through the per-run dispatch hook
// (the hook fails loud on an increment error, so a passing run proves the
// hook fired against the registry).
func TestRunOne_ToolDispatch_IncrementsToolCount(t *testing.T) {
	env := newFailDriverEnv(t)
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "echo-coverage"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: "echoed"}, nil
		},
	}); err != nil {
		t.Fatalf("register echo tool: %v", err)
	}
	artStore, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	executor := dispatch.NewToolExecutor(cat, artStore, env.reg,
		dispatch.WithHeavyThreshold(32768), dispatch.WithMaxSpawnDepth(2))

	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.Planner = &callToolThenFinishPlanner{}
		o.Catalog = cat
		o.Executor = executor
		o.ArtifactStore = artStore
		o.MaxStepsRunLoop = 4
	})
	id := spawnOn(t, env.reg, nil)
	if status := waitForTaskStatus(t, env.reg, id, tasks.StatusComplete, 5*time.Second); status != tasks.StatusComplete {
		t.Fatalf("tool-dispatching run stuck at %q, want complete", status)
	}
	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	got, err := env.reg.Get(ctx, id)
	if err != nil {
		t.Fatalf("reg.Get: %v", err)
	}
	if got.ToolCount != 1 {
		t.Errorf("ToolCount = %d, want 1 (the dispatch hook must have fired once)", got.ToolCount)
	}
}
