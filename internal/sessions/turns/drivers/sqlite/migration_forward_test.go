package sqlite_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
)

// TestMigrate_Forward_FlagsExistingBrokenRows — a schema-1 file holding
// a row persisted with an unavailable query / agent migrates forward
// and the existing row is flagged for identity repair.
func TestMigrate_Forward_FlagsExistingBrokenRows(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "v1.sqlite")
	ctx := context.Background()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	v1, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("read v1 migration: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(v1)); err != nil {
		t.Fatalf("apply v1: %v", err)
	}
	id := identity.Identity{TenantID: "t-fwd", UserID: "u-fwd", SessionID: "s-fwd"}
	seq := 0
	for turnID, row := range map[string]turns.TurnRow{
		"broken":   {Query: turns.Query{Complete: turns.CompletenessUnavailable}, Agent: turns.Agent{Complete: turns.CompletenessUnavailable}},
		"complete": {Query: turns.Query{Text: "hi", Complete: turns.CompletenessComplete}, Agent: turns.Agent{ID: "a", Complete: turns.CompletenessComplete}},
	} {
		seq++
		row.TurnID = turns.TurnID(turnID)
		row.Status = turns.StatusComplete
		row.Sealed = true
		row.Version = 1
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
            INSERT INTO turn_rows (tenant, user, session, turn_id, sequence, effective_agent_id, sealed, version, dto)
            VALUES (?, ?, ?, ?, ?, '', 1, 1, ?)`, id.TenantID, id.UserID, id.SessionID, turnID, seq, b); err != nil {
			t.Fatalf("insert v1 row: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO turn_session_state (tenant, user, session, checkpoint) VALUES ('t','u','s1', 50000), ('t','u','s2', 120)`); err != nil {
		t.Fatalf("seed checkpoints: %v", err)
	}
	_ = db.Close()

	s, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("forward migrate: %v", err)
	}
	defer func() { _ = s.Close(ctx) }()
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
	row, changed, err := ms.RepairTurnIdentity(ctx, id, "broken", turns.IdentityFill{QueryPresent: true, Query: "the question"})
	if err != nil || !changed || row.Query.Text != "the question" || !row.Sealed {
		t.Fatalf("repair migrated row = %+v changed=%v err=%v", row, changed, err)
	}
}
