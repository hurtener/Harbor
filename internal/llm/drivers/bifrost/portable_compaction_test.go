package bifrost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// Real pinned Bifrost HTTP decoding and encoding, not a simulated Harbor client.
// The only server-side fixture is ordinary OpenAI-compatible chat completion.
func TestE2E_PortableCompaction_ContinueOnAnotherModel(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprintf("truncated=%t", truncated), func(t *testing.T) {
			t.Setenv("HARBOR_TEST_COMPACTION_KEY", "synthetic-test-key")
			var mu sync.Mutex
			var payloads []string
			var models []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("unexpected/native-compaction endpoint %s", r.URL.Path)
				}
				var body struct {
					Model          string          `json:"model"`
					Messages       json.RawMessage `json:"messages"`
					ResponseFormat json.RawMessage `json:"response_format"`
					Tools          json.RawMessage `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "bad fixture request", 400)
					return
				}
				isSummary := len(body.ResponseFormat) > 0 && string(body.ResponseFormat) != "null"
				content, reason := "done", "stop"
				if isSummary {
					if len(body.Tools) > 0 && string(body.Tools) != "null" && string(body.Tools) != "[]" {
						t.Error("maintenance request declares executable tools")
					}
					content = `{"goals":["edit document"],"facts":["keep navigation"],"pending":["verify edit"],"last_output_digest":"older reads completed","note":"portable"}`
					if truncated {
						reason = "length"
					}
				}
				mu.Lock()
				payloads = append(payloads, string(body.Messages))
				models = append(models, body.Model)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "chatcmpl-portable", "object": "chat.completion", "created": 1700000000, "model": body.Model,
					"choices": []any{map[string]any{"index": 0, "finish_reason": reason, "message": map[string]any{"role": "assistant", "content": content}}},
					"usage":   map[string]int{"prompt_tokens": 100, "completion_tokens": 60, "total_tokens": 160},
				})
			}))
			defer server.Close()
			deps, cleanup := makeCustomProviderTestDeps(t)
			defer cleanup()
			cfg := llm.ConfigSnapshot{
				Driver: "bifrost", Provider: "context-fixture", Model: "model-a", ContextWindowReserve: 0.05, HeavyOutputThreshold: 128 * 1024,
				DisableCorrections: true, DisableDowngrade: true, DisableRetry: true, DisableGovernance: true,
				ModelProfiles:   map[string]llm.ModelProfile{"model-a": {ContextWindowTokens: 32768}, "model-b": {ContextWindowTokens: 24576}},
				CustomProviders: []llm.CustomProviderSpec{{Name: "context-fixture", BaseURL: server.URL, APIKeyEnvVar: "HARBOR_TEST_COMPACTION_KEY", Models: []string{"model-a", "model-b"}, Timeout: 10 * time.Second}},
			}
			client, err := llm.Open(t.Context(), cfg, deps)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(context.Background()) }()
			sum, err := summarizer.NewTrajectorySummariser(client, summarizer.WithTrajectoryHeavyOutputThreshold(16*1024))
			if err != nil {
				t.Fatal(err)
			}
			runner := planner.NewCompressionRunner(sum)
			tr := &planner.Trajectory{Query: "edit the existing document"}
			for i := range 10 {
				tr.Steps = append(tr.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: fmt.Sprintf("old-%d", i), Args: json.RawMessage(`{}`)}, LLMObservation: strings.Repeat("older-evidence ", 220)})
			}
			receipt := map[string]any{"source": strings.Repeat("source", 2440), "resource_id": "doc-a", "version": 7, "more": false}
			tr.Steps = append(tr.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: "fresh", Args: json.RawMessage(`{"id":"doc-a"}`)}, LLMObservation: receipt})
			rc := planner.RunContext{Quadruple: identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "portable"}, Query: tr.Query, Trajectory: tr, Budget: planner.Budget{TokenBudget: 6000}}
			ctx, err := identity.WithRun(t.Context(), rc.Quadruple.Identity, rc.Quadruple.RunID)
			if err != nil {
				t.Fatal(err)
			}
			err = runner.MaybeCompress(ctx, rc, tr)
			if truncated {
				if !errors.Is(err, summarizer.ErrTrajectorySummaryIncomplete) || tr.Summary != nil {
					t.Fatalf("truncated candidate installed: err=%v summary=%v", err, tr.Summary)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tr.Summary == nil || tr.Summary.Coverage == nil {
				t.Fatal("no checkpoint installed")
			}
			mu.Lock()
			summaryCalls := len(payloads)
			mu.Unlock()
			if summaryCalls < 2 {
				t.Fatal("fixture did not exercise chronological chunks")
			}
			serialized, err := tr.Serialize()
			if err != nil {
				t.Fatal(err)
			}
			restored, err := trajectory.Deserialize(serialized)
			if err != nil {
				t.Fatal(err)
			}
			// Checkpoint metadata round-trips. Cold execution replay is a later
			// slice: this test switches models in the same retained live run.
			if start, err := restored.ReplayStart(); err != nil || start != tr.Summary.Coverage.ThroughStep {
				t.Fatalf("checkpoint round-trip: start=%d err=%v", start, err)
			}
			modelB := "model-b"
			rc.LLMOverrides = &planner.LLMOverrides{Model: &modelB}
			if _, err := react.New(client).Next(ctx, rc); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			last := payloads[len(payloads)-1]
			lastModel := models[len(models)-1]
			mu.Unlock()
			var messages []struct {
				Role       string  `json:"role"`
				Content    *string `json:"content"`
				ToolCallID string  `json:"tool_call_id"`
			}
			if err := json.Unmarshal([]byte(last), &messages); err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(receipt)
			found := false
			for _, m := range messages {
				if m.ToolCallID == "fresh" && m.Content != nil && strings.Contains(*m.Content, string(encoded)) {
					found = true
				}
			}
			if !found || lastModel != "model-b" || !strings.Contains(last, "keep navigation") {
				t.Fatal("model switch lost fresh receipt or portable checkpoint")
			}
		})
	}
}
