package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// collisionServer starts one httptest-hosted MCP server the attach paths dial.
func collisionServer(t *testing.T) *httptest.Server {
	t.Helper()
	mockSrv := newMockServer()
	h := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return mockSrv.server }, nil)
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// catalogNames returns the sorted tool names currently in the catalog.
func catalogNames(cat tools.ToolCatalog) []string {
	listed := cat.List(tools.CatalogFilter{})
	out := make([]string, 0, len(listed))
	for _, tl := range listed {
		out = append(out, tl.Name)
	}
	sort.Strings(out)
	return out
}

// collisionHarness bundles the shared collaborators an attach needs.
type collisionHarness struct {
	url     string
	cat     tools.ToolCatalog
	closers []func(context.Context) error
}

// attach runs one Attach against the harness's shared catalog and the supplied
// registry, under the supplied owner.
func (h *collisionHarness) attach(t *testing.T, ctx context.Context, name string, owner auth.Owner, reg *Registry) error {
	t.Helper()
	return Attach(ctx, config.MCPServerConfig{
		Name:          name,
		TransportMode: string(TransportSSE),
		URL:           h.url,
	}, AttachDeps{
		Catalog:         h.cat,
		Registry:        reg,
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
		Closers:         &h.closers,
		Owner:           owner,
	})
}

func newCollisionHarness(t *testing.T) *collisionHarness {
	t.Helper()
	h := &collisionHarness{
		url:     collisionServer(t).URL,
		cat:     tools.NewCatalog(),
		closers: []func(context.Context) error{},
	}
	t.Cleanup(func() {
		for i := len(h.closers) - 1; i >= 0; i-- {
			_ = h.closers[i](context.Background())
		}
	})
	return h
}

func TestAttach_CrossOwnerSameName_IndependentPhysicalSources(t *testing.T) {
	h := newCollisionHarness(t)
	reg := NewRegistry()
	owners := []auth.Owner{{Tenant: "tenant-a", Agent: "agent-1"}, {Tenant: "tenant-b", Agent: "agent-1"}}
	for _, owner := range owners {
		if err := h.attach(t, context.Background(), "github", owner, reg); err != nil {
			t.Fatal(err)
		}
	}
	for _, owner := range owners {
		physical := PhysicalServerName("github", owner)
		if got, ok := reg.OwnerOf(physical); !ok || got != owner {
			t.Fatalf("physical owner: %v %v", got, ok)
		}
		found := false
		for _, name := range catalogNames(h.cat) {
			if strings.HasPrefix(name, physical+"_") {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing source catalog: %s", physical)
		}
	}
}

// These real MCP transport fixtures exercise owner-derived source names.
func TestAttach_CrossOwnerSameName_CatalogNamespaceIsIndependent(t *testing.T) {
	h := newCollisionHarness(t)
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if err := h.attach(t, context.Background(), "github", auth.Owner{Tenant: tenant, Agent: "agent-1"}, NewRegistry()); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.closers) != 2 {
		t.Fatalf("transport count: %d", len(h.closers))
	}
}
