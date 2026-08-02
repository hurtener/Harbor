package bifrost

import (
	"errors"
	"strings"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/llm"
)

// ErrReasoningBudgetTooLow is the typed error the request translator
// returns when an operator-requested reasoning budget falls below a
// provider-specific floor. fails LOUDLY (CLAUDE.md §5
// fail-loudly) rather than silently clamping: the operator sees the
// constraint and corrects their config. Compare via [errors.Is].
var ErrReasoningBudgetTooLow = errors.New("bifrost: provider-specific reasoning budget below floor")

// anthropicReasoningMinTokens is Anthropic's documented minimum for
// `reasoning.max_tokens` (extended-thinking budget). Requests below
// this floor are rejected by the Anthropic API; Harbor surfaces the
// rejection at translation time before the request leaves the process.
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
// provider reasoning: every provider — OpenRouter
// thinking-class models AND the native Gemini path — populates
// `reasoning_details[]` on the response message. Reading it here
// closes the Gemini-direct black hole (where the per-delta
// `delta.Reasoning` field is nil) and the unary-path gap (where
// `OnReasoning` never fires).
//
// Only `reasoning.text` and `reasoning.summary` entries contribute.
// Encrypted and content-block entries stay outside D-147's flat-text
// provider-capture boundary. A nil/empty slice returns the empty
// string.
//
// The caller (the driver's unary + streaming paths) stamps the result
// onto `llm.CompleteResponse.Reasoning`.
func reasoningFromMessage(msg *bfschemas.ChatMessage) string {
	if msg == nil || msg.ChatAssistantMessage == nil {
		return ""
	}
	return joinReasoningDetails(msg.ReasoningDetails)
}

// reasoningBlockFallback identifies a details-only semantic block
// when the provider did not supply a stable ID.
type reasoningBlockFallback struct {
	typeName bfschemas.BifrostReasoningDetailsType
	index    int
}

type reasoningBlock struct {
	text strings.Builder
}

// reasoningAccumulator is per-completion state. Raw reasoning is
// authoritative as soon as any non-nil delta is observed, including
// an empty string. Details are retained only as the fallback for
// providers that never expose the raw channel.
type reasoningAccumulator struct {
	raw         strings.Builder
	rawObserved bool
	blocks      []*reasoningBlock
	byID        map[string]*reasoningBlock
	byFallback  map[reasoningBlockFallback]*reasoningBlock
}

func newReasoningAccumulator() *reasoningAccumulator {
	return &reasoningAccumulator{
		byID:       make(map[string]*reasoningBlock),
		byFallback: make(map[reasoningBlockFallback]*reasoningBlock),
	}
}

// observeRaw records one decoded raw reasoning delta byte-for-byte.
func (a *reasoningAccumulator) observeRaw(delta string) {
	a.rawObserved = true
	a.raw.WriteString(delta)
}

// observeDetails coalesces detail fragments by stable semantic block
// identity. A provider ID is primary; each fallback is aliased only
// when it is still unclaimed so a later ID-less fragment can join the
// first ID-bearing block without collapsing two distinct provider IDs.
func (a *reasoningAccumulator) observeDetails(details []bfschemas.ChatReasoningDetails) {
	for _, detail := range details {
		fragment, ok := reasoningDetailText(detail)
		if !ok {
			continue
		}
		fallback := reasoningBlockFallback{typeName: detail.Type, index: detail.Index}
		var block *reasoningBlock
		if detail.ID != nil && *detail.ID != "" {
			block = a.byID[*detail.ID]
			if block == nil {
				block = a.addBlock()
				a.byID[*detail.ID] = block
			}
			if a.byFallback[fallback] == nil {
				a.byFallback[fallback] = block
			}
		} else {
			block = a.byFallback[fallback]
			if block == nil {
				block = a.addBlock()
				a.byFallback[fallback] = block
			}
		}
		block.text.WriteString(fragment)
	}
}

func (a *reasoningAccumulator) addBlock() *reasoningBlock {
	block := &reasoningBlock{}
	a.blocks = append(a.blocks, block)
	return block
}

// result returns raw bytes when raw reasoning was observed; otherwise
// it joins distinct details-only blocks in first-seen order.
func (a *reasoningAccumulator) result() string {
	if a.rawObserved {
		return a.raw.String()
	}
	var out strings.Builder
	for i, block := range a.blocks {
		if i > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(block.text.String())
	}
	return out.String()
}

func reasoningDetailText(detail bfschemas.ChatReasoningDetails) (string, bool) {
	switch detail.Type {
	case bfschemas.BifrostReasoningDetailsTypeText:
		if detail.Text != nil {
			return *detail.Text, true
		}
	case bfschemas.BifrostReasoningDetailsTypeSummary:
		if detail.Summary != nil {
			return *detail.Summary, true
		}
	}
	return "", false
}

// joinReasoningDetails coalesces details-only fragments without
// trimming or rewriting their bytes. Exactly one blank line separates
// distinct semantic blocks. Exposed at package scope so unary capture
// and fixture tests share the streaming fallback contract.
func joinReasoningDetails(details []bfschemas.ChatReasoningDetails) string {
	acc := newReasoningAccumulator()
	acc.observeDetails(details)
	return acc.result()
}
