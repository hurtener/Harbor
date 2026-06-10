// attach_test.go — Phase 110d (D-197) coverage for the promoted MCP
// attach helper + the config→ToolPolicy projection it absorbs.
package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestProjectToolPolicies_NilPolicy_ZeroDefault — a nil ms.Policy
// yields a zero default + nil per-tool map, so every tool inherits
// tools.DefaultPolicy() at dispatch (the no-policy behaviour).
func TestProjectToolPolicies_NilPolicy_ZeroDefault(t *testing.T) {
	t.Parallel()
	def, perTool, err := ProjectToolPolicies(config.MCPServerConfig{Name: "bare"})
	if err != nil {
		t.Fatalf("ProjectToolPolicies: %v", err)
	}
	if def.TimeoutMS != 0 || def.MaxRetries != 0 || def.RetryOn != nil {
		t.Errorf("expected zero default policy, got %+v", def)
	}
	if perTool != nil {
		t.Errorf("expected nil per-tool map, got %+v", perTool)
	}
}

// TestProjectToolPolicies_Golden_MapsEveryField — the operator YAML
// policy projects onto the runtime ToolPolicy field-for-field,
// including the retry_on class strings and per-tool overrides.
func TestProjectToolPolicies_Golden_MapsEveryField(t *testing.T) {
	t.Parallel()
	ms := config.MCPServerConfig{
		Name: "tuned",
		Policy: &config.ToolPolicyConfig{
			MaxAttempts:   5,
			TimeoutMS:     1234,
			RetryOn:       []string{"transient", "timeout"},
			BackoffBaseMS: 50,
			BackoffMult:   3,
			BackoffMaxMS:  9000,
		},
		ToolPolicies: map[string]config.ToolPolicyConfig{
			"echo": {MaxAttempts: 1, TimeoutMS: 10},
		},
	}
	def, perTool, err := ProjectToolPolicies(ms)
	if err != nil {
		t.Fatalf("ProjectToolPolicies: %v", err)
	}
	if def.TimeoutMS != 1234 || def.MaxRetries != 4 {
		t.Errorf("default policy mis-projected: %+v", def)
	}
	if len(def.RetryOn) != 2 || def.RetryOn[0] != tools.ErrorClass("transient") || def.RetryOn[1] != tools.ErrorClass("timeout") {
		t.Errorf("RetryOn mis-projected: %+v", def.RetryOn)
	}
	if def.BackoffBase != 50*time.Millisecond || def.BackoffMult != 3 || def.BackoffMax != 9*time.Second {
		t.Errorf("backoff mis-projected: %+v", def)
	}
	echo, ok := perTool["echo"]
	if !ok {
		t.Fatalf("per-tool override for echo missing: %+v", perTool)
	}
	// max_attempts=1 with no retry_on names → the EXPLICIT empty
	// non-nil RetryOn slice ("retry on nothing"), surviving the
	// resolved() fall-through (Phase 26b semantics).
	if echo.RetryOn == nil || len(echo.RetryOn) != 0 {
		t.Errorf("expected explicit empty RetryOn for max_attempts=1, got %+v", echo.RetryOn)
	}
	if echo.MaxRetries != 0 || echo.TimeoutMS != 10 {
		t.Errorf("per-tool override mis-projected: %+v", echo)
	}
}

// TestProjectToolPolicies_UnknownRetryClass_FailsLoud — an unknown
// retry_on class fails the projection loud (CLAUDE.md §5), for both
// the server default and a per-tool override.
func TestProjectToolPolicies_UnknownRetryClass_FailsLoud(t *testing.T) {
	t.Parallel()
	_, _, err := ProjectToolPolicies(config.MCPServerConfig{
		Name:   "bad-default",
		Policy: &config.ToolPolicyConfig{RetryOn: []string{"bogus"}},
	})
	if err == nil || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("expected loud default-policy projection error, got %v", err)
	}
	_, _, err = ProjectToolPolicies(config.MCPServerConfig{
		Name: "bad-tool",
		ToolPolicies: map[string]config.ToolPolicyConfig{
			"echo": {RetryOn: []string{"bogus"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), `tool_policies["echo"]`) {
		t.Fatalf("expected loud per-tool projection error naming the tool, got %v", err)
	}
}

// TestAttach_MandatoryDeps_FailLoud — nil Catalog / Registry / Closers
// are construction errors, not silent no-ops.
func TestAttach_MandatoryDeps_FailLoud(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ms := config.MCPServerConfig{Name: "x"}
	closers := []func(context.Context) error{}
	cases := []struct {
		name string
		deps AttachDeps
		want string
	}{
		{"nil catalog", AttachDeps{Registry: NewRegistry(), Closers: &closers}, "Catalog is required"},
		{"nil registry", AttachDeps{Catalog: tools.NewCatalog(), Closers: &closers}, "Registry is required"},
		{"nil closers", AttachDeps{Catalog: tools.NewCatalog(), Registry: NewRegistry()}, "Closers chain is required"},
	}
	for _, tc := range cases {
		if err := Attach(ctx, ms, tc.deps); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: expected error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

// TestAttach_BadPolicy_FailsBeforeSpawn — a policy projection error
// fails Attach before any transport spawns (no closer appended).
func TestAttach_BadPolicy_FailsBeforeSpawn(t *testing.T) {
	t.Parallel()
	closers := []func(context.Context) error{}
	err := Attach(context.Background(), config.MCPServerConfig{
		Name:   "badpolicy",
		Policy: &config.ToolPolicyConfig{RetryOn: []string{"bogus"}},
	}, AttachDeps{Catalog: tools.NewCatalog(), Registry: NewRegistry(), Closers: &closers})
	if err == nil || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("expected policy projection error, got %v", err)
	}
	if len(closers) != 0 {
		t.Errorf("no closer should be appended before Connect, got %d", len(closers))
	}
}

// TestAttach_ConnectFailure_FailsLoud — an unreachable server fails
// Attach loud (CLAUDE.md §13: a misconfigured MCP server must not boot
// silently) and leaves no closer behind.
func TestAttach_ConnectFailure_FailsLoud(t *testing.T) {
	t.Parallel()
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer reject.Close()
	closers := []func(context.Context) error{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Attach(ctx, config.MCPServerConfig{
		Name:          "unreachable",
		TransportMode: string(TransportStreamableHTTP),
		URL:           reject.URL,
	}, AttachDeps{
		Catalog:         tools.NewCatalog(),
		Registry:        NewRegistry(),
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
		Closers:         &closers,
	})
	if err == nil || !strings.Contains(err.Error(), "provider.Connect") {
		t.Fatalf("expected Connect failure, got %v", err)
	}
	if len(closers) != 0 {
		t.Errorf("Connect failure must not leave a closer (provider self-closed), got %d", len(closers))
	}
}

// TestAttach_EndToEnd_PolicyProjectionApplied — the D-197 regression
// gate for the devstack silent drop: a policy-carrying
// config.MCPServerConfig attached through the promoted helper lands
// (a) every discovered tool on the catalog, (b) the projected
// per-server policy on the Registry registration (NOT the zero /
// package default), (c) the discovery stats seeded, and (d) exactly
// one closer that drains the provider.
func TestAttach_EndToEnd_PolicyProjectionApplied(t *testing.T) {
	mockSrv := newMockServer()
	sseHandler := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server {
		return mockSrv.server
	}, nil)
	sseServer := httptest.NewServer(sseHandler)
	defer sseServer.Close()

	cat := tools.NewCatalog()
	reg := NewRegistry()
	closers := []func(context.Context) error{}
	ms := config.MCPServerConfig{
		Name:          "mockmcp",
		TransportMode: string(TransportSSE),
		URL:           sseServer.URL,
		Headers:       map[string]string{"X-Harbor-Test": "1"},
		Policy: &config.ToolPolicyConfig{
			MaxAttempts: 2,
			TimeoutMS:   2500,
			RetryOn:     []string{"transient"},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Attach(ctx, ms, AttachDeps{
		Catalog:         cat,
		Registry:        reg,
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
		Closers:         &closers,
	}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer func() {
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i](context.Background())
		}
	}()

	if len(closers) != 1 {
		t.Errorf("expected exactly 1 provider closer, got %d", len(closers))
	}
	// (a) discovered tools landed on the catalog under the
	// <source>_<tool> naming.
	if _, found := cat.Resolve("mockmcp_echo"); !found {
		t.Errorf("catalog missing discovered tool mockmcp_echo")
	}
	// (b)+(c) the registry registration carries the PROJECTED policy +
	// seeded discovery stats — the exact surface the pre-110d devstack
	// copy silently dropped.
	servers, _, lerr := reg.ListServers(idCtx(t), ListFilter{})
	if lerr != nil {
		t.Fatalf("ListServers: %v", lerr)
	}
	if len(servers) != 1 {
		t.Fatalf("expected 1 registered server, got %d", len(servers))
	}
	srv := servers[0]
	if srv.Policy.TimeoutMS != 2500 || srv.Policy.MaxRetries != 1 {
		t.Errorf("registry policy not the projected one (silent-drop regression): %+v", srv.Policy)
	}
	if srv.ToolCount == 0 {
		t.Errorf("discovery stats not seeded: tool_count=0")
	}
}

// TestAttach_CatalogDuplicate_FailsAfterCloserRegistered — a tool-name
// collision at catalog registration fails Attach loud, AND the
// provider's closer is already on the chain (appended right after the
// successful Connect) so the caller's rollback drains the live
// transport.
func TestAttach_CatalogDuplicate_FailsAfterCloserRegistered(t *testing.T) {
	mockSrv := newMockServer()
	sseHandler := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server {
		return mockSrv.server
	}, nil)
	sseServer := httptest.NewServer(sseHandler)
	defer sseServer.Close()

	cat := tools.NewCatalog()
	// Pre-register the name the discovery will collide on.
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:        "dupmcp_echo",
			Description: "collider",
			Transport:   tools.TransportInProcess,
			Source:      "test",
		},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatalf("pre-register: %v", err)
	}
	closers := []func(context.Context) error{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Attach(ctx, config.MCPServerConfig{
		Name:          "dupmcp",
		TransportMode: string(TransportSSE),
		URL:           sseServer.URL,
	}, AttachDeps{
		Catalog:         cat,
		Registry:        NewRegistry(),
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
		Closers:         &closers,
	})
	if err == nil || !strings.Contains(err.Error(), "catalog.Register") {
		t.Fatalf("expected catalog.Register collision error, got %v", err)
	}
	if len(closers) != 1 {
		t.Fatalf("the provider closer must already be on the chain post-Connect, got %d", len(closers))
	}
	for i := len(closers) - 1; i >= 0; i-- {
		if cErr := closers[i](context.Background()); cErr != nil {
			t.Errorf("rollback closer: %v", cErr)
		}
	}
}

// TestAttach_StdioCommand_URLOrCommandJoins — a command-shaped server
// (no URL) registers its argv join as URLOrCommand; an unreachable
// binary still fails loud at Connect. Covers the command-join branch.
func TestAttach_StdioCommand_FailsLoudOnMissingBinary(t *testing.T) {
	closers := []func(context.Context) error{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Attach(ctx, config.MCPServerConfig{
		Name:    "ghost",
		Command: []string{"/nonexistent/harbor-attach-test-binary", "--flag"},
		// TransportMode empty → auto (covers the default-mode branch).
	}, AttachDeps{
		Catalog:         tools.NewCatalog(),
		Registry:        NewRegistry(),
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
		Closers:         &closers,
	})
	if err == nil {
		t.Fatalf("expected loud failure for a missing stdio binary")
	}
	if len(closers) != 0 {
		t.Errorf("no closer must remain after a Connect failure, got %d", len(closers))
	}
}
