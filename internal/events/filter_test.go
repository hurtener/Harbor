package events_test

import (
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// makeEvent constructs a synthetic event for filter tests. Type,
// identity, timestamp are caller-controlled; Sequence is zero (the
// MatchWire / FilterFromWire surface is bus-independent — Sequence is
// the bus's concern).
func makeEvent(typ events.EventType, tenant, user, session, run string, at time.Time) events.Event {
	return events.Event{
		Type: typ,
		Identity: identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  tenant,
				UserID:    user,
				SessionID: session,
			},
			RunID: run,
		},
		OccurredAt: at,
		// Payload is intentionally a SafePayload so we don't pull in
		// the bus's redactor path; the filter is header-only.
		Payload: events.BusDroppedPayload{
			FromSeq: 1, ToSeq: 1, DroppedCount: 0, SubscriberID: 0,
		},
	}
}

// TestMatchWire_EmptyFilterMatchesEverything pins the "empty filter is
// the no-op default" contract — a request that does not narrow on any
// axis (every set empty, both windows zero) matches every event.
func TestMatchWire_EmptyFilterMatchesEverything(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	ev := makeEvent(events.EventTypeRuntimeError, "t1", "u1", "s1", "r1", now)
	if !events.MatchWire(ev, prototypes.EventFilter{}) {
		t.Fatal("empty filter should match every event")
	}
}

// TestMatchWire_AxisMatrix walks every axis a wire filter narrows by —
// event types, tenant, user, session, run — and pins the match/no-match
// behaviour for each.
func TestMatchWire_AxisMatrix(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	ev := makeEvent(events.EventTypeRuntimeError, "t1", "u1", "s1", "r1", now)

	tests := []struct {
		name string
		f    prototypes.EventFilter
		want bool
	}{
		{
			name: "EventTypes match",
			f:    prototypes.EventFilter{EventTypes: []string{"runtime.error"}},
			want: true,
		},
		{
			name: "EventTypes miss",
			f:    prototypes.EventFilter{EventTypes: []string{"tool.failed"}},
			want: false,
		},
		{
			name: "EventTypes match in set of many",
			f:    prototypes.EventFilter{EventTypes: []string{"tool.failed", "runtime.error", "runtime.warning"}},
			want: true,
		},
		{
			name: "TenantIDs match",
			f:    prototypes.EventFilter{TenantIDs: []string{"t1"}},
			want: true,
		},
		{
			name: "TenantIDs miss",
			f:    prototypes.EventFilter{TenantIDs: []string{"t2"}},
			want: false,
		},
		{
			name: "TenantIDs match in cross-tenant set",
			f:    prototypes.EventFilter{TenantIDs: []string{"t1", "t2"}},
			want: true,
		},
		{
			name: "UserIDs match",
			f:    prototypes.EventFilter{UserIDs: []string{"u1"}},
			want: true,
		},
		{
			name: "UserIDs miss",
			f:    prototypes.EventFilter{UserIDs: []string{"u2"}},
			want: false,
		},
		{
			name: "SessionIDs match",
			f:    prototypes.EventFilter{SessionIDs: []string{"s1"}},
			want: true,
		},
		{
			name: "SessionIDs miss",
			f:    prototypes.EventFilter{SessionIDs: []string{"s99"}},
			want: false,
		},
		{
			name: "RunIDs match",
			f:    prototypes.EventFilter{RunIDs: []string{"r1"}},
			want: true,
		},
		{
			name: "RunIDs miss",
			f:    prototypes.EventFilter{RunIDs: []string{"r99"}},
			want: false,
		},
		{
			name: "compound match",
			f: prototypes.EventFilter{
				EventTypes: []string{"runtime.error"},
				TenantIDs:  []string{"t1"},
				UserIDs:    []string{"u1"},
				SessionIDs: []string{"s1"},
				RunIDs:     []string{"r1"},
			},
			want: true,
		},
		{
			name: "compound miss on one axis",
			f: prototypes.EventFilter{
				EventTypes: []string{"runtime.error"},
				TenantIDs:  []string{"t1"},
				UserIDs:    []string{"u1"},
				SessionIDs: []string{"s99"}, // mismatch
				RunIDs:     []string{"r1"},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := events.MatchWire(ev, tc.f)
			if got != tc.want {
				t.Fatalf("MatchWire(%+v) = %v, want %v", tc.f, got, tc.want)
			}
		})
	}
}

// TestMatchWire_TimeBounds pins the half-open [Since, Until) window
// semantics: Since is inclusive, Until is exclusive, and an empty
// bound means "unbounded on that side."
func TestMatchWire_TimeBounds(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		at   time.Time
		f    prototypes.EventFilter
		want bool
	}{
		{
			name: "Since unset accepts old event",
			at:   t0.Add(-24 * time.Hour),
			f:    prototypes.EventFilter{Until: t0.Add(1 * time.Hour)},
			want: true,
		},
		{
			name: "Until unset accepts new event",
			at:   t0.Add(24 * time.Hour),
			f:    prototypes.EventFilter{Since: t0.Add(-1 * time.Hour)},
			want: true,
		},
		{
			name: "exact Since boundary inclusive",
			at:   t0,
			f:    prototypes.EventFilter{Since: t0, Until: t0.Add(1 * time.Hour)},
			want: true,
		},
		{
			name: "exact Until boundary exclusive",
			at:   t0.Add(1 * time.Hour),
			f:    prototypes.EventFilter{Since: t0, Until: t0.Add(1 * time.Hour)},
			want: false,
		},
		{
			name: "before Since",
			at:   t0.Add(-1 * time.Nanosecond),
			f:    prototypes.EventFilter{Since: t0, Until: t0.Add(1 * time.Hour)},
			want: false,
		},
		{
			name: "after Until",
			at:   t0.Add(2 * time.Hour),
			f:    prototypes.EventFilter{Since: t0, Until: t0.Add(1 * time.Hour)},
			want: false,
		},
		{
			name: "inside both bounds",
			at:   t0.Add(30 * time.Minute),
			f:    prototypes.EventFilter{Since: t0, Until: t0.Add(1 * time.Hour)},
			want: true,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := makeEvent(events.EventTypeRuntimeError, "t1", "u1", "s1", "r1", tc.at)
			got := events.MatchWire(ev, tc.f)
			if got != tc.want {
				t.Fatalf("MatchWire(at=%v, since=%v, until=%v) = %v, want %v",
					tc.at, tc.f.Since, tc.f.Until, got, tc.want)
			}
		})
	}
}

// TestFilterFromWire_BackfillsCallerTuple — an empty wire filter takes
// its identity from the caller's triple. RequiresAdminScope is false.
func TestFilterFromWire_BackfillsCallerTuple(t *testing.T) {
	t.Parallel()
	conv := events.FilterFromWire(prototypes.EventFilter{}, "tenant-A", "user-A", "session-A")
	if conv.RequiresAdminScope {
		t.Fatal("empty wire filter must not require admin scope")
	}
	if conv.Filter.Tenant != "tenant-A" || conv.Filter.User != "user-A" || conv.Filter.Session != "session-A" {
		t.Fatalf("triple not backfilled: %+v", conv.Filter)
	}
	if conv.Filter.Run != "" {
		t.Fatalf("Run should be empty when wire RunIDs is empty: got %q", conv.Filter.Run)
	}
	if len(conv.Filter.Types) != 0 {
		t.Fatalf("Types should be empty: got %v", conv.Filter.Types)
	}
}

// TestFilterFromWire_RequiresAdminOnCrossTenant — naming a tenant
// other than the caller's, or naming multiple tenants, requires admin.
func TestFilterFromWire_RequiresAdminOnCrossTenant(t *testing.T) {
	t.Parallel()
	t.Run("single non-caller tenant", func(t *testing.T) {
		t.Parallel()
		conv := events.FilterFromWire(
			prototypes.EventFilter{TenantIDs: []string{"tenant-B"}},
			"tenant-A", "user-A", "session-A",
		)
		if !conv.RequiresAdminScope {
			t.Fatal("cross-tenant filter must require admin scope")
		}
	})
	t.Run("multiple tenants", func(t *testing.T) {
		t.Parallel()
		conv := events.FilterFromWire(
			prototypes.EventFilter{TenantIDs: []string{"tenant-A", "tenant-B"}},
			"tenant-A", "user-A", "session-A",
		)
		if !conv.RequiresAdminScope {
			t.Fatal("multi-tenant filter must require admin scope")
		}
	})
	t.Run("caller's tenant only", func(t *testing.T) {
		t.Parallel()
		conv := events.FilterFromWire(
			prototypes.EventFilter{TenantIDs: []string{"tenant-A"}},
			"tenant-A", "user-A", "session-A",
		)
		if conv.RequiresAdminScope {
			t.Fatal("same-tenant filter must not require admin scope")
		}
	})
}

// TestFilterFromWire_TypesAndWindow — Types are copied through and the
// time window is normalised to UTC.
func TestFilterFromWire_TypesAndWindow(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	conv := events.FilterFromWire(
		prototypes.EventFilter{
			EventTypes: []string{"runtime.error", "tool.failed"},
			Since:      since,
			Until:      until,
		},
		"tenant-A", "user-A", "session-A",
	)
	if len(conv.Filter.Types) != 2 {
		t.Fatalf("Types not preserved: got %v", conv.Filter.Types)
	}
	if !conv.Since.Equal(since) {
		t.Fatalf("Since not preserved: got %v, want %v", conv.Since, since)
	}
	if !conv.Until.Equal(until) {
		t.Fatalf("Until not preserved: got %v, want %v", conv.Until, until)
	}
}

// TestFilterFromWire_EmptyEventTypeStringSkipped — an empty string in
// the EventTypes slice is silently dropped (defensive against
// malformed JSON), not turned into an EventType("") that would never
// match anything.
func TestFilterFromWire_EmptyEventTypeStringSkipped(t *testing.T) {
	t.Parallel()
	conv := events.FilterFromWire(
		prototypes.EventFilter{EventTypes: []string{"", "runtime.error", ""}},
		"tenant-A", "user-A", "session-A",
	)
	if len(conv.Filter.Types) != 1 || conv.Filter.Types[0] != events.EventTypeRuntimeError {
		t.Fatalf("empty EventTypes strings not filtered: got %v", conv.Filter.Types)
	}
}
