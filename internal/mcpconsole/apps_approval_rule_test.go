package mcpconsole_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
	"github.com/hurtener/Harbor/internal/tools/catalog"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// A tool an approval rule gates is refused on the rendered-App path with
// a typed error (it cannot park for a decision there) instead of running
// ungated; an ungated sibling still runs.
func TestAppCall_ApprovalRuleGatedToolRefused(t *testing.T) {
	ctx := idCtx(t)
	cat := gateCatalog(t)
	registry := newAppsRegistry(t, nil)
	if err := registry.Register(context.Background(), mcp.ServerRegistration{
		Provider: &stubProvider{id: "srv-b"}, Transport: "inmemory", InitialState: mcp.ServerStateOnline,
	}); err != nil {
		t.Fatalf("Register srv-b: %v", err)
	}
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry: registry, Catalog: cat, Store: newAppsStore(t), Bus: newAppsBus(t),
		ToolContext: newAppsToolCtx(t), Threshold: 1024,
		ApprovalGated: func(tl tools.Tool) bool { return tl.Name == "srv-a_echo" },
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	_, err = acc.CallTool(ctx, "", "srv-a_echo", json.RawMessage(`{}`))
	if !errors.Is(err, mcpconsole.ErrAppToolApprovalRequired) {
		t.Fatalf("gated App call err = %v, want ErrAppToolApprovalRequired", err)
	}
	if _, err := acc.CallTool(ctx, "", "srv-b_ping", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("ungated call rejected: %v", err)
	}
}

func TestAppCall_RevisionApprovalUsesExistingEntryGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(idCtx(t), 3*time.Second)
	defer cancel()
	cat, reg, bus := gateCatalog(t), gateRegistry(t), newAppsBus(t)
	entryGates := map[string]*approval.ApprovalGate{}
	builder := catalog.New([]config.ToolEntryConfig{
		{Name: "srv-a_echo", Approval: &config.ToolApprovalConfig{Policy: "deny-all", Reason: "entry policy"}},
		{Name: "srv-a_other", Approval: &config.ToolApprovalConfig{Policy: "approve-all"}},
	}, catalog.Deps{
		Catalog: cat, Coordinator: pauseresume.New(), Bus: bus, Redactor: patternsAudit.New(),
		Authorizer: approval.NewIdentityAuthorizer(), AppliedGates: entryGates,
	})
	if err := builder.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	for _, gate := range entryGates {
		t.Cleanup(func() {
			if err := gate.Close(context.Background()); err != nil {
				t.Errorf("close entry gate: %v", err)
			}
		})
	}
	if _, err := reg.SetRevision(ctx, gateID(), gateAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Approval: &agentcfg.ApprovalSection{Rules: []agentcfg.ApprovalRule{{
			Tools: []string{"srv-a_*"}, Approval: agentcfg.ApprovalPolicy{Policy: "deny-all"},
		}}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	acc := newGatedAccessor(t, cat, reg)
	args := json.RawMessage(`{}`)
	if _, err := acc.CallTool(ctx, "", "srv-a_other", args); !errors.Is(err, mcpconsole.ErrAppToolApprovalRequired) {
		t.Fatalf("callback with revision-only approval requirement = %v", err)
	}
	id := gateID()
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{approval.EventTypeToolApprovalRequested},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	done := make(chan error, 1)
	go func() {
		_, err := acc.CallTool(ctx, "", "srv-a_echo", args)
		done <- err
	}()
	select {
	case evt := <-sub.Events():
		payload, ok := evt.Payload.(approval.ToolApprovalRequestedPayload)
		if !ok || payload.Reason != "entry policy" {
			t.Fatalf("approval request = %+v, want the existing entry gate", evt.Payload)
		}
		if err := entryGates["srv-a_echo"].ResolveApproval(ctx, pauseresume.Token(payload.PauseToken), approval.DecisionApprove, "ok"); err != nil {
			t.Fatal(err)
		}
	case err := <-done:
		t.Fatalf("callback completed before entry approval: %v", err)
	case <-ctx.Done():
		t.Fatal("callback did not ask the entry gate for approval")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("approved callback: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("callback did not complete after entry approval")
	}
}
