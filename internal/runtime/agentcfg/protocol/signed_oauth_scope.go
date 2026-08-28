package protocol

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
)

// signedOAuthMCPConfigScopeKey is private so callers cannot forge a scope by
// copying a public context key. The user lifecycle sets it only after it has
// authenticated the verified identity and agent reach.
type signedOAuthMCPConfigScopeKey struct{}

func withSignedOAuthMCPConfigScope(ctx context.Context, scope agentcfg.ConfigScope) context.Context {
	return context.WithValue(ctx, signedOAuthMCPConfigScopeKey{}, scope)
}

func signedOAuthMCPConfigScope(ctx context.Context) agentcfg.ConfigScope {
	if scope, ok := ctx.Value(signedOAuthMCPConfigScopeKey{}).(agentcfg.ConfigScope); ok {
		return scope
	}
	return agentcfg.ConfigScopeAgent
}

func requireUserSignedOAuthMCPAuthorization(ctx context.Context, id identity.Identity, agentID string) error {
	verified, ok := identity.FromVerified(ctx)
	if !ok || verified != id {
		return fmt.Errorf("%w: request identity is not the verified caller", ErrIdentityRequired)
	}
	if !auth.HasScope(ctx, auth.ScopeAgentConfigUser) {
		return fmt.Errorf("%w: missing %q", ErrSignedCapabilityUserAuthorization, auth.ScopeAgentConfigUser)
	}
	if err := auth.NewAgentReachAuthorizer().AuthorizeAgentReach(ctx, agentID); err != nil {
		return fmt.Errorf("%w: %w", ErrSignedCapabilityUserAuthorization, err)
	}
	return nil
}
