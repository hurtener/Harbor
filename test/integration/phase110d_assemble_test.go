// phase110d_assemble_test.go — Phase 110d (D-197) wave-end coverage:
// the promoted assembly entry point exercised end-to-end on REAL
// drivers (CLAUDE.md §17.3 — no mocks at the seam; the mock LLM is
// the sanctioned D-089 dev driver, explicitly blank-imported).
//
// What this pins (also Wave B's §17.7 step-5 wave-end E2E):
//
//  1. The HEADLESS RECIPE PATH — docs/recipes/embed-harbor-headless.md
//     executed literally: config.Defaults → ValidateCore →
//     assemble.Assemble → drive ONE goal through the composed
//     planner/runloop/executor → build the planner.AnswerEnvelope →
//     Close. The recipe cannot ship as fiction (the 110d acceptance
//     gate).
//  2. Durable-event StateStore SHARING through the factory path
//     (events.OpenWith): events published on an assembled durable bus
//     survive a bus reopen against the SAME runtime StateStore.
//  3. Identity propagation at the store + event layers across ONE
//     shared stack, plus the missing-identity failure mode.
//  4. Failure modes: (a) a forced mid-assembly failure returns a
//     partial stack whose Close drains everything opened (goroutine
//     baseline restored); (b) a durable-events config with NO store at
//     all fails loud through OpenWith.
//  5. Concurrency: N≥10 concurrent Assemble/Close cycles (driver
//     registries are shared process state) and N≥100 concurrent runs
//     against ONE assembled stack (the stack is the compiled artifact;
//     D-025) — no cross-run bleed, baseline restored after Close.
package integration_test

import (
	"context"
	"fmt"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	// The production driver aggregator — the same import the recipe
	// tells a headless embedder to add (Phase 110c, D-196).
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	// Dev-only mock LLM (D-089) — explicit opt-in, never in the
	// aggregator.
	_ "github.com/hurtener/Harbor/internal/llm/mock"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tools"
)

// headlessRecipeCfg builds the recipe's config exactly as the recipe
// documents: config.Defaults() + the LLM block + ValidateCore. The
// driver is the mock (the recipe's "Mock LLM for CI smoke" variation).
func headlessRecipeCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.LLM.Driver = "mock"
	cfg.LLM.Model = "mock/echo"
	cfg.LLM.Timeout = 5 * time.Second
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"mock/echo": {
			ContextWindowTokens: 100000,
			TokenEstimator:      "chars_div_4",
		},
	}
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("ValidateCore: %v", err)
	}
	return cfg
}

// runRecipeGoal drives one goal through an assembled stack exactly as
// the recipe's step 4/5 documents and returns the AnswerEnvelope.
func runRecipeGoal(t *testing.T, ctx context.Context, stack *assemble.Stack, q identity.Quadruple, goal string) planner.AnswerEnvelope {
	t.Helper()
	traj := &planner.Trajectory{Query: goal}
	fin, err := stack.RunLoop.Run(ctx, steering.RunSpec{
		Planner: stack.Planner,
		Base: planner.RunContext{
			Quadruple:      q,
			Query:          goal,
			Goal:           goal,
			Trajectory:     traj,
			RepairCounters: &planner.RepairCounters{},
			Catalog: tools.NewPlannerView(stack.Catalog, tools.CatalogFilter{
				TenantID:  q.TenantID,
				UserID:    q.UserID,
				SessionID: q.SessionID,
			}),
		},
		ToolExecutor: stack.Executor,
		MaxSteps:     stack.Cfg.Planner.MaxSteps,
	})
	if err != nil {
		t.Fatalf("RunLoop.Run(%s): %v", q.RunID, err)
	}
	answer := ""
	if s, ok := fin.Payload.(string); ok {
		answer = s
	} else if m, ok := fin.Payload.(map[string]any); ok {
		if v, ok := m["answer"].(string); ok {
			answer = v
		}
	}
	return planner.AnswerEnvelope{
		Answer:        answer,
		FinishReason:  string(fin.Reason),
		ToolCallsSeen: planner.CountToolInvocations(traj),
	}
}

// phase110dMarkerPayload is a redactor-visible (non-SafePayload)
// event payload used as a deterministic replay anchor.
type phase110dMarkerPayload struct {
	events.Sealed
	Note string
}

func phase110dQuad(tenant, user, session, run string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: session},
		RunID:    run,
	}
}

// settleGoroutineBaseline polls until the goroutine count returns to
// (near) baseline — bounded real-time, no sleep-as-sync.
func settleGoroutineBaseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		goruntime.GC()
		if goruntime.NumGoroutine() <= baseline+3 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutine baseline not restored: started %d, now %d", baseline, goruntime.NumGoroutine())
}

// TestE2E_Phase110d_HeadlessRecipePath — the recipe, executed
// literally: Defaults → ValidateCore → prod import (file header) →
// Assemble → one goal through the composed planner/runloop/executor →
// AnswerEnvelope → Close.
func TestE2E_Phase110d_HeadlessRecipePath(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	ctx := context.Background()
	stack, err := assemble.Assemble(ctx, headlessRecipeCfg(t), assemble.Options{})
	if err != nil {
		if stack != nil {
			_ = stack.Close(ctx)
		}
		t.Fatalf("assemble: %v", err)
	}

	q := phase110dQuad("acme", "u-42", "s-1", "run-1")
	goal := "Summarise the latest deployment status."
	env := runRecipeGoal(t, ctx, stack, q, goal)
	if env.FinishReason != string(planner.FinishGoal) {
		t.Errorf("FinishReason = %q, want %q", env.FinishReason, planner.FinishGoal)
	}
	if env.Answer == "" || !strings.Contains(env.Answer, goal) {
		t.Errorf("answer did not flow from the run's own goal: %q", env.Answer)
	}

	if err := stack.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
	settleGoroutineBaseline(t, baseline)
}

// TestE2E_Phase110d_DurableEvents_SharedStateStore — the
// events.OpenWith acceptance: a durable-events config assembled via
// Assemble persists into the RUNTIME's StateStore, so a second bus
// opened over the same store (the restart shape, through the factory
// path) replays the first bus's events.
func TestE2E_Phase110d_DurableEvents_SharedStateStore(t *testing.T) {
	ctx := context.Background()
	cfg := headlessRecipeCfg(t)
	cfg.Events.Driver = "durable"
	cfg.Events.StateDriver = "" // no dedicated store — share the runtime's
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("ValidateCore: %v", err)
	}
	stack, err := assemble.Assemble(ctx, cfg, assemble.Options{})
	if err != nil {
		if stack != nil {
			_ = stack.Close(ctx)
		}
		t.Fatalf("assemble (durable events): %v", err)
	}
	defer func() { _ = stack.Close(ctx) }()

	// Drive a run (the recipe path works against the durable bus too),
	// then publish a marker event under the run's identity so the
	// replay assertion below has a deterministic anchor.
	q := phase110dQuad("acme", "u-1", "s-durable", "run-durable")
	_ = runRecipeGoal(t, ctx, stack, q, "durable goal")
	if err := stack.Bus.Publish(ctx, events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: q,
		Payload:  phase110dMarkerPayload{Note: "durable-marker"},
	}); err != nil {
		t.Fatalf("Publish marker: %v", err)
	}

	// Reopen a SECOND bus over the runtime's StateStore. If the
	// assembled bus had opened a private store (the pre-110d cmd-only
	// shape), this replay would be empty.
	bus2, err := events.OpenWith(ctx, cfg.Events, auditpatterns.New(), events.Deps{State: stack.State})
	if err != nil {
		t.Fatalf("OpenWith over the runtime store: %v", err)
	}
	defer func() { _ = bus2.Close(ctx) }()
	rp, ok := bus2.(events.Replayer)
	if !ok {
		t.Fatalf("durable bus must implement events.Replayer")
	}
	got, err := rp.Replay(ctx, events.Cursor{SessionID: q.SessionID},
		events.Filter{Tenant: q.TenantID, User: q.UserID, Session: q.SessionID})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("no events survived the bus reopen — the durable log did not share the runtime's StateStore")
	}

	// Failure mode (b): durable with NO store at all fails loud.
	noStore := cfg.Events
	if _, err := events.OpenWith(ctx, noStore, auditpatterns.New(), events.Deps{}); err == nil {
		t.Fatalf("durable with neither state_driver nor Deps.State must fail loud")
	}
}

// TestE2E_Phase110d_IdentityPropagation_AndMissingIdentity — two
// identities share ONE stack; each run's events stay inside its
// session filter (store + event layers), and an incomplete identity
// fails closed at the run-loop gate.
func TestE2E_Phase110d_IdentityPropagation_AndMissingIdentity(t *testing.T) {
	ctx := context.Background()
	stack, err := assemble.Assemble(ctx, headlessRecipeCfg(t), assemble.Options{})
	if err != nil {
		if stack != nil {
			_ = stack.Close(ctx)
		}
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = stack.Close(ctx) }()

	qA := phase110dQuad("tenant-a", "user-a", "session-a", "run-a")
	qB := phase110dQuad("tenant-b", "user-b", "session-b", "run-b")

	// Subscribe with B's filter BEFORE the runs so the bus-layer
	// isolation assertion sees the live window.
	subB, err := stack.Bus.Subscribe(ctx, events.Filter{
		Tenant: qB.TenantID, User: qB.UserID, Session: qB.SessionID,
	})
	if err != nil {
		t.Fatalf("Subscribe(B): %v", err)
	}
	defer subB.Cancel()

	envA := runRecipeGoal(t, ctx, stack, qA, "goal for tenant A")
	envB := runRecipeGoal(t, ctx, stack, qB, "goal for tenant B")
	if !strings.Contains(envA.Answer, "tenant A") || !strings.Contains(envB.Answer, "tenant B") {
		t.Errorf("cross-run answer bleed: A=%q B=%q", envA.Answer, envB.Answer)
	}

	// Event layer: drain whatever B's subscription buffered within a
	// bounded window; every event must carry B's triple.
	drain := time.After(500 * time.Millisecond)
	for {
		select {
		case ev, ok := <-subB.Events():
			if !ok {
				t.Fatalf("subscription closed unexpectedly")
			}
			if ev.Identity.TenantID != qB.TenantID || ev.Identity.SessionID != qB.SessionID {
				t.Fatalf("identity leak on the bus: B's filter delivered %v", ev.Identity)
			}
		case <-drain:
			goto drained
		}
	}
drained:

	// Missing identity fails closed (§6 rule 9): an empty triple never
	// reaches the planner.
	_, err = stack.RunLoop.Run(ctx, steering.RunSpec{
		Planner: stack.Planner,
		Base: planner.RunContext{
			Quadruple:      identity.Quadruple{RunID: "run-anon"},
			Query:          "anonymous",
			Goal:           "anonymous",
			Trajectory:     &planner.Trajectory{Query: "anonymous"},
			RepairCounters: &planner.RepairCounters{},
		},
		ToolExecutor: stack.Executor,
	})
	if err == nil {
		t.Fatalf("RunLoop.Run with an incomplete identity must fail closed")
	}
}

// TestE2E_Phase110d_ForcedMidAssemblyFailure_CleansUp — failure mode
// (a): a failure deep in the fan-out (unknown planner driver — every
// store/bus/llm layer has already opened) returns the partial stack;
// Close drains everything; goroutine baseline restored.
func TestE2E_Phase110d_ForcedMidAssemblyFailure_CleansUp(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	cfg := headlessRecipeCfg(t)
	cfg.Planner.Driver = "no-such-planner-110d"
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err == nil {
		_ = stack.Close(context.Background())
		t.Fatalf("expected the forced planner-stage failure")
	}
	if !strings.Contains(err.Error(), "planner:") {
		t.Errorf("error %v does not name the failing stage", err)
	}
	if stack == nil {
		t.Fatalf("expected a partial stack so the caller can drain")
	}
	if stack.Bus == nil || stack.State == nil || stack.LLM == nil {
		t.Errorf("the layers before the failure must be present on the partial stack")
	}
	if cErr := stack.Close(context.Background()); cErr != nil {
		t.Errorf("partial-stack Close: %v", cErr)
	}
	settleGoroutineBaseline(t, baseline)
}

// TestE2E_Phase110d_ConcurrentAssembleCloseCycles — N=10 concurrent
// Assemble+Close cycles against the shared process-wide driver
// registries: no cross-stack bleed (each stack runs its own goal and
// sees its own answer), no goroutine leak after all cycles return.
func TestE2E_Phase110d_ConcurrentAssembleCloseCycles(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			stack, err := assemble.Assemble(ctx, headlessRecipeCfg(t), assemble.Options{})
			if err != nil {
				if stack != nil {
					_ = stack.Close(ctx)
				}
				errs <- fmt.Errorf("cycle %d: assemble: %w", i, err)
				return
			}
			goal := fmt.Sprintf("cycle goal %d", i)
			q := phase110dQuad("acme", "u-cycle", fmt.Sprintf("s-cycle-%d", i), fmt.Sprintf("run-cycle-%d", i))
			env := runRecipeGoal(t, ctx, stack, q, goal)
			if !strings.Contains(env.Answer, goal) {
				errs <- fmt.Errorf("cycle %d: cross-stack bleed: %q", i, env.Answer)
			}
			if cErr := stack.Close(ctx); cErr != nil {
				errs <- fmt.Errorf("cycle %d: close: %w", i, cErr)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	settleGoroutineBaseline(t, baseline)
}

// TestE2E_Phase110d_ConcurrentRuns_OneSharedStack — D-025: the
// assembled Stack is the compiled artifact. N=100 concurrent runs
// against ONE stack, each under its own session/run identity; every
// answer must reflect its OWN goal (context-bleed gate), and the
// goroutine baseline is restored after Close.
func TestE2E_Phase110d_ConcurrentRuns_OneSharedStack(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	ctx := context.Background()
	stack, err := assemble.Assemble(ctx, headlessRecipeCfg(t), assemble.Options{})
	if err != nil {
		if stack != nil {
			_ = stack.Close(ctx)
		}
		t.Fatalf("assemble: %v", err)
	}

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			goal := fmt.Sprintf("distinct goal %04d", i)
			q := phase110dQuad("acme", fmt.Sprintf("u-%d", i%7), fmt.Sprintf("s-%d", i), fmt.Sprintf("run-%d", i))
			traj := &planner.Trajectory{Query: goal}
			fin, runErr := stack.RunLoop.Run(ctx, steering.RunSpec{
				Planner: stack.Planner,
				Base: planner.RunContext{
					Quadruple:      q,
					Query:          goal,
					Goal:           goal,
					Trajectory:     traj,
					RepairCounters: &planner.RepairCounters{},
					Catalog: tools.NewPlannerView(stack.Catalog, tools.CatalogFilter{
						TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID,
					}),
				},
				ToolExecutor: stack.Executor,
				MaxSteps:     stack.Cfg.Planner.MaxSteps,
			})
			if runErr != nil {
				errs <- fmt.Errorf("run %d: %w", i, runErr)
				return
			}
			answer, _ := fin.Payload.(string)
			if m, ok := fin.Payload.(map[string]any); ok {
				if v, ok := m["answer"].(string); ok {
					answer = v
				}
			}
			if !strings.Contains(answer, goal) {
				errs <- fmt.Errorf("run %d: context bleed — answer %q does not carry its own goal %q", i, answer, goal)
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
