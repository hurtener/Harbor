// Package runsnapshot coordinates process-local agent run snapshots with
// terminal agent-config retirement.
package runsnapshot

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrAdmissionClosed means the agent's local snapshot gate is terminal.
	ErrAdmissionClosed = errors.New("agent run snapshot admission is closed")
	// ErrInvalidKey means a tenant or agent component was empty.
	ErrInvalidKey = errors.New("agent run snapshot key is incomplete")
)

type key struct {
	tenant string
	agent  string
}

type slot struct {
	active   uint64
	terminal bool
	changed  chan struct{}
}

// Gate admits immutable run snapshots until retirement durably wins, then
// permanently closes that tenant+agent key and drains only leases acquired
// before the close. It is process-local by design; the durable lifecycle
// tombstone remains the cross-process authority.
type Gate struct {
	mu    sync.Mutex
	slots map[key]*slot
}

// NewGate returns an empty process-local run-snapshot gate.
func NewGate() *Gate {
	return &Gate{slots: make(map[key]*slot)}
}

// Lease keeps one immutable run snapshot alive until Release. Release is
// idempotent and safe to defer immediately after acquisition.
type Lease struct {
	once sync.Once
	gate *Gate
	key  key
}

// Acquire atomically admits one run snapshot while the tenant+agent key is
// open. A terminal key fails closed before any config or reconcile read.
func (g *Gate) Acquire(ctx context.Context, tenant, agent string) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	k, err := validKey(tenant, agent)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, fmt.Errorf("%w: gate is nil", ErrInvalidKey)
	}
	g.mu.Lock()
	s := g.slotLocked(k)
	if s.terminal {
		g.mu.Unlock()
		return nil, fmt.Errorf("%w: tenant=%q agent=%q", ErrAdmissionClosed, tenant, agent)
	}
	s.active++
	g.mu.Unlock()
	return &Lease{gate: g, key: k}, nil
}

// Release gives up the lease. A waiter is notified only after the active
// count changes, so retirement cannot mistake an admitted run for drained.
func (l *Lease) Release() {
	if l == nil || l.gate == nil {
		return
	}
	l.once.Do(func() {
		l.gate.mu.Lock()
		s := l.gate.slots[l.key]
		if s != nil && s.active > 0 {
			s.active--
			notifyLocked(s)
		}
		l.gate.mu.Unlock()
	})
}

// Drain is a terminal close receipt. Wait may be retried after cancellation;
// sealing is never undone because it follows a durable retirement tombstone.
type Drain struct {
	gate *Gate
	key  key
}

// Seal permanently refuses new run snapshots for tenant+agent and returns an
// idempotent drain receipt for all leases admitted before the seal.
func (g *Gate) Seal(tenant, agent string) (*Drain, error) {
	k, err := validKey(tenant, agent)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, fmt.Errorf("%w: gate is nil", ErrInvalidKey)
	}
	g.mu.Lock()
	s := g.slotLocked(k)
	if !s.terminal {
		s.terminal = true
		notifyLocked(s)
	}
	g.mu.Unlock()
	return &Drain{gate: g, key: k}, nil
}

// Wait blocks until every pre-seal lease has released. Cancellation is loud
// and leaves the gate terminal; a same-operation retirement retry may Wait on
// a fresh receipt and resume cleanup.
func (d *Drain) Wait(ctx context.Context) error {
	if d == nil || d.gate == nil {
		return fmt.Errorf("%w: drain is nil", ErrInvalidKey)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		d.gate.mu.Lock()
		s := d.gate.slots[d.key]
		if s == nil || s.active == 0 {
			d.gate.mu.Unlock()
			return nil
		}
		changed := s.changed
		d.gate.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (g *Gate) slotLocked(k key) *slot {
	s := g.slots[k]
	if s == nil {
		s = &slot{changed: make(chan struct{})}
		g.slots[k] = s
	}
	return s
}

func validKey(tenant, agent string) (key, error) {
	if tenant == "" || agent == "" {
		return key{}, fmt.Errorf("%w: tenant=%q agent=%q", ErrInvalidKey, tenant, agent)
	}
	return key{tenant: tenant, agent: agent}, nil
}

func notifyLocked(s *slot) {
	close(s.changed)
	s.changed = make(chan struct{})
}
