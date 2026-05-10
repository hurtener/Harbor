package postgres_test

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/hurtener/Harbor/internal/artifacts/drivers/postgres"
	"github.com/hurtener/Harbor/internal/config"
)

// TestMigrate_CleanDB_StartsClean — fresh schema, run migrations,
// verify schema_migrations contains exactly one row at version 1.
func TestMigrate_CleanDB_StartsClean(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	versions := readSchemaMigrations(t, dsn)
	if len(versions) != 1 || versions[0] != 1 {
		t.Errorf("schema_migrations = %v, want [1]", versions)
	}
}

// TestMigrate_Idempotent — running migrations twice is a no-op. The
// second New() must succeed and schema_migrations must still contain
// exactly one row at version 1.
func TestMigrate_Idempotent(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s1, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("first postgres.New: %v", err)
	}
	_ = s1.Close(context.Background())

	s2, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("second postgres.New: %v", err)
	}
	defer func() { _ = s2.Close(context.Background()) }()

	versions := readSchemaMigrations(t, dsn)
	if len(versions) != 1 || versions[0] != 1 {
		t.Errorf("schema_migrations after second run = %v, want [1]", versions)
	}
}

// TestMigrate_Concurrent_AdvisoryLockSerializes — N goroutines call
// New() simultaneously against the same schema. The advisory lock
// must serialise migration application: every goroutine succeeds and
// schema_migrations holds exactly one row at version 1 (no duplicate
// inserts, no SQL errors). This is the multi-replica-boot guarantee
// per RFC §9 + brief 05 §5.
func TestMigrate_Concurrent_AdvisoryLockSerializes(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	const n = 16

	var (
		wg     sync.WaitGroup
		errs   atomic.Int64
		stores = make([]interface{ Close(context.Context) error }, n)
	)
	wg.Add(n)
	start := make(chan struct{})
	for i := range n {
		go func() {
			defer wg.Done()
			<-start
			s, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: dsn})
			if err != nil {
				errs.Add(1)
				t.Errorf("goroutine %d: New: %v", i, err)
				return
			}
			stores[i] = s
		}()
	}
	close(start)
	wg.Wait()

	for _, s := range stores {
		if s != nil {
			_ = s.Close(context.Background())
		}
	}

	if errs.Load() != 0 {
		t.Fatalf("%d concurrent migration runs errored", errs.Load())
	}

	versions := readSchemaMigrations(t, dsn)
	if len(versions) != 1 || versions[0] != 1 {
		t.Errorf("schema_migrations after %d concurrent runs = %v, want [1]", n, versions)
	}
}

// readSchemaMigrations returns the sorted list of versions present
// in schema_migrations. Used by the migration tests.
func readSchemaMigrations(t *testing.T, dsn string) []int {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("readSchemaMigrations sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx,
		"SELECT version FROM schema_migrations ORDER BY version ASC")
	if err != nil {
		t.Fatalf("select schema_migrations: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := []int{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan schema_migrations: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}
