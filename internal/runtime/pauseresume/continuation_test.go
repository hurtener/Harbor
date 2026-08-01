package pauseresume_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

func registerContinuation(t *testing.T, c pauseresume.Coordinator, kind string, h pauseresume.ContinuationHandler) {
	t.Helper()
	registrar, ok := c.(pauseresume.ContinuationRegistrar)
	if !ok {
		t.Fatal("coordinator does not implement ContinuationRegistrar")
	}
	if err := registrar.RegisterContinuation(kind, h); err != nil {
		t.Fatalf("RegisterContinuation: %v", err)
	}
}

func TestContinuation_RestartRehydratesAndRunsExactWork(t *testing.T) {
	store := newStore(t)
	ctx := runCtx(t, testID, "run-continuation")
	c1 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	registerContinuation(t, c1, "test.restart", func(context.Context, pauseresume.ContinuationInvocation) error { return nil })
	p, err := c1.Request(ctx, pauseresume.PauseRequest{Identity: testID, Reason: pauseresume.ReasonExternalEvent,
		Continuation: &pauseresume.Continuation{Kind: "test.restart", Data: map[string]string{"agent": "a", "fingerprint": "sha256"}}})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	c2 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	called := make(chan pauseresume.ContinuationInvocation, 1)
	registerContinuation(t, c2, "test.restart", func(_ context.Context, in pauseresume.ContinuationInvocation) error {
		called <- in
		return nil
	})
	if err := c2.Resume(ctx, p.Token, pauseresume.DecisionApprove, map[string]any{"accepted": true}); err != nil {
		t.Fatalf("Resume after restart: %v", err)
	}
	in := <-called
	if in.Continuation.Data["agent"] != "a" || in.Continuation.Data["fingerprint"] != "sha256" {
		t.Fatalf("continuation data = %#v", in.Continuation.Data)
	}
	c3 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	if _, err := c3.Status(ctx, p.Token); !errors.Is(err, pauseresume.ErrPauseNotFound) {
		t.Fatalf("checkpoint survived successful continuation: %v", err)
	}
}

func TestContinuation_HandlerOutsideCoordinatorLockAndFailureRetriable(t *testing.T) {
	ctx := runCtx(t, testID, "run-retry")
	c := pauseresume.New()
	var calls atomic.Int32
	registerContinuation(t, c, "test.retry", func(_ context.Context, in pauseresume.ContinuationInvocation) error {
		if _, err := c.Status(ctx, in.Token); err != nil {
			return err
		}
		if calls.Add(1) == 1 {
			return errors.New("injected prepare failure")
		}
		return nil
	})
	p, err := c.Request(ctx, pauseresume.PauseRequest{Identity: testID, Reason: pauseresume.ReasonExternalEvent,
		Continuation: &pauseresume.Continuation{Kind: "test.retry", Data: map[string]string{"id": "x"}}})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := c.Resume(ctx, p.Token, pauseresume.DecisionResume, nil); err == nil {
		t.Fatal("first Resume succeeded, want injected handler failure")
	}
	st, err := c.Status(ctx, p.Token)
	if err != nil || st.State != pauseresume.StatusPaused {
		t.Fatalf("failed handler did not leave checkpoint paused: state=%q err=%v", st.State, err)
	}
	if err := c.Resume(ctx, p.Token, pauseresume.DecisionResume, nil); err != nil {
		t.Fatalf("retry Resume: %v", err)
	}
}

func TestContinuation_ConcurrentResumeInvokesHandlerOnce(t *testing.T) {
	ctx := runCtx(t, testID, "run-concurrent")
	c := pauseresume.New()
	var calls atomic.Int32
	registerContinuation(t, c, "test.concurrent", func(context.Context, pauseresume.ContinuationInvocation) error {
		calls.Add(1)
		time.Sleep(10 * time.Millisecond)
		return nil
	})
	p, err := c.Request(ctx, pauseresume.PauseRequest{Identity: testID, Reason: pauseresume.ReasonExternalEvent,
		Continuation: &pauseresume.Continuation{Kind: "test.concurrent", Data: map[string]string{"id": "x"}}})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	const n = 128
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			err := c.Resume(ctx, p.Token, pauseresume.DecisionResume, nil)
			if err != nil && !errors.Is(err, pauseresume.ErrAlreadyResumed) {
				t.Errorf("Resume: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
}

func TestContinuation_ConcurrentMixedDecisionWaitsForAcceptedWinner(t *testing.T) {
	ctx := runCtx(t, testID, "run-mixed-decision")
	c := pauseresume.New()
	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	registerContinuation(t, c, "test.mixed-decision", func(context.Context, pauseresume.ContinuationInvocation) error {
		close(handlerEntered)
		<-releaseHandler
		return nil
	})
	p, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonExternalEvent,
		Continuation: &pauseresume.Continuation{
			Kind: "test.mixed-decision",
			Data: map[string]string{"id": "x"},
		},
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	approveErr := make(chan error, 1)
	go func() {
		approveErr <- c.Resume(ctx, p.Token, pauseresume.DecisionApprove, map[string]any{"winner": "approve"})
	}()
	<-handlerEntered

	rejectErr := make(chan error, 1)
	go func() {
		rejectErr <- c.Resume(ctx, p.Token, pauseresume.DecisionReject, map[string]any{"winner": "reject"})
	}()
	select {
	case err := <-rejectErr:
		t.Fatalf("reject returned while accepted continuation was in flight: %v", err)
	case <-time.After(10 * time.Millisecond):
	}

	close(releaseHandler)
	if err := <-approveErr; err != nil {
		t.Fatalf("approve winner: %v", err)
	}
	if err := <-rejectErr; !errors.Is(err, pauseresume.ErrAlreadyResumed) {
		t.Fatalf("reject loser: err=%v, want ErrAlreadyResumed", err)
	}
	st, err := c.Status(ctx, p.Token)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != pauseresume.StatusResumed || st.Decision != pauseresume.DecisionApprove {
		t.Fatalf("terminal status = {state:%q decision:%q}, want approve winner", st.State, st.Decision)
	}
}

func TestContinuation_RejectSkipsHandler(t *testing.T) {
	ctx := runCtx(t, testID, "run-reject")
	c := pauseresume.New()
	var called atomic.Bool
	registerContinuation(t, c, "test.reject", func(context.Context, pauseresume.ContinuationInvocation) error {
		called.Store(true)
		return nil
	})
	p, err := c.Request(ctx, pauseresume.PauseRequest{Identity: testID, Reason: pauseresume.ReasonExternalEvent,
		Continuation: &pauseresume.Continuation{Kind: "test.reject", Data: map[string]string{"id": "x"}}})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := c.Resume(ctx, p.Token, pauseresume.DecisionReject, nil); err != nil {
		t.Fatalf("Resume reject: %v", err)
	}
	if called.Load() {
		t.Fatal("reject invoked continuation handler")
	}
}

func TestContinuation_RequestDefensivelyCopiesData(t *testing.T) {
	ctx := runCtx(t, testID, "run-copy")
	c := pauseresume.New()
	seen := make(chan string, 1)
	registerContinuation(t, c, "test.copy", func(_ context.Context, in pauseresume.ContinuationInvocation) error {
		seen <- in.Continuation.Data["fingerprint"]
		return nil
	})
	continuation := &pauseresume.Continuation{Kind: "test.copy", Data: map[string]string{"fingerprint": "original"}}
	p, err := c.Request(ctx, pauseresume.PauseRequest{Identity: testID, Reason: pauseresume.ReasonExternalEvent, Continuation: continuation})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	continuation.Data["fingerprint"] = "mutated"
	if err := c.Resume(ctx, p.Token, pauseresume.DecisionResume, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := <-seen; got != "original" {
		t.Fatalf("handler fingerprint = %q, want defensive copy", got)
	}
}
