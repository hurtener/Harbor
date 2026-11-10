package protocol

// agentpackimport.go — the shared Agent-pack destination of the two-phase
// complete-skill-package import (`agent_config.agent_packs.import_validate` /
// `agent_config.agent_packs.import_commit`).
//
// The package review, limits, token sealing, commit admission and fresh
// revalidation are the SAME code the personal import runs
// (UserSkillImportService.reviewPackage / issueProposal / openProposal /
// recheckPackage); only the destination differs:
//
//   - The package unit (skill + support bytes) is stored through the ONE
//     installed-package SkillStore contract at the Agent's control-plane key
//     agentcfg.AgentScope(tenant, agent) — runtime-owned state that is not
//     any end user's personal skill and is resolvable for every caller with
//     signed reach to the Agent (ResolveAgentPackSupport).
//   - The membership is ONE agent-scope config revision whose pack item
//     carries the server-stamped PackageHash. The revision write is
//     CAS-bound to the composition witness (the active agent-scope content
//     hash sealed at validate) and fenced by a token-derived commit ledger,
//     so a retry of the same token replays instead of writing twice.
//
// Write order: package first, then the fenced revision. A failed revision
// write compensates the package with the exact-receipt Delete/Restore
// primitives. Removing or replacing a package-backed item through any pack
// verb releases its package (releaseDroppedPackages). A package never grants
// tools: required tools are review warnings only.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// ErrAgentPackImportUnavailable — the shared-pack package import was called
// but no package-import core is wired on this runtime (→ 501 at the wire
// edge).
var ErrAgentPackImportUnavailable = errors.New("agentcfg/protocol: agent pack package import not wired on this runtime")

// agentPackImportKindPrefix namespaces the token-derived commit ledger slots
// (SHA-256 of the authenticated token bytes, under the caller's identity).
const agentPackImportKindPrefix = "agentcfg.agent_pack.import."

// WithPackageImport wires the skill-package import core so the
// `agent_packs.import_*` verbs are live. Optional — nil leaves them answering
// ErrAgentPackImportUnavailable.
func WithPackageImport(core *UserSkillImportService) Option {
	return func(s *Service) {
		if core != nil {
			s.packageImport = core
		}
	}
}

// agentPackImportRecord is the durable commit-ledger payload of one token.
type agentPackImportRecord struct {
	Phase             string `json:"phase"`
	Name              string `json:"name"`
	PackageHash       string `json:"package_hash"`
	TargetRevisionID  string `json:"target_revision_id"`
	TargetContentHash string `json:"target_content_hash"`
}

func agentPackImportKind(token string) string {
	sum := sha256.Sum256([]byte(token))
	return agentPackImportKindPrefix + hex.EncodeToString(sum[:])
}

func packImportedOriginRef(agentID, packageHash string) string {
	return "pack.imported." + agentID + "." + hashPrefix(strings.TrimPrefix(packageHash, skills.PackageHashVersionV1+":"))
}

// agentPackItemFromPackage projects the installed unit onto the shared pack
// item: the same parsed skill, the canonical package name, the server-stamped
// provenance and package reference. The pack-item bounds apply on top of the
// package bounds.
func agentPackItemFromPackage(unit skills.InstalledPackage, agentID string) (skills.AgentPackItem, skills.Skill, error) {
	item := skills.PackItemFromSkill(unit.Skill)
	item.Name = unit.Package.Name
	item.OriginRef = packImportedOriginRef(agentID, unit.PackageHash)
	item.PackageHash = unit.PackageHash
	if err := item.Validate(); err != nil {
		return skills.AgentPackItem{}, skills.Skill{}, fmt.Errorf("%w: %w", ErrUserSkillImportPackageInvalid, err)
	}
	skill, err := item.Skill()
	if err != nil {
		return skills.AgentPackItem{}, skills.Skill{}, fmt.Errorf("%w: %w", ErrUserSkillImportPackageInvalid, err)
	}
	return item, skill, nil
}

// withImportedPackItem places item in the pack. An existing item of the same
// canonical name is replaced only with explicit consent (an identical
// package-backed item needs none — there is nothing to replace).
func withImportedPackItem(current []skills.AgentPackItem, item skills.AgentPackItem, replace bool) ([]skills.AgentPackItem, error) {
	canonical := skills.CanonicalPackName(item.Name)
	out := make([]skills.AgentPackItem, 0, len(current)+1)
	placed := false
	for _, existing := range current {
		if skills.CanonicalPackName(existing.Name) != canonical {
			out = append(out, existing)
			continue
		}
		if existing.PackageHash != item.PackageHash && !replace {
			return nil, fmt.Errorf("%w: pack item %q already exists", ErrUserSkillImportReplaceRequired, canonical)
		}
		out = append(out, item)
		placed = true
	}
	if !placed {
		out = append(out, item)
	}
	return out, nil
}

func (s *Service) packImportCore() (*UserSkillImportService, error) {
	if s.packageImport == nil {
		return nil, ErrAgentPackImportUnavailable
	}
	if s.agentPackProposals == nil {
		return nil, ErrAgentPackProposalUnavailable
	}
	return s.packageImport, nil
}

// agentPackWitness is the Agent's shared-pack composition witness: the active
// agent-scope revision content hash, or ExpectNoActiveRevision.
func (s *Service) agentPackWitness(ctx context.Context, q identity.Quadruple, agentID string) (string, error) {
	active, set, err := s.registry.Active(ctx, q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return "", err
	}
	if !set {
		return agentcfg.ExpectNoActiveRevision, nil
	}
	return active.ContentHash, nil
}

// AgentPacksImportValidate is the ZERO-WRITE first phase: it reviews the
// caller-owned package artifact through the shared import core and seals a
// token bound to the caller, the Agent, the artifact, the package hash and
// the composition witness. No package, revision or ledger write happens.
func (s *Service) AgentPacksImportValidate(ctx context.Context, req prototypes.AgentConfigAgentPacksImportValidateRequest) (prototypes.AgentConfigAgentPacksImportValidateResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigAgentPacksImportValidateResponse{}, err
	}
	core, err := s.packImportCore()
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportValidateResponse{}, err
	}
	id, q, err := core.resolveIdentityAndReach(ctx, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportValidateResponse{}, err
	}
	reviewed, err := core.reviewPackage(ctx, id, q, req.AgentID, req.ArtifactID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportValidateResponse{}, err
	}
	item, skill, err := agentPackItemFromPackage(reviewed.unit, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportValidateResponse{}, err
	}
	witness, err := s.agentPackWitness(ctx, q, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportValidateResponse{}, err
	}
	// Refuse a package that could never fit before handing out a token.
	payload, err := s.packPayloadFromActive(ctx, q, req.AgentID, func(current []skills.AgentPackItem) []skills.AgentPackItem {
		out, _ := withImportedPackItem(current, item, true)
		return out
	})
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportValidateResponse{}, err
	}
	if err := boundPackSize(payload.AgentPacks); err != nil {
		return prototypes.AgentConfigAgentPacksImportValidateResponse{}, err
	}
	resp, err := core.issueProposal(importDestinationAgentPack, id, req.AgentID, req.ArtifactID, reviewed, skill, witness)
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportValidateResponse{}, err
	}
	return prototypes.AgentConfigAgentPacksImportValidateResponse{
		ProposalToken:       resp.ProposalToken,
		Review:              UserSkillImportReviewToWire(resp.Review),
		Warnings:            resp.Warnings,
		PackageHash:         resp.PackageHash,
		ExpectedContentHash: resp.ExpectedContentHash,
		ExpiresAt:           resp.ExpiresAt,
		ProtocolVersion:     prototypes.ProtocolVersion,
	}, nil
}

// AgentPacksImportCommit is the explicit second phase: it re-admits and
// freshly revalidates the reviewed package through the shared import core and
// installs exactly that package into the Agent's shared pack (package unit +
// one fenced revision). A replay of the same token returns the landed result
// without a second write; a moved witness is a revision conflict.
func (s *Service) AgentPacksImportCommit(ctx context.Context, req prototypes.AgentConfigAgentPacksImportCommitRequest) (prototypes.AgentConfigAgentPacksImportCommitResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, err
	}
	core, err := s.packImportCore()
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, err
	}
	id, q, err := core.resolveIdentityAndReach(ctx, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, err
	}
	commitReq := UserSkillImportCommitRequest{
		ProposalToken: req.ProposalToken, AgentID: req.AgentID, Name: req.Name,
		ReviewedPackageHash: req.ReviewedPackageHash, ExpectedContentHash: req.ExpectedContentHash, Replace: req.Replace,
	}
	claims, err := core.openProposal(ctx, id, q, importDestinationAgentPack, commitReq)
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, err
	}
	release := s.lockAgent(q.TenantID, req.AgentID)
	defer release()

	kind := agentPackImportKind(req.ProposalToken)
	loaded, err := s.agentPackProposals.Load(ctx, q, kind)
	if errors.Is(err, state.ErrNotFound) {
		return s.publishPackImport(ctx, core, id, q, commitReq, claims, kind, nil, "")
	}
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, fmt.Errorf("agent pack import: load commit ledger: %w", err)
	}
	var record agentPackImportRecord
	if err := json.Unmarshal(loaded.Bytes, &record); err != nil || record.Name != claims.Name || record.PackageHash != claims.PackageHash || record.TargetRevisionID == "" {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, ErrUserSkillImportProposalInvalid
	}
	switch record.Phase {
	case userSkillImportPhaseCommitted:
		return s.packImportResult(ctx, core, q, req.AgentID, record, true)
	case userSkillImportPhaseCommitting:
		if _, getErr := s.registry.Get(ctx, q, req.AgentID, record.TargetRevisionID, agentcfg.ConfigScopeAgent); getErr == nil {
			// The revision landed but the terminal marker did not: record it
			// and return the landed result.
			s.markPackImportCommitted(ctx, q, kind, loaded.ID, record)
			return s.packImportResult(ctx, core, q, req.AgentID, record, true)
		} else if !errors.Is(getErr, agentcfg.ErrRevisionNotFound) {
			return prototypes.AgentConfigAgentPacksImportCommitResponse{}, fmt.Errorf("agent pack import: load target revision: %w", getErr)
		}
		return s.publishPackImport(ctx, core, id, q, commitReq, claims, kind, &record, loaded.ID)
	default:
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, ErrUserSkillImportProposalInvalid
	}
}

// publishPackImport installs the package and publishes the fenced revision.
// resume is nil for a fresh commit (the ledger slot is absent) and names the
// recorded target when a previous attempt stopped before publication.
func (s *Service) publishPackImport(ctx context.Context, core *UserSkillImportService, id identity.Identity, q identity.Quadruple, req UserSkillImportCommitRequest, claims userSkillImportProposalClaims, kind string, resume *agentPackImportRecord, resumeSlot state.EventID) (prototypes.AgentConfigAgentPacksImportCommitResponse, error) {
	unit, err := core.recheckPackage(ctx, id, req.AgentID, claims)
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, err
	}
	item, skill, err := agentPackItemFromPackage(unit, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, err
	}
	if skill.ContentHash != claims.ContentHash {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, fmt.Errorf("%w: the pack item content changed after validation", ErrUserSkillImportHashMismatch)
	}
	// The composition witness: a moved shared pack is a revision conflict.
	opts := agentcfg.SetOptions{ExpectedContentHash: claims.ExpectedContentHash}
	if err := s.precheckExpectedRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, opts); err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, err
	}
	var placeErr error
	payload, err := s.packPayloadFromActive(ctx, q, req.AgentID, func(current []skills.AgentPackItem) []skills.AgentPackItem {
		var out []skills.AgentPackItem
		out, placeErr = withImportedPackItem(current, item, req.Replace)
		return out
	})
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, err
	}
	if placeErr != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, placeErr
	}
	if err := boundPackSize(payload.AgentPacks); err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, err
	}
	if err := GuardBootOwnedRevision(bootOwnershipFromContext(ctx), q.TenantID, req.AgentID, payload.AgentPacks); err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, err
	}
	targetHash, err := agentcfg.ContentHash(agentcfg.NormalizePayload(payload))
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, fmt.Errorf("agent pack import: hash publication: %w", err)
	}

	record := agentPackImportRecord{
		Phase: userSkillImportPhaseCommitting, Name: claims.Name, PackageHash: unit.PackageHash,
		TargetRevisionID: string(state.NewEventID()), TargetContentHash: targetHash,
	}
	slot := resumeSlot
	if resume != nil {
		if resume.TargetContentHash != targetHash {
			return prototypes.AgentConfigAgentPacksImportCommitResponse{}, fmt.Errorf("%w: resumed publication payload changed", ErrUserSkillImportProposalInvalid)
		}
		record = *resume
	} else {
		raw, err := json.Marshal(record)
		if err != nil {
			return prototypes.AgentConfigAgentPacksImportCommitResponse{}, fmt.Errorf("agent pack import: encode committing marker: %w", err)
		}
		slot = state.NewEventID()
		if err := s.agentPackProposals.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: kind}}, state.StateRecord{ID: slot, Identity: q, Kind: kind, Bytes: raw}); err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				return prototypes.AgentConfigAgentPacksImportCommitResponse{}, fmt.Errorf("%w: a concurrent commit of this proposal token is in progress", ErrUserSkillImportProposalInvalid)
			}
			return prototypes.AgentConfigAgentPacksImportCommitResponse{}, fmt.Errorf("agent pack import: mark committing: %w", err)
		}
	}

	packQ, err := agentcfg.AgentScope(q.TenantID, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, err
	}
	prior, priorFound, err := core.currentWinner(ctx, packQ, req.AgentID, unit.Package.Name)
	if err != nil {
		s.abandonPackImport(ctx, q, kind, slot, resume == nil)
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, err
	}
	cond := skills.InstalledPackageCondition{ExpectedAbsent: true}
	storeReplace := false
	if priorFound {
		cond = skills.InstalledPackageCondition{ExpectedHash: prior.PackageHash}
		storeReplace = prior.PackageHash != unit.PackageHash
	}
	receipt, err := core.store.PutInstalledPackage(ctx, packQ, req.AgentID, unit, cond, storeReplace)
	if err != nil {
		s.abandonPackImport(ctx, q, kind, slot, resume == nil)
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, fmt.Errorf("agent pack import: put installed package: %w", err)
	}

	opts.TargetRevisionID = record.TargetRevisionID
	opts.PublicationFence = &agentcfg.PublicationFence{Identity: q, Kind: kind, EventID: string(slot)}
	if _, err := s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload, opts); err != nil {
		// Exact-receipt compensation: undo ONLY the version this commit
		// wrote, restoring the displaced package when there was one.
		var compErr error
		switch {
		case !priorFound:
			_, compErr = core.store.DeleteInstalledPackage(ctx, packQ, req.AgentID, unit.Package.Name, receipt)
		case storeReplace:
			_, compErr = core.store.RestoreInstalledPackage(ctx, packQ, req.AgentID, unit.Package.Name, receipt, prior)
		}
		if compErr != nil {
			s.logger.ErrorContext(ctx, "agent pack import: compensate installed package failed",
				"agent_id", req.AgentID, "name", unit.Package.Name, "error", compErr.Error())
		}
		s.abandonPackImport(ctx, q, kind, slot, resume == nil)
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, errors.Join(err, compErr)
	}
	s.markPackImportCommitted(ctx, q, kind, slot, record)
	return s.packImportResult(ctx, core, q, req.AgentID, record, false)
}

// abandonPackImport restores the absent ledger slot after a failed fresh
// commit so the caller can retry (e.g. with replacement consent). A resumed
// marker is kept: its recorded target still binds the retry.
func (s *Service) abandonPackImport(ctx context.Context, q identity.Quadruple, kind string, slot state.EventID, fresh bool) {
	if !fresh {
		return
	}
	if _, err := s.agentPackProposals.DeleteIf(ctx, state.SlotExpectation{Identity: q, Kind: kind, ExpectedEventID: slot}); err != nil {
		s.logger.ErrorContext(ctx, "agent pack import: release committing marker failed", "error", err.Error())
	}
}

// markPackImportCommitted records the terminal marker. A failure is logged,
// not returned: the revision IS published and a retry recognizes it.
func (s *Service) markPackImportCommitted(ctx context.Context, q identity.Quadruple, kind string, slot state.EventID, record agentPackImportRecord) {
	record.Phase = userSkillImportPhaseCommitted
	raw, err := json.Marshal(record)
	if err == nil {
		err = s.agentPackProposals.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: kind, ExpectedEventID: slot}},
			state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: raw})
	}
	if err != nil && !errors.Is(err, state.ErrConditionFailed) {
		s.logger.WarnContext(ctx, "agent pack import: record committed marker failed (retry replays)", "error", err.Error())
	}
}

// packImportResult builds the terminal response from the published target
// revision, refusing when another package has since won the item's key.
func (s *Service) packImportResult(ctx context.Context, core *UserSkillImportService, q identity.Quadruple, agentID string, record agentPackImportRecord, replayed bool) (prototypes.AgentConfigAgentPacksImportCommitResponse, error) {
	rev, err := s.registry.Get(ctx, q, agentID, record.TargetRevisionID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, fmt.Errorf("agent pack import: load published revision: %w", err)
	}
	packQ, err := agentcfg.AgentScope(q.TenantID, agentID)
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, err
	}
	winner, found, err := core.currentWinner(ctx, packQ, agentID, record.Name)
	if err != nil {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, err
	}
	if !found || winner.PackageHash != record.PackageHash {
		return prototypes.AgentConfigAgentPacksImportCommitResponse{}, fmt.Errorf("%w: the pack item is held by a different package", ErrUserSkillImportConcurrentWinner)
	}
	for _, item := range rev.Payload.AgentPacks {
		if skills.CanonicalPackName(item.Name) != record.Name {
			continue
		}
		skill, err := item.Skill()
		if err != nil {
			return prototypes.AgentConfigAgentPacksImportCommitResponse{}, fmt.Errorf("%w: %w", ErrAgentPacksInvalid, err)
		}
		return prototypes.AgentConfigAgentPacksImportCommitResponse{
			Revision:        revisionToWire(rev),
			Skill:           packSkillSummary(skill),
			PackageHash:     record.PackageHash,
			Replayed:        replayed,
			ProtocolVersion: prototypes.ProtocolVersion,
		}, nil
	}
	return prototypes.AgentConfigAgentPacksImportCommitResponse{}, fmt.Errorf("%w: published revision lacks the imported item", ErrUserSkillImportProposalInvalid)
}

// releaseDroppedPackages deletes the installed package of every pack item the
// just-published revision no longer references (removed, or replaced by a
// body-only item). Best effort: an unreferenced package is unreachable
// (resolution requires the active item's PackageHash), so a failure is
// logged rather than failing the already-published mutation.
func (s *Service) releaseDroppedPackages(ctx context.Context, tenantID, agentID string, before, after []skills.AgentPackItem) {
	store := s.skills
	if store == nil && s.packageImport != nil {
		store = s.packageImport.store
	}
	if store == nil {
		return
	}
	kept := make(map[string]string, len(after))
	for _, item := range after {
		kept[skills.CanonicalPackName(item.Name)] = item.PackageHash
	}
	for _, item := range before {
		name := skills.CanonicalPackName(item.Name)
		if item.PackageHash == "" || kept[name] == item.PackageHash {
			continue
		}
		packQ, err := agentcfg.AgentScope(tenantID, agentID)
		if err != nil {
			return
		}
		winner, err := store.GetInstalledPackage(ctx, packQ, agentID, name)
		if err != nil || winner.PackageHash != item.PackageHash {
			continue // already gone, or re-imported in place
		}
		if _, err := store.DeleteInstalledPackage(ctx, packQ, agentID, name, receiptFromWinner(packQ, agentID, name, winner)); err != nil {
			s.logger.WarnContext(ctx, "agent pack: release dropped package failed", "agent_id", agentID, "name", name, "error", err.Error())
		}
	}
}

// ResolveAgentPackSupport resolves ONE support file of a package-backed
// shared pack item for ANY verified caller with signed reach to the Agent —
// the package is runtime-owned Agent state, not the importing operator's.
// The URI must name the package the ACTIVE pack item references; a removed,
// replaced, or foreign package resolves to skills.ErrSupportNotFound.
func (s *UserSkillImportService) ResolveAgentPackSupport(ctx context.Context, agentID, name string, uri skills.PackageURI) (skills.SupportFile, error) {
	_, q, err := s.resolveIdentityAndReach(ctx, agentID)
	if err != nil {
		return skills.SupportFile{}, err
	}
	if err := s.ensureNotRetiredImport(ctx, q, agentID); err != nil {
		return skills.SupportFile{}, err
	}
	active, set, err := s.registry.Active(ctx, q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return skills.SupportFile{}, err
	}
	canonical := skills.CanonicalPackName(name)
	if set {
		for _, item := range active.Payload.AgentPacks {
			if skills.CanonicalPackName(item.Name) != canonical || item.PackageHash == "" || item.PackageHash != uri.Hash {
				continue
			}
			packQ, err := agentcfg.AgentScope(q.TenantID, agentID)
			if err != nil {
				return skills.SupportFile{}, err
			}
			return s.store.ResolveSupport(ctx, packQ, agentID, canonical, uri)
		}
	}
	return skills.SupportFile{}, fmt.Errorf("%w: no active package-backed pack item %q for that package", skills.ErrSupportNotFound, canonical)
}

// UserSkillImportReviewToWire projects the shared package review onto the
// wire shape both import destinations return.
func UserSkillImportReviewToWire(r UserSkillImportReview) prototypes.AgentConfigUserSkillImportReview {
	out := prototypes.AgentConfigUserSkillImportReview{
		Name: r.Name, Title: r.Title, Trigger: r.Trigger, TaskType: r.TaskType,
		Tags: append([]string(nil), r.Tags...), StepCount: r.StepCount,
		RequiredTools: append([]string(nil), r.RequiredTools...),
		RequiredNS:    append([]string(nil), r.RequiredNS...),
		RequiredTags:  append([]string(nil), r.RequiredTags...),
		ContentHash:   r.ContentHash, PackageHash: r.PackageHash,
	}
	for _, f := range r.SupportFiles {
		out.SupportFiles = append(out.SupportFiles, prototypes.AgentConfigUserSkillImportSupportSummary{
			Path: f.Path, Mime: f.Mime, Size: f.Size, Digest: f.Digest,
		})
	}
	return out
}
