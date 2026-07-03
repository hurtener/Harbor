// Wave v1.10 cross-seam leg (CLAUDE.md §17 / A-W3 directive): the D-281
// loading-mode override composed with the D-278 southbound OAuth binding.
//
// This lives in `package integration` (not the `integration_test` wave
// suite) so it can reuse the Phase 148 streamable-HTTP MCP + RFC-8693 broker
// fixtures verbatim (newP148Env / env.echo / env.rec / p148Ctx) — the §17.8
// "derive the fixture from the real transport" discipline, not a hand fixture.
package integration

import (
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestE2E_WaveV110_LoadingOverride_OAuthBoundDeferredTool_BearerOnDispatch
// pins the "disable removes capability; defer removes prompt presence only"
// split (D-281) read against the credential seam (D-278): an oauth-bound MCP
// tool flipped to `deferred` drops out of the prompt-time List() but stays
// Resolve()-callable, and the per-identity bearer still injects when the
// post-tool_search dispatch runs. A deferred tool is NOT a deauthenticated
// tool — the loading projection must not strip the credential.
func TestE2E_WaveV110_LoadingOverride_OAuthBoundDeferredTool_BearerOnDispatch(t *testing.T) {
	t.Parallel()
	env := newP148Env(t) // OAuth-bound streamable-HTTP MCP fixture ("graph_echo")

	// Register the oauth-bound descriptor on a real catalog.
	cat := tools.NewCatalog()
	if err := cat.Register(env.echo); err != nil {
		t.Fatalf("catalog register: %v", err)
	}

	// The planner's prompt-time filter surfaces only LoadingAlways tools; the
	// runtime override then defers graph_echo (prompt-hidden, still callable) —
	// the exact projection composition ActivePlannerCatalogView applies at
	// run start.
	filter := tools.CatalogFilter{
		TenantID: "tenant-1", UserID: "alice", SessionID: "s1",
		LoadingModes: []tools.LoadingMode{tools.LoadingAlways},
	}
	view := tools.NewLoadingOverrideView(
		tools.NewPlannerView(cat, filter),
		map[string]tools.LoadingMode{"graph_echo": tools.LoadingDeferred},
		filter.LoadingModes,
	)

	// Prompt-hidden after the deferred override ...
	for _, tl := range view.List() {
		if tl.Name == "graph_echo" {
			t.Fatal("graph_echo must be absent from List() under the deferred override")
		}
	}
	// ... but discovery-callable (the D-167 tool_search cycle's Resolve path).
	if tl, ok := view.Resolve("graph_echo"); !ok || tl.Loading != tools.LoadingDeferred {
		t.Fatalf("graph_echo must Resolve() as deferred after the override (ok=%v loading=%q)", ok, tl.Loading)
	}

	// Post-discovery dispatch: the executor resolves the descriptor against the
	// FULL catalog and invokes it. The D-278 per-identity bearer must still
	// inject even though the tool is prompt-deferred.
	id := identity.Identity{TenantID: "tenant-1", UserID: "alice", SessionID: "s1"}
	desc, ok := cat.Resolve("graph_echo")
	if !ok {
		t.Fatal("graph_echo missing from the full catalog")
	}
	if _, err := desc.Invoke(p148Ctx(t, id, "agent-loading"), json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatalf("dispatch of the deferred oauth-bound tool: %v", err)
	}
	calls := env.rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("MCP server saw %d tools/call, want 1 (the post-discovery dispatch)", len(calls))
	}
	if calls[0].authz != "Bearer brokered-tenant-1-alice" {
		t.Fatalf("Authorization = %q, want the per-identity bearer on a deferred tool's dispatch — the loading override must not strip the credential seam", calls[0].authz)
	}
}
