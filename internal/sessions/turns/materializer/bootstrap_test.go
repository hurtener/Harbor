package materializer

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // the "pgx" database/sql driver (test-scoped schema setup)
	_ "modernc.org/sqlite"             // the "sqlite" database/sql driver (test-scoped upgrade simulation)

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/postgres"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tasks"
)

// upgradeStore is one durable store kind the upgrade bootstrap is proven
// against: it opens the store and simulates what migration 0002 records on
// a store an earlier release populated (seeded cursor, bootstrap pending).
type upgradeStore struct {
	name    string
	open    func(t *testing.T, clock func() time.Time) turns.Store
	upgrade func(t *testing.T, cursor uint64)
}

func upgradeStores(t *testing.T) []upgradeStore {
	t.Helper()
	dir := t.TempDir()
	sqliteDSN := filepath.Join(dir, "turns.sqlite")
	out := []upgradeStore{{
		name: "sqlite",
		open: func(t *testing.T, clock func() time.Time) turns.Store {
			s, err := sqlite.New(sqlite.Config{DSN: sqliteDSN, Clock: clock})
			if err != nil {
				t.Fatalf("sqlite: %v", err)
			}
			return s
		},
		upgrade: func(t *testing.T, cursor uint64) {
			db, err := sql.Open("sqlite", sqliteDSN)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Exec(`INSERT OR REPLACE INTO turn_materializer (name, cursor, bootstrap) VALUES ('turns', ?, 'pending')`, cursor); err != nil {
				t.Fatalf("simulate migration: %v", err)
			}
		},
	}}
	if base := os.Getenv("HARBOR_PG_DSN"); base != "" {
		dsn := pgSchemaDSN(t, base)
		out = append(out, upgradeStore{
			name: "postgres",
			open: func(t *testing.T, _ func() time.Time) turns.Store {
				s, err := postgres.New(postgres.Config{DSN: dsn})
				if err != nil {
					t.Fatalf("postgres: %v", err)
				}
				return s
			},
			upgrade: func(t *testing.T, cursor uint64) {
				db, err := sql.Open("pgx", dsn)
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				defer func() { _ = db.Close() }()
				if _, err := db.Exec(`
                    INSERT INTO turn_materializer (name, cursor, bootstrap) VALUES ('turns', $1, 'pending')
                    ON CONFLICT (name) DO UPDATE SET cursor = EXCLUDED.cursor, bootstrap = 'pending'`, int64(cursor)); err != nil { //nolint:gosec // G115: small test sequence
					t.Fatalf("simulate migration: %v", err)
				}
			},
		})
	}
	return out
}

// pgSchemaDSN creates a fresh schema and returns a DSN searching it.
func pgSchemaDSN(t *testing.T, base string) string {
	t.Helper()
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "harbor_turnboot_" + hex.EncodeToString(b[:])
	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("admin open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		d, err := sql.Open("pgx", base)
		if err != nil {
			return
		}
		defer func() { _ = d.Close() }()
		_, _ = d.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	q := u.Query()
	q.Set("options", "-c search_path="+schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// TestBootstrap_UpgradeRebuildsLegacyPendingAndRoutes reproduces the
// upgrade gap on SQLite and Postgres. An earlier release (simulated by an
// UNCOORDINATED materializer, which persists neither pending marks nor
// routes) projected two legacy turns: one completed while its answer was
// still missing, and one open with a child task bound to its own run.
// The migration then seeds the resume cursor far past those events. Fast
// resume alone would leave the completed turn running forever and drop the
// child's activity; the one-time bootstrap replays only the sessions that
// still hold unsealed rows, so the completion converges and the child's
// later events still fold into its root.
func TestBootstrap_UpgradeRebuildsLegacyPendingAndRoutes(t *testing.T) {
	for _, st := range upgradeStores(t) {
		t.Run(st.name, func(t *testing.T) {
			clock := newFakeClock()
			store := st.open(t, clock.now)
			proj, err := turns.New(store)
			if err != nil {
				t.Fatalf("projector: %v", err)
			}
			src := &fakeSource{}
			legacy := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "legacy"}
			other := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "busy"}
			reader := newFakeTaskReader()
			reader.set("pend", querySnap("pend", "answer me later"))
			reader.set("open", querySnap("open", "still working"))
			src.publish(t, spawnEv(legacy, "", "pend", tasks.KindForeground, ""))
			src.publish(t, startedEv(legacy, "pend"))
			src.publish(t, completedEv(legacy, "pend"))
			src.publish(t, spawnEv(legacy, "run-open", "open", tasks.KindForeground, ""))
			src.publish(t, spawnEv(legacy, "run-child", "child", tasks.KindForeground, "open"))
			src.mu.Lock()
			src.next = 50_000 // a long, busy history elsewhere
			src.mu.Unlock()
			reader.set("busy", querySnap("busy", "busy"))
			src.publish(t, spawnEv(other, "", "busy", tasks.KindForeground, ""))
			src.publish(t, failedEv(other, "busy", "timeout"))

			// The earlier release's projection: no coordination at all.
			old, err := New(src, proj, WithTaskSnapshotReader(reader))
			if err != nil {
				t.Fatalf("old materializer: %v", err)
			}
			if _, err := old.Materialize(context.Background()); err != nil {
				t.Fatalf("old pass: %v", err)
			}
			if row, err := proj.Get(context.Background(), legacy, "pend"); err != nil || row.Sealed {
				t.Fatalf("precondition: legacy pending turn = %+v, %v", row, err)
			}
			ms := store.(turns.MaterializerStore)
			if _, ok, _ := ms.LoadTaskRoute(context.Background(), legacy, "child"); ok {
				t.Fatalf("precondition: the earlier release persisted no routes")
			}
			// The upgrade: migration 0002 seeds the cursor (max checkpoint
			// minus the lookback) and marks the store for the bootstrap.
			st.upgrade(t, 50_002-10_000)

			// The answer converged while the deploy happened.
			done := querySnap("pend", "answer me later")
			done.AnswerPresent = true
			done.Answer = turns.Answer{State: turns.AnswerStateInline, Inline: "here"}
			reader.set("pend", done)

			m, err := New(src, proj,
				WithCoordination(ms), WithTaskSnapshotReader(reader),
				withClock(clock.now), withHolder("upgraded"))
			if err != nil {
				t.Fatalf("new materializer: %v", err)
			}
			pagesBefore := len(src.pageAfters)
			if _, err := m.Materialize(context.Background()); err != nil {
				t.Fatalf("upgraded pass: %v", err)
			}
			row, err := proj.Get(context.Background(), legacy, "pend")
			if err != nil || !row.Sealed || row.Answer.Inline != "here" {
				t.Fatalf("legacy completed turn after the upgrade = %+v, %v; want sealed with its answer", row, err)
			}
			if progress, _ := ms.LoadMaterializerBootstrap(context.Background()); progress != "" {
				t.Fatalf("bootstrap not finished: %q", progress)
			}
			at := time.Unix(1_700_000_900, 0)
			src.publish(t, toolInvokedEv(legacy, "run-child", "search", at))
			src.publish(t, toolCompletedEv(legacy, "run-child", "search", 5))
			if _, err := m.Materialize(context.Background()); err != nil {
				t.Fatalf("post-upgrade pass: %v", err)
			}
			open, err := proj.Get(context.Background(), legacy, "open")
			if err != nil || open.Activity.Totals.Succeeded != 1 {
				t.Fatalf("legacy open turn activity = %+v, %v; want the child's dispatch", open.Activity, err)
			}
			// The upgraded instance never paged the global log from zero.
			for _, after := range src.pageAfters[pagesBefore:] {
				if after < 40_000 {
					t.Fatalf("the upgraded instance paged the global log from %d", after)
				}
			}
			if src.sessionPageCalls == 0 {
				t.Fatalf("the bootstrap did not use targeted per-session replay")
			}
			m.releaseLease()
			_ = proj.Close(context.Background())
		})
	}
}

// TestBootstrap_ReplayDoesNotResurrectSealedRoutes: a restart that
// replays a sealed root's spawn and terminal leaves no route behind.
func TestBootstrap_ReplayDoesNotResurrectSealedRoutes(t *testing.T) {
	c := newCoordHarness(t)
	reader := newFakeTaskReader()
	for _, id := range []string{"open", "closed"} {
		reader.set(id, querySnap(id, id))
		c.src.publish(t, spawnEv(c.id, "", id, tasks.KindForeground, ""))
	}
	c.src.publish(t, failedEv(c.id, "closed", "timeout"))
	a := c.instance(t, "boot-a", reader)
	mustMaterialize(t, a)
	a.releaseLease()
	if _, ok, _ := c.ms.LoadTaskRoute(context.Background(), c.id, "closed"); ok {
		t.Fatalf("the seal did not prune")
	}
	b := c.instance(t, "boot-b", reader)
	defer b.releaseLease()
	mustMaterialize(t, b) // replays from the held-back cursor (the open turn pins it)
	if r, ok, _ := c.ms.LoadTaskRoute(context.Background(), c.id, "closed"); ok {
		t.Fatalf("replay re-created the sealed root's route: %+v", r)
	}
	if _, ok, _ := c.ms.LoadTaskRoute(context.Background(), c.id, "open"); !ok {
		t.Fatalf("the open turn's route was lost")
	}
}

// TestBootstrap_RootlessBackgroundRouteIsPruned: a background task with
// no turn keeps no route after it finishes.
func TestBootstrap_RootlessBackgroundRouteIsPruned(t *testing.T) {
	c := newCoordHarness(t)
	reader := newFakeTaskReader()
	c.src.publish(t, spawnEv(c.id, "background-run", "background", tasks.KindBackground, ""))
	c.src.publish(t, spawnEv(c.id, "child-run", "bg-child", tasks.KindBackground, "background"))
	c.src.publish(t, completedEv(c.id, "bg-child"))
	c.src.publish(t, completedEv(c.id, "background"))
	m := c.instance(t, "bg", reader)
	defer m.releaseLease()
	mustMaterialize(t, m)
	for _, id := range []string{"background", "bg-child"} {
		if r, ok, _ := c.ms.LoadTaskRoute(context.Background(), c.id, id); ok {
			t.Fatalf("finished task %s retains a route: %+v", id, r)
		}
	}
}
