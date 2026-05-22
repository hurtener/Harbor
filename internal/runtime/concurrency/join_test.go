package concurrency_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/runtime/concurrency"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

func mkEnv(i int) messages.Envelope {
	return messages.Envelope{
		Payload:   i,
		Headers:   messages.Headers{TenantID: "T", UserID: "U"},
		SessionID: "S",
		RunID:     fmt.Sprintf("R-%d", i),
	}
}

func TestJoinK_ReturnsKEnvelopes(t *testing.T) {
	t.Parallel()
	in := make(chan messages.Envelope, 5)
	for i := range 5 {
		in <- mkEnv(i)
	}

	got, err := concurrency.JoinK(context.Background(), in, 3)
	if err != nil {
		t.Fatalf("JoinK: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d, want 3", len(got))
	}
	for i, env := range got {
		if env.Payload.(int) != i {
			t.Errorf("got[%d].Payload=%v, want %d", i, env.Payload, i)
		}
	}
}

func TestJoinK_CancelsRemainingAfterK(t *testing.T) {
	t.Parallel()
	// Producer goroutine emits 10 envelopes onto a bounded channel,
	// honoring its own ctx. JoinK reads K=4 then we expect the
	// producer to be released by the ctx cancellation we wire in.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	in := make(chan messages.Envelope) // unbuffered — backpressure flows
	var (
		producerExited sync.WaitGroup
		emitted        int
		emittedMu      sync.Mutex
	)
	producerExited.Add(1)
	go func() {
		defer producerExited.Done()
		for i := range 10 {
			select {
			case <-ctx.Done():
				return
			case in <- mkEnv(i):
				emittedMu.Lock()
				emitted++
				emittedMu.Unlock()
			}
		}
	}()

	got, err := concurrency.JoinK(ctx, in, 4)
	if err != nil {
		t.Fatalf("JoinK: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len=%d, want 4", len(got))
	}
	// Cancel the producer's ctx now that we've drained K.
	cancel()
	producerExited.Wait()
	emittedMu.Lock()
	defer emittedMu.Unlock()
	if emitted < 4 {
		t.Errorf("producer emitted only %d, want >= 4", emitted)
	}
	if emitted >= 10 {
		t.Errorf("producer emitted all 10 — cancel did not propagate")
	}
}

func TestJoinK_CtxCancelled_ReturnsCtxErr(t *testing.T) {
	t.Parallel()
	in := make(chan messages.Envelope)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := concurrency.JoinK(ctx, in, 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if len(got) != 0 {
		t.Errorf("got=%v, want empty", got)
	}
}

func TestJoinK_ShortRead(t *testing.T) {
	t.Parallel()
	in := make(chan messages.Envelope, 2)
	in <- mkEnv(0)
	in <- mkEnv(1)
	close(in)
	got, err := concurrency.JoinK(context.Background(), in, 5)
	if !errors.Is(err, concurrency.ErrJoinKShortRead) {
		t.Fatalf("err=%v, want ErrJoinKShortRead", err)
	}
	if len(got) != 2 {
		t.Errorf("len=%d, want 2", len(got))
	}
}

func TestJoinK_InvalidK(t *testing.T) {
	t.Parallel()
	in := make(chan messages.Envelope)
	_, err := concurrency.JoinK(context.Background(), in, 0)
	if !errors.Is(err, concurrency.ErrInvalidK) {
		t.Fatalf("err=%v, want ErrInvalidK", err)
	}
}

func TestJoinK_NilChannel(t *testing.T) {
	t.Parallel()
	_, err := concurrency.JoinK(context.Background(), nil, 1)
	if err == nil {
		t.Fatal("nil channel must return error")
	}
}

// Sanity: if a producer is slow but channel eventually delivers K, we
// still return K cleanly without timing out.
func TestJoinK_SlowProducer(t *testing.T) {
	t.Parallel()
	in := make(chan messages.Envelope, 1)
	go func() {
		for i := range 3 {
			time.Sleep(5 * time.Millisecond)
			in <- mkEnv(i)
		}
	}()
	got, err := concurrency.JoinK(context.Background(), in, 3)
	if err != nil {
		t.Fatalf("JoinK: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("len=%d, want 3", len(got))
	}
}
