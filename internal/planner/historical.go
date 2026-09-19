package planner

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// HistoricalStep is an inert retained exchange. It does not implement Decision.
type HistoricalStep = trajectory.HistoricalStep

// ErrInvalidHistoricalStep rejects malformed or unsupported history explicitly.
var ErrInvalidHistoricalStep = errors.New("planner: invalid retained exchange")

// RetainStep records the action's concrete kind before JSON erases it. This
// does not execute, authorize, or rehydrate a Decision. Only the request renderer
// interprets the closed kind set; unfamiliar/legacy actions remain inert data.
func RetainStep(step Step, sourceRun string, index int) (Step, error) {
	if sourceRun == "" || index < 0 || step.Historical != nil {
		return Step{}, ErrInvalidHistoricalStep
	}
	kind := "context"
	switch step.Action.(type) {
	case CallTool:
		kind = "call_tool"
	case CallParallel:
		kind = "call_parallel"
	case Batch:
		kind = "batch"
	case TaskProgress:
		kind = "progress"
	case SpawnTask:
		kind = "spawn"
	case AwaitTask:
		kind = "await"
	case TaskStatusQuery:
		kind = "task_status"
	case CancelTask:
		kind = "cancel_task"
	case SteerTask:
		kind = "steer_task"
	case PauseTask:
		kind = "pause_task"
	case ResumeTask:
		kind = "resume_task"
	}
	model := trajectory.ModelStep(step)
	// Failure classification must survive restoration so native renderers keep
	// applying their existing failure-first and argument-redaction rules.
	if step.Failure != nil {
		failure := *step.Failure
		model.Failure = &failure
	}
	body, err := json.Marshal(model)
	if err != nil {
		return Step{}, ErrUnserializable{Field: "retained.exchange"}
	}
	body, err = redactFailedHistoricalArguments(model, kind, body)
	if err != nil {
		return Step{}, err
	}
	return Step{Historical: &HistoricalStep{
		Version: 1, SourceRun: sourceRun, Index: index, Kind: kind, Body: body,
	}}, nil
}

// Operate on a detached JSON tree; never mutate a live action or nested Args.
// Only the closed native action shapes have replayable argument fields.
func redactFailedHistoricalArguments(step Step, kind string, body []byte) ([]byte, error) {
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		return nil, ErrInvalidHistoricalStep
	}
	action, ok := object["action"].(map[string]any)
	if !ok {
		return body, nil
	}
	failureView := step
	failureView.LLMObservation = object["llm_observation"]
	failed := ReplayStepFailed(failureView)
	observation, ok := object["llm_observation"].(map[string]any)
	if !ok {
		observation = nil
	}
	scrub := func(value map[string]any, tool bool) {
		if tool {
			value["Args"] = map[string]any{}
			return
		}
		for key := range value {
			if key != "CallID" {
				delete(value, key)
			}
		}
	}
	scrubBranches := func(actionsKey, resultsKey string, tool bool) {
		branches, ok := action[actionsKey].([]any)
		if !ok {
			return
		}
		results, aggregate := observation[resultsKey].([]any)
		branchErrors := map[int]bool{}
		for _, entry := range results {
			result, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			index, ok := result["index"].(json.Number)
			if !ok {
				continue
			}
			n, err := index.Int64()
			message, isMessage := result["error"].(string)
			if !isMessage {
				continue
			}
			if err == nil && n >= 0 && n < int64(len(branches)) && message != "" {
				branchErrors[int(n)] = true
			}
		}
		for i, entry := range branches {
			if (aggregate && branchErrors[i]) || (!aggregate && failed) {
				if branch, ok := entry.(map[string]any); ok {
					scrub(branch, tool)
				}
			}
		}
	}
	switch kind {
	case "call_parallel":
		scrubBranches("Branches", "branches", true)
	case "batch":
		scrubBranches("Tools", "tools", true)
		scrubBranches("Spawns", "spawns", false)
		scrubBranches("Progress", "progress", false)
	case "call_tool":
		if failed {
			scrub(action, true)
		}
	case "context":
		if failed {
			object["action"] = map[string]any{"failed_action": true}
		}
	default:
		if failed {
			scrub(action, false)
		}
	}
	out, err := json.Marshal(object)
	if err != nil {
		return nil, ErrInvalidHistoricalStep
	}
	return out, nil
}

// ReadHistoricalStep validates the inert envelope before any inference path
// consumes it. The returned Action is a JSON tree, never a Decision. Arbitrary
// tool-result payloads remain data; only the host-owned envelope is interpreted.
func ReadHistoricalStep(outer Step) (Step, error) {
	h := outer.Historical
	if h == nil || h.Version != 1 || h.SourceRun == "" || h.Index < 0 || len(h.Body) > 512*1024 ||
		outer.Action != nil || outer.Observation != nil || outer.LLMObservation != nil ||
		outer.AssistantPreamble != "" || outer.ReasoningTrace != "" || outer.Failure != nil ||
		outer.Error != "" || outer.Streams != nil || !utf8.Valid(h.Body) {
		return Step{}, ErrInvalidHistoricalStep
	}
	switch h.Kind {
	case "context", "call_tool", "call_parallel", "batch", "progress", "spawn", "await",
		"task_status", "cancel_task", "steer_task", "pause_task", "resume_task":
	default:
		return Step{}, ErrInvalidHistoricalStep
	}
	// A duplicate field must not let storage, compaction and the request renderer
	// disagree on which value is authoritative. Require the canonical host field
	// names rather than JSON's case-insensitive aliases. Result bodies are opaque.
	fields := json.NewDecoder(bytes.NewReader(h.Body))
	token, err := fields.Token()
	if err != nil || token != json.Delim('{') {
		return Step{}, ErrInvalidHistoricalStep
	}
	seen := make(map[string]bool)
	for fields.More() {
		token, err = fields.Token()
		if err != nil {
			return Step{}, ErrInvalidHistoricalStep
		}
		key, ok := token.(string)
		if !ok {
			return Step{}, ErrInvalidHistoricalStep
		}
		switch key {
		case "action", "llm_observation", "assistant_preamble", "error", "failure",
			"started_at", "latency_ms", "token_estimate":
		default:
			return Step{}, ErrInvalidHistoricalStep
		}
		if seen[key] {
			return Step{}, ErrInvalidHistoricalStep
		}
		seen[key] = true
		var value json.RawMessage
		if err := fields.Decode(&value); err != nil {
			return Step{}, ErrInvalidHistoricalStep
		}
	}
	var step Step
	decoder := json.NewDecoder(bytes.NewReader(h.Body))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&step); err != nil {
		return Step{}, ErrInvalidHistoricalStep
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || step.Historical != nil || step.Observation != nil ||
		step.ReasoningTrace != "" || step.Streams != nil {
		return Step{}, ErrInvalidHistoricalStep
	}
	if h.Kind != "context" && step.Action == nil {
		return Step{}, ErrInvalidHistoricalStep
	}
	return step, nil
}
