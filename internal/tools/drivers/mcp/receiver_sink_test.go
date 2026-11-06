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
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

func TestReceiverSink_ExactSchemeHostAndPort(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, target string
		allowed                []string
		signed, accept         bool
	}{
		{"https default", "https://receiver.test/mcp", "https://receiver.test:443/messages", []string{"receiver.test"}, true, true},
		{"https port80 distinct", "https://receiver.test/mcp", "https://receiver.test:80/messages", []string{"receiver.test"}, true, false},
		{"explicit80 preserved", "https://receiver.test:80/mcp", "https://receiver.test:80/messages", []string{"receiver.test:80"}, true, true},
		{"explicit80 cannot443", "https://receiver.test:80/mcp", "https://receiver.test:443/messages", []string{"receiver.test:80"}, false, false},
		{"remote plaintext", "http://receiver.test/mcp", "http://receiver.test/messages", []string{"receiver.test:80"}, false, false},
		{"https downgrade", "https://127.0.0.1:9000/mcp", "http://127.0.0.1:9000/messages", []string{"127.0.0.1:9000"}, false, false},
		{"configured loopback", "http://127.0.0.1:9000/mcp", "http://127.0.0.1:9000/messages", []string{"127.0.0.1:9000"}, true, true},
		{"signed other allowed sink", "https://receiver.test/mcp", "https://other.test/messages", []string{"receiver.test", "other.test"}, true, false},
		{"unlisted authority", "https://receiver.test/mcp", "https://other.test/messages", []string{"receiver.test"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, _ := url.Parse(tc.endpoint)
			target, _ := url.Parse(tc.target)
			err := (receiverSinkPolicy{endpoint: endpoint, allowed: tc.allowed, signed: tc.signed}).validate(target)
			if (err == nil) != tc.accept {
				t.Fatalf("destination acceptance = %v, want %v", err, tc.accept)
			}
		})
	}
}

func TestReceiverSink_MalformedConfiguredURLRefusesActualDestination(t *testing.T) {
	credential := &preparationCredentialProvider{}
	credential.allowedHosts = []string{"receiver.test"}
	policy := newReceiverSinkPolicy(Config{
		URL:       "https://receiver.test/%zz",
		Injection: &CredentialInjection{Provider: credential},
	})
	target, err := url.Parse("https://receiver.test/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.validate(target); !errors.Is(err, ErrReceiverSinkRefused) {
		t.Fatalf("malformed configured sink accepted actual destination: %v", err)
	}
	credential.mu.Lock()
	pulls := len(credential.identities)
	credential.mu.Unlock()
	if pulls != 0 {
		t.Fatalf("configured sink refusal pulled %d credentials", pulls)
	}
}

func TestReceiverSink_ConstructionRejectsConfiguredDestinationBeforeCredentialPull(t *testing.T) {
	for _, form := range []InjectionForm{InjectionFormHeader, InjectionFormBasic, InjectionFormMeta} {
		for _, preparation := range []bool{false, true} {
			for _, tc := range []struct{ name, endpoint, allowed string }{
				{"plaintext", "http://receiver.test/mcp", "receiver.test:80"},
				{"exact port mismatch", "https://receiver.test:443/mcp", "receiver.test:80"},
			} {
				t.Run(fmt.Sprintf("%s/preparation=%v/%s", form, preparation, tc.name), func(t *testing.T) {
					credential := &preparationCredentialProvider{}
					credential.allowedHosts = []string{tc.allowed}
					provider, err := New(Config{Name: "receiver", URL: tc.endpoint, TransportMode: TransportStreamableHTTP, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), OwnedInjectionProvider: credential,
						Injection: &CredentialInjection{Provider: credential, AuthenticatePreparation: preparation, Form: form, Header: "X-Receiver-Api-Key", MetaKey: []string{"receiver", "api_key"}}})
					if !errors.Is(err, ErrReceiverSinkRefused) || !errors.Is(err, ErrInvalidConfig) || provider != nil {
						t.Fatalf("invalid configured sink constructed a provider: %v", err)
					}
					credential.mu.Lock()
					pulls := len(credential.identities)
					credential.mu.Unlock()
					if pulls != 0 {
						t.Fatalf("configured sink refusal pulled %d credentials", pulls)
					}
				})
			}
		}
	}
}

func TestReceiverSink_HostileSSEEndpointNeverReceivesCredential(t *testing.T) {
	for _, form := range []InjectionForm{InjectionFormHeader, InjectionFormBasic} {
		for _, preparation := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/preparation=%v", form, preparation), func(t *testing.T) {
				var targetRequests atomic.Int64
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					targetRequests.Add(1)
					w.WriteHeader(http.StatusAccepted)
				}))
				defer target.Close()
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s/messages\n\n", target.URL)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}))
				defer origin.Close()
				credential := &preparationCredentialProvider{}
				credential.allowedHosts = []string{origin.Listener.Addr().String()}
				p, err := New(Config{Name: "receiver", URL: origin.URL, TransportMode: TransportSSE, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), OwnedInjectionProvider: credential,
					Injection: &CredentialInjection{Provider: credential, AuthenticatePreparation: preparation, Form: form, Header: "X-Receiver-Api-Key", BasicUsername: "service"}})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = p.Close(context.Background()) }()
				ctx, cancel := context.WithTimeout(testIdentityCtx(t, identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}), 3*time.Second)
				defer cancel()
				if !preparation {
					ctx = withInjectedHeaders(ctx, map[string]string{"X-Receiver-Api-Key": "fixture-key", "Authorization": "Basic fixture"})
				}
				if err := p.Connect(ctx); err == nil {
					t.Fatal("hostile endpoint unexpectedly initialized")
				}
				if got := targetRequests.Load(); got != 0 {
					t.Fatalf("off-sink SSE endpoint received %d requests", got)
				}
			})
		}
	}
}

func TestReceiverSink_AllFormsBlockUnlistedActualURLBeforeWire(t *testing.T) {
	for _, form := range []InjectionForm{InjectionFormHeader, InjectionFormBasic, InjectionFormMeta} {
		for _, preparation := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/preparation=%v", form, preparation), func(t *testing.T) {
				var requests atomic.Int64
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNoContent) }))
				defer target.Close()
				origin := httptest.NewServer(http.NotFoundHandler())
				defer origin.Close()
				credential := &preparationCredentialProvider{}
				credential.allowedHosts = []string{origin.Listener.Addr().String()}
				client := buildHTTPClient(Config{Name: "receiver", URL: origin.URL, Injection: &CredentialInjection{Provider: credential, AuthenticatePreparation: preparation, Form: form, Header: "X-Receiver-Api-Key", MetaKey: []string{"receiver", "api_key"}}})
				ctx := testIdentityCtx(t, identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"})
				ctx = withInjectedHeaders(ctx, map[string]string{"X-Receiver-Api-Key": "fixture-key"})
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, strings.NewReader(`{"method":"tools/call","params":{"_meta":{"receiver":{"api_key":"fixture-key"}}}}`))
				response, err := client.Do(req)
				if response != nil {
					_ = response.Body.Close()
				}
				if !errors.Is(err, ErrReceiverSinkRefused) {
					t.Fatalf("off-sink actual URL was not refused: %v", err)
				}
				credential.mu.Lock()
				pulls := len(credential.identities)
				credential.mu.Unlock()
				if requests.Load() != 0 || pulls != 0 {
					t.Fatalf("off-sink request reached credential or network plane: requests=%d pulls=%d", requests.Load(), pulls)
				}
			})
		}
	}
}

func TestReceiverSink_RefusesRedirectBeforeCredentialInjection(t *testing.T) {
	for _, kind := range []string{"off-host", "plaintext", "strict-same-origin"} {
		t.Run(kind, func(t *testing.T) {
			var targetRequests atomic.Int64
			targetHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				targetRequests.Add(1)
				w.WriteHeader(http.StatusNoContent)
			})
			var target *httptest.Server
			if kind == "off-host" {
				target = httptest.NewTLSServer(targetHandler)
			} else {
				target = httptest.NewServer(targetHandler)
			}
			defer target.Close()
			redirect := target.URL
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/target" {
					targetRequests.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
			}))
			defer origin.Close()
			credential := &stubOAuthProvider{token: "fixture-key", allowedHosts: []string{origin.Listener.Addr().String(), target.Listener.Addr().String()}}
			injection := &CredentialInjection{Provider: credential, Form: InjectionFormHeader, Header: "X-Receiver-Api-Key"}
			if kind == "off-host" {
				redirect = strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
			}
			if kind == "strict-same-origin" {
				redirect = origin.URL + "/target"
				injection.Provider = strictRedirectProvider{credential}
			}
			// Trust only this local TLS fixture while retaining the real transport wrappers.
			base := origin.Client().Transport
			previous := http.DefaultTransport
			http.DefaultTransport = base
			client := buildHTTPClient(Config{Name: "receiver", URL: origin.URL, Injection: injection})
			http.DefaultTransport = previous
			req, _ := http.NewRequestWithContext(withInjectedHeaders(context.Background(), map[string]string{"X-Receiver-Api-Key": "fixture-key"}), http.MethodPost, origin.URL, bytes.NewBufferString(`{"method":"ping"}`))
			response, err := client.Do(req)
			if response != nil {
				_ = response.Body.Close()
			}
			if !errors.Is(err, ErrRedirectToUnlistedHost) {
				t.Fatalf("redirect not refused: %v", err)
			}
			if targetRequests.Load() != 0 {
				t.Fatal("refused redirect reached destination")
			}
		})
	}
}

func TestReceiverPreparation_ResponsesAndDeleteNeverPullAmbientIdentity(t *testing.T) {
	for _, form := range []InjectionForm{InjectionFormHeader, InjectionFormBasic, InjectionFormMeta} {
		t.Run(string(form), func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNoContent) }))
			defer server.Close()
			credential := &preparationCredentialProvider{}
			credential.allowedHosts = []string{server.Listener.Addr().String()}
			client := buildHTTPClient(Config{Name: "receiver", URL: server.URL, OwnedInjectionProvider: credential, Injection: &CredentialInjection{Provider: credential, AuthenticatePreparation: true, Form: form, Header: "X-Receiver-Api-Key", MetaKey: []string{"receiver", "api_key"}}})
			ctx := testIdentityCtx(t, identity.Identity{TenantID: "tenant", UserID: "connection-owner", SessionID: "connect-session"})
			for _, body := range []string{`{"jsonrpc":"2.0","id":1,"result":{}}`, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"unsupported"}}`} {
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(body))
				if response, err := client.Do(req); err == nil {
					_ = response.Body.Close()
					t.Error("server response received an ambient credential")
				}
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, server.URL, nil)
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			credential.mu.Lock()
			pulls := len(credential.identities)
			credential.mu.Unlock()
			if requests.Load() != 0 || pulls != 0 {
				t.Fatalf("response/cleanup used connection identity: requests=%d pulls=%d", requests.Load(), pulls)
			}
		})
	}
}

func TestResolveCredentialInjection_EmptyTokenReportsConnectionAndSource(t *testing.T) {
	p := &Provider{cfg: Config{Name: "logical-connection", Injection: &CredentialInjection{Provider: &emptyTokenProvider{}, Form: InjectionFormHeader, Header: "X-Receiver-Api-Key"}}, source: tools.ToolSourceID("physical-source")}
	_, err := p.resolveInjection(context.Background(), nil)
	if !errors.Is(err, ErrEmptyBearer) || !strings.Contains(err.Error(), `connection "logical-connection"`) || !strings.Contains(err.Error(), `source "physical-source"`) {
		t.Fatalf("lost connection/source diagnostic: %v", err)
	}
}

func TestReceiverPreparation_RealSDKToolRetryReauthorizesEachAttempt(t *testing.T) {
	for _, form := range []InjectionForm{InjectionFormHeader, InjectionFormBasic, InjectionFormMeta} {
		for _, refuse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/refuse=%v", form, refuse), func(t *testing.T) {
				credential := &preparationCredentialProvider{}
				mock := newMockServer()
				handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return mock.server }, nil)
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(bytes.NewReader(body))
					var message struct {
						Method string `json:"method"`
						Params struct {
							Meta map[string]any `json:"_meta"`
						} `json:"params"`
					}
					_ = json.Unmarshal(body, &message)
					if message.Method == "tools/call" {
						attempt := calls.Add(1)
						if attempt == 1 {
							credential.mu.Lock()
							if refuse {
								credential.err = errors.New("provider revoked")
							} else {
								credential.value = "rotated-value"
							}
							credential.mu.Unlock()
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						value := r.Header.Get("X-Receiver-Api-Key")
						if form == InjectionFormBasic {
							_, value, _ = r.BasicAuth()
						}
						if form == InjectionFormMeta {
							nested, _ := message.Params.Meta["receiver"].(map[string]any)
							value, _ = nested["api_key"].(string)
						}
						if value != "rotated-value" {
							t.Error("SDK retry reused prior credential")
						}
					}
					handler.ServeHTTP(w, r)
				}))
				defer server.Close()
				credential.allowedHosts = []string{server.Listener.Addr().String()}
				p, err := New(Config{Name: "receiver", URL: server.URL, TransportMode: TransportStreamableHTTP, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), OwnedInjectionProvider: credential,
					DefaultPolicy: tools.ToolPolicy{MaxRetries: 1, TimeoutMS: 2000, BackoffBase: time.Millisecond}, Injection: &CredentialInjection{Provider: credential, AuthenticatePreparation: true, Form: form, Header: "X-Receiver-Api-Key", BasicUsername: "service", MetaKey: []string{"receiver", "api_key"}}})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = p.Close(context.Background()) }()
				ctx, cancel := context.WithTimeout(testIdentityCtx(t, identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}), 5*time.Second)
				defer cancel()
				if err := p.Connect(ctx); err != nil {
					t.Fatal(err)
				}
				descriptors, err := p.Discover(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var echo tools.ToolDescriptor
				for _, descriptor := range descriptors {
					if descriptor.Tool.Name == "receiver_echo" {
						echo = descriptor
					}
				}
				if echo.Invoke == nil {
					t.Fatal("echo was not discovered")
				}
				_, err = echo.Invoke(ctx, json.RawMessage(`{"text":"retry"}`))
				if refuse {
					var policy *tools.PolicyError
					if err == nil || !errors.As(err, &policy) || policy.Attempts != 2 {
						t.Fatalf("refused retry accounting: %v", err)
					}
					if calls.Load() != 1 {
						t.Fatal("refused retry reached server")
					}
				} else if err != nil || calls.Load() != 2 {
					t.Fatalf("SDK retry: calls=%d err=%v", calls.Load(), err)
				}
			})
		}
	}
}
