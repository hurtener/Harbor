package integration_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
)

// activateFixtureAgent declares the exact fixture agent before a direct
// protocol/projection harness asks the session-owned tier to use it. This is
// the same real registry first-write CAS used by provisioning; it is not an
// overlay bypass. Callers must name a fixture agent already declared by their
// harness's reach/configuration surface.
func activateFixtureAgent(t *testing.T, registry agentcfg.Registry, id identity.Identity, agentID string) {
	t.Helper()
	if _, err := registry.SetRevision(context.Background(), identity.Quadruple{Identity: id}, agentID,
		agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{
			ExpectedContentHash: agentcfg.ExpectNoActiveRevision,
		}); err != nil {
		t.Fatalf("activate fixture agent %q for tenant %q: %v", agentID, id.TenantID, err)
	}
}
