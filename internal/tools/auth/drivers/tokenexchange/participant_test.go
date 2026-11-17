package tokenexchange_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
)

type participantSubject struct {
	TenantID     string `json:"tenant_id"`
	UserID       string `json:"user_id"`
	SessionID    string `json:"session_id"`
	SessionOwner string `json:"session_owner,omitempty"`
}

func decodeParticipantSubject(t *testing.T, s string) participantSubject {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	var st participantSubject
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("unmarshal subject: %v", err)
	}
	return st
}

// A participant writing in another user's session exchanges as itself,
// names the partition owner for the broker to authorize, and never shares
// a cache entry with either its own sessions or the owner's.
func TestToken_SessionParticipantActsAsWriter(t *testing.T) {
	broker := newFakeBroker(t)
	prov, _, _ := mkProvider(t, broker)
	partition := identity.Identity{TenantID: tDummyTenant, UserID: "owner-user", SessionID: "shared-session"}

	guest, err := identity.WithParticipant(mkCtx(t, partition), identity.Participant{UserID: "writer-user", Speaker: "Writer"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := prov.Token(guest, "any")
	if err != nil {
		t.Fatalf("participant Token: %v", err)
	}
	if tok.UserID != "writer-user" {
		t.Fatalf("token user = %q, want the writer", tok.UserID)
	}
	got := decodeParticipantSubject(t, broker.form().Get("subject_token"))
	want := participantSubject{TenantID: tDummyTenant, UserID: "writer-user", SessionID: "shared-session", SessionOwner: "owner-user"}
	if got != want {
		t.Fatalf("subject = %+v, want %+v", got, want)
	}

	// The partition owner in the same session: its own subject, no owner field.
	if _, err := prov.Token(mkCtx(t, partition), "any"); err != nil {
		t.Fatal(err)
	}
	if got := decodeParticipantSubject(t, broker.form().Get("subject_token")); got != (participantSubject{TenantID: tDummyTenant, UserID: "owner-user", SessionID: "shared-session"}) {
		t.Fatalf("owner subject = %+v", got)
	}

	// The writer in its own session is a separate cache family.
	if _, err := prov.Token(mkCtx(t, identity.Identity{TenantID: tDummyTenant, UserID: "writer-user", SessionID: "own"}), "any"); err != nil {
		t.Fatal(err)
	}
	if broker.calls() != 3 {
		t.Fatalf("broker calls = %d, want 3 (participant, owner and own-session never share a cached credential)", broker.calls())
	}
	// A repeat participant call in the shared session is served from its own cache.
	if _, err := prov.Token(guest, "any"); err != nil {
		t.Fatal(err)
	}
	if broker.calls() != 3 {
		t.Fatalf("broker calls = %d after a cached participant repeat, want 3", broker.calls())
	}
}

// A labelled owner (participant equal to the partition user) is an ordinary
// subject: byte-identical to an unlabelled call.
func TestToken_LabelledOwnerIsOrdinarySubject(t *testing.T) {
	broker := newFakeBroker(t)
	prov, _, _ := mkProvider(t, broker)
	ctx, err := identity.WithParticipant(mkCtx(t, aliceID()), identity.Participant{UserID: aliceID().UserID, Speaker: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Token(ctx, "any"); err != nil {
		t.Fatal(err)
	}
	if got := decodeParticipantSubject(t, broker.form().Get("subject_token")); got.SessionOwner != "" || got.UserID != aliceID().UserID {
		t.Fatalf("labelled owner subject = %+v", got)
	}
}

// participantBrokerTransport exercises the real HTTP request and broker
// handler without requiring a socket in the test environment.
type participantBrokerTransport struct{ broker *jwtBroker }

func (tr participantBrokerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	tr.broker.handle(recorder, req)
	return recorder.Result(), nil
}

func signedParticipantProvider(t *testing.T, binding auth.SignedCapabilityExchangeBinding) (auth.OAuthProvider, *jwtBroker) {
	t.Helper()
	broker := &jwtBroker{}
	broker.responseAudience, broker.responseResource = binding.Audience, binding.Resource
	broker.tokenFn = func(form map[string][]string) string { return form["subject_token"][0] }
	deps, _, _ := mkDeps(t)
	deps.HTTPClient = &http.Client{Transport: participantBrokerTransport{broker: broker}}
	prov, err := tokenexchange.New(auth.ProviderConfig{
		Name: binding.ProviderName, CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes: []string{"resource.read"}, TokenURL: "https://broker.example/oauth2/token", Audience: binding.Audience,
		AllowedDownstreamHosts: []string{"downstream.example"}, ResourceIndicator: binding.Resource, SignedCapability: &binding,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prov.Close(context.Background()) })
	return prov, broker
}

func signedParticipantContext(t *testing.T, binding auth.SignedCapabilityExchangeBinding) (context.Context, identity.Identity) {
	t.Helper()
	partition := identity.Identity{TenantID: binding.TenantID, UserID: "owner-user", SessionID: "shared-session"}
	ctx, err := identity.WithParticipant(mkCtx(t, partition), identity.Participant{UserID: "writer-user"})
	if err != nil {
		t.Fatal(err)
	}
	return tools.WithEffectiveAgentConfig(ctx, binding.AgentID), partition
}

func TestSignedCapability_AgentScopedParticipantActsAsWriter(t *testing.T) {
	binding := signedCapabilityBindingFixture("https://downstream.example")
	binding.AgentScoped = true
	prov, broker := signedParticipantProvider(t, binding)
	guest, partition := signedParticipantContext(t, binding)
	tok, err := prov.Token(guest, "any")
	if err != nil {
		t.Fatalf("participant Token: %v", err)
	}
	want := participantSubject{TenantID: partition.TenantID, UserID: "writer-user", SessionID: partition.SessionID, SessionOwner: partition.UserID}
	if got := decodeParticipantSubject(t, broker.formValue("subject_token")); got != want {
		t.Fatalf("participant subject = %+v, want %+v", got, want)
	}
	actorRaw, err := base64.RawURLEncoding.DecodeString(broker.formValue("actor_token"))
	if err != nil {
		t.Fatal(err)
	}
	if wantActor, _ := json.Marshal(binding); string(actorRaw) != string(wantActor) {
		t.Fatalf("actor = %s, want signed service binding %s", actorRaw, wantActor)
	}
	if err := prov.(interface{ AuthorizeUse(context.Context) error }).AuthorizeUse(guest); err != nil {
		t.Fatalf("participant AuthorizeUse: %v", err)
	}
	ownerCtx := tools.WithEffectiveAgentConfig(mkCtx(t, partition), binding.AgentID)
	ownerToken, err := prov.Token(ownerCtx, "any")
	if err != nil {
		t.Fatal(err)
	}
	if ownerToken.AccessToken == tok.AccessToken {
		t.Fatal("participant reused the owner's credential")
	}
	if _, err := prov.Token(guest, "any"); err != nil || broker.callCount() != 2 {
		t.Fatalf("cached participant Token = %v, broker calls = %d, want 2", err, broker.callCount())
	}
}

func TestSignedCapability_ParticipantRefusedWithoutAgentScope(t *testing.T) {
	for name, userID := range map[string]string{"zero_value_flag": "", "owner_personal": "owner-user"} {
		t.Run(name, func(t *testing.T) {
			binding := signedCapabilityBindingFixture("https://downstream.example")
			if userID != "" {
				binding.UserID = userID
			}
			prov, broker := signedParticipantProvider(t, binding)
			ctx, _ := signedParticipantContext(t, binding)
			if _, err := prov.Token(ctx, "any"); !errors.Is(err, auth.ErrIdentityRequired) {
				t.Fatalf("Token err = %v, want ErrIdentityRequired", err)
			}
			if err := prov.(interface{ AuthorizeUse(context.Context) error }).AuthorizeUse(ctx); err == nil {
				t.Fatal("AuthorizeUse accepted participant")
			}
			if broker.callCount() != 0 {
				t.Fatal("refused participant reached broker")
			}
		})
	}
}
