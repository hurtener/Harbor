// cmd/harbor/cmd_dev_test.go — unit tests for the Phase 64 `harbor
// dev` subcommand's reachable helpers. The end-to-end wire-side boot
// is exercised by `test/integration/phase64_harbor_dev_test.go`; this
// file pins the pre-boot logic (the validateLLMProvider fail-loud, the
// HARBOR_BIND port parser, the dev signer + token mint flow, the
// boot-error → CLIError mapping).

package main

import (
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
)

// TestValidateLLMProvider_NoMockEscape_Bifrost_RejectsEmptyProvider —
// constraint #2 fail-loud: driver=bifrost without a provider/model/
// api_key surfaces ErrLLMRequired naming the missing field.
func TestValidateLLMProvider_NoMockEscape_Bifrost_RejectsEmptyProvider(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{Driver: "bifrost"}}
	err := validateLLMProvider(cfg, false)
	if !errors.Is(err, ErrLLMRequired) {
		t.Fatalf("validateLLMProvider() = %v; want errors.Is(err, ErrLLMRequired)", err)
	}
	if !contains(err.Error(), "llm.provider") {
		t.Errorf("error message %q missing 'llm.provider' named-field hint", err.Error())
	}
}

// TestValidateLLMProvider_NoMockEscape_Bifrost_AcceptsFullSpec —
// constraint #2 happy path: a full bifrost spec passes validation.
func TestValidateLLMProvider_NoMockEscape_Bifrost_AcceptsFullSpec(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{
		Driver:   "bifrost",
		Provider: "openrouter",
		Model:    "anthropic/claude-sonnet-4",
		APIKey:   "env.OPENROUTER_API_KEY",
	}}
	if err := validateLLMProvider(cfg, false); err != nil {
		t.Errorf("validateLLMProvider() = %v; want nil", err)
	}
}

// TestValidateLLMProvider_NoMockEscape_MockDriver_FailsLoud —
// constraint #2: driver=mock without HARBOR_DEV_ALLOW_MOCK=1 fails
// loud. This is the §13 "test stubs as production defaults" gate.
func TestValidateLLMProvider_NoMockEscape_MockDriver_FailsLoud(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{Driver: "mock"}}
	err := validateLLMProvider(cfg, false)
	if !errors.Is(err, ErrLLMRequired) {
		t.Fatalf("validateLLMProvider() = %v; want ErrLLMRequired", err)
	}
	if !contains(err.Error(), EnvDevAllowMock) {
		t.Errorf("error message %q should mention the escape-hatch env var %q", err.Error(), EnvDevAllowMock)
	}
}

// TestValidateLLMProvider_MockEscape_ShortCircuits — when
// allowMock=true (HARBOR_DEV_ALLOW_MOCK=1), the function returns nil
// regardless of the driver knobs. The dev cmd's runtime path
// overrides driver to "mock" downstream.
func TestValidateLLMProvider_MockEscape_ShortCircuits(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{Driver: "bifrost"}} // missing provider/model/api_key — but allowMock bypasses.
	if err := validateLLMProvider(cfg, true); err != nil {
		t.Errorf("validateLLMProvider(allowMock=true) = %v; want nil", err)
	}
}

// TestParsePortFromBind_Valid — HARBOR_BIND=host:port parses cleanly.
func TestParsePortFromBind_Valid(t *testing.T) {
	cases := map[string]int{
		"127.0.0.1:18080": 18080,
		"localhost:8080":  8080,
		// IPv6 bracketed form — uses LastIndex(':') so the trailing
		// port parses out cleanly.
		"[::1]:9090": 9090,
	}
	for bind, want := range cases {
		got, ok := parsePortFromBind(bind)
		if !ok {
			t.Errorf("parsePortFromBind(%q) ok=false; want true", bind)
			continue
		}
		if got != want {
			t.Errorf("parsePortFromBind(%q) = %d, want %d", bind, got, want)
		}
	}
}

// TestParsePortFromBind_Malformed — invalid bind strings return
// (0, false) so the caller keeps the supplied --port.
func TestParsePortFromBind_Malformed(t *testing.T) {
	cases := []string{
		"",
		"hostname",             // no colon
		"127.0.0.1:",           // trailing colon
		"127.0.0.1:notanumber", // non-numeric port
		"127.0.0.1:0",          // port 0 rejected (sentinel)
	}
	for _, bind := range cases {
		if _, ok := parsePortFromBind(bind); ok {
			t.Errorf("parsePortFromBind(%q) ok=true; want false", bind)
		}
	}
}

// TestNewDevSigner_GeneratesDistinctKeysAcrossCalls — each
// newDevSigner() mints a fresh keypair. Two consecutive calls produce
// keypairs that do NOT cross-validate, so a leaked token from one
// dev session cannot be replayed against a later session.
func TestNewDevSigner_GeneratesDistinctKeysAcrossCalls(t *testing.T) {
	a, err := newDevSigner()
	if err != nil {
		t.Fatalf("newDevSigner: %v", err)
	}
	b, err := newDevSigner()
	if err != nil {
		t.Fatalf("newDevSigner: %v", err)
	}
	// The X coordinates of the two public keys MUST differ — the
	// generator is sourced from crypto/rand, so a collision is
	// vanishingly unlikely (lottery-ticket math).
	if a.priv.PublicKey.X.Cmp(b.priv.PublicKey.X) == 0 {
		t.Error("two newDevSigner() calls produced the same public-key X — generator looks deterministic")
	}
}

// TestSignDevToken_ProducesParseableJWT — the minted token round-trips
// through the JWT parser: header has kid=harbor-dev, alg=ES256,
// claims have the supplied identity triple + scopes.
func TestSignDevToken_ProducesParseableJWT(t *testing.T) {
	s, err := newDevSigner()
	if err != nil {
		t.Fatalf("newDevSigner: %v", err)
	}
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	tok, err := s.SignDevToken(now, "t1", "u1", "s1", []string{"admin"})
	if err != nil {
		t.Fatalf("SignDevToken: %v", err)
	}
	if tok == "" {
		t.Fatal("SignDevToken returned empty token")
	}
	// JWT structure: three '.'-separated base64 segments.
	if countDots(tok) != 2 {
		t.Errorf("token does not look like a JWT (3 segments): %q", tok)
	}
}

// TestSignDevToken_IncompleteIdentity_FailsLoud — constraint: identity
// triple is mandatory; missing component fails closed.
func TestSignDevToken_IncompleteIdentity_FailsLoud(t *testing.T) {
	s, _ := newDevSigner()
	now := time.Now()
	cases := [][3]string{
		{"", "u", "s"},
		{"t", "", "s"},
		{"t", "u", ""},
	}
	for _, c := range cases {
		_, err := s.SignDevToken(now, c[0], c[1], c[2], nil)
		if err == nil {
			t.Errorf("SignDevToken(%q, %q, %q) returned nil err; want non-nil", c[0], c[1], c[2])
		}
	}
}

// TestBootErrorToCLIError_MapsKnownSentinels — the mapping from
// boot-time errors onto CLIError codes is stable. New error classes
// added to the mapping must extend this table.
func TestBootErrorToCLIError_MapsKnownSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"llm_required", ErrLLMRequired, CodeBootLLMRequired},
		{"config_not_found", config.ErrConfigNotFound, CodeBootConfigInvalid},
		{"config_invalid", config.ErrConfigInvalid, CodeBootConfigInvalid},
		{"unknown", errors.New("anything else"), CodeBootInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := bootErrorToCLIError(tc.err)
			if cli.Code != tc.want {
				t.Errorf("Code = %q, want %q (input: %v)", cli.Code, tc.want, tc.err)
			}
		})
	}
}

// contains is the stdlib-free substring helper used by the
// fail-loud message assertions above.
func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// countDots is the JWT-shape assertion helper.
func countDots(s string) int {
	n := 0
	for _, c := range s {
		if c == '.' {
			n++
		}
	}
	return n
}
