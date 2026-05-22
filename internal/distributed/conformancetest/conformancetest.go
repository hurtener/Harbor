// Package conformancetest exposes the canonical correctness suites
// every distributed driver must pass.
//
// The suites live in a subpackage so the production-code path
// `internal/distributed` does not import the standard library
// `testing` package (precedent: `internal/state/conformancetest`,
// `internal/artifacts/conformancetest`, `internal/tasks/conformancetest`).
//
// Two top-level Run functions:
//
//	RunBus(t, factory)            — MessageBus correctness
//	RunRemoteTransport(t, factory) — RemoteTransport correctness
//
// Downstream drivers (post-V1 durable bus at phase 86, A2A wire
// RemoteTransport at phase 29) wire their own *_test.go that calls
// the matching Run.
//
// The RemoteTransport factory returns a transport plus an
// `AgentBinding` callback so tests can stage Agents (the
// `internal/distributed/drivers/loopback` Agent abstraction). Drivers
// that cannot stage Agents (because they connect to a real network)
// MAY supply a no-op binding; subtests requiring a staged Agent will
// `t.Skip` in that case.
package conformancetest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/distributed"
	"github.com/hurtener/Harbor/internal/distributed/a2a"
	"github.com/hurtener/Harbor/internal/distributed/drivers/loopback"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func tripleA() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"},
		RunID:    "run-a",
	}
}

func tripleB() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-b", UserID: "user-b", SessionID: "session-b"},
		RunID:    "run-b",
	}
}

func ctxWith(q identity.Quadruple) context.Context {
	ctx := context.Background()
	ctx, err := identity.With(ctx, q.Identity)
	if err != nil {
		panic(err)
	}
	if q.RunID != "" {
		ctx, err = identity.WithRun(ctx, q.Identity, q.RunID)
		if err != nil {
			panic(err)
		}
	}
	return ctx
}

// -----------------------------------------------------------------------------
// MessageBus suite
// -----------------------------------------------------------------------------

// BusFactory builds a fresh MessageBus paired with the EventBus it
// projects onto (the conformance suite subscribes there to observe
// delivery), and a cleanup callback. Drivers free to share an
// EventBus instance across N invocations; cleanup MUST close both.
type BusFactory func(t *testing.T) (bus distributed.MessageBus, eb events.EventBus, cleanup func())

// RunBus executes the canonical MessageBus correctness suite.
//
// Subtests:
//
//   - Publish_AtLeastOnce_DeliversToSubscribers
//   - Publish_Identity_Mandatory
//   - Publish_AfterClose_Errors
//   - Concurrent_Publish_NoRace (D-025)
//   - GoroutineLeak_AfterClose
func RunBus(t *testing.T, factory BusFactory) {
	t.Helper()

	t.Run("Publish_AtLeastOnce_DeliversToSubscribers", func(t *testing.T) {
		bus, eb, cleanup := factory(t)
		defer cleanup()

		triple := tripleA()
		sub, err := eb.Subscribe(context.Background(), events.Filter{
			Tenant: triple.TenantID, User: triple.UserID, Session: triple.SessionID,
			Types: []events.EventType{loopback.EventTypeDistributedBusEnvelope},
		})
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		defer sub.Cancel()

		env := distributed.BusEnvelope{
			Edge:      "planner.next",
			Source:    "planner",
			Target:    "memory",
			Identity:  triple,
			TaskID:    "task-1",
			EventID:   "evt-1",
			Payload:   []byte(`{"k":"v"}`),
			Timestamp: time.Now().UTC(),
		}
		if err := bus.Publish(ctxWith(triple), env); err != nil {
			t.Fatalf("publish: %v", err)
		}
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatalf("subscription closed unexpectedly")
			}
			if ev.Type != loopback.EventTypeDistributedBusEnvelope {
				t.Fatalf("type: got %q want %q", ev.Type, loopback.EventTypeDistributedBusEnvelope)
			}
			payload, ok := ev.Payload.(loopback.BusEnvelopePayload)
			if !ok {
				t.Fatalf("payload type: %T", ev.Payload)
			}
			if payload.Envelope.Edge != env.Edge {
				t.Errorf("Edge: got %q want %q", payload.Envelope.Edge, env.Edge)
			}
			if string(payload.Envelope.Payload) != string(env.Payload) {
				t.Errorf("Payload mismatch")
			}
			if payload.Envelope.TaskID != env.TaskID {
				t.Errorf("TaskID: %q != %q", payload.Envelope.TaskID, env.TaskID)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for envelope delivery")
		}
	})

	t.Run("Publish_Identity_Mandatory", func(t *testing.T) {
		bus, _, cleanup := factory(t)
		defer cleanup()

		env := distributed.BusEnvelope{
			Edge:      "x.y",
			EventID:   "evt-1",
			Payload:   []byte(`{}`),
			Timestamp: time.Now().UTC(),
			// Identity intentionally empty.
		}
		err := bus.Publish(context.Background(), env)
		if !errors.Is(err, distributed.ErrIdentityRequired) {
			t.Errorf("Publish with empty identity: got %v want ErrIdentityRequired", err)
		}
	})

	t.Run("Publish_AfterClose_Errors", func(t *testing.T) {
		bus, _, cleanup := factory(t)
		defer cleanup()
		if err := bus.Close(context.Background()); err != nil {
			t.Fatalf("close: %v", err)
		}
		triple := tripleA()
		err := bus.Publish(ctxWith(triple), distributed.BusEnvelope{
			Edge: "x", EventID: "evt", Identity: triple, Payload: []byte(`{}`), Timestamp: time.Now().UTC(),
		})
		if !errors.Is(err, distributed.ErrBusClosed) {
			t.Errorf("Publish after Close: got %v want ErrBusClosed", err)
		}
	})

	t.Run("Concurrent_Publish_NoRace", func(t *testing.T) {
		bus, eb, cleanup := factory(t)
		defer cleanup()

		const workers = 128
		const perWorker = 4

		// Subscribe per-triple so identity isolation is enforced.
		triple := tripleA()
		sub, err := eb.Subscribe(context.Background(), events.Filter{
			Tenant: triple.TenantID, User: triple.UserID, Session: triple.SessionID,
			Types: []events.EventType{loopback.EventTypeDistributedBusEnvelope},
		})
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		defer sub.Cancel()

		var (
			received atomic.Int64
			doneCh   = make(chan struct{})
		)
		go func() {
			for range sub.Events() {
				received.Add(1)
			}
			close(doneCh)
		}()

		var wg sync.WaitGroup
		wg.Add(workers)
		for w := range workers {
			go func(w int) {
				defer wg.Done()
				for i := range perWorker {
					env := distributed.BusEnvelope{
						Edge:      "concurrent",
						Identity:  triple,
						TaskID:    "t-1",
						EventID:   events.EventID(fmt.Sprintf("evt-%d-%d", w, i)),
						Payload:   []byte(`{}`),
						Timestamp: time.Now().UTC(),
					}
					if err := bus.Publish(ctxWith(triple), env); err != nil {
						t.Errorf("publish: %v", err)
						return
					}
				}
			}(w)
		}
		wg.Wait()

		// Drain: wait until received count stabilises at workers*perWorker.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if received.Load() >= int64(workers*perWorker) {
				break
			}
			runtime.Gosched()
		}
		if got, want := received.Load(), int64(workers*perWorker); got < want {
			t.Errorf("received: %d < %d (lost messages or stalled)", got, want)
		}
	})

	t.Run("GoroutineLeak_AfterClose", func(t *testing.T) {
		baseline := runtime.NumGoroutine()
		bus, _, cleanup := factory(t)
		triple := tripleA()
		_ = bus.Publish(ctxWith(triple), distributed.BusEnvelope{ //nolint:errcheck // warm-up publish for a goroutine-leak probe — the publish result is irrelevant to the leak assertion.
			Edge: "x", Identity: triple, EventID: "evt", Payload: []byte(`{}`), Timestamp: time.Now().UTC(),
		})
		if err := bus.Close(context.Background()); err != nil {
			t.Fatalf("close: %v", err)
		}
		cleanup()
		// Allow a short settle window — close may take a beat to flush
		// internal goroutines. We bound the wait, not its rate.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if delta := runtime.NumGoroutine() - baseline; delta <= 2 {
				return
			}
			runtime.Gosched()
		}
		if delta := runtime.NumGoroutine() - baseline; delta > 2 {
			t.Errorf("goroutine leak: baseline=%d now=%d delta=%d", baseline, baseline+delta, delta)
		}
	})
}

// -----------------------------------------------------------------------------
// RemoteTransport suite
// -----------------------------------------------------------------------------

// RemoteTransportFactory builds a fresh RemoteTransport plus an
// AgentBinding callback the conformance suite uses to stage a stub
// Agent. Drivers that cannot stage Agents (real-network drivers)
// supply a nil binding; the relevant subtests skip in that case.
//
// `cleanup` is called at the end of each subtest; it MUST close the
// transport.
type RemoteTransportFactory func(t *testing.T) (transport distributed.RemoteTransport, binding AgentBinding, cleanup func())

// AgentBinding installs an Agent for a target URL on the test's
// transport. Returns the URL the conformance subtests should use for
// req.AgentURL when calling Send / Stream / etc.
type AgentBinding func(url string, agent loopback.Agent)

// RunRemoteTransport executes the canonical RemoteTransport correctness
// suite.
//
// Subtests:
//
//   - Send_RoundTrip
//   - Send_Identity_Mandatory
//   - Stream_OrderedEventsWithDoneTrue
//   - Stream_RespectsClose
//   - GetTask_RoundTrip
//   - GetTask_NotFound
//   - ListTasks_FilterApplied
//   - Cancel_TerminalState
//   - Subscribe_DeliversArtifactAndStatusUpdates
//   - Subscribe_RespectsClose
//   - PushNotificationConfig_Crud_RoundTrip
//   - GetExtendedAgentCard_HappyPath
//   - Concurrent_Send_NoRace (D-025)
//   - GoroutineLeak_AfterClose
func RunRemoteTransport(t *testing.T, factory RemoteTransportFactory) {
	t.Helper()

	const agentURL = "https://agent.example/test"

	t.Run("Send_RoundTrip", func(t *testing.T) {
		tr, bind, cleanup := factory(t)
		defer cleanup()
		if bind == nil {
			t.Skip("driver cannot stage Agents; skipping")
		}
		bind(agentURL, &stubAgent{
			sendMessage: func(ctx context.Context, msg a2a.Message, cfg a2a.SendMessageConfiguration) (a2a.Task, error) {
				return a2a.Task{ID: "task-1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}, nil
			},
		})
		triple := tripleA()
		req := distributed.RemoteCallRequest{
			AgentURL: agentURL,
			Kind:     distributed.RemoteCallKindSend,
			Message:  a2a.Message{MessageID: "m-1", Role: a2a.RoleUser, Parts: a2a.Parts{&a2a.TextPart{Text: "hi"}}},
		}
		res, err := tr.Send(ctxWith(triple), req)
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if res.Task.ID != "task-1" || res.Task.Status.State != a2a.TaskStateCompleted {
			t.Errorf("response Task mismatch: %+v", res.Task)
		}
	})

	t.Run("Send_PropagatesIdentityToAgent", func(t *testing.T) {
		// Identity validation lives at the runtime's distributed-call
		// boundary, NOT inside the transport driver (see remote.go
		// "All methods receive identity via ctx … drivers SHOULD NOT
		// need to re-validate when the runtime owns the ctx" and
		// AGENTS.md §6 rule 9). What the transport MUST guarantee is
		// that the ctx Identity reaches the Agent intact — without
		// propagation, an authenticated runtime call would arrive at
		// the remote endpoint with no scope. This subtest asserts that
		// propagation: the stubbed Agent reads `identity.From(ctx)`
		// and the call only succeeds when the caller supplied one.
		//
		// (A future driver that DOES re-validate at its boundary —
		// e.g. an HTTP+JSON wire driver verifying signed identity
		// headers — would shadow this subtest with its own
		// `Send_RejectsMissingIdentity` test alongside this one.)
		tr, bind, cleanup := factory(t)
		defer cleanup()
		if bind == nil {
			t.Skip("driver cannot stage Agents; skipping")
		}
		bind(agentURL, &stubAgent{
			sendMessage: func(ctx context.Context, msg a2a.Message, cfg a2a.SendMessageConfiguration) (a2a.Task, error) {
				if _, ok := identity.From(ctx); !ok {
					return a2a.Task{}, fmt.Errorf("identity not propagated to Agent")
				}
				return a2a.Task{ID: "t-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}, nil
			},
		})
		req := distributed.RemoteCallRequest{
			AgentURL: agentURL,
			Message:  a2a.Message{MessageID: "m-1", Role: a2a.RoleUser, Parts: a2a.Parts{&a2a.TextPart{Text: "hi"}}},
		}
		triple := tripleA()
		if _, err := tr.Send(ctxWith(triple), req); err != nil {
			t.Errorf("Send with identity: %v", err)
		}
		// Identity-less ctx: transport propagates verbatim; the Agent
		// observes the missing identity and surfaces an error. Proves
		// the driver did NOT inject a default identity along the way.
		if _, err := tr.Send(context.Background(), req); err == nil {
			t.Errorf("Send with missing identity: expected agent-side error, got nil")
		}
	})

	t.Run("Stream_OrderedEventsWithDoneTrue", func(t *testing.T) {
		tr, bind, cleanup := factory(t)
		defer cleanup()
		if bind == nil {
			t.Skip("driver cannot stage Agents; skipping")
		}
		// Stage an Agent that streams 3 events then closes.
		bind(agentURL, &stubAgent{
			sendStreamingMessage: func(ctx context.Context, msg a2a.Message, cfg a2a.SendMessageConfiguration) (<-chan a2a.StreamResponse, error) {
				ch := make(chan a2a.StreamResponse, 3)
				ch <- a2a.StreamResponse{Task: &a2a.Task{ID: "t-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}}
				ch <- a2a.StreamResponse{StatusUpdate: &a2a.TaskStatusUpdateEvent{TaskID: "t-1", ContextID: "c-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}}
				ch <- a2a.StreamResponse{Task: &a2a.Task{ID: "t-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}}
				close(ch)
				return ch, nil
			},
		})
		triple := tripleA()
		stream, err := tr.Stream(ctxWith(triple), distributed.RemoteCallRequest{
			AgentURL: agentURL,
			Kind:     distributed.RemoteCallKindStream,
			Message:  a2a.Message{MessageID: "m-1", Role: a2a.RoleUser, Parts: a2a.Parts{&a2a.TextPart{Text: "go"}}},
		})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		defer stream.Close()
		var seen []string
		for {
			resp, err := stream.Recv(context.Background())
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("Recv: %v", err)
			}
			seen = append(seen, resp.Kind())
		}
		if len(seen) != 3 {
			t.Errorf("events: got %v, want 3", seen)
		}
	})

	t.Run("Stream_RespectsClose", func(t *testing.T) {
		tr, bind, cleanup := factory(t)
		defer cleanup()
		if bind == nil {
			t.Skip("driver cannot stage Agents; skipping")
		}
		// Agent emits one event then waits forever (channel left open).
		held := make(chan a2a.StreamResponse)
		started := make(chan struct{})
		bind(agentURL, &stubAgent{
			sendStreamingMessage: func(ctx context.Context, msg a2a.Message, cfg a2a.SendMessageConfiguration) (<-chan a2a.StreamResponse, error) {
				ch := make(chan a2a.StreamResponse, 1)
				ch <- a2a.StreamResponse{Task: &a2a.Task{ID: "t-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}}
				go func() {
					close(started)
					// Hold open until ctx cancels.
					select {
					case <-ctx.Done():
						close(ch)
					case held <- a2a.StreamResponse{}: // never sent — only path is ctx.Done
					}
				}()
				return ch, nil
			},
		})
		triple := tripleA()
		stream, err := tr.Stream(ctxWith(triple), distributed.RemoteCallRequest{
			AgentURL: agentURL,
			Kind:     distributed.RemoteCallKindStream,
			Message:  a2a.Message{MessageID: "m-1", Role: a2a.RoleUser, Parts: a2a.Parts{&a2a.TextPart{Text: "go"}}},
		})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		_, err = stream.Recv(context.Background())
		if err != nil {
			t.Fatalf("first Recv: %v", err)
		}
		<-started
		if err := stream.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		// Subsequent Recv returns io.EOF promptly.
		recvCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err = stream.Recv(recvCtx)
		if err == nil {
			t.Errorf("Recv after Close: expected error")
		}
	})

	t.Run("GetTask_RoundTrip", func(t *testing.T) {
		tr, bind, cleanup := factory(t)
		defer cleanup()
		if bind == nil {
			t.Skip("driver cannot stage Agents; skipping")
		}
		bind(agentURL, &stubAgent{
			getTask: func(ctx context.Context, taskID, contextID string) (a2a.Task, error) {
				return a2a.Task{ID: taskID, ContextID: contextID, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}, nil
			},
		})
		triple := tripleA()
		snap, err := tr.GetTask(ctxWith(triple), "task-99", "ctx-99")
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if snap == nil || snap.ID != "task-99" {
			t.Errorf("snapshot mismatch: %+v", snap)
		}
	})

	t.Run("GetTask_NotFound", func(t *testing.T) {
		tr, bind, cleanup := factory(t)
		defer cleanup()
		if bind == nil {
			t.Skip("driver cannot stage Agents; skipping")
		}
		bind(agentURL, &stubAgent{
			getTask: func(ctx context.Context, taskID, contextID string) (a2a.Task, error) {
				return a2a.Task{}, distributed.ErrTaskNotFound
			},
		})
		triple := tripleA()
		_, err := tr.GetTask(ctxWith(triple), "missing", "")
		if !errors.Is(err, distributed.ErrTaskNotFound) {
			t.Errorf("expected ErrTaskNotFound, got %v", err)
		}
	})

	t.Run("ListTasks_FilterApplied", func(t *testing.T) {
		tr, bind, cleanup := factory(t)
		defer cleanup()
		if bind == nil {
			t.Skip("driver cannot stage Agents; skipping")
		}
		bind(agentURL, &stubAgent{
			listTasks: func(ctx context.Context, filter loopback.ListTasksFilter) ([]a2a.Task, error) {
				if filter.Status != a2a.TaskStateCompleted {
					return nil, fmt.Errorf("filter.Status: got %v want Completed", filter.Status)
				}
				return []a2a.Task{
					{ID: "t-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}},
					{ID: "t-2", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}},
				}, nil
			},
		})
		triple := tripleA()
		snaps, err := tr.ListTasks(ctxWith(triple), distributed.RemoteTaskFilter{Status: a2a.TaskStateCompleted})
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		if len(snaps) != 2 {
			t.Errorf("want 2 snapshots, got %d", len(snaps))
		}
	})

	t.Run("Cancel_TerminalState", func(t *testing.T) {
		tr, bind, cleanup := factory(t)
		defer cleanup()
		if bind == nil {
			t.Skip("driver cannot stage Agents; skipping")
		}
		var canceled atomic.Bool
		bind(agentURL, &stubAgent{
			cancelTask: func(ctx context.Context, taskID, contextID string) (a2a.Task, error) {
				canceled.Store(true)
				return a2a.Task{ID: taskID, Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}, nil
			},
		})
		triple := tripleA()
		if err := tr.Cancel(ctxWith(triple), "task-1", "ctx-1"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if !canceled.Load() {
			t.Errorf("Cancel did not reach Agent")
		}
	})

	t.Run("Subscribe_DeliversArtifactAndStatusUpdates", func(t *testing.T) {
		tr, bind, cleanup := factory(t)
		defer cleanup()
		if bind == nil {
			t.Skip("driver cannot stage Agents; skipping")
		}
		bind(agentURL, &stubAgent{
			subscribeToTask: func(ctx context.Context, taskID, contextID string) (<-chan a2a.StreamResponse, error) {
				ch := make(chan a2a.StreamResponse, 3)
				ch <- a2a.StreamResponse{StatusUpdate: &a2a.TaskStatusUpdateEvent{TaskID: taskID, ContextID: contextID, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}}
				ch <- a2a.StreamResponse{ArtifactUpdate: &a2a.TaskArtifactUpdateEvent{TaskID: taskID, ContextID: contextID, Artifact: a2a.Artifact{ArtifactID: "a-1", Parts: a2a.Parts{&a2a.TextPart{Text: "chunk1"}}}}}
				ch <- a2a.StreamResponse{StatusUpdate: &a2a.TaskStatusUpdateEvent{TaskID: taskID, ContextID: contextID, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}}
				close(ch)
				return ch, nil
			},
		})
		triple := tripleA()
		stream, err := tr.Subscribe(ctxWith(triple), "task-1", "ctx-1")
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer stream.Close()
		var kinds []string
		for {
			r, err := stream.Recv(context.Background())
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("Recv: %v", err)
			}
			kinds = append(kinds, r.Kind())
		}
		if len(kinds) != 3 {
			t.Errorf("kinds: %v (want 3)", kinds)
		}
		seenArtifact := false
		seenStatus := false
		for _, k := range kinds {
			if k == a2a.StreamResponseKindArtifactUpdate {
				seenArtifact = true
			}
			if k == a2a.StreamResponseKindStatusUpdate {
				seenStatus = true
			}
		}
		if !seenArtifact || !seenStatus {
			t.Errorf("missing variant: artifact=%v status=%v", seenArtifact, seenStatus)
		}
	})

	t.Run("Subscribe_RespectsClose", func(t *testing.T) {
		tr, bind, cleanup := factory(t)
		defer cleanup()
		if bind == nil {
			t.Skip("driver cannot stage Agents; skipping")
		}
		started := make(chan struct{})
		bind(agentURL, &stubAgent{
			subscribeToTask: func(ctx context.Context, taskID, contextID string) (<-chan a2a.StreamResponse, error) {
				ch := make(chan a2a.StreamResponse, 1)
				ch <- a2a.StreamResponse{StatusUpdate: &a2a.TaskStatusUpdateEvent{TaskID: taskID, ContextID: contextID, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}}
				go func() {
					close(started)
					<-ctx.Done()
					close(ch)
				}()
				return ch, nil
			},
		})
		triple := tripleA()
		stream, err := tr.Subscribe(ctxWith(triple), "task-1", "ctx-1")
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		if _, err := stream.Recv(context.Background()); err != nil {
			t.Fatalf("first Recv: %v", err)
		}
		<-started
		if err := stream.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		recvCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := stream.Recv(recvCtx); err == nil {
			t.Errorf("Recv after Close: expected error")
		}
	})

	t.Run("PushNotificationConfig_Crud_RoundTrip", func(t *testing.T) {
		tr, bind, cleanup := factory(t)
		defer cleanup()
		if bind == nil {
			t.Skip("driver cannot stage Agents; skipping")
		}
		// In-memory store keyed by (TaskID, configID).
		store := map[string]a2a.TaskPushNotificationConfig{}
		var storeMu sync.Mutex
		bind(agentURL, &stubAgent{
			createTaskPushNotificationConfig: func(ctx context.Context, cfg a2a.TaskPushNotificationConfig) (a2a.TaskPushNotificationConfig, error) {
				storeMu.Lock()
				defer storeMu.Unlock()
				if cfg.ID == "" {
					cfg.ID = fmt.Sprintf("cfg-%d", len(store)+1)
				}
				store[cfg.TaskID+"/"+cfg.ID] = cfg
				return cfg, nil
			},
			getTaskPushNotificationConfig: func(ctx context.Context, taskID, configID string) (a2a.TaskPushNotificationConfig, error) {
				storeMu.Lock()
				defer storeMu.Unlock()
				cfg, ok := store[taskID+"/"+configID]
				if !ok {
					return a2a.TaskPushNotificationConfig{}, distributed.ErrTaskNotFound
				}
				return cfg, nil
			},
			listTaskPushNotificationConfigs: func(ctx context.Context, taskID string) ([]a2a.TaskPushNotificationConfig, error) {
				storeMu.Lock()
				defer storeMu.Unlock()
				var out []a2a.TaskPushNotificationConfig
				for k, v := range store {
					if len(k) > len(taskID) && k[:len(taskID)] == taskID {
						out = append(out, v)
					}
				}
				return out, nil
			},
			deleteTaskPushNotificationConfig: func(ctx context.Context, taskID, configID string) error {
				storeMu.Lock()
				defer storeMu.Unlock()
				delete(store, taskID+"/"+configID)
				return nil
			},
		})
		triple := tripleA()
		ctx := ctxWith(triple)
		cfg := a2a.TaskPushNotificationConfig{TaskID: "task-1", URL: "https://callback/x", Token: "t"}
		got, err := tr.CreateTaskPushNotificationConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.ID == "" {
			t.Errorf("Create: empty ID")
		}
		fetched, err := tr.GetTaskPushNotificationConfig(ctx, "task-1", got.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if fetched.URL != cfg.URL {
			t.Errorf("Get: URL mismatch")
		}
		list, err := tr.ListTaskPushNotificationConfigs(ctx, "task-1")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 1 {
			t.Errorf("List: want 1, got %d", len(list))
		}
		if err := tr.DeleteTaskPushNotificationConfig(ctx, "task-1", got.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		list, err = tr.ListTaskPushNotificationConfigs(ctx, "task-1")
		if err != nil {
			t.Fatalf("List post-Delete: %v", err)
		}
		if len(list) != 0 {
			t.Errorf("List post-Delete: want 0, got %d", len(list))
		}
	})

	t.Run("GetExtendedAgentCard_HappyPath", func(t *testing.T) {
		tr, bind, cleanup := factory(t)
		defer cleanup()
		if bind == nil {
			t.Skip("driver cannot stage Agents; skipping")
		}
		bind(agentURL, &stubAgent{
			getExtendedAgentCard: func(ctx context.Context) (a2a.AgentCard, error) {
				return a2a.AgentCard{
					Name:                "Test Agent",
					Description:         "Conformance stub",
					Version:             "1.0.0",
					SupportedInterfaces: []a2a.AgentInterface{{URL: agentURL, ProtocolBinding: a2a.ProtocolBindingHTTPJSON, ProtocolVersion: "1.0"}},
					Capabilities:        a2a.AgentCapabilities{},
					DefaultInputModes:   []string{"text/plain"},
					DefaultOutputModes:  []string{"text/plain"},
					Skills:              []a2a.AgentSkill{{ID: "s-1", Name: "echo", Description: "echo back", Tags: []string{"test"}}},
				}, nil
			},
		})
		triple := tripleA()
		card, err := tr.GetExtendedAgentCard(ctxWith(triple))
		if err != nil {
			t.Fatalf("GetExtendedAgentCard: %v", err)
		}
		if card == nil || card.Name != "Test Agent" {
			t.Errorf("card mismatch: %+v", card)
		}
	})

	t.Run("Concurrent_Send_NoRace", func(t *testing.T) {
		tr, bind, cleanup := factory(t)
		defer cleanup()
		if bind == nil {
			t.Skip("driver cannot stage Agents; skipping")
		}
		var calls atomic.Int64
		bind(agentURL, &stubAgent{
			sendMessage: func(ctx context.Context, msg a2a.Message, cfg a2a.SendMessageConfiguration) (a2a.Task, error) {
				calls.Add(1)
				// Read identity to assert no cross-bleed.
				if _, ok := identity.From(ctx); !ok {
					return a2a.Task{}, fmt.Errorf("identity missing")
				}
				return a2a.Task{ID: msg.MessageID, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}, nil
			},
		})
		const workers = 128
		var wg sync.WaitGroup
		wg.Add(workers)
		errs := make(chan error, workers)
		for w := range workers {
			triple := tripleA()
			if w%2 == 1 {
				triple = tripleB()
			}
			go func(w int, triple identity.Quadruple) {
				defer wg.Done()
				req := distributed.RemoteCallRequest{
					AgentURL: agentURL,
					Message: a2a.Message{
						MessageID: fmt.Sprintf("m-%d", w),
						Role:      a2a.RoleUser,
						Parts:     a2a.Parts{&a2a.TextPart{Text: "concurrent"}},
					},
				}
				if _, err := tr.Send(ctxWith(triple), req); err != nil {
					errs <- err
				}
			}(w, triple)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("Send: %v", err)
		}
		if got := calls.Load(); got != workers {
			t.Errorf("calls: got %d want %d", got, workers)
		}
	})

	t.Run("GoroutineLeak_AfterClose", func(t *testing.T) {
		baseline := runtime.NumGoroutine()
		tr, bind, cleanup := factory(t)
		if bind == nil {
			cleanup()
			t.Skip("driver cannot stage Agents; skipping")
		}
		bind(agentURL, &stubAgent{
			sendStreamingMessage: func(ctx context.Context, msg a2a.Message, cfg a2a.SendMessageConfiguration) (<-chan a2a.StreamResponse, error) {
				ch := make(chan a2a.StreamResponse, 1)
				ch <- a2a.StreamResponse{Task: &a2a.Task{ID: "t-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}}
				close(ch)
				return ch, nil
			},
		})
		triple := tripleA()
		stream, err := tr.Stream(ctxWith(triple), distributed.RemoteCallRequest{
			AgentURL: agentURL,
			Kind:     distributed.RemoteCallKindStream,
			Message:  a2a.Message{MessageID: "m-1", Role: a2a.RoleUser, Parts: a2a.Parts{&a2a.TextPart{Text: "go"}}},
		})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		// Drain.
		for {
			if _, err := stream.Recv(context.Background()); err != nil {
				break
			}
		}
		_ = stream.Close()
		if err := tr.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		cleanup()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if delta := runtime.NumGoroutine() - baseline; delta <= 2 {
				return
			}
			runtime.Gosched()
		}
		if delta := runtime.NumGoroutine() - baseline; delta > 2 {
			t.Errorf("goroutine leak: baseline=%d delta=%d", baseline, delta)
		}
	})
}

// -----------------------------------------------------------------------------
// stubAgent — function-pointer-driven Agent for the conformance suite
// -----------------------------------------------------------------------------

// stubAgent is a function-pointer-driven Agent. Subtests assign one
// or more callbacks; unset methods panic if invoked (signalling the
// driver called a method the subtest didn't stage).
type stubAgent struct {
	sendMessage                      func(ctx context.Context, msg a2a.Message, cfg a2a.SendMessageConfiguration) (a2a.Task, error)
	sendStreamingMessage             func(ctx context.Context, msg a2a.Message, cfg a2a.SendMessageConfiguration) (<-chan a2a.StreamResponse, error)
	getTask                          func(ctx context.Context, taskID, contextID string) (a2a.Task, error)
	listTasks                        func(ctx context.Context, filter loopback.ListTasksFilter) ([]a2a.Task, error)
	cancelTask                       func(ctx context.Context, taskID, contextID string) (a2a.Task, error)
	subscribeToTask                  func(ctx context.Context, taskID, contextID string) (<-chan a2a.StreamResponse, error)
	createTaskPushNotificationConfig func(ctx context.Context, cfg a2a.TaskPushNotificationConfig) (a2a.TaskPushNotificationConfig, error)
	getTaskPushNotificationConfig    func(ctx context.Context, taskID, configID string) (a2a.TaskPushNotificationConfig, error)
	listTaskPushNotificationConfigs  func(ctx context.Context, taskID string) ([]a2a.TaskPushNotificationConfig, error)
	deleteTaskPushNotificationConfig func(ctx context.Context, taskID, configID string) error
	getExtendedAgentCard             func(ctx context.Context) (a2a.AgentCard, error)
}

func (s *stubAgent) SendMessage(ctx context.Context, msg a2a.Message, cfg a2a.SendMessageConfiguration) (a2a.Task, error) {
	if s.sendMessage == nil {
		return a2a.Task{}, fmt.Errorf("stubAgent.SendMessage: not staged")
	}
	return s.sendMessage(ctx, msg, cfg)
}

func (s *stubAgent) SendStreamingMessage(ctx context.Context, msg a2a.Message, cfg a2a.SendMessageConfiguration) (<-chan a2a.StreamResponse, error) {
	if s.sendStreamingMessage == nil {
		return nil, fmt.Errorf("stubAgent.SendStreamingMessage: not staged")
	}
	return s.sendStreamingMessage(ctx, msg, cfg)
}

func (s *stubAgent) GetTask(ctx context.Context, taskID, contextID string) (a2a.Task, error) {
	if s.getTask == nil {
		return a2a.Task{}, fmt.Errorf("stubAgent.GetTask: not staged")
	}
	return s.getTask(ctx, taskID, contextID)
}

func (s *stubAgent) ListTasks(ctx context.Context, filter loopback.ListTasksFilter) ([]a2a.Task, error) {
	if s.listTasks == nil {
		return nil, fmt.Errorf("stubAgent.ListTasks: not staged")
	}
	return s.listTasks(ctx, filter)
}

func (s *stubAgent) CancelTask(ctx context.Context, taskID, contextID string) (a2a.Task, error) {
	if s.cancelTask == nil {
		return a2a.Task{}, fmt.Errorf("stubAgent.CancelTask: not staged")
	}
	return s.cancelTask(ctx, taskID, contextID)
}

func (s *stubAgent) SubscribeToTask(ctx context.Context, taskID, contextID string) (<-chan a2a.StreamResponse, error) {
	if s.subscribeToTask == nil {
		return nil, fmt.Errorf("stubAgent.SubscribeToTask: not staged")
	}
	return s.subscribeToTask(ctx, taskID, contextID)
}

func (s *stubAgent) CreateTaskPushNotificationConfig(ctx context.Context, cfg a2a.TaskPushNotificationConfig) (a2a.TaskPushNotificationConfig, error) {
	if s.createTaskPushNotificationConfig == nil {
		return a2a.TaskPushNotificationConfig{}, fmt.Errorf("stubAgent.CreateTaskPushNotificationConfig: not staged")
	}
	return s.createTaskPushNotificationConfig(ctx, cfg)
}

func (s *stubAgent) GetTaskPushNotificationConfig(ctx context.Context, taskID, configID string) (a2a.TaskPushNotificationConfig, error) {
	if s.getTaskPushNotificationConfig == nil {
		return a2a.TaskPushNotificationConfig{}, fmt.Errorf("stubAgent.GetTaskPushNotificationConfig: not staged")
	}
	return s.getTaskPushNotificationConfig(ctx, taskID, configID)
}

func (s *stubAgent) ListTaskPushNotificationConfigs(ctx context.Context, taskID string) ([]a2a.TaskPushNotificationConfig, error) {
	if s.listTaskPushNotificationConfigs == nil {
		return nil, fmt.Errorf("stubAgent.ListTaskPushNotificationConfigs: not staged")
	}
	return s.listTaskPushNotificationConfigs(ctx, taskID)
}

func (s *stubAgent) DeleteTaskPushNotificationConfig(ctx context.Context, taskID, configID string) error {
	if s.deleteTaskPushNotificationConfig == nil {
		return fmt.Errorf("stubAgent.DeleteTaskPushNotificationConfig: not staged")
	}
	return s.deleteTaskPushNotificationConfig(ctx, taskID, configID)
}

func (s *stubAgent) GetExtendedAgentCard(ctx context.Context) (a2a.AgentCard, error) {
	if s.getExtendedAgentCard == nil {
		return a2a.AgentCard{}, fmt.Errorf("stubAgent.GetExtendedAgentCard: not staged")
	}
	return s.getExtendedAgentCard(ctx)
}
