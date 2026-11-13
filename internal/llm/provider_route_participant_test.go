package llm

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

type participantRouteResolver struct{}

func (participantRouteResolver) SelectProviderRoute(context.Context, ProviderRouteRequest) (SelectedProviderRoute, error) {
	return SelectedProviderRoute{}, nil
}
func (participantRouteResolver) ResolveProviderRoute(context.Context, ProviderRouteRequest) (ResolvedProviderRoute, error) {
	return ResolvedProviderRoute{}, nil
}

// Review probe: in a participant run, which user does the provider-route
// resolver see?
func TestProviderRouteRequest_ParticipantActsAsWriter(t *testing.T) {
	owner := identity.Identity{TenantID: "t1", UserID: "owner", SessionID: "s1"}
	ctx, err := identity.WithRun(context.Background(), owner, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithParticipant(ctx, identity.Participant{UserID: "guest", Speaker: "Guest"})
	if err != nil {
		t.Fatal(err)
	}
	route := ProviderRoute{RouteID: "r", RouteGeneration: 1, ProviderConnectionID: "c", ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "m"}
	_, req, err := prepareProviderRouteRequest(ctx, ProviderRouteConfig{Resolver: participantRouteResolver{}}, TrustedProviderRouteContext{
		RuntimeID: "rt", Route: route, EffectiveAgentID: "a", TaskID: "run-1", Purpose: ProviderRoutePurposeRun,
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.UserID != "guest" || req.SessionOwner != "owner" || req.SessionID != "s1" {
		t.Errorf("provider-route request user=%q session_owner=%q, want the writer and the partition owner", req.UserID, req.SessionOwner)
	}
	if ProviderRouteSelectionKeyFor(req) == ProviderRouteSelectionKeyFor(ProviderRouteRequest{TenantID: req.TenantID, UserID: "guest", EffectiveAgentID: req.EffectiveAgentID,
		RouteID: req.RouteID, RouteGeneration: req.RouteGeneration, ProviderConnectionID: req.ProviderConnectionID, ProviderConnectionGeneration: req.ProviderConnectionGeneration,
		CredentialAssetGeneration: req.CredentialAssetGeneration, ModelSelector: req.ModelSelector, ModelProfileSupported: req.ModelProfileSupported, Purpose: req.Purpose}) {
		t.Error("a participant selection must not share a cache entry with the writer's own sessions")
	}
	// The partition owner in the same session: ordinary request.
	ownerCtx, _ := identity.WithRun(context.Background(), owner, "run-1")
	_, ownerReq, err := prepareProviderRouteRequest(ownerCtx, ProviderRouteConfig{Resolver: participantRouteResolver{}}, TrustedProviderRouteContext{
		RuntimeID: "rt", Route: route, EffectiveAgentID: "a", TaskID: "run-1", Purpose: ProviderRoutePurposeRun,
	})
	if err != nil || ownerReq.UserID != "owner" || ownerReq.SessionOwner != "" {
		t.Fatalf("owner request = %+v, %v", ownerReq, err)
	}
}
