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
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/tasks"
)

// phase158Config writes a devstack yaml with the scripted LLM provider and a
// `runtime.naming` fleet-default policy (opt-in enabled: auto, after_turns 1).
func phase158Config(t *testing.T, serverURL string, namingBlock string) *config.Config {
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
`, scriptedModel, serverURL, envKey, scriptedModel, scriptedModel, namingBlock)
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

// TestE2E_SessionAutoNaming_ManualWins proves a human-set title is never
// overwritten: the auto path refuses the manual title (no LLM naming call), so
// the manual title survives the run.
func TestE2E_SessionAutoNaming_ManualWins(t *testing.T) {
	// Only the planner's Finish is scripted — the naming trigger must NOT make
	// an LLM call over a manual title (an extra call would exhaust the script
	// and fail loudly).
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
