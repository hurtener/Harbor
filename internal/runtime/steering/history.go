package steering

import (
	"sync"
	"time"
)

// MaxControlHistory is the default per-session cap on the applied-control
// history ring (RFC §6.3 — "control-history capped per session"). A
// long-lived session can receive an unbounded number of control events
// over its lifetime; the runtime keeps only the newest MaxControlHistory
// applied entries so the bookkeeping is bounded. Overridable per RunLoop
// via WithMaxControlHistory.
const MaxControlHistory = 256

// AppliedControl is one entry in a session's applied-control history. It
// is the runtime's own bookkeeping record — what control was applied to
// which run, when, and whether the side effect succeeded. The rejected
// caller payload itself is NOT carried (mirroring ControlRejectedPayload):
// the history is a low-cardinality audit trail, not a payload archive.
type AppliedControl struct {
	// Type is the control type that was applied.
	Type ControlType
	// RunID is the run the control targeted (a session may host multiple
	// concurrent runs — the run component disambiguates).
	RunID string
	// AppliedAt is the wall-clock time the side effect was applied,
	// stamped from the RunLoop's Clock.
	AppliedAt time.Time
	// Err is the non-nil error when the side effect failed (e.g. a
	// PRIORITIZE whose task does not exist). A failed apply is still
	// recorded — the history is the audit trail, and a silent drop would
	// violate CLAUDE.md §5 "fail loudly".
	Err error
}

// controlHistory is the process-wide, per-session capped applied-control
// log. It is a compiled-artifact component (D-025): the per-session ring
// map is guarded by a documented-invariant sync.Mutex; no per-run state
// lives outside the map. One controlHistory is shared by one RunLoop
// across every run it drives.
//
// The cap is per session, not per run: RFC §6.3 says "control-history
// capped per session", and a session is the steering-relevant scope
// (multiple runs in one session share the operator's steering attention).
type controlHistory struct {
	cap int

	mu    sync.Mutex
	rings map[string][]AppliedControl // keyed by SessionID
}

// newControlHistory builds a per-session capped applied-control log. A
// non-positive cap falls back to MaxControlHistory.
func newControlHistory(cap int) *controlHistory {
	if cap <= 0 {
		cap = MaxControlHistory
	}
	return &controlHistory{
		cap:   cap,
		rings: make(map[string][]AppliedControl),
	}
}

// record appends an AppliedControl entry to the session's ring, evicting
// the oldest entry when the ring is at cap (newest-wins). sessionID is the
// session scope; the entry itself carries the run component.
func (h *controlHistory) record(sessionID string, entry AppliedControl) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ring := h.rings[sessionID]
	ring = append(ring, entry)
	if len(ring) > h.cap {
		// Drop the oldest entries so the ring is exactly cap-sized. A
		// copy keeps the backing array from growing unbounded across the
		// session's lifetime.
		trimmed := make([]AppliedControl, h.cap)
		copy(trimmed, ring[len(ring)-h.cap:])
		ring = trimmed
	}
	h.rings[sessionID] = ring
}

// snapshot returns a copy of the session's applied-control history in
// oldest-to-newest order. The returned slice is owned by the caller —
// controlHistory keeps no reference to it. An unknown session returns an
// empty (non-nil) slice.
func (h *controlHistory) snapshot(sessionID string) []AppliedControl {
	h.mu.Lock()
	defer h.mu.Unlock()
	ring := h.rings[sessionID]
	out := make([]AppliedControl, len(ring))
	copy(out, ring)
	return out
}

// forget drops a session's history entirely. Called when a session ends
// so the map does not grow unbounded across a long-lived process. Forget
// on an unknown session is a no-op.
func (h *controlHistory) forget(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.rings, sessionID)
}

// len returns the number of entries currently held for a session.
// Primarily for tests and observability.
func (h *controlHistory) len(sessionID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rings[sessionID])
}
