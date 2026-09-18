package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// TrajectorySummary is the canonical name for the trajectory's
// compaction artefact. Aliased onto the [trajectory.Summary]
// struct: same shape, the alias matches RFC §6.2 + the master-plan
// vocabulary so callers outside the trajectory package use
// the RFC name. Five fields per RFC §6.2: `Goals`, `Facts`, `Pending`,
// `LastOutputDigest`, `Note`.
type TrajectorySummary = trajectory.Summary

// Summariser is the runtime-side interface a configured compaction
// driver implements. The [CompressionRunner] calls
// [Summariser.Summarise] when the trajectory's token estimate exceeds
// [Budget.TokenBudget].
//
// Fail-loudly contract (CLAUDE.md §13): an error from Summarise
// propagates verbatim through [CompressionRunner.MaybeCompress] — no
// silent fall-through to raw history. Returning (nil, nil) is also a
// contract violation; the runner surfaces [ErrEmptySummary] so the
// bug is loud, not silent.
//
// Implementations MUST be safe for concurrent use across runs (the
// runner is a reusable artifact per the concurrent-reuse contract; the summariser is called
// under the run's ctx from MaybeCompress).
//
// The production implementation is the LLM-backed
// TrajectorySummariser in internal/llm/summarizer:
// it binds an LLM client + a versioned compaction prompt,
// invokes [llm.LLMClient.Complete] over the trajectory's
// planner-facing projection (structured-output JSON-schema
// mode with the existing downgrade ladder), and parses the response
// into the five [TrajectorySummary] fields. The production call site
// is the steering RunLoop's step loop, which calls
// [CompressionRunner.MaybeCompress] at each step boundary when
// [Budget.TokenBudget] > 0 — wired from the `planner.token_budget`
// config knob by the runtime assembly.
type Summariser interface {
	Summarise(ctx context.Context, rc RunContext, tr *Trajectory) (*TrajectorySummary, error)
}

// TokenEstimator estimates the active trajectory projection for standalone
// compaction callers. Complete request admission remains the LLM edge's job.
type TokenEstimator func(tr *Trajectory) (int, error)

// DefaultTokenEstimator excludes covered steps and diagnostic/raw duplicates.
// The reference planner's assembled-request budget includes its other sections.
func DefaultTokenEstimator(tr *Trajectory) (int, error) {
	if tr == nil {
		return 0, ErrNilTrajectory
	}
	start, err := tr.ReplayStart()
	if err != nil {
		return 0, err
	}
	view := *tr
	view.Summary = tr.ActiveSummary()
	view.Steps = make([]Step, len(tr.Steps)-start)
	for i, step := range tr.Steps[start:] {
		view.Steps[i] = trajectory.ModelStep(step)
	}
	view.ToolContext = trajectory.ToolContext{}
	view.Background = nil
	view.HintState = nil
	view.Sources = nil
	view.Artifacts = nil
	view.ResumeHint = nil
	view.TrancheBaseline = 0
	b, err := view.Serialize()
	if err != nil {
		return 0, fmt.Errorf("default token estimator: %w", err)
	}
	return len(b)/4 + 1, nil
}

// CompressionRunner replaces eligible older exchanges with a portable summary.
// The receiver is immutable; callers serialize mutation of each trajectory.
type CompressionRunner struct {
	summariser Summariser
	estimator  TokenEstimator
}

// CompressionOption configures a [CompressionRunner] at construction.
type CompressionOption func(*CompressionRunner)

// WithTokenEstimator overrides [DefaultTokenEstimator]. Tests use this
// to inject a deterministic counter; production wiring uses the
// default.
//
// A nil estimator is a no-op (the option is dropped) — defensive
// against callers wiring options in a loop.
func WithTokenEstimator(est TokenEstimator) CompressionOption {
	return func(r *CompressionRunner) {
		if est != nil {
			r.estimator = est
		}
	}
}

// NewCompressionRunner constructs a [CompressionRunner] from the
// supplied [Summariser] + options.
//
// **Panics on nil summariser** — composition error caught at boot,
// matching [react.New]'s nil-client behaviour. Operators that need a
// "no-op runner" should pass a no-op Summariser implementation, not
// nil; nil is a contract violation surfaced as a panic.
func NewCompressionRunner(summariser Summariser, opts ...CompressionOption) *CompressionRunner {
	if summariser == nil {
		panic("planner.NewCompressionRunner: nil Summariser")
	}
	r := &CompressionRunner{
		summariser: summariser,
		estimator:  DefaultTokenEstimator,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// MaybeCompress summarizes an older prefix, retaining the latest exchange and
// a recent tail targeted at one quarter of the working budget. Existing steps
// remain available for inspection; coverage alone selects prompt replay.
// Failures leave the previous checkpoint unchanged. Callers must serialize
// writes to tr; the steering loop snapshots outside its inspection mutex.
func (r *CompressionRunner) MaybeCompress(ctx context.Context, rc RunContext, tr *Trajectory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := assertCompressionIdentity(rc); err != nil {
		return err
	}
	if tr == nil {
		return ErrNilTrajectory
	}
	start, err := tr.ReplayStart()
	if err != nil {
		return err
	}
	if rc.Budget.TokenBudget <= 0 {
		return nil
	}
	estimate, err := r.estimator(tr)
	if err != nil {
		emitCompressionFailed(ctx, rc, tr, 0, "estimator_error", err)
		return fmt.Errorf("planner compression: estimator: %w", err)
	}
	if estimate <= rc.Budget.TokenBudget || len(tr.Steps)-start < 2 {
		return nil
	}
	// Always preserve the last complete exchange, even when it alone exceeds
	// the tail target. The physical request guard still applies afterward.
	end := len(tr.Steps) - 1
	tailTokens := 0
	for i := len(tr.Steps) - 1; i >= start; i-- {
		b, encErr := json.Marshal(trajectory.ModelStep(tr.Steps[i]))
		if encErr != nil {
			return fmt.Errorf("planner compression: step encoding: %w", encErr)
		}
		n := len(b)/4 + 1
		if i != len(tr.Steps)-1 && tailTokens+n > rc.Budget.TokenBudget/4 {
			break
		}
		tailTokens += n
		end = i
	}
	if tr.UnseenFrom != nil {
		if *tr.UnseenFrom < 0 || *tr.UnseenFrom > len(tr.Steps) {
			return trajectory.ErrInvalidCoverage
		}
		end = min(end, *tr.UnseenFrom)
	}
	if end <= start {
		return nil
	}
	original := tr.Summary
	digest, err := tr.PrefixDigest(end)
	if err != nil {
		return err
	}
	input := &Trajectory{Query: tr.Query, Summary: trajectory.CloneSummary(tr.ActiveSummary())}
	if input.Summary != nil {
		input.Summary.Coverage = nil // metadata is not generated narrative
	}
	for _, step := range tr.Steps[start:end] {
		input.Steps = append(input.Steps, trajectory.ModelStep(step))
	}
	// Detach JSON trees from caller-owned observations before invoking an
	// extension. A misbehaving summarizer cannot mutate live evidence.
	encoded, err := input.Serialize()
	if err != nil {
		return err
	}
	input, err = trajectory.Deserialize(encoded)
	if err != nil {
		return err
	}
	summaryRC := rc
	summaryRC.Trajectory = input
	result, err := r.summariser.Summarise(ctx, summaryRC, input)
	if err != nil {
		emitCompressionFailed(ctx, rc, tr, estimate, "summariser_error", err)
		return fmt.Errorf("planner compression: summariser: %w", err)
	}
	if !result.HasContent() {
		emitCompressionFailed(ctx, rc, tr, estimate, "empty_summary", ErrEmptySummary)
		return ErrEmptySummary
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := tr.PrefixDigest(end)
	if err != nil || current != digest || tr.Summary != original {
		return ErrStaleSummary
	}
	generation := uint64(1)
	if original != nil && original.Coverage != nil {
		generation = original.Coverage.Generation + 1
		if generation == 0 {
			return trajectory.ErrInvalidCoverage
		}
	}
	candidate := trajectory.CloneSummary(result)
	candidate.Coverage = &trajectory.SummaryCoverage{Version: 1, Generation: generation, ThroughStep: end, PrefixDigest: digest}
	tr.Summary = candidate
	emitCompressionSucceeded(ctx, rc, len(tr.Steps), len(tr.Steps), estimate)
	return nil
}

// Sentinel errors. Use errors.Is.
var (
	// ErrStaleSummary refuses a checkpoint whose source changed during generation.
	ErrStaleSummary = errors.New("planner: summary source changed during generation")
	// ErrNilTrajectory is the fail-loud sentinel returned by
	// [CompressionRunner.MaybeCompress] (and [DefaultTokenEstimator])
	// when the supplied trajectory pointer is nil. Distinct from the
	// "no budget set" / "under threshold" no-op paths — nil is a
	// composition error (the caller passed a wrong value); the runner
	// surfaces it loudly rather than treating it as "skip".
	ErrNilTrajectory = errors.New("planner: compression refuses nil trajectory")

	// ErrEmptySummary is the fail-loud sentinel returned by
	// [CompressionRunner.MaybeCompress] when the [Summariser] returns
	// (nil, nil). The summariser contract is "return a non-nil
	// summary on success OR a non-nil error"; returning (nil, nil) is
	// a bug, not a recovery state. The runner refuses to stamp a nil
	// summary; the call surfaces the contract violation loudly.
	ErrEmptySummary = errors.New("planner: summariser returned (nil, nil) — contract violation")
)

// assertCompressionIdentity rejects calls whose [RunContext.Quadruple]
// is missing any of the four scope components. Returns wrapped
// [llm.ErrIdentityMissing] for parity with the LLM-client edge (and
// the repair loop and react planner) — the runner
// fails closed with the same sentinel the rest of the runtime uses
// (§6 rule 9).
func assertCompressionIdentity(rc RunContext) error {
	q := rc.Quadruple
	if q.TenantID == "" || q.UserID == "" || q.SessionID == "" || q.RunID == "" {
		return fmt.Errorf(
			"%w (planner.CompressionRunner refuses missing-identity MaybeCompress)",
			llm.ErrIdentityMissing,
		)
	}
	return nil
}

// emitCompressionSucceeded publishes trajectory.compressed onto the
// run's emit closure. Best-effort; never blocks (subscribers handle
// their own drop policies). nil Emit means "no
// observability wired" — typical in tests; production runtime always
// wires Emit.
func emitCompressionSucceeded(
	ctx context.Context,
	rc RunContext,
	stepsBefore, stepsAfter, tokenEstimate int,
) {
	if rc.Emit == nil {
		return
	}
	now := nowFromRC(rc)
	rc.Emit(events.Event{
		Type:       EventTypeTrajectoryCompressed,
		Identity:   rc.Quadruple,
		OccurredAt: now,
		Payload: TrajectoryCompressedPayload{
			Identity:      rc.Quadruple,
			StepsBefore:   stepsBefore,
			StepsAfter:    stepsAfter,
			TokenEstimate: tokenEstimate,
			OccurredAt:    now,
		},
	})
	_ = ctx // ctx reserved for future cancellation-aware emits.
}

// emitCompressionFailed publishes trajectory.compression_failed onto
// the run's emit closure. Mirrors emitCompressionSucceeded; carries
// the error code + truncated message + the trajectory's step count
// at the moment of failure.
func emitCompressionFailed(
	ctx context.Context,
	rc RunContext,
	tr *Trajectory,
	tokenEstimate int,
	code string,
	cause error,
) {
	if rc.Emit == nil {
		return
	}
	stepsObserved := 0
	if tr != nil {
		stepsObserved = len(tr.Steps)
	}
	now := nowFromRC(rc)
	msg := ""
	if cause != nil {
		msg = truncateErrorMessage(cause.Error(), compressionErrorMessageCap)
	}
	rc.Emit(events.Event{
		Type:       EventTypeTrajectoryCompressionFailed,
		Identity:   rc.Quadruple,
		OccurredAt: now,
		Payload: TrajectoryCompressionFailedPayload{
			Identity:      rc.Quadruple,
			StepsObserved: stepsObserved,
			TokenEstimate: tokenEstimate,
			ErrorCode:     code,
			ErrorMessage:  msg,
			OccurredAt:    now,
		},
	})
	_ = ctx
}

// compressionErrorMessageCap is the byte cap on
// [TrajectoryCompressionFailedPayload.ErrorMessage] — keeps audit
// payloads bounded against runaway summariser error messages.
const compressionErrorMessageCap = 256

// truncateErrorMessage truncates s to at most n bytes, appending an
// ellipsis marker when truncation happens.
func truncateErrorMessage(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 4 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// nowFromRC reads [RunContext.Clock] when present, else falls back to
// wall-clock. Tests fix the clock to make event-payload timestamp
// assertions deterministic.
func nowFromRC(rc RunContext) time.Time {
	if rc.Clock != nil {
		return rc.Clock()
	}
	return time.Now()
}
