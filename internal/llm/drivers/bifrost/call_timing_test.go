package bifrost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func streamTimingClient(t *testing.T, stub *stubClient, logs *bytes.Buffer) llm.LLMClient {
	t.Helper()
	name := fmt.Sprintf("stream-timing-%d", streamTimingFactorySeq.Add(1))
	llm.Register(name, func(_ llm.ConfigSnapshot, deps llm.Deps) (llm.Driver, error) {
		return newDriverWithClientAndStore(stub, bfschemas.OpenAI, deps.Bus, deps.Artifacts), nil
	})
	deps, cleanup := makeCustomProviderTestDeps(t)
	t.Cleanup(cleanup)
	deps.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	cfg := llm.ConfigSnapshot{Driver: name, Provider: "openai", Model: "m", DisableCorrections: true, DisableDowngrade: true, DisableRetry: true, DisableGovernance: true, ModelProfiles: map[string]llm.ModelProfile{"m": {ContextWindowTokens: 10000}}}
	client, err := llm.Open(t.Context(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client
}

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
	var logs bytes.Buffer
	client := streamTimingClient(t, stub, &logs)
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

func TestStreamTiming_UsageBeforeCancellationRemainsObservable(t *testing.T) {
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(withIdentity(t, t.Context(), "usage-cancel"))
	defer cancel()
	stub := newStubClient()
	piece := "delta"
	stub.streamHandler = func(*bfschemas.BifrostChatRequest) (chan *bfschemas.BifrostStreamChunk, *bfschemas.BifrostError) {
		chunks := make(chan *bfschemas.BifrostStreamChunk, 2)
		chunks <- &bfschemas.BifrostStreamChunk{BifrostChatResponse: &bfschemas.BifrostChatResponse{Usage: &bfschemas.BifrostLLMUsage{PromptTokens: 23}}}
		chunks <- &bfschemas.BifrostStreamChunk{BifrostChatResponse: &bfschemas.BifrostChatResponse{Choices: []bfschemas.BifrostResponseChoice{{ChatStreamResponseChoice: &bfschemas.ChatStreamResponseChoice{Delta: &bfschemas.ChatStreamResponseChoiceDelta{Content: &piece}}}}}}
		close(chunks)
		return chunks, nil
	}
	client := streamTimingClient(t, stub, &logs)
	_, err := client.Complete(ctx, llm.CompleteRequest{Stream: true, OnContent: func(piece string, _ bool) {
		if piece != "" {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["outcome"] != "canceled" || record["input_tokens"] != float64(23) || record["output_tokens"] != float64(0) || record["ttft_ms"] == nil {
		t.Fatalf("timing record = %v", record)
	}
}

func TestStreamTiming_UsageSnapshotsDoNotAllocatePerContentChunk(t *testing.T) {
	var logs bytes.Buffer
	stub := newStubClient()
	piece := "x"
	usageChunk := &bfschemas.BifrostStreamChunk{BifrostChatResponse: &bfschemas.BifrostChatResponse{Usage: &bfschemas.BifrostLLMUsage{PromptTokens: 23}}}
	contentChunk := &bfschemas.BifrostStreamChunk{BifrostChatResponse: &bfschemas.BifrostChatResponse{Choices: []bfschemas.BifrostResponseChoice{{ChatStreamResponseChoice: &bfschemas.ChatStreamResponseChoice{Delta: &bfschemas.ChatStreamResponseChoiceDelta{Content: &piece}}}}}}
	chunksPerCall := 1
	stub.streamHandler = func(*bfschemas.BifrostChatRequest) (chan *bfschemas.BifrostStreamChunk, *bfschemas.BifrostError) {
		chunks := make(chan *bfschemas.BifrostStreamChunk, chunksPerCall+1)
		chunks <- usageChunk
		for range chunksPerCall {
			chunks <- contentChunk
		}
		close(chunks)
		return chunks, nil
	}
	client := streamTimingClient(t, stub, &logs)
	ctx := withIdentity(t, t.Context(), "usage-allocs")
	run := func() {
		logs.Reset()
		if _, err := client.Complete(ctx, llm.CompleteRequest{Stream: true}); err != nil {
			t.Fatal(err)
		}
	}
	small := testing.AllocsPerRun(5, run)
	chunksPerCall = 128
	large := testing.AllocsPerRun(5, run)
	// String assembly grows logarithmically. A usage snapshot on every
	// content chunk instead adds 127 allocations to this otherwise equal call.
	if large > small+20 {
		t.Fatalf("stream allocations grew per chunk: one=%g, 128=%g", small, large)
	}
	var record map[string]any
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["ttft_ms"] == nil || record["input_tokens"] != float64(23) || record["output_tokens"] != float64(0) {
		t.Fatalf("timing without callbacks = %v", record)
	}
}
