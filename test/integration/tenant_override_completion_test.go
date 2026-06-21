// Package integration_test — cross-subsystem E2E for Phase 92b
// (tenant-override completion). It proves the session-level override
// COMPOSES over the tenant default (session › tenant › config) and the
// composed result reaches a REAL ReAct planner's LLM request, and that
// the policy is multi-replica fresh over a shared StateStore.
//
// Reuses the Phase 92 harness in tenant_overrides_test.go (capturingLLM,
// runWithOverrides, newTenantOverrideStack, ovActor, ptrS/ptrF,
// reqBodyContains). The composition is the PRODUCTION
// runsprotocol.ComposeLLMOverrides (the same function cmd/harbor's run
// loop calls — no re-implemented copy, CLAUDE.md §17.4); the seam under
// test is the real governance policy + the real planner apply.
package integration_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/planner"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
)

// TestE2E_TenantOverrideCompletion_SessionOverTenant proves a session
// override (consumed from the runs Store) composes over the admin tenant
// default and the composed result reaches the real planner's LLM request:
// the session model + system-prompt REPLACE win; the tenant temperature +
// additive extra-instructions survive.
func TestE2E_TenantOverrideCompletion_SessionOverTenant(t *testing.T) {
	st := newTenantOverrideStack(t, nil)
	// Admin tenant default.
	if err := st.policy.Set(context.Background(), ovActor("t1"), "t1", governance.TenantOverrideSpec{
		Model:             ptrS("model-a"),
		Temperature:       ptrF(0.2),
		ExtraInstructions: ptrS("TENANT-ADDITIVE-SENTINEL"),
	}); err != nil {
		t.Fatalf("policy.Set: %v", err)
	}
	tenant := resolveOverrides(t, st.policy, "t1")

	// Session override (one-shot) for this run.
	store := runsprotocol.NewStore()
	store.Set(ovActor("t1").Identity, runsprotocol.PendingOverride{
		Model:                ptrS("model-b"),
		SystemPromptOverride: ptrS("SESSION-REPLACE-SENTINEL"),
	})
	session, ok := store.Consume(ovActor("t1").Identity)
	if !ok {
		t.Fatal("session override not recorded")
	}

	// No per-agent agent-config layer in this scenario (nil) — the per-agent
	// LLM-params layer is exercised in agentcfg_llm_params_test.go.
	composed := runsprotocol.ComposeLLMOverrides(&session, nil, tenant)
	req := runWithOverrides(t, &capturingLLM{}, "t1", composed)

	if req.Model != "model-b" {
		t.Errorf("req.Model = %q, want model-b (session wins)", req.Model)
	}
	if req.Temperature == nil || *req.Temperature != float32(0.2) {
		t.Errorf("req.Temperature = %v, want tenant 0.2", req.Temperature)
	}
	if !reqBodyContains(req.Messages, "SESSION-REPLACE-SENTINEL") {
		t.Errorf("system prompt was not replaced by the session SystemPromptOverride")
	}
	if !reqBodyContains(req.Messages, "TENANT-ADDITIVE-SENTINEL") {
		t.Errorf("tenant additive extra-instructions dropped under a session replace")
	}
	// The one-shot session override is consumed.
	if _, found := store.Consume(ovActor("t1").Identity); found {
		t.Error("session override not one-shot (still present after Consume)")
	}
}

// TestE2E_TenantOverrideCompletion_MultiReplicaFreshness proves two policy
// instances over ONE shared StateStore stay fresh: a Set on A is seen by
// B's next resolve, and a SECOND Set on A also lands on B (per-read
// freshness — no permanent cross-replica staleness). Then the freshly
// resolved value reaches the planner request.
func TestE2E_TenantOverrideCompletion_MultiReplicaFreshness(t *testing.T) {
	st := newTenantOverrideStack(t, nil) // replica A
	replicaB, err := governance.NewTenantOverridePolicy(st.state, st.bus, []string{"model-a", "model-b"}, nil)
	if err != nil {
		t.Fatalf("replica B: %v", err)
	}
	defer replicaB.Close(context.Background())

	if err := st.policy.Set(context.Background(), ovActor("t1"), "t1",
		governance.TenantOverrideSpec{Model: ptrS("model-a")}); err != nil {
		t.Fatalf("A Set 1: %v", err)
	}
	// B resolves + runs → model-a.
	req := runWithOverrides(t, &capturingLLM{}, "t1", resolveOverridesPolicy(t, replicaB, "t1"))
	if req.Model != "model-a" {
		t.Fatalf("B run 1 model = %q, want model-a", req.Model)
	}
	// A re-sets to model-b; B (already loaded t1) must see it.
	if err := st.policy.Set(context.Background(), ovActor("t1"), "t1",
		governance.TenantOverrideSpec{Model: ptrS("model-b")}); err != nil {
		t.Fatalf("A Set 2: %v", err)
	}
	req = runWithOverrides(t, &capturingLLM{}, "t1", resolveOverridesPolicy(t, replicaB, "t1"))
	if req.Model != "model-b" {
		t.Errorf("B run 2 model = %q, want model-b (stale cross-replica cache)", req.Model)
	}
}

// resolveOverridesPolicy is resolveOverrides for an arbitrary policy
// instance (the Phase 92 helper is bound to st.policy).
func resolveOverridesPolicy(t *testing.T, pol *governance.TenantOverridePolicy, tenant string) *planner.LLMOverrides {
	t.Helper()
	spec, set, err := pol.Get(context.Background(), tenant)
	if err != nil {
		t.Fatalf("policy.Get(%q): %v", tenant, err)
	}
	if !set {
		return nil
	}
	return &planner.LLMOverrides{
		Model:             spec.Model,
		Temperature:       spec.Temperature,
		MaxTokens:         spec.MaxTokens,
		ReasoningEffort:   spec.ReasoningEffort,
		ExtraInstructions: spec.ExtraInstructions,
	}
}
