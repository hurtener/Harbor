// bootpacks.go — the serve-band composition of the HA-66 boot operator
// skill baseline: the eager immutable boot-pack index loader, its
// static required_tools validation, the pre-read durable collision
// check, and the immutable index binding into the agent-config
// control-plane guards (BootOwnership) and the read-only composition
// preview.
//
// Production serve and the devstack kit share this ONE loader/composer
// path (openBootPackIndex). The loader never invokes admin pack verbs,
// lifecycle, SkillStore / ArtifactStore writes, or AgentConfig
// revisions: it is a pure eager filesystem read + parse + validate.
// The index is built BEFORE readiness/listeners (the serve Boot order
// places it ahead of the listener bind), and it is immutable for the
// process lifetime — config removal takes effect only in the next
// runtime instance and never erases an independent durable revision.
package serve

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
	"github.com/hurtener/Harbor/internal/skills/importer"
	"github.com/hurtener/Harbor/internal/tools"
)

// bootCatalogAdapter adapts the runtime's (wrapped) tool catalog to the
// loader's static compatibility reader: required_tools is METADATA ONLY
// and grants nothing — the check is presence in the static wrapped
// catalog (the policy/approval/OAuth-wrapped catalog after
// tools.entries[]), never a per-run projection (no invented user /
// session).
type bootCatalogAdapter struct {
	cat tools.ToolCatalog
}

func (a bootCatalogAdapter) Compatible(tool string) bool {
	if a.cat == nil {
		return false
	}
	_, ok := a.cat.Resolve(tool)
	return ok
}

var _ bootpacks.ToolCatalog = bootCatalogAdapter{}

// openBootPackIndex eagerly loads the immutable boot-pack index from
// the operator's `skills.boot_agent_packs` declarations. An empty
// declaration list returns (nil, nil) — no boot baseline is bound.
// Every failure (missing / invalid file, symlink / hardlink /
// special-file rejection, duplicate canonical name, an unsupported
// required_tools entry, an over-bound aggregate) fails the boot LOUD.
func OpenBootPackIndex(ctx context.Context, cfg *config.Config, cat tools.ToolCatalog) (*bootpacks.Index, error) {
	if len(cfg.Skills.BootAgentPacks) == 0 {
		return nil, nil
	}
	imp, err := importer.New(importer.Deps{})
	if err != nil {
		return nil, fmt.Errorf("boot_agent_packs importer: %w", err)
	}
	index, err := bootpacks.New(ctx, cfg.Skills.BootAgentPacks, bootpacks.Deps{
		Parser:  imp,
		Catalog: bootCatalogAdapter{cat: cat},
	})
	if err != nil {
		return nil, fmt.Errorf("boot_agent_packs: %w", err)
	}
	return index, nil
}

// validateBootAgentPacksForAgent checks every configured boot pack
// declaration against the AUTHORITATIVE resolved boot/default agent id:
// all configured agent IDs MUST equal the one resolved boot/default
// agent. Uses the config package's pure helper.
func ValidateBootAgentPacksForAgent(cfg *config.Config, resolvedAgentID string) error {
	if err := cfg.ValidateBootAgentPacksForAgent(resolvedAgentID); err != nil {
		return fmt.Errorf("skills.boot_agent_packs: %w", err)
	}
	return nil
}

// preReadBootPackCollisions eagerly pre-reads every declared
// (tenant, agent) key's DURABLE active revision WITHOUT materializing
// anything (no lifecycle, no writes, no revisions) and runs the strict
// composer's collision check against the frozen boot entries: a
// same-name / same-semantic-hash item dedupes (both), a same-name /
// differing-hash item FAILS the boot loud — the same typed conflict the
// run-start resolver and the read-only preview would refuse, surfaced
// at readiness instead of at first run. An absent active revision (or
// an absent boot key) passes.
//
// The active-revision read is user-scoped; the boot declaration carries
// no user, so the pre-read reads under the RESOLVED BOOT identity's
// user — the same (tenant, user) the run-loop resolves the boot
// agent's composition under at run start. A config removal between
// deployments is represented by the key's absence in the next index
// and never erases the durable revision.
func PreReadBootPackCollisions(ctx context.Context, index *bootpacks.Index, registry agentcfg.RetirementRegistry, bootUser string) error {
	if index == nil || registry == nil {
		return nil
	}
	for _, key := range index.Keys() {
		bootEntries, ok := index.Lookup(key.TenantID, key.AgentID)
		if !ok || len(bootEntries) == 0 {
			continue
		}
		q := identity.Quadruple{Identity: identity.Identity{TenantID: key.TenantID, UserID: bootUser}}
		rev, set, err := registry.Active(ctx, q, key.AgentID, agentcfg.ConfigScopeAgent)
		if err != nil {
			return fmt.Errorf("boot_agent_packs: pre-read active revision for tenant=%q agent=%q: %w", key.TenantID, key.AgentID, err)
		}
		if !set || len(rev.Payload.AgentPacks) == 0 {
			continue
		}
		revisionSkills, err := skills.PackItemsToSkills(rev.Payload.AgentPacks)
		if err != nil {
			return fmt.Errorf("boot_agent_packs: pre-read active revision (tenant=%q agent=%q): %w", key.TenantID, key.AgentID, err)
		}
		if _, err := sessionoverlay.ComposeOperatorTier(bootEntries, revisionSkills); err != nil {
			return fmt.Errorf("boot_agent_packs: pre-read composition conflict (tenant=%q agent=%q): %w", key.TenantID, key.AgentID, err)
		}
	}
	return nil
}
