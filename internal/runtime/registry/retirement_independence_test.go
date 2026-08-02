package registry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/registry"
)

// TestDeregister_IndependentOfAgentConfigRetirement proves fleet membership
// and the terminal agent-config lifecycle are deliberately separate. Neither
// operation may erase or synthesize the other subsystem's durable record.
func TestDeregister_IndependentOfAgentConfigRetirement(t *testing.T) {
	ctx := identityCtx(t, "tenant", "operator", "control")
	fleet, store, bus := newTestRegistry(t)
	config, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = config.Close(context.Background()) })

	rec, err := fleet.Register(ctx, "fleet-agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("fleet Register: %v", err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "operator", SessionID: "control"}}
	retirer := config.(agentcfg.RetirementRegistry)
	if _, err := retirer.Retire(context.Background(), q, rec.AgentID, agentcfg.RetirementRequest{
		OperationID: "retire-config-only", ExpectedContentHash: agentcfg.ExpectNoActiveRevision,
	}); err != nil {
		t.Fatalf("config Retire: %v", err)
	}
	if _, err := fleet.Get(ctx, rec.AgentID); err != nil {
		t.Fatalf("config retirement must not deregister fleet agent: %v", err)
	}
	if err := fleet.Deregister(ctx, rec.AgentID); err != nil {
		t.Fatalf("fleet Deregister: %v", err)
	}
	if _, err := fleet.Get(ctx, rec.AgentID); !errors.Is(err, registry.ErrAgentNotFound) {
		t.Fatalf("fleet record after Deregister: %v, want ErrAgentNotFound", err)
	}
	status, found, err := retirer.RetirementStatus(context.Background(), q, rec.AgentID)
	if err != nil || !found || status.OperationID != "retire-config-only" {
		t.Fatalf("fleet deregistration must not alter config tombstone: status=%+v found=%v err=%v", status, found, err)
	}
}
