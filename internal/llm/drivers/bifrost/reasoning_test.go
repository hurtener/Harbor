package bifrost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

// strptr is a tiny helper for building *string fixture entries.
func strptr(s string) *string { return &s }

// TestReasoningFromMessage_TextEntries asserts plain-text reasoning
// entries are concatenated.
func TestReasoningFromMessage_TextEntries(t *testing.T) {
	t.Parallel()
	msg := &bfschemas.ChatMessage{
		ChatAssistantMessage: &bfschemas.ChatAssistantMessage{
			ReasoningDetails: []bfschemas.ChatReasoningDetails{
				{Index: 0, Type: bfschemas.BifrostReasoningDetailsTypeText, Text: strptr("first block")},
				{Index: 1, Type: bfschemas.BifrostReasoningDetailsTypeText, Text: strptr("second block")},
			},
		},
	}
	got := reasoningFromMessage(msg)
	want := "first block\n\nsecond block"
	if got != want {
		t.Errorf("reasoningFromMessage = %q, want %q", got, want)
	}
}

// TestReasoningFromMessage_SummaryEntry asserts reasoning.summary
// entries contribute their Summary text.
func TestReasoningFromMessage_SummaryEntry(t *testing.T) {
	t.Parallel()
	msg := &bfschemas.ChatMessage{
		ChatAssistantMessage: &bfschemas.ChatAssistantMessage{
			ReasoningDetails: []bfschemas.ChatReasoningDetails{
				{Index: 0, Type: bfschemas.BifrostReasoningDetailsTypeSummary, Summary: strptr("a summary")},
			},
		},
	}
	if got := reasoningFromMessage(msg); got != "a summary" {
		t.Errorf("reasoningFromMessage = %q, want %q", got, "a summary")
	}
}

// TestReasoningFromMessage_EncryptedSkipped asserts encrypted /
// content-block entries are skipped — V1 has no provider_native mode
// (D-148), so signature-bearing thinking blocks have no use.
func TestReasoningFromMessage_EncryptedSkipped(t *testing.T) {
	t.Parallel()
	msg := &bfschemas.ChatMessage{
		ChatAssistantMessage: &bfschemas.ChatAssistantMessage{
			ReasoningDetails: []bfschemas.ChatReasoningDetails{
				{Index: 0, Type: bfschemas.BifrostReasoningDetailsTypeText, Text: strptr("visible")},
				{Index: 1, Type: bfschemas.BifrostReasoningDetailsTypeEncrypted, Data: strptr("AQID-encrypted-blob")},
				{Index: 2, Type: bfschemas.BifrostReasoningDetailsTypeContentBlocks},
			},
		},
	}
	if got := reasoningFromMessage(msg); got != "visible" {
		t.Errorf("reasoningFromMessage = %q, want only the text entry %q", got, "visible")
	}
}

// TestReasoningFromMessage_EmptyAndNil asserts nil / empty inputs
// return the empty string without panicking.
func TestReasoningFromMessage_EmptyAndNil(t *testing.T) {
	t.Parallel()
	if got := reasoningFromMessage(nil); got != "" {
		t.Errorf("nil message: got %q, want empty", got)
	}
	emptyAssistant := &bfschemas.ChatMessage{ChatAssistantMessage: &bfschemas.ChatAssistantMessage{}}
	if got := reasoningFromMessage(emptyAssistant); got != "" {
		t.Errorf("empty details: got %q, want empty", got)
	}
	noAssistant := &bfschemas.ChatMessage{}
	if got := reasoningFromMessage(noAssistant); got != "" {
		t.Errorf("nil assistant: got %q, want empty", got)
	}
	whitespaceOnly := &bfschemas.ChatMessage{
		ChatAssistantMessage: &bfschemas.ChatAssistantMessage{
			ReasoningDetails: []bfschemas.ChatReasoningDetails{
				{Index: 0, Type: bfschemas.BifrostReasoningDetailsTypeText, Text: strptr("   \n  ")},
			},
		},
	}
	if got := reasoningFromMessage(whitespaceOnly); got != "" {
		t.Errorf("whitespace-only: got %q, want empty", got)
	}
}

// reasoningFixtureProviders maps each probed-provider fixture file to
// its bifrost provider + the expected reasoning text its
// ReasoningDetails produce. The fixtures are recorded golden responses
// (brief 13 §2.6 live probe); no live API call runs in CI.
var reasoningFixtureProviders = []struct {
	file         string
	provider     bfschemas.ModelProvider
	wantContains string
}{
	{"openrouter-claude.json", bfschemas.OpenRouter, "weather"},
	{"openrouter-deepseek-r1.json", bfschemas.OpenRouter, "step by step"},
	{"openrouter-o4-mini.json", bfschemas.OpenRouter, "arithmetic expression"},
	{"openrouter-gemini-flash.json", bfschemas.OpenRouter, "entity x9"},
	{"gemini-direct-gemini-flash.json", bfschemas.Gemini, "black hole"},
}

// loadReasoningFixture decodes a recorded BifrostChatResponse fixture.
func loadReasoningFixture(t *testing.T, file string) *bfschemas.BifrostChatResponse {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "reasoning_fixtures", file))
	if err != nil {
		t.Fatalf("read fixture %s: %v", file, err)
	}
	var resp bfschemas.BifrostChatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode fixture %s: %v", file, err)
	}
	return &resp
}

// TestReasoningCapture_Conformance_AllProviders is the per-provider
// conformance pass (Phase 83e acceptance criterion). Each recorded
// fixture's ReasoningDetails must produce a non-empty Reasoning string
// after the driver's unary translation. The gemini-direct case
// explicitly passes — proving the Gemini-direct black hole is closed
// by reading the message-level reasoning_details (brief 13 §2.6).
func TestReasoningCapture_Conformance_AllProviders(t *testing.T) {
	t.Parallel()
	for _, fx := range reasoningFixtureProviders {
		t.Run(fx.file, func(t *testing.T) {
			t.Parallel()
			resp := loadReasoningFixture(t, fx.file)
			out := translateResponse(resp)
			if out.Reasoning == "" {
				t.Fatalf("%s: Reasoning is empty — capture failed for provider %s", fx.file, fx.provider)
			}
			if !contains(out.Reasoning, fx.wantContains) {
				t.Errorf("%s: Reasoning = %q, want it to contain %q", fx.file, out.Reasoning, fx.wantContains)
			}
			// The action JSON in Content must NOT carry reasoning — the
			// schema is narrowed to {tool, args}.
			if contains(out.Content, "reasoning") {
				t.Errorf("%s: Content unexpectedly carries a reasoning field: %q", fx.file, out.Content)
			}
		})
	}
}

// TestReasoningCapture_UnaryPath_StubDriver drives the fixture through
// the driver's full unary path against the stub bifrost client and
// asserts CompleteResponse.Reasoning is populated. This proves the
// capture is wired end-to-end at the driver edge, not just in the
// translate helper.
func TestReasoningCapture_UnaryPath_StubDriver(t *testing.T) {
	t.Parallel()
	for _, fx := range reasoningFixtureProviders {
		t.Run(fx.file, func(t *testing.T) {
			t.Parallel()
			resp := loadReasoningFixture(t, fx.file)
			stub := newStubClient()
			stub.chatHandler = func(_ *bfschemas.BifrostChatRequest) (*bfschemas.BifrostChatResponse, *bfschemas.BifrostError) {
				return resp, nil
			}
			drv := newDriverWithClient(stub, fx.provider, nil)
			defer func() { _ = drv.Close(context.Background()) }()

			ctx := withIdentity(t, context.Background(), "s-reasoning")
			text := "go"
			out, err := drv.Complete(ctx, llm.CompleteRequest{
				Model:    "m",
				Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if out.Reasoning == "" {
				t.Errorf("%s: CompleteResponse.Reasoning empty after unary path", fx.file)
			}
		})
	}
}

// TestReasoningCapture_StreamingPath asserts the streaming path
// prefers a final chunk's message-level ReasoningDetails over the
// per-delta accumulator, and falls back to the accumulator when no
// details array is present.
func TestReasoningCapture_StreamingPath(t *testing.T) {
	t.Parallel()

	t.Run("details_array_preferred", func(t *testing.T) {
		t.Parallel()
		stub := newStubClient()
		stub.streamHandler = func(req *bfschemas.BifrostChatRequest) (chan *bfschemas.BifrostStreamChunk, *bfschemas.BifrostError) {
			ch := make(chan *bfschemas.BifrostStreamChunk, 3)
			go func() {
				defer close(ch)
				content := "answer"
				perDelta := "partial-thought"
				// Chunk 1: content + a per-delta reasoning string.
				ch <- &bfschemas.BifrostStreamChunk{BifrostChatResponse: &bfschemas.BifrostChatResponse{
					Model: req.Model,
					Choices: []bfschemas.BifrostResponseChoice{{ChatStreamResponseChoice: &bfschemas.ChatStreamResponseChoice{
						Delta: &bfschemas.ChatStreamResponseChoiceDelta{Content: &content, Reasoning: &perDelta},
					}}},
				}}
				// Chunk 2: normalised reasoning_details — this wins.
				ch <- &bfschemas.BifrostStreamChunk{BifrostChatResponse: &bfschemas.BifrostChatResponse{
					Model: req.Model,
					Choices: []bfschemas.BifrostResponseChoice{{ChatStreamResponseChoice: &bfschemas.ChatStreamResponseChoice{
						Delta: &bfschemas.ChatStreamResponseChoiceDelta{
							ReasoningDetails: []bfschemas.ChatReasoningDetails{
								{Index: 0, Type: bfschemas.BifrostReasoningDetailsTypeText, Text: strptr("normalised final trace")},
							},
						},
					}}},
				}}
			}()
			return ch, nil
		}
		drv := newDriverWithClient(stub, bfschemas.OpenRouter, nil)
		defer func() { _ = drv.Close(context.Background()) }()
		ctx := withIdentity(t, context.Background(), "s-stream-1")
		text := "go"
		out, err := drv.Complete(ctx, llm.CompleteRequest{
			Model:    "m",
			Stream:   true,
			Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
		})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if out.Reasoning != "normalised final trace" {
			t.Errorf("Reasoning = %q, want the normalised details to win", out.Reasoning)
		}
	})

	t.Run("falls_back_to_accumulator", func(t *testing.T) {
		t.Parallel()
		stub := newStubClient()
		stub.streamHandler = func(req *bfschemas.BifrostChatRequest) (chan *bfschemas.BifrostStreamChunk, *bfschemas.BifrostError) {
			ch := make(chan *bfschemas.BifrostStreamChunk, 2)
			go func() {
				defer close(ch)
				content := "answer"
				r1, r2 := "thinking ", "out loud"
				for _, r := range []string{r1, r2} {
					rr := r
					ch <- &bfschemas.BifrostStreamChunk{BifrostChatResponse: &bfschemas.BifrostChatResponse{
						Model: req.Model,
						Choices: []bfschemas.BifrostResponseChoice{{ChatStreamResponseChoice: &bfschemas.ChatStreamResponseChoice{
							Delta: &bfschemas.ChatStreamResponseChoiceDelta{Content: &content, Reasoning: &rr},
						}}},
					}}
				}
			}()
			return ch, nil
		}
		drv := newDriverWithClient(stub, bfschemas.OpenRouter, nil)
		defer func() { _ = drv.Close(context.Background()) }()
		ctx := withIdentity(t, context.Background(), "s-stream-2")
		text := "go"
		out, err := drv.Complete(ctx, llm.CompleteRequest{
			Model:    "m",
			Stream:   true,
			Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
		})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if out.Reasoning != "thinking out loud" {
			t.Errorf("Reasoning = %q, want the accumulated per-delta trace", out.Reasoning)
		}
	})
}

// TestAnthropicReasoningBudget_BelowFloorFailsLoud asserts the
// Anthropic reasoning-budget floor is enforced at translation time:
// `effort=low` maps below the 1024-token floor and returns
// ErrReasoningBudgetTooLow BEFORE the request reaches bifrost.
func TestAnthropicReasoningBudget_BelowFloorFailsLoud(t *testing.T) {
	t.Parallel()
	text := "go"
	req := llm.CompleteRequest{
		Model:           "claude-sonnet-4.6",
		ReasoningEffort: llm.ReasoningLow,
		Messages:        []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	}
	_, err := translateRequest(bfschemas.Anthropic, req)
	if err == nil {
		t.Fatal("expected ErrReasoningBudgetTooLow, got nil")
	}
	if !errors.Is(err, ErrReasoningBudgetTooLow) {
		t.Errorf("err = %v, want ErrReasoningBudgetTooLow", err)
	}
}

// TestAnthropicReasoningBudget_AboveFloorPasses asserts medium / high
// effort against Anthropic clears the floor and stamps a MaxTokens.
func TestAnthropicReasoningBudget_AboveFloorPasses(t *testing.T) {
	t.Parallel()
	for _, eff := range []llm.ReasoningEffort{llm.ReasoningMedium, llm.ReasoningHigh} {
		t.Run(string(eff), func(t *testing.T) {
			t.Parallel()
			text := "go"
			req := llm.CompleteRequest{
				Model:           "claude-sonnet-4.6",
				ReasoningEffort: eff,
				Messages:        []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
			}
			bfReq, err := translateRequest(bfschemas.Anthropic, req)
			if err != nil {
				t.Fatalf("translateRequest: %v", err)
			}
			if bfReq.Params == nil || bfReq.Params.Reasoning == nil || bfReq.Params.Reasoning.MaxTokens == nil {
				t.Fatalf("expected a stamped reasoning.max_tokens, got %+v", bfReq.Params)
			}
			if *bfReq.Params.Reasoning.MaxTokens < anthropicReasoningMinTokens {
				t.Errorf("MaxTokens = %d, below floor %d", *bfReq.Params.Reasoning.MaxTokens, anthropicReasoningMinTokens)
			}
		})
	}
}

// TestNonAnthropicProvider_NoBudgetFloor asserts the budget floor is
// Anthropic-specific: a non-Anthropic provider with effort=low does
// NOT error (bifrost handles model-specific constraints internally).
func TestNonAnthropicProvider_NoBudgetFloor(t *testing.T) {
	t.Parallel()
	text := "go"
	req := llm.CompleteRequest{
		Model:           "openai/o4-mini",
		ReasoningEffort: llm.ReasoningLow,
		Messages:        []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	}
	if _, err := translateRequest(bfschemas.OpenRouter, req); err != nil {
		t.Errorf("non-Anthropic provider should not hit the budget floor: %v", err)
	}
}

// TestExtractReasoning_EmptyResponse asserts extractReasoning returns
// the empty string for nil / no-choice responses without panicking.
func TestExtractReasoning_EmptyResponse(t *testing.T) {
	t.Parallel()
	if got := extractReasoning(nil); got != "" {
		t.Errorf("nil response: got %q, want empty", got)
	}
	if got := extractReasoning(&bfschemas.BifrostChatResponse{}); got != "" {
		t.Errorf("no-choices response: got %q, want empty", got)
	}
	streamOnly := &bfschemas.BifrostChatResponse{
		Choices: []bfschemas.BifrostResponseChoice{{Index: 0}},
	}
	if got := extractReasoning(streamOnly); got != "" {
		t.Errorf("nil non-stream choice: got %q, want empty", got)
	}
}

// TestAnthropicReasoningBudget_DefaultCase asserts an unrecognised /
// off effort maps to a zero budget (the translator never reaches this
// for `off`, but the helper is defensive).
func TestAnthropicReasoningBudget_DefaultCase(t *testing.T) {
	t.Parallel()
	if got := anthropicReasoningBudget(llm.ReasoningOff); got != 0 {
		t.Errorf("anthropicReasoningBudget(off) = %d, want 0", got)
	}
	if got := anthropicReasoningBudget(llm.ReasoningEffort("bogus")); got != 0 {
		t.Errorf("anthropicReasoningBudget(bogus) = %d, want 0", got)
	}
}

// TestTranslateResponse_NilAndNoReasoning covers translateResponse's
// nil-response guard and the no-reasoning case (Content but empty
// Reasoning) — the common path for a non-reasoning provider.
func TestTranslateResponse_NilAndNoReasoning(t *testing.T) {
	t.Parallel()
	if out := translateResponse(nil); out.Content != "" || out.Reasoning != "" {
		t.Errorf("translateResponse(nil) = %+v, want zero value", out)
	}
	content := "plain answer"
	resp := &bfschemas.BifrostChatResponse{
		Choices: []bfschemas.BifrostResponseChoice{{
			Index: 0,
			ChatNonStreamResponseChoice: &bfschemas.ChatNonStreamResponseChoice{
				Message: &bfschemas.ChatMessage{
					Content: &bfschemas.ChatMessageContent{ContentStr: &content},
				},
			},
		}},
	}
	out := translateResponse(resp)
	if out.Content != content {
		t.Errorf("Content = %q, want %q", out.Content, content)
	}
	if out.Reasoning != "" {
		t.Errorf("Reasoning = %q, want empty for a no-reasoning response", out.Reasoning)
	}
}

// contains is a tiny substring helper kept local to avoid pulling
// strings into every test in this file.
func contains(haystack, needle string) bool {
	return len(needle) == 0 ||
		(len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
