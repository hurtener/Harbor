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
	// EventTypeSessionErased marks a completed data-lifecycle erasure
	// (`sessions.delete`). It is the ONLY durable trace of the erasure —
	// a redacted, content-free record-of-fact emitted under the actor's
	// observability scope, NEVER under the erased session's own identity
	// (re-persisting under the erased triple would leave the session's
	// `state.history` non-empty — RFC §6.13 / §7).
	EventTypeSessionErased events.EventType = "session.erased"
	// EventTypeSessionTitleChanged marks a successful `sessions.set_title`
	// (set OR clear). Content-free by construction: the title
	// string NEVER rides this payload — only the SessionID and the
	// resulting TitleSource. Consumers refetch the projection
	// (`sessions.list` / `sessions.inspect`) for the title text.
	EventTypeSessionTitleChanged events.EventType = "session.title_changed"
)

func init() {
	events.RegisterEventType(EventTypeSessionOpened)
	events.RegisterEventType(EventTypeSessionTouched)
	events.RegisterEventType(EventTypeSessionClosed)
	events.RegisterEventType(EventTypeSessionGCReaped)
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

// SessionTitleChangedPayload reports a successful `sessions.set_title`
// call (set OR clear). Carries the SessionID and the resulting Source
// ONLY — the title string itself NEVER rides this payload (the same
// content-free contract SessionErasedPayload follows): the title is
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
	// Source is the resulting TitleSource after the call: "manual" when
	// a non-empty title was set, "" (TitleSourceUnset) when the title
	// was cleared. Never "auto" — the wire verb cannot produce it.
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
