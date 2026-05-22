package bifrost

import (
	"errors"
	"strings"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

// ErrReasoningBudgetTooLow is the typed error the request translator
// returns when an operator-requested reasoning budget falls below a
// provider-specific floor. Phase 83e fails LOUDLY (CLAUDE.md §5
// fail-loudly) rather than silently clamping: the operator sees the
// constraint and corrects their config. Compare via [errors.Is].
var ErrReasoningBudgetTooLow = errors.New("bifrost: provider-specific reasoning budget below floor")

// anthropicReasoningMinTokens is Anthropic's documented minimum for
// `reasoning.max_tokens` (extended-thinking budget). Requests below
// this floor are rejected by the Anthropic API; Harbor surfaces the
// rejection at translation time before the request leaves the process
// (brief 13 §2.6 — "Anthropic requires `reasoning.max_tokens >= 1024`").
const anthropicReasoningMinTokens = 1024

// anthropicReasoningBudget maps Harbor's `ReasoningEffort` enum to an
// Anthropic `reasoning.max_tokens` budget. The `low` tier maps below
// the 1024-token floor on purpose: a `low` effort against Anthropic is
// a config error the operator must see — Anthropic's extended-thinking
// floor is not a "low" budget. The translator fails loud with
// [ErrReasoningBudgetTooLow] when the returned budget is below the
// floor; `medium` and `high` clear it.
func anthropicReasoningBudget(e llm.ReasoningEffort) int {
	switch e {
	case llm.ReasoningLow:
		return 512
	case llm.ReasoningMedium:
		return 4096
	case llm.ReasoningHigh:
		return 16384
	default:
		return 0
	}
}

// reasoningFromMessage walks a bifrost assistant message's normalised
// `ReasoningDetails` slice and returns the concatenated plain-text
// reasoning trace. This is bifrost's documented canonical surface for
// provider reasoning (brief 13 §2.6): every provider — OpenRouter
// thinking-class models AND the native Gemini path — populates
// `reasoning_details[]` on the response message. Reading it here
// closes the Gemini-direct black hole (where the per-delta
// `delta.Reasoning` field is nil) and the unary-path gap (where
// `OnReasoning` never fires).
//
// Only `reasoning.text` and `reasoning.summary` entries contribute —
// `reasoning.encrypted` (signature-bearing thinking blocks) is skipped
// because V1 ships no `provider_native` replay mode that would round-
// trip them (D-148). `reasoning.content_blocks` carry structured block
// data without a flat text field; they are skipped for the same
// reason. A nil/empty slice returns the empty string.
//
// The caller (the driver's unary + streaming paths) stamps the result
// onto `llm.CompleteResponse.Reasoning`.
func reasoningFromMessage(msg *bfschemas.ChatMessage) string {
	if msg == nil || msg.ChatAssistantMessage == nil {
		return ""
	}
	return joinReasoningDetails(msg.ChatAssistantMessage.ReasoningDetails)
}

// joinReasoningDetails concatenates the plain-text entries of a
// `[]ChatReasoningDetails` slice. Entries are joined with a blank line
// so a multi-block trace stays readable. Whitespace-only entries are
// dropped. Exposed at package scope so the streaming path and the
// fixture tests can call it directly.
func joinReasoningDetails(details []bfschemas.ChatReasoningDetails) string {
	if len(details) == 0 {
		return ""
	}
	var parts []string
	for _, d := range details {
		switch d.Type {
		case bfschemas.BifrostReasoningDetailsTypeText:
			if d.Text != nil && strings.TrimSpace(*d.Text) != "" {
				parts = append(parts, strings.TrimRight(*d.Text, "\n"))
			}
		case bfschemas.BifrostReasoningDetailsTypeSummary:
			if d.Summary != nil && strings.TrimSpace(*d.Summary) != "" {
				parts = append(parts, strings.TrimRight(*d.Summary, "\n"))
			}
		default:
			// reasoning.encrypted / reasoning.content_blocks — skipped.
			// V1 has no provider_native replay mode (D-148); encrypted
			// signature blocks have no use until that lands.
		}
	}
	return strings.Join(parts, "\n\n")
}
