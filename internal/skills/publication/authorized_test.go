package publication

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/skills"
)

func authorizedIdentity() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}}
}

func authorizedContext(t *testing.T, id identity.Identity, reach []string, scopes ...auth.Scope) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx = auth.WithAgentReach(ctx, reach)
	return auth.WithScopes(ctx, scopes)
}

func TestAuthorizedStore_RequiresVerifiedCallerAdminAndSignedAgentReach(t *testing.T) {
	caller := authorizedIdentity()
	store, err := NewAuthorizedStore(NewMemoryStore("runtime-a"), auth.NewAgentReachAuthorizer(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := authorizedContext(t, caller.Identity, []string{"agent-a"}, auth.ScopeAgentConfigUser)
	if _, _, err := store.Publish(ctx, caller, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "do"), ExpectedAbsent: true}); !errors.Is(err, ErrAdminRequired) {
		t.Fatalf("ordinary user publish=%v want admin denial", err)
	}
	if _, _, err := store.Publish(context.Background(), caller, PublishRequest{IdempotencyKey: "p2", Name: "ops", Skill: publicationSkill("ops", "do"), ExpectedAbsent: true}); !errors.Is(err, ErrVerifiedIdentityRequired) {
		t.Fatalf("unverified publish=%v want verified identity denial", err)
	}

	adminCtx := authorizedContext(t, caller.Identity, []string{"agent-a"}, auth.ScopeAdmin, auth.ScopeAgentConfigUser)
	pub, _, err := store.Publish(adminCtx, caller, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "do"), ExpectedAbsent: true})
	if err != nil {
		t.Fatalf("admin publish: %v", err)
	}
	if _, _, err := store.Install(adminCtx, caller, InstallRequest{IdempotencyKey: "install-missing-reach", AgentID: "agent-b", PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true}); !errors.Is(err, ErrAgentReachDenied) {
		t.Fatalf("out-of-reach install=%v want denial", err)
	}
	if _, _, err := store.Install(adminCtx, caller, InstallRequest{IdempotencyKey: "install", AgentID: "agent-a", PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true}); err != nil {
		t.Fatalf("in-reach install: %v", err)
	}

	foreign := caller
	foreign.UserID = "user-b"
	if _, err := store.ListReferences(adminCtx, foreign); !errors.Is(err, ErrVerifiedIdentityRequired) {
		t.Fatalf("foreign list=%v want verified caller denial", err)
	}
	if _, _, err := store.Resolve(adminCtx, caller, "agent-b"); !errors.Is(err, ErrAgentReachDenied) {
		t.Fatalf("out-of-reach resolve=%v want denial", err)
	}
	if body, _, err := store.Resolve(adminCtx, caller, "agent-a"); err != nil || body.Name != "ops" {
		t.Fatalf("in-reach resolve body=%+v err=%v", body, err)
	}
}

func TestAuthorizedStore_ConcurrentResolveN128(t *testing.T) {
	caller := authorizedIdentity()
	ctx := authorizedContext(t, caller.Identity, []string{"agent-a"}, auth.ScopeAdmin)
	store, err := NewAuthorizedStore(NewMemoryStore("runtime-a"), auth.NewAgentReachAuthorizer(), nil)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := store.Publish(ctx, caller, PublishRequest{IdempotencyKey: "p", Name: "ops", Skill: publicationSkill("ops", "do"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Install(ctx, caller, InstallRequest{IdempotencyKey: "i", AgentID: "agent-a", PublicationID: pub.PublicationID, RevisionID: pub.RevisionID, ExpectedAbsent: true}); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 128)
	var wg sync.WaitGroup
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, meta, resolveErr := store.Resolve(ctx, caller, "agent-a")
			if resolveErr != nil {
				errCh <- resolveErr
				return
			}
			if body.ContentHash != meta.ContentHash || body.Origin != skills.OriginPack || body.Scope != skills.ScopeTenant {
				errCh <- ErrContentHashMismatch
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}
