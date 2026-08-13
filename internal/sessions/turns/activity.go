package turns

import (
	"context"

	"github.com/hurtener/Harbor/internal/identity"
)

// ActivityReader is the SEPARATELY NAMED, OPTIONAL activity-read
// contract the row's explicit lower-bound points at.
//
// A turn row's Activity component retains only the newest
// MaxActivityRows (Activity.More / Activity.Dropped carry the explicit
// lower-bound: "at least these rows exist; the full activity is
// readable elsewhere"). The full activity is read through THIS
// contract — a single named method the runtime wires over the durable
// event log (state.history), never through the projection's own store.
//
// It is deliberately NOT a generic subresource framework: there is
// exactly one activity read, named once, and nothing else on a turn is
// subresource-readable. A caller that sees Activity.More == false is
// guaranteed the row's window is the complete activity and never needs
// to call this.
type ActivityReader interface {
	// Activity returns the turn's full activity rows (oldest first),
	// bounded to limit (which must be >= 1). hasMore reports whether
	// rows beyond the returned page exist; the caller keeps paging
	// from the tail until it reads false. Errors are loud: a reader
	// that cannot reach the durable log returns an error, never a
	// silently-short page.
	Activity(ctx context.Context, id identity.Identity, turnID TurnID, limit int) (rows []ActivityRow, hasMore bool, err error)
}
