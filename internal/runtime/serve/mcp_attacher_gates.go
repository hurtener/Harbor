package serve

import (
	"context"
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
