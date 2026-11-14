package turns

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

func TestAppCheckerFindsOnlyTheSessionsOwnApp(t *testing.T) {
	ctx := context.Background()
	store, err := newTestStore(1000, false)
	if err != nil {
		t.Fatal(err)
	}
	mine := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s1"}
	other := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s2"}
	// More rows than one page so the walk crosses a cursor.
	for i := range MaxListLimit + 5 {
		row := TurnRow{TurnID: TurnID(string(rune('a'+i%26)) + string(rune('a'+i/26)))}
		if i == 0 {
			row.Apps = []AppRef{{EffectiveAgentID: "agent", ServerID: "srv", ResourceURI: "ui://app"}}
		}
		if _, err := store.AppendTurnIf(ctx, mine, row); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	c := AppChecker{Store: store}
	for _, tc := range []struct {
		name               string
		id                 identity.Identity
		agent, server, uri string
		want               bool
	}{
		{"exact app in own session", mine, "agent", "srv", "ui://app", true},
		{"other agent", mine, "agent-2", "srv", "ui://app", false},
		{"other resource", mine, "agent", "srv", "ui://other", false},
		{"other session", other, "agent", "srv", "ui://app", false},
		{"empty agent", mine, "", "srv", "ui://app", false},
	} {
		got, err := c.SessionHasApp(ctx, tc.id, tc.agent, tc.server, tc.uri)
		if err != nil || got != tc.want {
			t.Fatalf("%s: SessionHasApp = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	if _, err := (AppChecker{}).SessionHasApp(ctx, mine, "agent", "srv", "ui://app"); err == nil {
		t.Fatal("checker without a store answered")
	}
}
