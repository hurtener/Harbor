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
	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
	"github.com/hurtener/Harbor/internal/skills/importer"
	"github.com/hurtener/Harbor/internal/tools"
)

// bootCatalogAdapter adapts the runtime's (wrapped) tool catalog to the
// loader's static compatibility reader: required_tools is METADATA ONLY
// and grants nothing — the check is (a) presence in the static wrapped
// catalog (the policy/approval/OAuth-wrapped catalog after
// tools.entries[]), AND (b) every auth scope the resolved tool
// descriptor declares being within the configured
// `tools.granted_scopes` ceiling. A tool whose declared scopes exceed
// the ceiling is NOT satisfiable — the boot baseline never grants a
// scope the operator's ceiling denies. The ceiling is immutable for the
// process lifetime and threaded through the ONE shared
// production/devstack loader (OpenBootPackIndex).
type bootCatalogAdapter struct {
	cat tools.ToolCatalog
	// grantedScopes is the immutable configured `tools.granted_scopes`
	// ceiling the compatibility check enforces. Nil / empty means no
	// scope is granted: any tool declaring an auth scope is unsatisfiable.
	grantedScopes []string
}

func (a bootCatalogAdapter) Compatible(tool string) bool {
	if a.cat == nil {
		return false
	}
	desc, ok := a.cat.Resolve(tool)
	if !ok {
		return false
	}
	// The granted-scopes ceiling: every declared tool auth scope must be
	// in the configured ceiling. Required metadata grants nothing.
	for _, required := range desc.Tool.AuthScopes {
		inCeiling := false
		for _, granted := range a.grantedScopes {
			if granted == required {
				inCeiling = true
				break
			}
		}
		if !inCeiling {
			return false
		}
	}
	return true
}

var _ bootpacks.ToolCatalog = bootCatalogAdapter{}

// openBootPackIndex eagerly loads the immutable boot-pack index from
// the operator's `skills.boot_agent_packs` declarations. An empty
// declaration list returns (nil, nil) — no boot baseline is bound.
// store is the assembled artifact store the pure single-document
// importer is constructed over (the importer's constructor requires it
// even though the boot loader's markdown path never touches the store —
// the loader is a pure eager filesystem read + parse + validate, never
// a store writer). Every failure (missing / invalid file, symlink /
// hardlink / special-file rejection, duplicate canonical name, an
// unsupported required_tools entry — including one whose tool
// descriptor declares an auth scope outside the configured
// `tools.granted_scopes` ceiling, an over-bound aggregate) fails the
// boot LOUD.
func OpenBootPackIndex(ctx context.Context, cfg *config.Config, cat tools.ToolCatalog, store artifacts.ArtifactStore) (*bootpacks.Index, error) {
	if len(cfg.Skills.BootAgentPacks) == 0 {
		return nil, nil
	}
	imp, err := importer.New(importer.Deps{Store: store})
	if err != nil {
		return nil, fmt.Errorf("boot_agent_packs importer: %w", err)
	}
	index, err := bootpacks.New(ctx, cfg.Skills.BootAgentPacks, bootpacks.Deps{
		Parser: imp,
		Catalog: bootCatalogAdapter{
			cat:           cat,
			grantedScopes: append([]string(nil), cfg.Tools.GrantedScopes...),
		},
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
// The active-revision read uses the CANONICAL reserved tenant-agent
// control-plane identity from agentcfg.AgentScope(tenant, agent) — the
// SAME synthetic (tenant, __agentcfg__, agent) slot the agent-level
// desired state persists under. There is no bootUser / default / MCP /
// dev-user parameter: production serve and the devstack kit call the
// same helper, and no caller-supplied or defaulted real-user value can
// choose (or miss) the active agent-scope revision. A config removal
// between deployments is represented by the key's absence in the next
// index and never erases the durable revision.
func PreReadBootPackCollisions(ctx context.Context, index *bootpacks.Index, registry agentcfg.RetirementRegistry) error {
	if index == nil || registry == nil {
		return nil
	}
	for _, key := range index.Keys() {
		bootEntries, ok := index.Lookup(key.TenantID, key.AgentID)
		if !ok || len(bootEntries) == 0 {
			continue
		}
		// The canonical reserved tenant-agent control-plane identity —
		// never a synthetic real-user read. AgentScope returns the full
		// (tenant, __agentcfg__, agent) triple the agent-level desired
		// state persists under.
		q, err := agentcfg.AgentScope(key.TenantID, key.AgentID)
		if err != nil {
			return fmt.Errorf("boot_agent_packs: pre-read agent scope for tenant=%q agent=%q: %w", key.TenantID, key.AgentID, err)
		}
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

// emptyBootPackReader is the narrow immutable BootPackReader seam for a
// runtime with NO boot declarations: every (tenant, agent) lookup
// resolves to an EMPTY boot contribution (ok=true, zero entries). The
// read-only composition preview stays available after boot config
// removal — an independently persisted active revision composes as
// provenance "revision". It creates no tombstone, erases no revision,
// and the strict composer still enforces the boot+revision collision
// defense (an empty baseline has no entries to collide with; a real
// index with a removed key keeps its non-oracular unavailable outcome).
type emptyBootPackReader struct{}

func (emptyBootPackReader) Lookup(tenantID, agentID string) ([]bootpacks.Entry, bool) {
	return nil, true
}

var _ agentcfgprotocol.BootPackReader = emptyBootPackReader{}

// PreviewBootReader resolves the read-only composition preview's frozen
// boot contribution: the eager immutable index when a baseline is bound,
// else the empty immutable reader. Production serve.Boot and the
// devstack kit both call this ONE helper so the no-baseline preview path
// is the same everywhere — the preview never 501s merely because no boot
// packs are declared, and an independently persisted active revision
// appears as provenance "revision".
func PreviewBootReader(index *bootpacks.Index) agentcfgprotocol.BootPackReader {
	if index != nil {
		return index
	}
	return emptyBootPackReader{}
}
