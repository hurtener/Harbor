package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/hurtener/Harbor/internal/memory"
	memorypostgres "github.com/hurtener/Harbor/internal/memory/drivers/postgres"
	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
)

func TestPostgres_RejectedStrategyDoesNotLeakPool(t *testing.T) {
	bus, store := buildDeps(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	// Documented dummy DSN: an invalid strategy must fail before allocating or
	// contacting a database. The pgx database/sql opener itself owns a goroutine.
	cfg := memory.ConfigSnapshot{Driver: "postgres", DSN: "postgres://unused.invalid/unused", Strategy: "truncation"}
	for range 128 {
		if _, err := memorypostgres.New(cfg, memory.Deps{State: store, Bus: bus}); !errors.Is(err, memory.ErrStrategyNotImplemented) {
			t.Fatalf("removed strategy: %v", err)
		}
	}
}

func TestPostgres_NewWithDB_PreservesBorrowedPool(t *testing.T) {
	dsn := freshSchema(t, requireDSN(t))
	bus, store := buildDeps(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := memory.ConfigSnapshot{Driver: "postgres", DSN: dsn, Strategy: memory.StrategyRollingSummary}
	deps := memory.Deps{State: store, Bus: bus}

	// Verification must reject an uninitialized schema without closing the
	// runtime's pool. The same pool must remain usable to apply migrations.
	cfg.MigrationMode = sqlmigrate.ModeVerify
	if m, err := memorypostgres.NewWithDB(cfg, deps, db); err == nil || m != nil {
		t.Fatalf("verify fresh schema: store=%v err=%v", m, err)
	}
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatalf("failed initialization closed borrowed pool: %v", err)
	}
	cfg.MigrationMode = sqlmigrate.ModeApply
	m, err := memorypostgres.NewWithDB(cfg, deps, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	for range 2 {
		if err := m.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatalf("Close closed borrowed pool: %v", err)
	}

	// A new adapter can verify and reuse the same pool after the first closes.
	cfg.MigrationMode = sqlmigrate.ModeVerify
	reopened, err := memorypostgres.NewWithDB(cfg, deps, db)
	if err != nil {
		t.Fatalf("reuse borrowed pool: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
}

func TestPostgres_NewWithDB_ValidationLeavesPoolUsable(t *testing.T) {
	dsn := freshSchema(t, requireDSN(t))
	bus, store := buildDeps(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := memory.ConfigSnapshot{Driver: "postgres", DSN: dsn, Strategy: memory.StrategyNone}
	deps := memory.Deps{State: store, Bus: bus}
	cases := []struct {
		name string
		cfg  memory.ConfigSnapshot
		deps memory.Deps
		want string
	}{
		{"missing bus", cfg, memory.Deps{State: store}, "deps.Bus is required"},
		{"missing state", cfg, memory.Deps{Bus: bus}, "deps.State is required"},
		{"missing DSN", memory.ConfigSnapshot{Strategy: memory.StrategyNone}, deps, "cfg.DSN is required"},
		{"removed strategy", memory.ConfigSnapshot{DSN: dsn, Strategy: "truncation"}, deps, "strategy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := memorypostgres.NewWithDB(tc.cfg, tc.deps, db)
			if err == nil || !strings.Contains(err.Error(), tc.want) || m != nil {
				t.Fatalf("store=%v error=%v; want %q", m, err, tc.want)
			}
			if err := db.PingContext(t.Context()); err != nil {
				t.Fatalf("validation closed borrowed pool: %v", err)
			}
		})
	}
	if _, err := memorypostgres.NewWithDB(cfg, deps, nil); err == nil || !strings.Contains(err.Error(), "injected db is required") {
		t.Fatalf("nil pool: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if m, err := memorypostgres.NewWithDB(cfg, deps, db); err == nil || !strings.Contains(err.Error(), "ping:") || m != nil {
		t.Fatalf("closed pool: store=%v error=%v", m, err)
	}
}

func TestPostgres_New_FailedInitialization(t *testing.T) {
	bus, store := buildDeps(t)
	deps := memory.Deps{State: store, Bus: bus}
	t.Run("missing state", func(t *testing.T) {
		if _, err := memorypostgres.New(memory.ConfigSnapshot{}, memory.Deps{Bus: bus}); err == nil || !strings.Contains(err.Error(), "deps.State is required") {
			t.Fatalf("missing state: %v", err)
		}
	})
	t.Run("malformed DSN", func(t *testing.T) {
		if _, err := memorypostgres.New(memory.ConfigSnapshot{DSN: "postgres://%zz", Strategy: memory.StrategyNone}, deps); err == nil || !strings.Contains(err.Error(), "cannot parse") {
			t.Fatalf("malformed DSN: %v", err)
		}
	})
	t.Run("migration failure", func(t *testing.T) {
		dsn := freshSchema(t, requireDSN(t))
		defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
		cfg := memory.ConfigSnapshot{DSN: dsn, Strategy: memory.StrategyNone, MigrationMode: sqlmigrate.ModeVerify}
		if m, err := memorypostgres.New(cfg, deps); err == nil || m != nil {
			t.Fatalf("verify uninitialized schema: store=%v err=%v", m, err)
		}
	})
}
