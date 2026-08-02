package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// mcp_detacher_owner_test.go — the production detacher's removal is scoped by
// the reconciling owner at the REGISTRY's own choke point, so a name that
// reaches Detach under the wrong owner cannot tear down another owner's live
// connection (or a boot-declared one).

// detacherStubProvider is a deterministic MCP provider for the detacher tests —
// the real driver needs a live MCP wire.
type detacherStubProvider struct {
	id        tools.ToolSourceID
	closed    int
	closeErrs []error
}

func (p *detacherStubProvider) SourceID() tools.ToolSourceID { return p.id }
func (p *detacherStubProvider) Close(context.Context) error {
	p.closed++
	if len(p.closeErrs) == 0 {
		return nil
	}
	err := p.closeErrs[0]
	p.closeErrs = p.closeErrs[1:]
	return err
}
func (p *detacherStubProvider) DisplayModes() []string { return nil }
func (p *detacherStubProvider) ReadResource(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New("no resources")
}
func (p *detacherStubProvider) Discover(context.Context) ([]tools.ToolDescriptor, error) {
	return []tools.ToolDescriptor{{Tool: tools.Tool{Name: string(p.id) + "_echo"}}}, nil
}

func detacherIDCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: "t-1", UserID: "u-1", SessionID: "s-1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// TestMCPConnectionDetacher_Detach_OwnerScoped drives the PRODUCTION detacher
// against a REAL registry: a detach carrying another owner's tag leaves the
// registration and its transport alone, while the owning tag removes it.
func TestMCPConnectionDetacher_Detach_OwnerScoped(t *testing.T) {
	ownerA := toolauth.Owner{Tenant: "tenant-a", Agent: "agent-a"}
	ownerB := toolauth.Owner{Tenant: "tenant-b", Agent: "agent-b"}

	registry := mcpdrv.NewRegistry()
	prov := &detacherStubProvider{id: "owned-srv"}
	if err := registry.Register(detacherIDCtx(t), mcpdrv.ServerRegistration{
		Provider: prov, Transport: "stdio", InitialState: mcpdrv.ServerStateOnline, Owner: ownerA,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := NewMCPConnectionDetacher(tools.NewCatalog(), registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A cross-owner detach is swallowed as already-detached (the registry answers
	// ErrServerNotFound, which the idempotent leg treats as a no-op) — but the
	// registration and its transport survive.
	if err := d.Detach(context.Background(), "owned-srv", ownerB); err != nil {
		t.Fatalf("cross-owner Detach: %v", err)
	}
	if got := registry.SourceIDs(); len(got) != 1 || got[0] != "owned-srv" {
		t.Fatalf("SourceIDs after a cross-owner detach = %v, want [owned-srv] retained", got)
	}
	if prov.closed != 0 {
		t.Fatalf("cross-owner detach closed the transport %d times, want 0", prov.closed)
	}

	if err := d.Detach(context.Background(), "owned-srv", ownerA); err != nil {
		t.Fatalf("owning Detach: %v", err)
	}
	if got := registry.SourceIDs(); len(got) != 0 {
		t.Fatalf("SourceIDs after the owning detach = %v, want empty", got)
	}
	if prov.closed != 1 {
		t.Fatalf("provider Close called %d times, want 1", prov.closed)
	}
}

func TestDetachSourceExpected_CloseFailureIsRetryableAndNeverAbsentSuccess(t *testing.T) {
	owner := toolauth.Owner{Tenant: "tenant-a", Agent: "agent-a"}
	closeFault := errors.New("injected exact close fault")
	registry := mcpdrv.NewRegistry()
	provider := &detacherStubProvider{id: "exact-retry", closeErrs: []error{closeFault, nil}}
	if err := registry.Register(context.Background(), mcpdrv.ServerRegistration{
		Provider: provider, Transport: "stdio", InitialState: mcpdrv.ServerStateOnline,
		Owner: owner, DescriptorFingerprint: "generation-a",
	}); err != nil {
		t.Fatal(err)
	}
	catalog := tools.NewCatalog()
	if err := catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "exact-retry_echo", Source: "exact-retry"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := detachSourceExpected(context.Background(), catalog, registry, "exact-retry", owner, "generation-a", nil, "test"); !errors.Is(err, closeFault) {
		t.Fatalf("first exact detach = %v, want close fault", err)
	}
	if _, ok := catalog.Resolve("exact-retry_echo"); ok {
		t.Fatal("catalog remained dispatchable after withdrawal")
	}
	if _, err := registry.StageRegistration(mcpdrv.ServerRegistration{
		Provider: &detacherStubProvider{id: "exact-retry"}, Transport: "stdio", Owner: owner, DescriptorFingerprint: "generation-b",
	}, nil); err == nil {
		t.Fatal("replacement staged before close receipt")
	}
	if err := detachSourceExpected(context.Background(), catalog, registry, "exact-retry", owner, "generation-a", nil, "test"); err != nil {
		t.Fatalf("exact detach retry: %v", err)
	}
	if provider.closed != 2 {
		t.Fatalf("provider close calls = %d, want genuine retry", provider.closed)
	}
	if err := detachSourceExpected(context.Background(), catalog, registry, "exact-retry", owner, "generation-a", nil, "test"); err != nil {
		t.Fatalf("post-receipt idempotent retry: %v", err)
	}
}

// TestMCPConnectionDetacher_Detach_NeverRemovesBootDeclared pins the second half
// of the guarantee: a reconciling owner's detach cannot remove a boot-declared
// (zero-owner) registration even if its name reaches Detach.
func TestMCPConnectionDetacher_Detach_NeverRemovesBootDeclared(t *testing.T) {
	registry := mcpdrv.NewRegistry()
	prov := &detacherStubProvider{id: "boot-srv"}
	if err := registry.Register(detacherIDCtx(t), mcpdrv.ServerRegistration{
		Provider: prov, Transport: "stdio", InitialState: mcpdrv.ServerStateOnline,
		// No Owner — boot-declared.
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := NewMCPConnectionDetacher(tools.NewCatalog(), registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := d.Detach(context.Background(), "boot-srv", toolauth.Owner{Tenant: "tenant-a", Agent: "agent-a"}); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if got := registry.SourceIDs(); len(got) != 1 || got[0] != "boot-srv" {
		t.Fatalf("SourceIDs = %v, want the boot-declared [boot-srv] retained", got)
	}
	if prov.closed != 0 {
		t.Fatalf("boot-declared transport closed %d times, want 0", prov.closed)
	}
}
