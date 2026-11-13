package types

// AgentConfigRunLimits replaces the runtime-default per-run limits for an
// agent's subsequent runs. A present section is authoritative: an omitted
// field means no limit for that dimension; an absent section inherits the
// runtime default. Set values are positive. MaxDurationSeconds is active
// wall-clock seconds (time waiting on a pause is excluded); MaxTokens is the
// cumulative prompt plus completion tokens of every model call in the run.
// A run that reaches a limit completes with finish reason limit_reached and
// a bounded partial answer.
type AgentConfigRunLimits struct {
	MaxDurationSeconds *int64 `json:"max_duration_seconds,omitempty"`
	MaxTokens          *int64 `json:"max_tokens,omitempty"`
}

// AgentConfigRunLimitsDiff compares explicit limits. An empty value means
// the section is absent (inherits the runtime default); "none" means the
// section is present without a limit for that dimension.
type AgentConfigRunLimitsDiff struct {
	MaxDurationChanged bool   `json:"max_duration_changed"`
	MaxDurationFrom    string `json:"max_duration_from"`
	MaxDurationTo      string `json:"max_duration_to"`
	MaxTokensChanged   bool   `json:"max_tokens_changed"`
	MaxTokensFrom      string `json:"max_tokens_from"`
	MaxTokensTo        string `json:"max_tokens_to"`
}

// RunLimitsRequest narrows the effective run limits for one started run.
// Each set value is positive and can only tighten the limit the agent or
// runtime already applies; it never loosens or removes one.
type RunLimitsRequest struct {
	MaxDurationSeconds int64 `json:"max_duration_seconds,omitempty"`
	MaxTokens          int64 `json:"max_tokens,omitempty"`
}
