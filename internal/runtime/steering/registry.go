package steering

import (
	"fmt"
	"sync"

	"github.com/hurtener/Harbor/internal/identity"
)

// Registry is the process-wide owner of per-run steering inboxes.
// The Runtime builds ONE Registry per process and shares it across
// every run; it mints an Inbox when a run starts (Open), hands the
// Inbox to the Protocol edge (Lookup) and the run loop, and retires
// the Inbox when the run ends (Retire).
//
// Registry is a compiled artifact (D-025): immutable after
// construction, with the run→inbox map behind a documented-invariant
// sync.Mutex. One Registry is safe to share across N concurrent
// goroutines — concurrent_test.go pins N≥100 under -race. Per-run
// state never leaks across runs: each run's events live on its own
// Inbox, keyed by its own identity quadruple.
type Registry struct {
	clock Clock

	mu      sync.Mutex
	inboxes map[identity.Quadruple]*Inbox
}

// Option configures a Registry at construction time.
type Option func(*Registry)

// WithClock overrides the Registry's time source — the Clock each
// Inbox stamps EnqueuedAt from. Tests inject a controllable clock so
// no test sleeps for synchronisation (CLAUDE.md §11). The default is
// the real-time system clock.
func WithClock(c Clock) Option {
	return func(r *Registry) {
		if c != nil {
			r.clock = c
		}
	}
}

// NewRegistry builds a process-wide steering inbox Registry. The
// returned Registry is ready for concurrent use.
func NewRegistry(opts ...Option) *Registry {
	r := &Registry{
		clock:   systemClock{},
		inboxes: make(map[identity.Quadruple]*Inbox),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Open mints a fresh per-run Inbox for the run quadruple and
// registers it. It fails closed:
//
//   - ErrIdentityRequired — the quadruple is incomplete (missing one
//     of tenant / user / session / run).
//   - ErrInboxExists — an Inbox is already open for this exact
//     quadruple. Opening twice would orphan the first inbox's queued
//     events, so the second call is rejected loud rather than
//     silently replacing it.
//
// The returned Inbox is owned by the Registry; retire it with
// Retire when the run ends.
func (r *Registry) Open(q identity.Quadruple) (*Inbox, error) {
	if err := validateQuadruple(q); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.inboxes[q]; exists {
		return nil, fmt.Errorf("%w: %+v", ErrInboxExists, q)
	}
	in := &Inbox{
		identity: q,
		clock:    r.clock,
		notify:   make(chan struct{}, 1),
	}
	r.inboxes[q] = in
	return in, nil
}

// Lookup returns the live Inbox for the run quadruple. The Protocol
// edge calls Lookup to enqueue an inbound control onto the right
// run's inbox; the run loop calls it to Drain. It fails closed with
// ErrInboxNotFound when no Inbox is open for the quadruple (the run
// never started, or its inbox was already retired) and
// ErrIdentityRequired on an incomplete quadruple.
func (r *Registry) Lookup(q identity.Quadruple) (*Inbox, error) {
	if err := validateQuadruple(q); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	in, ok := r.inboxes[q]
	if !ok {
		return nil, fmt.Errorf("%w: %+v", ErrInboxNotFound, q)
	}
	return in, nil
}

// Retire removes the run's Inbox from the Registry and closes it: any
// queued-but-undrained events are dropped (the run is ending; there
// is nothing left to apply them to) and further Enqueue / Drain on
// the retired Inbox fail with ErrInboxNotFound. Retire fails closed
// with ErrInboxNotFound when no Inbox is open for the quadruple, and
// ErrIdentityRequired on an incomplete quadruple. Retire is the
// run-lifecycle counterpart of Open; calling it twice for the same
// run returns ErrInboxNotFound on the second call.
func (r *Registry) Retire(q identity.Quadruple) error {
	if err := validateQuadruple(q); err != nil {
		return err
	}
	r.mu.Lock()
	in, ok := r.inboxes[q]
	if ok {
		delete(r.inboxes, q)
	}
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %+v", ErrInboxNotFound, q)
	}
	in.close()
	return nil
}

// Len returns the number of currently-open inboxes. Primarily for
// tests and observability.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.inboxes)
}
