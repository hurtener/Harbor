// Package integration_test — cross-subsystem E2E for the per-agent
// LLM-parameters layer of the agent-config control plane. It proves the
// three-layer run-start resolution session › per-agent › tenant-wide
// baseline › config reaches a REAL ReAct planner's LLM request, using the
// PRODUCTION composition (runsprotocol.ComposeLLMOverrides) and the
// PRODUCTION run-start projection (projection.ActiveLLMOverrides) — the same
// pieces cmd/harbor's run loop and the devstack twin call (no re-implemented
// copy, CLAUDE.md §17.4).
//
// Reuses the tenant-override harness in tenant_overrides_test.go
// (capturingLLM, runWithOverrides, resolveOverrides, newTenantOverrideStack,
// ovActor, ptrS/ptrF).
package integration_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
)

const llmParamsAgentID = "agent-llmparams"

// agentCfgFixture wires a real agent-config registry + Protocol Service over
// the SAME StateStore + bus the tenant-override stack uses, so per-agent and
// tenant-wide layers share one persistence floor.
type agentCfgFixture struct {
	reg agentcfg.Registry
	svc *agentcfgprotocol.Service
}

func newAgentCfgFixture(t *testing.T, stk tenantOverrideStack) agentCfgFixture {
	t.Helper()
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: stk.state, Bus: stk.bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithValidModels([]string{"model-a", "model-b"}))
	if err != nil {
		t.Fatalf("agentcfg NewService: %v", err)
	}
	return agentCfgFixture{reg: reg, svc: svc}
}

// llmTestTenant is the tenant every per-agent LLM-params E2E is scoped to
// (cross-tenant isolation is exercised separately via a direct projection
// call against a second tenant).
const llmTestTenant = "t1"

// agentScope is the admin identity scope a per-agent edit is authored under.
func agentScope() prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: llmTestTenant, User: "admin", Session: "admin-sess"}
}

// composeForRun mirrors cmd/harbor::resolveLLMOverrides exactly: tenant
// baseline (resolver) under per-agent (projection) under session (consumed),
// via the production ComposeLLMOverrides.
func composeForRun(t *testing.T, fx agentCfgFixture, stk tenantOverrideStack, agentID string, session *runsprotocol.PendingOverride) *planner.LLMOverrides {
	t.Helper()
	tenantLayer := resolveOverrides(t, stk.policy, llmTestTenant)
	q := identity.Quadruple{Identity: identity.Identity{TenantID: llmTestTenant, UserID: "u", SessionID: "s"}}
	agentLayer, err := projection.ActiveLLMOverrides(context.Background(), fx.reg, agentID, q)
	if err != nil {
		t.Fatalf("ActiveLLMOverrides: %v", err)
	}
	return runsprotocol.ComposeLLMOverrides(session, agentLayer, tenantLayer)
}

// TestE2E_AgentCfgLLMParams_PerAgentOverridesTenantBaseline proves the
// per-agent model overrides the tenant-wide baseline and the composed result
// reaches the real planner's LLM request; a DIFFERENT agent (no per-agent
// pin) falls back to the tenant baseline.
func TestE2E_AgentCfgLLMParams_PerAgentOverridesTenantBaseline(t *testing.T) {
	stk := newTenantOverrideStack(t, nil)
	fx := newAgentCfgFixture(t, stk)

	// Admin tenant-wide baseline: model-a + temperature 0.2.
	if err := stk.policy.Set(context.Background(), ovActor("t1"), "t1", governance.TenantOverrideSpec{Model: ptrS("model-a"), Temperature: ptrF(0.2)}); err != nil {
		t.Fatalf("tenant Set: %v", err)
	}
	// Per-agent pin for llmParamsAgentID: model-b (overrides the baseline).
	if _, err := fx.svc.SetLLMParams(context.Background(), prototypes.AgentConfigSetLLMParamsRequest{
		Identity: agentScope(), AgentID: llmParamsAgentID,
		LLMParams: prototypes.AgentConfigLLMParams{Model: ptrS("model-b")},
	}); err != nil {
		t.Fatalf("SetLLMParams: %v", err)
	}

	// The pinned agent uses model-b; temperature falls through to the tenant 0.2.
	req := runWithOverrides(t, &capturingLLM{}, "t1", composeForRun(t, fx, stk, llmParamsAgentID, nil))
	if req.Model != "model-b" {
		t.Errorf("pinned agent: req.Model = %q, want model-b (per-agent over tenant)", req.Model)
	}
	if req.Temperature == nil || *req.Temperature != float32(0.2) {
		t.Errorf("pinned agent: req.Temperature = %v, want tenant 0.2 (per-agent unset)", req.Temperature)
	}

	// A different agent has no per-agent pin → tenant baseline model-a.
	reqOther := runWithOverrides(t, &capturingLLM{}, "t1", composeForRun(t, fx, stk, "some-other-agent", nil))
	if reqOther.Model != "model-a" {
		t.Errorf("unpinned agent: req.Model = %q, want tenant model-a", reqOther.Model)
	}
}

// TestE2E_AgentCfgLLMParams_SessionBeatsPerAgent proves a one-shot session
// override (runs.set_overrides) wins over the per-agent pin for that run.
func TestE2E_AgentCfgLLMParams_SessionBeatsPerAgent(t *testing.T) {
	stk := newTenantOverrideStack(t, nil)
	fx := newAgentCfgFixture(t, stk)
	if _, err := fx.svc.SetLLMParams(context.Background(), prototypes.AgentConfigSetLLMParamsRequest{
		Identity: agentScope(), AgentID: llmParamsAgentID,
		LLMParams: prototypes.AgentConfigLLMParams{Model: ptrS("model-a")},
	}); err != nil {
		t.Fatalf("SetLLMParams: %v", err)
	}
	session := &runsprotocol.PendingOverride{Model: ptrS("model-b")}
	req := runWithOverrides(t, &capturingLLM{}, "t1", composeForRun(t, fx, stk, llmParamsAgentID, session))
	if req.Model != "model-b" {
		t.Errorf("req.Model = %q, want model-b (session beats per-agent)", req.Model)
	}
}

// TestE2E_AgentCfgLLMParams_RollbackRestoresBaseline proves rolling back the
// per-agent revision restores the tenant baseline for the agent's next run.
func TestE2E_AgentCfgLLMParams_RollbackRestoresBaseline(t *testing.T) {
	stk := newTenantOverrideStack(t, nil)
	fx := newAgentCfgFixture(t, stk)
	if err := stk.policy.Set(context.Background(), ovActor("t1"), "t1", governance.TenantOverrideSpec{Model: ptrS("model-a"), Temperature: ptrF(0.2)}); err != nil {
		t.Fatalf("tenant Set: %v", err)
	}
	// Revision 1: an empty payload (a base to roll back TO).
	rev1, err := fx.svc.SetRevision(context.Background(), prototypes.AgentConfigSetRevisionRequest{
		Identity: agentScope(), AgentID: llmParamsAgentID,
		Payload: prototypes.AgentConfigPayload{Skills: &prototypes.AgentConfigSkillsSelection{Names: []string{"a"}}},
	})
	if err != nil {
		t.Fatalf("SetRevision base: %v", err)
	}
	// Revision 2: pin model-b.
	if _, err := fx.svc.SetLLMParams(context.Background(), prototypes.AgentConfigSetLLMParamsRequest{
		Identity: agentScope(), AgentID: llmParamsAgentID,
		LLMParams: prototypes.AgentConfigLLMParams{Model: ptrS("model-b")},
	}); err != nil {
		t.Fatalf("SetLLMParams: %v", err)
	}
	// With the pin active, the run uses model-b.
	if req := runWithOverrides(t, &capturingLLM{}, "t1", composeForRun(t, fx, stk, llmParamsAgentID, nil)); req.Model != "model-b" {
		t.Fatalf("pre-rollback: req.Model = %q, want model-b", req.Model)
	}
	// Roll back to revision 1 (no LLM-params) → the run falls back to the tenant baseline.
	if _, err := fx.svc.Rollback(context.Background(), prototypes.AgentConfigRollbackRequest{
		Identity: agentScope(), AgentID: llmParamsAgentID, RevisionID: rev1.Revision.RevisionID,
	}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if req := runWithOverrides(t, &capturingLLM{}, "t1", composeForRun(t, fx, stk, llmParamsAgentID, nil)); req.Model != "model-a" {
		t.Errorf("post-rollback: req.Model = %q, want tenant baseline model-a", req.Model)
	}
}

// TestE2E_AgentCfgLLMParams_CrossTenantIsolation proves a per-agent pin under
// one tenant is invisible to the same agent id under another tenant
// (agent_id is the registry key, never an isolation principal — §6).
func TestE2E_AgentCfgLLMParams_CrossTenantIsolation(t *testing.T) {
	stk := newTenantOverrideStack(t, nil)
	fx := newAgentCfgFixture(t, stk)
	if _, err := fx.svc.SetLLMParams(context.Background(), prototypes.AgentConfigSetLLMParamsRequest{
		Identity: agentScope(), AgentID: llmParamsAgentID,
		LLMParams: prototypes.AgentConfigLLMParams{Model: ptrS("model-b")},
	}); err != nil {
		t.Fatalf("SetLLMParams t1: %v", err)
	}
	// t2's same agent id has no pin → nil per-agent layer.
	q2 := identity.Quadruple{Identity: identity.Identity{TenantID: "t2", UserID: "u", SessionID: "s"}}
	agentLayer, err := projection.ActiveLLMOverrides(context.Background(), fx.reg, llmParamsAgentID, q2)
	if err != nil {
		t.Fatalf("ActiveLLMOverrides t2: %v", err)
	}
	if agentLayer != nil {
		t.Errorf("tenant 2 saw tenant 1's per-agent pin: %+v", agentLayer)
	}
}

// TestE2E_AgentCfgLLMParams_ConcurrentResolution runs N≥100 concurrent
// run-start resolutions against a SINGLE shared registry under -race,
// asserting each goroutine sees ONLY its own agent's pinned model (no
// cross-agent / cross-tenant bleed; the projection reads a fresh bundle per
// call).
func TestE2E_AgentCfgLLMParams_ConcurrentResolution(t *testing.T) {
	stk := newTenantOverrideStack(t, nil)
	fx := newAgentCfgFixture(t, stk)

	// Two agents pinned to different models on the shared registry.
	for agentID, model := range map[string]string{"agent-A": "model-a", "agent-B": "model-b"} {
		if _, err := fx.svc.SetLLMParams(context.Background(), prototypes.AgentConfigSetLLMParamsRequest{
			Identity: agentScope(), AgentID: agentID,
			LLMParams: prototypes.AgentConfigLLMParams{Model: ptrS(model)},
		}); err != nil {
			t.Fatalf("SetLLMParams %s: %v", agentID, err)
		}
	}

	const n = 200
	baseline := runtime.NumGoroutine()
	errCh := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			agentID, want := "agent-A", "model-a"
			if i%2 == 1 {
				agentID, want = "agent-B", "model-b"
			}
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "t1", UserID: "u", SessionID: "s"}}
			ov, err := projection.ActiveLLMOverrides(context.Background(), fx.reg, agentID, q)
			if err != nil {
				errCh <- err
				return
			}
			if ov == nil || ov.Model == nil || *ov.Model != want {
				errCh <- fmt.Errorf("agent %s resolved model %v, want %s (cross-agent bleed)", agentID, modelOf(ov), want)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// Goroutine baseline restored — ActiveLLMOverrides is synchronous and
	// must spawn nothing per resolution (§11 leak clause).
	var after int
	for range 50 {
		after = runtime.NumGoroutine()
		if after <= baseline+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after > baseline+2 {
		t.Errorf("goroutine leak: baseline=%d after=%d", baseline, after)
	}
}

func modelOf(ov *planner.LLMOverrides) string {
	if ov == nil || ov.Model == nil {
		return "<nil>"
	}
	return *ov.Model
}
