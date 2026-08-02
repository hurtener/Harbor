package sessionoverlay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

const (
	maxSessionSkillResolverBaseRows  = 1000
	maxSessionSkillResolverOwnedRows = skills.SnapshotSemanticCandidateCap
)

// ErrInvalidSessionSkillResolver means a resolver was built without the
// complete run-start authority needed to compose the session skill tier.
var ErrInvalidSessionSkillResolver = errors.New("agentcfg/sessionoverlay: invalid session skill resolver")

// CutoverModeReader supplies the already boot-declared tenant cutover state.
// It is deliberately a narrow reader so resolver construction cannot advance
// migration or discover tenants.
type CutoverModeReader interface {
	Mode(ctx context.Context, tenantID string) (CutoverMode, error)
}

// SessionSkillMembership is the immutable membership input captured from the
// active admin and user config revisions at run start. AdminMembershipSet
// distinguishes an absent admin selection (all non-session base rows remain
// eligible) from an explicitly empty selection. UserPersonalNames are added
// back from the exact ScopeUser rung; neither list selects an agent.
type SessionSkillMembership struct {
	AdminMembershipSet bool
	AdminNames         []string
	UserPersonalNames  []string
}

// SessionSkillResolverConfig contains the complete authority inputs for one
// run snapshot. Agent selection and config-revision projection occur before
// this constructor; this type never reads tool-invocation provenance.
type SessionSkillResolverConfig struct {
	Run     identity.Quadruple
	AgentID string
	// Base is the configured production SkillStore. It supplies both the
	// durable shared-rung reader and its mandatory driver-owned frozen-candidate
	// search policy, so the resolver cannot substitute a portable scorer.
	Base       skills.SkillStore
	Personal   *DurableStore
	Cutover    CutoverModeReader
	Membership SessionSkillMembership
}

// SessionSkillResolver is an immutable, per-run SkillReader projection. It
// owns no goroutines and performs no writes. All returned skills are copies so
// a consumer cannot mutate the snapshot observed by another invocation.
type SessionSkillResolver struct {
	run      identity.Quadruple
	agentID  string
	searcher skills.SnapshotCandidateSearcher
	all      map[string]skills.Skill
	byScope  map[skills.Scope]map[string]skills.Skill
	session  map[string]skills.Skill
}

// NewSessionSkillResolver captures a fence-stable composed skill view for one
// selected agent and one full run quadruple. It retries only when a lifecycle
// or erasure fence changes while the view is being enumerated.
func NewSessionSkillResolver(ctx context.Context, cfg SessionSkillResolverConfig) (*SessionSkillResolver, error) {
	if err := validateResolverConfig(cfg); err != nil {
		return nil, err
	}
	admin, err := canonicalMembership(cfg.Membership.AdminNames)
	if err != nil {
		return nil, fmt.Errorf("%w: admin membership: %w", ErrInvalidSessionSkillResolver, err)
	}
	user, err := canonicalMembership(cfg.Membership.UserPersonalNames)
	if err != nil {
		return nil, fmt.Errorf("%w: user membership: %w", ErrInvalidSessionSkillResolver, err)
	}
	for range MaxSessionSkillReadAttempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		before, err := loadFences(ctx, cfg.Personal.state, cfg.Run, cfg.AgentID)
		if err != nil {
			return nil, err
		}
		if before.erased() {
			return nil, ErrSessionErased
		}
		if err := before.lifecycleError(); err != nil {
			return nil, err
		}

		resolver, err := buildResolver(ctx, cfg, admin, user)
		if err != nil {
			return nil, err
		}
		after, err := loadFences(ctx, cfg.Personal.state, cfg.Run, cfg.AgentID)
		if err != nil {
			return nil, err
		}
		if after.erased() {
			return nil, ErrSessionErased
		}
		if err := after.lifecycleError(); err != nil {
			return nil, err
		}
		if before.equal(after) {
			return resolver, nil
		}
	}
	return nil, ErrSessionSkillReadUnstable
}

func validateResolverConfig(cfg SessionSkillResolverConfig) error {
	if err := identity.Validate(cfg.Run.Identity); err != nil {
		return fmt.Errorf("%w: identity: %w", ErrInvalidSessionSkillResolver, err)
	}
	if strings.TrimSpace(cfg.Run.RunID) == "" {
		return fmt.Errorf("%w: run ID is required", ErrInvalidSessionSkillResolver)
	}
	if strings.TrimSpace(cfg.AgentID) == "" {
		return fmt.Errorf("%w: selected agent ID is required", ErrInvalidSessionSkillResolver)
	}
	if cfg.Base == nil || cfg.Personal == nil || cfg.Personal.state == nil || cfg.Cutover == nil {
		return fmt.Errorf("%w: base SkillStore, durable store, and cutover reader are required", ErrInvalidSessionSkillResolver)
	}
	return nil
}

func canonicalMembership(names []string) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		canonical := canonicalNameFor(name)
		if canonical == "" {
			return nil, errors.New("name is empty after canonicalization")
		}
		result[canonical] = struct{}{}
	}
	return result, nil
}

func buildResolver(ctx context.Context, cfg SessionSkillResolverConfig, admin, user map[string]struct{}) (*SessionSkillResolver, error) {
	base, err := cfg.Base.List(ctx, cfg.Run, skills.ListFilter{Limit: maxSessionSkillResolverBaseRows})
	if err != nil {
		return nil, fmt.Errorf("agentcfg/sessionoverlay: list base skills: %w", err)
	}
	if len(base) > maxSessionSkillResolverBaseRows {
		return nil, fmt.Errorf("%w: base enumeration exceeded %d rows", ErrInvalidSessionSkillResolver, maxSessionSkillResolverBaseRows)
	}
	if len(base) == maxSessionSkillResolverBaseRows {
		probe, err := cfg.Base.List(ctx, cfg.Run, skills.ListFilter{Limit: 1, Offset: maxSessionSkillResolverBaseRows})
		if err != nil {
			return nil, fmt.Errorf("agentcfg/sessionoverlay: probe base enumeration bound: %w", err)
		}
		if len(probe) > 0 {
			return nil, fmt.Errorf("%w: base enumeration exceeds %d rows", ErrInvalidSessionSkillResolver, maxSessionSkillResolverBaseRows)
		}
	}
	baseByScope := make(map[skills.Scope]map[string]skills.Skill)
	for _, skill := range base {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := validateResolverSkill(skill); err != nil {
			return nil, err
		}
		if skill.Scope == skills.ScopeSession {
			continue
		}
		if err := putScoped(baseByScope, skill); err != nil {
			return nil, err
		}
	}
	if cfg.Membership.AdminMembershipSet {
		for name := range admin {
			if !hasAnyScope(baseByScope, name) {
				return nil, fmt.Errorf("%w: admin-pinned skill body %q is missing", ErrInvalidSessionSkillResolver, name)
			}
		}
		baseByScope, err = filterScoped(baseByScope, admin)
		if err != nil {
			return nil, err
		}
	}
	for name := range user {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		skill, err := cfg.Base.GetScope(ctx, cfg.Run, name, skills.ScopeUser)
		if err != nil {
			// D-345 membership is only a selection hint for an independently
			// durable body. A deleted or not-yet-written ScopeUser body is
			// harmlessly absent; unlike an admin-pinned body it is not an
			// authority/configuration failure.
			if errors.Is(err, skills.ErrSkillNotFound) {
				continue
			}
			return nil, fmt.Errorf("agentcfg/sessionoverlay: read durable user skill %q: %w", name, err)
		}
		if err := validateExactResolverSkill(skill, name, skills.ScopeUser); err != nil {
			return nil, err
		}
		if err := putScoped(baseByScope, skill); err != nil {
			return nil, err
		}
	}

	mode, err := cfg.Cutover.Mode(ctx, cfg.Run.TenantID)
	if err != nil {
		return nil, fmt.Errorf("agentcfg/sessionoverlay: resolve cutover mode: %w", err)
	}
	var session map[string]skills.Skill
	switch mode {
	case CutoverDualRead:
		session, err = loadLegacySessionTier(ctx, cfg)
	case CutoverStateOnly:
		session, err = loadOwnedSessionTier(ctx, cfg)
	default:
		return nil, fmt.Errorf("%w: unknown cutover mode %q", ErrInvalidSessionSkillResolver, mode)
	}
	if err != nil {
		return nil, err
	}

	all := flattenScoped(baseByScope)
	for name, skill := range session {
		all[name] = cloneSkill(skill)
	}
	byScope := cloneScoped(baseByScope)
	if len(session) > 0 {
		byScope[skills.ScopeSession] = cloneSkillMap(session)
	}
	return &SessionSkillResolver{run: cfg.Run, agentID: cfg.AgentID, searcher: cfg.Base, all: all, byScope: byScope, session: cloneSkillMap(session)}, nil
}

func loadLegacySessionTier(ctx context.Context, cfg SessionSkillResolverConfig) (map[string]skills.Skill, error) {
	record, err := cfg.Personal.state.Load(ctx, durableSessionQuad(cfg.Run), LegacyOverlayKind(cfg.AgentID))
	if errors.Is(err, state.ErrNotFound) {
		return map[string]skills.Skill{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: load legacy overlay: %w", ErrStateUnavailable, err)
	}
	overlay, err := decodeResolverLegacyOverlay(record, cfg.Run.TenantID, cfg.AgentID)
	if err != nil {
		return nil, err
	}
	result := make(map[string]skills.Skill, len(overlay.PersonalSkills))
	for _, name := range overlay.PersonalSkills {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		skill, err := cfg.Base.GetScope(ctx, cfg.Run, name, skills.ScopeSession)
		if err != nil {
			return nil, fmt.Errorf("%w: exact legacy session skill %q: %w", ErrLegacySkillInvalid, name, err)
		}
		canonical := canonicalNameFor(name)
		if err := validateExactResolverSkill(skill, canonical, skills.ScopeSession); err != nil {
			return nil, err
		}
		if prior, ok := result[canonical]; ok && prior.ContentHash != skill.ContentHash {
			return nil, fmt.Errorf("%w: canonical legacy aliases for %q disagree", ErrLegacySkillInvalid, canonical)
		}
		normalized, err := normalizeResolverSkill(skill)
		if err != nil {
			return nil, err
		}
		result[canonical] = cloneSkill(normalized)
	}
	return result, nil
}

func decodeResolverLegacyOverlay(record state.StateRecord, tenantID, agentID string) (Overlay, error) {
	if err := validateLegacyOverlayCandidate(record, tenantID); err != nil {
		return Overlay{}, err
	}
	if record.Kind != LegacyOverlayKind(agentID) {
		return Overlay{}, fmt.Errorf("%w: legacy kind does not exactly bind selected agent", ErrLegacyOverlayInvalid)
	}
	if err := rejectDuplicateJSONObjectFields(record.Bytes); err != nil {
		return Overlay{}, fmt.Errorf("%w: duplicate envelope field: %w", ErrLegacyOverlayInvalid, err)
	}
	var envelope struct {
		Overlay json.RawMessage `json:"overlay"`
	}
	if err := json.Unmarshal(record.Bytes, &envelope); err != nil {
		return Overlay{}, fmt.Errorf("%w: decode legacy envelope: %w", ErrLegacyOverlayInvalid, err)
	}
	if err := rejectDuplicateJSONObjectFields(envelope.Overlay); err != nil {
		return Overlay{}, fmt.Errorf("%w: duplicate overlay field: %w", ErrLegacyOverlayInvalid, err)
	}
	var overlay Overlay
	if err := decodeStrictJSON(envelope.Overlay, &overlay); err != nil {
		return Overlay{}, fmt.Errorf("%w: decode legacy overlay: %w", ErrLegacyOverlayInvalid, err)
	}
	return overlay, nil
}

func loadOwnedSessionTier(ctx context.Context, cfg SessionSkillResolverConfig) (map[string]skills.Skill, error) {
	prefix, err := PersonalSkillPrefix(cfg.AgentID)
	if err != nil {
		return nil, err
	}
	records, err := cfg.Personal.state.ListKindForIdentityBounded(ctx, durableSessionQuad(cfg.Run), prefix, maxSessionSkillResolverOwnedRows+1)
	if err != nil {
		return nil, fmt.Errorf("%w: list owned personal records: %w", ErrStateUnavailable, err)
	}
	if len(records) > maxSessionSkillResolverOwnedRows {
		return nil, fmt.Errorf("%w: owned personal enumeration exceeds %d rows", ErrInvalidSessionSkillResolver, maxSessionSkillResolverOwnedRows)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Kind < records[j].Kind })
	result := make(map[string]skills.Skill, len(records))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		personal, err := decodeResolverPersonal(record, cfg.AgentID)
		if err != nil {
			return nil, err
		}
		if personal.Deleted {
			delete(result, personal.CanonicalName)
			continue
		}
		if _, exists := result[personal.CanonicalName]; exists {
			return nil, fmt.Errorf("%w: duplicate owned personal name %q", ErrPersonalRecordInvalid, personal.CanonicalName)
		}
		normalized, err := normalizeResolverSkill(personal.Skill)
		if err != nil {
			return nil, err
		}
		result[personal.CanonicalName] = cloneSkill(normalized)
	}
	return result, nil
}

func decodeResolverPersonal(record state.StateRecord, agentID string) (PersonalSkillRecord, error) {
	if !strings.HasPrefix(record.Kind, personalKindPrefix) {
		return PersonalSkillRecord{}, fmt.Errorf("%w: personal record has invalid kind", ErrPersonalRecordInvalid)
	}
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
		return PersonalSkillRecord{}, fmt.Errorf("%w: decode owned record: %w", ErrPersonalRecordInvalid, err)
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

// SessionSkills returns only the resolved session tier, never ScopeUser or a
// wider base rung. It is the safe backing surface for the session-only API.
func (r *SessionSkillResolver) SessionSkills(ctx context.Context, id identity.Quadruple) ([]skills.Skill, error) {
	if err := r.validateCall(ctx, id); err != nil {
		return nil, err
	}
	return sortedSkills(r.session), nil
}

// Get returns the highest-precedence composed skill for name.
func (r *SessionSkillResolver) Get(ctx context.Context, id identity.Quadruple, name string) (skills.Skill, error) {
	if err := r.validateCall(ctx, id); err != nil {
		return skills.Skill{}, err
	}
	skill, found := r.all[canonicalNameFor(name)]
	if !found {
		return skills.Skill{}, skills.ErrSkillNotFound
	}
	return cloneSkill(skill), nil
}

// GetScope returns a skill only from the requested composed rung.
func (r *SessionSkillResolver) GetScope(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) (skills.Skill, error) {
	if err := r.validateCall(ctx, id); err != nil {
		return skills.Skill{}, err
	}
	skill, found := r.byScope[scope][canonicalNameFor(name)]
	if !found {
		return skills.Skill{}, skills.ErrSkillNotFound
	}
	return cloneSkill(skill), nil
}

// List returns the composed view in deterministic canonical-name order.
func (r *SessionSkillResolver) List(ctx context.Context, id identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
	if err := r.validateCall(ctx, id); err != nil {
		return nil, err
	}
	if filter.Limit < 0 || filter.Offset < 0 || filter.Limit > 1000 {
		return nil, fmt.Errorf("%w: invalid list bounds", ErrInvalidSessionSkillResolver)
	}
	candidates := r.all
	if filter.Scope != "" {
		candidates = r.byScope[filter.Scope]
	}
	listed := make(map[string]skills.Skill, len(candidates))
	for name, skill := range candidates {
		if filter.TaskType != "" && skill.TaskType != filter.TaskType {
			continue
		}
		if len(filter.Tags) > 0 && !hasAnyTag(skill.Tags, filter.Tags) {
			continue
		}
		listed[name] = skill
	}
	result := sortedSkills(listed)
	if filter.Offset >= len(result) {
		return []skills.Skill{}, nil
	}
	result = result[filter.Offset:]
	limit := filter.Limit
	if limit == 0 {
		limit = 100
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// Search delegates exactly the frozen composed candidates to the run-bound
// candidate-search policy. The policy is required at construction so a
// semantic runtime cannot silently degrade to lexical ranking, and a lexical
// runtime retains its configured FTS5 -> regex -> exact ordering.
func (r *SessionSkillResolver) Search(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	if err := r.validateCall(ctx, id); err != nil {
		return nil, err
	}
	if limit < 0 || limit > 1000 {
		return nil, fmt.Errorf("%w: invalid search limit", ErrInvalidSessionSkillResolver)
	}
	if limit == 0 {
		limit = 20
	}
	result, err := r.searcher.SearchSnapshot(ctx, r.run, query, sortedSkills(r.all), limit)
	if err != nil {
		return nil, err
	}
	if len(result) > limit {
		return nil, fmt.Errorf("%w: candidate searcher exceeded limit", ErrInvalidSessionSkillResolver)
	}
	seen := make(map[string]struct{}, len(result))
	for i := range result {
		if math.IsNaN(result[i].Score) || math.IsInf(result[i].Score, 0) || result[i].Score < 0 || result[i].Score > 1 {
			return nil, fmt.Errorf("%w: candidate searcher returned invalid score", ErrInvalidSessionSkillResolver)
		}
		switch result[i].Path {
		case skills.PathFTS5, skills.PathFullText, skills.PathRegex, skills.PathExact, skills.PathSemantic:
		default:
			return nil, fmt.Errorf("%w: candidate searcher returned unknown path %q", ErrInvalidSessionSkillResolver, result[i].Path)
		}
		name := canonicalNameFor(result[i].Skill.Name)
		expected, ok := r.all[name]
		if !ok || skills.CanonicalContentHash(expected) != skills.CanonicalContentHash(result[i].Skill) {
			return nil, fmt.Errorf("%w: candidate searcher returned a non-snapshot skill %q", ErrInvalidSessionSkillResolver, result[i].Skill.Name)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("%w: candidate searcher returned duplicate skill %q", ErrInvalidSessionSkillResolver, result[i].Skill.Name)
		}
		seen[name] = struct{}{}
		result[i].Skill = cloneSkill(expected)
	}
	return result, nil
}

func (r *SessionSkillResolver) validateCall(ctx context.Context, id identity.Quadruple) error {
	if r == nil || r.searcher == nil || r.all == nil || r.run != id {
		return fmt.Errorf("%w: resolver identity does not match caller", ErrInvalidSessionSkillResolver)
	}
	return ctx.Err()
}

func validateResolverSkill(skill skills.Skill) error {
	if err := skill.Validate(); err != nil {
		return fmt.Errorf("%w: base skill %q: %w", ErrInvalidSessionSkillResolver, skill.Name, err)
	}
	if canonicalNameFor(skill.Name) == "" {
		return fmt.Errorf("%w: base skill name is empty", ErrInvalidSessionSkillResolver)
	}
	return nil
}

func validateExactResolverSkill(skill skills.Skill, name string, scope skills.Scope) error {
	if err := validateResolverSkill(skill); err != nil || skill.Scope != scope || canonicalNameFor(skill.Name) != canonicalNameFor(name) {
		return fmt.Errorf("%w: exact %s body %q is invalid", ErrLegacySkillInvalid, scope, name)
	}
	return nil
}

func putScoped(in map[skills.Scope]map[string]skills.Skill, skill skills.Skill) error {
	var err error
	skill, err = normalizeResolverSkill(skill)
	if err != nil {
		return err
	}
	if in[skill.Scope] == nil {
		in[skill.Scope] = make(map[string]skills.Skill)
	}
	canonical := canonicalNameFor(skill.Name)
	if prior, found := in[skill.Scope][canonical]; found && skills.CanonicalContentHash(prior) != skills.CanonicalContentHash(skill) {
		return fmt.Errorf("%w: same-scope canonical aliases for %q have conflicting bodies", ErrInvalidSessionSkillResolver, canonical)
	}
	in[skill.Scope][canonical] = cloneSkill(skill)
	return nil
}

func hasAnyScope(in map[skills.Scope]map[string]skills.Skill, name string) bool {
	for _, scoped := range in {
		if _, ok := scoped[name]; ok {
			return true
		}
	}
	return false
}

func filterScoped(in map[skills.Scope]map[string]skills.Skill, allowed map[string]struct{}) (map[skills.Scope]map[string]skills.Skill, error) {
	result := make(map[skills.Scope]map[string]skills.Skill, len(in))
	for _, scoped := range in {
		for name, skill := range scoped {
			if _, ok := allowed[name]; ok {
				if err := putScoped(result, skill); err != nil {
					return nil, err
				}
			}
		}
	}
	return result, nil
}

func flattenScoped(in map[skills.Scope]map[string]skills.Skill) map[string]skills.Skill {
	result := make(map[string]skills.Skill)
	for _, scope := range []skills.Scope{skills.ScopeGlobal, skills.ScopeTenant, skills.ScopeProject, skills.ScopeUser} {
		for name, skill := range in[scope] {
			result[name] = cloneSkill(skill)
		}
	}
	return result
}

func cloneScoped(in map[skills.Scope]map[string]skills.Skill) map[skills.Scope]map[string]skills.Skill {
	result := make(map[skills.Scope]map[string]skills.Skill, len(in))
	for scope, scoped := range in {
		result[scope] = cloneSkillMap(scoped)
	}
	return result
}

func cloneSkillMap(in map[string]skills.Skill) map[string]skills.Skill {
	result := make(map[string]skills.Skill, len(in))
	for name, skill := range in {
		result[name] = cloneSkill(skill)
	}
	return result
}

func sortedSkills(in map[string]skills.Skill) []skills.Skill {
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]skills.Skill, 0, len(names))
	for _, name := range names {
		result = append(result, cloneSkill(in[name]))
	}
	return result
}

func cloneSkill(skill skills.Skill) skills.Skill {
	result := skill
	result.Tags = append([]string(nil), skill.Tags...)
	result.Steps = append([]string(nil), skill.Steps...)
	result.Preconditions = append([]string(nil), skill.Preconditions...)
	result.FailureModes = append([]string(nil), skill.FailureModes...)
	result.RequiredTools = append([]string(nil), skill.RequiredTools...)
	result.RequiredNS = append([]string(nil), skill.RequiredNS...)
	result.RequiredTags = append([]string(nil), skill.RequiredTags...)
	if skill.Extra != nil {
		result.Extra = cloneExtra(skill.Extra)
	}
	return result
}

// normalizeResolverSkill accepts only JSON-compatible Extra values and
// normalizes them once at snapshot construction. This supplies a complete,
// cycle-safe immutable boundary: unsupported values and cycles are rejected
// before they can reach shared resolver state.
func normalizeResolverSkill(skill skills.Skill) (skills.Skill, error) {
	if err := validateResolverSkill(skill); err != nil {
		return skills.Skill{}, err
	}
	if skill.Extra == nil {
		return skill, nil
	}
	bytes, err := json.Marshal(skill.Extra)
	if err != nil {
		return skills.Skill{}, fmt.Errorf("%w: skill %q Extra must be JSON-compatible and acyclic: %w", ErrInvalidSessionSkillResolver, skill.Name, err)
	}
	var normalized map[string]any
	if err := json.Unmarshal(bytes, &normalized); err != nil {
		return skills.Skill{}, fmt.Errorf("%w: skill %q Extra normalization: %w", ErrInvalidSessionSkillResolver, skill.Name, err)
	}
	skill.Extra = normalized
	return skill, nil
}

// cloneExtra copies a normalized JSON object. normalizeResolverSkill proves
// the only reachable values are nil, bool, string, float64, []any, and
// map[string]any, so cloning cannot recurse through cycles or retain mutable
// aliases.
func cloneExtra(in map[string]any) map[string]any {
	result := make(map[string]any, len(in))
	for key, value := range in {
		result[key] = cloneExtraValue(value)
	}
	return result
}

func cloneExtraValue(value any) any {
	switch typed := value.(type) {
	case nil, bool, string, float64:
		return typed
	case []any:
		result := make([]any, len(typed))
		for i := range typed {
			result[i] = cloneExtraValue(typed[i])
		}
		return result
	case map[string]any:
		return cloneExtra(typed)
	default:
		// This is unreachable after normalizeResolverSkill. Keeping the value
		// out rather than exposing a shared mutable reference preserves the
		// resolver's immutable-output contract if an internal invariant regresses.
		return nil
	}
}

func hasAnyTag(skillTags, wanted []string) bool {
	for _, tag := range skillTags {
		for _, want := range wanted {
			if tag == want {
				return true
			}
		}
	}
	return false
}
