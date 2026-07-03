package llm

import (
	"context"
	"math"
	"sync/atomic"
)

// attemptCostTapKey is the private ctx key under which a governed call's
// attempt-cost tap rides. Unexported so nothing outside this package can
// forge or read the handle by key.
type attemptCostTapKey struct{}

// AttemptCostTap is the per-call, ctx-carried accumulator the governance
// wrapper installs around the LLM-edge wrapper chain so that provider
// attempts CONSUMED by the retry and downgrade wrappers — attempts whose
// response never propagates out to the governed-call boundary — still
// reach cost accounting.
//
// The propagate-or-report invariant is the exactness proof: every wrapper
// between governance and the driver treats each inner Complete outcome in
// exactly one of two ways — it PROPAGATES the response to its caller (so
// that response's Cost reaches the outer boundary and is accounted there),
// or it CONSUMES the response (loops onward / discards it), in which case
// the consuming site MUST Report the consumed attempt's cost into the tap.
// Never both, never neither. Each provider attempt is therefore counted
// exactly once, independent of compose order: a chain that (against the
// documented order) seats governance INSIDE retry simply carries no tap on
// the wrapper-visible ctx, so Report is a no-op and every attempt crosses
// the governance boundary individually — degrading to a no-op, never a
// double-count.
//
// Concurrent reuse (the concurrent-reuse contract): the tap is created
// fresh per governed call and carried on that call's ctx, never stored on
// any reusable wrapper. Its cumulative total is an atomic uint64-packed
// float64 mutated by a lock-free CAS loop (mirroring the cost accumulator's
// own add pattern), so N concurrent Report calls against one tap cannot
// lose an update. Drain is one-shot: the first call returns the folded
// total and attempt count and marks the tap drained; every subsequent call
// returns zero so a stray double-drain cannot double-count.
type AttemptCostTap struct {
	totalBits atomic.Uint64 // math.Float64bits of the cumulative reported USD
	attempts  atomic.Int64  // number of reported consumed attempts
	drained   atomic.Bool   // one-shot Drain gate
}

// ContextWithAttemptCostTap returns a child ctx carrying a fresh
// AttemptCostTap plus a handle to it. The governance wrapper installs one
// per governed call (after PreCall permits) and passes the returned ctx to
// both the inner wrapper chain and PostCall; the handle lets a direct
// caller (or a test) drain without a second ctx lookup.
func ContextWithAttemptCostTap(ctx context.Context) (context.Context, *AttemptCostTap) {
	tap := &AttemptCostTap{}
	return context.WithValue(ctx, attemptCostTapKey{}, tap), tap
}

// attemptCostTapFrom returns the tap carried on ctx, if any.
func attemptCostTapFrom(ctx context.Context) (*AttemptCostTap, bool) {
	tap, ok := ctx.Value(attemptCostTapKey{}).(*AttemptCostTap)
	return tap, ok
}

// ReportAttemptCost reports a CONSUMED provider attempt's cost into the tap
// carried on ctx. It is a deliberate no-op when no tap is installed — an
// absent tap means no accounting consumer exists (governance latent or the
// chain composed without governance), not a swallowed error. Only the
// wrapper site that consumes an attempt (never propagates its response)
// calls this, exactly once per consumed attempt.
func ReportAttemptCost(ctx context.Context, cost Cost) {
	tap, ok := attemptCostTapFrom(ctx)
	if !ok {
		return
	}
	tap.report(cost.TotalCost)
}

// PeekAttemptCost returns the tap's current cumulative total and attempt
// count WITHOUT draining it, or (0, 0) when no tap is installed. The
// governance accumulator uses it to decide whether a PostCall has real
// accounting work before it resolves per-identity key state; the drain
// itself happens only after that resolution succeeds.
func PeekAttemptCost(ctx context.Context) (totalUSD float64, attempts int) {
	tap, ok := attemptCostTapFrom(ctx)
	if !ok {
		return 0, 0
	}
	return tap.peek()
}

// DrainAttemptCost drains the tap carried on ctx exactly once, returning
// the folded total and attempt count, or (0, 0) when no tap is installed or
// the tap was already drained.
func DrainAttemptCost(ctx context.Context) (totalUSD float64, attempts int) {
	tap, ok := attemptCostTapFrom(ctx)
	if !ok {
		return 0, 0
	}
	return tap.Drain()
}

// report folds delta into the cumulative total via a lock-free CAS loop
// and bumps the attempt count.
func (t *AttemptCostTap) report(delta float64) {
	for {
		cur := t.totalBits.Load()
		next := math.Float64bits(math.Float64frombits(cur) + delta)
		if t.totalBits.CompareAndSwap(cur, next) {
			break
		}
	}
	t.attempts.Add(1)
}

// peek returns the current cumulative total + attempt count without
// draining. Safe to call concurrently with report.
func (t *AttemptCostTap) peek() (float64, int) {
	return math.Float64frombits(t.totalBits.Load()), int(t.attempts.Load())
}

// Drain returns the tap's accumulated attempt-cost total (USD) and the
// number of reported attempts, exactly once. The first call marks the tap
// drained; every subsequent call returns (0, 0) so no accounting fold can
// double-count a call's attempt spend.
//
// Failure direction (for wrapper authors): a report that races an
// already-completed Drain is LOST — its spend lands in the tap's atomics
// after the one-shot Drain has read them, so it is never folded into
// accounting. The pinned direction is therefore an UNDER-count, never a
// double-count: Drain is the boundary that closes the propagate-or-report
// window, so any consuming site MUST Report before the governed call's
// PostCall drains, not after.
func (t *AttemptCostTap) Drain() (totalUSD float64, attempts int) {
	if !t.drained.CompareAndSwap(false, true) {
		return 0, 0
	}
	return math.Float64frombits(t.totalBits.Load()), int(t.attempts.Load())
}
