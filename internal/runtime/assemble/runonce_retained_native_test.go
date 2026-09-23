package assemble_test

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
)

func TestRunOnce_RetainedNativeExchange(t *testing.T) {
	s, client, calls := retainedRecordingStack(t)
	s.Cfg.Memory.RecentTurns = 2
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "native"}
	for _, run := range []string{"first", "second"} {
		if _, err := s.RunOnce(t.Context(), "continue", id, assemble.WithRunID(run)); err != nil {
			t.Fatal(err)
		}
	}
	client.mu.Lock()
	req := client.requests["native/second"][0]
	client.mu.Unlock()
	callID := ""
	found := false
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleAssistant {
			for _, call := range msg.ToolCalls {
				if call.Name == "retained_read" {
					callID = call.ID
				}
			}
		}
		if msg.Role == llm.RoleTool && msg.ToolCallID != nil && *msg.ToolCallID == callID && callID != "" &&
			msg.Content.Text != nil && strings.Contains(*msg.Content.Text, `"version":9007199254740993`) &&
			strings.Contains(*msg.Content.Text, `"more":false`) {
			found = true
		}
	}
	if !found || calls.Load() != 1 {
		t.Fatalf("historical native pair missing or reexecuted: paired=%t executions=%d", found, calls.Load())
	}
}
