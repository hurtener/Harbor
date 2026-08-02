package integration_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/drivers/localdb" // test harness: real skills store on the seam (§4.4 carve-out)
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestE2E_DurableUserSkills wires the durable-per-user skills tier (D-345)
// end-to-end with REAL drivers on every seam — the localdb SkillStore, the
// StateStore-backed agent-config registry, the real Protocol Service + wire
// handler, the session-overlay store, and the run-start projection. It proves:
// a CLAIM-FREE caller authors a durable user skill; it is visible from a
// DIFFERENT session of the same (tenant, user); the run-start projection keeps
// it visible when the admin pins a skills membership that excludes it; a
// different user never sees it; and an incomplete identity fails loud at the
// edge with identity_required. Runs under -race.
func TestE2E_DurableUserSkills(t *testing.T) {
	const (
		dusTenant = "tenant-dus"
		dusAgent  = "agent-dus"
	)

	// --- real drivers on every seam ---
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
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	ov, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	store, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: filepath.Join(t.TempDir(), "skills.sqlite")},
		skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("localdb.New: %v", err)
	}
	authority, err := serve.NewSessionPersonalSkillAuthority(context.Background(), st, store, []config.SessionPersonalCutoverTenant{{
		TenantID: dusTenant, Epoch: "fixture-state-only", RosterDigest: "fixture", LegacyWritersDrained: true,
	}})
	if err != nil {
		t.Fatalf("session personal authority: %v", err)
	}
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithSkillStore(store),
		agentcfgprotocol.WithSessionOverlay(ov),
		agentcfgprotocol.WithSessionPersonalSkillController(authority.Controller),
		agentcfgprotocol.WithBus(bus))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc,
		stream.WithAgentConfigReachAuthorizer(auth.NewAgentReachAuthorizer()))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close(context.Background())
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})

	dus := &usHarness{handler: h, registry: reg, bus: bus, agentReach: []string{dusAgent}}
	alice := identity.Identity{TenantID: dusTenant, UserID: "alice", SessionID: "s-write"}
	aliceRead := identity.Identity{TenantID: dusTenant, UserID: "alice", SessionID: "s-read"}
	bob := identity.Identity{TenantID: dusTenant, UserID: "bob", SessionID: "s-read"}
	noScope := []auth.Scope{} // CLAIM-FREE: a valid identity is enough
	// dusAgent is explicitly reachable in this direct fixture. Provision its
	// real lifecycle before the claim-free session-owned skill path runs.
	activateFixtureAgent(t, reg, alice, dusAgent)

	// --- (1) claim-free upsert; scope forced to user ---
	up := decode[prototypes.AgentConfigUserSkillsUpsertResponse](t, dus.call(t,
		"/v1/agent_config/user/skills/upsert", prototypes.AgentConfigUserSkillsUpsertRequest{
			AgentID: dusAgent,
			Skill: prototypes.AgentConfigSkillInput{
				Name: "durable-a", Trigger: "when asked", Steps: []string{"do the durable thing"},
				Origin: "generated", Scope: "tenant", // ignored — forced to user
			},
		}, alice, noScope))
	if up.Skill.Scope != "user" {
		t.Fatalf("scope not forced to user: %q", up.Skill.Scope)
	}
	if up.Revision.AuthorTenant != dusTenant || up.Revision.AuthorUser != "alice" {
		t.Fatalf("membership author anchor not the real (tenant, user): %+v", up.Revision)
	}

	// --- (2) durable across sessions: visible from a DIFFERENT session ---
	bList := decode[prototypes.AgentConfigUserSkillsListResponse](t, dus.call(t,
		"/v1/agent_config/user/skills/list", prototypes.AgentConfigUserSkillsListRequest{AgentID: dusAgent},
		aliceRead, noScope))
	if len(bList.Skills) != 1 || bList.Skills[0].Name != "durable-a" {
		t.Fatalf("durable skill not visible from a different session: %+v", bList.Skills)
	}

	// --- (3) cross-user isolation ---
	bobList := decode[prototypes.AgentConfigUserSkillsListResponse](t, dus.call(t,
		"/v1/agent_config/user/skills/list", prototypes.AgentConfigUserSkillsListRequest{AgentID: dusAgent},
		bob, noScope))
	if len(bobList.Skills) != 0 {
		t.Fatalf("bob must not see alice's durable skill: %+v", bobList.Skills)
	}

	// --- (4) run-start projection seam: durable user skill survives an admin
	// membership pin that excludes it. Author a session-scoped "admin-a" so its
	// body is present in alice's write-session view, then admin-pin {admin-a}. ---
	ctxA, err := identity.WithRun(context.Background(), alice, "run-1")
	if err != nil {
		t.Fatalf("ctx: %v", err)
	}
	adminA := skills.Skill{Name: "admin-a", Trigger: "t", Steps: []string{"s"}, Origin: skills.OriginGenerated, Scope: skills.ScopeSession}
	adminA.ContentHash = skills.CanonicalContentHash(adminA)
	ephem := skills.Skill{Name: "ephemeral-x", Trigger: "t", Steps: []string{"s"}, Origin: skills.OriginGenerated, Scope: skills.ScopeSession}
	ephem.ContentHash = skills.CanonicalContentHash(ephem)
	aliceQ := identity.Quadruple{Identity: alice}
	if err := store.Upsert(ctxA, aliceQ, adminA); err != nil {
		t.Fatalf("seed admin-a: %v", err)
	}
	if err := store.Upsert(ctxA, aliceQ, ephem); err != nil {
		t.Fatalf("seed ephemeral-x: %v", err)
	}
	if _, err := reg.SetRevision(ctxA, aliceQ, dusAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"admin-a"}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("admin membership pin: %v", err)
	}

	dir, err := skills.NewDirectory(store, skills.Deps{Bus: bus}, skills.DirectoryConfig{MaxEntries: 20})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	views, err := dir.View(ctxA, skills.DirectoryCapability{})
	if err != nil {
		t.Fatalf("Directory.View: %v", err)
	}
	gated, err := projection.ActiveSkillViews(ctxA, reg, ov, dusAgent, aliceQ, views)
	if err != nil {
		t.Fatalf("ActiveSkillViews: %v", err)
	}
	got := map[string]bool{}
	for _, v := range gated {
		got[v.Name] = true
	}
	if !got["admin-a"] {
		t.Fatalf("admin-pinned skill missing from projection: %v", got)
	}
	if !got["durable-a"] {
		t.Fatalf("durable user skill dropped by admin membership pin (union broken): %v", got)
	}
	if got["ephemeral-x"] {
		t.Fatalf("non-member session skill leaked through projection: %v", got)
	}

	// --- (5) failure mode: incomplete identity fails loud at the edge ---
	bad := dus.call(t, "/v1/agent_config/user/skills/upsert", prototypes.AgentConfigUserSkillsUpsertRequest{
		AgentID: dusAgent,
		Skill:   prototypes.AgentConfigSkillInput{Name: "x", Trigger: "t", Steps: []string{"s"}, Origin: "generated", Scope: "user"},
	}, identity.Identity{TenantID: dusTenant, UserID: "", SessionID: "s"}, noScope)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("incomplete identity should be 401; got %d", bad.Code)
	}
	if c := errCode(t, bad); c != protoerrors.CodeIdentityRequired {
		t.Fatalf("incomplete identity code = %q, want %q", c, protoerrors.CodeIdentityRequired)
	}
}
