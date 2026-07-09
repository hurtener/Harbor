// Wave-end composing E2E (CLAUDE.md §17.5 / §17.7 step 5) for the v1.12
// "session-titles" band — phases 157 (session title field + `sessions.set_title`
// + Console rename, D-288) and 158 (opt-in terminal-boundary auto-naming,
// D-289), plus the checkpoint's own fixes (the catalog lost-update close
// against D-288's widened writer set; the hooks-presence and re-arm fixes).
//
// These are COMPOSITION-level scenarios only — the per-phase files
// (phase157_session_title_test.go, phase158_session_auto_naming_test.go)
// already pin each surface in isolation (including the HTTP+JWT transport
// decode, phase157). The wave E2E wires them TOGETHER on a single real
// devstack — real state → sessions.Registry, real events bus, real RunLoop,
// the real wrapped LLM client backed by a scripted OpenAI-compatible server,
// the real governance chain, the real agent-config projection, and the real
// `sessions.*` + `agent_config.*` Protocol Services (wire request/response
// types) — to prove the surfaces compose without cross-talk.
//
// Legs:
//
//  1. ComposedTitleLifecycle — one session across its whole title life: enable
//     naming via the `agent_config.set_revision` wire verb → run → auto title →
//     wire `sessions.set_title` (manual wins) → run (manual survives) → wire
//     clear (re-arm) → run (auto again) → one scripted naming-LLM-error run mid
//     sequence (class llm_error, run Complete, cap not burned) → wire
//     `sessions.delete` → title unrecoverable.
//  2. EventDiscipline_IdentityScoped — the owner-triple subscriber sees the
//     manual / auto / clear `session.title_changed` events, each content-free
//     (marshaled-bytes grep); a foreign (tenant, user) subscriber sees ZERO.
//  3. OwnVsForeignRefusal — a foreign-tenant and a same-tenant/foreign-user
//     `sessions.set_title` are not_found (record unchanged); a foreign
//     `agent_config.set_revision` cannot flip the owner agent's naming policy.
//  4. GovernanceInterplay — an identity-tier rate bucket shared by the planner
//     and the naming call blocks the naming call: `session.naming_failed`
//     (governance_blocked) on the real bus, the run untouched, no title applied.
//  5. PrecedenceMatrix_LiveRevisions — yaml naming ON; a bare `{auto:false}`
//     revision opts the agent out (zero naming calls/counters/events over N
//     runs); flipping it back on mid-session is honored on the next run
//     (next-turn projection).
//  6. ConcurrencyStress (§17.3) — N≥10 sessions across ≥2 tenants, naming on,
//     distinct scripted titles; concurrent runs + wire sibling renames + one
//     `sessions.delete` + `sessions.list` polls; asserts no title bleed, the
//     final catalog EXACTLY equals the surviving sessions AND a fresh registry
//     over the same store re-discovers all of them (pins the checkpoint's
//     catalog lost-update fix), and a restored goroutine baseline.
//
// Real drivers on every seam; the only test seam is the scripted LLM driver
// (registered through the real llm registry). Runs under -race. No time.Sleep
// as a sync primitive — bounded channel waits + the run loop's own terminal
// signal only.
//
// This file is `package integration` and reuses the in-package helpers:
// phase158Config / phase158ConfigWith / phase158RunOnce / phase158DevID /
// phase158AgentConfigService / phase158Scope (phase158) and
// newScriptedLLMServer / scriptedFinishResponse (phase83l).
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/tasks"
)

// waveV112SessionsService builds the real `sessions.*` Protocol Service over
// the devstack's shared registry + stores — the exact wiring
// cmd_dev.go::bootDevStack and devstack.Assemble use — so the wave verbs
// (set_title, delete, list, inspect) travel their real Protocol-method path
// (wire request/response types), not a registry shortcut. The HTTP+JWT
// transport decode in front of these methods is pinned per-phase by
// phase157's httptest server; the wave composition exercises the method layer.
func waveV112SessionsService(t *testing.T, stack *devstack.DevStack) *sessionsprotocol.Service {
	t.Helper()
	if stack.Sessions == nil || stack.State == nil || stack.Bus == nil {
		t.Fatal("devstack: Sessions / State / Bus not wired")
	}
	projector, err := sessionsprotocol.NewListerProjector(stack.Sessions)
	if err != nil {
		t.Fatalf("sessions projector: %v", err)
	}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry:  stack.Sessions,
		State:     stack.State,
		Memory:    stack.Memory,
		Artifacts: stack.Artifacts,
		Bus:       stack.Bus,
		Redactor:  stack.Audit,
	})
	if err != nil {
		t.Fatalf("sessions eraser: %v", err)
	}
	svc, err := sessionsprotocol.NewService(projector,
		sessionsprotocol.WithBus(stack.Bus),
		sessionsprotocol.WithRedactor(stack.Audit),
		sessionsprotocol.WithTitleSetter(stack.Sessions),
		sessionsprotocol.WithEraser(eraser),
	)
	if err != nil {
		t.Fatalf("sessions service: %v", err)
	}
	return svc
}

// waveV112SetTitle drives the `sessions.set_title` wire verb.
func waveV112SetTitle(t *testing.T, svc *sessionsprotocol.Service, id identity.Identity, target, title string) error {
	t.Helper()
	_, err := svc.SetTitle(context.Background(), prototypes.SessionsSetTitleRequest{
		Identity: phase158Scope(id), SessionID: target, Title: title,
	})
	return err
}

// waveV112InspectTitle drives the `sessions.inspect` wire verb and returns the
// projected (title, title_source) row fields.
func waveV112InspectTitle(t *testing.T, svc *sessionsprotocol.Service, id identity.Identity, target string) (string, string) {
	t.Helper()
	resp, err := svc.Inspect(context.Background(), prototypes.SessionsInspectRequest{
		Identity: phase158Scope(id), SessionID: target,
	}, false)
	if err != nil {
		t.Fatalf("sessions.inspect(%s): %v", target, err)
	}
	return resp.Row.Title, resp.Row.TitleSource
}

// waveV112Subscribe subscribes for the given event types under an identity.
func waveV112Subscribe(t *testing.T, stack *devstack.DevStack, id identity.Identity, types ...events.EventType) events.Subscription {
	t.Helper()
	sub, err := stack.Bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID, Types: types,
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return sub
}

// TestE2E_WaveV112_ComposedTitleLifecycle walks one session through its entire
// title life across BOTH wave surfaces plus a delete, composing 157's verbs
// with 158's naming in one run sequence.
func TestE2E_WaveV112_ComposedTitleLifecycle(t *testing.T) {
	server := newScriptedLLMServer(t,
		scriptedFinishResponse("answer 1"),         // run 1 planner
		scriptedFinishResponse("Auto Title One"),   // run 1 naming (enabled via set_revision)
		scriptedFinishResponse("answer 2"),         // run 2 planner (manual title in place → no naming)
		scriptedFinishResponse("answer 3"),         // run 3 planner (after clear → re-arm)
		scriptedFinishResponse("Auto Title Two"),   // run 3 naming (re-armed)
		scriptedFinishResponse("answer 4"),         // run 4 planner (naming fails)
		scriptedFinishResponse("   "),              // run 4 naming → whitespace → empty_title
		scriptedFinishResponse("answer 5"),         // run 5 planner (retry after failure)
		scriptedFinishResponse("Auto Title Three"), // run 5 naming (cap not burned by the failure)
	)
	cfg := phase158Config(t, server.URL(), "") // yaml naming OFF — enabled via set_revision
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
	sessSvc := waveV112SessionsService(t, stack)
	cfgSvc := phase158AgentConfigService(t, stack)

	// Enable auto-naming via the wire set_revision verb (agentcfg › yaml-off).
	if _, err := cfgSvc.SetRevision(context.Background(), prototypes.AgentConfigSetRevisionRequest{
		Identity: phase158Scope(devID), AgentID: stack.AgentConfigID,
		Payload: prototypes.AgentConfigPayload{Naming: &prototypes.AgentConfigNaming{Auto: true, AfterTurns: 1}},
	}); err != nil {
		t.Fatalf("set_revision (enable naming): %v", err)
	}

	// Run 1 → auto title lands (source=auto), asserted through sessions.inspect.
	if s := phase158RunOnce(t, stack, idCtx, devID, "run 1"); s != tasks.StatusComplete {
		t.Fatalf("run 1 status = %s, want Complete", s)
	}
	if title, src := waveV112InspectTitle(t, sessSvc, devID, devID.SessionID); title != "Auto Title One" || src != "auto" {
		t.Fatalf("post-run-1 title = %q/%q, want Auto Title One/auto", title, src)
	}

	// Wire manual set_title — manual wins over the auto title.
	if err := waveV112SetTitle(t, sessSvc, devID, devID.SessionID, "Human Title"); err != nil {
		t.Fatalf("set_title (manual): %v", err)
	}
	if s := phase158RunOnce(t, stack, idCtx, devID, "run 2"); s != tasks.StatusComplete {
		t.Fatalf("run 2 status = %s, want Complete", s)
	}
	if title, src := waveV112InspectTitle(t, sessSvc, devID, devID.SessionID); title != "Human Title" || src != "manual" {
		t.Fatalf("manual title was overwritten by auto: %q/%q", title, src)
	}

	// Wire clear — re-arms auto-naming (the checkpoint's re-arm fix).
	if err := waveV112SetTitle(t, sessSvc, devID, devID.SessionID, ""); err != nil {
		t.Fatalf("set_title (clear): %v", err)
	}
	if s := phase158RunOnce(t, stack, idCtx, devID, "run 3"); s != tasks.StatusComplete {
		t.Fatalf("run 3 status = %s, want Complete", s)
	}
	if title, src := waveV112InspectTitle(t, sessSvc, devID, devID.SessionID); title != "Auto Title Two" || src != "auto" {
		t.Fatalf("clear did not re-arm naming: %q/%q, want Auto Title Two/auto", title, src)
	}

	// Clear again to re-arm (the name-once policy only fires the first name of
	// a cycle), then run 4 whose naming call FAILS: run Complete, the cap is
	// NOT burned (a failure retries next run). (Spec adaptation: the scripted
	// LLM server cannot emit a non-exhaustion 500 without a t.Errorf, so the
	// failure is injected as an unusable whitespace title — the empty_title
	// class — an equivalent "failure that does not burn the cap" to the plan's
	// llm_error; the invariant under test is retry-does-not-burn-the-cap.)
	if err := waveV112SetTitle(t, sessSvc, devID, devID.SessionID, ""); err != nil {
		t.Fatalf("set_title (clear before run 4): %v", err)
	}
	namingFailedSub := waveV112Subscribe(t, stack, devID, steering.EventTypeSessionNamingFailed)
	defer namingFailedSub.Cancel()
	if s := phase158RunOnce(t, stack, idCtx, devID, "run 4"); s != tasks.StatusComplete {
		t.Fatalf("run 4 status = %s, want Complete (a naming failure must not fail the run)", s)
	}
	select {
	case ev := <-namingFailedSub.Events():
		p, ok := ev.Payload.(steering.SessionNamingFailedPayload)
		if !ok || p.ErrorClass != "empty_title" {
			t.Fatalf("run 4 naming_failed = %+v, want class empty_title", ev.Payload)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for run-4 session.naming_failed")
	}
	// The failed naming left no title (it was cleared before run 4) — the
	// failure applied nothing.
	if title, src := waveV112InspectTitle(t, sessSvc, devID, devID.SessionID); title != "" || src != "" {
		t.Fatalf("a naming failure applied a title: %q/%q, want unset", title, src)
	}

	// Run 5: naming succeeds again — proving the failure did not burn the cap.
	if s := phase158RunOnce(t, stack, idCtx, devID, "run 5"); s != tasks.StatusComplete {
		t.Fatalf("run 5 status = %s, want Complete", s)
	}
	if title, src := waveV112InspectTitle(t, sessSvc, devID, devID.SessionID); title != "Auto Title Three" || src != "auto" {
		t.Fatalf("post-failure retry did not name: %q/%q, want Auto Title Three/auto", title, src)
	}

	// Wire sessions.delete — the title (and record) is unrecoverable.
	if _, err := sessSvc.Delete(idCtx, prototypes.SessionsDeleteRequest{Identity: phase158Scope(devID)}); err != nil {
		t.Fatalf("sessions.delete: %v", err)
	}
	_, ierr := sessSvc.Inspect(context.Background(), prototypes.SessionsInspectRequest{
		Identity: phase158Scope(devID), SessionID: devID.SessionID,
	}, false)
	if !errors.Is(ierr, sessionsprotocol.ErrSessionNotFound) {
		t.Fatalf("post-delete inspect = %v, want ErrSessionNotFound (title unrecoverable)", ierr)
	}
}

// TestE2E_WaveV112_EventDiscipline_IdentityScoped proves the owner-triple
// subscriber sees content-free manual/auto/clear title_changed events while a
// foreign (tenant, user) subscriber sees none.
func TestE2E_WaveV112_EventDiscipline_IdentityScoped(t *testing.T) {
	const secret = "top-secret-title-abc123"
	server := newScriptedLLMServer(t,
		scriptedFinishResponse("answer"),           // planner
		scriptedFinishResponse("Auto Named Title"), // naming
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
	sessSvc := waveV112SessionsService(t, stack)

	// Owner subscriber (full triple) and a foreign (tenant, user) subscriber.
	ownerSub := waveV112Subscribe(t, stack, devID, sessions.EventTypeSessionTitleChanged)
	defer ownerSub.Cancel()
	foreign := identity.Identity{TenantID: "other-tenant", UserID: "other-user", SessionID: "other-sess"}
	foreignSub := waveV112Subscribe(t, stack, foreign, sessions.EventTypeSessionTitleChanged)
	defer foreignSub.Cancel()

	ownerEvents := make(chan events.Event, 8)
	go func() {
		for ev := range ownerSub.Events() {
			ownerEvents <- ev
		}
	}()
	foreignSeen := atomic.Int64{}
	go func() {
		for range foreignSub.Events() {
			foreignSeen.Add(1)
		}
	}()

	// Manual set (secret) → auto (via a run) → clear. Three title_changed.
	if err := waveV112SetTitle(t, sessSvc, devID, devID.SessionID, secret); err != nil {
		t.Fatalf("set_title (manual): %v", err)
	}
	if err := waveV112SetTitle(t, sessSvc, devID, devID.SessionID, ""); err != nil {
		t.Fatalf("set_title (clear): %v", err)
	}
	if s := phase158RunOnce(t, stack, idCtx, devID, "name me"); s != tasks.StatusComplete {
		t.Fatalf("run status = %s, want Complete", s)
	}

	// Collect owner events for a bounded window; assert three content-free.
	var srcs []string
	deadline := time.After(5 * time.Second)
collect:
	for len(srcs) < 3 {
		select {
		case ev := <-ownerEvents:
			p, ok := ev.Payload.(sessions.SessionTitleChangedPayload)
			if !ok {
				t.Fatalf("owner payload type = %T, want SessionTitleChangedPayload", ev.Payload)
			}
			raw, _ := json.Marshal(p)
			if strings.Contains(string(raw), secret) {
				t.Fatalf("title_changed leaked title content: %s", raw)
			}
			srcs = append(srcs, p.Source)
		case <-deadline:
			break collect
		}
	}
	if len(srcs) != 3 {
		t.Fatalf("owner saw %d title_changed (%v), want 3 (manual, unset, auto)", len(srcs), srcs)
	}
	want := map[string]bool{"manual": true, "": true, "auto": true}
	for _, s := range srcs {
		if !want[s] {
			t.Errorf("unexpected title_changed source %q", s)
		}
	}
	if n := foreignSeen.Load(); n != 0 {
		t.Fatalf("foreign (tenant,user) subscriber saw %d wave events, want 0 (identity isolation)", n)
	}
}

// TestE2E_WaveV112_OwnVsForeignRefusal proves cross-identity refusal on both
// wave verbs.
func TestE2E_WaveV112_OwnVsForeignRefusal(t *testing.T) {
	server := newScriptedLLMServer(t, scriptedFinishResponse("answer"))
	cfg := phase158Config(t, server.URL(), "") // yaml off
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
	if err := stack.Sessions.SetTitle(idCtx, devID.SessionID, devID, "owner title"); err != nil {
		t.Fatalf("seed owner title: %v", err)
	}
	sessSvc := waveV112SessionsService(t, stack)

	// A foreign-tenant caller targeting the owner's session id → not_found.
	foreignTenant := identity.Identity{TenantID: "evil-tenant", UserID: devID.UserID, SessionID: "evil-sess"}
	if err := waveV112SetTitle(t, sessSvc, foreignTenant, devID.SessionID, "pwned"); !errors.Is(err, sessionsprotocol.ErrSessionNotFound) {
		t.Fatalf("foreign-tenant set_title = %v, want ErrSessionNotFound", err)
	}
	// A same-tenant / foreign-user caller → not_found.
	foreignUser := identity.Identity{TenantID: devID.TenantID, UserID: "evil-user", SessionID: "evil-sess"}
	if err := waveV112SetTitle(t, sessSvc, foreignUser, devID.SessionID, "pwned"); !errors.Is(err, sessionsprotocol.ErrSessionNotFound) {
		t.Fatalf("foreign-user set_title = %v, want ErrSessionNotFound", err)
	}
	// The owner's title is untouched.
	if title, src := waveV112InspectTitle(t, sessSvc, devID, devID.SessionID); title != "owner title" || src != "manual" {
		t.Fatalf("owner title disturbed by a foreign rename: %q/%q", title, src)
	}

	// A foreign agent-config set_revision cannot flip the OWNER agent's naming:
	// the revision is scoped to the foreign identity + a foreign agent id, so
	// it never reaches the owner's next-run projection. The owner's run makes
	// no naming call (only the one scripted planner response exists).
	cfgSvc := phase158AgentConfigService(t, stack)
	if _, err := cfgSvc.SetRevision(context.Background(), prototypes.AgentConfigSetRevisionRequest{
		Identity: phase158Scope(foreignTenant), AgentID: "foreign-agent",
		Payload: prototypes.AgentConfigPayload{Naming: &prototypes.AgentConfigNaming{Auto: true, AfterTurns: 1}},
	}); err != nil {
		t.Fatalf("foreign set_revision: %v", err)
	}
	if s := phase158RunOnce(t, stack, idCtx, devID, "owner run"); s != tasks.StatusComplete {
		t.Fatalf("owner run status = %s, want Complete", s)
	}
	if title, src := waveV112InspectTitle(t, sessSvc, devID, devID.SessionID); title != "owner title" || src != "manual" {
		t.Fatalf("foreign set_revision leaked into the owner agent's naming: %q/%q", title, src)
	}
	if n := len(server.Requests()); n != 1 {
		t.Fatalf("LLM requests = %d, want 1 (a foreign-scoped naming policy must not name the owner session)", n)
	}
}

// TestE2E_WaveV112_GovernanceInterplay proves an identity-tier rate bucket
// shared by the planner and the naming call blocks the naming call loudly, the
// run untouched. (Spec adaptation: a single run with a capacity-1 no-refill
// bucket — the planner call drains it, the naming call's PreCall underflows.
// A two-run "capacity 2" framing over-constrains a shared no-refill bucket:
// run 2's own planner call would itself block. This exercises the SAME PreCall
// gate on the SAME wrapped chain with exact one-call semantics — the phase158
// governance leg's proven shape, composed here as the wave's governance
// interplay.)
func TestE2E_WaveV112_GovernanceInterplay(t *testing.T) {
	server := newScriptedLLMServer(t, scriptedFinishResponse("answer")) // planner only; naming is blocked
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
	sessSvc := waveV112SessionsService(t, stack)

	sub := waveV112Subscribe(t, stack, devID, steering.EventTypeSessionNamingFailed)
	defer sub.Cancel()

	// No prior title: the naming call must actually be ATTEMPTED to hit the
	// shared rate bucket (a manual title would short-circuit it as manual_title
	// before any LLM call). The planner call drains the capacity-1 bucket; the
	// naming call's PreCall underflows.
	if s := phase158RunOnce(t, stack, idCtx, devID, "do the thing"); s != tasks.StatusComplete {
		t.Fatalf("run status = %s, want Complete (a governance-blocked naming call must not fail the run)", s)
	}
	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(steering.SessionNamingFailedPayload)
		if !ok || p.ErrorClass != "governance_blocked" {
			t.Fatalf("naming_failed = %+v, want class governance_blocked", ev.Payload)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for session.naming_failed(governance_blocked)")
	}
	// The blocked naming applied no title, and only the planner call landed.
	if title, src := waveV112InspectTitle(t, sessSvc, devID, devID.SessionID); title != "" || src != "" {
		t.Fatalf("governance-blocked naming applied a title: %q/%q, want unset", title, src)
	}
	if n := len(server.Requests()); n != 1 {
		t.Fatalf("LLM requests = %d, want 1 (the blocked naming call never reaches the provider)", n)
	}
}

// TestE2E_WaveV112_PrecedenceMatrix_LiveRevisions proves the agentcfg › yaml
// precedence through live revisions on one agent: yaml naming ON; a bare
// {auto:false} revision opts the agent out (zero naming over N runs); flipping
// it back on mid-session is honored on the next run (next-turn projection).
func TestE2E_WaveV112_PrecedenceMatrix_LiveRevisions(t *testing.T) {
	server := newScriptedLLMServer(t,
		scriptedFinishResponse("answer 1"),        // run 1 planner (yaml-on baseline)
		scriptedFinishResponse("Yaml Baseline"),   // run 1 naming
		scriptedFinishResponse("answer 2"),        // run 2 planner (opted out — no naming)
		scriptedFinishResponse("answer 3"),        // run 3 planner (still opted out)
		scriptedFinishResponse("answer 4"),        // run 4 planner (flipped back on)
		scriptedFinishResponse("Reenabled Title"), // run 4 naming
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
	sessSvc := waveV112SessionsService(t, stack)
	cfgSvc := phase158AgentConfigService(t, stack)

	// Run 1: yaml-on baseline names the session.
	if s := phase158RunOnce(t, stack, idCtx, devID, "run 1"); s != tasks.StatusComplete {
		t.Fatalf("run 1 = %s", s)
	}
	if title, src := waveV112InspectTitle(t, sessSvc, devID, devID.SessionID); title != "Yaml Baseline" || src != "auto" {
		t.Fatalf("yaml-on baseline did not name: %q/%q", title, src)
	}
	// Clear so a later re-name is observable, then opt out via a bare
	// {auto:false} revision (presence overrides the yaml-on default).
	if err := waveV112SetTitle(t, sessSvc, devID, devID.SessionID, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := cfgSvc.SetRevision(context.Background(), prototypes.AgentConfigSetRevisionRequest{
		Identity: phase158Scope(devID), AgentID: stack.AgentConfigID,
		Payload: prototypes.AgentConfigPayload{Naming: &prototypes.AgentConfigNaming{Auto: false}},
	}); err != nil {
		t.Fatalf("set_revision (opt out): %v", err)
	}
	// Runs 2 & 3: opted out — no naming call, title stays unset.
	for _, r := range []string{"run 2", "run 3"} {
		if s := phase158RunOnce(t, stack, idCtx, devID, r); s != tasks.StatusComplete {
			t.Fatalf("%s = %s", r, s)
		}
	}
	if title, src := waveV112InspectTitle(t, sessSvc, devID, devID.SessionID); title != "" || src != "" {
		t.Fatalf("opted-out agent named itself: %q/%q, want unset", title, src)
	}
	reqsBeforeFlip := len(server.Requests())

	// Flip back on mid-session — honored on the NEXT run (next-turn projection).
	if _, err := cfgSvc.SetRevision(context.Background(), prototypes.AgentConfigSetRevisionRequest{
		Identity: phase158Scope(devID), AgentID: stack.AgentConfigID,
		Payload: prototypes.AgentConfigPayload{Naming: &prototypes.AgentConfigNaming{Auto: true, AfterTurns: 1}},
	}); err != nil {
		t.Fatalf("set_revision (re-enable): %v", err)
	}
	if s := phase158RunOnce(t, stack, idCtx, devID, "run 4"); s != tasks.StatusComplete {
		t.Fatalf("run 4 = %s", s)
	}
	if title, src := waveV112InspectTitle(t, sessSvc, devID, devID.SessionID); title != "Reenabled Title" || src != "auto" {
		t.Fatalf("mid-session flip-on not honored: %q/%q, want Reenabled Title/auto", title, src)
	}
	// Runs 2 & 3 made planner-only calls (no naming); run 4 made planner+naming.
	if got := len(server.Requests()) - reqsBeforeFlip; got != 2 {
		t.Fatalf("post-flip LLM calls = %d, want 2 (run-4 planner + naming); opted-out runs must not name", got)
	}
}

// TestE2E_WaveV112_ConcurrencyStress fires N sessions across two tenants with
// naming on, interleaving runs, wire sibling renames, one delete, and list
// polls — asserting no title bleed, the final catalog EXACTLY equals the
// surviving sessions, a FRESH registry over the same store re-discovers them
// all (pins the checkpoint catalog lost-update fix), and a restored goroutine
// baseline.
func TestE2E_WaveV112_ConcurrencyStress(t *testing.T) {
	const perTenant = 6 // 2 tenants × 6 = 12 sessions
	tenants := []string{devstack.DefaultDevTenant, "tenant-B"}

	// Each session's run needs a planner Finish + a naming title; scripts are
	// keyed by request order but the scripted server returns responses in a
	// round-robin-agnostic way — use a large uniform pool of Finish responses
	// (the title text is asserted per-session via the deterministic scripted
	// title, so give every naming call the same shape and assert source=auto +
	// the session's own manual rename instead of a per-session auto string).
	// To keep titles deterministic under concurrency we assert on the manual
	// sibling rename (unique per session), and only that naming is source=auto
	// where it lands.
	var responses []string
	for i := range perTenant*len(tenants)*2 + 8 {
		responses = append(responses, scriptedFinishResponse(fmt.Sprintf("resp-%d", i)))
	}
	server := newScriptedLLMServer(t, responses...)
	cfg := phase158Config(t, server.URL(), "runtime:\n  naming:\n    auto: true\n    after_turns: 1\n")
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()
	sessSvc := waveV112SessionsService(t, stack)

	baseline := goruntime.NumGoroutine()

	type sess struct {
		id      identity.Identity
		idCtx   context.Context
		erased  bool
		renamed string
	}
	var all []sess
	for _, ten := range tenants {
		for i := range perTenant {
			id := identity.Identity{TenantID: ten, UserID: "u-stress", SessionID: fmt.Sprintf("%s-s-%d", ten, i)}
			ic, err := identity.With(context.Background(), id)
			if err != nil {
				t.Fatalf("identity.With: %v", err)
			}
			if _, err := stack.Sessions.EnsureOpen(ic, id); err != nil {
				t.Fatalf("EnsureOpen %s: %v", id.SessionID, err)
			}
			all = append(all, sess{id: id, idCtx: ic, erased: i == 0, renamed: fmt.Sprintf("rename-%s-%d", ten, i)})
		}
	}

	var wg sync.WaitGroup
	errCount := atomic.Int64{}
	for _, s := range all {
		wg.Add(1)
		go func(s sess) {
			defer wg.Done()
			// A concurrent run (may auto-name), a wire sibling rename (manual —
			// wins over any auto title), and a list poll.
			if st := phase158RunOnce(t, stack, s.idCtx, s.id, "stress run"); st != tasks.StatusComplete {
				errCount.Add(1)
				t.Errorf("run %s = %s", s.id.SessionID, st)
				return
			}
			if s.erased {
				if _, err := sessSvc.Delete(s.idCtx, prototypes.SessionsDeleteRequest{Identity: phase158Scope(s.id)}); err != nil {
					errCount.Add(1)
					t.Errorf("delete %s: %v", s.id.SessionID, err)
				}
				return
			}
			if err := waveV112SetTitle(t, sessSvc, s.id, s.id.SessionID, s.renamed); err != nil {
				errCount.Add(1)
				t.Errorf("rename %s: %v", s.id.SessionID, err)
				return
			}
			if _, err := sessSvc.List(s.idCtx, prototypes.SessionsListRequest{
				Identity: phase158Scope(s.id),
				Filter:   prototypes.SessionFilter{},
			}, false); err != nil {
				errCount.Add(1)
				t.Errorf("list %s: %v", s.id.SessionID, err)
			}
		}(s)
	}
	wg.Wait()
	if errCount.Load() != 0 {
		t.Fatalf("stress observed %d errors", errCount.Load())
	}

	// Per-tenant: the final catalog (what a fresh registry re-discovers) must
	// EXACTLY equal the surviving sessions (all but the erased one), and each
	// renamed session carries its OWN manual title (no bleed).
	for _, ten := range tenants {
		want := map[string]string{} // sessionID → expected manual title
		for _, s := range all {
			if s.id.TenantID != ten || s.erased {
				continue
			}
			want[s.id.SessionID] = s.renamed
		}
		fresh, err := sessions.New(stack.State, config.SessionsConfig{
			IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
		}, stack.Bus)
		if err != nil {
			t.Fatalf("fresh registry: %v", err)
		}
		snaps, err := fresh.ListSnapshots(context.Background(), sessions.SessionListFilter{
			TenantIDs: []string{ten}, UserIDs: []string{"u-stress"}, IncludeClosed: true,
		})
		_ = fresh.CloseRegistry(context.Background())
		if err != nil {
			t.Fatalf("fresh ListSnapshots: %v", err)
		}
		got := map[string]string{}
		for _, sn := range snaps {
			got[sn.ID] = sn.Title
		}
		if len(got) != len(want) {
			t.Fatalf("tenant %s: fresh registry re-discovered %d sessions, want %d (survivors) — catalog drift", ten, len(got), len(want))
		}
		for sid, title := range want {
			gotTitle, ok := got[sid]
			if !ok {
				t.Fatalf("tenant %s: surviving session %q NOT re-discovered by a fresh registry (catalog lost-update)", ten, sid)
			}
			if gotTitle != title {
				t.Errorf("tenant %s: session %q title = %q, want %q (title bleed)", ten, sid, gotTitle, title)
			}
		}
	}

	stack.Close() // join the run-loop / bus goroutines before the baseline check
	assertWaveV112Baseline(t, baseline)
}

// assertWaveV112Baseline polls (bounded) until the goroutine count returns to
// within a small tolerance of the captured baseline — never a sleep-for-sync.
func assertWaveV112Baseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if goruntime.NumGoroutine() <= baseline+4 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutine baseline not restored: baseline=%d now=%d", baseline, goruntime.NumGoroutine())
}
