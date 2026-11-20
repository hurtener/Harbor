package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/catalog"
)

func TestRevisionApprovalRules_ComposeWithBootGates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		entry    bool
		boot     config.ToolApprovalConfig
		tags     []string
		revision string
		wantGate string
	}{
		{name: "entry approve-all", entry: true, boot: config.ToolApprovalConfig{Policy: "approve-all"}, revision: "deny-all", wantGate: "revision"},
		{name: "entry tagged", entry: true, boot: config.ToolApprovalConfig{Policy: "tagged", RequireTags: []string{"write"}}, revision: "deny-all", wantGate: "boot"},
		{name: "entry deny-all", entry: true, boot: config.ToolApprovalConfig{Policy: "deny-all"}, revision: "approve-all", wantGate: "boot"},
		{name: "boot approve-all", boot: config.ToolApprovalConfig{Policy: "approve-all"}, revision: "deny-all", wantGate: "revision"},
		{name: "boot tagged miss", boot: config.ToolApprovalConfig{Policy: "tagged", RequireTags: []string{"write"}}, revision: "deny-all", wantGate: "revision"},
		{name: "boot tagged hit", boot: config.ToolApprovalConfig{Policy: "tagged", RequireTags: []string{"write"}}, tags: []string{"write"}, revision: "deny-all", wantGate: "boot"},
		{name: "automatic first match", boot: config.ToolApprovalConfig{Policy: "approve-all"}, revision: "approve-all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const name = "write_doc"
			ctx, cancel := context.WithTimeout(ctxWithID(t, catalogTestID), 3*time.Second)
			defer cancel()
			coordinator, bus := pauseresume.New(), newRulesBus(t)
			deps := catalog.Deps{Coordinator: coordinator, Bus: bus, Redactor: patternsAudit.New(), Authorizer: approval.NewIdentityAuthorizer()}
			var entries []config.ToolEntryConfig
			tc.boot.Reason = "boot"
			if tc.entry {
				entries = []config.ToolEntryConfig{{Name: name, Approval: &tc.boot}}
			}
			boot, err := catalog.BuildApprovalRules([]config.ToolApprovalRuleConfig{{Tools: []string{name}, Approval: tc.boot}}, entries, deps)
			if err != nil {
				t.Fatal(err)
			}
			cat := tools.NewCatalog(tools.WithRegistrationGate(boot.Wrap))
			desc := tools.ToolDescriptor{Tool: tools.Tool{Name: name, Tags: tc.tags}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: "invoked"}, nil
			}}
			if err := cat.Register(desc); err != nil {
				t.Fatal(err)
			}
			if got := boot.Matches(desc.Tool); got != (!tc.entry && tc.wantGate == "boot") {
				t.Fatalf("App boot rule requires approval = %t", got)
			}
			deps.Catalog, deps.AppliedGates = cat, boot.Gates
			if err := catalog.New(entries, deps).Apply(ctx); err != nil {
				t.Fatal(err)
			}
			for _, gate := range boot.Gates {
				t.Cleanup(func() {
					if err := gate.Close(context.Background()); err != nil {
						t.Errorf("close boot gate: %v", err)
					}
				})
			}
			revisionGate, err := catalog.NewRevisionApprovalGate(deps)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := revisionGate.Close(context.Background()); err != nil {
					t.Errorf("close revision gate: %v", err)
				}
			})
			rules := []config.ToolApprovalRuleConfig{
				{Tools: []string{name}, Approval: config.ToolApprovalConfig{Policy: tc.revision, Reason: "revision"}},
				{Tools: []string{name}, Approval: config.ToolApprovalConfig{Policy: "deny-all", Reason: "later rule"}},
			}
			wrap, err := catalog.CompileRevisionApprovalRules(rules, revisionGate)
			if err != nil {
				t.Fatal(err)
			}
			appRequired, err := catalog.ApprovalRuleRequiresApproval(rules, desc.Tool)
			if err != nil || appRequired != (tc.revision == "deny-all") {
				t.Fatalf("App revision rule requires approval = %t, err = %v", appRequired, err)
			}
			ctx = tools.WithRunToolInvocationWrapper(ctx, wrap)
			invocation, ok := tools.ResolveToolInvocation(ctx, cat, name)
			if !ok {
				t.Fatal("invocation missing")
			}
			sub, err := bus.Subscribe(ctx, events.Filter{
				Tenant: catalogTestID.TenantID, User: catalogTestID.UserID, Session: catalogTestID.SessionID,
				Types: []events.EventType{approval.EventTypeToolApprovalRequested},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Cancel()
			done := make(chan error, 1)
			go func() {
				result, invokeErr := invocation.Invoke(ctx, json.RawMessage(`{}`))
				if invokeErr == nil && result.Value != "invoked" {
					invokeErr = errors.New("underlying tool did not run")
				}
				done <- invokeErr
			}()
			if tc.wantGate != "" {
				select {
				case evt := <-sub.Events():
					payload, ok := evt.Payload.(approval.ToolApprovalRequestedPayload)
					if !ok || payload.Reason != tc.wantGate {
						t.Fatalf("approval request = %+v, want gate reason %q", evt.Payload, tc.wantGate)
					}
					gate := revisionGate
					if tc.wantGate == "boot" {
						gate = boot.Gates["approval_rules[0]"]
						if tc.entry {
							gate = boot.Gates[name]
						}
					}
					if err := gate.ResolveApproval(ctx, pauseresume.Token(payload.PauseToken), approval.DecisionApprove, "ok"); err != nil {
						t.Fatal(err)
					}
				case err := <-done:
					t.Fatalf("invocation completed before required approval: %v", err)
				case <-ctx.Done():
					t.Fatal("expected approval request")
				}
			}
			select {
			case evt := <-sub.Events():
				t.Fatalf("unexpected additional approval request: %+v", evt.Payload)
			case err := <-done:
				if err != nil {
					t.Fatalf("invocation failed: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("invocation did not complete")
			}
		})
	}
}

func TestRevisionApprovalRules_UnattendedInvocation(t *testing.T) {
	cat, coordinator, bus, redactor := buildCatalogEnv(t)
	gate, err := catalog.NewRevisionApprovalGate(catalog.Deps{
		Coordinator: coordinator, Bus: bus, Redactor: redactor, Authorizer: approval.NewIdentityAuthorizer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := gate.Close(context.Background()); err != nil {
			t.Errorf("close revision gate: %v", err)
		}
	})
	wrap, err := catalog.CompileRevisionApprovalRules([]config.ToolApprovalRuleConfig{
		{Tools: []string{"delete_doc"}, Approval: config.ToolApprovalConfig{Policy: "deny-all"}},
		{Tools: []string{"echo_tool"}, Approval: config.ToolApprovalConfig{Policy: "approve-all"}},
	}, gate)
	if err != nil {
		t.Fatal(err)
	}
	oauthErr := errors.New("OAuth must not be reached before approval")
	builder := catalog.New([]config.ToolEntryConfig{{Name: "delete_doc", OAuth: &config.ToolOAuthConfig{Provider: "test-source", BindingScope: "user"}}}, catalog.Deps{
		Catalog: cat, OAuthProviders: map[string]auth.OAuthProvider{"test-source": &stubOAuthProvider{tokenErr: oauthErr}},
	})
	if err := builder.Apply(ctxWithID(t, catalogTestID)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(ctxWithID(t, catalogTestID), 2*time.Second)
	defer cancel()
	ctx = tools.WithRunToolInvocationWrapper(tools.WithUnattendedToolInvocation(ctx), wrap)
	protected, ok := tools.ResolveToolInvocation(ctx, cat, "delete_doc")
	if !ok {
		t.Fatal("protected tool missing")
	}
	if _, err := protected.Invoke(ctx, json.RawMessage(`{}`)); !errors.Is(err, approval.ErrInteractiveApprovalUnavailable) {
		t.Fatalf("unattended protected invocation = %v", err)
	}
	listed, err := coordinator.List(ctx, pauseresume.ListRequest{Identity: catalogTestID, PageSize: 1})
	if err != nil || listed.TotalRows != 0 {
		t.Fatalf("unattended invocation parked %d approvals, err=%v", listed.TotalRows, err)
	}
	automatic, ok := tools.ResolveToolInvocation(ctx, cat, "echo_tool")
	if !ok {
		t.Fatal("automatic tool missing")
	}
	if got, err := automatic.Invoke(ctx, json.RawMessage(`{}`)); err != nil || got.Value != "{}" {
		t.Fatalf("unattended automatic invocation = %+v, %v", got, err)
	}
}
