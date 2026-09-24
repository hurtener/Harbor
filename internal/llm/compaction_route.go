package llm

import (
	"context"
	"fmt"
	"time"
)

// PrepareRoutedCompactionRequest selects a separately configured maintenance
// route under the enclosing admitted run's identity. Selection contains no
// credential. The normal composed client must still authorize the actual call
// and the leaf must resolve fresh credentials on every attempt.
// A signed grant cannot be repurposed for a different route.
func PrepareRoutedCompactionRequest(ctx context.Context, req CompleteRequest, route ProviderRoute, cfg ProviderRouteConfig, reserve float64) (context.Context, CompleteRequest, CompactionBudget, error) {
	parent, hasParent := ctx.Value(compactionRequestKey{}).(compactionRequestConfig)
	if req.ExternalGrant != nil || (hasParent && parent.parent.ExternalGrant != nil) {
		return ctx, CompleteRequest{}, CompactionBudget{}, ErrProviderRouteInvalid
	}
	trusted, ok := TrustedProviderRouteFrom(ctx)
	if !ok || route.RouteID == "" {
		return ctx, CompleteRequest{}, CompactionBudget{}, ErrProviderRouteInvalid
	}
	// Replace only the selector. Runtime, effective agent and task remain the
	// admitted parent context's coordinates; YAML cannot supply those fields.
	trusted.Route = route
	callCtx, routeReq, err := prepareProviderRouteRequest(WithTrustedProviderRoute(ctx, trusted), cfg, trusted)
	if err != nil {
		return ctx, CompleteRequest{}, CompactionBudget{}, err
	}
	selected, err := SelectProviderRoute(callCtx, cfg, routeReq, time.Now())
	if err != nil {
		return ctx, CompleteRequest{}, CompactionBudget{}, err
	}
	if !selected.ExpiresAt.After(time.Now()) || selected.ModelProfile == nil {
		return ctx, CompleteRequest{}, CompactionBudget{}, fmt.Errorf("%w: compaction route requires a current model profile", ErrProviderRouteInvalid)
	}
	profile := selected.ModelProfile.modelProfile()
	req.Model = selected.Model
	// An independent model must not inherit the driving model's reasoning
	// effort or output reservation. Omit reasoning controls explicitly.
	req.ReasoningEffort = ""
	req.ReasoningEffortExplicit = true
	if req.MaxTokens != nil && *req.MaxTokens > selected.ModelProfile.MaxOutputTokens {
		output := selected.ModelProfile.MaxOutputTokens
		req.MaxTokens = &output
	}
	req = withTrustedModelProfile(req, selected.Model, profile)
	limit, _, err := requestInputLimit(req, profile, reserve)
	if err != nil {
		return ctx, CompleteRequest{}, CompactionBudget{}, err
	}
	if limit <= 0 {
		return ctx, CompleteRequest{}, CompactionBudget{}, ErrContextWindowExceeded
	}
	return WithSelectedProviderRoute(callCtx, selected), req, CompactionBudget{Profile: profile, InputLimit: limit}, nil
}
