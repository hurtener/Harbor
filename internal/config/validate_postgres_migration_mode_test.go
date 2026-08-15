package config_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
)

type migrationModeTarget struct {
	name string
	set  func(*config.Config, string, sqlmigrate.Mode)
	get  func(*config.Config) sqlmigrate.Mode
}

var migrationModeTargets = []migrationModeTarget{
	{
		name: "state",
		set: func(c *config.Config, driver string, mode sqlmigrate.Mode) {
			c.State = config.StateConfig{Driver: driver, DSN: migrationTestDSN(driver), MigrationMode: mode}
		},
		get: func(c *config.Config) sqlmigrate.Mode { return c.State.MigrationMode },
	},
	{
		name: "memory",
		set: func(c *config.Config, driver string, mode sqlmigrate.Mode) {
			c.Memory = config.MemoryConfig{Driver: driver, DSN: migrationTestDSN(driver), MigrationMode: mode}
		},
		get: func(c *config.Config) sqlmigrate.Mode { return c.Memory.MigrationMode },
	},
	{
		name: "artifacts",
		set: func(c *config.Config, driver string, mode sqlmigrate.Mode) {
			c.Artifacts.Driver = driver
			c.Artifacts.DSN = migrationTestDSN(driver)
			c.Artifacts.MigrationMode = mode
		},
		get: func(c *config.Config) sqlmigrate.Mode { return c.Artifacts.MigrationMode },
	},
	{
		name: "skills",
		set: func(c *config.Config, driver string, mode sqlmigrate.Mode) {
			c.Skills = config.SkillsConfig{Driver: driver, DSN: migrationTestDSN(driver), MigrationMode: mode}
		},
		get: func(c *config.Config) sqlmigrate.Mode { return c.Skills.MigrationMode },
	},
	{
		name: "sessions.turns",
		set: func(c *config.Config, driver string, mode sqlmigrate.Mode) {
			c.Sessions.Turns = config.TurnsConfig{Driver: driver, DSN: migrationTestDSN(driver), MigrationMode: mode}
		},
		get: func(c *config.Config) sqlmigrate.Mode { return c.Sessions.Turns.MigrationMode },
	},
	{
		name: "observability.rollups",
		set: func(c *config.Config, driver string, mode sqlmigrate.Mode) {
			c.Observability.Rollups = config.RollupsConfig{Driver: driver, DSN: migrationTestDSN(driver), MigrationMode: mode}
		},
		get: func(c *config.Config) sqlmigrate.Mode { return c.Observability.Rollups.MigrationMode },
	},
}

func migrationTestDSN(driver string) string {
	if driver == "postgres" {
		return "postgres://harbor:test@pooler:6432/harbor"
	}
	return "/tmp/harbor.sqlite"
}

func TestValidate_PostgresMigrationMode_AllStores(t *testing.T) {
	for _, target := range migrationModeTargets {
		t.Run(target.name, func(t *testing.T) {
			for _, mode := range []sqlmigrate.Mode{"", sqlmigrate.ModeApply, sqlmigrate.ModeVerify} {
				cfg := mustLoadValid(t)
				target.set(cfg, "postgres", mode)
				if err := cfg.Validate(); err != nil {
					t.Fatalf("postgres mode %q: Validate: %v", mode, err)
				}
			}

			cfg := mustLoadValid(t)
			target.set(cfg, "postgres", "future")
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), target.name+".migration_mode") {
				t.Fatalf("unknown mode error = %v, want field %s.migration_mode", err, target.name)
			}

			cfg = mustLoadValid(t)
			target.set(cfg, "sqlite", sqlmigrate.ModeVerify)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "must be empty unless driver=\"postgres\"") {
				t.Fatalf("sqlite verify error = %v", err)
			}
		})
	}
}

func TestLoad_PostgresMigrationMode_AllStores(t *testing.T) {
	base, err := os.ReadFile(validMinimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	yamlText := strings.Replace(string(base), "state:\n  driver: inmem", `state:
  driver: postgres
  dsn: postgres://harbor:test@pooler:6432/state
  migration_mode: verify`, 1) + `
memory:
  driver: postgres
  dsn: postgres://harbor:test@pooler:6432/memory
  migration_mode: verify
artifacts:
  driver: postgres
  dsn: postgres://harbor:test@pooler:6432/artifacts
  migration_mode: verify
skills:
  driver: postgres
  dsn: postgres://harbor:test@pooler:6432/skills
  migration_mode: verify
sessions:
  turns:
    driver: postgres
    dsn: postgres://harbor:test@pooler:6432/turns
    migration_mode: verify
observability:
  rollups:
    driver: postgres
    dsn: postgres://harbor:test@pooler:6432/rollups
    migration_mode: verify
`
	cfg, err := config.LoadFromBytes(context.Background(), []byte(yamlText))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	for _, target := range migrationModeTargets {
		if got := target.get(cfg); got != sqlmigrate.ModeVerify {
			t.Errorf("%s migration mode = %q, want verify", target.name, got)
		}
	}
}
