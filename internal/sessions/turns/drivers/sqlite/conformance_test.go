package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/conformancetest"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
)

// TestConformance_Run registers the SHARED canonical correctness suite
// against the file-backed SQLite driver. Every Store contract pin the
// suite carries — identity-mandatory, per-session monotonic sequence
// minting, idempotent append, versioned mutation guards, erasure
// fencing, keyset paging with bound cursors, monotonic checkpoints,
// byte-exact renderable-DTO round-trips, deep-copy isolation, N-way
// concurrency, close semantics — must pass against this driver.
//
// The factory uses a FILE-backed store in a tempdir (not `:memory:`)
// so the suite exercises the real embedded-migration path and the
// durable erasure-fence / snapshot-generation tables, and each subtest
// gets a fresh, empty store.
func TestConformance_Run(t *testing.T) {
	conformancetest.Run(t, func() (turns.Store, func()) {
		dsn := filepath.Join(t.TempDir(), "turns.sqlite")
		s, err := sqlite.New(sqlite.Config{DSN: dsn})
		if err != nil {
			t.Fatalf("sqlite.New: %v", err)
		}
		if !s.Durable() {
			t.Fatalf("file-backed store must report Durable() == true")
		}
		return s, func() { _ = s.Close(context.Background()) }
	})
}
