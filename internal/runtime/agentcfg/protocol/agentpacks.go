package protocol

// agentpacks.go — the operator-managed per-agent skill pack verbs
// (`agent_config.agent_packs.*`, HA-55). The pack is a bounded set of FULL
// skill bodies pinned inside the agent's content-addressed config revision
// (the `AgentPacks` payload section), so a pack body + its membership are ONE
// atomic write and can never dangle apart.
//
// The family:
//
//   - list — read the active pack (admin).
//   - upsert — DETERMINISTIC add/replace of one item (admin). Body +
//     membership atomically in one revision.
//   - remove — DETERMINISTIC drop of one item by name (admin).
//   - propose — GOVERNED drafting: a bounded intent is turned into a
//     canonical draft body by the configured model under the versioned
//     revision policy; returns the draft + content hash + warnings + a
//     deterministic provenance stamp. Only dry_run is non-persistent;
//     ordinary propose persists its durable receipt for commit.
//   - commit — GOVERNED two-phase landing: CAS-binds the EXACT reviewed hash
//     (a changed body / scope / origin / provenance is refused), then
//     atomically persists body + membership in one revision.
//
// Isolation: the pack is tenant+agent keyed through the registry's synthetic
// identity (ConfigScopeAgent), so agent A's pack is invisible to agent B and
// to another tenant. The run-start composition resolves the pack for every
// authorised user of the effective agent plus only that caller's
// personal/session skills (see agentcfg/sessionoverlay.SessionSkillResolver);
// the pack's Required* annotations are filter metadata that never widen the
// run's visible tool set (the capability filter + redactor sit in front).
//
// Every mutation records a config revision (the agent-config audit/revision
// evidence: immutable chain + author identity + `agent.config.revised` event
// from the registry).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

// Agent-pack sentinel errors. The wire handler maps each onto a canonical
// Protocol code; in-process callers compare with errors.Is.
var (
	// ErrAgentPacksReadOnly rejects the generic whole-payload authoring door.
	// Pack bodies are accepted only by the dedicated verbs, which apply the
	// server provenance stamp and the pack invariants before persistence.
	ErrAgentPacksReadOnly = errors.New("agentcfg/protocol: agent packs require the dedicated pack verbs")
	// ErrAgentPacksInvalid — a pack verb received a malformed item, a
	// non-agent scope, a smuggled origin, a caller-supplied origin_ref, an
	// over-bounded pack, or an empty/over-bounded intent. Fails closed
	// before any revision write.
	ErrAgentPacksInvalid = errors.New("agentcfg/protocol: invalid agent pack")
	// ErrAgentPackProposeUnavailable — propose was called but no
	// AgentPackProposer is wired on this runtime (→ 501 at the wire edge).
	// The deterministic pack verbs stay live regardless.
	ErrAgentPackProposeUnavailable = errors.New("agentcfg/protocol: agent pack proposer not wired on this runtime")
	// ErrAgentPackProposalUnavailable means the durable proposal ledger was not
	// wired. A proposal must never be represented by forgeable client text.
	ErrAgentPackProposalUnavailable = errors.New("agentcfg/protocol: durable agent pack proposal ledger not wired")
	// ErrAgentPackProposalInvalid means the proposal token is absent, expired,
	// consumed, or bound to different server-side inputs.
	ErrAgentPackProposalInvalid = errors.New("agentcfg/protocol: invalid or consumed agent pack proposal")
	// ErrAgentPackHashMismatch — commit's ReviewedHash does not equal the
	// canonical content hash of the submitted body. The commit is refused
	// and NOTHING is persisted (the two-phase CAS half).
	ErrAgentPackHashMismatch = errors.New("agentcfg/protocol: pack commit hash does not match the reviewed hash")
	// ErrAgentPackProvenanceMismatch — commit's Provenance does not match
	// the deterministic proposal stamp for (agent, reviewed hash). Refused
	// before any write — a commit must echo the exact proposal it reviewed.
	ErrAgentPackProvenanceMismatch = errors.New("agentcfg/protocol: pack commit provenance does not match the proposal stamp")
	// ErrAgentPackNotFound — remove named a pack item the active revision
	// does not contain. Fails loud so a stale remove can never silently
	// no-op (dangling-membership prevention).
	ErrAgentPackNotFound = errors.New("agentcfg/protocol: pack item not found")
)

// maxAgentPackIntentRunes bounds the natural-language brief
// `agent_packs.propose` accepts. Intent is operator-authored prose that the
// proposer turns into a bounded skill body; a generous but bounded ceiling
// keeps a runaway prompt out of the LLM call.
const maxAgentPackIntentRunes = 4000

// AgentPackDraft is the proposer's output: a bounded, validated pack item
// body plus optional review warnings.
type AgentPackDraft struct {
	// Item is the drafted body. The service re-validates + re-hashes it
	// (the proposer's validation is never trusted by itself).
	Item skills.AgentPackItem
	// Warnings are non-fatal review notes (e.g. a required tool that is not
	// currently run-visible — filter metadata only, never a grant).
	Warnings []string
}

type agentPackProposalRecord struct {
	AgentID             string               `json:"agent_id"`
	ExpectedContentHash string               `json:"expected_content_hash"`
	ReviewedHash        string               `json:"reviewed_hash"`
	Provenance          string               `json:"provenance"`
	Item                skills.AgentPackItem `json:"item"`
	Phase               string               `json:"phase,omitempty"`
	TargetRevisionID    string               `json:"target_revision_id,omitempty"`
	TargetContentHash   string               `json:"target_content_hash,omitempty"`
}

const agentPackProposalKindPrefix = "agentcfg.agent_pack.proposal."

func proposalKind(id string) string { return agentPackProposalKindPrefix + id }

func marshalProposal(r agentPackProposalRecord) ([]byte, error) { return json.Marshal(r) }

func unmarshalProposal(b []byte) (agentPackProposalRecord, error) {
	var r agentPackProposalRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("decode pack proposal: %w", err)
	}
	return r, nil
}

// AgentPackProposer is the governed two-phase authoring seam. The concrete
// (injected at the cmd/harbor + devstack boundary) owns the LLM call and
// uses the model the service resolves from the agent's active revision (the
// versioned policy). The Service depends only on this interface; a nil
// proposer leaves propose failing loud with ErrAgentPackProposeUnavailable.
type AgentPackProposer interface {
	// Draft turns a bounded operator intent into a bounded, validated pack
	// item body for the selected agent. `model` is the configured model the
	// service resolved (empty = the proposer's own default). Implementations
	// MUST honour ctx cancellation and MUST NOT persist anything.
	Draft(ctx context.Context, q identity.Quadruple, agentID, model, intent string) (AgentPackDraft, error)
}

// packProposedProvenance is the deterministic proposal stamp a commit must
// echo: `pack.proposed.<agent>.<hash[:16]>`. Derivable from exactly what the
// commit carries (agent + reviewed hash), so the service can re-derive and
// reject a changed provenance without the original intent.
func packProposedProvenance(agentID, hash string) string {
	return "pack.proposed." + agentID + "." + hashPrefix(hash)
}

// normalizePackProposalProvenance upgrades the pre-provenance receipt shape
// while keeping every non-empty provenance value strictly bound to the
// deterministic proposal stamp.
func normalizePackProposalProvenance(r *agentPackProposalRecord) error {
	want := packProposedProvenance(r.AgentID, r.ReviewedHash)
	if r.Provenance != "" && r.Provenance != want {
		return ErrAgentPackProposalInvalid
	}
	r.Provenance = want
	return nil
}

func packCommittedOriginRef(agentID, hash string) string {
	return "pack.committed." + agentID + "." + hashPrefix(hash)
}

func packUpsertedOriginRef(agentID, hash string) string {
	return "pack.upserted." + agentID + "." + hashPrefix(hash)
}

func hashPrefix(hash string) string {
	if len(hash) > 16 {
		return hash[:16]
	}
	return hash
}

// validateAgentPackScope enforces the non-widening rung: a pack verb accepts
// only "" (default) or "agent" as its declared scope. A pack item can never
// declare a project/tenant/global/user visibility rung.
func validateAgentPackScope(scope string) error {
	switch scope {
	case "", "agent":
		return nil
	default:
		return fmt.Errorf("%w: scope=%q (the pack rung is agent-only; non-widening)", ErrAgentPacksInvalid, scope)
	}
}

// validateAgentPackWireItem enforces the two read-only body fields at the
// wire edge: `origin` must be "" or "pack" (generated provenance cannot be
// smuggled through the pack door) and `scope` must be "" or "agent"
// (visibility cannot be widened through the body). A caller-supplied
// `origin_ref` is rejected — the service stamps it deterministically.
func validateAgentPackWireItem(item prototypes.AgentConfigAgentPackItem) error {
	switch item.Origin {
	case "", "pack":
	default:
		return fmt.Errorf("%w: origin=%q (pack provenance only; generated origin cannot be smuggled through the body)", ErrAgentPacksInvalid, item.Origin)
	}
	switch item.Scope {
	case "", "agent":
	default:
		return fmt.Errorf("%w: skill scope=%q (the pack rung is agent-only; non-widening)", ErrAgentPacksInvalid, item.Scope)
	}
	if strings.TrimSpace(item.OriginRef) != "" {
		return fmt.Errorf("%w: origin_ref is server-stamped (a caller-supplied provenance ref is refused)", ErrAgentPacksInvalid)
	}
	return nil
}

// agentPackItemToDomain projects a wire pack item onto the domain shape
// (defensive copies; no shared backing slices).
func agentPackItemToDomain(in prototypes.AgentConfigAgentPackItem) skills.AgentPackItem {
	return skills.AgentPackItem{
		Name:          strings.TrimSpace(in.Name),
		Title:         in.Title,
		Description:   in.Description,
		Trigger:       strings.TrimSpace(in.Trigger),
		TaskType:      in.TaskType,
		Tags:          append([]string(nil), in.Tags...),
		Steps:         append([]string(nil), in.Steps...),
		Preconditions: append([]string(nil), in.Preconditions...),
		FailureModes:  append([]string(nil), in.FailureModes...),
		RequiredTools: append([]string(nil), in.RequiredTools...),
		RequiredNS:    append([]string(nil), in.RequiredNS...),
		RequiredTags:  append([]string(nil), in.RequiredTags...),
		Extra:         cloneStringMapCopy(in.Extra),
	}
}

// agentPackItemToWire projects a domain pack item onto the wire shape. The
// read-only Origin/Scope fields are populated ("pack"/"agent") so revision
// reads faithfully display the fixed rung.
func agentPackItemToWire(in skills.AgentPackItem) prototypes.AgentConfigAgentPackItem {
	return prototypes.AgentConfigAgentPackItem{
		Name:          in.Name,
		Title:         in.Title,
		Description:   in.Description,
		Trigger:       in.Trigger,
		TaskType:      in.TaskType,
		Tags:          append([]string(nil), in.Tags...),
		Steps:         append([]string(nil), in.Steps...),
		Preconditions: append([]string(nil), in.Preconditions...),
		FailureModes:  append([]string(nil), in.FailureModes...),
		RequiredTools: append([]string(nil), in.RequiredTools...),
		RequiredNS:    append([]string(nil), in.RequiredNS...),
		RequiredTags:  append([]string(nil), in.RequiredTags...),
		Origin:        string(skills.OriginPack),
		Scope:         "agent",
		OriginRef:     in.OriginRef,
		Extra:         cloneStringMapCopy(in.Extra),
	}
}

func cloneStringMapCopy(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// AgentPacksList returns the agent's active pack (full items, canonical
// order) under the caller's verified identity. A missing revision or an
// absent pack section yields an empty list — the valid "no pack" state.
func (s *Service) AgentPacksList(ctx context.Context, req prototypes.AgentConfigAgentPacksListRequest) (prototypes.AgentConfigAgentPacksListResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigAgentPacksListResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksListResponse{}, err
	}
	q := identity.Quadruple{Identity: id}
	if err := s.ensureNotRetired(ctx, q, req.AgentID); err != nil {
		return prototypes.AgentConfigAgentPacksListResponse{}, err
	}
	rev, set, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigAgentPacksListResponse{}, err
	}
	out := make([]prototypes.AgentConfigAgentPackItem, 0, len(rev.Payload.AgentPacks))
	if set && rev.Payload.AgentPacks != nil {
		for _, item := range rev.Payload.AgentPacks {
			out = append(out, agentPackItemToWire(item))
		}
	}
	return prototypes.AgentConfigAgentPacksListResponse{
		Items:           out,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// AgentPacksUpsert DETERMINISTICALLY adds or replaces ONE pack item: body +
// membership are persisted atomically in a single revision. The item is
// validated (shape + bounds + the read-only origin/scope fields), its
// origin_ref is server-stamped, and the composed pack must stay within
// skills.MaxAgentPackItems. Sibling config sections are preserved.
func (s *Service) AgentPacksUpsert(ctx context.Context, req prototypes.AgentConfigAgentPacksUpsertRequest) (prototypes.AgentConfigAgentPacksUpsertResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigAgentPacksUpsertResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksUpsertResponse{}, err
	}
	q := identity.Quadruple{Identity: id}
	if err := s.ensureNotRetired(ctx, q, req.AgentID); err != nil {
		return prototypes.AgentConfigAgentPacksUpsertResponse{}, err
	}
	release := s.lockAgent(q.TenantID, req.AgentID)
	defer release()
	opts := agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash}
	if err := s.precheckExpectedRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, opts); err != nil {
		return prototypes.AgentConfigAgentPacksUpsertResponse{}, err
	}
	if err := validateAgentPackScope(req.Scope); err != nil {
		return prototypes.AgentConfigAgentPacksUpsertResponse{}, err
	}
	if err := validateAgentPackWireItem(req.Skill); err != nil {
		return prototypes.AgentConfigAgentPacksUpsertResponse{}, err
	}
	item := agentPackItemToDomain(req.Skill)
	if err := item.Validate(); err != nil {
		return prototypes.AgentConfigAgentPacksUpsertResponse{}, fmt.Errorf("%w: %w", ErrAgentPacksInvalid, err)
	}
	skill, err := item.Skill()
	if err != nil {
		return prototypes.AgentConfigAgentPacksUpsertResponse{}, fmt.Errorf("%w: %w", ErrAgentPacksInvalid, err)
	}
	hash := skill.ContentHash
	// Server-stamp the provenance ref: the caller cannot author it.
	item.OriginRef = packUpsertedOriginRef(req.AgentID, hash)

	payload, err := s.packPayloadFromActive(ctx, q, req.AgentID, func(current []skills.AgentPackItem) []skills.AgentPackItem {
		replaced := false
		canonical := skills.CanonicalPackName(item.Name)
		out := make([]skills.AgentPackItem, 0, len(current)+1)
		for _, existing := range current {
			if skills.CanonicalPackName(existing.Name) == canonical {
				out = append(out, item) // replace — one winner per canonical name
				replaced = true
				continue
			}
			out = append(out, existing)
		}
		if !replaced {
			out = append(out, item)
		}
		return out
	})
	if err != nil {
		return prototypes.AgentConfigAgentPacksUpsertResponse{}, err
	}
	if err := boundPackSize(payload.AgentPacks); err != nil {
		return prototypes.AgentConfigAgentPacksUpsertResponse{}, err
	}
	rev, err := s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload, opts)
	if err != nil {
		return prototypes.AgentConfigAgentPacksUpsertResponse{}, err
	}
	return prototypes.AgentConfigAgentPacksUpsertResponse{
		Revision:        revisionToWire(rev),
		Skill:           packSkillSummary(skill),
		Hash:            hash,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// AgentPacksRemove DETERMINISTICALLY drops ONE pack item by name. A missing
// name fails loud with ErrAgentPackNotFound (a stale remove can never
// silently no-op). Sibling config sections are preserved.
func (s *Service) AgentPacksRemove(ctx context.Context, req prototypes.AgentConfigAgentPacksRemoveRequest) (prototypes.AgentConfigAgentPacksRemoveResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigAgentPacksRemoveResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksRemoveResponse{}, err
	}
	q := identity.Quadruple{Identity: id}
	if err := s.ensureNotRetired(ctx, q, req.AgentID); err != nil {
		return prototypes.AgentConfigAgentPacksRemoveResponse{}, err
	}
	if strings.TrimSpace(req.Name) == "" {
		return prototypes.AgentConfigAgentPacksRemoveResponse{}, fmt.Errorf("%w: name is empty", ErrAgentPacksInvalid)
	}
	release := s.lockAgent(q.TenantID, req.AgentID)
	defer release()
	opts := agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash}
	if err := s.precheckExpectedRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, opts); err != nil {
		return prototypes.AgentConfigAgentPacksRemoveResponse{}, err
	}
	canonical := skills.CanonicalPackName(req.Name)
	var removed bool
	payload, err := s.packPayloadFromActive(ctx, q, req.AgentID, func(current []skills.AgentPackItem) []skills.AgentPackItem {
		out := make([]skills.AgentPackItem, 0, len(current))
		for _, existing := range current {
			if skills.CanonicalPackName(existing.Name) == canonical {
				removed = true
				continue
			}
			out = append(out, existing)
		}
		return out
	})
	if err != nil {
		return prototypes.AgentConfigAgentPacksRemoveResponse{}, err
	}
	if !removed {
		return prototypes.AgentConfigAgentPacksRemoveResponse{}, fmt.Errorf("%w: name=%q", ErrAgentPackNotFound, req.Name)
	}
	rev, err := s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload, opts)
	if err != nil {
		return prototypes.AgentConfigAgentPacksRemoveResponse{}, err
	}
	return prototypes.AgentConfigAgentPacksRemoveResponse{
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// AgentPacksPropose is the GOVERNED first phase: draft a bounded pack skill
// body from a bounded operator intent. The draft uses the agent's configured
// model (from the active revision's llm_params, validated against the
// configured ModelProfiles) under the versioned revision policy named by
// ExpectedContentHash — a stale expected revision is refused before any
// drafting. Propose persists a durable receipt unless dry_run is requested. It returns
// the canonical draft body, its content hash (the hash the operator
// reviews), capability warnings, and the deterministic provenance stamp the
// commit must echo.
func (s *Service) AgentPacksPropose(ctx context.Context, req prototypes.AgentConfigAgentPacksProposeRequest) (prototypes.AgentConfigAgentPacksProposeResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigAgentPacksProposeResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksProposeResponse{}, err
	}
	q := identity.Quadruple{Identity: id}
	if err := s.ensureNotRetired(ctx, q, req.AgentID); err != nil {
		return prototypes.AgentConfigAgentPacksProposeResponse{}, err
	}
	if err := validateAgentPackScope(req.Scope); err != nil {
		return prototypes.AgentConfigAgentPacksProposeResponse{}, err
	}
	if strings.TrimSpace(req.ExpectedContentHash) == "" {
		return prototypes.AgentConfigAgentPacksProposeResponse{}, fmt.Errorf("%w: expected_content_hash is mandatory", ErrAgentPacksInvalid)
	}
	if r := len([]rune(req.Intent)); r == 0 || r > maxAgentPackIntentRunes {
		return prototypes.AgentConfigAgentPacksProposeResponse{}, fmt.Errorf("%w: intent must be non-empty and ≤ %d runes (got %d)", ErrAgentPacksInvalid, maxAgentPackIntentRunes, r)
	}
	// Bind the proposal to the versioned revision policy the operator
	// reviewed: a moved base is refused before any LLM spend.
	if err := s.precheckExpectedRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash}); err != nil {
		return prototypes.AgentConfigAgentPacksProposeResponse{}, err
	}
	if s.agentPackProposer == nil {
		return prototypes.AgentConfigAgentPacksProposeResponse{}, ErrAgentPackProposeUnavailable
	}
	// Resolve the configured model from the versioned policy: the active
	// revision's per-agent llm_params.model (validated against the
	// configured ModelProfiles; a revision-pinned unknown model is a loud
	// policy failure, never a silent fallback).
	model, err := s.agentPackConfiguredModel(ctx, q, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksProposeResponse{}, err
	}
	draft, err := s.agentPackProposer.Draft(ctx, q, req.AgentID, model, req.Intent)
	if err != nil {
		return prototypes.AgentConfigAgentPacksProposeResponse{}, err
	}
	if err := draft.Item.Validate(); err != nil {
		// The proposer's output is never trusted by itself: a malformed
		// draft fails loud (a proposer bug is a runtime fault, not a
		// silently-skipped skill).
		return prototypes.AgentConfigAgentPacksProposeResponse{}, fmt.Errorf("%w: proposer returned an invalid draft: %w", ErrAgentPacksInvalid, err)
	}
	skill, err := draft.Item.Skill()
	if err != nil {
		return prototypes.AgentConfigAgentPacksProposeResponse{}, fmt.Errorf("%w: proposer returned an invalid draft: %w", ErrAgentPacksInvalid, err)
	}
	if s.agentPackProposals == nil {
		return prototypes.AgentConfigAgentPacksProposeResponse{}, ErrAgentPackProposalUnavailable
	}
	if req.DryRun {
		return prototypes.AgentConfigAgentPacksProposeResponse{
			Skill:               agentPackItemToWire(skills.PackItemFromSkill(skill)),
			Hash:                skill.ContentHash,
			Warnings:            append([]string(nil), draft.Warnings...),
			Provenance:          packProposedProvenance(req.AgentID, skill.ContentHash),
			ExpectedContentHash: req.ExpectedContentHash,
			DryRun:              true,
			ProtocolVersion:     prototypes.ProtocolVersion,
		}, nil
	}
	proposalID := state.NewEventID()
	recordBytes, err := marshalProposal(agentPackProposalRecord{
		AgentID: req.AgentID, ExpectedContentHash: req.ExpectedContentHash,
		ReviewedHash: skill.ContentHash, Provenance: packProposedProvenance(req.AgentID, skill.ContentHash),
		Item: skills.PackItemFromSkill(skill),
	})
	if err != nil {
		return prototypes.AgentConfigAgentPacksProposeResponse{}, fmt.Errorf("%w: encode proposal: %v", ErrAgentPacksInvalid, err)
	}
	if err := s.agentPackProposals.Save(ctx, state.StateRecord{ID: proposalID, Identity: q, Kind: proposalKind(string(proposalID)), Bytes: recordBytes}); err != nil {
		return prototypes.AgentConfigAgentPacksProposeResponse{}, fmt.Errorf("save pack proposal: %w", err)
	}
	return prototypes.AgentConfigAgentPacksProposeResponse{
		Skill:               agentPackItemToWire(skills.PackItemFromSkill(skill)),
		Hash:                skill.ContentHash,
		Warnings:            append([]string(nil), draft.Warnings...),
		Provenance:          packProposedProvenance(req.AgentID, skill.ContentHash),
		ProposalID:          string(proposalID),
		ExpectedContentHash: req.ExpectedContentHash,
		DryRun:              req.DryRun,
		ProtocolVersion:     prototypes.ProtocolVersion,
	}, nil
}

// agentPackConfiguredModel resolves the agent's configured model from the
// active revision's per-agent llm_params section, validated against the
// configured ModelProfiles. A nil/absent section yields "" (the proposer's
// own default); a revision-pinned unknown model is refused loudly.
func (s *Service) agentPackConfiguredModel(ctx context.Context, q identity.Quadruple, agentID string) (string, error) {
	rev, set, err := s.registry.Active(ctx, q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return "", err
	}
	if !set || rev.Payload.LLMParams == nil || rev.Payload.LLMParams.Model == nil || *rev.Payload.LLMParams.Model == "" {
		return "", nil
	}
	model := *rev.Payload.LLMParams.Model
	if err := s.validateModel(&model); err != nil {
		return "", err
	}
	return model, nil
}

// AgentPacksCommit is the GOVERNED second phase: CAS-bind the EXACT reviewed
// hash and atomically persist body + membership in ONE revision. The
// submitted body must hash to exactly ReviewedHash (a changed body — and
// therefore a changed hash / scope / capability annotation / provenance —
// is refused and NOTHING is persisted), the Provenance must echo the
// deterministic proposal stamp, and the expected-revision token must still
// match (the cross-write CAS half).
func (s *Service) AgentPacksCommit(ctx context.Context, req prototypes.AgentConfigAgentPacksCommitRequest) (prototypes.AgentConfigAgentPacksCommitResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, err
	}
	q := identity.Quadruple{Identity: id}
	if err := s.ensureNotRetired(ctx, q, req.AgentID); err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, err
	}
	release := s.lockAgent(q.TenantID, req.AgentID)
	defer release()
	if err := validateAgentPackScope(req.Scope); err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, err
	}
	if err := validateAgentPackWireItem(req.Skill); err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, err
	}
	if strings.TrimSpace(req.ReviewedHash) == "" {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("%w: reviewed_hash is empty", ErrAgentPacksInvalid)
	}
	if strings.TrimSpace(req.ExpectedContentHash) == "" || strings.TrimSpace(req.ProposalID) == "" {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("%w: proposal_id and expected_content_hash are mandatory", ErrAgentPacksInvalid)
	}
	if s.agentPackProposals == nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, ErrAgentPackProposalUnavailable
	}
	proposalRecord, err := s.agentPackProposals.Load(ctx, q, proposalKind(req.ProposalID))
	if err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, ErrAgentPackProposalInvalid
	}
	proposal, err := unmarshalProposal(proposalRecord.Bytes)
	if err != nil || proposal.AgentID != req.AgentID || proposal.ExpectedContentHash != req.ExpectedContentHash || proposal.ReviewedHash != req.ReviewedHash {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, ErrAgentPackProposalInvalid
	}
	if err := normalizePackProposalProvenance(&proposal); err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, err
	}
	item := agentPackItemToDomain(req.Skill)
	if err := item.Validate(); err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("%w: %w", ErrAgentPacksInvalid, err)
	}
	skill, err := item.Skill()
	if err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("%w: %w", ErrAgentPacksInvalid, err)
	}
	// CAS half 1 — the reviewed hash is binding: a body that no longer
	// hashes to the reviewed artifact is a changed review and is refused.
	if skill.ContentHash != req.ReviewedHash {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("%w: computed=%s reviewed=%s (a changed body is a changed review)",
			ErrAgentPackHashMismatch, skill.ContentHash, req.ReviewedHash)
	}
	wantBody, err := json.Marshal(skills.PackItemFromSkill(skill))
	if err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("compare pack proposal: %w", err)
	}
	proposalBody, err := json.Marshal(proposal.Item)
	if err != nil || string(wantBody) != string(proposalBody) {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, ErrAgentPackProposalInvalid
	}
	if req.Provenance != proposal.Provenance {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("%w: got=%q want=%q",
			ErrAgentPackProvenanceMismatch, req.Provenance, proposal.Provenance)
	}
	committing := proposal.Phase == "committing"
	if committing {
		if proposal.TargetRevisionID == "" || proposal.TargetContentHash == "" {
			return prototypes.AgentConfigAgentPacksCommitResponse{}, ErrAgentPackProposalInvalid
		}
		published, getErr := s.registry.Get(ctx, q, req.AgentID, proposal.TargetRevisionID, agentcfg.ConfigScopeAgent)
		if getErr != nil || published.ContentHash != proposal.TargetContentHash {
			return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("%w: committing receipt target was not published", ErrAgentPackProposalInvalid)
		}
		active, activeSet, activeErr := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent)
		if activeErr != nil || !activeSet || active.RevisionID != proposal.TargetRevisionID || active.ContentHash != proposal.TargetContentHash {
			return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("%w: committing receipt target is not active", ErrAgentPackProposalInvalid)
		}
		deleted, deleteErr := s.agentPackProposals.DeleteIf(ctx, state.SlotExpectation{Identity: q, Kind: proposalKind(req.ProposalID), ExpectedEventID: proposalRecord.ID})
		if deleteErr != nil || !deleted {
			return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("finalize pack proposal receipt: %w", ErrAgentPackProposalInvalid)
		}
		return prototypes.AgentConfigAgentPacksCommitResponse{Revision: revisionToWire(published), Skill: packSkillSummary(skill), Hash: skill.ContentHash, ProtocolVersion: prototypes.ProtocolVersion}, nil
	}
	opts := agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash}
	if err := s.precheckExpectedRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, opts); err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, err
	}
	// CAS half 2 — the proposal stamp is binding: a commit must echo the
	// exact proposal it reviewed.
	if want := packProposedProvenance(req.AgentID, req.ReviewedHash); req.Provenance != want {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("%w: got=%q want=%q",
			ErrAgentPackProvenanceMismatch, req.Provenance, want)
	}
	// Server-stamp the committed provenance ref.
	item.OriginRef = packCommittedOriginRef(req.AgentID, req.ReviewedHash)
	payload, err := s.packPayloadFromActive(ctx, q, req.AgentID, func(current []skills.AgentPackItem) []skills.AgentPackItem {
		replaced := false
		canonical := skills.CanonicalPackName(item.Name)
		out := make([]skills.AgentPackItem, 0, len(current)+1)
		for _, existing := range current {
			if skills.CanonicalPackName(existing.Name) == canonical {
				out = append(out, item)
				replaced = true
				continue
			}
			out = append(out, existing)
		}
		if !replaced {
			out = append(out, item)
		}
		return out
	})
	if err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, err
	}
	if err := boundPackSize(payload.AgentPacks); err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, err
	}
	targetHash, err := agentcfg.ContentHash(agentcfg.NormalizePayload(payload))
	if err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("hash pack publication: %w", err)
	}
	targetRevisionID := string(state.NewEventID())
	if proposal.Phase == "committing" {
		targetRevisionID = proposal.TargetRevisionID
	}
	committingBytes, err := marshalProposal(agentPackProposalRecord{
		AgentID: req.AgentID, ExpectedContentHash: req.ExpectedContentHash,
		ReviewedHash: req.ReviewedHash, Provenance: proposal.Provenance, Item: proposal.Item,
		Phase: "committing", TargetRevisionID: targetRevisionID, TargetContentHash: targetHash,
	})
	if err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("encode committing pack proposal: %w", err)
	}
	if proposal.Phase != "committing" {
		if err := s.agentPackProposals.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: proposalKind(req.ProposalID), ExpectedEventID: proposalRecord.ID}}, state.StateRecord{ID: proposalRecord.ID, Identity: q, Kind: proposalKind(req.ProposalID), Bytes: committingBytes}); err != nil {
			return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("mark pack proposal committing: %w", err)
		}
	}
	opts.TargetRevisionID = targetRevisionID
	opts.PublicationFence = &agentcfg.PublicationFence{Identity: q, Kind: proposalKind(req.ProposalID), EventID: string(proposalRecord.ID)}
	rev, err := s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload, opts)
	if err != nil {
		return prototypes.AgentConfigAgentPacksCommitResponse{}, err
	}
	if committing {
		if rev.RevisionID != targetRevisionID || rev.ContentHash != targetContentHash {
			return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("%w: resumed publication target changed", ErrAgentPackProposalInvalid)
		}
	}
	deleted, deleteErr := s.agentPackProposals.DeleteIf(ctx, state.SlotExpectation{Identity: q, Kind: proposalKind(req.ProposalID), ExpectedEventID: proposalRecord.ID})
	if deleteErr != nil || !deleted {
		// Keep the committing receipt. A retry can finalize the exact revision;
		// compensating the active pointer would create a second mutation.
		return prototypes.AgentConfigAgentPacksCommitResponse{}, fmt.Errorf("consume pack proposal receipt: %w", ErrAgentPackProposalInvalid)
	}
	return prototypes.AgentConfigAgentPacksCommitResponse{
		Revision:        revisionToWire(rev),
		Skill:           packSkillSummary(skill),
		Hash:            skill.ContentHash,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// packPayloadFromActive returns a payload that preserves every sibling
// section of the active revision (the symmetric invariant
// recordSkillsMembership honours for the legacy skills verbs) with the pack
// section rebuilt by mutate. The caller holds the agent write lock, so the
// read-modify-write is atomic within the process.
func (s *Service) packPayloadFromActive(ctx context.Context, q identity.Quadruple, agentID string, mutate func([]skills.AgentPackItem) []skills.AgentPackItem) (agentcfg.ConfigPayload, error) {
	active, hasActive, err := s.registry.Active(ctx, q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return agentcfg.ConfigPayload{}, err
	}
	var payload agentcfg.ConfigPayload
	if hasActive {
		payload = active.Payload
	}
	payload.AgentPacks = mutate(copyAgentPackItems(payload.AgentPacks))
	// NormalizePayload (run by SetRevision) sorts + de-dups + drops empties,
	// so the mutated slice is canonicalised before it reaches the hash.
	return payload, nil
}

func copyAgentPackItems(items []skills.AgentPackItem) []skills.AgentPackItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]skills.AgentPackItem, len(items))
	copy(out, items)
	return out
}

// boundPackSize fails loud when a composed pack would exceed the revision
// bound. Called AFTER the mutation and BEFORE SetRevision.
func boundPackSize(items []skills.AgentPackItem) error {
	if len(items) > skills.MaxAgentPackItems {
		return fmt.Errorf("%w: pack exceeds %d items (%d)", ErrAgentPacksInvalid, skills.MaxAgentPackItems, len(items))
	}
	return nil
}

// packSkillSummary projects a converted pack skill onto the wire summary —
// metadata only, never the body.
func packSkillSummary(sk skills.Skill) prototypes.AgentConfigSkillSummary {
	return prototypes.AgentConfigSkillSummary{
		Name:        sk.Name,
		Title:       sk.Title,
		Trigger:     sk.Trigger,
		TaskType:    sk.TaskType,
		Origin:      string(sk.Origin),
		Scope:       string(sk.Scope),
		ContentHash: sk.ContentHash,
		UpdatedAt:   sk.UpdatedAt,
	}
}
