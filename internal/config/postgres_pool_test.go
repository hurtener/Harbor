package config_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
)

func TestLoad_PostgresPool_UsesNestedOperatorKeys(t *testing.T) {
	base, err := os.ReadFile(validMinimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	yamlText := string(base) + `
postgres:
  pool:
    max_open: 7
    max_idle: 2
    conn_max_lifetime: 11m
    conn_max_idle_time: 17s
`
	cfg, err := config.LoadFromBytes(context.Background(), []byte(yamlText))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	pool := cfg.Postgres.Pool
	if pool.MaxOpen != 7 || pool.MaxIdle != 2 || pool.ConnMaxLifetime != 11*time.Minute || pool.ConnMaxIdleTime != 17*time.Second {
		t.Fatalf("Postgres.Pool = %+v, want nested pool values", pool)
	}
}

func TestLoad_PostgresPool_LegacyConfigWithoutBlockUsesSafeDefaults(t *testing.T) {
	cfg, err := config.Load(context.Background(), validMinimalFixture)
	if err != nil {
		t.Fatalf("Load(valid_minimal): %v", err)
	}
	pool := cfg.Postgres.Pool
	if pool.MaxOpen != 3 || pool.MaxIdle != 1 || pool.ConnMaxLifetime != 5*time.Minute || pool.ConnMaxIdleTime != 30*time.Second {
		t.Fatalf("default Postgres.Pool = %+v, want 3/1/5m/30s", pool)
	}
}

func TestLoad_PostgresPool_RejectsLegacyFlatKeys(t *testing.T) {
	base, err := os.ReadFile(validMinimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	yamlText := string(base) + `
postgres:
  max_open_conns: 7
`
	_, err = config.LoadFromBytes(context.Background(), []byte(yamlText))
	if err == nil {
		t.Fatal("LoadFromBytes accepted legacy flat postgres pool key")
	}
	if !errors.Is(err, config.ErrConfigInvalid) || !strings.Contains(err.Error(), "max_open_conns") {
		t.Fatalf("flat-key error = %v, want strict nested-key rejection", err)
	}
}

func TestValidate_PostgresPool_ReportsNestedFieldPaths(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.Postgres.Pool.MaxOpen = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "postgres.pool.max_open") {
		t.Fatalf("negative nested max_open error = %v, want postgres.pool.max_open", err)
	}

	cfg = mustLoadValid(t)
	cfg.Postgres.Pool.MaxOpen = 3
	cfg.Postgres.Pool.MaxIdle = 4
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "postgres.pool.max_idle") {
		t.Fatalf("oversized nested max_idle error = %v, want postgres.pool.max_idle", err)
	}
}
