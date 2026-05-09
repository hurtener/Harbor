package config_test

import (
	"context"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/hurtener/Harbor/internal/config"
)

func TestMarshalForLogging_RedactsTaggedSecret(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.LLM.APIKey = "super-secret-do-not-leak"

	out, err := cfg.MarshalForLogging()
	if err != nil {
		t.Fatalf("MarshalForLogging: %v", err)
	}
	if strings.Contains(string(out), "super-secret-do-not-leak") {
		t.Fatalf("redaction failed: secret value appears in output: %s", out)
	}
	if !strings.Contains(string(out), "***") {
		t.Errorf("redaction placeholder *** missing from output: %s", out)
	}
}

func TestMarshalForLogging_RedactsByNameFallback(t *testing.T) {
	// State.DSN has secret:"true"; verifies tag-based path even though
	// the field name does not match a fallback.
	cfg := mustLoadValid(t)
	cfg.State.Driver = "postgres"
	cfg.State.DSN = "postgres://harbor:hunter2@db.example.com/harbor"

	out, err := cfg.MarshalForLogging()
	if err != nil {
		t.Fatalf("MarshalForLogging: %v", err)
	}
	if strings.Contains(string(out), "hunter2") {
		t.Fatalf("dsn secret leaked: %s", out)
	}
}

func TestMarshalForLogging_LeavesNonSecretsIntact(t *testing.T) {
	cfg := mustLoadValid(t)
	out, err := cfg.MarshalForLogging()
	if err != nil {
		t.Fatalf("MarshalForLogging: %v", err)
	}
	for _, want := range []string{
		"0.0.0.0:9000",
		"https://issuer.example.com",
		"anthropic/claude-sonnet-4",
		"json",
		"info",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("expected non-secret %q to survive in output\n%s", want, out)
		}
	}
}

func TestMarshalForLogging_OutputIsValidYAML(t *testing.T) {
	cfg := mustLoadValid(t)
	out, err := cfg.MarshalForLogging()
	if err != nil {
		t.Fatalf("MarshalForLogging: %v", err)
	}
	var probe map[string]any
	if err := yaml.Unmarshal(out, &probe); err != nil {
		t.Fatalf("redacted output failed to re-parse as YAML: %v\n%s", err, out)
	}
}

func TestMarshalForLogging_DoesNotMutateOriginal(t *testing.T) {
	cfg := mustLoadValid(t)
	orig := cfg.LLM.APIKey
	if _, err := cfg.MarshalForLogging(); err != nil {
		t.Fatalf("MarshalForLogging: %v", err)
	}
	if cfg.LLM.APIKey != orig {
		t.Fatalf("MarshalForLogging mutated original config: APIKey=%q, want %q",
			cfg.LLM.APIKey, orig)
	}
}

func TestMarshalForLogging_NilReceiverErrors(t *testing.T) {
	var cfg *config.Config
	if _, err := cfg.MarshalForLogging(); err == nil {
		t.Fatal("MarshalForLogging on nil *Config returned nil error")
	}
}

func TestExampleHarborYAML_LoadsAndRoundTrips(t *testing.T) {
	cfg, err := config.Load(context.Background(), "../../examples/harbor.yaml")
	if err != nil {
		t.Fatalf("examples/harbor.yaml failed to Load: %v", err)
	}
	out, err := cfg.MarshalForLogging()
	if err != nil {
		t.Fatalf("MarshalForLogging on examples/harbor.yaml: %v", err)
	}
	var probe map[string]any
	if err := yaml.Unmarshal(out, &probe); err != nil {
		t.Fatalf("redacted example failed to re-parse: %v\n%s", err, out)
	}
}
