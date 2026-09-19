package react

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

// renderStepMessages is shared by live and retained exchange projection. Native
// pairing, parallel ordering, failure redaction and heavy-content handling must
// not acquire a second implementation just because a session was restored.
func renderStepMessages(step planner.Step, replay planner.ReasoningReplayMode, index int) []llm.ChatMessage {
	if parallel, ok := step.Action.(planner.CallParallel); ok {
		assistant, results := renderNativeParallelStep(step, parallel, replay, index)
		return append([]llm.ChatMessage{assistant}, results...)
	}
	if batch, ok := step.Action.(planner.Batch); ok {
		assistant, results := renderNativeBatchStep(step, batch, replay, index)
		return append([]llm.ChatMessage{assistant}, results...)
	}
	if assistant, result, ok := renderNativeStepPair(step, replay, index); ok {
		messages := []llm.ChatMessage{assistant}
		if result != nil {
			messages = append(messages, *result)
		}
		return messages
	}
	if assistant, result, ok := renderNativeControlStep(step, replay, index); ok {
		messages := []llm.ChatMessage{assistant}
		if result != nil {
			messages = append(messages, *result)
		}
		return messages
	}
	var messages []llm.ChatMessage
	if text := renderAssistantTurn(step, replay); text != "" {
		messages = append(messages, llm.ChatMessage{Role: llm.RoleAssistant, Content: textContent(text)})
	}
	if text := renderObservationForLLM(step); text != "" {
		messages = append(messages, llm.ChatMessage{Role: llm.RoleUser, Content: textContent(text)})
	}
	return messages
}

// Typed actions exist only in this local rendering copy. Historical data never
// becomes RunContext.PendingToolCalls, a planner Decision or a dispatch request.
func renderHistoricalStep(history *planner.HistoricalStep) ([]llm.ChatMessage, error) {
	if history.Version != 1 || history.SourceRun == "" || history.Index < 0 || len(history.Body) > 512*1024 {
		return nil, planner.ErrInvalidHistoricalStep
	}
	step, err := decodeHistorical[planner.Step](history.Body)
	if err != nil || step.Historical != nil || step.Observation != nil ||
		step.ReasoningTrace != "" || step.Streams != nil {
		return nil, planner.ErrInvalidHistoricalStep
	}
	if history.Kind == "context" {
		// A non-native/legacy decision is evidence, not a guessed native call.
		return []llm.ChatMessage{{Role: llm.RoleUser, Content: textContent(
			"Historical execution (context only): " + string(history.Body))}}, nil
	}
	var wire struct {
		Action         json.RawMessage `json:"action"`
		LLMObservation json.RawMessage `json:"llm_observation"`
	}
	if err := json.Unmarshal(history.Body, &wire); err != nil {
		return nil, planner.ErrInvalidHistoricalStep
	}
	if len(wire.Action) == 0 || bytes.Equal(bytes.TrimSpace(wire.Action), []byte("null")) {
		return nil, planner.ErrInvalidHistoricalStep
	}
	switch history.Kind {
	case "call_tool":
		step.Action, err = decodeHistorical[planner.CallTool](wire.Action)
	case "call_parallel":
		step.Action, err = decodeHistorical[planner.CallParallel](wire.Action)
	case "batch":
		step.Action, err = decodeHistorical[planner.Batch](wire.Action)
	case "progress":
		step.Action, err = decodeHistorical[planner.TaskProgress](wire.Action)
	case "spawn":
		step.Action, err = decodeHistorical[planner.SpawnTask](wire.Action)
	case "await":
		step.Action, err = decodeHistorical[planner.AwaitTask](wire.Action)
	case "task_status":
		step.Action, err = decodeHistorical[planner.TaskStatusQuery](wire.Action)
	case "cancel_task":
		step.Action, err = decodeHistorical[planner.CancelTask](wire.Action)
	case "steer_task":
		step.Action, err = decodeHistorical[planner.SteerTask](wire.Action)
	case "pause_task":
		step.Action, err = decodeHistorical[planner.PauseTask](wire.Action)
	case "resume_task":
		step.Action, err = decodeHistorical[planner.ResumeTask](wire.Action)
	default:
		return nil, planner.ErrInvalidHistoricalStep
	}
	if err != nil {
		return nil, err
	}
	// Rehydrate only tagged aggregate shapes. Flat classified failures remain
	// maps so the existing failure-first renderer can preserve them unchanged.
	if object, ok := step.LLMObservation.(map[string]any); ok {
		switch history.Kind {
		case "call_parallel":
			if _, present := object["branches"]; present {
				step.LLMObservation, err = decodeHistorical[planner.ParallelObservation](wire.LLMObservation)
			}
		case "batch":
			if _, present := object["tools"]; present {
				step.LLMObservation, err = decodeHistorical[planner.BatchObservation](wire.LLMObservation)
			} else if _, present := object["spawns"]; present {
				step.LLMObservation, err = decodeHistorical[planner.BatchObservation](wire.LLMObservation)
			} else if _, present := object["progress"]; present {
				step.LLMObservation, err = decodeHistorical[planner.BatchObservation](wire.LLMObservation)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	messages := renderStepMessages(step, planner.ReasoningReplayNever, history.Index)
	// Providers may reuse a call ID in different turns. Keep stored original
	// IDs intact, but use a stable origin/ordinal namespace in the active wire
	// projection so independent runs cannot create duplicate correlation IDs.
	source := sha256.Sum256([]byte(history.SourceRun))
	if len(messages) == 0 || len(messages[0].ToolCalls) == 0 ||
		len(messages) != len(messages[0].ToolCalls)+1 {
		return nil, planner.ErrInvalidHistoricalStep
	}
	for i := range messages[0].ToolCalls {
		id := fmt.Sprintf("hist_%x_%d_%d", source[:12], history.Index, i)
		messages[0].ToolCalls[i].ID = id
		if messages[i+1].Role != llm.RoleTool {
			return nil, planner.ErrInvalidHistoricalStep
		}
		messages[i+1].ToolCallID = &id
	}
	return messages, nil
}

func decodeHistorical[T any](data []byte) (T, error) {
	var value T
	if !utf8.Valid(data) || len(bytes.TrimSpace(data)) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return value, planner.ErrInvalidHistoricalStep
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, planner.ErrInvalidHistoricalStep
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return value, planner.ErrInvalidHistoricalStep
	}
	return value, nil
}
