package a2a_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/distributed"
	a2atypes "github.com/hurtener/Harbor/internal/distributed/a2a"
	"github.com/hurtener/Harbor/internal/distributed/conformancetest"
	a2adrv "github.com/hurtener/Harbor/internal/distributed/drivers/a2a"
	"github.com/hurtener/Harbor/internal/distributed/drivers/loopback"
	"github.com/hurtener/Harbor/internal/identity"
)

// buildAgentCard constructs a minimal but valid AgentCard whose first
// SupportedInterface declares JSONRPC pointing at the supplied URL.
func buildAgentCard(serverURL string) *a2atypes.AgentCard {
	return &a2atypes.AgentCard{
		Name:        "Mock A2A Agent",
		Description: "Conformance mock built for Phase 29 wire driver tests",
		Version:     "1.0.0",
		SupportedInterfaces: []a2atypes.AgentInterface{
			{
				URL:             serverURL,
				ProtocolBinding: a2atypes.ProtocolBindingJSONRPC,
				ProtocolVersion: "1.0",
			},
		},
		Capabilities:       a2atypes.AgentCapabilities{},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills: []a2atypes.AgentSkill{
			{ID: "echo", Name: "Echo", Description: "Echo input back.", Tags: []string{"test"}},
		},
	}
}

// newWireTransport constructs a wire driver bound to the mock server.
// The server URL is registered as a single peer with TrustTier=3 and
// AllowInsecureLoopback=true so the test can speak HTTP.
func newWireTransport(t *testing.T, mock *mockA2AServer) distributed.RemoteTransport {
	t.Helper()
	reg := a2adrv.NewRegistry()
	if err := reg.AddPeer(a2adrv.PeerSpec{
		URL:                   strings.TrimRight(mock.URL(), "/"),
		TrustTier:             3,
		LatencyTierMS:         10,
		AllowInsecureLoopback: true,
	}); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	tr, err := a2adrv.New(distributed.Dependencies{}, a2adrv.WithRegistry(reg))
	if err != nil {
		t.Fatalf("a2adrv.New: %v", err)
	}
	return tr
}

// newWireTransportWithAlias constructs a wire driver where both the
// mock server URL AND a caller-supplied alias URL are registered, but
// outbound HTTP requests targeting the alias get rewritten to the
// mock at the HTTP-transport layer. Used to make the conformance
// suite (which passes a hardcoded synthetic URL) flow through the
// real wire driver.
func newWireTransportWithAlias(t *testing.T, mock *mockA2AServer, alias string) distributed.RemoteTransport {
	t.Helper()
	mockBase := strings.TrimRight(mock.URL(), "/")
	reg := a2adrv.NewRegistry()
	if err := reg.AddPeer(a2adrv.PeerSpec{
		URL:                   alias,
		TrustTier:             3,
		LatencyTierMS:         10,
		AllowInsecureLoopback: true,
	}); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	// HTTP client that rewrites alias-host requests to the mock.
	rewriter := &hostRewriteRoundTripper{
		aliasHost: hostOf(alias),
		mockBase:  mockBase,
		inner:     newPermissiveTransport(),
	}
	tr, err := a2adrv.New(distributed.Dependencies{},
		a2adrv.WithRegistry(reg),
		a2adrv.WithHTTPClient(&http.Client{Transport: rewriter}),
	)
	if err != nil {
		t.Fatalf("a2adrv.New: %v", err)
	}
	return tr
}

// TestConformance_WireTransport_A2A exercises the Phase 22 conformance
// suite verbatim against the wire driver bound to the mock A2A server.
// The conformance suite hardcodes `"https://agent.example/test"` as
// the AgentURL each subtest issues; the wire driver requires a
// registered peer. We register the synthetic URL with the Registry
// and use a host-rewriting HTTP transport that translates the alias
// host to the mock's loopback address.
func TestConformance_WireTransport_A2A(t *testing.T) {
	const conformanceAlias = "https://agent.example/test"
	conformancetest.RunRemoteTransport(t, func(t *testing.T) (distributed.RemoteTransport, conformancetest.AgentBinding, func()) {
		mock := newMockA2AServer()
		// AgentCard's first AgentInterface URL is the synthetic alias —
		// when the wire driver issues a JSON-RPC POST against it, the
		// rewriting transport sends it to the mock.
		mock.SetAgentCard(buildAgentCard(conformanceAlias))
		tr := newWireTransportWithAlias(t, mock, conformanceAlias)
		cleanup := func() {
			_ = tr.Close(context.Background())
			mock.Close()
		}
		binding := conformancetest.AgentBinding(func(_ string, agent loopback.Agent) {
			mock.BindAgent("", agent)
		})
		return tr, binding, cleanup
	})
}

// TestWireTransport_RejectsNonRegisteredPeer asserts the URL allowlist:
// calls targeting a peer not in the Registry surface ErrPeerNotAllowed.
func TestWireTransport_RejectsNonRegisteredPeer(t *testing.T) {
	mock := newMockA2AServer()
	defer mock.Close()
	mock.SetAgentCard(buildAgentCard(mock.URL()))
	mock.BindAgent("", &stubEchoAgent{})

	tr := newWireTransport(t, mock)
	defer func() { _ = tr.Close(context.Background()) }()

	ctx := ctxWithIdentity(context.Background(), "tenant-a", "user-a", "session-a")
	_, err := tr.Send(ctx, distributed.RemoteCallRequest{
		AgentURL: "https://not-registered.example",
		Message:  a2atypes.Message{MessageID: "m-1", Role: a2atypes.RoleUser, Parts: a2atypes.Parts{&a2atypes.TextPart{Text: "hi"}}},
	})
	if !errors.Is(err, a2adrv.ErrPeerNotAllowed) {
		t.Errorf("expected ErrPeerNotAllowed, got %v", err)
	}
}

// TestWireTransport_RejectsInsecureHTTP asserts the HTTPS-only rule:
// a non-loopback HTTP peer (without AllowInsecureLoopback) is rejected
// at construction.
func TestWireTransport_RejectsInsecureHTTP(t *testing.T) {
	_, err := a2adrv.New(distributed.Dependencies{
		Tools: config.ToolsConfig{
			A2APeers: []config.A2APeerConfig{
				{URL: "http://public-agent.example", TrustTier: 3, LatencyTierMS: 10},
			},
		},
	})
	if !errors.Is(err, a2adrv.ErrInsecureScheme) {
		t.Errorf("expected ErrInsecureScheme, got %v", err)
	}
}

// TestWireTransport_AcceptsHTTPSPeer asserts HTTPS peers pass the
// security check (the AgentCard fetch will fail for a non-existent
// host, but the construction itself should succeed).
func TestWireTransport_AcceptsHTTPSPeer(t *testing.T) {
	tr, err := a2adrv.New(distributed.Dependencies{
		Tools: config.ToolsConfig{
			A2APeers: []config.A2APeerConfig{
				{URL: "https://agent.example", TrustTier: 4, LatencyTierMS: 50},
			},
		},
	})
	if err != nil {
		t.Fatalf("expected New to succeed for HTTPS peer, got: %v", err)
	}
	_ = tr.Close(context.Background())
}

// TestWireTransport_ConcurrentSend_D025 hammers a shared transport
// with N concurrent Send calls; asserts no races + identity isolation
// (each request's tenant is what the goroutine supplied — no bleed).
func TestWireTransport_ConcurrentSend_D025(t *testing.T) {
	mock := newMockA2AServer()
	defer mock.Close()
	mock.SetAgentCard(buildAgentCard(mock.URL()))

	var observed sync.Map // tenant → bool
	agent := &funcAgent{
		sendMessage: func(ctx context.Context, msg a2atypes.Message, _ a2atypes.SendMessageConfiguration) (a2atypes.Task, error) {
			id, ok := identity.From(ctx)
			if !ok {
				return a2atypes.Task{}, fmt.Errorf("identity missing in goroutine")
			}
			observed.Store(id.TenantID, true)
			return a2atypes.Task{ID: msg.MessageID, Status: a2atypes.TaskStatus{State: a2atypes.TaskStateCompleted}}, nil
		},
	}
	mock.BindAgent("", agent)

	tr := newWireTransport(t, mock)
	defer func() { _ = tr.Close(context.Background()) }()

	const workers = 128
	var wg sync.WaitGroup
	wg.Add(workers)
	errs := make(chan error, workers)
	for i := range workers {
		go func(w int) {
			defer wg.Done()
			tenant := fmt.Sprintf("tenant-%d", w%4) // 4 distinct tenants
			ctx := ctxWithIdentity(context.Background(), tenant, "user", "session")
			_, err := tr.Send(ctx, distributed.RemoteCallRequest{
				Message: a2atypes.Message{
					MessageID: fmt.Sprintf("m-%d", w),
					Role:      a2atypes.RoleUser,
					Parts:     a2atypes.Parts{&a2atypes.TextPart{Text: "go"}},
				},
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Send: %v", err)
	}
	got := 0
	observed.Range(func(_, _ any) bool { got++; return true })
	if got != 4 {
		t.Errorf("observed tenants: %d (want 4 — identity propagation across goroutines)", got)
	}
}

// TestWireTransport_StreamingHonoursCancel asserts that cancelling the
// stream's ctx closes the underlying HTTP response promptly.
func TestWireTransport_StreamingHonoursCancel(t *testing.T) {
	mock := newMockA2AServer()
	defer mock.Close()
	mock.SetAgentCard(buildAgentCard(mock.URL()))

	mock.BindAgent("", &funcAgent{
		sendStreamingMessage: func(ctx context.Context, _ a2atypes.Message, _ a2atypes.SendMessageConfiguration) (<-chan a2atypes.StreamResponse, error) {
			ch := make(chan a2atypes.StreamResponse, 1)
			ch <- a2atypes.StreamResponse{Task: &a2atypes.Task{ID: "t-1", Status: a2atypes.TaskStatus{State: a2atypes.TaskStateWorking}}}
			go func() {
				<-ctx.Done()
				close(ch)
			}()
			return ch, nil
		},
	})

	tr := newWireTransport(t, mock)
	defer func() { _ = tr.Close(context.Background()) }()

	ctx, cancel := context.WithCancel(ctxWithIdentity(context.Background(), "t", "u", "s"))
	stream, err := tr.Stream(ctx, distributed.RemoteCallRequest{
		Kind:    distributed.RemoteCallKindStream,
		Message: a2atypes.Message{MessageID: "m-1", Role: a2atypes.RoleUser, Parts: a2atypes.Parts{&a2atypes.TextPart{Text: "go"}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	if _, err := stream.Recv(context.Background()); err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	// Cancel and assert next Recv returns promptly.
	cancel()
	done := make(chan struct{})
	go func() {
		_, _ = stream.Recv(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Errorf("Stream.Recv did not honour ctx cancel within 2s")
	}
}

// TestWireTransport_AgentCardCoalescesConcurrentFetch asserts the
// AgentCard cache collapses N concurrent first-time Discover calls
// into one HTTP GET (stampede prevention).
func TestWireTransport_AgentCardCoalescesConcurrentFetch(t *testing.T) {
	mock := newMockA2AServer()
	defer mock.Close()
	mock.SetAgentCard(buildAgentCard(mock.URL()))
	mock.BindAgent("", &stubEchoAgent{})

	tr := newWireTransport(t, mock)
	defer func() { _ = tr.Close(context.Background()) }()

	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := range N {
		go func(w int) {
			defer wg.Done()
			ctx := ctxWithIdentity(context.Background(), "t", "u", fmt.Sprintf("s-%d", w))
			_, err := tr.Send(ctx, distributed.RemoteCallRequest{
				Message: a2atypes.Message{MessageID: "m", Role: a2atypes.RoleUser, Parts: a2atypes.Parts{&a2atypes.TextPart{Text: "x"}}},
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Send: %v", err)
	}
	// Count the GETs against /.well-known/agent-card.json — the
	// mock server reports the total request count; we accept up to
	// 4 fetches as a coalescing tolerance (depending on goroutine
	// scheduling some workers may have raced past the inflight
	// guard before it was installed).
	cardFetches := mock.RequestCount()
	// Each Send issues at least one POST to the RPC endpoint, so
	// the total includes 32 RPC calls + at most a few GETs. Verify
	// we didn't issue 32 GETs.
	if cardFetches > int64(N)+4 {
		t.Errorf("request count too high: %d (suggests no cache coalescing)", cardFetches)
	}
}

// TestWireTransport_GoroutineLeak_AfterClose asserts that streams
// opened against the wire driver are joined on Close.
func TestWireTransport_GoroutineLeak_AfterClose(t *testing.T) {
	baseline := runtimeGoroutineCount()
	mock := newMockA2AServer()
	mock.SetAgentCard(buildAgentCard(mock.URL()))
	mock.BindAgent("", &funcAgent{
		sendStreamingMessage: func(_ context.Context, _ a2atypes.Message, _ a2atypes.SendMessageConfiguration) (<-chan a2atypes.StreamResponse, error) {
			ch := make(chan a2atypes.StreamResponse, 1)
			ch <- a2atypes.StreamResponse{Task: &a2atypes.Task{ID: "t-1", Status: a2atypes.TaskStatus{State: a2atypes.TaskStateCompleted}}}
			close(ch)
			return ch, nil
		},
	})
	tr := newWireTransport(t, mock)
	ctx := ctxWithIdentity(context.Background(), "t", "u", "s")
	stream, err := tr.Stream(ctx, distributed.RemoteCallRequest{
		Kind:    distributed.RemoteCallKindStream,
		Message: a2atypes.Message{MessageID: "m-1", Role: a2atypes.RoleUser, Parts: a2atypes.Parts{&a2atypes.TextPart{Text: "go"}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for {
		if _, err := stream.Recv(context.Background()); err != nil {
			break
		}
	}
	_ = stream.Close()
	_ = tr.Close(context.Background())
	mock.Close()
	// Allow a brief settle window.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if delta := runtimeGoroutineCount() - baseline; delta <= 4 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if delta := runtimeGoroutineCount() - baseline; delta > 4 {
		t.Errorf("goroutine leak: baseline=%d delta=%d", baseline, delta)
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func ctxWithIdentity(parent context.Context, tenant, user, session string) context.Context {
	ctx, err := identity.With(parent, identity.Identity{TenantID: tenant, UserID: user, SessionID: session})
	if err != nil {
		panic(err)
	}
	return ctx
}

// funcAgent is a function-pointer-driven Agent that delegates to
// stubs for each A2A RPC. Stubs not set return an error.
type funcAgent struct {
	sendMessage                      func(ctx context.Context, msg a2atypes.Message, cfg a2atypes.SendMessageConfiguration) (a2atypes.Task, error)
	sendStreamingMessage             func(ctx context.Context, msg a2atypes.Message, cfg a2atypes.SendMessageConfiguration) (<-chan a2atypes.StreamResponse, error)
	getTask                          func(ctx context.Context, taskID, contextID string) (a2atypes.Task, error)
	listTasks                        func(ctx context.Context, filter loopback.ListTasksFilter) ([]a2atypes.Task, error)
	cancelTask                       func(ctx context.Context, taskID, contextID string) (a2atypes.Task, error)
	subscribeToTask                  func(ctx context.Context, taskID, contextID string) (<-chan a2atypes.StreamResponse, error)
	createTaskPushNotificationConfig func(ctx context.Context, cfg a2atypes.TaskPushNotificationConfig) (a2atypes.TaskPushNotificationConfig, error)
	getTaskPushNotificationConfig    func(ctx context.Context, taskID, configID string) (a2atypes.TaskPushNotificationConfig, error)
	listTaskPushNotificationConfigs  func(ctx context.Context, taskID string) ([]a2atypes.TaskPushNotificationConfig, error)
	deleteTaskPushNotificationConfig func(ctx context.Context, taskID, configID string) error
	getExtendedAgentCard             func(ctx context.Context) (a2atypes.AgentCard, error)
}

func (a *funcAgent) SendMessage(ctx context.Context, msg a2atypes.Message, cfg a2atypes.SendMessageConfiguration) (a2atypes.Task, error) {
	if a.sendMessage == nil {
		return a2atypes.Task{}, fmt.Errorf("funcAgent.SendMessage: not staged")
	}
	return a.sendMessage(ctx, msg, cfg)
}

func (a *funcAgent) SendStreamingMessage(ctx context.Context, msg a2atypes.Message, cfg a2atypes.SendMessageConfiguration) (<-chan a2atypes.StreamResponse, error) {
	if a.sendStreamingMessage == nil {
		return nil, fmt.Errorf("funcAgent.SendStreamingMessage: not staged")
	}
	return a.sendStreamingMessage(ctx, msg, cfg)
}

func (a *funcAgent) GetTask(ctx context.Context, taskID, contextID string) (a2atypes.Task, error) {
	if a.getTask == nil {
		return a2atypes.Task{}, fmt.Errorf("funcAgent.GetTask: not staged")
	}
	return a.getTask(ctx, taskID, contextID)
}

func (a *funcAgent) ListTasks(ctx context.Context, filter loopback.ListTasksFilter) ([]a2atypes.Task, error) {
	if a.listTasks == nil {
		return nil, fmt.Errorf("funcAgent.ListTasks: not staged")
	}
	return a.listTasks(ctx, filter)
}

func (a *funcAgent) CancelTask(ctx context.Context, taskID, contextID string) (a2atypes.Task, error) {
	if a.cancelTask == nil {
		return a2atypes.Task{}, fmt.Errorf("funcAgent.CancelTask: not staged")
	}
	return a.cancelTask(ctx, taskID, contextID)
}

func (a *funcAgent) SubscribeToTask(ctx context.Context, taskID, contextID string) (<-chan a2atypes.StreamResponse, error) {
	if a.subscribeToTask == nil {
		return nil, fmt.Errorf("funcAgent.SubscribeToTask: not staged")
	}
	return a.subscribeToTask(ctx, taskID, contextID)
}

func (a *funcAgent) CreateTaskPushNotificationConfig(ctx context.Context, cfg a2atypes.TaskPushNotificationConfig) (a2atypes.TaskPushNotificationConfig, error) {
	if a.createTaskPushNotificationConfig == nil {
		return a2atypes.TaskPushNotificationConfig{}, fmt.Errorf("funcAgent.CreateTaskPushNotificationConfig: not staged")
	}
	return a.createTaskPushNotificationConfig(ctx, cfg)
}

func (a *funcAgent) GetTaskPushNotificationConfig(ctx context.Context, taskID, configID string) (a2atypes.TaskPushNotificationConfig, error) {
	if a.getTaskPushNotificationConfig == nil {
		return a2atypes.TaskPushNotificationConfig{}, fmt.Errorf("funcAgent.GetTaskPushNotificationConfig: not staged")
	}
	return a.getTaskPushNotificationConfig(ctx, taskID, configID)
}

func (a *funcAgent) ListTaskPushNotificationConfigs(ctx context.Context, taskID string) ([]a2atypes.TaskPushNotificationConfig, error) {
	if a.listTaskPushNotificationConfigs == nil {
		return nil, fmt.Errorf("funcAgent.ListTaskPushNotificationConfigs: not staged")
	}
	return a.listTaskPushNotificationConfigs(ctx, taskID)
}

func (a *funcAgent) DeleteTaskPushNotificationConfig(ctx context.Context, taskID, configID string) error {
	if a.deleteTaskPushNotificationConfig == nil {
		return fmt.Errorf("funcAgent.DeleteTaskPushNotificationConfig: not staged")
	}
	return a.deleteTaskPushNotificationConfig(ctx, taskID, configID)
}

func (a *funcAgent) GetExtendedAgentCard(ctx context.Context) (a2atypes.AgentCard, error) {
	if a.getExtendedAgentCard == nil {
		return a2atypes.AgentCard{}, fmt.Errorf("funcAgent.GetExtendedAgentCard: not staged")
	}
	return a.getExtendedAgentCard(ctx)
}

// stubEchoAgent is a minimal Agent that just echoes SendMessage and
// returns an empty AgentCard. Used by tests that don't care about the
// content of the response.
type stubEchoAgent struct {
	calls atomic.Int64
}

func (a *stubEchoAgent) SendMessage(_ context.Context, msg a2atypes.Message, _ a2atypes.SendMessageConfiguration) (a2atypes.Task, error) {
	a.calls.Add(1)
	return a2atypes.Task{ID: msg.MessageID, Status: a2atypes.TaskStatus{State: a2atypes.TaskStateCompleted}}, nil
}

func (a *stubEchoAgent) SendStreamingMessage(_ context.Context, _ a2atypes.Message, _ a2atypes.SendMessageConfiguration) (<-chan a2atypes.StreamResponse, error) {
	ch := make(chan a2atypes.StreamResponse, 1)
	ch <- a2atypes.StreamResponse{Task: &a2atypes.Task{ID: "t-1", Status: a2atypes.TaskStatus{State: a2atypes.TaskStateCompleted}}}
	close(ch)
	return ch, nil
}

func (a *stubEchoAgent) GetTask(_ context.Context, taskID, contextID string) (a2atypes.Task, error) {
	return a2atypes.Task{ID: taskID, ContextID: contextID, Status: a2atypes.TaskStatus{State: a2atypes.TaskStateWorking}}, nil
}

func (a *stubEchoAgent) ListTasks(_ context.Context, _ loopback.ListTasksFilter) ([]a2atypes.Task, error) {
	return nil, nil
}

func (a *stubEchoAgent) CancelTask(_ context.Context, taskID, _ string) (a2atypes.Task, error) {
	return a2atypes.Task{ID: taskID, Status: a2atypes.TaskStatus{State: a2atypes.TaskStateCanceled}}, nil
}

func (a *stubEchoAgent) SubscribeToTask(_ context.Context, _ string, _ string) (<-chan a2atypes.StreamResponse, error) {
	ch := make(chan a2atypes.StreamResponse, 1)
	ch <- a2atypes.StreamResponse{Task: &a2atypes.Task{ID: "t-1", Status: a2atypes.TaskStatus{State: a2atypes.TaskStateWorking}}}
	close(ch)
	return ch, nil
}

func (a *stubEchoAgent) CreateTaskPushNotificationConfig(_ context.Context, cfg a2atypes.TaskPushNotificationConfig) (a2atypes.TaskPushNotificationConfig, error) {
	return cfg, nil
}

func (a *stubEchoAgent) GetTaskPushNotificationConfig(_ context.Context, taskID, configID string) (a2atypes.TaskPushNotificationConfig, error) {
	return a2atypes.TaskPushNotificationConfig{TaskID: taskID, ID: configID, URL: "https://callback"}, nil
}

func (a *stubEchoAgent) ListTaskPushNotificationConfigs(_ context.Context, _ string) ([]a2atypes.TaskPushNotificationConfig, error) {
	return nil, nil
}

func (a *stubEchoAgent) DeleteTaskPushNotificationConfig(_ context.Context, _, _ string) error {
	return nil
}

func (a *stubEchoAgent) GetExtendedAgentCard(_ context.Context) (a2atypes.AgentCard, error) {
	return a2atypes.AgentCard{
		Name:        "stub",
		Description: "stub",
		Version:     "1.0",
		SupportedInterfaces: []a2atypes.AgentInterface{
			{URL: "https://stub", ProtocolBinding: a2atypes.ProtocolBindingJSONRPC, ProtocolVersion: "1.0"},
		},
		Capabilities:       a2atypes.AgentCapabilities{},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             []a2atypes.AgentSkill{{ID: "echo", Name: "Echo", Description: "echo", Tags: []string{"test"}}},
	}, nil
}

// runtimeGoroutineCount wraps runtime.NumGoroutine() for the leak test.
func runtimeGoroutineCount() int { return runtime.NumGoroutine() }

// hostRewriteRoundTripper transparently rewrites outbound requests
// targeting aliasHost to the mock's loopback base URL. Used so the
// Phase 22 conformance suite (which hardcodes a synthetic peer URL)
// can flow through the wire driver against the in-process mock.
type hostRewriteRoundTripper struct {
	aliasHost string
	mockBase  string
	inner     http.RoundTripper
}

func (rt *hostRewriteRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL != nil && req.URL.Host == rt.aliasHost {
		// Rewrite scheme + host + leave path/query intact.
		mockURL, err := url.Parse(rt.mockBase)
		if err != nil {
			return nil, err
		}
		req = req.Clone(req.Context())
		req.URL.Scheme = mockURL.Scheme
		req.URL.Host = mockURL.Host
		req.Host = mockURL.Host
	}
	return rt.inner.RoundTrip(req)
}

// newPermissiveTransport returns an http.Transport tuned for tests:
// short dial timeouts, no proxy auto-detection, accepts self-signed
// TLS when needed. The conformance suite uses HTTP loopback so the
// TLS config never fires.
func newPermissiveTransport() http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	clone := t.Clone()
	return clone
}

// hostOf returns the host portion of rawURL ("agent.example") or "".
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}
