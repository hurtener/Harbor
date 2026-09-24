package protocol

import "errors"

// ErrInvalidMemory rejects unsupported or invalid working-input budget edits.
var ErrInvalidMemory = errors.New("agentcfg/protocol: memory budget requires a configured compactor and nonnegative budget_tokens")

// WithMemoryBudget enables versioned budget edits when the run driver has a
// configured compactor. It does not enable memory or create an LLM client.
func WithMemoryBudget(available bool) Option {
	return func(s *Service) { s.memoryBudgetAvailable = available }
}
