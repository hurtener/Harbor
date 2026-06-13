package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	artifactsInmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
	"github.com/hurtener/Harbor/internal/tools/catalog"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// TestE2E_MCPApps_ProxyParksOnGate is the load-bearing MCP Apps proxy
// test: an app-initiated tool call to a GATED tool, routed through the
// `mcp.apps.call_tool` Protocol surface, parks on the SAME unified pause
// primitive a planner-initiated call would. The test asserts the pause
// record EXISTS (a tool.approval_requested event with a live Coordinator
// status) BEFORE the call resolves — so it FAILS if the gate is bypassed
// (a bypass produces no pause, the wait times out). Real drivers across
// the seam: real catalog + real ApprovalGate + real Coordinator + real
// EventBus + real ArtifactStore. Runs under -race via the suite default.
func TestE2E_MCPApps_ProxyParksOnGate(t *testing.T) {
	id := identity.Identity{TenantID: "t-apps", UserID: "u-apps", SessionID: "s-apps"}
	red := auditpatterns.New()
	bus := mkAppsBus(t, red)
	coord := pauseresume.New(pauseresume.WithBus(bus))

	gate, err := approval.NewApprovalGate(approval.GateDeps{
		Policy:      alwaysRequireAppsPolicy{},
		Coordinator: coord,
		Bus:         bus,
		Redactor:    red,
		Authorizer:  approval.NewIdentityAuthorizer(),
	})
	if err != nil {
		t.Fatalf("NewApprovalGate: %v", err)
	}
	t.Cleanup(func() { _ = gate.Close(context.Background()) })

	cat := tools.NewCatalog()
	base := tools.ToolDescriptor{
		Tool: tools.Tool{Name: "srv-a_sensitive"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"did": "sensitive-thing"}}, nil
		},
	}
	gated := catalog.WrapWithApproval(base, gate, catalog.ApprovalWrapperOptions{})
	if err := cat.Register(gated); err != nil {
		t.Fatalf("Register gated tool: %v", err)
	}

	surface := mkAppsSurface(t, cat, bus)

	// Subscribe BEFORE dispatching so the park event is not raced.
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	sub, err := bus.Subscribe(subCtx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{approval.EventTypeToolApprovalRequested},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Dispatch the proxy call in a goroutine — it blocks in the gate's
	// RunGuarded until the approval resolves.
	type result struct {
		resp any
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, derr := surface.Dispatch(context.Background(), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
			Identity:  types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
			Tool:      "srv-a_sensitive",
			Arguments: json.RawMessage(`{}`),
		})
		done <- result{resp, derr}
	}()

	// The load-bearing assertion: the call PARKED on the pause primitive.
	var token pauseresume.Token
	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(approval.ToolApprovalRequestedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want ToolApprovalRequestedPayload", ev.Payload)
		}
		token = pauseresume.Token(p.PauseToken)
	case <-time.After(2 * time.Second):
		t.Fatal("app call to a gated tool did NOT park on the pause primitive — the gate was bypassed")
	}
	if token == "" {
		t.Fatal("parked but PauseToken is empty")
	}
	// The Coordinator has a live pause record for the token — proof the
	// pause exists before approval, not just the eventual result.
	if _, statusErr := coord.Status(context.Background(), token); statusErr != nil {
		t.Fatalf("coordinator has no pause record for token %q: %v", token, statusErr)
	}

	// The proxy call has NOT returned yet (it is parked).
	select {
	case r := <-done:
		t.Fatalf("proxy call returned before approval — gate bypassed: %+v / %v", r.resp, r.err)
	default:
	}

	// Resolve APPROVE under the run's own identity (NewIdentityAuthorizer).
	idCtx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if err := gate.ResolveApproval(idCtx, token, approval.DecisionApprove, "ok"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("proxy call after approval: %v", r.err)
		}
		out, ok := r.resp.(*types.MCPAppCallToolResponse)
		if !ok {
			t.Fatalf("response type = %T", r.resp)
		}
		if out.IsError {
			t.Errorf("IsError = true, want false")
		}
		if len(out.Content) == 0 {
			t.Errorf("Content empty after approval")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy call did not return after APPROVE")
	}
}

// TestE2E_MCPApps_ProxyFailsClosedOnMissingIdentity proves the proxy
// rejects a request with an incomplete identity triple fail-closed.
func TestE2E_MCPApps_ProxyFailsClosedOnMissingIdentity(t *testing.T) {
	red := auditpatterns.New()
	bus := mkAppsBus(t, red)
	surface := mkAppsSurface(t, tools.NewCatalog(), bus)
	_, err := surface.Dispatch(context.Background(), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: types.IdentityScope{Tenant: "t-apps"}, // user + session missing
		Tool:     "srv-a_x",
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) || perr.Code != protoerrors.CodeIdentityRequired {
		t.Fatalf("err = %v, want CodeIdentityRequired", err)
	}
}

// mkAppsSurface builds a real AppsSurface over a real AppsAccessor:
// real catalog + real (empty) MCP registry + real ArtifactStore + bus.
func mkAppsSurface(t *testing.T, cat tools.ToolCatalog, bus events.EventBus) *protocol.AppsSurface {
	t.Helper()
	store, err := artifactsInmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifactsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:  mcp.NewRegistry(),
		Catalog:   cat,
		Store:     store,
		Bus:       bus,
		Threshold: 32 * 1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{Resource: acc, Invoker: acc})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	return s
}

func mkAppsBus(t *testing.T, red *auditpatterns.Driver) events.EventBus {
	t.Helper()
	b, err := eventsInmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second,
	}, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

// alwaysRequireAppsPolicy always requires approval so the proxy call
// parks — the inverse (a non-gated tool) would never park, which is the
// property the bypass-detection test relies on.
type alwaysRequireAppsPolicy struct{}

func (alwaysRequireAppsPolicy) ShouldApprove(_ context.Context, _ *approval.ApprovalRequest) (bool, string, error) {
	return true, "mcp-apps-test", nil
}
