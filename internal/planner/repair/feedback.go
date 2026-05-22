package repair

import (
	"fmt"

	"github.com/hurtener/Harbor/internal/llm"
)

// appendCorrectiveTurn returns a copy of `req` with the LLM's bad
// response echoed back as an assistant message + a user-role
// corrective turn that names the failure and asks for a fixed
// response. The original `req.Messages` slice is NOT mutated (the
// loop holds the canonical request unchanged so its first user turn
// is preserved on every re-ask).
//
// The corrective-turn shape mirrors Phase 36's retry wrapper
// (`internal/llm/retry/retry.go`) so observers (audit, memory) see a
// coherent conversation history regardless of which layer triggered
// the re-ask. We do NOT impersonate the assistant when authoring the
// correction — the rejected response IS the assistant turn; our
// addition is a user message.
//
// `complaint` is the focused correction string. The shape is:
//
//	"Your previous response failed validation:
//	 - tool=`foo` arg-validation: missing required field `bar`
//	Please respond again, addressing this issue exactly."
//
// Phase 44 intentionally does NOT add a system message — the
// existing system prompt (the planner's tool catalog + schema
// guidance) is unchanged across the loop. Layering corrective system
// messages on every attempt would dilute the original system context.
func appendCorrectiveTurn(req llm.CompleteRequest, badResp llm.CompleteResponse, complaint string) llm.CompleteRequest {
	out := req
	clone := make([]llm.ChatMessage, 0, len(req.Messages)+2)
	clone = append(clone, req.Messages...)

	// Echo the assistant's rejected output back into the thread so
	// the model sees what was wrong with its own response.
	if badResp.Content != "" {
		assistantContent := badResp.Content
		clone = append(clone, llm.ChatMessage{
			Role:    llm.RoleAssistant,
			Content: llm.Content{Text: &assistantContent},
		})
	}

	user := complaint
	clone = append(clone, llm.ChatMessage{
		Role:    llm.RoleUser,
		Content: llm.Content{Text: &user},
	})
	out.Messages = clone
	return out
}

// formatArgsCorrection builds the focused corrective complaint for a
// schema-rejected [planner.CallTool]. Names the tool + the
// validator's error verbatim (truncated). The model sees a precise
// reason it can act on.
func formatArgsCorrection(toolName string, validatorErr error) string {
	return fmt.Sprintf(
		"Your previous response failed validation: tool=`%s` arg-validation: %s. "+
			"Please respond again with a corrected `args` object that satisfies the tool's schema.",
		safeName(toolName),
		truncate(validatorErr.Error()),
	)
}

// parserCorrection builds the focused corrective complaint when the
// parser found no actions (or only invalid-shape ones). The model is
// asked to re-emit a single JSON envelope matching the Harbor action
// shape — the parser's failure mode is well-known (the envelope
// shape) so a focused complaint is more useful than a generic "try
// again."
func parserCorrection(parseErr error) string {
	if parseErr == nil {
		parseErr = ErrNoActionsFound
	}
	return fmt.Sprintf(
		"Your previous response failed to parse as a Harbor tool-call envelope (`%s`). "+
			"Please respond with a single JSON object of the shape "+
			"`{\"tool\": \"<name>\", \"args\": {...}, \"reasoning\": \"...\"}` "+
			"(or a JSON array of such objects for multi-action plans).",
		truncate(parseErr.Error()),
	)
}
