package statestore

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// CatalogInputs implements agentcfg.CatalogInputReader. It reads only record
// identities — the agent active pointer (the lifecycle slot), the activation
// fence records, and the acting user's active pointer — never revision
// bodies, so its cost is four small reads regardless of config size: the
// deletion generation, then the agent pointer, the fence list and the user
// pointer it guards.
//
// The fence projection Active applies (applySignedOAuthMCPFence) is a pure
// function of the active revision and these fence records; it has no clock
// input, so no deadline needs to accompany the stamp.
func (r *registry) CatalogInputs(ctx context.Context, id identity.Quadruple, agentID string) (agentcfg.CatalogInputs, error) {
	if err := r.validate(id, agentID); err != nil {
		return agentcfg.CatalogInputs{}, err
	}
	if err := ctx.Err(); err != nil {
		return agentcfg.CatalogInputs{}, err
	}
	userKeys, err := keysFor(agentcfg.ConfigScopeUser, id, agentID)
	if err != nil {
		return agentcfg.CatalogInputs{}, err
	}
	agentQuad := syntheticQuad(id.TenantID, agentID)
	var out agentcfg.CatalogInputs
	// The deletion generation MUST be read before every slot it guards.
	// A deleting writer rewrites it before its delete; reading it first
	// means a touch that lands after this read is seen by the next stamp,
	// while a touch that landed before it orders the guarded write before
	// the slot reads below. Reading a slot first would let a cycle's touch
	// fall between the two reads and leave both stamps equal.
	if out.DeletionGeneration, err = r.slotEventID(ctx, agentQuad, agentcfg.CatalogDeletionGenerationKind); err != nil {
		return agentcfg.CatalogInputs{}, err
	}
	if out.AgentActive, err = r.slotEventID(ctx, agentQuad, kindActive); err != nil {
		return agentcfg.CatalogInputs{}, err
	}
	records, err := r.state.ListKindForIdentity(ctx, agentQuad, agentcfg.SignedOAuthMCPActivationFenceKind())
	if err != nil {
		return agentcfg.CatalogInputs{}, fmt.Errorf("%w: list signed capability activation fences: %w", agentcfg.ErrStateUnavailable, err)
	}
	out.Fences = make([]agentcfg.SlotGeneration, 0, len(records))
	for _, record := range records {
		out.Fences = append(out.Fences, agentcfg.SlotGeneration{Kind: record.Kind, EventID: record.ID})
	}
	sort.Slice(out.Fences, func(i, j int) bool { return out.Fences[i].Kind < out.Fences[j].Kind })
	if out.UserActive, err = r.slotEventID(ctx, userKeys.quad, userKeys.activeKind); err != nil {
		return agentcfg.CatalogInputs{}, err
	}
	return out, nil
}

// slotEventID returns the EventID of the record at (q, kind), or "" when the
// slot is absent. A store error is never reported as absence.
func (r *registry) slotEventID(ctx context.Context, q identity.Quadruple, kind string) (state.EventID, error) {
	rec, err := r.state.Load(ctx, q, kind)
	if errors.Is(err, state.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("%w: load %s: %w", agentcfg.ErrStateUnavailable, kind, err)
	}
	return rec.ID, nil
}

var _ agentcfg.CatalogInputReader = (*registry)(nil)
