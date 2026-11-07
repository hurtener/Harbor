package serve

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// attachGates serializes attach work per PHYSICAL connection name, so two
// attaches of one name (an add, a re-attach, a signed preparation) never race
// in the catalog or registry, while attaches of different names or owners run
// concurrently. Waiting is cancellable: a caller whose context ends stops
// waiting instead of queueing behind another owner's stalled network
// preparation. Cross-name invariants (separator ambiguity, owner conflict,
// duplicate staging) are enforced atomically by the registry's own staging.
// Entries are reference counted and removed with their last holder, so memory
// is bounded by in-flight attaches.
type attachGates struct {
	mu    sync.Mutex
	gates map[string]*attachGate
}

type attachGate struct {
	ch   chan struct{}
	refs int
}

func (g *attachGates) acquire(ctx context.Context, key string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.gates == nil {
		g.gates = make(map[string]*attachGate)
	}
	gate := g.gates[key]
	if gate == nil {
		gate = &attachGate{ch: make(chan struct{}, 1)}
		g.gates[key] = gate
	}
	gate.refs++
	g.mu.Unlock()
	select {
	case gate.ch <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-gate.ch
				g.put(key, gate)
			})
		}, nil
	case <-ctx.Done():
		g.put(key, gate)
		return nil, ctx.Err()
	}
}

func (g *attachGates) put(key string, gate *attachGate) {
	g.mu.Lock()
	gate.refs--
	if gate.refs == 0 && g.gates[key] == gate {
		delete(g.gates, key)
	}
	g.mu.Unlock()
}

func (g *attachGates) len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.gates)
}

// ErrAttacherClosed — the attacher was closed; no new preparation may start
// and an in-flight one may not publish.
var ErrAttacherClosed = errors.New("serve: mcp attacher is closed")

// attachLifecycle tracks in-flight attach work so Close can cancel and join
// it before draining closers: a preparation that races Close can never
// publish a live connection (or append a closer) after the drain. The zero
// value is ready to use.
type attachLifecycle struct {
	mu          sync.Mutex
	closed      bool
	inflight    int
	idle        chan struct{}
	closeCtx    context.Context
	closeCancel context.CancelFunc
}

func (l *attachLifecycle) closeContextLocked() context.Context {
	if l.closeCtx == nil {
		l.closeCtx, l.closeCancel = context.WithCancel(context.Background())
	}
	return l.closeCtx
}

// begin admits one unit of attach work. The returned context is cancelled
// when the caller's ctx ends or the attacher closes; done ends the unit and
// must be called exactly once (it is idempotent).
func (l *attachLifecycle) begin(ctx context.Context) (context.Context, func(), error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, nil, ErrAttacherClosed
	}
	l.inflight++
	closeCtx := l.closeContextLocked()
	l.mu.Unlock()
	workCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(closeCtx, cancel)
	var once sync.Once
	return workCtx, func() {
		once.Do(func() {
			stop()
			cancel()
			l.mu.Lock()
			l.inflight--
			if l.closed && l.inflight == 0 && l.idle != nil {
				close(l.idle)
				l.idle = nil
			}
			l.mu.Unlock()
		})
	}, nil
}

func (l *attachLifecycle) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// shutdown refuses new work, cancels in-flight work and waits for it to end.
func (l *attachLifecycle) shutdown(ctx context.Context) error {
	l.mu.Lock()
	l.closed = true
	l.closeContextLocked()
	l.closeCancel()
	var idle chan struct{}
	if l.inflight > 0 {
		if l.idle == nil {
			l.idle = make(chan struct{})
		}
		idle = l.idle
	}
	l.mu.Unlock()
	if idle == nil {
		return nil
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("serve: mcp attacher close: wait for in-flight attaches: %w", ctx.Err())
	}
}
