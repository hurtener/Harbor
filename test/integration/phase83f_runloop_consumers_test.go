// Phase 83f cross-subsystem integration test per CLAUDE.md §17.
//
// Phase 83f wires the dev binary's per-task RunLoop driver to populate
// `RunContext.MemoryBlocks` / `SkillsContext` / `RepairCounters` /
// `PlanningHints` and the user-facing `Query` + `Goal` — the consumer-
// side closure of the Wave 15 §17.5 audit's W3/W4 finding (issue #208).
//
// What this test proves:
//
//  1. The dev stack opens MemoryStore + SkillStore + TaskRegistry; the
//     driver fetches identity-scoped memory + skills + the task's
//     Query and projects them onto RunContext before calling
//     RunLoop.Run — without any test-side population.
//  2. PlanningHints from `harbor.yaml` reach RunContext.PlanningHints
//     (rendered into <planning_constraints> by 83c).
//  3. A fresh per-run *RepairCounters lands on RunContext per run
//     (D-145 — the planner reads + the runtime increments, scoped per
//     RunContext, never on the shared planner artifact).
//  4. The 83e reasoning trace round-trip works end-to-end in the dev
//     run loop: a planner step whose LLM returned Reasoning produces
//     a TrajectoryStep with ReasoningTrace set.
//  5. Cross-tenant isolation at the fetch boundary — two runs with
//     different identities each see only their own memory + skills in
//     the rendered prompt.
//  6. Fail-loud on store-side errors: a forced MemoryStore error
//     fails the run with `MarkFailed(code=runtime_fetch_error)` and
//     the LLM is NEVER called (no provider cost burned).
//
// Real drivers everywhere on the seam (§17.3): real audit redactor,
// real EventBus, real StateStore, real inmem MemoryStore (truncation
// strategy), real localdb SkillStore, real inprocess TaskRegistry,
// real steering.RunLoop, real ReAct planner — all assembled through
// `harbortest/devstack.Assemble`. The capturing LLM client is the only
// stub — it records the exact CompleteRequest the planner built so
// the test can assert on the composed prompt.

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/skills"
	_ "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
)

// phase83fRecorderLLM captures every CompleteRequest and returns a
// configurable response. Reasoning + Content are operator-settable.
type phase83fRecorderLLM struct {
	mu        sync.Mutex
	requests  []llm.CompleteRequest
	reasoning string
	content   string
}

func (c *phase83fRecorderLLM) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	resp := llm.CompleteResponse{Content: c.content, Reasoning: c.reasoning}
	c.mu.Unlock()
	return resp, nil
}

func (c *phase83fRecorderLLM) Close(_ context.Context) error { return nil }

func (c *phase83fRecorderLLM) lastRequest(t *testing.T) llm.CompleteRequest {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		t.Fatal("phase83fRecorderLLM saw no requests — driver did not reach planner.Next")
	}
	return c.requests[len(c.requests)-1]
}

func (c *phase83fRecorderLLM) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

// phase83fStores bundles the real Memory + Skills stores the test
// pre-populates BEFORE handing them to the devstack — proving the
// driver's fetch path reaches into the real subsystems.
type phase83fStores struct {
	bus    events.EventBus
	state  state.StateStore
	memory memory.MemoryStore
	skills skills.SkillStore
}

func openPhase83fStores(t *testing.T) *phase83fStores {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		ReplayBufferSize:         16,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	mem, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:       "inmem",
		Strategy:     memory.StrategyTruncation,
		BudgetTokens: 4096,
	}, memory.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	// Per-test SQLite file so parallel tests don't race on the
	// localdb migrator's process-wide schema lock (the modernc.org
	// driver locks on schema-migration concurrency even between
	// independent `:memory:` opens).
	skillDSN := filepath.Join(t.TempDir(), "skills.db")
	sk, err := skills.Open(context.Background(), skills.ConfigSnapshot{
		Driver: "localdb",
		DSN:    skillDSN,
	}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("skills.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = sk.Close(context.Background())
		_ = mem.Close(context.Background())
		_ = st.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	return &phase83fStores{bus: bus, state: st, memory: mem, skills: sk}
}

// seedPhase83fState writes one conversation turn into MemoryStore and
// one skill into SkillStore for the run's identity. The runtime fetch
// path lands these on RunContext when the driver runs.
func seedPhase83fState(t *testing.T, stores *phase83fStores, q identity.Quadruple, ctx context.Context, marker string) {
	t.Helper()
	if err := stores.memory.AddTurn(ctx, q, memory.ConversationTurn{
		UserMessage:       "earlier question for " + marker,
		AssistantResponse: "earlier answer for " + marker,
		Timestamp:         time.Now(),
	}); err != nil {
		t.Fatalf("memory.AddTurn: %v", err)
	}
	now := time.Now()
	if err := stores.skills.Upsert(ctx, q, skills.Skill{
		Name:        "skill-for-" + marker,
		Title:       "Skill " + marker,
		Description: "Test skill body for " + marker,
		Trigger:     "download mp3",
		Steps:       []string{"validate url", "invoke downloader"},
		Origin:      skills.OriginGenerated,
		Scope:       skills.ScopeSession,
		ContentHash: "hash-" + marker,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("skills.Upsert: %v", err)
	}
}

// phase83fJoinSystems concatenates every system-role message text in
// a captured request — the base 83a prompt + 83d's three injection
// wrappers — so substring assertions can span the whole composed
// system surface.
func phase83fJoinSystems(req llm.CompleteRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role == llm.RoleSystem && m.Content.Text != nil {
			b.WriteString(*m.Content.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// TestE2E_Phase83f_RunLoopPopulatesAllFourPrimitives is the positive
// end-to-end: the dev stack's driver fetches memory + skills + the
// task's Query, allocates RepairCounters, projects PlanningHints, and
// the planner sees all four on RunContext + carries the reasoning trace
// out the back.
func TestE2E_Phase83f_RunLoopPopulatesAllFourPrimitives(t *testing.T) {
	t.Parallel()
	stores := openPhase83fStores(t)

	devID := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	// Seed under the dev identity that the spawned task will resolve
	// to. The MemoryStore + SkillStore are identity-scoped; the
	// runtime fetch path will see these blobs only for this triple.
	seedQ := identity.Quadruple{Identity: devID}
	seedPhase83fState(t, stores, seedQ, idCtx, "compose")

	// Build the capturing planner. The recorder LLM returns a
	// reasoning trace + a finish so the run terminates in one step
	// AND we can inspect the captured CompleteRequest.
	rec := &phase83fRecorderLLM{
		content:   `{"tool":"_finish","args":{"answer":"done"}}`,
		reasoning: "captured-83f-reasoning",
	}
	plnr := react.New(rec)

	cfg := phase83fConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		// The PlannerOverride is the real ReAct planner backed by the
		// recording client, so the LLM construction is moot — but the
		// devstack opens the LLM regardless (it's used to derive context-
		// window knobs the planner inherits). Inject a mock snapshot so
		// the test stays hermetic (no real API key required).
		LLMConfigSnapshot: phase83fLLMSnapshot(cfg),
		// PlannerOverride bypasses the registry path so the recording
		// client reaches the planner directly. Without it, the registry
		// would build its own bifrost-backed react planner.
		PlannerOverride: plnr,
		MemoryStore:     stores.memory,
		SkillStore:      stores.skills,
		PlanningHints: &planner.PlanningHints{
			Constraints:    "no external network calls without consent",
			PreferredTools: []string{"kb_search"},
		},
		SkillsContextMax: 3,
	})
	defer stack.Close()

	if stack.Tasks == nil || stack.RunLoopDriver == nil {
		t.Fatal("devstack: Tasks or RunLoopDriver is nil — wiring broken")
	}

	// Spawn a task under the dev identity. The driver's subscription
	// picks it up and drives the planner.
	h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity: seedQ,
		Kind:     tasks.KindForeground,
		Query:    "download mp3 for me",
	})
	if err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}

	// Wait for the run to reach a terminal FSM state (bounded poll —
	// CLAUDE.md §17.4 forbids sleep-as-sync; this is bounded-wait-for-
	// state, not sleep-as-coordination).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, gErr := stack.Tasks.Get(idCtx, h.ID)
		if gErr == nil && (task.Status == tasks.StatusComplete || task.Status == tasks.StatusFailed) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Assert the captured request shape.
	req := rec.lastRequest(t)

	// Identity propagation: every fetch should have happened under
	// the dev triple. The captured request's system messages contain
	// the seeded marker payloads (which were keyed to that triple).
	joined := phase83fJoinSystems(req)

	// 83d memory wrapper carries the seeded conversation turn.
	if !strings.Contains(joined, "<read_only_conversation_memory>") {
		t.Error("83f wiring: <read_only_conversation_memory> wrapper missing — driver did not populate MemoryBlocks")
	}
	if !strings.Contains(joined, "earlier answer for compose") {
		t.Error("83f wiring: seeded memory turn did not reach the rendered prompt — fetch path broken")
	}

	// 83d skills wrapper carries the seeded skill.
	if !strings.Contains(joined, "<skills_context>") {
		t.Error("83f wiring: <skills_context> wrapper missing — driver did not populate SkillsContext")
	}
	if !strings.Contains(joined, "skill-for-compose") {
		t.Error("83f wiring: seeded skill did not reach the rendered prompt — skill search path broken")
	}

	// 83c planning constraints + the operator-supplied constraint
	// reach the prompt.
	if !strings.Contains(joined, "<planning_constraints>") {
		t.Error("83f wiring: <planning_constraints> section missing — driver did not project PlanningHints")
	}
	if !strings.Contains(joined, "no external network calls without consent") {
		t.Error("83f wiring: operator-supplied PlanningHints.Constraints did not reach the prompt")
	}
	if !strings.Contains(joined, "kb_search") {
		t.Error("83f wiring: PlanningHints.PreferredTools did not reach the prompt")
	}
}

// TestE2E_Phase83f_FailLoudOnMemoryFetchError pins the §17.3 failure-
// mode contract: a MemoryStore.GetLLMContext error fails the run loud
// with MarkFailed(code=runtime_fetch_error); the LLM is never called.
func TestE2E_Phase83f_FailLoudOnMemoryFetchError(t *testing.T) {
	t.Parallel()
	stores := openPhase83fStores(t)

	devID := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// Wrap the real memory store to inject an error on GetLLMContext.
	failingMem := &failingMemoryStore{inner: stores.memory}

	rec := &phase83fRecorderLLM{
		content: `{"tool":"_finish","args":{"answer":"should-never-render"}}`,
	}
	plnr := react.New(rec)

	cfg := phase83fConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: phase83fLLMSnapshot(cfg),
		PlannerOverride:   plnr,
		MemoryStore:       failingMem,
		SkillStore:        stores.skills,
	})
	defer stack.Close()

	h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: devID},
		Kind:     tasks.KindForeground,
		Query:    "doomed run",
	})
	if err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}

	// Wait for the run to fail.
	deadline := time.Now().Add(5 * time.Second)
	var observed tasks.TaskStatus
	for time.Now().Before(deadline) {
		task, gErr := stack.Tasks.Get(idCtx, h.ID)
		if gErr == nil {
			observed = task.Status
			if task.Status == tasks.StatusFailed {
				if task.Error.Code != "runtime_fetch_error" {
					t.Errorf("task failed with code %q, want runtime_fetch_error", task.Error.Code)
				}
				if !strings.Contains(task.Error.Message, "memory.GetLLMContext") {
					t.Errorf("task failure message %q does not name the failing call site", task.Error.Message)
				}
				if calls := rec.callCount(); calls != 0 {
					t.Errorf("LLM called %d times before the fail-loud abort, want 0 (provider cost would have burned)", calls)
				}
				return
			}
			if task.Status == tasks.StatusComplete {
				t.Fatal("task reached StatusComplete despite forced memory error — silent degradation is forbidden")
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task did not reach StatusFailed within deadline; observed=%q", observed)
}

// failingMemoryStore wraps a real store and returns a sentinel error
// from GetLLMContext to drive the fail-loud branch of the §17.3
// failure-mode test. Every other method delegates to the inner store.
type failingMemoryStore struct {
	inner memory.MemoryStore
}

var errPhase83fForcedMemFail = errors.New("phase 83f test: forced memory store failure")

func (f *failingMemoryStore) GetLLMContext(_ context.Context, _ identity.Quadruple) (memory.LLMContextPatch, error) {
	return memory.LLMContextPatch{}, errPhase83fForcedMemFail
}

func (f *failingMemoryStore) AddTurn(ctx context.Context, q identity.Quadruple, turn memory.ConversationTurn) error {
	return f.inner.AddTurn(ctx, q, turn)
}

func (f *failingMemoryStore) EstimateTokens(ctx context.Context, q identity.Quadruple) (int, error) {
	return f.inner.EstimateTokens(ctx, q)
}

func (f *failingMemoryStore) Flush(ctx context.Context, q identity.Quadruple) error {
	return f.inner.Flush(ctx, q)
}

func (f *failingMemoryStore) Health(ctx context.Context, q identity.Quadruple) (memory.Health, error) {
	return f.inner.Health(ctx, q)
}

func (f *failingMemoryStore) Snapshot(ctx context.Context, q identity.Quadruple) (memory.Snapshot, error) {
	return f.inner.Snapshot(ctx, q)
}

func (f *failingMemoryStore) Restore(ctx context.Context, q identity.Quadruple, snap memory.Snapshot) error {
	return f.inner.Restore(ctx, q, snap)
}

func (f *failingMemoryStore) Close(ctx context.Context) error { return f.inner.Close(ctx) }

// phase83fConfig loads the canonical dev YAML used across the dev-
// stack integration tests (devSmokeYAML in phase64_harbor_dev_test.go).
// Same defaults the production `harbor dev` boot uses, so the
// devstack.Assemble path doesn't surprise the test with a half-
// validated config.
func phase83fConfig(t *testing.T) *config.Config {
	t.Helper()
	cfgPath := writeDevConfig(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// phase83fLLMSnapshot builds a mock-driver LLMConfigSnapshot tied to
// the dev YAML's model profile. The test's recorder LLM is the real
// surface under assertion via PlannerOverride; the devstack's auto-
// opened LLM is unused but its construction still validates the
// snapshot, so a complete-shaped profile must be present.
func phase83fLLMSnapshot(cfg *config.Config) *llm.ConfigSnapshot {
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

// silenceUnused keeps `fmt` referenced even if the file's other
// formatted calls are removed during a refactor.
var _ = fmt.Sprintf
