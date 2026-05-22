package mcp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// stubProvider is a deterministic serverProvider for the registry unit
// tests — no MCP wire, no SDK. It returns a fixed descriptor set and a
// configurable error.
type stubProvider struct {
	id        tools.ToolSourceID
	mu        sync.Mutex
	calls     int
	discErr   error
	resources []string
	prompts   []string
	toolNames []string
}

func (p *stubProvider) SourceID() tools.ToolSourceID { return p.id }

func (p *stubProvider) Discover(ctx context.Context) ([]tools.ToolDescriptor, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.discErr != nil {
		return nil, p.discErr
	}
	var out []tools.ToolDescriptor
	for _, t := range p.toolNames {
		out = append(out, tools.ToolDescriptor{Tool: tools.Tool{Name: string(p.id) + "." + t}})
	}
	for _, r := range p.resources {
		out = append(out, tools.ToolDescriptor{Tool: tools.Tool{
			Name:        string(p.id) + resourceTypeSeparator + resourceNamePrefix + r,
			Description: "resource " + r,
		}})
	}
	for _, pr := range p.prompts {
		out = append(out, tools.ToolDescriptor{Tool: tools.Tool{
			Name:        string(p.id) + resourceTypeSeparator + promptNamePrefix + pr,
			Description: "prompt " + pr,
		}})
	}
	return out, nil
}

// idCtx returns a ctx carrying a complete identity triple.
func idCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID:  "t-1",
		UserID:    "u-1",
		SessionID: "s-1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// newTestRegistry builds a registry with two stub-backed servers.
func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry(WithRegistryClock(func() time.Time {
		return time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	}))
	if err := r.Register(ServerRegistration{
		Provider:          &stubProvider{id: "github-server", toolNames: []string{"issues"}, resources: []string{"repo://x"}, prompts: []string{"pr"}},
		Transport:         "http+sse",
		URLOrCommand:      "https://mcp.example.com/github",
		OAuthBindingCount: 2,
		InitialState:      ServerStateOnline,
	}); err != nil {
		t.Fatalf("Register github: %v", err)
	}
	if err := r.Register(ServerRegistration{
		Provider:     &stubProvider{id: "slack-server", toolNames: []string{"post"}},
		Transport:    "stdio",
		URLOrCommand: "/usr/bin/slack-mcp",
		InitialState: ServerStateOffline,
	}); err != nil {
		t.Fatalf("Register slack: %v", err)
	}
	return r
}

func TestRegistry_ListServers_Happy(t *testing.T) {
	r := newTestRegistry(t)
	servers, cur, err := r.ListServers(idCtx(t), ListFilter{})
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("want 2 servers, got %d", len(servers))
	}
	if servers[0].Name != "github-server" || servers[1].Name != "slack-server" {
		t.Fatalf("servers not name-sorted: %v %v", servers[0].Name, servers[1].Name)
	}
	if cur.NextPageToken != "" {
		t.Fatalf("want empty cursor, got %q", cur.NextPageToken)
	}
}

func TestRegistry_ListServers_FailsClosed_MissingIdentity(t *testing.T) {
	r := newTestRegistry(t)
	_, _, err := r.ListServers(context.Background(), ListFilter{})
	if !errors.Is(err, ErrRegistryIdentityMissing) {
		t.Fatalf("want ErrRegistryIdentityMissing, got %v", err)
	}
}

func TestRegistry_ListServers_FilterByTransport(t *testing.T) {
	r := newTestRegistry(t)
	servers, _, err := r.ListServers(idCtx(t), ListFilter{Transport: []string{"stdio"}})
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(servers) != 1 || servers[0].Name != "slack-server" {
		t.Fatalf("transport filter failed: %v", servers)
	}
}

func TestRegistry_ListServers_FilterByHasOAuth(t *testing.T) {
	r := newTestRegistry(t)
	yes := true
	servers, _, err := r.ListServers(idCtx(t), ListFilter{HasOAuth: &yes})
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(servers) != 1 || servers[0].Name != "github-server" {
		t.Fatalf("has-oauth filter failed: %v", servers)
	}
}

func TestRegistry_ListServers_PaginationCursorStable(t *testing.T) {
	r := newTestRegistry(t)
	page1, cur1, err := r.ListServers(idCtx(t), ListFilter{PageSize: 1})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 1 || cur1.NextPageToken == "" {
		t.Fatalf("page1 shape wrong: %v cursor=%q", page1, cur1.NextPageToken)
	}
	page2, cur2, err := r.ListServers(idCtx(t), ListFilter{PageSize: 1, PageToken: cur1.NextPageToken})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 1 || page2[0].Name == page1[0].Name {
		t.Fatalf("page2 overlaps page1: %v %v", page1, page2)
	}
	if cur2.NextPageToken != "" {
		t.Fatalf("want last page, got cursor %q", cur2.NextPageToken)
	}
}

func TestRegistry_GetServer_Happy(t *testing.T) {
	r := newTestRegistry(t)
	v, err := r.GetServer(idCtx(t), "github-server")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if v.Name != "github-server" || v.Transport != "http+sse" {
		t.Fatalf("GetServer shape wrong: %+v", v)
	}
}

func TestRegistry_GetServer_NotFound(t *testing.T) {
	r := newTestRegistry(t)
	_, err := r.GetServer(idCtx(t), "nope-server")
	if !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("want ErrServerNotFound, got %v", err)
	}
}

func TestRegistry_ListResources_Happy(t *testing.T) {
	r := newTestRegistry(t)
	res, err := r.ListResources(idCtx(t), "github-server")
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(res) != 1 || res[0].URI != "repo://x" {
		t.Fatalf("ListResources shape wrong: %v", res)
	}
}

func TestRegistry_ListPrompts_Happy(t *testing.T) {
	r := newTestRegistry(t)
	pr, err := r.ListPrompts(idCtx(t), "github-server")
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(pr) != 1 || pr[0].Name != "pr" {
		t.Fatalf("ListPrompts shape wrong: %v", pr)
	}
}

func TestRegistry_RefreshDiscovery_UpdatesCounts(t *testing.T) {
	r := newTestRegistry(t)
	res, err := r.RefreshDiscovery(idCtx(t), "github-server")
	if err != nil {
		t.Fatalf("RefreshDiscovery: %v", err)
	}
	if res.ToolCount != 1 || res.ResourceCount != 1 || res.PromptCount != 1 {
		t.Fatalf("RefreshDiscovery counts wrong: %+v", res)
	}
	if res.DiscoveryID == "" {
		t.Fatalf("want non-empty discovery id")
	}
	v, _ := r.GetServer(idCtx(t), "github-server")
	if v.ToolCount != 1 || v.State != ServerStateOnline {
		t.Fatalf("RefreshDiscovery did not update server view: %+v", v)
	}
}

func TestRegistry_Probe_Happy(t *testing.T) {
	r := newTestRegistry(t)
	res, err := r.Probe(idCtx(t), "github-server")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.OK {
		t.Fatalf("want OK probe, got %+v", res)
	}
}

func TestRegistry_Probe_ErrorRecorded(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(ServerRegistration{
		Provider:  &stubProvider{id: "bad-server", discErr: errors.New("transport down")},
		Transport: "http+sse",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	res, err := r.Probe(idCtx(t), "bad-server")
	if err != nil {
		t.Fatalf("Probe should not error on a probe-failure: %v", err)
	}
	if res.OK {
		t.Fatalf("want failed probe, got %+v", res)
	}
	v, _ := r.GetServer(idCtx(t), "bad-server")
	if v.State != ServerStateError {
		t.Fatalf("want error state after failed probe, got %v", v.State)
	}
}

func TestRegistry_Health_Snapshot(t *testing.T) {
	r := newTestRegistry(t)
	if _, err := r.RefreshDiscovery(idCtx(t), "github-server"); err != nil {
		t.Fatalf("RefreshDiscovery: %v", err)
	}
	r.RecordReconnect("github-server", "transport reset")
	snap, err := r.Health(idCtx(t), "github-server", 0)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(snap.HandshakeLatencyBuckets) != 1 {
		t.Fatalf("want 1 latency bucket, got %d", len(snap.HandshakeLatencyBuckets))
	}
	if len(snap.ReconnectHistory) != 1 {
		t.Fatalf("want 1 reconnect, got %d", len(snap.ReconnectHistory))
	}
}

func TestRegistry_SetRawHTMLTrust_PersistsFlag(t *testing.T) {
	r := newTestRegistry(t)
	prev, err := r.SetRawHTMLTrust(idCtx(t), "github-server", true)
	if err != nil {
		t.Fatalf("SetRawHTMLTrust: %v", err)
	}
	if prev {
		t.Fatalf("want prior value false, got true")
	}
	v, _ := r.GetServer(idCtx(t), "github-server")
	if !v.RawHTMLTrusted {
		t.Fatalf("trust flag not persisted")
	}
}

func TestRegistry_AllReads_FailClosed_MissingIdentity(t *testing.T) {
	r := newTestRegistry(t)
	bg := context.Background()
	if _, err := r.GetServer(bg, "github-server"); !errors.Is(err, ErrRegistryIdentityMissing) {
		t.Fatalf("GetServer: want identity-missing, got %v", err)
	}
	if _, err := r.ListResources(bg, "github-server"); !errors.Is(err, ErrRegistryIdentityMissing) {
		t.Fatalf("ListResources: want identity-missing, got %v", err)
	}
	if _, err := r.ListPrompts(bg, "github-server"); !errors.Is(err, ErrRegistryIdentityMissing) {
		t.Fatalf("ListPrompts: want identity-missing, got %v", err)
	}
	if _, err := r.RefreshDiscovery(bg, "github-server"); !errors.Is(err, ErrRegistryIdentityMissing) {
		t.Fatalf("RefreshDiscovery: want identity-missing, got %v", err)
	}
	if _, err := r.Probe(bg, "github-server"); !errors.Is(err, ErrRegistryIdentityMissing) {
		t.Fatalf("Probe: want identity-missing, got %v", err)
	}
	if _, err := r.Health(bg, "github-server", 0); !errors.Is(err, ErrRegistryIdentityMissing) {
		t.Fatalf("Health: want identity-missing, got %v", err)
	}
	if _, err := r.SetRawHTMLTrust(bg, "github-server", true); !errors.Is(err, ErrRegistryIdentityMissing) {
		t.Fatalf("SetRawHTMLTrust: want identity-missing, got %v", err)
	}
}
