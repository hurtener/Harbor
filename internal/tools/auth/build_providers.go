// build_providers.go — the exported OAuth provider assembly (Phase
// the assembly work; absorbs cmd/harbor's applyToolCatalogWiring KEK-resolve
// → sealer → token store → provider-factory loop from).
//
// BuildProviders walks `cfg.OAuthProviders[]`, constructs the shared
// crypto chain ONCE (one operator-supplied KEK env var per binary →
// AES-256-GCM Sealer → StateStore-backed TokenStore), and dispatches
// each entry to the §4.4 driver registry by `Driver` name. Each entry's
// OWN client credential resolves through the §4.4 credential-source seam
// (`internal/tools/auth/credsource`): `env` (the default — resolved at
// boot from `ClientIDEnv` / `ClientSecretEnv`, fail-loud when unset) or
// `remote` (an authenticated pull from a coordinator endpoint at first
// need). BuildProviders constructs the source, calls its boot-time
// `ValidateAtBoot`, and threads it onto `ProviderConfig.CredentialSource`;
// the driver resolves the credential at its need-point (CLAUDE.md §7
// rule 2 — never hardcoded, never logged). The KEK is read from the env
// var named in `cfg.OAuthTokenKEKEnv` (32 hex bytes; the Sealer enforces
// length). Every failure is loud: empty / wrong-length KEK, an unset env
// credential, a malformed remote block, unknown driver / source, and
// factory errors all crash assembly with a wrapped error naming the
// offending field (CLAUDE.md §13 amendment).
package auth

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
)

// BuildDeps bundles the shared runtime collaborators BuildProviders
// threads into every provider factory. All four fields are mandatory
// when at least one provider is declared; an empty
// `cfg.OAuthProviders` list short-circuits before any is read.
type BuildDeps struct {
	// State is the runtime's StateStore — the TokenStore persists
	// sealed tokens through it.
	State state.StateStore
	// Bus is the shared event bus (auth events).
	Bus events.EventBus
	// Redactor is the shared audit redactor.
	Redactor audit.Redactor
	// Coordinator is the unified pause/resume primitive — the ONE
	// pause path tool-side OAuth rides on (CLAUDE.md §7 rule 4).
	Coordinator pauseresume.Coordinator
}

// BuildProviders constructs the OAuth provider map declared by
// `cfg.OAuthProviders`, keyed by provider Name. An empty declaration
// list returns an empty (non-nil) map and never touches the KEK env —
// the binary boots cleanly when no operator declares OAuth bindings.
func BuildProviders(ctx context.Context, cfg config.ToolsConfig, deps BuildDeps) (map[string]OAuthProvider, error) {
	providers := make(map[string]OAuthProvider, len(cfg.OAuthProviders))
	if len(cfg.OAuthProviders) == 0 {
		return providers, nil
	}
	kek, err := resolveTokenKEK(cfg.OAuthTokenKEKEnv)
	if err != nil {
		return nil, err
	}
	sealer, err := NewAESGCMSealer(kek)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: sealer: %w", err)
	}
	tokenStore, err := NewTokenStore(deps.State, sealer)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: token store: %w", err)
	}
	factoryDeps := FactoryDeps{
		Store:       tokenStore,
		Bus:         deps.Bus,
		Redactor:    deps.Redactor,
		Coordinator: deps.Coordinator,
	}
	for i, p := range cfg.OAuthProviders {
		src, err := buildCredentialSource(i, p, deps)
		if err != nil {
			return nil, err
		}
		// The env source resolves + fails loud here (today's boot
		// behavior); the remote source validates only the block shape and
		// defers the network pull to first use.
		if err := src.ValidateAtBoot(ctx); err != nil {
			return nil, err
		}
		pcfg := ProviderConfig{
			Name:             p.Name,
			CredentialSource: src,
			Scopes:           append([]string(nil), p.Scopes...),
			AuthURL:          p.AuthURL,
			TokenURL:         p.TokenURL,
			RedirectURL:      p.RedirectURL,
			Extra:            p.Extra,
		}
		prov, err := Resolve(ctx, p.Driver, pcfg, factoryDeps)
		if err != nil {
			return nil, fmt.Errorf("tools/oauth: provider %q (oauth_providers[%d], driver=%q): %w",
				p.Name, i, p.Driver, err)
		}
		providers[p.Name] = prov
	}
	return providers, nil
}

// buildCredentialSource constructs the §4.4 credential source declared by
// the provider entry (defaulting to `env`), mapping the operator YAML
// onto the neutral `credsource.Config` boundary. The remote source
// receives the shared bus + redactor so it can emit its SafePayload fetch
// events; the env source ignores them.
func buildCredentialSource(index int, p config.ToolOAuthProviderConfig, deps BuildDeps) (credsource.Source, error) {
	name := p.CredentialSource
	if name == "" {
		name = credsource.SourceEnv
	}
	scfg := credsource.Config{
		ProviderName:    p.Name,
		ProviderIndex:   index,
		ClientIDEnv:     p.ClientIDEnv,
		ClientSecretEnv: p.ClientSecretEnv,
		Clock:           time.Now,
		Bus:             deps.Bus,
		Redactor:        deps.Redactor,
	}
	if p.Remote != nil {
		scfg.Remote = &credsource.RemoteConfig{
			URL:          p.Remote.URL,
			AuthTokenEnv: p.Remote.AuthTokenEnv,
			CacheTTL:     p.Remote.CacheTTL,
			Timeout:      p.Remote.Timeout,
		}
	}
	src, err := credsource.Resolve(name, scfg)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: provider %q (oauth_providers[%d], credential_source=%q): %w",
			p.Name, index, name, err)
	}
	return src, nil
}

// resolveTokenKEK reads the named env var and decodes its value as a
// 32-byte hex-encoded key-encryption key for AES-256-GCM token
// encryption at rest. Fail-loud per the §13 amendment: empty env or
// wrong-length decoded key crashes assembly with a wrapped error
// naming the env var.
func resolveTokenKEK(envName string) ([]byte, error) {
	if envName == "" {
		return nil, fmt.Errorf("tools/oauth: tools.oauth_token_kek_env must be set (validated upstream — this is a sanity check)")
	}
	raw := os.Getenv(envName)
	if raw == "" {
		return nil, fmt.Errorf("tools/oauth: env var %q (named by tools.oauth_token_kek_env) is unset or empty — operator must populate a 32-byte hex-encoded KEK",
			envName)
	}
	kek, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: env var %q is not valid hex: %w", envName, err)
	}
	if len(kek) != KEKSizeBytes {
		return nil, fmt.Errorf("tools/oauth: env var %q decoded to %d bytes, want %d (AES-256-GCM)",
			envName, len(kek), KEKSizeBytes)
	}
	return kek, nil
}
