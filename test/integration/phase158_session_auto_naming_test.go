// Package integration — Phase 158 (D-289): the session auto-naming E2E. A real
// devstack (real state → sessions.Registry, real events bus, real RunLoop, the
// real wrapped LLM client backed by a scripted OpenAI-compatible server, the
// real agent-config projection) drives a run to completion; the terminal-
// boundary trigger makes ONE governed Complete call over a bounded transcript
// digest and writes an auto title through the manual-safe registry path.
// Covers: title lands source=auto (enabled via yaml fleet default), manual-wins
// (a human title is never overwritten), the empty-output failure leg leaves the
// run outcome + title untouched, and identity is asserted on the write. Runs
// under -race.
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/tasks"
)

// phase158ConfigOpts parametrizes the devstack yaml the naming E2Es boot:
// governanceBlock (the identity-tier enforcement the governance leg composes)
// and namingBlock are appended top-level yaml blocks.
type phase158ConfigOpts struct {
	governanceBlock string
	namingBlock     string
}

// phase158Config writes a devstack yaml with the scripted LLM provider and the
// supplied `runtime.naming` block (corrections off, no governance — the
// baseline the happy-path legs boot).
func phase158Config(t *testing.T, serverURL string, namingBlock string) *config.Config {
	t.Helper()
	return phase158ConfigWith(t, serverURL, phase158ConfigOpts{namingBlock: namingBlock})
}

func phase158ConfigWith(t *testing.T, serverURL string, opts phase158ConfigOpts) *config.Config {
	t.Helper()
	const envKey = "HARBOR_TEST_158_FAKE_KEY"
	t.Setenv(envKey, "test-key-value")
	yaml := fmt.Sprintf(`
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 5s
identity:
  jwt_algorithms: [RS256, ES256]
  issuer: https://issuer.example.com
  audience: harbor-test-158
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: text
  log_level: error
  service_name: harbor-test-158
state:
  driver: inmem
llm:
  driver: bifrost
  provider: 158-fake
  model: %s
  timeout: 10s
  context_window_reserve: 0.05
  corrections:
    enabled: false
  custom_providers:
    - name: 158-fake
      base_url: %s
      api_key_env_var: %s
      models: [%s]
      timeout: 10s
      max_retries: 0
  model_profiles:
    %s:
      context_window_tokens: 8192
      token_estimator: chars_div_4
events:
  driver: inmem
  max_subscribers_per_session: 16
  subscriber_buffer_size: 256
  idle_timeout: 60s
  drop_window: 1s
  replay_buffer_size: 1024
sessions:
  idle_ttl: 24h
  hard_cap: 720h
  sweep_interval: 15m
artifacts:
  driver: inmem
  heavy_output_threshold_bytes: 32768
tasks:
  driver: inprocess
  retain_turn_timeout: 5m
  continuation_hop_limit: 8
distributed:
  bus_driver: loopback
  remote_driver: loopback
memory:
  driver: inmem
  strategy: none
planner:
  driver: react
  max_steps: 4
%s
%s
`, scriptedModel, serverURL, envKey, scriptedModel, scriptedModel,
		opts.governanceBlock, opts.namingBlock)
	dir := t.TempDir()
	p := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	cfg, err := config.Load(context.Background(), p)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func phase158DevID() identity.Identity {
	return identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
}

// runOneAndWait spawns a foreground task and waits for it to reach a terminal
// status, returning the status.
func phase158RunOnce(t *testing.T, stack *devstack.DevStack, idCtx context.Context, id identity.Identity, query string) tasks.TaskStatus {
	t.Helper()
	h, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id},
		Kind:     tasks.KindForeground,
		Query:    query,
	})
	if err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}
	return waitForTaskTerminal(t, stack, idCtx, h.ID, 10*time.Second)
}

// TestE2E_SessionAutoNaming_YamlDefault_TitleLandsAuto proves the opt-in fleet
// default enables auto-naming: a completed run titles the session via the
// scripted LLM, with source=auto, and the run outcome is unaffected.
func TestE2E_SessionAutoNaming_YamlDefault_TitleLandsAuto(t *testing.T) {
	server := newScriptedLLMServer(t,
		scriptedFinishResponse("Here is the answer to your question."), // the planner's Finish
		scriptedFinishResponse("Trip to Kyoto"),                        // the naming call's title
	)
	cfg := phase158Config(t, server.URL(), "runtime:\n  naming:\n    auto: true\n    after_turns: 1\n")
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()
	if stack.Sessions == nil {
		t.Fatal("devstack: Sessions is nil")
	}

	devID := phase158DevID()
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := stack.Sessions.EnsureOpen(idCtx, devID); err != nil {
		t.Fatalf("EnsureOpen: %v", err)
	}

	if status := phase158RunOnce(t, stack, idCtx, devID, "plan a trip to Kyoto"); status != tasks.StatusComplete {
		t.Fatalf("task status = %s, want Complete", status)
	}

	snap, err := stack.Sessions.Inspect(idCtx, devID.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.TitleSource != sessions.TitleSourceAuto {
		t.Fatalf("TitleSource = %q, want auto (session=%q title=%q)", snap.TitleSource, devID.SessionID, snap.Title)
	}
	if snap.Title != "Trip to Kyoto" {
		t.Errorf("Title = %q, want %q", snap.Title, "Trip to Kyoto")
	}
	// Identity assertion: the titled record belongs to the run's triple.
	if snap.Identity != devID {
		t.Errorf("titled record identity = %+v, want %+v", snap.Identity, devID)
	}
}

// TestE2E_SessionAutoNaming_ManualWins_ThenClearReArms proves (1) a human-set
// title is never overwritten — the auto path refuses the manual title (no LLM
// naming call, so the run's only scripted response is the planner's Finish) —
// and (2) CLEARING the manual title via the manual verb re-arms auto-naming:
// the next completed run names the session source=auto.
func TestE2E_SessionAutoNaming_ManualWins_ThenClearReArms(t *testing.T) {
	// Run 1 (manual title in place): planner Finish only — a naming call here
	// would exhaust the script early and fail loudly.
	// Run 2 (after the clear): planner Finish + the naming call's title.
	server := newScriptedLLMServer(t,
		scriptedFinishResponse("Here is the answer."),
		scriptedFinishResponse("Here is another answer."),
		scriptedFinishResponse("Re-Armed Title"),
	)
	cfg := phase158Config(t, server.URL(), "runtime:\n  naming:\n    auto: true\n    after_turns: 1\n")
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()

	devID := phase158DevID()
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := stack.Sessions.EnsureOpen(idCtx, devID); err != nil {
		t.Fatalf("EnsureOpen: %v", err)
	}
	if err := stack.Sessions.SetTitle(idCtx, devID.SessionID, devID, "Human Named This"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}

	if status := phase158RunOnce(t, stack, idCtx, devID, "do the thing"); status != tasks.StatusComplete {
		t.Fatalf("task status = %s, want Complete", status)
	}

	snap, err := stack.Sessions.Inspect(idCtx, devID.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.TitleSource != sessions.TitleSourceManual || snap.Title != "Human Named This" {
		t.Errorf("manual title was overwritten: %q/%q, want manual/Human Named This", snap.Title, snap.TitleSource)
	}
	if n := len(server.Requests()); n != 1 {
		t.Fatalf("LLM requests after manual-wins run = %d, want 1 (no naming call over a manual title)", n)
	}

	// Clear the manual title (the empty-clears semantics of the manual verb)
	// — auto-naming re-arms on the next completed run.
	if err := stack.Sessions.SetTitle(idCtx, devID.SessionID, devID, ""); err != nil {
		t.Fatalf("SetTitle (clear): %v", err)
	}
	if status := phase158RunOnce(t, stack, idCtx, devID, "do it again"); status != tasks.StatusComplete {
		t.Fatalf("post-clear task status = %s, want Complete", status)
	}
	snap, err = stack.Sessions.Inspect(idCtx, devID.SessionID)
	if err != nil {
		t.Fatalf("Inspect (post-clear): %v", err)
	}
	if snap.TitleSource != sessions.TitleSourceAuto || snap.Title != "Re-Armed Title" {
		t.Errorf("clear did not re-arm auto-naming: title = %q/%q, want auto/Re-Armed Title", snap.Title, snap.TitleSource)
	}
}

// TestE2E_SessionAutoNaming_EmptyOutput_RunUnaffected proves the empty-title
// failure leg: the scripted naming call returns an empty title, so no title is
// applied, yet the run completes normally (the failure never alters the
// outcome).
func TestE2E_SessionAutoNaming_EmptyOutput_RunUnaffected(t *testing.T) {
	server := newScriptedLLMServer(t,
		scriptedFinishResponse("Here is the answer."), // the planner's Finish
		scriptedFinishResponse("   "),                 // an unusable (whitespace) title
	)
	cfg := phase158Config(t, server.URL(), "runtime:\n  naming:\n    auto: true\n    after_turns: 1\n")
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()

	devID := phase158DevID()
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := stack.Sessions.EnsureOpen(idCtx, devID); err != nil {
		t.Fatalf("EnsureOpen: %v", err)
	}

	if status := phase158RunOnce(t, stack, idCtx, devID, "do the thing"); status != tasks.StatusComplete {
		t.Fatalf("task status = %s, want Complete (a naming failure must not fail the run)", status)
	}

	snap, err := stack.Sessions.Inspect(idCtx, devID.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.TitleSource != sessions.TitleSourceUnset || snap.Title != "" {
		t.Errorf("empty-output naming applied a title: %q/%q, want unset/empty", snap.Title, snap.TitleSource)
	}
}

// phase158AgentConfigService builds the real agent-config Protocol service
// over the devstack's SHARED registry, so a set_revision lands exactly where
// the run-start naming projection reads (the same verb path the Console
// takes, minus the HTTP handler).
func phase158AgentConfigService(t *testing.T, stack *devstack.DevStack) *agentcfgprotocol.Service {
	t.Helper()
	if stack.AgentConfig == nil || stack.AgentConfigID == "" {
		t.Fatal("devstack: AgentConfig registry / AgentConfigID not wired")
	}
	svc, err := agentcfgprotocol.NewService(stack.AgentConfig, agentcfgprotocol.WithBus(stack.Bus))
	if err != nil {
		t.Fatalf("agentcfgprotocol.NewService: %v", err)
	}
	return svc
}

func phase158Scope(id identity.Identity) prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
}

// TestE2E_SessionAutoNaming_SetRevisionEnable_OverridesYamlOff proves the
// agentcfg › yaml precedence through a REAL run: yaml carries no naming block
// (fleet default off); an agent_config.set_revision pinning `{auto: true}`
// enables auto-naming for the agent's next run, and the title lands
// source=auto.
func TestE2E_SessionAutoNaming_SetRevisionEnable_OverridesYamlOff(t *testing.T) {
	server := newScriptedLLMServer(t,
		scriptedFinishResponse("Here is the answer."),  // the planner's Finish
		scriptedFinishResponse("Enabled Via Revision"), // the naming call's title
	)
	cfg := phase158Config(t, server.URL(), "") // yaml: naming OFF
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()

	devID := phase158DevID()
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := stack.Sessions.EnsureOpen(idCtx, devID); err != nil {
		t.Fatalf("EnsureOpen: %v", err)
	}

	// Enable naming via the real set_revision verb path for the SAME agent id
	// the run-loop driver projects at run start.
	svc := phase158AgentConfigService(t, stack)
	if _, err := svc.SetRevision(context.Background(), prototypes.AgentConfigSetRevisionRequest{
		Identity: phase158Scope(devID), AgentID: stack.AgentConfigID,
		Payload: prototypes.AgentConfigPayload{Naming: &prototypes.AgentConfigNaming{Auto: true, AfterTurns: 1}},
	}); err != nil {
		t.Fatalf("set_revision (enable naming): %v", err)
	}

	if status := phase158RunOnce(t, stack, idCtx, devID, "do the thing"); status != tasks.StatusComplete {
		t.Fatalf("task status = %s, want Complete", status)
	}
	snap, err := stack.Sessions.Inspect(idCtx, devID.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.TitleSource != sessions.TitleSourceAuto || snap.Title != "Enabled Via Revision" {
		t.Errorf("title = %q/%q, want auto/Enabled Via Revision (set_revision enable did not reach the run)", snap.Title, snap.TitleSource)
	}
}

// TestE2E_SessionAutoNaming_BareAutoFalseRevision_OverridesYamlOn is THE M1
// footgun regression, end to end: yaml enables auto-naming fleet-wide; an
// agent_config.set_revision pinning a BARE `{auto: false}` section (no other
// field) opts the agent out. Before the normalization-preserve fix the bare
// section was silently dropped as "inert" and the agent kept auto-naming (and
// spending) after a 200 OK opt-out. Only the planner's Finish is scripted —
// a naming call would exhaust the script and fail loudly.
func TestE2E_SessionAutoNaming_BareAutoFalseRevision_OverridesYamlOn(t *testing.T) {
	server := newScriptedLLMServer(t,
		scriptedFinishResponse("Here is the answer."),
	)
	cfg := phase158Config(t, server.URL(), "runtime:\n  naming:\n    auto: true\n    after_turns: 1\n")
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()

	devID := phase158DevID()
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := stack.Sessions.EnsureOpen(idCtx, devID); err != nil {
		t.Fatalf("EnsureOpen: %v", err)
	}

	svc := phase158AgentConfigService(t, stack)
	if _, err := svc.SetRevision(context.Background(), prototypes.AgentConfigSetRevisionRequest{
		Identity: phase158Scope(devID), AgentID: stack.AgentConfigID,
		Payload: prototypes.AgentConfigPayload{Naming: &prototypes.AgentConfigNaming{Auto: false}},
	}); err != nil {
		t.Fatalf("set_revision (bare auto:false opt-out): %v", err)
	}

	if status := phase158RunOnce(t, stack, idCtx, devID, "do the thing"); status != tasks.StatusComplete {
		t.Fatalf("task status = %s, want Complete", status)
	}
	snap, err := stack.Sessions.Inspect(idCtx, devID.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.TitleSource != sessions.TitleSourceUnset || snap.Title != "" {
		t.Errorf("bare auto:false opt-out did not stick: title = %q/%q, want unset/empty", snap.Title, snap.TitleSource)
	}
	if n := len(server.Requests()); n != 1 {
		t.Errorf("LLM requests = %d, want exactly 1 (the planner call; an opted-out agent must make no naming call)", n)
	}
}

// TestE2E_SessionAutoNaming_GovernanceBlock_SkipsLoudly proves the governance
// leg through the REAL wrapped chain (governance.Wrap is composed by the
// assembly from the populated identity_tiers block, outermost in the LLM
// chain): a one-shot rate-limit bucket (capacity 1, no refill) lets the
// planner's call through and makes the NAMING call's PreCall block with
// ErrRateLimited — naming is skipped LOUDLY (`session.naming_failed` class
// governance_blocked, observed on the real bus) and the run's outcome is
// untouched. The rate limiter is used rather than the budget ceiling because
// a deterministic ceiling breach would need synthetic cost accounting (the
// scripted provider reports no cost; the corrections usage-backfill knob only
// fires on all-zero usage) — the rate limiter exercises the SAME PreCall gate
// on the SAME wrapped chain with exact one-call semantics.
func TestE2E_SessionAutoNaming_GovernanceBlock_SkipsLoudly(t *testing.T) {
	server := newScriptedLLMServer(t,
		scriptedFinishResponse("Here is the answer."), // the planner's Finish — the ONLY LLM call that lands
	)
	cfg := phase158ConfigWith(t, server.URL(), phase158ConfigOpts{
		governanceBlock: "governance:\n  repair_attempts: 1\n  default_tier: default\n  identity_tiers:\n" +
			"    default:\n      rate_limit:\n        capacity: 1\n",
		namingBlock: "runtime:\n  naming:\n    auto: true\n    after_turns: 1\n",
	})
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()

	devID := phase158DevID()
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := stack.Sessions.EnsureOpen(idCtx, devID); err != nil {
		t.Fatalf("EnsureOpen: %v", err)
	}

	// Subscribe for the naming-skip event BEFORE the run.
	sub, err := stack.Bus.Subscribe(context.Background(), events.Filter{
		Tenant: devID.TenantID, User: devID.UserID, Session: devID.SessionID,
		Types: []events.EventType{steering.EventTypeSessionNamingFailed},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// The RUN is untouched: the planner call drains the one-token bucket and
	// completes; the naming call is the one whose PreCall underflows.
	if status := phase158RunOnce(t, stack, idCtx, devID, "do the thing"); status != tasks.StatusComplete {
		t.Fatalf("task status = %s, want Complete (a governance-blocked naming call must not fail the run)", status)
	}

	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription closed before session.naming_failed arrived")
		}
		p, isP := ev.Payload.(steering.SessionNamingFailedPayload)
		if !isP {
			t.Fatalf("payload type = %T, want SessionNamingFailedPayload", ev.Payload)
		}
		if p.ErrorClass != "governance_blocked" {
			t.Errorf("naming_failed class = %q, want governance_blocked", p.ErrorClass)
		}
		if ev.Identity.TenantID != devID.TenantID || ev.Identity.SessionID != devID.SessionID {
			t.Errorf("event identity = %+v, want the run's triple", ev.Identity)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for session.naming_failed (governance_blocked)")
	}

	// No title landed; only the planner call reached the scripted server.
	snap, err := stack.Sessions.Inspect(idCtx, devID.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.TitleSource != sessions.TitleSourceUnset || snap.Title != "" {
		t.Errorf("governance-blocked naming still applied a title: %q/%q", snap.Title, snap.TitleSource)
	}
	if n := len(server.Requests()); n != 1 {
		t.Errorf("LLM requests = %d, want 1 (the blocked naming call must never reach the provider)", n)
	}
}

// TestE2E_SessionAutoNaming_RepeatCadence_CapHonored proves the re-naming
// cadence + cap across a REAL run sequence: after_turns=1, repeat_every=2,
// max_repetitions=2 → naming fires at turns 1 and 3, then STOPS at the cap.
// Five runs = five planner Finish responses + exactly two naming responses;
// a third naming call would exhaust the script and fail loudly.
func TestE2E_SessionAutoNaming_RepeatCadence_CapHonored(t *testing.T) {
	server := newScriptedLLMServer(t,
		scriptedFinishResponse("answer 1"), // run 1 (turn 1)
		scriptedFinishResponse("Title One"),
		scriptedFinishResponse("answer 2"), // run 2 (turn 2 — not due, 2 < 1+2)
		scriptedFinishResponse("answer 3"), // run 3 (turn 3 — due, 3 >= 1+2)
		scriptedFinishResponse("Title Two"),
		scriptedFinishResponse("answer 4"), // run 4 (cap reached — no naming)
		scriptedFinishResponse("answer 5"), // run 5 (cap reached — no naming)
	)
	cfg := phase158Config(t, server.URL(),
		"runtime:\n  naming:\n    auto: true\n    after_turns: 1\n    repeat_every: 2\n    max_repetitions: 2\n")
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()

	devID := phase158DevID()
	idCtx, err := identity.With(context.Background(), devID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := stack.Sessions.EnsureOpen(idCtx, devID); err != nil {
		t.Fatalf("EnsureOpen: %v", err)
	}

	for i := 1; i <= 5; i++ {
		if status := phase158RunOnce(t, stack, idCtx, devID, fmt.Sprintf("run %d", i)); status != tasks.StatusComplete {
			t.Fatalf("run %d status = %s, want Complete", i, status)
		}
	}

	snap, err := stack.Sessions.Inspect(idCtx, devID.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.TitleSource != sessions.TitleSourceAuto || snap.Title != "Title Two" {
		t.Errorf("final title = %q/%q, want auto/Title Two (the turn-3 re-name)", snap.Title, snap.TitleSource)
	}
	if snap.TurnCount != 5 || snap.AutoNameCount != 2 || snap.LastAutoNamedTurn != 3 {
		t.Errorf("counters = turn %d auto %d last %d, want 5/2/3", snap.TurnCount, snap.AutoNameCount, snap.LastAutoNamedTurn)
	}
	// Exactly 5 planner + 2 naming calls reached the scripted server.
	if n := len(server.Requests()); n != 7 {
		t.Errorf("LLM requests = %d, want 7 (5 planner + 2 naming — the cap stops the re-naming)", n)
	}
}
