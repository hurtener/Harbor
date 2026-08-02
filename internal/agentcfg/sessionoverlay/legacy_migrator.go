package sessionoverlay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

var (
	// ErrLegacyCopyConflict means an owned personal record already occupies a
	// legacy reference's exact target but is not the exact live copy for the
	// declared epoch and legacy content hash. Migration never overwrites that
	// record, including when it is a logical tombstone.
	ErrLegacyCopyConflict = errors.New("agentcfg/sessionoverlay: legacy copy conflicts with owned record")
	// ErrLegacySkillInvalid means a legacy overlay reference resolved to a
	// malformed, non-session, or differently named Skill body.
	ErrLegacySkillInvalid = errors.New("agentcfg/sessionoverlay: legacy skill body invalid")
)

// ExactLegacyMigrator copies schema-1 overlay references from the exact
// ScopeSession SkillStore rung into agent-owned personal records. It is
// immutable after construction and safe for concurrent reuse.
type ExactLegacyMigrator struct {
	reader   skills.SkillReader
	personal *DurableStore
}

// NewExactLegacyMigrator constructs the production legacy-copy bridge.
func NewExactLegacyMigrator(reader skills.SkillReader, personal *DurableStore) (*ExactLegacyMigrator, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: skills.SkillReader is required", ErrInvalidConfig)
	}
	if personal == nil || personal.state == nil {
		return nil, fmt.Errorf("%w: durable personal store is required", ErrInvalidConfig)
	}
	return &ExactLegacyMigrator{reader: reader, personal: personal}, nil
}

// CopyLegacyOverlay copies every currently eligible exact ScopeSession body
// named by one strictly decoded schema-1 overlay. The returned count is the
// number of canonical references resolved by an exact current-epoch copy, or
// by a durable terminal lifecycle/erasure fence. A retry therefore returns the
// same count without writing the exact copy again.
func (m *ExactLegacyMigrator) CopyLegacyOverlay(ctx context.Context, candidate state.StateRecord, declaration config.SessionPersonalCutoverTenant) (int, error) {
	decoded, err := decodeLegacyCandidate(candidate, declaration)
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	canonicalNames, err := canonicalLegacyNames(decoded.overlay.PersonalSkills)
	if err != nil {
		return 0, err
	}
	if len(canonicalNames) == 0 {
		return 0, nil
	}

	before, err := loadFences(ctx, m.personal.state, candidate.Identity, decoded.agentID)
	if err != nil {
		return 0, err
	}
	terminal, err := terminalLegacyFences(before)
	if err != nil {
		return 0, err
	}
	if terminal {
		return len(canonicalNames), nil
	}

	bodies, err := m.loadLegacyBodies(ctx, candidate.Identity, decoded.overlay.PersonalSkills)
	if err != nil {
		return 0, err
	}
	resolved := 0
	for _, body := range bodies {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		existing, found, loadErr := m.personal.LoadPersonal(ctx, candidate.Identity, decoded.agentID, body.canonicalName)
		if loadErr != nil {
			if errors.Is(loadErr, ErrSessionErased) || errors.Is(loadErr, agentcfg.ErrAgentRetired) {
				return len(canonicalNames), nil
			}
			return 0, loadErr
		}
		if found {
			if err := validateExactLegacyCopy(existing, declaration.Epoch, body.hash); err != nil {
				return 0, err
			}
			resolved++
			continue
		}

		_, saveErr := m.personal.SavePersonal(ctx, candidate.Identity, decoded.agentID, body.skill, declaration.Epoch, body.hash)
		if saveErr == nil {
			resolved++
			continue
		}
		if errors.Is(saveErr, ErrSessionErased) || errors.Is(saveErr, agentcfg.ErrAgentRetired) {
			return len(canonicalNames), nil
		}
		if !errors.Is(saveErr, state.ErrConditionFailed) {
			return 0, saveErr
		}
		// A concurrent exact copy is success. Any other target winner is a
		// loud conflict; it must never be overwritten by a retry.
		existing, found, loadErr = m.personal.LoadPersonal(ctx, candidate.Identity, decoded.agentID, body.canonicalName)
		if loadErr != nil {
			if errors.Is(loadErr, ErrSessionErased) || errors.Is(loadErr, agentcfg.ErrAgentRetired) {
				return len(canonicalNames), nil
			}
			return 0, loadErr
		}
		if !found {
			return 0, fmt.Errorf("%w: condition failed without a durable target for %q", ErrLegacyCopyConflict, body.canonicalName)
		}
		if err := validateExactLegacyCopy(existing, declaration.Epoch, body.hash); err != nil {
			return 0, err
		}
		resolved++
	}

	after, err := loadFences(ctx, m.personal.state, candidate.Identity, decoded.agentID)
	if err != nil {
		return 0, err
	}
	terminal, err = terminalLegacyFences(after)
	if err != nil {
		return 0, err
	}
	if terminal {
		return len(canonicalNames), nil
	}
	if !before.equal(after) {
		return 0, fmt.Errorf("%w: lifecycle or erasure fence changed during legacy copy", state.ErrConditionFailed)
	}
	return resolved, nil
}

// VerifyLegacyOverlay proves every currently eligible exact ScopeSession body
// has the declared epoch/hash marker, or that the whole overlay is durably
// terminal. It never repairs or mutates state.
func (m *ExactLegacyMigrator) VerifyLegacyOverlay(ctx context.Context, candidate state.StateRecord, declaration config.SessionPersonalCutoverTenant) (bool, error) {
	decoded, err := decodeLegacyCandidate(candidate, declaration)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := canonicalLegacyNames(decoded.overlay.PersonalSkills); err != nil {
		return false, err
	}

	before, err := loadFences(ctx, m.personal.state, candidate.Identity, decoded.agentID)
	if err != nil {
		return false, err
	}
	terminal, err := terminalLegacyFences(before)
	if err != nil {
		return false, err
	}
	if terminal {
		return true, nil
	}
	bodies, err := m.loadLegacyBodies(ctx, candidate.Identity, decoded.overlay.PersonalSkills)
	if err != nil {
		return false, err
	}
	for _, body := range bodies {
		record, found, loadErr := m.personal.LoadPersonal(ctx, candidate.Identity, decoded.agentID, body.canonicalName)
		if loadErr != nil {
			if errors.Is(loadErr, ErrSessionErased) || errors.Is(loadErr, agentcfg.ErrAgentRetired) {
				return true, nil
			}
			return false, loadErr
		}
		if !found {
			return false, nil
		}
		if err := validateExactLegacyCopy(record, declaration.Epoch, body.hash); err != nil {
			return false, err
		}
	}

	after, err := loadFences(ctx, m.personal.state, candidate.Identity, decoded.agentID)
	if err != nil {
		return false, err
	}
	terminal, err = terminalLegacyFences(after)
	if err != nil {
		return false, err
	}
	if terminal {
		return true, nil
	}
	return before.equal(after), nil
}

type decodedLegacyOverlay struct {
	agentID string
	overlay Overlay
}

func decodeLegacyCandidate(candidate state.StateRecord, declaration config.SessionPersonalCutoverTenant) (decodedLegacyOverlay, error) {
	if !declaration.LegacyWritersDrained || !validCutoverToken(declaration.TenantID, 128) || !validCutoverToken(declaration.Epoch, MaxSessionPersonalCopyEpochBytes) || !validCutoverToken(declaration.RosterDigest, 256) || candidate.Identity.TenantID != declaration.TenantID {
		return decodedLegacyOverlay{}, fmt.Errorf("%w: candidate is outside the drained declaration", ErrLegacyOverlayInvalid)
	}
	if err := validateLegacyOverlayCandidate(candidate, declaration.TenantID); err != nil {
		return decodedLegacyOverlay{}, err
	}
	if err := rejectDuplicateJSONObjectFields(candidate.Bytes); err != nil {
		return decodedLegacyOverlay{}, fmt.Errorf("%w: duplicate envelope field: %w", ErrLegacyOverlayInvalid, err)
	}
	var envelope struct {
		Schema    int             `json:"schema"`
		Overlay   json.RawMessage `json:"overlay"`
		UpdatedAt *time.Time      `json:"updated_at"`
	}
	if err := decodeStrictJSON(candidate.Bytes, &envelope); err != nil {
		return decodedLegacyOverlay{}, fmt.Errorf("%w: decode schema-1 envelope: %w", ErrLegacyOverlayInvalid, err)
	}
	if err := rejectDuplicateJSONObjectFields(envelope.Overlay); err != nil {
		return decodedLegacyOverlay{}, fmt.Errorf("%w: duplicate overlay field: %w", ErrLegacyOverlayInvalid, err)
	}
	var overlay Overlay
	if err := decodeStrictJSON(envelope.Overlay, &overlay); err != nil {
		return decodedLegacyOverlay{}, fmt.Errorf("%w: decode overlay body: %w", ErrLegacyOverlayInvalid, err)
	}
	agentID := candidate.Kind[len(legacyKindPrefix):]
	return decodedLegacyOverlay{agentID: agentID, overlay: overlay}, nil
}

type legacyBody struct {
	canonicalName string
	hash          string
	skill         skills.Skill
}

func (m *ExactLegacyMigrator) loadLegacyBodies(ctx context.Context, id identity.Quadruple, names []string) ([]legacyBody, error) {
	byName := make(map[string]legacyBody, len(names))
	order := make([]string, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		canonicalName := canonicalNameFor(name)
		skill, err := m.reader.GetScope(ctx, id, name, skills.ScopeSession)
		if errors.Is(err, skills.ErrSkillNotFound) {
			return nil, fmt.Errorf("%w: exact ScopeSession body missing for reference %q: %w", ErrLegacySkillInvalid, name, err)
		}
		if err != nil {
			return nil, fmt.Errorf("agentcfg/sessionoverlay: exact legacy skill read %q: %w", name, err)
		}
		if err := skill.Validate(); err != nil || skill.Scope != skills.ScopeSession || canonicalNameFor(skill.Name) != canonicalName {
			return nil, fmt.Errorf("%w: reference=%q", ErrLegacySkillInvalid, name)
		}
		hash := skills.CanonicalContentHash(skill)
		if !validCanonicalSHA256(skill.ContentHash) || skill.ContentHash != hash {
			return nil, fmt.Errorf("%w: reference=%q has a non-canonical content hash", ErrLegacySkillInvalid, name)
		}
		if prior, exists := byName[canonicalName]; exists {
			if prior.hash != hash {
				return nil, fmt.Errorf("%w: canonical aliases for %q have different bodies", ErrLegacyCopyConflict, canonicalName)
			}
			continue
		}
		byName[canonicalName] = legacyBody{canonicalName: canonicalName, hash: hash, skill: skill}
		order = append(order, canonicalName)
	}
	result := make([]legacyBody, 0, len(order))
	for _, name := range order {
		result = append(result, byName[name])
	}
	return result, nil
}

func canonicalLegacyNames(names []string) ([]string, error) {
	seen := make(map[string]struct{}, len(names))
	result := make([]string, 0, len(names))
	for _, name := range names {
		canonicalName := canonicalNameFor(name)
		if canonicalName == "" {
			return nil, fmt.Errorf("%w: personal skill name is empty after canonicalization", ErrLegacyOverlayInvalid)
		}
		if _, exists := seen[canonicalName]; exists {
			continue
		}
		seen[canonicalName] = struct{}{}
		result = append(result, canonicalName)
	}
	return result, nil
}

func validateExactLegacyCopy(record PersonalSkillRecord, epoch, legacyHash string) error {
	if record.Deleted {
		return fmt.Errorf("%w: target %q is tombstoned", ErrLegacyCopyConflict, record.CanonicalName)
	}
	if record.CopyEpoch != epoch || record.LegacyContentHash != legacyHash || record.ContentHash != legacyHash {
		return fmt.Errorf("%w: target %q has epoch/hash mismatch", ErrLegacyCopyConflict, record.CanonicalName)
	}
	return nil
}

func terminalLegacyFences(f fences) (bool, error) {
	if f.erased() {
		return true, nil
	}
	switch f.state {
	case lifecycleEnvelopeActive:
		return false, nil
	case lifecycleEnvelopeTerminal:
		return true, nil
	case lifecycleEnvelopeMissing:
		return false, ErrAgentLifecycleInactive
	case lifecycleEnvelopeInvalid:
		return false, ErrAgentLifecycleCorrupt
	default:
		return false, fmt.Errorf("%w: unknown lifecycle classification %d", ErrAgentLifecycleCorrupt, f.state)
	}
}
