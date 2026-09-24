package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	memorysqlite "github.com/hurtener/Harbor/internal/memory/drivers/sqlite"
)

func TestSQLite_New_SupportedDSNs(t *testing.T) {
	bus, store := buildDeps(t)
	fileURI := (&url.URL{Scheme: "file", Path: filepath.Join(t.TempDir(), "memory.sqlite")}).String()
	cases := []struct{ name, dsn string }{
		{"private memory", ":memory:"},
		{"shared memory URI", "file::memory:?cache=shared"},
		{"file URI", fileURI},
		{"explicit transaction mode", fileURI + "?_txlock=immediate"},
		{"bare path with query", filepath.Join(t.TempDir(), "memory.sqlite") + "?cache=private"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := memorysqlite.New(memory.ConfigSnapshot{Driver: "sqlite", DSN: tc.dsn, Strategy: memory.StrategyNone}, memory.Deps{State: store, Bus: bus})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = m.Close(context.Background()) })
			id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, RunID: "run"}
			if _, err := m.Inspect(t.Context(), id); err != nil {
				t.Fatalf("inspect initialized adapter: %v", err)
			}
			if err := m.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Inspect(t.Context(), id); !errors.Is(err, memory.ErrStoreClosed) {
				t.Fatalf("closed adapter: %v", err)
			}
		})
	}
}

func TestSQLite_New_FailsBeforeUse(t *testing.T) {
	bus, store := buildDeps(t)
	cases := []struct {
		name, dsn string
		strategy  memory.Strategy
		deps      memory.Deps
		want      string
	}{
		{"missing state", ":memory:", memory.StrategyNone, memory.Deps{Bus: bus}, "deps.State is required"},
		{"removed strategy", ":memory:", "truncation", memory.Deps{Bus: bus, State: store}, "strategy"},
		{"malformed file URI", "file:///invalid%zz", memory.StrategyNone, memory.Deps{Bus: bus, State: store}, "augment DSN"},
		{"missing parent", filepath.Join(t.TempDir(), "missing", "memory.sqlite"), memory.StrategyNone, memory.Deps{Bus: bus, State: store}, "read journal_mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := memorysqlite.New(memory.ConfigSnapshot{DSN: tc.dsn, Strategy: tc.strategy}, tc.deps)
			if m != nil {
				t.Cleanup(func() { _ = m.Close(context.Background()) })
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || m != nil {
				t.Fatalf("store=%v error=%v; want %q", m, err, tc.want)
			}
		})
	}
}

func TestSQLite_New_ReadOnlyUninitializedSchemaFails(t *testing.T) {
	bus, store := buildDeps(t)
	path := filepath.Join(t.TempDir(), "uninitialized.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), "PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	uri := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	m, err := memorysqlite.New(memory.ConfigSnapshot{DSN: uri, Strategy: memory.StrategyNone}, memory.Deps{State: store, Bus: bus})
	if err == nil || !strings.Contains(err.Error(), "migrate:") || m != nil {
		t.Fatalf("read-only initialization: store=%v error=%v", m, err)
	}
	// Failure must not leave a partially usable adapter or mutate the schema.
	var count int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed read-only initialization created %d tables", count)
	}
}
