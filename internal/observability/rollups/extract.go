package rollups

import (
	"encoding/json"
	"fmt"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/tasks"
)

// StoreGranularity is the bucket size rows are persisted at — the finest
// closed size (BucketHour). A query at a coarser size (BucketDay) groups
// stored rows into its own buckets at read time, so one storage granularity
// serves every closed query size.
const StoreGranularity = BucketHour

// Extract derives the rollup deltas of one canonical event. It is a PURE
// function of the event — deterministic, order-independent, and safe to call
// from any number of goroutines — which is what makes replay idempotency
// and concurrent reuse possible: re-extracting the same event always
// produces the same deltas.
//
// Supported event types and their measures (the ONLY source-backed measures;
// everything else is absent by design):
//
//   - `llm.cost.recorded` — MeasureLLMCostUSD (Cost.TotalCost),
//     MeasureLLMTokensPrompt / Completion / Total (Usage), the
//     MeasureLLMCompletions count (the record IS a successful completion),
//     and MeasureLLMLatencyMS (Usage.LatencyMS). The model dimension is
//     the payload's authoritative model.
//   - `task.completed` / `task.failed` / `task.cancelled` — the matching
//     outcome count. Task events carry no model — their rows are the
//     un-attributed (model "") aggregate for the triple.
//
// Every other event type returns (nil, nil): it contributes no supported
// measure to any row. That is the designed "absent" behaviour — an
// event-type axis that has no canonical payload backing is simply not in
// the rollup, never estimated.
//
// Payload decoding: live events carry the typed SafePayload
// (llm.CostRecordedPayload etc.); events rehydrated from the durable log
// carry events.RedactedMap with the payload's JSON object. Extract accepts
// both shapes and decodes the RedactedMap back into the typed struct, so a
// projector fed from the durable log reads exactly the fields the publisher
// recorded.
//
// A supported event whose payload cannot be decoded is a corruption of the
// log — Extract fails loudly (wrapped error) rather than silently recording
// a zero-value row or skipping the event, either of which would undercount
// a bucket without a trace.
func Extract(ev events.Event) ([]Delta, error) {
	switch ev.Type {
	case llm.EventTypeCostRecorded:
		p, err := decodeTypedPayload[llm.CostRecordedPayload](ev.Payload)
		if err != nil {
			return nil, fmt.Errorf("rollups: extract seq=%d type=%q: %w", ev.Sequence, ev.Type, err)
		}
		return []Delta{{
			Key: Key{
				BucketStart: BucketStart(ev.OccurredAt, StoreGranularity),
				TenantID:    ev.Identity.TenantID,
				UserID:      ev.Identity.UserID,
				SessionID:   ev.Identity.SessionID,
				Model:       p.Model,
				AgentID:     authoritativeAgent(ev),
			},
			Add: MeasureSet{
				LLMCostUSD:          p.Cost.TotalCost,
				LLMTokensPrompt:     int64(p.Usage.PromptTokens),
				LLMTokensCompletion: int64(p.Usage.CompletionTokens),
				LLMTokensTotal:      int64(p.Usage.TotalTokens),
				LLMCompletions:      1,
				LLMLatencyMS:        p.Usage.LatencyMS,
			},
		}}, nil

	case tasks.EventTypeTaskCompleted:
		return taskOutcomeDelta(ev, func(s *MeasureSet) { s.TasksCompleted++ }), nil
	case tasks.EventTypeTaskFailed:
		return taskOutcomeDelta(ev, func(s *MeasureSet) { s.TasksFailed++ }), nil
	case tasks.EventTypeTaskCancelled:
		return taskOutcomeDelta(ev, func(s *MeasureSet) { s.TasksCancelled++ }), nil

	default:
		return nil, nil
	}
}

// taskOutcomeDelta builds the single-delta row for a task outcome event.
// The measure is the event type itself (one count per terminal transition);
// the payload carries no rollup measures, so it is deliberately not read.
func taskOutcomeDelta(ev events.Event, bump func(*MeasureSet)) []Delta {
	return []Delta{{
		Key: Key{
			BucketStart: BucketStart(ev.OccurredAt, StoreGranularity),
			TenantID:    ev.Identity.TenantID,
			UserID:      ev.Identity.UserID,
			SessionID:   ev.Identity.SessionID,
			AgentID:     authoritativeAgent(ev),
		},
		Add: bumpSet(bump),
	}}
}

func bumpSet(bump func(*MeasureSet)) MeasureSet {
	var s MeasureSet
	bump(&s)
	return s
}

// authoritativeAgent returns the event's authoritative agent id, or "" when
// the event carries none. No V1 canonical payload carries an agent id, so
// this is empty for every event that reaches Extract today; the axis is
// closed and the extraction point exists so a payload that later carries an
// authoritative agent id populates the dimension without a shape change
// (see dimension.go).
func authoritativeAgent(ev events.Event) string {
	_ = ev // reserved: read the authoritative agent id from the payload when one exists
	return ""
}

// decodeTypedPayload recovers a typed payload from either the typed value
// itself or an events.RedactedMap (the durable-log rehydration shape). The
// RedactedMap path round-trips the JSON object back through the typed
// struct so field names and nested shapes stay single-sourced in the struct
// definition rather than duplicated as extraction literals.
func decodeTypedPayload[T any](p events.EventPayload) (T, error) {
	var zero T
	if typed, ok := p.(T); ok {
		return typed, nil
	}
	rm, ok := p.(events.RedactedMap)
	if !ok {
		return zero, fmt.Errorf("payload is %T, not %T or events.RedactedMap", p, zero)
	}
	raw, err := json.Marshal(rm.Data)
	if err != nil {
		return zero, fmt.Errorf("payload re-encode: %w", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, fmt.Errorf("payload decode: %w", err)
	}
	return out, nil
}
