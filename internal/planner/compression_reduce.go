package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/artifacts"
)

const (
	// ReducibleObservationBytes is the smallest encoded model-facing
	// observation the deterministic context reduction replaces. Smaller
	// observations cost less than their preview plus reference.
	ReducibleObservationBytes = 4 * 1024
	// ReducedObservationPreviewBytes bounds the head preview kept in place of
	// a reduced observation.
	ReducedObservationPreviewBytes = 2 * 1024
)

// WithCompressionArtifactStore lets deterministic context reduction retain the
// exact observation it replaces, so the model keeps a reference it can read
// back. Without a store the reduced form states the remainder was not retained.
func WithCompressionArtifactStore(store artifacts.ArtifactStore) CompressionOption {
	return func(r *CompressionRunner) {
		r.artifacts = store
	}
}

// ObservationReduction installs one reduced exchange. It returns done=true once
// the caller's request fits; an error stops the reduction.
type ObservationReduction func(index int, reduced Step) (done bool, err error)

// ReduceOldestObservations is the deterministic fallback when summary
// maintenance cannot run. It walks the uncovered exchanges oldest first,
// always keeping the latest exchange intact, skips failures and observations
// that are already references, and offers each large model-facing observation
// as a bounded preview plus a reference to its retained exact text. The raw
// Observation of a live step is unchanged; only the model projection shrinks.
// It returns how many exchanges install accepted.
func (r *CompressionRunner) ReduceOldestObservations(ctx context.Context, rc RunContext, tr *Trajectory, install ObservationReduction) (int, error) {
	if err := assertCompressionIdentity(rc); err != nil {
		return 0, err
	}
	if tr == nil {
		return 0, ErrNilTrajectory
	}
	start, err := tr.ReplayStart()
	if err != nil {
		return 0, err
	}
	reduced := 0
	for index := start; index < len(tr.Steps)-1; index++ {
		if err := ctx.Err(); err != nil {
			return reduced, err
		}
		replacement, ok, err := r.reduceStep(ctx, rc, index, tr.Steps[index])
		if err != nil {
			return reduced, err
		}
		if !ok {
			continue
		}
		done, err := install(index, replacement)
		if err != nil {
			return reduced, err
		}
		reduced++
		if done {
			return reduced, nil
		}
	}
	return reduced, nil
}

func (r *CompressionRunner) reduceStep(ctx context.Context, rc RunContext, index int, step Step) (Step, bool, error) {
	view := step
	if step.Historical != nil {
		var err error
		if view, err = ReadHistoricalStep(step); err != nil {
			return Step{}, false, nil //nolint:nilerr // unreadable history is skipped, never rewritten
		}
	}
	if ReplayStepFailed(view) {
		return Step{}, false, nil
	}
	observation := view.LLMObservation
	if observation == nil {
		observation = view.Observation
	}
	if observation == nil || isReducedObservation(observation) {
		return Step{}, false, nil
	}
	encoded, err := json.Marshal(observation)
	if err != nil || len(encoded) < ReducibleObservationBytes {
		return Step{}, false, nil //nolint:nilerr // an unencodable or small observation is skipped, not reduced
	}
	tool := toolNameOf(view.Action)
	small := map[string]any{
		"tool":            tool,
		"size_bytes":      len(encoded),
		"truncated":       true,
		"context_reduced": true,
		"preview":         clipUTF8(string(encoded), ReducedObservationPreviewBytes),
	}
	if r.artifacts != nil {
		ref, err := r.artifacts.PutText(ctx, artifacts.ArtifactScope{
			TenantID: rc.Quadruple.TenantID, UserID: rc.Quadruple.UserID,
			SessionID: rc.Quadruple.SessionID, TaskID: rc.Quadruple.RunID,
		}, string(encoded), artifacts.PutOpts{
			MimeType: "application/json",
			Filename: fmt.Sprintf("tool-result-%s.json", tool),
			Source: map[string]any{
				"source":     "tool",
				"producer":   "context-reduction",
				"tool":       tool,
				"run_id":     rc.Quadruple.RunID,
				"created_at": time.Now().UTC(),
			},
		})
		if err != nil {
			return Step{}, false, fmt.Errorf("planner context reduction: retain exchange %d: %w", index+1, err)
		}
		small["artifact_ref"] = ref.ID
	} else {
		small["remainder_not_retained"] = true
	}
	if step.Historical == nil {
		out := step
		out.LLMObservation = small
		return out, true, nil
	}
	body, err := rewriteHistoricalObservation(step.Historical.Body, small)
	if err != nil {
		return Step{}, false, nil //nolint:nilerr // a body that cannot be rewritten is skipped, never altered
	}
	historical := *step.Historical
	historical.Body = body
	out := Step{Historical: &historical}
	if _, err := ReadHistoricalStep(out); err != nil {
		return Step{}, false, nil //nolint:nilerr // an invalid rewrite is discarded; the original stays
	}
	return out, true, nil
}

// rewriteHistoricalObservation replaces only the model-facing observation of a
// retained exchange body, keeping every other canonical field byte-exact.
func rewriteHistoricalObservation(body []byte, observation any) ([]byte, error) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		return nil, err
	}
	fields["llm_observation"] = encoded
	return json.Marshal(fields)
}

// isReducedObservation reports an observation that is already a bounded
// preview (a stored heavy result or an earlier reduction).
func isReducedObservation(observation any) bool {
	m, ok := observation.(map[string]any)
	if !ok {
		return false
	}
	_, preview := m["preview"].(string)
	truncated, ok := m["truncated"].(bool)
	return preview && ok && truncated
}

func toolNameOf(action any) string {
	switch a := action.(type) {
	case CallTool:
		if a.Tool != "" {
			return a.Tool
		}
	case *CallTool:
		if a != nil && a.Tool != "" {
			return a.Tool
		}
	case map[string]any:
		for _, key := range []string{"Tool", "tool"} {
			if name, ok := a[key].(string); ok && name != "" {
				return name
			}
		}
	}
	return "context"
}

func clipUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
