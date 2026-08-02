package sessionoverlay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
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
	Run        identity.Quadruple
	AgentID    string
	Base       skills.SkillReader
	Personal   *DurableStore
	Cutover    CutoverModeReader
	Membership SessionSkillMembership
}

// SessionSkillResolver is an immutable, per-run SkillReader projection. It
// owns no goroutines and performs no writes. All returned skills are copies so
// a consumer cannot mutate the snapshot observed by another invocation.
type SessionSkillResolver struct {
	run     identity.Quadruple
	agentID string
	all     map[string]skills.Skill
	byScope map[skills.Scope]map[string]skills.Skill
	session map[string]skills.Skill
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
		return fmt.Errorf("%w: base reader, durable store, and cutover reader are required", ErrInvalidSessionSkillResolver)
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
	base, err := cfg.Base.List(ctx, cfg.Run, skills.ListFilter{Limit: 1000})
	if err != nil {
		return nil, fmt.Errorf("agentcfg/sessionoverlay: list base skills: %w", err)
	}
	baseByScope := make(map[skills.Scope]map[string]skills.Skill)
	for _, skill := range base {
		if err := validateResolverSkill(skill); err != nil {
			return nil, err
		}
		if skill.Scope == skills.ScopeSession {
			continue
		}
		putScoped(baseByScope, skill)
	}
	if cfg.Membership.AdminMembershipSet {
		for name := range admin {
			if !hasAnyScope(baseByScope, name) {
				return nil, fmt.Errorf("%w: admin-pinned skill body %q is missing", ErrInvalidSessionSkillResolver, name)
			}
		}
		baseByScope = filterScoped(baseByScope, admin)
	}
	for name := range user {
		skill, err := cfg.Base.GetScope(ctx, cfg.Run, name, skills.ScopeUser)
		if err != nil {
			return nil, fmt.Errorf("agentcfg/sessionoverlay: read durable user skill %q: %w", name, err)
		}
		if err := validateExactResolverSkill(skill, name, skills.ScopeUser); err != nil {
			return nil, err
		}
		putScoped(baseByScope, skill)
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
	return &SessionSkillResolver{run: cfg.Run, agentID: cfg.AgentID, all: all, byScope: byScope, session: cloneSkillMap(session)}, nil
}

func loadLegacySessionTier(ctx context.Context, cfg SessionSkillResolverConfig) (map[string]skills.Skill, error) {
	record, err := cfg.Personal.state.Load(ctx, durableSessionQuad(cfg.Run), LegacyOverlayKind(cfg.AgentID))
	if errors.Is(err, state.ErrNotFound) {
		return map[string]skills.Skill{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: load legacy overlay: %v", ErrStateUnavailable, err)
	}
	overlay, err := decodeResolverLegacyOverlay(record, cfg.Run.TenantID, cfg.AgentID)
	if err != nil {
		return nil, err
	}
	result := make(map[string]skills.Skill, len(overlay.PersonalSkills))
	for _, name := range overlay.PersonalSkills {
		skill, err := cfg.Base.GetScope(ctx, cfg.Run, name, skills.ScopeSession)
		if err != nil {
			return nil, fmt.Errorf("%w: exact legacy session skill %q: %v", ErrLegacySkillInvalid, name, err)
		}
		canonical := canonicalNameFor(name)
		if err := validateExactResolverSkill(skill, canonical, skills.ScopeSession); err != nil {
			return nil, err
		}
		if prior, ok := result[canonical]; ok && prior.ContentHash != skill.ContentHash {
			return nil, fmt.Errorf("%w: canonical legacy aliases for %q disagree", ErrLegacySkillInvalid, canonical)
		}
		result[canonical] = cloneSkill(skill)
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
	records, err := cfg.Personal.state.ListKindForIdentity(ctx, durableSessionQuad(cfg.Run), prefix)
	if err != nil {
		return nil, fmt.Errorf("%w: list owned personal records: %v", ErrStateUnavailable, err)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Kind < records[j].Kind })
	result := make(map[string]skills.Skill, len(records))
	for _, record := range records {
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
		result[personal.CanonicalName] = cloneSkill(personal.Skill)
	}
	return result, nil
}

func decodeResolverPersonal(record state.StateRecord, agentID string) (PersonalSkillRecord, error) {
	if !strings.HasPrefix(record.Kind, personalKindPrefix) {
		return PersonalSkillRecord{}, fmt.Errorf("%w: personal record has invalid kind", ErrPersonalRecordInvalid)
	}
	var envelope struct {
		CanonicalName *string `json:"canonical_name"`
	}
	if err := json.Unmarshal(record.Bytes, &envelope); err != nil || envelope.CanonicalName == nil {
		return PersonalSkillRecord{}, fmt.Errorf("%w: read canonical name: %v", ErrPersonalRecordInvalid, err)
	}
	decoded, found, err := decodePersonal(record.Bytes, agentID, *envelope.CanonicalName)
	if err != nil || !found {
		return PersonalSkillRecord{}, fmt.Errorf("%w: decode owned record: %v", ErrPersonalRecordInvalid, err)
	}
	wantKind, err := PersonalSkillKind(agentID, decoded.CanonicalName)
	if err != nil || record.Kind != wantKind {
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

// Search performs deterministic lexical ranking over exactly the composed
// view. Semantic retrieval is intentionally not surfaced by this seam; a
// future semantic API must require an injected skills.Embedder.
func (r *SessionSkillResolver) Search(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	if err := r.validateCall(ctx, id); err != nil {
		return nil, err
	}
	if limit < 0 || limit > 1000 {
		return nil, fmt.Errorf("%w: invalid search limit", ErrInvalidSessionSkillResolver)
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return []skills.RankedSkill{}, nil
	}
	result := make([]skills.RankedSkill, 0, len(r.all))
	for _, skill := range r.all {
		score, path, ok := lexicalScore(skill, needle)
		if ok {
			result = append(result, skills.RankedSkill{Skill: cloneSkill(skill), Score: score, Path: path})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Score != result[j].Score {
			return result[i].Score > result[j].Score
		}
		return canonicalNameFor(result[i].Skill.Name) < canonicalNameFor(result[j].Skill.Name)
	})
	if limit == 0 {
		limit = 20
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (r *SessionSkillResolver) validateCall(ctx context.Context, id identity.Quadruple) error {
	if r == nil || r.all == nil || r.run != id {
		return fmt.Errorf("%w: resolver identity does not match caller", ErrInvalidSessionSkillResolver)
	}
	return ctx.Err()
}

func validateResolverSkill(skill skills.Skill) error {
	if err := skill.Validate(); err != nil {
		return fmt.Errorf("%w: base skill %q: %v", ErrInvalidSessionSkillResolver, skill.Name, err)
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

func putScoped(in map[skills.Scope]map[string]skills.Skill, skill skills.Skill) {
	if in[skill.Scope] == nil {
		in[skill.Scope] = make(map[string]skills.Skill)
	}
	in[skill.Scope][canonicalNameFor(skill.Name)] = cloneSkill(skill)
}

func hasAnyScope(in map[skills.Scope]map[string]skills.Skill, name string) bool {
	for _, scoped := range in {
		if _, ok := scoped[name]; ok {
			return true
		}
	}
	return false
}

func filterScoped(in map[skills.Scope]map[string]skills.Skill, allowed map[string]struct{}) map[skills.Scope]map[string]skills.Skill {
	result := make(map[skills.Scope]map[string]skills.Skill, len(in))
	for scope, scoped := range in {
		for name, skill := range scoped {
			if _, ok := allowed[name]; ok {
				putScoped(result, skill)
			}
		}
		_ = scope
	}
	return result
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
		result.Extra = make(map[string]any, len(skill.Extra))
		for key, value := range skill.Extra {
			result.Extra[key] = value
		}
	}
	return result
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

func lexicalScore(skill skills.Skill, needle string) (float64, string, bool) {
	exact := func(value string) bool { return strings.EqualFold(strings.TrimSpace(value), needle) }
	if exact(skill.Name) || exact(skill.Title) || exact(skill.Trigger) {
		return 1, skills.PathExact, true
	}
	for _, tag := range skill.Tags {
		if exact(tag) {
			return 1, skills.PathExact, true
		}
	}
	if strings.Contains(strings.ToLower(skill.Name), needle) {
		return .90, skills.PathRegex, true
	}
	if strings.Contains(strings.ToLower(skill.Title), needle) || strings.Contains(strings.ToLower(skill.Trigger), needle) {
		return .85, skills.PathRegex, true
	}
	for _, tag := range skill.Tags {
		if strings.Contains(strings.ToLower(tag), needle) {
			return .85, skills.PathRegex, true
		}
	}
	if strings.Contains(strings.ToLower(skill.Description), needle) {
		return .75, skills.PathRegex, true
	}
	return 0, "", false
}
