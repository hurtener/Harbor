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
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
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

// TestEnricher_ParentSession_WithSessionLister covers the ParentSession
// path with a real session lister wired — the branch that actually
// queries the session registry and maps the snapshot onto the wire ref.
func TestEnricher_ParentSession_WithSessionLister(t *testing.T) {
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

	e := NewEnricher(nil, WithSessionLister(reg))
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}

	// Open a session so the lister returns a snapshot.
	ctx := context.Background()
	if _, openErr := reg.Open(ctx, id.SessionID, id); openErr != nil {
		t.Fatalf("Open: %v", openErr)
	}
	ref := e.ParentSession(ctx, id, "task-1")
	if ref.Status == "" {
		t.Errorf("ParentSession with a live session should return a non-empty status, got %+v", ref)
	}

	// Empty SessionID → zero-value ref (the nil-session guard).
	empty := e.ParentSession(ctx, identity.Identity{TenantID: "t", UserID: "u"}, "task-1")
	if empty.Status != "" {
		t.Errorf("ParentSession with empty SessionID should return zero-value ref, got %+v", empty)
	}
}

// TestMCPConnectionDetacher_NilRegistry covers the detacher's nil-registry
// short-circuits + the boot-declared helpers.
func TestMCPConnectionDetacher_NilRegistry(t *testing.T) {
	d := NewMCPConnectionDetacher(nil, nil, nil)
	if got := d.AttachedSources(context.Background(), toolauth.Owner{Tenant: "t1", Agent: "agent-1"}); got != nil {
		t.Errorf("AttachedSources with nil registry should be nil, got %v", got)
	}
	if err := d.Detach(context.Background(), "srv-x", toolauth.Owner{Tenant: "t1", Agent: "agent-1"}); err != nil {
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

	// BootDeclaredOAuthProviderNames: nil cfg → nil; populated cfg →
	// names; empty-name entries are filtered.
	if got := BootDeclaredOAuthProviderNames(nil); got != nil {
		t.Errorf("BootDeclaredOAuthProviderNames(nil) = %v, want nil", got)
	}
	cfg.Tools.OAuthProviders = []config.ToolOAuthProviderConfig{{Name: "github"}, {Name: ""}, {Name: "google"}}
	provNames := BootDeclaredOAuthProviderNames(cfg)
	if len(provNames) != 2 || provNames[0] != "github" || provNames[1] != "google" {
		t.Errorf("BootDeclaredOAuthProviderNames = %v, want [github google]", provNames)
	}
}

// TestMCPConnectionDetacher_SetOAuthDiscoveryOrigins_NilRegistry covers
// the detacher's SetOAuthDiscoveryOrigins nil-registry short-circuit —
// the path a boot with no MCP registry takes when the agent-config
// service calls the discovery-origin applier.
func TestMCPConnectionDetacher_SetOAuthDiscoveryOrigins_NilRegistry(t *testing.T) {
	d := NewMCPConnectionDetacher(nil, nil, nil)
	prev, err := d.SetOAuthDiscoveryOrigins(context.Background(), toolauth.Owner{Tenant: "t1", Agent: "a1"}, "srv-x", []string{"https://origin.example.com"})
	if err != nil {
		t.Errorf("SetOAuthDiscoveryOrigins with nil registry should be a no-op, got %v", err)
	}
	if prev != nil {
		t.Errorf("SetOAuthDiscoveryOrigins with nil registry should return nil prev, got %v", prev)
	}
}

// TestMCPConnectionAttacher_SetOAuthDiscoveryOrigins_NilRegistry covers
// the attacher's SetOAuthDiscoveryOrigins nil-registry error path —
// the guard fires loud (no silent degradation).
func TestMCPConnectionAttacher_SetOAuthDiscoveryOrigins_NilRegistry(t *testing.T) {
	a := NewMCPConnectionAttacher(nil, nil, nil, nil, identity.Identity{}, nil, nil, nil)
	_, err := a.SetOAuthDiscoveryOrigins(context.Background(), "t1", "a1", "srv-x", []string{"https://origin.example.com"})
	if err == nil {
		t.Fatal("SetOAuthDiscoveryOrigins with nil registry must fail loud (no silent degradation)")
	}
	if !strings.Contains(err.Error(), "no registry wired") {
		t.Errorf("expected 'no registry wired' error, got %v", err)
	}
}

// TestMCPConnectionAttacher_SetOAuthDiscoveryOrigins_IncompleteOwner pins the
// fail-closed owner guard on the live discovery-origin write: both owner
// components are mandatory, matching the attach path's own owner requirement.
func TestMCPConnectionAttacher_SetOAuthDiscoveryOrigins_IncompleteOwner(t *testing.T) {
	a := NewMCPConnectionAttacher(nil, mcpdrv.NewRegistry(), nil, nil, identity.Identity{}, nil, nil, nil)
	for _, tc := range []struct{ tenant, agent string }{{"", "a1"}, {"t1", ""}, {"", ""}} {
		_, err := a.SetOAuthDiscoveryOrigins(context.Background(), tc.tenant, tc.agent, "srv-x", []string{"https://origin.example.com"})
		if !errors.Is(err, ErrRuntimeAddOwnerMissing) {
			t.Fatalf("owner (tenant=%q agent=%q): err = %v, want ErrRuntimeAddOwnerMissing", tc.tenant, tc.agent, err)
		}
	}
}

// TestParentSessionStatus_LifecycleMapping covers the pure helper that
// maps a session snapshot onto the parent-session card's lifecycle
// status string. All four branches are exercised.
func TestParentSessionStatus_LifecycleMapping(t *testing.T) {
	cases := []struct {
		name string
		snap sessions.SessionSnapshot
		want string
	}{
		{"running", sessions.SessionSnapshot{Running: true}, string(prototypes.SessionStatusRunning)},
		{"open_not_closed", sessions.SessionSnapshot{Running: false}, string(prototypes.SessionStatusRunning)},
		{"closed_failed", sessions.SessionSnapshot{Session: sessions.Session{Closed: true, ClosedReason: "failed"}}, string(prototypes.SessionStatusFailed)},
		{"closed_completed", sessions.SessionSnapshot{Session: sessions.Session{Closed: true, ClosedReason: "done"}}, string(prototypes.SessionStatusCompleted)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parentSessionStatus(tc.snap)
			if got != tc.want {
				t.Errorf("parentSessionStatus(%+v) = %q, want %q", tc.snap, got, tc.want)
			}
		})
	}
}

// TestOAuthProviderInstaller_NilInputsReturnsNil covers the constructor's
// nil-guard: a nil builder OR a nil set returns a nil installer (so the
// caller leaves the install verbs unwired → 501 at the wire edge).
func TestOAuthProviderInstaller_NilInputsReturnsNil(t *testing.T) {
	if got := NewOAuthProviderInstaller(nil, nil, false, nil); got != nil {
		t.Error("NewOAuthProviderInstaller(nil, nil, ...) must return nil")
	}
}

// TestApprovalChecker_NilCoordinatorReturnsNil + the HasPendingApproval
// guard branches cover the approval-checker seam: nil coordinator → nil
// installer; invalid identity / empty runID → false (honest no gate).
func TestApprovalChecker_NilCoordinatorReturnsNil(t *testing.T) {
	if got := NewApprovalChecker(nil); got != nil {
		t.Error("NewApprovalChecker(nil) must return nil")
	}
}

// TestApprovalChecker_HasPendingApproval_GuardBranches covers the
// identity-validation and empty-runID guards — the two short-circuit
// paths that return false without touching the coordinator.
func TestApprovalChecker_HasPendingApproval_GuardBranches(t *testing.T) {
	// A non-nil coordinator constructs a non-nil checker. We use
	// pauseresume.New() — the real coordinator constructor.
	coord := pauseresume.New()
	checker := NewApprovalChecker(coord)
	if checker == nil {
		t.Fatal("NewApprovalChecker with a real coordinator returned nil")
	}
	ctx := context.Background()
	// Invalid identity → false (the validate guard).
	if checker.HasPendingApproval(ctx, identity.Identity{}, "run-1") {
		t.Error("HasPendingApproval with an empty identity must return false")
	}
	// Empty RunID → false (the run-less-task guard).
	valid := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	if checker.HasPendingApproval(ctx, valid, "") {
		t.Error("HasPendingApproval with an empty RunID must return false")
	}
	// Valid identity + runID + no paused gates → false (the coordinator
	// returns zero rows). This covers the List call + the TotalRows==0
	// branch.
	if checker.HasPendingApproval(ctx, valid, "run-1") {
		t.Error("HasPendingApproval with no paused gates must return false")
	}
}

// TestOAuthProviderInstaller_EmptyOwnerFailsLoud covers InstallProvider's
// empty-owner guard: a missing tenant or agent fails with
// ErrInvalidProvider before the builder is called. The builder is
// constructed with no brokers (Build fails loud), so reaching the owner
// check is the only path that does not invoke Build.
func TestOAuthProviderInstaller_EmptyOwnerFailsLoud(t *testing.T) {
	builder, err := toolauth.NewProviderBuilder(context.Background(), config.ToolsConfig{}, toolauth.BuildDeps{})
	if err != nil {
		t.Fatalf("NewProviderBuilder: %v", err)
	}
	set := toolauth.NewProviderSet(nil)
	installer := NewOAuthProviderInstaller(builder, set, false, nil)
	if installer == nil {
		t.Fatal("NewOAuthProviderInstaller returned nil for non-nil inputs")
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		tenant string
		agent  string
	}{
		{"empty_tenant", "", "agent-1"},
		{"empty_agent", "tenant-1", ""},
		{"both_empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := installer.InstallProvider(ctx, tc.tenant, tc.agent, agentcfg.OAuthProviderDescriptor{
				Name: "test-provider", CredentialBroker: "unknown-broker",
			})
			if !errors.Is(err, agentcfgprotocol.ErrInvalidProvider) {
				t.Errorf("expected ErrInvalidProvider, got %v", err)
			}
		})
	}
}

// TestOAuthProviderInstaller_UnknownBrokerFailsLoud covers the
// InstallProvider path where the builder rejects an unknown broker —
// the error is wrapped into ErrProviderBrokerUnknown so the wire
// handler classifies it as a 400.
func TestOAuthProviderInstaller_UnknownBrokerFailsLoud(t *testing.T) {
	builder, err := toolauth.NewProviderBuilder(context.Background(), config.ToolsConfig{}, toolauth.BuildDeps{})
	if err != nil {
		t.Fatalf("NewProviderBuilder: %v", err)
	}
	set := toolauth.NewProviderSet(nil)
	installer := NewOAuthProviderInstaller(builder, set, false, nil)
	err = installer.InstallProvider(context.Background(), "tenant-1", "agent-1", agentcfg.OAuthProviderDescriptor{
		Name: "test-provider", CredentialBroker: "unknown-broker",
	})
	if !errors.Is(err, agentcfgprotocol.ErrProviderBrokerUnknown) {
		t.Errorf("expected ErrProviderBrokerUnknown, got %v", err)
	}
}

// TestOAuthProviderInstaller_UninstallAndInstalledFor covers the
// uninstall + installed-for methods against an empty provider set —
// the no-op paths that the agent-config service calls.
func TestOAuthProviderInstaller_UninstallAndInstalledFor(t *testing.T) {
	builder, err := toolauth.NewProviderBuilder(context.Background(), config.ToolsConfig{}, toolauth.BuildDeps{})
	if err != nil {
		t.Fatalf("NewProviderBuilder: %v", err)
	}
	set := toolauth.NewProviderSet(nil)
	installer := NewOAuthProviderInstaller(builder, set, false, nil)
	// Uninstall on an empty set — the set's Uninstall returns nil for
	// a not-found name (idempotent delete semantics).
	_ = installer.UninstallProvider(context.Background(), "t", "a", "nonexistent")
	// InstalledFor on an empty owner returns an empty slice.
	names := installer.InstalledFor(context.Background(), toolauth.Owner{Tenant: "t", Agent: "a"})
	if len(names) != 0 {
		t.Errorf("InstalledFor on an empty set returned %v, want empty", names)
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
	if _, err := agentCfgReg.SetRevision(context.Background(), identity.Quadruple{Identity: runLoopDriverTestID}, "coverage-agent", agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed coverage agent lifecycle: %v", err)
	}
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
		failed, getErr := reg.Get(ctx, h.ID)
		t.Fatalf("fully-wired run stuck at %q, want %q (task error=%+v, get error=%v)", status, tasks.StatusComplete, failed.Error, getErr)
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
		identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, nil, nil, nil)

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
		identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, nil, nil, nil)
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
	if err := d.Detach(context.Background(), "never-attached", toolauth.Owner{Tenant: "t1", Agent: "agent-1"}); err != nil {
		t.Errorf("Detach of an unknown source must be idempotent, got %v", err)
	}
}

// TestSessionEnsurerAdapter_SentinelTranslation covers the adapter's two
// lifecycle outcomes after RFC §6.9 was amended (D-312): a closed session
// RE-ACTIVATES on the next EnsureSession (no error), and an ERASED session
// translates the registry's ErrReopenAfterErase onto the protocol-side
// ErrSessionReopenAfterErase sentinel the surface's error mapper reads
// (→ CodeSessionErased).
func TestSessionEnsurerAdapter_SentinelTranslation(t *testing.T) {
	ctxBg := context.Background()
	st, err := state.Open(ctxBg, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(ctxBg) })
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg, err := sessions.New(st, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: 15 * time.Minute,
	}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(ctxBg) })

	ad := NewSessionEnsurerAdapter(reg)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "sess-close-me"}
	ctx, err := identity.With(ctxBg, id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if err := ad.EnsureSession(ctx, id); err != nil {
		t.Fatalf("EnsureSession (create): %v", err)
	}
	if err := reg.Close(ctx, id.SessionID, "test close"); err != nil {
		t.Fatalf("registry.Close: %v", err)
	}
	// Reopen-after-close now SUCCEEDS (RFC §6.9 amended — D-312): the adapter
	// returns nil, and the surface proceeds to `start` on the resumed session.
	if err := ad.EnsureSession(ctx, id); err != nil {
		t.Fatalf("EnsureSession after close = %v, want reopen (nil)", err)
	}

	// Now erase the session and assert the adapter translates the terminal
	// ErrReopenAfterErase onto the protocol sentinel.
	mem, err := memory.Open(ctxBg, memory.ConfigSnapshot{
		Driver: "inmem", Strategy: memory.StrategyTruncation, BudgetTokens: 1000,
	}, memory.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close(ctxBg) })
	arts, err := artifacts.Open(ctxBg, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = arts.Close(ctxBg) })
	skillStore, err := skills.Open(ctxBg, skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("skills.Open: %v", err)
	}
	t.Cleanup(func() { _ = skillStore.Close(ctxBg) })
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: reg, State: st, Memory: mem, Artifacts: arts, Skills: skillStore, Bus: bus, Redactor: red,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}
	if _, derr := eraser.Erase(ctx, id); derr != nil {
		t.Fatalf("Erase: %v", derr)
	}
	err = ad.EnsureSession(ctx, id)
	if !errors.Is(err, protocol.ErrSessionReopenAfterErase) {
		t.Fatalf("EnsureSession after erase = %v, want the protocol reopen-after-erase sentinel", err)
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
