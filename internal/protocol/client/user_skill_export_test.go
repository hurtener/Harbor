package client

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestUserSkillExportClient_RouteAndBoundIdentity(t *testing.T) {
	srv, path, body := roundTripFixture(t, map[string]string{
		"/v1/agent_config/user/skills/export": `{"artifact":{"id":"export-id","mime_type":"application/zip","size_bytes":123,"scope":{"tenant":"tenant","user":"user","session":"session"}},"package_hash":"v1:hash","name":"personal","size_bytes":123,"protocol_version":"0.1.0"}`,
	})
	c, ok := testClient(t, srv).(UserSkillExportClient)
	if !ok {
		t.Fatal("client does not implement UserSkillExportClient")
	}
	out, err := c.AgentConfigUserSkillsExport(context.Background(), types.AgentConfigUserSkillsExportRequest{
		Identity: types.IdentityScope{Tenant: "foreign", User: "foreign", Session: "foreign"},
		AgentID:  "agent-x", Name: "personal",
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if *path != "/v1/agent_config/user/skills/export" || (*body)["agent_id"] != "agent-x" || (*body)["name"] != "personal" {
		t.Fatalf("export request = %s %v", *path, *body)
	}
	scope, ok := (*body)["identity"].(map[string]any)
	if !ok || scope["tenant"] != "tenant" || scope["user"] != "user" || scope["session"] != "session" {
		t.Fatalf("export identity = %v, want client-bound triple", (*body)["identity"])
	}
	if out.Artifact.ID != "export-id" || out.Name != "personal" || out.PackageHash != "v1:hash" || out.SizeBytes != 123 {
		t.Fatalf("export response = %+v", out)
	}
}
