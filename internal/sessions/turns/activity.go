package turns

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
)

// ActivityReader is the SEPARATELY NAMED, OPTIONAL activity-read
// contract the row's explicit lower-bound points at.
//
// A turn row's Activity component retains only the newest
// configured-window rows (Activity.More / Activity.Dropped carry the
// explicit lower-bound: "at least these rows exist; the full activity
// is readable elsewhere"). The inline window is configured on the
// projector (WithActivityLimit) and must cover the runtime's
// configured per-turn tool-call budget when that budget is at or below
// the Protocol ceiling; when the budget EXCEEDS the ceiling
// (MaxActivityRows) the inline window is capped at the ceiling, the
// row overflows honestly (More + Dropped + Partial), and the full
// activity is read through THIS contract — the named bounded
// fallback. The reader is wired by the runtime over the durable event
// log (state.history), never through the projection's own store.
//
// It is deliberately NOT a generic subresource framework: there is
// exactly one activity read, named once, and nothing else on a turn is
// subresource-readable. A caller that sees Activity.More == false is
// guaranteed the row's window is the complete activity and never needs
// to call this.
type ActivityReader interface {
	// Activity returns one OLDEST-FIRST page of the turn's full
	// activity rows strictly after before (nil = the first / oldest
	// page), bounded to limit. The keyset key is each row's IMMUTABLE
	// Position ordinal: a page request with cursor C returns the rows
	// with Position > C.Position, so appends (which only add HIGHER
	// positions) can never satisfy an already-issued cursor — no skips,
	// no duplicates, stable under concurrent append. next is non-nil
	// iff rows beyond the page exist (the reader fetches limit+1 to
	// know exactly); the page is COMPLETE when next is nil. Errors are
	// loud: a reader that cannot reach the durable log returns an
	// error, never a silently-short page.
	Activity(ctx context.Context, id identity.Identity, turnID TurnID, before *ActivityCursor, limit int) (rows []ActivityRow, next *ActivityCursor, err error)
}

// ActivityCursor is the decoded keyset position of one activity
// paging page: the immutable Position ordinal of the last row of the
// previous page (nil = the first / oldest page). Because Position is
// immutable per row and appends only ever add HIGHER positions, an
// issued cursor is stable under concurrent appends — an already-read
// row can never be returned again and an appended row never slips
// before it.
type ActivityCursor struct {
	// Position is the boundary row's immutable 0-based ordinal.
	Position int
}

// ActivityPage is one OLDEST-FIRST page of a turn's full activity,
// served by Projector.PageActivity through the wired ActivityReader.
type ActivityPage struct {
	// Rows are the page's activity rows, oldest first.
	Rows []ActivityRow
	// NextCursor is the keyset position of the next page — pass it as
	// the next PageActivity before. Nil when HasMore is false.
	NextCursor *ActivityCursor
	// HasMore reports whether older activity rows exist beyond this
	// page.
	HasMore bool
}

// PageActivity pages the turn's FULL activity oldest-first through the
// runtime-wired bounded ActivityReader — the named fallback for rows
// whose inline window overflowed (Activity.More == true). The page
// size is validated against the bounded contract ([1,
// MaxActivityPageSize]) so no caller can turn the paged read into an
// accidental unbounded dump, and the keyset cursor guarantees no skips
// / no duplicates under concurrent appends (rows only ever gain higher
// positions).
//
// A Projector constructed without WithActivityReader refuses the call
// loudly (ErrInvalidInput) — an over-budget turn's full activity is
// only readable when the runtime wired the fallback.
func (p *Projector) PageActivity(ctx context.Context, id identity.Identity, turnID TurnID, before *ActivityCursor, limit int) (ActivityPage, error) {
	if err := validateIdentity(id); err != nil {
		return ActivityPage{}, err
	}
	if turnID == "" {
		return ActivityPage{}, fmt.Errorf("%w: turn id is empty", ErrInvalidInput)
	}
	if limit < 1 || limit > MaxActivityPageSize {
		return ActivityPage{}, fmt.Errorf("%w: activity page limit %d outside [1, %d]", ErrInvalidInput, limit, MaxActivityPageSize)
	}
	if p.activityReader == nil {
		return ActivityPage{}, fmt.Errorf("%w: no activity reader wired (WithActivityReader) — the full activity of an over-budget turn is unreadable", ErrInvalidInput)
	}
	rows, next, err := p.activityReader.Activity(ctx, id, turnID, before, limit)
	if err != nil {
		return ActivityPage{}, fmt.Errorf("turns: page activity %q: %w", turnID, err)
	}
	return ActivityPage{
		Rows:       rows,
		NextCursor: next,
		HasMore:    next != nil,
	}, nil
}
