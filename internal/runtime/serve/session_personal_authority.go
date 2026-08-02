package serve

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

// SessionPersonalSkillAuthority is the single boot-assembled dependency graph
// shared by the Protocol session-personal surface and the per-run skill
// snapshot. Its components are immutable after construction and safe for
// concurrent reuse.
type SessionPersonalSkillAuthority struct {
	Personal   *sessionoverlay.DurableStore
	Cutover    *sessionoverlay.CutoverController
	Controller *sessionoverlay.SessionPersonalController
}

// NewSessionPersonalSkillAuthority constructs the durable personal store,
// validates and initializes the static cutover declarations, resumes every
// declared drained tenant to state_only, and builds the Protocol controller.
// It never discovers tenants: only the supplied finite declaration list is
// visited. A migration or validation error fails boot loudly.
func NewSessionPersonalSkillAuthority(
	ctx context.Context,
	stateStore state.StateStore,
	skillStore skills.SkillStore,
	declarations []config.SessionPersonalCutoverTenant,
) (*SessionPersonalSkillAuthority, error) {
	if stateStore == nil {
		return nil, fmt.Errorf("session personal authority: state store is required")
	}
	if skillStore == nil {
		return nil, fmt.Errorf("session personal authority: skill store is required")
	}
	personal, err := sessionoverlay.NewDurableStore(stateStore, nil)
	if err != nil {
		return nil, fmt.Errorf("session personal authority: durable store: %w", err)
	}
	declared := append([]config.SessionPersonalCutoverTenant(nil), declarations...)
	cutover, err := sessionoverlay.NewCutoverController(stateStore, declared)
	if err != nil {
		return nil, fmt.Errorf("session personal authority: cutover controller: %w", err)
	}
	if err := cutover.Ensure(ctx); err != nil {
		return nil, fmt.Errorf("session personal authority: initialize cutover: %w", err)
	}
	migrator, err := sessionoverlay.NewExactLegacyMigrator(skillStore, personal)
	if err != nil {
		return nil, fmt.Errorf("session personal authority: legacy migrator: %w", err)
	}
	for _, declaration := range declared {
		if !declaration.LegacyWritersDrained {
			continue
		}
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			mode, advanceErr := cutover.Advance(ctx, declaration.TenantID, state.MaxStateScanLimit, migrator)
			if advanceErr != nil {
				return nil, fmt.Errorf("session personal authority: advance tenant %q: %w", declaration.TenantID, advanceErr)
			}
			if mode == sessionoverlay.CutoverStateOnly {
				break
			}
		}
	}
	controller, err := sessionoverlay.NewSessionPersonalController(personal, cutover, skillStore)
	if err != nil {
		return nil, fmt.Errorf("session personal authority: Protocol controller: %w", err)
	}
	return &SessionPersonalSkillAuthority{Personal: personal, Cutover: cutover, Controller: controller}, nil
}
