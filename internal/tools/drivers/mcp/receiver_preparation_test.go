package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

type preparationCredentialProvider struct {
	identityCredProvider
	mu         sync.Mutex
	err        error
	value      string
	empty      bool
	identities []identity.Identity
}

func (p *preparationCredentialProvider) Token(ctx context.Context, source tools.ToolSourceID) (auth.Token, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id, ok := identity.From(ctx); ok {
		p.identities = append(p.identities, id)
	}
	if p.err != nil {
		return auth.Token{}, p.err
	}
	if p.empty {
		return auth.Token{}, nil
	}
	if p.value != "" {
		return auth.Token{AccessToken: p.value, BindingScope: auth.ScopeUser}, nil
	}
	return p.identityCredProvider.Token(ctx, source)
}

func TestReceiverPreparation_AuthenticatesInitializeDiscoveryAndRefresh(t *testing.T) {
	for _, form := range []InjectionForm{InjectionFormHeader, InjectionFormBasic, InjectionFormMeta} {
		t.Run(string(form), func(t *testing.T) {
			mock := newMockServer()
			handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return mock.server }, nil)
			var mu sync.Mutex
			methods := make(map[string]int)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					handler.ServeHTTP(w, r)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request: %v", err)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				var request struct {
					Method string `json:"method"`
					Params struct {
						Meta map[string]any `json:"_meta"`
					} `json:"params"`
				}
				if err := json.Unmarshal(body, &request); err != nil {
					t.Errorf("decode request: %v", err)
					return
				}
				valid := false
				switch form {
				case InjectionFormHeader:
					valid = r.Header.Get("X-Receiver-Api-Key") == "cred-tenant-user"
				case InjectionFormBasic:
					user, password, ok := r.BasicAuth()
					valid = ok && user == "service" && password == "cred-tenant-user"
				case InjectionFormMeta:
					nested, _ := request.Params.Meta["receiver"].(map[string]any)
					valid = nested["api_key"] == "cred-tenant-user"
				}
				if !valid {
					t.Errorf("unauthenticated %s (%s)", request.Method, form)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				mu.Lock()
				methods[request.Method]++
				mu.Unlock()
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			credential := &preparationCredentialProvider{}
			p, err := New(Config{Name: "receiver", URL: server.URL, TransportMode: TransportStreamableHTTP, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(),
				OwnedInjectionProvider: credential, Injection: &CredentialInjection{Provider: credential, AuthenticatePreparation: true, Form: form, Header: "X-Receiver-Api-Key", BasicUsername: "service", MetaKey: []string{"receiver", "api_key"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = p.Close(context.Background()) }()
			ctx, cancel := context.WithTimeout(testIdentityCtx(t, identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}), 10*time.Second)
			defer cancel()
			if err := p.Connect(ctx); err != nil {
				t.Fatalf("authenticated initialize: %v", err)
			}
			if _, err := p.Discover(ctx); err != nil {
				t.Fatalf("authenticated discovery: %v", err)
			}
			registry := NewRegistry()
			if err := registry.Register(ctx, ServerRegistration{Provider: p, Transport: string(TransportStreamableHTTP)}); err != nil {
				t.Fatal(err)
			}
			if _, err := registry.RefreshDiscovery(ctx, "receiver"); err != nil {
				t.Fatalf("authenticated refresh: %v", err)
			}
			mu.Lock()
			for _, method := range []string{"initialize", "tools/list", "resources/list", "prompts/list"} {
				want := 2
				if method == "initialize" {
					want = 1
				}
				if methods[method] < want {
					t.Errorf("%s requests = %d, want at least %d", method, methods[method], want)
				}
			}
			counts := 0
			for _, count := range methods {
				counts += count
			}
			mu.Unlock()
			credential.mu.Lock()
			credential.err = errors.New("credential unavailable")
			credential.mu.Unlock()
			if _, err := registry.RefreshDiscovery(ctx, "receiver"); err == nil {
				t.Fatal("failed credential pull allowed refresh")
			}
			mu.Lock()
			after := 0
			for _, count := range methods {
				after += count
			}
			mu.Unlock()
			if after != counts {
				t.Fatal("failed credential pull sent a request")
			}
			credential.mu.Lock()
			for _, id := range credential.identities {
				if id.TenantID != "tenant" || id.UserID != "user" || id.SessionID != "session" {
					t.Errorf("credential identity = %+v", id)
				}
			}
			credential.err = nil
			credential.mu.Unlock()
		})
	}
}

func TestReceiverPreparation_FailedOrEmptyPullSendsNothing(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "empty"}[empty], func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusUnauthorized) }))
			defer server.Close()
			credential := &preparationCredentialProvider{empty: empty}
			if !empty {
				credential.err = errors.New("credential unavailable")
			}
			p, err := New(Config{Name: "receiver", URL: server.URL, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), OwnedInjectionProvider: credential,
				Injection: &CredentialInjection{Provider: credential, AuthenticatePreparation: true, Form: InjectionFormHeader, Header: "X-Receiver-Api-Key"}})
			if err != nil {
				t.Fatal(err)
			}
			ctx := testIdentityCtx(t, identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"})
			if err := p.Connect(ctx); err == nil {
				t.Fatal("failed or empty credential allowed initialize")
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("sent %d requests before credential admission", got)
			}
		})
	}
}

func TestReceiverPreparation_FalsePreservesCredentialNeutralDiscovery(t *testing.T) {
	mock := newMockServer()
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return mock.server }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Receiver-Api-Key") != "" {
			t.Error("false preparation injected a credential")
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	credential := &preparationCredentialProvider{err: errors.New("must not pull during credential-neutral preparation")}
	p, err := New(Config{Name: "receiver", URL: server.URL, TransportMode: TransportStreamableHTTP, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), OwnedInjectionProvider: credential,
		Injection: &CredentialInjection{Provider: credential, Form: InjectionFormHeader, Header: "X-Receiver-Api-Key"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Discover(ctx); err != nil {
		t.Fatalf("false preparation pulled credentials: %v", err)
	}
}

func TestReceiverPreparation_TransportRetryReauthorizesInsteadOfReusingCredential(t *testing.T) {
	for _, form := range []InjectionForm{InjectionFormHeader, InjectionFormBasic, InjectionFormMeta} {
		t.Run(string(form), func(t *testing.T) {
			credential := &preparationCredentialProvider{}
			var requests atomic.Int64
			var expected atomic.Value
			expected.Store("cred-tenant-user")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				value := ""
				switch form {
				case InjectionFormHeader:
					value = r.Header.Get("X-Receiver-Api-Key")
				case InjectionFormBasic:
					_, value, _ = r.BasicAuth()
				case InjectionFormMeta:
					var message struct {
						Params struct {
							Meta map[string]any `json:"_meta"`
						} `json:"params"`
					}
					if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
						t.Error(err)
					}
					nested, _ := message.Params.Meta["receiver"].(map[string]any)
					value, _ = nested["api_key"].(string)
				}
				if value != expected.Load().(string) {
					t.Error("HTTP retry retained stale credential")
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			ctx := testIdentityCtx(t, identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"})
			client := buildHTTPClient(Config{Name: "receiver", Injection: &CredentialInjection{Provider: credential, AuthenticatePreparation: true, Form: form, Header: "X-Receiver-Api-Key", BasicUsername: "service", MetaKey: []string{"receiver", "api_key"}}})
			send := func() error {
				body := bytes.NewBufferString(`{"method":"initialize","params":{"_meta":{"receiver":{"api_key":"stale-value"}}}}`)
				request, err := http.NewRequestWithContext(withInjectedHeaders(ctx, map[string]string{"X-Receiver-Api-Key": "stale-value"}), http.MethodPost, server.URL, body)
				if err != nil {
					return err
				}
				response, err := client.Do(request)
				if err == nil {
					_ = response.Body.Close()
				}
				return err
			}
			if err := send(); err != nil {
				t.Fatal(err)
			}
			expected.Store("rotated-value")
			credential.mu.Lock()
			credential.value = "rotated-value"
			credential.mu.Unlock()
			if err := send(); err != nil {
				t.Fatalf("refreshed credential retry: %v", err)
			}
			credential.mu.Lock()
			credential.err = errors.New("authority removed")
			credential.mu.Unlock()
			if err := send(); err == nil {
				t.Fatal("retry reused stale credential after provider refusal")
			}
			if requests.Load() != 2 {
				t.Fatal("provider refusal reached downstream")
			}
		})
	}
}

func TestReceiverPreparation_AbsentFlagLeavesInitializeUnauthorized(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("X-Receiver-Api-Key") != "" {
			t.Error("absent flag authenticated initialize")
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	credential := &preparationCredentialProvider{err: errors.New("preparation must not pull")}
	p, err := New(Config{Name: "receiver", URL: server.URL, TransportMode: TransportStreamableHTTP, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), OwnedInjectionProvider: credential,
		Injection: &CredentialInjection{Provider: credential, Form: InjectionFormHeader, Header: "X-Receiver-Api-Key"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Connect(ctx); !errors.Is(err, ErrTransportFailed) {
		t.Fatalf("absent preparation flag changed initialize rejection: %v", err)
	}
	if requests.Load() == 0 {
		t.Fatal("absent preparation flag changed bearerless initialize")
	}
}

func TestReceiverPreparation_ConcurrentRequestsKeepExactIdentityAndCancellation(t *testing.T) {
	credential := &preparationCredentialProvider{}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("X-Receiver-Api-Key") != "cred-tenant-"+r.URL.Path[1:] {
			t.Error("receiver credential crossed request identities")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := buildHTTPClient(Config{Name: "receiver", Injection: &CredentialInjection{Provider: credential, AuthenticatePreparation: true, Form: InjectionFormHeader, Header: "X-Receiver-Api-Key"}})
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			user := fmt.Sprintf("user-%d", i)
			ctx, err := identity.With(context.Background(), identity.Identity{TenantID: "tenant", UserID: user, SessionID: "session-" + user})
			if err != nil {
				t.Error(err)
				return
			}
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/"+user, nil)
			if err != nil {
				t.Error(err)
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
		}()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/cancelled", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := client.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Error("cancelled request reached receiver")
	}
	wg.Wait()
	if got := requests.Load(); got != 100 {
		t.Fatalf("requests = %d, want 100 independent successes", got)
	}
}
