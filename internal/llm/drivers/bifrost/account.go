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
// referenced by `LLMConfig.APIKey` (native primary) or by a custom
// provider's `APIKeyEnvVar` is not present at construction time.
// Fail-closed at New (AGENTS.md §5 "fail loudly"); the error's message
// names the env var so the operator can fix it. NEVER includes the
// key value.
var ErrMissingAPIKey = errors.New("bifrost: API key missing")

// ErrInvalidProvider — `LLMConfig.Provider` is empty or matches
// neither a native bifrost provider nor a declared custom-provider
// `Name`. Fails at New time.
var ErrInvalidProvider = errors.New("bifrost: invalid provider")

// ErrInvalidCustomProvider — `LLMConfig.CustomProviders` contains an
// invalid entry (empty name, empty base URL, empty env var, empty
// model list). The config-layer validator catches these at boot;
// this sentinel guards programmatic snapshot construction (tests,
// future Protocol-driven setters).
var ErrInvalidCustomProvider = errors.New("bifrost: invalid custom provider")

// Account implements `bifrost/schemas.Account` for Harbor's
// single-primary, optional-custom-providers deployment (Phase 33a).
//
// Concurrent-reuse: the struct is read-only after `newAccount`
// returns. Safe for N concurrent bifrost goroutines.
type Account struct {
	primaryConfig *bfschemas.ProviderConfig
	customByName  map[string]customRuntime
	provider      bfschemas.ModelProvider
	apiKey        string
	primaryModels []string
}

// customRuntime is the resolved-at-construction shape of one custom
// provider. Holds the API-key value (resolved from env) and the
// pre-computed `ProviderConfig`. Internal; never serialized.
type customRuntime struct {
	config *bfschemas.ProviderConfig
	apiKey string
	models []string
}

// newAccount resolves the configured provider + API key from the
// snapshot. The primary may be either a native bifrost provider (in
// which case `cfg.APIKey` carries the literal-or-env reference and
// the legacy `cfg.Timeout` / `cfg.BaseURL` apply) OR a declared
// custom-provider entry (in which case the entry's `APIKeyEnvVar` /
// network knobs / `Models` apply and the legacy fields are ignored).
// Missing keys fail closed with `ErrMissingAPIKey`.
func newAccount(cfg llm.ConfigSnapshot) (*Account, error) {
	if cfg.Provider == "" {
		return nil, fmt.Errorf("%w: LLMConfig.Provider is empty", ErrInvalidProvider)
	}

	// Build the custom-provider runtime table first so the primary
	// resolution can consult it. Each entry's API key is resolved
	// at this point — if the key is unset for ANY declared custom
	// provider, we fail closed at boot rather than at first request.
	customByName := make(map[string]customRuntime, len(cfg.CustomProviders))
	for _, spec := range cfg.CustomProviders {
		if spec.Name == "" || spec.BaseURL == "" || spec.APIKeyEnvVar == "" || len(spec.Models) == 0 {
			return nil, fmt.Errorf("%w: name=%q base_url=%q api_key_env_var=%q models=%d",
				ErrInvalidCustomProvider, spec.Name, spec.BaseURL, spec.APIKeyEnvVar, len(spec.Models))
		}
		key, err := resolveCustomAPIKey(spec.APIKeyEnvVar)
		if err != nil {
			return nil, err
		}
		runtimeCfg := buildCustomProviderConfig(spec, cfg.NetworkDefaults)
		customByName[spec.Name] = customRuntime{
			apiKey: key,
			models: append([]string(nil), spec.Models...),
			config: runtimeCfg,
		}
	}

	provider := bfschemas.ModelProvider(cfg.Provider)

	// Primary is either a custom-provider name or a native bifrost
	// name. Decide based on whether the name appears in the custom
	// table.
	if entry, ok := customByName[cfg.Provider]; ok {
		return &Account{
			provider:      provider,
			apiKey:        entry.apiKey,
			primaryModels: entry.models,
			primaryConfig: entry.config,
			customByName:  customByName,
		}, nil
	}
	if !isKnownProvider(provider) {
		return nil, fmt.Errorf("%w: %q (allowed native: %s; declared custom: %s)",
			ErrInvalidProvider, cfg.Provider, knownProvidersHuman(), customNamesHuman(customByName))
	}
	key, err := resolveAPIKey(cfg.APIKey)
	if err != nil {
		return nil, err
	}
	nativeCfg := buildNativeProviderConfig(cfg)
	return &Account{
		provider:      provider,
		apiKey:        key,
		primaryModels: []string{"*"}, // bifrost's "all non-blacklisted" wildcard; see Account.primaryModels godoc
		primaryConfig: nativeCfg,
		customByName:  customByName,
	}, nil
}

// resolveAPIKey reads either a literal key or an `env.NAME` reference.
// Empty input → `ErrMissingAPIKey`. The error message names the env
// var when applicable so the operator can fix it; the key VALUE is
// never logged or surfaced. Native-primary path only — custom
// providers use `resolveCustomAPIKey` because their config field is
// the raw env-var name (without the `env.` prefix).
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

// resolveCustomAPIKey reads the value of the operator-declared env
// var. Custom-provider config writes the env NAME directly (no
// `env.` prefix) so the indirection is one step shorter than the
// native path. Missing env var → `ErrMissingAPIKey` with the env var
// named.
func resolveCustomAPIKey(envVar string) (string, error) {
	if envVar == "" {
		return "", fmt.Errorf("%w: custom provider api_key_env_var is empty", ErrMissingAPIKey)
	}
	val := os.Getenv(envVar)
	if val == "" {
		return "", fmt.Errorf("%w: env var %q is unset for custom provider", ErrMissingAPIKey, envVar)
	}
	return val, nil
}

// isKnownProvider checks `cfg.Provider` against bifrost's enumerated
// `StandardProviders`. Custom providers are looked up separately in
// `newAccount`.
func isKnownProvider(p bfschemas.ModelProvider) bool {
	for _, sp := range bfschemas.StandardProviders {
		if sp == p {
			return true
		}
	}
	return false
}

// knownProvidersHuman renders the standard providers as a
// comma-separated string for the error message. Stable order
// matches bifrost's slice.
func knownProvidersHuman() string {
	names := make([]string, 0, len(bfschemas.StandardProviders))
	for _, sp := range bfschemas.StandardProviders {
		names = append(names, string(sp))
	}
	return strings.Join(names, ",")
}

// customNamesHuman renders the declared custom-provider names. Empty
// table → "(none)" so error messages stay readable.
func customNamesHuman(table map[string]customRuntime) string {
	if len(table) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(table))
	for n := range table {
		names = append(names, n)
	}
	// Stable order — tiny manual sort.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return strings.Join(names, ",")
}

// buildNativeProviderConfig builds the `*ProviderConfig` for the
// native primary path. Honours `cfg.NetworkDefaults` for fields the
// legacy single-provider config left zero. Zero-valued fields fall
// through to bifrost's `CheckAndSetDefaults`.
func buildNativeProviderConfig(cfg llm.ConfigSnapshot) *bfschemas.ProviderConfig {
	out := &bfschemas.ProviderConfig{
		NetworkConfig: bfschemas.NetworkConfig{
			BaseURL: cfg.BaseURL,
		},
		ConcurrencyAndBufferSize: bfschemas.ConcurrencyAndBufferSize{
			Concurrency: cfg.NetworkDefaults.Concurrency,
			BufferSize:  cfg.NetworkDefaults.BufferSize,
		},
	}
	// Per-request timeout precedence: legacy LLMConfig.Timeout >
	// NetworkDefaults.Timeout > bifrost's package-level default.
	if cfg.Timeout > 0 {
		out.NetworkConfig.DefaultRequestTimeoutInSeconds = int(cfg.Timeout.Seconds())
	} else if cfg.NetworkDefaults.Timeout > 0 {
		out.NetworkConfig.DefaultRequestTimeoutInSeconds = int(cfg.NetworkDefaults.Timeout.Seconds())
	}
	if cfg.NetworkDefaults.MaxRetries > 0 {
		out.NetworkConfig.MaxRetries = cfg.NetworkDefaults.MaxRetries
	}
	if cfg.NetworkDefaults.RetryBackoffInitial > 0 {
		out.NetworkConfig.RetryBackoffInitial = cfg.NetworkDefaults.RetryBackoffInitial
	}
	if cfg.NetworkDefaults.RetryBackoffMax > 0 {
		out.NetworkConfig.RetryBackoffMax = cfg.NetworkDefaults.RetryBackoffMax
	}
	return out
}

// buildCustomProviderConfig builds the `*ProviderConfig` for a custom
// provider. Per-provider fields override `NetworkDefaults`; both
// override bifrost's package-level defaults. `CustomProviderConfig`
// is populated with `BaseProviderType = schemas.OpenAI` (Phase 33a
// only supports OpenAI-compatible).
func buildCustomProviderConfig(spec llm.CustomProviderSpec, nd llm.NetworkDefaults) *bfschemas.ProviderConfig {
	out := &bfschemas.ProviderConfig{
		NetworkConfig: bfschemas.NetworkConfig{
			BaseURL: spec.BaseURL,
		},
		CustomProviderConfig: &bfschemas.CustomProviderConfig{
			BaseProviderType: bfschemas.OpenAI,
		},
	}
	if len(spec.RequestPathOverrides) > 0 {
		overrides := make(map[bfschemas.RequestType]string, len(spec.RequestPathOverrides))
		for k, v := range spec.RequestPathOverrides {
			overrides[bfschemas.RequestType(k)] = v
		}
		out.CustomProviderConfig.RequestPathOverrides = overrides
	}
	// Per-provider network config with NetworkDefaults fallthrough.
	switch {
	case spec.Timeout > 0:
		out.NetworkConfig.DefaultRequestTimeoutInSeconds = int(spec.Timeout.Seconds())
	case nd.Timeout > 0:
		out.NetworkConfig.DefaultRequestTimeoutInSeconds = int(nd.Timeout.Seconds())
	}
	switch {
	case spec.MaxRetries > 0:
		out.NetworkConfig.MaxRetries = spec.MaxRetries
	case nd.MaxRetries > 0:
		out.NetworkConfig.MaxRetries = nd.MaxRetries
	}
	switch {
	case spec.RetryBackoffInitial > 0:
		out.NetworkConfig.RetryBackoffInitial = spec.RetryBackoffInitial
	case nd.RetryBackoffInitial > 0:
		out.NetworkConfig.RetryBackoffInitial = nd.RetryBackoffInitial
	}
	switch {
	case spec.RetryBackoffMax > 0:
		out.NetworkConfig.RetryBackoffMax = spec.RetryBackoffMax
	case nd.RetryBackoffMax > 0:
		out.NetworkConfig.RetryBackoffMax = nd.RetryBackoffMax
	}
	switch {
	case spec.Concurrency > 0:
		out.ConcurrencyAndBufferSize.Concurrency = spec.Concurrency
	case nd.Concurrency > 0:
		out.ConcurrencyAndBufferSize.Concurrency = nd.Concurrency
	}
	switch {
	case spec.BufferSize > 0:
		out.ConcurrencyAndBufferSize.BufferSize = spec.BufferSize
	case nd.BufferSize > 0:
		out.ConcurrencyAndBufferSize.BufferSize = nd.BufferSize
	}
	return out
}

// GetConfiguredProviders implements `bfschemas.Account`. Returns the
// single configured PRIMARY provider — Phase 33a preserves D-040's
// "single-provider per Harbor instance" contract even when custom
// providers are declared. Multi-provider routing is a future
// extension; the seam is ready (`customByName` holds the table).
func (a *Account) GetConfiguredProviders() ([]bfschemas.ModelProvider, error) {
	return []bfschemas.ModelProvider{a.provider}, nil
}

// GetKeysForProvider implements `bfschemas.Account`. Returns the
// primary's resolved key. The `ctx` is unused at this layer —
// Harbor's identity is propagated separately (the safety pass
// enforces presence; the driver's `Complete` rejects on missing).
func (a *Account) GetKeysForProvider(_ context.Context, providerKey bfschemas.ModelProvider) ([]bfschemas.Key, error) {
	if providerKey != a.provider {
		return nil, fmt.Errorf("bifrost: provider %q is not configured for this Harbor account", providerKey)
	}
	return []bfschemas.Key{
		{
			ID:     "harbor-default",
			Name:   "harbor-default",
			Value:  bfschemas.EnvVar{Val: a.apiKey},
			Models: append([]string(nil), a.primaryModels...),
			Weight: 1.0,
		},
	}, nil
}

// GetConfigForProvider implements `bfschemas.Account`. Returns the
// pre-built primary `ProviderConfig` (network + concurrency, plus
// `CustomProviderConfig` when the primary is a custom-declared
// entry).
func (a *Account) GetConfigForProvider(providerKey bfschemas.ModelProvider) (*bfschemas.ProviderConfig, error) {
	if providerKey != a.provider {
		return nil, fmt.Errorf("bifrost: provider %q is not configured for this Harbor account", providerKey)
	}
	return a.primaryConfig, nil
}
