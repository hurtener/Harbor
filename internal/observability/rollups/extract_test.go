package rollups

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/tasks"
)

var extractQuad = identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-1"}, RunID: "run-1"}

func TestExtract_LLMCostRecorded_Typed(t *testing.T) {
	at := time.Date(2026, 8, 13, 12, 34, 56, 0, time.UTC)
	ev := events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   extractQuad,
		OccurredAt: at,
		Sequence:   42,
		Payload: llm.CostRecordedPayload{
			Identity: extractQuad,
			Model:    "model-x",
			Cost:     llm.Cost{InputTokensCost: 0.5, OutputTokensCost: 0.25, TotalCost: 0.75, Currency: "USD"},
			Usage:    llm.Usage{PromptTokens: 100, CompletionTokens: 20, ReasoningTokens: 5, TotalTokens: 125, LatencyMS: 812},
		},
	}
	deltas, err := Extract(ev)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(deltas) != 1 {
		t.Fatalf("deltas = %d; want 1", len(deltas))
	}
	d := deltas[0]
	if d.Key.TenantID != "tenant-a" || d.Key.UserID != "user-1" || d.Key.SessionID != "session-1" {
		t.Fatalf("key identity = %+v; want the event triple", d.Key)
	}
	if d.Key.Model != "model-x" {
		t.Fatalf("key model = %q; want model-x", d.Key.Model)
	}
	if d.Key.AgentID != "" {
		t.Fatalf("key agent = %q; want empty (no authoritative agent on any V1 payload)", d.Key.AgentID)
	}
	if want := BucketStart(at, StoreGranularity); !d.Key.BucketStart.Equal(want) {
		t.Fatalf("bucket start = %v; want %v", d.Key.BucketStart, want)
	}
	if d.Add.LLMCostUSD != 0.75 || d.Add.LLMTokensPrompt != 100 || d.Add.LLMTokensCompletion != 20 ||
		d.Add.LLMTokensTotal != 125 || d.Add.LLMCompletions != 1 || d.Add.LLMLatencyMS != 812 {
		t.Fatalf("measures = %+v; want cost 0.75, tokens 100/20/125, completions 1, latency 812", d.Add)
	}
	if d.Add.IsZero() {
		t.Fatal("Add must not be zero")
	}
}

// TestExtract_LLMCostRecorded_RedactedMap pins the durable-log path: a
// successfully-persisted event rehydrates as events.RedactedMap (the
// durable driver stores payloads as generic JSON objects), and Extract
// recovers exactly the fields the publisher recorded.
func TestExtract_LLMCostRecorded_RedactedMap(t *testing.T) {
	at := time.Date(2026, 8, 13, 12, 34, 56, 0, time.UTC)
	typed := llm.CostRecordedPayload{
		Identity: extractQuad,
		Model:    "model-x",
		Cost:     llm.Cost{TotalCost: 1.5, Currency: "USD"},
		Usage:    llm.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10, LatencyMS: 99},
	}
	raw, err := json.Marshal(typed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ev := events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   extractQuad,
		OccurredAt: at,
		Sequence:   7,
		Payload:    events.RedactedMap{Data: data},
	}
	deltas, err := Extract(ev)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(deltas) != 1 {
		t.Fatalf("deltas = %d; want 1", len(deltas))
	}
	d := deltas[0]
	if d.Key.Model != "model-x" {
		t.Fatalf("model = %q; want model-x", d.Key.Model)
	}
	if d.Add.LLMCostUSD != 1.5 || d.Add.LLMTokensPrompt != 7 || d.Add.LLMTokensCompletion != 3 ||
		d.Add.LLMTokensTotal != 10 || d.Add.LLMCompletions != 1 || d.Add.LLMLatencyMS != 99 {
		t.Fatalf("measures = %+v; want cost 1.5, tokens 7/3/10, completions 1, latency 99", d.Add)
	}
}

func TestExtract_TaskOutcomes(t *testing.T) {
	at := time.Date(2026, 8, 13, 12, 34, 56, 0, time.UTC)
	cases := []struct {
		typ   events.EventType
		field func(MeasureSet) float64
	}{
		{tasks.EventTypeTaskCompleted, func(m MeasureSet) float64 { return float64(m.TasksCompleted) }},
		{tasks.EventTypeTaskFailed, func(m MeasureSet) float64 { return float64(m.TasksFailed) }},
		{tasks.EventTypeTaskCancelled, func(m MeasureSet) float64 { return float64(m.TasksCancelled) }},
	}
	for _, tc := range cases {
		t.Run(string(tc.typ), func(t *testing.T) {
			var payload events.EventPayload
			switch tc.typ {
			case tasks.EventTypeTaskCompleted:
				payload = tasks.TaskCompletedPayload{TaskID: "t1"}
			case tasks.EventTypeTaskFailed:
				payload = tasks.TaskFailedPayload{TaskID: "t1", ErrorCode: "E"}
			case tasks.EventTypeTaskCancelled:
				payload = tasks.TaskCancelledPayload{TaskID: "t1"}
			}
			ev := events.Event{Type: tc.typ, Identity: extractQuad, OccurredAt: at, Sequence: 1, Payload: payload}
			deltas, err := Extract(ev)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if len(deltas) != 1 {
				t.Fatalf("deltas = %d; want 1", len(deltas))
			}
			d := deltas[0]
			if d.Key.Model != "" {
				t.Fatalf("task outcome model = %q; want empty (un-attributed)", d.Key.Model)
			}
			if got := tc.field(d.Add); got != 1 {
				t.Fatalf("outcome count = %v; want 1", got)
			}
			if d.Add.LLMCompletions != 0 || d.Add.LLMCostUSD != 0 {
				t.Fatalf("task outcome must carry no LLM measures: %+v", d.Add)
			}
		})
	}
}

func TestExtract_UnsupportedTypeAbsent(t *testing.T) {
	at := time.Date(2026, 8, 13, 12, 34, 56, 0, time.UTC)
	ev := events.Event{
		Type:       events.EventTypeRuntimeError,
		Identity:   extractQuad,
		OccurredAt: at,
		Sequence:   9,
		Payload:    events.RedactedMap{Data: map[string]any{"msg": "boom"}},
	}
	deltas, err := Extract(ev)
	if err != nil {
		t.Fatalf("Extract unsupported: %v", err)
	}
	if deltas != nil {
		t.Fatalf("unsupported type deltas = %v; want nil (absent by design)", deltas)
	}
}

func TestExtract_CorruptSupportedPayloadFailsLoud(t *testing.T) {
	at := time.Date(2026, 8, 13, 12, 34, 56, 0, time.UTC)
	// A supported type whose payload is neither the typed value nor a
	// RedactedMap — corruption.
	ev := events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   extractQuad,
		OccurredAt: at,
		Sequence:   3,
		Payload:    struct{ events.Sealed }{},
	}
	if _, err := Extract(ev); err == nil {
		t.Fatal("corrupt supported payload must fail loudly")
	}

	// A RedactedMap that cannot decode back into the typed struct.
	ev.Payload = events.RedactedMap{Data: map[string]any{"cost": func() {}}}
	if _, err := Extract(ev); err == nil {
		t.Fatal("undecodable RedactedMap must fail loudly")
	}
}

func TestExtract_OccurredAtIsAuthoritativeForBucketing(t *testing.T) {
	// The bucket comes from the EVENT's OccurredAt, never the payload's
	// own stamp (the bus owns the event clock; payload stamps are
	// informational).
	payloadAt := time.Date(2026, 8, 13, 5, 0, 0, 0, time.UTC)
	evAt := time.Date(2026, 8, 13, 18, 30, 0, 0, time.UTC)
	ev := events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   extractQuad,
		OccurredAt: evAt,
		Sequence:   1,
		Payload: llm.CostRecordedPayload{
			Model:      "m",
			Cost:       llm.Cost{TotalCost: 1},
			OccurredAt: payloadAt,
		},
	}
	deltas, err := Extract(ev)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if want := BucketStart(evAt, StoreGranularity); !deltas[0].Key.BucketStart.Equal(want) {
		t.Fatalf("bucket = %v; want %v (event OccurredAt)", deltas[0].Key.BucketStart, want)
	}
}

func TestExtract_NilPayloadFailsLoud(t *testing.T) {
	ev := events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   extractQuad,
		OccurredAt: time.Now(),
		Sequence:   1,
		Payload:    nil,
	}
	if _, err := Extract(ev); err == nil {
		t.Fatal("nil payload on a supported type must fail loudly")
	}
}

func TestMeasureSet_Get(t *testing.T) {
	var m MeasureSet
	m.Add(MeasureSet{LLMCostUSD: 2.5, LLMTokensPrompt: 10, LLMTokensCompletion: 20, LLMTokensTotal: 30, LLMCompletions: 2, LLMLatencyMS: 500, TasksCompleted: 1, TasksFailed: 2, TasksCancelled: 3})
	m.Add(MeasureSet{LLMCostUSD: 0.5, LLMTokensPrompt: 1})
	if m.Get(MeasureLLMCostUSD) != 3.0 {
		t.Fatalf("cost = %v; want 3.0", m.Get(MeasureLLMCostUSD))
	}
	if m.Get(MeasureLLMTokensPrompt) != 11 {
		t.Fatalf("prompt = %v; want 11", m.Get(MeasureLLMTokensPrompt))
	}
	if m.Get(MeasureLLMCompletions) != 2 || m.Get(MeasureLLMLatencyMS) != 500 {
		t.Fatalf("completions/latency = %v/%v", m.Get(MeasureLLMCompletions), m.Get(MeasureLLMLatencyMS))
	}
	if m.Get(MeasureTasksCompleted) != 1 || m.Get(MeasureTasksFailed) != 2 || m.Get(MeasureTasksCancelled) != 3 {
		t.Fatalf("task measures wrong: %+v", m)
	}
	if m.IsZero() {
		t.Fatal("zero check on non-zero set")
	}
	var z MeasureSet
	if !z.IsZero() {
		t.Fatal("zero set must report zero")
	}
}

func TestClosedSets(t *testing.T) {
	if len(AllDimensions) != 5 {
		t.Fatalf("closed dimensions = %d; want 5", len(AllDimensions))
	}
	if MeasureCount != len(AllMeasures) {
		t.Fatalf("MeasureCount = %d; want %d", MeasureCount, len(AllMeasures))
	}
	for _, d := range AllDimensions {
		if err := d.Validate(); err != nil {
			t.Fatalf("closed dimension %q must validate: %v", d, err)
		}
	}
	for _, m := range AllMeasures {
		if err := m.Validate(); err != nil {
			t.Fatalf("closed measure %q must validate: %v", m, err)
		}
	}
	// Unknown values are refused loudly.
	if err := Dimension("run").Validate(); err == nil {
		t.Fatal("unknown dimension must fail validation")
	}
	if err := Measure("tokens_estimated").Validate(); err == nil {
		t.Fatal("unknown measure must fail validation")
	}
}
