package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
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
	v1, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("read v1 migration: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(v1)); err != nil {
		t.Fatalf("apply v1: %v", err)
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
		t.Fatalf("forward migrate: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
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
