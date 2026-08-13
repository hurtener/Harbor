package rollups

import (
	"encoding/json"
	"errors"
	"math"
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
			Usage: llm.Usage{
				PromptTokens:     100,
				CompletionTokens: 20,
				ReasoningTokens:  5,
				CacheReadTokens:  80,
				CacheWriteTokens: 15,
				TotalTokens:      125,
				LatencyMS:        812,
			},
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
	if want := BucketStart(at, StoreGranularity); !d.Key.BucketStart.Equal(want) {
		t.Fatalf("bucket start = %v; want %v", d.Key.BucketStart, want)
	}
	if !d.Key.BucketStart.Equal(time.Date(2026, 8, 13, 12, 34, 0, 0, time.UTC)) {
		t.Fatalf("storage bucket = %v; want the MINUTE grid 12:34:00Z", d.Key.BucketStart)
	}
	if d.Add.LLMCompletions != 1 || d.Add.LLMTokensPrompt != 100 || d.Add.LLMTokensCompletion != 20 ||
		d.Add.LLMTokensReasoning != 5 || d.Add.LLMTokensCacheRead != 80 || d.Add.LLMTokensCacheWrite != 15 ||
		d.Add.LLMTokensTotal != 125 || d.Add.LLMCostMicros != 750_000 || d.Add.LLMLatencyCount != 1 ||
		d.Add.LLMLatencySumMS != 812 || d.Add.LLMLatencyMinMS != 812 || d.Add.LLMLatencyMaxMS != 812 {
		t.Fatalf("measures = %+v; want completions 1, tokens 100/20/5/80/15/125, cost 750_000 micros, latency 1/812/812/812", d.Add)
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
	if d.Add.LLMCostMicros != 1_500_000 || d.Add.LLMTokensPrompt != 7 || d.Add.LLMTokensCompletion != 3 ||
		d.Add.LLMTokensTotal != 10 || d.Add.LLMCompletions != 1 || d.Add.LLMLatencySumMS != 99 {
		t.Fatalf("measures = %+v; want cost 1_500_000 micros, tokens 7/3/10, completions 1, latency 99", d.Add)
	}
}

// TestExtract_CostRefusal pins the strict integer conversion: NaN, ±Inf,
// negative, and out-of-int64-micro-range costs fail loudly with
// ErrInvalidCost at Extract — never silently zeroed or float-accumulated.
func TestExtract_CostRefusal(t *testing.T) {
	at := time.Date(2026, 8, 13, 12, 34, 56, 0, time.UTC)
	// The source float is converted to micro-units EXACTLY ONCE (never
	// accumulated as float64); 0.75 USD → 750_000 micros, deterministically.
	for _, c := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.001, -1, 9.3e12, float64(math.MaxInt64)} {
		ev := events.Event{
			Type:       llm.EventTypeCostRecorded,
			Identity:   extractQuad,
			OccurredAt: at,
			Sequence:   1,
			Payload: llm.CostRecordedPayload{
				Model: "m",
				Cost:  llm.Cost{TotalCost: c, Currency: "USD"},
			},
		}
		if _, err := Extract(ev); !errors.Is(err, ErrInvalidCost) {
			t.Fatalf("Extract(cost=%v) err = %v; want ErrInvalidCost", c, err)
		}
	}
}

// TestExtract_NegativeUsageRefused pins the closed nonnegative gate on the
// canonical usage fields Extract converts: a negative token count or a
// negative latency is a corrupted payload and fails loudly with
// ErrNegativeMeasure — it is never cast into a shrinking counter. Exact
// zero remains valid.
func TestExtract_NegativeUsageRefused(t *testing.T) {
	at := time.Date(2026, 8, 13, 12, 34, 56, 0, time.UTC)
	bad := []llm.Usage{
		{PromptTokens: -1},
		{CompletionTokens: -1},
		{ReasoningTokens: -1},
		{CacheReadTokens: -1},
		{CacheWriteTokens: -1},
		{TotalTokens: -1},
		{LatencyMS: -1},
	}
	for _, usage := range bad {
		ev := events.Event{
			Type:       llm.EventTypeCostRecorded,
			Identity:   extractQuad,
			OccurredAt: at,
			Sequence:   1,
			Payload: llm.CostRecordedPayload{
				Model: "m",
				Cost:  llm.Cost{TotalCost: 1, Currency: "USD"},
				Usage: usage,
			},
		}
		if _, err := Extract(ev); !errors.Is(err, ErrNegativeMeasure) {
			t.Fatalf("Extract(usage=%+v) err = %v; want ErrNegativeMeasure", usage, err)
		}
	}

	// Exact zero remains valid: zero tokens and zero latency are
	// legitimate provider reports, and the row still records the
	// successful-completion count and the (zero) latency fold.
	ev := events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   extractQuad,
		OccurredAt: at,
		Sequence:   2,
		Payload: llm.CostRecordedPayload{
			Model: "m",
			Cost:  llm.Cost{TotalCost: 0, Currency: "USD"},
			Usage: llm.Usage{},
		},
	}
	deltas, err := Extract(ev)
	if err != nil {
		t.Fatalf("Extract(zero usage): %v", err)
	}
	if len(deltas) != 1 {
		t.Fatalf("deltas = %d; want 1", len(deltas))
	}
	d := deltas[0].Add
	if d.LLMCompletions != 1 || d.LLMLatencyCount != 1 || d.LLMTokensTotal != 0 || d.LLMLatencySumMS != 0 || d.LLMLatencyMinMS != 0 || d.LLMLatencyMaxMS != 0 {
		t.Fatalf("zero-usage measures = %+v; want completions/latency count 1, token and latency sums 0", d)
	}
}

// TestExtract_CostConversionExact pins the deterministic rounding: the
// float64 artifacts of 0.1, 0.2, and 0.1+0.2 all convert to exact integer
// micro-units whose SUM is exactly 0.60 USD.
func TestExtract_CostConversionExact(t *testing.T) {
	at := time.Date(2026, 8, 13, 12, 34, 56, 0, time.UTC)
	var total int64
	for _, c := range []float64{0.1, 0.2, 0.1 + 0.2} {
		ev := events.Event{
			Type:       llm.EventTypeCostRecorded,
			Identity:   extractQuad,
			OccurredAt: at,
			Sequence:   1,
			Payload: llm.CostRecordedPayload{
				Model: "m",
				Cost:  llm.Cost{TotalCost: c, Currency: "USD"},
			},
		}
		ds, err := Extract(ev)
		if err != nil {
			t.Fatalf("Extract(cost=%v): %v", c, err)
		}
		total += ds[0].Add.LLMCostMicros
	}
	if total != 600_000 {
		t.Fatalf("0.1+0.2+(0.1+0.2) = %d micros; want exactly 600_000", total)
	}
}

func TestExtract_TaskOutcomes(t *testing.T) {
	at := time.Date(2026, 8, 13, 12, 34, 56, 0, time.UTC)
	cases := []struct {
		typ   events.EventType
		field func(MeasureSet) int64
	}{
		{tasks.EventTypeTaskCompleted, func(m MeasureSet) int64 { return m.TasksCompleted }},
		{tasks.EventTypeTaskFailed, func(m MeasureSet) int64 { return m.TasksFailed }},
		{tasks.EventTypeTaskCancelled, func(m MeasureSet) int64 { return m.TasksCancelled }},
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
				t.Fatalf("outcome count = %d; want 1", got)
			}
			if d.Add.LLMCompletions != 0 || d.Add.LLMCostMicros != 0 || d.Add.LLMLatencyCount != 0 {
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

func TestMeasureSet_Add_Get(t *testing.T) {
	var m MeasureSet
	mustAdd(t, &m, MeasureSet{LLMCostMicros: 2_500_000, LLMTokensPrompt: 10, LLMTokensCompletion: 20, LLMTokensTotal: 30, LLMCompletions: 2, LLMLatencySumMS: 500, TasksCompleted: 1, TasksFailed: 2, TasksCancelled: 3})
	mustAdd(t, &m, MeasureSet{LLMCostMicros: 500_000, LLMTokensPrompt: 1})
	if got := m.Get(MeasureLLMCostMicros).N; got != 3_000_000 {
		t.Fatalf("cost = %d micros; want 3_000_000", got)
	}
	if got := m.Get(MeasureLLMCostMicros).Scale; got != CostScaleMicros {
		t.Fatalf("cost scale = %d; want %d", got, CostScaleMicros)
	}
	if got := m.Get(MeasureLLMTokensPrompt).N; got != 11 {
		t.Fatalf("prompt = %d; want 11", got)
	}
	if got := m.Get(MeasureLLMCompletions).N; got != 2 {
		t.Fatalf("completions = %d; want 2", got)
	}
	if got := m.Get(MeasureLLMLatencySumMS).N; got != 500 {
		t.Fatalf("latency sum = %d; want 500", got)
	}
	if got := m.Get(MeasureTasksCompleted).N; got != 1 {
		t.Fatalf("tasks completed = %d; want 1", got)
	}
	if got := m.Get(MeasureTasksFailed).N; got != 2 || m.Get(MeasureTasksCancelled).N != 3 {
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

// TestMeasureSet_LatencyMinMaxFold pins the fold semantics: min/max are the
// group-wise minimum / maximum, unaffected by task-only events and by a
// zero-latency record.
func TestMeasureSet_LatencyMinMaxFold(t *testing.T) {
	var m MeasureSet
	mustAdd(t, &m, MeasureSet{LLMLatencyCount: 1, LLMLatencySumMS: 100, LLMLatencyMinMS: 100, LLMLatencyMaxMS: 100, hasLatency: true})
	mustAdd(t, &m, MeasureSet{LLMLatencyCount: 1, LLMLatencySumMS: 50, LLMLatencyMinMS: 50, LLMLatencyMaxMS: 50, hasLatency: true})
	mustAdd(t, &m, MeasureSet{LLMLatencyCount: 1, LLMLatencySumMS: 200, LLMLatencyMinMS: 200, LLMLatencyMaxMS: 200, hasLatency: true})
	mustAdd(t, &m, MeasureSet{TasksCompleted: 1}) // no latency contribution
	if got := m.Get(MeasureLLMLatencyCount).N; got != 3 {
		t.Fatalf("latency count = %d; want 3", got)
	}
	if got := m.Get(MeasureLLMLatencySumMS).N; got != 350 {
		t.Fatalf("latency sum = %d; want 350", got)
	}
	if got := m.Get(MeasureLLMLatencyMinMS).N; got != 50 {
		t.Fatalf("latency min = %d; want 50", got)
	}
	if got := m.Get(MeasureLLMLatencyMaxMS).N; got != 200 {
		t.Fatalf("latency max = %d; want 200", got)
	}
	// A zero-latency record folds min down to 0 without the identity
	// colliding (hasLatency gate).
	mustAdd(t, &m, MeasureSet{LLMLatencyCount: 1, LLMLatencySumMS: 0, LLMLatencyMinMS: 0, LLMLatencyMaxMS: 0, hasLatency: true})
	if got := m.Get(MeasureLLMLatencyMinMS).N; got != 0 {
		t.Fatalf("latency min after zero-latency record = %d; want 0", got)
	}
	// A task-only set (no latency) must NOT report min/max garbage.
	var tOnly MeasureSet
	mustAdd(t, &tOnly, MeasureSet{TasksCompleted: 2})
	if got := tOnly.Get(MeasureLLMLatencyMinMS).N; got != 0 {
		t.Fatalf("task-only latency min = %d; want 0 (undefined)", got)
	}
}

// TestMeasureSet_Add_MaxBoundaryAndOverflow pins the fail-loud checked
// accumulation: a sum that exactly reaches the int64 bounds is accepted, a
// sum that would overflow is refused with ErrMeasureOverflow, and the folds
// stay exact at the boundary (they are comparisons, not sums).
func TestMeasureSet_Add_MaxBoundaryAndOverflow(t *testing.T) {
	// The exact upper boundary: MaxInt64-5 + 5 = MaxInt64 is representable
	// and accepted; one more would wrap.
	m := MeasureSet{LLMCompletions: math.MaxInt64 - 5, LLMTokensTotal: 10}
	mustAdd(t, &m, MeasureSet{LLMCompletions: 5})
	if got := m.Get(MeasureLLMCompletions).N; got != math.MaxInt64 {
		t.Fatalf("completions = %d; want %d (exact upper boundary)", got, int64(math.MaxInt64))
	}
	// The latency folds remain exact at the boundary: merging a fold into
	// a set whose sums sit at MaxInt64 succeeds.
	mustAdd(t, &m, MeasureSet{LLMLatencyCount: 1, LLMLatencySumMS: 10, LLMLatencyMinMS: 10, LLMLatencyMaxMS: 10, hasLatency: true})
	if got := m.Get(MeasureLLMLatencyMinMS).N; got != 10 {
		t.Fatalf("latency min = %d; want 10", got)
	}
	// One past the boundary is refused loudly, and the set stays intact.
	if err := m.Add(MeasureSet{LLMCompletions: 1}); !errors.Is(err, ErrMeasureOverflow) {
		t.Fatalf("overflow add err = %v; want ErrMeasureOverflow", err)
	}
	if got := m.Get(MeasureLLMCompletions).N; got != math.MaxInt64 {
		t.Fatalf("completions after refused add = %d; want %d (receiver untouched)", got, int64(math.MaxInt64))
	}

	// A NEGATIVE delta is refused with ErrNegativeMeasure even when the
	// result would stay in range — a counter never shrinks, and the int64
	// underflow branch is unreachable because any negative delta is
	// rejected before range arithmetic. The receiver is untouched.
	m = MeasureSet{LLMCompletions: 10, LLMTokensTotal: 7}
	if err := m.Add(MeasureSet{LLMCompletions: -5}); !errors.Is(err, ErrNegativeMeasure) {
		t.Fatalf("negative delta err = %v; want ErrNegativeMeasure", err)
	}
	if got := m.Get(MeasureLLMCompletions).N; got != 10 {
		t.Fatalf("completions after refused negative delta = %d; want 10 (receiver untouched)", got)
	}
	// A zero delta is valid and a no-op.
	mustAdd(t, &m, MeasureSet{LLMCompletions: 0})
	if got := m.Get(MeasureLLMCompletions).N; got != 10 {
		t.Fatalf("completions after zero delta = %d; want 10", got)
	}
}

// TestMeasureSet_Add_NoPartialMutation pins the atomicity contract: when ANY
// additive field would overflow, the receiver is untouched in EVERY field —
// the sums that would fit are not applied, and the latency folds are not
// merged — so a rejected accumulation can never leave a half-updated row.
func TestMeasureSet_Add_NoPartialMutation(t *testing.T) {
	m := MeasureSet{
		LLMCompletions:  7,
		LLMTokensPrompt: 100,
		LLMTokensTotal:  math.MaxInt64, // would overflow when +1
		LLMCostMicros:   1_000_000,
		LLMLatencyCount: 3,
		LLMLatencySumMS: 900,
		LLMLatencyMinMS: 100,
		LLMLatencyMaxMS: 500,
		hasLatency:      true,
		TasksCompleted:  2,
		TasksFailed:     1,
	}
	before := m
	delta := MeasureSet{
		LLMCompletions:  1, // would fit
		LLMTokensPrompt: 1, // would fit
		LLMTokensTotal:  1, // would OVERFLOW (MaxInt64 + 1)
		LLMLatencyCount: 1,
		LLMLatencySumMS: 10,
		LLMLatencyMinMS: 1,   // would fold min down
		LLMLatencyMaxMS: 999, // would fold max up
		hasLatency:      true,
		TasksFailed:     1, // would fit
	}
	if err := m.Add(delta); !errors.Is(err, ErrMeasureOverflow) {
		t.Fatalf("Add err = %v; want ErrMeasureOverflow", err)
	}
	if m != before {
		t.Fatalf("rejected Add mutated the receiver:\n before: %+v\n after:  %+v", before, m)
	}
}

// TestMeasureSet_Add_NegativeRejectionAtomic pins the fail-loud
// nonnegative gate: ANY negative additive field — even one whose sum would
// stay in range — refuses the whole merge with ErrNegativeMeasure, and the
// receiver is left EXACTLY as it was (the fields that would fit are not
// applied).
func TestMeasureSet_Add_NegativeRejectionAtomic(t *testing.T) {
	negatives := []struct {
		name  string
		delta MeasureSet
	}{
		{"llm_completions", MeasureSet{LLMCompletions: -1}},
		{"llm_tokens_prompt", MeasureSet{LLMTokensPrompt: -1}},
		{"llm_tokens_completion", MeasureSet{LLMTokensCompletion: -1}},
		{"llm_tokens_reasoning", MeasureSet{LLMTokensReasoning: -1}},
		{"llm_tokens_cache_read", MeasureSet{LLMTokensCacheRead: -1}},
		{"llm_tokens_cache_write", MeasureSet{LLMTokensCacheWrite: -1}},
		{"llm_tokens_total", MeasureSet{LLMTokensTotal: -1}},
		{"llm_cost_micros", MeasureSet{LLMCostMicros: -1}},
		{"llm_latency_count", MeasureSet{LLMLatencyCount: -1}},
		{"llm_latency_sum_ms", MeasureSet{LLMLatencySumMS: -1}},
		{"tasks_completed", MeasureSet{TasksCompleted: -1}},
		{"tasks_failed", MeasureSet{TasksFailed: -1}},
		{"tasks_cancelled", MeasureSet{TasksCancelled: -1}},
	}
	for _, tc := range negatives {
		t.Run(tc.name, func(t *testing.T) {
			m := MeasureSet{LLMCompletions: 3, LLMTokensPrompt: 40, LLMCostMicros: 5_000_000}
			before := m
			if err := m.Add(tc.delta); !errors.Is(err, ErrNegativeMeasure) {
				t.Fatalf("Add(negative %s) err = %v; want ErrNegativeMeasure", tc.name, err)
			}
			if m != before {
				t.Fatalf("rejected negative delta mutated the receiver:\n before: %+v\n after:  %+v", before, m)
			}
		})
	}

	// Atomicity across fields: a delta that mixes a negative field with
	// positive fields that would fit is refused WHOLE — nothing applies.
	m := MeasureSet{LLMTokensTotal: 100, TasksCompleted: 2}
	before := m
	delta := MeasureSet{
		LLMTokensTotal:  10, // would fit
		LLMTokensPrompt: 1,  // would fit
		LLMLatencySumMS: -5, // NEGATIVE — refuses the whole merge
		TasksCompleted:  1,  // would fit
	}
	if err := m.Add(delta); !errors.Is(err, ErrNegativeMeasure) {
		t.Fatalf("mixed negative delta err = %v; want ErrNegativeMeasure", err)
	}
	if m != before {
		t.Fatalf("rejected mixed delta mutated the receiver:\n before: %+v\n after:  %+v", before, m)
	}
}

// mustAdd applies a checked merge that is expected to succeed, failing the
// test on any error.
func mustAdd(t *testing.T, m *MeasureSet, other MeasureSet) {
	t.Helper()
	if err := m.Add(other); err != nil {
		t.Fatalf("Add(%+v): %v", other, err)
	}
}

func TestClosedSets(t *testing.T) {
	// The dimension set is EXACTLY tenant/user/session/model. Agent is not
	// a v1.28 rollup dimension — even as an empty axis it must be rejected.
	if len(AllDimensions) != 4 {
		t.Fatalf("closed dimensions = %d; want 4 (tenant, user, session, model)", len(AllDimensions))
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
	// Unknown / former-agent / unsupported values are refused loudly.
	for _, d := range []Dimension{"agent", "agent_id", "run", "task"} {
		if err := d.Validate(); err == nil {
			t.Fatalf("dimension %q must fail validation (not in the closed set)", d)
		}
	}
	for _, m := range []Measure{"tokens_estimated", "llm_attempts", "llm_failed_calls", "user_messages", "llm_cost_usd"} {
		if err := m.Validate(); err == nil {
			t.Fatalf("measure %q must fail validation (no canonical source)", m)
		}
	}
}

func TestStoreGranularity_IsMinute(t *testing.T) {
	if StoreGranularity != BucketMinute {
		t.Fatalf("StoreGranularity = %q; want %q (the fixed UTC minute grid)", StoreGranularity, BucketMinute)
	}
	for _, size := range AllBucketSizes {
		if size == BucketHour {
			continue
		}
		if err := size.Validate(); err != nil {
			t.Fatalf("closed bucket size %q must validate: %v", size, err)
		}
	}
}

// TestTripleOf_NoNULCollision pins the fence-key contract: identity
// validation permits NUL inside ids, so NUL-joined string keys would alias
// distinct triples; the comparable SessionTriple struct never does.
func TestTripleOf_NoNULCollision(t *testing.T) {
	a := identity.Identity{TenantID: "a\x00b", UserID: "c", SessionID: "d"}
	b := identity.Identity{TenantID: "a", UserID: "b\x00c", SessionID: "d"}
	if a == b {
		t.Fatal("fixture identities must differ")
	}
	if TripleOf(a) == TripleOf(b) {
		t.Fatal("SessionTriple must distinguish triples that NUL-joining would alias")
	}
	if TripleOf(a) == (SessionTriple{}) || TripleOf(b) == (SessionTriple{}) {
		t.Fatal("SessionTriple must preserve every component")
	}
}
