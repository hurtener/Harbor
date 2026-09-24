package bifrost

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

func TestUsagePresence_NormalizedObjectsNotInventedZeros(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body                      string
		usage, prompt, completion, cost bool
	}{
		{"missing", `{}`, false, false, false, false},
		{"null", `{"usage":null}`, false, false, false, false},
		{"zero report", `{"usage":{"total_tokens":0}}`, true, false, false, false},
		{"details and zero cost", `{"usage":{"total_tokens":0,"prompt_tokens_details":{},"completion_tokens_details":{},"cost":{}}}`, true, true, true, true},
		{"audio details only", `{"usage":{"prompt_tokens":5,"prompt_tokens_details":{"audio_tokens":5}}}`, true, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var response bfschemas.BifrostChatResponse
			if err := json.Unmarshal([]byte(tc.body), &response); err != nil {
				t.Fatal(err)
			}
			u, c := extractUsageAndCost(&response)
			assertPresenceJSON(t, u, map[string]bool{"ReportPresent": tc.usage, "PromptDetailsPresent": tc.prompt, "CompletionDetailsPresent": tc.completion, "Estimated": false})
			assertPresenceJSON(t, c, map[string]bool{"ReportPresent": tc.cost, "Estimated": false})
			if u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
				t.Fatal("invented cache tokens")
			}
		})
	}
}

func assertPresenceJSON(t *testing.T, value any, want map[string]bool) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	for key, expected := range want {
		got, _ := fields[key].(bool)
		if got != expected {
			t.Errorf("%s=%t, want %t in %s", key, got, expected, b)
		}
	}
}

func TestUsagePresence_StreamingPartialReportsPreserveAccounting(t *testing.T) {
	t.Parallel()
	var output strings.Builder
	var calls []llm.ToolCallStructured
	var usage llm.Usage
	var cost llm.Cost
	reasoning := newReasoningAccumulator()
	apply := func(raw string) {
		t.Helper()
		var r bfschemas.BifrostChatResponse
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatal(err)
		}
		processStreamChunk(&r, &output, reasoning, &calls, &usage, &cost, nil, nil)
	}
	apply(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":80},"completion_tokens_details":{"reasoning_tokens":7}}}`)
	apply(`{"usage":{"cost":{"total_cost":0.01}}}`)
	if usage.PromptTokens != 100 || usage.CompletionTokens != 20 || usage.TotalTokens != 120 || usage.CacheReadTokens != 80 || usage.ReasoningTokens != 7 || cost.TotalCost != 0.01 {
		t.Fatalf("cost-only chunk erased token report: %+v %+v", usage, cost)
	}
	apply(`{"usage":{"prompt_tokens":100,"completion_tokens":25,"total_tokens":125}}`)
	if usage.TotalTokens != 125 || usage.CacheReadTokens != 80 || usage.ReasoningTokens != 7 || cost.TotalCost != 0.01 {
		t.Fatalf("sparse token update erased detail/cost report: %+v %+v", usage, cost)
	}
	apply(`{"usage":{"completion_tokens":26}}`)
	apply(`{"usage":{"total_tokens":126}}`)
	if usage.PromptTokens != 100 || usage.CompletionTokens != 26 || usage.TotalTokens != 126 {
		t.Fatal("single-field update erased other normalized totals")
	}
	apply(`{}`)
	apply(`{"usage":{}}`)
	if usage.TotalTokens != 126 || usage.CacheReadTokens != 80 || usage.ReasoningTokens != 7 {
		t.Fatal("empty terminal chunk erased accounting")
	}
	apply(`{"usage":{"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0},"cost":{"total_cost":0}}}`)
	if usage.TotalTokens != 126 || usage.CacheReadTokens != 0 || usage.ReasoningTokens != 0 || cost.TotalCost != 0 {
		t.Fatal("present zero detail/cost object was ignored")
	}
	assertPresenceJSON(t, usage, map[string]bool{"ReportPresent": true, "PromptDetailsPresent": true, "CompletionDetailsPresent": true})
	assertPresenceJSON(t, cost, map[string]bool{"ReportPresent": true})
}

func TestUsagePresence_ConcurrentIsolation(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &bfschemas.BifrostChatResponse{Usage: &bfschemas.BifrostLLMUsage{PromptTokens: i, TotalTokens: i}}
			u, c := extractUsageAndCost(r)
			if u.PromptTokens != i || c.TotalCost != 0 {
				t.Errorf("scope %d changed accounting", i)
			}
			assertPresenceJSON(t, u, map[string]bool{"ReportPresent": true, "PromptDetailsPresent": false})
		}()
	}
	wg.Wait()
}
