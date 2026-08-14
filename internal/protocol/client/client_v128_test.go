package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

// client_v128_test.go — the v1.28 Protocol-client surface: the
// session-turns read pair, the observability query, the two-phase
// skill-package import, the composition preview, and the HA-56 MCP Apps
// render-admission fields. Each test pins the exact route the client
// targets (nested session routes are pinned explicitly — never derived
// generically) and the request-body shape.

// v128Client is the additive surface the v1.28 client methods live on.
// The curated `Client` interface keeps its narrow projection; the concrete
// client implements the full surface, asserted here.
type v128Client interface {
	SessionTurnsList(context.Context, types.SessionTurnsListRequest) (types.SessionTurnsListResponse, error)
	SessionTurnsGet(context.Context, types.SessionTurnsGetRequest) (types.SessionTurnsGetResponse, error)
	ObservabilityQuery(context.Context, types.ObservabilityQueryRequest) (types.ObservabilityQueryResponse, error)
	AgentConfigUserSkillsImportValidate(context.Context, types.AgentConfigUserSkillsImportValidateRequest) (types.AgentConfigUserSkillsImportValidateResponse, error)
	AgentConfigUserSkillsImportCommit(context.Context, types.AgentConfigUserSkillsImportCommitRequest) (types.AgentConfigUserSkillsImportCommitResponse, error)
	AgentConfigCompositionPreview(context.Context, types.AgentConfigCompositionPreviewRequest) (types.AgentConfigCompositionPreviewResponse, error)
	MCPReadResource(context.Context, types.ReadMCPResourceRequest) (types.ReadMCPResourceResponse, error)
	MCPAppsCallTool(context.Context, types.MCPAppCallToolRequest) (types.MCPAppCallToolResponse, error)
}

// roundTripFixture returns a server that records the last request path +
// decoded body and serves a canned JSON response per request path (a
// missing entry answers 404).
func roundTripFixture(t *testing.T, responses map[string]string) (*httptest.Server, *string, *map[string]any) {
	t.Helper()
	var lastPath string
	var lastBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&lastBody)
		}
		resp, ok := responses[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, &lastPath, &lastBody
}

func v128(t *testing.T, srv *httptest.Server) v128Client {
	t.Helper()
	c, ok := testClient(t, srv).(v128Client)
	if !ok {
		t.Fatal("client does not implement the v1.28 surface")
	}
	return c
}

func TestClient_SessionTurnsRoutesArePinnedExplicitly(t *testing.T) {
	srv, path, body := roundTripFixture(t, map[string]string{
		"/v1/sessions/turns/list": `{
			"header":{"session_id":"session","snapshot_id":3,"as_of":"2026-05-19T09:00:00Z"},
			"turns":[],"order":"newest_first","has_more":false,"count_exact":true,
			"live_resume_seq":0,"page_completeness":"complete","protocol_version":"0.1.0"}`,
		"/v1/sessions/turns/get": `{"session_id":"session","protocol_version":"0.1.0"}`,
	})
	c := v128(t, srv)

	if _, err := c.SessionTurnsList(context.Background(), types.SessionTurnsListRequest{}); err != nil {
		t.Fatalf("SessionTurnsList: %v", err)
	}
	if *path != "/v1/sessions/turns/list" {
		t.Errorf("turns.list path = %q, want the PINNED /v1/sessions/turns/list (never derived)", *path)
	}
	if (*body)["session_id"] != "session" {
		t.Errorf("turns.list body session_id = %v, want the client's own session", (*body)["session_id"])
	}

	if _, err := c.SessionTurnsGet(context.Background(), types.SessionTurnsGetRequest{TaskID: "task-1"}); err != nil {
		t.Fatalf("SessionTurnsGet: %v", err)
	}
	if *path != "/v1/sessions/turns/get" {
		t.Errorf("turns.get path = %q, want the PINNED /v1/sessions/turns/get", *path)
	}
}

func TestClient_ObservabilityQueryRoute(t *testing.T) {
	srv, path, body := roundTripFixture(t, map[string]string{
		"/v1/observability/query": `{
			"rows":[],"quality":{"state":"current","watermark":42,"coverage":"covered"},
			"protocol_version":"0.1.0"}`,
	})
	c := v128(t, srv)
	if _, err := c.ObservabilityQuery(context.Background(), types.ObservabilityQueryRequest{
		Bucket: "hour", Measures: []string{"llm_completions"}, Limit: 100,
	}); err != nil {
		t.Fatalf("ObservabilityQuery: %v", err)
	}
	if *path != "/v1/observability/query" {
		t.Errorf("observability path = %q, want /v1/observability/query", *path)
	}
	if (*body)["bucket"] != "hour" || (*body)["limit"] != float64(100) {
		t.Errorf("observability body = %v, want bucket/limit carried", *body)
	}
}

func TestClient_AgentConfigImportPreviewRoutes(t *testing.T) {
	srv, path, body := roundTripFixture(t, map[string]string{
		"/v1/agent_config/user/skills/import_validate": `{
			"proposal_token":"tok","review":{"name":"n","trigger":"t","step_count":1,"support_files":[],"content_hash":"c","package_hash":"p"},
			"package_hash":"p","expected_content_hash":"e","expires_at":"2026-05-20T09:00:00Z","protocol_version":"0.1.0"}`,
		"/v1/agent_config/user/skills/import_commit": `{
			"receipt":{"tenant_id":"tenant","user_id":"user","agent_id":"agent-x","name":"n","written_hash":"p","written_version":"1"},
			"skill":{"name":"n","trigger":"t","origin":"generated","scope":"user","content_hash":"c"},
			"package_hash":"p","replayed":false,"protocol_version":"0.1.0"}`,
		"/v1/agent_config/composition/preview": `{
			"outcome":"available","boot_pack_set_hash":"b","combined_hash":"c","widened":false,"protocol_version":"0.1.0"}`,
	})
	c := v128(t, srv)

	if _, err := c.AgentConfigUserSkillsImportValidate(context.Background(), types.AgentConfigUserSkillsImportValidateRequest{
		AgentID: "agent-x", ArtifactID: "art-1",
	}); err != nil {
		t.Fatalf("ImportValidate: %v", err)
	}
	if *path != "/v1/agent_config/user/skills/import_validate" {
		t.Errorf("import_validate path = %q", *path)
	}
	if (*body)["artifact_id"] != "art-1" {
		t.Errorf("import_validate body = %v, want artifact_id carried", *body)
	}

	if _, err := c.AgentConfigUserSkillsImportCommit(context.Background(), types.AgentConfigUserSkillsImportCommitRequest{
		ProposalToken: "tok", AgentID: "agent-x", Name: "n",
		ReviewedPackageHash: "p", ExpectedContentHash: "e", Replace: true,
	}); err != nil {
		t.Fatalf("ImportCommit: %v", err)
	}
	if *path != "/v1/agent_config/user/skills/import_commit" {
		t.Errorf("import_commit path = %q", *path)
	}
	if (*body)["replace"] != true || (*body)["proposal_token"] != "tok" {
		t.Errorf("import_commit body = %v, want token + replace consent carried", *body)
	}

	if _, err := c.AgentConfigCompositionPreview(context.Background(), types.AgentConfigCompositionPreviewRequest{
		AgentID: "agent-x",
	}); err != nil {
		t.Fatalf("CompositionPreview: %v", err)
	}
	if *path != "/v1/agent_config/composition/preview" {
		t.Errorf("composition.preview path = %q", *path)
	}
}

func TestClient_MCPAppsAdmissionFieldsReachTheWire(t *testing.T) {
	srv, path, body := roundTripFixture(t, map[string]string{
		"/v1/control/mcp.servers.read_resource": `{"resource_uri":"ui://app/main.html","protocol_version":"0.1.0"}`,
		"/v1/control/mcp.apps.call_tool":        `{"tool":"srv_tool","is_error":false,"protocol_version":"0.1.0"}`,
	})
	c := v128(t, srv)

	// Opt-in read: the request_render_admission flag rides the wire.
	if _, err := c.MCPReadResource(context.Background(), types.ReadMCPResourceRequest{
		ServerID: "srv", ResourceURI: "ui://app/main.html", RequestRenderAdmission: true,
	}); err != nil {
		t.Fatalf("MCPReadResource: %v", err)
	}
	if *path != "/v1/control/mcp.servers.read_resource" {
		t.Errorf("read_resource path = %q", *path)
	}
	if (*body)["request_render_admission"] != true {
		t.Errorf("read_resource body = %v, want request_render_admission: true", *body)
	}

	// Call tool: the fresh render admission rides the DISTINCT field.
	if _, err := c.MCPAppsCallTool(context.Background(), types.MCPAppCallToolRequest{
		ServerID: "srv", Tool: "srv_tool", ResourceURI: "ui://app/main.html",
		RenderAdmission: "opaque-token",
	}); err != nil {
		t.Fatalf("MCPAppsCallTool: %v", err)
	}
	if *path != "/v1/control/mcp.apps.call_tool" {
		t.Errorf("call_tool path = %q", *path)
	}
	if (*body)["render_admission"] != "opaque-token" {
		t.Errorf("call_tool body = %v, want render_admission carried as the distinct authority", *body)
	}
}
