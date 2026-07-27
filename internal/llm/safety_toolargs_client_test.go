package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
)

// TestSafety_OversizeToolCallArgs_FailsLoudlyThroughTheClient proves the
// widened arm reaches the MANDATORY safety wrapper, not only the
// white-box helper — and that it fails with the same sentinel and emits
// the same event as every other arm. The widening changes which field is
// walked, nothing else.
func TestSafety_OversizeToolCallArgs_FailsLoudlyThroughTheClient(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1_000_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	sub := subscribeBus(t, deps.Bus, llm.EventTypeContextLeak)

	ctx := withIdentity(t, context.Background())
	// 33 KiB of tool-call ARGUMENTS — the field the prompt builder
	// replays turn over turn and the driver translators map onto the
	// provider.
	heavyArgs := json.RawMessage(fmt.Sprintf(`{"doc":%q}`, strings.Repeat("Z", 33*1024)))
	callID := "call_args"
	okText := "ok"
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{
			{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCallStructured{{ID: callID, Name: "summarize", Args: heavyArgs}},
			},
			{Role: llm.RoleTool, ToolCallID: &callID, Content: llm.Content{Text: &okText}},
		},
	}
	if _, err := client.Complete(ctx, req); !errors.Is(err, llm.ErrContextLeak) {
		t.Fatalf("err=%v, want ErrContextLeak", err)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != llm.EventTypeContextLeak {
			t.Fatalf("event type=%q, want %q", ev.Type, llm.EventTypeContextLeak)
		}
		p, ok := ev.Payload.(llm.ContextLeakPayload)
		if !ok {
			t.Fatalf("event payload type=%T, want llm.ContextLeakPayload", ev.Payload)
		}
		if p.LeakSite != "Messages[0].ToolCalls[0].Args" {
			t.Errorf("payload.LeakSite=%q, want Messages[0].ToolCalls[0].Args", p.LeakSite)
		}
		if p.SizeBytes != int64(len(heavyArgs)) {
			t.Errorf("payload.SizeBytes=%d, want %d", p.SizeBytes, len(heavyArgs))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe llm.context_leak within 2s")
	}
}

// TestSafety_OrdinaryToolCallArgs_StillComplete — the widening must not
// turn ordinary native tool calling into a refusal. Without this pin the
// test above would pass just as well against a check that rejected every
// tool call.
func TestSafety_OrdinaryToolCallArgs_StillComplete(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1_000_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background())
	callID := "call_ok"
	okText := "ok"
	if _, err := client.Complete(ctx, llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{
			{
				Role: llm.RoleAssistant,
				ToolCalls: []llm.ToolCallStructured{{
					ID:   callID,
					Name: "summarize",
					Args: json.RawMessage(`{"doc":"tool_ab12cd34ef56","max_words":200}`),
				}},
			},
			{Role: llm.RoleTool, ToolCallID: &callID, Content: llm.Content{Text: &okText}},
		},
	}); err != nil {
		t.Fatalf("an ordinary tool call was rejected: %v", err)
	}
}
