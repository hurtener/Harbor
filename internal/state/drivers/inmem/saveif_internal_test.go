package inmem

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

type cancelledAfterFirstCheckContext struct {
	firstCheck chan struct{}
	calls      int
}

func (c *cancelledAfterFirstCheckContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (c *cancelledAfterFirstCheckContext) Done() <-chan struct{} { return nil }

func (c *cancelledAfterFirstCheckContext) Err() error {
	c.calls++
	if c.calls == 1 {
		close(c.firstCheck)
		return nil
	}
	return context.Canceled
}

func (c *cancelledAfterFirstCheckContext) Value(any) any { return nil }

// TestSaveIf_CancelledWhileWaitingForMutexDoesNotWrite pins the cancellation
// point after SaveIf acquires the shared map mutex. A context that expired
// while another writer held the lock must not publish after the wait.
func TestSaveIf_CancelledWhileWaitingForMutexDoesNotWrite(t *testing.T) {
	store, err := New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := store.(*driver)
	defer func() { _ = d.Close(context.Background()) }()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	next := state.StateRecord{ID: "01HABXXX00000000IM", Identity: q, Kind: "conditional.cancel", Bytes: []byte("x")}

	d.mu.Lock()
	ctx := &cancelledAfterFirstCheckContext{firstCheck: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- d.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: next.Kind}}, next)
	}()
	<-ctx.firstCheck
	d.mu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("SaveIf = %v, want context.Canceled", err)
	}
	if _, err := d.Load(context.Background(), q, next.Kind); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("cancelled SaveIf persisted a record: %v", err)
	}
}
