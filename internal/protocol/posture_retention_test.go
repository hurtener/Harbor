package protocol_test

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// Every caller uses the events-only seam; even a failing diagnostics seam and
// refusing redactor cannot put stored reads or audit work on health.
func TestPostureHealth_AllScopes_EventsOnlyNoAudit(t *testing.T) {
	for _, scope := range []auth.Scope{"", auth.ScopeConsoleFleet, auth.ScopeAdmin} {
		t.Run(string(scope), func(t *testing.T) {
			deps := basePostureDeps(t)
			deps.Redactor = failingRedactor{}
			deps.EventsHorizon = func(context.Context) []types.RetentionHorizon {
				return []types.RetentionHorizon{{Surface: "events", Scope: types.RetentionScopeRuntime}}
			}
			deps.RuntimeRetention = func(context.Context) ([]types.RetentionHorizon, error) {
				t.Error("health invoked diagnostics")
				return nil, context.DeadlineExceeded
			}
			s, err := protocol.NewPostureSurface(deps)
			if err != nil {
				t.Fatal(err)
			}
			ctx := auth.WithScopes(mustCtx(t, identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-x"}), []auth.Scope{scope})
			out, err := s.Dispatch(ctx, methods.MethodRuntimeHealth, validRequest())
			if err != nil {
				t.Fatal(err)
			}
			h := out.(*types.RuntimeHealth)
			if len(h.Retention) != 1 || h.Retention[0].Surface != "events" {
				t.Fatalf("health=%+v", h)
			}
		})
	}
}

func TestPostureRetention_VerifiedAdminOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verified bool
		scope    auth.Scope
		allowed  bool
	}{
		{"admin", true, auth.ScopeAdmin, true}, {"fleet", true, auth.ScopeConsoleFleet, false},
		{"unscoped", true, "", false}, {"trust", false, "", false}, {"scope-without-actor", false, auth.ScopeAdmin, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := newPostureBus(t)
			sub, err := bus.Subscribe(t.Context(), events.Filter{Admin: true})
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Cancel()
			calls := 0
			deps := basePostureDeps(t)
			deps.Bus = bus
			deps.RuntimeRetention = func(context.Context) ([]types.RetentionHorizon, error) {
				calls++
				return []types.RetentionHorizon{{Surface: "events", Scope: types.RetentionScopeRuntime}, {Surface: "tasks", Scope: types.RetentionScopeRuntime}, {Surface: "sessions", Scope: types.RetentionScopeRuntime}}, nil
			}
			s, err := protocol.NewPostureSurface(deps)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			actor := identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-x"}
			if tc.verified {
				ctx = mustCtx(t, actor)
			}
			ctx = auth.WithScopes(ctx, []auth.Scope{tc.scope})
			out, err := s.Dispatch(ctx, methods.MethodRuntimeRetention, validRequest())
			if !tc.allowed {
				var pe *protoerrors.Error
				if !stderrors.As(err, &pe) || pe.Code != protoerrors.CodeScopeMismatch || out != nil || calls != 0 {
					t.Fatalf("out=%+v err=%v calls=%d", out, err, calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(out.(*types.RuntimeRetention).Retention) != 3 {
				t.Fatalf("out=%+v calls=%d", out, calls)
			}
			count := 0
			deadline := time.After(30 * time.Millisecond)
			for {
				select {
				case ev := <-sub.Events():
					if ev.Type == events.EventTypeAdminScopeUsed && ev.Identity.TenantID == actor.TenantID {
						count++
						payload, ok := ev.Payload.(events.AdminScopeUsedPayload)
						if !ok || payload.Method != string(methods.MethodRuntimeRetention) || ev.Identity.Identity != actor {
							t.Fatalf("audit=%+v", ev)
						}
					}
				case <-deadline:
					if count != 1 {
						t.Fatalf("audit count=%d", count)
					}
					return
				}
			}
		})
	}
}

func TestPostureRetention_ReadAndAuditFailures_NoResponse(t *testing.T) {
	for _, kind := range []string{"read", "redactor", "publish", "unwired"} {
		t.Run(kind, func(t *testing.T) {
			deps := basePostureDeps(t)
			deps.RuntimeRetention = func(context.Context) ([]types.RetentionHorizon, error) {
				if kind == "read" {
					return nil, context.DeadlineExceeded
				}
				return nil, nil
			}
			if kind == "redactor" {
				deps.Redactor = failingRedactor{}
			}
			if kind == "publish" {
				deps.Bus = failingPublishBus{EventBus: deps.Bus}
			}
			if kind == "unwired" {
				deps.RuntimeRetention = nil
			}
			s, err := protocol.NewPostureSurface(deps)
			if err != nil {
				t.Fatal(err)
			}
			ctx := auth.WithScopes(mustCtx(t, identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-x"}), []auth.Scope{auth.ScopeAdmin})
			out, err := s.Dispatch(ctx, methods.MethodRuntimeRetention, validRequest())
			var pe *protoerrors.Error
			if !stderrors.As(err, &pe) || pe.Code != protoerrors.CodeRuntimeError || out != nil {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
}

type failingPublishBus struct{ events.EventBus }

func (f failingPublishBus) Publish(context.Context, events.Event) error {
	return context.DeadlineExceeded
}

func TestPostureRetention_FencedActorFailsWithoutAudit(t *testing.T) {
	for _, driver := range []string{"inmem", "durable"} {
		t.Run(driver, func(t *testing.T) {
			deps := basePostureDeps(t)
			if driver == "durable" {
				store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = store.Close(t.Context()) }()
				bus, err := durable.New(t.Context(), config.EventsConfig{Driver: "durable", MaxSubscribersPerSession: 4, SubscriberBufferSize: 4, IdleTimeout: time.Minute, DropWindow: time.Second, LegacyWritersDrained: true}, deps.Redactor, store)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = bus.Close(t.Context()) }()
				deps.Bus = bus
			}
			actor := identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-x"}
			if err := deps.Bus.(events.Fencer).Fence(t.Context(), actor); err != nil {
				t.Fatal(err)
			}
			deps.RuntimeRetention = func(context.Context) ([]types.RetentionHorizon, error) { return nil, nil }
			s, err := protocol.NewPostureSurface(deps)
			if err != nil {
				t.Fatal(err)
			}
			out, err := s.Dispatch(auth.WithScopes(mustCtx(t, actor), []auth.Scope{auth.ScopeAdmin}), methods.MethodRuntimeRetention, validRequest())
			var pe *protoerrors.Error
			if out != nil || !stderrors.As(err, &pe) || pe.Code != protoerrors.CodeRuntimeError {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
}
