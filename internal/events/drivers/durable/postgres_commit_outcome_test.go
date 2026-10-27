package durable

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/state"
	statepostgres "github.com/hurtener/Harbor/internal/state/drivers/postgres"
)

// Real-Postgres acceptance for ambiguous commit handling. Each test uses a
// fresh schema so the global sequence authority starts empty.

func openPostgresCommitBus(t *testing.T, opts ...Option) (*bus, state.StateStore, *sql.DB) {
	t.Helper()
	baseDSN := os.Getenv("HARBOR_PG_DSN")
	if baseDSN == "" {
		t.Skip("HARBOR_PG_DSN not set; skipping real-Postgres commit-outcome acceptance")
	}
	dsn := durablePostgresFreshSchema(t, baseDSN)
	store, err := statepostgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("statepostgres.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("admin sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	opened, err := New(context.Background(), metadataCfg(), auditpatterns.New(), store, opts...)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	b := opened.(*bus)
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b, store, admin
}

func requireHealthyWatermark(t *testing.T, b *bus, want uint64) {
	t.Helper()
	if b.pendingCommit != nil {
		t.Fatal("pending reconciliation left behind")
	}
	if err := b.PersistenceHealthy(); err != nil {
		t.Fatalf("PersistenceHealthy = %v, want healthy", err)
	}
	if seq, _, err := b.loadSequenceAuthority(context.Background()); err != nil || seq != want {
		t.Fatalf("persisted authority = %d, %v; want %d", seq, err, want)
	}
	if wm, err := b.Watermark(context.Background()); err != nil || wm != want {
		t.Fatalf("Watermark = %d, %v; want %d", wm, err, want)
	}
}

// A commit that reached Postgres but whose acknowledgement was reported as
// unknown reconciles to success instead of poisoning the process.
func TestDurable_Postgres_AmbiguousLandedCommitReconcilesToSuccess(t *testing.T) {
	b, store, _ := openPostgresCommitBus(t)
	b.store = &unknownCommitStore{StateStore: store}
	if err := b.Publish(context.Background(), authorityTestEvent()); err != nil {
		t.Fatalf("Publish = %v, want reconciled success", err)
	}
	b.store = store
	if err := b.Publish(context.Background(), authorityTestEvent()); err != nil {
		t.Fatalf("Publish after reconciliation: %v", err)
	}
	requireHealthyWatermark(t, b, 2)
}

// Caller cancellation while COMMIT is in flight no longer aborts the
// round-trip: the detached transaction completes and the publish succeeds.
// A deferred constraint trigger makes COMMIT itself slow.
func TestDurable_Postgres_CancelDuringCommitCompletesWithoutLatch(t *testing.T) {
	b, _, admin := openPostgresCommitBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, stmt := range []string{
		`CREATE FUNCTION harbor_test_slow_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(0.4); RETURN NULL; END $$`,
		`CREATE CONSTRAINT TRIGGER harbor_test_slow_commit AFTER INSERT OR UPDATE ON state_records DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION harbor_test_slow_commit()`,
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("install slow-commit trigger: %v", err)
		}
	}
	pubCtx, pubCancel := context.WithCancel(context.Background())
	b.store = &cancelAfterStartStore{StateStore: b.store, cancel: pubCancel, after: 300 * time.Millisecond}
	if err := b.Publish(pubCtx, authorityTestEvent()); err != nil {
		t.Fatalf("Publish with cancellation during COMMIT = %v, want success", err)
	}
	if pubCtx.Err() == nil {
		t.Fatal("test did not cancel the caller context during COMMIT")
	}
	if _, err := admin.ExecContext(ctx, `DROP TRIGGER harbor_test_slow_commit ON state_records`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if err := b.Publish(context.Background(), authorityTestEvent()); err != nil {
		t.Fatalf("Publish after cancelled caller: %v", err)
	}
	requireHealthyWatermark(t, b, 2)
}

// Caller cancellation before COMMIT is a definite rollback: nothing is
// persisted, nothing is latched, and the next publish reuses the sequence.
func TestDurable_Postgres_CancelBeforeCommitIsCleanRollback(t *testing.T) {
	b, _, admin := openPostgresCommitBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Block the batch's UPSERTs behind a table lock, cancel the caller while
	// it waits, then release the lock so the transaction reaches the
	// pre-commit check with a cancelled caller.
	lockTx, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.ExecContext(ctx, `LOCK TABLE state_records IN EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock table: %v", err)
	}
	pubCtx, pubCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Publish(pubCtx, authorityTestEvent()) }()
	time.Sleep(300 * time.Millisecond)
	pubCancel()
	time.Sleep(100 * time.Millisecond)
	if err := lockTx.Rollback(); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	err = <-done
	if !errors.Is(err, context.Canceled) || errors.Is(err, state.ErrCommitOutcomeUnknown) {
		t.Fatalf("Publish = %v, want definite context.Canceled rollback", err)
	}
	requireHealthyWatermark(t, b, 0)
	if err := b.Publish(context.Background(), authorityTestEvent()); err != nil {
		t.Fatalf("Publish after clean rollback: %v", err)
	}
	requireHealthyWatermark(t, b, 1)
}

// A reconciliation read that fails leaves publication paused (not latched);
// health turns 503-worthy only after the bound; recovery clears it.
func TestDurable_Postgres_ReconcileFailureThenRecoveryClears(t *testing.T) {
	clock := &steppedClock{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	b, store, _ := openPostgresCommitBus(t, WithClock(clock))
	wrapped := &reconcileStore{StateStore: store, apply: true}
	wrapped.failLoads.Store(true)
	b.store = wrapped
	if err := b.Publish(context.Background(), authorityTestEvent()); !errors.Is(err, state.ErrCommitOutcomeUnknown) {
		t.Fatalf("Publish = %v, want unresolved ErrCommitOutcomeUnknown", err)
	}
	clock.advance(persistenceUnhealthyAfter - time.Second)
	if err := b.PersistenceHealthy(); err != nil {
		t.Fatalf("PersistenceHealthy before bound = %v", err)
	}
	clock.advance(2 * time.Second)
	if err := b.PersistenceHealthy(); err == nil {
		t.Fatal("PersistenceHealthy past bound = nil, want failure")
	}
	wrapped.failLoads.Store(false)
	wrapped.ambiguous.Store(false)
	if err := b.Publish(context.Background(), authorityTestEvent()); err != nil {
		t.Fatalf("Publish after recovery: %v", err)
	}
	requireHealthyWatermark(t, b, 2)
}

// cancelAfterStartStore cancels the caller context a fixed delay after the
// atomic batch transaction starts.
type cancelAfterStartStore struct {
	state.StateStore
	cancel context.CancelFunc
	after  time.Duration
}

func (s *cancelAfterStartStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	timer := time.AfterFunc(s.after, s.cancel)
	defer timer.Stop()
	return s.StateStore.SaveBatchIf(ctx, expectations, writes)
}
