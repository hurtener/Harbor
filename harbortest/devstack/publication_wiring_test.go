package devstack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/publication"
)

func TestAssemble_PublicationWiring_ConfiguredKekMountsSharedStateAndCapability(t *testing.T) {
	t.Setenv(v128KEKEnv, v128DummyKEKHex)
	cfg := devstackV128Config(t, func(c *config.Config) {
		c.Tools.OAuthTokenKEKEnv = v128KEKEnv
		c.Skills = config.SkillsConfig{Driver: "localdb", DSN: filepath.Join(t.TempDir(), "skills.sqlite")}
	})
	stack, err := TryAssemble(cfg, AssembleOpts{})
	if err != nil {
		if stack != nil {
			stack.Close()
		}
		t.Fatalf("TryAssemble: %v", err)
	}
	defer stack.Close()
	if stack.PublicationStore == nil {
		t.Fatal("configured restart-stable KEK did not mount the publication store")
	}
	if want := publication.NewRuntimeID("harbor-devstack"); stack.PublicationRuntimeID != want {
		t.Fatalf("publication runtime/deployment ID = %q, want immutable %q", stack.PublicationRuntimeID, want)
	}

	id := identity.Identity{TenantID: DefaultDevTenant, UserID: DefaultDevUser, SessionID: DefaultDevSession}
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	adminCtx := auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin})
	caller := identity.Quadruple{Identity: id}
	meta, _, err := stack.PublicationStore.Publish(adminCtx, caller, publication.PublishRequest{
		IdempotencyKey: "devstack-publication-1", Name: "ops",
		Skill:          skills.Skill{Name: "ops", Trigger: "when ops", Steps: []string{"run"}, Origin: skills.OriginGenerated, Scope: skills.ScopeUser},
		ExpectedAbsent: true,
	})
	if err != nil {
		t.Fatalf("publish through shared StateStore-backed store: %v", err)
	}

	code, body := devstackPublicationPOST(t, stack.Handler, stack.Token, "/v1/control/skills.publications.available")
	if code != http.StatusOK {
		t.Fatalf("available status = %d, body = %s", code, body)
	}
	var available types.SkillPublicationAvailableResponse
	if err := json.Unmarshal(body, &available); err != nil {
		t.Fatalf("decode available response: %v; body=%s", err, body)
	}
	if len(available.Publications) != 1 || available.Publications[0].PublicationID != meta.PublicationID {
		t.Fatalf("available publications = %+v, want shared publication %q", available.Publications, meta.PublicationID)
	}

	code, body = devstackPublicationPOST(t, stack.Handler, stack.Token, "/v1/control/runtime.info")
	if code != http.StatusOK {
		t.Fatalf("runtime.info status = %d, body = %s", code, body)
	}
	var info types.RuntimeInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("decode runtime.info: %v; body=%s", err, body)
	}
	if !devstackHasCapability(info.Capabilities, types.CapSkillPublications) {
		t.Fatalf("runtime.info capabilities = %v, missing %q", info.Capabilities, types.CapSkillPublications)
	}

	if _, _, err := stack.PublicationStore.Install(ctx, caller, publication.InstallRequest{
		IdempotencyKey: "devstack-install-denied", AgentID: "harbor-dev-agent", PublicationID: meta.PublicationID,
		RevisionID: meta.RevisionID, ExpectedAbsent: true,
	}); !errors.Is(err, publication.ErrAgentReachDenied) {
		t.Fatalf("install without signed Agent reach error = %v, want ErrAgentReachDenied", err)
	}
	if _, _, err := stack.PublicationStore.Install(auth.WithAgentReach(ctx, []string{"harbor-dev-agent"}), caller, publication.InstallRequest{
		IdempotencyKey: "devstack-install-allowed", AgentID: "harbor-dev-agent", PublicationID: meta.PublicationID,
		RevisionID: meta.RevisionID, ExpectedAbsent: true,
	}); err != nil {
		t.Fatalf("install with signed Agent reach: %v", err)
	}

	const n = 128
	errs := make(chan error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			_, err := stack.PublicationStore.ListAvailable(ctx, caller)
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

func TestAssemble_PublicationWiring_WithoutAdmissionAuthorityStaysUnavailable(t *testing.T) {
	cfg := devstackV128Config(t, nil)
	stack, err := TryAssemble(cfg, AssembleOpts{})
	if err != nil {
		if stack != nil {
			stack.Close()
		}
		t.Fatalf("TryAssemble: %v", err)
	}
	defer stack.Close()
	if stack.PublicationStore != nil || stack.PublicationRuntimeID != "" {
		t.Fatalf("publication wiring without a restart-stable admission authority = (%v, %q), want (nil, empty)", stack.PublicationStore, stack.PublicationRuntimeID)
	}

	code, body := devstackPublicationPOST(t, stack.Handler, stack.Token, "/v1/control/skills.publications.available")
	if code != http.StatusNotFound {
		t.Fatalf("publication route without admission authority status = %d, body = %s; want 404", code, body)
	}
	code, body = devstackPublicationPOST(t, stack.Handler, stack.Token, "/v1/control/runtime.info")
	if code != http.StatusOK {
		t.Fatalf("runtime.info without admission authority status = %d, body = %s", code, body)
	}
	var info types.RuntimeInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("decode runtime.info: %v; body=%s", err, body)
	}
	if devstackHasCapability(info.Capabilities, types.CapSkillPublications) {
		t.Fatalf("runtime.info advertised %q without mounted publication store: %v", types.CapSkillPublications, info.Capabilities)
	}
}

func devstackPublicationPOST(t *testing.T, h http.Handler, token, path string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"identity":{"tenant":"dev","user":"dev","session":"dev"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func devstackHasCapability(caps []types.Capability, want types.Capability) bool {
	for _, cap := range caps {
		if cap == want {
			return true
		}
	}
	return false
}
