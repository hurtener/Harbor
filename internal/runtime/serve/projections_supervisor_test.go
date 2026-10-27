package serve

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/sessions/turns/materializer"
)

func TestSuperviseTurnsMaterializer_RestartsAfterTransientError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var calls atomic.Int32
	run := func(runCtx context.Context) error {
		if calls.Add(1) == 1 {
			return errors.New("postgres: load: connection reset")
		}
		<-runCtx.Done()
		return nil
	}
	done := make(chan struct{})
	go func() { defer close(done); superviseTurnsMaterializer(ctx, run, slog.Default()) }()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if calls.Load() != 2 {
		t.Fatalf("run calls = %d, want restart after transient error", calls.Load())
	}
}

func TestSuperviseTurnsMaterializer_StopsOnPermanentUnavailability(t *testing.T) {
	calls := 0
	superviseTurnsMaterializer(context.Background(), func(context.Context) error {
		calls++
		return materializer.ErrSourceUnavailable
	}, slog.Default())
	if calls != 1 {
		t.Fatalf("run calls = %d, want 1 for permanent unavailability", calls)
	}
}
