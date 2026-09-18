package llm_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
)

func TestEstimateRequestTokens_NativeInput(t *testing.T) {
	t.Parallel()
	text := "Inspect the resource."
	callID := "read-1"
	base := llm.CompleteRequest{Messages: []llm.ChatMessage{
		{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
	}}
	baseline := llm.EstimateRequestTokens(base, llm.ModelProfile{})
	cases := []struct {
		name string
		req  llm.CompleteRequest
		min  int
	}{
		{
			name: "tool schema",
			req: llm.CompleteRequest{Messages: base.Messages, Tools: []llm.ToolDeclaration{{
				Name: "read", Schema: json.RawMessage(`{"description":"` + strings.Repeat("s", 8000) + `"}`),
			}}},
			min: 2000,
		},
		{
			name: "tool description",
			req: llm.CompleteRequest{Messages: base.Messages, Tools: []llm.ToolDeclaration{{
				Name: "read", Description: strings.Repeat("d", 8000),
			}}},
			min: 2000,
		},
		{
			name: "historical call arguments without text",
			req: llm.CompleteRequest{Messages: append([]llm.ChatMessage{base.Messages[0]}, llm.ChatMessage{
				Role: llm.RoleAssistant, ToolCalls: []llm.ToolCallStructured{{
					ID: callID, Name: "edit", Args: json.RawMessage(`{"source":"` + strings.Repeat("a", 8000) + `"}`),
				}},
			})},
			min: 2000,
		},
		{
			name: "tool result correlation",
			req: llm.CompleteRequest{Messages: []llm.ChatMessage{{
				Role: llm.RoleTool, Content: llm.Content{Text: &text}, ToolCallID: &callID,
			}}},
			min: 1,
		},
		{
			name: "tool selection",
			req:  llm.CompleteRequest{Messages: base.Messages, ToolChoice: "required"},
			min:  1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := llm.EstimateRequestTokens(tc.req, llm.ModelProfile{})
			if got-baseline < tc.min {
				t.Fatalf("estimate=%d baseline=%d: native input must contribute at least %d", got, baseline, tc.min)
			}
		})
	}
}

func TestEstimateRequestTokens_OutputReservationIsNotInput(t *testing.T) {
	t.Parallel()
	text := "Question"
	req := llm.CompleteRequest{Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}}
	before := llm.EstimateRequestTokens(req, llm.ModelProfile{})
	maxTokens := 32000
	req.MaxTokens = &maxTokens
	if after := llm.EstimateRequestTokens(req, llm.ModelProfile{}); after != before {
		t.Fatalf("output reservation counted as input: before=%d after=%d", before, after)
	}
}

func TestEstimateRequestTokens_ConcurrentReadOnly(t *testing.T) {
	t.Parallel()
	callID := "call-1"
	text := "The complete result"
	req := llm.CompleteRequest{
		Messages: []llm.ChatMessage{
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCallStructured{{ID: callID, Name: "read", Args: json.RawMessage(`{"id":"a"}`)}}},
			{Role: llm.RoleTool, ToolCallID: &callID, Content: llm.Content{Text: &text}},
		},
		Tools: []llm.ToolDeclaration{{Name: "read", Description: "Read", Schema: json.RawMessage(`{"type":"object"}`)}},
	}
	before := req
	before.Messages = append([]llm.ChatMessage(nil), req.Messages...)
	before.Messages[0].ToolCalls = append([]llm.ToolCallStructured(nil), req.Messages[0].ToolCalls...)
	before.Messages[0].ToolCalls[0].Args = append(json.RawMessage(nil), req.Messages[0].ToolCalls[0].Args...)
	before.Tools = append([]llm.ToolDeclaration(nil), req.Tools...)
	before.Tools[0].Schema = append(json.RawMessage(nil), req.Tools[0].Schema...)
	want := llm.EstimateRequestTokens(req, llm.ModelProfile{})
	var wg sync.WaitGroup
	for range 128 {
		wg.Go(func() {
			if got := llm.EstimateRequestTokens(req, llm.ModelProfile{}); got != want {
				t.Errorf("estimate=%d want %d", got, want)
			}
		})
	}
	wg.Wait()
	if !reflect.DeepEqual(req, before) {
		t.Fatal("token estimation mutated shared input")
	}
}
