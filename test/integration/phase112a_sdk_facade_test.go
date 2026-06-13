// phase112a_sdk_facade_test.go — Phase 112a (D-205): the public SDK
// facade integrity test. RFC §3.6's curated `sdk/` alias tree is the
// first Harbor surface external modules can import; this file proves
// it carries the full embedding story WITHOUT touching `internal/`:
//
//  1. THE HEADLESS RECIPE THROUGH THE FACADE — the
//     docs/recipes/embed-harbor-headless.md path expressed exclusively
//     through `sdk/` imports: config.Defaults → ValidateCore →
//     sdk/drivers/prod blank import → assemble.Assemble → an in-proc
//     tool registered via sdk/tools/inproc → a deterministic planner
//     (sdk/planner/deterministic) driving CallTool + Finish through
//     the assembled RunLoop → planner.AnswerEnvelope → Close.
//  2. IDENTITY PROPAGATION — two identities share one stack; per-run
//     answers carry their own goal, the event bus delivers only the
//     subscribed identity's events, and an incomplete identity fails
//     closed at the run-loop gate (§6 rule 9).
//  3. FAILURE MODE — the missing-LLM-config misconfiguration fails
//     LOUD through the facade (ValidateCore names the missing key).
//  4. PROD PARITY — `sdk/drivers/prod` seats the same production
//     registrations as the internal aggregator (asserted through each
//     facade's RegisteredDrivers; structural parity is grep-asserted
//     by scripts/smoke/phase-112a.sh).
//  5. COMPILE COVERAGE — every exported facade name is referenced at
//     the bottom of this file, so a facade re-export that stops
//     resolving breaks this build (the plan's ≥95% integrity bar).
//
// BINDING: this file MUST NOT import any internal/ package — the
// phase-112a smoke greps for that. Alias identity is proven by
// construction throughout: assemble.Stack's fields are internal-typed
// values flowing into sdk-typed variables and back.
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	// The public production driver aggregator — the facade twin of the
	// internal blank-import home (Phase 110c, D-196).
	_ "github.com/hurtener/Harbor/sdk/drivers/prod"

	sdkartifacts "github.com/hurtener/Harbor/sdk/artifacts"
	sdkassemble "github.com/hurtener/Harbor/sdk/assemble"
	sdkconfig "github.com/hurtener/Harbor/sdk/config"
	sdkdispatch "github.com/hurtener/Harbor/sdk/dispatch"
	sdkembeddings "github.com/hurtener/Harbor/sdk/embeddings"
	sdkevents "github.com/hurtener/Harbor/sdk/events"
	sdkidentity "github.com/hurtener/Harbor/sdk/identity"
	sdkllm "github.com/hurtener/Harbor/sdk/llm"
	sdkmemory "github.com/hurtener/Harbor/sdk/memory"
	sdkplanner "github.com/hurtener/Harbor/sdk/planner"
	sdkdeterministic "github.com/hurtener/Harbor/sdk/planner/deterministic"
	sdkreact "github.com/hurtener/Harbor/sdk/planner/react"
	sdkrunctx "github.com/hurtener/Harbor/sdk/runctx"
	sdkskills "github.com/hurtener/Harbor/sdk/skills"
	sdkstate "github.com/hurtener/Harbor/sdk/state"
	sdksteering "github.com/hurtener/Harbor/sdk/steering"
	sdktasks "github.com/hurtener/Harbor/sdk/tasks"
	sdktools "github.com/hurtener/Harbor/sdk/tools"
	sdkbuiltin "github.com/hurtener/Harbor/sdk/tools/builtin"
	sdkinproc "github.com/hurtener/Harbor/sdk/tools/inproc"
)

// phase112aCfg builds a ValidateCore-passing config exclusively
// through the facade. The LLM block uses a custom-provider entry with
// a loopback BaseURL and an env-var key: the bifrost client constructs
// offline (no network at Open) and the deterministic planner override
// means no completion is ever requested.
func phase112aCfg(t *testing.T) *sdkconfig.Config {
	t.Helper()
	t.Setenv("HARBOR_PHASE112A_TEST_KEY", "phase112a-dummy-key-not-real") // documented dummy (§7)
	cfg := sdkconfig.Defaults()
	cfg.LLM.Provider = "phase112a"
	cfg.LLM.Model = "phase112a/facade-model"
	cfg.LLM.CustomProviders = []sdkconfig.LLMCustomProviderConfig{{
		Name:         "phase112a",
		BaseURL:      "http://127.0.0.1:9", // RFC 863 discard port — never dialed
		APIKeyEnvVar: "HARBOR_PHASE112A_TEST_KEY",
		Models:       []string{"phase112a/facade-model"},
		Timeout:      2 * time.Second,
	}}
	cfg.LLM.ModelProfiles = map[string]sdkconfig.LLMModelProfileConfig{
		"phase112a/facade-model": {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4"},
	}
	// Skills has no Defaults() driver — opt in so the assembled stack
	// carries a SkillStore the facade-alias assertions can observe.
	cfg.Skills.Driver = sdkskills.DefaultDriver
	cfg.Skills.DSN = ":memory:"
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("ValidateCore through the facade: %v", err)
	}
	return cfg
}

// phase112aEchoArgs / phase112aEchoOut are the in-proc tool's typed
// contract (schemas reflection-derived by sdk/tools/inproc).
type phase112aEchoArgs struct {
	Text string `json:"text"`
}

type phase112aEchoOut struct {
	Echo string `json:"echo"`
}

// phase112aPlanner builds the deterministic decision tree: dispatch
// the sdk_echo tool once, then finish with an answer derived from the
// run's OWN goal (the context-bleed gate).
func phase112aPlanner(t *testing.T) *sdkdeterministic.DeterministicPlanner {
	t.Helper()
	p, err := sdkdeterministic.NewDeterministicPlanner(
		sdkdeterministic.WithName("phase112a-facade"),
		sdkdeterministic.WithSteps(
			&sdkdeterministic.CallToolStep{
				Tool: "sdk_echo",
				ArgsBuilder: func(rc sdkplanner.RunContext) (json.RawMessage, error) {
					return json.Marshal(phase112aEchoArgs{Text: rc.Goal})
				},
				When: func(rc sdkplanner.RunContext) bool { return len(rc.Trajectory.Steps) == 0 },
			},
			&sdkdeterministic.FinishStep{
				Reason: sdkplanner.FinishGoal,
				PayloadBuilder: func(rc sdkplanner.RunContext) (any, error) {
					return map[string]any{"answer": "done: " + rc.Goal}, nil
				},
				When: func(rc sdkplanner.RunContext) bool { return len(rc.Trajectory.Steps) > 0 },
			},
		),
	)
	if err != nil {
		t.Fatalf("NewDeterministicPlanner through the facade: %v", err)
	}
	return p
}

func phase112aQuad(tenant, user, session, run string) sdkidentity.Quadruple {
	return sdkidentity.Quadruple{
		Identity: sdkidentity.Identity{TenantID: tenant, UserID: user, SessionID: session},
		RunID:    run,
	}
}

// phase112aRunGoal drives one goal through the assembled stack via
// facade types only and returns the canonical AnswerEnvelope.
func phase112aRunGoal(ctx context.Context, stack *sdkassemble.Stack, q sdkidentity.Quadruple, goal string) (sdkplanner.AnswerEnvelope, error) {
	traj := &sdkplanner.Trajectory{Query: goal}
	fin, err := stack.RunLoop.Run(ctx, sdksteering.RunSpec{
		Planner: stack.Planner,
		Base: sdkplanner.RunContext{
			Quadruple:      q,
			Query:          goal,
			Goal:           goal,
			Trajectory:     traj,
			RepairCounters: &sdkplanner.RepairCounters{},
			Catalog: sdktools.NewPlannerView(stack.Catalog, sdktools.CatalogFilter{
				TenantID:  q.TenantID,
				UserID:    q.UserID,
				SessionID: q.SessionID,
			}),
		},
		ToolExecutor: stack.Executor,
		MaxSteps:     stack.Cfg.Planner.MaxSteps,
	})
	if err != nil {
		return sdkplanner.AnswerEnvelope{}, err
	}
	return sdkplanner.AnswerEnvelope{
		Answer:        sdkrunctx.ExtractAssistantAnswer(fin),
		FinishReason:  string(fin.Reason),
		ToolCallsSeen: len(traj.Steps),
	}, nil
}

// phase112aMarkerPayload is a redactor-visible marker payload proving
// the Sealed alias satisfies EventPayload from outside the module.
type phase112aMarkerPayload struct {
	sdkevents.Sealed
	Note string
}

// TestE2E_Phase112a_FacadeHeadlessAssembly — the headless recipe
// expressed exclusively through sdk/ imports, with identity
// propagation across two sessions, the fail-closed missing-identity
// gate, and a small concurrent slice against the one shared stack.
func TestE2E_Phase112a_FacadeHeadlessAssembly(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	ctx := context.Background()

	stack, err := sdkassemble.Assemble(ctx, phase112aCfg(t), sdkassemble.Options{
		PlannerOverride: phase112aPlanner(t),
	})
	if err != nil {
		if stack != nil {
			_ = stack.Close(ctx)
		}
		t.Fatalf("assemble through the facade: %v", err)
	}

	// Alias identity by construction: the Stack's internal-typed
	// fields flow into sdk-typed slots (an alias IS the type).
	facade := struct {
		Bus       sdkevents.EventBus
		State     sdkstate.StateStore
		Artifacts sdkartifacts.ArtifactStore
		Tasks     sdktasks.TaskRegistry
		LLM       sdkllm.LLMClient
		Memory    sdkmemory.MemoryStore
		Skills    sdkskills.SkillStore
		Executor  sdksteering.ToolExecutor
		Planner   sdkplanner.Planner
	}{
		Bus:       stack.Bus,
		State:     stack.State,
		Artifacts: stack.Artifacts,
		Tasks:     stack.Tasks,
		LLM:       stack.LLM,
		Memory:    stack.Memory,
		Skills:    stack.Skills,
		Executor:  stack.Executor,
		Planner:   stack.Planner,
	}
	for i, v := range []any{
		facade.Bus, facade.State, facade.Artifacts, facade.Tasks, facade.LLM,
		facade.Memory, facade.Skills, facade.Executor, facade.Planner,
	} {
		if v == nil {
			t.Fatalf("stack field %d is nil — the facade-assembled stack is incomplete", i)
		}
	}
	bus := facade.Bus

	// Register the in-proc tool through the facade driver; record the
	// goals it sees so the dispatch path is provably exercised.
	var seenGoals sync.Map
	if err := sdkinproc.RegisterFunc(stack.Catalog, "sdk_echo",
		func(_ context.Context, in phase112aEchoArgs) (phase112aEchoOut, error) {
			seenGoals.Store(in.Text, true)
			return phase112aEchoOut{Echo: "echo: " + in.Text}, nil
		},
		sdktools.WithDescription("Phase 112a facade echo tool."),
		sdktools.WithSideEffect(sdktools.SideEffectPure),
	); err != nil {
		t.Fatalf("inproc.RegisterFunc through the facade: %v", err)
	}

	qA := phase112aQuad("tenant-a", "user-a", "session-a", "run-a")
	qB := phase112aQuad("tenant-b", "user-b", "session-b", "run-b")

	// Subscribe with B's identity filter BEFORE the runs.
	subB, err := bus.Subscribe(ctx, sdkevents.Filter{
		Tenant: qB.TenantID, User: qB.UserID, Session: qB.SessionID,
	})
	if err != nil {
		t.Fatalf("Subscribe(B) through the facade: %v", err)
	}
	defer subB.Cancel()

	envA, err := phase112aRunGoal(ctx, stack, qA, "facade goal for tenant A")
	if err != nil {
		t.Fatalf("run A: %v", err)
	}
	envB, err := phase112aRunGoal(ctx, stack, qB, "facade goal for tenant B")
	if err != nil {
		t.Fatalf("run B: %v", err)
	}
	for _, tc := range []struct {
		env  sdkplanner.AnswerEnvelope
		goal string
	}{
		{envA, "facade goal for tenant A"},
		{envB, "facade goal for tenant B"},
	} {
		if tc.env.FinishReason != string(sdkplanner.FinishGoal) {
			t.Errorf("FinishReason = %q, want %q", tc.env.FinishReason, sdkplanner.FinishGoal)
		}
		if !strings.Contains(tc.env.Answer, tc.goal) {
			t.Errorf("answer did not flow from the run's own goal: %q", tc.env.Answer)
		}
		if tc.env.ToolCallsSeen == 0 {
			t.Errorf("expected ≥1 trajectory step (the CallTool dispatch), got 0")
		}
		if _, ok := seenGoals.Load(tc.goal); !ok {
			t.Errorf("the sdk_echo tool never saw goal %q — CallTool did not dispatch", tc.goal)
		}
	}

	// Bus-layer isolation: publish one marker per identity; B's
	// subscription must deliver B's marker and nothing off-identity.
	for _, q := range []sdkidentity.Quadruple{qA, qB} {
		if err := bus.Publish(ctx, sdkevents.Event{
			Type:     sdkevents.EventTypeRuntimeWarning,
			Identity: q,
			Payload:  phase112aMarkerPayload{Note: "phase112a-marker"},
		}); err != nil {
			t.Fatalf("Publish marker for %s: %v", q.TenantID, err)
		}
	}
	sawB := false
	drain := time.After(2 * time.Second)
drainLoop:
	for {
		select {
		case ev, ok := <-subB.Events():
			if !ok {
				t.Fatalf("subscription closed unexpectedly")
			}
			if ev.Identity.TenantID != qB.TenantID || ev.Identity.SessionID != qB.SessionID {
				t.Fatalf("identity leak on the bus: B's filter delivered %+v", ev.Identity)
			}
			if ev.Type == sdkevents.EventTypeRuntimeWarning {
				sawB = true
				break drainLoop
			}
		case <-drain:
			break drainLoop
		}
	}
	if !sawB {
		t.Errorf("B's subscription never delivered B's marker event")
	}

	// Missing identity fails closed (§6 rule 9) through the facade.
	if _, err := phase112aRunGoal(ctx, stack, sdkidentity.Quadruple{RunID: "run-anon"}, "anonymous"); err == nil {
		t.Fatalf("RunLoop.Run with an incomplete identity must fail closed")
	}

	// A small concurrent slice against the ONE shared stack (the
	// D-025 paths are already gated by the 110d stress; this pins the
	// same guarantee through the facade types).
	const n = 25
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			goal := fmt.Sprintf("facade concurrent goal %03d", i)
			q := phase112aQuad("tenant-c", fmt.Sprintf("user-%d", i%5), fmt.Sprintf("session-%d", i), fmt.Sprintf("run-%d", i))
			env, runErr := phase112aRunGoal(ctx, stack, q, goal)
			if runErr != nil {
				errs <- fmt.Errorf("run %d: %w", i, runErr)
				return
			}
			if !strings.Contains(env.Answer, goal) {
				errs <- fmt.Errorf("run %d: context bleed — answer %q does not carry its own goal", i, env.Answer)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if err := stack.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
	settleGoroutineBaseline(t, baseline)
}

// TestE2E_Phase112a_MissingLLMConfigFailsLoud — the facade's
// fail-loud misconfiguration path: a Defaults() config with no LLM
// provider/model/key must fail ValidateCore with an error that names
// the missing key (CLAUDE.md §13 — no silent stub fallback).
func TestE2E_Phase112a_MissingLLMConfigFailsLoud(t *testing.T) {
	cfg := sdkconfig.Defaults()
	err := cfg.ValidateCore()
	if err == nil {
		t.Fatalf("ValidateCore on a provider-less Defaults() config must fail loud")
	}
	if !strings.Contains(err.Error(), "config.llm.provider") {
		t.Errorf("the failure must name the missing config key, got %q", err.Error())
	}
}

// TestPhase112a_ProdParity_RegistriesSeated — blank-importing
// sdk/drivers/prod seats the SAME production driver registrations the
// internal aggregator seats, observed through each facade's
// RegisteredDrivers. (Structural parity — the sdk aggregator's only
// content is the internal aggregator import — is grep-asserted by
// scripts/smoke/phase-112a.sh.)
func TestPhase112a_ProdParity_RegistriesSeated(t *testing.T) {
	want := map[string][]string{
		"state":      {sdkstate.DefaultDriver, "sqlite", "postgres"},
		"memory":     {sdkmemory.DefaultDriver, "sqlite", "postgres"},
		"artifacts":  {sdkartifacts.DefaultDriver, "fs", "sqlite", "postgres", "s3"},
		"events":     {sdkevents.DefaultDriver, "durable"},
		"llm":        {sdkllm.DefaultDriver},
		"skills":     {sdkskills.DefaultDriver},
		"tasks":      {sdktasks.DefaultDriver},
		"planner":    {sdkreact.DriverName},
		"embeddings": {sdkembeddings.DefaultDriver},
	}
	got := map[string][]string{
		"state":      sdkstate.RegisteredDrivers(),
		"memory":     sdkmemory.RegisteredDrivers(),
		"artifacts":  sdkartifacts.RegisteredDrivers(),
		"events":     sdkevents.RegisteredDrivers(),
		"llm":        sdkllm.RegisteredDrivers(),
		"skills":     sdkskills.RegisteredDrivers(),
		"tasks":      sdktasks.RegisteredDrivers(),
		"planner":    sdkplanner.RegisteredDrivers(),
		"embeddings": sdkembeddings.RegisteredDrivers(),
	}
	for subsystem, names := range want {
		seated := make(map[string]bool, len(got[subsystem]))
		for _, n := range got[subsystem] {
			seated[n] = true
		}
		for _, n := range names {
			if !seated[n] {
				t.Errorf("%s: production driver %q not seated through sdk/drivers/prod (have %v)", subsystem, n, got[subsystem])
			}
		}
	}
	// The built-in tool names resolve through the facade too.
	if len(sdkbuiltin.KnownNames()) == 0 {
		t.Errorf("builtin.KnownNames through the facade is empty")
	}
}

// ---------------------------------------------------------------------------
// Compile coverage — every exported facade name referenced once, so a
// re-export that stops resolving breaks this build (the plan's ≥95%
// integrity-completeness bar; generic funcs are exercised above).
// ---------------------------------------------------------------------------
// Type re-exports — one zero-value reference per exported facade type.
var (
	_ sdkidentity.Identity
	_ sdkidentity.Quadruple
	_ sdkevents.EventBus
	_ sdkevents.Event
	_ sdkevents.EventID
	_ sdkevents.EventType
	_ sdkevents.EventPayload
	_ sdkevents.Sealed
	_ sdkevents.SafePayload
	_ sdkevents.SafeSealed
	_ sdkevents.Filter
	_ sdkevents.Subscription
	_ sdkevents.Cursor
	_ sdkevents.Replayer
	_ sdkevents.Deps
	_ sdkconfig.Config
	_ sdkconfig.LoadOption
	_ sdkconfig.A2APeerConfig
	_ sdkconfig.ArtifactsConfig
	_ sdkconfig.AuditConfig
	_ sdkconfig.CLIConfig
	_ sdkconfig.CustomToolConfig
	_ sdkconfig.DevHotReloadConfig
	_ sdkconfig.DistributedConfig
	_ sdkconfig.EventsConfig
	_ sdkconfig.GovernanceConfig
	_ sdkconfig.GovernanceRateLimitConfig
	_ sdkconfig.GovernanceTierConfig
	_ sdkconfig.IdentityConfig
	_ sdkconfig.LLMConfig
	_ sdkconfig.LLMCorrectionsConfig
	_ sdkconfig.LLMCorrectionsProfileConfig
	_ sdkconfig.LLMCostOverridesConfig
	_ sdkconfig.LLMCustomProviderConfig
	_ sdkconfig.LLMModelProfileConfig
	_ sdkconfig.LLMNetworkDefaults
	_ sdkconfig.MCPServerConfig
	_ sdkconfig.MemoryConfig
	_ sdkconfig.PauseResumeConfig
	_ sdkconfig.PlannerConfig
	_ sdkconfig.PlannerPlanningHintsCfg
	_ sdkconfig.ProjectedToolPolicy
	_ sdkconfig.ProtocolConfig
	_ sdkconfig.RuntimeConfig
	_ sdkconfig.ServerConfig
	_ sdkconfig.SessionsConfig
	_ sdkconfig.SkillsConfig
	_ sdkconfig.SkillsDirectoryConfig
	_ sdkconfig.StateConfig
	_ sdkconfig.TasksConfig
	_ sdkconfig.TelemetryConfig
	_ sdkconfig.ToolApprovalConfig
	_ sdkconfig.ToolEntryConfig
	_ sdkconfig.ToolOAuthConfig
	_ sdkconfig.ToolOAuthProviderConfig
	_ sdkconfig.ToolPolicyConfig
	_ sdkconfig.ToolsConfig
	_ sdktools.Tool
	_ sdktools.ToolCatalog
	_ sdktools.CatalogOption
	_ sdktools.CatalogFilter
	_ sdktools.ToolDescriptor
	_ sdktools.ToolExample
	_ sdktools.ToolResult
	_ sdktools.ToolPolicy
	_ sdktools.ToolProvider
	_ sdktools.ToolSourceID
	_ sdktools.DescriptorOption
	_ sdktools.LoadingMode
	_ sdktools.SideEffect
	_ sdktools.TransportKind
	_ sdktools.ValidateMode
	_ sdktools.PlannerView
	_ sdkbuiltin.RegistryContext
	_ sdkllm.LLMClient
	_ sdkllm.ConfigSnapshot
	_ sdkllm.Deps
	_ sdkllm.CompleteRequest
	_ sdkllm.CompleteResponse
	_ sdkllm.ChatMessage
	_ sdkllm.Content
	_ sdkllm.ContentPart
	_ sdkllm.PartType
	_ sdkllm.ImagePart
	_ sdkllm.AudioPart
	_ sdkllm.FilePart
	_ sdkllm.Role
	_ sdkllm.ToolDeclaration
	_ sdkllm.ToolCallStructured
	_ sdkllm.ResponseFormat
	_ sdkllm.ResponseFormatKind
	_ sdkllm.OutputMode
	_ sdkllm.Usage
	_ sdkllm.Cost
	_ sdkllm.ArtifactStub
	_ sdkllm.StubFetch
	_ sdkmemory.MemoryStore
	_ sdkmemory.ConfigSnapshot
	_ sdkmemory.Deps
	_ sdkmemory.Record
	_ sdkmemory.Snapshot
	_ sdkmemory.ConversationTurn
	_ sdkmemory.LLMContextPatch
	_ sdkmemory.TrajectoryDigest
	_ sdkmemory.Summarizer
	_ sdkmemory.SummarizeRequest
	_ sdkmemory.SummarizeResponse
	_ sdkmemory.Health
	_ sdkmemory.Strategy
	_ sdkmemory.OverflowPolicy
	_ sdkstate.StateStore
	_ sdkstate.StateRecord
	_ sdkstate.EventID
	_ sdkartifacts.ArtifactStore
	_ sdkartifacts.ArtifactRef
	_ sdkartifacts.ArtifactScope
	_ sdkartifacts.PutOpts
	_ sdkartifacts.ScopedArtifacts
	_ sdkartifacts.Presigner
	_ sdkskills.SkillStore
	_ sdkskills.ConfigSnapshot
	_ sdkskills.Deps
	_ sdkskills.Skill
	_ sdkskills.SkillView
	_ sdkskills.RankedSkill
	_ sdkskills.ListFilter
	_ sdkskills.Directory
	_ sdkskills.DirectoryConfig
	_ sdkskills.DirectoryCapability
	_ sdkskills.Origin
	_ sdkskills.Scope
	_ sdkskills.Selection
	_ sdkplanner.Planner
	_ sdkplanner.RunContext
	_ sdkplanner.Decision
	_ sdkplanner.Finish
	_ sdkplanner.FinishReason
	_ sdkplanner.CallTool
	_ sdkplanner.CallParallel
	_ sdkplanner.SpawnTask
	_ sdkplanner.AwaitTask
	_ sdkplanner.RequestPause
	_ sdkplanner.ToolCallDeferred
	_ sdkplanner.PauseReason
	_ sdkplanner.SpawnSpec
	_ sdkplanner.JoinSpec
	_ sdkplanner.JoinKind
	_ sdkplanner.Trajectory
	_ sdkplanner.Step
	_ sdkplanner.Summary
	_ sdkplanner.Source
	_ sdkplanner.StreamChunk
	_ sdkplanner.ChunkKind
	_ sdkplanner.SteeringInjection
	_ sdkplanner.ResumeHint
	_ sdkplanner.FailureRecord
	_ sdkplanner.BackgroundResult
	_ sdkplanner.BackgroundMemberOutcome
	_ sdkplanner.ToolContext
	_ sdkplanner.HandleID
	_ sdkplanner.HandleRegistry
	_ sdkplanner.ErrUnserializable
	_ sdkplanner.ErrToolContextLost
	_ sdkplanner.RepairCounters
	_ sdkplanner.ControlSignals
	_ sdkplanner.PlanningNudges
	_ sdkplanner.PlanningHints
	_ sdkplanner.Budget
	_ sdkplanner.BudgetHints
	_ sdkplanner.ParallelObservation
	_ sdkplanner.ParallelBranchObservation
	_ sdkplanner.MemoryView
	_ sdkplanner.MemoryBlocks
	_ sdkplanner.ToolCatalogView
	_ sdkplanner.SkillLookup
	_ sdkplanner.Skill
	_ sdkplanner.SkillResult
	_ sdkplanner.InputArtifactView
	_ sdkplanner.AnswerEnvelope
	_ sdkplanner.PlannerConfig
	_ sdkplanner.Factory
	_ sdkplanner.FactoryDeps
	_ sdkplanner.WakeMode
	_ sdkplanner.WakeAware
	_ sdkplanner.ReasoningReplayMode
	_ sdkplanner.CompressionRunner
	_ sdkplanner.CompressionOption
	_ sdkplanner.TokenEstimator
	_ sdkplanner.Summariser
	_ sdkreact.ReActPlanner
	_ sdkreact.Option
	_ sdkreact.PromptBuilder
	_ sdkdeterministic.DeterministicPlanner
	_ sdkdeterministic.Option
	_ sdkdeterministic.DecisionTreeStep
	_ sdkdeterministic.CallToolStep
	_ sdkdeterministic.FinishStep
	_ sdkdeterministic.PauseStep
	_ sdkdeterministic.SpawnAndAwaitStep
	_ sdkdeterministic.WatchGroupStep
	_ sdktasks.TaskRegistry
	_ sdktasks.Dependencies
	_ sdktasks.SpawnRequest
	_ sdktasks.SpawnToolRequest
	_ sdktasks.Task
	_ sdktasks.TaskHandle
	_ sdktasks.TaskID
	_ sdktasks.TaskKind
	_ sdktasks.TaskStatus
	_ sdktasks.TaskResult
	_ sdktasks.TaskError
	_ sdktasks.TaskFilter
	_ sdktasks.TaskSummary
	_ sdktasks.TaskGroup
	_ sdktasks.TaskGroupID
	_ sdktasks.TaskGroupStatus
	_ sdktasks.GroupRequest
	_ sdktasks.GroupCompletion
	_ sdktasks.MemberOutcome
	_ sdksteering.RunLoop
	_ sdksteering.RunSpec
	_ sdksteering.ToolExecutor
	_ sdksteering.Registry
	_ sdksteering.Option
	_ sdksteering.Clock
	_ sdksteering.Inbox
	_ sdksteering.ControlType
	_ sdksteering.ControlEvent
	_ sdksteering.AppliedControl
	_ sdksteering.Scope
	_ sdkdispatch.Option
	_ sdkassemble.Stack
	_ sdkassemble.Options
)

// Const + var (sentinel / forward) re-exports — one value reference each.
var _ = []any{
	sdkidentity.ErrIdentityMissing,
	sdkidentity.ErrIdentityIncomplete,
	sdkidentity.Validate,
	sdkidentity.With,
	sdkidentity.WithRun,
	sdkidentity.From,
	sdkidentity.MustFrom,
	sdkidentity.QuadrupleFrom,
	sdkidentity.MustQuadrupleFrom,
	sdkevents.DefaultDriver,
	sdkevents.EventTypeRuntimeError,
	sdkevents.EventTypeRuntimeWarning,
	sdkevents.EventTypeBusDropped,
	sdkevents.EventTypeBusSubscriptionIdleClosed,
	sdkevents.EventTypeAuditRedactionFailed,
	sdkevents.EventTypeAdminScopeUsed,
	sdkevents.EventTypeGovernanceBudgetExceeded,
	sdkevents.EventTypeGovernanceRateLimited,
	sdkevents.EventTypeRuntimeRunCancelled,
	sdkevents.EventTypeTopologyChanged,
	sdkevents.ErrUnknownEventType,
	sdkevents.ErrIdentityScopeRequired,
	sdkevents.ErrAdminScopeRequired,
	sdkevents.ErrSubscriberLimitReached,
	sdkevents.ErrBusClosed,
	sdkevents.ErrInvalidEvent,
	sdkevents.ErrIdentityRequired,
	sdkevents.ErrCursorTooOld,
	sdkevents.ErrReplayUnavailable,
	sdkevents.ErrUnknownDriver,
	sdkevents.Open,
	sdkevents.OpenWith,
	sdkevents.OpenDriver,
	sdkevents.RegisteredDrivers,
	sdkevents.RegisterEventType,
	sdkevents.IsValidEventType,
	sdkevents.EventTypes,
	sdkevents.ValidateEvent,
	sdkevents.WithBus,
	sdkevents.From,
	sdkevents.MustFrom,
	sdkconfig.ErrConfigInvalid,
	sdkconfig.ErrConfigNotFound,
	sdkconfig.Defaults,
	sdkconfig.Load,
	sdkconfig.LoadFromBytes,
	sdkconfig.WithOverrides,
	sdkconfig.WithLogger,
	sdkconfig.IsValidationError,
	sdktools.LoadingAlways,
	sdktools.LoadingDeferred,
	sdktools.SideEffectPure,
	sdktools.SideEffectRead,
	sdktools.SideEffectWrite,
	sdktools.SideEffectExternal,
	sdktools.SideEffectStateful,
	sdktools.TransportInProcess,
	sdktools.TransportHTTP,
	sdktools.TransportMCP,
	sdktools.TransportA2A,
	sdktools.TransportFlow,
	sdktools.ValidateNone,
	sdktools.ValidateBoth,
	sdktools.ValidateIn,
	sdktools.ValidateOut,
	sdktools.EventTypeToolInvoked,
	sdktools.EventTypeToolCompleted,
	sdktools.EventTypeToolFailed,
	sdktools.EventTypeToolInvalidArgs,
	sdktools.EventTypeToolPolicyExhausted,
	sdktools.ErrToolNotFound,
	sdktools.ErrToolInvalidArgs,
	sdktools.ErrToolPolicyExhausted,
	sdktools.ErrToolDuplicateName,
	sdktools.NewCatalog,
	sdktools.NewPlannerView,
	sdktools.DefaultPolicy,
	sdktools.VisibleNames,
	sdktools.WithCatalog,
	sdktools.Catalog,
	sdktools.MustCatalog,
	sdktools.WithAuthScopes,
	sdktools.WithBus,
	sdktools.WithCostHint,
	sdktools.WithDescription,
	sdktools.WithExamples,
	sdktools.WithLatencyHint,
	sdktools.WithLoading,
	sdktools.WithPolicy,
	sdktools.WithSafetyNotes,
	sdktools.WithSideEffect,
	sdktools.WithSource,
	sdktools.WithTags,
	sdkbuiltin.ErrUnknownBuiltIn,
	sdkbuiltin.ErrRegisterFailed,
	sdkbuiltin.ErrIdentityRequired,
	sdkinproc.ErrSchemaBuild,
	sdkinproc.ErrUnsupportedType,
	sdkbuiltin.RegisterWith,
	sdkbuiltin.KnownNames,
	sdkllm.DefaultDriver,
	sdkllm.PartText,
	sdkllm.PartImage,
	sdkllm.PartAudio,
	sdkllm.PartFile,
	sdkllm.RoleSystem,
	sdkllm.RoleUser,
	sdkllm.RoleAssistant,
	sdkllm.RoleTool,
	sdkllm.FormatText,
	sdkllm.FormatJSONObject,
	sdkllm.FormatJSONSchema,
	sdkllm.OutputModeUnset,
	sdkllm.OutputModeNative,
	sdkllm.OutputModeTools,
	sdkllm.OutputModePrompted,
	sdkllm.ErrUnknownDriver,
	sdkllm.ErrClientClosed,
	sdkllm.ErrIdentityMissing,
	sdkllm.ErrInvalidContent,
	sdkllm.ErrContextLeak,
	sdkllm.ErrContextWindowExceeded,
	sdkllm.ErrInvalidConfig,
	sdkllm.ErrUnsupportedModel,
	sdkllm.ErrInvalidJSONSchema,
	sdkllm.ErrDowngradeExhausted,
	sdkllm.ErrRetryExhausted,
	sdkllm.ErrValidationFailed,
	sdkllm.Open,
	sdkllm.SnapshotFromConfig,
	sdkllm.RegisteredDrivers,
	sdkmemory.DefaultDriver,
	sdkmemory.HealthHealthy,
	sdkmemory.HealthRetry,
	sdkmemory.HealthDegraded,
	sdkmemory.HealthRecovering,
	sdkmemory.StrategyNone,
	sdkmemory.StrategyTruncation,
	sdkmemory.StrategyRollingSummary,
	sdkmemory.OverflowDropOldest,
	sdkmemory.ErrNotFound,
	sdkmemory.ErrIdentityRequired,
	sdkmemory.ErrUnknownDriver,
	sdkmemory.ErrStoreClosed,
	sdkmemory.ErrInvalidSnapshot,
	sdkmemory.Open,
	sdkmemory.OpenDriver,
	sdkmemory.SnapshotFromConfig,
	sdkmemory.RegisteredDrivers,
	sdkmemory.WithStore,
	sdkmemory.From,
	sdkmemory.MustFrom,
	sdkstate.DefaultDriver,
	sdkstate.ErrNotFound,
	sdkstate.ErrIdempotencyConflict,
	sdkstate.ErrIdentityRequired,
	sdkstate.ErrStoreClosed,
	sdkstate.ErrInvalidRecord,
	sdkstate.ErrUnknownDriver,
	sdkstate.Open,
	sdkstate.OpenDriver,
	sdkstate.NewEventID,
	sdkstate.RegisteredDrivers,
	sdkstate.WithStore,
	sdkstate.From,
	sdkstate.MustFrom,
	sdkartifacts.DefaultDriver,
	sdkartifacts.ErrNotFound,
	sdkartifacts.ErrScopeMismatch,
	sdkartifacts.ErrIdentityRequired,
	sdkartifacts.ErrInvalidScope,
	sdkartifacts.ErrUnknownDriver,
	sdkartifacts.ErrStoreClosed,
	sdkartifacts.ErrPresignUnsupported,
	sdkartifacts.Open,
	sdkartifacts.OpenDriver,
	sdkartifacts.NewScoped,
	sdkartifacts.RegisteredDrivers,
	sdkskills.DefaultDriver,
	sdkskills.OriginPack,
	sdkskills.OriginGenerated,
	sdkskills.ScopeSession,
	sdkskills.ScopeProject,
	sdkskills.ScopeTenant,
	sdkskills.ScopeGlobal,
	sdkskills.SelectionPinnedThenRecent,
	sdkskills.SelectionPinnedThenTop,
	sdkskills.ErrSkillNotFound,
	sdkskills.ErrPackOverwriteRefused,
	sdkskills.ErrStoreClosed,
	sdkskills.ErrInvalidSkill,
	sdkskills.ErrUnknownDriver,
	sdkskills.ErrIdentityRequired,
	sdkskills.ErrInvalidConfig,
	sdkskills.Open,
	sdkskills.OpenDriver,
	sdkskills.SnapshotFromConfig,
	sdkskills.NewDirectory,
	sdkskills.DirectoryFromConfig,
	sdkskills.RegisteredDrivers,
	sdkplanner.FinishGoal,
	sdkplanner.FinishNoPath,
	sdkplanner.FinishCancelled,
	sdkplanner.FinishDeadlineExceeded,
	sdkplanner.FinishConstraintsConflict,
	sdkplanner.PauseApprovalRequired,
	sdkplanner.PauseAwaitInput,
	sdkplanner.PauseExternalEvent,
	sdkplanner.PauseConstraintsConflict,
	sdkplanner.JoinAll,
	sdkplanner.JoinFirstSuccess,
	sdkplanner.JoinKeyed,
	sdkplanner.JoinN,
	sdkplanner.WakePush,
	sdkplanner.WakePoll,
	sdkplanner.WakeHybrid,
	sdkplanner.TaskErrorCodeRunLoopError,
	sdkplanner.TaskErrorCodeCancelled,
	sdkplanner.ErrPlannerClosed,
	sdkplanner.ErrInvalidDecision,
	sdkplanner.ErrRepairExhausted,
	sdkplanner.ErrIdentityRequired,
	sdkplanner.ErrInvalidConfig,
	sdkplanner.ErrDeterministicStep,
	sdkplanner.ErrParallelCapExceeded,
	sdkplanner.ErrParallelInvalidJoin,
	sdkplanner.ErrMemoryBlockUnserializable,
	sdkplanner.ErrDriverUnknown,
	sdkplanner.ErrNilTrajectory,
	sdkplanner.ErrEmptySummary,
	sdkplanner.Resolve,
	sdkplanner.Register,
	sdkplanner.MustRegister,
	sdkplanner.RegisteredDrivers,
	sdkplanner.ConfigFromOperator,
	sdkplanner.HintsFromConfig,
	sdkplanner.IsValidFinishReason,
	sdkplanner.IsValidPauseReason,
	sdkplanner.TaskErrorCodeForFinish,
	sdkplanner.ResolveWakeMode,
	sdkplanner.NewCompressionRunner,
	sdkplanner.WithTokenEstimator,
	sdkplanner.DefaultTokenEstimator,
	sdkplanner.NewProcessLocalRegistry,
	sdkreact.DriverName,
	sdkreact.DefaultMaxSteps,
	sdkreact.DefaultSystemPrompt,
	sdkreact.FinishToolName,
	sdkreact.SpawnTaskToolName,
	sdkreact.AwaitTaskToolName,
	sdkreact.DeclarativeActionToolName,
	sdkreact.New,
	sdkreact.WithArgFillEnabled,
	sdkreact.WithMaxConsecutiveArgFailures,
	sdkreact.WithMaxSteps,
	sdkreact.WithMaxToolExamplesPerTool,
	sdkreact.WithParallelToolCalls,
	sdkreact.WithPromptBuilder,
	sdkreact.WithReasoningReplay,
	sdkreact.WithRepairAttempts,
	sdkreact.WithSystemPrompt,
	sdkreact.WithSystemPromptExtra,
	sdkdeterministic.DefaultName,
	sdkdeterministic.NewDeterministicPlanner,
	sdkdeterministic.WithName,
	sdkdeterministic.WithRegistry,
	sdkdeterministic.WithSteps,
	sdktasks.DefaultDriver,
	sdktasks.KindForeground,
	sdktasks.KindBackground,
	sdktasks.StatusPending,
	sdktasks.StatusRunning,
	sdktasks.StatusPaused,
	sdktasks.StatusComplete,
	sdktasks.StatusFailed,
	sdktasks.StatusCancelled,
	sdktasks.GroupOpen,
	sdktasks.GroupSealed,
	sdktasks.GroupCompleted,
	sdktasks.GroupCancelled,
	sdktasks.PropagateCascade,
	sdktasks.PropagateIsolate,
	sdktasks.ErrNotFound,
	sdktasks.ErrInvalidTransition,
	sdktasks.ErrIdempotencyConflict,
	sdktasks.ErrIdentityRequired,
	sdktasks.ErrUnknownDriver,
	sdktasks.ErrRegistryClosed,
	sdktasks.ErrInvalidRequest,
	sdktasks.ErrGroupNotFound,
	sdktasks.ErrGroupSealed,
	sdktasks.ErrGroupNotSealed,
	sdktasks.Open,
	sdktasks.OpenDriver,
	sdktasks.ValidateRequest,
	sdktasks.RegisteredDrivers,
	sdktasks.WithRegistry,
	sdktasks.From,
	sdktasks.MustFrom,
	sdksteering.DefaultMaxSteps,
	sdksteering.ControlInjectContext,
	sdksteering.ControlRedirect,
	sdksteering.ControlCancel,
	sdksteering.ControlPrioritize,
	sdksteering.ControlPause,
	sdksteering.ControlResume,
	sdksteering.ControlApprove,
	sdksteering.ControlReject,
	sdksteering.ControlUserMessage,
	sdksteering.ScopeSessionUser,
	sdksteering.ScopeOwnerUser,
	sdksteering.ScopeAdmin,
	sdksteering.ErrIdentityRequired,
	sdksteering.ErrUnknownControlType,
	sdksteering.ErrPayloadInvalid,
	sdksteering.ErrScopeMismatch,
	sdksteering.ErrInvalidScope,
	sdksteering.ErrInboxNotFound,
	sdksteering.ErrNoPlanner,
	sdksteering.ErrRunLoopMisconfigured,
	sdksteering.ErrNoOutstandingPause,
	sdksteering.ErrMaxStepsExceeded,
	sdksteering.ErrDecisionShapeUnsupported,
	sdksteering.NewRegistry,
	sdksteering.WithClock,
	sdksteering.IsValidControlType,
	sdksteering.ControlTypes,
	sdksteering.IsValidScope,
	sdksteering.RequiredScope,
	sdksteering.ValidatePayload,
	sdkdispatch.NewToolExecutor,
	sdkdispatch.WithHeavyThreshold,
	sdkdispatch.WithLogger,
	sdkdispatch.WithMaxSpawnDepth,
	sdkrunctx.ExtractAssistantAnswer,
	sdkrunctx.ProjectMemoryBlocks,
	sdkrunctx.ProjectSkillsContext,
	sdkrunctx.ProjectSkillsDirectory,
	sdkrunctx.ResolveInputArtifacts,
	sdkassemble.DefaultMCPIdentity,
	sdkassemble.Assemble,
}
