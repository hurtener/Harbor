package harbortest

import (
	"strings"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// AssertSequence verifies that `want` appears as an ordered
// subsequence of the captured event types in log. The captured log
// may contain ADDITIONAL events between matches — only the order of
// `want` is checked. This is the right semantic for flow-level
// tests where bus-internal events (audit.admin_scope_used,
// bus.dropped, etc.) may interleave with the agent's emits and the
// test author cares only about the sequence of meaningful types.
//
// Returns true on a successful match. On failure, calls t.Errorf
// naming the first missing want entry and listing the captured
// event-type sequence so the diff is actionable, then returns false.
//
// Empty `want` always matches (vacuously). A captured log shorter
// than `want` always fails.
func AssertSequence(t TestingT, log *EventLog, want []events.EventType) bool {
	t.Helper()
	if len(want) == 0 {
		return true
	}
	if log == nil {
		t.Errorf("harbortest: AssertSequence called with nil EventLog (want %d types)", len(want))
		return false
	}
	captured := log.All()
	idx := 0
	for _, ev := range captured {
		if idx < len(want) && ev.Type == want[idx] {
			idx++
		}
		if idx == len(want) {
			return true
		}
	}
	t.Errorf("harbortest: AssertSequence: missing %q at position %d of want=%v; captured=%v",
		want[idx], idx, want, capturedTypes(captured))
	return false
}

// capturedTypes returns the EventType sequence from captured. Used
// by AssertSequence's error message; exposed in test diagnostics so
// the diff is readable.
func capturedTypes(captured []events.Event) []events.EventType {
	out := make([]events.EventType, len(captured))
	for i, ev := range captured {
		out[i] = ev.Type
	}
	return out
}

// AssertNoLeaks verifies the cross-tenant / cross-session isolation
// contract over the captured EventLog. Specifically:
//
//  1. Group the events by their identity triple (TenantID, UserID,
//     SessionID).
//  2. Collect the set of RunIDs each triple owns.
//  3. For every event, the event's RunID MUST be owned by the
//     event's own identity triple — an event tagged with triple A
//     whose RunID belongs to triple B is a leak (run-id cross-talk).
//  4. Additionally, the event's payload MAY embed an identity
//     quadruple (RFC §6.4 tool payloads, planner payloads); if it
//     does, the embedded triple MUST match the event's outer
//     identity triple. A payload triple from a different identity is
//     also a leak (payload cross-talk — the worst kind, since it
//     means the producer captured a foreign identity).
//
// The kit's RunOnce subscribes with Admin scope so the log naturally
// observes events across identity triples — that's the source of
// data this assertion analyses. Test authors that share a single
// Deps.Bus across multiple RunOnce invocations get an automatic
// regression test for run-isolation by piping the union log through
// AssertNoLeaks at the end of the test.
//
// Returns true on success; on failure calls t.Errorf naming the
// offending event(s) and their identity context, then returns false.
func AssertNoLeaks(t TestingT, log *EventLog) bool {
	t.Helper()
	if log == nil {
		t.Errorf("harbortest: AssertNoLeaks called with nil EventLog")
		return false
	}
	captured := log.All()

	// Phase 1: build "which triple owns which RunIDs?" map. We
	// determine ownership from the events themselves — the first
	// triple to publish an event under a RunID is its owner. (For
	// the kit's typical use this is exact; the bus's monotonic
	// Sequence makes the first-publisher choice well-defined.)
	type triple struct{ tenant, user, session string }
	owner := make(map[string]triple, len(captured))
	for _, ev := range captured {
		if ev.Identity.RunID == "" {
			continue
		}
		t3 := triple{tenant: ev.Identity.TenantID, user: ev.Identity.UserID, session: ev.Identity.SessionID}
		if _, ok := owner[ev.Identity.RunID]; !ok {
			owner[ev.Identity.RunID] = t3
		}
	}

	// Phase 2: walk every event and flag any whose outer identity
	// disagrees with the RunID owner OR whose payload embeds a
	// foreign identity quadruple.
	leaks := 0
	for i, ev := range captured {
		evT := triple{tenant: ev.Identity.TenantID, user: ev.Identity.UserID, session: ev.Identity.SessionID}
		if ev.Identity.RunID != "" {
			if own, ok := owner[ev.Identity.RunID]; ok && own != evT {
				t.Errorf("harbortest: AssertNoLeaks: event #%d type=%q tagged %s but RunID=%q is owned by %s (run-id cross-talk)",
					i, ev.Type, formatTriple(evT), ev.Identity.RunID, formatTriple(own))
				leaks++
			}
		}
		if pq, ok := payloadQuadruple(ev.Payload); ok {
			pT := triple{tenant: pq.TenantID, user: pq.UserID, session: pq.SessionID}
			if (pT.tenant != "" || pT.user != "" || pT.session != "") && pT != evT {
				t.Errorf("harbortest: AssertNoLeaks: event #%d type=%q tagged %s but payload carries %s (payload cross-talk)",
					i, ev.Type, formatTriple(evT), formatTriple(pT))
				leaks++
			}
		}
	}
	return leaks == 0
}

// formatTriple returns a stable string repr of an identity triple.
// Used in AssertNoLeaks error messages; the exact format is a
// readable comma-joined "tenant=… user=… session=…" because t.Errorf
// output is consumed by humans.
func formatTriple(t struct{ tenant, user, session string }) string {
	var sb strings.Builder
	sb.WriteString("(tenant=")
	sb.WriteString(quoteOrDash(t.tenant))
	sb.WriteString(" user=")
	sb.WriteString(quoteOrDash(t.user))
	sb.WriteString(" session=")
	sb.WriteString(quoteOrDash(t.session))
	sb.WriteString(")")
	return sb.String()
}

func quoteOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return "\"" + s + "\""
}

// payloadQuadruple extracts the identity quadruple from a payload
// if the payload's concrete type embeds one. Many Harbor payloads
// (tools.ToolInvokedPayload, tools.ToolCompletedPayload, etc.) carry
// `Identity identity.Quadruple` as a named field — we reach for it
// via a small type switch on the known shapes plus a reflection
// fallback for arbitrary payloads.
//
// Returns ok=false when the payload doesn't expose an identity
// quadruple — that case is treated as "no payload-level claim to
// check" by AssertNoLeaks.
func payloadQuadruple(p events.EventPayload) (identity.Quadruple, bool) {
	if p == nil {
		return identity.Quadruple{}, false
	}
	type identityHolder interface {
		IdentityQuadruple() identity.Quadruple
	}
	if h, ok := p.(identityHolder); ok {
		return h.IdentityQuadruple(), true
	}
	// Reflection fallback: look for an `Identity` field of type
	// identity.Quadruple. We use unsafe-free reflection — read-only
	// field lookup is fine for the kit's purposes.
	return reflectQuadruple(p)
}
