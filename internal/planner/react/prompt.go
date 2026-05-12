package react

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// defaultBuilder is the in-package [PromptBuilder]. It produces a
// three-block conversation:
//
//  1. System message: the supplied system prompt + a rendered tool
//     catalog block (name + description per tool from rc.Catalog).
//  2. User message: the run's Goal (or Query when Goal is empty),
//     followed by an optional Summary block when rc.Trajectory.Summary
//     is non-nil (Phase 46 populates Summary; Phase 45 reads but does
//     not write it).
//  3. Per-step assistant + user pair: the prior planner action
//     rendered as JSON (assistant) and the rendered observation
//     (user). Observations prefer LLMObservation over raw Observation
//     per D-026 heavy-content discipline.
//
// The builder reads from rc; it MUST NOT mutate rc. The result is
// always safe to discard / re-build per call — the builder is
// stateless.
type defaultBuilder struct{}

// Build implements [PromptBuilder].
func (defaultBuilder) Build(rc planner.RunContext, systemPrompt string) llm.CompleteRequest {
	if systemPrompt == "" {
		systemPrompt = DefaultSystemPrompt
	}

	var messages []llm.ChatMessage

	// 1. System block: prompt + tool catalog.
	sysContent := buildSystemContent(systemPrompt, rc)
	messages = append(messages, llm.ChatMessage{
		Role:    llm.RoleSystem,
		Content: textContent(sysContent),
	})

	// 2. User block: goal/query + optional summary.
	userContent := buildUserContent(rc)
	messages = append(messages, llm.ChatMessage{
		Role:    llm.RoleUser,
		Content: textContent(userContent),
	})

	// 3. Trajectory steps: assistant (prior action) + user
	// (observation) per completed step.
	if rc.Trajectory != nil {
		for _, step := range rc.Trajectory.Steps {
			asst := renderActionForLLM(step.Action)
			obs := renderObservationForLLM(step)
			if asst != "" {
				messages = append(messages, llm.ChatMessage{
					Role:    llm.RoleAssistant,
					Content: textContent(asst),
				})
			}
			if obs != "" {
				messages = append(messages, llm.ChatMessage{
					Role:    llm.RoleUser,
					Content: textContent(obs),
				})
			}
		}
		// Optional: emit any resolved background-task outcomes (push
		// wake — D-032 / Phase 45 spec) as a final user message so
		// the planner sees them on the very next step.
		if len(rc.Trajectory.Background) > 0 {
			if bg := renderBackground(rc.Trajectory.Background); bg != "" {
				messages = append(messages, llm.ChatMessage{
					Role:    llm.RoleUser,
					Content: textContent(bg),
				})
			}
		}
	}

	return llm.CompleteRequest{
		Messages: messages,
	}
}

// buildSystemContent composes the system prompt + tool catalog.
func buildSystemContent(systemPrompt string, rc planner.RunContext) string {
	var b strings.Builder
	b.WriteString(systemPrompt)
	b.WriteString("\n\nAvailable tools:\n")

	tools := listTools(rc)
	if len(tools) == 0 {
		b.WriteString("  (no tools registered for this run)\n")
	} else {
		for _, t := range tools {
			b.WriteString("  - ")
			b.WriteString(t.Name)
			if t.Description != "" {
				b.WriteString(": ")
				b.WriteString(oneLine(t.Description))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\nRemember: emit `_finish` when you have enough information.\n")
	return b.String()
}

// buildUserContent composes the user goal + optional summary.
func buildUserContent(rc planner.RunContext) string {
	goal := rc.Goal
	if goal == "" {
		goal = rc.Query
	}
	if goal == "" {
		goal = "(no goal supplied)"
	}

	var b strings.Builder
	b.WriteString("User goal: ")
	b.WriteString(goal)

	if rc.Trajectory != nil && rc.Trajectory.Summary != nil {
		s := rc.Trajectory.Summary
		b.WriteString("\n\nTrajectory summary so far:\n")
		if len(s.Goals) > 0 {
			b.WriteString("  Goals tracked: ")
			b.WriteString(strings.Join(s.Goals, "; "))
			b.WriteString("\n")
		}
		if len(s.Facts) > 0 {
			b.WriteString("  Facts: ")
			b.WriteString(strings.Join(s.Facts, "; "))
			b.WriteString("\n")
		}
		if len(s.Pending) > 0 {
			b.WriteString("  Pending: ")
			b.WriteString(strings.Join(s.Pending, "; "))
			b.WriteString("\n")
		}
		if s.LastOutputDigest != "" {
			b.WriteString("  Last output: ")
			b.WriteString(oneLine(s.LastOutputDigest))
			b.WriteString("\n")
		}
		if s.Note != "" {
			b.WriteString("  Note: ")
			b.WriteString(oneLine(s.Note))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// listTools returns the tools visible to the planner via the
// RunContext's catalog view. Nil catalog yields an empty slice.
func listTools(rc planner.RunContext) []tools.Tool {
	if rc.Catalog == nil {
		return nil
	}
	return rc.Catalog.List()
}

// renderActionForLLM converts a Step.Action (typed as `any` in the
// trajectory subpackage to avoid an import cycle) into the JSON
// envelope the LLM previously emitted. Supports the V1 minimum-viable
// Decision shapes; non-CallTool actions render as a JSON object
// carrying the shape's name + a debug field.
//
// Returns the empty string when the action is nil or unrenderable
// (the prompt builder skips empty messages — defensive against
// trajectory shapes the planner doesn't recognise).
func renderActionForLLM(action any) string {
	if action == nil {
		return ""
	}
	switch a := action.(type) {
	case planner.CallTool:
		// Echo the JSON envelope the LLM emitted, normalised.
		env := map[string]any{
			"tool":      a.Tool,
			"args":      json.RawMessage(safeArgs(a.Args)),
			"reasoning": a.Reasoning,
		}
		out, err := json.Marshal(env)
		if err != nil {
			return ""
		}
		return string(out)
	case planner.Finish:
		// A prior Finish in the trajectory is unusual (the runtime
		// should have terminated), but render defensively for
		// observability.
		out, err := json.Marshal(map[string]any{
			"action": "finish",
			"reason": string(a.Reason),
		})
		if err != nil {
			return ""
		}
		return string(out)
	default:
		// Unknown action shape — render a minimal marker so the
		// trajectory render preserves ordering.
		out, err := json.Marshal(map[string]any{
			"action": fmt.Sprintf("%T", action),
		})
		if err != nil {
			return ""
		}
		return string(out)
	}
}

// renderObservationForLLM picks the projection the planner shows to
// the LLM. Per D-026 ("heavy content discipline"), the planner prefers
// LLMObservation over raw Observation: producer-side renderers
// (Phase 44+) populate LLMObservation as the compressed / redacted
// projection; raw Observation may carry full tool results that aren't
// safe to round-trip through the LLM.
//
// Error / Failure are surfaced first when present (the planner needs
// to see failures to course-correct).
func renderObservationForLLM(step planner.Step) string {
	if step.Failure != nil {
		return fmt.Sprintf("Observation (failure): %s — %s",
			step.Failure.Code, oneLine(step.Failure.Message))
	}
	if step.Error != "" {
		return "Observation (error): " + oneLine(step.Error)
	}
	if step.LLMObservation != nil {
		return "Observation: " + renderAny(step.LLMObservation)
	}
	if step.Observation != nil {
		return "Observation: " + renderAny(step.Observation)
	}
	return ""
}

// renderBackground renders resolved background-task outcomes as a
// JSON-encoded user message. Phase 45 ships the read path (D-032 push
// wake declaration); the runtime engine populates
// `rc.Trajectory.Background` in Phase 47+.
func renderBackground(bg map[string]planner.BackgroundResult) string {
	if len(bg) == 0 {
		return ""
	}
	out, err := json.Marshal(map[string]any{
		"background_resolved": bg,
	})
	if err != nil {
		return ""
	}
	return string(out)
}

// renderAny is a small renderer for arbitrary observation values. We
// avoid `fmt.Sprintf("%v", x)` for structured shapes because it can
// surface heavy content that should have been redacted. Strings pass
// through (one-lined); maps / structs render via json.Marshal with a
// fall-through to the type name when marshalling fails.
func renderAny(v any) string {
	if v == nil {
		return "(nil)"
	}
	switch x := v.(type) {
	case string:
		return oneLine(x)
	case json.RawMessage:
		return string(x)
	case []byte:
		// Avoid leaking raw bytes; show a marker. The planner contract
		// is that the producer should have rendered LLMObservation as
		// a string-shaped projection; a []byte here is a producer-side
		// bug surfaced rather than papered over.
		return fmt.Sprintf("(<%d raw bytes — producer should render LLMObservation as text>)", len(x))
	default:
		out, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("(<unrenderable %T>)", v)
		}
		return string(out)
	}
}

// safeArgs returns the args slice, or `{}` when empty/nil — matches
// the Phase 44 parser's normalisation so the echoed envelope is
// byte-identical to the LLM's original (modulo whitespace).
func safeArgs(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}

// oneLine collapses internal newlines + carriage returns to spaces so
// the rendered prompt remains a single line per message. The LLM-side
// tokenisation is largely whitespace-insensitive; collapsing keeps the
// prompt size bounded against malicious or runaway observations.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

// textContent constructs the [llm.Content] sum-type with the supplied
// text. Helper to keep call sites compact.
func textContent(s string) llm.Content {
	return llm.Content{Text: &s}
}
