package turns

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg/sessionfence"
	"github.com/hurtener/Harbor/internal/identity"
)

// Store is the mandatory, conformance-ready persistence interface of
// the turn projection. A concrete driver (in-memory / SQLite /
// Postgres — the §4.4 typed-wrapper-over-StateStore shape) lands in a
// later lane and MUST pass the conformance suite
// (`internal/sessions/turns/conformancetest`); this package ships no
// driver.
//
// Contract highlights (each pinned by the conformance suite):
//
//   - Identity is mandatory: every method takes the full
//     (tenant, user, session) triple and fails closed with
//     ErrIdentityRequired on an incomplete one. Run id is NOT a
//     storage identity axis — turns are session-scoped; the turn id
//     keys rows WITHIN the triple.
//   - AppendTurnIf mints the next immutable per-session sequence
//     ATOMICALLY with the row write, so concurrent appends can never
//     observe a duplicate sequence. It is IDEMPOTENT on the turn id:
//     an existing row is returned as-is (a replay of an
//     already-applied append is a no-op, never an error).
//   - Mutable rows (running / paused) are VERSIONED: every accepted
//     write bumps the row's Version, and UpdateTurnIf / SealTurnIf
//     refuse a stale expected version (ErrStaleVersion). Sealed
//     (terminal) rows are immutable: every mutation of a sealed row
//     fails with ErrTurnSealed.
//   - Every write is ERASURE-FENCED: the caller passes the Fence the
//     write must clear (FenceFor), and the store refuses the write
//     when either fence slot is present (ErrErasureFenced).
//   - SaveCheckpoint is MONOTONIC and IDEMPOTENT: saving a sequence
//     at or below the stored checkpoint is a no-op (never a
//     regression).
//   - ListTurns pages newest-first by the immutable keys
//     `(Sequence DESC, TurnID DESC)` with no skips or duplicates
//     under concurrent appends, and reports the session's truncation
//     flag (retention eviction is explicit, never silent).
//   - The store enforces the retention bound configured at its
//     construction: a session retains only its newest N rows; beyond
//     N the oldest rows are evicted and the session's truncation flag
//     is set. `Durable()` reports whether the backing store survives
//     a process restart (an in-memory driver reports false — explicit
//     restart loss; the projection then rebuilds via Reconcile).
//   - Implementations MUST be safe for N concurrent goroutines on one
//     shared instance (the store contract's concurrent-reuse gate:
//     no data races, no identity bleed, no cancellation cross-talk,
//     no goroutine leaks).
type Store interface {
	// Durable reports whether the backing store survives a process
	// restart. An in-memory driver returns false: after a restart its
	// projection is EMPTY (rows AND checkpoints gone — explicit loss,
	// never a silent claim of durability) and the runtime rebuilds it
	// by reconciling from sequence zero.
	Durable() bool

	// AppendTurnIf creates the mutable row for id / TurnID, minting
	// the next immutable per-session sequence atomically with the
	// write. Idempotent on the turn id: an existing row is returned
	// unchanged (no error) — a replay of an already-applied append is
	// a no-op. Refused with ErrErasureFenced when fence does not
	// clear, ErrStoreClosed after Close. The driver may evict the
	// session's oldest rows beyond its retention bound, setting the
	// session's truncation flag.
	AppendTurnIf(ctx context.Context, id identity.Identity, row TurnRow, fence Fence) (TurnRow, error)

	// UpdateTurnIf atomically replaces a MUTABLE row at an expected
	// version. Refused with ErrStaleVersion on a version mismatch,
	// ErrTurnSealed when the stored row is already sealed,
	// ErrTurnNotFound when the row is not retained, ErrErasureFenced
	// when fence does not clear. On success the returned row carries
	// Version + 1.
	UpdateTurnIf(ctx context.Context, id identity.Identity, turnID TurnID, expectedVersion int, row TurnRow, fence Fence) (TurnRow, error)

	// SealTurnIf atomically replaces a MUTABLE row with its SEALED
	// terminal form. Same refusals as UpdateTurnIf (ErrStaleVersion /
	// ErrTurnSealed / ErrTurnNotFound / ErrErasureFenced); a sealed
	// row is immutable thereafter. On success the returned row
	// carries Sealed == true and Version + 1.
	SealTurnIf(ctx context.Context, id identity.Identity, turnID TurnID, expectedVersion int, row TurnRow, fence Fence) (TurnRow, error)

	// GetTurn reads one retained row; ErrTurnNotFound when the turn
	// was never created, was evicted past the retention bound, or was
	// erased. Identity scoping is the store's job: a turn under a
	// different (tenant, user) is not addressable from this triple.
	GetTurn(ctx context.Context, id identity.Identity, turnID TurnID) (TurnRow, error)

	// ListTurns returns one newest-first keyset page of at most limit
	// retained rows strictly older than before (nil before = the
	// newest page), ordered by (Sequence DESC, TurnID DESC). next is
	// non-nil iff older rows remain (the driver fetches limit+1 to
	// know exactly); truncated reports whether the session's retained
	// window ever hit its bound. No skips / no duplicates under
	// concurrent appends (immutable ordering keys).
	ListTurns(ctx context.Context, id identity.Identity, before *Cursor, limit int) (rows []TurnRow, next *Cursor, truncated bool, err error)

	// LoadCheckpoint returns the session's last-applied runtime event
	// sequence; 0 when none was ever saved (a fresh store, or an
	// in-memory store after restart).
	LoadCheckpoint(ctx context.Context, id identity.Identity) (uint64, error)

	// SaveCheckpoint records the session's last-applied runtime event
	// sequence. MONOTONIC and IDEMPOTENT: a sequence at or below the
	// stored checkpoint is a no-op (never a regression), so a
	// reconcile retry cannot rewind the checkpoint.
	SaveCheckpoint(ctx context.Context, id identity.Identity, seq uint64) error

	// DeleteScope removes every retained turn row and the checkpoint
	// under id (the erasure cascade's projection leg). Idempotent: an
	// absent scope returns (0, nil). The erasure FENCE is the
	// cascade's separate concern (it writes the pending ledger and the
	// terminal tombstone via the shared sessionfence slots) — this
	// method only clears this projection's own records.
	DeleteScope(ctx context.Context, id identity.Identity) (int, error)

	// Close releases driver resources; subsequent calls fail with
	// ErrStoreClosed (wrapped). Idempotent.
	Close(ctx context.Context) error
}

// Slot names one durable erasure-fence slot a turn write must clear:
// the slot must be ABSENT for the write to proceed. The slots are the
// sessionfence pending-erasure ledger and terminal-erasure tombstone
// (internal/agentcfg/sessionfence) — they live in the shared
// StateStore (the erasure cascade writes them), so a driver maps a
// Slot onto its backend's exact-absence check (StateStore
// SlotExpectation with an empty expected EventID for StateStore-backed
// drivers).
type Slot struct {
	// Identity is the slot's full identity (the erasure slots ride the
	// reserved observability scope — see sessionfence).
	Identity identity.Quadruple
	// Kind is the slot's StateStore kind (the literal erasure
	// pending / tombstone kind).
	Kind string
}

// Fence is the erasure fence a turn write must clear: both named
// slots must be ABSENT. A fence with a present pending ledger means an
// erasure is in flight; a present tombstone means it has converged —
// in either case no turn write is admitted (ErrErasureFenced).
type Fence struct {
	// PendingAbsent is the session's pending-erasure ledger slot; must
	// be absent.
	PendingAbsent Slot
	// TombstoneAbsent is the session's terminal erasure tombstone
	// slot; must be absent.
	TombstoneAbsent Slot
}

// FenceFor builds the erasure fence every turn write for id must
// clear, from the shared sessionfence slots (the same slots the
// runtime's session-erasure cascade writes). A session whose erasure
// has begun or converged therefore admits no turn writes.
func FenceFor(id identity.Identity) (Fence, error) {
	if err := identity.Validate(id); err != nil {
		return Fence{}, fmt.Errorf("turns: fence for %q: %w", id.SessionID, ErrIdentityRequired)
	}
	q := identity.Quadruple{Identity: id}
	pendingQ, pendingKind, err := sessionfence.PendingSlot(q)
	if err != nil {
		return Fence{}, fmt.Errorf("turns: pending erasure slot: %w", err)
	}
	tombQ, tombKind, err := sessionfence.TombstoneSlot(q)
	if err != nil {
		return Fence{}, fmt.Errorf("turns: tombstone erasure slot: %w", err)
	}
	return Fence{
		PendingAbsent:   Slot{Identity: pendingQ, Kind: pendingKind},
		TombstoneAbsent: Slot{Identity: tombQ, Kind: tombKind},
	}, nil
}
