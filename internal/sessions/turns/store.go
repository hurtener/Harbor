package turns

import (
	"context"

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
//   - Mutable rows (pending / running / paused) are VERSIONED: every
//     accepted write bumps the row's Version, and UpdateTurnIf /
//     SealTurnIf refuse a stale expected version (ErrStaleVersion).
//     Sealed (terminal) rows are immutable: every mutation of a sealed
//     row fails with ErrTurnSealed.
//   - Every write is ERASURE-FENCED by a STORE-LOCAL durable session
//     fence: FenceSession marks the session fenced in the driver's OWN
//     backend, and AppendTurnIf / UpdateTurnIf / SealTurnIf /
//     SaveCheckpoint atomically refuse a fenced session
//     (ErrErasureFenced) in the same transaction as their write. A
//     driver cannot transactionally inspect arbitrary external
//     StateStore slots, so the fence lives with the rows it guards.
//     DeleteScope NEVER removes the fence — an erased session stays
//     fenced (no resurrection after replay / restart), and re-erase
//     stays idempotent.
//   - SaveCheckpoint is MONOTONIC and IDEMPOTENT: saving a sequence
//     at or below the stored checkpoint is a no-op (never a
//     regression).
//   - ListTurns pages newest-first by the immutable keys
//     `(Sequence DESC, TurnID DESC)` with no skips or duplicates
//     under concurrent appends, and reports the session's truncation
//     flag (retention eviction is explicit, never silent). The page
//     cursor is BOUND to its owning session + the session's projection
//     snapshot generation AND its authoritative boundary row: a
//     foreign-session cursor is refused with
//     ErrCursorForeignSession, a stale-snapshot cursor (the projection
//     was erased / rebuilt under the walk) with ErrCursorSnapshotStale,
//     a cursor whose boundary row is no longer retained with
//     ErrCursorExpired, and a forged / altered cursor that names a
//     retained boundary row but carries a sequence that does not equal
//     the stored row's immutable sequence with ErrInvalidCursor — each
//     a distinct domain error for Protocol mapping, and none ever
//     silently re-keysets. Appends during a walk never invalidate an
//     issued cursor.
//   - Deep-copy on every row boundary: a driver MUST NOT let caller
//     memory reach (or escape) durable state. A driver that retains a
//     caller-supplied row struct (an in-memory driver) stores a DEEP
//     copy of every slice and of every optional pointer-backed mutable
//     field (Answer.Ref, UsageMeasure.Value); reads (GetTurn /
//     ListTurns / returned rows) return deep copies too. Caller
//     mutation and concurrent reuse must never alias durable state
//     (the concurrent-reuse gate below).
//   - The store enforces the retention bound configured at its
//     construction: a session retains only its newest N rows; beyond
//     N the oldest rows are evicted and the session's truncation flag
//     is set. `Durable()` reports whether the backing store survives
//     a process restart (an in-memory driver reports false — explicit
//     restart loss; the projection then rebuilds via Reconcile, which
//     consults the runtime's durable erasure probe BEFORE rebuilding).
//   - Implementations MUST be safe for N concurrent goroutines on one
//     shared instance (the store contract's concurrent-reuse gate:
//     no data races, no identity bleed, no cancellation cross-talk,
//     no goroutine leaks).
type Store interface {
	// Durable reports whether the backing store survives a process
	// restart. An in-memory driver returns false: after a restart its
	// projection is EMPTY (rows, checkpoints, AND erasure fences gone
	// — explicit loss, never a silent claim of durability) and the
	// runtime rebuilds it by reconciling from sequence zero — a rebuild
	// the Projector gates on the runtime's durable erasure probe
	// (ErasureProbe) so an erased session is never rebuilt merely
	// because the in-memory store restarted. A durable driver's
	// STORE-LOCAL erasure fence survives restarts permanently.
	Durable() bool

	// AppendTurnIf creates the mutable row for id / TurnID, minting
	// the next immutable per-session sequence atomically with the
	// write. Idempotent on the turn id: an existing row is returned
	// unchanged (no error) — a replay of an already-applied append is
	// a no-op. Refused with ErrErasureFenced when the session is
	// fenced, ErrStoreClosed after Close. The driver may evict the
	// session's oldest rows beyond its retention bound, setting the
	// session's truncation flag.
	AppendTurnIf(ctx context.Context, id identity.Identity, row TurnRow) (TurnRow, error)

	// UpdateTurnIf atomically replaces a MUTABLE row at an expected
	// version. Refused with ErrStaleVersion on a version mismatch,
	// ErrTurnSealed when the stored row is already sealed,
	// ErrTurnNotFound when the row is not retained, ErrErasureFenced
	// when the session is fenced. On success the returned row carries
	// Version + 1.
	UpdateTurnIf(ctx context.Context, id identity.Identity, turnID TurnID, expectedVersion int, row TurnRow) (TurnRow, error)

	// SealTurnIf atomically replaces a MUTABLE row with its SEALED
	// terminal form. Same refusals as UpdateTurnIf (ErrStaleVersion /
	// ErrTurnSealed / ErrTurnNotFound / ErrErasureFenced); a sealed
	// row is immutable thereafter. On success the returned row
	// carries Sealed == true and Version + 1.
	SealTurnIf(ctx context.Context, id identity.Identity, turnID TurnID, expectedVersion int, row TurnRow) (TurnRow, error)

	// FenceSession marks id's session as ERASURE-FENCED in the
	// driver's own durable backend, ATOMICALLY with respect to the
	// row writes it guards. After FenceSession, AppendTurnIf /
	// UpdateTurnIf / SealTurnIf / SaveCheckpoint refuse with
	// ErrErasureFenced, and the fence is NEVER removed by
	// DeleteScope — the erasure cascade calls FenceSession BEFORE
	// DeleteScope, so an erased session stays fenced across replay
	// and restart (no resurrection). Idempotent: fencing an already
	// fenced session is a no-op. No cross-runtime authority is
	// invented — this is this store's own fence, not an external slot.
	FenceSession(ctx context.Context, id identity.Identity) error

	// GetTurn reads one retained row; ErrTurnNotFound when the turn
	// was never created, was evicted past the retention bound, or was
	// erased. Identity scoping is the store's job: a turn under a
	// different (tenant, user) is not addressable from this triple.
	GetTurn(ctx context.Context, id identity.Identity, turnID TurnID) (TurnRow, error)

	// ListTurns returns one newest-first keyset page of at most limit
	// retained rows strictly older than before (nil before = the
	// newest page), ordered by (Sequence DESC, TurnID DESC). next is
	// non-nil iff older rows remain (the driver fetches limit+1 to
	// know exactly); info carries the page's snapshot binding and
	// completeness. The cursor is BOUND to (session, projection
	// snapshot, authoritative boundary row): a foreign-session cursor
	// fails with ErrCursorForeignSession, a stale-snapshot cursor (the
	// session's snapshot generation advanced — e.g. the projection was
	// erased / rebuilt) with ErrCursorSnapshotStale, a cursor whose
	// boundary row is no longer retained with ErrCursorExpired, and a
	// forged / altered cursor that names a retained boundary row but
	// carries a sequence that does not equal the stored row's
	// immutable sequence with ErrInvalidCursor — it would otherwise
	// silently skip or repeat rows. No skips / no duplicates under
	// concurrent appends (immutable ordering keys; appends never
	// invalidate an issued cursor).
	ListTurns(ctx context.Context, id identity.Identity, before *Cursor, limit int) (rows []TurnRow, next *Cursor, info ListPageInfo, err error)

	// LoadCheckpoint returns the session's last-applied runtime event
	// sequence; 0 when none was ever saved (a fresh store, an erased
	// session — the erasure cleared it — or an in-memory store after
	// restart). Reads are not fenced: a fenced session's checkpoint is
	// still readable (it reads 0 after erasure).
	LoadCheckpoint(ctx context.Context, id identity.Identity) (uint64, error)

	// SaveCheckpoint records the session's last-applied runtime event
	// sequence. MONOTONIC and IDEMPOTENT: a sequence at or below the
	// stored checkpoint is a no-op (never a regression), so a
	// reconcile retry cannot rewind the checkpoint. Refused with
	// ErrErasureFenced when the session is fenced — a rebuild must not
	// advance the checkpoint of an erased session (no resurrection).
	SaveCheckpoint(ctx context.Context, id identity.Identity, seq uint64) error

	// DeleteScope removes every retained turn row and the checkpoint
	// under id (the erasure cascade's projection leg). Idempotent: an
	// absent scope returns (0, nil). The erasure FENCE is NOT removed
	// — the caller (the runtime's erasure cascade or Projector.Erase)
	// sets it via FenceSession before calling DeleteScope, and this
	// method deliberately never clears it, so an erased session stays
	// fenced. This method only clears this projection's own records.
	DeleteScope(ctx context.Context, id identity.Identity) (int, error)

	// Close releases driver resources; subsequent calls fail with
	// ErrStoreClosed (wrapped). Idempotent.
	Close(ctx context.Context) error
}

// ListPageInfo is the store-level per-page metadata ListTurns returns
// alongside the rows and next cursor. The Projector maps it onto the
// public Page's snapshot / completeness / remaining fields.
type ListPageInfo struct {
	// Snapshot is the session's projection snapshot generation (as-of
	// retention generation) the page — and its minted cursors — bind
	// to. It starts at 0 for a fresh session and advances on erasure
	// (DeleteScope), so a cursor minted before an erase can never be
	// confused with one minted after.
	Snapshot uint64
	// Remaining is the exact number of older RETAINED rows beyond the
	// page when the driver knows it without a full scan, or -1
	// otherwise. CountExact reports which.
	Remaining  int
	CountExact bool
	// Truncated reports whether the session's retained window ever hit
	// its bound (retention eviction is explicit, never silent).
	Truncated bool
}
