// Package posture derives Runtime health and diagnostics from Protocol values.
package posture

import (
	"strings"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/surface"
)

// State is the canonical Runtime, governance, LLM, and diagnostics projection.
type State struct {
	Surfaces   map[string]surface.State
	Info       types.RuntimeInfo
	Health     types.RuntimeHealth
	Counters   types.RuntimeCounters
	Drivers    types.RuntimeDrivers
	Metrics    types.MetricsSnapshot
	Governance types.GovernancePostureResponse
	LLM        types.LLMPostureResponse
	Partial    []string
}

// Attention is derived only from canonical health and stream state.
func (s State) Attention(stream string, interventions, failedTasks int) (level, label string) {
	if stream != "live" {
		return "error", "stream " + stream
	}
	if interventions > 0 {
		return "warning", "intervention required"
	}
	if failedTasks > 0 {
		return "warning", "task failure"
	}
	for name, state := range s.Surfaces {
		if state.Status == surface.Failed || state.Status == surface.Unavailable {
			return "warning", name + " " + state.Label()
		}
		if state.Status == surface.Partial || strings.Contains(strings.ToLower(state.Reason), "trunc") || strings.Contains(strings.ToLower(state.Reason), "replay") || strings.Contains(strings.ToLower(state.Reason), "retention") {
			return "warning", name + " " + state.Label()
		}
	}
	for _, subsystem := range s.Health.Subsystems {
		if subsystem.Status != types.HealthStatusReady {
			return "warning", subsystem.Subsystem + " " + subsystem.Status
		}
	}
	return "success", "healthy"
}
