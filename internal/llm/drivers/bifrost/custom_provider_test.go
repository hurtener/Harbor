package bifrost

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

// TestNewAccount_CustomProviderPrimary — `Provider` matches a declared
// custom-provider name; the entry's `APIKeyEnvVar` resolves; the
// resulting Account routes via that provider.
func TestNewAccount_CustomProviderPrimary(t *testing.T) {
	const envName = "HARBOR_TEST_NIM_KEY"
	t.Setenv(envName, "nim-secret-value")
	cfg := llm.ConfigSnapshot{
		Provider: "nim",
		Model:    "google/gemma-4-31b-it",
		// Legacy fields ignored when Provider names a custom entry.
		APIKey:  "",
		BaseURL: "",
		Timeout: 0,
		CustomProviders: []llm.CustomProviderSpec{
			{
				Name:         "nim",
				BaseURL:      "https://integrate.api.nvidia.com/v1",
				APIKeyEnvVar: envName,
				Models:       []string{"google/gemma-4-31b-it"},
				Timeout:      180 * time.Second,
			},
		},
	}
	a, err := newAccount(cfg)
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	if a.provider != bfschemas.ModelProvider("nim") {
		t.Errorf("provider = %q want nim", a.provider)
	}
	if a.apiKey != "nim-secret-value" {
		t.Errorf("apiKey not propagated: %q", a.apiKey)
	}
	if len(a.primaryModels) != 1 || a.primaryModels[0] != "google/gemma-4-31b-it" {
		t.Errorf("primaryModels = %v", a.primaryModels)
	}
}

// TestNewAccount_CustomProviderMissingEnv — declared custom provider
// whose env var is unset fails closed at construction with
// ErrMissingAPIKey naming the env var.
func TestNewAccount_CustomProviderMissingEnv(t *testing.T) {
	const envName = "HARBOR_DEFINITELY_UNSET_CUSTOM_KEY"
	cfg := llm.ConfigSnapshot{
		Provider: "nim",
		CustomProviders: []llm.CustomProviderSpec{
			{
				Name:         "nim",
				BaseURL:      "https://example.com",
				APIKeyEnvVar: envName,
				Models:       []string{"m1"},
			},
		},
	}
	_, err := newAccount(cfg)
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("err = %v; want ErrMissingAPIKey", err)
	}
	if !strings.Contains(err.Error(), envName) {
		t.Errorf("err = %q; should name the env var %q", err.Error(), envName)
	}
}

// TestNewAccount_CustomProviderInvalidSpec — empty fields in a custom
// provider spec fail closed with ErrInvalidCustomProvider (defensive
// against programmatic snapshot construction; config-layer validator
// catches before this point in normal flow).
func TestNewAccount_CustomProviderInvalidSpec(t *testing.T) {
	cases := []struct {
		name string
		spec llm.CustomProviderSpec
	}{
		{
			name: "empty name",
			spec: llm.CustomProviderSpec{BaseURL: "u", APIKeyEnvVar: "E", Models: []string{"m"}},
		},
		{
			name: "empty base URL",
			spec: llm.CustomProviderSpec{Name: "x", APIKeyEnvVar: "E", Models: []string{"m"}},
		},
		{
			name: "empty env var",
			spec: llm.CustomProviderSpec{Name: "x", BaseURL: "u", Models: []string{"m"}},
		},
		{
			name: "empty models",
			spec: llm.CustomProviderSpec{Name: "x", BaseURL: "u", APIKeyEnvVar: "E"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := llm.ConfigSnapshot{
				Provider:        "x",
				CustomProviders: []llm.CustomProviderSpec{tc.spec},
			}
			_, err := newAccount(cfg)
			if !errors.Is(err, ErrInvalidCustomProvider) {
				t.Errorf("err = %v; want ErrInvalidCustomProvider", err)
			}
		})
	}
}

// TestNewAccount_UnknownPrimaryWithoutCustom — Provider name that's
// neither a native bifrost provider nor a declared custom entry fails
// closed; error message names both candidate sets.
func TestNewAccount_UnknownPrimaryWithoutCustom(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		Provider: "ghost-provider",
		APIKey:   "k",
	}
	_, err := newAccount(cfg)
	if !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("err = %v; want ErrInvalidProvider", err)
	}
	if !strings.Contains(err.Error(), "(none)") {
		t.Errorf("err = %q; should report (none) for empty custom table", err.Error())
	}
}

// TestAccount_GetConfigForProvider_CustomPrimary — custom primary's
// ProviderConfig carries CustomProviderConfig.BaseProviderType=OpenAI
// and the operator's network knobs.
func TestAccount_GetConfigForProvider_CustomPrimary(t *testing.T) {
	const envName = "HARBOR_TEST_CUSTOM_CFG"
	t.Setenv(envName, "k")
	cfg := llm.ConfigSnapshot{
		Provider: "nim",
		CustomProviders: []llm.CustomProviderSpec{
			{
				Name:                "nim",
				BaseURL:             "https://endpoint.example.com/v1",
				APIKeyEnvVar:        envName,
				Models:              []string{"model-a"},
				Timeout:             45 * time.Second,
				MaxRetries:          7,
				RetryBackoffInitial: 200 * time.Millisecond,
				RetryBackoffMax:     2 * time.Second,
				Concurrency:         8,
				BufferSize:          32,
			},
		},
	}
	a, err := newAccount(cfg)
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	got, err := a.GetConfigForProvider("nim")
	if err != nil {
		t.Fatalf("GetConfigForProvider: %v", err)
	}
	if got.CustomProviderConfig == nil {
		t.Fatal("CustomProviderConfig is nil; want set for custom primary")
	}
	if got.CustomProviderConfig.BaseProviderType != bfschemas.OpenAI {
		t.Errorf("BaseProviderType = %q want %q", got.CustomProviderConfig.BaseProviderType, bfschemas.OpenAI)
	}
	if got.NetworkConfig.BaseURL != "https://endpoint.example.com/v1" {
		t.Errorf("BaseURL = %q", got.NetworkConfig.BaseURL)
	}
	if got.NetworkConfig.DefaultRequestTimeoutInSeconds != 45 {
		t.Errorf("Timeout = %d want 45", got.NetworkConfig.DefaultRequestTimeoutInSeconds)
	}
	if got.NetworkConfig.MaxRetries != 7 {
		t.Errorf("MaxRetries = %d want 7", got.NetworkConfig.MaxRetries)
	}
	if got.NetworkConfig.RetryBackoffInitial != 200*time.Millisecond {
		t.Errorf("RetryBackoffInitial = %v want 200ms", got.NetworkConfig.RetryBackoffInitial)
	}
	if got.NetworkConfig.RetryBackoffMax != 2*time.Second {
		t.Errorf("RetryBackoffMax = %v want 2s", got.NetworkConfig.RetryBackoffMax)
	}
	if got.ConcurrencyAndBufferSize.Concurrency != 8 {
		t.Errorf("Concurrency = %d want 8", got.ConcurrencyAndBufferSize.Concurrency)
	}
	if got.ConcurrencyAndBufferSize.BufferSize != 32 {
		t.Errorf("BufferSize = %d want 32", got.ConcurrencyAndBufferSize.BufferSize)
	}
}

// TestAccount_NetworkDefaults_FallthroughOnCustom — per-provider knobs
// left zero fall through to NetworkDefaults; zero NetworkDefaults
// leaves the field zero so bifrost's own defaults apply.
func TestAccount_NetworkDefaults_FallthroughOnCustom(t *testing.T) {
	const envName = "HARBOR_TEST_CUSTOM_FALLTHROUGH"
	t.Setenv(envName, "k")
	cfg := llm.ConfigSnapshot{
		Provider: "vllm",
		NetworkDefaults: llm.NetworkDefaults{
			Timeout:             60 * time.Second,
			MaxRetries:          5,
			Concurrency:         4,
			BufferSize:          16,
			RetryBackoffInitial: 100 * time.Millisecond,
			RetryBackoffMax:     1 * time.Second,
		},
		CustomProviders: []llm.CustomProviderSpec{
			{
				Name:         "vllm",
				BaseURL:      "http://localhost:8000/v1",
				APIKeyEnvVar: envName,
				Models:       []string{"llama"},
				// Every per-provider knob zero — fall through.
			},
		},
	}
	a, err := newAccount(cfg)
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	got, err := a.GetConfigForProvider("vllm")
	if err != nil {
		t.Fatalf("GetConfigForProvider: %v", err)
	}
	if got.NetworkConfig.DefaultRequestTimeoutInSeconds != 60 {
		t.Errorf("Timeout = %d want 60 (NetworkDefaults)", got.NetworkConfig.DefaultRequestTimeoutInSeconds)
	}
	if got.NetworkConfig.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d want 5", got.NetworkConfig.MaxRetries)
	}
	if got.ConcurrencyAndBufferSize.Concurrency != 4 {
		t.Errorf("Concurrency = %d want 4", got.ConcurrencyAndBufferSize.Concurrency)
	}
	if got.ConcurrencyAndBufferSize.BufferSize != 16 {
		t.Errorf("BufferSize = %d want 16", got.ConcurrencyAndBufferSize.BufferSize)
	}
}

// TestAccount_NetworkDefaults_PerProviderOverride — per-provider knobs
// take precedence over NetworkDefaults.
func TestAccount_NetworkDefaults_PerProviderOverride(t *testing.T) {
	const envName = "HARBOR_TEST_CUSTOM_OVERRIDE"
	t.Setenv(envName, "k")
	cfg := llm.ConfigSnapshot{
		Provider: "vllm",
		NetworkDefaults: llm.NetworkDefaults{
			Timeout:    60 * time.Second,
			MaxRetries: 5,
		},
		CustomProviders: []llm.CustomProviderSpec{
			{
				Name:         "vllm",
				BaseURL:      "http://localhost:8000/v1",
				APIKeyEnvVar: envName,
				Models:       []string{"llama"},
				Timeout:      180 * time.Second, // override
				MaxRetries:   2,                 // override
			},
		},
	}
	a, err := newAccount(cfg)
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	got, _ := a.GetConfigForProvider("vllm")
	if got.NetworkConfig.DefaultRequestTimeoutInSeconds != 180 {
		t.Errorf("Timeout = %d want 180 (per-provider)", got.NetworkConfig.DefaultRequestTimeoutInSeconds)
	}
	if got.NetworkConfig.MaxRetries != 2 {
		t.Errorf("MaxRetries = %d want 2 (per-provider)", got.NetworkConfig.MaxRetries)
	}
}

// TestAccount_NetworkDefaults_OnNativePrimary — NetworkDefaults flow
// through to native providers when LLMConfig.Timeout is zero.
func TestAccount_NetworkDefaults_OnNativePrimary(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		Provider: string(bfschemas.OpenAI),
		APIKey:   "k",
		NetworkDefaults: llm.NetworkDefaults{
			Timeout:    45 * time.Second,
			MaxRetries: 4,
		},
	}
	a, err := newAccount(cfg)
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	got, _ := a.GetConfigForProvider(bfschemas.OpenAI)
	if got.NetworkConfig.DefaultRequestTimeoutInSeconds != 45 {
		t.Errorf("Timeout = %d want 45", got.NetworkConfig.DefaultRequestTimeoutInSeconds)
	}
	if got.NetworkConfig.MaxRetries != 4 {
		t.Errorf("MaxRetries = %d want 4", got.NetworkConfig.MaxRetries)
	}
}

// TestAccount_NativePrimary_LegacyTimeoutWins — legacy LLMConfig.Timeout
// takes precedence over NetworkDefaults for native primary (backwards
// compat).
func TestAccount_NativePrimary_LegacyTimeoutWins(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		Provider:        string(bfschemas.OpenAI),
		APIKey:          "k",
		Timeout:         30 * time.Second,
		NetworkDefaults: llm.NetworkDefaults{Timeout: 90 * time.Second},
	}
	a, _ := newAccount(cfg)
	got, _ := a.GetConfigForProvider(bfschemas.OpenAI)
	if got.NetworkConfig.DefaultRequestTimeoutInSeconds != 30 {
		t.Errorf("Timeout = %d want 30 (legacy wins)", got.NetworkConfig.DefaultRequestTimeoutInSeconds)
	}
}

// TestAccount_CustomPrimary_RequestPathOverrides — the operator-
// provided overrides flow into CustomProviderConfig.
func TestAccount_CustomPrimary_RequestPathOverrides(t *testing.T) {
	const envName = "HARBOR_TEST_CUSTOM_PATHS"
	t.Setenv(envName, "k")
	cfg := llm.ConfigSnapshot{
		Provider: "vllm",
		CustomProviders: []llm.CustomProviderSpec{
			{
				Name:         "vllm",
				BaseURL:      "http://localhost:8000",
				APIKeyEnvVar: envName,
				Models:       []string{"m"},
				RequestPathOverrides: map[string]string{
					"chat_completion": "/chat/completions",
				},
			},
		},
	}
	a, err := newAccount(cfg)
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	got, _ := a.GetConfigForProvider("vllm")
	if got.CustomProviderConfig == nil {
		t.Fatal("CustomProviderConfig nil")
	}
	if got.CustomProviderConfig.RequestPathOverrides[bfschemas.RequestType("chat_completion")] != "/chat/completions" {
		t.Errorf("RequestPathOverrides not propagated: %v", got.CustomProviderConfig.RequestPathOverrides)
	}
}

// TestAccount_GetConfigForProvider_DeclinesUnknownProvider — bifrost
// asking about a non-primary provider gets a clean error.
func TestAccount_GetConfigForProvider_DeclinesUnknownProvider(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		Provider: string(bfschemas.Anthropic),
		APIKey:   "k",
	}
	a, _ := newAccount(cfg)
	_, err := a.GetConfigForProvider(bfschemas.OpenAI)
	if err == nil {
		t.Fatal("expected error for unconfigured provider")
	}
}

// TestConcurrent_D025_Account_Mixed — D-025 concurrent-reuse contract
// for the Phase 33a Account extension. N=128 concurrent goroutines
// call `GetConfigForProvider` and `GetKeysForProvider` against ONE
// shared Account built with both a native and a custom-provider
// declaration. Asserts no data races (the run command runs under
// `-race`) and stable returns under contention. The Account struct
// is read-only after `newAccount`; the test pins the contract.
func TestConcurrent_D025_Account_Mixed(t *testing.T) {
	const envName = "HARBOR_TEST_D025_KEY"
	t.Setenv(envName, "k")
	cfg := llm.ConfigSnapshot{
		Provider: string(bfschemas.OpenAI),
		APIKey:   "openai-key",
		CustomProviders: []llm.CustomProviderSpec{
			{
				Name:         "vllm",
				BaseURL:      "http://localhost:8000",
				APIKeyEnvVar: envName,
				Models:       []string{"m"},
				Timeout:      30 * time.Second,
			},
		},
	}
	a, err := newAccount(cfg)
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	const N = 128
	var (
		wg sync.WaitGroup
		ok atomic.Int64
	)
	wg.Add(N)
	for range N {
		go func() {
			defer wg.Done()
			cfg, err := a.GetConfigForProvider(bfschemas.OpenAI)
			if err != nil || cfg == nil {
				return
			}
			keys, err := a.GetKeysForProvider(context.Background(), bfschemas.OpenAI)
			if err != nil || len(keys) == 0 {
				return
			}
			ok.Add(1)
		}()
	}
	wg.Wait()
	if ok.Load() != int64(N) {
		t.Errorf("ok=%d want %d (concurrent reads failed under -race)", ok.Load(), N)
	}
}

// TestAccount_NativeAndCustomCoexist — declared custom provider that
// isn't the primary still validates (its env key gets resolved at
// construction). GetConfiguredProviders still returns only the
// primary per D-040.
func TestAccount_NativeAndCustomCoexist(t *testing.T) {
	const envName = "HARBOR_TEST_COEXIST_KEY"
	t.Setenv(envName, "k")
	cfg := llm.ConfigSnapshot{
		Provider: string(bfschemas.OpenAI),
		APIKey:   "openai-key",
		CustomProviders: []llm.CustomProviderSpec{
			{
				Name:         "nim",
				BaseURL:      "https://example.com",
				APIKeyEnvVar: envName,
				Models:       []string{"m"},
			},
		},
	}
	a, err := newAccount(cfg)
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	providers, _ := a.GetConfiguredProviders()
	if len(providers) != 1 || providers[0] != bfschemas.OpenAI {
		t.Errorf("providers = %v; want [openai]", providers)
	}
	// The custom-table entry was resolved (key from env).
	if entry, ok := a.customByName["nim"]; !ok {
		t.Error("nim missing from customByName")
	} else if entry.apiKey != "k" {
		t.Errorf("nim key = %q", entry.apiKey)
	}
}
