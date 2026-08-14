// build_providers_test.go — Phase 110d (D-197) fail-loud matrix for
// the promoted OAuth provider assembly. KEK / client credentials are
// test dummies injected via t.Setenv (CLAUDE.md §7 rule 2 — env-var
// indirection, nothing hardcoded as a real secret).
package auth

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	// Register the `env` credential source so BuildProviders can resolve
	// the default source at boot (the driver self-registers via init()).
	_ "github.com/hurtener/Harbor/internal/tools/auth/credsource/drivers/env"
)

// bpTestDriverOnce registers the in-test provider driver exactly once
// per test binary (safe under `go test -count=N` — package state
// persists across iterations).
var bpTestDriverOnce sync.Once

func registerBPTestDriver(t *testing.T) {
	t.Helper()
	bpTestDriverOnce.Do(func() {
		MustRegister("bp-test", func(cfg ProviderConfig, deps FactoryDeps) (OAuthProvider, error) {
			// Mirror the oauth2 driver's shape: resolve the credential
			// through the source seam, then build ONE OAuthConfig per
			// provider entry over the shared crypto chain.
			cred, err := cfg.CredentialSource.Resolve(context.Background())
			if err != nil {
				return nil, err
			}
			oauthCfg := OAuthConfig{
				Source:       toolsToolSource(cfg.Name),
				SourceName:   cfg.Name,
				BindingScope: ScopeUser,
				ClientID:     cred.ClientID,
				ClientSecret: cred.ClientSecret,
				AuthorizeURL: cfg.AuthURL,
				TokenURL:     cfg.TokenURL,
				RedirectURI:  cfg.RedirectURL,
				Scopes:       append([]string(nil), cfg.Scopes...),
			}
			return NewProvider([]OAuthConfig{oauthCfg}, ProviderDeps{
				Store:       deps.Store,
				Flows:       deps.Flows,
				Bus:         deps.Bus,
				Redactor:    deps.Redactor,
				Coordinator: deps.Coordinator,
			})
		})
	})
}

func bpDeps(t *testing.T) BuildDeps {
	t.Helper()
	red := mkRedactor(t)
	return BuildDeps{
		State:       mkStore(t),
		Bus:         mkBus(t, red),
		Redactor:    red,
		Coordinator: mkCoordinator(t),
	}
}

// dummyKEKHex is a documented test dummy: 32 bytes of 0x01, hex-encoded.
const dummyKEKHex = "0101010101010101010101010101010101010101010101010101010101010101"

func bpProviderCfg(driver string) config.ToolOAuthProviderConfig {
	return config.ToolOAuthProviderConfig{
		Name:            "gh",
		Driver:          driver,
		ClientIDEnv:     "HARBOR_BP_TEST_CLIENT_ID",
		ClientSecretEnv: "HARBOR_BP_TEST_CLIENT_SECRET",
		Scopes:          []string{"repo"},
		AuthURL:         "https://auth.example.com/authorize",
		TokenURL:        "https://auth.example.com/token",
		RedirectURL:     "https://localhost/callback",
	}
}

// TestBuildProviders_EmptyDeclaration_NoKEKTouched — an empty
// oauth_providers list returns an empty non-nil map and never reads
// the KEK env (the binary boots cleanly with no OAuth declared).
func TestBuildProviders_EmptyDeclaration_NoKEKTouched(t *testing.T) {
	providers, err := BuildProviders(context.Background(), config.ToolsConfig{}, bpDeps(t))
	if err != nil {
		t.Fatalf("BuildProviders: %v", err)
	}
	if providers == nil || len(providers) != 0 {
		t.Fatalf("expected empty non-nil map, got %v", providers)
	}
}

// TestBuildProviders_FailLoudMatrix — every misconfiguration crashes
// assembly with an error naming the offending field (§13 amendment).
func TestBuildProviders_FailLoudMatrix(t *testing.T) {
	registerBPTestDriver(t)
	cases := []struct {
		name  string
		setup func(t *testing.T) config.ToolsConfig
		want  string
	}{
		{
			name: "kek env name unset",
			setup: func(_ *testing.T) config.ToolsConfig {
				return config.ToolsConfig{OAuthProviders: []config.ToolOAuthProviderConfig{bpProviderCfg("bp-test")}}
			},
			want: "oauth_token_kek_env must be set",
		},
		{
			name: "kek env empty",
			setup: func(_ *testing.T) config.ToolsConfig {
				return config.ToolsConfig{
					OAuthTokenKEKEnv: "HARBOR_BP_TEST_KEK_UNSET",
					OAuthProviders:   []config.ToolOAuthProviderConfig{bpProviderCfg("bp-test")},
				}
			},
			want: `env var "HARBOR_BP_TEST_KEK_UNSET"`,
		},
		{
			name: "kek not hex",
			setup: func(t *testing.T) config.ToolsConfig {
				t.Setenv("HARBOR_BP_TEST_KEK", "not-hex!!")
				return config.ToolsConfig{
					OAuthTokenKEKEnv: "HARBOR_BP_TEST_KEK",
					OAuthProviders:   []config.ToolOAuthProviderConfig{bpProviderCfg("bp-test")},
				}
			},
			want: "not valid hex",
		},
		{
			name: "kek wrong length",
			setup: func(t *testing.T) config.ToolsConfig {
				t.Setenv("HARBOR_BP_TEST_KEK", "0102")
				return config.ToolsConfig{
					OAuthTokenKEKEnv: "HARBOR_BP_TEST_KEK",
					OAuthProviders:   []config.ToolOAuthProviderConfig{bpProviderCfg("bp-test")},
				}
			},
			want: "decoded to 2 bytes, want 32",
		},
		{
			name: "client id env unset",
			setup: func(t *testing.T) config.ToolsConfig {
				t.Setenv("HARBOR_BP_TEST_KEK", dummyKEKHex)
				return config.ToolsConfig{
					OAuthTokenKEKEnv: "HARBOR_BP_TEST_KEK",
					OAuthProviders:   []config.ToolOAuthProviderConfig{bpProviderCfg("bp-test")},
				}
			},
			want: `env var "HARBOR_BP_TEST_CLIENT_ID"`,
		},
		{
			name: "client secret env unset",
			setup: func(t *testing.T) config.ToolsConfig {
				t.Setenv("HARBOR_BP_TEST_KEK", dummyKEKHex)
				t.Setenv("HARBOR_BP_TEST_CLIENT_ID", "dummy-client-id")
				return config.ToolsConfig{
					OAuthTokenKEKEnv: "HARBOR_BP_TEST_KEK",
					OAuthProviders:   []config.ToolOAuthProviderConfig{bpProviderCfg("bp-test")},
				}
			},
			want: `env var "HARBOR_BP_TEST_CLIENT_SECRET"`,
		},
		{
			name: "unknown driver",
			setup: func(t *testing.T) config.ToolsConfig {
				t.Setenv("HARBOR_BP_TEST_KEK", dummyKEKHex)
				t.Setenv("HARBOR_BP_TEST_CLIENT_ID", "dummy-client-id")
				t.Setenv("HARBOR_BP_TEST_CLIENT_SECRET", "dummy-client-secret")
				return config.ToolsConfig{
					OAuthTokenKEKEnv: "HARBOR_BP_TEST_KEK",
					OAuthProviders:   []config.ToolOAuthProviderConfig{bpProviderCfg("no-such-driver")},
				}
			},
			want: `driver="no-such-driver"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.setup(t)
			providers, err := BuildProviders(context.Background(), cfg, bpDeps(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
			if providers != nil {
				t.Errorf("expected nil providers map on error, got %v", providers)
			}
		})
	}
}

// TestBuildProviders_HappyPath_BuildsAndCloses — a well-formed
// declaration yields a keyed provider map; the provider closes
// cleanly.
func TestBuildProviders_HappyPath_BuildsAndCloses(t *testing.T) {
	registerBPTestDriver(t)
	t.Setenv("HARBOR_BP_TEST_KEK", dummyKEKHex)
	t.Setenv("HARBOR_BP_TEST_CLIENT_ID", "dummy-client-id")
	t.Setenv("HARBOR_BP_TEST_CLIENT_SECRET", "dummy-client-secret")
	cfg := config.ToolsConfig{
		OAuthTokenKEKEnv: "HARBOR_BP_TEST_KEK",
		OAuthProviders:   []config.ToolOAuthProviderConfig{bpProviderCfg("bp-test")},
	}
	providers, err := BuildProviders(context.Background(), cfg, bpDeps(t))
	if err != nil {
		t.Fatalf("BuildProviders: %v", err)
	}
	prov, ok := providers["gh"]
	if !ok || prov == nil {
		t.Fatalf("expected provider keyed by name 'gh', got %v", providers)
	}
	if err := prov.Close(context.Background()); err != nil {
		t.Errorf("provider Close: %v", err)
	}
}

// TestNewSealerFromEnv_Matrix pins the generalized restart-stable
// KEK-backed sealer construction (HA-56 shared authority): an empty env
// name, an unset/empty env value, non-hex content, and a wrong-length
// key all fail loud naming the env var; a valid 32-byte hex KEK yields
// an immutable Sealer that round-trips Seal/Open.
func TestNewSealerFromEnv_Matrix(t *testing.T) {
	const env = "HARBOR_BP_TEST_NEW_SEALER_KEK"

	// Empty env name → loud failure.
	if _, err := NewSealerFromEnv(""); err == nil {
		t.Fatal("NewSealerFromEnv(\"\") must fail loud")
	}

	// Unset env → loud failure.
	if _, err := NewSealerFromEnv(env); err == nil {
		t.Fatal("NewSealerFromEnv(unset env) must fail loud")
	} else if !strings.Contains(err.Error(), env) {
		t.Fatalf("unset-env error %q does not name the env var", err)
	}

	// Empty env value → loud failure.
	t.Setenv(env, "")
	if _, err := NewSealerFromEnv(env); err == nil {
		t.Fatal("NewSealerFromEnv(empty env) must fail loud")
	}

	// Non-hex content → loud failure.
	t.Setenv(env, "not-hex!!")
	if _, err := NewSealerFromEnv(env); err == nil {
		t.Fatal("NewSealerFromEnv(non-hex env) must fail loud")
	}

	// Wrong-length key → loud failure.
	t.Setenv(env, "0102")
	if _, err := NewSealerFromEnv(env); err == nil {
		t.Fatal("NewSealerFromEnv(short env) must fail loud")
	}

	// Valid 32-byte hex KEK → a usable AES-256-GCM sealer.
	t.Setenv(env, dummyKEKHex)
	sealer, err := NewSealerFromEnv(env)
	if err != nil {
		t.Fatalf("NewSealerFromEnv(valid env): %v", err)
	}
	if sealer == nil {
		t.Fatal("NewSealerFromEnv(valid env) returned nil")
	}
	sealed, err := sealer.Seal([]byte("proposal-token-plaintext"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	opened, err := sealer.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(opened) != "proposal-token-plaintext" {
		t.Fatalf("Open round-trip = %q, want the sealed plaintext", opened)
	}
	// The same authority opens tokens sealed earlier — restart-stability
	// within one process; the AES-GCM key is the env-derived KEK.
	if _, err := sealer.Open([]byte("not-a-real-ciphertext")); err == nil {
		t.Fatal("Open of garbage must fail loud (AEAD authentication)")
	}
}
