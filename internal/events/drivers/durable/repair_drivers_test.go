package durable

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
	"github.com/hurtener/Harbor/internal/state/drivers/postgres"
	"github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

func TestLegacyRepair_SQLiteStateStoreConformance(t *testing.T) {
	store, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: t.TempDir() + "/repair.db"})
	if err != nil {
		t.Fatalf("open sqlite StateStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	seedRepairHead(t, store, "sqlite", []uint64{10, 11, 11, 12}, false)
	report, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true})
	if err != nil {
		t.Fatalf("sqlite apply: %v", err)
	}
	if report.AffectedHeadCount != 1 || report.RedundantReferenceCount != 1 {
		t.Fatalf("sqlite report = %+v", report)
	}
	if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairVerify}); err != nil {
		t.Fatalf("sqlite verify: %v", err)
	}
}

func TestLegacyRepair_PostgresDirect5432Acceptance(t *testing.T) {
	base := os.Getenv("HARBOR_PG_DSN")
	if base == "" {
		t.Skip("HARBOR_PG_DSN not set; skipping real PostgreSQL legacy-head repair acceptance")
	}
	dsn := repairFreshSchema(t, base)
	if err := ValidateLegacyRepairApplyDSN(dsn); err != nil {
		t.Fatalf("fresh schema DSN is not direct PostgreSQL 5432: %v", err)
	}
	applyStore, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn, MigrationMode: sqlmigrate.ModeApply})
	if err != nil {
		t.Fatalf("apply postgres migrations over direct 5432: %v", err)
	}
	if err := applyStore.Close(context.Background()); err != nil {
		t.Fatalf("close migration StateStore: %v", err)
	}
	store, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn, MigrationMode: sqlmigrate.ModeVerify})
	if err != nil {
		t.Fatalf("open postgres StateStore in verify mode after direct apply: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	seedRepairHead(t, store, "postgres", []uint64{20, 21, 21, 22}, false)
	report, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairApply, WriterDrained: true})
	if err != nil {
		t.Fatalf("postgres apply: %v", err)
	}
	if report.AffectedHeadCount != 1 || report.RedundantReferenceCount != 1 {
		t.Fatalf("postgres report = %+v", report)
	}
	if _, err := RepairLegacyHeads(context.Background(), store, LegacyRepairOptions{Mode: LegacyRepairVerify}); err != nil {
		t.Fatalf("postgres verify: %v", err)
	}
}

func repairFreshSchema(t *testing.T, base string) string {
	t.Helper()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("generate schema suffix: %v", err)
	}
	schema := "harbor_repair_" + hex.EncodeToString(suffix[:])
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open postgres admin: %v", err)
	}
	defer func() { _ = admin.Close() }()
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA \""+schema+"\""); err != nil {
		t.Fatalf("create repair schema: %v", err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("pgx", base)
		if err != nil {
			t.Logf("open cleanup admin: %v", err)
			return
		}
		defer func() { _ = db.Close() }()
		if _, err := db.ExecContext(context.Background(), "DROP SCHEMA \""+schema+"\" CASCADE"); err != nil {
			t.Logf("drop repair schema: %v", err)
		}
	})
	if strings.HasPrefix(base, "postgres://") || strings.HasPrefix(base, "postgresql://") {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatalf("parse postgres DSN: %v", err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return fmt.Sprintf("%s search_path=%s", base, schema)
}
