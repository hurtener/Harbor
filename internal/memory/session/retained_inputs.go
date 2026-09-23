package session

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/planner"
)

const retainedInputRefsKey = "retained_input_refs"

// ValidateRetainedInputs prevents opt-in continuity from silently accepting an
// input that the legacy best-effort materializer omitted. Non-retained callers
// keep their existing input policy. The guard separately verifies current scope
// and lifetime before inference and dependent dispatch.
func ValidateRetainedInputs(requested []string, resolved []planner.InputArtifactView) error {
	if len(requested) > maxRetainedResultRefs || len(resolved) > maxRetainedResultRefs {
		return ErrRetainedContextCapacity
	}
	ids := make(map[string]bool, len(requested))
	for _, id := range requested {
		if !validRetainedInputID(id) {
			return ErrRetainedContextUnavailable
		}
		ids[id] = false
	}
	for _, view := range resolved {
		if _, ok := ids[view.ID]; !ok {
			return ErrRetainedContextUnavailable
		}
		ids[view.ID] = true
	}
	for _, found := range ids {
		if !found {
			return ErrRetainedContextUnavailable
		}
	}
	return nil
}

func validRetainedInputID(id string) bool {
	return id != "" && len(id) <= 256 && strings.TrimSpace(id) == id && utf8.ValidString(id)
}

// Store only attachment identity in an ordinary context frame. The existing
// artifact store owns bytes and current metadata. This entry associates those
// references with their user turn without claiming the model inspected them.
func retainedInputContext(inputs []planner.InputArtifactView) (*planner.Step, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if len(inputs) > maxRetainedResultRefs {
		return nil, ErrRetainedContextCapacity
	}
	ids := make([]string, 0, len(inputs))
	seen := make(map[string]bool, len(inputs))
	for _, input := range inputs {
		if !validRetainedInputID(input.ID) {
			return nil, ErrRetainedContextUnavailable
		}
		if !seen[input.ID] {
			seen[input.ID] = true
			ids = append(ids, input.ID)
		}
	}
	data, err := json.Marshal(map[string]any{
		retainedInputRefsKey: ids,
		"context_notice":     "Attachments supplied with this user request. References only, not proof that their contents or images were inspected. Retrieve authorized content when needed.",
	})
	if err != nil {
		return nil, ErrRetainedContextUnavailable
	}
	return &planner.Step{LLMObservation: json.RawMessage(data)}, nil
}
