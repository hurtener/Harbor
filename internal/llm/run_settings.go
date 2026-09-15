package llm

import (
	"fmt"
	"strings"
)

// RunSettings is an immutable, task-bound selection. It is execution input,
// never a saved user preference. Nil preserves legacy next-message behavior.
type RunSettings struct {
	Model           *string `json:"model,omitempty"`
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
	MaxTokens       *int    `json:"max_tokens,omitempty"`
}

// ValidateRunSettings rejects malformed settings and ambiguous model sources.
// Provider routes remain authoritative for routed models and capabilities.
func ValidateRunSettings(s *RunSettings, route *ProviderRoute) error {
	if s == nil {
		return nil
	}
	if s.Model != nil {
		if route != nil {
			return fmt.Errorf("llm_settings.model and provider_route are mutually exclusive")
		}
		if strings.TrimSpace(*s.Model) == "" || strings.TrimSpace(*s.Model) != *s.Model || len(*s.Model) > 512 {
			return fmt.Errorf("llm_settings.model must be a non-empty model name of at most 512 bytes")
		}
	}
	if s.ReasoningEffort != nil {
		switch ReasoningEffort(*s.ReasoningEffort) {
		case "", ReasoningOff, ReasoningLow, ReasoningMedium, ReasoningHigh:
		default:
			return fmt.Errorf("llm_settings.reasoning_effort must be empty, off, low, medium, or high")
		}
	}
	if s.MaxTokens != nil && *s.MaxTokens <= 0 {
		return fmt.Errorf("llm_settings.max_tokens must be positive")
	}
	return nil
}

// CloneRunSettings detaches every optional scalar from its caller.
func CloneRunSettings(s *RunSettings) *RunSettings {
	if s == nil {
		return nil
	}
	out := *s
	if s.Model != nil {
		v := *s.Model
		out.Model = &v
	}
	if s.ReasoningEffort != nil {
		v := *s.ReasoningEffort
		out.ReasoningEffort = &v
	}
	if s.MaxTokens != nil {
		v := *s.MaxTokens
		out.MaxTokens = &v
	}
	return &out
}
