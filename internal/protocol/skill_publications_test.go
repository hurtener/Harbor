package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills/publication"
)

func publicationTestContext(t *testing.T, tenant, user, session string, scopes []auth.Scope, reach []string) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{
		TenantID: tenant, UserID: user, SessionID: session,
	})
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	ctx = auth.WithScopes(ctx, scopes)
	return auth.WithAgentReach(ctx, reach)
}

func publicationWireSkill() types.SkillPublicationSkill {
	return types.SkillPublicationSkill{
		Name:    "ops",
		Trigger: "when ops",
		Steps:   []string{"run ops"},
	}
}

func TestSkillPublicationsSurface_BodyIdentityCannotGrantAdminAuthority(t *testing.T) {
	store := publication.NewMemoryStore("runtime-a")
	surface, err := protocol.NewSkillPublicationsSurface(protocol.SkillPublicationsDeps{Store: store})
	if err != nil {
		t.Fatalf("NewSkillPublicationsSurface: %v", err)
	}
	ctx := publicationTestContext(t, "tenant-a", "user-a", "session-a", []auth.Scope{auth.ScopeAdmin}, nil)
	_, err = surface.Dispatch(ctx, methods.MethodSkillsPublicationsPublish, &types.SkillPublicationPublishRequest{
		Identity:       types.IdentityScope{Tenant: "tenant-b", User: "attacker", Session: "session-b"},
		Name:           "ops",
		Skill:          publicationWireSkill(),
		IdempotencyKey: "publish-forged",
		ExpectedAbsent: true,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) || perr.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("forged body identity error = %v, want CodeScopeMismatch", err)
	}
	items, listErr := store.List(context.Background(), identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}})
	if listErr != nil {
		t.Fatalf("store.List: %v", listErr)
	}
	if len(items) != 0 {
		t.Fatalf("forged body identity created %d publications", len(items))
	}
}

func TestSkillPublicationsSurface_AdminPublishAndSignedAgentReachInstall(t *testing.T) {
	store := publication.NewMemoryStore("runtime-a")
	surface, err := protocol.NewSkillPublicationsSurface(protocol.SkillPublicationsDeps{Store: store})
	if err != nil {
		t.Fatalf("NewSkillPublicationsSurface: %v", err)
	}
	adminCtx := publicationTestContext(t, "tenant-a", "admin", "session-admin", []auth.Scope{auth.ScopeAdmin}, nil)
	publishResp, err := surface.Dispatch(adminCtx, methods.MethodSkillsPublicationsPublish, &types.SkillPublicationPublishRequest{
		Identity:       types.IdentityScope{Tenant: "tenant-a", User: "admin", Session: "session-admin"},
		Name:           "ops",
		Skill:          publicationWireSkill(),
		IdempotencyKey: "publish-1",
		ExpectedAbsent: true,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	published, ok := publishResp.(*types.SkillPublicationPublishResponse)
	if !ok || published.Publication.PublicationID == "" || published.Publication.ContentHash == "" {
		t.Fatalf("publish response = %#v", publishResp)
	}

	userCtx := publicationTestContext(t, "tenant-a", "user-a", "session-a", nil, []string{"agent-a"})
	installResp, err := surface.Dispatch(userCtx, methods.MethodSkillsPublicationsInstall, &types.SkillPublicationInstallRequest{
		Identity:       types.IdentityScope{Tenant: "tenant-a", User: "user-a", Session: "session-a"},
		AgentID:        "agent-a",
		PublicationID:  published.Publication.PublicationID,
		RevisionID:     published.Publication.RevisionID,
		ExpectedAbsent: true,
		IdempotencyKey: "install-1",
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	installed, ok := installResp.(*types.SkillPublicationInstallResponse)
	if !ok || installed.Reference.AgentID != "agent-a" || installed.Reference.RevisionID != published.Publication.RevisionID {
		t.Fatalf("install response = %#v", installResp)
	}

	deniedCtx := publicationTestContext(t, "tenant-a", "user-a", "session-b", nil, []string{"agent-other"})
	_, err = surface.Dispatch(deniedCtx, methods.MethodSkillsPublicationsInstall, &types.SkillPublicationInstallRequest{
		Identity:       types.IdentityScope{Tenant: "tenant-a", User: "user-a", Session: "session-b"},
		AgentID:        "agent-a",
		PublicationID:  published.Publication.PublicationID,
		RevisionID:     published.Publication.RevisionID,
		ExpectedAbsent: true,
		IdempotencyKey: "install-denied",
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) || perr.Code != protoerrors.CodeIdentityScopeRequired {
		t.Fatalf("unsigned/unreachable Agent install error = %v, want CodeIdentityScopeRequired", err)
	}
}
