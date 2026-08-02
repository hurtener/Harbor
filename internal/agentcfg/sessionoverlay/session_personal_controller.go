package sessionoverlay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

// SessionPersonalController is the production read and mutation authority for
// one selected agent's session-personal skill tier. It is immutable after
// construction and safe for concurrent reuse.
type SessionPersonalController struct {
	personal *DurableStore
	cutover  CutoverModeReader
	legacy   skills.SkillReader
}

// NewSessionPersonalController constructs the session-personal authority.
// The legacy reader remains mandatory in state-only mode so a mode change does
// not require rebuilding the production dependency graph.
func NewSessionPersonalController(personal *DurableStore, cutover CutoverModeReader, legacy skills.SkillReader) (*SessionPersonalController, error) {
	if personal == nil || personal.state == nil {
		return nil, fmt.Errorf("%w: durable personal store is required", ErrInvalidConfig)
	}
	if cutover == nil {
		return nil, fmt.Errorf("%w: cutover mode reader is required", ErrInvalidConfig)
	}
	if legacy == nil {
		return nil, fmt.Errorf("%w: legacy skill reader is required", ErrInvalidConfig)
	}
	return &SessionPersonalController{personal: personal, cutover: cutover, legacy: legacy}, nil
}

// SessionSkills returns only the selected agent's ScopeSession tier. The full
// caller triple is the storage principal; RunID is deliberately zeroed at the
// durable boundary and never becomes a fourth storage authority.
func (c *SessionPersonalController) SessionSkills(ctx context.Context, id identity.Quadruple, agentID string) ([]skills.Skill, error) {
	if err := c.validate(ctx, id, agentID); err != nil {
		return nil, err
	}
	for range MaxSessionSkillReadAttempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		before, err := loadFences(ctx, c.personal.state, id, agentID)
		if err != nil {
			return nil, err
		}
		if err := controllerFenceError(before); err != nil {
			return nil, err
		}
		mode, err := c.cutover.Mode(ctx, id.TenantID)
		if err != nil {
			return nil, err
		}
		var result []skills.Skill
		switch mode {
		case CutoverDualRead:
			result, err = c.loadLegacyTier(ctx, id, agentID)
		case CutoverStateOnly:
			result, err = c.loadOwnedTier(ctx, id, agentID)
		default:
			return nil, fmt.Errorf("%w: unknown cutover mode %q", ErrCutoverRecordInvalid, mode)
		}
		if err != nil {
			return nil, err
		}
		after, err := loadFences(ctx, c.personal.state, id, agentID)
		if err != nil {
			return nil, err
		}
		if err := controllerFenceError(after); err != nil {
			return nil, err
		}
		if before.equal(after) {
			return result, nil
		}
	}
	return nil, ErrSessionSkillReadUnstable
}

// UpsertSessionSkill writes one live agent-owned session record after the
// durable tenant cutover reaches state-only. It never stamps migration data.
func (c *SessionPersonalController) UpsertSessionSkill(ctx context.Context, id identity.Quadruple, agentID string, skill skills.Skill) error {
	if err := c.validate(ctx, id, agentID); err != nil {
		return err
	}
	if err := skill.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if skill.Scope != skills.ScopeSession {
		return fmt.Errorf("%w: session personal skill scope must be %q", ErrInvalidInput, skills.ScopeSession)
	}
	if err := c.requireStateOnly(ctx, id, agentID); err != nil {
		return err
	}
	_, err := c.personal.SavePersonal(ctx, id, agentID, skill, "", "")
	return err
}

// DeleteSessionSkill writes one logical tombstone after the durable tenant
// cutover reaches state-only. It never touches a legacy SkillStore row.
func (c *SessionPersonalController) DeleteSessionSkill(ctx context.Context, id identity.Quadruple, agentID, name string) error {
	if err := c.validate(ctx, id, agentID); err != nil {
		return err
	}
	if canonicalNameFor(name) == "" {
		return fmt.Errorf("%w: session personal skill name is empty", ErrInvalidInput)
	}
	if err := c.requireStateOnly(ctx, id, agentID); err != nil {
		return err
	}
	_, err := c.personal.DeletePersonal(ctx, id, agentID, name)
	return err
}

func (c *SessionPersonalController) validate(ctx context.Context, id identity.Quadruple, agentID string) error {
	if c == nil || c.personal == nil || c.personal.state == nil || c.cutover == nil || c.legacy == nil {
		return fmt.Errorf("%w: controller is not initialized", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return validateSessionInput(id, agentID)
}

func (c *SessionPersonalController) requireStateOnly(ctx context.Context, id identity.Quadruple, agentID string) error {
	fences, err := loadFences(ctx, c.personal.state, id, agentID)
	if err != nil {
		return err
	}
	if err := controllerFenceError(fences); err != nil {
		return err
	}
	mode, err := c.cutover.Mode(ctx, id.TenantID)
	if err != nil {
		return err
	}
	switch mode {
	case CutoverStateOnly:
		return nil
	case CutoverDualRead:
		return ErrCutoverPending
	default:
		return fmt.Errorf("%w: unknown cutover mode %q", ErrCutoverRecordInvalid, mode)
	}
}

func controllerFenceError(f fences) error {
	if f.erased() {
		return ErrSessionErased
	}
	return f.lifecycleError()
}

func (c *SessionPersonalController) loadLegacyTier(ctx context.Context, id identity.Quadruple, agentID string) ([]skills.Skill, error) {
	q := durableSessionQuad(id)
	kind := LegacyOverlayKind(agentID)
	record, err := c.personal.state.Load(ctx, q, kind)
	if errors.Is(err, state.ErrNotFound) {
		return []skills.Skill{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: load legacy overlay: %w", ErrStateUnavailable, err)
	}
	if record.Identity != q || record.Kind != kind {
		return nil, fmt.Errorf("%w: legacy slot does not match selected agent and identity", ErrLegacyOverlayInvalid)
	}
	overlay, err := decodeOverlayRecord(record.Bytes)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]skills.Skill, len(overlay.PersonalSkills))
	for _, name := range overlay.PersonalSkills {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		canonicalName := canonicalNameFor(name)
		skill, err := c.legacy.GetScope(ctx, q, name, skills.ScopeSession)
		if err != nil {
			return nil, fmt.Errorf("%w: exact legacy session skill %q: %w", ErrLegacySkillInvalid, name, err)
		}
		if err := validateControllerLegacySkill(skill, canonicalName); err != nil {
			return nil, err
		}
		cloned, err := cloneControllerSkill(skill)
		if err != nil {
			return nil, fmt.Errorf("%w: clone exact legacy body %q: %w", ErrLegacySkillInvalid, name, err)
		}
		if prior, found := byName[canonicalName]; found && skills.CanonicalContentHash(prior) != skills.CanonicalContentHash(cloned) {
			return nil, fmt.Errorf("%w: canonical legacy aliases for %q disagree", ErrLegacySkillInvalid, canonicalName)
		}
		byName[canonicalName] = cloned
	}
	return sortedControllerSkills(byName), nil
}

func (c *SessionPersonalController) loadOwnedTier(ctx context.Context, id identity.Quadruple, agentID string) ([]skills.Skill, error) {
	prefix, err := PersonalSkillPrefix(agentID)
	if err != nil {
		return nil, err
	}
	q := durableSessionQuad(id)
	records, err := c.personal.state.ListKindForIdentity(ctx, q, prefix)
	if err != nil {
		return nil, fmt.Errorf("%w: list owned personal records: %w", ErrStateUnavailable, err)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Kind < records[j].Kind })
	byName := make(map[string]skills.Skill, len(records))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if record.Identity != q {
			return nil, fmt.Errorf("%w: owned record identity mismatch", ErrPersonalRecordInvalid)
		}
		personal, err := decodeControllerPersonal(record, agentID)
		if err != nil {
			return nil, err
		}
		if personal.Deleted {
			delete(byName, personal.CanonicalName)
			continue
		}
		if _, exists := byName[personal.CanonicalName]; exists {
			return nil, fmt.Errorf("%w: duplicate owned personal name %q", ErrPersonalRecordInvalid, personal.CanonicalName)
		}
		cloned, err := cloneControllerSkill(personal.Skill)
		if err != nil {
			return nil, fmt.Errorf("%w: clone owned body %q: %w", ErrPersonalRecordInvalid, personal.CanonicalName, err)
		}
		byName[personal.CanonicalName] = cloned
	}
	return sortedControllerSkills(byName), nil
}

func decodeControllerPersonal(record state.StateRecord, agentID string) (PersonalSkillRecord, error) {
	if err := rejectDuplicateJSONObjectFields(record.Bytes); err != nil {
		return PersonalSkillRecord{}, fmt.Errorf("%w: duplicate owned record field: %w", ErrPersonalRecordInvalid, err)
	}
	var envelope struct {
		CanonicalName *string `json:"canonical_name"`
	}
	if err := json.Unmarshal(record.Bytes, &envelope); err != nil {
		return PersonalSkillRecord{}, fmt.Errorf("%w: read canonical name: %w", ErrPersonalRecordInvalid, err)
	}
	if envelope.CanonicalName == nil {
		return PersonalSkillRecord{}, fmt.Errorf("%w: canonical name is absent", ErrPersonalRecordInvalid)
	}
	decoded, found, err := decodePersonal(record.Bytes, agentID, *envelope.CanonicalName)
	if err != nil {
		return PersonalSkillRecord{}, err
	}
	if !found {
		return PersonalSkillRecord{}, fmt.Errorf("%w: owned record is absent", ErrPersonalRecordInvalid)
	}
	wantKind, err := PersonalSkillKind(agentID, decoded.CanonicalName)
	if err != nil {
		return PersonalSkillRecord{}, fmt.Errorf("%w: derive exact personal key: %w", ErrPersonalRecordInvalid, err)
	}
	if record.Kind != wantKind {
		return PersonalSkillRecord{}, fmt.Errorf("%w: key does not match payload", ErrPersonalRecordInvalid)
	}
	return decoded, nil
}

func validateControllerLegacySkill(skill skills.Skill, canonicalName string) error {
	if err := skill.Validate(); err != nil || skill.Scope != skills.ScopeSession || canonicalNameFor(skill.Name) != canonicalName {
		return fmt.Errorf("%w: exact ScopeSession body %q is invalid", ErrLegacySkillInvalid, canonicalName)
	}
	wantHash := skills.CanonicalContentHash(skill)
	if !validCanonicalSHA256(skill.ContentHash) || skill.ContentHash != wantHash {
		return fmt.Errorf("%w: exact ScopeSession body %q has a non-canonical content hash", ErrLegacySkillInvalid, canonicalName)
	}
	return nil
}

func sortedControllerSkills(byName map[string]skills.Skill) []skills.Skill {
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]skills.Skill, 0, len(names))
	for _, name := range names {
		result = append(result, byName[name])
	}
	return result
}

func cloneControllerSkill(skill skills.Skill) (skills.Skill, error) {
	result := skill
	result.Tags = append([]string(nil), skill.Tags...)
	result.Steps = append([]string(nil), skill.Steps...)
	result.Preconditions = append([]string(nil), skill.Preconditions...)
	result.FailureModes = append([]string(nil), skill.FailureModes...)
	result.RequiredTools = append([]string(nil), skill.RequiredTools...)
	result.RequiredNS = append([]string(nil), skill.RequiredNS...)
	result.RequiredTags = append([]string(nil), skill.RequiredTags...)
	if skill.Extra == nil {
		return result, nil
	}
	cloned, err := cloneControllerValue(reflect.ValueOf(skill.Extra), make(map[controllerCloneVisit]bool))
	if err != nil {
		return skills.Skill{}, err
	}
	extra, ok := cloned.Interface().(map[string]any)
	if !ok {
		return skills.Skill{}, errors.New("cloned Extra has an incompatible type")
	}
	result.Extra = extra
	return result, nil
}

type controllerCloneVisit struct {
	typ reflect.Type
	ptr uintptr
}

func cloneControllerValue(value reflect.Value, active map[controllerCloneVisit]bool) (reflect.Value, error) {
	if !value.IsValid() {
		return value, nil
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		cloned, err := cloneControllerValue(value.Elem(), active)
		if err != nil {
			return reflect.Value{}, err
		}
		result := reflect.New(value.Type()).Elem()
		result.Set(cloned)
		return result, nil
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		if value.Type().Key().Kind() != reflect.String {
			return reflect.Value{}, errors.New("extra maps must use string keys")
		}
		visit := controllerCloneVisit{typ: value.Type(), ptr: value.Pointer()}
		if active[visit] {
			return reflect.Value{}, errors.New("extra contains a map cycle")
		}
		active[visit] = true
		defer delete(active, visit)
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			cloned, err := cloneControllerValue(iter.Value(), active)
			if err != nil {
				return reflect.Value{}, err
			}
			result.SetMapIndex(iter.Key(), cloned)
		}
		return result, nil
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		visit := controllerCloneVisit{typ: value.Type(), ptr: value.Pointer()}
		if active[visit] {
			return reflect.Value{}, errors.New("extra contains a slice cycle")
		}
		active[visit] = true
		defer delete(active, visit)
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := range value.Len() {
			cloned, err := cloneControllerValue(value.Index(i), active)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(i).Set(cloned)
		}
		return result, nil
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for i := range value.Len() {
			cloned, err := cloneControllerValue(value.Index(i), active)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(i).Set(cloned)
		}
		return result, nil
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		visit := controllerCloneVisit{typ: value.Type(), ptr: value.Pointer()}
		if active[visit] {
			return reflect.Value{}, errors.New("extra contains a pointer cycle")
		}
		active[visit] = true
		defer delete(active, visit)
		cloned, err := cloneControllerValue(value.Elem(), active)
		if err != nil {
			return reflect.Value{}, err
		}
		result := reflect.New(value.Type().Elem())
		result.Elem().Set(cloned)
		return result, nil
	case reflect.Struct, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return reflect.Value{}, fmt.Errorf("extra contains unsupported %s value", value.Kind())
	default:
		return value, nil
	}
}
