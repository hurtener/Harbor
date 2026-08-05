package protocol

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// extrasystemblocks.go — the agent-config additive-prompt-blocks control
// method: `agent_config.set_extra_system_blocks`. It is a consumer of the
// registry primitive (CLAUDE.md §13) — a block edit composes a new config
// revision (so the blocks inherit the unified diff + rollback) and the
// registry emits the generic `agent.config.revised` event.
//
// Desired-state WHOLE-SECTION replace: a set REPLACES ONLY the blocks
// section of the envelope; every sibling section of the active revision is
// carried forward. There are deliberately no per-block upsert / delete
// verbs — a block is one element of an ORDERED composition whose order is
// semantic, so a per-item upsert has no well-defined insertion position.
// A second contributor composes by read-modify-write, sending the read
// revision's content hash as the expected-revision token so a concurrent
// sibling's write is refused rather than silently reverted. The BASE CASE
// has its own token: when the read answered `set: false` (no config yet)
// there is no hash to echo, so the caller sends the reserved
// [agentcfg.ExpectNoActiveRevision] value, which succeeds only while no
// revision exists. Omitting the token there would be an unconditional write,
// and two contributors composing onto a fresh agent would silently revert
// each other — which is the one case this protocol could not express until
// the sentinel existed.
//
// The edit applies NEXT-turn (the run-start projection reads the active
// revision; the immutable per-run snapshot is undisturbed for in-flight
// runs).
//
// # Why the bodies render verbatim
//
// This verb sits in the ADMIN method set — the same tier that writes
// PromptLayers.Base, which is already rendered verbatim and is strictly
// more powerful (it is the whole spine). Escaping a block while leaving
// Base unescaped would defend against a writer who can already replace the
// entire prompt. The write door's authority tier IS the boundary, which is
// why the section has no lower-tier write path and why keeping it that way
// is a tested invariant rather than an assumption.

// blockNameRE is the restricted identifier charset a block name must match.
// It is for LEGIBILITY (a name shows up in a transcript, a diff and an
// operator's read-modify-write), not for structural safety: a name is never
// emitted as an XML tag, so no name can perturb the prompt's taxonomy.
var blockNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// validateExtraSystemBlocks rejects a blocks section with a malformed or
// duplicated name, or a BLANK body, BEFORE any registry write — no
// revision, no active-pointer move, no event (CLAUDE.md §13).
//
// A duplicate name is refused rather than de-duplicated because uniqueness
// is what makes remove-by-name well defined, and remove-by-name is the
// composability property the whole section exists to provide. The message
// names BOTH offending positions so the caller can find them.
//
// THE BODY CHECK IS `TrimSpace`, NOT `== ""`, AND THE TWO ARE NOT
// INTERCHANGEABLE. Canonicalisation drops any block whose body trims to
// empty, so an exact-empty-only check at the door accepted `{"name":"x",
// "body":"  "}` with a 200, minted a revision, and left the block out of
// it — a caller told its write succeeded, reading back a config that does
// not contain what it wrote. That is silent degradation (§13) on an
// operator-facing door. The door's rule and the canonical form's rule must
// name the same set, so the door refuses everything canonicalisation would
// drop.
func validateExtraSystemBlocks(blocks []prototypes.AgentConfigNamedBlock) error {
	seen := make(map[string]int, len(blocks))
	for i, b := range blocks {
		if !blockNameRE.MatchString(b.Name) {
			return fmt.Errorf("%w: blocks[%d].name %q must match %s",
				ErrInvalidExtraSystemBlocks, i, b.Name, blockNameRE.String())
		}
		if strings.TrimSpace(b.Body) == "" {
			return fmt.Errorf("%w: blocks[%d] (name %q) has a blank body — a body that is empty or only whitespace is dropped by canonicalisation, so accepting it would return success for a block the stored revision does not contain",
				ErrInvalidExtraSystemBlocks, i, b.Name)
		}
		if first, dup := seen[b.Name]; dup {
			return fmt.Errorf("%w: duplicate block name %q at blocks[%d] and blocks[%d] — names must be unique so a contributor can remove exactly its own block",
				ErrInvalidExtraSystemBlocks, b.Name, first, i)
		}
		seen[b.Name] = i
	}
	return nil
}

// blocksToDomain projects the wire block list onto the domain shape,
// PRESERVING the declared order (the order is the render order). A nil /
// empty list returns nil so an empty desired state clears the section.
func blocksToDomain(in []prototypes.AgentConfigNamedBlock) []agentcfg.NamedBlock {
	if len(in) == 0 {
		return nil
	}
	out := make([]agentcfg.NamedBlock, 0, len(in))
	for _, b := range in {
		out = append(out, agentcfg.NamedBlock{Name: b.Name, Body: b.Body})
	}
	return out
}

// SetExtraSystemBlocks records a new config revision pinning the supplied
// ORDERED list of named additive prompt blocks as a desired-state replace
// of the blocks section. Every sibling section of the active revision is
// carried forward unchanged.
//
// The supplied order is preserved end to end — no map is the CARRIER, and
// nothing on the write → normalise → hash → project → render path sorts
// the list — so a re-ordering is a real new revision with a different
// content hash and a visible diff.
//
// Stated that precisely because the broader "no map appears on the path"
// is FALSE and would mislead a future author: the duplicate-name check
// above and normalizeNamedBlocks both build a local `seen` map. Neither
// determines order — they are membership sets over an already-ordered
// slice — and that distinction is the actual invariant. What must never
// happen is a map becoming the carrier, because map iteration order is
// not a composition order.
func (s *Service) SetExtraSystemBlocks(ctx context.Context, req prototypes.AgentConfigSetExtraSystemBlocksRequest) (prototypes.AgentConfigSetExtraSystemBlocksResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSetExtraSystemBlocksResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSetExtraSystemBlocksResponse{}, err
	}
	// Validate BEFORE taking the lock or reading the registry — an invalid
	// section must never reach the spine.
	if err := validateExtraSystemBlocks(req.ExtraSystemBlocks.Blocks); err != nil {
		return prototypes.AgentConfigSetExtraSystemBlocksResponse{}, err
	}
	q := identity.Quadruple{Identity: id}
	defer s.lockAgent(id.TenantID, req.AgentID)()

	// Read the active revision so the new revision PRESERVES every sibling
	// section (a block edit replaces only its own section).
	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigSetExtraSystemBlocksResponse{}, err
	}
	payload := agentcfg.ConfigPayload{}
	if blocks := blocksToDomain(req.ExtraSystemBlocks.Blocks); len(blocks) > 0 {
		payload.ExtraSystemBlocks = &agentcfg.ExtraSystemBlocks{Blocks: blocks}
	}
	if hasActive {
		payload.PromptLayers = active.Payload.PromptLayers
		payload.Skills = active.Payload.Skills
		payload.ToolExposure = active.Payload.ToolExposure
		payload.Connections = active.Payload.Connections
		payload.LLMParams = active.Payload.LLMParams
		payload.Hooks = active.Payload.Hooks
		payload.Naming = active.Payload.Naming
		payload.OAuthProviders = active.Payload.OAuthProviders
		payload.SignedOAuthMCPPair = active.Payload.SignedOAuthMCPPair
		payload.SignedOAuthMCPPairs = active.Payload.SignedOAuthMCPPairs
	}

	rev, err := s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload,
		agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash})
	if err != nil {
		return prototypes.AgentConfigSetExtraSystemBlocksResponse{}, err
	}
	return prototypes.AgentConfigSetExtraSystemBlocksResponse{
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}
