package concurrency_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/runtime/concurrency"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

func mkInput(n int) []messages.Envelope {
	out := make([]messages.Envelope, n)
	for i := 0; i < n; i++ {
		out[i] = messages.Envelope{
			Payload:   i,
			Headers:   messages.Headers{TenantID: "T", UserID: "U"},
			SessionID: "S",
			RunID:     fmt.Sprintf("R-%d", i),
		}
	}
	return out
}

func TestMapConcurrent_HonorsBound(t *testing.T) {
	t.Parallel()
	in := mkInput(20)
	const bound = 4

	var (
		mu        sync.Mutex
		inFlight  int
		highWater int
	)
	fn := func(ctx context.Context, env messages.Envelope) (messages.Envelope, error) {
		mu.Lock()
		inFlight++
		if inFlight > highWater {
			highWater = inFlight
		}
		mu.Unlock()

		// Hold the slot briefly so concurrent invocations can pile up.
		time.Sleep(10 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()
		return env, nil
	}
	out, err := concurrency.MapConcurrent(context.Background(), in, fn, bound)
	if err != nil {
		t.Fatalf("MapConcurrent: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len(out)=%d, want %d", len(out), len(in))
	}
	if highWater > bound {
		t.Errorf("max in-flight=%d, want <= %d", highWater, bound)
	}
	if highWater < 1 {
		t.Errorf("max in-flight=%d; expected at least 1 invocation", highWater)
	}
}

func TestMapConcurrent_PreservesOrder(t *testing.T) {
	t.Parallel()
	in := mkInput(50)
	fn := func(_ context.Context, env messages.Envelope) (messages.Envelope, error) {
		// Random-ish stagger — the goroutine scheduler may interleave
		// completion order, but output must still be in input order.
		switch v := env.Payload.(type) {
		case int:
			time.Sleep(time.Duration(v%5) * time.Millisecond)
		}
		return env, nil
	}
	out, err := concurrency.MapConcurrent(context.Background(), in, fn, 8)
	if err != nil {
		t.Fatalf("MapConcurrent: %v", err)
	}
	for i, env := range out {
		if env.Payload.(int) != i {
			t.Errorf("out[%d].Payload=%v, want %d (order broken)", i, env.Payload, i)
		}
	}
}

func TestMapConcurrent_PartialFailure_ReturnsErr(t *testing.T) {
	t.Parallel()
	in := mkInput(20)
	boom := errors.New("synthetic boom")
	var calls atomic.Int64
	fn := func(ctx context.Context, env messages.Envelope) (messages.Envelope, error) {
		calls.Add(1)
		if env.Payload.(int) == 7 {
			return messages.Envelope{}, boom
		}
		// Honor ctx so the cancel after the error short-circuits us.
		select {
		case <-ctx.Done():
			return messages.Envelope{}, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		return env, nil
	}
	_, err := concurrency.MapConcurrent(context.Background(), in, fn, 4)
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want boom", err)
	}
	// We don't insist on a specific call count (timing-dependent), but
	// a clean cancel should mean we didn't run all 20.
	if calls.Load() == int64(len(in)) {
		t.Logf("cancel race: all %d calls fired despite boom", len(in))
	}
}

func TestMapConcurrent_EmptyInput(t *testing.T) {
	t.Parallel()
	out, err := concurrency.MapConcurrent(context.Background(), nil, func(_ context.Context, e messages.Envelope) (messages.Envelope, error) {
		return e, nil
	}, 4)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out != nil {
		t.Errorf("out=%v, want nil", out)
	}
}

func TestMapConcurrent_InvalidConcurrency(t *testing.T) {
	t.Parallel()
	_, err := concurrency.MapConcurrent(context.Background(), mkInput(1), func(_ context.Context, e messages.Envelope) (messages.Envelope, error) {
		return e, nil
	}, 0)
	if !errors.Is(err, concurrency.ErrInvalidConcurrency) {
		t.Fatalf("err=%v, want ErrInvalidConcurrency", err)
	}
}

func TestMapConcurrent_NilFn(t *testing.T) {
	t.Parallel()
	_, err := concurrency.MapConcurrent(context.Background(), mkInput(1), nil, 4)
	if err == nil {
		t.Fatal("nil fn must return an error")
	}
}

func TestMapConcurrent_CtxCancelled_ReturnsCtxErr(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	_, err := concurrency.MapConcurrent(ctx, mkInput(10), func(_ context.Context, e messages.Envelope) (messages.Envelope, error) {
		return e, nil
	}, 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}
