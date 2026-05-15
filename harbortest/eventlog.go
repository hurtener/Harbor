package harbortest

import (
	"sync"

	"github.com/hurtener/Harbor/internal/events"
)

// EventLog is the captured event stream from one RunOnce invocation
// (or, when the caller shares a Deps.Bus across runs, the union of
// every event published during the lifetime of the captured
// subscription). Events are appended in the order they arrive from
// the bus subscription channel — Harbor's bus assigns monotonic
// Sequence numbers, so the slice order matches Sequence order
// within a single bus.
//
// Concurrent reuse contract (D-025). EventLog serialises appends
// behind an internal mutex; All() returns a defensive copy so
// callers may iterate while the producer side is still publishing.
// Readers and writers may run concurrently.
type EventLog struct {
	mu     sync.RWMutex
	events []events.Event
}

// newEventLog constructs an empty EventLog. Kept unexported so
// callers always obtain logs via RunOnce — a freshly-constructed
// EventLog with no subscription wiring is a footgun.
func newEventLog() *EventLog {
	return &EventLog{events: make([]events.Event, 0, 16)}
}

// append records ev. Called from the kit's bus-drain goroutine; the
// signature is unexported so external callers cannot mutate the log
// (they consume via All / RecordedEvents / Len).
func (l *EventLog) append(ev events.Event) {
	l.mu.Lock()
	l.events = append(l.events, ev)
	l.mu.Unlock()
}

// All returns a defensive copy of the captured events in arrival
// order. The slice is safe to retain across goroutines; mutating
// the slice does not affect the EventLog.
func (l *EventLog) All() []events.Event {
	l.mu.RLock()
	out := make([]events.Event, len(l.events))
	copy(out, l.events)
	l.mu.RUnlock()
	return out
}

// Len returns the number of captured events. Equivalent to
// len(l.All()) but avoids the defensive copy when callers only
// need the count.
func (l *EventLog) Len() int {
	l.mu.RLock()
	n := len(l.events)
	l.mu.RUnlock()
	return n
}

// RecordedEvents returns the subset of captured events whose
// Identity.RunID equals runID, in arrival order. Returns an empty
// slice (never nil) when no event matches — callers iterate without
// a nil-check.
//
// This is the test author's hook into "what did THIS run emit?"
// when one EventLog spans multiple runs (the typical case when a
// test author shares a Deps.Bus across several RunOnce invocations
// to verify isolation behaviour).
func (l *EventLog) RecordedEvents(runID string) []events.Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]events.Event, 0, len(l.events))
	for _, ev := range l.events {
		if ev.Identity.RunID == runID {
			out = append(out, ev)
		}
	}
	return out
}
