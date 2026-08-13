package rollups

import (
	"fmt"
)

// Measure is a closed, additive rollup measure. Every measure is sourced
// ONLY from existing canonical event payloads — there are no derived,
// estimated, or sampled values. Measures that have no canonical source are
// ABSENT from the set: they cannot be requested (the query fails loud) and
// no row ever carries them. All measures are sums (the rollup value for a
// bucket+dimension group is the exact sum of the per-event source values).
type Measure string

const (
	// MeasureLLMCostUSD is the sum of provider-reported TotalCost (USD)
	// across successful LLM completions (`llm.cost.recorded`). Exact
	// float64 sum of the source values — see the package doc on billing
	// claims.
	MeasureLLMCostUSD Measure = "llm_cost_usd"
	// MeasureLLMTokensPrompt is the sum of Usage.PromptTokens.
	MeasureLLMTokensPrompt Measure = "llm_tokens_prompt"
	// MeasureLLMTokensCompletion is the sum of Usage.CompletionTokens.
	MeasureLLMTokensCompletion Measure = "llm_tokens_completion"
	// MeasureLLMTokensTotal is the sum of Usage.TotalTokens.
	MeasureLLMTokensTotal Measure = "llm_tokens_total"
	// MeasureLLMCompletions is the count of successfully-recorded LLM
	// completions (`llm.cost.recorded` events). "Successful" here means the
	// provider returned a completion AND the runtime emitted the cost
	// record for it — the only successful-completion signal the canonical
	// payloads carry.
	MeasureLLMCompletions Measure = "llm_completions"
	// MeasureLLMLatencyMS is the sum of Usage.LatencyMS across LLM
	// completions. Average latency for a group is
	// MeasureLLMLatencyMS / MeasureLLMCompletions (exact integer
	// arithmetic when the quotient is exact).
	MeasureLLMLatencyMS Measure = "llm_latency_ms"
	// MeasureTasksCompleted is the count of `task.completed` events.
	MeasureTasksCompleted Measure = "tasks_completed"
	// MeasureTasksFailed is the count of `task.failed` events.
	MeasureTasksFailed Measure = "tasks_failed"
	// MeasureTasksCancelled is the count of `task.cancelled` events.
	MeasureTasksCancelled Measure = "tasks_cancelled"
)

// AllMeasures is the closed measure set in canonical order.
var AllMeasures = [...]Measure{
	MeasureLLMCostUSD,
	MeasureLLMTokensPrompt,
	MeasureLLMTokensCompletion,
	MeasureLLMTokensTotal,
	MeasureLLMCompletions,
	MeasureLLMLatencyMS,
	MeasureTasksCompleted,
	MeasureTasksFailed,
	MeasureTasksCancelled,
}

// MeasureCount is the fixed width of a MeasureSet (len(AllMeasures)).
const MeasureCount = len(AllMeasures)

// Validate reports whether m is a closed measure.
func (m Measure) Validate() error {
	switch m {
	case MeasureLLMCostUSD,
		MeasureLLMTokensPrompt,
		MeasureLLMTokensCompletion,
		MeasureLLMTokensTotal,
		MeasureLLMCompletions,
		MeasureLLMLatencyMS,
		MeasureTasksCompleted,
		MeasureTasksFailed,
		MeasureTasksCancelled:
		return nil
	default:
		return fmt.Errorf("%w: measure %q (allowed: %v)", ErrQueryInvalid, m, allMeasures())
	}
}

// ValidateMeasures validates a query's Measures: non-empty, every member
// closed, no repeats. Returns a wrapped ErrQueryInvalid otherwise.
func ValidateMeasures(measures []Measure) error {
	if len(measures) == 0 {
		return fmt.Errorf("%w: Measures must name at least one measure", ErrQueryInvalid)
	}
	seen := make(map[Measure]struct{}, len(measures))
	for _, m := range measures {
		if err := m.Validate(); err != nil {
			return err
		}
		if _, dup := seen[m]; dup {
			return fmt.Errorf("%w: Measures repeats measure %q", ErrQueryInvalid, m)
		}
		seen[m] = struct{}{}
	}
	return nil
}

// MeasureSet is the fixed-width set of all measures for one bucket row. Each
// field is an exact sum of the source payload values — int64 for tokens,
// latency, and counts; float64 for cost. A zero MeasureSet is the additive
// identity.
type MeasureSet struct {
	LLMCostUSD          float64
	LLMTokensPrompt     int64
	LLMTokensCompletion int64
	LLMTokensTotal      int64
	LLMCompletions      int64
	LLMLatencyMS        int64
	TasksCompleted      int64
	TasksFailed         int64
	TasksCancelled      int64
}

// Add accumulates other into m in place.
func (m *MeasureSet) Add(other MeasureSet) {
	m.LLMCostUSD += other.LLMCostUSD
	m.LLMTokensPrompt += other.LLMTokensPrompt
	m.LLMTokensCompletion += other.LLMTokensCompletion
	m.LLMTokensTotal += other.LLMTokensTotal
	m.LLMCompletions += other.LLMCompletions
	m.LLMLatencyMS += other.LLMLatencyMS
	m.TasksCompleted += other.TasksCompleted
	m.TasksFailed += other.TasksFailed
	m.TasksCancelled += other.TasksCancelled
}

// Get returns the value of measure m (float64-normalised: int64 fields are
// converted exactly). m MUST be a closed measure — Validate is the entry
// point for untrusted input; Get assumes the caller validated.
func (m MeasureSet) Get(measure Measure) float64 {
	switch measure {
	case MeasureLLMCostUSD:
		return m.LLMCostUSD
	case MeasureLLMTokensPrompt:
		return float64(m.LLMTokensPrompt)
	case MeasureLLMTokensCompletion:
		return float64(m.LLMTokensCompletion)
	case MeasureLLMTokensTotal:
		return float64(m.LLMTokensTotal)
	case MeasureLLMCompletions:
		return float64(m.LLMCompletions)
	case MeasureLLMLatencyMS:
		return float64(m.LLMLatencyMS)
	case MeasureTasksCompleted:
		return float64(m.TasksCompleted)
	case MeasureTasksFailed:
		return float64(m.TasksFailed)
	case MeasureTasksCancelled:
		return float64(m.TasksCancelled)
	default:
		panic(fmt.Sprintf("rollups: MeasureSet.Get with unvalidated measure %q", measure))
	}
}

// IsZero reports whether every field is zero.
func (m MeasureSet) IsZero() bool {
	return m == MeasureSet{}
}

func allMeasures() []string {
	out := make([]string, 0, len(AllMeasures))
	for _, m := range AllMeasures {
		out = append(out, string(m))
	}
	return out
}
