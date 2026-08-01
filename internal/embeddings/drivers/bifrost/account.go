package bifrost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/embeddings"
)

// ErrMissingAPIKey — the operator configured the embeddings block
// but the key referenced by `embeddings.api_key` is not present at
// construction time. Fail-closed at New (AGENTS.md §5 "fail
// loudly"); the error names the env var so the operator can fix it.
// NEVER includes the key value.
var ErrMissingAPIKey = errors.New("embeddings/bifrost: API key missing")

// ErrInvalidProvider — `embeddings.provider` is empty or not a known
// gateway provider. Fails at New time.
var ErrInvalidProvider = errors.New("embeddings/bifrost: invalid provider")

// ErrMissingModel — `embeddings.model` is empty. The embedding model
// is its own operator choice, configured separately from the chat
// model; there is no implicit default model.
var ErrMissingModel = errors.New("embeddings/bifrost: model missing")

// account implements `bfschemas.Account` for the embeddings driver's
// single-provider deployment: one provider, one resolved key, the
// model allowlist wide open (the configured model is enforced by the
// driver, not by key scoping).
//
// Concurrent reuse: read-only after `newAccount` returns.
type account struct {
	provider bfschemas.ModelProvider
	apiKey   string
	config   *bfschemas.ProviderConfig
}

// newAccount resolves the configured provider + API key from the
// snapshot. `cfg.APIKey` carries either a literal key or an
// `env.NAME` reference (the same convention the chat client uses).
// Missing keys fail closed with `ErrMissingAPIKey`.
func newAccount(cfg embeddings.ConfigSnapshot) (*account, error) {
	if cfg.Provider == "" {
		return nil, fmt.Errorf("%w: embeddings provider is empty (set embeddings.provider)", ErrInvalidProvider)
	}
	provider := bfschemas.ModelProvider(cfg.Provider)
	if !isKnownProvider(provider) {
		return nil, fmt.Errorf("%w: %q (allowed: %s)", ErrInvalidProvider, cfg.Provider, knownProvidersHuman())
	}
	key, err := resolveAPIKey(cfg.APIKey)
	if err != nil {
		return nil, err
	}
	pc := &bfschemas.ProviderConfig{
		NetworkConfig: bfschemas.NetworkConfig{
			BaseURL: cfg.BaseURL,
		},
	}
	if cfg.Timeout > 0 {
		pc.NetworkConfig.DefaultRequestTimeoutInSeconds = int(cfg.Timeout.Seconds())
	}
	return &account{
		provider: provider,
		apiKey:   key,
		config:   pc,
	}, nil
}

// resolveAPIKey reads either a literal key or an `env.NAME`
// reference. Empty input → `ErrMissingAPIKey`. The error message
// names the env var when applicable; the key VALUE is never logged
// or surfaced.
func resolveAPIKey(input string) (string, error) {
	if input == "" {
		return "", fmt.Errorf("%w: embeddings api_key is empty (set embeddings.api_key)", ErrMissingAPIKey)
	}
	if name, ok := strings.CutPrefix(input, "env."); ok {
		val := os.Getenv(name)
		if val == "" {
			return "", fmt.Errorf("%w: env var %q is unset (set the value in the deployment environment or change embeddings.api_key to a literal)",
				ErrMissingAPIKey, name)
		}
		return val, nil
	}
	return input, nil
}

// isKnownProvider checks the provider against the gateway's
// enumerated standard providers.
func isKnownProvider(p bfschemas.ModelProvider) bool {
	for _, sp := range bfschemas.StandardProviders {
		if sp == p {
			return true
		}
	}
	return false
}

// knownProvidersHuman renders the standard providers as a
// comma-separated string for the error message.
func knownProvidersHuman() string {
	names := make([]string, 0, len(bfschemas.StandardProviders))
	for _, sp := range bfschemas.StandardProviders {
		names = append(names, string(sp))
	}
	return strings.Join(names, ",")
}

// GetConfiguredProviders implements `bfschemas.Account`. Returns the
// single configured embedding provider.
func (a *account) GetConfiguredProviders() ([]bfschemas.ModelProvider, error) {
	return []bfschemas.ModelProvider{a.provider}, nil
}

// GetKeysForProvider implements `bfschemas.Account`. Returns the
// resolved key with the gateway's "all non-blacklisted models"
// wildcard — the configured model is pinned by the driver per-call,
// not by key scoping.
func (a *account) GetKeysForProvider(_ context.Context, providerKey bfschemas.ModelProvider) ([]bfschemas.Key, error) {
	if providerKey != a.provider {
		return nil, fmt.Errorf("embeddings/bifrost: provider %q is not configured for this account", providerKey)
	}
	return []bfschemas.Key{
		{
			ID:     "harbor-embeddings",
			Name:   "harbor-embeddings",
			Value:  bfschemas.SecretVar{Val: a.apiKey},
			Models: []string{"*"},
			Weight: 1.0,
		},
	}, nil
}

// GetConfigForProvider implements `bfschemas.Account`.
func (a *account) GetConfigForProvider(providerKey bfschemas.ModelProvider) (*bfschemas.ProviderConfig, error) {
	if providerKey != a.provider {
		return nil, fmt.Errorf("embeddings/bifrost: provider %q is not configured for this account", providerKey)
	}
	return a.config, nil
}
