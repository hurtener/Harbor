package mcp

import (
	"context"
	"encoding/json"
	"errors"
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

// attach_reattach_test.go — coverage for the live-layer idempotent MCP
// re-attach (D-339): a same-name attach against a still-live registration is
// an atomic upsert (deregister the old server's catalog tools + close its
// transport, then register the new connection) instead of a blind register
// that collides on a duplicate tool name. It also pins the compounding
// registry fix: Registry.Register's same-name path closes the displaced
// provider's transport instead of leaking it.

// TestRegistry_Register_SameName_ClosesReplacedTransport pins the registry
// leak fix: re-registering a name that is already live replaces the entry AND
// closes the displaced provider's transport (never a bare map overwrite that
// drops the old session). MUTATION: revert Register's close-on-replace to a
// bare `r.servers[name] = ...` and this fails (old.closed stays 0).
func TestRegistry_Register_SameName_ClosesReplacedTransport(t *testing.T) {
	r := NewRegistry()
	old := &stubProvider{id: "srv", toolNames: []string{"t"}}
	fresh := &stubProvider{id: "srv", toolNames: []string{"t2"}}

	if err := r.Register(idCtx(t), ServerRegistration{Provider: old, Transport: "stdio", InitialState: ServerStateOnline}); err != nil {
		t.Fatalf("Register old: %v", err)
	}
	if err := r.Register(idCtx(t), ServerRegistration{Provider: fresh, Transport: "stdio", InitialState: ServerStateOnline}); err != nil {
		t.Fatalf("Register fresh (same name): %v", err)
	}

	// The displaced provider's transport was closed exactly once.
	old.mu.Lock()
	oldClosed := old.closed
	old.mu.Unlock()
	if oldClosed != 1 {
		t.Fatalf("displaced provider Close called %d times, want 1 (transport leak if 0)", oldClosed)
	}
	// The fresh provider is untouched (it is the live one now).
	fresh.mu.Lock()
	freshClosed := fresh.closed
	fresh.mu.Unlock()
	if freshClosed != 0 {
		t.Fatalf("fresh provider Close called %d times, want 0", freshClosed)
	}
	// Exactly one entry survives, backed by the fresh provider.
	if ids := r.SourceIDs(); len(ids) != 1 || ids[0] != "srv" {
		t.Fatalf("SourceIDs = %v, want [srv]", ids)
	}
}

// TestRegistry_Register_SameProvider_DoesNotCloseItself guards the idempotent
// re-register: re-registering the SAME provider instance under its own name
// must not close the transport we just re-registered.
func TestRegistry_Register_SameProvider_DoesNotCloseItself(t *testing.T) {
	r := NewRegistry()
	p := &stubProvider{id: "srv", toolNames: []string{"t"}}
	if err := r.Register(idCtx(t), ServerRegistration{Provider: p, Transport: "stdio", InitialState: ServerStateOnline}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Register(idCtx(t), ServerRegistration{Provider: p, Transport: "stdio", InitialState: ServerStateOnline}); err != nil {
		t.Fatalf("re-Register same provider: %v", err)
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed != 0 {
		t.Fatalf("re-registering the same provider closed its transport %d times, want 0", closed)
	}
}

// TestRegistry_Register_SameName_PropagatesReplacedCloseError proves the leak
// fix fails LOUD, never silent: a displaced provider whose Close errors
// surfaces the error rather than swallowing it (the entry is still swapped).
func TestRegistry_Register_SameName_PropagatesReplacedCloseError(t *testing.T) {
	r := NewRegistry()
	old := &stubProvider{id: "srv", closeErr: errors.New("close boom")}
	fresh := &stubProvider{id: "srv", toolNames: []string{"t"}}
	if err := r.Register(idCtx(t), ServerRegistration{Provider: old, Transport: "stdio", InitialState: ServerStateOnline}); err != nil {
		t.Fatalf("Register old: %v", err)
	}
	err := r.Register(idCtx(t), ServerRegistration{Provider: fresh, Transport: "stdio", InitialState: ServerStateOnline})
	if err == nil {
		t.Fatal("want the displaced provider's close error surfaced, got nil")
	}
	// The swap still happened (the fresh provider is live).
	if _, getErr := r.GetServer(idCtx(t), "srv"); getErr != nil {
		t.Fatalf("entry not swapped despite close error: %v", getErr)
	}
}

// TestAttach_ReAttach_ReplacesLiveRegistration is the core D-339 gate: a
// same-name attach against a still-live registration succeeds as an atomic
// upsert — the old server's catalog tools are deregistered AND its transport
// closed, then the new connection's tools are registered — instead of failing
// on a duplicate tool name.
//
// MUTATION: revert the same-name replace in attach.go (blind register loop)
// and this fails — catalog.Register collides on the pre-seeded reattach_echo
// (ErrToolDuplicateName) so Attach returns an error at the "must succeed" gate.
func TestAttach_ReAttach_ReplacesLiveRegistration(t *testing.T) {
	mockSrv := newMockServer()
	sseHandler := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server {
		return mockSrv.server
	}, nil)
	sseServer := httptest.NewServer(sseHandler)
	defer sseServer.Close()

	const name = "reattach"
	cat := tools.NewCatalog()
	reg := NewRegistry()
	owner := auth.Owner{Tenant: "tenant-A", Agent: "agent-A"}

	// Pre-seed a still-LIVE same-name registration OWNED BY the re-attaching
	// caller: an old provider in the registry (its deferred detach has not run)
	// plus its catalog tool under the exact name the new server discovers
	// (reattach_echo). This reproduces the duplicate-tool-name collision the
	// blind register loop hit, for a same-owner supersede.
	oldProv := &stubProvider{id: tools.ToolSourceID(name), toolNames: []string{"echo"}}
	if err := reg.Register(idCtx(t), ServerRegistration{Provider: oldProv, Transport: "stdio", InitialState: ServerStateOnline, Owner: owner}); err != nil {
		t.Fatalf("pre-seed registry: %v", err)
	}
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:        name + "_echo",
			Description: "OLD",
			Transport:   tools.TransportInProcess,
			Source:      tools.ToolSourceID(name),
		},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatalf("pre-seed catalog: %v", err)
	}

	closers := []func(context.Context) error{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Attach(ctx, config.MCPServerConfig{
		Name:          name,
		TransportMode: string(TransportSSE),
		URL:           sseServer.URL,
	}, AttachDeps{
		Catalog:         cat,
		Registry:        reg,
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
		Closers:         &closers,
		Owner:           owner,
	}); err != nil {
		t.Fatalf("re-attach must succeed (idempotent upsert), got: %v", err)
	}
	defer func() {
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i](context.Background())
		}
	}()

	// The OLD transport was closed exactly once by the registry deregister leg.
	oldProv.mu.Lock()
	oldClosed := oldProv.closed
	oldProv.mu.Unlock()
	if oldClosed != 1 {
		t.Fatalf("old transport Close called %d times, want 1 (leak if 0)", oldClosed)
	}

	// The NEW tool set is live: reattach_echo now resolves to the MCP-transport
	// descriptor from the real server (not the pre-seeded in-process placeholder).
	d, ok := cat.Resolve(name + "_echo")
	if !ok {
		t.Fatal("catalog missing the re-attached tool reattach_echo")
	}
	if d.Tool.Transport != tools.TransportMCP {
		t.Fatalf("resolved tool is not the new MCP descriptor: transport=%q description=%q", d.Tool.Transport, d.Tool.Description)
	}

	// The registry holds exactly the new provider, online with real tool count.
	servers, _, lerr := reg.ListServers(idCtx(t), ListFilter{})
	if lerr != nil {
		t.Fatalf("ListServers: %v", lerr)
	}
	if len(servers) != 1 {
		t.Fatalf("want exactly 1 registered server after re-attach, got %d", len(servers))
	}
	if servers[0].State != ServerStateOnline || servers[0].ToolCount == 0 {
		t.Fatalf("re-attached server not online with tools: %+v", servers[0])
	}
}

// TestAttach_ReAttach_CrossOwner_RejectedPreservesLiveRegistration is the
// multi-isolation gate: a same-name attach by a DIFFERENT owner must NOT tear
// down the live owner's tools/transport. Owner A has a live "github"
// registration; owner B's same-name attach is rejected loud
// (ErrConnectionNameOwnerConflict) and A's transport stays open, A's tool stays
// resolvable, A's registration stays put.
//
// MUTATION: drop the `priorOwner != deps.Owner` reject in attach.go (make the
// replace owner-blind) and this fails — B evicts A (A's transport closes, A's
// tool is deregistered).
func TestAttach_ReAttach_CrossOwner_RejectedPreservesLiveRegistration(t *testing.T) {
	const name = "github"
	cat := tools.NewCatalog()
	reg := NewRegistry()
	ownerA := auth.Owner{Tenant: "tenant-A", Agent: "agent-A"}
	ownerB := auth.Owner{Tenant: "tenant-B", Agent: "agent-B"}

	// Owner A's still-live registration + its catalog tool.
	provA := &stubProvider{id: tools.ToolSourceID(name), toolNames: []string{"echo"}}
	if err := reg.Register(idCtx(t), ServerRegistration{Provider: provA, Transport: "stdio", InitialState: ServerStateOnline, Owner: ownerA}); err != nil {
		t.Fatalf("pre-seed owner A registry: %v", err)
	}
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:      name + "_echo",
			Transport: tools.TransportInProcess,
			Source:    tools.ToolSourceID(name),
		},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatalf("pre-seed owner A catalog: %v", err)
	}

	// Owner B attaches the SAME name. The owner check fires BEFORE Connect, so
	// the URL never needs to be reachable — the reject is synchronous.
	closers := []func(context.Context) error{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Attach(ctx, config.MCPServerConfig{
		Name:          name,
		TransportMode: string(TransportStreamableHTTP),
		URL:           "http://127.0.0.1:1/mcp", // never dialled — rejected first
	}, AttachDeps{
		Catalog:         cat,
		Registry:        reg,
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
		Closers:         &closers,
		Owner:           ownerB,
	})
	if !errors.Is(err, ErrConnectionNameOwnerConflict) {
		t.Fatalf("cross-owner same-name attach must be rejected with ErrConnectionNameOwnerConflict, got: %v", err)
	}

	// Owner A's transport was NOT closed (no cross-owner eviction).
	provA.mu.Lock()
	aClosed := provA.closed
	provA.mu.Unlock()
	if aClosed != 0 {
		t.Fatalf("owner A's transport was closed %d times by a cross-owner attach (eviction/DoS!), want 0", aClosed)
	}
	// Owner A's tool is still resolvable (still the in-process placeholder).
	d, ok := cat.Resolve(name + "_echo")
	if !ok {
		t.Fatal("owner A's tool was evicted from the catalog by a cross-owner attach")
	}
	if d.Tool.Transport != tools.TransportInProcess {
		t.Fatalf("owner A's tool was replaced by a cross-owner attach: transport=%q", d.Tool.Transport)
	}
	// Owner A's registration stays put, still owned by A.
	if got, exists := reg.OwnerOf(name); !exists || got != ownerA {
		t.Fatalf("owner A's registration was disturbed: exists=%v owner=%+v", exists, got)
	}
	// No closer was left behind (the reject happened before Connect).
	if len(closers) != 0 {
		t.Fatalf("a cross-owner reject must not leave a closer, got %d", len(closers))
	}
}

// TestAttach_FirstAttach_NoPrior_Unaffected proves a first attach (no live
// same-name registration) is behaviorally unchanged: the deregister legs no-op
// (DeregisterSource removes 0, Registry.Deregister returns ErrServerNotFound,
// both swallowed) and the attach lands the tools + registration normally.
func TestAttach_FirstAttach_NoPrior_Unaffected(t *testing.T) {
	mockSrv := newMockServer()
	sseHandler := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server {
		return mockSrv.server
	}, nil)
	sseServer := httptest.NewServer(sseHandler)
	defer sseServer.Close()

	cat := tools.NewCatalog()
	reg := NewRegistry()
	closers := []func(context.Context) error{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Attach(ctx, config.MCPServerConfig{
		Name:          "fresh",
		TransportMode: string(TransportSSE),
		URL:           sseServer.URL,
	}, AttachDeps{
		Catalog:         cat,
		Registry:        reg,
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
		Closers:         &closers,
	}); err != nil {
		t.Fatalf("first attach must succeed with no prior registration, got: %v", err)
	}
	defer func() {
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i](context.Background())
		}
	}()
	if _, ok := cat.Resolve("fresh_echo"); !ok {
		t.Fatal("first attach did not land the discovered tool fresh_echo")
	}
	if ids := reg.SourceIDs(); len(ids) != 1 || ids[0] != "fresh" {
		t.Fatalf("SourceIDs = %v, want [fresh]", ids)
	}
}

// TestReAttach_ConcurrentReuse is the D-025 concurrent-reuse gate: N≥100
// interleaved same-owner same-name attach/re-attach against ONE shared Registry
// + Catalog, serialised by a mutex that mirrors the production attacher's
// serialise-the-whole-attach lock. mcpdrv.Attach constructs a real transport
// internally, so this test drives the same-owner replace LEGS it runs — the
// teardown pair (DeregisterSource + Registry.Deregister) followed by the
// register pair (catalog tool + Registry.Register), in that production order —
// against stubs, giving deterministic close accounting the real-transport path
// cannot. It is a mirror of those legs, not the attach code path itself.
//
// Guarantees asserted under -race: no data race; no duplicate-registration
// error (the replace makes every same-name register a clean upsert); no leaked
// transport (every displaced provider is closed exactly once — only the sole
// survivor stays open); no cross-talk (exactly one entry survives).
func TestReAttach_ConcurrentReuse(t *testing.T) {
	const name = "srv"
	const n = 200

	cat := tools.NewCatalog()
	dereg, ok := cat.(tools.CatalogSourceDeregisterer)
	if !ok {
		t.Fatal("catalog does not implement CatalogSourceDeregisterer")
	}
	reg := NewRegistry()

	// Distinct provider instance per iteration, all sharing the one name.
	provs := make([]*stubProvider, n)
	for i := range provs {
		provs[i] = &stubProvider{id: tools.ToolSourceID(name), toolNames: []string{"echo"}}
	}

	// attachMu mirrors the production attacher's mutex: the whole same-name
	// replace runs inside one critical section so two adds cannot race the
	// catalog/registry (the attacher holds this for the entire Attach; here we
	// hold it for the equivalent live-layer legs).
	var attachMu sync.Mutex
	replace := func(ctx context.Context, p *stubProvider) error {
		attachMu.Lock()
		defer attachMu.Unlock()
		// Teardown pair FIRST (production order): deregister the old server's
		// catalog tools (idempotent — 0 on a first attach) then deregister +
		// close the old transport (swallow ErrServerNotFound).
		dereg.DeregisterSource(tools.ToolSourceID(name))
		if derr := reg.Deregister(ctx, name, auth.Owner{}); derr != nil && !errors.Is(derr, ErrServerNotFound) {
			return derr
		}
		// Register pair: the new source's catalog tool, then the new provider.
		// A dropped teardown leg would surface here as a duplicate-tool-name.
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{
				Name:      name + "_echo",
				Transport: tools.TransportMCP,
				Source:    tools.ToolSourceID(name),
			},
			Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{}, nil
			},
		}); err != nil {
			return err
		}
		return reg.Register(ctx, ServerRegistration{Provider: p, Transport: "stdio", InitialState: ServerStateOnline})
	}

	var wg sync.WaitGroup
	errs := make(chan error, n)
	ctx := idCtx(t)
	for i := range provs {
		wg.Add(1)
		go func(p *stubProvider) {
			defer wg.Done()
			if err := replace(ctx, p); err != nil {
				errs <- err
			}
		}(provs[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent same-name replace errored (duplicate registration?): %v", err)
	}

	// Exactly one entry survives — no cross-talk.
	if ids := reg.SourceIDs(); len(ids) != 1 || ids[0] != name {
		t.Fatalf("SourceIDs = %v, want exactly [%s]", ids, name)
	}
	// No leaked transport: every displaced provider closed exactly once, the
	// sole survivor stays open. Total closed == n-1.
	var totalClosed, survivors int
	for _, p := range provs {
		p.mu.Lock()
		c := p.closed
		p.mu.Unlock()
		if c > 1 {
			t.Fatalf("a provider was closed %d times (double-close)", c)
		}
		if c == 1 {
			totalClosed++
		} else {
			survivors++
		}
	}
	if totalClosed != n-1 {
		t.Fatalf("displaced providers closed = %d, want %d (a leak if fewer closed)", totalClosed, n-1)
	}
	if survivors != 1 {
		t.Fatalf("open (survivor) providers = %d, want 1", survivors)
	}
}

// TestRegistry_Register_SameName_ConcurrentReuse stresses the registry
// close-on-replace leg directly (the dev hot-reload path): N≥100 same-name
// re-registers against one shared Registry under -race. Asserts no race, no
// double-close, exactly one survivor, every displaced transport closed once.
func TestRegistry_Register_SameName_ConcurrentReuse(t *testing.T) {
	const name = "srv"
	const n = 200
	reg := NewRegistry()
	provs := make([]*stubProvider, n)
	for i := range provs {
		provs[i] = &stubProvider{id: tools.ToolSourceID(name), toolNames: []string{"t"}}
	}
	// A mutex serialises the register calls so the "which provider was
	// displaced" accounting is deterministic (the Registry is internally
	// locked, but two concurrent Registers could otherwise both read a nil
	// prior on a truly empty map — here the map is never empty after the first,
	// so serialising keeps close-count == n-1 exact for the assertion).
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, n)
	ctx := idCtx(t)
	for i := range provs {
		wg.Add(1)
		go func(p *stubProvider) {
			defer wg.Done()
			mu.Lock()
			err := reg.Register(ctx, ServerRegistration{Provider: p, Transport: "stdio", InitialState: ServerStateOnline})
			mu.Unlock()
			if err != nil {
				errs <- err
			}
		}(provs[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent same-name Register errored: %v", err)
	}
	if ids := reg.SourceIDs(); len(ids) != 1 {
		t.Fatalf("SourceIDs len = %d, want 1", len(ids))
	}
	var totalClosed int
	for _, p := range provs {
		p.mu.Lock()
		c := p.closed
		p.mu.Unlock()
		if c > 1 {
			t.Fatalf("provider closed %d times (double-close)", c)
		}
		totalClosed += c
	}
	if totalClosed != n-1 {
		t.Fatalf("displaced transports closed = %d, want %d", totalClosed, n-1)
	}
}
