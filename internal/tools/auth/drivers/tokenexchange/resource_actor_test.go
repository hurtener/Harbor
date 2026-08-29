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
	"time"

	"github.com/hurtener/Harbor/internal/identity"
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
	calls    int
	// tokenFn maps the request form to the access_token string the broker
	// returns. Set per test.
	tokenFn          func(form map[string][]string) string
	responseAudience string
	responseResource string
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

func (b *jwtBroker) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
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
	b.calls++
	fn := b.tokenFn
	b.mu.Unlock()
	tok := "opaque-token"
	if fn != nil {
		tok = fn(r.Form)
	}
	w.Header().Set("Content-Type", "application/json")
	response := map[string]any{
		"access_token":      tok,
		"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"token_type":        "Bearer",
		"expires_in":        3600,
		"scope":             r.Form.Get("scope"),
	}
	if b.responseAudience != "" {
		response["audience"] = b.responseAudience
	}
	if b.responseResource != "" {
		response["resource"] = b.responseResource
	}
	_ = json.NewEncoder(w).Encode(response)
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
	// second request AND the second call fails the same loud way. A
	// regression that silently cached the mismatched token would drop the
	// broker call count and/or return a stale success here, defeating the
	// confused-deputy guard this leg exists for.
	before := broker.callCount()
	_, err2 := prov.Token(ctx, tools.ToolSourceID("any"))
	if got := broker.callCount(); got != before+1 {
		t.Fatalf("second Token() broker calls = %d, want %d (nothing may be cached on audience mismatch)", got, before+1)
	}
	if !errors.Is(err2, tokenexchange.ErrAudienceMismatch) {
		t.Fatalf("second Token() err = %v, want ErrAudienceMismatch (mismatch must fail loud every time)", err2)
	}
}

func signedCapabilityBindingFixture(resource string) auth.SignedCapabilityExchangeBinding {
	return auth.SignedCapabilityExchangeBinding{
		TenantID: "tenant-signed", UserID: "user-signed", SessionID: "session-signed",
		AgentID: "agent-signed", ProviderName: "signed-provider", CapabilityRevision: "revision-signed",
		PairFingerprint: "pair-fingerprint-signed", URLDigest: "url-digest-signed", SinkDigest: "sink-digest-signed",
		Audience: resource, Resource: resource,
		AuthorityOperationKind: "signed-operation", PublisherEpoch: "publisher-epoch", UseAuthorizer: allowSignedCapabilityUseAuthorizer{},
	}
}

// TestSignedCapability_ThirdPartyJWTAppAudienceDoesNotRequireResourceURI proves
// the signed-capability path uses the exact broker response destination tuple
// rather than assuming a third-party JWT's application audience identifier is
// the RFC 8707 resource URI. The signed tuple is exact, while the audit bit
// remains false because no JWT `aud` comparison was performed.
func TestSignedCapability_ThirdPartyJWTAppAudienceDoesNotRequireResourceURI(t *testing.T) {
	t.Parallel()
	const (
		resource    = "https://downstream.example"
		appAudience = "00000000-0000-0000-0000-000000000001"
	)
	binding := signedCapabilityBindingFixture(resource)
	broker := newJWTBroker(t)
	broker.tokenFn = func(_ map[string][]string) string { return mkJWT(t, appAudience) }
	broker.responseAudience = binding.Audience
	broker.responseResource = binding.Resource
	deps, _, bus := mkDeps(t)
	prov, err := tokenexchange.New(auth.ProviderConfig{
		Name: binding.ProviderName, CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes: []string{"resource.read"}, TokenURL: broker.tokenURL(), Audience: binding.Audience,
		AllowedDownstreamHosts: []string{"downstream.example"}, ResourceIndicator: binding.Resource,
		SignedCapability: &binding,
	}, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	id := identity.Identity{TenantID: binding.TenantID, UserID: binding.UserID, SessionID: binding.SessionID}
	ctx := tools.WithEffectiveAgentConfig(mkCtx(t, id), binding.AgentID)
	sub := subscribe(t, bus, id)
	tok, err := prov.Token(ctx, tools.ToolSourceID("signed-provider"))
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatal("Token returned an empty access token")
	}
	if got := broker.formValue("audience"); got != binding.Audience {
		t.Fatalf("audience form param = %q, want exact signed audience %q", got, binding.Audience)
	}
	if got := broker.formValue("resource"); got != binding.Resource {
		t.Fatalf("resource form param = %q, want exact signed resource %q", got, binding.Resource)
	}
	ev := waitEvent(t, sub, auth.EventTypeToolCredentialExchanged)
	if got := ev.Payload.(auth.ToolCredentialExchangedPayload).AudienceVerified; got {
		t.Fatal("AudienceVerified = true, want false when signed destination proof replaces JWT aud comparison")
	}
}

// TestSignedCapability_DestinationMismatchFailsLoudAndReexchanges proves both
// broker response destination fields remain strict on the signed path. A
// mismatch emits no success event, is not cached, and the next call reaches the
// broker again rather than serving a misbound token.
func TestSignedCapability_DestinationMismatchFailsLoudAndReexchanges(t *testing.T) {
	t.Parallel()
	const resource = "https://downstream.example"
	for _, tc := range []struct {
		name             string
		responseAudience string
		responseResource string
	}{
		{name: "audience", responseAudience: "https://wrong-audience.example", responseResource: resource},
		{name: "resource", responseAudience: resource, responseResource: "https://wrong-resource.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			binding := signedCapabilityBindingFixture(resource)
			broker := newJWTBroker(t)
			broker.tokenFn = func(_ map[string][]string) string {
				return mkJWT(t, "00000000-0000-0000-0000-000000000001")
			}
			broker.responseAudience = tc.responseAudience
			broker.responseResource = tc.responseResource
			deps, _, bus := mkDeps(t)
			prov, err := tokenexchange.New(auth.ProviderConfig{
				Name: binding.ProviderName, CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
				Scopes: []string{"resource.read"}, TokenURL: broker.tokenURL(), Audience: binding.Audience,
				AllowedDownstreamHosts: []string{"downstream.example"}, ResourceIndicator: binding.Resource,
				SignedCapability: &binding,
			}, deps)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = prov.Close(context.Background()) })

			id := identity.Identity{TenantID: binding.TenantID, UserID: binding.UserID, SessionID: binding.SessionID}
			ctx := tools.WithEffectiveAgentConfig(mkCtx(t, id), binding.AgentID)
			sub := subscribe(t, bus, id)
			if _, err := prov.Token(ctx, tools.ToolSourceID(binding.ProviderName)); !errors.Is(err, auth.ErrExchangeFailed) {
				t.Fatalf("Token err = %v, want auth.ErrExchangeFailed", err)
			}
			select {
			case ev := <-sub:
				t.Fatalf("mismatched destination emitted event %q", ev.Type)
			case <-time.After(100 * time.Millisecond):
			}
			before := broker.callCount()
			if _, err := prov.Token(ctx, tools.ToolSourceID(binding.ProviderName)); !errors.Is(err, auth.ErrExchangeFailed) {
				t.Fatalf("second Token err = %v, want auth.ErrExchangeFailed", err)
			}
			if got := broker.callCount(); got != before+1 {
				t.Fatalf("second Token broker calls = %d, want %d (destination mismatch must not cache)", got, before+1)
			}
		})
	}
}

// TestExchange_MalformedJWT_ParseSafeOpaqueNoOp proves a returned token that
// LOOKS JWT-shaped but is structurally malformed (wrong segment count, non-
// base64url payload, valid-base64-but-non-JSON payload) is a panic-safe
// OPAQUE no-op: the exchange still SUCCEEDS, records AudienceVerified:false,
// and never fails loud — a hostile/broken broker cannot crash the exchange
// nor forge a false-positive audience pass. (verifyAudience returns
// (false, nil) for every unparseable shape; the 64 KiB LimitReader bounds
// the body so a pathological token cannot exhaust memory.)
func TestExchange_MalformedJWT_ParseSafeOpaqueNoOp(t *testing.T) {
	t.Parallel()
	const resource = "https://graph.microsoft.com"
	// A valid-base64url segment whose decoded bytes are NOT JSON.
	nonJSONPayload := base64.RawURLEncoding.EncodeToString([]byte("not-json-at-all"))
	cases := []struct {
		name  string
		token string
	}{
		{"two_segments", "aGVhZGVy.cGF5bG9hZA"},              // only 2 segments
		{"non_base64url_payload", "x.@@@not-base64@@@.z"},    // middle segment not base64url
		{"valid_b64_non_json", "x." + nonJSONPayload + ".z"}, // decodes, but not JSON
		{"empty_payload", "x..z"},                            // empty middle segment
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			broker := newJWTBroker(t)
			broker.tokenFn = func(_ map[string][]string) string { return tc.token }
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
			// A malformed JWT is treated as opaque — the exchange must
			// SUCCEED (opaque tokens are honest no-ops, not mismatches).
			if _, err := prov.Token(ctx, tools.ToolSourceID("any")); err != nil {
				t.Fatalf("Token with malformed-JWT token = %v, want success (opaque no-op)", err)
			}
			ev := waitEvent(t, sub, auth.EventTypeToolCredentialExchanged)
			p := ev.Payload.(auth.ToolCredentialExchangedPayload)
			if p.AudienceVerified {
				t.Fatalf("AudienceVerified = true for malformed JWT %q, want false (unparseable ⇒ opaque)", tc.token)
			}
		})
	}
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

type allowSignedCapabilityUseAuthorizer struct{}

func (allowSignedCapabilityUseAuthorizer) AuthorizeSignedCapabilityUse(context.Context, string, string, string, bool) error {
	return nil
}

func TestSignedCapability_RegistrarActorAndInvokerSubjectAreSeparated(t *testing.T) {
	broker := newJWTBroker(t)
	broker.tokenFn = func(_ map[string][]string) string { return mkJWT(t, "https://downstream.example") }
	broker.responseAudience = "https://downstream.example"
	broker.responseResource = "https://downstream.example"
	deps, _, _ := mkDeps(t)
	registrar := aliceID()
	binding := auth.SignedCapabilityExchangeBinding{
		TenantID: registrar.TenantID, UserID: registrar.UserID, SessionID: registrar.SessionID,
		AgentID: "agent-selected", ProviderName: "signed-provider", CapabilityRevision: "revision-1",
		PairFingerprint: "pair-fingerprint", URLDigest: "url-digest", SinkDigest: "sink-digest",
		Audience: "https://downstream.example", Resource: "https://downstream.example",
		AuthorityOperationKind: "signed-operation", PublisherEpoch: "publisher-epoch", UseAuthorizer: allowSignedCapabilityUseAuthorizer{},
	}
	prov, err := tokenexchange.New(auth.ProviderConfig{
		Name: "signed-provider", CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes: []string{"Mail.Read"}, TokenURL: broker.tokenURL(), Audience: binding.Audience,
		AllowedDownstreamHosts: []string{"downstream.example"}, ResourceIndicator: binding.Resource, SignedCapability: &binding,
	}, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })

	invoker := registrar
	invoker.UserID, invoker.SessionID = "user-invoker", "session-invoker"
	invokerCtx := tools.WithEffectiveAgentConfig(mkCtx(t, invoker), binding.AgentID)
	if _, err := prov.Token(invokerCtx, tools.ToolSourceID("any")); err != nil {
		t.Fatalf("invoker Token: %v", err)
	}
	if got := decodeSubject(broker.formValue("subject_token")); got != (subjectTriple{TenantID: invoker.TenantID, UserID: invoker.UserID, SessionID: invoker.SessionID}) {
		t.Fatalf("subject_token = %+v, want live invoker", got)
	}
	actorRaw, err := base64.RawURLEncoding.DecodeString(broker.formValue("actor_token"))
	if err != nil {
		t.Fatalf("decode immutable registrar actor: %v", err)
	}
	var actor map[string]any
	if err := json.Unmarshal(actorRaw, &actor); err != nil {
		t.Fatalf("decode immutable registrar actor JSON: %v", err)
	}
	if actor["user_id"] != registrar.UserID || actor["session_id"] != registrar.SessionID || actor["agent_id"] != binding.AgentID {
		t.Fatalf("actor = %#v, want immutable registrar and agent", actor)
	}

	secondInvoker := invoker
	secondInvoker.UserID, secondInvoker.SessionID = "user-second", "session-second"
	if _, err := prov.Token(tools.WithEffectiveAgentConfig(mkCtx(t, secondInvoker), binding.AgentID), tools.ToolSourceID("any")); err != nil {
		t.Fatalf("second invoker Token: %v", err)
	}
	if broker.callCount() != 2 {
		t.Fatalf("broker calls = %d, want distinct live-subject cache entries", broker.callCount())
	}
	useAuthorizer, ok := prov.(interface{ AuthorizeUse(context.Context) error })
	if !ok {
		t.Fatal("signed provider does not expose final bearer authorization")
	}
	if err := useAuthorizer.AuthorizeUse(mkCtx(t, invoker)); err == nil {
		t.Fatal("cached bearer final authorization accepted absent run admission")
	}

	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "absent_admission", ctx: mkCtx(t, invoker)},
		{name: "wrong_agent", ctx: tools.WithEffectiveAgentConfig(mkCtx(t, invoker), "other-agent")},
		{name: "wrong_tenant", ctx: tools.WithEffectiveAgentConfig(mkCtx(t, func() identity.Identity { id := invoker; id.TenantID = "other-tenant"; return id }()), binding.AgentID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := broker.callCount()
			if _, err := prov.Token(tc.ctx, tools.ToolSourceID("any")); err == nil {
				t.Fatal("Token succeeded without exact caller admission")
			}
			if got := broker.callCount(); got != before {
				t.Fatalf("denied caller broker calls = %d, want %d", got, before)
			}
		})
	}
}
