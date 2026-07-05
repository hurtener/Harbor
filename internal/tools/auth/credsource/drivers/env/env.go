// Package env ships the default credential source: the OAuth provider's
// client credential is read from the process environment ONCE at boot.
//
// This is the historical behavior, preserved byte-for-byte. An operator
// config that omits `credential_source` (or sets it to `env`) resolves
// `client_id_env` / `client_secret_env` at boot and fails loud when
// either env var is unset — the same message the pre-seam `BuildProviders`
// emitted. The process environment is fixed at exec, so the resolved
// credential is immutable for the runtime's life; the source caches it at
// [source.ValidateAtBoot] and every [source.Resolve] returns the cached
// value. The env source NEVER performs I/O and emits NO events.
package env

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
)

// init self-registers the `env` credential source. `internal/drivers/prod`
// blank-imports this package so the registration fires at boot.
func init() {
	credsource.MustRegister(credsource.SourceEnv, New)
}

// New constructs the `env` source for one provider entry. Registered as
// the source Factory; `BuildProviders` dispatches by name.
func New(cfg credsource.Config) (credsource.Source, error) {
	if cfg.ClientIDEnv == "" || cfg.ClientSecretEnv == "" {
		// Defensive: the config validator requires both for the env
		// source. A direct caller that skips validation still fails loud.
		return nil, fmt.Errorf("credsource/env: provider %q: client_id_env and client_secret_env must both be named", cfg.ProviderName)
	}
	return &source{
		providerName:    cfg.ProviderName,
		providerIndex:   cfg.ProviderIndex,
		clientIDEnv:     cfg.ClientIDEnv,
		clientSecretEnv: cfg.ClientSecretEnv,
	}, nil
}

// source is the concrete env credential source. Immutable after
// construction except the once-resolved cache, guarded by resolveOnce.
type source struct {
	providerName    string
	providerIndex   int
	clientIDEnv     string
	clientSecretEnv string

	resolveOnce sync.Once
	cred        credsource.ClientCredential
	resolveErr  error
}

// ValidateAtBoot resolves and caches the credential now, reproducing the
// historical boot-time fail-loud: an unset / empty env var crashes
// assembly with a message naming the offending field. Idempotent — the
// underlying resolution runs at most once.
func (s *source) ValidateAtBoot(context.Context) error {
	_, err := s.resolve()
	return err
}

// Resolve returns the cached credential (resolving on first call). The
// process env is fixed at exec, so there is no refetch.
func (s *source) Resolve(context.Context) (credsource.ClientCredential, error) {
	return s.resolve()
}

func (s *source) resolve() (credsource.ClientCredential, error) {
	s.resolveOnce.Do(func() {
		clientID := os.Getenv(s.clientIDEnv)
		if clientID == "" {
			s.resolveErr = fmt.Errorf("tools/oauth: provider %q (oauth_providers[%d]): env var %q (named by client_id_env) is unset or empty",
				s.providerName, s.providerIndex, s.clientIDEnv)
			return
		}
		clientSecret := os.Getenv(s.clientSecretEnv)
		if clientSecret == "" {
			s.resolveErr = fmt.Errorf("tools/oauth: provider %q (oauth_providers[%d]): env var %q (named by client_secret_env) is unset or empty",
				s.providerName, s.providerIndex, s.clientSecretEnv)
			return
		}
		s.cred = credsource.ClientCredential{ClientID: clientID, ClientSecret: clientSecret}
	})
	if s.resolveErr != nil {
		return credsource.ClientCredential{}, s.resolveErr
	}
	return s.cred, nil
}
