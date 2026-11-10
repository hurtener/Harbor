package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

func TestInjectedHeaderTransport_PreparationCapturesChallengeBeforeRefusal(t *testing.T) {
	challenge := wwwAuthenticateInsufficientScope(t)
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"unauthorized", http.StatusUnauthorized},
		{"insufficient scope", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var authChallenge *AuthChallenge
			var shortfall *ScopeShortfall
			transport := &injectedHeaderTransport{
				base: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: tc.status, Header: http.Header{"Www-Authenticate": []string{challenge}}, Body: io.NopCloser(strings.NewReader("private body")), Request: req}, nil
				}),
				preparationInjection: &CredentialInjection{Provider: &preparationCredentialProvider{}, Form: InjectionFormHeader, Header: "X-Receiver-Api-Key"},
				onChallenge:          func(ch AuthChallenge) { authChallenge = &ch },
				onScopeShortfall:     func(sf ScopeShortfall) { shortfall = &sf },
			}
			ctx, err := identity.With(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"})
			if err != nil {
				t.Fatal(err)
			}
			slot := &scopeShortfallSlot{}
			ctx = withScopeShortfallSlot(ctx, slot)
			req := httptest.NewRequest(http.MethodPost, "https://receiver.test/mcp?secret=private", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`)).WithContext(ctx)
			resp, err := transport.RoundTrip(req)
			if resp != nil || !errors.Is(err, ErrDownstreamUnauthorized) {
				t.Fatalf("response/error = %v/%v, want typed refusal", resp, err)
			}
			if tc.status == http.StatusUnauthorized {
				if authChallenge == nil || authChallenge.Raw != challenge || shortfall != nil {
					t.Fatalf("401 capture = %+v/%+v", authChallenge, shortfall)
				}
				return
			}
			captured, _ := slot.get()
			if authChallenge != nil || shortfall == nil || captured == nil ||
				shortfall.WWWAuthenticate != challenge || captured.RequiredScopes[0] != "read:calendar" ||
				strings.Contains(shortfall.Origin, "secret") {
				t.Fatalf("403 capture = %+v/%+v/%+v", authChallenge, shortfall, captured)
			}
		})
	}
}

func TestProvider_AutoModeSignedPreparationRefusalDoesNotTrySSE(t *testing.T) {
	var posts atomic.Int64
	var sseGets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			sseGets.Add(1)
		} else if req.Method == http.MethodPost {
			posts.Add(1)
		}
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://auth.example.test/metadata"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	credential := &preparationCredentialProvider{}
	credential.allowedHosts = []string{server.Listener.Addr().String()}
	var captured atomic.Bool
	p, err := New(Config{
		Name: "refused", URL: server.URL, TransportMode: TransportAuto,
		Bus: newTestBus(t), DefaultIdentity: defaultIdentity(),
		OwnedInjectionProvider: credential,
		Injection:              &CredentialInjection{Provider: credential, AuthenticatePreparation: true, Form: InjectionFormHeader, Header: "X-Receiver-Api-Key"},
		OnAuthChallenge:        func(AuthChallenge) { captured.Store(true) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	ctx, cancel := context.WithTimeout(testIdentityCtx(t, defaultIdentity()), 5*time.Second)
	defer cancel()
	if err := p.Connect(ctx); !errors.Is(err, ErrDownstreamUnauthorized) {
		t.Fatalf("Connect = %v, want typed downstream refusal", err)
	}
	if !captured.Load() || posts.Load() != 1 || sseGets.Load() != 0 {
		t.Fatalf("challenge=%v posts=%d SSE GETs=%d; want one POST and no fallback", captured.Load(), posts.Load(), sseGets.Load())
	}
}

func TestCallTool_SignedPreparationInsufficientScope_TypedError(t *testing.T) {
	challenge := wwwAuthenticateInsufficientScope(t)
	server := newScope403Server(t, challenge)
	credential := &preparationCredentialProvider{}
	credential.allowedHosts = []string{server.Listener.Addr().String()}
	var recorded atomic.Pointer[ScopeShortfall]
	p, err := New(Config{
		Name: "signedscope", URL: server.URL, TransportMode: TransportStreamableHTTP,
		Bus: mkScopeTestBus(t), DefaultIdentity: defaultIdentity(),
		OwnedInjectionProvider: credential,
		Injection:              &CredentialInjection{Provider: credential, AuthenticatePreparation: true, Form: InjectionFormHeader, Header: "X-Receiver-Api-Key"},
		OnScopeShortfall:       func(sf ScopeShortfall) { copy := sf; recorded.Store(&copy) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	ctx, err := identity.With(context.Background(), defaultIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	descs, err := p.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var tool tools.ToolDescriptor
	for _, desc := range descs {
		if desc.Tool.Name == "signedscope_needs_scope" {
			tool = desc
			break
		}
	}
	if tool.Invoke == nil {
		t.Fatal("needs_scope tool was not discovered")
	}
	_, err = tool.Invoke(ctx, json.RawMessage(`{}`))
	var scopeErr *tools.ErrInsufficientScope
	if !errors.As(err, &scopeErr) || scopeErr.WWWAuthenticate != challenge {
		t.Fatalf("Invoke error = %v, want typed shortfall", err)
	}
	if row := recorded.Load(); row == nil || row.ToolName != "needs_scope" {
		t.Fatalf("recorded shortfall = %+v, want named tool", row)
	}
}
