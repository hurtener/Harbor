package mcpconsole

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// identityCtx builds a ctx carrying a complete triple. This file lives in the
// INTERNAL test package (it calls the unexported markNotFound), so it cannot
// reuse the external suite's helper of the same purpose.
func identityCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: "t-1", UserID: "u-1", SessionID: "s-1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// notfound_translation_test.go — every accessor method that can surface the
// DRIVER's not-found must translate it into the PROTOCOL's sentinel.
//
// The Protocol edge classifies not-found by errors.Is against
// protocol.ErrAccessorNotFound; it deliberately no longer substring-matches the
// error chain, because a southbound MCP server's text rides that chain verbatim
// and a remote party must not get to decide a Harbor classification.
//
// The cost of that correctness is that a MISSED translation is silent: the
// method keeps working, it just answers CodeRuntimeError (HTTP 500) where it
// used to answer CodeNotFound (404). One method was in fact missed on the first
// pass (Probe), and the only thing that caught it was a preflight smoke whose
// SKIP-on-404 flipped to a FAIL-on-500. This test makes the requirement
// explicit and enumerable instead of relying on that coincidence.
//
// A NEW accessor method that can return a driver not-found belongs in this
// table. If it is absent from the table the test cannot fail — so the table is
// the checklist, and the godoc on markNotFound points here.
func TestRegistryAccessor_EveryNotFoundPathTranslatesToTheProtocolSentinel(t *testing.T) {
	// An unknown name against an EMPTY registry: every read below resolves the
	// server first, so each one takes its not-found branch.
	acc, err := NewRegistryAccessor(mcp.NewRegistry())
	if err != nil {
		t.Fatalf("NewRegistryAccessor: %v", err)
	}
	// Identity is mandatory on every registry read; without it the methods fail
	// closed before they ever resolve a server name.
	ctx := identityCtx(t)
	const unknown = "no-such-server"

	cases := []struct {
		name string
		call func() error
	}{
		{"GetServer", func() error { _, err := acc.GetServer(ctx, unknown); return err }},
		{"ListResources", func() error { _, err := acc.ListResources(ctx, unknown); return err }},
		{"ListPrompts", func() error { _, err := acc.ListPrompts(ctx, unknown); return err }},
		{"RefreshDiscovery", func() error { _, err := acc.RefreshDiscovery(ctx, unknown); return err }},
		{"Probe", func() error { _, err := acc.Probe(ctx, unknown); return err }},
		{"Health", func() error { _, err := acc.Health(ctx, unknown); return err }},
		{"SetRawHTMLTrust", func() error { _, err := acc.SetRawHTMLTrust(ctx, unknown, true); return err }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil {
				t.Fatalf("%s on an unknown server returned nil error", c.name)
			}
			if !errors.Is(err, mcp.ErrServerNotFound) {
				t.Fatalf("%s: err = %v, want it to wrap mcp.ErrServerNotFound "+
					"(the test's premise — this method may not resolve a server at all)", c.name, err)
			}
			if !errors.Is(err, protocol.ErrAccessorNotFound) {
				t.Fatalf("%s: a driver not-found reached the Protocol edge WITHOUT "+
					"protocol.ErrAccessorNotFound, so it will be classified CodeRuntimeError (HTTP 500) "+
					"instead of CodeNotFound (404). Wrap it with markNotFound. err = %v", c.name, err)
			}
		})
	}
}

// SetRawHTMLTrust must not turn a SUCCESSFUL call into an error: markNotFound
// is applied unconditionally on that return path, so its nil-passthrough is
// load-bearing rather than incidental.
func TestRegistryAccessor_MarkNotFound_PassesSuccessThrough(t *testing.T) {
	if err := markNotFound(nil); err != nil {
		t.Fatalf("markNotFound(nil) = %v, want nil", err)
	}
	// A non-not-found error is returned unchanged (not re-wrapped as one).
	other := errors.New("mcpconsole: transport reset")
	if got := markNotFound(other); !errors.Is(got, other) || errors.Is(got, protocol.ErrAccessorNotFound) {
		t.Fatalf("markNotFound(other) = %v, want the original error, unwrapped as not-found", got)
	}
}
