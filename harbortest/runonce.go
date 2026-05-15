package harbortest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem" // self-register inmem bus
	"github.com/hurtener/Harbor/internal/identity"
)

// Deps carries optional dependencies for RunOnce. The zero-value
// Deps causes RunOnce to synthesise deterministic defaults
// (canonical identity triple, fresh in-mem bus + redactor, fresh
// RunID). Callers that need cross-RunOnce coordination — e.g. a
// shared bus that captures events from multiple agents — pass an
// explicit Deps with the shared component set.
//
// Identity and RunID. When Identity is nil the kit uses a canonical
// "harbortest" triple (TenantID="harbortest", UserID="harbortest",
// SessionID="harbortest"). A nil Identity AND a non-empty RunID is
// honoured — the RunID is paired with the canonical identity. When
// the caller supplies an Identity, it must validate (non-empty
// triple) or RunOnce returns a wrapped identity.ErrIdentityIncomplete.
type Deps struct {
	// Bus is the canonical EventBus the kit subscribes to. When nil,
	// RunOnce opens a fresh in-mem bus AND closes it before
	// returning. When non-nil, the caller owns the bus's lifecycle.
	Bus events.EventBus
	// Redactor is the audit redactor RunOnce uses if it has to open
	// its own bus (Bus == nil). When Bus is non-nil this field is
	// ignored — the bus already has its redactor configured.
	Redactor audit.Redactor
	// Identity overrides the deterministic canonical identity. When
	// nil, RunOnce uses the canonical "harbortest" triple.
	Identity *identity.Identity
	// RunID overrides the auto-generated RunID. When empty, RunOnce
	// synthesises a fresh ULID-ish RunID via runIDFromCounter so
	// concurrent invocations don't collide.
	RunID string
}

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrNilAgent — RunOnce was called with a nil Agent.
	ErrNilAgent = errors.New("harbortest: RunOnce called with nil Agent")
	// ErrStackConstruction — a required component (bus, redactor)
	// could not be constructed. The wrapped error names the failing
	// component. RunOnce fails loudly per CLAUDE.md §5.
	ErrStackConstruction = errors.New("harbortest: failed to construct test stack")
)

// runCounter is the monotonic source for synthesised RunIDs. The
// counter is package-level (write-once seed + atomic increment) so
// concurrent RunOnce calls get distinct IDs without coordination.
// The shape "harbortest-run-<N>" is deterministic — tests that
// inspect RunID strings can predict them once they fix a starting
// counter via a Deps.RunID override.
//
// Per CLAUDE.md §5: package-level mutable state is allowed only for
// driver registries and metric definitions. A monotonic counter is
// neither, BUT — D-025 §5 carves out "atomic primitives for
// genuinely shared counters" explicitly. The counter is observed
// only through the synthesised RunID string, never read back, so
// the shared-counter exception applies.
var runCounter = newRunCounter()

// canonicalIdentity is the deterministic default identity the kit
// uses when no caller override is supplied. The values are stable
// across invocations so test authors can predict the triple.
var canonicalIdentity = identity.Identity{
	TenantID:  "harbortest",
	UserID:    "harbortest",
	SessionID: "harbortest",
}

// RunOnce executes agent.Run under a deterministic identity
// quadruple (or one the caller supplies via deps), captures every
// event the run emits onto the kit's event bus, and returns the
// (Output, EventLog, error) triple.
//
// The captured EventLog is built by subscribing to the bus with an
// Admin filter — that's the only way the kit can observe events
// across identity triples, which is what AssertNoLeaks needs to do
// its job. The Admin subscription causes the bus to emit one
// audit.admin_scope_used event (which itself appears in the log,
// since the subscription IS the admin caller); test authors should
// expect to see that event present alongside their agent's emits.
//
// Stack construction. When deps.Bus is nil, RunOnce opens a fresh
// in-mem bus AND closes it before returning. Construction errors
// fail loudly (CLAUDE.md §5): missing components are surfaced via
// ErrStackConstruction with the failing component named.
//
// Identity. RunOnce calls identity.WithRun on the supplied (or
// canonical) identity + RunID and passes the resulting ctx to
// agent.Run. The bus subscription closes BEFORE RunOnce returns
// so subscription goroutines do not leak past the call.
//
// Concurrent reuse (D-025). RunOnce is safe to call from N
// concurrent goroutines. Each invocation builds its own EventLog
// and its own subscription; the package-level runCounter ensures
// RunIDs do not collide even when concurrent callers omit RunID.
// A caller sharing a Deps.Bus across goroutines is responsible for
// the bus's lifetime; RunOnce never closes a caller-supplied bus.
func RunOnce(ctx context.Context, agent Agent, input any, deps ...Deps) (any, *EventLog, error) {
	if agent == nil {
		return nil, nil, ErrNilAgent
	}
	var d Deps
	if len(deps) > 0 {
		d = deps[0]
	}

	// Identity assembly.
	id := canonicalIdentity
	if d.Identity != nil {
		id = *d.Identity
	}
	if err := identity.Validate(id); err != nil {
		return nil, nil, fmt.Errorf("%w: identity: %w", ErrStackConstruction, err)
	}
	runID := d.RunID
	if runID == "" {
		runID = runCounter.next()
	}

	// Bus assembly. Caller-supplied bus is reused; otherwise we open
	// our own in-mem bus + redactor and close them on exit.
	bus := d.Bus
	closeBus := false
	if bus == nil {
		red := d.Redactor
		if red == nil {
			red = auditpatterns.New()
		}
		b, err := events.Open(context.Background(), config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 64,
			SubscriberBufferSize:     512,
			IdleTimeout:              60 * time.Second,
			DropWindow:               time.Second,
			ReplayBufferSize:         256,
		}, red)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: events.Open: %w", ErrStackConstruction, err)
		}
		bus = b
		closeBus = true
	}

	// Subscribe with Admin so we observe events across identity
	// triples — the kit must see cross-tenant emits to assert the
	// no-leak property. The bus emits an audit.admin_scope_used
	// event in response; we accept it as part of the captured log.
	subCtx, subCancel := context.WithCancel(ctx)
	sub, err := bus.Subscribe(subCtx, events.Filter{Admin: true})
	if err != nil {
		subCancel()
		if closeBus {
			_ = bus.Close(context.Background())
		}
		return nil, nil, fmt.Errorf("%w: bus.Subscribe: %w", ErrStackConstruction, err)
	}

	log := newEventLog()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ev := range sub.Events() {
			log.append(ev)
		}
	}()

	// Identity ctx for the agent. The Agent's interior reads the
	// quadruple via identity.MustQuadrupleFrom(ctx); tool drivers
	// and bus publishers do the same.
	runCtx, err := identity.With(ctx, id)
	if err != nil {
		sub.Cancel()
		subCancel()
		<-drained
		if closeBus {
			_ = bus.Close(context.Background())
		}
		return nil, nil, fmt.Errorf("%w: identity.With: %w", ErrStackConstruction, err)
	}
	runCtx, err = identity.WithRun(runCtx, id, runID)
	if err != nil {
		sub.Cancel()
		subCancel()
		<-drained
		if closeBus {
			_ = bus.Close(context.Background())
		}
		return nil, nil, fmt.Errorf("%w: identity.WithRun: %w", ErrStackConstruction, err)
	}
	runCtx = events.WithBus(runCtx, bus)

	// Execute the agent. Errors propagate; the captured log is
	// returned alongside whatever output the agent produced (the
	// caller may want to inspect the events EVEN ON FAILURE).
	output, runErr := agent.Run(runCtx, input)

	// Teardown. Cancel the subscription, wait for the drain loop to
	// flush any pending events, then close the bus if we own it.
	sub.Cancel()
	subCancel()
	// Wait for drain with a deadline so a misbehaving bus doesn't
	// pin the test forever; on timeout we still return what we have.
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		// Defensive: bus.Subscribe is documented to close the channel
		// on Cancel, so this branch SHOULD be unreachable. If it
		// fires, the test is left with whatever has been captured.
	}
	if closeBus {
		_ = bus.Close(context.Background())
	}

	return output, log, runErr
}

// runIDCounter wraps a sync.Mutex-guarded counter. The next() method
// returns a deterministic, monotonic RunID string. The shape is
// "harbortest-run-<seed>-<n>" where seed disambiguates package-test
// runs (a unique boot-time stamp).
type runIDCounter struct {
	mu   sync.Mutex
	seed string
	n    uint64
}

func newRunCounter() *runIDCounter {
	// Seed = nanoseconds at package init. Different test processes
	// see different seeds; within one process the counter is
	// monotonic. The seed is opaque to callers — the only contract
	// is uniqueness within a process AND determinism within a single
	// counter sequence.
	return &runIDCounter{seed: fmt.Sprintf("%x", time.Now().UnixNano())}
}

func (c *runIDCounter) next() string {
	c.mu.Lock()
	c.n++
	id := fmt.Sprintf("harbortest-run-%s-%d", c.seed, c.n)
	c.mu.Unlock()
	return id
}
