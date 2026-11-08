package react

import (
	"context"
	"strings"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// availableToolsMarker opens the first per-catalog system section. Every
// default-builder section before it is static for the agent's configuration
// (the date lives in the trailing <current_date>), so the text before it
// is the system prompt's stable spine.
const availableToolsMarker = "\n\n<available_tools>\n"

// promptCacheScope derives the opaque prompt-cache scope for one planner
// call. It covers exactly the prefix that stays unchanged across one
// agent's turns:
//
//   - the tenant and the acting agent (hashed, never sent in clear);
//   - the planner-control declarations plus the always-loaded tools below
//     the user-owned tier — user-owned connections and per-run discoveries
//     follow them in the declaration order and are deliberately excluded,
//     so a lazily attached connection or a tool_search result neither
//     changes the key mid-run nor moves the shared prefix to another
//     cache shard;
//   - the static system-prompt spine (for the default builder; a custom
//     builder's prompt is opaque and contributes nothing).
//
// listed is the number of leading projected tools that came from the
// always-loaded List (see projectModelToolsListed).
func promptCacheScope(ctx context.Context, rc planner.RunContext, req llm.CompleteRequest, projected []tools.Tool, listed int) string {
	agent, ok := tools.EffectiveAgentConfigFrom(ctx)
	if !ok {
		agent, _ = tools.InvokingAgentFrom(ctx)
	}
	return llm.PromptCacheScope(llm.PromptCacheScopeInput{
		TenantID:     rc.Quadruple.TenantID,
		AgentID:      agent,
		ToolsDigest:  stableToolsDigest(req.Tools, projected, listed),
		SystemDigest: llm.DigestParts(systemSpine(req.Messages)),
	})
}

// stableToolsDigest digests the stable leading declarations: the planner
// controls, then every always-loaded tool below the user-owned tier.
// Declarations are reserved controls followed by projected tools in order,
// and projected tools are tier-ordered, so the stable set is a prefix.
func stableToolsDigest(decls []llm.ToolDeclaration, projected []tools.Tool, listed int) string {
	stable := 0
	for _, t := range projected[:min(listed, len(projected))] {
		if tools.PromptStabilityTierOf(t) >= tools.PromptTierUser {
			break
		}
		stable++
	}
	n := min(len(decls), len(reservedPlannerControlDeclarations())+stable)
	parts := make([]string, 0, 3*n)
	for _, d := range decls[:n] {
		parts = append(parts, d.Name, d.Description, string(llm.StripSchemaAnnotations(d.Schema)))
	}
	return llm.DigestParts(parts...)
}

// systemSpine returns the leading static part of the base system message:
// the text before the first per-catalog section, or "" when the request was
// not shaped by the default builder.
func systemSpine(messages []llm.ChatMessage) string {
	if len(messages) == 0 || messages[0].Role != llm.RoleSystem || messages[0].Content.Text == nil {
		return ""
	}
	text := *messages[0].Content.Text
	idx := strings.Index(text, availableToolsMarker)
	if idx < 0 {
		return ""
	}
	return text[:idx]
}
