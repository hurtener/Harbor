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
	"github.com/hurtener/Harbor/internal/identity"
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
		DescriptorFingerprint: "fingerprint-" + name,
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

func TestPreparedAttachment_PostPublicationAdmissionErrorRetainsLiveGeneration(t *testing.T) {
	ctx := context.Background()
	cat := tools.NewCatalog()
	reg := NewRegistry()
	p := preparedFixture(t, "post-publish", cat, reg, auth.Owner{Tenant: "tenant", Agent: "agent"})
	var logs bytes.Buffer
	p.deps.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	originalClose := p.closeFn
	closeCalls := 0
	p.closeFn = func(closeCtx context.Context) error {
		closeCalls++
		return originalClose(closeCtx)
	}
	releaseErr := errors.New("injected fence transaction release error")
	if err := p.ActivateUnder(ctx, func(_ context.Context, publish func() error) error {
		if err := publish(); err != nil {
			return err
		}
		return releaseErr
	}); err != nil {
		t.Fatalf("ActivateUnder after irreversible publication = %v, want success", err)
	}
	descriptor, ok := cat.Resolve("post-publish_echo")
	if !ok {
		t.Fatal("post-publication admission error withdrew the live catalog generation")
	}
	invokeCtx, err := identity.With(ctx, defaultIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := descriptor.Invoke(invokeCtx, json.RawMessage(`{"text":"still live"}`)); err != nil {
		t.Fatalf("live descriptor invoke after admission error: %v", err)
	}
	if _, _, ok := reg.RegistrationIdentity("post-publish"); !ok {
		t.Fatal("post-publication admission error withdrew the live registry handle")
	}
	if !p.activated || !bytes.Contains(logs.Bytes(), []byte(releaseErr.Error())) {
		t.Fatalf("post-publication ambiguity was not retained and warned: activated=%t logs=%s", p.activated, logs.String())
	}
	if err := p.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if closeCalls != 1 {
		t.Fatalf("activated provider close calls = %d, want one", closeCalls)
	}
}

func TestPreparedAttachment_AuthorityLostBeforeReservationNeverPublishes(t *testing.T) {
	cat := tools.NewCatalog()
	reg := NewRegistry()
	owner := auth.Owner{Tenant: "tenant", Agent: "agent"}
	p := preparedFixture(t, "lost-before-stage", cat, reg, owner)
	authorityLost := errors.New("durable pair was removed before final proof")

	// The durable removal completed after network preparation but before this
	// process could establish/finalize its reservation. ActivateIf may stage
	// privately, but the final proof refuses and rollback removes that stage.
	if err := p.ActivateIf(context.Background(), func(context.Context) error { return authorityLost }); !errors.Is(err, authorityLost) {
		t.Fatalf("ActivateIf = %v, want authority loss", err)
	}
	if _, ok := cat.Resolve("lost-before-stage_echo"); ok {
		t.Fatal("authority-lost preparation became dispatchable")
	}
	if pending, live, closing := reg.reservationState("lost-before-stage"); pending || live || closing {
		t.Fatalf("authority-lost reservation leaked: pending=%t live=%t closing=%t", pending, live, closing)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("close refused preparation: %v", err)
	}
}

func TestPreparedAttachment_ExactRemovalAfterReservationInvalidatesPublication(t *testing.T) {
	cat := tools.NewCatalog()
	reg := NewRegistry()
	owner := auth.Owner{Tenant: "tenant", Agent: "agent"}
	p := preparedFixture(t, "removed-while-staged", cat, reg, owner)
	proofEntered := make(chan struct{})
	proofRelease := make(chan struct{})
	activateDone := make(chan error, 1)
	go func() {
		activateDone <- p.ActivateIf(context.Background(), func(context.Context) error {
			close(proofEntered)
			<-proofRelease
			return nil
		})
	}()
	select {
	case <-proofEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("ActivateIf never reached the post-reservation authority proof")
	}
	if pending, live, closing := reg.reservationState("removed-while-staged"); !pending || live || closing {
		t.Fatalf("proof barrier reservation = pending=%t live=%t closing=%t, want true/false/false", pending, live, closing)
	}
	withdrawals := 0
	removed, err := reg.DeregisterExact(context.Background(), "removed-while-staged", owner, "fingerprint-removed-while-staged", func() int {
		withdrawals++
		return cat.(tools.CatalogSourceDeregisterer).DeregisterSource("removed-while-staged")
	})
	if err != nil || removed != 0 || withdrawals != 0 {
		t.Fatalf("remove exact private stage = removed=%d withdrawals=%d err=%v, want 0/0/nil", removed, withdrawals, err)
	}
	close(proofRelease)
	if err := <-activateDone; err == nil {
		t.Fatal("publication succeeded after exact teardown invalidated its reservation")
	}
	if _, ok := cat.Resolve("removed-while-staged_echo"); ok {
		t.Fatal("invalidated staged provider became dispatchable")
	}
	if pending, live, closing := reg.reservationState("removed-while-staged"); pending || live || closing {
		t.Fatalf("invalidated reservation leaked: pending=%t live=%t closing=%t", pending, live, closing)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("close invalidated preparation: %v", err)
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

	// Exact teardown starts after the authority proof but while publication is
	// at the catalog linearization point. It must wait for the registry-locked
	// publication and then observe/close the newly live exact handle; it can
	// never receipt absence in the catalog-only interval.
	detachDone := make(chan error, 1)
	go func() {
		_, err := reg.DeregisterExact(ctx, "staged", owner, "fingerprint-staged", func() int {
			return baseCatalog.(tools.CatalogSourceDeregisterer).DeregisterSource("staged")
		})
		detachDone <- err
	}()
	select {
	case err := <-detachDone:
		t.Fatalf("exact teardown returned during atomic publication: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	release()
	if err := <-activateDone; err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := <-detachDone; err != nil {
		t.Fatalf("exact teardown after publication: %v", err)
	}
	if _, ok := baseCatalog.Resolve("staged_echo"); ok {
		t.Fatal("exact teardown left the just-published catalog source dispatchable")
	}
	if _, _, ok := reg.RegistrationIdentity("staged"); ok {
		t.Fatal("exact teardown failed to remove the just-published registry handle")
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
