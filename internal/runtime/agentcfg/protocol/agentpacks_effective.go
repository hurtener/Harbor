package protocol

// agentpacks_effective.go contains the domain-neutral agent-pack inspection
// and same-runtime copy port. It deliberately does not define Protocol wire
// types: the transport adapter owns request authorization (admin and signed
// reach) and projects these values onto its own versioned envelope.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
	"github.com/hurtener/Harbor/internal/state"
)

var (
	// ErrAgentPackInspectionIdentityRequired reports a missing or incomplete
	// verified identity on an inspection or copy request.
	ErrAgentPackInspectionIdentityRequired = errors.New("agentcfg/protocol: agent-pack operation requires a complete verified identity")
	// ErrAgentPackCopyInvalid reports malformed copy input or an unavailable
	// durable copy ledger.
	ErrAgentPackCopyInvalid = errors.New("agentcfg/protocol: invalid agent-pack copy request")
	// ErrAgentPackCopyExpectation reports a stale source or target composition
	// expectation. No target write is attempted on this path.
	ErrAgentPackCopyExpectation = errors.New("agentcfg/protocol: agent-pack copy composition expectation failed")
	// ErrAgentPackCopyCollision reports an independently-authored target item.
	// A collision fails the whole copy; selected siblings are never partially
	// applied.
	ErrAgentPackCopyCollision = errors.New("agentcfg/protocol: agent-pack copy collision")
	// ErrAgentPackCopyReplay reports a durable idempotency key reused with a
	// different request or after another writer superseded its target.
	ErrAgentPackCopyReplay = errors.New("agentcfg/protocol: agent-pack copy replay conflict")
	// ErrAgentPackCopyIdempotencyConflict distinguishes reuse of an
	// idempotency key with a different request from a prepared receipt whose
	// target was superseded. Both remain replay failures, but only this
	// sentinel maps to the Protocol idempotency-conflict code.
	ErrAgentPackCopyIdempotencyConflict = errors.New("agentcfg/protocol: agent-pack copy idempotency conflict")
)

const (
	agentPackCopyKindPrefix = "agentcfg.agent_pack.copy."
	agentPackCopySchema     = 1
	maxAgentPackCopyIDs     = skills.MaxAgentPackItems
)

// AgentPackLayerItem is one full body from exactly one source layer. Keeping
// boot and active-revision layers separate prevents an effective `both` item
// from losing its distinct source bodies during inspection.
type AgentPackLayerItem struct {
	// PackID is the canonical pack name.
	PackID string
	// Item is the complete pack body from this layer.
	Item skills.AgentPackItem
	// SemanticHash is the canonical attachment-free body hash.
	SemanticHash string
}

// AgentPackEffectiveItem is one deduplicated item in the strict effective
// boot-plus-revision composition.
type AgentPackEffectiveItem struct {
	// PackID is the canonical pack name.
	PackID string
	// Item is the composed full body; for SourceBoth the boot body wins.
	Item skills.AgentPackItem
	// Source is boot, revision, or both.
	Source skills.OperatorTierSource
	// SemanticHash is the canonical attachment-free body hash.
	SemanticHash string
	// Editable is true only for a revision-only item. Boot-owned content is
	// read-only even when its body also exists in a revision.
	Editable bool
}

// AgentPackCopyOutcome is the per-pack result of a copy selection.
type AgentPackCopyOutcome string

const (
	// AgentPackCopyOutcomeCopied means the selected body was written to the
	// target revision (or was already written by this operation's replay).
	AgentPackCopyOutcomeCopied AgentPackCopyOutcome = "copied"
	// AgentPackCopyOutcomeNoop means the target already had the selected body;
	// no target pack write was needed for that selection.
	AgentPackCopyOutcomeNoop AgentPackCopyOutcome = "noop"
)

// AgentPackCopyItemResult gives the deterministic outcome for one selected
// pack. Outcomes are returned in canonical PackID order.
type AgentPackCopyItemResult struct {
	// PackID is the canonical selected pack name.
	PackID string
	// Outcome is copied or noop.
	Outcome AgentPackCopyOutcome
}

// AgentPackInspection is the complete effective pack view for one tenant-local
// agent. BootItems and RevisionItems retain their distinct bodies; Items is
// the deterministic deduplicated composition used by a run.
type AgentPackInspection struct {
	// AgentID is the inspected agent key.
	AgentID string
	// RevisionID is the active durable revision, or empty when absent.
	RevisionID string
	// ContentHash is the active revision's full config hash, or empty when
	// no active revision exists.
	ContentHash string
	// BootItems are the complete frozen boot-layer bodies.
	BootItems []AgentPackLayerItem
	// RevisionItems are the complete active-revision pack bodies.
	RevisionItems []AgentPackLayerItem
	// Items is the strict deduplicated effective view in canonical name order.
	Items []AgentPackEffectiveItem
	// BootPackSetHash identifies the boot layer.
	BootPackSetHash string
	// CompositionHash identifies the deduplicated effective view.
	CompositionHash string
	// RevisionHash identifies the active revision pack layer.
	RevisionHash string

	// payload is the complete active config envelope retained only by the
	// same-runtime copy port. It is intentionally not part of the public
	// inspection projection; copy must preserve sibling config sections.
	payload     agentcfg.ConfigPayload
	hasRevision bool
}

// AgentPackCopyRequest selects source bodies and names the target CAS and
// idempotency expectations. The verified tenant comes from ctx; callers cannot
// supply or widen a tenant through this value.
type AgentPackCopyRequest struct {
	// SourceAgentID is the source runtime agent key.
	SourceAgentID string
	// TargetAgentID is the target runtime agent key.
	TargetAgentID string
	// PackIDs is the selected canonical pack-name set. The set is also the
	// reconciliation set: untouched prior copies from this source are removed.
	PackIDs []string
	// ExpectedSourceCompositionHash binds the source snapshot and is required.
	ExpectedSourceCompositionHash string
	// ExpectedTargetCompositionHash binds the target snapshot and is required.
	ExpectedTargetCompositionHash string
	// ExpectedTargetContentHash optionally binds the target full revision
	// content hash. Use agentcfg.ExpectNoActiveRevision for an absent target.
	ExpectedTargetContentHash string
	// IdempotencyKey names the durable copy operation.
	IdempotencyKey string
}

// AgentPackCopyResult reports the target snapshot after a successful copy.
type AgentPackCopyResult struct {
	// SourceAgentID is the source key from the request.
	SourceAgentID string
	// TargetAgentID is the target key from the request.
	TargetAgentID string
	// Target is the resulting target effective view.
	Target AgentPackInspection
	// SourceRevisionID is the source active revision captured at admission.
	SourceRevisionID string
	// SourceContentHash is the source full config hash captured at admission.
	SourceContentHash string
	// SourceCompositionHash is the source effective pack hash captured at
	// admission.
	SourceCompositionHash string
	// TargetRevisionID is the resulting target active revision.
	TargetRevisionID string
	// TargetContentHash is the resulting target full config hash.
	TargetContentHash string
	// TargetCompositionHash is the resulting target effective pack hash.
	TargetCompositionHash string
	// Outcomes contains one result for every selected pack.
	Outcomes []AgentPackCopyItemResult
	// Changed reports whether a target revision was newly published.
	Changed bool
	// Replayed reports that the durable operation was already completed.
	Replayed bool
}

// InspectEffective reads the verified caller's tenant-local effective pack.
// It performs no admin or agent-reach decision: the Protocol adapter must
// complete those checks before calling this same-runtime port.
func (s *Service) InspectEffective(ctx context.Context, agentID string) (AgentPackInspection, error) {
	caller, err := verifiedPackCaller(ctx)
	if err != nil {
		return AgentPackInspection{}, err
	}
	return s.inspectEffective(ctx, caller, strings.TrimSpace(agentID))
}

// CopySelected copies selected effective source bodies into a target in one
// revision CAS. Equal content is a no-op; independently authored collisions
// fail before publication; server-stamped copies can be reconciled only while
// their target body remains untouched.
func (s *Service) CopySelected(ctx context.Context, req AgentPackCopyRequest) (AgentPackCopyResult, error) {
	caller, err := verifiedPackCaller(ctx)
	if err != nil {
		return AgentPackCopyResult{}, err
	}
	// Cancellation is part of admission: a caller that has already withdrawn
	// the copy must not reserve an idempotency record or publish a revision.
	if err := ctx.Err(); err != nil {
		return AgentPackCopyResult{}, err
	}
	normalized, err := normalizePackCopyRequest(req)
	if err != nil {
		return AgentPackCopyResult{}, err
	}
	if normalized.SourceAgentID == normalized.TargetAgentID {
		return AgentPackCopyResult{}, fmt.Errorf("%w: source and target agents must differ", ErrAgentPackCopyInvalid)
	}
	if s.agentPackProposals == nil {
		return AgentPackCopyResult{}, fmt.Errorf("%w: copy ledger is not wired", ErrAgentPackCopyInvalid)
	}
	registryQ := identity.Quadruple{Identity: caller}
	if err := s.ensureNotRetired(ctx, registryQ, normalized.SourceAgentID); err != nil {
		return AgentPackCopyResult{}, err
	}
	if err := s.ensureNotRetired(ctx, registryQ, normalized.TargetAgentID); err != nil {
		return AgentPackCopyResult{}, err
	}
	ledgerQ, err := agentcfg.AgentScope(caller.TenantID, normalized.TargetAgentID)
	if err != nil {
		return AgentPackCopyResult{}, err
	}
	digest, err := packCopyRequestDigest(normalized)
	if err != nil {
		return AgentPackCopyResult{}, fmt.Errorf("%w: digest request: %w", ErrAgentPackCopyInvalid, err)
	}
	kind := agentPackCopyKind(normalized.IdempotencyKey)
	prior, loadErr := s.agentPackProposals.Load(ctx, ledgerQ, kind)
	if loadErr == nil {
		record, decodeErr := unmarshalPackCopyRecord(prior.Bytes)
		if decodeErr != nil {
			return AgentPackCopyResult{}, fmt.Errorf("%w: decode existing operation: %w", ErrAgentPackCopyReplay, decodeErr)
		}
		if record.Digest != digest {
			return AgentPackCopyResult{}, fmt.Errorf("%w: %w: idempotency key is bound to a different request", ErrAgentPackCopyReplay, ErrAgentPackCopyIdempotencyConflict)
		}
		return s.replayPackCopy(ctx, registryQ, ledgerQ, kind, prior, record)
	}
	if !errors.Is(loadErr, state.ErrNotFound) {
		return AgentPackCopyResult{}, fmt.Errorf("%w: load operation: %w", ErrAgentPackCopyInvalid, loadErr)
	}

	source, err := s.inspectEffective(ctx, caller, normalized.SourceAgentID)
	if err != nil {
		return AgentPackCopyResult{}, err
	}
	target, err := s.inspectEffective(ctx, caller, normalized.TargetAgentID)
	if err != nil {
		return AgentPackCopyResult{}, err
	}
	if err := checkPackCopyExpectations(normalized, source, target); err != nil {
		return AgentPackCopyResult{}, err
	}
	desired, err := reconcileCopiedPack(source, target, normalized.PackIDs, normalized.SourceAgentID, digest)
	if err != nil {
		return AgentPackCopyResult{}, err
	}
	same, err := samePackPayload(target, desired)
	if err != nil {
		return AgentPackCopyResult{}, fmt.Errorf("%w: compare target payload: %w", ErrAgentPackCopyInvalid, err)
	}
	outcomes, err := copyPackOutcomes(source, target, normalized.PackIDs)
	if err != nil {
		return AgentPackCopyResult{}, err
	}
	if same {
		record, err := newPackCopyRecord(normalized, digest, registryQ, source, target, desired, false, outcomes)
		if err != nil {
			return AgentPackCopyResult{}, fmt.Errorf("%w: prepare no-op receipt: %w", ErrAgentPackCopyInvalid, err)
		}
		if err := s.reservePackCopy(ctx, ledgerQ, kind, &record); err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				return s.replayPackCopyAfterRace(ctx, registryQ, ledgerQ, kind, digest)
			}
			return AgentPackCopyResult{}, fmt.Errorf("%w: reserve no-op: %w", ErrAgentPackCopyInvalid, err)
		}
		return packCopyResult(record, target, false, false), nil
	}

	baseHash := agentcfg.ExpectNoActiveRevision
	if target.ContentHash != "" {
		baseHash = target.ContentHash
	}
	record, err := newPackCopyRecord(normalized, digest, registryQ, source, target, desired, true, outcomes)
	if err != nil {
		return AgentPackCopyResult{}, fmt.Errorf("%w: prepare copy receipt: %w", ErrAgentPackCopyInvalid, err)
	}
	record.BaseContentHash = baseHash
	if err := s.reservePackCopy(ctx, ledgerQ, kind, &record); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return s.replayPackCopyAfterRace(ctx, registryQ, ledgerQ, kind, digest)
		}
		return AgentPackCopyResult{}, fmt.Errorf("%w: reserve copy: %w", ErrAgentPackCopyInvalid, err)
	}
	return s.applyPackCopy(ctx, registryQ, ledgerQ, kind, record)
}

func verifiedPackCaller(ctx context.Context) (identity.Identity, error) {
	if ctx == nil {
		return identity.Identity{}, ErrAgentPackInspectionIdentityRequired
	}
	id, ok := identity.FromVerified(ctx)
	if !ok {
		return identity.Identity{}, ErrAgentPackInspectionIdentityRequired
	}
	if err := identity.Validate(id); err != nil {
		return identity.Identity{}, fmt.Errorf("%w: %w", ErrAgentPackInspectionIdentityRequired, err)
	}
	return id, nil
}

func (s *Service) inspectEffective(ctx context.Context, caller identity.Identity, agentID string) (AgentPackInspection, error) {
	if agentID == "" {
		return AgentPackInspection{}, fmt.Errorf("%w: agent id is empty", ErrAgentPackInspectionIdentityRequired)
	}
	q := identity.Quadruple{Identity: caller}
	if err := s.ensureNotRetired(ctx, q, agentID); err != nil {
		return AgentPackInspection{}, err
	}
	var bootEntries []bootpacks.Entry
	if s.agentPackBoot != nil {
		bootEntries, _ = s.agentPackBoot.Lookup(caller.TenantID, agentID)
	}
	revision, set, err := s.registry.Active(ctx, q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return AgentPackInspection{}, err
	}
	var revisionSkills []skills.Skill
	if set {
		revisionSkills, err = skills.PackItemsToSkills(revision.Payload.AgentPacks)
		if err != nil {
			return AgentPackInspection{}, fmt.Errorf("%w: convert active revision pack: %w", ErrAgentPacksInvalid, err)
		}
	}
	tier, err := sessionoverlay.ComposeOperatorTier(bootEntries, revisionSkills)
	if err != nil {
		return AgentPackInspection{}, fmt.Errorf("%w: compose effective pack: %w", ErrAgentPacksInvalid, err)
	}
	out := AgentPackInspection{
		AgentID:         agentID,
		BootPackSetHash: tier.BootPackSetHash(),
		CompositionHash: tier.CombinedHash(),
		RevisionHash:    tier.RevisionHash(),
		BootItems:       layerItemsFromBoot(bootEntries),
		payload:         agentcfg.ConfigPayload{},
		hasRevision:     set,
	}
	if set {
		out.payload = clonePayload(revision.Payload)
		out.RevisionID = revision.RevisionID
		out.ContentHash = revision.ContentHash
		out.RevisionItems, err = layerItemsFromPack(revision.Payload.AgentPacks)
		if err != nil {
			return AgentPackInspection{}, fmt.Errorf("%w: preserve active revision layer: %w", ErrAgentPacksInvalid, err)
		}
	}
	for _, item := range tier.Items() {
		out.Items = append(out.Items, AgentPackEffectiveItem{
			PackID:       skills.CanonicalPackName(item.Skill.Name),
			Item:         skills.PackItemFromSkill(item.Skill),
			Source:       item.Source,
			SemanticHash: item.SemanticHash,
			Editable:     item.Source == skills.OperatorTierSourceRevision,
		})
	}
	return out, nil
}

func layerItemsFromBoot(entries []bootpacks.Entry) []AgentPackLayerItem {
	if len(entries) == 0 {
		return nil
	}
	out := make([]AgentPackLayerItem, 0, len(entries))
	for _, entry := range entries {
		item := skills.PackItemFromSkill(entry.Skill)
		hash := entry.SemanticHash
		if hash == "" {
			hash = skills.CanonicalContentHash(entry.Skill)
		}
		out = append(out, AgentPackLayerItem{PackID: skills.CanonicalPackName(item.Name), Item: item, SemanticHash: hash})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PackID < out[j].PackID })
	return out
}

func layerItemsFromPack(items []skills.AgentPackItem) ([]AgentPackLayerItem, error) {
	if len(items) == 0 {
		return nil, nil
	}
	skillsView, err := skills.PackItemsToSkills(items)
	if err != nil {
		return nil, err
	}
	out := make([]AgentPackLayerItem, 0, len(skillsView))
	for _, skill := range skillsView {
		item := skills.PackItemFromSkill(skill)
		out = append(out, AgentPackLayerItem{PackID: skills.CanonicalPackName(item.Name), Item: item, SemanticHash: skills.CanonicalContentHash(skill)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PackID < out[j].PackID })
	return out, nil
}

func normalizePackCopyRequest(req AgentPackCopyRequest) (AgentPackCopyRequest, error) {
	req.SourceAgentID = strings.TrimSpace(req.SourceAgentID)
	req.TargetAgentID = strings.TrimSpace(req.TargetAgentID)
	req.ExpectedSourceCompositionHash = strings.TrimSpace(req.ExpectedSourceCompositionHash)
	req.ExpectedTargetCompositionHash = strings.TrimSpace(req.ExpectedTargetCompositionHash)
	req.ExpectedTargetContentHash = strings.TrimSpace(req.ExpectedTargetContentHash)
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.SourceAgentID == "" || req.TargetAgentID == "" || req.IdempotencyKey == "" {
		return AgentPackCopyRequest{}, fmt.Errorf("%w: source_agent_id, target_agent_id, and idempotency_key are required", ErrAgentPackCopyInvalid)
	}
	if req.ExpectedSourceCompositionHash == "" || req.ExpectedTargetCompositionHash == "" {
		return AgentPackCopyRequest{}, fmt.Errorf("%w: source and target composition expectations are required", ErrAgentPackCopyInvalid)
	}
	if len([]rune(req.IdempotencyKey)) > 256 {
		return AgentPackCopyRequest{}, fmt.Errorf("%w: idempotency_key exceeds 256 runes", ErrAgentPackCopyInvalid)
	}
	if len(req.PackIDs) > maxAgentPackCopyIDs {
		return AgentPackCopyRequest{}, fmt.Errorf("%w: pack_ids exceeds %d items", ErrAgentPackCopyInvalid, maxAgentPackCopyIDs)
	}
	seen := make(map[string]struct{}, len(req.PackIDs))
	req.PackIDs = append([]string(nil), req.PackIDs...)
	for i, id := range req.PackIDs {
		id = skills.CanonicalPackName(id)
		if id == "" {
			return AgentPackCopyRequest{}, fmt.Errorf("%w: pack_ids contains an empty name", ErrAgentPackCopyInvalid)
		}
		if _, exists := seen[id]; exists {
			return AgentPackCopyRequest{}, fmt.Errorf("%w: pack_ids contains duplicate %q", ErrAgentPackCopyInvalid, id)
		}
		seen[id] = struct{}{}
		req.PackIDs[i] = id
	}
	sort.Strings(req.PackIDs)
	return req, nil
}

func packCopyRequestDigest(req AgentPackCopyRequest) (string, error) {
	canonical := struct {
		Source        string   `json:"source_agent_id"`
		Target        string   `json:"target_agent_id"`
		Packs         []string `json:"pack_ids"`
		SourceHash    string   `json:"source_composition_hash,omitempty"`
		TargetHash    string   `json:"target_composition_hash,omitempty"`
		TargetContent string   `json:"target_content_hash,omitempty"`
	}{req.SourceAgentID, req.TargetAgentID, req.PackIDs, req.ExpectedSourceCompositionHash, req.ExpectedTargetCompositionHash, req.ExpectedTargetContentHash}
	b, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func agentPackCopyKind(key string) string {
	sum := sha256.Sum256([]byte(key))
	return agentPackCopyKindPrefix + hex.EncodeToString(sum[:])
}

func checkPackCopyExpectations(req AgentPackCopyRequest, source, target AgentPackInspection) error {
	if req.ExpectedSourceCompositionHash != "" && source.CompositionHash != req.ExpectedSourceCompositionHash {
		return fmt.Errorf("%w: source expected %q, got %q", ErrAgentPackCopyExpectation, req.ExpectedSourceCompositionHash, source.CompositionHash)
	}
	if req.ExpectedTargetCompositionHash != "" && target.CompositionHash != req.ExpectedTargetCompositionHash {
		return fmt.Errorf("%w: target expected %q, got %q", ErrAgentPackCopyExpectation, req.ExpectedTargetCompositionHash, target.CompositionHash)
	}
	if req.ExpectedTargetContentHash == agentcfg.ExpectNoActiveRevision {
		if target.ContentHash != "" {
			return fmt.Errorf("%w: target expected no active revision, got %q", ErrAgentPackCopyExpectation, target.ContentHash)
		}
	} else if req.ExpectedTargetContentHash != "" && target.ContentHash != req.ExpectedTargetContentHash {
		return fmt.Errorf("%w: target content expected %q, got %q", ErrAgentPackCopyExpectation, req.ExpectedTargetContentHash, target.ContentHash)
	}
	return nil
}

func samePackPayload(target AgentPackInspection, desired agentcfg.ConfigPayload) (bool, error) {
	current := clonePayload(target.payload)
	left, errLeft := agentcfg.ContentHash(current)
	right, errRight := agentcfg.ContentHash(desired)
	if errLeft != nil {
		return false, errLeft
	}
	if errRight != nil {
		return false, errRight
	}
	return left == right, nil
}

func copyPackOutcomes(source, target AgentPackInspection, selected []string) ([]AgentPackCopyItemResult, error) {
	sourceItems := make(map[string]AgentPackEffectiveItem, len(source.Items))
	for _, item := range source.Items {
		sourceItems[item.PackID] = item
	}
	out := make([]AgentPackCopyItemResult, 0, len(selected))
	for _, packID := range selected {
		sourceItem, ok := sourceItems[packID]
		if !ok {
			return nil, fmt.Errorf("%w: source pack %q", ErrAgentPackNotFound, packID)
		}
		outcome := AgentPackCopyOutcomeCopied
		if targetItem, exists := effectiveItemByName(target.Items, packID); exists && targetItem.SemanticHash == sourceItem.SemanticHash {
			outcome = AgentPackCopyOutcomeNoop
		}
		out = append(out, AgentPackCopyItemResult{PackID: packID, Outcome: outcome})
	}
	return out, nil
}

func reconcileCopiedPack(source, target AgentPackInspection, selected []string, sourceAgentID, operationDigest string) (agentcfg.ConfigPayload, error) {
	current := make(map[string]skills.AgentPackItem, len(target.RevisionItems))
	for _, layer := range target.RevisionItems {
		current[layer.PackID] = clonePackItem(layer.Item)
	}
	sourceItems := make(map[string]AgentPackEffectiveItem, len(source.Items))
	for _, item := range source.Items {
		sourceItems[item.PackID] = item
	}
	selectedSet := make(map[string]struct{}, len(selected))
	for _, name := range selected {
		selectedSet[name] = struct{}{}
		item, ok := sourceItems[name]
		if !ok {
			return agentcfg.ConfigPayload{}, fmt.Errorf("%w: source pack %q", ErrAgentPackNotFound, name)
		}
		targetItem, targetEffective := effectiveItemByName(target.Items, name)
		if targetEffective && !targetItem.Editable && targetItem.SemanticHash != item.SemanticHash {
			return agentcfg.ConfigPayload{}, fmt.Errorf("%w: target boot-owned pack %q", ErrBootPackOwned, name)
		}
		if existing, exists := current[name]; exists {
			existingHash := packItemSemanticHash(existing)
			if existingHash == item.SemanticHash {
				continue
			}
			origin, valid := parseCopiedOrigin(existing.OriginRef)
			if !valid || origin.SourceAgentID != sourceAgentID || origin.PackID != name || origin.SemanticHash != existingHash {
				return agentcfg.ConfigPayload{}, fmt.Errorf("%w: target pack %q is independently authored", ErrAgentPackCopyCollision, name)
			}
			current[name] = stampedCopiedItem(item.Item, sourceAgentID, source.RevisionID, name, item.SemanticHash, operationDigest, target.ContentHash)
			continue
		}
		if targetItem, bootExists := effectiveItemByName(target.Items, name); bootExists && !targetItem.Editable {
			if targetItem.SemanticHash == item.SemanticHash {
				continue
			}
			return agentcfg.ConfigPayload{}, fmt.Errorf("%w: target boot-owned pack %q", ErrBootPackOwned, name)
		}
		current[name] = stampedCopiedItem(item.Item, sourceAgentID, source.RevisionID, name, item.SemanticHash, operationDigest, target.ContentHash)
	}
	for name, existing := range current {
		origin, valid := parseCopiedOrigin(existing.OriginRef)
		if !valid || origin.SourceAgentID != sourceAgentID || origin.PackID != name {
			continue
		}
		if _, keep := selectedSet[name]; keep {
			continue
		}
		if origin.SemanticHash != packItemSemanticHash(existing) {
			return agentcfg.ConfigPayload{}, fmt.Errorf("%w: copied target pack %q was modified", ErrAgentPackCopyCollision, name)
		}
		delete(current, name)
	}
	items := make([]skills.AgentPackItem, 0, len(current))
	for _, item := range current {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		return skills.CanonicalPackName(items[i].Name) < skills.CanonicalPackName(items[j].Name)
	})
	desired := clonePayload(target.payload)
	desired.AgentPacks = items
	return desired, nil
}

func effectiveItemByName(items []AgentPackEffectiveItem, name string) (AgentPackEffectiveItem, bool) {
	for _, item := range items {
		if item.PackID == name {
			return item, true
		}
	}
	return AgentPackEffectiveItem{}, false
}

type copiedOrigin struct {
	SourceAgentID    string
	SourceRevisionID string
	PackID           string
	SemanticHash     string
	OperationDigest  string
	TargetHash       string
}

const copiedOriginPrefix = "pack.copied.v1."

func stampedCopiedItem(item skills.AgentPackItem, sourceAgentID, sourceRevisionID, packID, hash, operationDigest, targetHash string) skills.AgentPackItem {
	out := clonePackItem(item)
	out.Name = packID
	if sourceRevisionID == "" {
		sourceRevisionID = "boot"
	}
	if targetHash == "" {
		targetHash = agentcfg.ExpectNoActiveRevision
	}
	out.OriginRef = copiedOriginPrefix + base64.RawURLEncoding.EncodeToString([]byte(sourceAgentID)) + "." + base64.RawURLEncoding.EncodeToString([]byte(sourceRevisionID)) + "." + base64.RawURLEncoding.EncodeToString([]byte(packID)) + "." + hash + "." + operationDigest + "." + targetHash
	return out
}

func parseCopiedOrigin(value string) (copiedOrigin, bool) {
	if !strings.HasPrefix(value, copiedOriginPrefix) {
		return copiedOrigin{}, false
	}
	parts := strings.Split(strings.TrimPrefix(value, copiedOriginPrefix), ".")
	if len(parts) != 6 || len(parts[3]) != 64 || len(parts[4]) != 64 || (len(parts[5]) != 64 && parts[5] != agentcfg.ExpectNoActiveRevision) {
		return copiedOrigin{}, false
	}
	source, errSource := base64.RawURLEncoding.DecodeString(parts[0])
	sourceRevision, errRevision := base64.RawURLEncoding.DecodeString(parts[1])
	packID, errPack := base64.RawURLEncoding.DecodeString(parts[2])
	if errSource != nil || errRevision != nil || errPack != nil || skills.CanonicalPackName(string(packID)) != string(packID) || string(packID) == "" {
		return copiedOrigin{}, false
	}
	if _, err := hex.DecodeString(parts[3]); err != nil {
		return copiedOrigin{}, false
	}
	if _, err := hex.DecodeString(parts[4]); err != nil {
		return copiedOrigin{}, false
	}
	if parts[5] != agentcfg.ExpectNoActiveRevision {
		if _, err := hex.DecodeString(parts[5]); err != nil {
			return copiedOrigin{}, false
		}
	}
	return copiedOrigin{SourceAgentID: string(source), SourceRevisionID: string(sourceRevision), PackID: string(packID), SemanticHash: parts[3], OperationDigest: parts[4], TargetHash: parts[5]}, true
}

func packItemSemanticHash(item skills.AgentPackItem) string {
	skill, err := item.Skill()
	if err != nil {
		return ""
	}
	return skills.CanonicalContentHash(skill)
}

func clonePackItem(item skills.AgentPackItem) skills.AgentPackItem {
	item.Tags = append([]string(nil), item.Tags...)
	item.Steps = append([]string(nil), item.Steps...)
	item.Preconditions = append([]string(nil), item.Preconditions...)
	item.FailureModes = append([]string(nil), item.FailureModes...)
	item.RequiredTools = append([]string(nil), item.RequiredTools...)
	item.RequiredNS = append([]string(nil), item.RequiredNS...)
	item.RequiredTags = append([]string(nil), item.RequiredTags...)
	item.Extra = cloneStringMapCopy(item.Extra)
	return item
}

func clonePayload(payload agentcfg.ConfigPayload) agentcfg.ConfigPayload {
	return agentcfg.NormalizePayload(payload)
}

// agentPackCopyRecord is the durable copy operation receipt. The desired
// payload is stored before publication so a response-loss retry can resume
// the exact CAS operation without recomputing from a moved source.
type agentPackCopyRecord struct {
	Schema                int                       `json:"schema"`
	Digest                string                    `json:"digest"`
	Author                identity.Quadruple        `json:"author"`
	SourceAgentID         string                    `json:"source_agent_id"`
	SourceRevisionID      string                    `json:"source_revision_id,omitempty"`
	SourceContentHash     string                    `json:"source_content_hash,omitempty"`
	SourceCompositionHash string                    `json:"source_composition_hash"`
	TargetAgentID         string                    `json:"target_agent_id"`
	PackIDs               []string                  `json:"pack_ids"`
	Outcomes              []AgentPackCopyItemResult `json:"outcomes"`
	BaseContentHash       string                    `json:"base_content_hash,omitempty"`
	TargetRevisionID      string                    `json:"target_revision_id,omitempty"`
	TargetContentHash     string                    `json:"target_content_hash,omitempty"`
	// The final projection hashes are part of the committed receipt, not a
	// live target precondition. They let an idempotent retry return the exact
	// original result after a later legitimate target edit.
	TargetCompositionHash string                 `json:"target_composition_hash,omitempty"`
	TargetBootPackSetHash string                 `json:"target_boot_pack_set_hash,omitempty"`
	DesiredPayload        agentcfg.ConfigPayload `json:"desired_payload"`
	Changed               bool                   `json:"changed"`
	Phase                 string                 `json:"phase"`
	ReceiptID             string                 `json:"receipt_id"`
}

func newPackCopyRecord(req AgentPackCopyRequest, digest string, author identity.Quadruple, source, target AgentPackInspection, desired agentcfg.ConfigPayload, changed bool, outcomes []AgentPackCopyItemResult) (agentPackCopyRecord, error) {
	targetHash, err := agentcfg.ContentHash(desired)
	if err != nil {
		return agentPackCopyRecord{}, err
	}
	record := agentPackCopyRecord{
		Schema:                agentPackCopySchema,
		Digest:                digest,
		Author:                author,
		SourceAgentID:         req.SourceAgentID,
		SourceRevisionID:      source.RevisionID,
		SourceContentHash:     source.ContentHash,
		SourceCompositionHash: source.CompositionHash,
		TargetAgentID:         req.TargetAgentID,
		PackIDs:               append([]string(nil), req.PackIDs...),
		Outcomes:              append([]AgentPackCopyItemResult(nil), outcomes...),
		TargetRevisionID:      target.RevisionID,
		TargetContentHash:     targetHash,
		DesiredPayload:        clonePayload(desired),
		Changed:               changed,
		Phase:                 "committed",
	}
	if !changed {
		record.TargetCompositionHash = target.CompositionHash
		record.TargetBootPackSetHash = target.BootPackSetHash
		if !target.hasRevision {
			record.TargetRevisionID = ""
			record.TargetContentHash = ""
		}
		return record, nil
	}
	record.Phase = "committing"
	record.TargetRevisionID = string(state.NewEventID())
	return record, nil
}

func marshalPackCopyRecord(record agentPackCopyRecord) ([]byte, error) {
	if record.Schema != agentPackCopySchema || record.Digest == "" || record.SourceAgentID == "" || record.TargetAgentID == "" || record.Phase == "" || record.ReceiptID == "" {
		return nil, fmt.Errorf("%w: incomplete copy receipt", ErrAgentPackCopyInvalid)
	}
	if err := validatePackCopyOutcomes(record.PackIDs, record.Outcomes); err != nil {
		return nil, err
	}
	if err := identity.Validate(record.Author.Identity); err != nil {
		return nil, fmt.Errorf("%w: receipt author identity: %w", ErrAgentPackCopyInvalid, err)
	}
	return json.Marshal(record)
}

func unmarshalPackCopyRecord(raw []byte) (agentPackCopyRecord, error) {
	var record agentPackCopyRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	if record.Schema != agentPackCopySchema || record.Digest == "" || record.SourceAgentID == "" || record.TargetAgentID == "" || (record.Phase != "committing" && record.Phase != "committed") || record.ReceiptID == "" {
		return record, fmt.Errorf("%w: invalid copy receipt", ErrAgentPackCopyReplay)
	}
	if err := validatePackCopyOutcomes(record.PackIDs, record.Outcomes); err != nil {
		return record, fmt.Errorf("%w: %w", ErrAgentPackCopyReplay, err)
	}
	if err := identity.Validate(record.Author.Identity); err != nil {
		return record, fmt.Errorf("%w: receipt author identity: %w", ErrAgentPackCopyReplay, err)
	}
	return record, nil
}

func validatePackCopyOutcomes(packIDs []string, outcomes []AgentPackCopyItemResult) error {
	if len(packIDs) != len(outcomes) {
		return fmt.Errorf("%w: receipt outcome count does not match selected packs", ErrAgentPackCopyInvalid)
	}
	for i, outcome := range outcomes {
		if outcome.PackID != packIDs[i] || (outcome.Outcome != AgentPackCopyOutcomeCopied && outcome.Outcome != AgentPackCopyOutcomeNoop) {
			return fmt.Errorf("%w: invalid receipt outcome for pack %q", ErrAgentPackCopyInvalid, outcome.PackID)
		}
	}
	return nil
}

func (s *Service) reservePackCopy(ctx context.Context, q identity.Quadruple, kind string, record *agentPackCopyRecord) error {
	recordID := state.NewEventID()
	record.ReceiptID = string(recordID)
	raw, err := marshalPackCopyRecord(*record)
	if err != nil {
		return err
	}
	return s.agentPackProposals.SaveIf(ctx,
		[]state.SlotExpectation{{Identity: q, Kind: kind}},
		state.StateRecord{ID: recordID, Identity: q, Kind: kind, Bytes: raw})
}

func (s *Service) finalizePackCopy(ctx context.Context, q identity.Quadruple, kind string, prior state.StateRecord, record agentPackCopyRecord) error {
	record.Phase = "committed"
	recordID := state.NewEventID()
	record.ReceiptID = string(recordID)
	raw, err := marshalPackCopyRecord(record)
	if err != nil {
		return err
	}
	if err := s.agentPackProposals.SaveIf(ctx,
		[]state.SlotExpectation{{Identity: q, Kind: kind, ExpectedEventID: prior.ID}},
		state.StateRecord{ID: recordID, Identity: q, Kind: kind, Bytes: raw}); err != nil {
		return fmt.Errorf("%w: finalize copy receipt: %w", ErrAgentPackCopyInvalid, err)
	}
	return nil
}

func (s *Service) replayPackCopyAfterRace(ctx context.Context, registryQ, ledgerQ identity.Quadruple, kind, digest string) (AgentPackCopyResult, error) {
	prior, err := s.agentPackProposals.Load(ctx, ledgerQ, kind)
	if err != nil {
		return AgentPackCopyResult{}, fmt.Errorf("%w: load raced copy receipt: %w", ErrAgentPackCopyReplay, err)
	}
	record, err := unmarshalPackCopyRecord(prior.Bytes)
	if err != nil || record.Digest != digest {
		return AgentPackCopyResult{}, fmt.Errorf("%w: %w: raced idempotency key mismatch", ErrAgentPackCopyReplay, ErrAgentPackCopyIdempotencyConflict)
	}
	return s.replayPackCopy(ctx, registryQ, ledgerQ, kind, prior, record)
}

func (s *Service) replayPackCopy(ctx context.Context, registryQ, ledgerQ identity.Quadruple, kind string, prior state.StateRecord, record agentPackCopyRecord) (AgentPackCopyResult, error) {
	if record.Phase == "committed" {
		// A committed key is a receipt lookup, not a fresh target read. The
		// target may have been edited legitimately after the original commit;
		// returning live state here would turn an idempotent retry into a
		// conflict and would change the response after response loss.
		if err := s.hydratePackCopyReceiptHashes(registryQ.Identity, &record); err != nil {
			return AgentPackCopyResult{}, err
		}
		return packCopyReceiptResult(record, true), nil
	}
	target, err := s.inspectEffective(ctx, registryQ.Identity, record.TargetAgentID)
	if err != nil {
		return AgentPackCopyResult{}, err
	}
	if target.RevisionID == record.TargetRevisionID && target.ContentHash == record.TargetContentHash {
		record.TargetCompositionHash = target.CompositionHash
		record.TargetBootPackSetHash = target.BootPackSetHash
		if err := s.finalizePackCopy(ctx, ledgerQ, kind, prior, record); err != nil {
			return AgentPackCopyResult{}, err
		}
		return packCopyResult(record, target, true, true), nil
	}
	if target.ContentHash != "" && target.ContentHash != record.BaseContentHash {
		return AgentPackCopyResult{}, fmt.Errorf("%w: target changed while copy was committing", ErrAgentPackCopyReplay)
	}
	return s.applyPackCopy(ctx, registryQ, ledgerQ, kind, record)
}

// hydratePackCopyReceiptHashes fills fields absent from a pre-hash receipt
// using only its durable desired payload and immutable boot baseline. It never
// reads the mutable active target, so legacy committed receipts preserve the
// same replay semantics as new receipts.
func (s *Service) hydratePackCopyReceiptHashes(caller identity.Identity, record *agentPackCopyRecord) error {
	if record.TargetCompositionHash != "" && record.TargetBootPackSetHash != "" {
		return nil
	}
	var bootEntries []bootpacks.Entry
	if s.agentPackBoot != nil {
		bootEntries, _ = s.agentPackBoot.Lookup(caller.TenantID, record.TargetAgentID)
	}
	revisionSkills, err := skills.PackItemsToSkills(record.DesiredPayload.AgentPacks)
	if err != nil {
		return fmt.Errorf("%w: hydrate committed receipt payload: %w", ErrAgentPackCopyReplay, err)
	}
	tier, err := sessionoverlay.ComposeOperatorTier(bootEntries, revisionSkills)
	if err != nil {
		return fmt.Errorf("%w: hydrate committed receipt composition: %w", ErrAgentPackCopyReplay, err)
	}
	if record.TargetCompositionHash == "" {
		record.TargetCompositionHash = tier.CombinedHash()
	}
	if record.TargetBootPackSetHash == "" {
		record.TargetBootPackSetHash = tier.BootPackSetHash()
	}
	return nil
}

func (s *Service) applyPackCopy(ctx context.Context, registryQ, ledgerQ identity.Quadruple, kind string, record agentPackCopyRecord) (AgentPackCopyResult, error) {
	if record.Phase != "committing" || record.TargetRevisionID == "" {
		return AgentPackCopyResult{}, fmt.Errorf("%w: copy receipt is not publishable", ErrAgentPackCopyReplay)
	}
	_, err := s.registry.SetRevision(ctx, record.Author, record.TargetAgentID, agentcfg.ConfigScopeAgent, clonePayload(record.DesiredPayload), agentcfg.SetOptions{
		ExpectedContentHash: record.BaseContentHash,
		TargetRevisionID:    record.TargetRevisionID,
		PublicationFence: &agentcfg.PublicationFence{
			Identity: ledgerQ,
			Kind:     kind,
			EventID:  record.ReceiptID,
		},
	})
	if err != nil {
		return AgentPackCopyResult{}, err
	}
	target, err := s.inspectEffective(ctx, registryQ.Identity, record.TargetAgentID)
	if err != nil {
		return AgentPackCopyResult{}, err
	}
	if target.RevisionID != record.TargetRevisionID {
		return AgentPackCopyResult{}, fmt.Errorf("%w: published target revision changed", ErrAgentPackCopyReplay)
	}
	prior, err := s.agentPackProposals.Load(ctx, ledgerQ, kind)
	if err != nil {
		return AgentPackCopyResult{}, fmt.Errorf("%w: load committing receipt: %w", ErrAgentPackCopyInvalid, err)
	}
	record.TargetContentHash = target.ContentHash
	record.TargetCompositionHash = target.CompositionHash
	record.TargetBootPackSetHash = target.BootPackSetHash
	if err := s.finalizePackCopy(ctx, ledgerQ, kind, prior, record); err != nil {
		return AgentPackCopyResult{}, err
	}
	return packCopyResult(record, target, true, false), nil
}

func packCopyReceiptResult(record agentPackCopyRecord, replayed bool) AgentPackCopyResult {
	target := AgentPackInspection{
		AgentID:         record.TargetAgentID,
		RevisionID:      record.TargetRevisionID,
		ContentHash:     record.TargetContentHash,
		BootPackSetHash: record.TargetBootPackSetHash,
		CompositionHash: record.TargetCompositionHash,
		hasRevision:     record.TargetRevisionID != "" || record.TargetContentHash != "",
	}
	return packCopyResult(record, target, record.Changed, replayed)
}

func packCopyResult(record agentPackCopyRecord, target AgentPackInspection, changed, replayed bool) AgentPackCopyResult {
	return AgentPackCopyResult{
		SourceAgentID:         record.SourceAgentID,
		TargetAgentID:         record.TargetAgentID,
		Target:                target,
		SourceRevisionID:      record.SourceRevisionID,
		SourceContentHash:     record.SourceContentHash,
		SourceCompositionHash: record.SourceCompositionHash,
		TargetRevisionID:      target.RevisionID,
		TargetContentHash:     target.ContentHash,
		TargetCompositionHash: target.CompositionHash,
		Outcomes:              append([]AgentPackCopyItemResult(nil), record.Outcomes...),
		Changed:               changed,
		Replayed:              replayed,
	}
}
