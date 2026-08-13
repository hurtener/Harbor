package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/tools"
)

// The HA-56 attach partition gate: ONE discovered snapshot must rebuild
// BOTH views. The ordinary planner/model catalog receives only the
// non-app-only descriptors; the app-only callbacks ride the registry's
// per-server App dispatch catalog. The two attach legs below exercise the
// REAL wire transports the ask names — HTTP (SSE) and stdio — with the
// same fixture server (one ordinary tool, one `["app"]` callback, one
// tool visible to both).

// assertAttachPartition is the shared assertion body both transport legs
// run: the ordinary view (generic resolve + planner view + tools/list
// filter + search cache) excludes the app-only callback while the App
// dispatch view resolves it ONLY through its own server.
func assertAttachPartition(t *testing.T, cat tools.ToolCatalog, reg *Registry, serverID string) {
	t.Helper()

	// --- Ordinary planner/model projection ---
	if _, ok := cat.Resolve(serverID + "_plain"); !ok {
		t.Fatal("ordinary tool missing from the planner/model catalog")
	}
	if _, ok := cat.Resolve(serverID + "_both"); !ok {
		t.Fatal("both-visible tool missing from the planner/model catalog")
	}
	if _, ok := cat.Resolve(serverID + "_callback"); ok {
		t.Fatal("app-only callback leaked into the ordinary planner/model catalog — a generic caller could invoke it")
	}

	// Planner context: List + Resolve both exclude the callback.
	filter := tools.CatalogFilter{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"}
	view := tools.NewPlannerView(cat, filter)
	if _, ok := view.Resolve(serverID + "_callback"); ok {
		t.Fatal("planner Resolve found the app-only callback")
	}
	for _, tl := range view.List() {
		if tl.Name == serverID+"_callback" {
			t.Fatal("planner List contains the app-only callback")
		}
	}

	// Generic tools/list projection (the full-discovery view): same
	// exclusion — app-only callbacks are not advertised as model tools.
	listed := make(map[string]bool)
	for _, tl := range cat.List(tools.CatalogFilter{
		TenantID: "t-1", UserID: "u-1", SessionID: "s-1",
		LoadingModes: []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred},
	}) {
		listed[tl.Name] = true
	}
	if listed[serverID+"_callback"] {
		t.Fatal("generic tools/list contains the app-only callback")
	}
	if !listed[serverID+"_plain"] || !listed[serverID+"_both"] {
		t.Fatal("ordinary / both-visible tools missing from generic tools/list")
	}

	// --- App dispatch view (the SAME discovered snapshot) ---
	if _, ok := reg.ResolveAppTool(serverID, serverID+"_callback"); !ok {
		t.Fatal("app-only callback did not resolve through its own server's App dispatch catalog")
	}
	if _, ok := reg.ResolveAppTool(serverID, serverID+"_plain"); ok {
		t.Fatal("ordinary tool resolved as an app-only callback")
	}
	if _, ok := reg.ResolveAppTool("other-server", serverID+"_callback"); ok {
		t.Fatal("another server's App dispatch catalog resolved a foreign callback")
	}
}

// TestAttach_PartitionsAppOnlyOutOfPlannerCatalog_SSEHTTP drives the real
// HTTP (SSE) attach path — dial, handshake, discovery, catalog + registry
// publication — against the fixture server and asserts the partition.
func TestAttach_PartitionsAppOnlyOutOfPlannerCatalog_SSEHTTP(t *testing.T) {
	handler := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server {
		return appCatalogFixtureServer()
	}, nil)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	cat := tools.NewCatalog()
	reg := NewRegistry()
	closers := []func(context.Context) error{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	prepared, err := Prepare(ctx, config.MCPServerConfig{
		Name: "fixture-http", TransportMode: string(TransportSSE), URL: server.URL,
	}, AttachDeps{
		Catalog: cat, Registry: reg, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), Closers: &closers,
	})
	if err != nil {
		t.Fatalf("Prepare(SSE): %v", err)
	}
	if err := prepared.Activate(ctx); err != nil {
		t.Fatalf("Activate(SSE): %v", err)
	}
	t.Cleanup(func() { _ = prepared.Close(context.Background()) })

	assertAttachPartition(t, cat, reg, "fixture-http")
}

// TestAttach_PartitionsAppOnly_StaleEntryRemovedOnReplacement proves a
// same-name replacement (re-attach) rebuilds BOTH views from the NEW
// discovered snapshot over the real SSE wire: the first generation's
// app-only callback stops resolving in the App dispatch view AND never
// entered the ordinary catalog, while the replacement generation's fresh
// callback becomes usable only through its own server.
func TestAttach_PartitionsAppOnly_StaleEntryRemovedOnReplacement(t *testing.T) {
	var mu sync.Mutex
	secondGen := false
	handler := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server {
		mu.Lock()
		defer mu.Unlock()
		if secondGen {
			return appCatalogFixtureServerNamed("callback-v2", "ui://app/callback-v2.html")
		}
		return appCatalogFixtureServer()
	}, nil)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cat := tools.NewCatalog()
	reg := NewRegistry()
	closers := []func(context.Context) error{}

	attach := func() {
		t.Helper()
		prepared, err := Prepare(ctx, config.MCPServerConfig{
			Name: "fixture-repl", TransportMode: string(TransportSSE), URL: server.URL,
		}, AttachDeps{
			Catalog: cat, Registry: reg, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), Closers: &closers,
		})
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if err := prepared.Activate(ctx); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		closers = append(closers, prepared.Close)
	}

	// Generation 1: callback (app-only), plain + both (ordinary).
	attach()
	if _, ok := cat.Resolve("fixture-repl_callback"); ok {
		t.Fatal("generation-1 app-only callback leaked into the ordinary catalog")
	}
	if _, ok := reg.ResolveAppTool("fixture-repl", "fixture-repl_callback"); !ok {
		t.Fatal("generation-1 callback missing from the App dispatch view")
	}

	// Generation 2: the server now publishes callback-v2 (app-only) and
	// has DROPPED callback. The same-name re-attach must swap both views.
	mu.Lock()
	secondGen = true
	mu.Unlock()
	attach()

	// Ordinary view: neither generation's callback is a model tool.
	if _, ok := cat.Resolve("fixture-repl_callback"); ok {
		t.Fatal("stale generation-1 callback leaked into the ordinary catalog after replacement")
	}
	if _, ok := cat.Resolve("fixture-repl_callback-v2"); ok {
		t.Fatal("generation-2 app-only callback leaked into the ordinary catalog")
	}
	if _, ok := cat.Resolve("fixture-repl_plain"); !ok {
		t.Fatal("ordinary tool missing after replacement")
	}

	// App dispatch view: the removed callback stops resolving everywhere;
	// the fresh one is usable only through its own server.
	if _, ok := reg.ResolveAppTool("fixture-repl", "fixture-repl_callback"); ok {
		t.Fatal("stale generation-1 callback survived the replacement — stale App dispatch entry")
	}
	if _, ok := reg.ResolveAppTool("fixture-repl", "fixture-repl_callback-v2"); !ok {
		t.Fatal("generation-2 callback did not become usable through its own server after replacement")
	}
	if _, ok := reg.ResolveAppTool("other-server", "fixture-repl_callback-v2"); ok {
		t.Fatal("generation-2 callback leaked into another server's App dispatch view")
	}
}

// TestMCPAppCatalogStdioServerHelper is NOT a test — it is the re-exec'd
// stdio MCP server process the stdio attach test spawns (the same
// self-spawn pattern the TUI PTY integration test uses). Guarded by an
// env var so an ordinary test run never executes the server body. The
// server writes ONLY the MCP protocol to stdout; the parent kills the
// process at teardown.
func TestMCPAppCatalogStdioServerHelper(t *testing.T) {
	if os.Getenv("HARBOR_MCP_APP_CATALOG_STDIO_HELPER") != "1" {
		return
	}
	srv := appCatalogFixtureServer()
	if err := srv.Run(context.Background(), &mcpsdk.StdioTransport{}); err != nil {
		t.Fatalf("stdio fixture server: %v", err)
	}
}

// TestAttach_PartitionsAppOnly_Stdio drives the real stdio attach path
// (subprocess spawn, handshake over stdin/stdout, discovery, catalog +
// registry publication) against the fixture server re-exec'd from the test
// binary itself — no external fixture binary, CI-safe.
func TestAttach_PartitionsAppOnly_Stdio(t *testing.T) {
	t.Setenv("HARBOR_MCP_APP_CATALOG_STDIO_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	cat := tools.NewCatalog()
	reg := NewRegistry()
	closers := []func(context.Context) error{}
	prepared, err := Prepare(ctx, config.MCPServerConfig{
		Name: "fixture-stdio", TransportMode: string(TransportStdio),
		Command: []string{os.Args[0], "-test.run=^TestMCPAppCatalogStdioServerHelper$", "-test.v=false"},
	}, AttachDeps{
		Catalog: cat, Registry: reg, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), Closers: &closers,
	})
	if err != nil {
		t.Fatalf("Prepare(stdio): %v", err)
	}
	if err := prepared.Activate(ctx); err != nil {
		t.Fatalf("Activate(stdio): %v", err)
	}
	t.Cleanup(func() { _ = prepared.Close(context.Background()) })

	assertAttachPartition(t, cat, reg, "fixture-stdio")
}
