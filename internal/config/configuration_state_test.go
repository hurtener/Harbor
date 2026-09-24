package config_test

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

func TestConfigurationState_Validation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store config.StateConfig
		want  string
	}{
		{"omitted", config.StateConfig{}, ""},
		{"memory", config.StateConfig{Driver: "inmem"}, ""},
		{"sqlite", config.StateConfig{Driver: "sqlite", DSN: ":memory:"}, ""},
		{"postgres", config.StateConfig{Driver: "postgres", DSN: "postgres://localhost/test", MigrationMode: "verify"}, ""},
		{"missing driver", config.StateConfig{DSN: "test"}, "configuration_state.driver"},
		{"unknown", config.StateConfig{Driver: "other"}, "configuration_state.driver"},
		{"missing dsn", config.StateConfig{Driver: "postgres"}, "configuration_state.dsn"},
		{"blank dsn", config.StateConfig{Driver: "sqlite", DSN: " "}, "configuration_state.dsn"},
		{"invalid migration", config.StateConfig{Driver: "postgres", DSN: "test", MigrationMode: "wrong"}, "configuration_state.migration_mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoadValid(t)
			cfg.ConfigurationState = tc.store
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v; want field %s", err, tc.want)
			}
		})
	}
}

func TestConfigurationState_EnvironmentAndRedaction(t *testing.T) {
	// Dummy local fixture, never a live credential.
	const dsn = "postgres://fixture:config-fixture-secret@localhost/test"
	t.Setenv("HARBOR_CONFIGURATION_STATE_DRIVER", "postgres")
	t.Setenv("HARBOR_CONFIGURATION_STATE_DSN", dsn)
	t.Setenv("HARBOR_CONFIGURATION_STATE_MIGRATION_MODE", "verify")
	cfg := mustLoadValid(t)
	if cfg.ConfigurationState.Driver != "postgres" || cfg.ConfigurationState.DSN != dsn || cfg.ConfigurationState.MigrationMode != "verify" {
		t.Fatal("configuration store environment overrides not applied")
	}
	body, err := cfg.MarshalForLogging()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "config-fixture-secret") {
		t.Fatal("configuration DSN leaked")
	}
}
