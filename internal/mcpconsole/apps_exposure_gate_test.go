package mcpconsole_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/tools"
)

const gateAgentID = "gate-agent"

// gateCatalog registers two MCP tools sharing the "srv-a" source plus one on
// "srv-b", so the gate's per-server + per-tool exclusion can be exercised.
func gateCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	reg := func(name string, source tools.ToolSourceID) {
		t.Helper()
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: name, Source: source, Transport: tools.TransportMCP},
			Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: map[string]any{"echo": string(args)}}, nil
			},
		}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	reg("srv-a_echo", "srv-a")
	reg("srv-a_other", "srv-a")
	reg("srv-b_ping", "srv-b")
	return cat
}

func gateRegistry(t *testing.T) agentcfg.Registry {
	t.Helper()
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{},
		agentcfg.Deps{State: newAppsState(t), Bus: newAppsBus(t)})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	return reg
}

// gateID is the identity the idCtx fixture carries (t-1/u-1/s-1).
func gateID() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"}}
}

func newGatedAccessor(t *testing.T, cat tools.ToolCatalog, reg agentcfg.Registry) *mcpconsole.AppsAccessor {
	t.Helper()
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    newAppsRegistry(t, nil),
		Catalog:     cat,
		Store:       newAppsStore(t),
		Bus:         newAppsBus(t),
		ToolContext: newAppsToolCtx(t),
		AgentConfig: reg,
		AgentID:     gateAgentID,
		Threshold:   1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	return acc
}

// TestAppCallGate_PausedServer_Rejected proves an App callback to a tool
// whose server is CURRENTLY paused is rejected with the typed exposure-denied
// authorization error, while a tool on a non-paused server still invokes.
func TestAppCallGate_PausedServer_Rejected(t *testing.T) {
	ctx := idCtx(t)
	cat := gateCatalog(t)
	reg := gateRegistry(t)
	if _, err := reg.SetRevision(ctx, gateID(), gateAgentID, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{PausedServers: []string{"srv-a"}},
	}); err != nil {
		t.Fatalf("set paused: %v", err)
	}
	acc := newGatedAccessor(t, cat, reg)

	_, err := acc.CallTool(ctx, "srv-a_echo", json.RawMessage(`{"x":1}`))
	if !errors.Is(err, mcpconsole.ErrAppToolExposureDenied) {
		t.Fatalf("paused-server call err = %v, want ErrAppToolExposureDenied", err)
	}
	// A tool on a non-paused server is unaffected.
	if _, err := acc.CallTool(ctx, "srv-b_ping", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("non-paused call rejected: %v", err)
	}
}

// TestAppCallGate_DisabledTool_Rejected proves an individually-disabled tool
// is rejected while its sibling on the same server still invokes.
func TestAppCallGate_DisabledTool_Rejected(t *testing.T) {
	ctx := idCtx(t)
	cat := gateCatalog(t)
	reg := gateRegistry(t)
	if _, err := reg.SetRevision(ctx, gateID(), gateAgentID, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{DisabledTools: []string{"srv-a_echo"}},
	}); err != nil {
		t.Fatalf("set disabled: %v", err)
	}
	acc := newGatedAccessor(t, cat, reg)

	if _, err := acc.CallTool(ctx, "srv-a_echo", json.RawMessage(`{}`)); !errors.Is(err, mcpconsole.ErrAppToolExposureDenied) {
		t.Fatalf("disabled-tool call err = %v, want ErrAppToolExposureDenied", err)
	}
	if _, err := acc.CallTool(ctx, "srv-a_other", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("sibling tool rejected: %v", err)
	}
}

// TestAppCallGate_SessionOverlayDisable_Rejected is the wave-end regression
// for the app-gate / session-overlay divergence (audit W3): the run-start
// planner-view projection UNIONS the session overlay's narrow-only disables,
// but the app→host gate previously read only the admin revision — so a tool a
// SESSION user disabled was still invocable from an MCP App. The gate now
// unions the overlay too. Here the admin revision pins NO tool-exposure; the
// rejection comes solely from the session overlay.
func TestAppCallGate_SessionOverlayDisable_Rejected(t *testing.T) {
	ctx := idCtx(t)
	cat := gateCatalog(t)
	reg := gateRegistry(t)
	ov, err := sessionoverlay.NewStore(newAppsState(t), nil)
	if err != nil {
		t.Fatalf("overlay store: %v", err)
	}
	// The session disables a server (srv-b) and an individual tool (srv-a_echo).
	if _, err := ov.SetSourceDisables(ctx, gateID(), gateAgentID, []string{"srv-b"}, []string{"srv-a_echo"}); err != nil {
		t.Fatalf("set session disables: %v", err)
	}
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:       newAppsRegistry(t, nil),
		Catalog:        cat,
		Store:          newAppsStore(t),
		Bus:            newAppsBus(t),
		ToolContext:    newAppsToolCtx(t),
		AgentConfig:    reg,
		AgentID:        gateAgentID,
		SessionOverlay: ov,
		Threshold:      1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}

	if _, err := acc.CallTool(ctx, "srv-b_ping", json.RawMessage(`{}`)); !errors.Is(err, mcpconsole.ErrAppToolExposureDenied) {
		t.Fatalf("session-disabled SERVER call err = %v, want ErrAppToolExposureDenied", err)
	}
	if _, err := acc.CallTool(ctx, "srv-a_echo", json.RawMessage(`{}`)); !errors.Is(err, mcpconsole.ErrAppToolExposureDenied) {
		t.Fatalf("session-disabled TOOL call err = %v, want ErrAppToolExposureDenied", err)
	}
	// A tool neither admin- nor session-disabled still invokes.
	if _, err := acc.CallTool(ctx, "srv-a_other", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("undisabled sibling rejected: %v", err)
	}
}

// TestAppCallGate_NotPaused_Allowed proves the gate is a pass-through when the
// active revision has no tool-exposure section.
func TestAppCallGate_NotPaused_Allowed(t *testing.T) {
	ctx := idCtx(t)
	cat := gateCatalog(t)
	reg := gateRegistry(t)
	// An active revision with only a skills section — no tool exposure.
	if _, err := reg.SetRevision(ctx, gateID(), gateAgentID, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"x"}},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	acc := newGatedAccessor(t, cat, reg)
	if _, err := acc.CallTool(ctx, "srv-a_echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("call rejected with no tool-exposure section: %v", err)
	}
}

// TestAppCallGate_Inert_WhenUnwired proves the gate is inert (backward
// compatible) when no agent-config registry is wired into the accessor.
func TestAppCallGate_Inert_WhenUnwired(t *testing.T) {
	ctx := idCtx(t)
	cat := gateCatalog(t)
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    newAppsRegistry(t, nil),
		Catalog:     cat,
		Store:       newAppsStore(t),
		Bus:         newAppsBus(t),
		ToolContext: newAppsToolCtx(t),
		// No AgentConfig / AgentID — the gate must be inert.
		Threshold: 1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	if _, err := acc.CallTool(ctx, "srv-a_echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("inert gate rejected a call: %v", err)
	}
}

// TestAppCallGate_Asymmetry_SnapshotVsCurrent proves the planner-snapshot /
// app-call-current asymmetry (D-234): a tool view captured BEFORE the pause
// (a planner snapshot) still contains the tool, while the app→host CallTool —
// gated against CURRENT desired state — is rejected after the pause.
func TestAppCallGate_Asymmetry_SnapshotVsCurrent(t *testing.T) {
	ctx := idCtx(t)
	cat := gateCatalog(t)
	reg := gateRegistry(t)

	// Pre-pause "planner snapshot": the run-start view over the catalog. No
	// tool exposure yet, so the snapshot contains srv-a_echo.
	snapshot := tools.NewPlannerView(cat, tools.CatalogFilter{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"})
	if _, ok := snapshot.Resolve("srv-a_echo"); !ok {
		t.Fatal("pre-pause snapshot missing srv-a_echo")
	}

	// Admin pauses srv-a AFTER the snapshot was captured.
	if _, err := reg.SetRevision(ctx, gateID(), gateAgentID, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{PausedServers: []string{"srv-a"}},
	}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	acc := newGatedAccessor(t, cat, reg)

	// The app→host CallTool is gated against CURRENT state → rejected.
	if _, err := acc.CallTool(ctx, "srv-a_echo", json.RawMessage(`{}`)); !errors.Is(err, mcpconsole.ErrAppToolExposureDenied) {
		t.Fatalf("app call after pause err = %v, want ErrAppToolExposureDenied", err)
	}

	// The in-flight planner snapshot — captured before the pause — is
	// undisturbed: it still resolves the tool.
	if _, ok := snapshot.Resolve("srv-a_echo"); !ok {
		t.Fatal("in-flight planner snapshot lost srv-a_echo after a pause — the asymmetry is violated")
	}
}

// TestAppCallGate_IdentityRequired proves the gate fails closed when ctx
// carries no identity triple.
func TestAppCallGate_IdentityRequired(t *testing.T) {
	cat := gateCatalog(t)
	reg := gateRegistry(t)
	acc := newGatedAccessor(t, cat, reg)
	if _, err := acc.CallTool(context.Background(), "srv-a_echo", json.RawMessage(`{}`)); err == nil {
		t.Fatal("CallTool with no identity should fail closed")
	}
}
