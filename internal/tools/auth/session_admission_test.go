package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/sessionadmission"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func TestCallbackHandler_EnrolledSessionPreservesNativeOAuth(t *testing.T) {
	har := newProviderHarness(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)
	_, err := har.provider.Token(ctx, har.userCfg.Source)
	var required *ErrAuthRequired
	if !errors.As(err, &required) {
		t.Fatalf("expected native OAuth pause: %v", err)
	}
	code, _, err := har.server.VisitAuthorizeURL(required.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	gate, err := sessionadmission.New(store)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := identity.WithVerified(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	admin = protocolauth.WithScopes(admin, []protocolauth.Scope{protocolauth.ScopeAdmin})
	admin = protocolauth.WithTokenAuthority(admin, protocolauth.TokenAuthority{Issuer: "https://issuer.example", Subject: "coordinator"})
	if _, err := gate.Enroll(admin, id, 0, 1); err != nil {
		t.Fatal(err)
	}
	handler := CallbackHandler(map[string]OAuthProvider{"github": har.provider})
	request := httptest.NewRequest(http.MethodGet, CallbackPath+"?state="+required.State+"&code="+code, nil)
	// The callback carries no broad bearer and cannot get steering authority
	// from an empty method set. Its existing opaque flow state is authoritative.
	callbackCtx := protocolauth.WithMethodReach(sessionadmission.WithGate(request.Context(), gate), nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request.WithContext(callbackCtx))
	if response.Code != http.StatusOK {
		t.Fatalf("native callback status=%d body=%s", response.Code, response.Body.String())
	}
	status, err := har.coordinator.Status(ctx, pauseresume.Token(required.PauseToken))
	if err != nil || status.State != pauseresume.StatusResumed {
		t.Fatalf("native pause not resumed: %+v %v", status, err)
	}
	if _, err := gate.Enroll(admin, id, 1, 2); err != nil {
		t.Fatalf("callback did not finish acceptance: %v", err)
	}
	if _, err := har.provider.Token(ctx, har.userCfg.Source); err != nil {
		t.Fatalf("native credential completion lost: %v", err)
	}
}
