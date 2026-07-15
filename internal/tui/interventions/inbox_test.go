package interventions

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestInbox_PauseTokenCorrelationOrderingAndAntiResurrection(t *testing.T) {
	now := time.Now()
	inbox := New().Reconcile([]types.PauseSnapshot{{Token: "old", Reason: "approval_required", State: types.PauseStatePaused, Identity: types.IdentityScope{Run: "same-run"}, PausedAt: now}, {Token: "new", Reason: "await_input", State: types.PauseStatePaused, Identity: types.IdentityScope{Run: "same-run"}, PausedAt: now.Add(time.Second)}}, 2)
	items := inbox.Items()
	if len(items) != 2 || items[0].Token != "new" || items[1].Token != "old" || items[0].Kind != KindInput {
		t.Fatalf("items=%#v", items)
	}
	inbox = inbox.Resolve("new", "resolved elsewhere", 3)
	inbox = inbox.Reconcile([]types.PauseSnapshot{{Token: "new", State: types.PauseStatePaused, PausedAt: now}}, 2)
	if got := inbox.Items(); len(got) != 0 {
		t.Fatalf("stale snapshot resurrected token: %#v", got)
	}
}

func TestInbox_MultipleSameRunKindsExpiryAndResolvedTombstones(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	snapshots := []types.PauseSnapshot{
		{Token: "approve", Reason: "approval_required", State: types.PauseStatePaused, Identity: types.IdentityScope{Run: "same"}, PausedAt: now.Add(-time.Minute)},
		{Token: "oauth", Reason: "oauth_required", State: types.PauseStatePaused, Identity: types.IdentityScope{Run: "same"}, PausedAt: now.Add(-2 * time.Minute)},
		{Token: "input", Reason: "input_required", State: types.PauseStatePaused, Identity: types.IdentityScope{Run: "same"}, PausedAt: now.Add(-3 * time.Minute), Payload: map[string]any{"expires_at": now.Add(-time.Second).Format(time.RFC3339)}},
	}
	inbox := New().ReconcilePage(types.PauseListResponse{Snapshots: snapshots, Page: 1, PageCount: 1, TotalRows: 3}, 4, now)
	if got := len(inbox.Items()); got != 2 {
		t.Fatalf("active=%d want=2 history=%#v", got, inbox.History())
	}
	if got := len(inbox.History()); got != 3 {
		t.Fatalf("history=%d want=3", got)
	}
	inbox = inbox.Select("oauth").Resolve("approve", "approved here", 5)
	selected, ok := inbox.Selected()
	if !ok || selected.Token != "oauth" {
		t.Fatalf("selection=%#v ok=%t", selected, ok)
	}
	inbox = inbox.ReconcilePage(types.PauseListResponse{Snapshots: []types.PauseSnapshot{snapshots[1]}, Page: 1, PageCount: 1, TotalRows: 1}, 5, now.Add(time.Second))
	if len(inbox.Items()) != 1 || len(inbox.History()) != 3 {
		t.Fatalf("active/history=%#v/%#v", inbox.Items(), inbox.History())
	}
	for _, item := range inbox.History() {
		if item.Token == "approve" && item.Resolution == "" {
			t.Fatal("approval tombstone lost")
		}
		if item.Token == "input" && item.Status != "expired" {
			t.Fatalf("expiry=%#v", item)
		}
	}
	stale := inbox.Reconcile(snapshots, 4)
	if len(stale.Items()) != 1 {
		t.Fatalf("stale snapshot resurrected tombstones: %#v", stale.Items())
	}
}

func TestInbox_ConcurrentImmutableUpdates(t *testing.T) {
	base := New()
	var wait sync.WaitGroup
	for n := range 128 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			token := fmt.Sprintf("token-%03d", n)
			got := base.Reconcile([]types.PauseSnapshot{{Token: token, State: types.PauseStatePaused}}, uint64(n+1))
			if len(got.Items()) != 1 || got.Items()[0].Token != token {
				t.Errorf("cross-talk for %s", token)
			}
		}()
	}
	wait.Wait()
	if len(base.Items()) != 0 {
		t.Fatal("immutable base mutated")
	}
}
