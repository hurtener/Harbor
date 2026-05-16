package pauseresume

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/state"
)

// coordinator is the V1 process-local Coordinator implementation.
//
// Concurrent reuse contract (D-025): every field below is either set
// once at construction (store, registry, bus, now — all immutable
// after New returns) or is the registry map guarded by mu. There is no
// per-run state on the struct: Request / Resume / Status read their
// run-specific data from ctx + arguments. One coordinator is safe to
// share across N goroutines; concurrent_test.go pins N≥100 under
// -race.
type coordinator struct {
	// store is the OPTIONAL checkpoint store. nil ⇒ pauses are
	// process-local only and do not survive Runtime restart. Set once
	// at construction.
	store state.StateStore
	// registry is the handle registry for re-attaching the
	// non-serialisable half of ToolContext on resume. Always non-nil
	// (New defaults to a process-local registry). Set once at
	// construction; the registry is itself internally synchronised
	// (sync.Map-backed).
	registry trajectory.HandleRegistry
	// bus is the OPTIONAL event bus. nil ⇒ no events emitted. Set once
	// at construction.
	bus events.EventBus
	// now is the clock. Defaults to time.Now; overridable for tests
	// (CLAUDE.md §11 — time-sensitive tests use a controllable clock).
	// Set once at construction.
	now func() time.Time

	// mu guards pauses. The map is the coordinator's only mutable
	// state and is documented internally-synchronised per the D-025
	// concurrent-reuse contract (CLAUDE.md §5).
	mu sync.Mutex
	// pauses is the process-local pause registry, keyed by Token.
	pauses map[Token]*pauseEntry
}

// pauseEntry is the in-memory pause record. Guarded by coordinator.mu;
// never escapes the coordinator (callers receive value copies via the
// Pause / Status return types).
type pauseEntry struct {
	token      Token
	reason     Reason
	state      State
	identity   identity.Identity
	runID      string
	payload    map[string]any
	pausedAt   time.Time
	resumedAt  time.Time
	trajectory *trajectory.Trajectory
}

// Option configures a coordinator at construction. Options are applied
// in order; later options override earlier ones for the same field.
type Option func(*coordinator)

// WithCheckpointStore hands the Coordinator a state.StateStore for
// durable pauses. When set, Request persists every pause record and a
// fresh Coordinator over the same store rehydrates pauses on demand —
// pauses survive a Runtime restart. When NOT set, pauses are
// process-local only and explicitly do not survive restart.
//
// Phase 50 deliberately does not mint a parallel persistence-driver
// seam: state.StateStore is already the §4.4 persistence seam (three
// V1 drivers at conformance parity). See D-067.
func WithCheckpointStore(s state.StateStore) Option {
	return func(c *coordinator) { c.store = s }
}

// WithHandleRegistry overrides the handle registry used to re-attach
// the non-serialisable half of ToolContext on resume. Defaults to a
// fresh process-local registry (trajectory.NewProcessLocalRegistry).
// Pass a shared registry when tool dispatch and pause/resume must see
// the same handle table.
func WithHandleRegistry(r trajectory.HandleRegistry) Option {
	return func(c *coordinator) {
		if r != nil {
			c.registry = r
		}
	}
}

// WithClock overrides the wall-clock source. Defaults to time.Now.
// Tests pass a controllable clock so PausedAt / ResumedAt are
// deterministic (CLAUDE.md §11).
func WithClock(now func() time.Time) Option {
	return func(c *coordinator) {
		if now != nil {
			c.now = now
		}
	}
}

// WithBus hands the Coordinator an event bus. When set, Request emits
// pause.requested and Resume emits pause.resumed. When not set, no
// events are emitted (the Coordinator still functions — event
// emission is observability, not correctness).
func WithBus(b events.EventBus) Option {
	return func(c *coordinator) { c.bus = b }
}

// New constructs the V1 process-local Coordinator. The returned value
// is immutable after construction (D-025) and safe for concurrent use
// by N goroutines.
//
// With no options, the Coordinator is fully process-local: no
// checkpoint store (pauses do not survive restart), a fresh
// process-local handle registry, no event bus, time.Now as the clock.
func New(opts ...Option) Coordinator {
	c := &coordinator{
		registry: trajectory.NewProcessLocalRegistry(),
		now:      time.Now,
		pauses:   make(map[Token]*pauseEntry),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// newToken mints a fresh opaque Token. ULID-shaped: monotonic-ish,
// lexicographically sortable, crypto-strong entropy. ulid.MustNew with
// crypto/rand.Reader is safe for concurrent use (rand.Reader is
// concurrency-safe; ulid.MustNew does no shared-state mutation with a
// stateless entropy source).
func newToken() Token {
	return Token(ulid.MustNew(ulid.Now(), rand.Reader).String())
}

// Request records a pause and returns its opaque Token. See the
// Coordinator interface godoc for the full contract.
func (c *coordinator) Request(ctx context.Context, req PauseRequest) (Pause, error) {
	if err := ctx.Err(); err != nil {
		return Pause{}, fmt.Errorf("pauseresume: request cancelled: %w", err)
	}
	if err := identity.Validate(req.Identity); err != nil {
		return Pause{}, fmt.Errorf("%w: %v", ErrIdentityRequired, err)
	}
	if !IsValidReason(req.Reason) {
		return Pause{}, fmt.Errorf("%w: %q", ErrInvalidReason, req.Reason)
	}

	// Fail-loudly serialise contract (Phase 51 / D-069): the pause
	// Payload is the pause record's caller-controlled wire shape — it
	// MUST be JSON-encodable whether or not a checkpoint store is
	// configured. A non-encodable leaf is rejected LOUD here, before a
	// Token is minted or anything is recorded — never silently carried
	// on a process-local-only pause that could never round-trip
	// (RFC §3.4 — no silent degradation). When a checkpoint store IS
	// configured, the full envelope is re-walked by SerializeRecord
	// below; this pre-check makes the no-store path fail-loud too.
	if req.Payload != nil {
		// Root at "PauseRecord.payload" — the canonical envelope
		// vocabulary the plan + glossary use, and the same root
		// SerializeRecord's full-envelope walk produces for a bad
		// payload leaf. One operator-facing field-path vocabulary
		// whether the leaf is caught here or in SerializeRecord.
		if err := trajectory.ValidateEncodable(req.Payload, "PauseRecord.payload"); err != nil {
			// trajectory.ErrUnserializable propagates verbatim — the
			// caller reaches it via errors.As. No Token minted, no pause
			// recorded, no checkpoint written.
			return Pause{}, err
		}
	}

	token := newToken()
	pausedAt := c.now()

	entry := &pauseEntry{
		token:      token,
		reason:     req.Reason,
		state:      StatusPaused,
		identity:   req.Identity,
		runID:      runIDFromContext(ctx),
		payload:    cloneStringMap(req.Payload),
		pausedAt:   pausedAt,
		trajectory: req.Trajectory,
	}

	// Persist the checkpoint BEFORE recording in the in-memory
	// registry: if serialisation fails, the pause is rejected loud and
	// nothing — neither the store nor the registry — is mutated. No
	// half-persist (RFC §3.4, CLAUDE.md §13).
	if c.store != nil {
		rec, err := entry.toCheckpoint()
		if err != nil {
			// trajectory.ErrUnserializable propagates verbatim — the
			// caller reaches it via errors.As against the trajectory
			// package's struct sentinel.
			return Pause{}, err
		}
		if err := saveCheckpoint(ctx, c.store, rec); err != nil {
			return Pause{}, err
		}
	}

	c.mu.Lock()
	c.pauses[token] = entry
	c.mu.Unlock()

	c.emit(ctx, EventTypePauseRequested, entry, PauseRequestedPayload{
		Token:  string(token),
		Reason: string(req.Reason),
	})

	return Pause{
		Token:    token,
		Reason:   req.Reason,
		Payload:  cloneStringMap(req.Payload),
		PausedAt: pausedAt,
		Identity: req.Identity,
	}, nil
}

// Resume terminates a pause. See the Coordinator interface godoc for
// the full contract.
//
// Resume is DESTRUCTIVE on the durable record: it flips the in-memory
// entry to StatusResumed and then DELETES the checkpoint from the
// StateStore. The resumed state is therefore queryable only via
// Status on the SAME Coordinator instance (in-memory) — a fresh
// Coordinator over the same store (a "restart") will get
// ErrPauseNotFound for a resumed token, NOT Status{State: resumed}.
// This is intentional: a resumed pause is terminal, and keeping a
// resumed checkpoint around would be an unbounded store leak with no
// consumer. Do not "fix" the missing post-resume-across-restart
// Status — checkpoint_test.go / phase50_durability_test.go assert
// this behaviour.
func (c *coordinator) Resume(ctx context.Context, token Token, decision Decision, payload map[string]any) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("pauseresume: resume cancelled: %w", err)
	}

	// Fail loudly on an unknown Decision — a `pause.resumed` event with
	// an untyped Decision defeats the marker the field exists for
	// (issue #113, D-096). Validated BEFORE identity / token lookup so
	// the contract violation surfaces verbatim without touching any
	// pause record.
	if !IsValidDecision(decision) {
		return fmt.Errorf("%w: %q", ErrInvalidDecision, decision)
	}

	resumingID, err := identityFromContext(ctx)
	if err != nil {
		return err
	}

	c.mu.Lock()
	entry, ok := c.pauses[token]
	if !ok {
		// Not in the in-memory registry — try the checkpoint store
		// (the restart-survival path). Release the lock for the store
		// I/O, then re-acquire to install the rehydrated entry.
		c.mu.Unlock()
		rehydrated, rerr := c.rehydrate(ctx, token)
		if rerr != nil {
			return rerr
		}
		c.mu.Lock()
		// Another goroutine may have rehydrated/installed the same
		// token while we did store I/O — prefer the already-installed
		// entry to keep a single source of truth.
		if existing, raced := c.pauses[token]; raced {
			entry = existing
		} else {
			entry = rehydrated
			c.pauses[token] = entry
		}
	}

	if entry.state == StatusResumed {
		c.mu.Unlock()
		return fmt.Errorf("%w: token %q", ErrAlreadyResumed, token)
	}
	if !sameScope(entry.identity, resumingID) {
		c.mu.Unlock()
		return fmt.Errorf("%w: token %q", ErrScopeMismatch, token)
	}

	// Re-attach the non-serialisable half of ToolContext. A lost
	// handle fails loud with trajectory.ErrToolContextLost — the run
	// is never resumed with a nil tool context. Done under the lock so
	// a concurrent Resume of the same token cannot both pass the
	// not-yet-resumed check; the registry Get is O(1) (sync.Map load).
	if err := c.reattachHandles(entry); err != nil {
		c.mu.Unlock()
		return err
	}

	entry.state = StatusResumed
	entry.resumedAt = c.now()
	// Merge the resume payload into the entry payload so a subsequent
	// Status reflects what the resume supplied.
	mergeStringMap(&entry.payload, payload)
	resumed := *entry
	c.mu.Unlock()

	// Clear the checkpoint AFTER the in-memory flip: the pause is
	// terminal regardless of whether the store delete succeeds; a
	// failed delete surfaces loud but the resume itself has happened.
	if c.store != nil {
		rec, cerr := resumed.toCheckpoint()
		if cerr != nil {
			// A resumed entry's trajectory should still serialise;
			// surface a corruption-shaped failure loud rather than
			// leaving an orphan checkpoint silently.
			return cerr
		}
		if err := deleteCheckpoint(ctx, c.store, rec); err != nil {
			return err
		}
	}

	c.emit(ctx, EventTypePauseResumed, &resumed, PauseResumedPayload{
		Token:    string(token),
		Reason:   string(resumed.reason),
		Decision: decision,
	})

	return nil
}

// Status returns a read-only snapshot of a pause record. See the
// Coordinator interface godoc for the full contract.
func (c *coordinator) Status(ctx context.Context, token Token) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, fmt.Errorf("pauseresume: status cancelled: %w", err)
	}

	c.mu.Lock()
	entry, ok := c.pauses[token]
	if ok {
		st := Status{
			State:     entry.state,
			Reason:    entry.reason,
			PausedAt:  entry.pausedAt,
			ResumedAt: entry.resumedAt,
		}
		c.mu.Unlock()
		return st, nil
	}
	c.mu.Unlock()

	// Not in the in-memory registry — fall back to the checkpoint
	// store (the restart-survival path).
	rehydrated, err := c.rehydrate(ctx, token)
	if err != nil {
		return Status{}, err
	}
	return Status{
		State:     rehydrated.state,
		Reason:    rehydrated.reason,
		PausedAt:  rehydrated.pausedAt,
		ResumedAt: rehydrated.resumedAt,
	}, nil
}

// rehydrate loads a pause record from the checkpoint store. Returns
// ErrPauseNotFound when no checkpoint store is configured (a token
// absent from the in-memory registry with no store is genuinely not
// found) or when the store has no checkpoint for the token.
func (c *coordinator) rehydrate(ctx context.Context, token Token) (*pauseEntry, error) {
	if c.store == nil {
		return nil, fmt.Errorf("%w: token %q (no checkpoint store configured)", ErrPauseNotFound, token)
	}
	rec, err := loadCheckpoint(ctx, c.store, token)
	if err != nil {
		return nil, err
	}
	return entryFromCheckpoint(rec)
}

// reattachHandles re-attaches every HandleID carried on the entry's
// trajectory ToolContext via the handle registry. A missing handle
// fails loud with trajectory.ErrToolContextLost (propagated verbatim).
// A nil trajectory or an empty Handles slice is a no-op.
func (c *coordinator) reattachHandles(entry *pauseEntry) error {
	if entry.trajectory == nil {
		return nil
	}
	for _, h := range entry.trajectory.ToolContext.Handles {
		if _, err := c.registry.Get(h); err != nil {
			// trajectory.ErrToolContextLost propagates verbatim — the
			// caller reaches it via errors.As. No silent nil context.
			return err
		}
	}
	return nil
}

// emit publishes a Coordinator event when a bus is configured. A
// publish failure is swallowed deliberately: event emission is
// observability, not correctness — a failed pause.requested emit must
// not unwind an already-recorded pause. (This is NOT silent
// degradation of a correctness path: the pause is recorded; only the
// best-effort notification was lost.)
func (c *coordinator) emit(ctx context.Context, evType events.EventType, entry *pauseEntry, payload events.EventPayload) {
	if c.bus == nil {
		return
	}
	_ = c.bus.Publish(ctx, events.Event{
		Type:     evType,
		Identity: identity.Quadruple{Identity: entry.identity, RunID: entry.runID},
		Payload:  payload,
	})
}

// toCheckpoint builds the persisted checkpoint envelope from the
// in-memory entry. Calls trajectory.Trajectory.Serialize when a
// trajectory is present; trajectory.ErrUnserializable propagates
// verbatim (the caller reaches it via errors.As).
func (e *pauseEntry) toCheckpoint() (checkpointRecord, error) {
	rec := checkpointRecord{
		// FormatVersion is set here for completeness; SerializeRecord
		// re-stamps it to the current FormatVersion on every write, so
		// the version field is single-sourced there (Phase 51 / D-069).
		FormatVersion: FormatVersion,
		Token:         e.token,
		Reason:        e.reason,
		State:         e.state,
		Identity:      e.identity,
		RunID:         e.runID,
		Payload:       e.payload,
		PausedAt:      e.pausedAt,
		ResumedAt:     e.resumedAt,
	}
	if e.trajectory != nil {
		b, err := e.trajectory.Serialize()
		if err != nil {
			// trajectory.ErrUnserializable — propagate verbatim.
			return checkpointRecord{}, err
		}
		rec.TrajectoryBytes = b
	}
	return rec, nil
}

// entryFromCheckpoint reconstructs an in-memory pause entry from a
// persisted checkpoint envelope. Deserialises the trajectory bytes
// when present; a corrupt trajectory surfaces ErrCheckpointCorrupt.
func entryFromCheckpoint(rec checkpointRecord) (*pauseEntry, error) {
	entry := &pauseEntry{
		token:     rec.Token,
		reason:    rec.Reason,
		state:     rec.State,
		identity:  rec.Identity,
		runID:     rec.RunID,
		payload:   rec.Payload,
		pausedAt:  rec.PausedAt,
		resumedAt: rec.ResumedAt,
	}
	if len(rec.TrajectoryBytes) > 0 {
		tr, err := trajectory.Deserialize(rec.TrajectoryBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: token %q trajectory: %v", ErrCheckpointCorrupt, rec.Token, err)
		}
		entry.trajectory = tr
	}
	return entry, nil
}

// sameScope reports whether two identity triples address the same
// (tenant, user, session). RunID is intentionally NOT compared — the
// isolation boundary is the triple (CLAUDE.md §6), and a resume may
// arrive on a different run-execution than the pause.
func sameScope(a, b identity.Identity) bool {
	return a.TenantID == b.TenantID &&
		a.UserID == b.UserID &&
		a.SessionID == b.SessionID
}

// cloneStringMap returns a shallow copy of m so a caller's later
// mutation of the passed map cannot reach into the Coordinator's
// recorded state (and vice versa). nil in ⇒ nil out.
func cloneStringMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// mergeStringMap merges src into *dst, allocating *dst when nil. Used
// to fold a resume payload into the recorded pause payload.
func mergeStringMap(dst *map[string]any, src map[string]any) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(map[string]any, len(src))
	}
	for k, v := range src {
		(*dst)[k] = v
	}
}
