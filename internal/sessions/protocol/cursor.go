package protocol

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// cursorVersion is the 1-byte version prefix every opaque pagination
// cursor carries. A future change to the cursor encoding bumps this; a
// mismatched version fails loudly with ErrInvalidRequest rather than
// silently degrading (D-122 risk-mitigation — CLAUDE.md §13).
const cursorVersion = "1"

// pageCursor is the decoded form of an opaque `sessions.list`
// pagination cursor. It records the sort-key value + the SessionID of
// the last row of the previous page so the next page resumes after it.
type pageCursor struct {
	// sortKeyNanos is the StartedAt / LastActivityAt of the boundary row
	// in Unix nanoseconds; unused for the cost sort.
	sortKeyNanos int64
	// costCents is the TotalCostCents of the boundary row; used only for
	// SessionSortCostDesc.
	costCents int64
	// sessionID is the boundary row's SessionID — the tie-breaker that
	// makes the order (and the cursor) total.
	sessionID string
}

// encodeCursor builds the opaque base64 cursor for the boundary row
// under the given sort. The wire form is `v|sortKey|cost|sessionID`,
// base64-url-encoded — opaque to clients (D-122 risk-mitigation).
func encodeCursor(row prototypes.SessionRow, srt prototypes.SessionSort) string {
	var sortKeyNanos int64
	switch srt {
	case prototypes.SessionSortLastActivityDesc:
		sortKeyNanos = row.LastActivityAt.UnixNano()
	case prototypes.SessionSortCostDesc:
		sortKeyNanos = 0
	default:
		sortKeyNanos = row.StartedAt.UnixNano()
	}
	raw := fmt.Sprintf("%s|%d|%d|%s", cursorVersion, sortKeyNanos, row.TotalCostCents, row.SessionID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses an opaque cursor string. An empty string decodes
// to nil (the first page). A malformed or version-mismatched cursor
// fails loudly with ErrInvalidRequest — never a silent reset to page 1
// (CLAUDE.md §13).
func decodeCursor(cursor string) (*pageCursor, error) {
	if cursor == "" {
		return nil, nil
	}
	rawBytes, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("%w: cursor is not valid base64", ErrInvalidRequest)
	}
	parts := strings.SplitN(string(rawBytes), "|", 4)
	if len(parts) != 4 {
		return nil, fmt.Errorf("%w: cursor has %d fields, want 4", ErrInvalidRequest, len(parts))
	}
	if parts[0] != cursorVersion {
		return nil, fmt.Errorf("%w: cursor version %q, want %q", ErrInvalidRequest, parts[0], cursorVersion)
	}
	sortKey, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: cursor sort-key is not an integer", ErrInvalidRequest)
	}
	cost, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: cursor cost is not an integer", ErrInvalidRequest)
	}
	if parts[3] == "" {
		return nil, fmt.Errorf("%w: cursor session id is empty", ErrInvalidRequest)
	}
	return &pageCursor{sortKeyNanos: sortKey, costCents: cost, sessionID: parts[3]}, nil
}

// afterCursor reports whether row sorts strictly after the cursor
// boundary under srt — i.e. row belongs on the page that resumes after
// the cursor. The comparison mirrors lessForSort: a row is "after the
// cursor" when it would sort after a synthetic row carrying the
// cursor's key.
func afterCursor(row prototypes.SessionRow, c pageCursor, srt prototypes.SessionSort) bool {
	switch srt {
	case prototypes.SessionSortStartedAsc:
		rk := row.StartedAt.UnixNano()
		if rk != c.sortKeyNanos {
			return rk > c.sortKeyNanos
		}
	case prototypes.SessionSortLastActivityDesc:
		rk := row.LastActivityAt.UnixNano()
		if rk != c.sortKeyNanos {
			return rk < c.sortKeyNanos
		}
	case prototypes.SessionSortCostDesc:
		if row.TotalCostCents != c.costCents {
			return row.TotalCostCents < c.costCents
		}
	default: // SessionSortStartedDesc
		rk := row.StartedAt.UnixNano()
		if rk != c.sortKeyNanos {
			return rk < c.sortKeyNanos
		}
	}
	return row.SessionID > c.sessionID
}
