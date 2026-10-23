package mcp

import (
	"context"
	"errors"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/appoperation"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// resolveAgentBearerCtx promotes only the signed broker's precise governed
// refusal, and only at a trusted planner invocation boundary. Generic OAuth
// failures, discovery, and browser operations retain their existing behavior.
func (p *Provider) resolveAgentBearerCtx(ctx context.Context, key string, bind func(context.Context, string) (context.Context, error)) (context.Context, error) {
	resolved, err := p.resolveBearerCtx(ctx, key)
	if err == nil {
		return resolved, nil
	}
	var governed *auth.GovernedAppMintRequiredError
	agent, trusted := appoperation.AgentInvocationFrom(ctx)
	effective, admitted := tools.EffectiveAgentConfigFrom(ctx)
	if !errors.As(err, &governed) || hasAppOperation(ctx) || !trusted || !admitted || agent != effective {
		return ctx, err
	}
	bound, bindErr := bind(ctx, agent)
	if bindErr != nil {
		return ctx, agentOperationError{bindErr}
	}
	resolved, err = p.resolveBearerCtx(bound, key)
	if err != nil {
		// Preserve the bound context so the enclosing shell stops every retry.
		return bound, err
	}
	return resolved, nil
}

func (p *Provider) resolveAgentResourceBearerCtx(ctx context.Context, uri string) (context.Context, error) {
	return p.resolveAgentBearerCtx(ctx, uri, func(ctx context.Context, agent string) (context.Context, error) {
		return appoperation.WithAgentRead(ctx, agent, string(p.source), uri)
	})
}

type agentOperationError struct{ error }

func (e agentOperationError) Unwrap() error   { return e.error }
func (e agentOperationError) Permanent() bool { return true }

func stopAgentOperationRetry(ctx context.Context, err error) error {
	if binding, ok := appoperation.From(ctx); err != nil && ok && (binding.Kind == "agent_tool" || binding.Kind == "agent_resource") {
		return agentOperationError{err}
	}
	return err
}
