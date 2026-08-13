package turns

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// cursorVersion is the version prefix every opaque page cursor
// carries. A future change to the cursor encoding bumps this; a
// mismatched version fails loudly with ErrInvalidCursor rather than
// silently degrading (CLAUDE.md §13).
const cursorVersion = "1"

// Cursor is the decoded form of an opaque List page cursor — the
// immutable keyset position of the boundary row of the previous page.
//
// Newest-first paging pages over `(Sequence DESC, TieBreaker DESC)`:
// a page request with cursor C returns the rows strictly AFTER C in
// that order — `(Seq < C.Seq) || (Seq == C.Seq && TurnID < C.TurnID)`.
// Because the ordering keys are immutable per turn, an already-issued
// cursor is stable under concurrent appends: a newly appended turn
// (higher sequence) can never satisfy it, and an already-returned
// turn can never be returned again — no skips, no duplicates.
type Cursor struct {
	// Seq is the boundary row's immutable sequence.
	Seq Seq
	// TurnID is the boundary row's immutable tie-breaker.
	TurnID TurnID
}

// Encode renders the cursor as its opaque wire form (versioned,
// base64url). Empty never encodes to "": a caller that wants the
// first page passes a nil Cursor.
func (c *Cursor) Encode() string {
	raw := fmt.Sprintf("%s|%d|%s", cursorVersion, int64(c.Seq), string(c.TurnID))
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses an opaque cursor string. An empty string
// decodes to (nil, nil) — the first page. A malformed,
// version-mismatched, or empty-turn-id cursor fails loudly with
// ErrInvalidCursor — never a silent reset to page one (CLAUDE.md §13).
func DecodeCursor(s string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	rawBytes, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: cursor is not valid base64", ErrInvalidCursor)
	}
	parts := strings.SplitN(string(rawBytes), "|", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: cursor has %d fields, want 3", ErrInvalidCursor, len(parts))
	}
	if parts[0] != cursorVersion {
		return nil, fmt.Errorf("%w: cursor version %q, want %q", ErrInvalidCursor, parts[0], cursorVersion)
	}
	seq, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: cursor sequence is not an integer", ErrInvalidCursor)
	}
	if parts[2] == "" {
		return nil, fmt.Errorf("%w: cursor turn id is empty", ErrInvalidCursor)
	}
	return &Cursor{Seq: Seq(seq), TurnID: TurnID(parts[2])}, nil
}

// ListOptions configures one List page. The zero value is the default
// first page of DefaultListLimit rows.
type ListOptions struct {
	// Before is the keyset position to page strictly after (older
	// than). Nil means the newest page.
	Before *Cursor
	// Limit bounds the page. Zero means DefaultListLimit; a limit
	// above MaxListLimit fails loudly with ErrInvalidInput.
	Limit int
}

// Page is one newest-first page of the projection. Rows are ordered
// `(Sequence DESC, TieBreaker DESC)` and are SNAPSHOT reads of the
// current rows at read time (sealed rows are immutable, so an
// already-served page never changes; mutable rows may update between
// pages but their ordering keys never move).
type Page struct {
	// Rows are the page's turns, newest first.
	Rows []TurnRow
	// NextCursor is the keyset position of the next page — pass it as
	// the next ListOptions.Before. Nil when HasMore is false.
	NextCursor *Cursor
	// HasMore reports whether older turns exist beyond this page
	// within the retained window.
	HasMore bool
	// Truncated reports whether the session's retained window ever
	// hit its bound (older turns exist in the durable event log but
	// were evicted from this projection). This is the explicit
	// bounded-projection marker — eviction is never silent.
	Truncated bool
}
