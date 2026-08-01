// Package integration_test — cross-subsystem E2E for the admin-set
// tenant-default LLM override (the RFC §6.15 ModelOverride governance
// seam). Real drivers on the seam: a real inmem StateStore + EventBus
// back the governance.TenantOverridePolicy; a real react planner consumes
// the resolved planner.LLMOverrides through its capturing LLM client. The
// test proves the run-start resolution → apply path end-to-end, the
// tenant-default › config resolution order, tenant isolation, next-turn
// re-resolution, the audit event, a failure mode (state read error fails
// loud), and a concurrency stress.
//
// The spec→LLMOverrides projection mirrors cmd/harbor's
// perTaskRunLoopDriver.resolveLLMOverrides (unexported in package main);
// the production projection itself is unit-tested in
// cmd/harbor/cmd_dev_runloop_test.go, and the live run-loop wiring is
// exercised by the Console live verification. Here the seam under test is
// the real policy + real planner.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// capturingLLM records the last request and returns a Finish tool-call.
type capturingLLM struct {
	mu   sync.Mutex
	last llm.CompleteRequest
}

func (c *capturingLLM) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.mu.Lock()
	c.last = req
	c.mu.Unlock()
	return llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{{
			ID:   "call_done",
			Name: "_finish",
			Args: json.RawMessage(`{"answer":"ok"}`),
		}},
	}, nil
}

func (c *capturingLLM) lastReq() llm.CompleteRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

func (c *capturingLLM) Close(context.Context) error { return nil }

// resolveOverrides mirrors the production run-start projection
// (cmd/harbor::perTaskRunLoopDriver.resolveLLMOverrides): read the tenant
// default and project the set record onto the planner bundle. A nil return
// means "no override — use agent/config defaults".
func resolveOverrides(t *testing.T, pol *governance.TenantOverridePolicy, tenant string) *planner.LLMOverrides {
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

func runWithOverrides(t *testing.T, cap *capturingLLM, tenant string, ov *planner.LLMOverrides) llm.CompleteRequest {
	t.Helper()
	p := react.New(cap)
	rc := planner.RunContext{
		Quadruple: identity.Quadruple{
			Identity: identity.Identity{TenantID: tenant, UserID: "u", SessionID: "s"},
			RunID:    "r-" + tenant,
		},
		Goal:         "do the thing",
		LLMOverrides: ov,
	}
	if _, err := p.Next(context.Background(), rc); err != nil {
		t.Fatalf("planner.Next: %v", err)
	}
	return cap.lastReq()
}

type tenantOverrideStack struct {
	policy *governance.TenantOverridePolicy
	bus    events.EventBus
	state  state.StateStore
}

func newTenantOverrideStack(t *testing.T, st state.StateStore) tenantOverrideStack {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	if st == nil {
		st, err = state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
		if err != nil {
			t.Fatalf("state.Open: %v", err)
		}
	}
	pol, err := governance.NewTenantOverridePolicy(st, bus, []string{"model-a", "model-b"}, nil)
	if err != nil {
		t.Fatalf("NewTenantOverridePolicy: %v", err)
	}
	t.Cleanup(func() {
		_ = pol.Close(context.Background())
		_ = st.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	return tenantOverrideStack{policy: pol, bus: bus, state: st}
}

func ovActor(tenant string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "admin", SessionID: "admin-sess"}}
}

func ptrS(s string) *string   { return &s }
func ptrF(f float64) *float64 { return &f }

// TestE2E_TenantOverrides_ResolutionAndApply proves the tenant-default ›
// config resolution order and that the resolved override lands on the
// planner's LLM request. It also asserts the audit event fires.
func TestE2E_TenantOverrides_ResolutionAndApply(t *testing.T) {
	st := newTenantOverrideStack(t, nil)

	// No record yet → resolve nil → planner request carries no model
	// (the safety edge would fill cfg.Model downstream).
	reqBefore := runWithOverrides(t, &capturingLLM{}, "t1", resolveOverrides(t, st.policy, "t1"))
	if reqBefore.Model != "" {
		t.Errorf("no tenant default: req.Model = %q, want empty", reqBefore.Model)
	}

	sub, err := st.bus.Subscribe(context.Background(), events.Filter{
		Tenant: "t1", User: "admin", Session: "admin-sess",
		Types: []events.EventType{governance.EventTypeTenantOverridesSet},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Admin sets a tenant default (model + temp + additive extra).
	if err := st.policy.Set(context.Background(), ovActor("t1"), "t1", governance.TenantOverrideSpec{
		Model:             ptrS("model-b"),
		Temperature:       ptrF(0.3),
		ExtraInstructions: ptrS("ALWAYS answer in French."),
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Audit event observed.
	select {
	case ev := <-sub.Events():
		if ev.Type != governance.EventTypeTenantOverridesSet {
			t.Fatalf("event type = %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no tenant_overrides_set event")
	}

	// Next run for t1 picks up the default — applied to the request.
	cap := &capturingLLM{}
	req := runWithOverrides(t, cap, "t1", resolveOverrides(t, st.policy, "t1"))
	if req.Model != "model-b" {
		t.Errorf("req.Model = %q, want model-b", req.Model)
	}
	if req.Temperature == nil || *req.Temperature != float32(0.3) {
		t.Errorf("req.Temperature = %v, want 0.3", req.Temperature)
	}
	if !reqBodyContains(req.Messages, "ALWAYS answer in French.") {
		t.Errorf("system prompt missing additive extra-instructions")
	}
}

// TestE2E_TenantOverrides_Isolation proves a tenant-A default never
// reaches tenant B's run.
func TestE2E_TenantOverrides_Isolation(t *testing.T) {
	st := newTenantOverrideStack(t, nil)
	if err := st.policy.Set(context.Background(), ovActor("A"), "A",
		governance.TenantOverrideSpec{Model: ptrS("model-a")}); err != nil {
		t.Fatalf("Set A: %v", err)
	}
	reqB := runWithOverrides(t, &capturingLLM{}, "B", resolveOverrides(t, st.policy, "B"))
	if reqB.Model != "" {
		t.Errorf("tenant B leaked A's model: %q", reqB.Model)
	}
	reqA := runWithOverrides(t, &capturingLLM{}, "A", resolveOverrides(t, st.policy, "A"))
	if reqA.Model != "model-a" {
		t.Errorf("tenant A lost its model: %q", reqA.Model)
	}
}

// TestE2E_TenantOverrides_NextTurn proves a re-set changes the NEXT run's
// model (the run-start snapshot re-resolves each run).
func TestE2E_TenantOverrides_NextTurn(t *testing.T) {
	st := newTenantOverrideStack(t, nil)
	if err := st.policy.Set(context.Background(), ovActor("t1"), "t1",
		governance.TenantOverrideSpec{Model: ptrS("model-a")}); err != nil {
		t.Fatalf("Set 1: %v", err)
	}
	req1 := runWithOverrides(t, &capturingLLM{}, "t1", resolveOverrides(t, st.policy, "t1"))
	if req1.Model != "model-a" {
		t.Fatalf("run 1 model = %q, want model-a", req1.Model)
	}
	if err := st.policy.Set(context.Background(), ovActor("t1"), "t1",
		governance.TenantOverrideSpec{Model: ptrS("model-b")}); err != nil {
		t.Fatalf("Set 2: %v", err)
	}
	req2 := runWithOverrides(t, &capturingLLM{}, "t1", resolveOverrides(t, st.policy, "t1"))
	if req2.Model != "model-b" {
		t.Errorf("run 2 model = %q, want model-b (next-turn re-resolution)", req2.Model)
	}
}

// failOnLoadState wraps a StateStore and forces Load to error, simulating
// a state-read failure at resolution time.
type failOnLoadState struct {
	state.StateStore
}

func (f failOnLoadState) Load(context.Context, identity.Quadruple, string) (state.StateRecord, error) {
	return state.StateRecord{}, errors.New("simulated state read failure")
}

func (f failOnLoadState) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	return f.StateStore.SaveIf(ctx, expectations, next)
}

// TestE2E_TenantOverrides_StateFailure_FailsLoud proves a resolution-time
// state read error surfaces rather than silently dropping the override
// (CLAUDE.md §13). The run loop turns this into a failed run.
func TestE2E_TenantOverrides_StateFailure_FailsLoud(t *testing.T) {
	base, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	st := newTenantOverrideStack(t, failOnLoadState{StateStore: base})
	if _, _, err := st.policy.Get(context.Background(), "t1"); err == nil {
		t.Fatal("want state read error to surface, got nil")
	} else if !errors.Is(err, governance.ErrStateUnavailable) {
		t.Errorf("want ErrStateUnavailable, got %v", err)
	}
}

// TestE2E_TenantOverrides_ConcurrencyStress runs N≥10 concurrent
// set+resolve+apply chains across distinct tenants against one shared
// policy + planner client, asserting no cross-talk under -race.
func TestE2E_TenantOverrides_ConcurrencyStress(t *testing.T) {
	st := newTenantOverrideStack(t, nil)
	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			tenant := "tenant-" + itoaOv(i)
			model := "model-a"
			if i%2 == 0 {
				model = "model-b"
			}
			if err := st.policy.Set(context.Background(), ovActor(tenant), tenant,
				governance.TenantOverrideSpec{Model: ptrS(model)}); err != nil {
				t.Errorf("%s Set: %v", tenant, err)
				return
			}
			req := runWithOverrides(t, &capturingLLM{}, tenant, resolveOverrides(t, st.policy, tenant))
			if req.Model != model {
				t.Errorf("%s cross-talk: req.Model = %q, want %q", tenant, req.Model, model)
			}
		}(i)
	}
	wg.Wait()
}

// reqBodyContains reports whether any message's textual content contains s.
func reqBodyContains(msgs []llm.ChatMessage, s string) bool {
	for _, m := range msgs {
		if m.Content.Text != nil && strings.Contains(*m.Content.Text, s) {
			return true
		}
		for _, p := range m.Content.Parts {
			if p.Text != "" && strings.Contains(p.Text, s) {
				return true
			}
		}
	}
	return false
}

func itoaOv(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
