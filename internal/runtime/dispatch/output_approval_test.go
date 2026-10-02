package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	agentregistry "github.com/hurtener/Harbor/internal/runtime/registry"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/catalog"
)

type outputOAuth struct {
	auth.OAuthProvider
	ready atomic.Bool
}

func (p *outputOAuth) Token(context.Context, tools.ToolSourceID) (auth.Token, error) {
	if !p.ready.Load() {
		return auth.Token{}, &auth.ErrAuthRequired{Source: "fixture", Message: "fixture approval"}
	}
	return auth.Token{AccessToken: "synthetic-ephemeral", TokenType: "Bearer"}, nil
}

func TestOutputWitness_ApprovalAndOAuthResumeBeforeAdmission(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	cat := tools.NewCatalog(tools.WithCatalogBus(bus))
	oauth := &outputOAuth{}
	var calls atomic.Int64
	gate, err := approval.NewApprovalGate(approval.GateDeps{Policy: approval.AlwaysDenyPolicy{}, Coordinator: pauseresume.New(), Bus: bus, Redactor: auditpatterns.New(), Authorizer: approval.NewIdentityAuthorizer()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Close(context.Background()) }()
	desc := tools.ToolDescriptor{Tool: tools.Tool{Name: "guarded", Source: "fixture"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		calls.Add(1)
		return tools.ToolResult{Value: dispatchContentResult{Data: []byte{0, 255}}}, nil
	}}
	desc = catalog.WrapWithOAuth(desc, oauth, catalog.OAuthWrapperOptions{})
	desc = catalog.WrapWithApproval(desc, gate, catalog.ApprovalWrapperOptions{})
	if err = cat.Register(desc); err != nil {
		t.Fatal(err)
	}
	q := dispatchTestQuad("approval-run")
	ctx := outputTaskContext(t, q)
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
	if err != nil {
		t.Fatal(err)
	}
	if err = reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	ctx = tasks.WithOutputTask(ctx, h.ID)
	rc := dispatchRunContext(cat, q)
	rc.Trajectory = &planner.Trajectory{}
	exec := NewToolExecutor(cat, newTestArtifactStore(t), reg)
	sub, err := bus.Subscribe(ctx, events.Filter{Tenant: q.TenantID, User: q.UserID, Session: q.SessionID, Types: []events.EventType{approval.EventTypeToolApprovalRequested}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			oauth.ready.Store(true)
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := exec.ExecuteDecision(ctx, rc, planner.CallTool{Tool: "guarded", Args: json.RawMessage(`{}`)})
			done <- err
		}()
		var token pauseresume.Token
		select {
		case ev := <-sub.Events():
			payload, ok := ev.Payload.(approval.ToolApprovalRequestedPayload)
			if !ok {
				t.Fatalf("approval payload %T", ev.Payload)
			}
			token = pauseresume.Token(payload.PauseToken)
		case <-time.After(3 * time.Second):
			t.Fatal("approval did not park")
		}
		task, err := reg.Get(ctx, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(task.OutputManifest.Invocations) != 0 || calls.Load() != 0 {
			t.Fatal("approval wait persisted a possible remote invocation")
		}
		if err = gate.ResolveApproval(agentregistry.WithControlScope(ctx), token, approval.DecisionApprove, "synthetic test"); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("approval resume stuck")
		}
		if attempt == 0 {
			var need *auth.ErrAuthRequired
			if !errors.As(err, &need) {
				t.Fatalf("expected auth pause: %v", err)
			}
			task, _ = reg.Get(ctx, h.ID)
			if len(task.OutputManifest.Invocations) != 0 {
				t.Fatal("auth pause left a pending/settled fence")
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("remote calls=%d", calls.Load())
	}
	if err = reg.MarkComplete(ctx, h.ID, tasks.TaskResult{}); err != nil {
		t.Fatal(err)
	}
	task, _ := reg.Get(ctx, h.ID)
	if len(task.OutputManifest.Artifacts) != 1 || !task.OutputManifest.Sealed {
		t.Fatal("approved output missing")
	}
}
