package bifrost

import (
	"context"
	"errors"
	"strings"
	"testing"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

// TestNewAccount_LiteralKey — APIKey supplied as a literal value.
func TestNewAccount_LiteralKey(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		Provider: string(bfschemas.OpenAI),
		APIKey:   "sk-test-literal",
	}
	a, err := newAccount(cfg, llm.Deps{})
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	if a.primaryKey.Get() != "sk-test-literal" {
		t.Errorf("apiKey not propagated: %q", a.primaryKey.Get())
	}
	if a.provider != bfschemas.OpenAI {
		t.Errorf("provider = %q want %q", a.provider, bfschemas.OpenAI)
	}
}

// TestNewAccount_EnvKey — APIKey via `env.NAME` reference resolves
// from os.Getenv at construction time.
func TestNewAccount_EnvKey(t *testing.T) {
	const envName = "HARBOR_TEST_OPENROUTER_KEY"
	t.Setenv(envName, "sk-from-env-1234")
	cfg := llm.ConfigSnapshot{
		Provider: string(bfschemas.OpenRouter),
		APIKey:   "env." + envName,
	}
	a, err := newAccount(cfg, llm.Deps{})
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	if a.primaryKey.Get() != "sk-from-env-1234" {
		t.Errorf("env-resolved key not propagated: %q", a.primaryKey.Get())
	}
}

// TestNewAccount_MissingEnvVar — env reference whose env var is
// unset fails closed with ErrMissingAPIKey + names the var.
func TestNewAccount_MissingEnvVar(t *testing.T) {
	const envName = "HARBOR_DEFINITELY_UNSET_KEY"
	cfg := llm.ConfigSnapshot{
		Provider: string(bfschemas.OpenAI),
		APIKey:   "env." + envName,
	}
	_, err := newAccount(cfg, llm.Deps{})
	if err == nil {
		t.Fatalf("newAccount succeeded with unset env var; want ErrMissingAPIKey")
	}
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("err = %v; want errors.Is(err, ErrMissingAPIKey)", err)
	}
	if !strings.Contains(err.Error(), envName) {
		t.Errorf("err = %q; should name the env var %q", err.Error(), envName)
	}
}

// TestNewAccount_EmptyAPIKey — empty APIKey fails closed.
func TestNewAccount_EmptyAPIKey(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		Provider: string(bfschemas.OpenAI),
		APIKey:   "",
	}
	_, err := newAccount(cfg, llm.Deps{})
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Errorf("err = %v; want ErrMissingAPIKey", err)
	}
}

// TestNewAccount_EmptyProvider — empty Provider fails closed.
func TestNewAccount_EmptyProvider(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		Provider: "",
		APIKey:   "sk-test",
	}
	_, err := newAccount(cfg, llm.Deps{})
	if !errors.Is(err, ErrInvalidProvider) {
		t.Errorf("err = %v; want ErrInvalidProvider", err)
	}
}

// TestNewAccount_UnknownProvider — non-standard provider name fails
// closed with the allowed list in the error message.
func TestNewAccount_UnknownProvider(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		Provider: "totally-not-real",
		APIKey:   "sk-test",
	}
	_, err := newAccount(cfg, llm.Deps{})
	if !errors.Is(err, ErrInvalidProvider) {
		t.Errorf("err = %v; want ErrInvalidProvider", err)
	}
	if !strings.Contains(err.Error(), "openai") {
		t.Errorf("err = %q; should list known providers", err.Error())
	}
}

// TestAccount_GetKeysForProvider — returns the cached key for the
// configured provider; declines unknown providers.
func TestAccount_GetKeysForProvider(t *testing.T) {
	const literalKey = "sk-cache-test"
	cfg := llm.ConfigSnapshot{
		Provider: string(bfschemas.Anthropic),
		APIKey:   literalKey,
	}
	a, err := newAccount(cfg, llm.Deps{})
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	keys, err := a.GetKeysForProvider(context.Background(), bfschemas.Anthropic)
	if err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1", len(keys))
	}
	if keys[0].Value.Val != literalKey {
		t.Errorf("key value = %q want %q", keys[0].Value.Val, literalKey)
	}
	// Asking for a different provider declines.
	_, err = a.GetKeysForProvider(context.Background(), bfschemas.OpenAI)
	if err == nil {
		t.Errorf("expected error for non-configured provider, got nil")
	}
}

// TestAccount_GetConfigForProvider — returns NetworkConfig with the
// snapshot's BaseURL and Timeout.
func TestAccount_GetConfigForProvider(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		Provider: string(bfschemas.OpenRouter),
		APIKey:   "k",
		BaseURL:  "https://proxy.example.com",
		Timeout:  90_000_000_000, // 90s in nanoseconds
	}
	a, err := newAccount(cfg, llm.Deps{})
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	got, err := a.GetConfigForProvider(bfschemas.OpenRouter)
	if err != nil {
		t.Fatalf("GetConfigForProvider: %v", err)
	}
	if got.NetworkConfig.BaseURL != "https://proxy.example.com" {
		t.Errorf("BaseURL = %q", got.NetworkConfig.BaseURL)
	}
	if got.NetworkConfig.DefaultRequestTimeoutInSeconds != 90 {
		t.Errorf("Timeout = %d want 90", got.NetworkConfig.DefaultRequestTimeoutInSeconds)
	}
}

// TestAccount_GetConfiguredProviders — single-provider V1 deployment.
func TestAccount_GetConfiguredProviders(t *testing.T) {
	cfg := llm.ConfigSnapshot{
		Provider: string(bfschemas.OpenAI),
		APIKey:   "k",
	}
	a, err := newAccount(cfg, llm.Deps{})
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	providers, err := a.GetConfiguredProviders()
	if err != nil {
		t.Fatalf("GetConfiguredProviders: %v", err)
	}
	if len(providers) != 1 || providers[0] != bfschemas.OpenAI {
		t.Errorf("providers = %v want [openai]", providers)
	}
}

// TestAccount_GetKeysForProvider_ReflectsRotate proves the per-call read
// path picks up an atomic key rotation on the SHARED holder injected via
// deps — the seam Console-driven key rotation depends on.
func TestAccount_GetKeysForProvider_ReflectsRotate(t *testing.T) {
	holder := llm.NewLiveKey()
	cfg := llm.ConfigSnapshot{Provider: "openai", APIKey: "sk-boot"}
	a, err := newAccount(cfg, llm.Deps{LiveKey: holder})
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	keys, err := a.GetKeysForProvider(context.Background(), a.provider)
	if err != nil || len(keys) != 1 {
		t.Fatalf("GetKeysForProvider: keys=%d err=%v", len(keys), err)
	}
	if keys[0].Value.Val != "sk-boot" {
		t.Fatalf("boot key = %q, want sk-boot", keys[0].Value.Val)
	}
	// Rotate the shared holder (what the rotate service does).
	if err := holder.Rotate("sk-rotated"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	keys, err = a.GetKeysForProvider(context.Background(), a.provider)
	if err != nil {
		t.Fatalf("GetKeysForProvider after rotate: %v", err)
	}
	if keys[0].Value.Val != "sk-rotated" {
		t.Errorf("after rotate, GetKeysForProvider = %q, want sk-rotated", keys[0].Value.Val)
	}
}

// TestAccount_CustomPrimary_ReflectsRotate proves the custom-provider
// PRIMARY path also seeds + reads the injected shared holder, so a rotate
// reflects through GetKeysForProvider (the native path is covered by
// TestAccount_GetKeysForProvider_ReflectsRotate).
func TestAccount_CustomPrimary_ReflectsRotate(t *testing.T) {
	t.Setenv("HARBOR_TEST_CUSTOM_PRIMARY_KEY", "sk-custom-boot")
	holder := llm.NewLiveKey()
	cfg := llm.ConfigSnapshot{
		Provider: "vllm-local",
		CustomProviders: []llm.CustomProviderSpec{{
			Name:         "vllm-local",
			BaseURL:      "http://127.0.0.1:1",
			APIKeyEnvVar: "HARBOR_TEST_CUSTOM_PRIMARY_KEY",
			Models:       []string{"m"},
		}},
	}
	a, err := newAccount(cfg, llm.Deps{LiveKey: holder})
	if err != nil {
		t.Fatalf("newAccount: %v", err)
	}
	keys, err := a.GetKeysForProvider(context.Background(), a.provider)
	if err != nil || len(keys) != 1 || keys[0].Value.Val != "sk-custom-boot" {
		t.Fatalf("custom-primary boot key: keys=%d err=%v val=%q", len(keys), err, func() string {
			if len(keys) == 1 {
				return keys[0].Value.Val
			}
			return ""
		}())
	}
	if err := holder.Rotate("sk-custom-rotated"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	keys, _ = a.GetKeysForProvider(context.Background(), a.provider)
	if keys[0].Value.Val != "sk-custom-rotated" {
		t.Errorf("custom-primary after rotate = %q, want sk-custom-rotated", keys[0].Value.Val)
	}
}
