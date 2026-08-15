package postgres_test

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/postgres"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
)

// TestMigrate_CleanDB_StartsClean — fresh schema, run migrations,
// verify both forward-only migrations are recorded.
func TestMigrate_CleanDB_StartsClean(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	versions := readSchemaMigrations(t, dsn)
	if !equalVersions(versions, []int{1, 2}) {
		t.Errorf("schema_migrations = %v, want [1 2]", versions)
	}
}

// TestMigrate_Idempotent — running migrations twice is a no-op. The
// second New() must succeed and schema_migrations must still contain
// exactly the two recorded versions.
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
	if !equalVersions(versions, []int{1, 2}) {
		t.Errorf("schema_migrations after second run = %v, want [1 2]", versions)
	}
}

func TestMigrate_VerifyMode_AfterApply(t *testing.T) {
	dsn := freshSchema(t, requireDSN(t))
	applyStore, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	_ = applyStore.Close(context.Background())
	verifyStore, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: dsn, MigrationMode: sqlmigrate.ModeVerify})
	if err != nil {
		t.Fatalf("verify applied ledger: %v", err)
	}
	_ = verifyStore.Close(context.Background())
}

// equalVersions compares two ascending version slices.
func equalVersions(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestMigrate_LegacyTaskKeyedRows_CollapseOntoTheTriple builds a schema
// in the PRE-RECONCILIATION shape by hand — the four-field primary key
// `(tenant, user, session, task, namespace, id)` with two rows differing
// only in `task` — and opens it through the driver, which runs migration
// 0002.
//
// After the migration the primary key forbids the duplicate, so no
// sequence of interface calls can construct the input. A migration whose
// data path is never executed is a migration nobody has tested.
func TestMigrate_LegacyTaskKeyedRows_CollapseOntoTheTriple(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	seed, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	ctx := context.Background()
	const v1Schema = `
		CREATE TABLE artifacts_blobs (
			tenant      TEXT     NOT NULL,
			"user"      TEXT     NOT NULL,
			session     TEXT     NOT NULL,
			task        TEXT     NOT NULL,
			namespace   TEXT     NOT NULL,
			id          TEXT     NOT NULL,
			mime_type   TEXT     NOT NULL,
			size_bytes  BIGINT   NOT NULL,
			filename    TEXT     NOT NULL,
			sha256      TEXT     NOT NULL,
			source_json BYTEA    NOT NULL,
			bytes       BYTEA    NOT NULL,
			PRIMARY KEY (tenant, "user", session, task, namespace, id)
		);
		CREATE TABLE schema_migrations (
			version    INTEGER     PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		INSERT INTO schema_migrations (version) VALUES (1);`
	if _, err := seed.ExecContext(ctx, v1Schema); err != nil {
		t.Fatalf("seed v1 schema: %v", err)
	}

	const payload = "identical bytes, two legacy rows"
	const id = "ns_0123456789ab"
	const insert = `
		INSERT INTO artifacts_blobs
			(tenant, "user", session, task, namespace, id,
			 mime_type, size_bytes, filename, sha256, source_json, bytes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`
	// Inserted LATEST-task-first so a collapse rule that silently
	// depended on physical row order would keep the wrong row.
	for _, task := range []string{"run-zulu", "run-alpha"} {
		if _, err := seed.ExecContext(ctx, insert,
			"T", "U", "S", task, "ns", id,
			"application/json", int64(len(payload)), "legacy.json",
			"deadbeef", []byte("null"), []byte(payload),
		); err != nil {
			t.Fatalf("seed row task=%s: %v", task, err)
		}
	}
	// A row in a DIFFERENT session with the same id must survive
	// untouched — the collapse is scoped to one triple.
	if _, err := seed.ExecContext(ctx, insert,
		"T", "U", "other-session", "run-zulu", "ns", id,
		"application/json", int64(len(payload)), "legacy.json",
		"deadbeef", []byte("null"), []byte(payload),
	); err != nil {
		t.Fatalf("seed sibling-session row: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed handle: %v", err)
	}

	store, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("New over a legacy schema (migration 0002 failed): %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	triple := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	rows, err := store.List(ctx, triple)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("legacy duplicates did not collapse: %d rows, want 1", len(rows))
	}
	if rows[0].Scope.TaskID != "run-alpha" {
		t.Errorf("survivor stamp=%q, want the smallest task %q",
			rows[0].Scope.TaskID, "run-alpha")
	}

	got, found, err := store.Get(ctx, triple, id)
	if err != nil || !found {
		t.Fatalf("Get on the migrated row: found=%v err=%v", found, err)
	}
	if string(got) != payload {
		t.Errorf("migrated bytes=%q, want %q", got, payload)
	}

	otherRows, err := store.List(ctx,
		artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "other-session"})
	if err != nil {
		t.Fatalf("List sibling session: %v", err)
	}
	if len(otherRows) != 1 {
		t.Errorf("sibling session lost its row: %d, want 1", len(otherRows))
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
	if !equalVersions(versions, []int{1, 2}) {
		t.Errorf("schema_migrations after %d concurrent runs = %v, want [1 2]", n, versions)
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
