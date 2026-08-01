// Package integration_test — cross-subsystem E2E for the authority-separated
// prompt seam (D-387): `RunOverrides.extra_instructions` reaches a real ReAct
// user-personalization section alongside admin-set tenant-wide guidance.
//
// Real drivers on every seam: a real inmem StateStore + inmem EventBus with
// the real patterns redactor back the real governance.TenantOverridePolicy; a
// real runs/protocol Service + Store records the run-level value through the
// same identity-checked entry point the wire handler calls; the PRODUCTION
// runsprotocol.ComposeLLMOverrides performs the authority split (the same function
// cmd/harbor's run loop reaches through resolveLLMOverrides — no
// re-implemented copy, CLAUDE.md §17.4); a real react planner renders the
// composed bundle into a real llm.CompleteRequest.
//
// Reuses the tenant-override harness in tenant_overrides_test.go
// (capturingLLM, runWithOverrides, newTenantOverrideStack, resolveOverrides,
// ovActor, ptrS/ptrF, reqBodyContains).
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
)

// runsService builds the real `runs.set_overrides` Service over a real Store,
// publishing onto the supplied real bus.
func runsService(t *testing.T, bus events.EventBus) (*runsprotocol.Service, *runsprotocol.Store) {
	t.Helper()
	store := runsprotocol.NewStore()
	svc, err := runsprotocol.NewService(store, runsprotocol.WithBus(bus))
	if err != nil {
		t.Fatalf("runsprotocol.NewService: %v", err)
	}
	return svc, store
}

// setRunGuidance records a run-level additive-guidance block through the real
// Service for the identity triple `(tenant, user, session)`.
func setRunGuidance(t *testing.T, svc *runsprotocol.Service, id identity.Identity, guidance string) {
	t.Helper()
	if _, err := svc.SetOverrides(context.Background(), prototypes.RunSetOverridesRequest{
		Identity: prototypes.IdentityScope{
			Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		},
		Overrides: prototypes.RunOverrides{
			SessionID: id.SessionID, ExtraInstructions: ptrS(guidance),
		},
	}); err != nil {
		t.Fatalf("SetOverrides(%+v): %v", id, err)
	}
}

// consumeSession Consumes the pending override for id, as the run loop does at
// run start. Returns nil when the slot is empty.
func consumeSession(store *runsprotocol.Store, id identity.Identity) *runsprotocol.PendingOverride {
	po, ok := store.Consume(id)
	if !ok {
		return nil
	}
	return &po
}

// guidanceSystemText joins every system-role message body in a captured
// request, so a positional assertion can be made over the rendered prompt.
func guidanceSystemText(t *testing.T, msgs []llm.ChatMessage) string {
	t.Helper()
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == llm.RoleSystem && m.Content.Text != nil {
			b.WriteString(*m.Content.Text)
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		t.Fatal("no system-role message in the captured LLM request")
	}
	return b.String()
}

// runIdentity is the triple runWithOverrides mints for a tenant — the helper
// pins user "u" / session "s", so the run-level slot must be keyed the same
// way for the Consume at run start to find it.
func runIdentity(tenant string) identity.Identity {
	return identity.Identity{TenantID: tenant, UserID: "u", SessionID: "s"}
}

// TestE2E_RunExtraInstructions_TwoProducerSeam is the seam end to end: an
// admin sets a tenant-wide additive block through the real governance policy;
// a session caller sets a run-level one through the real `runs.set_overrides`
// Service; the run's resolved bundle carries BOTH in order and the real ReAct
// system prompt renders them in distinct operator and personalization sections.
func TestE2E_RunExtraInstructions_TwoProducerSeam(t *testing.T) {
	const (
		tenantBlock  = "TENANT-COMPLIANCE-SENTINEL: cite every source."
		sessionBlock = "SESSION-GUIDANCE-SENTINEL: answer in the imperative mood."
	)
	st := newTenantOverrideStack(t, nil)
	svc, store := runsService(t, st.bus)

	// Admin sets the tenant-wide additive block (admin-gated surface).
	if err := st.policy.Set(context.Background(), ovActor("t1"), "t1", governance.TenantOverrideSpec{
		ExtraInstructions: ptrS(tenantBlock),
	}); err != nil {
		t.Fatalf("policy.Set: %v", err)
	}
	// The session caller sets the run-level block (NOT admin-gated).
	id := runIdentity("t1")
	setRunGuidance(t, svc, id, sessionBlock)

	// Run start: resolve the tenant layer, Consume the session slot, compose.
	tenant := resolveOverrides(t, st.policy, "t1")
	composed := runsprotocol.ComposeLLMOverrides(consumeSession(store, id), nil, tenant)
	if composed == nil || composed.ExtraInstructions == nil || composed.UserPersonalization == nil {
		t.Fatal("operator guidance or user personalization missing at run start")
	}
	if *composed.ExtraInstructions != tenantBlock {
		t.Fatalf("operator guidance = %q, want %q", *composed.ExtraInstructions, tenantBlock)
	}
	if *composed.UserPersonalization != sessionBlock {
		t.Fatalf("user personalization = %q, want %q", *composed.UserPersonalization, sessionBlock)
	}

	// The real planner renders both, in order, in distinct authority sections.
	req := runWithOverrides(t, &capturingLLM{}, "t1", composed)
	if !reqBodyContains(req.Messages, tenantBlock) {
		t.Error("the tenant block never reached the system prompt")
	}
	if !reqBodyContains(req.Messages, sessionBlock) {
		t.Error("the run-level block never reached the system prompt")
	}
	body := guidanceSystemText(t, req.Messages)
	if got := strings.Count(body, "<additional_guidance>"); got != 1 {
		t.Fatalf("operator guidance section count = %d, want 1. Body: %s", got, body)
	}
	if got := strings.Count(body, "<user_personalization>"); got != 1 {
		t.Fatalf("user personalization section count = %d, want 1. Body: %s", got, body)
	}
	if strings.Index(body, tenantBlock) == strings.Index(body, sessionBlock) {
		t.Fatalf("authority-separated contributions collapsed to one position. Body: %s", body)
	}
}

// TestE2E_RunExtraInstructions_TenantBlockSurvivesEverySessionShape proves the
// no-clear property at the seam: neither an empty run-level value nor a
// whole-spine SystemPromptOverride in the same request removes the admin-set
// tenant block from the rendered prompt.
func TestE2E_RunExtraInstructions_TenantBlockSurvivesEverySessionShape(t *testing.T) {
	const tenantBlock = "TENANT-COMPLIANCE-SENTINEL: never disclose internal ids."
	st := newTenantOverrideStack(t, nil)
	svc, store := runsService(t, st.bus)
	if err := st.policy.Set(context.Background(), ovActor("t1"), "t1", governance.TenantOverrideSpec{
		ExtraInstructions: ptrS(tenantBlock),
	}); err != nil {
		t.Fatalf("policy.Set: %v", err)
	}
	id := runIdentity("t1")

	t.Run("an empty run-level value is not a clear", func(t *testing.T) {
		setRunGuidance(t, svc, id, "")
		composed := runsprotocol.ComposeLLMOverrides(
			consumeSession(store, id), nil, resolveOverrides(t, st.policy, "t1"))
		req := runWithOverrides(t, &capturingLLM{}, "t1", composed)
		if !reqBodyContains(req.Messages, tenantBlock) {
			t.Fatal("an empty run-level extra_instructions ERASED the admin-set tenant block")
		}
	})

	t.Run("a whole-spine replace in the same request does not clear it", func(t *testing.T) {
		if _, err := svc.SetOverrides(context.Background(), prototypes.RunSetOverridesRequest{
			Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
			Overrides: prototypes.RunOverrides{
				SessionID:            id.SessionID,
				SystemPromptOverride: ptrS("SESSION-REPLACE-SENTINEL"),
				ExtraInstructions:    ptrS("RUN-LEVEL-SENTINEL"),
			},
		}); err != nil {
			t.Fatalf("SetOverrides: %v", err)
		}
		composed := runsprotocol.ComposeLLMOverrides(
			consumeSession(store, id), nil, resolveOverrides(t, st.policy, "t1"))
		req := runWithOverrides(t, &capturingLLM{}, "t1", composed)
		if !reqBodyContains(req.Messages, "SESSION-REPLACE-SENTINEL") {
			t.Error("the system-prompt replace did not apply")
		}
		if !reqBodyContains(req.Messages, tenantBlock) {
			t.Error("the tenant block was dropped under a session replace")
		}
		if !reqBodyContains(req.Messages, "RUN-LEVEL-SENTINEL") {
			t.Error("the run-level block was dropped under a session replace")
		}
	})
}

// TestE2E_RunExtraInstructions_FailureModes covers the three refusals the
// plan names: a cross-session target stores NOTHING for either identity, an
// incomplete triple is refused before the Store is touched, and the one-shot
// property holds with the new field (a second run sees the tenant block only).
func TestE2E_RunExtraInstructions_FailureModes(t *testing.T) {
	const tenantBlock = "TENANT-COMPLIANCE-SENTINEL: stay on topic."
	st := newTenantOverrideStack(t, nil)
	svc, store := runsService(t, st.bus)
	if err := st.policy.Set(context.Background(), ovActor("t1"), "t1", governance.TenantOverrideSpec{
		ExtraInstructions: ptrS(tenantBlock),
	}); err != nil {
		t.Fatalf("policy.Set: %v", err)
	}
	id := runIdentity("t1")

	t.Run("cross-session target: refused and nothing stored", func(t *testing.T) {
		_, err := svc.SetOverrides(context.Background(), prototypes.RunSetOverridesRequest{
			Identity: prototypes.IdentityScope{Tenant: "t1", User: "u", Session: "s"},
			Overrides: prototypes.RunOverrides{
				SessionID: "someone-elses-session", ExtraInstructions: ptrS("INJECTED"),
			},
		})
		if !errors.Is(err, runsprotocol.ErrCrossSessionScope) {
			t.Fatalf("error = %v, want ErrCrossSessionScope", err)
		}
		for name, probe := range map[string]identity.Identity{
			"caller's own session": id,
			"the named session":    {TenantID: "t1", UserID: "u", SessionID: "someone-elses-session"},
		} {
			if _, ok := store.Peek(probe); ok {
				t.Errorf("%s: a refused cross-session set stored an override", name)
			}
		}
	})

	t.Run("incomplete identity: refused before the Store is touched", func(t *testing.T) {
		_, err := svc.SetOverrides(context.Background(), prototypes.RunSetOverridesRequest{
			Identity: prototypes.IdentityScope{Tenant: "t1", Session: "s"}, // no user
			Overrides: prototypes.RunOverrides{
				SessionID: "s", ExtraInstructions: ptrS("INJECTED"),
			},
		})
		if !errors.Is(err, runsprotocol.ErrIdentityRequired) {
			t.Fatalf("error = %v, want ErrIdentityRequired", err)
		}
		if _, ok := store.Peek(identity.Identity{TenantID: "t1", UserID: "", SessionID: "s"}); ok {
			t.Error("a refused incomplete-identity set stored an override")
		}
	})

	t.Run("one-shot: the run-level block is not re-applied to the next run", func(t *testing.T) {
		setRunGuidance(t, svc, id, "ONE-SHOT-SENTINEL")
		first := runsprotocol.ComposeLLMOverrides(
			consumeSession(store, id), nil, resolveOverrides(t, st.policy, "t1"))
		if r := runWithOverrides(t, &capturingLLM{}, "t1", first); !reqBodyContains(r.Messages, "ONE-SHOT-SENTINEL") {
			t.Fatal("the first run did not see the run-level block")
		}
		second := runsprotocol.ComposeLLMOverrides(
			consumeSession(store, id), nil, resolveOverrides(t, st.policy, "t1"))
		r := runWithOverrides(t, &capturingLLM{}, "t1", second)
		if reqBodyContains(r.Messages, "ONE-SHOT-SENTINEL") {
			t.Error("the run-level block was re-applied to a second run — the slot is not one-shot")
		}
		if !reqBodyContains(r.Messages, tenantBlock) {
			t.Error("the tenant block vanished on the second run")
		}
	})
}

// TestE2E_RunExtraInstructions_IdentityIsolationUnderConcurrency is the
// multi-isolation stress: N tenants, each with its own tenant block and its
// own run-level block, resolved and rendered CONCURRENTLY through one shared
// policy + one shared runs Service + one shared Store. Every run must see
// exactly its own two authority-separated segments and no other tenant's bytes.
func TestE2E_RunExtraInstructions_IdentityIsolationUnderConcurrency(t *testing.T) {
	const n = 128
	st := newTenantOverrideStack(t, nil)
	svc, store := runsService(t, st.bus)

	tenantOf := func(i int) string { return fmt.Sprintf("tenant-%03d", i) }
	tenantBlockOf := func(i int) string { return fmt.Sprintf("TENANT-BLOCK-%03d-END", i) }
	runBlockOf := func(i int) string { return fmt.Sprintf("RUN-BLOCK-%03d-END", i) }

	for i := range n {
		if err := st.policy.Set(context.Background(), ovActor(tenantOf(i)), tenantOf(i),
			governance.TenantOverrideSpec{ExtraInstructions: ptrS(tenantBlockOf(i))}); err != nil {
			t.Fatalf("policy.Set(%s): %v", tenantOf(i), err)
		}
		setRunGuidance(t, svc, runIdentity(tenantOf(i)), runBlockOf(i))
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			tn := tenantOf(i)
			spec, set, err := st.policy.Get(context.Background(), tn)
			if err != nil {
				t.Errorf("%s: policy.Get: %v", tn, err)
				return
			}
			if !set {
				t.Errorf("%s: tenant record missing", tn)
				return
			}
			tenant := &planner.LLMOverrides{ExtraInstructions: spec.ExtraInstructions}
			composed := runsprotocol.ComposeLLMOverrides(consumeSession(store, runIdentity(tn)), nil, tenant)
			req := runWithOverrides(t, &capturingLLM{}, tn, composed)
			body := guidanceSystemText(t, req.Messages)
			if !strings.Contains(body, tenantBlockOf(i)) || !strings.Contains(body, runBlockOf(i)) {
				t.Errorf("%s: own segments missing from the prompt", tn)
				return
			}
			for j := range n {
				if j == i {
					continue
				}
				if strings.Contains(body, tenantBlockOf(j)) || strings.Contains(body, runBlockOf(j)) {
					t.Errorf("%s: cross-tenant bleed — tenant %02d's guidance reached this run", tn, j)
					return
				}
			}
		}()
	}
	// Bounded join: a hung goroutine must fail the test, never wedge it.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent isolation arm did not settle within 30s")
	}
}
