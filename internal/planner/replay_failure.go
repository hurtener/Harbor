package planner

// ReplayStepFailed identifies the terminal failure surfaces that forbid
// replaying raw action arguments. This is shared by retained capture and ReAct.
func ReplayStepFailed(step Step) bool {
	if step.Failure != nil || step.Error != "" {
		return true
	}
	observation, ok := step.LLMObservation.(map[string]any)
	if !ok {
		return false
	}
	if _, ok := observation["error"]; !ok {
		return false
	}
	for _, key := range []string{ObservationClassKey, ObservationMCPClassKey, ObservationPolicyClassKey, "result"} {
		if _, ok := observation[key]; ok {
			return true
		}
	}
	return false
}
