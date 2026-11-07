package bifrost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	bfschemas "github.com/maximhq/bifrost/core/schemas"
)

func TestStreamTiming_ActualDeltasOnly(t *testing.T) {
	text, empty, role, id := "delta", "", "assistant", "call-id"
	cases := []struct {
		name  string
		delta *bfschemas.ChatStreamResponseChoiceDelta
		want  bool
	}{
		{"role only", &bfschemas.ChatStreamResponseChoiceDelta{Role: &role}, false},
		{"empty text", &bfschemas.ChatStreamResponseChoiceDelta{Content: &empty, Reasoning: &empty}, false},
		{"content", &bfschemas.ChatStreamResponseChoiceDelta{Content: &text}, true},
		{"reasoning", &bfschemas.ChatStreamResponseChoiceDelta{Reasoning: &text}, true},
		{"reasoning details", &bfschemas.ChatStreamResponseChoiceDelta{ReasoningDetails: []bfschemas.ChatReasoningDetails{{Type: bfschemas.BifrostReasoningDetailsTypeText, Text: &text}}}, true},
		{"tool id", &bfschemas.ChatStreamResponseChoiceDelta{ToolCalls: []bfschemas.ChatAssistantMessageToolCall{{ID: &id}}}, true},
		{"tool arguments", &bfschemas.ChatStreamResponseChoiceDelta{ToolCalls: []bfschemas.ChatAssistantMessageToolCall{{Function: bfschemas.ChatAssistantMessageToolCallFunction{Arguments: "{"}}}}, true},
		{"empty tool", &bfschemas.ChatStreamResponseChoiceDelta{ToolCalls: []bfschemas.ChatAssistantMessageToolCall{{}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := &bfschemas.BifrostChatResponse{Choices: []bfschemas.BifrostResponseChoice{{ChatStreamResponseChoice: &bfschemas.ChatStreamResponseChoice{Delta: tc.delta}}}}
			if got := hasStreamDelta(response); got != tc.want {
				t.Errorf("hasStreamDelta=%v want %v", got, tc.want)
			}
		})
	}
	if hasStreamDelta(&bfschemas.BifrostChatResponse{}) {
		t.Error("usage/terminal-only chunk has a delta")
	}
}

var streamTimingFactorySeq atomic.Int64

func TestStreamTiming_UsageBeforeErrorRemainsObservable(t *testing.T) {
	const privateDiagnostic = "private upstream stream diagnostic"
	stub := newStubClient()
	stub.streamHandler = func(*bfschemas.BifrostChatRequest) (chan *bfschemas.BifrostStreamChunk, *bfschemas.BifrostError) {
		chunks := make(chan *bfschemas.BifrostStreamChunk, 2)
		chunks <- &bfschemas.BifrostStreamChunk{BifrostChatResponse: &bfschemas.BifrostChatResponse{Usage: &bfschemas.BifrostLLMUsage{PromptTokens: 23, CompletionTokens: 7, TotalTokens: 30}}}
		chunks <- &bfschemas.BifrostStreamChunk{BifrostError: &bfschemas.BifrostError{Error: &bfschemas.ErrorField{Message: privateDiagnostic}}}
		close(chunks)
		return chunks, nil
	}
	name := fmt.Sprintf("stream-timing-%d", streamTimingFactorySeq.Add(1))
	llm.Register(name, func(_ llm.ConfigSnapshot, deps llm.Deps) (llm.Driver, error) {
		return newDriverWithClientAndStore(stub, bfschemas.OpenAI, deps.Bus, deps.Artifacts), nil
	})
	deps, cleanup := makeCustomProviderTestDeps(t)
	defer cleanup()
	var logs bytes.Buffer
	deps.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	cfg := llm.ConfigSnapshot{Driver: name, Provider: "openai", Model: "m", DisableCorrections: true, DisableDowngrade: true, DisableRetry: true, DisableGovernance: true, ModelProfiles: map[string]llm.ModelProfile{"m": {ContextWindowTokens: 10000}}}
	client, err := llm.Open(t.Context(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	response, err := client.Complete(withIdentity(t, t.Context(), "usage-error"), llm.CompleteRequest{Stream: true})
	if err == nil {
		t.Fatal("wanted stream failure")
	}
	if response.Content != "" || response.Usage.ReportPresent || response.Usage.PromptTokens != 0 || response.Cost.ReportPresent {
		t.Fatal("stream error changed its returned content/accounting")
	}
	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["outcome"] != "error" || record["input_tokens"] != float64(23) || record["output_tokens"] != float64(7) {
		t.Fatalf("timing record = %v", record)
	}
	if _, exists := record["ttft_ms"]; exists {
		t.Fatal("usage-only stream invented first delta")
	}
	if strings.Contains(logs.String(), privateDiagnostic) {
		t.Fatal("provider diagnostic leaked")
	}
}
