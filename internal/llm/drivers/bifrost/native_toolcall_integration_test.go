package bifrost

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
)

// TestE2E_Bifrost_NativeToolCalling_ProducesToolCalls is the
// Phase 107c step 11 / AC-28 live test — a real bifrost client
// hitting a real provider through OpenRouter, asserting the
// native tool-calling round-trip works end-to-end. The mock-driven
// `TestReactPlanner_NativeToolCall_DiscoveryCycle` (AC-26) proves the
// React planner's projector reads `resp.ToolCalls` correctly; this
// test proves the BIFROST mapping actually surfaces ToolCalls from a
// live provider response.
//
// Three probes, each against the same model:
//
//  1. **Single tool call**. A prompt that nudges the LLM to invoke
//     ONE declared tool (`get_weather`). Asserts `resp.ToolCalls`
//     is non-empty with `Name == "get_weather"`. `resp.Content` may
//     carry a preamble or be empty per provider behavior.
//  2. **Tool-free terminal**. A direct question with the same tool
//     declared but irrelevant to the question. Asserts
//     `resp.ToolCalls` is empty and `resp.Content` non-empty
//     (the projector maps this to `Finish{Goal, Payload: content}`).
//  3. **Parallel tool calls**. A prompt that nudges the LLM to call
//     two tools in one response. Provider-dependent: not every
//     model emits parallel tool_calls reliably, so the assertion
//     is "at least one tool_call" (graceful degradation: a single
//     call is fine, two-or-more is the optimistic case).
//
// SKIP-gates: requires `HARBOR_LIVE_LLM=1` AND a provider key
// (`OPENROUTER_API_KEY` or `ANTHROPIC_API_KEY`). The conformance test
// uses the same gate so CI default skips. The wave-end E2E exercises
// this against the operator's real key (per the conformance test's
// precedent).
//
// Pause-and-ask checkpoint #2 (Provider-specific JSON-Schema
// compatibility): the `get_weather` schema is a simple
// `{type: object, properties: {city: {type: string}}, required:[city],
// additionalProperties: false}` shape. This validates against OpenAI
// strict mode (the strictest of the three major providers). If a
// provider rejects this schema, the failure surface is the
// `client.Complete` call returning an error — STOP per the plan and
// ask before adjusting.
func TestE2E_Bifrost_NativeToolCalling_ProducesToolCalls(t *testing.T) {
	if os.Getenv("HARBOR_LIVE_LLM") != "1" {
		t.Skip("set HARBOR_LIVE_LLM=1 to run the live native-tool-calling probe (this test burns API credits)")
	}
	if os.Getenv("OPENROUTER_API_KEY") == "" && os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("no LLM provider key — set OPENROUTER_API_KEY or ANTHROPIC_API_KEY to run AC-28")
	}

	client, cleanup := openLiveBifrost(t)
	defer cleanup()

	// Provider routing: prefer Anthropic via OpenRouter (the project's
	// default smoke target per the phase plan's "OpenRouter Anthropic
	// is the default smoke target" risk note). The conformance test
	// pins this model in its profile map.
	const model = "anthropic/claude-haiku-4.5"

	weatherSchema := json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"city": {
				"type": "string",
				"description": "The city to look up the current weather for."
			}
		},
		"required": ["city"]
	}`)
	cityListSchema := json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"cities": {
				"type": "array",
				"items": {"type": "string"},
				"description": "City names to list."
			}
		},
		"required": ["cities"]
	}`)
	declaredTools := []llm.ToolDeclaration{
		{
			Name:        "get_weather",
			Description: "Return current weather for a named city.",
			Schema:      weatherSchema,
		},
		{
			Name:        "list_cities",
			Description: "Return a deduplicated list of city names.",
			Schema:      cityListSchema,
		},
	}

	t.Run("single_tool_call", func(t *testing.T) {
		ctx := liveCtx(t, "ac28-single")
		prompt := "Use get_weather to check the current weather in Paris."
		resp, err := client.Complete(ctx, llm.CompleteRequest{
			Model:    model,
			Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &prompt}}},
			Tools:    declaredTools,
		})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if len(resp.ToolCalls) == 0 {
			t.Fatalf("resp.ToolCalls is empty — model did not emit a tool call (content=%q)", resp.Content)
		}
		call := resp.ToolCalls[0]
		if call.Name != "get_weather" {
			t.Errorf("ToolCalls[0].Name = %q, want %q", call.Name, "get_weather")
		}
		if call.ID == "" {
			t.Errorf("ToolCalls[0].ID is empty — providers MUST surface a call_id for tool-result round-trip")
		}
		var args struct {
			City string `json:"city"`
		}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("ToolCalls[0].Args unmarshal: %v (raw=%s)", err, string(call.Args))
		}
		if !strings.EqualFold(args.City, "paris") {
			t.Errorf("args.city = %q, want \"paris\" (case-insensitive)", args.City)
		}
	})

	t.Run("tool_free_terminal", func(t *testing.T) {
		ctx := liveCtx(t, "ac28-terminal")
		prompt := "Answer in one short sentence: what is 2+2?"
		resp, err := client.Complete(ctx, llm.CompleteRequest{
			Model:    model,
			Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &prompt}}},
			Tools:    declaredTools,
		})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if len(resp.ToolCalls) != 0 {
			t.Errorf("resp.ToolCalls non-empty on a tool-irrelevant prompt: %+v", resp.ToolCalls)
		}
		if resp.Content == "" {
			t.Errorf("resp.Content empty — no terminal answer; cannot project Finish{Goal}")
		}
		// The projector maps this to Finish{Goal, Payload: resp.Content}.
		// We don't run the projector here (LLM-driver test) — just pin
		// the wire shape it consumes.
	})

	t.Run("parallel_tool_calls", func(t *testing.T) {
		ctx := liveCtx(t, "ac28-parallel")
		prompt := "Call list_cities with [\"Paris\",\"Berlin\"] AND ALSO call get_weather for Paris. Make both tool calls in the same response."
		resp, err := client.Complete(ctx, llm.CompleteRequest{
			Model:             model,
			Messages:          []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &prompt}}},
			Tools:             declaredTools,
			ParallelToolCalls: true,
		})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		// Provider-dependent: graceful degradation per the test
		// godoc — at-least-one tool call is the gate. Two-or-more is
		// the optimistic case that proves the parallel path round-trips.
		if len(resp.ToolCalls) == 0 {
			t.Fatalf("resp.ToolCalls empty — model rejected the parallel prompt entirely (content=%q)", resp.Content)
		}
		if len(resp.ToolCalls) < 2 {
			t.Logf("model emitted only %d tool call; parallel path is provider-dependent — single call is acceptable", len(resp.ToolCalls))
		}
		// Every tool call MUST carry a stable ID for the tool-result
		// round-trip (the projector stamps the ID onto
		// `planner.CallTool.CallID` so the next turn's prompt threads
		// the `RoleTool` message with matching `ToolCallID`).
		for i, call := range resp.ToolCalls {
			if call.ID == "" {
				t.Errorf("ToolCalls[%d].ID is empty", i)
			}
			if call.Name == "" {
				t.Errorf("ToolCalls[%d].Name is empty", i)
			}
		}
	})
}

// Compile-time use of context — keeps the import clean if a future
// refactor strips the explicit ctx wiring above.
var _ = context.Background
