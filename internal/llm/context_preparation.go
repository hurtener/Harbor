package llm

import (
	"context"

	"github.com/hurtener/Harbor/internal/identity"
)

// ContextPreparation binds one run's compactor to request construction. The
// runtime owns Compact; the planner owns CompleteRequest.RebuildMessages.
// Nothing here is provider state, a persistent registry, or a callable tool.
type ContextPreparation struct {
	InputTarget int
	Compact     func(context.Context, int, int) (bool, error)
}

type contextPreparationKey struct{}

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
			changed, err := preparation.Compact(ctx, estimated, target)
			if err != nil {
				return CompleteResponse{}, err
			}
			if changed {
				messages, err := req.RebuildMessages()
				if err != nil {
					return CompleteResponse{}, err
				}
				req.Messages = messages
			}
		}
	}
	// No maintenance loop: protect the latest exchange even if it exceeds the
	// soft target. The hard input/output limit is always enforced downstream.
	// Do not carry the renderer closure into retry or provider implementations.
	req.RebuildMessages = nil
	return c.inner.Complete(ctx, req)
}

func (c *contextPreparationClient) Close(ctx context.Context) error { return c.inner.Close(ctx) }
