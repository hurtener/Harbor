package serve

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	llmgrant "github.com/hurtener/Harbor/internal/llm/grant"
	"github.com/hurtener/Harbor/internal/llm/leases"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

type agentBoundProvider struct {
	calls  atomic.Int32
	tokens int
}

func (p *agentBoundProvider) Complete(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
	p.calls.Add(1)
	tokens := p.tokens
	if tokens == 0 {
		tokens = 1
	}
	return llm.CompleteResponse{Content: "done", Usage: llm.Usage{TotalTokens: tokens}}, nil
}

func (*agentBoundProvider) Close(context.Context) error { return nil }

type runLoopReasonedTopUpper struct {
	signer *llmgrant.Signer
	calls  atomic.Int32
}

func (t *runLoopReasonedTopUpper) TopUp(ctx context.Context, current llm.ExternalGrant, needed int64) (llm.ExternalGrant, error) {
	reason := llm.ExternalGrantRenewalExpired
	if current.Lease.RemainingTokens() < needed {
		reason = llm.ExternalGrantRenewalLeaseInsufficient
	}
	return t.Renew(ctx, current, needed, reason)
}

func (t *runLoopReasonedTopUpper) Renew(_ context.Context, current llm.ExternalGrant, needed int64, reason llm.ExternalGrantRenewalReason) (llm.ExternalGrant, error) {
	t.calls.Add(1)
	current.Lease.Epoch++
	if reason == llm.ExternalGrantRenewalLeaseInsufficient {
		current.Lease.TokenUnits += needed
	}
	current.IssuedAt = current.IssuedAt.Add(time.Second)
	current.ExpiresAt = current.ExpiresAt.Add(time.Second)
	current.Lease.ExpiresAt = current.Lease.ExpiresAt.Add(time.Second)
	return t.signer.Sign(current)
}

type immutableGrantRunLoopPlanner struct {
	client llm.LLMClient
	raw    json.RawMessage
}

func (p *immutableGrantRunLoopPlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	if string(rc.ExternalGrant) != string(p.raw) {
		return nil, llm.ErrExternalGrantInvalid
	}
	for step := 1; step <= 3; step++ {
		var root llm.ExternalGrant
		if err := json.Unmarshal(rc.ExternalGrant, &root); err != nil {
			return nil, err
		}
		maxTokens := 10
		if _, err := p.client.Complete(llm.WithAttemptStep(ctx, step), llm.CompleteRequest{Model: "model-fast", MaxTokens: &maxTokens, ExternalGrant: &root}); err != nil {
			return nil, err
		}
		if string(rc.ExternalGrant) != string(p.raw) {
			return nil, llm.ErrExternalGrantInvalid
		}
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

type agentBoundPlanner struct {
	client  llm.LLMClient
	signer  *llmgrant.Signer
	agentID string
	now     time.Time
	done    chan error
}

func (p *agentBoundPlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	grant, err := p.sign(rc)
	if err == nil {
		_, err = p.client.Complete(ctx, llm.CompleteRequest{Model: "model-fast", ExternalGrant: &grant})
	}
	select {
	case p.done <- err:
	default:
	}
	if err != nil {
		return nil, err
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

func (p *agentBoundPlanner) sign(rc planner.RunContext) (llm.ExternalGrant, error) {
	q := rc.Quadruple
	return p.signer.Sign(llm.ExternalGrant{
		Version: llm.ExternalGrantVersionAgentBound, GrantID: "grant-" + q.RunID,
		RouteMode: llm.ExternalGrantRouteRuntimeDefault, OrganizationID: "org-a", RuntimeID: "runtime-a", AgentID: p.agentID,
		TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID, LogicalRunID: q.RunID,
		PolicyGeneration: 1, MaxReasoning: llm.ReasoningLow, MaxOutputTokens: 16,
		Lease:    llm.ComputeLease{LeaseID: "lease-" + q.RunID, TokenUnits: 16, ExpiresAt: p.now.Add(time.Minute)},
		IssuedAt: p.now, ExpiresAt: p.now.Add(time.Minute),
	})
}

// TestRunLoop_V2GrantBindsExplicitAndDefaultReachAdmissionsAndRejectsForgedTaskAgent drives the real
// durable reach receipt, run loop, reference verifier, reservation store, and
// provider wrapper. The control.start producer half of this handoff is pinned
// by protocol_test.TestDispatchStart_NamedAgent_TwoCheckRule. An omitted task
// AgentID still binds the reach-admitted runtime default; the raw persisted
// selection is never used as authority.
func TestRunLoop_V2GrantBindsExplicitAndDefaultReachAdmissionsAndRejectsForgedTaskAgent(t *testing.T) {
	for _, tc := range []struct {
		name           string
		requestedAgent string
		effectiveAgent string
		admitted       bool
		wantProvider   int32
	}{
		{name: "explicit", requestedAgent: "agent-explicit", effectiveAgent: "agent-explicit", admitted: true, wantProvider: 1},
		{name: "default", requestedAgent: "", effectiveAgent: "agent-default", admitted: true, wantProvider: 1},
		{name: "forged_task_agent", requestedAgent: "agent-forged", effectiveAgent: "agent-forged", admitted: false, wantProvider: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
			signer, err := llmgrant.NewSigner("key-v2", "harbor-runtime", nil, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			verifier, err := llmgrant.NewVerifier(llmgrant.VerifierConfig{
				Audience: "harbor-runtime", RuntimeID: "runtime-a", AuthorizedOrganizations: []string{"org-a"},
				Keys: map[string]ed25519.PublicKey{"key-v2": signer.PublicKey()}, Clock: func() time.Time { return now },
				RouteMode: llm.ExternalGrantRouteRuntimeDefault,
			})
			if err != nil {
				t.Fatal(err)
			}
			stateStore, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stateStore.Close(context.Background()) }()
			reservations, err := leases.New(stateStore, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			provider := &agentBoundProvider{}
			client := llmgrant.Wrap(provider, llm.ConfigSnapshot{Provider: "mock", Model: "model-fast"}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
				Mode: llm.ExternalGrantRequired, RouteMode: llm.ExternalGrantRouteRuntimeDefault,
				Verifier: verifier, Reservations: reservations, ReceiptSink: testReceiptSink{}, ReceiptRequired: true,
			}})
			plannerProbe := &agentBoundPlanner{client: client, signer: signer, agentID: tc.effectiveAgent, now: now, done: make(chan error, 1)}

			red := auditpatterns.New()
			bus := mkDriverTestBus(t, red)
			reg := mkDriverTestTaskRegistry(t, bus, red)
			steerReg := steering.NewRegistry()
			coord := pauseresume.New(pauseresume.WithBus(bus))
			runLoop, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
			if err != nil {
				t.Fatal(err)
			}
			sealer, err := toolauth.NewAESGCMSealer(make([]byte, toolauth.KEKSizeBytes))
			if err != nil {
				t.Fatal(err)
			}
			authority, err := tasks.NewAgentReachAdmissionAuthority(sealer)
			if err != nil {
				t.Fatal(err)
			}
			driver, err := NewRunLoopDriver(RunLoopDriverOptions{
				Bus: bus, RunLoop: runLoop, Planner: plannerProbe, Tasks: reg,
				AgentConfigID: tc.effectiveAgent, AgentReachAdmissions: authority,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := driver.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = driver.Close(context.Background()) }()

			spawnCtx, err := identity.With(context.Background(), runLoopDriverTestID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.admitted {
				spawnCtx, err = authority.Admit(spawnCtx, runLoopDriverTestID, tc.effectiveAgent)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := reg.Spawn(spawnCtx, tasks.SpawnRequest{
				Identity: identity.Quadruple{Identity: runLoopDriverTestID}, Kind: tasks.KindForeground,
				Query: "agent-bound grant", AgentID: tc.requestedAgent,
			}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-plannerProbe.done:
				if tc.admitted && err != nil {
					t.Fatalf("grant-bearing Complete: %v", err)
				}
				if !tc.admitted && err == nil {
					t.Fatal("unadmitted task AgentID reached the provider")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("grant-bearing planner call did not finish")
			}
			if got := provider.calls.Load(); got != tc.wantProvider {
				t.Fatalf("provider calls=%d, want %d", got, tc.wantProvider)
			}
			if err := client.Close(context.Background()); err != nil {
				t.Fatalf("close grant client: %v", err)
			}
		})
	}
}

// TestRunLoop_ImmutableBaseGrantReusesAndRenewsDurableSuccessors proves the
// real run-loop Base carrier stays byte-identical while three LLM calls use
// the durable current successor. The first and third calls renew; the middle
// call reuses already-applied capacity without contacting the coordinator.
func TestRunLoop_ImmutableBaseGrantReusesAndRenewsDurableSuccessors(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	signer, err := llmgrant.NewSigner("key-runloop-renewal", "harbor-runtime", nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := llmgrant.NewVerifier(llmgrant.VerifierConfig{
		Audience: "harbor-runtime", RuntimeID: "runtime-a", AuthorizedOrganizations: []string{"org-a"},
		Keys: map[string]ed25519.PublicKey{"key-runloop-renewal": signer.PublicKey()}, Clock: func() time.Time { return now },
		RouteMode: llm.ExternalGrantRouteRuntimeDefault,
	})
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-runloop", UserID: "user-runloop", SessionID: "session-runloop"}, RunID: "run-runloop"}
	root, err := signer.Sign(llm.ExternalGrant{
		Version: llm.ExternalGrantVersionLegacy, GrantID: "grant-runloop", RouteMode: llm.ExternalGrantRouteRuntimeDefault,
		OrganizationID: "org-a", RuntimeID: "runtime-a", TenantID: q.TenantID, UserID: q.UserID,
		SessionID: q.SessionID, LogicalRunID: q.RunID, LogicalCallID: "call-runloop", AttemptNonce: "nonce-runloop",
		PolicyGeneration: 1, MaxReasoning: llm.ReasoningLow, MaxOutputTokens: 10,
		Lease:    llm.ComputeLease{LeaseID: "lease-runloop", Epoch: 1, TokenUnits: 5, ExpiresAt: now.Add(time.Minute)},
		IssuedAt: now.Add(-10 * time.Second), ExpiresAt: now.Add(30 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	stateStore, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stateStore.Close(context.Background()) }()
	durable, err := leases.New(stateStore, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	provider := &agentBoundProvider{tokens: 4}
	topUpper := &runLoopReasonedTopUpper{signer: signer}
	client := llmgrant.Wrap(provider, llm.ConfigSnapshot{Provider: "mock", Model: "model-fast"}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantRequired, RouteMode: llm.ExternalGrantRouteRuntimeDefault,
		Verifier: verifier, RenewalVerifier: verifier, Reservations: durable, Successors: durable, SuccessorResolver: durable,
		TopUpper: topUpper, ReceiptSink: testReceiptSink{}, ReceiptRequired: true,
	}})
	runLoop, err := steering.NewRunLoop(steering.NewRegistry(), pauseresume.New())
	if err != nil {
		t.Fatal(err)
	}
	plannerProbe := &immutableGrantRunLoopPlanner{client: client, raw: append(json.RawMessage(nil), raw...)}
	runCtx, err := identity.WithVerified(context.Background(), q.Identity)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, err = identity.WithRun(runCtx, q.Identity, q.RunID)
	if err != nil {
		t.Fatal(err)
	}
	runCtx = llm.WithVerifiedOrganization(runCtx, "org-a")
	if _, err := runLoop.Run(runCtx, steering.RunSpec{Planner: plannerProbe, Base: planner.RunContext{
		Quadruple: q, Goal: "exercise immutable grant", ExternalGrant: raw, Trajectory: &planner.Trajectory{Query: "exercise immutable grant"},
	}}); err != nil {
		t.Fatal(err)
	}
	if got := provider.calls.Load(); got != 3 {
		t.Fatalf("provider calls=%d, want 3", got)
	}
	if got := topUpper.calls.Load(); got != 2 {
		t.Fatalf("renewal calls=%d, want 2", got)
	}
}

var _ planner.Planner = (*agentBoundPlanner)(nil)
var _ llm.LLMClient = (*agentBoundProvider)(nil)
