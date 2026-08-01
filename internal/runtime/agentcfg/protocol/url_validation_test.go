package protocol_test

import (
	"context"
	"errors"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

func TestAddMCPConnection_HTTPURLStrictAndQueryPreserved(t *testing.T) {
	h := newAddHarness(t, nil)
	for _, raw := range []string{"ftp://mcp.example.test/x", "https://user:pass@mcp.example.test/x", "https://mcp.example.test/x#fragment"} {
		_, err := h.svc.AddMCPConnection(context.Background(), addReq(prototypes.AgentConfigMCPConnectionDescriptor{
			Name: "invalid", Transport: "http", URL: raw,
		}, nil))
		if !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
			t.Fatalf("URL %q error = %v, want ErrInvalidConnection", raw, err)
		}
	}
	resp, err := h.svc.AddMCPConnection(context.Background(), addReq(prototypes.AgentConfigMCPConnectionDescriptor{
		Name: "query", Transport: "http", URL: " HTTPS://mcp.example.test/x?tenant=t&mode=sse ",
	}, nil))
	if err != nil {
		t.Fatalf("valid URL: %v", err)
	}
	if resp.Connection.URL != "https://mcp.example.test/x?tenant=t&mode=sse" {
		t.Fatalf("persisted URL = %q", resp.Connection.URL)
	}
}
