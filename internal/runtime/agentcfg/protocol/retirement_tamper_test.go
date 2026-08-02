package protocol_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

type tamperedRetirementRegistry struct {
	agentcfg.Registry
	completeCalls atomic.Int64
}

func (r *tamperedRetirementRegistry) Retire(_ context.Context, _ identity.Quadruple, _ string, req agentcfg.RetirementRequest) (agentcfg.RetirementStatus, error) {
	return agentcfg.RetirementStatus{OperationID: req.OperationID, Cleanup: []agentcfg.CleanupStep{{Class: "mcp_connection", Resource: "altered-resource"}}}, nil
}

func (r *tamperedRetirementRegistry) RetirementStatus(context.Context, identity.Quadruple, string) (agentcfg.RetirementStatus, bool, error) {
	return agentcfg.RetirementStatus{}, true, agentcfg.ErrRetirementConflict
}

func (r *tamperedRetirementRegistry) CompleteRetirementStep(context.Context, identity.Quadruple, string, string, string, string) (agentcfg.RetirementStatus, error) {
	r.completeCalls.Add(1)
	return agentcfg.RetirementStatus{}, nil
}

type retirementCountingDetacher struct {
	calls atomic.Int64
}

func (d *retirementCountingDetacher) DetachConnection(context.Context, string, string, string) error {
	d.calls.Add(1)
	return nil
}

func TestRetire_TamperedStatusFailsBeforeExternalTeardown(t *testing.T) {
	reg := &tamperedRetirementRegistry{Registry: newRegistry(t)}
	detacher := &retirementCountingDetacher{}
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithConnectionDetacher(detacher))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Retire(context.Background(), prototypes.AgentConfigRetireRequest{
		Identity:            scope(),
		AgentID:             testAgentID,
		OperationID:         "tampered-operation",
		ExpectedContentHash: agentcfg.ExpectNoActiveRevision,
	})
	if !errors.Is(err, agentcfg.ErrRetirementConflict) {
		t.Fatalf("Retire error=%v, want tamper conflict", err)
	}
	if detacher.calls.Load() != 0 || reg.completeCalls.Load() != 0 {
		t.Fatalf("tampered status caused side effects: detach=%d complete=%d", detacher.calls.Load(), reg.completeCalls.Load())
	}
}

func TestRetire_UnfencedAbsentPersonalTargetFailsBeforeExternalTeardown(t *testing.T) {
	ctx := context.Background()
	st, err := newStateStore(t)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: 30 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
	})

	admin := identity.Quadruple{Identity: identity.Identity{TenantID: scope().Tenant, UserID: scope().User, SessionID: scope().Session}}
	target := identity.Quadruple{Identity: identity.Identity{TenantID: admin.TenantID, UserID: "missing-user", SessionID: "missing-session"}}
	revision, err := reg.SetRevision(ctx, admin, testAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"history"}}}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	personal, err := sessionoverlay.NewDurableStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	personalSkill := skills.Skill{Name: "missing", Trigger: "when needed", Steps: []string{"do it"}, Origin: skills.OriginGenerated, Scope: skills.ScopeSession}
	if _, err := personal.SavePersonal(ctx, target, testAgentID, personalSkill, "", ""); err != nil {
		t.Fatal(err)
	}
	const operation = "unfenced-service-operation"
	status, err := reg.(agentcfg.RetirementRegistry).Retire(ctx, admin, testAgentID, agentcfg.RetirementRequest{OperationID: operation, ExpectedContentHash: revision.ContentHash})
	if err != nil || len(status.Cleanup) != 1 || status.Cleanup[0].Class != "session_personal" {
		t.Fatalf("frozen status=(%+v,%v)", status, err)
	}
	kind, err := sessionoverlay.PersonalSkillKind(testAgentID, personalSkill.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, target, kind); err != nil {
		t.Fatal(err)
	}

	detacher := &retirementCountingDetacher{}
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithConnectionDetacher(detacher))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Retire(ctx, prototypes.AgentConfigRetireRequest{
		Identity: scope(), AgentID: testAgentID, OperationID: operation, ExpectedContentHash: revision.ContentHash,
	})
	if !errors.Is(err, sessionoverlay.ErrStateUnavailable) {
		t.Fatalf("Retire error=%v, want ErrStateUnavailable", err)
	}
	if detacher.calls.Load() != 0 {
		t.Fatalf("unfenced absent target caused external teardown: detach=%d", detacher.calls.Load())
	}
	if _, err := st.Load(ctx, target, kind); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("missing target changed: %v", err)
	}
}
