package protocol

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
)

// A closed session's row reports its last real activity (LastSeen), not the
// time it was closed: an idle-sweep close is not activity.
func TestProjectRow_ClosedSession_ReportsLastSeenNotClosedAt(t *testing.T) {
	opened := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	lastSeen := opened.Add(30 * time.Minute)
	closedAt := lastSeen.Add(24 * time.Hour)
	snap := sessions.SessionSnapshot{Session: sessions.Session{
		ID:           "s1",
		Identity:     identity.Identity{TenantID: "t", UserID: "u", SessionID: "s1"},
		OpenedAt:     opened,
		LastSeen:     lastSeen,
		Closed:       true,
		ClosedAt:     closedAt,
		ClosedReason: "gc:idle",
	}}
	row := projectRow(snap)
	if !row.LastActivityAt.Equal(lastSeen) {
		t.Fatalf("LastActivityAt = %v, want LastSeen %v (not ClosedAt %v)", row.LastActivityAt, lastSeen, closedAt)
	}
	if row.Duration != 30*time.Minute {
		t.Fatalf("Duration = %v, want 30m (open → last activity)", row.Duration)
	}

	// A legacy record without LastSeen falls back to ClosedAt, then OpenedAt.
	snap.LastSeen = time.Time{}
	if got := projectRow(snap).LastActivityAt; !got.Equal(closedAt) {
		t.Fatalf("zero-LastSeen closed row LastActivityAt = %v, want ClosedAt %v", got, closedAt)
	}
	snap.Closed, snap.ClosedAt = false, time.Time{}
	if got := projectRow(snap).LastActivityAt; !got.Equal(opened) {
		t.Fatalf("zero-LastSeen open row LastActivityAt = %v, want OpenedAt %v", got, opened)
	}
}

// last_activity_desc orders by real use across open and closed sessions,
// and the cursor pages over the same value without gaps or repeats.
func TestService_List_LastActivityDesc_ClosedRowsByLastSeen(t *testing.T) {
	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	mk := func(id string, opened, lastSeen time.Duration, closedAt time.Duration) sessions.SessionSnapshot {
		s := sessions.Session{
			ID:       id,
			Identity: identity.Identity{TenantID: "tenant-1", UserID: "user-1", SessionID: id},
			OpenedAt: base.Add(opened),
			LastSeen: base.Add(lastSeen),
		}
		if closedAt > 0 {
			s.Closed, s.ClosedAt, s.ClosedReason = true, base.Add(closedAt), "gc:idle"
		}
		return sessions.SessionSnapshot{Session: s}
	}
	snapshots := []sessions.SessionSnapshot{
		// Old chat the sweep closed just now: must sort by its last use.
		mk("old-swept", 0, time.Hour, 72*time.Hour),
		// Chat used minutes ago.
		mk("recent", 2*time.Hour, 71*time.Hour+50*time.Minute, 0),
		// Chat used yesterday.
		mk("yesterday", time.Hour, 48*time.Hour, 0),
		// Closed by the operator after real use.
		mk("closed-used", 3*time.Hour, 60*time.Hour, 61*time.Hour),
	}
	want := []string{"recent", "closed-used", "yesterday", "old-swept"}

	projector, err := NewListerProjector(catalogLister{snapshots: snapshots})
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	svc, err := NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	caller := lifecycleCaller()
	var got []string
	cursor := ""
	for page := 0; page < 10; page++ {
		resp, err := svc.List(context.Background(), prototypes.SessionsListRequest{
			Identity: prototypes.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID},
			Sort:     prototypes.SessionSortLastActivityDesc,
			Limit:    1,
			Cursor:   cursor,
		}, false)
		if err != nil {
			t.Fatalf("List page %d: %v", page, err)
		}
		for _, r := range resp.Rows {
			got = append(got, r.SessionID)
		}
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	if len(got) != len(want) {
		t.Fatalf("paged order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paged order = %v, want %v", got, want)
		}
	}
}
