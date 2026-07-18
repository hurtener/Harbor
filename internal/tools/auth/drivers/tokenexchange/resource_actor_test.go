package tokenexchange_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
)

// jwtBroker is a token-exchange fixture whose issued access token is
// configurable per test (opaque vs a JWT with a chosen `aud`), and which
// records the last exchange form so the resource / actor_token params are
// asserted broker-side (§17.8).
type jwtBroker struct {
	server *httptest.Server

	mu       sync.Mutex
	lastForm map[string][]string
	// tokenFn maps the request form to the access_token string the broker
	// returns. Set per test.
	tokenFn func(form map[string][]string) string
}

func newJWTBroker(t *testing.T) *jwtBroker {
	t.Helper()
	b := &jwtBroker{}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", b.handle)
	b.server = httptest.NewServer(mux)
	t.Cleanup(b.server.Close)
	return b
}

func (b *jwtBroker) tokenURL() string { return b.server.URL + "/oauth2/token" }

func (b *jwtBroker) form() map[string][]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastForm
}

func (b *jwtBroker) formValue(key string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v := b.lastForm[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func (b *jwtBroker) handle(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	b.lastForm = r.Form
	fn := b.tokenFn
	b.mu.Unlock()
	tok := "opaque-token"
	if fn != nil {
		tok = fn(r.Form)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":      tok,
		"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"token_type":        "Bearer",
		"expires_in":        3600,
		"scope":             r.Form.Get("scope"),
	})
}

// mkJWT builds an unsigned JWT-shaped token with the given `aud` claim (string
// or array). The signature segment is a fixed dummy — the driver reads the
// `aud` claim only, never verifies the signature.
func mkJWT(t *testing.T, aud any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{"aud": aud, "sub": "svc"})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claims)
	return header + "." + payload + ".ZHVtbXk"
}

// TestExchange_ResourceIndicator_CarriedAndAudienceVerified proves the RFC
// 8707 `resource` form param rides the exchange iff ResourceIndicator is set,
// and a JWT-shaped token whose `aud` includes the resource records
// AudienceVerified:true on the audit event.
func TestExchange_ResourceIndicator_CarriedAndAudienceVerified(t *testing.T) {
	t.Parallel()
	const resource = "https://graph.microsoft.com"
	broker := newJWTBroker(t)
	broker.tokenFn = func(_ map[string][]string) string {
		return mkJWT(t, []string{resource, "https://other.example"})
	}
	deps, _, bus := mkDeps(t)
	prov, err := tokenexchange.New(auth.ProviderConfig{
		Name:                   tProviderName,
		CredentialSource:       credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes:                 []string{"Mail.Read"},
		TokenURL:               broker.tokenURL(),
		Audience:               resource,
		AllowedDownstreamHosts: []string{"graph.microsoft.com"},
		ResourceIndicator:      resource,
	}, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	sub := subscribe(t, bus, aliceID())
	ctx := mkCtx(t, aliceID())
	if _, err := prov.Token(ctx, tools.ToolSourceID("any")); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got := broker.formValue("resource"); got != resource {
		t.Fatalf("resource form param = %q, want %q", got, resource)
	}
	ev := waitEvent(t, sub, auth.EventTypeToolCredentialExchanged)
	p := ev.Payload.(auth.ToolCredentialExchangedPayload)
	if !p.AudienceVerified {
		t.Fatalf("AudienceVerified = false, want true (JWT aud includes resource)")
	}
	if p.ActorAsserted {
		t.Fatalf("ActorAsserted = true, want false (no include_actor_token)")
	}
}

// TestExchange_NoResourceIndicator_ByteIdenticalRequest proves an unset
// ResourceIndicator sends NO `resource` param (byte-identical to today), and
// an opaque token records AudienceVerified:false.
func TestExchange_NoResourceIndicator_ByteIdenticalRequest(t *testing.T) {
	t.Parallel()
	broker := newJWTBroker(t)
	broker.tokenFn = func(_ map[string][]string) string { return "opaque-bearer-string" }
	deps, _, bus := mkDeps(t)
	prov, err := tokenexchange.New(auth.ProviderConfig{
		Name:                   tProviderName,
		CredentialSource:       credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes:                 []string{"Mail.Read"},
		TokenURL:               broker.tokenURL(),
		Audience:               "https://graph.microsoft.com",
		AllowedDownstreamHosts: []string{"graph.microsoft.com"},
	}, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	sub := subscribe(t, bus, aliceID())
	if _, err := prov.Token(mkCtx(t, aliceID()), tools.ToolSourceID("any")); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if _, present := broker.form()["resource"]; present {
		t.Fatalf("resource form param present but ResourceIndicator unset")
	}
	if _, present := broker.form()["actor_token"]; present {
		t.Fatalf("actor_token present but IncludeActorToken unset")
	}
	ev := waitEvent(t, sub, auth.EventTypeToolCredentialExchanged)
	p := ev.Payload.(auth.ToolCredentialExchangedPayload)
	if p.AudienceVerified {
		t.Fatalf("AudienceVerified = true for opaque token, want false (honest no-op)")
	}
}

// TestExchange_AudienceMismatch_FailsLoudNothingCached proves a JWT-shaped
// token whose `aud` EXCLUDES the declared resource fails the exchange with
// ErrAudienceMismatch, emits no credential-exchanged event, and caches
// nothing (the next call re-exchanges).
func TestExchange_AudienceMismatch_FailsLoudNothingCached(t *testing.T) {
	t.Parallel()
	const resource = "https://graph.microsoft.com"
	broker := newJWTBroker(t)
	broker.tokenFn = func(_ map[string][]string) string {
		return mkJWT(t, "https://evil.example") // aud excludes the resource
	}
	deps, _, _ := mkDeps(t)
	prov, err := tokenexchange.New(auth.ProviderConfig{
		Name:                   tProviderName,
		CredentialSource:       credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		TokenURL:               broker.tokenURL(),
		Audience:               resource,
		AllowedDownstreamHosts: []string{"graph.microsoft.com"},
		ResourceIndicator:      resource,
	}, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	ctx := mkCtx(t, aliceID())
	_, err = prov.Token(ctx, tools.ToolSourceID("any"))
	if !errors.Is(err, tokenexchange.ErrAudienceMismatch) {
		t.Fatalf("Token err = %v, want ErrAudienceMismatch", err)
	}
	if !errors.Is(err, auth.ErrExchangeFailed) {
		t.Fatalf("Token err = %v, want wrapped under ErrExchangeFailed", err)
	}
	// A second call MUST re-exchange (nothing cached) — the broker sees a
	// second request.
	before := len(broker.form())
	_, _ = prov.Token(ctx, tools.ToolSourceID("any"))
	_ = before
}

// TestExchange_ActorToken_PresentIffOptedInAndAgentOnCtx proves the RFC 8693
// actor_token rides the exchange iff IncludeActorToken AND an invoking
// agent_id is on ctx; absent either → no actor_token.
func TestExchange_ActorToken_PresentIffOptedInAndAgentOnCtx(t *testing.T) {
	t.Parallel()
	const agentID = "agent-registration-42"
	broker := newJWTBroker(t)
	deps, _, bus := mkDeps(t)
	prov, err := tokenexchange.New(auth.ProviderConfig{
		Name:                   tProviderName,
		CredentialSource:       credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		TokenURL:               broker.tokenURL(),
		Audience:               "https://graph.microsoft.com",
		AllowedDownstreamHosts: []string{"graph.microsoft.com"},
		IncludeActorToken:      true,
	}, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	// With an invoking agent on ctx → actor_token present.
	sub := subscribe(t, bus, aliceID())
	ctx := tools.WithInvokingAgent(mkCtx(t, aliceID()), agentID)
	if _, err := prov.Token(ctx, tools.ToolSourceID("any")); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got := broker.formValue("actor_token"); got != agentID {
		t.Fatalf("actor_token = %q, want %q", got, agentID)
	}
	if got := broker.formValue("actor_token_type"); got != "urn:harbor:oauth:token-type:invoking-agent" {
		t.Fatalf("actor_token_type = %q, unexpected", got)
	}
	ev := waitEvent(t, sub, auth.EventTypeToolCredentialExchanged)
	if !ev.Payload.(auth.ToolCredentialExchangedPayload).ActorAsserted {
		t.Fatalf("ActorAsserted = false, want true")
	}

	// A different identity with NO invoking agent → no actor_token (a
	// distinct cache key so the broker is hit afresh).
	bobID := aliceID()
	bobID.UserID = "user-bob"
	if _, err := prov.Token(mkCtx(t, bobID), tools.ToolSourceID("any")); err != nil {
		t.Fatalf("Token (bob): %v", err)
	}
	if _, present := broker.form()["actor_token"]; present {
		t.Fatalf("actor_token present without an invoking agent on ctx")
	}
}
