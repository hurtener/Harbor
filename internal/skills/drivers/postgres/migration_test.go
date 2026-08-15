package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/drivers/postgres"
)

// TestMigrate_CleanDB_StartsClean — fresh schema, run migrations,
// verify schema_migrations contains exactly one row per version
// (1, 2, 3).
func TestMigrate_CleanDB_StartsClean(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	bus := buildBus(t)

	s, err := postgres.New(skills.ConfigSnapshot{Driver: "postgres", DSN: dsn}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	versions := readSchemaMigrations(t, dsn)
	if len(versions) != 3 || versions[0] != 1 || versions[1] != 2 || versions[2] != 3 {
		t.Errorf("schema_migrations = %v, want [1 2 3]", versions)
	}
}

// TestMigrate_Idempotent — running migrations twice is a no-op against
// an existing DB. The second New() must succeed and schema_migrations
// must still contain exactly the applied versions.
func TestMigrate_Idempotent(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	bus := buildBus(t)

	s1, err := postgres.New(skills.ConfigSnapshot{Driver: "postgres", DSN: dsn}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("first postgres.New: %v", err)
	}
	_ = s1.Close(context.Background())

	s2, err := postgres.New(skills.ConfigSnapshot{Driver: "postgres", DSN: dsn}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("second postgres.New: %v", err)
	}
	defer func() { _ = s2.Close(context.Background()) }()

	versions := readSchemaMigrations(t, dsn)
	if len(versions) != 3 || versions[0] != 1 || versions[1] != 2 || versions[2] != 3 {
		t.Errorf("schema_migrations after second run = %v, want [1 2 3]", versions)
	}
}

func TestMigrate_VerifyMode_AfterApply(t *testing.T) {
	dsn := freshSchema(t, requireDSN(t))
	bus := buildBus(t)
	applyStore, err := postgres.New(skills.ConfigSnapshot{Driver: "postgres", DSN: dsn}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	_ = applyStore.Close(context.Background())
	verifyStore, err := postgres.New(skills.ConfigSnapshot{Driver: "postgres", DSN: dsn, MigrationMode: sqlmigrate.ModeVerify}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("verify applied ledger: %v", err)
	}
	_ = verifyStore.Close(context.Background())
}

// TestMigrate_Concurrent_AdvisoryLockSerializes — N goroutines call
// New() simultaneously against the same schema. The advisory lock must
// serialise migration application: every goroutine succeeds and
// schema_migrations holds exactly the applied versions (no duplicate
// inserts, no SQL errors). The multi-replica-boot guarantee (RFC §9).
func TestMigrate_Concurrent_AdvisoryLockSerializes(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	bus := buildBus(t)

	const n = 16
	var (
		wg     sync.WaitGroup
		errs   atomic.Int64
		stores = make([]skills.SkillStore, n)
	)
	wg.Add(n)
	start := make(chan struct{})
	for i := range n {
		go func() {
			defer wg.Done()
			<-start
			s, err := postgres.New(skills.ConfigSnapshot{Driver: "postgres", DSN: dsn}, skills.Deps{Bus: bus})
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
	if len(versions) != 3 || versions[0] != 1 || versions[1] != 2 || versions[2] != 3 {
		t.Errorf("schema_migrations after %d concurrent runs = %v, want [1 2 3]", n, versions)
	}
}

// TestMigrate_InstalledPackageSchema — the additive installed-package
// migration lands the two tables keyed at the session-zeroed (tenant,
// user, agent, name) target on the ScopeUser rung, each with the exact
// PRIMARY KEY that every package lookup (Get / Resolve / fence probe /
// compensation) uses.
func TestMigrate_InstalledPackageSchema(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	bus := buildBus(t)

	s, err := postgres.New(skills.ConfigSnapshot{Driver: "postgres", DSN: dsn}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Both tables exist.
	for _, table := range []string{"installed_packages", "installed_package_supports"} {
		var one int
		err := db.QueryRowContext(ctx, `
            SELECT 1 FROM information_schema.tables
            WHERE table_schema = current_schema() AND table_name = $1`, table).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("table %q missing after migration", table)
		}
		if err != nil {
			t.Fatalf("probe table %q: %v", table, err)
		}
	}

	// The exact primary-key constraints (the identity columns + the
	// exact-path key of the support manifest).
	pkCases := []struct {
		table string
		cols  string
	}{
		{table: "installed_packages", cols: "tenant_id,user_id,agent_id,name"},
		{table: "installed_package_supports", cols: "tenant_id,user_id,agent_id,name,path"},
	}
	for _, tc := range pkCases {
		var cols string
		err := db.QueryRowContext(ctx, `
            SELECT string_agg(a.attname, ',' ORDER BY u.ord)
            FROM pg_constraint pk
            JOIN LATERAL unnest(pk.conkey) WITH ORDINALITY AS u(attnum, ord) ON true
            JOIN pg_attribute a ON a.attrelid = pk.conrelid AND a.attnum = u.attnum
            WHERE pk.conrelid = (quote_ident(current_schema()) || '.' || quote_ident($1))::regclass
              AND pk.contype = 'p'
            GROUP BY pk.conname`, tc.table).Scan(&cols)
		if err != nil {
			t.Fatalf("read PK of %q: %v", tc.table, err)
		}
		if cols != tc.cols {
			t.Errorf("PRIMARY KEY of %q = %q, want %q", tc.table, cols, tc.cols)
		}
	}
}

// readSchemaMigrations returns the sorted list of versions present in
// schema_migrations.
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
