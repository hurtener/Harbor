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
// The Console scopes a rendered `ui://` App to its own server by qualifying
// every app-supplied tool name with the App's HOST-DERIVED server id. The
// qualification is unconditional and the App cannot choose the id, so the
// boundary is as strong as the key space is unambiguous — and no stronger,
// because `<sourceID>_<tool>` is a single-underscore join and neither side is
// charset-constrained. Two server ids that are underscore-extensions of one
// another make the join non-injective; the contract is that ids must be
// separator-safe, which the registration guard enforces.
//
// TestCatalogKeyJoin_IsNotInjective_DocumentsWhyTheGuardExists demonstrates
// the escape against the REAL catalog, so the guard's value is a demonstrated
// property rather than an assertion about a string. The Registry tests then
// prove the guard makes that pairing unreachable in the first place.

// The property, shown against the real tools.Catalog: nothing in the catalog
// can distinguish which server produced a key, so a key built by prefixing one
// id can resolve to a tool owned by an underscore-extended id. That is why the
// guard has to prevent such a pairing from existing at all.
func TestCatalogKeyJoin_IsNotInjective_DocumentsWhyTheGuardExists(t *testing.T) {
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

	// A host qualifying a bare name with its own (host-derived) server id.
	appServerID := "github"
	appSuppliedName := "enterprise_delete_repo"
	dispatched := fmt.Sprintf("%s_%s", appServerID, appSuppliedName)

	// The qualified key is inside the qualifying id's namespace by every string
	// test the host can perform...
	if got := dispatched; got != "github_enterprise_delete_repo" {
		t.Fatalf("qualified name = %q, want github_enterprise_delete_repo", got)
	}

	// ...and yet it resolves to a tool owned by a DIFFERENT source. Downstream
	// gates evaluate the posture of whichever server the key resolved to, so
	// they cannot detect the mismatch either.
	desc, ok := cat.Resolve(dispatched)
	if !ok {
		t.Fatalf("Resolve(%q) missed — the join is injective after all?", dispatched)
	}
	if desc.Tool.Source != tools.ToolSourceID("github_enterprise") {
		t.Fatalf("resolved Source = %q, want github_enterprise", desc.Tool.Source)
	}
	t.Logf("confirmed non-injective: a key qualified with %q resolved to a tool owned by a "+
		"different source — hence the registration guard", appServerID)
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
