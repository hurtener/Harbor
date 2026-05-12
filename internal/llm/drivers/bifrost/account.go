package bifrost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

// ErrMissingAPIKey — the operator configured `bifrost` but the key
// referenced by `LLMConfig.APIKey` is not present at construction
// time. Fail-closed at New (AGENTS.md §5 "fail loudly"); the error's
// message names the env var so the operator can fix it. NEVER
// includes the key value.
var ErrMissingAPIKey = errors.New("bifrost: API key missing")

// ErrInvalidProvider — `LLMConfig.Provider` is empty or is not a
// recognised bifrost provider. Fails at New time.
var ErrInvalidProvider = errors.New("bifrost: invalid provider")

// Account implements `bifrost/schemas.Account` for Harbor's
// single-provider, single-key V1 deployment. The struct holds the
// resolved API key value (read once at construction); subsequent
// `GetKeysForProvider` calls return the cached value.
//
// Concurrent-reuse: the struct is read-only after `newAccount` returns.
// Safe for N concurrent bifrost goroutines.
type Account struct {
	provider bfschemas.ModelProvider
	apiKey   string
	baseURL  string
	timeout  int // seconds; 0 → bifrost default
}

// newAccount resolves the configured provider + API key from the
// snapshot. The `APIKey` field can be either a literal key (e.g.
// `"sk-..."`) or an env reference (`"env.NAME"` per the bifrost
// convention, see bifrost's `EnvVar`). Missing env vars fail closed
// with `ErrMissingAPIKey` — Harbor's runtime principle is to fail at
// boot, not at the first user request.
func newAccount(cfg llm.ConfigSnapshot) (*Account, error) {
	if cfg.Provider == "" {
		return nil, fmt.Errorf("%w: LLMConfig.Provider is empty", ErrInvalidProvider)
	}
	provider := bfschemas.ModelProvider(cfg.Provider)
	if !isKnownProvider(provider) {
		return nil, fmt.Errorf("%w: %q (allowed: %s)",
			ErrInvalidProvider, cfg.Provider, knownProvidersHuman())
	}
	key, err := resolveAPIKey(cfg.APIKey)
	if err != nil {
		return nil, err
	}
	timeoutSeconds := 0
	if cfg.Timeout > 0 {
		timeoutSeconds = int(cfg.Timeout.Seconds())
	}
	return &Account{
		provider: provider,
		apiKey:   key,
		baseURL:  cfg.BaseURL,
		timeout:  timeoutSeconds,
	}, nil
}

// resolveAPIKey reads either a literal key or an `env.NAME` reference.
// Empty input → `ErrMissingAPIKey`. The error message names the env
// var when applicable so the operator can fix it; the key VALUE is
// never logged or surfaced.
func resolveAPIKey(input string) (string, error) {
	if input == "" {
		return "", fmt.Errorf("%w: LLMConfig.APIKey is empty", ErrMissingAPIKey)
	}
	if name, ok := strings.CutPrefix(input, "env."); ok {
		val := os.Getenv(name)
		if val == "" {
			return "", fmt.Errorf("%w: env var %q is unset (set the value in the deployment environment or change llm.api_key to a literal)",
				ErrMissingAPIKey, name)
		}
		return val, nil
	}
	return input, nil
}

// isKnownProvider checks `cfg.Provider` against bifrost's enumerated
// `StandardProviders`. Custom providers (operator-defined) are out of
// scope for V1; if an operator needs one they file an RFC PR.
func isKnownProvider(p bfschemas.ModelProvider) bool {
	for _, sp := range bfschemas.StandardProviders {
		if sp == p {
			return true
		}
	}
	return false
}

// knownProvidersHuman renders the standard providers as a
// comma-separated string for the error message. Sorted for stable
// output (bifrost's slice is already in a canonical order, but we
// don't rely on it).
func knownProvidersHuman() string {
	names := make([]string, 0, len(bfschemas.StandardProviders))
	for _, sp := range bfschemas.StandardProviders {
		names = append(names, string(sp))
	}
	return strings.Join(names, ",")
}

// GetConfiguredProviders implements `bfschemas.Account`. Harbor's V1
// deployment uses one provider per Harbor instance.
func (a *Account) GetConfiguredProviders() ([]bfschemas.ModelProvider, error) {
	return []bfschemas.ModelProvider{a.provider}, nil
}

// GetKeysForProvider implements `bfschemas.Account`. Returns the
// cached key; bifrost's request path is responsible for routing it.
// The `ctx` is unused at this layer — Harbor's identity is propagated
// separately (the safety pass enforces presence; the driver's
// `Complete` rejects on missing).
func (a *Account) GetKeysForProvider(ctx context.Context, providerKey bfschemas.ModelProvider) ([]bfschemas.Key, error) {
	if providerKey != a.provider {
		// Bifrost should never ask for a non-configured provider, but
		// if a plugin redirects routing, decline cleanly.
		return nil, fmt.Errorf("bifrost: provider %q is not configured for this Harbor account", providerKey)
	}
	return []bfschemas.Key{
		{
			ID:    "harbor-default",
			Name:  "harbor-default",
			Value: bfschemas.EnvVar{Val: a.apiKey},
			// Empty `Models` whitelist means "this key serves every
			// model bifrost routes to the configured provider." V1
			// runs one provider per Harbor instance; the WhiteList
			// is unnecessary friction.
			Weight: 1.0,
		},
	}, nil
}

// GetConfigForProvider implements `bfschemas.Account`. Returns the
// per-provider config (network + concurrency + buffer). Harbor's
// driver lets bifrost's defaults handle most of the surface; only
// `BaseURL` (when overridden) and `DefaultRequestTimeoutInSeconds`
// flow through from Harbor's config.
func (a *Account) GetConfigForProvider(providerKey bfschemas.ModelProvider) (*bfschemas.ProviderConfig, error) {
	cfg := &bfschemas.ProviderConfig{
		NetworkConfig: bfschemas.NetworkConfig{
			BaseURL:                        a.baseURL,
			DefaultRequestTimeoutInSeconds: a.timeout,
		},
	}
	return cfg, nil
}
