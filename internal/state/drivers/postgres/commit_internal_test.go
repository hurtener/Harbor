package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/state"
)

func TestCommitConditionalBatch_AppliedThenContextCanceledIsUnknown(t *testing.T) {
	applied := false
	err := commitConditionalBatch(context.Background(), func() error {
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
	err := commitConditionalBatch(ctx, func() error { called = true; return nil })
	if called || !errors.Is(err, context.Canceled) || errors.Is(err, state.ErrCommitOutcomeUnknown) {
		t.Fatalf("called=%v err=%v, want definite pre-commit cancellation", called, err)
	}
}
