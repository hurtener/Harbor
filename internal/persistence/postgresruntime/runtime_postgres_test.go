package postgresruntime_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/hurtener/Harbor/internal/audit/drivers/noop"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/memory"
	rollupspg "github.com/hurtener/Harbor/internal/observability/rollups/drivers/postgres"
	"github.com/hurtener/Harbor/internal/persistence/postgresruntime"
	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
	turnspg "github.com/hurtener/Harbor/internal/sessions/turns/drivers/postgres"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

const postgresRuntimeDSNEnv = "HARBOR_PG_DSN"

// TestPostgresRuntime_SixStoresOneSharedDatabase is the non-vacuous hosted
// acceptance for the v1.29.1 runtime-owned PostgreSQL topology. It uses one
// private schema so the six stores can share a logical database without
// colliding with any other test running against the postgres:16 service.
//
// The local developer path intentionally skips when HARBOR_PG_DSN is absent;
// CI runs this exact test with the service DSN and asserts its PASS line.
func TestPostgresRuntime_SixStoresOneSharedDatabase(t *testing.T) {
	baseDSN := os.Getenv(postgresRuntimeDSNEnv)
	if baseDSN == "" {
		t.Skip("HARBOR_PG_DSN not set; hosted state-postgres job runs this gate")
	}
	dsn := freshSchema(t, baseDSN)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cfg := postgresConfig(dsn, sqlmigrate.ModeApply)
	rt, stores, bus := openAll(t, ctx, cfg)
	shared, ok := rt.Pools.DB(dsn)
	if !ok || shared == nil {
		t.Fatal("runtime manager did not expose the shared database pool")
	}
	for name, got := range map[string]*sql.DB{
		"state":                 shared,
		"memory":                mustDB(t, rt, dsn),
		"artifacts":             mustDB(t, rt, dsn),
		"skills":                mustDB(t, rt, dsn),
		"sessions.turns":        mustDB(t, rt, dsn),
		"observability.rollups": mustDB(t, rt, dsn),
	} {
		if got != shared {
			t.Fatalf("%s resolved a different *sql.DB; equal DSNs must share one pool", name)
		}
	}
	stats := shared.Stats()
	if stats.MaxOpenConnections != 3 || stats.Idle > 1 {
		t.Fatalf("shared pool caps open=%d idle=%d; want max 3/1", stats.MaxOpenConnections, stats.Idle)
	}
	assertCanonicalLedgers(t, ctx, shared)

	closeStores(t, ctx, stores)
	if err := bus.Close(ctx); err != nil {
		t.Fatalf("event bus close: %v", err)
	}
	if err := rt.Pools.Close(ctx); err != nil {
		t.Fatalf("first runtime pool close: %v", err)
	}
	if err := rt.Pools.Close(ctx); err != nil {
		t.Fatalf("second runtime pool close: %v", err)
	}
	if _, ok := rt.Pools.DB(dsn); ok {
		t.Fatal("runtime manager returned a pool after Close")
	}
	if err := shared.PingContext(ctx); err == nil {
		t.Fatal("shared database accepted PingContext after manager Close")
	}

	// Restart against the same schema in verify-only mode. This is the
	// transaction-pool-compatible steady-state posture and proves each
	// subsystem's canonical identity remains independently verifiable.
	verifyCfg := postgresConfig(dsn, sqlmigrate.ModeVerify)
	verifyRT, verifyStores, verifyBus := openAll(t, ctx, verifyCfg)
	verifyDB, ok := verifyRT.Pools.DB(dsn)
	if !ok || verifyDB == nil {
		t.Fatal("verify runtime did not expose the shared database pool")
	}
	assertCanonicalLedgers(t, ctx, verifyDB)
	closeStores(t, ctx, verifyStores)
	if err := verifyBus.Close(ctx); err != nil {
		t.Fatalf("verify event bus close: %v", err)
	}
	if err := verifyRT.Pools.Close(ctx); err != nil {
		t.Fatalf("verify runtime pool close: %v", err)
	}
}

type runtimeStores struct {
	state     state.StateStore
	memory    memory.MemoryStore
	artifacts interface{ Close(context.Context) error }
	skills    interface{ Close(context.Context) error }
	turns     interface{ Close(context.Context) error }
	rollups   interface{ Close(context.Context) error }
}

func openAll(t *testing.T, ctx context.Context, cfg *config.Config) (*postgresruntime.Runtime, runtimeStores, events.EventBus) {
	t.Helper()
	rt, err := postgresruntime.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("postgresruntime.Open: %v", err)
	}
	bus, err := inmem.New(cfg.Events, noop.New())
	if err != nil {
		_ = rt.Pools.Close(ctx)
		t.Fatalf("inmem event bus: %v", err)
	}
	stateStore, err := rt.State(ctx, cfg.State)
	if err != nil {
		_ = bus.Close(ctx)
		_ = rt.Pools.Close(ctx)
		t.Fatalf("state store: %v", err)
	}
	memoryStore, err := rt.Memory(ctxMemoryConfig(cfg), memory.Deps{State: stateStore, Bus: bus})
	if err != nil {
		_ = bus.Close(ctx)
		_ = rt.Pools.Close(ctx)
		t.Fatalf("memory store: %v", err)
	}
	artifactStore, err := rt.Artifacts(cfg.Artifacts)
	if err != nil {
		_ = bus.Close(ctx)
		_ = rt.Pools.Close(ctx)
		t.Fatalf("artifact store: %v", err)
	}
	skillStore, err := rt.Skills(ctxSkillsConfig(cfg), skills.Deps{Bus: bus})
	if err != nil {
		_ = bus.Close(ctx)
		_ = rt.Pools.Close(ctx)
		t.Fatalf("skills store: %v", err)
	}
	turnStore, err := rt.Turns(turnspg.Config{DSN: cfg.Sessions.Turns.DSN, MigrationMode: cfg.Sessions.Turns.MigrationMode})
	if err != nil {
		_ = bus.Close(ctx)
		_ = rt.Pools.Close(ctx)
		t.Fatalf("turns store: %v", err)
	}
	rollupStore, err := rt.Rollups(rollupspg.Config{DSN: cfg.Observability.Rollups.DSN, MigrationMode: cfg.Observability.Rollups.MigrationMode})
	if err != nil {
		_ = bus.Close(ctx)
		_ = rt.Pools.Close(ctx)
		t.Fatalf("rollups store: %v", err)
	}
	return rt, runtimeStores{
		state: stateStore, memory: memoryStore, artifacts: artifactStore,
		skills: skillStore, turns: turnStore, rollups: rollupStore,
	}, bus
}

func closeStores(t *testing.T, ctx context.Context, stores runtimeStores) {
	t.Helper()
	// Close the dependent projections before the StateStore they borrow in
	// their strategy executors.
	ordered := []struct {
		name    string
		closeFn func(context.Context) error
	}{
		{name: "rollups", closeFn: stores.rollups.Close},
		{name: "turns", closeFn: stores.turns.Close},
		{name: "skills", closeFn: stores.skills.Close},
		{name: "artifacts", closeFn: stores.artifacts.Close},
		{name: "memory", closeFn: stores.memory.Close},
		{name: "state", closeFn: stores.state.Close},
	}
	for _, store := range ordered {
		if err := store.closeFn(ctx); err != nil {
			t.Fatalf("%s close: %v", store.name, err)
		}
	}
}

func postgresConfig(dsn string, mode sqlmigrate.Mode) *config.Config {
	cfg := config.Defaults()
	cfg.Postgres.Pool.MaxOpen = 3
	cfg.Postgres.Pool.MaxIdle = 1
	cfg.Postgres.Pool.ConnMaxLifetime = 5 * time.Minute
	cfg.Postgres.Pool.ConnMaxIdleTime = 30 * time.Second
	cfg.State = config.StateConfig{Driver: "postgres", DSN: dsn, MigrationMode: mode}
	cfg.Memory = config.MemoryConfig{Driver: "postgres", DSN: dsn, MigrationMode: mode, Strategy: string(memory.StrategyNone)}
	cfg.Artifacts = config.ArtifactsConfig{Driver: "postgres", DSN: dsn, MigrationMode: mode}
	cfg.Skills = config.SkillsConfig{Driver: "postgres", DSN: dsn, MigrationMode: mode}
	cfg.Sessions.Turns = config.TurnsConfig{Driver: "postgres", DSN: dsn, MigrationMode: mode}
	cfg.Observability.Rollups = config.RollupsConfig{Driver: "postgres", DSN: dsn, MigrationMode: mode}
	cfg.Events = config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 16,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}
	return cfg
}

func ctxMemoryConfig(cfg *config.Config) memory.ConfigSnapshot {
	return memory.ConfigSnapshot{
		Driver: cfg.Memory.Driver, DSN: cfg.Memory.DSN,
		MigrationMode: cfg.Memory.MigrationMode, Strategy: memory.Strategy(cfg.Memory.Strategy),
	}
}

func ctxSkillsConfig(cfg *config.Config) skills.ConfigSnapshot {
	return skills.ConfigSnapshot{Driver: cfg.Skills.Driver, DSN: cfg.Skills.DSN, MigrationMode: cfg.Skills.MigrationMode}
}

func mustDB(t *testing.T, rt *postgresruntime.Runtime, dsn string) *sql.DB {
	t.Helper()
	db, ok := rt.Pools.DB(dsn)
	if !ok {
		t.Fatalf("runtime manager has no pool for %q", dsn)
	}
	return db
}

func assertCanonicalLedgers(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	want := map[string]bool{
		"state": true, "memory": true, "artifacts": true, "skills": true,
		"sessions.turns": true, "observability.rollups": true,
	}
	rows, err := db.QueryContext(ctx, `SELECT subsystem, COUNT(*) FROM harbor_schema_migrations GROUP BY subsystem`)
	if err != nil {
		t.Fatalf("read canonical migration ledger: %v", err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[string]int)
	for rows.Next() {
		var subsystem string
		var count int
		if err := rows.Scan(&subsystem, &count); err != nil {
			t.Fatalf("scan canonical migration ledger: %v", err)
		}
		if !want[subsystem] {
			t.Fatalf("unexpected canonical migration subsystem %q", subsystem)
		}
		if count < 1 {
			t.Fatalf("canonical migration subsystem %q has no rows", subsystem)
		}
		seen[subsystem] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate canonical migration ledger: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close canonical migration ledger rows: %v", err)
	}
	if len(seen) != len(want) {
		t.Fatalf("canonical migration subsystems=%v; want exactly %v", seen, want)
	}

	identityRows, err := db.QueryContext(ctx, `SELECT subsystem, schema_version, contract_checksum_sha256 FROM harbor_store_identity`)
	if err != nil {
		t.Fatalf("read canonical store identities: %v", err)
	}
	defer func() { _ = identityRows.Close() }()
	identitySeen := make(map[string]bool)
	for identityRows.Next() {
		var subsystem, checksum string
		var version int64
		if err := identityRows.Scan(&subsystem, &version, &checksum); err != nil {
			t.Fatalf("scan canonical store identity: %v", err)
		}
		if !want[subsystem] {
			t.Fatalf("unexpected canonical store identity subsystem %q", subsystem)
		}
		if version < 1 || !isLowerSHA256(checksum) {
			t.Fatalf("invalid identity for %q: schema_version=%d checksum=%q", subsystem, version, checksum)
		}
		identitySeen[subsystem] = true
	}
	if err := identityRows.Err(); err != nil {
		t.Fatalf("iterate canonical store identities: %v", err)
	}
	if err := identityRows.Close(); err != nil {
		t.Fatalf("close canonical store identity rows: %v", err)
	}
	if len(identitySeen) != len(want) {
		t.Fatalf("canonical store identity rows=%v; want exactly six subsystems", identitySeen)
	}
}

func isLowerSHA256(value string) bool {
	if len(value) != hex.EncodedLen(sha256Size) || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

const sha256Size = 32

func freshSchema(t *testing.T, baseDSN string) string {
	t.Helper()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("generate schema suffix: %v", err)
	}
	schema := "harbor_runtime_test_" + hex.EncodeToString(suffix[:])
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open admin postgres: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoteIdent(schema)); err != nil {
		_ = admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	_ = admin.Close()
	t.Cleanup(func() {
		drop, err := sql.Open("pgx", baseDSN)
		if err != nil {
			t.Logf("open cleanup postgres: %v", err)
			return
		}
		defer func() { _ = drop.Close() }()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer dropCancel()
		if _, err := drop.ExecContext(dropCtx, "DROP SCHEMA "+quoteIdent(schema)+" CASCADE"); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})
	return appendSearchPath(baseDSN, schema)
}

func quoteIdent(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func appendSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err == nil {
			q := u.Query()
			opts := q.Get("options")
			add := "-c search_path=" + schema
			if opts != "" {
				add = opts + " " + add
			}
			q.Set("options", add)
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	return dsn + " options='-c search_path=" + schema + "'"
}
