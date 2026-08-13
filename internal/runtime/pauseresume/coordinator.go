package pauseresume

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
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
// Concurrent reuse contract: every field below is either set
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
	// maxPark is the OPTIONAL max-park duration.
	// When > 0, every pause carries an expiry derived from
	// PausedAt + maxPark; the pause sweeper (sweeper.go) resumes
	// expired pauses with DecisionTimeout. Zero (the default) means
	// pauses never expire — today's pre-111c behaviour. Set once at
	// construction.
	maxPark time.Duration
	// continuations is the construction-time handler registry. It shares mu
	// with pauses; handlers are looked up under the lock and invoked outside it.
	continuations map[string]ContinuationHandler

	// mu guards pauses. The map is the coordinator's only mutable
	// state and is documented internally-synchronised per the concurrent-reuse
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
	// expiresAt is the max-park deadline (PausedAt + maxPark) when the
	// Coordinator was constructed WithMaxParkDuration; the zero value
	// means "never expires" (the default). Derived, never persisted:
	// the rehydrate path re-stamps it from the rehydrating
	// Coordinator's own maxPark so an operator can change the knob
	// across a restart without a checkpoint-format change.
	expiresAt time.Time
	// decision is the typed Decision the pause was resumed with; the
	// zero value while State == StatusPaused. Recorded so Status (and
	// the RunLoop's out-of-band timeout detection) can distinguish a
	// timeout-reaped pause from an approve / reject / generic resume.
	decision  Decision
	available bool
	// deletePending marks a RESUMED entry whose checkpoint delete
	// failed: Resume flips the state before
	// the store delete, so a delete failure would otherwise orphan the
	// checkpoint forever — the sweeper skips non-paused entries and
	// Resume rejects an already-resumed token. The sweeper's
	// retryPendingDeletes pass re-attempts the delete (and the skipped
	// pause.resumed emit) until it lands, then clears this flag.
	deletePending bool
	continuation  *Continuation
	resuming      bool
	resumeDone    chan struct{}
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
// deliberately does not mint a parallel persistence-driver
// seam: state.StateStore is already the §4.4 persistence seam (three
// V1 drivers at conformance parity).
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

// WithMaxParkDuration sets the operator-configured ceiling on how long
// a pause may stay parked (RFC §3.3's typed
// `timeout` Decision). When d > 0, every pause carries an
// expiry derived from PausedAt + d, and the pause sweeper (RunSweeper)
// resumes expired pauses with DecisionTimeout — terminal for the
// waiting run (a deadline the human missed is a constraint the
// planner cannot resolve; mirrors the REJECT posture). A
// non-positive d is the documented "never expire" default — today's
// pre-111c behaviour, not an error.
func WithMaxParkDuration(d time.Duration) Option {
	return func(c *coordinator) {
		if d > 0 {
			c.maxPark = d
		}
	}
}

// New constructs the V1 process-local Coordinator. The returned value
// is immutable after construction and safe for concurrent use
// by N goroutines.
//
// With no options, the Coordinator is fully process-local: no
// checkpoint store (pauses do not survive restart), a fresh
// process-local handle registry, no event bus, time.Now as the clock.
func New(opts ...Option) Coordinator {
	c := &coordinator{
		registry:      trajectory.NewProcessLocalRegistry(),
		now:           time.Now,
		pauses:        make(map[Token]*pauseEntry),
		continuations: make(map[string]ContinuationHandler),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// RegisterContinuation registers one construction-time continuation handler.
// Duplicate kinds fail loudly; handlers are never replaced beneath live pause
// records.
func (c *coordinator) RegisterContinuation(kind string, handler ContinuationHandler) error {
	kind = strings.TrimSpace(kind)
	if kind == "" || handler == nil {
		return fmt.Errorf("%w: kind=%q", ErrInvalidContinuation, kind)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.continuations[kind]; exists {
		return fmt.Errorf("%w: %q", ErrContinuationKindRegistered, kind)
	}
	c.continuations[kind] = handler
	return nil
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
		return Pause{}, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	if !IsValidReason(req.Reason) {
		return Pause{}, fmt.Errorf("%w: %q", ErrInvalidReason, req.Reason)
	}
	continuation := req.Continuation
	if continuation == nil {
		continuation = continuationFromContext(ctx)
	}
	if err := validateContinuation(continuation); err != nil {
		return Pause{}, err
	}
	if continuation != nil {
		c.mu.Lock()
		_, registered := c.continuations[continuation.Kind]
		c.mu.Unlock()
		if !registered {
			return Pause{}, fmt.Errorf("%w: %q", ErrContinuationHandlerMissing, continuation.Kind)
		}
		cloned := cloneContinuation(*continuation)
		continuation = &cloned
	}

	// Fail-loudly serialise contract: the pause
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
		token:        token,
		reason:       req.Reason,
		state:        StatusPaused,
		available:    true,
		identity:     req.Identity,
		runID:        runIDFromContext(ctx),
		payload:      cloneStringMap(req.Payload),
		pausedAt:     pausedAt,
		trajectory:   req.Trajectory,
		continuation: continuation,
	}
	// Max-park expiry: derived from PausedAt + the
	// construction-time knob. Zero maxPark ⇒ zero expiresAt ⇒ the pause
	// never expires (the default).
	if c.maxPark > 0 {
		entry.expiresAt = pausedAt.Add(c.maxPark)
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
// Status — the package's checkpoint tests and the pause-durability
// integration suite assert this behaviour.
func (c *coordinator) Resume(ctx context.Context, token Token, decision Decision, payload map[string]any) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("pauseresume: resume cancelled: %w", err)
	}

	// Fail loudly on an unknown Decision — a `pause.resumed` event with
	// an untyped Decision defeats the marker the field exists for
	// (issue #113). Validated BEFORE identity / token lookup so
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
		if !sameScope(rehydrated.identity, resumingID) || (rehydrated.runID != "" && rehydrated.runID != runIDFromContext(ctx)) {
			return fmt.Errorf("%w: token %q", ErrPauseNotFound, token)
		}
		if rehydrated.reason == ReasonConstraintsConflict {
			if _, tranche := TrancheExceededFromMap(rehydrated.payload); tranche {
				rehydrated.available = false
				return fmt.Errorf("%w: token %q", ErrRestartUnavailable, token)
			}
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
		// Tokens are private selectors. Do not let a caller distinguish a
		// foreign receipt from a missing one.
		return fmt.Errorf("%w: token %q", ErrPauseNotFound, token)
	}
	if entry.runID != "" && entry.runID != runIDFromContext(ctx) {
		c.mu.Unlock()
		return fmt.Errorf("%w: token %q", ErrPauseNotFound, token)
	}
	// An accepted decision may be running durable continuation work outside
	// c.mu. Its claim wins against every later decision, including terminal
	// reject/timeout decisions that do not themselves invoke a continuation.
	// Wait for the winner to finish so callers never observe or overwrite
	// half-applied effects, then report that this concurrent decision lost the
	// arbitration. If the winner's continuation fails, the pause remains
	// retryable by a future call; this already-waiting call still does not
	// become an implicit retry with potentially different decision semantics.
	if entry.resuming {
		done := entry.resumeDone
		c.mu.Unlock()
		select {
		case <-done:
			return fmt.Errorf("%w: token %q", ErrAlreadyResumed, token)
		case <-ctx.Done():
			return fmt.Errorf("pauseresume: wait for concurrent resume: %w", ctx.Err())
		}
	}

	// Re-attach the non-serialisable half of ToolContext. A lost
	// handle fails loud with trajectory.ErrToolContextLost — the run
	// is never resumed with a nil tool context. Done under the lock so
	// a concurrent Resume of the same token cannot both pass the
	// not-yet-resumed check; the registry Get is O(1) (sync.Map load).
	//
	// DecisionTimeout deliberately SKIPS the re-attach: a
	// timeout resume is terminal — the waiting run finishes with
	// Finish{ConstraintsConflict} and the planner is never re-entered
	// with the trajectory (call 4), so the non-serialisable tool
	// half is never needed. Requiring it would wedge the sweeper's
	// crash-orphan reap forever: a crashed process's handle registry
	// is empty by definition, so a crash-orphaned checkpoint whose
	// trajectory carries handles could never be reaped. Every
	// run-continuing decision (approve / reject / resume) keeps the
	// fail-loud re-attach unchanged.
	if decision != DecisionTimeout {
		if err := c.reattachHandles(entry); err != nil {
			c.mu.Unlock()
			return err
		}
	}

	// Accepted resumes execute durable continuation work BEFORE the terminal
	// state flip and checkpoint delete. Claim the token under c.mu, then release
	// the global coordinator lock before any handler I/O. A failure or
	// cancellation clears the claim but leaves the entry paused and checkpoint
	// intact, so the same token is retryable. Reject/timeout are terminal
	// decisions and intentionally do not run continuation work.
	if entry.continuation != nil && (decision == DecisionResume || decision == DecisionApprove) {
		handler, registered := c.continuations[entry.continuation.Kind]
		if !registered {
			c.mu.Unlock()
			return fmt.Errorf("%w: %q", ErrContinuationHandlerMissing, entry.continuation.Kind)
		}
		entry.resuming = true
		entry.resumeDone = make(chan struct{})
		invocation := ContinuationInvocation{
			Token:         token,
			Identity:      entry.identity,
			RunID:         entry.runID,
			Continuation:  cloneContinuation(*entry.continuation),
			Decision:      decision,
			ResumePayload: cloneStringMap(payload),
		}
		c.mu.Unlock()
		handlerErr := handler(ctx, invocation)
		c.mu.Lock()
		entry.resuming = false
		if handlerErr != nil {
			close(entry.resumeDone)
			entry.resumeDone = nil
			c.mu.Unlock()
			return fmt.Errorf("pauseresume: continuation %q for token %q: %w", entry.continuation.Kind, token, handlerErr)
		}
	}

	entry.state = StatusResumed
	entry.resumedAt = c.now()
	entry.decision = decision
	if entry.resumeDone != nil {
		close(entry.resumeDone)
		entry.resumeDone = nil
	}
	// Merge the resume payload into the entry payload so a subsequent
	// Status reflects what the resume supplied.
	mergeStringMap(&entry.payload, payload)
	resumed := *entry
	c.mu.Unlock()

	// Clear the checkpoint AFTER the in-memory flip: the pause is
	// terminal regardless of whether the store delete succeeds; a
	// failed delete surfaces loud but the resume itself has happened.
	// A failed delete (or a resumed entry that no longer serialises)
	// additionally marks the entry delete-pending so the sweeper's
	// retryPendingDeletes pass re-attempts the cleanup — the
	// checkpoint must not orphan silently (checkpoint audit;
	// the sweeper's expired scan only selects StatusPaused entries, so
	// without the flag nothing would ever retry).
	if c.store != nil {
		rec, cerr := resumed.toCheckpoint()
		if cerr != nil {
			// A resumed entry's trajectory should still serialise;
			// surface a corruption-shaped failure loud rather than
			// leaving an orphan checkpoint silently.
			c.markDeletePending(token)
			return cerr
		}
		if err := deleteCheckpoint(ctx, c.store, rec); err != nil {
			c.markDeletePending(token)
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

// CancelTranche atomically consumes a live step-tranche pause. Cancellation
// and Resume share the coordinator mutex, so exactly one terminal decision
// wins and the token is never continuable afterward.
func (c *coordinator) CancelTranche(ctx context.Context, token Token) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("pauseresume: tranche cancellation cancelled: %w", err)
	}
	id, err := identityFromContext(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	entry, ok := c.pauses[token]
	if !ok {
		c.mu.Unlock()
		return fmt.Errorf("%w: token %q", ErrPauseNotFound, token)
	}
	if !sameScope(entry.identity, id) || (entry.runID != "" && entry.runID != runIDFromContext(ctx)) {
		c.mu.Unlock()
		return fmt.Errorf("%w: token %q", ErrPauseNotFound, token)
	}
	if entry.state == StatusResumed {
		c.mu.Unlock()
		return fmt.Errorf("%w: token %q", ErrAlreadyResumed, token)
	}
	if entry.resuming {
		c.mu.Unlock()
		return fmt.Errorf("%w: token %q", ErrResumeInProgress, token)
	}
	if entry.reason != ReasonConstraintsConflict {
		c.mu.Unlock()
		return fmt.Errorf("%w: token %q", ErrNotTranchePause, token)
	}
	if _, ok := TrancheExceededFromMap(entry.payload); !ok {
		c.mu.Unlock()
		return fmt.Errorf("%w: token %q", ErrNotTranchePause, token)
	}
	entry.state, entry.available, entry.resumedAt, entry.decision = StatusResumed, false, c.now(), DecisionCancelled
	terminal := *entry
	c.mu.Unlock()
	// Project cancellation before cleanup. Cleanup remains a loud, observable
	// retry obligation, but must not delay the terminal cancellation event.
	c.emit(ctx, EventTypePauseResumed, &terminal, PauseResumedPayload{Token: string(token), Reason: string(terminal.reason), Decision: DecisionCancelled})
	if c.store != nil {
		rec, rerr := terminal.toCheckpoint()
		if rerr != nil {
			c.markDeletePending(token)
			return &TrancheCancellationError{Err: rerr}
		}
		if derr := deleteCheckpoint(ctx, c.store, rec); derr != nil {
			c.markDeletePending(token)
			return &TrancheCancellationError{Err: derr}
		}
	}
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
			Decision:  entry.decision,
			Available: entry.available,
		}
		c.mu.Unlock()
		return st, nil
	}
	c.mu.Unlock()

	// Not in the in-memory registry — fall back to the checkpoint
	// store (the restart-survival path). A rehydrated entry is always
	// StatusPaused (Resume deletes the checkpoint), so Decision is the
	// zero value here by construction.
	rehydrated, err := c.rehydrate(ctx, token)
	if err != nil {
		return Status{}, err
	}
	return Status{
		State:     rehydrated.state,
		Reason:    rehydrated.reason,
		PausedAt:  rehydrated.pausedAt,
		ResumedAt: rehydrated.resumedAt,
		Available: rehydrated.available,
	}, nil
}

// StatusForIdentity returns a token status only for its owning identity
// scope. A foreign token is indistinguishable from an absent token.
func (c *coordinator) StatusForIdentity(ctx context.Context, token Token, id identity.Identity, runID string) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, fmt.Errorf("pauseresume: scoped status cancelled: %w", err)
	}
	if err := identity.Validate(id); err != nil {
		return Status{}, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	c.mu.Lock()
	entry, ok := c.pauses[token]
	c.mu.Unlock()
	if !ok {
		var err error
		entry, err = c.rehydrate(ctx, token)
		if err != nil {
			return Status{}, err
		}
	}
	if !sameScope(entry.identity, id) || entry.runID != runID {
		return Status{}, fmt.Errorf("%w: token %q", ErrPauseNotFound, token)
	}
	return Status{State: entry.state, Reason: entry.reason, PausedAt: entry.pausedAt, ResumedAt: entry.resumedAt, Decision: entry.decision, Available: entry.available}, nil
}

// markDeletePending flags a resumed entry whose checkpoint cleanup
// failed so the sweeper's retry pass can re-attempt it. A token that
// raced out of the registry is a no-op (nothing to retry against).
func (c *coordinator) markDeletePending(token Token) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.pauses[token]; ok {
		entry.deletePending = true
	}
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
	entry, err := entryFromCheckpoint(rec)
	if err != nil {
		return nil, err
	}
	// Re-stamp the max-park expiry from THIS Coordinator's knob: the
	// deadline is derived (PausedAt + maxPark), never persisted, so a
	// restarted Runtime with a different max_park_duration applies its
	// own ceiling to rehydrated pauses.
	if c.maxPark > 0 && entry.state == StatusPaused {
		entry.expiresAt = entry.pausedAt.Add(c.maxPark)
	}
	return entry, nil
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
	_ = c.bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort emit; pause is already recorded (see doc above)
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
		// the version field is single-sourced there.
		FormatVersion: FormatVersion,
		Token:         e.token,
		Reason:        e.reason,
		State:         e.state,
		Identity:      e.identity,
		RunID:         e.runID,
		Payload:       e.payload,
		Continuation:  e.continuation,
		PausedAt:      e.pausedAt,
		ResumedAt:     e.resumedAt,
		Available:     e.available,
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
	if err := validateContinuation(rec.Continuation); err != nil {
		return nil, fmt.Errorf("%w: token %q continuation: %w", ErrCheckpointCorrupt, rec.Token, err)
	}
	var continuation *Continuation
	if rec.Continuation != nil {
		cloned := cloneContinuation(*rec.Continuation)
		continuation = &cloned
	}
	entry := &pauseEntry{
		token:        rec.Token,
		reason:       rec.Reason,
		state:        rec.State,
		identity:     rec.Identity,
		runID:        rec.RunID,
		payload:      rec.Payload,
		pausedAt:     rec.PausedAt,
		resumedAt:    rec.ResumedAt,
		available:    rec.Available,
		continuation: continuation,
	}
	if rec.Reason == ReasonConstraintsConflict {
		if _, tranche := TrancheExceededFromMap(rec.Payload); tranche {
			entry.available = false
		}
	}
	if len(rec.TrajectoryBytes) > 0 {
		tr, err := trajectory.Deserialize(rec.TrajectoryBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: token %q trajectory: %w", ErrCheckpointCorrupt, rec.Token, err)
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
