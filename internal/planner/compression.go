package planner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// TrajectorySummary is the canonical name for the trajectory's
// compaction artefact. Aliased onto the Phase 43 [trajectory.Summary]
// struct: same shape, the alias matches RFC §6.2 + the master-plan
// Phase 46 vocabulary so callers outside the trajectory package use
// the RFC name. Five fields per RFC §6.2: `Goals`, `Facts`, `Pending`,
// `LastOutputDigest`, `Note`. D-055.
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
// runner is a reusable artifact per D-025; the summariser is called
// under the run's ctx from MaybeCompress).
//
// Production wiring binds an LLM client + a compaction prompt; the
// summariser invokes [llm.LLMClient.Complete] with the trajectory's
// state and parses the response into the five [TrajectorySummary]
// fields. Phase 46 ships the seam; the production LLM-backed
// summariser lands when the runtime engine consumes the runner
// (Phase 47+).
type Summariser interface {
	Summarise(ctx context.Context, rc RunContext, tr *Trajectory) (*TrajectorySummary, error)
}

// TokenEstimator is the runner's pluggable token-count function. The
// default implementation ([DefaultTokenEstimator]) walks
// [trajectory.Trajectory.Serialize] bytes and returns `len/4 + 1` —
// mirroring [internal/llm/tokens.go]'s `chars4Estimator` so the two
// estimators agree (single surface; no parallel implementation per
// §13). D-055.
//
// Estimator errors propagate; an [trajectory.ErrUnserializable] from
// Serialize is the typical failure mode and is surfaced verbatim
// (Phase 43 fail-loudly contract).
type TokenEstimator func(tr *Trajectory) (int, error)

// DefaultTokenEstimator is the chars/4 estimator backed by
// [trajectory.Trajectory.Serialize]. Used when [WithTokenEstimator]
// is not set.
//
// A nil trajectory returns (0, [ErrNilTrajectory]) — the estimator
// fails closed rather than returning a meaningless zero estimate.
//
// The chars/4 algorithm under-counts multimodal content compared to
// the LLM-edge estimator (which adds 256 tokens per non-text part);
// trajectories don't typically carry multimodal parts directly in
// LLMContext (heavy content is upstream of the trajectory per the
// D-026 safety pass), so the simpler walker is sufficient at Phase
// 46. A future estimator that structurally walks the trajectory is a
// Phase 47+ refinement; the [TokenEstimator] functional-option seam
// is the unwind point.
func DefaultTokenEstimator(tr *Trajectory) (int, error) {
	if tr == nil {
		return 0, ErrNilTrajectory
	}
	b, err := tr.Serialize()
	if err != nil {
		return 0, fmt.Errorf("default token estimator: %w", err)
	}
	// chars/4 + 1 mirrors internal/llm/tokens.go::chars4Estimator's
	// per-fragment formula. The trailing +1 is the single-token
	// overhead the LLM-edge estimator applies per text fragment;
	// applied once here (the trajectory is treated as one fragment by
	// the runner since the Serialize byte stream is the planner-facing
	// projection).
	return len(b)/4 + 1, nil
}

// CompressionRunner drives the "estimate → optional summariser → stamp
// Trajectory.Summary" loop the runtime invokes between planner steps
// (or at any cadence the engine decides — Phase 47+ owns the cadence
// policy).
//
// Reusable artifact (D-025): one constructed instance is safe to
// share across N concurrent runs; per-call state lives entirely in
// `ctx` + [RunContext] + the per-call serialised bytes. The receiver
// is read-only after construction.
//
// **Idempotent on `tr.Summary != nil`.** A second call with an
// already-stamped trajectory returns nil without invoking the
// summariser. The engine that owns the cadence policy is the layer
// responsible for clearing the summary when re-compaction is needed
// (a Phase 47+ concern; Phase 46 ships the V1 idempotency contract).
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

// MaybeCompress is the runner's entrypoint. The flow (in order):
//
//  1. Honour `ctx.Err()` at entry; return verbatim if cancelled.
//  2. Reject missing identity (wrapped [llm.ErrIdentityMissing]).
//  3. Defensive nil guard: nil `tr` returns wrapped
//     [ErrNilTrajectory].
//  4. Already-compressed short-circuit: when `tr.Summary != nil`,
//     return nil without calling the summariser (idempotent).
//  5. Compute the token estimate via the configured estimator.
//     Estimator errors propagate (with the
//     `trajectory.compression_failed` emit carrying error code
//     "estimator_error").
//  6. Budget check: when `rc.Budget.TokenBudget <= 0`, return nil (no
//     budget enforced).
//  7. When `estimate <= rc.Budget.TokenBudget`, return nil (below
//     threshold).
//  8. Invoke `summariser.Summarise(ctx, rc, tr)`. A non-nil error →
//     emit `trajectory.compression_failed` with code
//     "summariser_error", return the error wrapped. A nil
//     `*TrajectorySummary` with nil error → wrapped [ErrEmptySummary]
//     + emit with code "empty_summary".
//  9. Stamp `tr.Summary = result` and emit `trajectory.compressed`
//     carrying the identity + before/after step count + token
//     estimate.
//  10. Return nil.
//
// **Fail-loudly (CLAUDE.md §13).** Every error path emits the
// failure event before returning; there is no silent fall-through to
// raw history. The success-path emit pairs with the failure-path emit
// so compression is observable in both directions.
//
// **Identity is mandatory (§6 rule 9 + D-001).** A partial quadruple
// returns wrapped [llm.ErrIdentityMissing] — parity with the
// react/repair planner's identity-rejection sentinel.
//
// **Reusable (D-025).** Safe to invoke concurrently against a single
// shared runner from N goroutines. Per-call state lives in ctx + rc
// + the per-call serialised bytes; the receiver is read-only.
func (r *CompressionRunner) MaybeCompress(
	ctx context.Context,
	rc RunContext,
	tr *Trajectory,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := assertCompressionIdentity(rc); err != nil {
		return err
	}
	if tr == nil {
		return fmt.Errorf("planner.CompressionRunner.MaybeCompress: %w", ErrNilTrajectory)
	}

	// Idempotency: already compressed, nothing to do. The engine that
	// owns the cadence policy clears tr.Summary when re-compaction is
	// needed (Phase 47+ concern); Phase 46 ships the V1 short-circuit.
	if tr.Summary != nil {
		return nil
	}

	estimate, err := r.estimator(tr)
	if err != nil {
		// Fail-loudly per §13: emit the failure event before
		// returning so observability picks up the estimator failure
		// (typically a Phase 43 ErrUnserializable surfaced through
		// Serialize).
		emitCompressionFailed(ctx, rc, tr, 0, "estimator_error", err)
		return fmt.Errorf("planner.CompressionRunner.MaybeCompress: estimator: %w", err)
	}

	// No budget set → no compression. Zero is the "unset" sentinel,
	// matching the Budget.HopBudget / Budget.CostCap conventions.
	if rc.Budget.TokenBudget <= 0 {
		return nil
	}

	// Under threshold → no compression.
	if estimate <= rc.Budget.TokenBudget {
		return nil
	}

	// Over budget — invoke the summariser.
	result, summErr := r.summariser.Summarise(ctx, rc, tr)
	if summErr != nil {
		emitCompressionFailed(ctx, rc, tr, estimate, "summariser_error", summErr)
		return fmt.Errorf("planner.CompressionRunner.MaybeCompress: summariser: %w", summErr)
	}
	if result == nil {
		// Contract violation: the Summariser returned (nil, nil).
		// This is a fail-loud surface (§13) — the bug is the
		// implementation's, not the runner's; we surface it loudly
		// rather than papering over with raw history.
		emitCompressionFailed(ctx, rc, tr, estimate, "empty_summary", ErrEmptySummary)
		return fmt.Errorf("planner.CompressionRunner.MaybeCompress: %w", ErrEmptySummary)
	}

	stepsBefore := len(tr.Steps)
	// Stamp the summary. Phase 46 does NOT truncate the Steps slice;
	// the planner observes the compacted view through the prompt
	// builder's summary-only rendering. A future phase MAY truncate;
	// the StepsBefore / StepsAfter event-payload fields are the seam.
	tr.Summary = result
	stepsAfter := len(tr.Steps)

	emitCompressionSucceeded(ctx, rc, stepsBefore, stepsAfter, estimate)
	return nil
}

// Sentinel errors. Use errors.Is.
var (
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
// the Phase 44 repair loop and Phase 45 react planner) — the runner
// fails closed with the same sentinel the rest of the runtime uses
// (§6 rule 9 + D-001).
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
// their own drop policies per Phase 05). nil Emit means "no
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
