package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/protocol"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/sessionadmission"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/catalog"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

type nativeAdmissionResolver struct{}

func (nativeAdmissionResolver) EffectiveAgentID(requested string) (string, error) {
	if requested == "" {
		return "agent", nil
	}
	return requested, nil
}
func (nativeAdmissionResolver) ResolveAgent(_ context.Context, id identity.Identity, name string) (bool, error) {
	return id.TenantID != "" && name == "agent", nil
}

type nativeAdmissionFixture struct {
	env      *phase30Env
	gate     *sessionadmission.Gate
	app      *protocol.AppsSurface
	approval *approval.ApprovalGate
	id       identity.Identity
	invoked  atomic.Int64
}

func nativeAdmissionContext(t *testing.T, f *nativeAdmissionFixture, epoch uint64, methodsAllowed ...methods.Method) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(t.Context(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	ctx = sessionadmission.WithGate(ctx, f.gate)
	ctx = protocolauth.WithTokenAuthority(ctx, protocolauth.TokenAuthority{Issuer: "https://issuer.example", Subject: "coordinator"})
	ctx = protocolauth.WithScopes(ctx, []protocolauth.Scope{protocolauth.ScopeAdmin})
	ctx = protocolauth.WithAgentReach(ctx, []string{"agent"})
	if epoch > 0 {
		ctx = protocolauth.WithMethodReach(ctx, methodsAllowed)
		ctx = protocolauth.WithSessionAdmission(ctx, &protocolauth.SessionAdmissionAuthority{Identity: f.id, Epoch: epoch, Coordinator: "coordinator"})
	}
	return ctx
}

func newNativeAdmissionFixture(t *testing.T, needsApproval, needsOAuth, unknownWrite bool) *nativeAdmissionFixture {
	t.Helper()
	f := &nativeAdmissionFixture{env: buildPhase30Env(t, phase30StoreCases()[0]), id: identity.Identity{TenantID: "admission-native", UserID: "owner", SessionID: "session"}}
	var err error
	f.gate, err = sessionadmission.New(f.env.store)
	if err != nil {
		t.Fatal(err)
	}
	cat := tools.NewCatalog()
	source := tools.ToolSourceID("")
	if needsOAuth {
		source = f.env.userCfg.Source
	}
	if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "native_action", Transport: tools.TransportInProcess, Source: source}, Invoke: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		actual, ok := identity.From(ctx)
		if !ok || actual != f.id {
			return tools.ToolResult{}, errors.New("wrong invocation identity")
		}
		f.invoked.Add(1)
		if unknownWrite {
			return tools.ToolResult{}, errors.New("outcome unknown after accepted external write")
		}
		return tools.ToolResult{Value: map[string]any{"ok": true}}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	entry := config.ToolEntryConfig{Name: "native_action"}
	if needsApproval {
		entry.Approval = &config.ToolApprovalConfig{Policy: "deny-all"}
	}
	if needsOAuth {
		entry.OAuth = &config.ToolOAuthConfig{Provider: "native", BindingScope: string(toolauth.ScopeUser)}
	}
	gates := map[string]*approval.ApprovalGate{}
	builder := catalog.New([]config.ToolEntryConfig{entry}, catalog.Deps{Catalog: cat, Coordinator: f.env.coordinator, Bus: f.env.bus, Redactor: patterns.New(), Authorizer: approval.NewIdentityAuthorizer(), OAuthProviders: map[string]toolauth.OAuthProvider{"native": f.env.provider}, AppliedGates: gates})
	if err := builder.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.approval = gates["native_action"]
	if f.approval != nil {
		t.Cleanup(func() { _ = f.approval.Close(context.Background()) })
	}
	art, err := artifacts.Open(t.Context(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = art.Close(context.Background()) })
	toolCtx, err := mcpconsole.NewToolContextStore(mcpconsole.ToolContextDeps{State: f.env.store, Store: art, Bus: f.env.bus})
	if err != nil {
		t.Fatal(err)
	}
	accessor, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{Registry: mcp.NewRegistry(), Catalog: cat, Store: art, Bus: f.env.bus, ToolContext: toolCtx, Threshold: 4096})
	if err != nil {
		t.Fatal(err)
	}
	f.app, err = protocol.NewAppsSurface(protocol.AppsDeps{Resource: accessor, Invoker: accessor, ToolContext: accessor, AgentResolver: nativeAdmissionResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.gate.Enroll(nativeAdmissionContext(t, f, 0), f.id, 0, 1); err != nil {
		t.Fatal(err)
	}
	return f
}
func nativeAppRequest(f *nativeAdmissionFixture) *types.MCPAppCallToolRequest {
	return &types.MCPAppCallToolRequest{Identity: types.IdentityScope{Tenant: f.id.TenantID, User: f.id.UserID, Session: f.id.SessionID}, AgentID: "agent", Tool: "native_action", Arguments: json.RawMessage(`{}`)}
}
func nativeEvent(t *testing.T, ch <-chan events.Event) events.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("native lifecycle event missing")
		return events.Event{}
	}
}

func TestSessionAdmission_AppNativeApprovalHandoff(t *testing.T) {
	for _, name := range []string{"approve", "reject", "epoch_changed", "cancel"} {
		t.Run(name, func(t *testing.T) {
			f := newNativeAdmissionFixture(t, true, false, false)
			sub, err := f.env.bus.Subscribe(t.Context(), events.Filter{Tenant: f.id.TenantID, User: f.id.UserID, Session: f.id.SessionID, Types: []events.EventType{approval.EventTypeToolApprovalRequested, approval.EventTypeToolApproved}})
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Cancel()
			ctx, cancel := context.WithCancel(nativeAdmissionContext(t, f, 1, methods.MethodMCPAppsCallTool))
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := f.app.Dispatch(ctx, methods.MethodMCPAppsCallTool, nativeAppRequest(f))
				result <- err
			}()
			event := nativeEvent(t, sub.Events())
			payload, ok := event.Payload.(approval.ToolApprovalRequestedPayload)
			if !ok {
				t.Fatalf("approval payload %T", event.Payload)
			}
			if f.invoked.Load() != 0 {
				t.Fatal("descriptor ran before native approval")
			}
			if name == "cancel" {
				cancel()
				if err := <-result; err == nil {
					t.Fatal("cancelled invocation succeeded")
				}
				if _, err := f.gate.Enroll(nativeAdmissionContext(t, f, 0), f.id, 1, 2); err != nil {
					t.Fatalf("parked cancellation left active acceptance: %v", err)
				}
				if f.invoked.Load() != 0 {
					t.Fatal("cancelled invocation executed")
				}
				return
			}
			epoch := uint64(1)
			if name == "epoch_changed" {
				if _, err := f.gate.Enroll(nativeAdmissionContext(t, f, 0), f.id, 1, 2); err != nil {
					t.Fatalf("native pause still holds acceptance: %v", err)
				}
				epoch = 2
			}
			method, decision := methods.MethodApprove, approval.DecisionApprove
			if name == "reject" {
				method, decision = methods.MethodReject, approval.DecisionReject
			}
			control := nativeAdmissionContext(t, f, epoch, method)
			accepted, permit, err := sessionadmission.Begin(control, f.id, method)
			if err != nil {
				t.Fatalf("native control deadlocked on App: %v", err)
			}
			if err := f.approval.ResolveApproval(accepted, pauseresume.Token(payload.PauseToken), decision, "explicit native decision"); err != nil {
				t.Fatal(err)
			}
			if name == "approve" {
				nativeEvent(t, sub.Events())
				if f.invoked.Load() != 0 {
					t.Fatal("descriptor ran before exact admission reacquisition")
				}
			}
			if err := permit.Finish(control); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if name == "approve" && err != nil {
					t.Fatalf("approved App failed: %v", err)
				}
				if name != "approve" && err == nil {
					t.Fatalf("%s unexpectedly executed", name)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("native decision left App blocked")
			}
			want := int64(0)
			if name == "approve" {
				want = 1
			}
			if f.invoked.Load() != want {
				t.Fatalf("invocations=%d want%d", f.invoked.Load(), want)
			}
			if err := f.approval.ResolveApproval(control, pauseresume.Token(payload.PauseToken), decision, "repeated native decision"); err == nil {
				t.Fatal("duplicate native approval accepted")
			}
			if f.invoked.Load() != want {
				t.Fatal("duplicate native delivery invoked again")
			}
		})
	}
}

func TestSessionAdmission_AppOAuthHandoffAndRepeatedCallback(t *testing.T) {
	f := newNativeAdmissionFixture(t, false, true, false)
	sub, err := f.env.bus.Subscribe(t.Context(), events.Filter{Tenant: f.id.TenantID, User: f.id.UserID, Session: f.id.SessionID, Types: []events.EventType{toolauth.EventTypeToolAuthRequired}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	old := nativeAdmissionContext(t, f, 1, methods.MethodMCPAppsCallTool)
	if _, err := f.app.Dispatch(old, methods.MethodMCPAppsCallTool, nativeAppRequest(f)); err == nil {
		t.Fatal("OAuth-required invocation succeeded")
	}
	event := nativeEvent(t, sub.Events())
	required, ok := event.Payload.(toolauth.ToolAuthRequiredPayload)
	if !ok {
		t.Fatalf("OAuth payload %T", event.Payload)
	}
	if f.invoked.Load() != 0 {
		t.Fatal("descriptor ran before OAuth completion")
	}
	if _, err := f.gate.Enroll(nativeAdmissionContext(t, f, 0), f.id, 1, 2); err != nil {
		t.Fatalf("OAuth pause retained App acceptance: %v", err)
	}
	code, _, err := f.env.server.VisitAuthorizeURL(required.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	callback := toolauth.CallbackHandler(map[string]toolauth.OAuthProvider{"native": f.env.provider})
	for n := range 2 {
		req := httptest.NewRequest(http.MethodGet, toolauth.CallbackPath+"?state="+required.State+"&code="+code, nil)
		req = req.WithContext(sessionadmission.WithGate(req.Context(), f.gate))
		rec := httptest.NewRecorder()
		callback.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("native OAuth callback%d status%d body%s", n, rec.Code, rec.Body.String())
		}
	}
	if f.invoked.Load() != 0 {
		t.Fatal("native callback invented a new App invocation")
	}
	if _, err := f.app.Dispatch(old, methods.MethodMCPAppsCallTool, nativeAppRequest(f)); err == nil {
		t.Fatal("old App authority survived enrollment advance")
	}
	current := nativeAdmissionContext(t, f, 2, methods.MethodMCPAppsCallTool)
	if _, err := f.app.Dispatch(current, methods.MethodMCPAppsCallTool, nativeAppRequest(f)); err != nil {
		t.Fatalf("new explicit App action failed: %v", err)
	}
	if f.invoked.Load() != 1 {
		t.Fatalf("new action count=%d", f.invoked.Load())
	}
}

func TestSessionAdmission_AppUnknownWriteCannotBeRefundedByNativeControl(t *testing.T) {
	f := newNativeAdmissionFixture(t, false, false, true)
	ctx := nativeAdmissionContext(t, f, 1, methods.MethodMCPAppsCallTool)
	if _, err := f.app.Dispatch(ctx, methods.MethodMCPAppsCallTool, nativeAppRequest(f)); err == nil {
		t.Fatal("uncertain write reported success")
	}
	if f.invoked.Load() != 1 {
		t.Fatal("fixture did not perform accepted write")
	}
	for _, method := range []methods.Method{methods.MethodCancel, methods.MethodReject, methods.MethodResume} {
		if _, _, err := sessionadmission.Begin(nativeAdmissionContext(t, f, 1, method), f.id, method); !errors.Is(err, sessionadmission.ErrBusy) {
			t.Fatalf("%s cleared uncertain App write: %v", method, err)
		}
	}
	if _, err := f.gate.Enroll(nativeAdmissionContext(t, f, 0), f.id, 1, 2); !errors.Is(err, sessionadmission.ErrBusy) {
		t.Fatalf("unknown write lost its fence: %v", err)
	}
}

func TestSessionAdmission_AppApprovalThenOAuthUsesOriginalOuterChain(t *testing.T) {
	f := newNativeAdmissionFixture(t, true, true, false)
	sub, err := f.env.bus.Subscribe(t.Context(), events.Filter{Tenant: f.id.TenantID, User: f.id.UserID, Session: f.id.SessionID, Types: []events.EventType{approval.EventTypeToolApprovalRequested, toolauth.EventTypeToolAuthRequired}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	result := make(chan error, 1)
	go func() {
		_, err := f.app.Dispatch(nativeAdmissionContext(t, f, 1, methods.MethodMCPAppsCallTool), methods.MethodMCPAppsCallTool, nativeAppRequest(f))
		result <- err
	}()
	first := nativeEvent(t, sub.Events())
	prompt, ok := first.Payload.(approval.ToolApprovalRequestedPayload)
	if !ok {
		t.Fatalf("outer approval wrapper was bypassed: %T", first.Payload)
	}
	control := nativeAdmissionContext(t, f, 1, methods.MethodApprove)
	_, err = sessionadmission.Run(control, f.id, methods.MethodApprove, func(accepted context.Context) (bool, error) {
		return true, f.approval.ResolveApproval(accepted, pauseresume.Token(prompt.PauseToken), approval.DecisionApprove, "explicit approval")
	})
	if err != nil {
		t.Fatal(err)
	}
	second := nativeEvent(t, sub.Events())
	if _, ok := second.Payload.(toolauth.ToolAuthRequiredPayload); !ok {
		t.Fatalf("inner OAuth wrapper was bypassed: %T", second.Payload)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("OAuth-required tool reported execution")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("approval/OAuth chain deadlocked")
	}
	if f.invoked.Load() != 0 {
		t.Fatal("descriptor executed before the complete original wrapper chain")
	}
	if _, err := f.gate.Enroll(nativeAdmissionContext(t, f, 0), f.id, 1, 2); err != nil {
		t.Fatalf("second native park retained acceptance: %v", err)
	}
}
