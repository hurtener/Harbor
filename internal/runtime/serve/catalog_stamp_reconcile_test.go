package serve

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// TestCatalogStamp_NoOpRunStartReconcileAndConvergenceKeepRevision drives the
// PRODUCTION run-start attach/detach concretes and the production discovery
// refresh. The first reconcile attaches the declared connection and moves the
// catalog revision; a no-op reconcile and a convergence-style refresh of the
// unchanged server must leave it untouched, otherwise every run start and
// every convergence sweep would invalidate every structural catalog cache.
func TestCatalogStamp_NoOpRunStartReconcileAndConvergenceKeepRevision(t *testing.T) {
	fixture := reattachFixtureServer(t)
	attacher, catalog, registry, _ := reAttacherFor(t, WithReattachTimeout(15*time.Second))
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	agents := lifecycleTestRegistry(t, st)
	overlay, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	declared := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{reHTTPDesc("conv", fixture.URL)}}}
	if _, err := agents.SetRevision(ctx, identity.Quadruple{Identity: reQuad().Identity}, reAgent, agentcfg.ConfigScopeAgent, declared, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	resolver := projection.CatalogViewResolver{Registry: agents, SessionOverlay: overlay, Catalog: catalog, OwnerResolver: registry}
	stamp := func() string {
		t.Helper()
		revision, _, ok, err := resolver.CatalogStamp(ctx, reQuad().Identity, reAgent, "configuration")
		if err != nil || !ok {
			t.Fatalf("stamp ok=%v err=%v", ok, err)
		}
		return revision
	}
	detacher := NewMCPConnectionDetacher(catalog, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	before := stamp()
	if _, attached, err := projection.ReconcileConnections(ctx, agents, reAgent, reQuad(), detacher, attacher, nil); err != nil || attached != 1 {
		t.Fatalf("first reconcile attached=%d err=%v", attached, err)
	}
	attachedStamp := stamp()
	if attachedStamp == before {
		t.Fatal("attaching the declared connection did not move the revision")
	}
	view, err := resolver.ConfigurationCatalogView(ctx, reQuad().Identity, reAgent)
	if err != nil {
		t.Fatal(err)
	}
	physical := mcpdrv.PhysicalServerName("conv", reOwner())
	found := false
	for _, tool := range view.List() {
		found = found || strings.HasPrefix(tool.Name, physical+"_")
	}
	if !found {
		t.Fatalf("attached source missing from the configuration inventory: %v", view.List())
	}

	if detached, attached, err := projection.ReconcileConnections(ctx, agents, reAgent, reQuad(), detacher, attacher, nil); err != nil || detached != 0 || attached != 0 {
		t.Fatalf("no-op reconcile detached=%d attached=%d err=%v", detached, attached, err)
	}
	if got := stamp(); got != attachedStamp {
		t.Fatal("a no-op run-start reconcile moved the catalog revision")
	}

	if _, err := registry.RefreshDiscovery(reOwnedCtx(t), physical); err != nil {
		t.Fatalf("convergence refresh: %v", err)
	}
	if got := stamp(); got != attachedStamp {
		t.Fatal("a convergence refresh of an unchanged server moved the catalog revision")
	}

	// Removing the declaration and reconciling detaches and moves it again.
	if _, err := agents.SetRevision(ctx, identity.Quadruple{Identity: reQuad().Identity}, reAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	removedRevision := stamp()
	if removedRevision == attachedStamp {
		t.Fatal("removing the declaration did not move the revision")
	}
	if detached, _, err := projection.ReconcileConnections(ctx, agents, reAgent, reQuad(), detacher, attacher, nil); err != nil || detached != 1 {
		t.Fatalf("detach reconcile detached=%d err=%v", detached, err)
	}
	if got := stamp(); got == removedRevision {
		t.Fatal("detaching the connection did not move the revision")
	}
}
