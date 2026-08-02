package serve

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// EnsureBootAgentLifecycle materialises the first empty agent-level revision
// for one boot-declared synthetic agent when, and only when, its lifecycle
// slot is absent. Session-owned state uses that active slot as its durable
// authority fence. An existing slot is deliberately not interpreted or
// replaced here: terminal and malformed slots remain visible to the runtime's
// fail-closed lifecycle reader and can never be reactivated by boot.
func EnsureBootAgentLifecycle(ctx context.Context, st state.StateStore, registry agentcfg.Registry, id identity.Identity, agentID string) error {
	if st == nil || registry == nil {
		return errors.New("state store and agent-config registry are required")
	}
	if err := identity.Validate(id); err != nil {
		return fmt.Errorf("boot identity: %w", err)
	}
	slot, kind, err := agentcfg.LifecycleSlot(id.TenantID, agentID)
	if err != nil {
		return fmt.Errorf("lifecycle slot: %w", err)
	}
	if _, err := st.Load(ctx, slot, kind); err == nil {
		return nil
	} else if !errors.Is(err, state.ErrNotFound) {
		return fmt.Errorf("load lifecycle slot: %w", err)
	}
	if _, err := registry.SetRevision(ctx, identity.Quadruple{Identity: id}, agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{ExpectedContentHash: agentcfg.ExpectNoActiveRevision}); err != nil {
		if !errors.Is(err, agentcfg.ErrRevisionConflict) {
			return fmt.Errorf("materialise empty agent revision: %w", err)
		}
		// A sibling boot or real first writer won the no-active CAS. Its
		// revision is the authority, not an invitation to overwrite it.
		_, active, readErr := registry.Active(ctx, identity.Quadruple{Identity: id}, agentID, agentcfg.ConfigScopeAgent)
		if readErr != nil {
			return fmt.Errorf("re-read concurrent lifecycle winner: %w", readErr)
		}
		if active {
			return nil
		}
		return fmt.Errorf("materialise empty agent revision: %w", err)
	}
	return nil
}
