package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/appoperation"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

type markerPreparationProvider struct {
	identityCredProvider
	preparationCalls atomic.Int64
}

func (p *markerPreparationProvider) Token(ctx context.Context, _ tools.ToolSourceID) (auth.Token, error) {
	if _, ok := identity.From(ctx); !ok {
		return auth.Token{}, ErrIdentityMissing
	}
	if auth.IsSignedCapabilityPreparation(ctx) {
		p.preparationCalls.Add(1)
		return auth.Token{AccessToken: "prepared", BindingScope: auth.ScopeUser}, nil
	}
	return auth.Token{AccessToken: "ordinary", BindingScope: auth.ScopeUser}, nil
}

func TestMCPSession_ReconnectUsesAttachPreparationAfterCallerCancellation(t *testing.T) {
	mock := newMockServer()
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return mock.server }, &mcpsdk.StreamableHTTPOptions{JSONResponse: false})
	var initializes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read request", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var request struct {
				Method string `json:"method"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				http.Error(w, "decode request", http.StatusBadRequest)
				return
			}
			if request.Method == "initialize" {
				initializes.Add(1)
				if r.Header.Get("Authorization") != "Bearer prepared" {
					http.Error(w, "initialize lacks preparation bearer", http.StatusUnauthorized)
					return
				}
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	credential := &markerPreparationProvider{}
	credential.allowedHosts = []string{server.Listener.Addr().String()}
	p, err := New(Config{Name: "receiver", URL: server.URL, TransportMode: TransportStreamableHTTP,
		Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), OAuthProvider: credential, OwnOAuthProvider: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	attachCtx, cancelAttach := context.WithTimeout(testIdentityCtx(t, defaultIdentity()), 5*time.Second)
	if err := p.Connect(auth.WithSignedCapabilityPreparation(attachCtx)); err != nil {
		cancelAttach()
		t.Fatal(err)
	}
	cancelAttach()
	requestCtx, cancelRequest := context.WithTimeout(testIdentityCtx(t, defaultIdentity()), 10*time.Second)
	defer cancelRequest()
	if _, err := p.Discover(requestCtx); err != nil {
		t.Fatalf("session after attach caller cancellation: %v", err)
	}
	before := credential.preparationCalls.Load()
	failed, _ := p.sessionForRead()
	if err := failed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Discover(requestCtx); err != nil {
		t.Fatalf("reconnect from ordinary request: %v", err)
	}
	if got := initializes.Load(); got != 2 {
		t.Fatalf("initialize attempts = %d, want 2", got)
	}
	if got := credential.preparationCalls.Load(); got <= before {
		t.Fatalf("preparation bearer calls after reconnect = %d, want > %d", got, before)
	}
}

func TestMCPSession_PreparationBaseDropsAppOperationAndDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(testIdentityCtx(t, defaultIdentity()), time.Second)
	defer cancel()
	operationCtx, err := appoperation.WithAgentRead(ctx, "agent", "receiver", "ui://app/index.html")
	if err != nil {
		t.Fatal(err)
	}
	base := preparationBase(auth.WithSignedCapabilityPreparation(operationCtx))
	if _, ok := appoperation.From(base); ok {
		t.Fatal("request-local App operation leaked into session preparation")
	}
	if _, ok := identity.From(base); !ok || !auth.IsSignedCapabilityPreparation(base) {
		t.Fatal("attach identity or preparation marker missing")
	}
	if _, ok := base.Deadline(); ok || base.Done() != nil {
		t.Fatal("session preparation inherited caller lifetime")
	}
}
