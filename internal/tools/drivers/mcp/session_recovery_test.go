package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/tools"
)

// recoveryServer drives the official SDK handler over real HTTP. Replacing the
// handler forgets server-side session IDs just as a restarted MCP server does.
type recoveryServer struct {
	sdkServer         *mcpsdk.Server
	mu                sync.Mutex
	inner             http.Handler
	counts            map[string]int
	rejectMethod      string
	rejectStatus      int
	rejectRemaining   int
	credential        string
	initializeGate    <-chan struct{}
	initializeEntered chan<- struct{}
}

func (s *recoveryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var method string
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		var request struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "decode request", http.StatusBadRequest)
			return
		}
		method = request.Method
	}
	s.mu.Lock()
	if method != "" {
		s.counts[method]++
	}
	inner := s.inner
	reject := method != "" && method == s.rejectMethod && s.rejectRemaining != 0
	status := s.rejectStatus
	if reject && s.rejectRemaining > 0 {
		s.rejectRemaining--
	}
	credential := s.credential
	gate, entered := s.initializeGate, s.initializeEntered
	s.mu.Unlock()
	if method == "initialize" && gate != nil {
		if entered != nil {
			entered <- struct{}{}
		}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	if credential != "" && r.Header.Get("X-Receiver-Api-Key") != credential {
		http.Error(w, "unexpected credential", http.StatusUnauthorized)
		return
	}
	if reject {
		http.Error(w, "refused", status)
		return
	}
	inner.ServeHTTP(w, r)
}

func (s *recoveryServer) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[method]
}
func (s *recoveryServer) refuse(method string, status, remaining int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectMethod, s.rejectStatus, s.rejectRemaining = method, status, remaining
}

func newRecoveryProvider(t *testing.T) (*Provider, *recoveryServer, context.Context) {
	t.Helper()
	mock := newMockServer()
	newHandler := func() http.Handler {
		return mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return mock.server }, &mcpsdk.StreamableHTTPOptions{JSONResponse: true})
	}
	front := &recoveryServer{sdkServer: mock.server, inner: newHandler(), counts: make(map[string]int), credential: "cred-tenant-user"}
	server := httptest.NewServer(front)
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		for session := range mock.server.Sessions() {
			_ = session.Close()
		}
	})
	credential := &preparationCredentialProvider{}
	credential.allowedHosts = []string{server.Listener.Addr().String()}
	p, err := New(Config{Name: "receiver", URL: server.URL, TransportMode: TransportStreamableHTTP, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), DefaultPolicy: tools.DefaultPolicy(),
		OwnedInjectionProvider: credential, Injection: &CredentialInjection{Provider: credential, AuthenticatePreparation: true, Form: InjectionFormHeader, Header: "X-Receiver-Api-Key"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(testIdentityCtx(t, defaultIdentity()), 15*time.Second)
	t.Cleanup(cancel)
	// Match this fixture's credential to the helper's default identity.
	front.credential = "cred-" + defaultIdentity().TenantID + "-" + defaultIdentity().UserID
	if err := p.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	return p, front, ctx
}

func assertDownstreamRefusal(t *testing.T, err error, status int) {
	t.Helper()
	var refused *DownstreamUnauthorizedError
	if !errors.Is(err, ErrDownstreamUnauthorized) || !errors.As(err, &refused) || refused.StatusCode != status {
		t.Fatalf("error = %v, want typed HTTP %d refusal", err, status)
	}
}

func TestReceiverSession_RefreshRefusalPreservesSession(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			p, server, ctx := newRecoveryProvider(t)
			registry := NewRegistry()
			if err := registry.Register(ctx, ServerRegistration{Provider: p, Transport: string(TransportStreamableHTTP)}); err != nil {
				t.Fatal(err)
			}
			original, _ := p.sessionForRead()
			server.refuse("tools/list", status, 1)
			_, err := registry.RefreshDiscovery(ctx, "receiver")
			assertDownstreamRefusal(t, err, status)
			before := server.count("tools/list")
			if _, err := registry.RefreshDiscovery(ctx, "receiver"); err != nil {
				t.Fatalf("second refresh: %v", err)
			}
			if server.count("tools/list") != before+1 {
				t.Fatal("second refresh did not issue a new HTTP request")
			}
			current, _ := p.sessionForRead()
			if current != original || server.count("initialize") != 1 {
				t.Fatal("authorization refusal replaced the session")
			}
		})
	}
}

func TestReceiverSession_InvokeRefusalPreservesSession(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			p, server, ctx := newRecoveryProvider(t)
			descriptors, err := p.Discover(ctx)
			if err != nil {
				t.Fatal(err)
			}
			echo := findByName(descriptors, "receiver_echo")
			if echo == nil {
				t.Fatal("echo missing")
			}
			server.refuse("tools/call", status, 1)
			_, err = echo.Invoke(ctx, json.RawMessage(`{"text":"hello"}`))
			assertDownstreamRefusal(t, err, status)
			if server.count("tools/call") != 1 {
				t.Fatal("authorization refusal was transparently retried")
			}
			if _, err := echo.Invoke(ctx, json.RawMessage(`{"text":"hello"}`)); err != nil {
				t.Fatalf("second invocation: %v", err)
			}
			if server.count("tools/call") != 2 || server.count("initialize") != 1 {
				t.Fatal("second invocation did not reuse the live session over HTTP")
			}
		})
	}
}

func TestMCPSession_RestartRecoversEverySurface(t *testing.T) {
	for _, surface := range []string{"discovery", "tool", "resource", "resource descriptor", "prompt", "subscribe"} {
		t.Run(surface, func(t *testing.T) {
			p, server, ctx := newRecoveryProvider(t)
			descriptors, err := p.Discover(ctx)
			if err != nil {
				t.Fatal(err)
			}
			mock := newMockServer()
			t.Cleanup(func() {
				for session := range mock.server.Sessions() {
					_ = session.Close()
				}
			})
			server.mu.Lock()
			server.inner = mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return mock.server }, &mcpsdk.StreamableHTTPOptions{JSONResponse: true})
			server.mu.Unlock()
			switch surface {
			case "discovery":
				_, err = p.Discover(ctx)
			case "tool":
				_, err = findByName(descriptors, "receiver_echo").Invoke(ctx, json.RawMessage(`{"text":"hello"}`))
			case "resource":
				_, err = p.ReadResourceWithMetadata(ctx, "mem://hello")
			case "resource descriptor":
				_, err = findByName(descriptors, "receiver__resource.mem://hello").Invoke(ctx, json.RawMessage(`{}`))
			case "prompt":
				_, err = findByName(descriptors, "receiver__prompt.greet").Invoke(ctx, json.RawMessage(`{"name":"reader"}`))
			case "subscribe":
				err = p.SubscribeResource(ctx, "mem://hello")
			}
			if err != nil {
				t.Fatalf("%s after restart: %v", surface, err)
			}
			if server.count("initialize") != 2 {
				t.Fatalf("initialize = %d, want one reconnect", server.count("initialize"))
			}
		})
	}
}

func TestMCPSession_ClosedAndPermanentlyFailedRecover(t *testing.T) {
	for _, closed := range []bool{true, false} {
		t.Run(map[bool]string{true: "closed", false: "permanent HTTP failure"}[closed], func(t *testing.T) {
			p, server, ctx := newRecoveryProvider(t)
			if closed {
				session, _ := p.sessionForRead()
				if err := session.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				server.refuse("tools/list", http.StatusBadRequest, 1)
				if _, err := p.Discover(ctx); err == nil {
					t.Fatal("permanent failure hidden")
				}
				if server.count("initialize") != 1 {
					t.Fatal("ambiguous initial failure replayed")
				}
			}
			if _, err := p.Discover(ctx); err != nil {
				t.Fatalf("recovery: %v", err)
			}
			if server.count("initialize") != 2 {
				t.Fatal("session did not reconnect exactly once")
			}
		})
	}
}

func TestMCPSession_ConcurrentRecoverySharesReconnect(t *testing.T) {
	p, server, ctx := newRecoveryProvider(t)
	session, _ := p.sessionForRead()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := p.Discover(ctx); err != nil {
				t.Errorf("concurrent discovery: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if server.count("initialize") != 2 {
		t.Fatalf("initialize = %d, want 2", server.count("initialize"))
	}
}

func TestMCPSession_RecoveryBoundedAndRetryableLater(t *testing.T) {
	p, server, ctx := newRecoveryProvider(t)
	server.refuse("tools/list", http.StatusNotFound, -1)
	if _, err := p.Discover(ctx); !errors.Is(err, mcpsdk.ErrSessionMissing) {
		t.Fatalf("error = %v, want missing session", err)
	}
	if server.count("initialize") != 2 || server.count("tools/list") != 2 {
		t.Fatalf("recovery exceeded one retry: initialize=%d list=%d", server.count("initialize"), server.count("tools/list"))
	}
	server.refuse("initialize", http.StatusServiceUnavailable, -1)
	if _, err := p.Discover(ctx); err == nil {
		t.Fatal("failed reconnect hidden")
	}
	server.refuse("", 0, 0)
	if _, err := p.Discover(ctx); err != nil {
		t.Fatalf("later recovery: %v", err)
	}
}

func TestMCPSession_RepeatedRecoveryJoinsClientGoroutines(t *testing.T) {
	p, server, ctx := newRecoveryProvider(t)
	baseline := runtime.NumGoroutine()
	for range 12 {
		session, _ := p.sessionForRead()
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		// Signed receiver teardown deliberately omits a network DELETE. Close
		// the fixture server handles separately to isolate client leak accounting.
		for remote := range server.sdkServer.Sessions() {
			_ = remote.Close()
		}
		if _, err := p.Discover(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if server.count("initialize") != 13 {
		t.Fatal("unexpected reconnect count")
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+12 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+12 {
		t.Fatalf("goroutines grew across recovery: before=%d after=%d", baseline, got)
	}
}

func TestMCPSession_ConcurrentFailedRecoverySharesAttempt(t *testing.T) {
	p, server, ctx := newRecoveryProvider(t)
	original, _ := p.sessionForRead()
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	server.refuse("initialize", http.StatusServiceUnavailable, -1)
	const callers = 100
	ready := make(chan struct{}, callers)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := callSession(ctx, p, func(session *mcpsdk.ClientSession) (*mcpsdk.ListToolsResult, error) {
				if session == original {
					ready <- struct{}{}
					<-release
				}
				return session.ListTools(ctx, nil)
			})
			if err == nil {
				t.Error("failed initialize hidden")
			}
		}()
	}
	for range callers {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("callers did not reach failed session")
		}
	}
	close(release)
	wg.Wait()
	if got := server.count("initialize"); got != 2 {
		t.Fatalf("initialize = %d, want one shared failed reconnect", got)
	}
	server.refuse("", 0, 0)
	if _, err := p.Discover(ctx); err != nil {
		t.Fatalf("later independent recovery: %v", err)
	}
	if got := server.count("initialize"); got != 3 {
		t.Fatalf("later initialize = %d, want 3", got)
	}
}

type refusalBody struct {
	*bytes.Reader
	closed bool
}

func (b *refusalBody) Close() error { b.closed = true; return nil }

type refusalRoundTripper struct {
	body   *refusalBody
	status int
}

func (r refusalRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: r.status, Header: make(http.Header), Body: r.body, Request: req}, nil
}

func TestReceiverSession_RefusalDrainsClosesOnlySignedPreparation(t *testing.T) {
	for _, signed := range []bool{false, true} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			t.Run(http.StatusText(status)+map[bool]string{false: " ordinary", true: " signed"}[signed], func(t *testing.T) {
				body := &refusalBody{Reader: bytes.NewReader([]byte("sensitive downstream response"))}
				transport := &injectedHeaderTransport{base: refusalRoundTripper{body: body, status: status}}
				if signed {
					transport.preparationInjection = &CredentialInjection{Provider: &preparationCredentialProvider{}, Form: InjectionFormHeader, Header: "X-Receiver-Api-Key"}
				}
				req, err := http.NewRequestWithContext(testIdentityCtx(t, defaultIdentity()), http.MethodPost, "https://receiver.test/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
				if err != nil {
					t.Fatal(err)
				}
				resp, err := transport.RoundTrip(req)
				if signed {
					assertDownstreamRefusal(t, err, status)
					if resp != nil || !body.closed || body.Len() != 0 {
						t.Fatal("refusal body was not drained, closed, and discarded")
					}
				} else if err != nil || resp == nil || body.closed || body.Len() == 0 {
					t.Fatal("ordinary binding response changed")
				}
			})
		}
	}
}

func stallInitialize(server *recoveryServer) (<-chan struct{}, func()) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	server.mu.Lock()
	server.initializeGate, server.initializeEntered = release, entered
	server.mu.Unlock()
	var once sync.Once
	return entered, func() { once.Do(func() { close(release) }) }
}

func TestMCPSession_StalledInitializeWaiterCancelsIndependently(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(map[bool]string{false: "reconnect", true: "initial connect"}[initial], func(t *testing.T) {
			p, server, ctx := newRecoveryProvider(t)
			if initial {
				fresh, err := New(p.cfg)
				if err != nil {
					t.Fatal(err)
				}
				p = fresh
				t.Cleanup(func() { _ = fresh.Close(context.Background()) })
			} else {
				session, _ := p.sessionForRead()
				if err := session.Close(); err != nil {
					t.Fatal(err)
				}
			}
			entered, release := stallInitialize(server)
			defer release()
			leader := make(chan error, 1)
			go func() {
				if initial {
					leader <- p.Connect(ctx)
				} else {
					_, err := p.Discover(ctx)
					leader <- err
				}
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("initialize did not stall")
			}
			waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
			waiter := make(chan error, 1)
			go func() {
				if initial {
					waiter <- p.Connect(waitCtx)
				} else {
					_, err := p.Discover(waitCtx)
					waiter <- err
				}
			}()
			select {
			case err := <-waiter:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("waiter = %v, want own deadline", err)
				}
			case <-time.After(time.Second):
				t.Fatal("waiter blocked behind another caller's initialize")
			}
			release()
			select {
			case err := <-leader:
				if err != nil {
					t.Fatalf("leader after waiter cancellation: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("leader did not complete after release")
			}
			if server.count("initialize") != 2 {
				t.Fatalf("initialize = %d, want 2", server.count("initialize"))
			}
		})
	}
}

func TestMCPSession_TeardownCancelsStalledReconnect(t *testing.T) {
	p, server, ctx := newRecoveryProvider(t)
	failed, _ := p.sessionForRead()
	if err := failed.Close(); err != nil {
		t.Fatal(err)
	}
	entered, release := stallInitialize(server)
	defer release()
	leader := make(chan error, 1)
	go func() { _, err := p.Discover(ctx); leader <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("initialize did not stall")
	}
	closeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := p.Close(closeCtx); err != nil {
		t.Fatalf("close during initialize: %v", err)
	}
	select {
	case err := <-leader:
		if !errors.Is(err, ErrNotConnected) {
			t.Fatalf("leader = %v, want closed provider", err)
		}
	case <-time.After(time.Second):
		t.Fatal("initialize survived teardown")
	}
	p.mu.RLock()
	closed, session, flight := p.closed, p.session, p.connectionFlight
	p.mu.RUnlock()
	if !closed || session != nil || flight != nil {
		t.Fatal("teardown left a published or connecting session")
	}
}
