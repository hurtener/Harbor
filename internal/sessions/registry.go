package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// sessionKind is the StateStore Kind constant for session-lifecycle
// records. Centralised so callers / tests / Phase 60 Protocol mappers
// reference one symbol.
const sessionKind = "session.lifecycle"

// Option configures a Registry at construction. Only WithClock is
// exported; production code does not touch it. Tests use a fake
// clock to drive GC deterministically.
type Option func(*Registry)

// WithClock injects a custom Clock. Production code uses realClock;
// the test suite uses a controllable clock to exercise hard-cap GC
// without time.Sleep (per AGENTS.md §11).
func WithClock(c Clock) Option {
	return func(r *Registry) { r.clock = c }
}

// WithGCPolicy injects an explicit GCPolicy at construction. When
// omitted, the GCPolicy is built from the SessionsConfig defaults +
// the no-op RunningProbe. Phase 20 will replace the probe via this
// option in production wiring.
func WithGCPolicy(p GCPolicy) Option {
	return func(r *Registry) {
		p = p.withDefaults()
		r.gcPolicy = p
	}
}

// Registry is the StateStore-backed implementation of SessionRegistry.
// It is the single concrete impl in V1 — driver pluralism lives at
// the StateStore layer.
//
// Concurrency model:
//   - StateStore writes go through `state.StateStore.Save` which is
//     itself concurrent-safe per D-025.
//   - The cross-tenant SessionID-uniqueness map is guarded by mu.
//   - The sweeper goroutine's lifecycle is owned by `done` + `wg`.
type Registry struct {
	store    state.StateStore
	bus      events.EventBus
	clock    Clock
	gcPolicy GCPolicy

	// mu guards idIndex AND openSessions (both used by GC + Open).
	mu sync.Mutex
	// idIndex maps SessionID → (Tenant, User) of the most-recent
	// session opened with that SessionID. Closed sessions are NOT
	// removed (per RFC §6.9 the closed record is preserved; reopening
	// the same SessionID is forbidden regardless of tenant).
	idIndex map[string]identity.Identity
	// openSessions is the list of currently-open session quadruples
	// the GC sweeper iterates. Entries are removed on Close (operator
	// or GC). We carry an in-registry index because the StateStore
	// surface is `(Quadruple, Kind, Bytes)` with no List operation —
	// the typed wrapper layer owns enumeration.
	openSessions map[string]identity.Quadruple // SessionID → quadruple

	// Sweeper goroutine plumbing.
	done   chan struct{}
	wg     sync.WaitGroup
	closed atomic.Bool
}

// New constructs a Registry. The store and bus are required; cfg
// supplies the GC tunables (defaults applied if zero); opts can
// override the Clock or GCPolicy.
//
// The sweeper goroutine starts immediately and runs at
// gcPolicy.SweepInterval until CloseRegistry is called. The probe
// defaults to no-op until Phase 20 (TaskRegistry) plugs the real one
// via WithGCPolicy in production wiring.
func New(store state.StateStore, cfg config.SessionsConfig, bus events.EventBus, opts ...Option) (*Registry, error) {
	if store == nil {
		return nil, fmt.Errorf("sessions: New requires a non-nil StateStore")
	}
	if bus == nil {
		return nil, fmt.Errorf("sessions: New requires a non-nil EventBus")
	}
	policy := GCPolicy{
		IdleTTL:       cfg.IdleTTL,
		HardCap:       cfg.HardCap,
		SweepInterval: cfg.SweepInterval,
	}.withDefaults()
	r := &Registry{
		store:        store,
		bus:          bus,
		clock:        realClock{},
		gcPolicy:     policy,
		idIndex:      map[string]identity.Identity{},
		openSessions: map[string]identity.Quadruple{},
		done:         make(chan struct{}),
	}
	for _, opt := range opts {
		opt(r)
	}
	r.startSweeper()
	return r, nil
}

// Open creates a new session record, captures the identity triple
// immutably, and emits session.opened. Cross-tenant SessionID reuse
// and reopen-after-close are rejected; same-triple double-open is
// rejected with ErrSessionAlreadyOpen.
func (r *Registry) Open(ctx context.Context, id string, ident identity.Identity) (*Session, error) {
	if r.closed.Load() {
		return nil, ErrRegistryClosed
	}
	if err := identity.Validate(ident); err != nil {
		return nil, err
	}
	if ident.SessionID != id {
		return nil, fmt.Errorf("sessions: Open identity.SessionID=%q must match id=%q", ident.SessionID, id)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Cross-tenant SessionID uniqueness.
	if existing, ok := r.idIndex[id]; ok {
		if existing.TenantID != ident.TenantID || existing.UserID != ident.UserID {
			return nil, fmt.Errorf("%w: SessionID=%q already opened for tenant=%q user=%q",
				ErrSessionIDReuse, id, existing.TenantID, existing.UserID)
		}
	}

	// Look in the StateStore for an existing record at this triple.
	q := identity.Quadruple{Identity: ident}
	existing, err := r.store.Load(ctx, q, sessionKind)
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		return nil, fmt.Errorf("sessions: Open load: %w", err)
	}
	if err == nil {
		var stored Session
		if uerr := json.Unmarshal(existing.Bytes, &stored); uerr != nil {
			return nil, fmt.Errorf("sessions: Open unmarshal: %w", uerr)
		}
		if stored.Closed {
			return nil, fmt.Errorf("%w: SessionID=%q closed at %s reason=%q",
				ErrReopenAfterClose, id, stored.ClosedAt.Format(time.RFC3339), stored.ClosedReason)
		}
		// Open record already exists for this exact triple → already open.
		return nil, fmt.Errorf("%w: SessionID=%q tenant=%q user=%q",
			ErrSessionAlreadyOpen, id, ident.TenantID, ident.UserID)
	}

	now := r.clock.Now()
	s := Session{
		ID:       id,
		Identity: ident,
		OpenedAt: now,
		LastSeen: now,
		Context:  map[string]any{},
	}
	if err := r.save(ctx, s); err != nil {
		return nil, err
	}
	r.idIndex[id] = ident
	r.openSessions[id] = q

	// Emit session.opened.
	r.publish(ctx, q, events.Event{
		Type:     EventTypeSessionOpened,
		Identity: q,
		Payload: SessionOpenedPayload{
			SessionID: id,
			OpenedAt:  now.UnixNano(),
		},
	})
	cp := s
	return &cp, nil
}

// Get loads the session with `id` for the identity in ctx. Returns
// ErrSessionNotFound when the record is absent. Identity-mandatory:
// the ctx Identity is used to scope the StateStore read; cross-tenant
// access is prevented because the StateStore key contains the full
// triple.
func (r *Registry) Get(ctx context.Context, id string) (*Session, error) {
	if r.closed.Load() {
		return nil, ErrRegistryClosed
	}
	ident, ok := identity.From(ctx)
	if !ok {
		return nil, fmt.Errorf("sessions: Get requires identity in ctx: %w", identity.ErrIdentityMissing)
	}
	if ident.SessionID != id {
		return nil, fmt.Errorf("sessions: Get id=%q does not match ctx SessionID=%q", id, ident.SessionID)
	}
	return r.loadSession(ctx, ident)
}

// Touch updates LastSeen and re-saves. Identity-mandatory; ctx
// Identity is compared against the stored Identity, mismatch returns
// ErrIdentityMismatch. Touch on a Closed session returns
// ErrReopenAfterClose (Closed records are read-only).
func (r *Registry) Touch(ctx context.Context, id string) error {
	if r.closed.Load() {
		return ErrRegistryClosed
	}
	ident, ok := identity.From(ctx)
	if !ok {
		return fmt.Errorf("sessions: Touch requires identity in ctx: %w", identity.ErrIdentityMissing)
	}
	stored, err := r.loadSession(ctx, ident)
	if err != nil {
		return err
	}
	if !sameIdentity(stored.Identity, ident) {
		return fmt.Errorf("%w: stored=(%s,%s,%s) ctx=(%s,%s,%s)",
			ErrIdentityMismatch,
			stored.Identity.TenantID, stored.Identity.UserID, stored.Identity.SessionID,
			ident.TenantID, ident.UserID, ident.SessionID)
	}
	if stored.Closed {
		return fmt.Errorf("%w: SessionID=%q closed at %s",
			ErrReopenAfterClose, id, stored.ClosedAt.Format(time.RFC3339))
	}
	now := r.clock.Now()
	stored.LastSeen = now
	if err := r.save(ctx, *stored); err != nil {
		return err
	}
	r.publish(ctx, identity.Quadruple{Identity: ident}, events.Event{
		Type:     EventTypeSessionTouched,
		Identity: identity.Quadruple{Identity: ident},
		Payload: SessionTouchedPayload{
			SessionID: id,
			LastSeen:  now.UnixNano(),
		},
	})
	return nil
}

// Close marks the session Closed and emits session.closed. Idempotent:
// closing an already-closed session is a no-op AND preserves the
// original ClosedReason.
func (r *Registry) Close(ctx context.Context, id string, reason string) error {
	if r.closed.Load() {
		return ErrRegistryClosed
	}
	ident, ok := identity.From(ctx)
	if !ok {
		return fmt.Errorf("sessions: Close requires identity in ctx: %w", identity.ErrIdentityMissing)
	}
	stored, err := r.loadSession(ctx, ident)
	if err != nil {
		return err
	}
	if !sameIdentity(stored.Identity, ident) {
		return fmt.Errorf("%w: stored=(%s,%s,%s) ctx=(%s,%s,%s)",
			ErrIdentityMismatch,
			stored.Identity.TenantID, stored.Identity.UserID, stored.Identity.SessionID,
			ident.TenantID, ident.UserID, ident.SessionID)
	}

	r.mu.Lock()
	if stored.Closed {
		// Idempotent: original reason wins, no event re-emit.
		r.mu.Unlock()
		return nil
	}
	now := r.clock.Now()
	stored.Closed = true
	stored.ClosedAt = now
	stored.ClosedReason = reason
	if err := r.save(ctx, *stored); err != nil {
		r.mu.Unlock()
		return err
	}
	delete(r.openSessions, id)
	r.mu.Unlock()

	r.publish(ctx, identity.Quadruple{Identity: ident}, events.Event{
		Type:     EventTypeSessionClosed,
		Identity: identity.Quadruple{Identity: ident},
		Payload: SessionClosedPayload{
			SessionID: id,
			ClosedAt:  now.UnixNano(),
			Reason:    reason,
		},
	})
	return nil
}

// Inspect returns a SessionSnapshot. Running is derived from the
// configured RunningProbe at inspection time.
func (r *Registry) Inspect(ctx context.Context, id string) (*SessionSnapshot, error) {
	if r.closed.Load() {
		return nil, ErrRegistryClosed
	}
	ident, ok := identity.From(ctx)
	if !ok {
		return nil, fmt.Errorf("sessions: Inspect requires identity in ctx: %w", identity.ErrIdentityMissing)
	}
	if ident.SessionID != id {
		return nil, fmt.Errorf("sessions: Inspect id=%q does not match ctx SessionID=%q", id, ident.SessionID)
	}
	stored, err := r.loadSession(ctx, ident)
	if err != nil {
		return nil, err
	}
	q := identity.Quadruple{Identity: stored.Identity}
	running := false
	if r.gcPolicy.RunningProbe != nil {
		running, err = r.gcPolicy.RunningProbe(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("sessions: Inspect probe: %w", err)
		}
	}
	return &SessionSnapshot{Session: *stored, Running: running}, nil
}

// ListSnapshots implements sessions.SessionLister — the Phase 72c
// `search.sessions` read-side projection. Returns snapshots for every
// session the registry has seen (open OR closed) matching the filter.
//
// The registry's in-memory `idIndex` is the catalog of every SessionID
// that has been Opened during this registry's lifetime. The snapshot
// is built by Loading each matching session from the StateStore so the
// Closed / ClosedAt / LastSeen fields are current; Running is derived
// from the GCPolicy RunningProbe at inspection time (mirroring
// Inspect's contract).
//
// The caller (the search subsystem) is responsible for the auth scope
// gate — ListSnapshots does NOT re-check scope. It DOES validate that
// every supplied `TenantIDs` / `UserIDs` / `SessionIDs` entry is
// non-empty (a no-op for empty filters).
//
// Concurrent reuse (D-025): ListSnapshots only reads `idIndex` /
// `openSessions` under the registry's mutex; no per-call state lives
// on `*Registry`. One Registry serves N concurrent ListSnapshots safely.
func (r *Registry) ListSnapshots(ctx context.Context, f SessionListFilter) ([]SessionSnapshot, error) {
	if r.closed.Load() {
		return nil, ErrRegistryClosed
	}

	// Snapshot the in-memory catalogs under the lock.
	r.mu.Lock()
	type entry struct {
		id    string
		ident identity.Identity
	}
	candidates := make([]entry, 0, len(r.idIndex))
	for sid, ident := range r.idIndex {
		candidates = append(candidates, entry{id: sid, ident: ident})
	}
	r.mu.Unlock()

	tenantSet := newStringSet(f.TenantIDs)
	userSet := newStringSet(f.UserIDs)
	sessionSet := newStringSet(f.SessionIDs)

	out := make([]SessionSnapshot, 0, len(candidates))
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !tenantSet.allow(c.ident.TenantID) {
			continue
		}
		if !userSet.allow(c.ident.UserID) {
			continue
		}
		if !sessionSet.allow(c.id) {
			continue
		}
		stored, err := r.loadSession(ctx, c.ident)
		if err != nil {
			// A registry record may have been deleted out-of-band;
			// skip rather than fail the whole listing (the next
			// ListSnapshots call observes the absence too).
			continue
		}
		if !f.IncludeClosed && stored.Closed {
			continue
		}
		if !f.SinceLastSeen.IsZero() && stored.LastSeen.Before(f.SinceLastSeen) {
			continue
		}
		if !f.UntilLastSeen.IsZero() && stored.LastSeen.After(f.UntilLastSeen) {
			continue
		}
		running := false
		if r.gcPolicy.RunningProbe != nil {
			if rb, perr := r.gcPolicy.RunningProbe(ctx, identity.Quadruple{Identity: stored.Identity}); perr == nil {
				running = rb
			}
		}
		out = append(out, SessionSnapshot{Session: *stored, Running: running})
	}
	return out, nil
}

// stringSet is a small inclusion-filter helper for ListSnapshots. An
// empty set matches everything; a non-empty set matches members only.
type stringSet map[string]struct{}

func newStringSet(values []string) stringSet {
	s := make(stringSet, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		s[v] = struct{}{}
	}
	return s
}

func (s stringSet) allow(value string) bool {
	if len(s) == 0 {
		return true
	}
	_, ok := s[value]
	return ok
}

// CloseRegistry cancels the sweeper goroutine and joins it.
// Idempotent. Subsequent operations return ErrRegistryClosed.
func (r *Registry) CloseRegistry(_ context.Context) error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(r.done)
	r.wg.Wait()
	return nil
}

// loadSession is the shared "fetch the session record by SessionID"
// path. Identity is taken from ctx; the StateStore key uses the full
// triple; cross-tenant access is prevented at the StateStore level.
func (r *Registry) loadSession(ctx context.Context, ident identity.Identity) (*Session, error) {
	rec, err := r.store.Load(ctx, identity.Quadruple{Identity: ident}, sessionKind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return nil, fmt.Errorf("%w: tenant=%q user=%q session=%q",
				ErrSessionNotFound, ident.TenantID, ident.UserID, ident.SessionID)
		}
		return nil, fmt.Errorf("sessions: load: %w", err)
	}
	var s Session
	if err := json.Unmarshal(rec.Bytes, &s); err != nil {
		return nil, fmt.Errorf("sessions: unmarshal: %w", err)
	}
	return &s, nil
}

// save serialises the Session and persists it through the StateStore.
// EventID is fresh on every save; same-EventID idempotency is
// surfaced by the StateStore (Phase 07 contract).
func (r *Registry) save(ctx context.Context, s Session) error {
	bytes, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("sessions: marshal: %w", err)
	}
	rec := state.StateRecord{
		ID:        state.NewEventID(),
		Identity:  identity.Quadruple{Identity: s.Identity},
		Kind:      sessionKind,
		Bytes:     bytes,
		UpdatedAt: r.clock.Now(),
	}
	if err := r.store.Save(ctx, rec); err != nil {
		return fmt.Errorf("sessions: save: %w", err)
	}
	return nil
}

// publish best-effort emits the event onto the bus. A publish error
// is swallowed (sessions own lifecycle correctness, not bus delivery)
// — but the event was the bus's chance to surface the lifecycle
// transition; consumers that miss it can still query the StateStore.
//
// Identity-mandatory at the bus boundary: q must already carry the
// triple (caller is the registry, which always has it).
func (r *Registry) publish(ctx context.Context, _ identity.Quadruple, ev events.Event) {
	_ = r.bus.Publish(ctx, ev) //nolint:errcheck // best-effort emit; StateStore is source of truth (see doc above)
}

// sameIdentity is a triple equality check.
func sameIdentity(a, b identity.Identity) bool {
	return a.TenantID == b.TenantID && a.UserID == b.UserID && a.SessionID == b.SessionID
}

// Compile-time assertion: *Registry satisfies SessionRegistry.
var _ SessionRegistry = (*Registry)(nil)

// Compile-time assertion: *Registry satisfies SessionLister (Phase 72c).
var _ SessionLister = (*Registry)(nil)
