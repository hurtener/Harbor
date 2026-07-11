package events_test

import (
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// seqEvent builds a sequenced event for the ListWindow helper tests. Only
// the tenant varies across call sites (to exercise the identity predicate);
// user/session are fixed.
func seqEvent(seq uint64, typ events.EventType, tenant string) events.Event {
	return events.Event{
		Type:     typ,
		Sequence: seq,
		Identity: identity.Quadruple{Identity: identity.Identity{
			TenantID: tenant, UserID: "u", SessionID: "s",
		}},
		OccurredAt: time.Unix(int64(seq), 0).UTC(),
		Payload:    events.BusDroppedPayload{FromSeq: 1, ToSeq: 1},
	}
}

func listSeqs(evs []events.Event) []uint64 {
	out := make([]uint64, len(evs))
	for i, e := range evs {
		out[i] = e.Sequence
	}
	return out
}

func TestWireFilterHasFullTriple(t *testing.T) {
	t.Parallel()
	full := prototypes.EventFilter{TenantIDs: []string{"t"}, UserIDs: []string{"u"}, SessionIDs: []string{"s"}}
	if !events.WireFilterHasFullTriple(full) {
		t.Fatal("full triple reported as incomplete")
	}
	for _, missing := range []prototypes.EventFilter{
		{UserIDs: []string{"u"}, SessionIDs: []string{"s"}},
		{TenantIDs: []string{"t"}, SessionIDs: []string{"s"}},
		{TenantIDs: []string{"t"}, UserIDs: []string{"u"}},
		{},
	} {
		if events.WireFilterHasFullTriple(missing) {
			t.Fatalf("incomplete triple %+v reported as full", missing)
		}
	}
}

func TestWireFilterFirst(t *testing.T) {
	t.Parallel()
	if got := events.WireFilterFirst([]string{"a", "b"}); got != "a" {
		t.Fatalf("WireFilterFirst = %q, want a", got)
	}
	if got := events.WireFilterFirst(nil); got != "" {
		t.Fatalf("WireFilterFirst(nil) = %q, want empty", got)
	}
}

func TestWireFilterMatchesTriple(t *testing.T) {
	t.Parallel()
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	// Empty sets = "any".
	if !events.WireFilterMatchesTriple(prototypes.EventFilter{}, id) {
		t.Fatal("empty filter should match any identity")
	}
	// Matching set.
	if !events.WireFilterMatchesTriple(prototypes.EventFilter{TenantIDs: []string{"t1"}}, id) {
		t.Fatal("matching tenant should pass")
	}
	// Non-matching set on any axis.
	if events.WireFilterMatchesTriple(prototypes.EventFilter{TenantIDs: []string{"t2"}}, id) {
		t.Fatal("foreign tenant should not match")
	}
	if events.WireFilterMatchesTriple(prototypes.EventFilter{SessionIDs: []string{"s2"}}, id) {
		t.Fatal("foreign session should not match")
	}
}

func TestListWindowFromSnapshot_TailFirstOldestFirst(t *testing.T) {
	t.Parallel()
	snap := []events.Event{
		seqEvent(1, events.EventTypeRuntimeError, "t"),
		seqEvent(2, events.EventTypeRuntimeError, "t"),
		seqEvent(3, events.EventTypeRuntimeError, "t"),
		seqEvent(4, events.EventTypeRuntimeError, "t"),
		seqEvent(5, events.EventTypeRuntimeError, "t"),
	}
	wire := prototypes.EventFilter{TenantIDs: []string{"t"}, UserIDs: []string{"u"}, SessionIDs: []string{"s"}}
	page := events.ListWindowFromSnapshot(snap, 0, 2, wire)
	if got := listSeqs(page.Events); len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("tail page = %v, want [4 5] oldest-first", got)
	}
	if !page.HasMore || page.NextCursor != 4 {
		t.Fatalf("HasMore/NextCursor = %v/%d, want true/4", page.HasMore, page.NextCursor)
	}
}

func TestListWindowFromSnapshot_CursorAndHead(t *testing.T) {
	t.Parallel()
	snap := []events.Event{
		seqEvent(1, events.EventTypeRuntimeError, "t"),
		seqEvent(2, events.EventTypeRuntimeError, "t"),
		seqEvent(3, events.EventTypeRuntimeError, "t"),
	}
	wire := prototypes.EventFilter{TenantIDs: []string{"t"}, UserIDs: []string{"u"}, SessionIDs: []string{"s"}}
	// before=3 → rows below 3, most-recent-first collected then oldest-first.
	page := events.ListWindowFromSnapshot(snap, 3, 5, wire)
	if got := listSeqs(page.Events); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("before=3 page = %v, want [1 2]", got)
	}
	// Reached the head — no older events remain.
	if page.HasMore || page.NextCursor != 0 {
		t.Fatalf("HasMore/NextCursor = %v/%d, want false/0 at head", page.HasMore, page.NextCursor)
	}
}

func TestListWindowFromSnapshot_ExcludesBusInternalAndNonMatching(t *testing.T) {
	t.Parallel()
	snap := []events.Event{
		seqEvent(1, events.EventTypeRuntimeError, "t"),
		seqEvent(2, events.EventTypeAdminScopeUsed, "t"), // bus-internal notice
		seqEvent(3, events.EventTypeRuntimeError, "t2"),  // foreign tenant
		seqEvent(4, events.EventTypeRuntimeError, "t"),
	}
	wire := prototypes.EventFilter{TenantIDs: []string{"t"}, UserIDs: []string{"u"}, SessionIDs: []string{"s"}}
	page := events.ListWindowFromSnapshot(snap, 0, 50, wire)
	if got := listSeqs(page.Events); len(got) != 2 || got[0] != 1 || got[1] != 4 {
		t.Fatalf("filtered page = %v, want [1 4] (notice + foreign excluded)", got)
	}
}

func TestListWindowFromSnapshot_EmptyInputs(t *testing.T) {
	t.Parallel()
	wire := prototypes.EventFilter{}
	if p := events.ListWindowFromSnapshot(nil, 0, 10, wire); len(p.Events) != 0 || p.HasMore {
		t.Fatal("nil snapshot should yield an empty page")
	}
	snap := []events.Event{seqEvent(1, events.EventTypeRuntimeError, "t")}
	if p := events.ListWindowFromSnapshot(snap, 0, 0, wire); len(p.Events) != 0 {
		t.Fatal("zero limit should yield an empty page")
	}
}
