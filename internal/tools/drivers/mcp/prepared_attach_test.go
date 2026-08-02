package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

type recordingSearchCache struct {
	mu    sync.Mutex
	tools map[string]tools.Tool
}

func (c *recordingSearchCache) Search(_ context.Context, _ string, _ []string, _ int) ([]tools.Tool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]tools.Tool, 0, len(c.tools))
	for _, tool := range c.tools {
		out = append(out, tool)
	}
	return out, nil
}
func (c *recordingSearchCache) Sync(_ context.Context, in []tools.Tool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tools == nil {
		c.tools = make(map[string]tools.Tool)
	}
	for _, tool := range in {
		c.tools[tool.Name] = tool
	}
	return nil
}
func (*recordingSearchCache) Close() error { return nil }

type blockingStageCatalog struct {
	tools.ToolCatalog
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingStageCatalog) StageSource(source tools.ToolSourceID, replacements []tools.ToolDescriptor, replaceExisting bool) (tools.CatalogSourceSwap, error) {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return c.ToolCatalog.StageSource(source, replacements, replaceExisting)
}

func preparedFixture(t *testing.T, name string, cat tools.ToolCatalog, reg *Registry, owner auth.Owner) *PreparedAttachment {
	t.Helper()
	mockSrv := newMockServer()
	handler := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return mockSrv.server }, nil)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	closers := []func(context.Context) error{}
	prepared, err := Prepare(ctx, config.MCPServerConfig{Name: name, TransportMode: string(TransportSSE), URL: server.URL}, AttachDeps{
		Catalog: cat, Registry: reg, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), Closers: &closers, Owner: owner,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return prepared
}

func TestPreparedAttachment_PrepareDoesNotPublishAndCloseIsIdempotent(t *testing.T) {
	cat := tools.NewCatalog()
	reg := NewRegistry()
	p := preparedFixture(t, "staged", cat, reg, auth.Owner{Tenant: "tenant", Agent: "agent"})
	if _, ok := cat.Resolve("staged_echo"); ok {
		t.Fatal("Prepare published a catalog tool before Activate")
	}
	if got := reg.SourceIDs(); len(got) != 0 {
		t.Fatalf("Prepare published a registry entry before Activate: %v", got)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := p.Activate(context.Background()); err == nil {
		t.Fatal("Activate succeeded after Close")
	}
}

func TestPreparedAttachment_SignedToolProjectionAppliesAllowAndDenyBeforePublication(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow []string
		deny  []string
		want  bool
	}{
		{name: "allow exact tool", allow: []string{"echo"}, want: true},
		{name: "allow excludes tool", allow: []string{"other"}, want: false},
		{name: "deny overrides allow", allow: []string{"echo"}, deny: []string{"echo"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockSrv := newMockServer()
			handler := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return mockSrv.server }, nil)
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cat := tools.NewCatalog()
			reg := NewRegistry()
			closers := []func(context.Context) error{}
			prepared, err := Prepare(ctx, config.MCPServerConfig{Name: "signed", TransportMode: string(TransportSSE), URL: server.URL}, AttachDeps{
				Catalog: cat, Registry: reg, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), Closers: &closers,
				Owner: auth.Owner{Tenant: "tenant", Agent: "agent"}, DescriptorFingerprint: "signed-descriptor",
				ToolAllowlist: tc.allow, ToolDenylist: tc.deny,
			})
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			t.Cleanup(func() { _ = prepared.Close(context.Background()) })
			if err := prepared.Activate(ctx); err != nil {
				t.Fatalf("activate: %v", err)
			}
			_, got := cat.Resolve("signed_echo")
			if got != tc.want {
				t.Fatalf("published echo=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestPreparedAttachment_SameOwnerOldRegistrationLivesUntilActivation(t *testing.T) {
	ctx := context.Background()
	cat := tools.NewCatalog()
	reg := NewRegistry()
	owner := auth.Owner{Tenant: "tenant", Agent: "agent"}
	old := &stubProvider{id: "staged"}
	if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "staged_echo", Source: "staged", Description: "old descriptor", Transport: tools.TransportMCP}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }}); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if err := reg.Register(ctx, ServerRegistration{Provider: old, Transport: "http+sse", Owner: owner}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	p := preparedFixture(t, "staged", cat, reg, owner)
	before, ok := cat.Resolve("staged_echo")
	if !ok || before.Tool.Description != "old descriptor" {
		t.Fatalf("old catalog entry changed during Prepare: ok=%v desc=%q", ok, before.Tool.Description)
	}
	old.mu.Lock()
	closedBefore := old.closed
	old.mu.Unlock()
	if closedBefore != 0 {
		t.Fatalf("old transport closed during Prepare: %d", closedBefore)
	}
	if err := p.Activate(ctx); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	after, ok := cat.Resolve("staged_echo")
	if !ok || after.Tool.Description == "old descriptor" {
		t.Fatalf("new catalog entry not activated: ok=%v desc=%q", ok, after.Tool.Description)
	}
	old.mu.Lock()
	closedAfter := old.closed
	old.mu.Unlock()
	if closedAfter != 1 {
		t.Fatalf("old transport close count after activation = %d, want 1", closedAfter)
	}
	if err := p.Close(ctx); err != nil {
		t.Fatalf("close activated provider: %v", err)
	}
}

func TestPreparedAttachment_RegistryStagesBeforeCatalogDispatchLinearization(t *testing.T) {
	ctx := context.Background()
	baseCatalog := tools.NewCatalog()
	cat := &blockingStageCatalog{
		ToolCatalog: baseCatalog,
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(cat.release) }) }
	defer release()

	reg := NewRegistry()
	owner := auth.Owner{Tenant: "tenant", Agent: "agent"}
	old := &stubProvider{id: "staged", resourceBody: []byte("old resource"), resourceMime: "text/plain"}
	oldCalls := 0
	oldInvoke := func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		oldCalls++
		return tools.ToolResult{}, nil
	}
	if err := baseCatalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "staged_echo", Source: "staged", Description: "old descriptor", Transport: tools.TransportMCP}, Invoke: oldInvoke}); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if err := reg.Register(ctx, ServerRegistration{Provider: old, Transport: "http+sse", Owner: owner}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	p := preparedFixture(t, "staged", cat, reg, owner)

	activateDone := make(chan error, 1)
	go func() { activateDone <- p.Activate(ctx) }()
	select {
	case <-cat.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Activate did not reach the catalog publication barrier")
	}

	reg.mu.RLock()
	visibleProvider := reg.servers["staged"].provider
	_, privatelyStaged := reg.pending["staged"]
	reg.mu.RUnlock()
	if visibleProvider != old || !privatelyStaged {
		t.Fatalf("catalog barrier registry state: visible=%T old=%T privately_staged=%v", visibleProvider, old, privatelyStaged)
	}
	readCtx := idCtx(t)
	body, mime, err := reg.ReadResource(readCtx, "staged", "mem://hello")
	if err != nil || string(body) != "old resource" || mime != "text/plain" {
		t.Fatalf("direct registry read crossed private stage: body=%q mime=%q err=%v", body, mime, err)
	}
	d, ok := baseCatalog.Resolve("staged_echo")
	if !ok || d.Tool.Description != "old descriptor" {
		t.Fatalf("dispatch changed before the catalog linearization point: ok=%v descriptor=%+v", ok, d.Tool)
	}
	if _, err := d.Invoke(ctx, nil); err != nil {
		t.Fatalf("old descriptor stopped dispatching during registry staging: %v", err)
	}
	if oldCalls != 1 {
		t.Fatalf("old descriptor invocation count = %d, want 1", oldCalls)
	}
	old.mu.Lock()
	closed := old.closed
	old.mu.Unlock()
	if closed != 0 {
		t.Fatalf("old provider closed before catalog publication: %d", closed)
	}

	release()
	if err := <-activateDone; err != nil {
		t.Fatalf("Activate: %v", err)
	}
	body, mime, err = reg.ReadResource(readCtx, "staged", "mem://hello")
	if err != nil || string(body) != "hello world" || mime != "text/plain" {
		t.Fatalf("direct registry read did not switch after catalog publication: body=%q mime=%q err=%v", body, mime, err)
	}
	after, ok := baseCatalog.Resolve("staged_echo")
	if !ok || after.Tool.Description == "old descriptor" {
		t.Fatalf("new descriptor absent after activation: ok=%v descriptor=%+v", ok, after.Tool)
	}
	if err := p.Close(ctx); err != nil {
		t.Fatalf("close activated provider: %v", err)
	}
}

func TestPreparedAttachment_ActivatedToolsEnterSearchIndex(t *testing.T) {
	cache := &recordingSearchCache{}
	cat := tools.NewCatalog(tools.WithSearchCache(cache))
	p := preparedFixture(t, "searchable", cat, NewRegistry(), auth.Owner{Tenant: "tenant", Agent: "agent"})
	if got := cat.Search(context.Background(), "echo", nil, 10); len(got) != 0 {
		t.Fatalf("prepared but unpublished tools entered search: %v", got)
	}
	if err := p.Activate(context.Background()); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	got := cat.Search(context.Background(), "echo", nil, 10)
	found := false
	for _, tool := range got {
		found = found || tool.Name == "searchable_echo"
	}
	if !found {
		t.Fatalf("activated tool missing from search index: %+v", got)
	}
}

func TestPreparedAttachment_PublicationRefusalLeavesExactPriorLiveState(t *testing.T) {
	ctx := context.Background()
	cat := tools.NewCatalog()
	reg := NewRegistry()
	owner := auth.Owner{Tenant: "tenant", Agent: "agent"}
	old := &stubProvider{id: "staged"}
	oldInvoke := func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }
	if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "staged_echo", Source: "staged", Description: "old", Transport: tools.TransportMCP}, Invoke: oldInvoke}); err != nil {
		t.Fatalf("seed old catalog: %v", err)
	}
	if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "staged_add", Source: "foreign", Description: "collision", Transport: tools.TransportMCP}, Invoke: oldInvoke}); err != nil {
		t.Fatalf("seed collision: %v", err)
	}
	if err := reg.Register(ctx, ServerRegistration{Provider: old, Transport: "http+sse", Owner: owner}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	reg.mu.RLock()
	priorEntry := reg.servers["staged"]
	reg.mu.RUnlock()
	p := preparedFixture(t, "staged", cat, reg, owner)
	if err := p.Activate(ctx); err == nil {
		t.Fatal("Activate succeeded despite a foreign catalog-name collision")
	}
	got, ok := cat.Resolve("staged_echo")
	if !ok || got.Tool.Description != "old" {
		t.Fatalf("failed activation changed old source: ok=%v descriptor=%+v", ok, got.Tool)
	}
	reg.mu.RLock()
	currentEntry := reg.servers["staged"]
	reg.mu.RUnlock()
	if currentEntry != priorEntry || currentEntry.provider != old {
		t.Fatal("catalog refusal did not restore the exact prior registry entry")
	}
	old.mu.Lock()
	closed := old.closed
	old.mu.Unlock()
	if closed != 0 {
		t.Fatalf("failed activation closed the prior provider %d times", closed)
	}
	_ = p.Close(ctx)
}

func TestPreparedAttachment_DisplacedCloseFailureWarnsAfterSuccessfulPublication(t *testing.T) {
	ctx := context.Background()
	cat := tools.NewCatalog()
	reg := NewRegistry()
	owner := auth.Owner{Tenant: "tenant", Agent: "agent"}
	old := &stubProvider{id: "staged", closeErr: errors.New("close refused")}
	if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "staged_echo", Source: "staged", Description: "old", Transport: tools.TransportMCP}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }}); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if err := reg.Register(ctx, ServerRegistration{Provider: old, Transport: "http+sse", Owner: owner}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	p := preparedFixture(t, "staged", cat, reg, owner)
	p.deps.Logger = logger
	if err := p.Activate(ctx); err != nil {
		t.Fatalf("post-commit cleanup failure changed activation outcome: %v", err)
	}
	got, ok := cat.Resolve("staged_echo")
	if !ok || got.Tool.Description == "old" {
		t.Fatalf("new catalog state was not committed: ok=%v descriptor=%+v", ok, got.Tool)
	}
	reg.mu.RLock()
	current := reg.servers["staged"].provider
	reg.mu.RUnlock()
	if current == old {
		t.Fatal("new registry provider was not committed")
	}
	if !bytes.Contains(logs.Bytes(), []byte("cleanup failed")) {
		t.Fatalf("displaced close failure was not warned: %s", logs.String())
	}
}
