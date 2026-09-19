package llm

import (
	"context"
	"fmt"
)

// CompactionBudget describes provisional input capacity, not execution authority.
// The composed client still validates the route, grant and final request on every
// actual attempt. InputLimit is exclusive; zero means no enclosing request supplied
// a profile (standalone/custom callers retain the ordinary final safety guard).
type CompactionBudget struct {
	Profile    ModelProfile
	InputLimit int
}

type compactionRequestKey struct{}

type compactionRequestConfig struct {
	parent     CompleteRequest
	profiles   map[string]ModelProfile
	reserve    float64
	routeBound bool
}

func withCompactionRequest(ctx context.Context, req CompleteRequest, cfg ConfigSnapshot) context.Context {
	// Retain only model controls. Do not copy prompt bodies, closures, credentials
	// or provider endpoints into the maintenance context.
	parent := CompleteRequest{Model: req.Model, ReasoningEffort: req.ReasoningEffort,
		ReasoningEffortExplicit: req.ReasoningEffortExplicit}
	if req.ExternalGrant != nil {
		grant := *req.ExternalGrant
		parent.ExternalGrant = &grant
	}
	if req.MaxTokens != nil {
		value := *req.MaxTokens
		parent.MaxTokens = &value
	}
	if req.modelProfile != nil {
		parent = withTrustedModelProfile(parent, req.modelProfileModel, *req.modelProfile)
	}
	_, routed := TrustedProviderRouteFrom(ctx)
	return context.WithValue(ctx, compactionRequestKey{}, compactionRequestConfig{
		parent: parent, profiles: cfg.ModelProfiles, reserve: cfg.ContextWindowReserve,
		routeBound: routed || req.ExternalGrant != nil,
	})
}

// PrepareCompactionRequest applies the enclosing run's resolved model controls
// and calculates capacity for this maintenance call's own output allowance. It
// never calls a provider or grants authority. An explicit different static model
// is allowed only without a bound external route/grant; those require the existing
// authorization path instead of silently selecting another model.
func PrepareCompactionRequest(ctx context.Context, req CompleteRequest) (CompleteRequest, CompactionBudget, error) {
	config, present := ctx.Value(compactionRequestKey{}).(compactionRequestConfig)
	if !present {
		return req, CompactionBudget{}, nil
	}
	if req.Model == "" {
		req.Model = config.parent.Model
	}
	if req.Model != config.parent.Model && config.routeBound {
		return CompleteRequest{}, CompactionBudget{}, fmt.Errorf("%w: compaction model differs from bound route", ErrProviderRouteInvalid)
	}
	if config.parent.ExternalGrant != nil {
		grant := *config.parent.ExternalGrant
		req.ExternalGrant = &grant
	}
	if req.Model == config.parent.Model {
		req.ReasoningEffort = config.parent.ReasoningEffort
		req.ReasoningEffortExplicit = config.parent.ReasoningEffortExplicit
		if config.parent.modelProfile != nil {
			req = withTrustedModelProfile(req, req.Model, *config.parent.modelProfile)
		}
		if config.parent.MaxTokens != nil && req.MaxTokens != nil && *req.MaxTokens > *config.parent.MaxTokens {
			output := *config.parent.MaxTokens
			req.MaxTokens = &output
		}
	}
	profile, found := EffectiveModelProfile(req, ConfigSnapshot{ModelProfiles: config.profiles})
	if !found {
		return CompleteRequest{}, CompactionBudget{}, ErrUnsupportedModel
	}
	if profile.DefaultMaxTokens != nil && req.MaxTokens != nil && *req.MaxTokens > *profile.DefaultMaxTokens {
		output := *profile.DefaultMaxTokens
		req.MaxTokens = &output
	}
	limit, _, err := requestInputLimit(req, profile, config.reserve)
	if err != nil {
		return CompleteRequest{}, CompactionBudget{}, err
	}
	if limit <= 0 {
		return CompleteRequest{}, CompactionBudget{}, ErrContextWindowExceeded
	}
	return req, CompactionBudget{Profile: profile, InputLimit: limit}, nil
}
