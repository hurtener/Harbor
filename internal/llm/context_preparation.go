package llm

import (
	"context"
	"errors"

	"github.com/hurtener/Harbor/internal/identity"
)

// ContextPreparation binds one run's compactor to request construction. The
// runtime owns Compact; the planner owns CompleteRequest.RebuildMessages.
// Nothing here is provider state, a persistent registry, or a callable tool.
type ContextPreparation struct {
	InputTarget int
	Compact     func(context.Context, int, int) (bool, error)
	// History returns a detached runtime snapshot after any checkpoint publication.
	// Optional: maintenance calls and callers without runtime history omit it.
	History func() *ContextHistory
}

type contextPreparationKey struct{}
type contextCandidateCheckKey struct{}

// ErrCompactionNoProgress rejects a narrative that does not reduce the actual
// request. A fitting original request may continue with its prior checkpoint.
var ErrCompactionNoProgress = errors.New("llm: compaction did not reduce input")

// CheckContextCandidate validates a temporarily selected checkpoint before the
// runtime publishes it. The runtime restores the old checkpoint on failure.
// It only rebuilds messages and estimates input; it performs no provider work.
// Standalone trajectory compaction has no assembled-request check to invoke.
func CheckContextCandidate(ctx context.Context) error {
	if check, ok := ctx.Value(contextCandidateCheckKey{}).(func() error); ok {
		return check()
	}
	return nil
}

// WithContextPreparation seats a run-local context preparation callback.
// A nil callback or a non-positive target leaves ordinary requests unchanged.
func WithContextPreparation(ctx context.Context, preparation ContextPreparation) context.Context {
	return context.WithValue(ctx, contextPreparationKey{}, preparation)
}

type contextPreparationClient struct {
	inner LLMClient
	cfg   ConfigSnapshot
}

// Preparation runs after credential-free route selection and before governance,
// grants, retries, and provider work. The mandatory inner safety pass still
// remeasures the final transformed request on every actual attempt.
func (c *contextPreparationClient) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	preparation, ok := ctx.Value(contextPreparationKey{}).(ContextPreparation)
	if !ok || preparation.InputTarget <= 0 || preparation.Compact == nil || req.RebuildMessages == nil {
		return c.inner.Complete(ctx, req)
	}
	if err := ctx.Err(); err != nil {
		return CompleteResponse{}, err
	}
	if !HasIdentity(ctx) || identity.Validate(identityQuad(ctx).Identity) != nil {
		return CompleteResponse{}, ErrIdentityMissing
	}

	// These are provisional capacity inputs, not authority. The normal grant
	// wrapper still verifies the complete signed grant for every actual call,
	// including maintenance calls, and may narrow an omitted output allowance.
	bound := req
	if bound.Model == "" {
		bound.Model = c.cfg.Model
		if bound.ExternalGrant != nil && EffectiveExternalGrantRouteMode(bound.ExternalGrant.RouteMode) == ExternalGrantRouteCoordinatorBound {
			bound.Model = bound.ExternalGrant.ProviderModelID
		}
	}
	if bound.MaxTokens == nil && bound.ExternalGrant != nil && bound.ExternalGrant.MaxOutputTokens > 0 {
		output := bound.ExternalGrant.MaxOutputTokens
		bound.MaxTokens = &output
	}
	if err := validateRequest(bound); err != nil {
		return CompleteResponse{}, err
	}
	profile, found := EffectiveModelProfile(bound, c.cfg)
	if !found {
		// Preserve the ordinary unknown-model failure; never invent a window.
		return c.inner.Complete(ctx, req)
	}
	capacity, _, err := requestInputLimit(bound, profile, c.cfg.ContextWindowReserve)
	if err != nil {
		return CompleteResponse{}, err
	}
	// Input capacity is exclusive. Even an empty history cannot repair an
	// exhausted output reservation; the normal safety pass reports that error.
	if capacity > 1 {
		target := min(preparation.InputTarget, capacity-1)
		estimated := EstimateRequestTokens(bound, profile)
		if estimated > target {
			var checkedMessages []ChatMessage
			checked := false
			check := func() error {
				checked = true
				messages, err := req.RebuildMessages()
				if err != nil {
					return err
				}
				candidate := bound
				candidate.Messages = messages
				if err := validateRequest(candidate); err != nil {
					return err
				}
				after := EstimateRequestTokens(candidate, profile)
				if after >= capacity {
					return ErrContextWindowExceeded
				}
				if after >= estimated {
					return ErrCompactionNoProgress
				}
				checkedMessages = messages
				return nil
			}
			checkCtx := context.WithValue(withCompactionRequest(ctx, bound, c.cfg), contextCandidateCheckKey{}, check)
			changed, err := preparation.Compact(checkCtx, estimated, target)
			if err != nil && (!errors.Is(err, ErrCompactionNoProgress) || estimated >= capacity || changed) {
				return CompleteResponse{}, err
			}
			if changed {
				if !checked {
					// Compatibility for custom callbacks that only rebuild after
					// their own publication. The stock runtime checks before commit.
					messages, err := req.RebuildMessages()
					if err != nil {
						return CompleteResponse{}, err
					}
					checkedMessages = messages
				}
				req.Messages = checkedMessages
			}
		}
	}
	// The decision and its maintenance used the same resolved model for
	// capacity. Keep that model explicit for downstream governance as well;
	// an empty caller default must not create a second per-model rate bucket.
	// Grant verification still owns authorization of these provisional controls.
	req.Model = bound.Model
	// No maintenance loop: protect the latest exchange even if it exceeds the
	// soft target. The hard input/output limit is always enforced downstream.
	// Do not carry the renderer closure into retry or provider implementations.
	req.RebuildMessages = nil
	return c.inner.Complete(ctx, req)
}

func (c *contextPreparationClient) Close(ctx context.Context) error { return c.inner.Close(ctx) }
