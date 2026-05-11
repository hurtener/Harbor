package loopback_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/distributed"
	"github.com/hurtener/Harbor/internal/distributed/a2a"
	"github.com/hurtener/Harbor/internal/distributed/drivers/loopback"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
)

func freshEventBus(t *testing.T) (events.EventBus, func()) {
	t.Helper()
	eb, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 32,
		SubscriberBufferSize:     128,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
		ReplayBufferSize:         32,
	}, patterns.New())
	if err != nil {
		t.Fatalf("events bus: %v", err)
	}
	return eb, func() { _ = eb.Close(context.Background()) }
}

func TestNewBus_RequiresEventBus(t *testing.T) {
	_, err := loopback.NewBus(distributed.Dependencies{})
	if err == nil {
		t.Fatalf("NewBus with nil EventBus: expected error")
	}
}

func TestLoopback_BusPublishProjectsToEventBus(t *testing.T) {
	eb, cleanupEB := freshEventBus(t)
	defer cleanupEB()

	bus, err := loopback.NewBus(distributed.Dependencies{EventBus: eb})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	defer bus.Close(context.Background())

	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	sub, err := eb.Subscribe(context.Background(), events.Filter{
		Tenant: triple.TenantID, User: triple.UserID, Session: triple.SessionID,
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Cancel()

	env := distributed.BusEnvelope{
		Edge: "x", Source: "test", Identity: triple, EventID: "evt-1",
		Payload: []byte(`"hi"`), Timestamp: time.Now().UTC(),
	}
	if err := bus.Publish(context.Background(), env); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != loopback.EventTypeDistributedBusEnvelope {
			t.Errorf("type %q != %q", ev.Type, loopback.EventTypeDistributedBusEnvelope)
		}
		p, ok := ev.Payload.(loopback.BusEnvelopePayload)
		if !ok {
			t.Errorf("payload type %T", ev.Payload)
		}
		if p.Envelope.Edge != env.Edge {
			t.Errorf("edge mismatch")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for event")
	}
}

func TestLoopback_RemoteTransport_AgentNotFound(t *testing.T) {
	rt, err := loopback.NewRemoteTransport(distributed.Dependencies{})
	if err != nil {
		t.Fatalf("NewRemoteTransport: %v", err)
	}
	defer rt.Close(context.Background())

	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx, err := identity.With(context.Background(), triple.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	_, err = rt.Send(ctx, distributed.RemoteCallRequest{AgentURL: "missing"})
	if !errors.Is(err, distributed.ErrAgentNotFound) {
		t.Errorf("want ErrAgentNotFound, got %v", err)
	}
}

func TestLoopback_RemoteTransport_RegisterAndUnregister(t *testing.T) {
	rt, err := loopback.NewRemoteTransport(distributed.Dependencies{})
	if err != nil {
		t.Fatalf("NewRemoteTransport: %v", err)
	}
	defer rt.Close(context.Background())

	lt := rt.(loopback.LoopbackTransport)
	lt.RegisterAgent("u-1", &echoAgent{})

	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx, _ := identity.With(context.Background(), triple.Identity)
	res, err := rt.Send(ctx, distributed.RemoteCallRequest{AgentURL: "u-1", Message: a2a.Message{MessageID: "m-1", Role: a2a.RoleUser, Parts: a2a.Parts{&a2a.TextPart{Text: "hi"}}}})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Task.ID != "echo-m-1" {
		t.Errorf("Task ID: %q", res.Task.ID)
	}

	lt.UnregisterAgent("u-1")
	_, err = rt.Send(ctx, distributed.RemoteCallRequest{AgentURL: "u-1"})
	if !errors.Is(err, distributed.ErrAgentNotFound) {
		t.Errorf("want ErrAgentNotFound after unregister, got %v", err)
	}
}

func TestLoopback_RemoteTransport_AfterClose(t *testing.T) {
	rt, err := loopback.NewRemoteTransport(distributed.Dependencies{})
	if err != nil {
		t.Fatalf("NewRemoteTransport: %v", err)
	}
	lt := rt.(loopback.LoopbackTransport)
	lt.RegisterAgent("u-1", &echoAgent{})
	if err := rt.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := rt.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v (expected idempotent)", err)
	}
	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx, _ := identity.With(context.Background(), triple.Identity)
	_, err = rt.Send(ctx, distributed.RemoteCallRequest{AgentURL: "u-1"})
	if !errors.Is(err, distributed.ErrTransportClosed) {
		t.Errorf("want ErrTransportClosed, got %v", err)
	}
}

func TestLoopback_RemoteTransport_GetExtendedAgentCard_DefaultCard(t *testing.T) {
	rt, err := loopback.NewRemoteTransport(distributed.Dependencies{})
	if err != nil {
		t.Fatalf("NewRemoteTransport: %v", err)
	}
	defer rt.Close(context.Background())
	lt := rt.(loopback.LoopbackTransport)
	stub := &a2a.AgentCard{Name: "Default Card", Description: "from cardSource"}
	lt.SetDefaultAgentCard(func() *a2a.AgentCard { return stub })

	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx, _ := identity.With(context.Background(), triple.Identity)
	got, err := rt.GetExtendedAgentCard(ctx)
	if err != nil {
		t.Fatalf("GetExtendedAgentCard: %v", err)
	}
	if got == nil || got.Name != stub.Name {
		t.Errorf("expected default card, got %+v", got)
	}
}

func TestLoopback_RemoteTransport_AllPushNotificationConfigMethods(t *testing.T) {
	rt, err := loopback.NewRemoteTransport(distributed.Dependencies{})
	if err != nil {
		t.Fatalf("NewRemoteTransport: %v", err)
	}
	defer rt.Close(context.Background())
	lt := rt.(loopback.LoopbackTransport)
	lt.RegisterAgent("u-1", &echoAgent{})

	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx, _ := identity.With(context.Background(), triple.Identity)
	cfg := a2a.TaskPushNotificationConfig{TaskID: "task-1", URL: "https://x"}
	got, err := rt.CreateTaskPushNotificationConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.ID == "" {
		t.Errorf("Create: empty ID")
	}
	if _, err := rt.GetTaskPushNotificationConfig(ctx, "task-1", got.ID); err != nil {
		t.Errorf("Get: %v", err)
	}
	if _, err := rt.ListTaskPushNotificationConfigs(ctx, "task-1"); err != nil {
		t.Errorf("List: %v", err)
	}
	if err := rt.DeleteTaskPushNotificationConfig(ctx, "task-1", got.ID); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

func TestLoopback_RemoteTransport_AllOps_AfterClose(t *testing.T) {
	rt, err := loopback.NewRemoteTransport(distributed.Dependencies{})
	if err != nil {
		t.Fatalf("NewRemoteTransport: %v", err)
	}
	lt := rt.(loopback.LoopbackTransport)
	lt.RegisterAgent("u-1", &echoAgent{})
	if err := rt.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx, _ := identity.With(context.Background(), triple.Identity)
	if _, err := rt.GetTask(ctx, "t-1", "c-1"); !errors.Is(err, distributed.ErrTransportClosed) {
		t.Errorf("GetTask: want ErrTransportClosed, got %v", err)
	}
	if _, err := rt.ListTasks(ctx, distributed.RemoteTaskFilter{}); !errors.Is(err, distributed.ErrTransportClosed) {
		t.Errorf("ListTasks: want ErrTransportClosed, got %v", err)
	}
	if err := rt.Cancel(ctx, "t-1", "c-1"); !errors.Is(err, distributed.ErrTransportClosed) {
		t.Errorf("Cancel: want ErrTransportClosed, got %v", err)
	}
	if _, err := rt.Subscribe(ctx, "t-1", "c-1"); !errors.Is(err, distributed.ErrTransportClosed) {
		t.Errorf("Subscribe: want ErrTransportClosed, got %v", err)
	}
	if _, err := rt.GetExtendedAgentCard(ctx); !errors.Is(err, distributed.ErrTransportClosed) {
		t.Errorf("GetExtendedAgentCard: want ErrTransportClosed, got %v", err)
	}
	if _, err := rt.CreateTaskPushNotificationConfig(ctx, a2a.TaskPushNotificationConfig{}); !errors.Is(err, distributed.ErrTransportClosed) {
		t.Errorf("Create: want ErrTransportClosed, got %v", err)
	}
	if _, err := rt.GetTaskPushNotificationConfig(ctx, "t-1", "c-1"); !errors.Is(err, distributed.ErrTransportClosed) {
		t.Errorf("Get: want ErrTransportClosed, got %v", err)
	}
	if _, err := rt.ListTaskPushNotificationConfigs(ctx, "t-1"); !errors.Is(err, distributed.ErrTransportClosed) {
		t.Errorf("List: want ErrTransportClosed, got %v", err)
	}
	if err := rt.DeleteTaskPushNotificationConfig(ctx, "t-1", "c-1"); !errors.Is(err, distributed.ErrTransportClosed) {
		t.Errorf("Delete: want ErrTransportClosed, got %v", err)
	}
	if _, err := rt.Stream(ctx, distributed.RemoteCallRequest{AgentURL: "u-1"}); !errors.Is(err, distributed.ErrTransportClosed) {
		t.Errorf("Stream: want ErrTransportClosed, got %v", err)
	}
}

func TestLoopback_RemoteTransport_StreamSubscribeKind(t *testing.T) {
	rt, err := loopback.NewRemoteTransport(distributed.Dependencies{})
	if err != nil {
		t.Fatalf("NewRemoteTransport: %v", err)
	}
	defer rt.Close(context.Background())
	lt := rt.(loopback.LoopbackTransport)
	lt.RegisterAgent("u-1", &echoAgent{})

	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx, _ := identity.With(context.Background(), triple.Identity)
	stream, err := rt.Stream(ctx, distributed.RemoteCallRequest{
		AgentURL: "u-1",
		Kind:     distributed.RemoteCallKindSubscribe,
		TaskID:   "t-1",
	})
	if err != nil {
		t.Fatalf("Stream(Subscribe): %v", err)
	}
	defer stream.Close()
	// echoAgent.SubscribeToTask returns a closed empty channel, so Recv → io.EOF immediately.
}

func TestLoopback_RemoteTransport_UnknownKind_Errors(t *testing.T) {
	rt, err := loopback.NewRemoteTransport(distributed.Dependencies{})
	if err != nil {
		t.Fatalf("NewRemoteTransport: %v", err)
	}
	defer rt.Close(context.Background())
	lt := rt.(loopback.LoopbackTransport)
	lt.RegisterAgent("u-1", &echoAgent{})
	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx, _ := identity.With(context.Background(), triple.Identity)
	_, err = rt.Stream(ctx, distributed.RemoteCallRequest{
		AgentURL: "u-1",
		Kind:     "weird",
	})
	if err == nil {
		t.Errorf("Stream with unknown kind: expected error")
	}
}

func TestLoopback_BusClose_Idempotent(t *testing.T) {
	eb, cleanup := freshEventBus(t)
	defer cleanup()
	bus, err := loopback.NewBus(distributed.Dependencies{EventBus: eb})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Errorf("first close: %v", err)
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Errorf("second close: %v (expected idempotent)", err)
	}
}

func TestLoopback_RemoteTransport_AllOps_AgentNotFound(t *testing.T) {
	rt, err := loopback.NewRemoteTransport(distributed.Dependencies{})
	if err != nil {
		t.Fatalf("NewRemoteTransport: %v", err)
	}
	defer rt.Close(context.Background())
	// No agents registered — every method should return ErrAgentNotFound.
	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx, _ := identity.With(context.Background(), triple.Identity)
	if _, err := rt.GetTask(ctx, "t-1", "c-1"); !errors.Is(err, distributed.ErrAgentNotFound) {
		t.Errorf("GetTask: want ErrAgentNotFound, got %v", err)
	}
	if _, err := rt.ListTasks(ctx, distributed.RemoteTaskFilter{}); !errors.Is(err, distributed.ErrAgentNotFound) {
		t.Errorf("ListTasks: want ErrAgentNotFound, got %v", err)
	}
	if err := rt.Cancel(ctx, "t-1", "c-1"); !errors.Is(err, distributed.ErrAgentNotFound) {
		t.Errorf("Cancel: want ErrAgentNotFound, got %v", err)
	}
	if _, err := rt.Subscribe(ctx, "t-1", "c-1"); !errors.Is(err, distributed.ErrAgentNotFound) {
		t.Errorf("Subscribe: want ErrAgentNotFound, got %v", err)
	}
	if _, err := rt.GetExtendedAgentCard(ctx); !errors.Is(err, distributed.ErrAgentNotFound) {
		t.Errorf("GetExtendedAgentCard: want ErrAgentNotFound, got %v", err)
	}
	if _, err := rt.Stream(ctx, distributed.RemoteCallRequest{AgentURL: "missing", Kind: distributed.RemoteCallKindStream}); !errors.Is(err, distributed.ErrAgentNotFound) {
		t.Errorf("Stream: want ErrAgentNotFound, got %v", err)
	}
	if _, err := rt.CreateTaskPushNotificationConfig(ctx, a2a.TaskPushNotificationConfig{}); !errors.Is(err, distributed.ErrAgentNotFound) {
		t.Errorf("Create: want ErrAgentNotFound, got %v", err)
	}
	if _, err := rt.GetTaskPushNotificationConfig(ctx, "t-1", "c-1"); !errors.Is(err, distributed.ErrAgentNotFound) {
		t.Errorf("Get: want ErrAgentNotFound, got %v", err)
	}
	if _, err := rt.ListTaskPushNotificationConfigs(ctx, "t-1"); !errors.Is(err, distributed.ErrAgentNotFound) {
		t.Errorf("List: want ErrAgentNotFound, got %v", err)
	}
	if err := rt.DeleteTaskPushNotificationConfig(ctx, "t-1", "c-1"); !errors.Is(err, distributed.ErrAgentNotFound) {
		t.Errorf("Delete: want ErrAgentNotFound, got %v", err)
	}
}

func TestLoopback_RemoteTransport_Stream_ReceivesAndCloses(t *testing.T) {
	rt, err := loopback.NewRemoteTransport(distributed.Dependencies{})
	if err != nil {
		t.Fatalf("NewRemoteTransport: %v", err)
	}
	defer rt.Close(context.Background())
	lt := rt.(loopback.LoopbackTransport)
	lt.RegisterAgent("u-1", &echoAgent{})

	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx, _ := identity.With(context.Background(), triple.Identity)
	stream, err := rt.Stream(ctx, distributed.RemoteCallRequest{
		AgentURL: "u-1",
		Kind:     distributed.RemoteCallKindStream,
		Message:  a2a.Message{MessageID: "m-1", Role: a2a.RoleUser, Parts: a2a.Parts{&a2a.TextPart{Text: "hi"}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	resp, err := stream.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv first: %v", err)
	}
	if resp.Kind() != "task" {
		t.Errorf("kind: %q", resp.Kind())
	}
	// Subsequent Recv hits end-of-stream.
	if _, err := stream.Recv(ctx); err == nil {
		t.Errorf("expected EOF on second Recv")
	}
	if err := stream.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Recv after Close: returns EOF.
	if _, err := stream.Recv(ctx); err == nil {
		t.Errorf("expected error after Close")
	}
	// Second Close: idempotent.
	if err := stream.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestLoopback_RemoteTransport_Stream_CtxCancellation(t *testing.T) {
	rt, err := loopback.NewRemoteTransport(distributed.Dependencies{})
	if err != nil {
		t.Fatalf("NewRemoteTransport: %v", err)
	}
	defer rt.Close(context.Background())
	lt := rt.(loopback.LoopbackTransport)
	holdAgent := &holdingAgent{ch: make(chan a2a.StreamResponse)}
	lt.RegisterAgent("u-1", holdAgent)

	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx, _ := identity.With(context.Background(), triple.Identity)
	stream, err := rt.Stream(ctx, distributed.RemoteCallRequest{
		AgentURL: "u-1",
		Kind:     distributed.RemoteCallKindStream,
		Message:  a2a.Message{MessageID: "m-1", Role: a2a.RoleUser, Parts: a2a.Parts{&a2a.TextPart{Text: "hi"}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	recvCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := stream.Recv(recvCtx); err == nil {
		t.Errorf("expected context cancellation error")
	}
}

// holdingAgent serves a channel-driven SendStreamingMessage that never
// emits anything. The channel is closed when the agent's ctx cancels.
type holdingAgent struct {
	echoAgent
	ch chan a2a.StreamResponse
}

func (h *holdingAgent) SendStreamingMessage(ctx context.Context, _ a2a.Message, _ a2a.SendMessageConfiguration) (<-chan a2a.StreamResponse, error) {
	go func() {
		<-ctx.Done()
		close(h.ch)
	}()
	return h.ch, nil
}

func TestLoopback_RemoteTransport_TimeoutHonored(t *testing.T) {
	rt, err := loopback.NewRemoteTransport(distributed.Dependencies{})
	if err != nil {
		t.Fatalf("NewRemoteTransport: %v", err)
	}
	defer rt.Close(context.Background())
	lt := rt.(loopback.LoopbackTransport)
	lt.RegisterAgent("u-1", &echoAgent{})
	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx, _ := identity.With(context.Background(), triple.Identity)
	req := distributed.RemoteCallRequest{
		AgentURL: "u-1",
		Timeout:  100 * time.Millisecond,
		Message:  a2a.Message{MessageID: "m-1", Role: a2a.RoleUser, Parts: a2a.Parts{&a2a.TextPart{Text: "hi"}}},
	}
	if _, err := rt.Send(ctx, req); err != nil {
		t.Errorf("Send with timeout: %v", err)
	}
}

func TestLoopback_RemoteTransport_GetTask_Subscribe_Cancel_ListTasks(t *testing.T) {
	rt, err := loopback.NewRemoteTransport(distributed.Dependencies{})
	if err != nil {
		t.Fatalf("NewRemoteTransport: %v", err)
	}
	defer rt.Close(context.Background())
	lt := rt.(loopback.LoopbackTransport)
	lt.RegisterAgent("u-1", &echoAgent{})

	triple := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ctx, _ := identity.With(context.Background(), triple.Identity)

	// GetTask happy path.
	snap, err := rt.GetTask(ctx, "task-1", "ctx-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if snap == nil || snap.ID != "task-1" {
		t.Errorf("snap: %+v", snap)
	}

	// ListTasks happy path.
	if _, err := rt.ListTasks(ctx, distributed.RemoteTaskFilter{Status: a2a.TaskStateWorking, StatusTimestampAfter: time.Now().Add(-1 * time.Hour)}); err != nil {
		t.Errorf("ListTasks: %v", err)
	}

	// Cancel happy path.
	if err := rt.Cancel(ctx, "task-1", "ctx-1"); err != nil {
		t.Errorf("Cancel: %v", err)
	}

	// Subscribe happy path (echoAgent returns empty closed channel → Recv returns EOF immediately).
	stream, err := rt.Subscribe(ctx, "task-1", "ctx-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer stream.Close()
}

func TestLoopback_BusValidatesEnvelope(t *testing.T) {
	eb, cleanup := freshEventBus(t)
	defer cleanup()
	bus, err := loopback.NewBus(distributed.Dependencies{EventBus: eb})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	defer bus.Close(context.Background())
	err = bus.Publish(context.Background(), distributed.BusEnvelope{Edge: "x"})
	if !errors.Is(err, distributed.ErrIdentityRequired) {
		t.Errorf("want ErrIdentityRequired, got %v", err)
	}
}

// echoAgent is a minimal Agent stub used by the per-driver tests.
type echoAgent struct{}

func (echoAgent) SendMessage(ctx context.Context, msg a2a.Message, _ a2a.SendMessageConfiguration) (a2a.Task, error) {
	return a2a.Task{ID: "echo-" + msg.MessageID, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}, nil
}
func (echoAgent) SendStreamingMessage(ctx context.Context, _ a2a.Message, _ a2a.SendMessageConfiguration) (<-chan a2a.StreamResponse, error) {
	ch := make(chan a2a.StreamResponse, 1)
	ch <- a2a.StreamResponse{Task: &a2a.Task{ID: "e-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}}
	close(ch)
	return ch, nil
}
func (echoAgent) GetTask(_ context.Context, taskID, contextID string) (a2a.Task, error) {
	return a2a.Task{ID: taskID, ContextID: contextID, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}, nil
}
func (echoAgent) ListTasks(_ context.Context, _ loopback.ListTasksFilter) ([]a2a.Task, error) {
	return nil, nil
}
func (echoAgent) CancelTask(_ context.Context, taskID, contextID string) (a2a.Task, error) {
	return a2a.Task{ID: taskID, ContextID: contextID, Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}, nil
}
func (echoAgent) SubscribeToTask(_ context.Context, _ string, _ string) (<-chan a2a.StreamResponse, error) {
	ch := make(chan a2a.StreamResponse)
	close(ch)
	return ch, nil
}
func (echoAgent) CreateTaskPushNotificationConfig(_ context.Context, cfg a2a.TaskPushNotificationConfig) (a2a.TaskPushNotificationConfig, error) {
	if cfg.ID == "" {
		cfg.ID = "cfg-1"
	}
	return cfg, nil
}
func (echoAgent) GetTaskPushNotificationConfig(_ context.Context, _ string, _ string) (a2a.TaskPushNotificationConfig, error) {
	return a2a.TaskPushNotificationConfig{ID: "cfg-1"}, nil
}
func (echoAgent) ListTaskPushNotificationConfigs(_ context.Context, _ string) ([]a2a.TaskPushNotificationConfig, error) {
	return nil, nil
}
func (echoAgent) DeleteTaskPushNotificationConfig(_ context.Context, _ string, _ string) error {
	return nil
}
func (echoAgent) GetExtendedAgentCard(_ context.Context) (a2a.AgentCard, error) {
	return a2a.AgentCard{Name: "Echo", Description: "echo test"}, nil
}
