package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	statepostgres "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	"github.com/hurtener/Harbor/internal/tools"
	toolapproval "github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// countingStore counts every StateStore read so the tests can assert the
// exact cost of a stamp, a cache hit and a conditional read.
type countingStore struct {
	state.StateStore
	reads atomic.Int64
}

func (c *countingStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	c.reads.Add(1)
	return c.StateStore.Load(ctx, id, kind)
}

func (c *countingStore) LoadByEventID(ctx context.Context, eventID state.EventID) (state.StateRecord, error) {
	c.reads.Add(1)
	return c.StateStore.LoadByEventID(ctx, eventID)
}

func (c *countingStore) ListKind(ctx context.Context, scope state.ListScope, prefix string) ([]state.StateRecord, error) {
	c.reads.Add(1)
	return c.StateStore.ListKind(ctx, scope, prefix)
}

func (c *countingStore) ListKindBounded(ctx context.Context, scope state.ListScope, prefix string, limit int) ([]state.StateRecord, error) {
	c.reads.Add(1)
	return c.StateStore.ListKindBounded(ctx, scope, prefix, limit)
}

func (c *countingStore) ListKindForIdentity(ctx context.Context, id identity.Quadruple, prefix string) ([]state.StateRecord, error) {
	c.reads.Add(1)
	return c.StateStore.ListKindForIdentity(ctx, id, prefix)
}

func (c *countingStore) ListKindForIdentityBounded(ctx context.Context, id identity.Quadruple, prefix string, limit int) ([]state.StateRecord, error) {
	c.reads.Add(1)
	return c.StateStore.ListKindForIdentityBounded(ctx, id, prefix, limit)
}

func (c *countingStore) ScanKindForTenant(ctx context.Context, scope state.ListScope, tenantID, prefix string, limit int, continuation string) (state.StateScanPage, error) {
	c.reads.Add(1)
	return c.StateStore.ScanKindForTenant(ctx, scope, tenantID, prefix, limit, continuation)
}

// countingResolver counts structural builds of the configuration inventory.
type countingResolver struct {
	projection.CatalogViewResolver
	builds atomic.Int64
}

func (r *countingResolver) ConfigurationCatalogView(ctx context.Context, id identity.Identity, agentID string) (tools.PlannerCatalogView, error) {
	r.builds.Add(1)
	return r.CatalogViewResolver.ConfigurationCatalogView(ctx, id, agentID)
}

// fakeServer is a minimal MCP provider whose discovery snapshot the test
// controls.
type fakeServer struct {
	source tools.ToolSourceID
	mu     sync.Mutex
	descs  []tools.ToolDescriptor
}

func (f *fakeServer) SourceID() tools.ToolSourceID { return f.source }
func (f *fakeServer) Discover(context.Context) ([]tools.ToolDescriptor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tools.ToolDescriptor(nil), f.descs...), nil
}
func (f *fakeServer) DisplayModes() []string { return nil }
func (f *fakeServer) ReadResource(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New("no resources")
}
func (f *fakeServer) Close(context.Context) error { return nil }

func (f *fakeServer) setTools(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.descs = f.descs[:0]
	for _, name := range names {
		f.descs = append(f.descs, tools.ToolDescriptor{
			Tool:   tools.Tool{Name: string(f.source) + "_" + name, Source: f.source, Transport: tools.TransportMCP, SideEffects: tools.SideEffectRead, Loading: tools.LoadingDeferred},
			Invoke: noopInvoke,
		})
	}
}

func noopInvoke(context.Context, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{}, nil
}

const (
	revAgent = "agent-rev"
)

type catalogFixture struct {
	t         *testing.T
	tenant    string
	store     *countingStore
	agents    agentcfg.Registry
	overlay   sessionoverlay.Store
	catalog   tools.ToolCatalog
	mcp       *mcpdrv.Registry
	resolver  *countingResolver
	projector *CatalogProjector
	service   *Service
}

func newInmemStore(t *testing.T) state.StateStore {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

// newCatalogFixture assembles one "runtime": its own process-local catalog
// and MCP registry over a (possibly shared) StateStore.
func newCatalogFixture(t *testing.T, st state.StateStore, tenant string) *catalogFixture {
	t.Helper()
	store := &countingStore{StateStore: st}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	agents, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	overlay, err := sessionoverlay.NewStore(store, nil)
	if err != nil {
		t.Fatalf("sessionoverlay: %v", err)
	}
	catalog := tools.NewCatalog()
	registry := mcpdrv.NewRegistry()
	resolver := &countingResolver{CatalogViewResolver: projection.CatalogViewResolver{
		Registry: agents, SessionOverlay: overlay, Catalog: catalog, OwnerResolver: registry,
	}}
	projector, err := NewCatalogProjector(catalog, WithCatalogViewResolver(resolver), WithLogicalSourceResolver(registry))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(projector)
	if err != nil {
		t.Fatal(err)
	}
	f := &catalogFixture{t: t, tenant: tenant, store: store, agents: agents, overlay: overlay, catalog: catalog, mcp: registry, resolver: resolver, projector: projector, service: service}
	// The agent must have an active revision so the user tier validates.
	f.setAgentRevision(agentcfg.ConfigPayload{})
	return f
}

func (f *catalogFixture) id(user, session string) identity.Identity {
	return identity.Identity{TenantID: f.tenant, UserID: user, SessionID: session}
}

func (f *catalogFixture) setAgentRevision(payload agentcfg.ConfigPayload) {
	f.t.Helper()
	q := identity.Quadruple{Identity: f.id("operator", "op-session")}
	if _, err := f.agents.SetRevision(context.Background(), q, revAgent, agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{}); err != nil {
		f.t.Fatalf("agent revision: %v", err)
	}
}

func (f *catalogFixture) registerBootTools(n int) {
	f.t.Helper()
	for i := range n {
		if err := f.catalog.Register(tools.ToolDescriptor{
			Tool:   tools.Tool{Name: fmt.Sprintf("boot_%03d", i), Description: "boot tool", SideEffects: tools.SideEffectWrite},
			Invoke: noopInvoke,
		}); err != nil {
			f.t.Fatal(err)
		}
	}
}

// attachUserSource grants user the signed user-tier pair for logical and
// attaches the matching user-owned MCP server, publishing its tools.
func (f *catalogFixture) attachUserSource(user, logical string, toolNames ...string) *fakeServer {
	f.t.Helper()
	ctx := context.Background()
	owner := toolauth.Owner{Tenant: f.tenant, Agent: revAgent, User: user}
	pair := &agentcfg.SignedOAuthMCPPair{
		ProviderName: "provider-" + user, Broker: "broker", Audience: "audience", Scopes: []string{"read"},
		CapabilityRevision: "capability", URLDigest: agentcfg.OAuthMCPURLDigest("https://example.test/mcp"),
		Sink: "https://example.test", SinkDigest: agentcfg.OAuthMCPURLDigest("https://example.test"),
		Connection:             agentcfg.SignedOAuthMCPConnectionDescriptor{Name: logical, URL: "https://example.test/mcp"},
		AuthorityOperationKind: "operation-" + user, OwnerAgentID: revAgent, OwnerUserID: user, OwnerSessionID: "s-" + user,
	}
	q := identity.Quadruple{Identity: f.id(user, "s-"+user)}
	if _, err := f.agents.SetRevision(agentcfg.WithSignedOAuthMCPFenceOperation(ctx, pair.AuthorityOperationKind), q, revAgent, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{SignedOAuthMCPPair: pair}, agentcfg.SetOptions{}); err != nil {
		f.t.Fatalf("user pair: %v", err)
	}
	server := &fakeServer{source: tools.ToolSourceID(mcpdrv.PhysicalServerName(logical, owner))}
	server.setTools(toolNames...)
	if err := f.mcp.Register(ctx, mcpdrv.ServerRegistration{Provider: server, Transport: "streamable-http", Owner: owner, LogicalName: logical, DescriptorFingerprint: "fp-" + user, Catalog: f.catalog}); err != nil {
		f.t.Fatalf("register: %v", err)
	}
	f.refresh(user, server)
	return server
}

func (f *catalogFixture) refresh(user string, server *fakeServer) {
	f.t.Helper()
	ctx, err := identity.With(context.Background(), f.id(user, "s-"+user))
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.mcp.RefreshDiscovery(ctx, string(server.source)); err != nil {
		f.t.Fatalf("refresh: %v", err)
	}
}

func (f *catalogFixture) list(id identity.Identity, mutate func(*prototypes.ToolListRequest)) (prototypes.ToolListResponse, error) {
	req := prototypes.ToolListRequest{
		View:     prototypes.ToolCatalogViewConfiguration,
		Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		AgentID:  revAgent,
		PageSize: prototypes.MaxToolListPageSize,
	}
	if mutate != nil {
		mutate(&req)
	}
	return f.service.ListForView(context.Background(), req, true, revAgent)
}

func (f *catalogFixture) mustList(id identity.Identity, mutate func(*prototypes.ToolListRequest)) prototypes.ToolListResponse {
	f.t.Helper()
	resp, err := f.list(id, mutate)
	if err != nil {
		f.t.Fatalf("list: %v", err)
	}
	return resp
}

func rowNames(rows []prototypes.Tool) map[string]prototypes.Tool {
	out := make(map[string]prototypes.Tool, len(rows))
	for _, row := range rows {
		out[row.ID] = row
	}
	return out
}

// AC3: an unchanged repeated list is served from the cache after exactly the
// four stamp reads; a matching conditional read costs the same four reads and
// returns not_modified with no rows.
func TestCatalogRevision_CacheHitCostsOnlyStampReads(t *testing.T) {
	f := newCatalogFixture(t, newInmemStore(t), "tenant-ac3")
	f.registerBootTools(100)
	admin := f.id("admin-1", "sess-1")

	first := f.mustList(admin, nil)
	if first.Revision == "" || len(first.Tools) != 100 {
		t.Fatalf("first list: revision=%q rows=%d", first.Revision, len(first.Tools))
	}
	if got := f.resolver.builds.Load(); got != 1 {
		t.Fatalf("builds after first list = %d, want 1", got)
	}
	for _, row := range first.Tools {
		if row.SideEffect != string(tools.SideEffectWrite) || row.LoadingMode != string(tools.LoadingAlways) {
			t.Fatalf("row %s missing structural side_effect/loading_mode: %+v", row.ID, row)
		}
	}

	before := f.store.reads.Load()
	second := f.mustList(admin, nil)
	if reads := f.store.reads.Load() - before; reads != 4 {
		t.Fatalf("cached list made %d StateStore reads, want exactly 4 (stamp only)", reads)
	}
	if f.resolver.builds.Load() != 1 || second.Revision != first.Revision || len(second.Tools) != 100 {
		t.Fatalf("second list not served from cache: builds=%d rev=%q rows=%d", f.resolver.builds.Load(), second.Revision, len(second.Tools))
	}

	before = f.store.reads.Load()
	cond := f.mustList(admin, func(r *prototypes.ToolListRequest) { r.IfRevision = first.Revision })
	if reads := f.store.reads.Load() - before; reads != 4 {
		t.Fatalf("conditional list made %d reads, want 4", reads)
	}
	if !cond.NotModified || cond.Revision != first.Revision || len(cond.Tools) != 0 {
		t.Fatalf("conditional list = %+v, want not_modified with no rows", cond)
	}
	if f.resolver.builds.Load() != 1 {
		t.Fatal("conditional list built the inventory")
	}

	// A stale revision returns full rows.
	stale := f.mustList(admin, func(r *prototypes.ToolListRequest) { r.IfRevision = "stale" })
	if stale.NotModified || len(stale.Tools) != 100 || stale.Revision != first.Revision {
		t.Fatalf("stale conditional list = notModified=%v rows=%d", stale.NotModified, len(stale.Tools))
	}
}

// AC4: describing every tool of a 100-tool inventory performs one build.
func TestCatalogRevision_DescribeAllToolsBuildsOnce(t *testing.T) {
	f := newCatalogFixture(t, newInmemStore(t), "tenant-ac4")
	f.registerBootTools(100)
	admin := f.id("admin-1", "sess-1")
	listed := f.mustList(admin, nil)
	for _, row := range listed.Tools {
		m, err := f.service.DescribeForView(context.Background(), prototypes.ToolDescribeRequest{
			View: prototypes.ToolCatalogViewConfiguration, Identity: prototypes.IdentityScope{Tenant: admin.TenantID, User: admin.UserID, Session: admin.SessionID},
			ID: row.ID, AgentID: revAgent,
		}, true, revAgent)
		if err != nil {
			t.Fatalf("describe %s: %v", row.ID, err)
		}
		if m.Revision != listed.Revision || m.Tool.ID != row.ID || m.Tool.SideEffect != m.SideEffect || m.Tool.LoadingMode != m.LoadingMode {
			t.Fatalf("describe %s: revision/structure mismatch: %+v", row.ID, m)
		}
	}
	if got := f.resolver.builds.Load(); got != 1 {
		t.Fatalf("describing 100 tools made %d structural builds, want 1", got)
	}
}

// AC6: a mutation landing between the pre-build stamp and the build is never
// cached under the stale revision; the next read rebuilds and sees it.
func TestCatalogRevision_ABAGuardPreventsInsertion(t *testing.T) {
	f := newCatalogFixture(t, newInmemStore(t), "tenant-ac6")
	f.registerBootTools(3)
	admin := f.id("admin-1", "sess-1")
	var once sync.Once
	f.projector.afterStructuralBuild = func() {
		once.Do(func() {
			if err := f.catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "late_tool"}, Invoke: noopInvoke}); err != nil {
				t.Error(err)
			}
		})
	}
	first := f.mustList(admin, nil)
	if f.projector.structural.len() != 0 {
		t.Fatal("entry inserted although the stamp moved during the build")
	}
	second := f.mustList(admin, nil)
	if got := f.resolver.builds.Load(); got != 2 {
		t.Fatalf("builds=%d, want a rebuild after the guarded miss", got)
	}
	if second.Revision == first.Revision {
		t.Fatal("revision did not move across the injected mutation")
	}
	if _, ok := rowNames(second.Tools)["late_tool"]; !ok {
		t.Fatal("rebuilt inventory misses the mutation")
	}
	if f.projector.structural.len() != 1 {
		t.Fatal("stable rebuild was not cached")
	}
	// The pre-mutation revision is never answered not_modified again.
	cond := f.mustList(admin, func(r *prototypes.ToolListRequest) { r.IfRevision = first.Revision })
	if cond.NotModified {
		t.Fatal("stale pre-mutation revision answered not_modified")
	}
}

// AC7: callers that fail admission are refused before any stamp read.
func TestCatalogRevision_UnauthorizedRefusedBeforeStampReads(t *testing.T) {
	f := newCatalogFixture(t, newInmemStore(t), "tenant-ac7")
	f.registerBootTools(5)
	admin := f.id("admin-1", "sess-1")
	scope := prototypes.IdentityScope{Tenant: admin.TenantID, User: admin.UserID, Session: admin.SessionID}
	for _, tc := range []struct {
		name     string
		req      prototypes.ToolListRequest
		admin    bool
		admitted string
		want     error
	}{
		{name: "not admin", req: prototypes.ToolListRequest{View: prototypes.ToolCatalogViewConfiguration, Identity: scope, AgentID: revAgent, IfRevision: "x"}, admitted: revAgent, want: ErrAdminScopeRequired},
		{name: "not admitted", req: prototypes.ToolListRequest{View: prototypes.ToolCatalogViewConfiguration, Identity: scope, AgentID: revAgent}, admin: true, want: ErrInvalidRequest},
		{name: "if_revision on execution view", req: prototypes.ToolListRequest{Identity: scope, IfRevision: "x"}, admin: true, want: ErrInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := f.store.reads.Load()
			_, err := f.service.ListForView(context.Background(), tc.req, tc.admin, tc.admitted)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want %v", err, tc.want)
			}
			if reads := f.store.reads.Load() - before; reads != 0 {
				t.Fatalf("refused request made %d StateStore reads", reads)
			}
		})
	}
	// The execution view never reports a revision.
	resp, err := f.service.ListForView(context.Background(), prototypes.ToolListRequest{Identity: scope}, false, "")
	if err != nil || resp.Revision != "" {
		t.Fatalf("execution view revision=%q err=%v", resp.Revision, err)
	}
}

// AC5: entries never cross principals. A tenant_user source owned by user A
// stays invisible to user B (who has no user tier) while both read
// concurrently through the shared cache; two admins and filtered/unfiltered
// reads keep independent results.
func TestCatalogRevision_CrossPrincipalIsolation(t *testing.T) {
	f := newCatalogFixture(t, newInmemStore(t), "tenant-ac5")
	f.registerBootTools(4)
	f.attachUserSource("user-a", "private", "alpha", "beta")
	userA := f.id("user-a", "s-user-a")
	userB := f.id("user-b", "s-user-b")
	privateTool := mcpdrv.PhysicalServerName("private", toolauth.Owner{Tenant: f.tenant, Agent: revAgent, User: "user-a"}) + "_alpha"

	a := f.mustList(userA, nil)
	if _, ok := rowNames(a.Tools)[privateTool]; !ok {
		t.Fatalf("owner does not see own source: %v", rowNames(a.Tools))
	}
	if row := rowNames(a.Tools)[privateTool]; row.Owner != "private" || row.LoadingMode != string(tools.LoadingDeferred) || row.SideEffect != string(tools.SideEffectRead) {
		t.Fatalf("owner row projection = %+v", row)
	}

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			who := userA
			if i%2 == 1 {
				who = userB
			}
			resp, err := f.list(who, nil)
			if err != nil {
				t.Error(err)
				return
			}
			_, visible := rowNames(resp.Tools)[privateTool]
			if who == userB && visible {
				t.Error("user B observed user A's tenant_user source")
			}
			if who == userA && !visible {
				t.Error("user A lost own source")
			}
		}(i)
	}
	wg.Wait()
	b := f.mustList(userB, nil)
	if a.Revision == b.Revision {
		t.Fatal("two principals share one revision")
	}
	if len(b.Tools) != 4 {
		t.Fatalf("user B rows=%d, want only the 4 boot tools", len(b.Tools))
	}
	// A revision minted for user A is never not_modified for user B.
	if cond := f.mustList(userB, func(r *prototypes.ToolListRequest) { r.IfRevision = a.Revision }); cond.NotModified {
		t.Fatal("user A's revision answered not_modified for user B")
	}

	// Filtered and unfiltered reads: the filter is applied after the cached
	// structural inventory, so neither contaminates the other.
	filtered := f.mustList(userA, func(r *prototypes.ToolListRequest) {
		r.Filter.Transports = []prototypes.ToolTransport{prototypes.ToolTransportMCP}
	})
	if len(filtered.Tools) != 2 || filtered.Revision != a.Revision {
		t.Fatalf("filtered rows=%d rev match=%v", len(filtered.Tools), filtered.Revision == a.Revision)
	}
	unfiltered := f.mustList(userA, nil)
	if len(unfiltered.Tools) != 6 {
		t.Fatalf("unfiltered after filtered rows=%d, want 6", len(unfiltered.Tools))
	}
}

// AC5 (two acting admins): each admin's user-tier exposure is part of its own
// entry.
func TestCatalogRevision_TwoAdminsIndependentEntries(t *testing.T) {
	f := newCatalogFixture(t, newInmemStore(t), "tenant-ac5b")
	f.registerBootTools(2)
	f.attachUserSource("admin-a", "mine", "one")
	f.attachUserSource("admin-b", "theirs", "two")
	a := f.mustList(f.id("admin-a", "s-admin-a"), nil)
	b := f.mustList(f.id("admin-b", "s-admin-b"), nil)
	if a.Revision == b.Revision {
		t.Fatal("admins share a revision")
	}
	for name := range rowNames(a.Tools) {
		if strings.HasPrefix(name, "theirs") {
			t.Fatalf("admin A sees admin B's source %s", name)
		}
	}
	for name := range rowNames(b.Tools) {
		if strings.HasPrefix(name, "mine") {
			t.Fatalf("admin B sees admin A's source %s", name)
		}
	}
	if f.projector.structural.len() != 2 {
		t.Fatalf("cache entries=%d, want one per admin", f.projector.structural.len())
	}
}

// pageAll is the documented client algorithm: page 1 conditional; later
// pages full; restart once when a page's revision differs from page 1's.
func pageAll(t *testing.T, f *catalogFixture, id identity.Identity, cached string, betweenPages func(page int)) (rows []prototypes.Tool, revision string, notModified bool, restarts int) {
	t.Helper()
	for range 2 {
		first := f.mustList(id, func(r *prototypes.ToolListRequest) { r.IfRevision = cached; r.Page = 1; r.PageSize = 200 })
		if first.NotModified {
			return nil, first.Revision, true, restarts
		}
		rows = append([]prototypes.Tool(nil), first.Tools...)
		consistent := true
		for page := 2; page <= first.PageCount; page++ {
			if betweenPages != nil {
				betweenPages(page)
			}
			next := f.mustList(id, func(r *prototypes.ToolListRequest) { r.IfRevision = first.Revision; r.Page = page; r.PageSize = 200 })
			if next.NotModified {
				t.Fatalf("page %d answered not_modified; if_revision must be ignored after page 1", page)
			}
			if next.Revision != first.Revision {
				consistent = false
				break
			}
			rows = append(rows, next.Tools...)
		}
		if consistent {
			return rows, first.Revision, false, restarts
		}
		restarts++
		cached = ""
	}
	t.Fatal("catalog kept changing across a restart")
	return nil, "", false, restarts
}

// AC8: a 450-tool inventory (3 pages of 200).
func TestCatalogRevision_MultiPageConditionalPaging(t *testing.T) {
	f := newCatalogFixture(t, newInmemStore(t), "tenant-ac8")
	f.registerBootTools(450)
	admin := f.id("admin-1", "sess-1")

	rows, revision, notModified, restarts := pageAll(t, f, admin, "", nil)
	if notModified || restarts != 0 || len(rows) != 450 || revision == "" {
		t.Fatalf("full fetch rows=%d restarts=%d notModified=%v", len(rows), restarts, notModified)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.ID] {
			t.Fatalf("duplicate row %s across pages", row.ID)
		}
		seen[row.ID] = true
	}

	_, again, notModified, _ := pageAll(t, f, admin, revision, nil)
	if !notModified || again != revision {
		t.Fatal("unchanged 450-tool catalog did not answer page 1 not_modified")
	}

	mutated := false
	rows, after, notModified, restarts := pageAll(t, f, admin, "", func(page int) {
		if page == 2 && !mutated {
			mutated = true
			if err := f.catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "boot_zzz_new"}, Invoke: noopInvoke}); err != nil {
				t.Fatal(err)
			}
		}
	})
	if notModified || restarts != 1 || len(rows) != 451 || after == revision {
		t.Fatalf("mid-paging mutation: rows=%d restarts=%d newRevision=%v", len(rows), restarts, after != revision)
	}
}

// AC1 (projector half): discovery with an identical snapshot is a no-op for
// the revision; a changed snapshot moves it; an approval-policy write and a
// tool invocation never move it.
func TestCatalogRevision_DiscoveryNoOpKeepsRevision(t *testing.T) {
	f := newCatalogFixture(t, newInmemStore(t), "tenant-ac1")
	server := f.attachUserSource("user-a", "src", "one", "two")
	userA := f.id("user-a", "s-user-a")
	base := f.mustList(userA, nil)

	f.refresh("user-a", server) // convergence-style refresh, unchanged server
	if got := f.mustList(userA, nil); got.Revision != base.Revision {
		t.Fatal("no-op discovery moved the revision")
	}
	if f.resolver.builds.Load() != 1 {
		t.Fatal("no-op discovery invalidated the cache")
	}

	policies, err := toolapproval.NewStatePolicyStore(f.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := policies.SetPolicy(context.Background(), userA, base.Tools[0].ID, prototypes.ToolApprovalGated); err != nil {
		t.Fatal(err)
	}
	if got := f.mustList(userA, nil); got.Revision != base.Revision {
		t.Fatal("approval-policy write moved the revision")
	}
	desc, ok := f.catalog.Resolve(base.Tools[0].ID)
	if !ok {
		t.Fatal("resolve")
	}
	if _, err := desc.Invoke(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if got := f.mustList(userA, nil); got.Revision != base.Revision {
		t.Fatal("tool invocation moved the revision")
	}

	server.setTools("one", "two", "three")
	f.refresh("user-a", server)
	changed := f.mustList(userA, nil)
	if changed.Revision == base.Revision || len(changed.Tools) != 3 {
		t.Fatalf("changed discovery: revision moved=%v rows=%d", changed.Revision != base.Revision, len(changed.Tools))
	}
}

// AC2: two runtimes over one store. Runtime 2's conditional read after
// runtime 1's write returns full rows and a new revision without any event.
// Runs on a shared in-memory store always, and on Postgres when
// HARBOR_TEST_PG_DSN (or HARBOR_PG_DSN) is set.
func TestCatalogRevision_TwoRuntimesSharedStore(t *testing.T) {
	stores := map[string]func(*testing.T) state.StateStore{"inmem": newInmemStore}
	dsn := os.Getenv("HARBOR_TEST_PG_DSN")
	if dsn == "" {
		dsn = os.Getenv("HARBOR_PG_DSN")
	}
	if dsn != "" {
		stores["postgres"] = func(t *testing.T) state.StateStore {
			st, err := statepostgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
			if err != nil {
				t.Fatalf("postgres: %v", err)
			}
			t.Cleanup(func() { _ = st.Close(context.Background()) })
			return st
		}
	}
	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			st := open(t)
			tenant := fmt.Sprintf("tenant-ac2-%d", time.Now().UnixNano())
			r1 := newCatalogFixture(t, st, tenant)
			r2 := newCatalogFixture(t, st, tenant)
			for _, r := range []*catalogFixture{r1, r2} {
				r.registerBootTools(3)
			}
			admin := r2.id("admin-1", "sess-1")
			before := r2.mustList(admin, nil)
			if len(before.Tools) != 3 {
				t.Fatalf("rows=%d", len(before.Tools))
			}
			// Runtime 1 disables a tool for the agent (a durable revision).
			r1.setAgentRevision(agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{DisabledTools: []string{"boot_000"}}})
			after := r2.mustList(admin, func(r *prototypes.ToolListRequest) { r.IfRevision = before.Revision })
			if after.NotModified || after.Revision == before.Revision || len(after.Tools) != 3 {
				t.Fatalf("runtime 2 after runtime 1 write: notModified=%v revisionMoved=%v rows=%d", after.NotModified, after.Revision != before.Revision, len(after.Tools))
			}
		})
	}
}

// Race: concurrent mutation, list and describe never corrupt an entry or
// serve rows for another principal (run under -race).
func TestCatalogRevision_ConcurrentMutateListDescribe(t *testing.T) {
	f := newCatalogFixture(t, newInmemStore(t), "tenant-race")
	f.registerBootTools(20)
	server := f.attachUserSource("user-a", "src", "one")
	ids := []identity.Identity{f.id("user-a", "s-user-a"), f.id("user-b", "s-user-b"), f.id("admin-c", "s-c")}
	var wg sync.WaitGroup
	var mutations atomic.Int64
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := ids[i%len(ids)]
			for j := range 10 {
				switch (i + j) % 4 {
				case 0:
					n := mutations.Add(1)
					if err := f.catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: fmt.Sprintf("late_%d", n)}, Invoke: noopInvoke}); err != nil {
						t.Error(err)
					}
				case 1:
					if i%2 == 0 {
						server.setTools("one", fmt.Sprintf("v%d", j%2))
						f.refresh("user-a", server)
					}
				case 2:
					if _, err := f.service.DescribeForView(context.Background(), prototypes.ToolDescribeRequest{
						View: prototypes.ToolCatalogViewConfiguration, Identity: prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
						ID: "boot_001", AgentID: revAgent,
					}, true, revAgent); err != nil {
						t.Error(err)
					}
				default:
					resp, err := f.list(id, nil)
					if err != nil {
						t.Error(err)
						continue
					}
					for _, row := range resp.Tools {
						if id.UserID != "user-a" && strings.HasPrefix(row.ID, "src") {
							t.Errorf("%s saw user-a source %s", id.UserID, row.ID)
						}
					}
				}
			}
		}(i)
	}
	wg.Wait()
}
