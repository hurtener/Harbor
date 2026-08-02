package agentcfg

import (
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
)

// ReservedAgentConfigUser is the synthetic user slot used by agent-level
// configuration and lifecycle records. Verified end-user identities must not
// occupy this reserved control-plane namespace.
const ReservedAgentConfigUser = "__agentcfg__"

// ActiveSlotKind is the durable agent lifecycle slot. Its payload is owned by
// the registry implementation; consumers use the slot only as a generation
// fence and must not interpret its bytes.
const ActiveSlotKind = "agentcfg.active"

// AgentScope returns the identity that contains an agent's control-plane
// records. The tenant remains the isolation boundary; agentID is a key in the
// synthetic session component and is never an isolation principal.
func AgentScope(tenantID, agentID string) (identity.Quadruple, error) {
	if tenantID == "" || agentID == "" {
		return identity.Quadruple{}, fmt.Errorf("%w: tenant and agent are required", ErrIdentityRequired)
	}
	return identity.Quadruple{Identity: identity.Identity{
		TenantID:  tenantID,
		UserID:    ReservedAgentConfigUser,
		SessionID: agentID,
	}}, nil
}

// LifecycleSlot returns the exact durable slot used as the lifecycle fence
// for one tenant-local agent.
func LifecycleSlot(tenantID, agentID string) (identity.Quadruple, string, error) {
	q, err := AgentScope(tenantID, agentID)
	if err != nil {
		return identity.Quadruple{}, "", err
	}
	return q, ActiveSlotKind, nil
}
