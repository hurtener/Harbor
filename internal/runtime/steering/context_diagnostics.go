package steering

import (
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

// contextHistory is read only after candidate publication has released the
// inspection lock. A failed candidate cannot masquerade as installed coverage.
func contextHistory(spec RunSpec, rc planner.RunContext) *llm.ContextHistory {
	if spec.TrajectoryMu != nil {
		spec.TrajectoryMu.RLock()
		defer spec.TrajectoryMu.RUnlock()
	}
	tr := rc.Trajectory
	if tr == nil {
		return nil
	}
	start, err := tr.ReplayStart()
	if err != nil {
		// The request builder owns invalid-history failure. Diagnostics must
		// not manufacture trustworthy coverage for an invalid checkpoint.
		return nil
	}
	history := &llm.ContextHistory{ReplayStart: start, ReplayEnd: len(tr.Steps)}
	if summary := tr.ActiveSummary(); summary != nil && summary.Coverage != nil {
		history.CheckpointVersion = summary.Coverage.Version
		history.CheckpointGeneration = summary.Coverage.Generation
	}
	if tr.UnseenFrom != nil && *tr.UnseenFrom >= 0 && *tr.UnseenFrom <= len(tr.Steps) {
		history.UnseenKnown = true
		history.UnseenFrom = *tr.UnseenFrom
	}
	return history
}
