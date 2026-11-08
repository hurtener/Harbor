package llm_test

import (
	"bytes"
	"context"
	"github.com/hurtener/Harbor/internal/llm"
	"testing"
)

func TestCallTiming_AudioOnlyDetailsAreNotCacheMeasurements(t *testing.T) {
	var logs bytes.Buffer
	// Bifrost extractUsageAndCost maps {prompt_tokens_details:{audio_tokens:5}} to this exact Usage.
	usage := llm.Usage{ReportPresent: true, PromptDetailsPresent: true, PromptTokens: 5}
	c := timingClient(t, timingDriver{complete: func(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: "ok", Usage: usage}, nil
	}}, &logs)
	text := "hello"
	if _, err := c.Complete(withIdentity(t, t.Context()), llm.CompleteRequest{Model: "m", Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}}); err != nil {
		t.Fatal(err)
	}
	records := timingRecords(t, &logs)
	if len(records) != 1 {
		t.Fatalf("records=%v", records)
	}
	for _, field := range []string{"cached_input_tokens", "cache_write_tokens"} {
		if v, ok := records[0][field]; ok {
			t.Errorf("unreported %s logged as measured %v", field, v)
		}
	}
}
