// Package a2a is Harbor's southbound A2A wire driver.
//
// It implements `distributed.RemoteTransport` against the full A2A v1
// spec (vendored at `docs/specifications/a2a.proto`, pinned in
// `docs/specifications/README.md`). Phase 22 froze the Go shapes
// (`internal/distributed/a2a/types.go`) and the contract surface
// (`internal/distributed/remote.go`); Phase 29 ships the wire
// realisation: JSON-RPC 2.0 over HTTPS for unary calls, Server-Sent
// Events for streaming, and `GET <peer>/.well-known/agent-card.json`
// for discovery.
//
// Driver registration. `init()` calls
// `distributed.RegisterRemoteTransport("a2a", factory)` so
// `distributed.OpenRemoteTransport` resolves `"a2a"` after the
// `cmd/harbor/main.go` blank import fires.
//
// Configuration. The driver reads its peer list from
// `Dependencies.Cfg.Tools.A2APeers` (Phase 29 introduces
// `config.ToolsConfig` — see `internal/config`). Each peer carries
// (URL, TrustTier, LatencyTierMS, AllowInsecureLoopback, AgentCardTTL).
// HTTPS is required by default; HTTP is accepted only when the host is
// loopback or `AllowInsecureLoopback` is set. The driver builds its
// internal `Registry` from this list at construction.
//
// Discovery. Before a peer's first call, the driver fetches its
// AgentCard via `GET <peer>/.well-known/agent-card.json`, validates
// it against the Phase 22 Go shapes, caches it (per-peer TTL,
// default 10 minutes), and locates the JSONRPC AgentInterface (the
// peer's HTTP endpoint for JSON-RPC calls). Peers that advertise no
// JSONRPC interface fail with `ErrNoJSONRPCInterface`.
//
// Identity propagation (AGENTS.md §6 rule 9). Every outbound call
// reads `identity.Identity` from `ctx` and stamps the triple onto
// the request headers (`X-Harbor-Tenant` / `X-Harbor-User` /
// `X-Harbor-Session`). The wire driver does NOT validate identity at
// its boundary — Phase 22's contract puts that responsibility on the
// runtime above; the wire driver propagates verbatim so a missing
// identity surfaces at the peer.
//
// Reliability shell (D-024). The wire driver does NOT add its own
// retry/timeout shell. `ToolPolicy` (Phase 26) wraps every Tool
// invocation that routes through this driver; double-wrapping is
// forbidden per D-024.
//
// Concurrent reuse (D-025). The driver is safe for N concurrent
// goroutines against a single instance: the `*http.Client` is shared,
// the JSON-RPC ID counter is atomic, the AgentCard cache + Registry
// take an RWMutex on reads, and per-call state (request body,
// response stream, parsed envelope) lives on the goroutine stack.
package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/distributed"
	"github.com/hurtener/Harbor/internal/distributed/a2a"
	"github.com/hurtener/Harbor/internal/identity"
)

// DriverName is the registry key for `distributed.OpenRemoteTransport`.
const DriverName = "a2a"

func init() {
	distributed.RegisterRemoteTransport(DriverName, func(deps distributed.Dependencies) (distributed.RemoteTransport, error) {
		return New(deps)
	})
}

// Option configures the wire driver at construction.
type Option func(*driverConfig)

// driverConfig accumulates option settings.
type driverConfig struct {
	httpc        *http.Client
	agentCardTTL time.Duration
	registry     *Registry
	now          func() time.Time
}

// WithHTTPClient overrides the default *http.Client. Useful for tests
// that bind to httptest.Server (an explicit InsecureSkipVerify or
// per-test transport tuning lives on the supplied client).
func WithHTTPClient(c *http.Client) Option {
	return func(cfg *driverConfig) { cfg.httpc = c }
}

// WithAgentCardTTL overrides the AgentCard cache TTL (default 10min).
func WithAgentCardTTL(d time.Duration) Option {
	return func(cfg *driverConfig) { cfg.agentCardTTL = d }
}

// WithRegistry seeds the driver's route registry with a fixed peer
// list (skipping the `Dependencies.Cfg.Tools.A2APeers` lookup). The
// caller retains ownership; the driver does not Close the registry.
func WithRegistry(r *Registry) Option {
	return func(cfg *driverConfig) { cfg.registry = r }
}

// withClock injects a deterministic clock for cache-TTL tests.
func withClock(now func() time.Time) Option {
	return func(cfg *driverConfig) { cfg.now = now }
}

// transport is the wire driver state. Constructed once; safe for N
// concurrent goroutines against the single instance (D-025).
type transport struct {
	httpc        *http.Client
	registry     *Registry
	cardCache    *agentCardCache
	jsonRPC      *jsonRPCClient
	agentCardTTL time.Duration

	// streams tracks open SSE / Subscribe streams so Close can wait
	// for them.
	streams sync.WaitGroup

	closed atomic.Bool
}

// New constructs a wire RemoteTransport. Returns an error when:
//   - Any configured peer has an invalid URL or violates the HTTPS-only
//     rule.
//   - No peers are configured AND no `WithRegistry` override is supplied
//     (the driver is then not callable — fail at construction).
func New(deps distributed.Dependencies, opts ...Option) (distributed.RemoteTransport, error) {
	cfg := driverConfig{
		httpc:        http.DefaultClient,
		agentCardTTL: defaultAgentCardTTL,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	reg := cfg.registry
	if reg == nil {
		built, err := buildRegistryFromConfig(deps.Tools.A2APeers)
		if err != nil {
			return nil, err
		}
		reg = built
	}
	if len(reg.Peers()) == 0 {
		return nil, fmt.Errorf("a2a.New: no peers configured (set ToolsConfig.A2APeers or use WithRegistry)")
	}

	return &transport{
		httpc:        cfg.httpc,
		registry:     reg,
		cardCache:    newAgentCardCache(cfg.httpc, cfg.now),
		jsonRPC:      newJSONRPCClient(cfg.httpc),
		agentCardTTL: cfg.agentCardTTL,
	}, nil
}

// buildRegistryFromConfig translates ToolsConfig.A2APeers into a
// Registry. Returns the first invalid-peer error so config validation
// fails fast.
func buildRegistryFromConfig(peers []config.A2APeerConfig) (*Registry, error) {
	reg := NewRegistry()
	for i, p := range peers {
		if _, err := validatePeerURL(p.URL, p.AllowInsecureLoopback); err != nil {
			return nil, fmt.Errorf("a2a.New: peers[%d]: %w", i, err)
		}
		spec := PeerSpec{
			URL:                   strings.TrimRight(p.URL, "/"),
			TrustTier:             p.TrustTier,
			LatencyTierMS:         p.LatencyTierMS,
			AllowInsecureLoopback: p.AllowInsecureLoopback,
			AgentCardTTL:          p.AgentCardTTL,
		}
		if err := reg.AddPeer(spec); err != nil {
			return nil, fmt.Errorf("a2a.New: peers[%d]: %w", i, err)
		}
	}
	return reg, nil
}

// Registry exposes the driver's route registry. Callers may inspect
// peers + update discovered capabilities post-construction.
func (t *transport) Registry() *Registry { return t.registry }

// closedErr returns ErrTransportClosed when Close has been called.
func (t *transport) closedErr() error {
	if t.closed.Load() {
		return distributed.ErrTransportClosed
	}
	return nil
}

// resolveEndpoint returns the JSON-RPC endpoint URL for the supplied
// peer URL. Performs Agent Card discovery on-demand, caches the card
// and the resolved endpoint.
//
// peerURL MUST already be a registered peer; resolveEndpoint enforces
// the allowlist and rejects unknown peers with ErrPeerNotAllowed.
func (t *transport) resolveEndpoint(ctx context.Context, peerURL string) (string, *a2a.AgentCard, error) {
	if peerURL == "" {
		// Pick the first registered peer; useful for tests + for
		// "default peer" calls (ListTasks, GetExtendedAgentCard).
		urls := t.registry.Peers()
		if len(urls) == 0 {
			return "", nil, fmt.Errorf("%w: no peers registered", ErrPeerNotAllowed)
		}
		peerURL = urls[0]
	}
	canonical := strings.TrimRight(peerURL, "/")
	spec, ok := t.registry.PeerSpec(canonical)
	if !ok {
		return "", nil, fmt.Errorf("%w: %q", ErrPeerNotAllowed, canonical)
	}
	if _, err := validatePeerURL(canonical, spec.AllowInsecureLoopback); err != nil {
		return "", nil, err
	}
	ttl := spec.AgentCardTTL
	if ttl <= 0 {
		ttl = t.agentCardTTL
	}
	card, err := t.cardCache.Fetch(ctx, canonical, ttl)
	if err != nil {
		return "", nil, err
	}
	iface := firstJSONRPCInterface(card)
	if iface == nil {
		return "", nil, fmt.Errorf("%w: peer=%q", ErrNoJSONRPCInterface, canonical)
	}
	endpoint := iface.URL
	if endpoint == "" {
		// Fall back to peer base URL when the AgentInterface URL is
		// empty (a relaxed shape the spec doesn't forbid).
		endpoint = canonical
	}
	return endpoint, card, nil
}

// -----------------------------------------------------------------------------
// RemoteTransport implementation
// -----------------------------------------------------------------------------

func (t *transport) Send(ctx context.Context, req distributed.RemoteCallRequest) (distributed.RemoteCallResult, error) {
	if err := t.closedErr(); err != nil {
		return distributed.RemoteCallResult{}, err
	}
	endpoint, _, err := t.resolveEndpoint(ctx, req.AgentURL)
	if err != nil {
		return distributed.RemoteCallResult{}, err
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	params := buildSendMessageParams(req)
	raw, err := t.jsonRPC.Call(ctx, endpoint, MethodSendMessage, params)
	if err != nil {
		return distributed.RemoteCallResult{}, err
	}
	// SendMessage returns the SendMessageResponse oneof (Task or
	// Message). For the unary contract, we surface the Task (when
	// present); a Message-only reply means the peer responded
	// without instantiating a Task — surface an empty Task with
	// the Message attached to History so the caller sees the reply.
	var resp a2a.SendMessageResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return distributed.RemoteCallResult{}, fmt.Errorf("a2a.Send: parse SendMessageResponse: %w", err)
	}
	if resp.Task != nil {
		return distributed.RemoteCallResult{Task: *resp.Task}, nil
	}
	if resp.Message != nil {
		return distributed.RemoteCallResult{
			Task: a2a.Task{
				ContextID: resp.Message.ContextID,
				Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
				History:   []a2a.Message{*resp.Message},
			},
		}, nil
	}
	return distributed.RemoteCallResult{}, fmt.Errorf("a2a.Send: empty SendMessageResponse oneof")
}

func (t *transport) Stream(ctx context.Context, req distributed.RemoteCallRequest) (distributed.RemoteEventStream, error) {
	if err := t.closedErr(); err != nil {
		return nil, err
	}
	endpoint, _, err := t.resolveEndpoint(ctx, req.AgentURL)
	if err != nil {
		return nil, err
	}
	var method string
	var params any
	switch req.Kind {
	case distributed.RemoteCallKindSubscribe:
		method = MethodSubscribeToTask
		params = a2a.SubscribeToTaskRequest{ID: req.TaskID}
		if id := tenantFromCtx(ctx); id != "" {
			params = a2a.SubscribeToTaskRequest{Tenant: id, ID: req.TaskID}
		}
	case "", distributed.RemoteCallKindSend, distributed.RemoteCallKindStream:
		method = MethodSendStreamingMessage
		params = buildSendMessageParams(req)
	default:
		return nil, fmt.Errorf("a2a.Stream: unknown kind %q", req.Kind)
	}
	stream, err := openSSEStream(ctx, t.httpc, endpoint, method, params)
	if err != nil {
		return nil, err
	}
	t.streams.Add(1)
	return &trackedStream{inner: stream, wg: &t.streams}, nil
}

func (t *transport) GetTask(ctx context.Context, taskID, contextID string) (*distributed.RemoteTaskSnapshot, error) {
	if err := t.closedErr(); err != nil {
		return nil, err
	}
	endpoint, _, err := t.resolveEndpoint(ctx, "")
	if err != nil {
		return nil, err
	}
	params := a2a.GetTaskRequest{
		Tenant: tenantFromCtx(ctx),
		ID:     taskID,
	}
	raw, err := t.jsonRPC.Call(ctx, endpoint, MethodGetTask, params)
	if err != nil {
		return nil, err
	}
	var task a2a.Task
	if err := json.Unmarshal(raw, &task); err != nil {
		return nil, fmt.Errorf("a2a.GetTask: parse Task: %w", err)
	}
	if task.ContextID == "" {
		task.ContextID = contextID
	}
	snap := distributed.RemoteTaskSnapshot(task)
	return &snap, nil
}

func (t *transport) ListTasks(ctx context.Context, filter distributed.RemoteTaskFilter) ([]distributed.RemoteTaskSnapshot, error) {
	if err := t.closedErr(); err != nil {
		return nil, err
	}
	endpoint, _, err := t.resolveEndpoint(ctx, "")
	if err != nil {
		return nil, err
	}
	tenant := filter.Tenant
	if tenant == "" {
		tenant = tenantFromCtx(ctx)
	}
	req := a2a.ListTasksRequest{
		Tenant:    tenant,
		ContextID: filter.ContextID,
		Status:    filter.Status,
		PageToken: filter.PageToken,
	}
	if filter.PageSize > 0 {
		ps := filter.PageSize
		req.PageSize = &ps
	}
	if filter.HistoryLength > 0 {
		hl := filter.HistoryLength
		req.HistoryLength = &hl
	}
	if !filter.StatusTimestampAfter.IsZero() {
		req.StatusTimestampAfter = filter.StatusTimestampAfter
	}
	if filter.IncludeArtifacts {
		v := true
		req.IncludeArtifacts = &v
	}
	raw, err := t.jsonRPC.Call(ctx, endpoint, MethodListTasks, req)
	if err != nil {
		return nil, err
	}
	var resp a2a.ListTasksResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("a2a.ListTasks: parse ListTasksResponse: %w", err)
	}
	out := make([]distributed.RemoteTaskSnapshot, len(resp.Tasks))
	for i := range resp.Tasks {
		out[i] = distributed.RemoteTaskSnapshot(resp.Tasks[i])
	}
	return out, nil
}

func (t *transport) Cancel(ctx context.Context, taskID, _ string) error {
	if err := t.closedErr(); err != nil {
		return err
	}
	endpoint, _, err := t.resolveEndpoint(ctx, "")
	if err != nil {
		return err
	}
	params := a2a.CancelTaskRequest{
		Tenant: tenantFromCtx(ctx),
		ID:     taskID,
	}
	if _, err := t.jsonRPC.Call(ctx, endpoint, MethodCancelTask, params); err != nil {
		return err
	}
	return nil
}

func (t *transport) Subscribe(ctx context.Context, taskID, _ string) (distributed.RemoteTaskEventStream, error) {
	if err := t.closedErr(); err != nil {
		return nil, err
	}
	endpoint, _, err := t.resolveEndpoint(ctx, "")
	if err != nil {
		return nil, err
	}
	params := a2a.SubscribeToTaskRequest{
		Tenant: tenantFromCtx(ctx),
		ID:     taskID,
	}
	stream, err := openSSEStream(ctx, t.httpc, endpoint, MethodSubscribeToTask, params)
	if err != nil {
		return nil, err
	}
	t.streams.Add(1)
	return &trackedStream{inner: stream, wg: &t.streams}, nil
}

func (t *transport) CreateTaskPushNotificationConfig(ctx context.Context, cfg a2a.TaskPushNotificationConfig) (a2a.TaskPushNotificationConfig, error) {
	if err := t.closedErr(); err != nil {
		return a2a.TaskPushNotificationConfig{}, err
	}
	endpoint, _, err := t.resolveEndpoint(ctx, "")
	if err != nil {
		return a2a.TaskPushNotificationConfig{}, err
	}
	if cfg.Tenant == "" {
		cfg.Tenant = tenantFromCtx(ctx)
	}
	raw, err := t.jsonRPC.Call(ctx, endpoint, MethodCreateTaskPushNotificationConfig, cfg)
	if err != nil {
		return a2a.TaskPushNotificationConfig{}, err
	}
	var out a2a.TaskPushNotificationConfig
	if err := json.Unmarshal(raw, &out); err != nil {
		return a2a.TaskPushNotificationConfig{}, fmt.Errorf("a2a.CreatePushConfig: parse: %w", err)
	}
	return out, nil
}

func (t *transport) GetTaskPushNotificationConfig(ctx context.Context, taskID, configID string) (a2a.TaskPushNotificationConfig, error) {
	if err := t.closedErr(); err != nil {
		return a2a.TaskPushNotificationConfig{}, err
	}
	endpoint, _, err := t.resolveEndpoint(ctx, "")
	if err != nil {
		return a2a.TaskPushNotificationConfig{}, err
	}
	params := a2a.GetTaskPushNotificationConfigRequest{
		Tenant: tenantFromCtx(ctx),
		TaskID: taskID,
		ID:     configID,
	}
	raw, err := t.jsonRPC.Call(ctx, endpoint, MethodGetTaskPushNotificationConfig, params)
	if err != nil {
		return a2a.TaskPushNotificationConfig{}, err
	}
	var out a2a.TaskPushNotificationConfig
	if err := json.Unmarshal(raw, &out); err != nil {
		return a2a.TaskPushNotificationConfig{}, fmt.Errorf("a2a.GetPushConfig: parse: %w", err)
	}
	return out, nil
}

func (t *transport) ListTaskPushNotificationConfigs(ctx context.Context, taskID string) ([]a2a.TaskPushNotificationConfig, error) {
	if err := t.closedErr(); err != nil {
		return nil, err
	}
	endpoint, _, err := t.resolveEndpoint(ctx, "")
	if err != nil {
		return nil, err
	}
	params := a2a.ListTaskPushNotificationConfigsRequest{
		Tenant: tenantFromCtx(ctx),
		TaskID: taskID,
	}
	raw, err := t.jsonRPC.Call(ctx, endpoint, MethodListTaskPushNotificationConfigs, params)
	if err != nil {
		return nil, err
	}
	var resp a2a.ListTaskPushNotificationConfigsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("a2a.ListPushConfigs: parse: %w", err)
	}
	return resp.Configs, nil
}

func (t *transport) DeleteTaskPushNotificationConfig(ctx context.Context, taskID, configID string) error {
	if err := t.closedErr(); err != nil {
		return err
	}
	endpoint, _, err := t.resolveEndpoint(ctx, "")
	if err != nil {
		return err
	}
	params := a2a.DeleteTaskPushNotificationConfigRequest{
		Tenant: tenantFromCtx(ctx),
		TaskID: taskID,
		ID:     configID,
	}
	if _, err := t.jsonRPC.Call(ctx, endpoint, MethodDeleteTaskPushNotificationConfig, params); err != nil {
		return err
	}
	return nil
}

func (t *transport) GetExtendedAgentCard(ctx context.Context) (*a2a.AgentCard, error) {
	if err := t.closedErr(); err != nil {
		return nil, err
	}
	// Default to the first registered peer when callers don't pass
	// an explicit AgentURL. resolveEndpoint already fetches the
	// AgentCard via discovery; we issue the JSON-RPC method too so
	// peers that distinguish "public card" from "extended
	// authenticated card" can return more.
	endpoint, card, err := t.resolveEndpoint(ctx, "")
	if err != nil {
		return nil, err
	}
	params := a2a.GetExtendedAgentCardRequest{Tenant: tenantFromCtx(ctx)}
	raw, err := t.jsonRPC.Call(ctx, endpoint, MethodGetExtendedAgentCard, params)
	if err != nil {
		// If the peer doesn't implement the extended card RPC, fall
		// back to the discovered card so callers always see
		// something. This is V1-friendly: many peers ship only the
		// .well-known card.
		var e *jsonRPCError
		if errors.As(err, &e) && e.Code == jsonRPCErrMethodNotFound {
			return card, nil
		}
		return nil, err
	}
	var extended a2a.AgentCard
	if err := json.Unmarshal(raw, &extended); err != nil {
		return nil, fmt.Errorf("a2a.GetExtendedAgentCard: parse: %w", err)
	}
	return &extended, nil
}

func (t *transport) Close(_ context.Context) error {
	if !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	// Wait for any in-flight streams to drain.
	t.streams.Wait()
	return nil
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// trackedStream wraps an inner stream with a WaitGroup so Close on the
// transport can wait for outstanding SSE consumers to finish.
type trackedStream struct {
	inner  distributed.RemoteEventStream
	wg     *sync.WaitGroup
	closed atomic.Bool
}

func (s *trackedStream) Recv(ctx context.Context) (a2a.StreamResponse, error) {
	return s.inner.Recv(ctx)
}

func (s *trackedStream) Close() error {
	err := s.inner.Close()
	if s.closed.CompareAndSwap(false, true) {
		s.wg.Done()
	}
	return err
}

// buildSendMessageParams produces the SendMessageRequest envelope from
// the RemoteCallRequest. ContextID/TaskID on the request override the
// message-level fields when set.
func buildSendMessageParams(req distributed.RemoteCallRequest) a2a.SendMessageRequest {
	msg := req.Message
	if req.ContextID != "" {
		msg.ContextID = req.ContextID
	}
	if req.TaskID != "" {
		msg.TaskID = req.TaskID
	}
	out := a2a.SendMessageRequest{
		Message: msg,
	}
	// SendMessageConfiguration is the proto's named oneof for the
	// per-send config. Surface it on the wire only when at least one
	// field is non-zero so peers that don't expect it aren't
	// confused by an empty object.
	if !isZeroSendConfig(req.Config) {
		cfg := req.Config
		out.Configuration = &cfg
	}
	return out
}

func isZeroSendConfig(c a2a.SendMessageConfiguration) bool {
	return len(c.AcceptedOutputModes) == 0 &&
		c.TaskPushNotificationConfig == nil &&
		c.HistoryLength == nil &&
		!c.ReturnImmediately
}

// tenantFromCtx returns the tenant ID for ctx-resident identity, or "".
func tenantFromCtx(ctx context.Context) string {
	id, ok := identity.From(ctx)
	if !ok {
		return ""
	}
	return id.TenantID
}

// Compile-time guards.
var (
	_ distributed.RemoteTransport = (*transport)(nil)
)
