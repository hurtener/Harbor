package serve

import (
	"bufio"
	"context"
	errpkg "errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestEnricher_ZeroValuesAndTrajectory covers the tasks.get enricher's four
// methods: the three zero-value projections and the trajectory accessor.
func TestEnricher_ZeroValuesAndTrajectory(t *testing.T) {
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}

	// Nil accessor → nil trajectory.
	e := NewEnricher(nil)
	if e.Trajectory(context.Background(), id, "task-1") != nil {
		t.Error("nil trajectoryFn should yield a nil trajectory")
	}
	if got := e.ParentSession(context.Background(), id, "task-1"); got.SessionID != "" {
		t.Errorf("ParentSession should be zero-valued, got %+v", got)
	}
	if got := e.Cost(context.Background(), id, "task-1"); len(got.PerStep) != 0 {
		t.Errorf("Cost.PerStep should be empty, got %+v", got.PerStep)
	}
	if e.PlannerSnapshot(context.Background(), id, "task-1") != nil {
		t.Error("PlannerSnapshot should be nil")
	}

	// A trajectory with a reasoning-carrying step projects onto the wire.
	e2 := NewEnricher(func(tasks.TaskID) *planner.Trajectory {
		return &planner.Trajectory{Steps: []planner.Step{
			{ReasoningTrace: ""},              // filtered (empty)
			{ReasoningTrace: "thinking hard"}, // kept
		}}
	})
	ref := e2.Trajectory(context.Background(), id, "task-1")
	if ref == nil || len(ref.Steps) != 1 {
		t.Fatalf("expected 1 projected step, got %+v", ref)
	}
	if ref.Steps[0].ReasoningTrace != "thinking hard" {
		t.Errorf("wrong reasoning trace: %q", ref.Steps[0].ReasoningTrace)
	}

	// A trajectory with only empty traces yields nil.
	e3 := NewEnricher(func(tasks.TaskID) *planner.Trajectory {
		return &planner.Trajectory{Steps: []planner.Step{{ReasoningTrace: ""}}}
	})
	if e3.Trajectory(context.Background(), id, "task-1") != nil {
		t.Error("all-empty trajectory should project to nil")
	}
}

// TestMCPConnectionDetacher_NilRegistry covers the detacher's nil-registry
// short-circuits + the boot-declared helpers.
func TestMCPConnectionDetacher_NilRegistry(t *testing.T) {
	d := NewMCPConnectionDetacher(nil, nil, nil)
	if got := d.AttachedSources(context.Background()); got != nil {
		t.Errorf("AttachedSources with nil registry should be nil, got %v", got)
	}
	if err := d.Detach(context.Background(), "srv-x"); err != nil {
		t.Errorf("Detach with nil registry should be a no-op, got %v", err)
	}

	cfg := &config.Config{}
	cfg.Tools.MCPServers = []config.MCPServerConfig{{Name: "alpha"}, {Name: ""}, {Name: "beta"}}
	names := BootDeclaredMCPServerNames(cfg)
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Errorf("BootDeclaredMCPServerNames = %v, want [alpha beta]", names)
	}
	set := BootDeclaredMCPServerSet(cfg)
	if _, ok := set["alpha"]; !ok {
		t.Error("BootDeclaredMCPServerSet missing alpha")
	}
	if BootDeclaredMCPServerSet(&config.Config{}) != nil {
		t.Error("empty config should yield a nil set")
	}
}

// TestSessionEnsurerAdapter_EnsureSession covers the ensurer adapter over a
// real StateStore-backed session registry (create-on-first-use).
func TestSessionEnsurerAdapter_EnsureSession(t *testing.T) {
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg, err := sessions.New(st, config.SessionsConfig{
		IdleTTL:       24 * time.Hour,
		HardCap:       720 * time.Hour,
		SweepInterval: 15 * time.Minute,
	}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })

	ad := NewSessionEnsurerAdapter(reg)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	if err := ad.EnsureSession(context.Background(), id); err != nil {
		t.Fatalf("EnsureSession (create-on-first-use): %v", err)
	}
	// A missing-identity request fails closed.
	if err := ad.EnsureSession(context.Background(), identity.Identity{}); err == nil {
		t.Error("EnsureSession with an empty identity should fail closed")
	}
}

// TestServedHandle_ServeRealListener covers Handle.Serve + BindAddr against a
// real ephemeral listener (the concurrent-reuse test uses Handler() only).
func TestServedHandle_ServeRealListener(t *testing.T) {
	pr, pw := io.Pipe()
	opts := baseOptions(t)
	opts.Stderr = pw

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Boot(ctx, opts)
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- h.Serve(ctx) }()

	// Read stderr until the HARBOR_DEV_BOUND contract line appears.
	boundCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "HARBOR_DEV_BOUND=") {
				boundCh <- strings.TrimPrefix(sc.Text(), "HARBOR_DEV_BOUND=")
				return
			}
		}
		boundCh <- ""
	}()

	var bound string
	select {
	case bound = <-boundCh:
	case <-time.After(10 * time.Second):
		t.Fatal("HARBOR_DEV_BOUND never printed")
	}
	if bound == "" {
		t.Fatal("empty bound address")
	}
	if h.BindAddr() != bound {
		t.Errorf("BindAddr() = %q, want %q", h.BindAddr(), bound)
	}

	resp, err := http.Get("http://" + bound + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz over real listener: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz over real listener = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}
	_ = pw.Close()

	cc, ccCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccCancel()
	h.Close(cc)
}

// TestTransportModeForAdd covers the control-plane→driver transport mapping.
func TestTransportModeForAdd(t *testing.T) {
	if got := transportModeForAdd(agentcfg.MCPTransportStdio); got != "stdio" {
		t.Errorf("stdio → %q, want stdio", got)
	}
	if got := transportModeForAdd(agentcfg.MCPTransportHTTP); got != "auto" {
		t.Errorf("http → %q, want auto (URL-negotiated)", got)
	}
}

// TestWrapErr covers the shared surface-construction error wrapper.
func TestWrapErr(t *testing.T) {
	base := errTestSentinel
	got := wrapErr("posture surface", base)
	if got == nil || !strings.Contains(got.Error(), "posture surface") {
		t.Fatalf("wrapErr lost context: %v", got)
	}
	if !errorsIs(got, base) {
		t.Error("wrapErr must preserve the wrapped error for errors.Is")
	}
}

// TestSessionOverridesStore_Accessor covers the driver's shared-store accessor.
func TestSessionOverridesStore_Accessor(t *testing.T) {
	d := &RunLoopDriver{}
	if d.SessionOverridesStore() != nil {
		t.Error("a driver with no session-override store should return nil")
	}
}

var errTestSentinel = errpkg.New("serve: coverage-test sentinel")

func errorsIs(err, target error) bool { return errpkg.Is(err, target) }

// TestPerTaskRunLoop_WithMemoryWired_DrivesCompletingRun exercises runOne's
// optional-subsystem branches (the memory fetch + agent-config projections)
// that the minimal-wiring driver tests skip. The devstack-backed integration
// tests (phase83f / phase111e) additionally drive these branches with every
// optional subsystem wired end-to-end.
func TestPerTaskRunLoop_WithMemoryWired_DrivesCompletingRun(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)

	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	mem, err := memoryOpen(t, bus, st)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}

	steerReg := steeringNewRegistry()
	rl := newTestRunLoop(t, steerReg, bus)
	p := &driverTestPlanner{finishGoalImmediately: true, finishPayload: map[string]any{"answer": "ok"}}

	driver, err := NewRunLoopDriver(RunLoopDriverOptions{
		Bus:     bus,
		RunLoop: rl,
		Planner: p,
		Tasks:   reg,
		Memory:  mem,
	})
	if err != nil {
		t.Fatalf("NewRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	taskID := spawnDriverTestTask(t, reg)
	if status := waitForTaskStatus(t, reg, taskID, tasks.StatusComplete, 3*time.Second); status != tasks.StatusComplete {
		t.Fatalf("task FSM stuck at %q with memory wired, want %q", status, tasks.StatusComplete)
	}
}

func memoryOpen(t *testing.T, bus events.EventBus, st state.StateStore) (memory.MemoryStore, error) {
	t.Helper()
	return memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:       "inmem",
		Strategy:     memory.StrategyTruncation,
		BudgetTokens: 4096,
	}, memory.Deps{State: st, Bus: bus})
}

func steeringNewRegistry() *steering.Registry { return steering.NewRegistry() }

func newTestRunLoop(t *testing.T, reg *steering.Registry, bus events.EventBus) *steering.RunLoop {
	t.Helper()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(reg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	return rl
}
