package serve

import (
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

func applyTaskLLMSettings(base *planner.LLMOverrides, settings *llm.RunSettings) *planner.LLMOverrides {
	if settings == nil {
		return base
	}
	out := &planner.LLMOverrides{}
	if base != nil {
		*out = *base
	}
	s := llm.CloneRunSettings(settings)
	if s.Model != nil {
		out.Model = s.Model
	}
	if s.ReasoningEffort != nil {
		out.ReasoningEffort = s.ReasoningEffort
	}
	if s.MaxTokens != nil {
		out.MaxTokens = s.MaxTokens
	}
	return out
}
