package mcpconsole_test

import (
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/tools"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// notfound_enumeration_test.go — EVERY accessor path that can surface a
// not-found must translate it into protocol.ErrAccessorNotFound.
//
// # Why this file exists rather than a comment
//
// The Protocol edge classifies not-found by errors.Is against the sentinel; it
// deliberately no longer substring-matches the error chain, because a
// southbound MCP server's text rides that chain verbatim and a remote party
// must not get to decide a Harbor classification.
//
// The cost of that correctness is that a MISSED translation is SILENT. The
// method keeps working and merely answers CodeRuntimeError (HTTP 500) where it
// answered CodeNotFound (404). Nothing panics, no test that asserts "an error
// happened" changes colour, and — the trap this file closes — a test that
// asserts the error TEXT keeps passing, because the text survives the missing
// wrap perfectly well. That shape is the §17.8 rubber-stamp one level up: the
// fixture encodes a belief about what the accessor emits rather than what the
// consumer reads.
//
// Two translations were in fact missed across this work: `Probe` (caught only
// by a preflight smoke whose SKIP-on-404 flipped to a FAIL-on-500) and the
// three Apps seams below (caught only by review). Both times the code was
// fine-looking and every Go test was green.
//
// **A new accessor method that can return a not-found belongs in one of these
// tables.** Absent from the table, it cannot fail — so the table IS the
// checklist, and `markNotFound`'s godoc points here.
//
// # The one site deliberately NOT in a table
//
// `RegistryAccessor.ListServers` also calls `markNotFound`, but no assertion
// covers it and none can: the registry's `ListServers` filters a snapshot and
// returns an empty page for an unmatched filter — it has no not-found branch to
// reach. The wrap there is defence-in-depth against that changing, not a live
// path. Recorded explicitly so the gap is a stated fact rather than something a
// future reader has to rediscover by finding the un-mutatable line.

// The Apps seams. These are the paths a rendered `ui://` MCP App depends on, so
// a missed translation here has a user-visible consequence beyond the status
// code: the Console branches on `code === 'not_found'` to decide between an
// honest placeholder and a loud error.
func TestAppsAccessor_EveryNotFoundPathTranslatesToTheProtocolSentinel(t *testing.T) {
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		// A registry holding NO server, a catalog holding NO tool, and a
		// tool-context store holding NO record: every seam below takes its
		// not-found branch against real drivers.
		Registry:    mcp.NewRegistry(),
		Catalog:     tools.NewCatalog(),
		Store:       newAppsStore(t),
		Bus:         newAppsBus(t),
		ToolContext: newAppsToolCtx(t),
		Threshold:   1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	ctx := idCtx(t)

	cases := []struct {
		name string
		why  string
		call func() error
	}{
		{
			name: "ReadResource",
			why:  "mcp.servers.read_resource fetches the app's ui:// document; a 500 here is an unrecoverable render error where a 404 is an honest miss",
			call: func() error { _, err := acc.ReadResource(ctx, "no-such-server", "ui://x/y.html"); return err },
		},
		{
			name: "CallTool",
			why:  "an app-initiated tools/call that does not resolve inside the app's own server namespace IS the confinement rejection — the Console maps not_found onto the typed MCPAppToolNotFoundError an App branches on",
			call: func() error { _, err := acc.CallTool(ctx, "no-such-server", "no-such-server_nope", nil); return err },
		},
		{
			name: "ToolContext",
			why:  "mcp.apps.tool_context backs the replayed-app MISS path; without the sentinel the renderer takes its loud-error branch instead of the honest 'no longer available' placeholder",
			call: func() error { _, err := acc.ToolContext(ctx, "no-such-server", "no-such-call"); return err },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil {
				t.Fatalf("%s against an empty backend returned nil error", c.name)
			}
			if !errors.Is(err, protocol.ErrAccessorNotFound) {
				t.Fatalf("%s: a not-found reached the Protocol edge WITHOUT "+
					"protocol.ErrAccessorNotFound, so it is classified CodeRuntimeError (HTTP 500) "+
					"instead of CodeNotFound (404). Wrap it with markNotFound.\n  why it matters: %s\n  err = %v",
					c.name, c.why, err)
			}
		})
	}
}

// A non-not-found failure on the same seams must NOT be re-labelled. The
// classification has to be a two-way property or the sentinel is just a
// blanket, and an App would read a transient transport failure as a permanent
// "this action does not exist here".
func TestAppsAccessor_NonNotFoundFailuresAreNotRelabelled(t *testing.T) {
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    mcp.NewRegistry(),
		Catalog:     tools.NewCatalog(),
		Store:       newAppsStore(t),
		Bus:         newAppsBus(t),
		ToolContext: newAppsToolCtx(t),
		Threshold:   1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	// A missing identity is a fail-closed error, not a not-found: it must not
	// acquire the sentinel on its way out (that would tell an App "no such
	// tool" when the real answer is "you are not scoped").
	if _, err := acc.ReadResource(t.Context(), "srv", "ui://x"); err == nil {
		t.Fatal("ReadResource without identity: want error")
	} else if errors.Is(err, protocol.ErrAccessorNotFound) {
		t.Fatalf("a missing-identity failure was labelled not-found: %v", err)
	}
}
