package agentcfg

import "strconv"

// RunLimitsSection replaces the runtime-default per-run limits for an agent's
// subsequent runs. A present section is authoritative: a nil field means no
// limit for that dimension. An absent section inherits `runtime.run_limits`.
// Set values are positive; MaxDurationSeconds is active wall-clock seconds
// and MaxTokens is cumulative prompt plus completion tokens per run.
type RunLimitsSection struct {
	MaxDurationSeconds *int64 `json:"max_duration_seconds,omitempty"`
	MaxTokens          *int64 `json:"max_tokens,omitempty"`
}

// Clone detaches the section from its caller.
func (s *RunLimitsSection) Clone() *RunLimitsSection {
	if s == nil {
		return nil
	}
	out := &RunLimitsSection{}
	if s.MaxDurationSeconds != nil {
		v := *s.MaxDurationSeconds
		out.MaxDurationSeconds = &v
	}
	if s.MaxTokens != nil {
		v := *s.MaxTokens
		out.MaxTokens = &v
	}
	return out
}

// RunLimitsDiff compares the explicit limits. Empty means the section is
// absent (inherits the runtime default); "none" means present without a
// limit for that dimension.
type RunLimitsDiff struct {
	MaxDurationChanged bool
	MaxDurationFrom    string
	MaxDurationTo      string
	MaxTokensChanged   bool
	MaxTokensFrom      string
	MaxTokensTo        string
}

// Changed reports whether either limit or its inheritance changed.
func (d RunLimitsDiff) Changed() bool { return d.MaxDurationChanged || d.MaxTokensChanged }

// DiffRunLimits compares two payloads' run-limit sections.
func DiffRunLimits(from, to ConfigPayload) RunLimitsDiff {
	render := func(s *RunLimitsSection, pick func(*RunLimitsSection) *int64) string {
		if s == nil {
			return ""
		}
		if v := pick(s); v != nil {
			return strconv.FormatInt(*v, 10)
		}
		return "none"
	}
	duration := func(s *RunLimitsSection) *int64 { return s.MaxDurationSeconds }
	tokens := func(s *RunLimitsSection) *int64 { return s.MaxTokens }
	d := RunLimitsDiff{
		MaxDurationFrom: render(from.RunLimits, duration), MaxDurationTo: render(to.RunLimits, duration),
		MaxTokensFrom: render(from.RunLimits, tokens), MaxTokensTo: render(to.RunLimits, tokens),
	}
	d.MaxDurationChanged = d.MaxDurationFrom != d.MaxDurationTo
	d.MaxTokensChanged = d.MaxTokensFrom != d.MaxTokensTo
	return d
}
