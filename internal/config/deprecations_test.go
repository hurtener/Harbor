package config_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// baseFixtureYAML is the smallest valid config the tests append a
// governance block to. It is intentionally hand-built (not a
// testdata/ fixture) so test additions / removals around the
// governance block stay diff-local to this file.
const baseFixtureYAML = `server:
  bind_addr: 127.0.0.1:8080
  shutdown_grace_period: 30s
identity:
  jwt_algorithms: [RS256]
  issuer: https://issuer.example.com
  audience: harbor
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: json
  log_level: info
  service_name: harbor-test
state:
  driver: inmem
llm:
  provider: openrouter
  model: m
  api_key: k
  timeout: 30s
events:
  driver: inmem
  max_subscribers_per_session: 16
  subscriber_buffer_size: 64
  idle_timeout: 60s
  drop_window: 1s
`

// captureLogger returns a *slog.Logger that writes JSON records to
// `buf` plus the buffer itself for assertion. Tests assert on the
// `field` attribute of each captured record.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	return slog.New(h)
}

// warningFields parses the JSON-line records `captureLogger` emits and
// returns the value of every record's `field` attribute. A record
// missing `field` is skipped — the deprecation warning is the only
// warning the config loader emits today.
func warningFields(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var fields []string
	for _, line := range bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("warning line is not JSON: %s: %v", line, err)
		}
		if rec["msg"] != "config.deprecated_field" {
			continue
		}
		if f, ok := rec["field"].(string); ok {
			fields = append(fields, f)
		}
	}
	return fields
}

// TestLoad_DeprecatedField_DefaultMaxTokens proves the loader emits
// `config.deprecated_field` for the pre-Phase-36a `default_max_tokens`
// key, drops the value from the resulting *Config, and accepts the
// rest of the file. D-081.
func TestLoad_DeprecatedField_DefaultMaxTokens(t *testing.T) {
	yaml := baseFixtureYAML + `governance:
  default_max_tokens: 8192
  repair_attempts: 2
`
	var buf bytes.Buffer
	cfg, err := config.LoadFromBytes(
		context.Background(),
		[]byte(yaml),
		config.WithLogger(captureLogger(&buf)),
	)
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if cfg.Governance.RepairAttempts != 2 {
		t.Errorf("RepairAttempts = %d, want 2", cfg.Governance.RepairAttempts)
	}
	fields := warningFields(t, &buf)
	if got, want := fields, []string{"governance.default_max_tokens"}; !equalStrings(got, want) {
		t.Errorf("warning fields = %v, want %v", got, want)
	}
}

// TestLoad_DeprecatedField_CostCeilingUSD mirrors DefaultMaxTokens
// for the cost-ceiling knob. D-081.
func TestLoad_DeprecatedField_CostCeilingUSD(t *testing.T) {
	yaml := baseFixtureYAML + `governance:
  cost_ceiling_usd: 100
  repair_attempts: 2
`
	var buf bytes.Buffer
	if _, err := config.LoadFromBytes(
		context.Background(),
		[]byte(yaml),
		config.WithLogger(captureLogger(&buf)),
	); err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if got, want := warningFields(t, &buf), []string{"governance.cost_ceiling_usd"}; !equalStrings(got, want) {
		t.Errorf("warning fields = %v, want %v", got, want)
	}
}

// TestLoad_DeprecatedField_RateLimitTPS mirrors DefaultMaxTokens for
// the rate-limit knob. D-081.
func TestLoad_DeprecatedField_RateLimitTPS(t *testing.T) {
	yaml := baseFixtureYAML + `governance:
  rate_limit_tps: 10
  repair_attempts: 2
`
	var buf bytes.Buffer
	if _, err := config.LoadFromBytes(
		context.Background(),
		[]byte(yaml),
		config.WithLogger(captureLogger(&buf)),
	); err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if got, want := warningFields(t, &buf), []string{"governance.rate_limit_tps"}; !equalStrings(got, want) {
		t.Errorf("warning fields = %v, want %v", got, want)
	}
}

// TestLoad_DeprecatedField_WarningAttrShape pins the exact slog
// attribute shape so a future change to the warning surface
// (replacement string, removed_in version, msg text) is a deliberate
// edit caught by the test. D-081's "deprecation warning emitted at
// load if legacy YAML keys appear" clause sets these as the
// migration signal.
func TestLoad_DeprecatedField_WarningAttrShape(t *testing.T) {
	yaml := baseFixtureYAML + `governance:
  default_max_tokens: 8192
  repair_attempts: 2
`
	var buf bytes.Buffer
	if _, err := config.LoadFromBytes(
		context.Background(),
		[]byte(yaml),
		config.WithLogger(captureLogger(&buf)),
	); err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("warning line is not JSON: %v", err)
	}
	if rec["level"] != "WARN" {
		t.Errorf("level=%v, want WARN", rec["level"])
	}
	if rec["msg"] != "config.deprecated_field" {
		t.Errorf("msg=%v, want config.deprecated_field", rec["msg"])
	}
	if rec["field"] != "governance.default_max_tokens" {
		t.Errorf("field=%v, want governance.default_max_tokens", rec["field"])
	}
	if rec["replacement"] != "governance.identity_tiers" {
		t.Errorf("replacement=%v, want governance.identity_tiers", rec["replacement"])
	}
	if rec["removed_in"] != "v0.x" {
		t.Errorf("removed_in=%v, want v0.x", rec["removed_in"])
	}
	if _, ok := rec["source"]; !ok {
		t.Errorf("missing source attr; got %v", rec)
	}
}

// TestLoad_DeprecatedField_FiresOncePerFieldPerLoad covers the
// "warning fires once per field per load" wording from the D-081
// requirements. A single legacy key in the YAML produces exactly one
// `config.deprecated_field` record for that field — not one per parse
// pass, not one per validator iteration. D-081.
func TestLoad_DeprecatedField_FiresOncePerFieldPerLoad(t *testing.T) {
	yaml := baseFixtureYAML + `governance:
  default_max_tokens: 8192
  repair_attempts: 2
`
	var buf bytes.Buffer
	if _, err := config.LoadFromBytes(
		context.Background(),
		[]byte(yaml),
		config.WithLogger(captureLogger(&buf)),
	); err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	got := strings.Count(buf.String(), `"field":"governance.default_max_tokens"`)
	if got != 1 {
		t.Errorf("warning fired %d times, want 1; logs:\n%s", got, buf.String())
	}
}

// TestLoad_DeprecatedField_IdentityTiersStillWins proves the modern
// `identity_tiers` block continues to populate `*Config.Governance`
// even when the deprecated keys appear in the same YAML — the loader
// drops the legacy keys, the strict decoder reads `identity_tiers`,
// and the deprecation warning fires once per legacy key. D-081.
func TestLoad_DeprecatedField_IdentityTiersStillWins(t *testing.T) {
	yaml := baseFixtureYAML + `governance:
  default_max_tokens: 8192
  cost_ceiling_usd: 100
  repair_attempts: 2
  default_tier: default
  identity_tiers:
    default:
      budget_ceiling_usd: 5.00
      max_tokens: 4096
`
	var buf bytes.Buffer
	cfg, err := config.LoadFromBytes(
		context.Background(),
		[]byte(yaml),
		config.WithLogger(captureLogger(&buf)),
	)
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if cfg.Governance.DefaultTier != "default" {
		t.Errorf("DefaultTier=%q, want default", cfg.Governance.DefaultTier)
	}
	tier, ok := cfg.Governance.IdentityTiers["default"]
	if !ok {
		t.Fatalf("IdentityTiers missing 'default' entry; got %v", cfg.Governance.IdentityTiers)
	}
	if tier.BudgetCeilingUSD != 5.00 {
		t.Errorf("BudgetCeilingUSD=%v, want 5.00", tier.BudgetCeilingUSD)
	}
	if tier.MaxTokens != 4096 {
		t.Errorf("MaxTokens=%d, want 4096", tier.MaxTokens)
	}
	fields := warningFields(t, &buf)
	if len(fields) != 2 {
		t.Fatalf("warning fields = %v, want exactly 2", fields)
	}
	wantSet := map[string]bool{
		"governance.default_max_tokens": true,
		"governance.cost_ceiling_usd":   true,
	}
	for _, f := range fields {
		if !wantSet[f] {
			t.Errorf("unexpected field %q in warnings %v", f, fields)
		}
	}
}

// TestLoad_DeprecatedField_NoneFireWhenAbsent is the negative — a
// config with neither a legacy key nor an `identity_tiers` block
// loads cleanly with zero warnings. The latent-default governance
// posture (D-044) is preserved. D-081.
func TestLoad_DeprecatedField_NoneFireWhenAbsent(t *testing.T) {
	yaml := baseFixtureYAML + `governance:
  repair_attempts: 2
`
	var buf bytes.Buffer
	cfg, err := config.LoadFromBytes(
		context.Background(),
		[]byte(yaml),
		config.WithLogger(captureLogger(&buf)),
	)
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if cfg.Governance.RepairAttempts != 2 {
		t.Errorf("RepairAttempts=%d, want 2", cfg.Governance.RepairAttempts)
	}
	if buf.Len() != 0 {
		t.Errorf("expected zero warnings on a clean config; got %q", buf.String())
	}
}

// TestLoad_DeprecatedField_AllThreeAtOnce proves the loader walks the
// full governance block (not just the first deprecated entry it
// finds) and emits one warning per legacy key. D-081's "fires once
// per field per load" reading covers this multi-field shape.
func TestLoad_DeprecatedField_AllThreeAtOnce(t *testing.T) {
	yaml := baseFixtureYAML + `governance:
  default_max_tokens: 8192
  cost_ceiling_usd: 100
  rate_limit_tps: 10
  repair_attempts: 2
`
	var buf bytes.Buffer
	if _, err := config.LoadFromBytes(
		context.Background(),
		[]byte(yaml),
		config.WithLogger(captureLogger(&buf)),
	); err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	got := warningFields(t, &buf)
	want := map[string]bool{
		"governance.default_max_tokens": true,
		"governance.cost_ceiling_usd":   true,
		"governance.rate_limit_tps":     true,
	}
	if len(got) != len(want) {
		t.Fatalf("warning fields = %v, want exactly %d entries", got, len(want))
	}
	for _, f := range got {
		if !want[f] {
			t.Errorf("unexpected warning field %q", f)
		}
		delete(want, f)
	}
	if len(want) != 0 {
		t.Errorf("missing warnings for %v", want)
	}
}

// TestLoad_DeprecatedField_NoLeakIntoOtherSections proves the
// stripping logic targets the top-level `governance:` block only —
// an `llm.model_profiles[<name>].default_max_tokens` field (a real
// Phase 36b knob on `ModelProfile`) is untouched.
func TestLoad_DeprecatedField_NoLeakIntoOtherSections(t *testing.T) {
	// Hand-build the YAML so the `llm:` block carries the
	// `model_profiles[x].default_max_tokens` knob that MUST NOT be
	// stripped — sharing the base fixture's `llm:` block would
	// double-define the top-level key.
	yaml := `server:
  bind_addr: 127.0.0.1:8080
  shutdown_grace_period: 30s
identity:
  jwt_algorithms: [RS256]
  issuer: https://issuer.example.com
  audience: harbor
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: json
  log_level: info
  service_name: harbor-test
state:
  driver: inmem
llm:
  driver: bifrost
  provider: openrouter
  model: x
  api_key: k
  timeout: 30s
  model_profiles:
    x:
      context_window_tokens: 200000
      default_max_tokens: 4096
governance:
  repair_attempts: 2
events:
  driver: inmem
  max_subscribers_per_session: 16
  subscriber_buffer_size: 64
  idle_timeout: 60s
  drop_window: 1s
`
	var buf bytes.Buffer
	cfg, err := config.LoadFromBytes(
		context.Background(),
		[]byte(yaml),
		config.WithLogger(captureLogger(&buf)),
	)
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	prof, ok := cfg.LLM.ModelProfiles["x"]
	if !ok {
		t.Fatalf("ModelProfiles missing 'x' entry; got %v", cfg.LLM.ModelProfiles)
	}
	if prof.DefaultMaxTokens == nil || *prof.DefaultMaxTokens != 4096 {
		t.Errorf("ModelProfile.DefaultMaxTokens not preserved; got %v", prof.DefaultMaxTokens)
	}
	if buf.Len() != 0 {
		t.Errorf("ModelProfile.DefaultMaxTokens should NOT trigger a deprecation warning; got %q", buf.String())
	}
}

// equalStrings returns true iff a and b contain the same elements in
// the same order. The tests above use this to pin the deterministic
// per-load warning ordering — keys are warned in the order they
// appear in the YAML, not in alphabetical order, so an operator's
// log lines mirror their file.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
