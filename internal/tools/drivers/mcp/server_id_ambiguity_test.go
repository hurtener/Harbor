package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

// server_id_ambiguity_test.go — the registration-time precondition the
// MCP-Apps app→host tool-namespace confinement depends on.
//
// The Console confines a rendered `ui://` App to its own server by prefixing
// every app-supplied tool name with the App's HOST-DERIVED server id. The
// prefix is unconditional and the App cannot choose the id, so the control is
// airtight against name choice — but it silently is NOT airtight against ID
// choice, because `<sourceID>_<tool>` is a single-underscore join and neither
// side is charset-constrained. Two operator-chosen ids that differ by a `_`
// segment make the join non-injective, and an App on the shorter id can then
// address the longer id's tools.
//
// TestCatalogKeyJoin_IsNotInjective_WithoutTheRegistrationGuard demonstrates
// the escape against the REAL catalog, so the guard's value is a demonstrated
// property rather than an assertion about a string. The Registry tests then
// prove the guard makes that pairing unreachable in the first place.

// The escape, shown end-to-end against the real tools.Catalog: nothing in the
// catalog can distinguish which server produced a key, so an App confined to
// `github` reaches `github_enterprise`'s tool by name alone. This test does not
// need the guard disabled — it shows the JOIN is ambiguous, which is exactly
// why the guard has to prevent the pairing from existing.
func TestCatalogKeyJoin_IsNotInjective_WithoutTheRegistrationGuard(t *testing.T) {
	cat := tools.NewCatalog()

	// `github_enterprise` registers its own tool exactly as the driver does:
	// fmt.Sprintf("%s_%s", source, toolName) — see Provider.Discover.
	victimKey := fmt.Sprintf("%s_%s", "github_enterprise", "delete_repo")
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:      victimKey,
			Source:    tools.ToolSourceID("github_enterprise"),
			Transport: tools.TransportMCP,
		},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatalf("register victim tool: %v", err)
	}

	// An App hosted by server `github` asks for the bare name
	// `enterprise_delete_repo`. The Console's confinement qualifier prefixes it
	// with the App's own server id — unconditionally, and the App cannot
	// influence that id.
	appServerID := "github"
	appSuppliedName := "enterprise_delete_repo"
	dispatched := fmt.Sprintf("%s_%s", appServerID, appSuppliedName)

	// The dispatched key is inside the App's own `github_` namespace by every
	// string test the host can perform...
	if got := dispatched; got != "github_enterprise_delete_repo" {
		t.Fatalf("qualified name = %q, want github_enterprise_delete_repo", got)
	}

	// ...and yet it resolves to a tool owned by a DIFFERENT server. This is the
	// confinement escape: the downstream exposure gate would evaluate
	// `github_enterprise`'s posture, so every gate on the path approves.
	desc, ok := cat.Resolve(dispatched)
	if !ok {
		t.Fatalf("Resolve(%q) missed — the join is injective after all?", dispatched)
	}
	if desc.Tool.Source != tools.ToolSourceID("github_enterprise") {
		t.Fatalf("resolved Source = %q, want github_enterprise", desc.Tool.Source)
	}
	t.Logf("confirmed: an App on %q reaches %q's tool via key %q — hence the registration guard",
		appServerID, desc.Tool.Source, dispatched)
}

// The guard: an ambiguous pair can never both be registered, so the escape
// above has no reachable precondition. Both ORDERS are covered — whichever
// lands second is refused, so boot order cannot decide which runtime state we
// end up in.
func TestRegistry_Register_RefusesSeparatorAmbiguousServerID(t *testing.T) {
	t.Run("longer id after shorter", func(t *testing.T) {
		r := NewRegistry()
		if err := r.Register(idCtx(t), ServerRegistration{
			Provider: &stubProvider{id: "github"}, Transport: "stdio",
		}); err != nil {
			t.Fatalf("register github: %v", err)
		}
		err := r.Register(idCtx(t), ServerRegistration{
			Provider: &stubProvider{id: "github_enterprise"}, Transport: "stdio",
		})
		if !errors.Is(err, ErrAmbiguousServerID) {
			t.Fatalf("register github_enterprise = %v, want ErrAmbiguousServerID", err)
		}
		if ids := r.SourceIDs(); len(ids) != 1 || ids[0] != "github" {
			t.Fatalf("SourceIDs = %v, want [github] only — the refused id must not land", ids)
		}
	})

	t.Run("shorter id after longer", func(t *testing.T) {
		r := NewRegistry()
		if err := r.Register(idCtx(t), ServerRegistration{
			Provider: &stubProvider{id: "github_enterprise"}, Transport: "stdio",
		}); err != nil {
			t.Fatalf("register github_enterprise: %v", err)
		}
		err := r.Register(idCtx(t), ServerRegistration{
			Provider: &stubProvider{id: "github"}, Transport: "stdio",
		})
		if !errors.Is(err, ErrAmbiguousServerID) {
			t.Fatalf("register github = %v, want ErrAmbiguousServerID", err)
		}
		if ids := r.SourceIDs(); len(ids) != 1 || ids[0] != "github_enterprise" {
			t.Fatalf("SourceIDs = %v, want [github_enterprise] only", ids)
		}
	})
}

// Ids that merely SHARE A PREFIX without a separator boundary are unambiguous
// — `githubby_x` can never be produced by `github` (that would need
// `github_by_x`). Refusing them would be an over-broad gate that blocks
// legitimate operator naming, so pin the boundary explicitly.
func TestRegistry_Register_AllowsPrefixWithoutSeparatorBoundary(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"github", "githubby", "git"} {
		if err := r.Register(idCtx(t), ServerRegistration{
			Provider: &stubProvider{id: tools.ToolSourceID(id)}, Transport: "stdio",
		}); err != nil {
			t.Fatalf("register %q: %v", id, err)
		}
	}
	if ids := r.SourceIDs(); len(ids) != 3 {
		t.Fatalf("SourceIDs = %v, want all three registered", ids)
	}
}

// Re-registering the SAME id is the hot-reload / runtime re-attach path and
// must stay allowed: replacing an entry cannot introduce an ambiguity the id
// did not already have.
func TestRegistry_Register_SameIDReplacementStillAllowed(t *testing.T) {
	r := NewRegistry()
	first := &stubProvider{id: "github"}
	if err := r.Register(idCtx(t), ServerRegistration{Provider: first, Transport: "stdio"}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := r.Register(idCtx(t), ServerRegistration{
		Provider: &stubProvider{id: "github"}, Transport: "stdio",
	}); err != nil {
		t.Fatalf("re-register same id: %v, want allowed", err)
	}
	first.mu.Lock()
	closed := first.closed
	first.mu.Unlock()
	if closed != 1 {
		t.Fatalf("displaced provider Close called %d times, want 1", closed)
	}
}

// CheckServerIDUnambiguous is the early, side-effect-free copy Attach calls
// before spawning a transport or writing catalog rows. It must agree with the
// gate Register enforces.
func TestRegistry_CheckServerIDUnambiguous_AgreesWithRegister(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(idCtx(t), ServerRegistration{
		Provider: &stubProvider{id: "github"}, Transport: "stdio",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.CheckServerIDUnambiguous("github_enterprise"); !errors.Is(err, ErrAmbiguousServerID) {
		t.Fatalf("Check(github_enterprise) = %v, want ErrAmbiguousServerID", err)
	}
	if err := r.CheckServerIDUnambiguous("slack"); err != nil {
		t.Fatalf("Check(slack) = %v, want nil", err)
	}
	if err := r.CheckServerIDUnambiguous("github"); err != nil {
		t.Fatalf("Check(github) = %v, want nil (same-id replacement)", err)
	}
}
