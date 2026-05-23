// cmd/harbor/cmd_dev_executor.go — the dev binary's `steering.ToolExecutor`
// implementation. Phase 83i (D-152).
//
// Before 83i the runloop's default case dropped every planner CallTool
// decision on the floor (Phase 53's deliberately-punted scope), which
// made multi-step ReAct structurally broken against real LLMs because
// the planner never saw tool observations. The audit pinned this as
// the root cause of the "64 steps, 0 tools called" failure mode that
// surfaced in the v1.1 operator validation.
//
// devToolExecutor closes that seam against the production
// `tools.ToolCatalog`:
//
//   - CallTool: look up the descriptor by name, call Invoke under
//     the per-step ctx, return the typed ToolResult.Value (plus Meta)
//     as the observation. The planner's next step sees this on
//     `RunContext.Trajectory.Steps[N].Observation`.
//
//   - CallParallel / SpawnTask / AwaitTask: V1.1 returns
//     ErrDecisionShapeUnsupported. The runloop surfaces this to the
//     planner as the step's observation, and the planner can choose
//     a different path (most ReAct planners will repair to a serial
//     CallTool or Finish).
//
// The executor is constructed once per dev-stack and shared across
// every run; it holds only the catalog (immutable after construction)
// + a logger, so the D-025 reuse contract is trivially satisfied.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tools"
)

// devToolExecutor is the dev binary's production `steering.ToolExecutor`
// implementation. Concurrent-safe — the catalog + artifact store are
// immutable after the dev-stack boot wiring runs.
//
// D-026 heavy-content discipline: tool results whose JSON encoding
// exceeds `heavyThreshold` get stored in the artifact store; the
// runloop's trajectory append uses the small llmObservation summary
// so the next LLM prompt never carries raw heavy content. Before
// this discipline the v1.1 operator validation hit `ErrContextLeak`
// after the first tool call (the youtube_get_metadata tool returns
// ~1.5 MB which would otherwise reach the LLM verbatim).
type devToolExecutor struct {
	cat            tools.ToolCatalog
	artifacts      artifacts.ArtifactStore
	heavyThreshold int
	logger         *slog.Logger
}

// newDevToolExecutor binds the catalog + artifact store the runloop
// dispatches against. Both are the SAME instances bootDevStack
// already constructs. `heavyThreshold` is the operator-configured
// cfg.Artifacts.HeavyOutputThresholdBytes; tool results whose JSON
// encoding exceeds it get promoted to ArtifactStub-shaped
// llmObservations.
func newDevToolExecutor(cat tools.ToolCatalog, artStore artifacts.ArtifactStore, heavyThreshold int, logger *slog.Logger) *devToolExecutor {
	if logger == nil {
		logger = slog.Default()
	}
	if heavyThreshold <= 0 {
		heavyThreshold = 32 * 1024 // safety floor matches Wave 11 default
	}
	return &devToolExecutor{
		cat:            cat,
		artifacts:      artStore,
		heavyThreshold: heavyThreshold,
		logger:         logger,
	}
}

// ExecuteDecision implements `steering.ToolExecutor`. Dispatches the
// decision per its shape:
//
//   - CallTool: catalog.Resolve(name) → descriptor.Invoke(ctx, args)
//     under the run's identity-scoped ctx. The raw `observation` is
//     the typed ToolResult.Value; the `llmObservation` is the same
//     value unless the encoded result exceeds `heavyThreshold`, in
//     which case it gets promoted to an artifact-backed summary.
//   - CallParallel / SpawnTask / AwaitTask: ErrDecisionShapeUnsupported.
//     The runloop wraps this as the step's observation; the planner
//     re-plans (typically repairs to a serial CallTool).
func (e *devToolExecutor) ExecuteDecision(ctx context.Context, rc planner.RunContext, decision planner.Decision) (any, any, error) {
	switch d := decision.(type) {
	case planner.CallTool:
		return e.callTool(ctx, rc, d)
	case planner.CallParallel:
		return nil, nil, fmt.Errorf("%w: CallParallel (parallel-execution dispatcher lands post-V1.1)",
			steering.ErrDecisionShapeUnsupported)
	case planner.SpawnTask:
		return nil, nil, fmt.Errorf("%w: SpawnTask (background-task dispatcher lands post-V1.1)",
			steering.ErrDecisionShapeUnsupported)
	case planner.AwaitTask:
		return nil, nil, fmt.Errorf("%w: AwaitTask (background-task dispatcher lands post-V1.1)",
			steering.ErrDecisionShapeUnsupported)
	default:
		return nil, nil, fmt.Errorf("%w: %T", steering.ErrDecisionShapeUnsupported, decision)
	}
}

// callTool dispatches a single CallTool. Errors:
//   - tool name does not resolve → wrapped tools.ErrToolNotFound.
//   - descriptor.Invoke returns an error → wrapped + surfaced.
//   - the result's Value is the observation the planner sees on its
//     next step (after D-026 heavy-content projection).
func (e *devToolExecutor) callTool(ctx context.Context, rc planner.RunContext, d planner.CallTool) (any, any, error) {
	if d.Tool == "" {
		return nil, nil, errors.New("CallTool.Tool is empty")
	}
	desc, ok := e.cat.Resolve(d.Tool)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %q", tools.ErrToolNotFound, d.Tool)
	}
	if desc.Invoke == nil {
		return nil, nil, fmt.Errorf("tool %q is registered without an Invoke function", d.Tool)
	}
	result, err := desc.Invoke(ctx, d.Args)
	if err != nil {
		e.logger.Warn("devToolExecutor: tool invoke failed",
			slog.String("tool", d.Tool),
			slog.String("err", err.Error()))
		return nil, nil, fmt.Errorf("tool %q invoke: %w", d.Tool, err)
	}
	raw := result.Value
	if raw == nil && len(result.Meta) > 0 {
		raw = map[string]any{"meta": result.Meta}
	}
	llmObs := e.projectForLLM(ctx, rc, d.Tool, raw)
	return raw, llmObs, nil
}

// projectForLLM applies D-026 heavy-content discipline to the tool
// result before it reaches the planner's next-step renderer. When the
// JSON encoding is small enough (< heavyThreshold), the projection is
// the raw value (the planner sees the full result). When it exceeds
// the threshold, the projection is a summary map referencing an
// ArtifactRef stored in the artifact store — the planner sees a small
// representation + a stable id it can mention in its Finish answer.
//
// If artifact storage is unavailable OR fails, the projection
// degrades to a truncated string preview. We log a Warn (silent
// degradation is forbidden) but do not fail the run — losing a
// tool's full result is recoverable in the planner's eyes.
func (e *devToolExecutor) projectForLLM(ctx context.Context, rc planner.RunContext, tool string, raw any) any {
	if raw == nil {
		return raw
	}
	encoded, encErr := json.Marshal(raw)
	if encErr != nil {
		// Marshaling failure: hand the planner a summary string so
		// SOMETHING lands — silent context loss is §13-forbidden.
		return map[string]any{
			"tool":  tool,
			"error": fmt.Sprintf("observation could not be JSON-encoded: %v", encErr),
		}
	}
	size := len(encoded)
	if size < e.heavyThreshold {
		return raw
	}
	// Heavy result — promote to artifact store.
	if e.artifacts == nil {
		// No artifact store wired (degraded dev stack) — truncate
		// loudly so the operator can see exactly what was elided.
		e.logger.Warn("devToolExecutor: heavy tool result without artifact store; truncating",
			slog.String("tool", tool),
			slog.Int("size_bytes", size))
		return heavyTruncationSummary(tool, encoded, size, "")
	}
	scope := artifacts.ArtifactScope{
		TenantID:  rc.Quadruple.TenantID,
		UserID:    rc.Quadruple.UserID,
		SessionID: rc.Quadruple.SessionID,
	}
	ref, putErr := e.artifacts.PutText(ctx, scope, string(encoded), artifacts.PutOpts{
		Filename: fmt.Sprintf("tool-result-%s.json", tool),
		MimeType: "application/json",
		Source: map[string]any{
			"producer": "dev-tool-executor",
			"tool":     tool,
			"run_id":   rc.Quadruple.RunID,
		},
	})
	if putErr != nil {
		e.logger.Warn("devToolExecutor: artifact PutText failed; truncating",
			slog.String("tool", tool),
			slog.Int("size_bytes", size),
			slog.String("err", putErr.Error()))
		return heavyTruncationSummary(tool, encoded, size, "")
	}
	return heavyTruncationSummary(tool, encoded, size, ref.ID)
}

// heavyTruncationSummary builds the small llmObservation the planner
// renders for a heavy tool result. Carries: the tool name, the byte
// size of the full result, a short preview (first ~512 chars of the
// JSON-encoded result), and the artifact ID when available.
func heavyTruncationSummary(tool string, encoded []byte, size int, artifactID string) any {
	const previewBytes = 512
	prev := string(encoded)
	if len(prev) > previewBytes {
		prev = prev[:previewBytes] + "...(truncated)"
	}
	out := map[string]any{
		"tool":       tool,
		"size_bytes": size,
		"truncated":  true,
		"preview":    prev,
	}
	if artifactID != "" {
		out["artifact_ref"] = artifactID
	}
	return out
}

// _ avoids the "identity imported but not used" warning when callers
// don't reference identity types directly.
var _ = identity.Identity{}

// ensure interface satisfaction at compile time.
var _ steering.ToolExecutor = (*devToolExecutor)(nil)
