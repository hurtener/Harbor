package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// sessionKind is the StateStore Kind constant for session-lifecycle
// records. Centralised so callers / tests / Protocol mappers
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
// the no-op RunningProbe. Production wiring (cmd/harbor and
// harbortest/devstack) passes a policy whose RunningProbe is
// TaskRunningProbe so GC honors RFC §6.9.
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
//     itself concurrent-safe per the concurrent-reuse contract.
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
	// removed (the closed record is preserved so it can be RE-ACTIVATED on
	// the next Open — RFC §6.9 amended); a cross-tenant reuse of the
	// same SessionID string stays rejected (ErrSessionIDReuse) regardless.
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
// defaults to no-op; assemblies that own a TaskRegistry MUST pass
// TaskRunningProbe via WithGCPolicy (both reference assemblies do)
// so GC never reaps a session with a RUNNING task.
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
// immutably, and emits session.opened. A closed record is RE-ACTIVATED in
// place (RFC §6.9 amended): its identity + OpenedAt are preserved,
// the closed flags cleared, LastReopenedAt stamped, and a content-free
// session.reopened emitted; an ERASED session instead fails loud with
// ErrReopenAfterErase (the terminal exception, detected via the erasure
// ledger/tombstone even after the record is gone). Cross-tenant SessionID
// reuse is rejected with ErrSessionIDReuse; same-triple double-open is
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
			// Reopen-after-close (RFC §6.9 amended): RE-ACTIVATE the
			// closed record in place so the conversation resumes with its
			// durable history intact (close/GC reap the record, not the data).
			//
			// The ONE terminal exception fires first: an ERASED session (a
			// pending erasure ledger from an in-flight erase, OR a converged
			// erasure's durable tombstone) fails loud rather than reviving
			// deleted data. isErased runs under the r.mu Open already holds, so
			// it serialises against the erasure cascade's r.mu-held record
			// clear (deleteScopeSerialized) + catalog clear (clearErased).
			erased, eerr := r.isErased(ctx, ident)
			if eerr != nil {
				return nil, eerr
			}
			if erased {
				return nil, fmt.Errorf("%w: SessionID=%q", ErrReopenAfterErase, id)
			}
			// Defence-in-depth identity check — the StateStore load is already
			// triple-keyed, so a mismatch here would be a driver bug, not a
			// forgery path (mirrors Touch/Close).
			if !sameIdentity(stored.Identity, ident) {
				return nil, fmt.Errorf("%w: stored=(%s,%s,%s) ctx=(%s,%s,%s)",
					ErrIdentityMismatch,
					stored.Identity.TenantID, stored.Identity.UserID, stored.Identity.SessionID,
					ident.TenantID, ident.UserID, ident.SessionID)
			}
			priorReason := stored.ClosedReason
			now := r.clock.Now()
			stored.Closed = false
			stored.ClosedAt = time.Time{}
			stored.ClosedReason = ""
			stored.LastReopenedAt = now
			stored.LastSeen = now
			// OpenedAt is deliberately NOT refreshed — it is the erasure
			// cascade's lifecycle discriminator; the separate LastReopenedAt
			// stamp is what the GC hard cap measures max(OpenedAt, …) from.
			if serr := r.save(ctx, stored); serr != nil {
				return nil, serr
			}
			r.idIndex[id] = ident
			r.openSessions[id] = q
			// Re-add to the (tenant, user) discovery catalog so a later process
			// re-discovers the now-open session (mirrors the fresh-Open catalog
			// write; a failure fails the reopen loud — §13).
			if cerr := r.addToCatalog(ctx, ident.TenantID, ident.UserID, id); cerr != nil {
				return nil, cerr
			}
			// Lift any erasure fence on the triple so the resumed session's
			// events are retained normally (best-effort, mirrors fresh Open).
			r.unfenceSession(ctx, ident)
			r.publish(ctx, q, events.Event{
				Type:     EventTypeSessionReopened,
				Identity: q,
				Payload: SessionReopenedPayload{
					SessionID:         id,
					ReopenedAt:        now.UnixNano(),
					PriorClosedReason: priorReason,
				},
			})
			cp := stored
			return &cp, nil
		}
		// Open record already exists for this exact triple → already open.
		// fix: hydrate idIndex + openSessions so the in-memory
		// catalog reflects the persisted record. Without this, a reboot
		// that finds an existing dev session would return the error and
		// leave ListSnapshots reading an empty idIndex — exactly the P8
		// symptom on the Sessions page after the first boot.
		r.idIndex[id] = ident
		r.openSessions[id] = q
		return nil, fmt.Errorf("%w: SessionID=%q tenant=%q user=%q",
			ErrSessionAlreadyOpen, id, ident.TenantID, ident.UserID)
	}

	// Not-found / fresh-create fall-through (err was state.ErrNotFound — the
	// err==nil branch above always returns). A fully-CONVERGED erasure removed
	// the session.lifecycle record, so a reopen of an erased id lands HERE, and
	// naively minting a fresh session would SILENTLY RESURRECT it.
	// Gate on the durable erasure tombstone BEFORE minting, under the same r.mu
	// hold so it serialises against the erasure cascade's write-before-delete
	// tombstone ordering.
	erased, eerr := r.isErased(ctx, ident)
	if eerr != nil {
		return nil, eerr
	}
	if erased {
		return nil, fmt.Errorf("%w: SessionID=%q", ErrReopenAfterErase, id)
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

	// record the new SessionID in the (tenant, user) catalog so a
	// later process can re-discover it (the StateStore has no List). This
	// runs while holding r.mu — the serialized catalog RMW path
	// (mutateCatalog) requires it — so the add serializes with a concurrent
	// erasure's removeFromCatalog and neither lost-updates the other. A
	// catalog write failure fails the Open loud (§13 — no silent
	// degradation): a session the catalog never learned about would
	// silently vanish from sessions.list after a restart.
	if cerr := r.addToCatalog(ctx, ident.TenantID, ident.UserID, id); cerr != nil {
		return nil, cerr
	}

	// A fresh session on this triple may be REUSING a session id whose
	// prior occupant was erased (`sessions.delete`). The erasure fenced the
	// triple on the event bus to drop a defunct task's late events; lift the
	// fence now so this new session's events — starting with the
	// session.opened below — are retained normally (events.Fencer). Ordered
	// before the publish so the opened event itself is not dropped.
	r.unfenceSession(ctx, ident)

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

// EnsureOpen is the create-on-first-use entry point. It returns
// the live session for `ident`, creating it if no record exists. The
// session id is `ident.SessionID` (the per-request session the client
// chose); the (tenant, user) come from the verified connection token.
//
// Semantics:
//
//   - No record yet → Open it (create) and return the fresh session
//     (UNLESS the id was erased — ErrReopenAfterErase, never a silent
//     fresh empty session on a converged erasure).
//   - Open record at this exact triple → no-op, return the existing
//     session (a second turn in the same conversation is not an error).
//   - Closed record at this triple → RE-ACTIVATE it in place and return
//     the resumed session (RFC §6.9 amended: a GC-reaped or
//     operator-closed session reopens with its durable history intact).
//     An ERASED session is the terminal exception → ErrReopenAfterErase.
//
// EnsureOpen is identity-mandatory: an incomplete triple fails closed.
// It is the seam the Protocol ControlSurface calls on `start` so the
// first turn of a brand-new conversation materialises the session row
// the Console's sessions.list reads.
func (r *Registry) EnsureOpen(ctx context.Context, ident identity.Identity) (*Session, error) {
	if r.closed.Load() {
		return nil, ErrRegistryClosed
	}
	if err := identity.Validate(ident); err != nil {
		return nil, err
	}
	// Hydrate this (tenant, user) from the catalog first so a session a
	// prior process created (and that is still open) is recognised as
	// already-open rather than re-created.
	if herr := r.hydrateFromCatalog(ctx, ident.TenantID, ident.UserID); herr != nil {
		return nil, herr
	}

	s, err := r.Open(ctx, ident.SessionID, ident)
	switch {
	case err == nil:
		// Fresh create OR a re-activated closed session (RFC §6.9 amended):
		// Open returns the live/resumed session directly.
		return s, nil
	case errors.Is(err, ErrSessionAlreadyOpen):
		// The exact triple is already open — return the live record. This
		// is the steady-state path for the second-and-later turn of a
		// conversation. Open already re-hydrated idIndex/openSessions.
		return r.loadSession(ctx, ident)
	default:
		// ErrReopenAfterErase (the terminal exception), ErrSessionIDReuse,
		// store errors — surface loud. An erased session is NEVER silently
		// resurrected as a fresh empty one (RFC §6.9 amended / §7).
		return nil, err
	}
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
// ErrIdentityMismatch. Touch on a Closed session returns ErrSessionClosed
// (Touch is a read-only refresh, NOT a reopen entry point — a caller must
// go through Open / EnsureOpen / `start` to re-activate a closed session).
// The load→mutate→save runs through mutateSession (one r.mu critical
// section) so a concurrent Close / SetTitle / GC reap can never lose
// this write — or have this write resurrect theirs.
func (r *Registry) Touch(ctx context.Context, id string) error {
	if r.closed.Load() {
		return ErrRegistryClosed
	}
	ident, ok := identity.From(ctx)
	if !ok {
		return fmt.Errorf("sessions: Touch requires identity in ctx: %w", identity.ErrIdentityMissing)
	}
	now := r.clock.Now()
	if _, err := r.mutateSession(ctx, ident, func(stored *Session) error {
		if !sameIdentity(stored.Identity, ident) {
			return fmt.Errorf("%w: stored=(%s,%s,%s) ctx=(%s,%s,%s)",
				ErrIdentityMismatch,
				stored.Identity.TenantID, stored.Identity.UserID, stored.Identity.SessionID,
				ident.TenantID, ident.UserID, ident.SessionID)
		}
		if stored.Closed {
			return fmt.Errorf("%w: SessionID=%q closed at %s",
				ErrSessionClosed, id, stored.ClosedAt.Format(time.RFC3339))
		}
		stored.LastSeen = now
		return nil
	}); err != nil {
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
// original ClosedReason. The load→mutate→save runs through
// mutateSession (one r.mu critical section — this previously loaded
// OUTSIDE the lock, leaving a lost-update window where a concurrent
// SetTitle / Touch could re-persist Closed=false after Close returned).
func (r *Registry) Close(ctx context.Context, id string, reason string) error {
	if r.closed.Load() {
		return ErrRegistryClosed
	}
	ident, ok := identity.From(ctx)
	if !ok {
		return fmt.Errorf("sessions: Close requires identity in ctx: %w", identity.ErrIdentityMissing)
	}
	now := r.clock.Now()
	closedNow := false
	if _, err := r.mutateSession(ctx, ident, func(stored *Session) error {
		if !sameIdentity(stored.Identity, ident) {
			return fmt.Errorf("%w: stored=(%s,%s,%s) ctx=(%s,%s,%s)",
				ErrIdentityMismatch,
				stored.Identity.TenantID, stored.Identity.UserID, stored.Identity.SessionID,
				ident.TenantID, ident.UserID, ident.SessionID)
		}
		if stored.Closed {
			// Idempotent: original reason wins, no event re-emit.
			return errSkipSave
		}
		stored.Closed = true
		stored.ClosedAt = now
		stored.ClosedReason = reason
		closedNow = true
		return nil
	}); err != nil {
		return err
	}
	if !closedNow {
		return nil
	}
	// Drop the in-memory tracking entry only after the save succeeded (a
	// failed save must not orphan the entry). The brief window where the
	// record is persisted Closed but still listed in openSessions is
	// benign — GC's already-closed branch drops such entries idempotently.
	r.mu.Lock()
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

// SetTitle sets or clears the manual title of session `id` for the
// CALLER's verified `(tenant, user)` (`ident`). Unlike Touch/Close/
// Inspect, the target `id` is NOT required to equal `ident.SessionID` —
// the write scope is `(tenant, user)`, matching `sessions.list`, so a
// caller may rename a sibling session. `ident` still carries a full
// triple (identity.Validate requires all three components) — its
// SessionID is the caller's OWN connecting session and is used only for
// identity validation, never as the load key.
//
// Semantics: the title is trimmed; an empty-after-trim value clears
// Title, resets TitleSource to TitleSourceUnset, AND zeroes the
// auto-naming counters (AutoNameCount / LastAutoNamedTurn) in the same
// save — a clear starts a NEW auto-naming arming cycle, so a re-arm fires
// again (the cap is per-cycle, not per-session-lifetime). A non-empty
// value is validated (MaxSessionTitleLen, no newline/control characters —
// ErrInvalidTitle on violation, never a silent clamp per CLAUDE.md
// §13) and stored with TitleSource = TitleSourceManual. A write that
// changes nothing (clearing an already-unset title, or an identical
// manual re-set) is a no-op: nothing is persisted and no
// session.title_changed event is emitted. The target
// session is loaded under the caller's own (tenant, user) plus the
// target id — a session belonging to a DIFFERENT (tenant, user) is
// simply not found at that StateStore key, so a cross-user/cross-tenant
// rename attempt surfaces as the same ErrSessionNotFound a genuinely
// unknown id would (existence is never revealed across identities).
// Renaming a CLOSED session is allowed (title is metadata on a
// historical conversation, not a lifecycle mutation); renaming an
// ERASED session is ErrSessionNotFound (DeleteScope already removed
// the record).
func (r *Registry) SetTitle(ctx context.Context, id string, ident identity.Identity, title string) error {
	if r.closed.Load() {
		return ErrRegistryClosed
	}
	if err := identity.Validate(ident); err != nil {
		return err
	}
	trimmed := strings.TrimSpace(title)
	if trimmed != "" {
		if err := validateTitle(trimmed); err != nil {
			return err
		}
	}

	// The load→verify→mutate→save runs through mutateSession (one r.mu
	// critical section, shared with Touch / Close / the GC reap and the
	// eraser's deleteScopeSerialized). Without it, a SetTitle racing a
	// Close could re-persist Closed=false after Close returned nil
	// (resurrecting a closed session the GC no longer tracks), and a
	// SetTitle racing an erasure could re-persist the session.lifecycle
	// record after DeleteScope's irreversible clear.
	target := identity.Identity{TenantID: ident.TenantID, UserID: ident.UserID, SessionID: id}
	changed := false
	stored, err := r.mutateSession(ctx, target, func(stored *Session) error {
		// Defence-in-depth (mirrors Touch/Close): verify the record we
		// got back actually belongs to the caller's (tenant, user). The
		// StateStore key already scopes the load to exactly this pair, so
		// this should be unreachable via the public API — it guards
		// against a future driver bug rather than a real forgery path.
		if stored.Identity.TenantID != ident.TenantID || stored.Identity.UserID != ident.UserID {
			return fmt.Errorf("%w: stored=(%s,%s) caller=(%s,%s)",
				ErrIdentityMismatch,
				stored.Identity.TenantID, stored.Identity.UserID,
				ident.TenantID, ident.UserID)
		}
		if trimmed == "" {
			// Clearing an already-unset, never-auto-named title is a no-op:
			// skip the save AND the event (a content-free session.title_changed
			// on a write that changed nothing is a spurious wake-up for every
			// subscriber).
			if stored.Title == "" && stored.TitleSource == TitleSourceUnset &&
				stored.AutoNameCount == 0 && stored.LastAutoNamedTurn == 0 {
				return errSkipSave
			}
			stored.Title = ""
			stored.TitleSource = TitleSourceUnset
			// A manual clear STARTS A NEW auto-naming arming cycle: reset the
			// naming counters in the SAME record save so a re-arm fires again.
			// Without this, once any auto-name landed (AutoNameCount ≥ 1),
			// namingDue's AutoNameCount==0 first branch never re-triggered under
			// repeat_every==0 — clear→re-arm was a silent dead-end. The
			// max-repetitions cap is therefore PER-CYCLE: each clear opens a
			// fresh cycle with its own cap budget.
			stored.AutoNameCount = 0
			stored.LastAutoNamedTurn = 0
		} else {
			// Identical manual re-set is a no-op: skip the save and the event.
			if stored.Title == trimmed && stored.TitleSource == TitleSourceManual {
				return errSkipSave
			}
			stored.Title = trimmed
			stored.TitleSource = TitleSourceManual
		}
		changed = true
		return nil
	})
	if err != nil {
		return err
	}
	if !changed {
		// No-op write (errSkipSave): nothing persisted, no event emitted.
		return nil
	}
	r.publish(ctx, identity.Quadruple{Identity: stored.Identity}, events.Event{
		Type:     EventTypeSessionTitleChanged,
		Identity: identity.Quadruple{Identity: stored.Identity},
		Payload: SessionTitleChangedPayload{
			SessionID: id,
			Source:    string(stored.TitleSource),
		},
	})
	return nil
}

// RecordCompletedTurn increments the session's TurnCount and returns the new
// value. The run loop's terminal boundary calls it once per completed run
// WHEN a naming policy is active — never otherwise, so a naming-off runtime
// writes nothing (the opt-in invariant). The load→bump→save runs through
// mutateSession (one r.mu critical section, shared with Touch / Close /
// SetTitle / SetTitleAuto / the GC reap and the eraser's
// deleteScopeSerialized) so a concurrent write can never lose the increment
// or resurrect an erased record.
//
// `ident` is the run's verified (tenant, user, session). The target session
// is loaded under that exact triple; a session belonging to a different
// (tenant, user, session) is not found at that StateStore key
// (ErrSessionNotFound). A stored-identity mismatch (defence-in-depth) returns
// ErrIdentityMismatch. Renaming/counting a CLOSED session is allowed (a run
// can complete against a session the operator closed mid-flight); an ERASED
// session is ErrSessionNotFound.
func (r *Registry) RecordCompletedTurn(ctx context.Context, id string, ident identity.Identity) (int, error) {
	if r.closed.Load() {
		return 0, ErrRegistryClosed
	}
	if err := identity.Validate(ident); err != nil {
		return 0, err
	}
	target := identity.Identity{TenantID: ident.TenantID, UserID: ident.UserID, SessionID: id}
	var count int
	if _, err := r.mutateSession(ctx, target, func(stored *Session) error {
		if stored.Identity.TenantID != ident.TenantID || stored.Identity.UserID != ident.UserID {
			return fmt.Errorf("%w: stored=(%s,%s) caller=(%s,%s)",
				ErrIdentityMismatch,
				stored.Identity.TenantID, stored.Identity.UserID,
				ident.TenantID, ident.UserID)
		}
		stored.TurnCount++
		count = stored.TurnCount
		return nil
	}); err != nil {
		return 0, err
	}
	return count, nil
}

// SetTitleAuto sets the session's title from the internal auto-namer. It
// REFUSES with ErrManualTitle when the current TitleSource is
// TitleSourceManual — manual wins, structurally, so a human's title is never
// overwritten by the runtime. On success it stores the (defensively
// re-clamped) title with TitleSource = TitleSourceAuto and bumps AutoNameCount
// + LastAutoNamedTurn in the SAME record save (never a torn two-write update),
// then publishes a content-free session.title_changed (source=auto).
//
// `title` is expected to be already clamped by the caller (the trigger) to the
// naming policy's max-title-len; the registry re-clamps defensively to
// MaxSessionTitleLen runes on a single line and rejects an empty-after-trim
// title with ErrInvalidTitle (the auto path never intentionally clears — an
// empty candidate is the caller's `empty_title` skip, handled before this
// call). The whole read→refuse/mutate→save runs through mutateSession (the one
// serialized whole-record path).
func (r *Registry) SetTitleAuto(ctx context.Context, id string, ident identity.Identity, title string) error {
	if r.closed.Load() {
		return ErrRegistryClosed
	}
	if err := identity.Validate(ident); err != nil {
		return err
	}
	clamped := clampAutoTitle(title)
	if clamped == "" {
		return fmt.Errorf("%w: auto title empty after trim/clamp", ErrInvalidTitle)
	}
	target := identity.Identity{TenantID: ident.TenantID, UserID: ident.UserID, SessionID: id}
	stored, err := r.mutateSession(ctx, target, func(stored *Session) error {
		if stored.Identity.TenantID != ident.TenantID || stored.Identity.UserID != ident.UserID {
			return fmt.Errorf("%w: stored=(%s,%s) caller=(%s,%s)",
				ErrIdentityMismatch,
				stored.Identity.TenantID, stored.Identity.UserID,
				ident.TenantID, ident.UserID)
		}
		if stored.TitleSource == TitleSourceManual {
			return ErrManualTitle
		}
		stored.Title = clamped
		stored.TitleSource = TitleSourceAuto
		stored.AutoNameCount++
		stored.LastAutoNamedTurn = stored.TurnCount
		return nil
	})
	if err != nil {
		return err
	}
	r.publish(ctx, identity.Quadruple{Identity: stored.Identity}, events.Event{
		Type:     EventTypeSessionTitleChanged,
		Identity: identity.Quadruple{Identity: stored.Identity},
		Payload: SessionTitleChangedPayload{
			SessionID: id,
			Source:    string(stored.TitleSource),
		},
	})
	return nil
}

// AutoNamingState reads the session's title provenance + naming counters for
// the terminal-boundary eligibility check. It is a read-only load (no
// lifecycle mutation, no lock held across the read) under the run's verified
// triple; an unknown / erased id returns ErrSessionNotFound, a stored-identity
// mismatch returns ErrIdentityMismatch.
func (r *Registry) AutoNamingState(ctx context.Context, id string, ident identity.Identity) (AutoNamingState, error) {
	if r.closed.Load() {
		return AutoNamingState{}, ErrRegistryClosed
	}
	if err := identity.Validate(ident); err != nil {
		return AutoNamingState{}, err
	}
	target := identity.Identity{TenantID: ident.TenantID, UserID: ident.UserID, SessionID: id}
	stored, err := r.loadSession(ctx, target)
	if err != nil {
		return AutoNamingState{}, err
	}
	if stored.Identity.TenantID != ident.TenantID || stored.Identity.UserID != ident.UserID {
		return AutoNamingState{}, fmt.Errorf("%w: stored=(%s,%s) caller=(%s,%s)",
			ErrIdentityMismatch,
			stored.Identity.TenantID, stored.Identity.UserID,
			ident.TenantID, ident.UserID)
	}
	return AutoNamingState{
		TitleSource:       stored.TitleSource,
		CurrentTitle:      stored.Title,
		TurnCount:         stored.TurnCount,
		AutoNameCount:     stored.AutoNameCount,
		LastAutoNamedTurn: stored.LastAutoNamedTurn,
	}, nil
}

// clampAutoTitle deterministically post-processes the auto-namer's own model
// output into a stored title: it takes the first non-empty line, trims
// surrounding whitespace, strips control characters, and cuts to
// MaxSessionTitleLen runes. Unlike SetTitle (an untrusted boundary that
// rejects oversize input), the auto path CLAMPS — it is trusted-internal
// post-processing of the runtime's own LLM call, and the asymmetry is
// intentional.
func clampAutoTitle(title string) string {
	// Single line: take the first line with visible content.
	line := title
	if idx := strings.IndexAny(title, "\r\n"); idx >= 0 {
		// Prefer the first non-empty line rather than blindly cutting at the
		// first newline (a leading blank line must not yield an empty title).
		for _, cand := range strings.FieldsFunc(title, func(r rune) bool { return r == '\n' || r == '\r' }) {
			if strings.TrimSpace(cand) != "" {
				line = cand
				break
			}
		}
	}
	// Strip control characters (defence-in-depth; the model should not emit
	// them, but a stray one must never reach the stored record).
	line = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, line)
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) > MaxSessionTitleLen {
		runes := []rune(line)
		line = strings.TrimSpace(string(runes[:MaxSessionTitleLen]))
	}
	return line
}

// Erase is the registry-side pre-flight + in-memory clear for a session
// erasure (`sessions.delete`). It runs the fail-loud preconditions that
// MUST hold before any store is touched — load+verify the record under
// the caller's verified ctx identity, then probe the RunningProbe seam —
// and, when they pass, clears the in-memory open-sessions / id-index map
// entry and the per-(tenant, user) discovery catalog.
//
// Erase performs NO durable record delete of its own: the durable
// `session.lifecycle` record is itself a StateStore record under the
// triple, removed by the kind-agnostic StateStore.DeleteScope step of
// the cascade. Calling Erase standalone leaves the durable record in
// place (the caller is expected to run DeleteScope); the erasure
// orchestrator composes the two via the unexported preflightErase /
// clearErased halves so the probe runs FIRST (a refusal touches
// nothing) and the in-memory clear runs LAST (after DeleteScope).
//
// Identity-mandatory: the ctx identity scopes the read, and its
// SessionID must equal id. Returns ErrSessionNotFound when no record is
// visible to the caller's identity, ErrSessionRunning when the probe
// reports a RUNNING task (no store touched), ErrIdentityMismatch when
// the stored identity disagrees with the caller's.
func (r *Registry) Erase(ctx context.Context, id string) error {
	sess, err := r.preflightErase(ctx, id)
	if err != nil {
		return err
	}
	return r.clearErased(ctx, sess.Identity)
}

// preflightErase runs the fail-loud preconditions for an erasure without
// mutating anything: it validates the ctx identity (its SessionID must
// equal id), loads+verifies the session record under that identity, and
// probes the RunningProbe seam. A refusal (ErrSessionNotFound /
// ErrSessionRunning / ErrIdentityMismatch) touches no store. On success
// it returns the verified stored session: the cascade scopes its
// deletions to its Identity, and its OpenedAt is the lifecycle stamp the
// erasure ledger uses to tell a stale checkpoint left by an ABANDONED
// prior lifecycle of a reused session id apart from a mid-cascade retry
// of the current one (see CascadeEraser).
func (r *Registry) preflightErase(ctx context.Context, id string) (*Session, error) {
	if r.closed.Load() {
		return nil, ErrRegistryClosed
	}
	ident, ok := identity.From(ctx)
	if !ok {
		return nil, fmt.Errorf("sessions: Erase requires identity in ctx: %w", identity.ErrIdentityMissing)
	}
	if ident.SessionID != id {
		return nil, fmt.Errorf("sessions: Erase id=%q does not match ctx SessionID=%q", id, ident.SessionID)
	}
	stored, err := r.loadSession(ctx, ident)
	if err != nil {
		return nil, err
	}
	if !sameIdentity(stored.Identity, ident) {
		return nil, fmt.Errorf("%w: stored=(%s,%s,%s) ctx=(%s,%s,%s)",
			ErrIdentityMismatch,
			stored.Identity.TenantID, stored.Identity.UserID, stored.Identity.SessionID,
			ident.TenantID, ident.UserID, ident.SessionID)
	}
	if r.gcPolicy.RunningProbe != nil {
		running, perr := r.gcPolicy.RunningProbe(ctx, identity.Quadruple{Identity: ident})
		if perr != nil {
			return nil, fmt.Errorf("sessions: Erase probe: %w", perr)
		}
		if running {
			return nil, fmt.Errorf("%w: SessionID=%q", ErrSessionRunning, id)
		}
	}
	return stored, nil
}

// clearErased removes the session's in-memory catalogs (openSessions +
// idIndex) and its persisted per-(tenant, user) discovery catalog entry,
// all under ONE r.mu critical section. It performs no session-record delete
// (DeleteScope owns that). Idempotent — clearing an already-cleared session
// is a no-op.
//
// The catalog remove is folded INTO the r.mu section (it previously ran
// AFTER releasing r.mu, racing Open's addToCatalog as a lost update):
// holding r.mu across the map deletes AND the catalog read-modify-write
// serializes it with a concurrent Open of a sibling session, so an
// Open(C)-during-Erase(B) can never drop C's freshly-added catalog entry.
// Lock order is preserved — the eraser holds its striped per-session lock
// OUTSIDE r.mu (striped → r.mu).
func (r *Registry) clearErased(ctx context.Context, ident identity.Identity) error {
	if r.closed.Load() {
		return ErrRegistryClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.openSessions, ident.SessionID)
	delete(r.idIndex, ident.SessionID)
	// Remove the session from the (tenant, user) discovery catalog so a
	// later process does not re-hydrate a now-erased session id.
	if err := r.removeFromCatalog(ctx, ident.TenantID, ident.UserID, ident.SessionID); err != nil {
		return fmt.Errorf("sessions: Erase catalog cleanup: %w", err)
	}
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

// ListSnapshots implements sessions.SessionLister — the
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
// Concurrent reuse: ListSnapshots only reads `idIndex` /
// `openSessions` under the registry's mutex; no per-call state lives
// on `*Registry`. One Registry serves N concurrent ListSnapshots safely.
func (r *Registry) ListSnapshots(ctx context.Context, f SessionListFilter) ([]SessionSnapshot, error) {
	if r.closed.Load() {
		return nil, ErrRegistryClosed
	}

	// hydrate the in-memory idIndex from the persisted
	// per-(tenant, user) catalog so sessions created by a PRIOR process
	// (the StateStore survived a restart) are discoverable. The
	// StateStore has no List, so the catalog is how a fresh process
	// re-learns which SessionIDs exist. Hydration is keyed on each
	// (tenant, user) the filter names; an unscoped filter (admin fleet
	// view) cannot enumerate tenants without a store scan and is left to
	// whatever the live idIndex already holds (documented gap — a
	// store-level List is post-V1).
	for _, tenant := range f.TenantIDs {
		if tenant == "" {
			continue
		}
		for _, user := range f.UserIDs {
			if user == "" {
				continue
			}
			if herr := r.hydrateFromCatalog(ctx, tenant, user); herr != nil {
				return nil, fmt.Errorf("sessions: ListSnapshots hydrate: %w", herr)
			}
		}
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

// OldestRetainedAt reports the OBSERVED oldest session-open instant
// across the WHOLE retained session set — every (tenant, user, session)
// this registry has seen (open OR closed-but-retained) — as the
// identity-free runtime-wide retention horizon a fleet-observe caller
// reads on `runtime.health`. It is the runtime-wide analogue of
// events.RetentionReporter, discovered by type assertion at the posture
// wiring seam (no `Supports*` capability protocol — CLAUDE.md §4.4). The
// returned instant carries no per-session content — only the bare oldest
// OpenedAt — so it is safe to expose runtime-wide, never a cross-tenant
// enumeration.
//
// "Oldest RETAINED", not "oldest ever": the registry's idle GC reaps old
// sessions, so the horizon tracks what is still held. Returns
// (zero, false, nil) when the registry retains no sessions (present is
// false), and (zero, false, ErrRegistryClosed) after CloseRegistry.
//
// The catalog scanned is the in-memory `idIndex` (every SessionID Opened
// during this process's lifetime); a session created by a PRIOR process
// whose (tenant, user) has not been hydrated is not counted — the same
// documented lazy-hydration gap ListSnapshots carries for an unscoped
// fleet read (a store-level List is post-V1). A per-session load error
// is skipped rather than failing the whole horizon (best-effort
// observability, never a load-bearing path).
func (r *Registry) OldestRetainedAt(ctx context.Context) (time.Time, bool, error) {
	if r.closed.Load() {
		return time.Time{}, false, ErrRegistryClosed
	}

	r.mu.Lock()
	idents := make([]identity.Identity, 0, len(r.idIndex))
	for _, ident := range r.idIndex {
		idents = append(idents, ident)
	}
	r.mu.Unlock()

	var oldest time.Time
	for _, ident := range idents {
		if err := ctx.Err(); err != nil {
			return time.Time{}, false, err
		}
		stored, err := r.loadSession(ctx, ident)
		if err != nil {
			// A record may have been deleted out-of-band; skip it rather
			// than failing the whole horizon read.
			continue
		}
		if stored.OpenedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || stored.OpenedAt.Before(oldest) {
			oldest = stored.OpenedAt
		}
	}
	if oldest.IsZero() {
		return time.Time{}, false, nil
	}
	return oldest.UTC(), true, nil
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

// errSkipSave is the sentinel a mutateSession mutator returns to report
// "this call is a no-op — do not persist and do not fail" (Close's
// idempotent already-closed branch). Unexported and never wrapped;
// mutateSession swallows it after skipping the save.
var errSkipSave = errors.New("sessions: skip save")

// mutateSession is the ONE serialized read-modify-write path for the
// session.lifecycle record: it loads the record at ident, applies fn,
// and persists the result, all inside a single r.mu critical section.
//
// Every whole-record writer (Touch / Close / SetTitle / the GC reap)
// MUST route through it — a load→mutate→save that doesn't hold the lock
// across all three steps races the others as a classic lost update: a
// SetTitle that loads before a Close saves would re-persist
// Closed=false AFTER Close returned nil, resurrecting a closed session
// as a GC-invisible zombie (Close already dropped it from openSessions)
// and violating the pinned reopen-after-close invariant. Serializing
// the family also closes the Touch-vs-SetTitle field clobber, and —
// together with the eraser's deleteScopeSerialized — the
// re-persist-after-erasure window.
//
// fn runs WITH r.mu held: it may mutate the loaded record and the
// registry's in-memory maps (openSessions / idIndex), but MUST NOT call
// back into any registry method that takes r.mu, publish to the bus, or
// perform its own store I/O. Returning errSkipSave skips the save and
// reports success (the idempotent no-op shape); any other error aborts
// without persisting. Event publishes happen at the call sites AFTER
// the critical section, mirroring Close's original shape.
func (r *Registry) mutateSession(ctx context.Context, ident identity.Identity, fn func(*Session) error) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, err := r.loadSession(ctx, ident)
	if err != nil {
		return nil, err
	}
	if err := fn(stored); err != nil {
		if errors.Is(err, errSkipSave) {
			return stored, nil
		}
		return nil, err
	}
	if err := r.save(ctx, *stored); err != nil {
		return nil, err
	}
	return stored, nil
}

// deleteScopeSerialized runs the erasure cascade's irreversible
// StateStore.DeleteScope while holding the SAME r.mu the whole-record
// writers (mutateSession) hold across their load→save. Without it, a
// Touch / SetTitle that loaded the record before DeleteScope could save
// it back AFTER the clear — re-persisting the session.lifecycle record
// of an erased session (undermining the erasure-integrity guarantee
// that the erased triple stays empty). Under the shared lock the writer
// either completes fully before the clear (its write is removed with
// everything else) or loads after it and observes ErrSessionNotFound.
//
// The store is the CALLER's (the eraser's) StateStore handle — in every
// production assembly it is the same store the registry wraps, and the
// serialization guarantee only holds when it is. Lock ordering: the
// eraser holds its striped per-session lock OUTSIDE r.mu (striped →
// r.mu, the same order clearErased uses); no registry method acquires
// the striped locks, so no cycle exists.
func (r *Registry) deleteScopeSerialized(ctx context.Context, store state.StateStore, id identity.Identity) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return store.DeleteScope(ctx, id)
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
// surfaced by the StateStore (contract).
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

// unfenceSession lifts any erasure fence on the triple so a reused session
// id retains events normally (events.Fencer). Best-effort, mirroring the
// publish posture: the fence is an optional bus capability, and a fence
// lift only matters when the id is genuinely being reused after an erasure;
// a lift error (only ErrBusClosed) is logged, not fatal to the Open.
func (r *Registry) unfenceSession(ctx context.Context, ident identity.Identity) {
	fencer, ok := r.bus.(events.Fencer)
	if !ok {
		return
	}
	if err := fencer.Unfence(ctx, ident); err != nil {
		slog.WarnContext(ctx, "sessions: event-bus unfence on open failed — a reused session id may briefly drop events",
			slog.String("session_id", ident.SessionID),
			slog.String("tenant_id", ident.TenantID),
			slog.String("user_id", ident.UserID),
			slog.String("error", err.Error()))
	}
}

// sameIdentity is a triple equality check.
func sameIdentity(a, b identity.Identity) bool {
	return a.TenantID == b.TenantID && a.UserID == b.UserID && a.SessionID == b.SessionID
}

// Compile-time assertion: *Registry satisfies SessionRegistry.
var _ SessionRegistry = (*Registry)(nil)

// Compile-time assertion: *Registry satisfies SessionLister.
var _ SessionLister = (*Registry)(nil)
