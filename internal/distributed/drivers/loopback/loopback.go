package loopback

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/distributed"
	"github.com/hurtener/Harbor/internal/distributed/a2a"
	"github.com/hurtener/Harbor/internal/events"
)

func init() {
	distributed.RegisterBus(distributed.DefaultDriver, func(deps distributed.Dependencies) (distributed.MessageBus, error) {
		return NewBus(deps)
	})
	distributed.RegisterRemoteTransport(distributed.DefaultDriver, func(deps distributed.Dependencies) (distributed.RemoteTransport, error) {
		return NewRemoteTransport(deps)
	})
}

// -----------------------------------------------------------------------------
// MessageBus loopback — projects envelopes onto the typed event bus
// -----------------------------------------------------------------------------

// EventTypeDistributedBusEnvelope is the canonical event type the
// loopback MessageBus emits onto the typed `events.EventBus` for each
// Publish. Subscribers wired to the event bus see envelopes by
// filtering on this type. Registered once at package init.
const EventTypeDistributedBusEnvelope events.EventType = "distributed.bus_envelope"

func init() {
	events.RegisterEventType(EventTypeDistributedBusEnvelope)
}

// BusEnvelopePayload is the typed event payload carrying a
// distributed.BusEnvelope projection. SafePayload — the envelope's
// Payload bytes are assumed pre-redacted by the publisher (D-020).
type BusEnvelopePayload struct {
	events.SafeSealed
	// Envelope is the published BusEnvelope, projected onto the typed
	// event bus. Consumers idempotency-key on
	// `(Envelope.TaskID, Envelope.Edge, Envelope.EventID)`.
	Envelope distributed.BusEnvelope
}

// bus is the loopback MessageBus driver.
type bus struct {
	eventBus events.EventBus
	closed   atomic.Bool
}

// NewBus builds the loopback MessageBus directly. Exposed for tests
// that want to skip the registry.
func NewBus(deps distributed.Dependencies) (distributed.MessageBus, error) {
	if deps.EventBus == nil {
		return nil, fmt.Errorf("distributed/loopback: NewBus requires a non-nil EventBus dependency")
	}
	return &bus{eventBus: deps.EventBus}, nil
}

// Publish projects env onto the typed event bus as an Event of type
// EventTypeDistributedBusEnvelope with the envelope wrapped in a
// BusEnvelopePayload. At-least-once contract is trivially satisfied
// in-process; consumers MUST still be idempotent on
// `(TaskID, Edge, EventID)` so the same consumer code works against
// post-V1 durable drivers.
func (b *bus) Publish(ctx context.Context, env distributed.BusEnvelope) error {
	if b.closed.Load() {
		return distributed.ErrBusClosed
	}
	if err := env.Validate(); err != nil {
		return err
	}
	ev := events.Event{
		Type:       EventTypeDistributedBusEnvelope,
		Identity:   env.Identity,
		OccurredAt: env.Timestamp,
		Payload:    BusEnvelopePayload{Envelope: env},
	}
	if err := b.eventBus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("distributed/loopback: bus publish: %w", err)
	}
	return nil
}

// Close flips the closed flag; subsequent Publish returns ErrBusClosed.
// Idempotent. The driver owns no goroutines (the event bus does), so
// Close is fast and never blocks.
func (b *bus) Close(_ context.Context) error {
	b.closed.Store(true)
	return nil
}

// -----------------------------------------------------------------------------
// RemoteTransport loopback — in-memory agent registry
// -----------------------------------------------------------------------------

// transport is the loopback RemoteTransport driver. Maintains an
// in-memory map of agent URLs to Agent implementations. Every
// RemoteTransport method dispatches synchronously to the registered
// Agent for the target URL.
type transport struct {
	mu         sync.RWMutex
	agents     map[string]Agent     // by URL
	defaultURL string               // URL used when a call's AgentURL is empty
	cardSource func() *a2a.AgentCard // populates GetExtendedAgentCard if set
	streams    sync.WaitGroup       // tracks open stream goroutines for GoroutineLeak gate
	closed     atomic.Bool
}

// NewRemoteTransport builds the loopback RemoteTransport directly.
// Exposed for tests that want to skip the registry.
//
// Deps are intentionally ignored: the loopback driver is in-process
// dispatch — it has no EventBus / Cfg consumers at V1. The Phase 29
// wire RemoteTransport driver (post-V1) WILL read deps.EventBus to
// surface transport-level events (`distributed.send_failed` etc.)
// and deps.Cfg for endpoint configuration; reviewers porting this
// signature forward to the wire driver MUST replace this stub with
// real deps consumption.
func NewRemoteTransport(_ distributed.Dependencies) (distributed.RemoteTransport, error) {
	return &transport{
		agents: map[string]Agent{},
	}, nil
}

// LoopbackTransport is the concrete type exposed for tests so they can
// register / unregister Agents against the in-memory registry without
// resolving through the interface.
type LoopbackTransport interface {
	distributed.RemoteTransport
	// RegisterAgent installs agent for url. Replaces any prior
	// registration at the same URL. The first registration becomes
	// the "default" URL for GetExtendedAgentCard when no AgentURL
	// is set on the request.
	RegisterAgent(url string, agent Agent)
	// UnregisterAgent removes the agent at url. No-op if absent.
	UnregisterAgent(url string)
	// SetDefaultAgentCard sets the AgentCard returned by
	// GetExtendedAgentCard when the registered Agent itself returns
	// a zero value (test convenience).
	SetDefaultAgentCard(source func() *a2a.AgentCard)
}

// RegisterAgent installs agent for url.
func (t *transport) RegisterAgent(url string, agent Agent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.agents[url] = agent
	if t.defaultURL == "" {
		t.defaultURL = url
	}
}

// UnregisterAgent removes the agent at url.
func (t *transport) UnregisterAgent(url string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.agents, url)
	if t.defaultURL == url {
		t.defaultURL = ""
	}
}

// SetDefaultAgentCard sets the fallback AgentCard source.
func (t *transport) SetDefaultAgentCard(source func() *a2a.AgentCard) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cardSource = source
}

// resolveAgent returns the Agent registered for req.AgentURL (or the
// default URL when req.AgentURL is empty). Returns ErrAgentNotFound
// when no Agent is registered.
func (t *transport) resolveAgent(url string) (Agent, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if url == "" {
		url = t.defaultURL
	}
	a, ok := t.agents[url]
	if !ok {
		return nil, fmt.Errorf("%w: url=%q", distributed.ErrAgentNotFound, url)
	}
	return a, nil
}

// closedErr returns ErrTransportClosed when the transport is closed,
// nil otherwise.
func (t *transport) closedErr() error {
	if t.closed.Load() {
		return distributed.ErrTransportClosed
	}
	return nil
}

// Send maps to A2A SendMessage.
func (t *transport) Send(ctx context.Context, req distributed.RemoteCallRequest) (distributed.RemoteCallResult, error) {
	if err := t.closedErr(); err != nil {
		return distributed.RemoteCallResult{}, err
	}
	agent, err := t.resolveAgent(req.AgentURL)
	if err != nil {
		return distributed.RemoteCallResult{}, err
	}
	ctx, cancel := withTimeout(ctx, req.Timeout)
	defer cancel()
	task, err := agent.SendMessage(ctx, req.Message, req.Config)
	if err != nil {
		return distributed.RemoteCallResult{}, fmt.Errorf("distributed/loopback: send: %w", err)
	}
	return distributed.RemoteCallResult{Task: task}, nil
}

// Stream maps to A2A SendStreamingMessage or SubscribeToTask depending
// on req.Kind.
func (t *transport) Stream(ctx context.Context, req distributed.RemoteCallRequest) (distributed.RemoteEventStream, error) {
	if err := t.closedErr(); err != nil {
		return nil, err
	}
	agent, err := t.resolveAgent(req.AgentURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := withTimeout(ctx, req.Timeout)
	var src <-chan a2a.StreamResponse
	switch req.Kind {
	case distributed.RemoteCallKindSubscribe:
		taskID := req.TaskID
		ctxID := req.ContextID
		src, err = agent.SubscribeToTask(ctx, taskID, ctxID)
	case "", distributed.RemoteCallKindSend, distributed.RemoteCallKindStream:
		src, err = agent.SendStreamingMessage(ctx, req.Message, req.Config)
	default:
		cancel()
		return nil, fmt.Errorf("distributed/loopback: stream: unknown kind %q", req.Kind)
	}
	if err != nil {
		cancel()
		return nil, fmt.Errorf("distributed/loopback: stream: %w", err)
	}
	return newStream(t, ctx, cancel, src), nil
}

// GetTask maps to A2A GetTask.
func (t *transport) GetTask(ctx context.Context, taskID, contextID string) (*distributed.RemoteTaskSnapshot, error) {
	if err := t.closedErr(); err != nil {
		return nil, err
	}
	agent, err := t.resolveAgent("")
	if err != nil {
		return nil, err
	}
	task, err := agent.GetTask(ctx, taskID, contextID)
	if err != nil {
		return nil, fmt.Errorf("distributed/loopback: get_task: %w", err)
	}
	snapshot := distributed.RemoteTaskSnapshot(task)
	return &snapshot, nil
}

// ListTasks maps to A2A ListTasks.
func (t *transport) ListTasks(ctx context.Context, filter distributed.RemoteTaskFilter) ([]distributed.RemoteTaskSnapshot, error) {
	if err := t.closedErr(); err != nil {
		return nil, err
	}
	agent, err := t.resolveAgent("")
	if err != nil {
		return nil, err
	}
	af := ListTasksFilter{
		Tenant:           filter.Tenant,
		ContextID:        filter.ContextID,
		Status:           filter.Status,
		PageSize:         filter.PageSize,
		PageToken:        filter.PageToken,
		HistoryLength:    filter.HistoryLength,
		IncludeArtifacts: filter.IncludeArtifacts,
	}
	if !filter.StatusTimestampAfter.IsZero() {
		af.StatusTimestampAfter = filter.StatusTimestampAfter.UnixNano()
	}
	tasks, err := agent.ListTasks(ctx, af)
	if err != nil {
		return nil, fmt.Errorf("distributed/loopback: list_tasks: %w", err)
	}
	out := make([]distributed.RemoteTaskSnapshot, len(tasks))
	for i, t := range tasks {
		out[i] = distributed.RemoteTaskSnapshot(t)
	}
	return out, nil
}

// Cancel maps to A2A CancelTask.
func (t *transport) Cancel(ctx context.Context, taskID, contextID string) error {
	if err := t.closedErr(); err != nil {
		return err
	}
	agent, err := t.resolveAgent("")
	if err != nil {
		return err
	}
	if _, err := agent.CancelTask(ctx, taskID, contextID); err != nil {
		return fmt.Errorf("distributed/loopback: cancel: %w", err)
	}
	return nil
}

// Subscribe maps to A2A SubscribeToTask.
func (t *transport) Subscribe(ctx context.Context, taskID, contextID string) (distributed.RemoteTaskEventStream, error) {
	if err := t.closedErr(); err != nil {
		return nil, err
	}
	agent, err := t.resolveAgent("")
	if err != nil {
		return nil, err
	}
	subCtx, cancel := context.WithCancel(ctx)
	src, err := agent.SubscribeToTask(subCtx, taskID, contextID)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("distributed/loopback: subscribe: %w", err)
	}
	return newStream(t, subCtx, cancel, src), nil
}

// CreateTaskPushNotificationConfig maps to A2A
// CreateTaskPushNotificationConfig.
func (t *transport) CreateTaskPushNotificationConfig(ctx context.Context, cfg a2a.TaskPushNotificationConfig) (a2a.TaskPushNotificationConfig, error) {
	if err := t.closedErr(); err != nil {
		return a2a.TaskPushNotificationConfig{}, err
	}
	agent, err := t.resolveAgent("")
	if err != nil {
		return a2a.TaskPushNotificationConfig{}, err
	}
	out, err := agent.CreateTaskPushNotificationConfig(ctx, cfg)
	if err != nil {
		return a2a.TaskPushNotificationConfig{}, fmt.Errorf("distributed/loopback: push_config.create: %w", err)
	}
	return out, nil
}

// GetTaskPushNotificationConfig maps to A2A
// GetTaskPushNotificationConfig.
func (t *transport) GetTaskPushNotificationConfig(ctx context.Context, taskID, configID string) (a2a.TaskPushNotificationConfig, error) {
	if err := t.closedErr(); err != nil {
		return a2a.TaskPushNotificationConfig{}, err
	}
	agent, err := t.resolveAgent("")
	if err != nil {
		return a2a.TaskPushNotificationConfig{}, err
	}
	out, err := agent.GetTaskPushNotificationConfig(ctx, taskID, configID)
	if err != nil {
		return a2a.TaskPushNotificationConfig{}, fmt.Errorf("distributed/loopback: push_config.get: %w", err)
	}
	return out, nil
}

// ListTaskPushNotificationConfigs maps to A2A
// ListTaskPushNotificationConfigs.
func (t *transport) ListTaskPushNotificationConfigs(ctx context.Context, taskID string) ([]a2a.TaskPushNotificationConfig, error) {
	if err := t.closedErr(); err != nil {
		return nil, err
	}
	agent, err := t.resolveAgent("")
	if err != nil {
		return nil, err
	}
	out, err := agent.ListTaskPushNotificationConfigs(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("distributed/loopback: push_config.list: %w", err)
	}
	return out, nil
}

// DeleteTaskPushNotificationConfig maps to A2A
// DeleteTaskPushNotificationConfig.
func (t *transport) DeleteTaskPushNotificationConfig(ctx context.Context, taskID, configID string) error {
	if err := t.closedErr(); err != nil {
		return err
	}
	agent, err := t.resolveAgent("")
	if err != nil {
		return err
	}
	if err := agent.DeleteTaskPushNotificationConfig(ctx, taskID, configID); err != nil {
		return fmt.Errorf("distributed/loopback: push_config.delete: %w", err)
	}
	return nil
}

// GetExtendedAgentCard maps to A2A GetExtendedAgentCard.
func (t *transport) GetExtendedAgentCard(ctx context.Context) (*a2a.AgentCard, error) {
	if err := t.closedErr(); err != nil {
		return nil, err
	}
	t.mu.RLock()
	source := t.cardSource
	t.mu.RUnlock()
	if source != nil {
		card := source()
		if card != nil {
			return card, nil
		}
	}
	agent, err := t.resolveAgent("")
	if err != nil {
		return nil, err
	}
	card, err := agent.GetExtendedAgentCard(ctx)
	if err != nil {
		return nil, fmt.Errorf("distributed/loopback: get_extended_agent_card: %w", err)
	}
	return &card, nil
}

// Close releases any driver-held resources and waits for in-flight
// stream goroutines to finish. Idempotent.
func (t *transport) Close(_ context.Context) error {
	if !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	t.streams.Wait()
	return nil
}

// -----------------------------------------------------------------------------
// Stream wrapper
// -----------------------------------------------------------------------------

// stream wraps a channel of a2a.StreamResponse plus the cancel
// function that releases the Agent-side context. The transport's
// WaitGroup tracks open streams so Close can wait for them.
type stream struct {
	t      *transport
	ctx    context.Context
	cancel context.CancelFunc
	src    <-chan a2a.StreamResponse
	closed atomic.Bool
}

func newStream(t *transport, ctx context.Context, cancel context.CancelFunc, src <-chan a2a.StreamResponse) *stream {
	t.streams.Add(1)
	return &stream{t: t, ctx: ctx, cancel: cancel, src: src}
}

// Recv returns the next StreamResponse or an error.
//
// Termination signals:
//   - The source channel is closed → returns an error wrapping io.EOF.
//   - ctx.Done() fires → returns ctx.Err().
//   - Close() was called → returns an error wrapping io.EOF.
func (s *stream) Recv(ctx context.Context) (a2a.StreamResponse, error) {
	if s.closed.Load() {
		return a2a.StreamResponse{}, io.EOF
	}
	select {
	case <-ctx.Done():
		return a2a.StreamResponse{}, ctx.Err()
	case <-s.ctx.Done():
		return a2a.StreamResponse{}, s.ctx.Err()
	case resp, ok := <-s.src:
		if !ok {
			return a2a.StreamResponse{}, io.EOF
		}
		return resp, nil
	}
}

// Close releases the stream. Idempotent.
func (s *stream) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.cancel()
	s.t.streams.Done()
	return nil
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// withTimeout wraps ctx with a deadline iff timeout > 0. Otherwise
// returns ctx + a no-op cancel.
func withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// Compile-time guards.
var (
	_ distributed.MessageBus      = (*bus)(nil)
	_ distributed.RemoteTransport = (*transport)(nil)
	_ LoopbackTransport           = (*transport)(nil)
)

