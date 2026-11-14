package mcpconsole_test

import (
	"context"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

// TestSessionAppLedgerIsExactAndSessionScoped pins the D-510 ledger: an App
// recorded in one session answers only for that exact (agent, server,
// resource) in that exact (tenant, user, session) partition.
func TestSessionAppLedgerIsExactAndSessionScoped(t *testing.T) {
	tc := newToolCtxStore(t, 1024)
	ctx := idCtxFor(t, "t1", "u1", "s1")
	if err := tc.RecordSessionApp(ctx, "agent", "srv", "ui://app"); err != nil {
		t.Fatal(err)
	}
	mine := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	for _, c := range []struct {
		name               string
		id                 identity.Identity
		agent, server, uri string
		want               bool
	}{
		{"exact", mine, "agent", "srv", "ui://app", true},
		{"other agent", mine, "agent-2", "srv", "ui://app", false},
		{"other server", mine, "agent", "srv-2", "ui://app", false},
		{"other resource", mine, "agent", "srv", "ui://other", false},
		{"other session", identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s2"}, "agent", "srv", "ui://app", false},
		{"other user", identity.Identity{TenantID: "t1", UserID: "u2", SessionID: "s1"}, "agent", "srv", "ui://app", false},
		{"other tenant", identity.Identity{TenantID: "t2", UserID: "u1", SessionID: "s1"}, "agent", "srv", "ui://app", false},
		{"empty agent", mine, "", "srv", "ui://app", false},
	} {
		got, err := tc.SessionHasApp(context.Background(), c.id, c.agent, c.server, c.uri)
		if err != nil || got != c.want {
			t.Fatalf("%s: SessionHasApp = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if err := tc.RecordSessionApp(context.Background(), "agent", "srv", "ui://app"); err == nil {
		t.Fatal("recorded an App without an identity")
	}
}

// TestSessionAppLedgerConcurrentReuse exercises one shared ledger across many
// sessions at once (CLAUDE.md §5 concurrent reuse).
func TestSessionAppLedgerConcurrentReuse(t *testing.T) {
	tc := newToolCtxStore(t, 1024)
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session := "s" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			ctx, err := identity.With(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: session})
			if err != nil {
				errs <- err
				return
			}
			if err := tc.RecordSessionApp(ctx, "agent", "srv", "ui://"+session); err != nil {
				errs <- err
				return
			}
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: session}
			if has, err := tc.SessionHasApp(context.Background(), id, "agent", "srv", "ui://"+session); err != nil || !has {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent ledger: %v", err)
	}
}
