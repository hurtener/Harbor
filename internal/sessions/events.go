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
)

func init() {
	events.RegisterEventType(EventTypeSessionOpened)
	events.RegisterEventType(EventTypeSessionTouched)
	events.RegisterEventType(EventTypeSessionClosed)
	events.RegisterEventType(EventTypeSessionGCReaped)
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
// re-redact SafePayload types (D-020 / D-028).
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
