package sessions

import (
	"github.com/hurtener/Harbor/internal/events"
)

// Session lifecycle event types. Each is registered with the events
// package's exhaustive registry via init() so Publish accepts them
// without ErrUnknownEventType. Subscribers can filter on these via
// events.Filter.Types.
const (
	EventTypeSessionOpened   events.EventType = "session.opened"
	EventTypeSessionTouched  events.EventType = "session.touched"
	EventTypeSessionClosed   events.EventType = "session.closed"
	EventTypeSessionGCReaped events.EventType = "session.gc_reaped"
	// EventTypeSessionReopened marks a closed session being re-activated in
	// place (RFC §6.9 amended): the next Open / EnsureOpen on a
	// Closed record clears the closed flags, preserves the immutable identity
	// and OpenedAt, and resumes the durable history. Content-free by
	// construction — carries only the SessionID, the reopen timestamp, and
	// the (operator/GC-set) prior close reason, NEVER user content or the
	// resumed conversation body. Consumers refetch the projection / windowed
	// history for the body.
	EventTypeSessionReopened events.EventType = "session.reopened"
	// EventTypeSessionErased marks a completed data-lifecycle erasure
	// (`sessions.delete`). It is the ONLY durable trace of the erasure —
	// a redacted, content-free record-of-fact emitted under the actor's
	// observability scope, NEVER under the erased session's own identity
	// (re-persisting under the erased triple would leave the session's
	// `state.history` non-empty — RFC §6.13 / §7).
	EventTypeSessionErased events.EventType = "session.erased"
	// EventTypeSessionTitleChanged marks a successful title change. It has TWO
	// producers: the `sessions.set_title` verb via SetTitle (a manual set OR
	// clear) and the internal auto-namer via SetTitleAuto (an auto set).
	// Content-free by construction: the title string NEVER rides this payload
	// — only the SessionID and the resulting TitleSource ("manual" / "auto" /
	// "" on clear). Consumers refetch the projection (`sessions.list` /
	// `sessions.inspect`) for the title text.
	EventTypeSessionTitleChanged events.EventType = "session.title_changed"
)

func init() {
	events.RegisterEventType(EventTypeSessionOpened)
	events.RegisterEventType(EventTypeSessionTouched)
	events.RegisterEventType(EventTypeSessionClosed)
	events.RegisterEventType(EventTypeSessionGCReaped)
	events.RegisterEventType(EventTypeSessionReopened)
	events.RegisterEventType(EventTypeSessionErased)
	events.RegisterEventType(EventTypeSessionTitleChanged)
}

// SessionOpenedPayload reports a successful Open. Carries the
// SessionID and the OpenedAt timestamp; the identity triple lives on
// the Event itself, so it is intentionally NOT duplicated here.
//
// SafePayload by construction — no secret-shaped fields.
type SessionOpenedPayload struct {
	events.SafeSealed
	SessionID string
	OpenedAt  int64 // unix nanoseconds; identity-of-record across drivers
}

// SessionTouchedPayload reports a Touch. Carries the SessionID and
// the new LastSeen timestamp. SafePayload by construction.
type SessionTouchedPayload struct {
	events.SafeSealed
	SessionID string
	LastSeen  int64 // unix nanoseconds
}

// SessionClosedPayload reports a Close. Carries the SessionID, the
// ClosedAt timestamp, and the operator-provided Reason. Reason is a
// short caller-controlled string — callers MUST NOT pass tool args,
// raw user input, or any secret-shaped material; the bus does not
// re-redact SafePayload types.
type SessionClosedPayload struct {
	events.SafeSealed
	SessionID string
	ClosedAt  int64 // unix nanoseconds
	Reason    string
}

// SessionGCReapedPayload reports a GC sweep reaping. Reason is one
// of "gc:idle" or "gc:hard_cap"; the same string is also stored in
// Session.ClosedReason. SafePayload by construction.
type SessionGCReapedPayload struct {
	events.SafeSealed
	SessionID string
	ReapedAt  int64 // unix nanoseconds
	Reason    string
}

// SessionReopenedPayload reports a session being re-activated from a
// Closed state (RFC §6.9 amended). Carries the SessionID, the
// ReopenedAt timestamp, and the PriorClosedReason (the reason the session
// had been closed with — "gc:idle" / "gc:hard_cap" for a GC reap, or the
// operator-supplied Close reason). Content-free by construction: NO title,
// NO user content, NO conversation body — the identity triple lives on the
// Event itself. Consumers refetch the projection / windowed history
// for the resumed conversation. SafePayload by construction.
type SessionReopenedPayload struct {
	events.SafeSealed
	// SessionID is the session that was reopened.
	SessionID string
	// ReopenedAt is the reopen timestamp (unix nanoseconds).
	ReopenedAt int64
	// PriorClosedReason is the reason the session had been closed with
	// before this reopen — a GC reason ("gc:idle" / "gc:hard_cap") or the
	// operator-supplied Close reason. A bounded, closed-set-or-operator
	// string, never user content (the Close reason is caller-controlled but
	// MUST NOT carry secret-shaped material — the same contract
	// SessionClosedPayload.Reason follows).
	PriorClosedReason string
}

// SessionTitleChangedPayload reports a successful title change from EITHER
// producer — the `sessions.set_title` verb (SetTitle: manual set or clear)
// or the internal auto-namer (SetTitleAuto: auto set). Carries the SessionID
// and the resulting Source ONLY — the title string itself NEVER rides this
// payload (the same content-free contract SessionErasedPayload follows): the
// title is
// user-derived free text, and SafePayload types are published WITHOUT a
// redactor pass (the bus skips the redactor for types sealed via
// SafeSealed), so any raw content here would leak straight to every
// subscriber. Consumers that want the title text refetch
// `sessions.list` / `sessions.inspect`.
//
// SafePayload by construction — every field is a bounded id or a
// closed-set enum string.
type SessionTitleChangedPayload struct {
	events.SafeSealed
	// SessionID is the session whose title changed.
	SessionID string
	// Source is the resulting TitleSource after the call — a three-value
	// domain: "manual" when a caller set a non-empty title via
	// sessions.set_title, "auto" when the internal auto-namer set it via
	// SetTitleAuto, and "" (TitleSourceUnset) when the title was cleared. The
	// wire verb produces only "manual" / ""; the "auto" value comes from the
	// runtime's own auto-naming path.
	Source string
}

// SessionErasedPayload reports a completed session erasure. It carries
// the erased SessionID plus the per-store deletion counts and the
// erasure timestamp — non-sensitive telemetry only, NO user content. The
// erased session id is a payload FIELD (not the event's Identity): the
// event is published under the actor's observability scope so it never
// re-persists durable state under the erased triple. SafePayload by
// construction — every field is a bounded id, count, or timestamp.
//
// This is the durable record-of-fact for a right-to-erasure operation,
// and publishing it is part of `sessions.delete`'s SUCCESS CRITERIA
// rather than a best-effort afterthought: Erase only ever returns
// success once this payload has been redacted and durably published.
// The count fields are CUMULATIVE across any converging retries the
// cascade needed (see internal/sessions.CascadeEraser's package godoc)
// — the true total actually removed, never just the last attempt's own
// count.
type SessionErasedPayload struct {
	events.SafeSealed
	// SessionID is the session that was erased.
	SessionID string
	// StateRecordsDeleted is the number of StateStore records removed by
	// the kind-agnostic scope delete. Cumulative across converging
	// attempts.
	StateRecordsDeleted int
	// ArtifactsDeleted is the number of artifacts removed. Cumulative
	// across converging attempts.
	ArtifactsDeleted int
	// MemoryPurged is true when the session's memory was flushed clean.
	MemoryPurged bool
	// ErasedAt is the erasure timestamp (unix nanoseconds).
	ErasedAt int64
}
