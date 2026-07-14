package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/artifacts"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
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
	if got := d.AttachedSources(context.Background(), toolauth.Owner{Tenant: "t1", Agent: "agent-1"}); got != nil {
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

var errTestSentinel = errors.New("serve: coverage-test sentinel")

func errorsIs(err, target error) bool { return errors.Is(err, target) }

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

// TestPerTaskRunLoop_FullyWired_DrivesCompletingRun drives runOne with EVERY
// optional subsystem wired — catalog + executor, artifact store, skills
// directory, agent-config registry + session overlay, tenant + session
// overrides, an output schema, and the disposition policy — so the run-start
// projection branches the minimal-wiring tests skip all execute. (The
// devstack-backed integration suite additionally drives these end-to-end.)
func TestPerTaskRunLoop_FullyWired_DrivesCompletingRun(t *testing.T) {
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
	artStore, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	skillStore, err := skills.Open(context.Background(), skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("skills.Open: %v", err)
	}
	skillsDir, err := skills.NewDirectory(skillStore, skills.Deps{Bus: bus},
		skills.DirectoryFromConfig(config.SkillsConfig{}, 5))
	if err != nil {
		t.Fatalf("skills.NewDirectory: %v", err)
	}
	agentCfgReg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = agentCfgReg.Close(context.Background()) })
	overlay, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatalf("sessionoverlay.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = overlay.Close(context.Background()) })

	cat := tools.NewCatalog()
	executor := dispatch.NewToolExecutor(cat, artStore, reg,
		dispatch.WithHeavyThreshold(32768), dispatch.WithMaxSpawnDepth(2))

	runsStore := runsprotocol.NewStore()
	steerReg := steering.NewRegistry()
	rl := newTestRunLoop(t, steerReg, bus)
	p := &driverTestPlanner{finishGoalImmediately: true, finishPayload: map[string]any{"answer": "ok"}}

	driver, err := NewRunLoopDriver(RunLoopDriverOptions{
		Bus:              bus,
		RunLoop:          rl,
		Planner:          p,
		Tasks:            reg,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Memory:           mem,
		SkillsDirectory:  skillsDir,
		Catalog:          cat,
		Executor:         executor,
		ArtifactStore:    artStore,
		GrantedScopes:    []string{"scope-a"},
		AgentConfig:      agentCfgReg,
		AgentConfigID:    "coverage-agent",
		SessionOverlay:   overlay,
		SessionOverrides: runsStore,
		TenantOverrides:  fakeTenantOverrides{set: false},
		MaxStepsRunLoop:  4,
	})
	if err != nil {
		t.Fatalf("NewRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	// Spawn WITH an output schema so the compile branch executes (a
	// permissive object schema the finish payload satisfies).
	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity:     identity.Quadruple{Identity: runLoopDriverTestID},
		Kind:         tasks.KindForeground,
		Query:        "fully-wired coverage goal",
		OutputSchema: json.RawMessage(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatalf("reg.Spawn: %v", err)
	}
	if status := waitForTaskStatus(t, reg, h.ID, tasks.StatusComplete, 5*time.Second); status != tasks.StatusComplete {
		t.Fatalf("fully-wired run stuck at %q, want %q", status, tasks.StatusComplete)
	}
	// The trajectory accessor serves the completed run.
	if driver.TrajectoryByTaskID(h.ID) == nil {
		t.Error("TrajectoryByTaskID returned nil for the completed run")
	}
}

// TestRunLoopDriver_HandleEvent_SkipsAndMalformed covers handleEvent's
// non-driven-kind skip and the malformed-payload guard.
func TestRunLoopDriver_HandleEvent_SkipsAndMalformed(t *testing.T) {
	d := &RunLoopDriver{
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		taskKind: tasks.KindForeground,
	}
	// Malformed payload type — logged and skipped, never a panic.
	d.handleEvent(events.Event{Type: tasks.EventTypeTaskSpawned, Payload: wrongSpawnPayload{}})
	// A kind this driver does not drive — skipped.
	d.handleEvent(events.Event{
		Type:     tasks.EventTypeTaskSpawned,
		Identity: identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}},
		Payload:  tasks.TaskSpawnedPayload{TaskID: "task-x", Kind: tasks.KindBackground},
	})
	// An incomplete identity — validated and skipped.
	d.handleEvent(events.Event{
		Type:    tasks.EventTypeTaskSpawned,
		Payload: tasks.TaskSpawnedPayload{TaskID: "task-y", Kind: tasks.KindForeground},
	})
}

// TestMCPConnectionAttacher_Attach_FailsFastAndDrains covers Attach's
// full path against a real catalog + registry with an unreachable HTTP
// endpoint: the dial fails loud (fast), the non-auth error is NOT wrapped as
// auth-required, and Close drains whatever closers the partial attach merged.
func TestMCPConnectionAttacher_Attach_FailsFastAndDrains(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	cat := tools.NewCatalog()
	registry := mcpdrv.NewRegistry()
	a := NewMCPConnectionAttacher(cat, registry, bus, slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := a.Attach(ctx, agentcfgprotocol.AttachRequest{
		// A full (tenant, agent) owner so the attach reaches the DIAL — this
		// test exercises the unreachable-endpoint classification, not the
		// owner fail-closed guard (covered separately).
		Identity:  identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
		AgentID:   "agent-1",
		Name:      "unreachable",
		Transport: agentcfg.MCPTransportHTTP,
		URL:       "http://127.0.0.1:1/mcp", // nothing listens on port 1
	})
	if err == nil {
		t.Fatal("Attach against an unreachable endpoint must fail loud")
	}
	if errors.Is(err, agentcfgprotocol.ErrAuthRequired) {
		t.Errorf("a connection-refused dial must not classify as auth-required: %v", err)
	}
	if errors.Is(err, ErrRuntimeAddOwnerMissing) {
		t.Errorf("a valid-owner attach must not trip the owner guard: %v", err)
	}
	if cErr := a.Close(ctx); cErr != nil {
		t.Errorf("Close after a failed attach: %v", cErr)
	}
}

// TestMCPConnectionAttacher_MissingOwner_FailsClosed covers the fail-closed
// owner guard: a runtime add reaching the attacher without a full (tenant,
// agent) owner is rejected loud (ErrRuntimeAddOwnerMissing) BEFORE any dial,
// and nothing is registered.
func TestMCPConnectionAttacher_MissingOwner_FailsClosed(t *testing.T) {
	bus := mkDriverTestBus(t, auditpatterns.New())
	cat := tools.NewCatalog()
	registry := mcpdrv.NewRegistry()
	a := NewMCPConnectionAttacher(cat, registry, bus, slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, nil)
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	cases := []struct {
		name string
		req  agentcfgprotocol.AttachRequest
	}{
		{"no agent", agentcfgprotocol.AttachRequest{
			Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
			Name:     "orphan", Transport: agentcfg.MCPTransportHTTP, URL: "http://127.0.0.1:1/mcp",
		}},
		{"no tenant", agentcfgprotocol.AttachRequest{
			Identity: identity.Identity{UserID: "u", SessionID: "s"}, AgentID: "agent-1",
			Name: "orphan", Transport: agentcfg.MCPTransportHTTP, URL: "http://127.0.0.1:1/mcp",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := a.Attach(context.Background(), tc.req)
			if !errors.Is(err, ErrRuntimeAddOwnerMissing) {
				t.Fatalf("Attach(%s) err = %v, want ErrRuntimeAddOwnerMissing", tc.name, err)
			}
		})
	}
	// Nothing was registered — the guard fired before any dial / registration.
	if got := registry.SourceIDs(); len(got) != 0 {
		t.Fatalf("registry has %v after fail-closed attaches, want empty", got)
	}
}

// TestMCPConnectionDetacher_RealRegistry covers Detach + AttachedSources over
// a real (empty) registry: enumeration is empty and a detach of an unknown
// source is an idempotent no-op (ErrServerNotFound swallowed).
func TestMCPConnectionDetacher_RealRegistry(t *testing.T) {
	registry := mcpdrv.NewRegistry()
	d := NewMCPConnectionDetacher(tools.NewCatalog(), registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := d.AttachedSources(context.Background(), toolauth.Owner{Tenant: "t1", Agent: "agent-1"}); len(got) != 0 {
		t.Errorf("AttachedSources on an empty registry = %v, want empty", got)
	}
	if err := d.Detach(context.Background(), "never-attached"); err != nil {
		t.Errorf("Detach of an unknown source must be idempotent, got %v", err)
	}
}

// TestSessionEnsurerAdapter_SentinelTranslation covers the reopen-after-close
// sentinel translation (the registry sentinel maps onto the protocol-side
// sentinel the surface's error mapper reads).
func TestSessionEnsurerAdapter_SentinelTranslation(t *testing.T) {
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg, err := sessions.New(st, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: 15 * time.Minute,
	}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })

	ad := NewSessionEnsurerAdapter(reg)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "sess-close-me"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if err := ad.EnsureSession(ctx, id); err != nil {
		t.Fatalf("EnsureSession (create): %v", err)
	}
	if err := reg.Close(ctx, id.SessionID, "test close"); err != nil {
		t.Fatalf("registry.Close: %v", err)
	}
	err = ad.EnsureSession(ctx, id)
	if !errors.Is(err, protocol.ErrSessionReopenAfterClose) {
		t.Fatalf("EnsureSession after close = %v, want the protocol reopen-after-close sentinel", err)
	}
}

// TestMCPAddStdioAllowlist_Branches covers the nil-config / nil-block /
// populated branches.
func TestMCPAddStdioAllowlist_Branches(t *testing.T) {
	if got := MCPAddStdioAllowlist(nil); got != nil {
		t.Errorf("nil cfg → %v, want nil (fail-closed)", got)
	}
	if got := MCPAddStdioAllowlist(&config.Config{}); got != nil {
		t.Errorf("nil block → %v, want nil (fail-closed)", got)
	}
	cfg := &config.Config{}
	cfg.Tools.MCPAddConnection = &config.MCPAddConnectionConfig{StdioAllowlist: []string{"/bin/x"}}
	if got := MCPAddStdioAllowlist(cfg); len(got) != 1 || got[0] != "/bin/x" {
		t.Errorf("populated allowlist → %v, want [/bin/x]", got)
	}
}

// wrongSpawnPayload is a valid EventPayload of the WRONG concrete type — the
// malformed-payload guard's fixture.
type wrongSpawnPayload struct{ events.SafeSealed }

// TestBootDeclaredMCPServerNames_NilConfig covers the nil-config guard.
func TestBootDeclaredMCPServerNames_NilConfig(t *testing.T) {
	if got := BootDeclaredMCPServerNames(nil); got != nil {
		t.Errorf("BootDeclaredMCPServerNames(nil) = %v, want nil", got)
	}
}

// TestLooksLikeAuthRequired_Nil covers the nil-error guard.
func TestLooksLikeAuthRequired_Nil(t *testing.T) {
	if looksLikeAuthRequired(nil) {
		t.Error("looksLikeAuthRequired(nil) must be false")
	}
}

// TestSessionEnsurerAdapter_SessionIDReuse covers the session-id-reuse
// sentinel translation: the same session id under a DIFFERENT (tenant,user)
// maps onto the protocol-side reuse sentinel.
func TestSessionEnsurerAdapter_SessionIDReuse(t *testing.T) {
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg, err := sessions.New(st, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: 15 * time.Minute,
	}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })

	ad := NewSessionEnsurerAdapter(reg)
	idA := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "shared-sess"}
	ctxA, err := identity.With(context.Background(), idA)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if err := ad.EnsureSession(ctxA, idA); err != nil {
		t.Fatalf("EnsureSession (first owner): %v", err)
	}
	idB := identity.Identity{TenantID: "tenant-b", UserID: "user-b", SessionID: "shared-sess"}
	ctxB, err := identity.With(context.Background(), idB)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	err = ad.EnsureSession(ctxB, idB)
	if !errors.Is(err, protocol.ErrSessionIDReuse) {
		t.Fatalf("EnsureSession under a different owner = %v, want the session-id-reuse sentinel", err)
	}
}
