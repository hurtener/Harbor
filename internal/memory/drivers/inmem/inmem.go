// Package inmem provides the inmem driver for cumulative session-memory access.
// The injected StateStore remains the authoritative memory owner.
package inmem

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
)

// New constructs a memory driver over the supplied authoritative StateStore.
func New(cfg memory.ConfigSnapshot, deps memory.Deps) (memory.MemoryStore, error) {
	if deps.State == nil {
		return nil, fmt.Errorf("memory/inmem: deps.State is required")
	}
	if deps.Bus == nil {
		return nil, fmt.Errorf("memory/inmem: deps.Bus is required")
	}
	if err := memory.ValidateStrategy(cfg.Strategy); err != nil {
		return nil, err
	}
	return &driver{access: memory.NewAccess(cfg, deps), bus: deps.Bus}, nil
}

func init() { memory.Register("inmem", New) }

// driver is the in-memory MemoryStore. The driver itself
// owns identity-rejection emit + the closed flag; per-key state +
// strategy logic live behind the strategy executor.
//
// Concurrent-reuse contract: one instance is safe to share
// across N concurrent goroutines. The closed flag is `atomic.Bool`
// + a sync.Mutex serialises Close to guarantee idempotency.
type driver struct {
	access *memory.Access
	bus    events.EventBus

	mu     sync.Mutex
	closed atomic.Bool
}

// Inspect implements memory.MemoryStore using the execution-memory owner.
func (d *driver) Inspect(ctx context.Context, id identity.Quadruple) (memory.Inspection, error) {
	if d.closed.Load() {
		return memory.Inspection{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.Inspection{}, memory.EmitIdentityRejected(ctx, d.bus, id, "Inspect")
	}
	return d.access.Inspect(ctx, id)
}

// Put implements memory.MemoryStore and returns a committed item identity.
func (d *driver) Put(ctx context.Context, id identity.Quadruple, turn memory.ConversationTurn) (string, error) {
	if d.closed.Load() {
		return "", memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return "", memory.EmitIdentityRejected(ctx, d.bus, id, "Put")
	}
	return d.access.Put(ctx, id, turn)
}

// Delete implements memory.MemoryStore through its conditional owner mutation.
func (d *driver) Delete(ctx context.Context, id identity.Quadruple, key string) (int, error) {
	if d.closed.Load() {
		return 0, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return 0, memory.EmitIdentityRejected(ctx, d.bus, id, "Delete")
	}
	return d.access.Delete(ctx, id, key)
}

// Close implements memory.MemoryStore. Idempotent. Tears down the
// strategy executor's per-strategy resources (recovery loop
// goroutine for rolling_summary; nothing for none/truncation).
func (d *driver) Close(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed.Load() {
		return nil
	}
	d.closed.Store(true)
	return nil
}

// Compile-time assertion that *driver satisfies memory.MemoryStore.
var _ memory.MemoryStore = (*driver)(nil)
