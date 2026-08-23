package serve

// projections_test.go — the serve-band HA-64/65 projection composition:
// the unwired posture (an empty driver config resolves NO projection
// and the routes stay at 501) and the driver dispatch matrix. The
// projection stores / materializer / worker themselves are covered by
// their own phases' conformance suites; these tests pin the serve-band
// wiring, not the drivers.

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
)

func TestProjectionPostgresConfig_MapsMigrationMode(t *testing.T) {
	turnsCfg := turnsPostgresConfig(config.TurnsConfig{
		DSN:           "postgres://turns",
		MigrationMode: sqlmigrate.ModeVerify,
		Retention:     17,
	})
	if turnsCfg.DSN != "postgres://turns" || turnsCfg.MigrationMode != sqlmigrate.ModeVerify || turnsCfg.Retention != 17 {
		t.Fatalf("turns postgres config = %+v", turnsCfg)
	}

	rollupsCfg := rollupsPostgresConfig(config.RollupsConfig{
		DSN:           "postgres://rollups",
		MigrationMode: sqlmigrate.ModeVerify,
	})
	if rollupsCfg.DSN != "postgres://rollups" || rollupsCfg.MigrationMode != sqlmigrate.ModeVerify {
		t.Fatalf("rollups postgres config = %+v", rollupsCfg)
	}
}

// TestTurnsPollInterval_ExactIdleWatermarkBudget pins the production
// fallback's database-read budget. The materializer's source watch remains
// the fast path; an idle durable runtime should need exactly two fallback
// watermark reads per minute, not one read every two seconds.
func TestTurnsPollInterval_ExactIdleWatermarkBudget(t *testing.T) {
	if turnsPollInterval <= 0 {
		t.Fatalf("turnsPollInterval = %s; want a positive bounded fallback", turnsPollInterval)
	}
	if time.Minute%turnsPollInterval != 0 {
		t.Fatalf("turnsPollInterval = %s does not divide one minute exactly", turnsPollInterval)
	}
	if got, want := int(time.Minute/turnsPollInterval), 2; got != want {
		t.Fatalf("idle turns watermark reads per minute = %d; want %d", got, want)
	}
}

func projectionsLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestOpenTurnsProjection_Unwired_NoProjection pins the partial-build
// convention: an absent `sessions.turns.driver` resolves (nil, nil, nil,
// nil) — no projector, no service, no closer — and the turn routes stay
// at 501.
func TestOpenTurnsProjection_Unwired_NoProjection(t *testing.T) {
	cfg := &config.Config{}
	proj, svc, closer, err := OpenTurnsProjection(context.Background(), cfg, TurnsProjectionDeps{
		Bus: nil, Sessions: nil, Tasks: nil, Artifacts: nil, Logger: projectionsLogger(),
	})
	if err != nil {
		t.Fatalf("OpenTurnsProjection (unwired): %v", err)
	}
	if proj != nil {
		t.Fatal("OpenTurnsProjection (unwired) returned a non-nil projector")
	}
	if svc != nil {
		t.Fatal("OpenTurnsProjection (unwired) returned a non-nil service")
	}
	if closer != nil {
		t.Fatal("OpenTurnsProjection (unwired) returned a non-nil closer")
	}
}

// TestOpenRollupsProjection_Unwired_NoProjection pins the same posture
// for the observability rollup surface.
func TestOpenRollupsProjection_Unwired_NoProjection(t *testing.T) {
	cfg := &config.Config{}
	store, worker, closer, err := OpenRollupsProjection(context.Background(), cfg, RollupsProjectionDeps{
		Bus: nil, Logger: projectionsLogger(),
	})
	if err != nil {
		t.Fatalf("OpenRollupsProjection (unwired): %v", err)
	}
	if store != nil {
		t.Fatal("OpenRollupsProjection (unwired) returned a non-nil store")
	}
	if worker != nil {
		t.Fatal("OpenRollupsProjection (unwired) returned a non-nil worker")
	}
	if closer != nil {
		t.Fatal("OpenRollupsProjection (unwired) returned a non-nil closer")
	}
}

// TestOpenTurnsStore_DispatchMatrix pins the driver-name dispatch: the
// empty/`inmem` names construct the reference driver; `sqlite` requires
// a dsn (validated upstream — the sanity check still fires); an
// unknown driver fails loud with the closed set.
func TestOpenTurnsStore_DispatchMatrix(t *testing.T) {
	// Empty driver → the in-memory reference driver (dev/embedded).
	if _, err := openTurnsStore(config.TurnsConfig{}); err != nil {
		t.Fatalf("openTurnsStore (empty driver): %v", err)
	}
	if _, err := openTurnsStore(config.TurnsConfig{Driver: "inmem"}); err != nil {
		t.Fatalf("openTurnsStore (inmem): %v", err)
	}
	// SQLite without a dsn fails the sanity check; with a dsn it
	// constructs a durable driver.
	if _, err := openTurnsStore(config.TurnsConfig{Driver: "sqlite"}); err == nil {
		t.Fatal("openTurnsStore (sqlite, no dsn) must fail loud")
	}
	s, err := openTurnsStore(config.TurnsConfig{Driver: "sqlite", DSN: ":memory:", Retention: 5})
	if err != nil {
		t.Fatalf("openTurnsStore (sqlite, dsn): %v", err)
	}
	// The driver honestly reports durability by DSN: a `:memory:` SQLite
	// store does NOT survive a restart (explicit loss, never a silent
	// durability claim).
	if s.Durable() {
		t.Error("sqlite turns store with a :memory: DSN must report non-durable")
	}
	// Unknown driver names the closed set.
	if _, err := openTurnsStore(config.TurnsConfig{Driver: "oracle"}); err == nil {
		t.Fatal("openTurnsStore (unknown driver) must fail loud")
	} else if !strings.Contains(err.Error(), "known: inmem, sqlite, postgres") {
		t.Fatalf("unknown-driver error %q does not name the closed set", err)
	}
}

// TestOpenRollupsStore_DispatchMatrix mirrors the turns dispatch for
// the rollup surface.
func TestOpenRollupsStore_DispatchMatrix(t *testing.T) {
	if _, err := openRollupsStore(config.RollupsConfig{}); err != nil {
		t.Fatalf("openRollupsStore (empty driver): %v", err)
	}
	if _, err := openRollupsStore(config.RollupsConfig{Driver: "inmem"}); err != nil {
		t.Fatalf("openRollupsStore (inmem): %v", err)
	}
	if _, err := openRollupsStore(config.RollupsConfig{Driver: "sqlite"}); err == nil {
		t.Fatal("openRollupsStore (sqlite, no dsn) must fail loud")
	}
	if _, err := openRollupsStore(config.RollupsConfig{Driver: "sqlite", DSN: ":memory:"}); err != nil {
		t.Fatalf("openRollupsStore (sqlite, dsn): %v", err)
	}
	if _, err := openRollupsStore(config.RollupsConfig{Driver: "oracle"}); err == nil {
		t.Fatal("openRollupsStore (unknown driver) must fail loud")
	}
}
