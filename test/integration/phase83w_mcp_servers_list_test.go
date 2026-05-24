// Phase 83w F6 (D-164) — `mcp.servers.list` wire surface end-to-end.
//
// Before 83w F6, `bootDevStack` constructed the *mcp.Registry at
// boot but NEVER built or wired the Phase 73k MCPSurface — so every
// call to `mcp.servers.list` from the Console MCP Connections page
// returned `unknown_method` (HTTP 404 with code=unknown_method) and
// the page rendered a scary red error on every visit. F6 wires the
// MCPSurface into transports.NewMux so the surface resolves.
//
// This test boots a real devstack (no MCP servers configured — the
// default `harbor dev` shape) and asserts:
//
//   - POST /v1/control/mcp.servers.list with a complete identity body
//     returns 200 + a wire response carrying an empty .servers array.
//   - The route is NOT unknown_method.
//   - Cross-call: the same surface accepts a non-empty filter and
//     keeps responding 200 (the page passes filters across panel
//     transitions).
//
// Real drivers throughout (devstack.Assemble), per CLAUDE.md §17.3.

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm"
)

// phase83wMockLLMSnapshot pins the deterministic mock-LLM driver so
// the test stays hermetic — the same posture phase83g uses.
func phase83wMockLLMSnapshot(cfg *config.Config) *llm.ConfigSnapshot {
	return &llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: cfg.LLM.ContextWindowReserve,
		HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
		ModelProfiles: map[string]llm.ModelProfile{
			"anthropic/claude-sonnet-4": {
				ContextWindowTokens: 200000,
				TokenEstimator:      "chars_div_4",
			},
		},
	}
}

func TestE2E_Phase83w_F6_MCPServersList_Reachable(t *testing.T) {
	t.Parallel()

	cfgPath := writeDevConfig(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: phase83wMockLLMSnapshot(cfg),
	})
	defer stack.Close()
	if stack.Handler == nil {
		t.Fatal("devstack: Handler is nil")
	}
	if stack.Token == "" {
		t.Fatal("devstack: Token is empty")
	}

	srv := httptest.NewServer(stack.Handler)
	defer srv.Close()

	// Case 1: minimal call. The body carries the dev-token identity
	// triple so the Phase 61 backfill is a no-op and the MCPSurface's
	// identity-mandatory gate passes.
	body := map[string]any{
		"identity": map[string]any{
			"tenant":  devstack.DefaultDevTenant,
			"user":    devstack.DefaultDevUser,
			"session": devstack.DefaultDevSession,
		},
	}
	status, respBody := postProtocol(t, srv.URL+"/v1/control/mcp.servers.list", stack.Token, body)
	if status == http.StatusNotFound {
		t.Fatalf("mcp.servers.list returned 404 (unknown_method) — F6 wiring did not land: %s", string(respBody))
	}
	if status != http.StatusOK {
		t.Fatalf("mcp.servers.list status: got %d, want 200; body=%s", status, string(respBody))
	}
	// Decode + assert the response carries the canonical wire shape.
	var resp struct {
		Servers         []any  `json:"servers"`
		NextPageToken   string `json:"next_page_token"`
		ProtocolVersion string `json:"protocol_version"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("decode response: %v\n%s", err, string(respBody))
	}
	// The dev config in devSmokeYAML configures NO MCP servers, so the
	// list is empty. The slice MUST be non-nil so the JSON renders `[]`
	// rather than `null` — the Console PageState's empty-list branch
	// keys off `Array.isArray(servers) && servers.length === 0`.
	if resp.Servers == nil {
		t.Error("mcp.servers.list: .servers is nil; want empty (non-nil) array for [] rendering")
	}
	if len(resp.Servers) != 0 {
		t.Errorf("mcp.servers.list: got %d servers, want 0 (devSmokeYAML configures none)", len(resp.Servers))
	}
	if resp.ProtocolVersion == "" {
		t.Error("mcp.servers.list: .protocol_version missing")
	}

	// Case 2: same surface with a filter. The page passes the filter
	// across panel transitions; the surface must keep responding 200.
	body2 := map[string]any{
		"identity": map[string]any{
			"tenant":  devstack.DefaultDevTenant,
			"user":    devstack.DefaultDevUser,
			"session": devstack.DefaultDevSession,
		},
		"name_prefix": "abc",
		"page_size":   25,
	}
	status2, _ := postProtocol(t, srv.URL+"/v1/control/mcp.servers.list", stack.Token, body2)
	if status2 != http.StatusOK {
		t.Errorf("mcp.servers.list (with filter) status: got %d, want 200", status2)
	}
}

// TestE2E_Phase83w_F6_MCPServersList_RejectsMissingIdentity confirms
// the identity-mandatory gate at the MCPSurface (CLAUDE.md §6).
func TestE2E_Phase83w_F6_MCPServersList_RejectsMissingIdentity(t *testing.T) {
	t.Parallel()

	cfgPath := writeDevConfig(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: phase83wMockLLMSnapshot(cfg),
		SkipAuth:          true, // we want to test the MCPSurface gate, not auth middleware
	})
	defer stack.Close()
	if stack.Handler == nil {
		t.Fatal("devstack: Handler is nil")
	}

	srv := httptest.NewServer(stack.Handler)
	defer srv.Close()

	// Empty body — no identity. With SkipAuth, the auth middleware is
	// not wrapped, so the MCPSurface itself must fail closed.
	status, respBody := postProtocol(t, srv.URL+"/v1/control/mcp.servers.list", "", map[string]any{})
	if status == http.StatusOK {
		t.Fatalf("mcp.servers.list with no identity returned 200: %s", string(respBody))
	}
	// Verify the wire-error carries the identity_required code shape.
	var errBody struct {
		Code  string `json:"code"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(respBody, &errBody)
	code := errBody.Code
	if code == "" {
		code = errBody.Error.Code
	}
	if code != "identity_required" {
		t.Errorf("mcp.servers.list with no identity: code=%q, want identity_required; body=%s",
			code, string(respBody))
	}
}

// postProtocol POSTs body (encoded as JSON) to url with an optional
// Bearer token. Returns the status + body. Used by the F6 tests.
func postProtocol(t *testing.T, url, token string, body any) (int, []byte) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, respBody
}
