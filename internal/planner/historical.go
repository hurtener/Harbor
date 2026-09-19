package planner

import (
	"bytes"
	"encoding/json"
	"errors"

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
