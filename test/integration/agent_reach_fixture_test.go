package integration_test

import (
	"context"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
)

// explicitAgentReachResolver makes the effective agent named at a legacy
// integration-test boundary. Callers must still supply matching signed reach;
// this helper does not install authority implicitly.
type explicitAgentReachResolver struct {
	effective string
}

func (r explicitAgentReachResolver) ResolveAgent(_ context.Context, _ identity.Identity, agentID string) (bool, error) {
	return agentID == r.effective, nil
}

func (r explicitAgentReachResolver) EffectiveAgentID(requested string) (string, error) {
	if requested == "" {
		return r.effective, nil
	}
	return requested, nil
}

// explicitAgentReachValidator is limited to pre-auth Phase 60 fixtures. It
// keeps their transport focus while explicitly seating bounded reach.
type explicitAgentReachValidator struct {
	verified auth.Verified
}

func (v explicitAgentReachValidator) Validate(_ context.Context, _ string) (auth.Verified, error) {
	return v.verified, nil
}
