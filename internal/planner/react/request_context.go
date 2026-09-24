package react

import (
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// PreparesRequestContext keeps runtime compaction at the assembled-request
// boundary rather than estimating a serialized trajectory before this planner.
func (*ReActPlanner) PreparesRequestContext() {}

var _ planner.RequestContextPlanner = (*ReActPlanner)(nil)

func (p *ReActPlanner) buildContextRequest(rc planner.RunContext, projected []tools.Tool) (llm.CompleteRequest, error) {
	if builder, ok := p.builder.(defaultBuilder); ok {
		return builder.buildRequestWithProjectedTools(rc, p.systemPrompt, projected)
	}
	return p.builder.Build(rc, p.systemPrompt), nil
}

func (p *ReActPlanner) rebuildMessages(rc planner.RunContext, projected []tools.Tool) func() ([]llm.ChatMessage, error) {
	return func() ([]llm.ChatMessage, error) {
		// A rebuild is not another planner decision or guidance injection.
		rc.Emit = nil
		req, err := p.buildContextRequest(rc, projected)
		return req.Messages, err
	}
}
