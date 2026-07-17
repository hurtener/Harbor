package tui_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/sdk/protocolclient"
	"github.com/hurtener/Harbor/sdk/tui"
)

// TestRun_RequiresBaseURL proves the facade fails loud when BaseURL is
// blank — no silent default, no anonymous loopback.
func TestRun_RequiresBaseURL(t *testing.T) {
	t.Parallel()
	err := tui.Run(context.Background(), tui.Options{
		Token: protocolclient.StaticToken("e30.e30.e30", protocolclient.IdentityScope{Tenant: "t", User: "u", Session: "s"}),
	})
	if err == nil {
		t.Fatal("Run succeeded with a blank BaseURL")
	}
}

// TestRun_RequiresTokenSource proves the facade fails loud when Token is
// nil — no anonymous loopback, no automatic token minting, no mock
// fallback (the AC2 non-goal).
func TestRun_RequiresTokenSource(t *testing.T) {
	t.Parallel()
	err := tui.Run(context.Background(), tui.Options{
		BaseURL: "http://127.0.0.1:9999",
	})
	if err == nil {
		t.Fatal("Run succeeded with a nil Token source")
	}
}

// TestRun_RequiresTokenSource_NonEmpty proves the facade rejects an empty
// token — the operator MUST supply a real JWT.
func TestRun_RequiresTokenSource_NonEmpty(t *testing.T) {
	t.Parallel()
	err := tui.Run(context.Background(), tui.Options{
		BaseURL: "http://127.0.0.1:9999",
		Token:   protocolclient.StaticToken("", protocolclient.IdentityScope{Tenant: "t", User: "u", Session: "s"}),
	})
	// The facade forwards to entry.Run, which calls Token(ctx, IdentityScope{})
	// to resolve the initial token. An empty token triggers a parse error
	// (not the facade-level nil check), proving the facade exercises the
	// TokenSource it received.
	if err == nil {
		t.Fatal("Run succeeded with an empty token value")
	}
}
