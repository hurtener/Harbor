package sqlite_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// This file pins the child-collection REPLACEMENT contract: every
// accepted write replaces the row's two dynamic bounded collections
// (activity rows + MCP App refs) WHOLESALE under one version bump, in
// the same transaction as the row. A stale child can never survive a
// row rewrite — an update that drops activity/apps entries must be
// observable as dropped on the next read, and the ORDER of the
// retained collection is preserved exactly.

func TestChildren_WriteReplacesWholesale(t *testing.T) {
	s := openMem(t)
	defer func() { _ = s.Close(context.Background()) }()
	ctx := context.Background()
	id := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}

	row := richRow("run-1", id, 1)
	if _, err := s.AppendTurnIf(ctx, id, row); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Update: shrink the activity window from 1 row to 2 (add one),
	// shrink apps from 2 to 1, and reverse... the update carries the
	// COMPLETE new component, so the stored children must exactly match
	// the fed list.
	cur, err := s.GetTurn(ctx, id, "run-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	next := cur
	next.Activity = turns.Activity{
		Rows: []turns.ActivityRow{
			{Position: 0, Tool: "a", Status: turns.ActivityInvoked, TerminalClass: turns.ActivityTerminalNone},
			{Position: 1, Tool: "b", Status: turns.ActivityRetried, TerminalClass: turns.ActivityTerminalNone},
		},
		Complete: turns.CompletenessComplete,
		Totals:   turns.ActivityTotals{Invoked: 1, Retried: 1},
	}
	next.Apps = []turns.AppRef{
		{EffectiveAgentID: "a9", ServerID: "s9", ResourceURI: "ui://s9/app", ToolName: "tool-9", Availability: turns.AppAvailable, Complete: turns.CompletenessComplete},
	}
	got, err := s.UpdateTurnIf(ctx, id, cur.TurnID, cur.Version, next)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Version != cur.Version+1 {
		t.Fatalf("version=%d, want %d", got.Version, cur.Version+1)
	}

	read, err := s.GetTurn(ctx, id, "run-1")
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if !reflect.DeepEqual(read.Activity, next.Activity) {
		t.Errorf("activity children not replaced wholesale:\n got: %+v\nwant: %+v", read.Activity, next.Activity)
	}
	if !reflect.DeepEqual(read.Apps, next.Apps) {
		t.Errorf("app children not replaced wholesale:\n got: %+v\nwant: %+v", read.Apps, next.Apps)
	}
	if len(read.Apps) != 1 || read.Apps[0].ServerID != "s9" {
		t.Errorf("app collection wrong after replace: %+v", read.Apps)
	}
}
