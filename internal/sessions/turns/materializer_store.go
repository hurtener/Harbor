package turns

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/identity"
)

// ErrLeaseNotHeld reports that a lease-fenced materializer write (a
// cursor save) was refused because the caller does not currently hold
// the single-writer lease — it expired, was released, or another
// instance took it over. The caller must stop writing and stand by.
var ErrLeaseNotHeld = errors.New("turns: materializer lease not held")

// MaterializerStore is the durable coordination surface the turn
// materializer needs on top of the row Store. Every production turns
// driver (in-memory / SQLite / Postgres) implements it next to Store,
// pinned by conformancetest.RunMaterializerStore. Beyond the lease,
// cursor and repair queue below, it persists the durable recovery state
// a resumed materializer needs for turns older than its cursor (pending
// completion marks, task routes), and its drivers enforce the lease
// write fence (WithWriteFence) on every projection write. Its core
// concerns:
//
//   - The SINGLE-WRITER LEASE. Several runtime instances can share one
//     durable projection (a rolling deploy overlaps the old and the new
//     instance). Only the lease holder materializes; the others stand
//     by and take over when the holder releases the lease or stops
//     renewing it (expiry). The lease is a row, not a session-level
//     advisory lock, so it works through transaction-pooling proxies.
//     Expiry is evaluated against the store's own clock (the database
//     clock for Postgres), so instance clock skew cannot split the
//     lease.
//   - The RESUME CURSOR. The holder persists the global event-source
//     position it can safely resume from, so a restart (or a takeover)
//     pages only the events after it instead of the whole log. Saves
//     are fenced by the lease (ErrLeaseNotHeld) and monotonic.
//   - The IDENTITY REPAIR queue. A row appended while its task record
//     was not readable carries an unavailable query / agent. Such rows
//     are flagged at append; the materializer pages the flagged rows
//     (bounded, keyset ordered) and fills the missing components once
//     from the task record (RepairTurnIdentity), or settles the flag
//     when the record proves there is nothing to fill.
type MaterializerStore interface {
	// AcquireMaterializerLease takes or renews the single-writer lease
	// for holder with the given time-to-live. The lease is granted when
	// it is free, expired, or already held by holder (a renewal extends
	// it). The result reports whether holder now holds it, the current
	// holder, and the persisted resume cursor (0 when none was saved).
	AcquireMaterializerLease(ctx context.Context, holder string, ttl time.Duration) (MaterializerLease, error)

	// ReleaseMaterializerLease gives the lease up immediately when
	// holder holds it (a graceful shutdown hands over without waiting
	// for expiry). Releasing a lease held by someone else, or no lease,
	// is a no-op. The persisted cursor is kept.
	ReleaseMaterializerLease(ctx context.Context, holder string) error

	// SaveMaterializerCursor persists the resume cursor. It is fenced by
	// the lease: refused with ErrLeaseNotHeld unless holder holds an
	// unexpired lease. Monotonic: a cursor at or below the stored one is
	// a no-op (never a regression).
	SaveMaterializerCursor(ctx context.Context, holder string, cursor uint64) error

	// ListIdentityRepairCandidates returns at most limit flagged rows
	// strictly after the keyset position after (the zero key starts at
	// the beginning), ordered by (tenant, user, session, turn id). Work
	// is proportional to limit, served by an index over the flag.
	ListIdentityRepairCandidates(ctx context.Context, after IdentityRepairKey, limit int) ([]IdentityRepairKey, error)

	// RepairTurnIdentity applies fill to the row through ApplyIdentityFill
	// — the single sanctioned write that may touch a SEALED row, and only
	// to fill a query / agent component that is still Unavailable (a
	// complete component is never overwritten). When the fill changed the
	// row it bumps Version; the repair flag is cleared when the fill
	// filled every unavailable component or fill.Settle is set. Refused
	// with ErrErasureFenced for a fenced session; ErrTurnNotFound when the
	// row is not retained. Returns the stored row and whether it changed.
	RepairTurnIdentity(ctx context.Context, id identity.Identity, turnID TurnID, fill IdentityFill) (TurnRow, bool, error)

	// MarkPendingCompletion durably records that the row's complete seal
	// is deferred until its answer source converges, so a restart or a
	// takeover rebuilds the pending-completion queue from the store
	// instead of from (possibly long past) events. A sealed row is a
	// no-op; SealTurnIf clears the mark atomically with the seal.
	// ErrTurnNotFound when the row is not retained, ErrErasureFenced for
	// a fenced session.
	MarkPendingCompletion(ctx context.Context, id identity.Identity, turnID TurnID) error

	// ListPendingCompletions pages (keyset, like the repair queue) the
	// rows carrying a pending-completion mark.
	ListPendingCompletions(ctx context.Context, after IdentityRepairKey, limit int) ([]IdentityRepairKey, error)

	// SaveTaskRoute upserts one task's routing binding (parent task, run
	// id, root turn) so a materializer that resumes past the task's spawn
	// event still routes its events exactly. Refused with
	// ErrErasureFenced for a fenced session. Routes whose root turn seals
	// are pruned with the seal; the session's erasure removes the rest.
	SaveTaskRoute(ctx context.Context, id identity.Identity, r TaskRoute) error

	// LoadTaskRoute reads the binding of one task (found=false if none).
	LoadTaskRoute(ctx context.Context, id identity.Identity, taskID string) (TaskRoute, bool, error)

	// LoadRunRoute reads the binding of the task most recently bound to
	// runID (found=false if none).
	LoadRunRoute(ctx context.Context, id identity.Identity, runID string) (TaskRoute, bool, error)
}

// TaskRoute is one persisted task routing binding.
type TaskRoute struct {
	TaskID       string
	ParentTaskID string // "" = a root task
	RunID        string // "" = none bound
	RootTaskID   string // the root turn's task id ("" = not yet known)
	// EventSeq is the spawn event's sequence (orders run rebinding).
	EventSeq uint64
}

// MaterializerLease is the result of a lease acquisition attempt.
type MaterializerLease struct {
	// Held reports whether the caller now holds the lease.
	Held bool
	// Holder is the current holder ("" when none).
	Holder string
	// Generation is the lease generation: it advances every time the
	// lease changes hands (a takeover, or an acquisition after a
	// release), never on a renewal. Writes fenced with an older
	// generation are refused (see WithWriteFence).
	Generation uint64
	// Cursor is the persisted resume cursor (0 = none saved).
	Cursor uint64
}

// WriteFence binds projection writes to one lease tenure: every write a
// driver performs under a context carrying a fence checks, in the same
// transaction as the write, that the lease row still names Holder at
// Generation, and refuses with ErrLeaseNotHeld otherwise. A paused or
// slow former holder therefore cannot write after a takeover, whatever
// its own clock believes. Renewal alone cannot provide this.
type WriteFence struct {
	Holder     string
	Generation uint64
}

type writeFenceKey struct{}

// WithWriteFence returns ctx carrying the lease write fence.
func WithWriteFence(ctx context.Context, f WriteFence) context.Context {
	return context.WithValue(ctx, writeFenceKey{}, f)
}

// WriteFenceFrom returns the write fence ctx carries, if any. Drivers
// implementing MaterializerStore honour it on AppendTurnIf, UpdateTurnIf,
// SealTurnIf, SaveCheckpoint, RepairTurnIdentity and
// SaveMaterializerCursor.
func WriteFenceFrom(ctx context.Context) (WriteFence, bool) {
	f, ok := ctx.Value(writeFenceKey{}).(WriteFence)
	return f, ok
}

// IdentityRepairKey addresses one flagged row; it is also the keyset
// position of ListIdentityRepairCandidates.
type IdentityRepairKey struct {
	Identity identity.Identity
	TurnID   TurnID
}

// Less reports whether k orders strictly before o in the repair keyset
// order (tenant, user, session, turn id; byte-wise).
func (k IdentityRepairKey) Less(o IdentityRepairKey) bool {
	if k.Identity.TenantID != o.Identity.TenantID {
		return k.Identity.TenantID < o.Identity.TenantID
	}
	if k.Identity.UserID != o.Identity.UserID {
		return k.Identity.UserID < o.Identity.UserID
	}
	if k.Identity.SessionID != o.Identity.SessionID {
		return k.Identity.SessionID < o.Identity.SessionID
	}
	return k.TurnID < o.TurnID
}

// IdentityFill is the bounded, already-redacted query / agent data a
// repair may fill from the authoritative task record. A component is
// filled only when it is Present, carries a value, and the row's
// component is still Unavailable.
type IdentityFill struct {
	QueryPresent bool
	Query        string
	QueryAt      time.Time

	AgentPresent       bool
	AgentID            string
	AgentName          string
	AgentBindingSource AgentBindingSource

	// Settle clears the row's repair flag even when a component stays
	// unavailable — the task record proved there is nothing to fill (it
	// exists and carries no query / agent, or it can no longer appear).
	Settle bool
}

// NeedsIdentityRepair reports whether a row carries an unavailable
// query or agent component — the condition that flags it for repair at
// append.
func NeedsIdentityRepair(row TurnRow) bool {
	return row.Query.Complete == CompletenessUnavailable || row.Agent.Complete == CompletenessUnavailable
}

// ApplyIdentityFill is the pure, driver-shared repair rule: it returns
// the row with fill applied to every still-Unavailable query / agent
// component, whether anything changed, and whether the repair flag may
// be cleared (nothing unavailable remains, or fill.Settle). Complete
// components are NEVER overwritten. The bounds mirror Append: the query
// is at most MaxQueryRunes valid UTF-8, the binding provenance is the
// closed set. Drivers call it inside their write transaction so the
// rule is single-sourced.
func ApplyIdentityFill(row TurnRow, fill IdentityFill) (TurnRow, bool, bool, error) {
	changed := false
	if fill.QueryPresent && fill.Query != "" && row.Query.Complete == CompletenessUnavailable {
		if !utf8.ValidString(fill.Query) {
			return TurnRow{}, false, false, fmt.Errorf("%w: repair query is not valid UTF-8", ErrInvalidInput)
		}
		if utf8.RuneCountInString(fill.Query) > MaxQueryRunes {
			return TurnRow{}, false, false, fmt.Errorf("%w: repair query exceeds %d runes", ErrInvalidInput, MaxQueryRunes)
		}
		at := fill.QueryAt
		if at.IsZero() {
			at = row.Query.At
		}
		row.Query = Query{Text: fill.Query, At: at, Complete: CompletenessComplete}
		changed = true
	}
	if fill.AgentPresent && (fill.AgentID != "" || fill.AgentName != "") && row.Agent.Complete == CompletenessUnavailable {
		source, err := deriveBindingSource(fill.AgentID, fill.AgentBindingSource)
		if err != nil {
			return TurnRow{}, false, false, err
		}
		row.Agent = Agent{
			ID:            fill.AgentID,
			Name:          fill.AgentName,
			BindingSource: source,
			Complete:      agentCompleteness(fill.AgentID, fill.AgentName),
		}
		changed = true
	}
	settled := fill.Settle || !NeedsIdentityRepair(row)
	return row, changed, settled, nil
}
