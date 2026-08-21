package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/publication"
	"github.com/hurtener/Harbor/internal/state"
)

const servePublicationKEKEnv = "HARBOR_HA68_SERVE_TEST_KEK"

const servePublicationKEK = "0303030303030303030303030303030303030303030303030303030303030303"

func TestNewSkillPublicationStore_FailsClosedOnMissingWiring(t *testing.T) {
	if _, err := NewSkillPublicationStore(nil, "runtime", auth.NewAgentReachAuthorizer()); !errors.Is(err, ErrPublicationWiringMisconfigured) {
		t.Fatalf("nil StateStore error = %v, want ErrPublicationWiringMisconfigured", err)
	}

	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	for name, runtimeID := range map[string]string{"empty runtime id": "", "whitespace runtime id": "  "} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSkillPublicationStore(st, runtimeID, auth.NewAgentReachAuthorizer()); !errors.Is(err, ErrPublicationWiringMisconfigured) {
				t.Fatalf("runtime id %q error = %v, want ErrPublicationWiringMisconfigured", runtimeID, err)
			}
		})
	}
	if _, err := NewSkillPublicationStore(st, "runtime", nil); !errors.Is(err, ErrPublicationWiringMisconfigured) {
		t.Fatalf("nil Agent-reach authorizer error = %v, want ErrPublicationWiringMisconfigured", err)
	}
}

func TestBuildMux_PublicationWiring_MountsCapabilityAndSharesState(t *testing.T) {
	deps := buildProjWiringMux(t)
	id := identity.Identity{TenantID: "publication-tenant", UserID: "publication-user", SessionID: "publication-session"}
	reach := auth.NewAgentReachAuthorizer()
	runtimeID := publication.NewRuntimeID("serve-publication-wiring")
	store, err := NewSkillPublicationStore(deps.in.State, runtimeID, reach)
	if err != nil {
		t.Fatalf("NewSkillPublicationStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	in := deps.in
	in.AgentReach = reach
	in.PublicationStore = store
	in.PublicationRuntimeID = runtimeID
	built, err := BuildMux(in)
	if err != nil {
		t.Fatalf("BuildMux: %v", err)
	}

	adminCtx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	adminCtx = auth.WithScopes(adminCtx, []auth.Scope{auth.ScopeAdmin})
	caller := identity.Quadruple{Identity: id}
	meta, _, err := store.Publish(adminCtx, caller, publication.PublishRequest{
		IdempotencyKey: "serve-publication-1",
		Name:           "ops",
		Skill: skills.Skill{
			Name: "ops", Trigger: "when ops", Steps: []string{"run"},
			Origin: skills.OriginGenerated, Scope: skills.ScopeUser,
		},
		ExpectedAbsent: true,
	})
	if err != nil {
		t.Fatalf("publish through shared store: %v", err)
	}

	availableCode, availableBody := postMux(t, built.Mux, "/v1/control/skills.publications.available", id,
		`{"identity":{"tenant":"publication-tenant","user":"publication-user","session":"publication-session"}}`)
	if availableCode != http.StatusOK {
		t.Fatalf("skills.publications.available status = %d, body = %s", availableCode, availableBody)
	}
	var available types.SkillPublicationAvailableResponse
	if err := json.Unmarshal(availableBody, &available); err != nil {
		t.Fatalf("decode available response: %v; body=%s", err, availableBody)
	}
	if len(available.Publications) != 1 || available.Publications[0].PublicationID != meta.PublicationID {
		t.Fatalf("available publications = %+v, want shared publication %q", available.Publications, meta.PublicationID)
	}

	infoCode, infoBody := postMux(t, built.Mux, "/v1/control/runtime.info", id,
		`{"identity":{"tenant":"publication-tenant","user":"publication-user","session":"publication-session"}}`)
	if infoCode != http.StatusOK {
		t.Fatalf("runtime.info status = %d, body = %s", infoCode, infoBody)
	}
	var info types.RuntimeInfo
	if err := json.Unmarshal(infoBody, &info); err != nil {
		t.Fatalf("decode runtime.info: %v; body=%s", err, infoBody)
	}
	if !containsCapability(info.Capabilities, types.CapSkillPublications) {
		t.Fatalf("runtime.info capabilities = %v, missing %q", info.Capabilities, types.CapSkillPublications)
	}

	// The same authorized wrapper remains fail-closed for agent mutations when
	// the request carries no signed effective-Agent reach.
	installCode, installBody := postMux(t, built.Mux, "/v1/control/skills.publications.install", id, `{"identity":{"tenant":"publication-tenant","user":"publication-user","session":"publication-session"},"agent_id":"agent-1","publication_id":"`+meta.PublicationID+`","revision_id":"`+meta.RevisionID+`","expected_absent":true,"idempotency_key":"install-1"}`)
	if installCode != http.StatusForbidden {
		t.Fatalf("install without signed reach status = %d, body = %s; want 403", installCode, installBody)
	}

	allowedCtx := auth.WithAgentReach(adminCtx, []string{"agent-1"})
	if _, _, err := store.Install(allowedCtx, caller, publication.InstallRequest{
		IdempotencyKey: "install-1", AgentID: "agent-1", PublicationID: meta.PublicationID,
		RevisionID: meta.RevisionID, ExpectedAbsent: true,
	}); err != nil {
		t.Fatalf("install with signed reach through shared store: %v", err)
	}

	// The mux must reject a raw publication store when the shared reach gate is
	// absent instead of silently constructing a weaker surface.
	misconfigured := in
	misconfigured.AgentReach = nil
	if _, err := BuildMux(misconfigured); !errors.Is(err, ErrPublicationWiringMisconfigured) {
		t.Fatalf("BuildMux with publication store but nil reach = %v, want ErrPublicationWiringMisconfigured", err)
	}
}

func TestBuildMux_PublicationWiring_ConcurrentReads(t *testing.T) {
	deps := buildProjWiringMux(t)
	reach := auth.NewAgentReachAuthorizer()
	store, err := NewSkillPublicationStore(deps.in.State, publication.NewRuntimeID("serve-publication-race"), reach)
	if err != nil {
		t.Fatalf("NewSkillPublicationStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	id := identity.Identity{TenantID: "publication-race-tenant", UserID: "publication-race-user", SessionID: "publication-race-session"}
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	caller := identity.Quadruple{Identity: id}
	if _, _, err := store.Publish(auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin}), caller, publication.PublishRequest{
		IdempotencyKey: "race-publish", Name: "ops",
		Skill:          skills.Skill{Name: "ops", Trigger: "when ops", Steps: []string{"run"}, Origin: skills.OriginGenerated, Scope: skills.ScopeUser},
		ExpectedAbsent: true,
	}); err != nil {
		t.Fatalf("seed publication: %v", err)
	}

	const n = 128
	errs := make(chan error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			_, err := store.ListAvailable(ctx, caller)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent ListAvailable: %v", err)
		}
	}
}

func TestBoot_PublicationsConfigured_MountsSharedStoreAndCapability(t *testing.T) {
	t.Setenv(servePublicationKEKEnv, servePublicationKEK)
	signer := newTestSigner(t)
	opts := baseOptions(t)
	opts.ConfigPath = writeServePublicationConfig(t)
	opts.AuthValidatorFactory = signer.factory()
	h := bootTest(t, context.Background(), opts)
	id := identity.Identity{TenantID: "serve-tenant", UserID: "serve-user", SessionID: "serve-session"}
	token := signer.sign(t, id, []string{"admin"})

	code, body := servePublicationPOST(t, h.Handler(), token, "/v1/control/skills.publications.publish", `{"identity":{"tenant":"serve-tenant","user":"serve-user","session":"serve-session"},"name":"ops","skill":{"name":"ops","trigger":"when ops","steps":["run"]},"idempotency_key":"publish-1","expected_absent":true}`)
	if code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", code, body)
	}
	var published types.SkillPublicationPublishResponse
	if err := json.Unmarshal(body, &published); err != nil {
		t.Fatalf("decode publish response: %v; body=%s", err, body)
	}
	if published.Publication.PublicationID == "" || published.Publication.RuntimeID != publication.NewRuntimeID(opts.InstanceID) {
		t.Fatalf("published metadata = %+v, want immutable runtime id %q", published.Publication, publication.NewRuntimeID(opts.InstanceID))
	}

	code, body = servePublicationPOST(t, h.Handler(), token, "/v1/control/skills.publications.available", `{"identity":{"tenant":"serve-tenant","user":"serve-user","session":"serve-session"}}`)
	if code != http.StatusOK {
		t.Fatalf("available status = %d, body = %s", code, body)
	}
	var available types.SkillPublicationAvailableResponse
	if err := json.Unmarshal(body, &available); err != nil {
		t.Fatalf("decode available response: %v; body=%s", err, body)
	}
	if len(available.Publications) != 1 || available.Publications[0].PublicationID != published.Publication.PublicationID {
		t.Fatalf("available publications = %+v, want published id %q", available.Publications, published.Publication.PublicationID)
	}

	code, body = servePublicationPOST(t, h.Handler(), token, "/v1/control/runtime.info", `{"identity":{"tenant":"serve-tenant","user":"serve-user","session":"serve-session"}}`)
	if code != http.StatusOK {
		t.Fatalf("runtime.info status = %d, body = %s", code, body)
	}
	var info types.RuntimeInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("decode runtime.info: %v; body=%s", err, body)
	}
	if !containsCapability(info.Capabilities, types.CapSkillPublications) {
		t.Fatalf("runtime.info capabilities = %v, missing %q", info.Capabilities, types.CapSkillPublications)
	}

	// A valid bearer without the signed reach claim remains authenticated for
	// discovery but cannot install an exact reference to an Agent.
	code, body = servePublicationPOST(t, h.Handler(), token, "/v1/control/skills.publications.install", `{"identity":{"tenant":"serve-tenant","user":"serve-user","session":"serve-session"},"agent_id":"harbor-dev-agent","publication_id":"`+published.Publication.PublicationID+`","revision_id":"`+published.Publication.RevisionID+`","expected_absent":true,"idempotency_key":"install-1"}`)
	if code != http.StatusForbidden {
		t.Fatalf("install without signed reach status = %d, body = %s; want 403", code, body)
	}

	reachToken := signer.signWithAgentReach(t, id, []string{"admin"}, []string{"harbor-dev-agent"})
	code, body = servePublicationPOST(t, h.Handler(), reachToken, "/v1/control/skills.publications.install", `{"identity":{"tenant":"serve-tenant","user":"serve-user","session":"serve-session"},"agent_id":"harbor-dev-agent","publication_id":"`+published.Publication.PublicationID+`","revision_id":"`+published.Publication.RevisionID+`","expected_absent":true,"idempotency_key":"install-2"}`)
	if code != http.StatusOK {
		t.Fatalf("install with signed reach status = %d, body = %s", code, body)
	}
}

func TestBoot_PublicationsWithoutAdmissionAuthority_UnmountedAndUnadvertised(t *testing.T) {
	signer := newTestSigner(t)
	opts := baseOptions(t)
	opts.AuthValidatorFactory = signer.factory()
	h := bootTest(t, context.Background(), opts)
	id := identity.Identity{TenantID: "serve-no-publication-tenant", UserID: "serve-no-publication-user", SessionID: "serve-no-publication-session"}
	token := signer.sign(t, id, []string{"admin"})

	code, body := servePublicationPOST(t, h.Handler(), token, "/v1/control/skills.publications.available", `{"identity":{"tenant":"serve-no-publication-tenant","user":"serve-no-publication-user","session":"serve-no-publication-session"}}`)
	if code != http.StatusNotFound {
		t.Fatalf("publication route without admission authority status = %d, body = %s; want 404", code, body)
	}
	code, body = servePublicationPOST(t, h.Handler(), token, "/v1/control/runtime.info", `{"identity":{"tenant":"serve-no-publication-tenant","user":"serve-no-publication-user","session":"serve-no-publication-session"}}`)
	if code != http.StatusOK {
		t.Fatalf("runtime.info without admission authority status = %d, body = %s", code, body)
	}
	var info types.RuntimeInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("decode runtime.info: %v; body=%s", err, body)
	}
	if containsCapability(info.Capabilities, types.CapSkillPublications) {
		t.Fatalf("runtime.info advertised %q without a mounted publication store: %v", types.CapSkillPublications, info.Capabilities)
	}
}

func writeServePublicationConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "harbor.yaml")
	body := serveTestYAML + "\nskills:\n  driver: localdb\n  dsn: \":memory:\"\ntools:\n  oauth_token_kek_env: " + servePublicationKEKEnv + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write publication config: %v", err)
	}
	return p
}

func servePublicationPOST(t *testing.T, h http.Handler, token, path, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func containsCapability(caps []types.Capability, want types.Capability) bool {
	for _, cap := range caps {
		if cap == want {
			return true
		}
	}
	return false
}

func (s *testSigner) signWithAgentReach(t *testing.T, id identity.Identity, scopes, reach []string) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"sub": id.UserID, "exp": now.Add(time.Hour).Unix(), "nbf": now.Add(-time.Minute).Unix(), "iat": now.Unix(),
		"tenant": id.TenantID, "user": id.UserID, "session": id.SessionID, "scopes": scopes, "agent_reach": reach,
	})
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.priv)
	if err != nil {
		t.Fatalf("sign reach test token: %v", err)
	}
	return signed
}
