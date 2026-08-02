package serve

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// SignedOAuthMCPCapabilityAuthoritiesFromConfig constructs D-401's fixed
// broker trust anchors at boot. A configured anchor fetches/parses its JWKS
// synchronously, so a bad issuer/key source cannot leave the registration
// surface half-enabled. Brokers without the explicit opt-in are deliberately
// absent from the returned map.
func SignedOAuthMCPCapabilityAuthoritiesFromConfig(ctx context.Context, cfg *config.Config, logger *slog.Logger) (map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority, error) {
	if cfg == nil {
		return nil, nil
	}
	authorities := make(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority)
	for _, broker := range cfg.Tools.OAuthCredentialBrokers {
		anchor := broker.SignedOAuthMCPCapabilityAuthority
		if anchor == nil {
			continue
		}
		// Config.Validate owns the precise field diagnostics. This is a
		// defensive construction boundary for embedders that pass a Config
		// directly instead of the normal loader path.
		if !anchor.Enabled || anchor.Issuer == "" || anchor.MaxAuthorityLifetime <= 0 {
			return nil, fmt.Errorf("signed oauth mcp capability authority for broker %q is incomplete", broker.Name)
		}
		keyOpts := []auth.JWKSOption{}
		if logger != nil {
			keyOpts = append(keyOpts, auth.WithJWKSLogger(logger))
		}
		keys, err := auth.NewJWKSKeySet(ctx, auth.JWKSSource{URL: anchor.JWKSURL, File: anchor.JWKSFile}, nil, keyOpts...)
		if err != nil {
			return nil, fmt.Errorf("build signed oauth mcp capability authority for broker %q: %w", broker.Name, err)
		}
		authorities[broker.Name] = agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
			Broker:               broker.Name,
			Issuer:               anchor.Issuer,
			Keys:                 keys,
			ScopeCeiling:         append([]string(nil), broker.ScopeCeiling...),
			MaxAuthorityLifetime: anchor.MaxAuthorityLifetime,
		}
	}
	return authorities, nil
}
