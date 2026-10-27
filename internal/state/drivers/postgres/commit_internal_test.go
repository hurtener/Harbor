package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/state"
)

func TestCommitConditionalBatch_AppliedThenContextCanceledIsUnknown(t *testing.T) {
	applied := false
	err := commitConditionalBatch(context.Background(), context.Background(), func() error {
		applied = true
		return context.Canceled
	})
	if !applied || !errors.Is(err, state.ErrCommitOutcomeUnknown) {
		t.Fatalf("applied=%v err=%v, want applied and ErrCommitOutcomeUnknown", applied, err)
	}
}

func TestCommitConditionalBatch_PreCommitCancellationDoesNotInvokeCommit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := commitConditionalBatch(ctx, context.Background(), func() error { called = true; return nil })
	if called || !errors.Is(err, context.Canceled) || errors.Is(err, state.ErrCommitOutcomeUnknown) {
		t.Fatalf("called=%v err=%v, want definite pre-commit cancellation", called, err)
	}
}

func TestCommitConditionalBatch_ExpiredTransactionDeadlineIsDefiniteRollback(t *testing.T) {
	txCtx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	called := false
	err := commitConditionalBatch(context.Background(), txCtx, func() error { called = true; return nil })
	if called || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, state.ErrCommitOutcomeUnknown) {
		t.Fatalf("called=%v err=%v, want definite rollback before COMMIT", called, err)
	}
}

func TestCommitConditionalBatch_TxDoneIsDefiniteRollback(t *testing.T) {
	err := commitConditionalBatch(context.Background(), context.Background(), func() error { return sql.ErrTxDone })
	if !errors.Is(err, sql.ErrTxDone) || errors.Is(err, state.ErrCommitOutcomeUnknown) {
		t.Fatalf("err=%v, want definite rollback (COMMIT never sent)", err)
	}
}
