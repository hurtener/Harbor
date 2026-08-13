package turns

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cursorVersion is the version prefix every opaque page cursor
// carries. A future change to the cursor encoding bumps this; a
// mismatched version fails loudly with ErrInvalidCursor rather than
// silently degrading (CLAUDE.md §13). Version 2 binds the cursor to
// its owning session AND the projection snapshot (as-of retention
// generation) it was minted against.
const cursorVersion = "2"

// Cursor is the decoded form of an opaque List page cursor: the
// immutable keyset position of the boundary row of the previous page,
// BOUND to the session and the projection snapshot it was minted
// against.
//
// Newest-first paging pages over `(Sequence DESC, TieBreaker DESC)`:
// a page request with cursor C returns the rows strictly AFTER C in
// that order — `(Seq < C.Seq) || (Seq == C.Seq && TurnID < C.TurnID)`.
// Because the ordering keys are immutable per turn, an already-issued
// cursor is stable under concurrent appends: a newly appended turn
// (higher sequence) can never satisfy it, and an already-returned
// turn can never be returned again — no skips, no duplicates.
//
// The binding fields are the opaque-cursor integrity contract: a
// cursor minted for a DIFFERENT session (SessionID mismatch) is
// rejected with ErrCursorForeignSession; a cursor whose projection
// snapshot generation no longer matches the session's current snapshot
// (the projection was erased / rebuilt — the as-of retention
// generation advanced) is rejected with ErrCursorSnapshotStale; a
// cursor whose boundary row is no longer retained (evicted past the
// retention bound) is rejected with ErrCursorExpired. Each is a
// DISTINCT domain error so the Protocol layer can map them onto
// distinct wire codes.
type Cursor struct {
	// SessionID is the owning session the cursor was minted for.
	SessionID string
	// Snapshot is the projection snapshot generation (as-of retention
	// generation) the cursor was minted against — the session's
	// snapshot at read time. Erasure advances it; a mismatch rejects
	// the cursor as stale.
	Snapshot uint64
	// Seq is the boundary row's immutable sequence.
	Seq Seq
	// TurnID is the boundary row's immutable tie-breaker.
	TurnID TurnID
}

// Encode renders the cursor as its opaque wire form (versioned,
// base64url). Empty never encodes to "": a caller that wants the
// first page passes a nil Cursor.
func (c *Cursor) Encode() string {
	raw := fmt.Sprintf("%s|%d|%d|%s|%s", cursorVersion, c.Snapshot, int64(c.Seq), string(c.TurnID), c.SessionID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses an opaque cursor string. An empty string
// decodes to (nil, nil) — the first page. A malformed,
// version-mismatched, or empty-field cursor fails loudly with
// ErrInvalidCursor — never a silent reset to page one (CLAUDE.md §13).
// The session / snapshot BINDING is enforced by the store at list
// time (foreign-session / stale-snapshot / expired-cursor refusals).
func DecodeCursor(s string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	rawBytes, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: cursor is not valid base64", ErrInvalidCursor)
	}
	parts := strings.SplitN(string(rawBytes), "|", 5)
	if len(parts) != 5 {
		return nil, fmt.Errorf("%w: cursor has %d fields, want 5", ErrInvalidCursor, len(parts))
	}
	if parts[0] != cursorVersion {
		return nil, fmt.Errorf("%w: cursor version %q, want %q", ErrInvalidCursor, parts[0], cursorVersion)
	}
	snapshot, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: cursor snapshot is not an integer", ErrInvalidCursor)
	}
	seq, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: cursor sequence is not an integer", ErrInvalidCursor)
	}
	if parts[3] == "" {
		return nil, fmt.Errorf("%w: cursor turn id is empty", ErrInvalidCursor)
	}
	if parts[4] == "" {
		return nil, fmt.Errorf("%w: cursor session id is empty", ErrInvalidCursor)
	}
	return &Cursor{SessionID: parts[4], Snapshot: snapshot, Seq: Seq(seq), TurnID: TurnID(parts[3])}, nil
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
	// AsOf is the instant this snapshot page was read — the page's
	// as-of timestamp.
	AsOf time.Time
	// Snapshot is the projection snapshot generation the page (and
	// its cursors) binds to: the session's as-of retention generation
	// at read time. A cursor minted against a different generation is
	// rejected by the store as stale.
	Snapshot uint64
	// Remaining is the exact number of older RETAINED turns beyond
	// this page when the store knows it exactly (CountExact), or -1
	// when it does not. Never counts rows evicted past the retention
	// bound.
	Remaining int
	// CountExact reports whether Remaining is exact.
	CountExact bool
	// Complete is true when the session's retained window is untruncated
	// (no retention eviction), so the paging walk covers the FULL
	// projection. False when the window was truncated — PartialReason
	// names why.
	Complete bool
	// PartialReason names why the page (and the walk it belongs to) is
	// PARTIAL: "retention_eviction" when older turns exist in the
	// durable event log but were evicted from this projection. Empty
	// when Complete is true. The stable token is Protocol-mappable.
	PartialReason string
	// LiveResumeSeq is the durable event-log sequence of the newest
	// observation reflected in this page — the maximum LastAppliedEventSeq
	// across the page's rows (0 when no row recorded one). It is
	// enough live-resume sequence to compose subscribe-before-page: a
	// consumer pages the NEWEST page, then subscribes to the session's
	// event stream from LiveResumeSeq+1 and processes events from
	// there — nothing the page reflects is re-processed, and nothing
	// applied after the read is missed.
	LiveResumeSeq uint64
}
