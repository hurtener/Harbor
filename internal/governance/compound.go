package governance

import (
	"context"
	"errors"
	"log/slog"

	"github.com/hurtener/Harbor/internal/llm"
)

// NewCompound bundles N Subsystems into a single Subsystem that fans
// PreCall + PostCall across all members. PreCall short-circuits on the
// first member that returns a non-nil error; subsequent members are NOT
// invoked. PostCall runs every member regardless of any individual
// failure (observability semantics) and joins their errors via
// `errors.Join` for the wrapper to log.
//
// The fan-out order is the slice order — operators put cheap checks
// (MaxTokens) before expensive ones (cost-ceiling state lookup) to keep
// the rejection path fast. The Phase 36a/36b binary order is:
//
//  1. MaxTokensEnforcer — purely in-memory cap check; cheapest reject.
//  2. RateLimiter — bucket drain (lock + small state lookup).
//  3. CostAccumulator — accumulator lookup (state I/O).
//
// Concurrent reuse (D-025): NewCompound's returned Subsystem is a thin
// adapter; concurrent reuse depends on each member honouring D-025.
func NewCompound(subs ...Subsystem) Subsystem {
	out := make([]Subsystem, 0, len(subs))
	for _, s := range subs {
		if s == nil {
			continue
		}
		out = append(out, s)
	}
	return &compoundSubsystem{subs: out}
}

type compoundSubsystem struct {
	subs []Subsystem
}

// PreCall fans out PreCall across members; the first non-nil return
// short-circuits.
func (c *compoundSubsystem) PreCall(ctx context.Context, req llm.CompleteRequest) error {
	for _, s := range c.subs {
		if err := s.PreCall(ctx, req); err != nil {
			return err
		}
	}
	return nil
}

// PostCall fans out PostCall across members. Every member runs (no
// short-circuit) — accumulators need their update regardless of upstream
// failures. Any non-nil member error is joined via `errors.Join` and
// returned; the wrapper logs but does not propagate.
func (c *compoundSubsystem) PostCall(ctx context.Context, req llm.CompleteRequest, resp llm.CompleteResponse, callErr error) error {
	var errs []error
	for _, s := range c.subs {
		if err := s.PostCall(ctx, req, resp, callErr); err != nil {
			errs = append(errs, err)
			slog.Warn("governance: compound PostCall member error",
				slog.String("error", err.Error()),
				slog.String("model", req.Model),
			)
		}
	}
	return errors.Join(errs...)
}
