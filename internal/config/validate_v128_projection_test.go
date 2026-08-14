package config_test

// validate_v128_projection_test.go — the HA-64 / HA-65 projection-store
// config blocks (`sessions.turns`, `observability.rollups`): the
// optional empty block leaves the surface unwired, and a non-empty
// driver must be one of the closed triad with the same DSN contract as
// every other driver-selecting block (a durable driver requires an
// explicit dsn; `inmem` never does).

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// TestValidate_TurnsProjection_OptionalAbsentBlock — the zero-value
// block (no `sessions.turns.driver`) validates cleanly: the projection
// is unwired and the turn routes stay at 501 (the partial-build
// convention), never a validation failure.
func TestValidate_TurnsProjection_OptionalAbsentBlock(t *testing.T) {
	cfg := mustLoadValid(t)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate (absent turns block): %v", err)
	}
}

// TestValidate_TurnsProjection_DriverMatrix — the closed triad with the
// DSN contract: inmem needs no dsn, sqlite/postgres require one.
func TestValidate_TurnsProjection_DriverMatrix(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*config.Config)
		want string // empty = must validate
	}{
		{"inmem without dsn", func(c *config.Config) {
			c.Sessions.Turns = config.TurnsConfig{Driver: "inmem"}
		}, ""},
		{"sqlite with dsn", func(c *config.Config) {
			c.Sessions.Turns = config.TurnsConfig{Driver: "sqlite", DSN: "/var/lib/harbor/turns.sqlite"}
		}, ""},
		{"postgres with dsn", func(c *config.Config) {
			c.Sessions.Turns = config.TurnsConfig{Driver: "postgres", DSN: "postgres://harbor:secret@localhost:5432/harbor?sslmode=disable"}
		}, ""},
		{"sqlite without dsn", func(c *config.Config) {
			c.Sessions.Turns = config.TurnsConfig{Driver: "sqlite"}
		}, "sessions.turns.dsn"},
		{"postgres without dsn", func(c *config.Config) {
			c.Sessions.Turns = config.TurnsConfig{Driver: "postgres"}
		}, "sessions.turns.dsn"},
		{"unknown driver", func(c *config.Config) {
			c.Sessions.Turns = config.TurnsConfig{Driver: "oracle", DSN: "x"}
		}, "sessions.turns.driver"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoadValid(t)
			tc.mut(cfg)
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate accepted %s, want %q error", tc.name, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

// TestValidate_ObservabilityRollups_DriverMatrix mirrors the turns
// block for the HA-65 rollup store.
func TestValidate_ObservabilityRollups_DriverMatrix(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*config.Config)
		want string
	}{
		{"absent block", func(c *config.Config) {}, ""},
		{"inmem without dsn", func(c *config.Config) {
			c.Observability = config.ObservabilityConfig{Rollups: config.RollupsConfig{Driver: "inmem"}}
		}, ""},
		{"sqlite with dsn", func(c *config.Config) {
			c.Observability = config.ObservabilityConfig{Rollups: config.RollupsConfig{Driver: "sqlite", DSN: "/var/lib/harbor/rollups.sqlite"}}
		}, ""},
		{"sqlite without dsn", func(c *config.Config) {
			c.Observability = config.ObservabilityConfig{Rollups: config.RollupsConfig{Driver: "sqlite"}}
		}, "observability.rollups.dsn"},
		{"unknown driver", func(c *config.Config) {
			c.Observability = config.ObservabilityConfig{Rollups: config.RollupsConfig{Driver: "oracle", DSN: "x"}}
		}, "observability.rollups.driver"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoadValid(t)
			tc.mut(cfg)
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate accepted %s, want %q error", tc.name, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %v, want mention of %q", err, tc.want)
			}
		})
	}
}
