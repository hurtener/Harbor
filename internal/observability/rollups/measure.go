package rollups

import (
	"errors"
	"fmt"
	"math"
)

// CostScaleMicros is the decimal denominator of the cost measure: one USD =
// CostScaleMicros micro-units. Cost is accumulated, stored, and queried
// ONLY as integer micro-units — never as float64. The source float cost is
// converted to micro-units exactly once, per canonical event, in Extract
// (see microsFromUSD); a consumer formats decimal USD at the edge as
// N / CostScaleMicros, in exact integer arithmetic.
const CostScaleMicros uint32 = 1_000_000

// Measure is a closed, additive rollup measure. Every measure is sourced
// ONLY from existing canonical event payloads — there are no derived,
// estimated, or sampled values. Measures that have no canonical source are
// ABSENT from the set: they cannot be requested (the query fails loud) and
// no row ever carries them. In particular, attempts, failed LLM calls, and
// user-message counts have no canonical payload backing and are absent.
//
// All measures accumulate in exact integer form (int64): counts and token
// counts are plain integers, cost is integer micro-units of USD (see
// CostScaleMicros), latency is integer milliseconds. Nothing is normalised
// to float64 anywhere in accumulation, storage, or query — see MeasureValue.
//
// Most measures are sums; the latency min/max measures are folds (the
// per-group minimum / maximum of the per-event latencies), merged by
// MeasureSet.Add — which range-checks every additive field and fails loudly
// (ErrMeasureOverflow) rather than letting a sum wrap.
type Measure string

const (
	// MeasureLLMCompletions is the count of successfully-recorded LLM
	// completions (`llm.cost.recorded` events). "Successful" here means the
	// provider returned a completion AND the runtime emitted the cost
	// record for it — the only successful-completion signal the canonical
	// payloads carry.
	MeasureLLMCompletions Measure = "llm_completions"
	// MeasureLLMTokensPrompt is the sum of Usage.PromptTokens.
	MeasureLLMTokensPrompt Measure = "llm_tokens_prompt"
	// MeasureLLMTokensCompletion is the sum of Usage.CompletionTokens.
	MeasureLLMTokensCompletion Measure = "llm_tokens_completion"
	// MeasureLLMTokensReasoning is the sum of Usage.ReasoningTokens.
	MeasureLLMTokensReasoning Measure = "llm_tokens_reasoning"
	// MeasureLLMTokensCacheRead is the sum of Usage.CacheReadTokens.
	MeasureLLMTokensCacheRead Measure = "llm_tokens_cache_read"
	// MeasureLLMTokensCacheWrite is the sum of Usage.CacheWriteTokens.
	MeasureLLMTokensCacheWrite Measure = "llm_tokens_cache_write"
	// MeasureLLMTokensTotal is the sum of Usage.TotalTokens.
	MeasureLLMTokensTotal Measure = "llm_tokens_total"
	// MeasureLLMCostMicros is the sum of provider-reported TotalCost in
	// exact integer micro-units of USD (USD * CostScaleMicros), across
	// successful LLM completions. The source float is converted once per
	// canonical event in Extract; no float is accumulated or exposed.
	MeasureLLMCostMicros Measure = "llm_cost_micros"
	// MeasureLLMLatencyCount is the count of latency-bearing completions
	// (the same population as MeasureLLMCompletions).
	MeasureLLMLatencyCount Measure = "llm_latency_count"
	// MeasureLLMLatencySumMS is the sum of Usage.LatencyMS. Average latency
	// for a group is SumMS / LatencyCount (exact integer arithmetic when
	// the quotient is exact).
	MeasureLLMLatencySumMS Measure = "llm_latency_sum_ms"
	// MeasureLLMLatencyMinMS is the minimum Usage.LatencyMS in the group.
	// Defined exactly when MeasureLLMLatencyCount > 0.
	MeasureLLMLatencyMinMS Measure = "llm_latency_min_ms"
	// MeasureLLMLatencyMaxMS is the maximum Usage.LatencyMS in the group.
	// Defined exactly when MeasureLLMLatencyCount > 0.
	MeasureLLMLatencyMaxMS Measure = "llm_latency_max_ms"
	// MeasureTasksCompleted is the count of `task.completed` events.
	MeasureTasksCompleted Measure = "tasks_completed"
	// MeasureTasksFailed is the count of `task.failed` events.
	MeasureTasksFailed Measure = "tasks_failed"
	// MeasureTasksCancelled is the count of `task.cancelled` events.
	MeasureTasksCancelled Measure = "tasks_cancelled"
)

// AllMeasures is the closed measure set in canonical order.
var AllMeasures = [...]Measure{
	MeasureLLMCompletions,
	MeasureLLMTokensPrompt,
	MeasureLLMTokensCompletion,
	MeasureLLMTokensReasoning,
	MeasureLLMTokensCacheRead,
	MeasureLLMTokensCacheWrite,
	MeasureLLMTokensTotal,
	MeasureLLMCostMicros,
	MeasureLLMLatencyCount,
	MeasureLLMLatencySumMS,
	MeasureLLMLatencyMinMS,
	MeasureLLMLatencyMaxMS,
	MeasureTasksCompleted,
	MeasureTasksFailed,
	MeasureTasksCancelled,
}

// MeasureCount is the fixed width of a MeasureSet (len(AllMeasures)).
const MeasureCount = len(AllMeasures)

// Validate reports whether m is a closed measure.
func (m Measure) Validate() error {
	for _, closed := range AllMeasures {
		if m == closed {
			return nil
		}
	}
	return fmt.Errorf("%w: measure %q (allowed: %v)", ErrQueryInvalid, m, allMeasures())
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
// field is an exact integer accumulation of the source payload values: int64
// for tokens, latency, counts, and cost micro-units — there is NO float64
// field. A zero MeasureSet is the additive identity.
//
// Latency min/max are folds, not sums: Add merges them by taking the
// group-wise minimum / maximum. LLMLatencyMinMS / LLMLatencyMaxMS are defined
// exactly when LLMLatencyCount > 0 (the hasLatency flag tracks whether any
// latency-bearing record has been folded in).
//
// Accumulation is CHECKED: every additive field is range-verified against
// the exact int64 bounds before any write, and a merge that would overflow
// fails loudly with ErrMeasureOverflow instead of wrapping into negative or
// corrupt data. The receiver is never partially mutated by a refused merge.
type MeasureSet struct {
	LLMCompletions      int64
	LLMTokensPrompt     int64
	LLMTokensCompletion int64
	LLMTokensReasoning  int64
	LLMTokensCacheRead  int64
	LLMTokensCacheWrite int64
	LLMTokensTotal      int64
	LLMCostMicros       int64
	LLMLatencyCount     int64
	LLMLatencySumMS     int64
	LLMLatencyMinMS     int64 // fold-min; defined when LLMLatencyCount > 0
	LLMLatencyMaxMS     int64 // fold-max; defined when LLMLatencyCount > 0
	TasksCompleted      int64
	TasksFailed         int64
	TasksCancelled      int64

	// hasLatency records that at least one latency-bearing record has been
	// folded in, so the min/max identity (unset) never collides with a real
	// zero latency. Unexported: only the rollups package constructs
	// MeasureSet literals with it (Extract); every other package merges via
	// Add / reads via Get.
	hasLatency bool
}

// ErrMeasureOverflow reports that an accumulation would overflow the exact
// int64 measure representation. The merge is REFUSED before any field is
// mutated: a row is never left partially updated, and an overflowing sum
// never wraps into negative or corrupt data. ApplyBatch rejects the whole
// batch and Query aggregation fails loudly with this sentinel.
var ErrMeasureOverflow = errors.New("rollups: measure accumulation overflow")

// Add accumulates other into m in place. Sum measures add; the latency
// min/max fold as the group-wise minimum / maximum. Every additive field
// (counts, tokens, cost micro-units, latency sums) is range-checked against
// the exact int64 bounds BEFORE the first write: when any sum would
// overflow, Add fails loudly with ErrMeasureOverflow and m is left EXACTLY
// as it was — no partial merge, no wrapped-negative counter. The latency
// folds never overflow (they are comparisons, not sums) and remain exact.
// All measure values are non-negative in this domain; the check covers both
// directions so a negative delta is equally refused rather than silently
// corrupting a counter.
func (m *MeasureSet) Add(other MeasureSet) error {
	var (
		completions      int64
		tokensPrompt     int64
		tokensCompletion int64
		tokensReasoning  int64
		tokensCacheRead  int64
		tokensCacheWrite int64
		tokensTotal      int64
		costMicros       int64
		latencyCount     int64
		latencySumMS     int64
		tasksCompleted   int64
		tasksFailed      int64
		tasksCancelled   int64
	)
	var err error
	if completions, err = checkedSum(m.LLMCompletions, other.LLMCompletions, MeasureLLMCompletions); err != nil {
		return err
	}
	if tokensPrompt, err = checkedSum(m.LLMTokensPrompt, other.LLMTokensPrompt, MeasureLLMTokensPrompt); err != nil {
		return err
	}
	if tokensCompletion, err = checkedSum(m.LLMTokensCompletion, other.LLMTokensCompletion, MeasureLLMTokensCompletion); err != nil {
		return err
	}
	if tokensReasoning, err = checkedSum(m.LLMTokensReasoning, other.LLMTokensReasoning, MeasureLLMTokensReasoning); err != nil {
		return err
	}
	if tokensCacheRead, err = checkedSum(m.LLMTokensCacheRead, other.LLMTokensCacheRead, MeasureLLMTokensCacheRead); err != nil {
		return err
	}
	if tokensCacheWrite, err = checkedSum(m.LLMTokensCacheWrite, other.LLMTokensCacheWrite, MeasureLLMTokensCacheWrite); err != nil {
		return err
	}
	if tokensTotal, err = checkedSum(m.LLMTokensTotal, other.LLMTokensTotal, MeasureLLMTokensTotal); err != nil {
		return err
	}
	if costMicros, err = checkedSum(m.LLMCostMicros, other.LLMCostMicros, MeasureLLMCostMicros); err != nil {
		return err
	}
	if latencyCount, err = checkedSum(m.LLMLatencyCount, other.LLMLatencyCount, MeasureLLMLatencyCount); err != nil {
		return err
	}
	if latencySumMS, err = checkedSum(m.LLMLatencySumMS, other.LLMLatencySumMS, MeasureLLMLatencySumMS); err != nil {
		return err
	}
	if tasksCompleted, err = checkedSum(m.TasksCompleted, other.TasksCompleted, MeasureTasksCompleted); err != nil {
		return err
	}
	if tasksFailed, err = checkedSum(m.TasksFailed, other.TasksFailed, MeasureTasksFailed); err != nil {
		return err
	}
	if tasksCancelled, err = checkedSum(m.TasksCancelled, other.TasksCancelled, MeasureTasksCancelled); err != nil {
		return err
	}

	// Latency min/max are folds — exact comparisons, never sums, so they
	// cannot overflow. Merge them into locals and commit with the sums.
	minMS, maxMS, hasLatency := m.LLMLatencyMinMS, m.LLMLatencyMaxMS, m.hasLatency
	if other.hasLatency {
		if !hasLatency || other.LLMLatencyMinMS < minMS {
			minMS = other.LLMLatencyMinMS
		}
		if !hasLatency || other.LLMLatencyMaxMS > maxMS {
			maxMS = other.LLMLatencyMaxMS
		}
		hasLatency = true
	}

	// Commit — reached only when every additive field fit the int64 range.
	m.LLMCompletions = completions
	m.LLMTokensPrompt = tokensPrompt
	m.LLMTokensCompletion = tokensCompletion
	m.LLMTokensReasoning = tokensReasoning
	m.LLMTokensCacheRead = tokensCacheRead
	m.LLMTokensCacheWrite = tokensCacheWrite
	m.LLMTokensTotal = tokensTotal
	m.LLMCostMicros = costMicros
	m.LLMLatencyCount = latencyCount
	m.LLMLatencySumMS = latencySumMS
	m.LLMLatencyMinMS = minMS
	m.LLMLatencyMaxMS = maxMS
	m.hasLatency = hasLatency
	m.TasksCompleted = tasksCompleted
	m.TasksFailed = tasksFailed
	m.TasksCancelled = tasksCancelled
	return nil
}

// checkedSum returns a+b when the exact int64 sum is representable, failing
// loudly (wrapped ErrMeasureOverflow naming the measure) when it would
// overflow — the additive counterpart of the exact-cost range check in
// microsFromUSD.
func checkedSum(a, b int64, measure Measure) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, fmt.Errorf("%w: %s sum would overflow the int64 range", ErrMeasureOverflow, measure)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, fmt.Errorf("%w: %s sum would overflow the int64 range", ErrMeasureOverflow, measure)
	}
	return a + b, nil
}

// Get returns the exact value of measure m. m MUST be a closed measure —
// Validate is the entry point for untrusted input; Get assumes the caller
// validated. The returned MeasureValue carries the measure's fixed decimal
// scale, so a consumer formats decimal USD exactly without any float.
func (m MeasureSet) Get(measure Measure) MeasureValue {
	switch measure {
	case MeasureLLMCompletions:
		return MeasureValue{N: m.LLMCompletions, Scale: 1}
	case MeasureLLMTokensPrompt:
		return MeasureValue{N: m.LLMTokensPrompt, Scale: 1}
	case MeasureLLMTokensCompletion:
		return MeasureValue{N: m.LLMTokensCompletion, Scale: 1}
	case MeasureLLMTokensReasoning:
		return MeasureValue{N: m.LLMTokensReasoning, Scale: 1}
	case MeasureLLMTokensCacheRead:
		return MeasureValue{N: m.LLMTokensCacheRead, Scale: 1}
	case MeasureLLMTokensCacheWrite:
		return MeasureValue{N: m.LLMTokensCacheWrite, Scale: 1}
	case MeasureLLMTokensTotal:
		return MeasureValue{N: m.LLMTokensTotal, Scale: 1}
	case MeasureLLMCostMicros:
		return MeasureValue{N: m.LLMCostMicros, Scale: CostScaleMicros}
	case MeasureLLMLatencyCount:
		return MeasureValue{N: m.LLMLatencyCount, Scale: 1}
	case MeasureLLMLatencySumMS:
		return MeasureValue{N: m.LLMLatencySumMS, Scale: 1}
	case MeasureLLMLatencyMinMS:
		return MeasureValue{N: m.LLMLatencyMinMS, Scale: 1}
	case MeasureLLMLatencyMaxMS:
		return MeasureValue{N: m.LLMLatencyMaxMS, Scale: 1}
	case MeasureTasksCompleted:
		return MeasureValue{N: m.TasksCompleted, Scale: 1}
	case MeasureTasksFailed:
		return MeasureValue{N: m.TasksFailed, Scale: 1}
	case MeasureTasksCancelled:
		return MeasureValue{N: m.TasksCancelled, Scale: 1}
	default:
		panic(fmt.Sprintf("rollups: MeasureSet.Get with unvalidated measure %q", measure))
	}
}

// IsZero reports whether every field is zero (the additive identity).
func (m MeasureSet) IsZero() bool {
	return m == MeasureSet{}
}

// microsFromUSD converts a provider-reported float cost (USD) into exact
// integer micro-units, deterministically. This is the ONLY float→integer
// conversion in the domain: it runs once per canonical event in Extract.
//
// The conversion is strict — it FAILS LOUDLY (wrapping ErrInvalidCost) for
// any value that cannot be represented as a deterministic nonnegative exact
// integer: NaN, ±Inf, negative costs, and costs whose micro-unit value
// exceeds the int64 range. Rounding is deterministic: math.Round (half away
// from zero) of the micro-scaled value, so e.g. 0.1 and 0.2 each convert to
// exact 100_000 / 200_000 micro-units and SUM to exactly 300_000 — no
// accumulated float drift.
func microsFromUSD(usd float64) (int64, error) {
	if math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0, fmt.Errorf("%w: cost %v is not finite", ErrInvalidCost, usd)
	}
	if usd < 0 {
		return 0, fmt.Errorf("%w: cost %v is negative", ErrInvalidCost, usd)
	}
	scaled := usd * float64(CostScaleMicros)
	// Airtight range check: refuse at/above 2^63 (float64(math.MaxInt64)
	// rounds up to exactly 2^63). The largest representable float below
	// 2^63 is 2^63-1024, and math.Round of any representable scaled < 2^63
	// stays ≤ 2^63-1024 < math.MaxInt64, so the int64 conversion below is
	// always in range — never implementation-defined.
	if scaled >= float64(math.MaxInt64) {
		return 0, fmt.Errorf("%w: cost %v exceeds the micro-unit int64 range", ErrInvalidCost, usd)
	}
	return int64(math.Round(scaled)), nil
}

func allMeasures() []string {
	out := make([]string, 0, len(AllMeasures))
	for _, m := range AllMeasures {
		out = append(out, string(m))
	}
	return out
}
