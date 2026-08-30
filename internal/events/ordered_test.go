package events_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

type orderedContextKey string

func TestOrderedQueue_FifoBatchAndFlushBarrier(t *testing.T) {
	t.Parallel()
	var (
		mu         sync.Mutex
		batches    [][]events.Event
		entered    = make(chan struct{})
		enteredOne sync.Once
		release    = make(chan struct{})
	)
	commit := func(ctx context.Context, batch []events.Event, _ uint64, _ string) error {
		mu.Lock()
		copyBatch := append([]events.Event(nil), batch...)
		batches = append(batches, copyBatch)
		mu.Unlock()
		enteredOne.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	q, err := events.NewOrderedQueue(4, 8, commit, nil)
	if err != nil {
		t.Fatalf("NewOrderedQueue: %v", err)
	}

	ctx := context.Background()
	first := events.Event{Type: events.EventTypeRuntimeWarning}
	second := events.Event{Type: events.EventTypeRuntimeError}
	if err := q.PublishAsync(ctx, first); err != nil {
		t.Fatalf("PublishAsync: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("queue worker did not begin first commit")
	}
	if err := q.PublishAsync(ctx, second); err != nil {
		t.Fatalf("second PublishAsync: %v", err)
	}

	ordinaryDone := make(chan error, 1)
	go func() { ordinaryDone <- q.Publish(ctx, events.Event{Type: events.EventTypeRuntimeWarning}) }()
	select {
	case err := <-ordinaryDone:
		t.Fatalf("ordinary Publish overtook blocked async commit: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-ordinaryDone:
		if err != nil {
			t.Fatalf("ordinary Publish: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary Publish did not complete after release")
	}

	if err := q.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := q.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var flattened []events.Event
	for _, batch := range batches {
		flattened = append(flattened, batch...)
	}
	if len(flattened) != 3 {
		t.Fatalf("committed event count=%d, want 3 across batches=%v", len(flattened), batches)
	}
	wantTypes := []events.EventType{first.Type, second.Type, events.EventTypeRuntimeWarning}
	for i, want := range wantTypes {
		if flattened[i].Type != want {
			t.Fatalf("committed event %d type=%q, want %q; batches=%v", i, flattened[i].Type, want, batches)
		}
	}
}

func TestOrderedQueue_AsyncCommitFailureIsReported(t *testing.T) {
	t.Parallel()
	want := errors.New("store unavailable")
	failures := make(chan error, 1)
	q, err := events.NewOrderedQueue(2, 4,
		func(_ context.Context, _ []events.Event, _ uint64, _ string) error { return want },
		func(_ context.Context, _ []events.Event, err error) { failures <- err },
	)
	if err != nil {
		t.Fatalf("NewOrderedQueue: %v", err)
	}
	if err := q.PublishAsync(context.Background(), events.Event{Type: events.EventTypeRuntimeWarning}); err != nil {
		t.Fatalf("PublishAsync: %v", err)
	}
	select {
	case err := <-failures:
		if !errors.Is(err, want) {
			t.Fatalf("failure=%v, want %v", err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("async failure was not reported")
	}
	if err := q.Flush(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Flush error=%v, want %v", err, want)
	}
	if err := q.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestOrderedQueue_AsyncAdmissionIsBounded(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	q, err := events.NewOrderedQueue(1, 1, func(ctx context.Context, _ []events.Event, _ uint64, _ string) error {
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil)
	if err != nil {
		t.Fatalf("NewOrderedQueue: %v", err)
	}
	if err := q.PublishAsync(context.Background(), events.Event{Type: events.EventTypeRuntimeWarning}); err != nil {
		t.Fatalf("first PublishAsync: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("queue worker did not begin first commit")
	}
	if err := q.PublishAsync(context.Background(), events.Event{Type: events.EventTypeRuntimeError}); err != nil {
		t.Fatalf("second PublishAsync: %v", err)
	}
	started := time.Now()
	err = q.PublishAsync(context.Background(), events.Event{Type: events.EventTypeRuntimeWarning})
	if !errors.Is(err, events.ErrAsyncQueueFull) {
		t.Fatalf("third PublishAsync error=%v, want ErrAsyncQueueFull", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("saturated PublishAsync blocked for %s", elapsed)
	}
	close(release)
	if err := q.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestOrderedQueue_AsyncContextDetachedAndBatchLimit(t *testing.T) {
	t.Parallel()
	type contextKey struct{}
	key := contextKey{}
	seen := make(chan struct {
		value any
		err   error
	}, 1)
	q, err := events.NewOrderedQueue(2, 2, func(ctx context.Context, _ []events.Event, _ uint64, _ string) error {
		seen <- struct {
			value any
			err   error
		}{value: ctx.Value(key), err: ctx.Err()}
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("NewOrderedQueue: %v", err)
	}
	tooLarge := []events.Event{
		{Type: events.EventTypeRuntimeWarning},
		{Type: events.EventTypeRuntimeWarning},
		{Type: events.EventTypeRuntimeWarning},
	}
	if err := q.PublishBatch(context.Background(), tooLarge); err == nil {
		t.Fatal("PublishBatch accepted more events than maxBatch")
	}
	callerCtx, cancel := context.WithCancel(context.WithValue(context.Background(), key, "retained"))
	if err := q.PublishAsync(callerCtx, events.Event{Type: events.EventTypeRuntimeWarning}); err != nil {
		t.Fatalf("PublishAsync: %v", err)
	}
	cancel()
	select {
	case got := <-seen:
		if got.value != "retained" {
			t.Fatalf("commit context value=%v, want retained", got.value)
		}
		if got.err != nil {
			t.Fatalf("accepted async commit inherited caller cancellation: %v", got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("detached async commit did not run")
	}
	if err := q.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestOrderedQueue_DerivedContextsSameSessionRemainFifo(t *testing.T) {
	t.Parallel()
	identity := identity.Quadruple{Identity: identity.Identity{
		TenantID: "tenant", UserID: "user", SessionID: "session",
	}}
	var (
		mu          sync.Mutex
		batches     [][]events.Event
		entered     = make(chan struct{})
		release     = make(chan struct{})
		enteredOnce sync.Once
	)
	commitCount := 0
	q, err := events.NewOrderedQueue(4, 4, func(ctx context.Context, batch []events.Event, _ uint64, _ string) error {
		mu.Lock()
		batches = append(batches, append([]events.Event(nil), batch...))
		commitCount++
		first := commitCount == 1
		mu.Unlock()
		if first {
			enteredOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("NewOrderedQueue: %v", err)
	}
	ctxA := context.WithValue(context.Background(), orderedContextKey("derived"), "a")
	ctxB := context.WithValue(context.Background(), orderedContextKey("derived"), "b")
	ctxC := context.WithValue(context.Background(), orderedContextKey("derived"), "c")
	first := events.Event{Type: events.EventTypeRuntimeWarning, Identity: identity}
	second := events.Event{Type: events.EventTypeRuntimeError, Identity: identity}
	third := events.Event{Type: events.EventTypeRuntimeWarning, Identity: identity}
	if err := q.PublishAsync(ctxA, first); err != nil {
		t.Fatalf("first PublishAsync: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("queue worker did not begin first commit")
	}
	if err := q.PublishAsync(ctxB, second); err != nil {
		t.Fatalf("second PublishAsync: %v", err)
	}
	ordinaryDone := make(chan error, 1)
	go func() { ordinaryDone <- q.Publish(ctxC, third) }()
	select {
	case err := <-ordinaryDone:
		t.Fatalf("ordinary Publish overtook earlier derived-context telemetry: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-ordinaryDone:
		if err != nil {
			t.Fatalf("ordinary Publish: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary Publish did not complete")
	}
	if err := q.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	var flattened []events.Event
	for _, batch := range batches {
		flattened = append(flattened, batch...)
	}
	if len(flattened) != 3 {
		t.Fatalf("committed event count=%d, want 3", len(flattened))
	}
	for i, want := range []events.EventType{first.Type, second.Type, third.Type} {
		if flattened[i].Type != want {
			t.Fatalf("committed event %d type=%q, want %q", i, flattened[i].Type, want)
		}
	}
}
