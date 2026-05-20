package notifications

import "errors"

// ErrUnmappable is returned by Map when a triggering event is
// structurally invalid for mapping — its payload type does not match
// the expected typed payload for its declared event type. Callers
// compare via errors.Is.
//
// Fail-loudly per CLAUDE.md §13: a structurally invalid trigger event
// MUST NOT silently degrade to "no notifications." The Subscriber
// catches ErrUnmappable, logs at Error, and emits a runtime.error
// observability event; it does NOT republish a malformed notification.
var ErrUnmappable = errors.New("notifications: triggering event structurally invalid for mapping")
