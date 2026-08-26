package llm

import (
	"context"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// providerRouteClient is the outermost driver-neutral route selector. It runs
// before governance and every model-sensitive wrapper, while carrying only a
// credential-free selection in context. The leaf resolves credentials anew
// for each actual retry/downgrade/provider attempt.
type providerRouteClient struct {
	inner     LLMClient
	cfg       ProviderRouteConfig
	validator ProviderRouteSelectionValidator
	now       func() time.Time
}

func newProviderRouteClient(inner LLMClient, cfg ProviderRouteConfig, validator ProviderRouteSelectionValidator) LLMClient {
	return &providerRouteClient{inner: inner, cfg: cfg, validator: validator, now: time.Now}
}

func (c *providerRouteClient) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	trusted, explicit := TrustedProviderRouteFrom(ctx)
	if !explicit {
		return c.inner.Complete(ctx, req)
	}
	requestCtx, routeReq, err := prepareProviderRouteRequest(ctx, c.cfg, trusted)
	if err != nil {
		return CompleteResponse{}, err
	}
	if c.validator == nil {
		return CompleteResponse{}, ErrProviderRouteInvalid
	}
	selected, err := SelectProviderRoute(requestCtx, c.cfg, routeReq, c.now())
	if err != nil {
		return CompleteResponse{}, err
	}
	if err := c.validator.ValidateProviderRouteSelection(selected); err != nil {
		return CompleteResponse{}, ErrProviderRouteInvalid
	}
	req.Model = selected.Model
	return c.inner.Complete(WithSelectedProviderRoute(requestCtx, selected), req)
}

func (c *providerRouteClient) Close(ctx context.Context) error { return c.inner.Close(ctx) }

func prepareProviderRouteRequest(ctx context.Context, cfg ProviderRouteConfig, trusted TrustedProviderRouteContext) (context.Context, ProviderRouteRequest, error) {
	if cfg.Resolver == nil {
		return ctx, ProviderRouteRequest{}, ErrProviderRouteResolverUnavailable
	}
	if err := ValidateProviderRoute(trusted.Route); err != nil || trusted.Route.RouteID == "" ||
		trusted.RuntimeID == "" || trusted.RuntimeID != cfg.RuntimeID || trusted.EffectiveAgentID == "" || trusted.TaskID == "" ||
		trusted.Purpose != ProviderRoutePurposeRun {
		return ctx, ProviderRouteRequest{}, ErrProviderRouteInvalid
	}
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		id, present := identity.From(ctx)
		if !present {
			return ctx, ProviderRouteRequest{}, ErrIdentityMissing
		}
		q = identity.Quadruple{Identity: id}
	}
	if err := identity.Validate(q.Identity); err != nil || q.RunID == "" {
		return ctx, ProviderRouteRequest{}, ErrIdentityMissing
	}
	requestCtx, scope, err := EnsureAttemptScope(ctx)
	if err != nil {
		return ctx, ProviderRouteRequest{}, err
	}
	return requestCtx, ProviderRouteRequest{
		TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID, LogicalRunID: q.RunID,
		EffectiveAgentID: trusted.EffectiveAgentID, RuntimeID: trusted.RuntimeID, TaskID: trusted.TaskID,
		LogicalCallID: scope.LogicalCallID, RouteID: trusted.Route.RouteID,
		RouteGeneration: trusted.Route.RouteGeneration, ProviderConnectionID: trusted.Route.ProviderConnectionID,
		ProviderConnectionGeneration: trusted.Route.ProviderConnectionGeneration,
		CredentialAssetGeneration:    trusted.Route.CredentialAssetGeneration, ModelSelector: trusted.Route.ModelSelector,
		Purpose: ProviderRoutePurposeRun,
	}, nil
}
