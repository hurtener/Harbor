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

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
)

// Exercise the real run loop, portable compactor, composed client and pinned
// Bifrost serializer. Only ordinary provider responses are scripted locally.
func TestRequestContext_RealBifrostRunLoop(t *testing.T) {
	for _, outcome := range []string{"success", "bad summary", "protected overflow", "expanding summary", "expanding prior checkpoint", "expanding unlocked"} {
		t.Run(outcome, func(t *testing.T) {
			t.Setenv("HARBOR_REQUEST_CONTEXT_KEY", "synthetic-test-key")
			var mu sync.Mutex
			var kinds, payloads []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("unexpected provider endpoint %s", r.URL.Path)
				}
				var body struct {
					Messages       json.RawMessage `json:"messages"`
					ResponseFormat json.RawMessage `json:"response_format"`
					Tools          json.RawMessage `json:"tools"`
					MaxTokens      int             `json:"max_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				kind, content := "decision", "The edit is ready."
				if len(body.ResponseFormat) > 0 && string(body.ResponseFormat) != "null" {
					kind = "summary"
					content = `{"goals":["edit document"],"facts":["older checks passed"],"pending":["apply the current source"],"last_output_digest":"older work retained","note":"portable"}`
					if strings.HasPrefix(outcome, "expanding") {
						content = `{"goals":["edit"],"facts":["` + strings.Repeat("expansion ", 1000) + `"],"pending":[],"last_output_digest":"older work","note":"too large"}`
					}
					if outcome == "bad summary" {
						content = `{"note":"not a summary"}`
					}
					if len(body.Tools) > 0 && string(body.Tools) != "null" && string(body.Tools) != "[]" {
						t.Error("maintenance could call tools")
					}
				}
				mu.Lock()
				kinds = append(kinds, kind)
				payloads = append(payloads, string(body.Messages))
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "fixture", "object": "chat.completion", "created": 1700000000, "model": "m",
					"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": content}}},
					"usage":   map[string]int{"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150},
				})
			}))
			defer server.Close()
			deps, cleanup := makeCustomProviderTestDeps(t)
			defer cleanup()
			cfg := llm.ConfigSnapshot{
				Driver: "bifrost", Provider: "request-context-fixture", Model: "m", ContextWindowReserve: .05, HeavyOutputThreshold: 128 * 1024,
				DisableCorrections: true, DisableDowngrade: true, DisableRetry: true, DisableGovernance: true,
				ModelProfiles:   map[string]llm.ModelProfile{"m": {ContextWindowTokens: 32768}},
				CustomProviders: []llm.CustomProviderSpec{{Name: "request-context-fixture", BaseURL: server.URL, APIKeyEnvVar: "HARBOR_REQUEST_CONTEXT_KEY", Models: []string{"m"}, Timeout: 10 * time.Second}},
			}
			client, err := llm.Open(t.Context(), cfg, deps)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(context.Background()) }()
			sum, err := summarizer.NewTrajectorySummariser(client)
			if err != nil {
				t.Fatal(err)
			}
			runner := planner.NewCompressionRunner(sum, planner.WithTokenEstimator(func(*planner.Trajectory) (int, error) {
				t.Error("production request-aware path used trajectory-only estimator")
				return 0, errors.New("unexpected trajectory estimate")
			}))
			loop, err := steering.NewRunLoop(steering.NewRegistry(), pauseresume.New(pauseresume.WithBus(deps.Bus)), steering.WithRunLoopBus(deps.Bus))
			if err != nil {
				t.Fatal(err)
			}
			tr := &planner.Trajectory{Query: "edit this document"}
			for i := range 2 {
				tr.Steps = append(tr.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: fmt.Sprint("old-", i), Args: json.RawMessage(`{}`)}, LLMObservation: strings.Repeat("old evidence ", 100)})
			}
			receipt := json.RawMessage(`{"source":"` + strings.Repeat("x", 14586) + `","resource_id":"doc-a","version":9007199254740993,"more":false}`)
			tr.Steps = append(tr.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: "fresh", Args: json.RawMessage(`{"id":"doc-a"}`)}, LLMObservation: receipt})
			// Older exchanges were presented in a preceding decision. The newest
			// one is still protected by both the unseen boundary and recent tail.
			unseen := 2
			tr.UnseenFrom = &unseen
			guidance := strings.Repeat("Stable operator guidance. ", 1800)
			if outcome == "protected overflow" {
				guidance = strings.Repeat("Stable operator guidance. ", 4700)
			}
			var observed []events.Event
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: outcome}, RunID: "request-test"}
			workingTarget := 12000
			trajectoryEstimate, err := planner.DefaultTokenEstimator(tr)
			if err != nil || trajectoryEstimate >= workingTarget {
				t.Fatalf("fixture must be below standalone trajectory target: %d %v", trajectoryEstimate, err)
			}
			if outcome == "expanding prior checkpoint" {
				digest, digestErr := tr.PrefixDigest(1)
				if digestErr != nil {
					t.Fatal(digestErr)
				}
				tr.Summary = &planner.Summary{Facts: []string{"Keep approved navigation"}, Coverage: &planner.SummaryCoverage{Version: 1, Generation: 1, ThroughStep: 1, PrefixDigest: digest}}
			}
			original := tr.Summary
			var trajectoryMu sync.RWMutex
			inspectionMu := &trajectoryMu
			if outcome == "expanding unlocked" {
				inspectionMu = nil
			}
			fin, err := loop.Run(t.Context(), steering.RunSpec{
				Planner: react.New(client, react.WithSystemPrompt(guidance)), Compression: runner, TrajectoryMu: inspectionMu,
				Base: planner.RunContext{Quadruple: q, Query: tr.Query, Trajectory: tr, Budget: planner.Budget{TokenBudget: workingTarget}, Emit: func(ev events.Event) { observed = append(observed, ev) }}, MaxSteps: 4,
			})
			mu.Lock()
			defer mu.Unlock()
			if outcome == "bad summary" {
				if err == nil || tr.Summary != nil || len(kinds) != 1 || kinds[0] != "summary" {
					t.Fatalf("failed candidate reached decision: %v kinds=%v", err, kinds)
				}
				return
			}
			if outcome == "protected overflow" {
				if !errors.Is(err, llm.ErrContextWindowExceeded) || tr.Summary != nil || len(kinds) != 1 || kinds[0] != "summary" {
					t.Fatalf("protected overflow looped or reached provider: %v kinds=%v", err, kinds)
				}
				return
			}
			if err != nil || fin.Reason != planner.FinishGoal || len(kinds) != 2 || kinds[0] != "summary" || kinds[1] != "decision" {
				t.Fatalf("request-level compaction failed: %v kinds=%v", err, kinds)
			}
			if strings.HasPrefix(outcome, "expanding") {
				if tr.Summary != original {
					t.Fatal("non-shrinking candidate replaced the original checkpoint")
				}
				failures := 0
				for _, ev := range observed {
					if ev.Type == planner.EventTypeTrajectoryCompressed {
						t.Fatal("rejected candidate emitted success")
					}
					if ev.Type == planner.EventTypeTrajectoryCompressionFailed {
						failures++
					}
				}
				if failures != 1 || !strings.Contains(payloads[1], "9007199254740993") {
					t.Fatal("rejection lost diagnostics or fresh evidence")
				}
				if original != nil && !strings.Contains(payloads[1], "Keep approved navigation") {
					t.Fatal("previous checkpoint lost on failed replacement")
				}
				return
			}
			if tr.Summary == nil || tr.Summary.Coverage.ThroughStep != 2 || strings.Contains(payloads[0], "9007199254740993") {
				t.Fatal("fresh evidence entered historical compaction")
			}
			var messages []struct {
				Role       string  `json:"role"`
				Content    *string `json:"content"`
				ToolCallID string  `json:"tool_call_id"`
			}
			if err := json.Unmarshal([]byte(payloads[1]), &messages); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, msg := range messages {
				if msg.Role == "tool" && msg.ToolCallID == "fresh" && msg.Content != nil && strings.Contains(*msg.Content, string(receipt)) {
					found = true
				}
			}
			if !found {
				t.Fatal("rebuilt wire request omitted exact fresh receipt")
			}
			compressed := 0
			for _, ev := range observed {
				if ev.Type == planner.EventTypeTrajectoryCompressed {
					compressed++
					if ev.Identity != q {
						t.Fatal("compaction lost run identity")
					}
					payload := ev.Payload.(planner.TrajectoryCompressedPayload)
					if payload.TokenEstimate <= workingTarget {
						t.Fatal("compaction event did not measure assembled input")
					}
				}
			}
			if compressed != 1 {
				t.Fatalf("compaction events=%d", compressed)
			}
		})
	}
}
