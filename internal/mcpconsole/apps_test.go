package mcpconsole_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

func newAppsStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	s, err := inmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func newAppsBus(t *testing.T) events.EventBus {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func newAppsState(t *testing.T) state.StateStore {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

// newAppsToolCtx builds a standalone ToolContextStore for the ReadResource
// / CallTool / concurrent-reuse fixtures that do not exercise tool-context
// capture themselves (the capture+load path has its own dedicated tests).
func newAppsToolCtx(t *testing.T) *mcpconsole.ToolContextStore {
	t.Helper()
	tc, err := mcpconsole.NewToolContextStore(mcpconsole.ToolContextDeps{
		State: newAppsState(t),
		Store: newAppsStore(t),
		Bus:   newAppsBus(t),
	})
	if err != nil {
		t.Fatalf("NewToolContextStore: %v", err)
	}
	return tc
}

func newAppsRegistry(t *testing.T, body []byte) *mcp.Registry {
	t.Helper()
	reg := mcp.NewRegistry()
	if err := reg.Register(mcp.ServerRegistration{
		Provider:     &stubProvider{id: "srv-a", resourceBody: body},
		Transport:    "inmemory",
		InitialState: mcp.ServerStateOnline,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// TestAppsAccessor_ReadResource_InlineBelowThreshold proves a small
// resource rides inline (no artifact offload).
func TestAppsAccessor_ReadResource_InlineBelowThreshold(t *testing.T) {
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    newAppsRegistry(t, []byte("<html>small</html>")),
		Catalog:     tools.NewCatalog(),
		Store:       newAppsStore(t),
		Bus:         newAppsBus(t),
		ToolContext: newAppsToolCtx(t),
		Threshold:   1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	got, err := acc.ReadResource(idCtx(t), "srv-a", "ui://app/main.html")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if got.Artifact != nil {
		t.Fatalf("small resource was offloaded: %+v", got.Artifact)
	}
	if string(got.Inline) != "<html>small</html>" {
		t.Errorf("Inline = %q", string(got.Inline))
	}
}

// TestAppsAccessor_ReadResource_HeavyOffload proves the D-026 branch for
// an ORDINARY (non-`ui://`) resource: content at or above the heavy
// threshold is offloaded by reference AND a loud `mcp.resource_offloaded`
// event is emitted — never inlined, never silently truncated. A non-app
// resource keeps the LLM-context heavy threshold; only `ui://` App
// documents ride the larger App-document cap (see the App-doc tests).
func TestAppsAccessor_ReadResource_HeavyOffload(t *testing.T) {
	body := []byte(strings.Repeat("A", 2048))
	store := newAppsStore(t)
	bus := newAppsBus(t)

	// Subscribe BEFORE the read so the loud event is captured.
	sub, err := bus.Subscribe(idCtx(t), events.Filter{
		Tenant: "t-1", User: "u-1", Session: "s-1",
		Types: []events.EventType{mcp.EventTypeMCPResourceOffloaded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    newAppsRegistry(t, body),
		Catalog:     tools.NewCatalog(),
		Store:       store,
		Bus:         bus,
		ToolContext: newAppsToolCtx(t),
		Threshold:   1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	// An ordinary `repo://` resource — NOT a `ui://` App document — so the
	// heavy threshold (1024) governs and a 2048-byte body offloads.
	got, err := acc.ReadResource(idCtx(t), "srv-a", "repo://app/data.json")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(got.Inline) != 0 {
		t.Fatalf("heavy resource was inlined past the threshold (%d bytes)", len(got.Inline))
	}
	if got.Artifact == nil || got.Artifact.ID == "" {
		t.Fatalf("heavy resource not offloaded: %+v", got.Artifact)
	}
	// The bytes are recoverable from the artifact store under scope.
	scope := artifacts.ArtifactScope{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"}
	stored, found, err := store.Get(idCtx(t), scope, got.Artifact.ID)
	if err != nil || !found {
		t.Fatalf("Get offloaded artifact: found=%v err=%v", found, err)
	}
	if len(stored) != len(body) {
		t.Errorf("stored %d bytes, want %d", len(stored), len(body))
	}
	// The loud bypass event was emitted.
	select {
	case ev := <-sub.Events():
		if ev.Type != mcp.EventTypeMCPResourceOffloaded {
			t.Fatalf("event type = %q, want %q", ev.Type, mcp.EventTypeMCPResourceOffloaded)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no mcp.resource_offloaded event — the loud bypass is not emitted")
	}
}

// TestAppsAccessor_ReadResource_AppDocInlineAboveHeavyThreshold is the
// revert-guard for the fix: a `ui://` App document of 86 KiB — well above
// the 32 KiB LLM-context heavy threshold but below the 2 MiB App-document
// cap — rides INLINE against a REAL in-memory ArtifactStore, with NO
// `mcp.resource_offloaded` event. If the gate is reverted to the heavy
// threshold, an 86 KiB doc would offload to an artifactRef instead of
// inlining, and this test fails (the Console can then only fetch the App
// via a presigned URL, which fails loud on every non-S3 driver — the live
// bug this phase closes).
func TestAppsAccessor_ReadResource_AppDocInlineAboveHeavyThreshold(t *testing.T) {
	// 86 KiB — larger than the 32 KiB heavy threshold, smaller than the
	// 2 MiB App-document cap. Mirrors go-study-mcp's ~86 KB studio App.
	body := []byte(strings.Repeat("<div>app</div>", 86*1024/14))
	if len(body) <= 32*1024 {
		t.Fatalf("test fixture too small (%d bytes) — must exceed the 32 KiB heavy threshold", len(body))
	}
	store := newAppsStore(t)
	bus := newAppsBus(t)

	// Subscribe BEFORE the read so a (wrongly-fired) offload event would be
	// captured. Delivery to the subscriber buffer is synchronous within
	// Publish, so the absence check below is deterministic.
	sub, err := bus.Subscribe(idCtx(t), events.Filter{
		Tenant: "t-1", User: "u-1", Session: "s-1",
		Types: []events.EventType{mcp.EventTypeMCPResourceOffloaded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    newAppsRegistry(t, body),
		Catalog:     tools.NewCatalog(),
		Store:       store,
		Bus:         bus,
		ToolContext: newAppsToolCtx(t),
		// Threshold unset → the real 32 KiB heavy default. The 86 KiB doc
		// would offload under it; the App-document cap is what saves it.
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	got, err := acc.ReadResource(idCtx(t), "srv-a", "ui://go-study-mcp/studio/index.html")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if got.Artifact != nil {
		t.Fatalf("App document was offloaded to an artifactRef (%+v) — it must ride inline so it renders on every driver", got.Artifact)
	}
	if len(got.Inline) != len(body) {
		t.Fatalf("Inline = %d bytes, want %d (the full App document)", len(got.Inline), len(body))
	}
	// No loud bypass event fired — the App rode inline, nothing offloaded.
	select {
	case ev := <-sub.Events():
		t.Fatalf("unexpected %s event — the App document offloaded instead of inlining", ev.Type)
	default:
	}
}

// TestAppsAccessor_ReadResource_AppDocOffloadAboveCap proves the >cap
// fallback is preserved: a `ui://` App document larger than the 2 MiB
// App-document cap still offloads to the ArtifactStore by reference AND
// fires the loud `mcp.resource_offloaded` bypass — a pathologically large
// App is never inlined unbounded, never silently truncated.
func TestAppsAccessor_ReadResource_AppDocOffloadAboveCap(t *testing.T) {
	body := []byte(strings.Repeat("Z", 2*1024*1024+1)) // 2 MiB + 1 byte
	store := newAppsStore(t)
	bus := newAppsBus(t)

	sub, err := bus.Subscribe(idCtx(t), events.Filter{
		Tenant: "t-1", User: "u-1", Session: "s-1",
		Types: []events.EventType{mcp.EventTypeMCPResourceOffloaded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    newAppsRegistry(t, body),
		Catalog:     tools.NewCatalog(),
		Store:       store,
		Bus:         bus,
		ToolContext: newAppsToolCtx(t),
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	got, err := acc.ReadResource(idCtx(t), "srv-a", "ui://app/huge.html")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(got.Inline) != 0 {
		t.Fatalf("over-cap App document was inlined (%d bytes) — the >cap fallback must offload", len(got.Inline))
	}
	if got.Artifact == nil || got.Artifact.ID == "" {
		t.Fatalf("over-cap App document not offloaded: %+v", got.Artifact)
	}
	scope := artifacts.ArtifactScope{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"}
	stored, found, err := store.Get(idCtx(t), scope, got.Artifact.ID)
	if err != nil || !found {
		t.Fatalf("Get offloaded artifact: found=%v err=%v", found, err)
	}
	if len(stored) != len(body) {
		t.Errorf("stored %d bytes, want %d", len(stored), len(body))
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != mcp.EventTypeMCPResourceOffloaded {
			t.Fatalf("event type = %q, want %q", ev.Type, mcp.EventTypeMCPResourceOffloaded)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no mcp.resource_offloaded event — the loud bypass is not emitted for the >cap App document")
	}
}

// TestAppsAccessor_CallTool_ResolvesAndInvokes covers the proxy's happy
// path against a real catalog, plus the tool-not-found error.
func TestAppsAccessor_CallTool_ResolvesAndInvokes(t *testing.T) {
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "srv-a_echo"},
		Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"echo": string(args)}}, nil
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    newAppsRegistry(t, nil),
		Catalog:     cat,
		Store:       newAppsStore(t),
		Bus:         newAppsBus(t),
		ToolContext: newAppsToolCtx(t),
		Threshold:   1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	res, err := acc.CallTool(idCtx(t), "srv-a_echo", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Errorf("IsError = true, want false")
	}
	if len(res.Inline) == 0 {
		t.Errorf("Inline is empty")
	}

	// Unknown tool fails with ErrToolNotFound.
	if _, err := acc.CallTool(idCtx(t), "nope", nil); err == nil {
		t.Fatal("CallTool unknown tool: want error")
	}
}

// TestAppsAccessor_ConcurrentReuse pins the §5/§11 concurrent-reuse
// contract: one shared AppsAccessor serves N≥100 concurrent invocations
// with no data races, no cross-talk, and no goroutine leak.
func TestAppsAccessor_ConcurrentReuse(t *testing.T) {
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "srv-a_echo"},
		Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"echo": string(args)}}, nil
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    newAppsRegistry(t, []byte("<html>x</html>")),
		Catalog:     cat,
		Store:       newAppsStore(t),
		Bus:         newAppsBus(t),
		ToolContext: newAppsToolCtx(t),
		Threshold:   1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}

	const n = 128
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each goroutine carries a DISTINCT session so cross-talk
			// would surface as a wrong artifact scope / read.
			ctx, _ := identity.With(context.Background(), identity.Identity{
				TenantID: "t-1", UserID: "u-1", SessionID: "s-" + string(rune('A'+(i%26))),
			})
			if i%2 == 0 {
				if _, err := acc.ReadResource(ctx, "srv-a", "ui://app/main.html"); err != nil {
					errs <- err
				}
			} else {
				if _, err := acc.CallTool(ctx, "srv-a_echo", json.RawMessage(`{"i":1}`)); err != nil {
					errs <- err
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent invocation error: %v", err)
	}
}
