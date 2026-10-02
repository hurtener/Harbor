package protocol_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	"github.com/hurtener/Harbor/internal/skills/publication"
)

func TestMethodReach_DirectDispatchRejectsUnlistedCanonicalFamilies(t *testing.T) {
	id := testRun("").Identity
	wireID := types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
	ctx := auth.WithAgentReach(authCtx(t, id, auth.ScopeAdmin), []string{"source"})
	ctx = auth.WithMethodReach(ctx, []methods.Method{methods.MethodEventsSubscribe})

	posture := newPostureFixture(t)
	mcp, _ := newMCPSurface(t)
	registry, err := search.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	searchSurface, err := protocol.NewSearchSurface(registry, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	port := &recordingAgentPacksPort{}
	packs, err := protocol.NewAgentPacksSurface(protocol.AgentPacksDeps{Port: port, AgentResolver: agentPacksTestResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	store := publication.NewMemoryStore("method-reach-runtime")
	publications, err := protocol.NewSkillPublicationsSurface(protocol.SkillPublicationsDeps{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"posture", func() error {
			_, err := posture.Dispatch(ctx, methods.MethodRuntimeInfo, &types.RuntimeInfoRequest{Identity: wireID})
			return err
		}},
		{"search", func() error {
			_, err := searchSurface.Dispatch(ctx, methods.MethodSearchQuery, &types.SearchRequest{})
			return err
		}},
		{"mcp", func() error {
			_, err := mcp.Dispatch(ctx, methods.MethodMCPServersList, &types.MCPServersListRequest{Identity: wireID})
			return err
		}},
		{"agent packs", func() error {
			_, err := packs.Dispatch(ctx, methods.MethodAgentConfigAgentPacksInspect, &types.AgentConfigAgentPacksInspectRequest{Identity: wireID, AgentID: "source"})
			return err
		}},
		{"skill publications", func() error {
			_, err := publications.Dispatch(ctx, methods.MethodSkillsPublicationsPublish, &types.SkillPublicationPublishRequest{
				Identity: wireID, Name: "must-not-publish", Skill: publicationWireSkill(),
				IdempotencyKey: "reader-publication-denied", ExpectedAbsent: true,
			})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := codeOf(t, tc.call()); code != protoerrors.CodeScopeMismatch {
				t.Fatalf("unlisted canonical dispatch = %s", code)
			}
		})
	}
	if port.inspectCalls != 0 || port.copyCalls != 0 {
		t.Fatal("reader invoked unlisted agent pack operation")
	}
	published, err := store.List(ctx, testRun(""))
	if err != nil || len(published) != 0 {
		t.Fatalf("reader created publications: %#v, %v", published, err)
	}
}
