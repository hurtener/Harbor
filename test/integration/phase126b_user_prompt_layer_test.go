package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestE2E_Phase126b_DurableUserPromptReachesRun is the §13 producer→consumer
// round-trip: a user_prompt written through 126a's REAL durable write path
// (Service.UserSetRevision, scope ConfigScopeUser) appears in the NEXT run's
// composed <user_instructions> block, in the correct precedence position,
// under the same (tenant, user) — and is isolated across users and invariant
// across the user's sessions.
func TestE2E_Phase126b_DurableUserPromptReachesRun(t *testing.T) {
	ctx := context.Background()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 32, SubscriberBufferSize: 256,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithBus(bus))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	ov, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}

	const agent = plAgentID
	alice := identity.Identity{TenantID: "t", UserID: "alice", SessionID: "s1"}
	aliceS2 := identity.Identity{TenantID: "t", UserID: "alice", SessionID: "s2"}
	bob := identity.Identity{TenantID: "t", UserID: "bob", SessionID: "sb"}
	// This direct fixture selects agent through real projection calls; declare
	// its agent-level lifecycle before the lower session tier is used.
	if _, err := reg.SetRevision(ctx, identity.Quadruple{Identity: alice}, agent, agentcfg.ConfigScopeAgent,
		agentcfg.ConfigPayload{}, agentcfg.SetOptions{ExpectedContentHash: agentcfg.ExpectNoActiveRevision}); err != nil {
		t.Fatalf("activate fixture agent: %v", err)
	}

	// PRODUCER — write alice's durable user_prompt through the real 126a verb.
	if _, err := svc.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
		Identity: prototypes.IdentityScope{Tenant: "t", User: "alice", Session: "s1"},
		AgentID:  agent,
		Payload:  prototypes.AgentConfigUserPayload{UserPrompt: "always answer in metric units"},
	}); err != nil {
		t.Fatalf("UserSetRevision: %v", err)
	}

	// CONSUMER — the durable layer reaches alice's next run with no admin or
	// session layer set (proves the durable layer alone reaches the run).
	got := userLayer(t, ctx, reg, ov, alice)
	if got != "always answer in metric units" {
		t.Fatalf("durable layer alone did not reach the run: %q", got)
	}

	// CROSS-SESSION INVARIANCE — alice's OTHER session sees the same durable
	// layer (it spans her sessions for the agent).
	if s2 := userLayer(t, ctx, reg, ov, aliceS2); s2 != "always answer in metric units" {
		t.Fatalf("durable layer not invariant across alice's sessions: %q", s2)
	}

	// CROSS-USER ISOLATION — bob's run composes WITHOUT alice's durable layer.
	if b := userLayer(t, ctx, reg, ov, bob); b != "" {
		t.Fatalf("cross-user bleed: bob's run carried alice's durable layer: %q", b)
	}

	// PRECEDENCE — with admin User + session overlay also set, the composed
	// block is admin User, then USER-durable, then session User, below the
	// always-spine admin Base.
	if _, err := reg.SetRevision(ctx, identity.Quadruple{Identity: alice}, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: ptr("operator base"), User: ptr("admin user layer")},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	if _, err := ov.SetUserPrompt(ctx, identity.Quadruple{Identity: alice}, agent, "session refinement"); err != nil {
		t.Fatalf("set session: %v", err)
	}
	full := userLayer(t, ctx, reg, ov, alice)
	want := "admin user layer\n\nalways answer in metric units\n\nsession refinement"
	if full != want {
		t.Fatalf("precedence wrong:\n got=%q\nwant=%q", full, want)
	}

	// FAIL-LOUD — a closed StateStore makes the projection's registry read
	// fail the run loudly rather than silently dropping a layer (the
	// durable-read-specific fail-loud path is isolated in the unit test
	// TestApplyPromptLayers_DurableReadError_FailsLoud).
	if err := st.Close(ctx); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if _, err := projection.ApplyPromptLayers(ctx, reg, ov, agent, identity.Quadruple{Identity: alice}, nil); err == nil {
		t.Fatal("a closed StateStore must fail the prompt-layer projection loudly, got nil")
	}
}

// plAgentID is the agent id used throughout this test's prompt-layer
// projection.
const plAgentID = "agent-pl"

// userLayer applies the prompt layers for id's run and returns the composed
// lower-trust user layer (empty string when none).
func userLayer(t *testing.T, ctx context.Context, reg agentcfg.Registry, ov sessionoverlay.Store, id identity.Identity) string {
	t.Helper()
	out, err := projection.ApplyPromptLayers(ctx, reg, ov, plAgentID, identity.Quadruple{Identity: id}, nil)
	if err != nil {
		t.Fatalf("ApplyPromptLayers: %v", err)
	}
	if out == nil || out.UserPromptLayer == nil {
		return ""
	}
	return strings.TrimSpace(*out.UserPromptLayer)
}

func ptr(s string) *string { return &s }
