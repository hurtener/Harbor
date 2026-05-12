package corrections

import (
	"fmt"

	"github.com/hurtener/Harbor/internal/llm"
)

// normalizeMessages applies the requested `MessageOrderingPolicy` to
// the chat-message slice. Produces a fresh slice; the input slice is
// never mutated (concurrent callers may share the input).
//
// `OrderingSystemFirstStrict` collapses all system-role messages to
// the front of the slice while preserving their relative order, then
// emits the remaining messages in their original order. This shape
// is what NIM and some OpenAI-compatible proxies require — they
// reject mid-thread `system` messages as a protocol violation.
//
// Tool-observation messages (Harbor convention: `RoleTool` rendered as
// user messages — see `internal/llm/llm.go::Role` comment) are
// treated as user messages by the reordering pass; the role label
// stays on the message so downstream renderers can still distinguish.
//
// Other orderings are passed through unchanged.
func normalizeMessages(in []llm.ChatMessage, mode llm.MessageOrderingPolicy) ([]llm.ChatMessage, error) {
	if mode == llm.OrderingDefault {
		// Caller already shortcuts this, but defensive.
		out := make([]llm.ChatMessage, len(in))
		copy(out, in)
		return out, nil
	}
	switch mode {
	case llm.OrderingSystemFirstStrict:
		return systemFirstStrict(in), nil
	default:
		return nil, fmt.Errorf("unknown MessageOrderingPolicy %q", mode)
	}
}

// systemFirstStrict implements `OrderingSystemFirstStrict`. Time
// complexity is O(n) — single pass to partition, then concatenate.
func systemFirstStrict(in []llm.ChatMessage) []llm.ChatMessage {
	systems := make([]llm.ChatMessage, 0, len(in))
	rest := make([]llm.ChatMessage, 0, len(in))
	for _, m := range in {
		if m.Role == llm.RoleSystem {
			systems = append(systems, m)
		} else {
			rest = append(rest, m)
		}
	}
	out := make([]llm.ChatMessage, 0, len(in))
	out = append(out, systems...)
	out = append(out, rest...)
	return out
}
