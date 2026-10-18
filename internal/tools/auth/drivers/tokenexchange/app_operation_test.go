package tokenexchange_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/appoperation"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
	mcpdriver "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

func appOperationProvider(t *testing.T, handler http.HandlerFunc, authorizers ...auth.SignedCapabilityUseAuthorizer) (auth.OAuthProvider, context.Context) {
	t.Helper()
	broker := httptest.NewServer(handler)
	t.Cleanup(broker.Close)
	binding := signedCapabilityBindingFixture("https://downstream.example")
	if len(authorizers) > 0 {
		binding.UseAuthorizer = authorizers[0]
	}
	deps, _, _ := mkDeps(t)
	p, err := tokenexchange.New(auth.ProviderConfig{
		Name: binding.ProviderName, CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes: []string{"legacy.write"}, TokenURL: broker.URL, Audience: binding.Audience,
		AllowedDownstreamHosts: []string{"downstream.example"}, ResourceIndicator: binding.Resource,
		SignedCapability: &binding,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	ctx := tools.WithEffectiveAgentConfig(mkCtx(t, identity.Identity{
		TenantID: binding.TenantID, UserID: binding.UserID, SessionID: binding.SessionID,
	}), binding.AgentID)
	return p, ctx
}

func writeAppToken(t *testing.T, w http.ResponseWriter, r *http.Request, expires int, ack bool) {
	t.Helper()
	sum := sha256.Sum256([]byte(r.Form.Get("app_operation")))
	response := map[string]any{"access_token": r.Form.Get("app_operation"), "expires_in": expires,
		"token_type": "Bearer", "audience": "https://downstream.example", "resource": "https://downstream.example"}
	if ack {
		response["app_operation_sha256"] = hex.EncodeToString(sum[:])
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		t.Error(err)
	}
}

func appOperationContext(t *testing.T, ctx context.Context, ref string) context.Context {
	t.Helper()
	ctx, err := appoperation.WithCall(ctx, ref, "agent-signed", "server", "ui://report", "generation", "save", json.RawMessage(`{"id":"target"}`))
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestAppOperation_ConcurrentCallsNeverShareCredentials(t *testing.T) {
	var calls atomic.Int64
	p, ctx := appOperationProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		calls.Add(1)
		if r.Form.Get("app_operation") == "" {
			if err := json.NewEncoder(w).Encode(map[string]any{"access_token": "legacy-cached", "expires_in": 3600,
				"token_type": "Bearer", "audience": "https://downstream.example", "resource": "https://downstream.example"}); err != nil {
				t.Error(err)
			}
			return
		}
		if r.Form.Get("scope") != "" {
			t.Error("App exchange inherited configured broad scopes")
		}
		var binding appoperation.Binding
		if err := json.Unmarshal([]byte(r.Form.Get("app_operation")), &binding); err != nil {
			t.Error(err)
			return
		}
		if binding.Kind != "tool" || binding.Tool != "save" || binding.ServerID != "server" || binding.ArgumentsSHA256 == "" {
			t.Error("operation coordinates missing")
		}
		if r.Form.Get("actor_token") == "" || r.Form.Get("subject_token") == "" {
			t.Error("existing authenticated binding missing")
		}
		writeAppToken(t, w, r, 30, true)
	})
	// Prime the same provider's ordinary cache. App calls may not use it.
	if tok, err := p.Token(ctx, "server"); err != nil || tok.AccessToken != "legacy-cached" {
		t.Fatalf("prime: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref := fmt.Sprintf("%064x", i+1)
			callCtx := appOperationContext(t, ctx, ref)
			tok, err := p.Token(callCtx, "server")
			if err != nil {
				t.Error(err)
				return
			}
			var binding appoperation.Binding
			if err := json.Unmarshal([]byte(tok.AccessToken), &binding); err != nil || binding.Reference != ref {
				t.Errorf("cross-call credential: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if calls.Load() != 129 {
		t.Fatalf("exchanges=%d, want 129", calls.Load())
	}
	if tok, err := p.Token(ctx, "server"); err != nil || tok.AccessToken != "legacy-cached" {
		t.Fatalf("ordinary cache changed: %v", err)
	}
	if calls.Load() != 129 {
		t.Fatal("ordinary profile stopped caching")
	}
	// Even an identical operation is freshly authorized on another pull.
	same := appOperationContext(t, ctx, fmt.Sprintf("%064x", 1))
	for i := 0; i < 2; i++ {
		if _, err := p.Token(same, "server"); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 131 {
		t.Fatal("identical operation silently reused authority")
	}
}

func TestAppOperation_RequiresBoundedBrokerAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  int
		ack  bool
	}{{"old broker", 30, false}, {"no expiry", 0, true}, {"long expiry", 31, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			p, ctx := appOperationProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
					return
				}
				calls.Add(1)
				writeAppToken(t, w, r, tc.ttl, tc.ack)
			})
			ctx = appOperationContext(t, ctx, strings.Repeat("a", 64))
			for i := 0; i < 2; i++ {
				if _, err := p.Token(ctx, "server"); !errors.Is(err, auth.ErrExchangeFailed) {
					t.Fatalf("unbounded/old broker accepted: %v", err)
				}
			}
			if calls.Load() != 2 {
				t.Fatal("failed response cached")
			}
			if _, err := p.Token(ctx, "other-server"); !errors.Is(err, auth.ErrExchangeFailed) {
				t.Fatal("source swap accepted")
			}
			if calls.Load() != 2 {
				t.Fatal("source swap reached broker")
			}
		})
	}
}

func TestAppOperation_CancellationDoesNotPoisonAnotherCall(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	p, ctx := appOperationProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		var binding appoperation.Binding
		if err := json.Unmarshal([]byte(r.Form.Get("app_operation")), &binding); err != nil {
			t.Error(err)
			return
		}
		if binding.Reference == strings.Repeat("a", 64) {
			close(entered)
			<-r.Context().Done()
			close(cancelled)
			return
		}
		writeAppToken(t, w, r, 30, true)
	})
	first, cancel := context.WithCancel(appOperationContext(t, ctx, strings.Repeat("a", 64)))
	done := make(chan error, 1)
	go func() { _, err := p.Token(first, "server"); done <- err }()
	<-entered
	if _, err := p.Token(appOperationContext(t, ctx, strings.Repeat("b", 64)), "server"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	<-cancelled
}

func TestAppOperation_RealMCPBrokerDispatchAndUnknownEffect(t *testing.T) {
	var exchanges atomic.Int64
	broker, ctx := appOperationProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		exchanges.Add(1)
		writeAppToken(t, w, r, 30, true)
	})
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var toolCalls, reads atomic.Int64
	var fail atomic.Bool
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "operation-fixture", Version: "v0"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "save", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, r *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			toolCalls.Add(1)
			// The SDK must receive the exact integer, without float64 rounding.
			if !strings.Contains(string(r.Params.Arguments), "9007199254740993") {
				t.Error("integer changed on MCP wire")
			}
			if fail.Load() {
				return nil, errors.New("fixture lost result after effect")
			}
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "saved"}}}, nil
		})
	server.AddResource(&mcpsdk.Resource{URI: "ui://report", Name: "report", MIMEType: "text/html"},
		func(context.Context, *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
			reads.Add(1)
			return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: "ui://report", MIMEType: "text/html", Text: "<html>report</html>"}}}, nil
		})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			var rpc struct {
				Method string
				Params json.RawMessage
			}
			if err := json.Unmarshal(body, &rpc); err != nil {
				t.Error(err)
				return
			}
			if rpc.Method == "tools/call" || rpc.Method == "resources/read" {
				var binding appoperation.Binding
				if err := json.Unmarshal([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), &binding); err != nil {
					t.Error("missing operation-specific bearer")
					http.Error(w, "denied", 403)
					return
				}
				if rpc.Method == "tools/call" {
					var call struct {
						Name      string
						Arguments json.RawMessage
					}
					if err := json.Unmarshal(rpc.Params, &call); err != nil {
						t.Error(err)
						return
					}
					args, err := appoperation.DecodeArguments(call.Arguments)
					if err != nil {
						t.Error(err)
						return
					}
					if err := binding.CheckCall("server", "server_"+call.Name, args); err != nil {
						t.Error("wire authority/input mismatch")
						http.Error(w, "denied", 403)
						return
					}
				} else if binding.Kind != "resource" {
					t.Error("tool authority used for resource")
					http.Error(w, "denied", 403)
					return
				}
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	p, err := mcpdriver.New(mcpdriver.Config{Name: "server", URL: front.URL, TransportMode: mcpdriver.TransportStreamableHTTP,
		Bus: mkBus(t, mkRedactor()), DefaultIdentity: aliceID(), OAuthProvider: broker})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := p.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	descs, err := p.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var invoke func(context.Context, json.RawMessage) (tools.ToolResult, error)
	for _, d := range descs {
		if d.Tool.Name == "server_save" {
			invoke = d.Invoke
		}
	}
	if invoke == nil {
		t.Fatal("tool missing")
	}
	args := json.RawMessage(`{"n":9007199254740993}`)
	callCtx, err := appoperation.WithCall(ctx, strings.Repeat("a", 64), "agent-signed", "server", "ui://report", "generation", "server_save", args)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(callCtx, args); err != nil {
		t.Fatal(err)
	}
	if toolCalls.Load() != 1 || exchanges.Load() != 1 {
		t.Fatalf("call/exchange counts: %d/%d", toolCalls.Load(), exchanges.Load())
	}
	if _, err := invoke(callCtx, json.RawMessage(`{"n":9007199254740992}`)); err == nil {
		t.Fatal("changed input accepted")
	}
	if toolCalls.Load() != 1 || exchanges.Load() != 1 {
		t.Fatal("changed input reached broker/provider")
	}
	readCtx, err := appoperation.WithRead(ctx, strings.Repeat("b", 64), "agent-signed", "server", "ui://report")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.ReadResource(readCtx, "ui://report"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.ReadResource(readCtx, "ui://other"); err == nil {
		t.Fatal("changed resource accepted")
	}
	if reads.Load() != 1 || exchanges.Load() != 2 {
		t.Fatal("resource binding lost")
	}
	fail.Store(true)
	if _, err := invoke(callCtx, args); err == nil {
		t.Fatal("lost result returned success")
	}
	if toolCalls.Load() != 2 || exchanges.Load() != 3 {
		t.Fatalf("unknown effect retried: calls=%d exchanges=%d", toolCalls.Load(), exchanges.Load())
	}
}

type appOperationUseGate struct{ revoked atomic.Bool }

func (g *appOperationUseGate) AuthorizeSignedCapabilityUse(context.Context, string, string, string, bool) error {
	if g.revoked.Load() {
		return auth.ErrCredentialRejected
	}
	return nil
}
func TestAppOperation_WithdrawalDuringExchangeRefusesResult(t *testing.T) {
	gate := &appOperationUseGate{}
	var calls atomic.Int64
	p, ctx := appOperationProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		calls.Add(1)
		gate.revoked.Store(true)
		writeAppToken(t, w, r, 30, true)
	}, gate)
	ctx = appOperationContext(t, ctx, strings.Repeat("a", 64))
	if _, err := p.Token(ctx, "server"); !errors.Is(err, auth.ErrCredentialRejected) {
		t.Fatalf("withdrawn result returned: %v", err)
	}
	if _, err := p.Token(ctx, "server"); !errors.Is(err, auth.ErrCredentialRejected) {
		t.Fatalf("withdrawn credential reused: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("withdrawn use reached broker again")
	}
}
