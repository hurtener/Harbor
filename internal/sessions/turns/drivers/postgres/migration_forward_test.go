package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/postgres"
)

// TestMigrate_Forward_FlagsExistingBrokenRows — a database at schema 1
// holding a row whose query / agent were persisted unavailable (the
// rolling-deploy overlap bug) migrates forward: the materializer
// coordination table appears and the existing broken row is flagged
// for identity repair, while a complete row is not.
func TestMigrate_Forward_FlagsExistingBrokenRows(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	// Initialize the database exactly the way v1.39 did: the same runner,
	// the same immutable 0001 file, and the v1.39 store contract (which
	// records its identity row). The golden checksum pins that contract.
	v1, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("read v1 migration: %v", err)
	}
	v139FS := fstest.MapFS{"migrations/0001_init.sql": &fstest.MapFile{Data: v1}}
	if err := sqlmigrate.RunPostgresNamed(ctx, db, v139FS, v139Spec(), "turns/postgres", "harbor-turns-migrations", dsn, sqlmigrate.ModeApply); err != nil {
		t.Fatalf("initialize with the v1.39 contract: %v", err)
	}
	var version int64
	var contract string
	if err := db.QueryRowContext(ctx, `SELECT schema_version, contract_checksum_sha256 FROM harbor_store_identity WHERE subsystem = 'sessions.turns'`).Scan(&version, &contract); err != nil {
		t.Fatalf("read v1.39 identity: %v", err)
	}
	if version != 1 || contract != v139ContractChecksum {
		t.Fatalf("v1.39 identity = (%d, %s); want (1, %s)", version, contract, v139ContractChecksum)
	}
	id := identity.Identity{TenantID: "t-fwd", UserID: "u-fwd", SessionID: "s-fwd"}
	insert := func(turnID string, row turns.TurnRow) {
		row.TurnID = turns.TurnID(turnID)
		row.TieBreaker = row.TurnID
		row.Version = 1
		row.Sealed = true
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
            INSERT INTO turn_rows (tenant_id, user_id, session_id, turn_id, effective_agent_id,
                sequence, sealed, version, last_applied_event_seq, row_json)
            VALUES ($1, $2, $3, $4, '', $6, true, 1, 1, $5)`,
			id.TenantID, id.UserID, id.SessionID, turnID, string(b), len(turnID)); err != nil {
			t.Fatalf("insert v1 row: %v", err)
		}
	}
	insert("broken", turns.TurnRow{
		Status: turns.StatusComplete,
		Query:  turns.Query{Complete: turns.CompletenessUnavailable},
		Agent:  turns.Agent{Complete: turns.CompletenessUnavailable},
	})
	// A NUL-bearing stored query must not break the backfill.
	insert("complete", turns.TurnRow{
		Status: turns.StatusComplete,
		Query:  turns.Query{Text: "hi\x00there", At: time.Unix(1, 0).UTC(), Complete: turns.CompletenessComplete},
		Agent:  turns.Agent{ID: "a", Complete: turns.CompletenessComplete},
	})

	if _, err := db.ExecContext(ctx, `INSERT INTO turn_sessions (tenant_id, user_id, session_id, checkpoint) VALUES ('t','u','s1', 50000), ('t','u','s2', 120)`); err != nil {
		t.Fatalf("seed checkpoints: %v", err)
	}
	s, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("forward migrate a v1.39 database: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	if err := db.QueryRowContext(ctx, `SELECT schema_version, contract_checksum_sha256 FROM harbor_store_identity WHERE subsystem = 'sessions.turns'`).Scan(&version, &contract); err != nil {
		t.Fatalf("read migrated identity: %v", err)
	}
	if version != 2 || contract == v139ContractChecksum {
		t.Fatalf("migrated identity = (%d, %s); want version 2 on the current contract", version, contract)
	}
	// A second boot (apply) and a pooled verify boot both accept it.
	for _, mode := range []sqlmigrate.Mode{sqlmigrate.ModeApply, sqlmigrate.ModeVerify} {
		again, err := postgres.New(postgres.Config{DSN: dsn, MigrationMode: mode})
		if err != nil {
			t.Fatalf("reopen (%s) after the upgrade: %v", mode, err)
		}
		_ = again.Close(context.Background())
	}
	ms := s.(turns.MaterializerStore)
	lease, err := ms.AcquireMaterializerLease(ctx, "first-boot", time.Minute)
	if err != nil || lease.Cursor != 40000 {
		t.Fatalf("seeded resume cursor = %+v, %v; want 40000 (max checkpoint minus the lookback)", lease, err)
	}
	keys, err := ms.ListIdentityRepairCandidates(ctx, turns.IdentityRepairKey{}, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 1 || keys[0].TurnID != "broken" {
		t.Fatalf("candidates after forward migration = %+v; want only the broken row", keys)
	}
	row, changed, err := ms.RepairTurnIdentity(ctx, id, "broken", turns.IdentityFill{QueryPresent: true, Query: "the question", AgentPresent: true, AgentID: "agent"})
	if err != nil || !changed || row.Query.Text != "the question" || !row.Sealed {
		t.Fatalf("repair migrated row = %+v changed=%v err=%v", row, changed, err)
	}
}

// v139ContractChecksum is the store contract checksum v1.39 recorded for
// sessions.turns (computed by that release's runner over v139Spec).
const v139ContractChecksum = "75bd772bf5d196eea68a5b09437297a33cefda7f96f1e98fee37c499c3e27b57"

// v139Spec is the exact sessions.turns migration spec v1.39 shipped.
func v139Spec() sqlmigrate.PostgresMigrationSpec {
	return sqlmigrate.PostgresMigrationSpec{
		Subsystem:      "sessions.turns",
		RequiredTables: []string{"turn_rows", "turn_sessions"},
		RequiredColumns: map[string][]string{
			"turn_rows":     {"tenant_id", "user_id", "session_id", "turn_id", "effective_agent_id", "sequence", "sealed", "version", "last_applied_event_seq", "row_json"},
			"turn_sessions": {"tenant_id", "user_id", "session_id", "next_seq", "checkpoint", "snapshot", "truncated", "fenced"},
		},
	}
}

// TestMigrate_PreviousContractOnlyAtItsVersion: the previous contract is
// accepted only at the schema version it described; an identity claiming a
// newer version under the old contract fails closed.
func TestMigrate_PreviousContractOnlyAtItsVersion(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	s, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_ = s.Close(context.Background())
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE harbor_store_identity SET contract_checksum_sha256 = $1 WHERE subsystem = 'sessions.turns'`, v139ContractChecksum); err != nil {
		t.Fatalf("tamper identity: %v", err)
	}
	if s, err := postgres.New(postgres.Config{DSN: dsn}); err == nil {
		_ = s.Close(context.Background())
		t.Fatalf("a version-2 store carrying the version-1 contract was accepted")
	}
}
