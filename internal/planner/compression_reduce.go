package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
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
// maintenance cannot run. It never touches the latest exchange.
//
// Stage one walks the uncovered exchanges oldest first and offers each large
// model-facing observation as a bounded preview plus a reference to its
// retained exact text (failures and existing previews are skipped; a live
// step's raw Observation is unchanged). If the request still does not fit,
// stage two collapses whole exchanges oldest first — call, arguments,
// preamble and result — into one context marker that references the retained
// exchange. It reports how many replacements install accepted and whether
// the request fits.
func (r *CompressionRunner) ReduceOldestObservations(ctx context.Context, rc RunContext, tr *Trajectory, install ObservationReduction) (int, bool, error) {
	if err := assertCompressionIdentity(rc); err != nil {
		return 0, false, err
	}
	if tr == nil {
		return 0, false, ErrNilTrajectory
	}
	start, err := tr.ReplayStart()
	if err != nil {
		return 0, false, err
	}
	reduced := 0
	for stage := range 2 {
		for index := start; index < len(tr.Steps)-1; index++ {
			if err := ctx.Err(); err != nil {
				return reduced, false, err
			}
			var replacement Step
			var ok bool
			if stage == 0 {
				replacement, ok = r.reduceStep(ctx, rc, index, tr.Steps[index])
			} else {
				replacement, ok = r.collapseStep(ctx, rc, tr.Steps[index])
			}
			if !ok {
				continue
			}
			done, err := install(index, replacement)
			if err != nil {
				return reduced, false, err
			}
			reduced++
			if done {
				return reduced, true, nil
			}
		}
	}
	return reduced, false, nil
}

// collapseStep replaces one whole exchange with a small context marker that
// references the retained exact exchange. Context-only steps and markers are
// left alone.
func (r *CompressionRunner) collapseStep(ctx context.Context, rc RunContext, step Step) (Step, bool) {
	view := step
	if step.Historical != nil {
		var err error
		if view, err = ReadHistoricalStep(step); err != nil {
			return Step{}, false
		}
	}
	if view.Action == nil {
		return Step{}, false
	}
	encoded, err := json.Marshal(trajectory.ModelStep(view))
	if err != nil {
		return Step{}, false
	}
	tool := toolNameOf(view.Action)
	marker := map[string]any{
		"collapsed_exchange": true,
		"tool":               tool,
		"size_bytes":         len(encoded),
		"note":               "An earlier " + tool + " exchange was removed to fit the context window.",
	}
	if ref := r.retain(ctx, rc, tool, encoded); ref != "" {
		marker["artifact_ref"] = ref
	} else {
		marker["remainder_not_retained"] = true
	}
	if step.Historical == nil {
		return Step{LLMObservation: marker}, true
	}
	body, err := json.Marshal(map[string]any{"llm_observation": marker})
	if err != nil {
		return Step{}, false
	}
	historical := *step.Historical
	historical.Kind, historical.Body = "context", body
	out := Step{Historical: &historical}
	if _, err := ReadHistoricalStep(out); err != nil {
		return Step{}, false
	}
	return out, true
}

// retain stores exact evidence under the run's isolation triple and returns
// its reference, or "" when no store is configured or storage fails: losing
// the reference must not fail the run, and the marker then says so.
func (r *CompressionRunner) retain(ctx context.Context, rc RunContext, tool string, encoded []byte) string {
	if r.artifacts == nil {
		return ""
	}
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
		slog.WarnContext(ctx, "planner context reduction: evidence not retained", slog.String("run_id", rc.Quadruple.RunID))
		return ""
	}
	return ref.ID
}

func (r *CompressionRunner) reduceStep(ctx context.Context, rc RunContext, _ int, step Step) (Step, bool) {
	view := step
	if step.Historical != nil {
		var err error
		if view, err = ReadHistoricalStep(step); err != nil {
			return Step{}, false
		}
	}
	if ReplayStepFailed(view) {
		return Step{}, false
	}
	observation := view.LLMObservation
	if observation == nil {
		observation = view.Observation
	}
	if observation == nil || isReducedObservation(observation) {
		return Step{}, false
	}
	encoded, err := json.Marshal(observation)
	if err != nil || len(encoded) < ReducibleObservationBytes {
		return Step{}, false
	}
	tool := toolNameOf(view.Action)
	small := map[string]any{
		"tool":            tool,
		"size_bytes":      len(encoded),
		"truncated":       true,
		"context_reduced": true,
		"preview":         clipUTF8(string(encoded), ReducedObservationPreviewBytes),
	}
	if ref := r.retain(ctx, rc, tool, encoded); ref != "" {
		small["artifact_ref"] = ref
	} else {
		small["remainder_not_retained"] = true
	}
	if step.Historical == nil {
		out := step
		out.LLMObservation = small
		return out, true
	}
	body, err := rewriteHistoricalObservation(step.Historical.Body, small)
	if err != nil {
		return Step{}, false
	}
	historical := *step.Historical
	historical.Body = body
	out := Step{Historical: &historical}
	if _, err := ReadHistoricalStep(out); err != nil {
		return Step{}, false
	}
	return out, true
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
