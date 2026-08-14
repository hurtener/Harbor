package protocol

// composition_preview.go — the read-only effective-composition preview
// service.
//
// The preview reports what the strict run-start composer WOULD compose for a
// target (tenant, user, session) and effective boot-agent, WITHOUT
// materialising anything: no lifecycle creation, no admin pack verb, no
// AgentConfig revision write, no SkillStore/ArtifactStore write, no
// identity/reach minting, and no boot-file reread (the boot baseline comes
// from the frozen eager boot-pack index). The composition itself is
// performed by THE ONE accepted strict composer the run-start resolver uses
// (sessionoverlay.ComposeOperatorTier) — this service never re-implements or
// shadows a second resolution path.
//
// Authorization:
//
//   - An ordinary verified caller may preview only its own exact
//     (tenant, user, session) and effective boot-agent, after the signed
//     session-reach gate, the signed agent-reach gate, and the retirement
//     gate. The signed gates are the canonical auth.SessionReachAuthorizer /
//     auth.AgentReachAuthorizer contract: a PRESENT claim must contain the
//     target session / effective agent, and the agent gate fails closed when
//     no reach is established on ctx.
//   - An admin or console:fleet caller may address a SAME-TENANT user only
//     with signed effective-agent reach. The widening is audited on the
//     canonical audit.admin_scope_used event BEFORE any composition read.
//     Foreign, cross-tenant, and missing targets are NON-ORACULAR: they
//     return the same unavailable outcome a caller would get for a target
//     that does not exist, so the surface never reveals whether a foreign
//     triple or a non-declared agent exists.
//
// The durable agent_packs section is READ-ONLY here: the preview reads the
// EXACT active revision's agent_packs section — the same durable,
// content-addressed revision the admin `agent_config.agent_packs.list` verb
// reads and the pack verbs author. The list verb's durable revision-authoring
// meaning is untouched; the preview never writes, repoints, or clears the
// pack.
//
// The service is immutable after construction and safe for N concurrent
// goroutines: it holds only the injected read seams, the signed-reach gates,
// and an optional audit bus; per-call state lives in arguments. Every
// returned item and slice is a deep copy.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
)

// compositionPreviewMethod is the Protocol method this service backs. The
// wire handler maps the widened-audit event's Method to this name.
const compositionPreviewMethod = "agent_config.composition.preview"

// Composition-preview sentinel errors. In-process callers compare with
// errors.Is; the wire handler maps each onto a canonical Protocol code.
var (
	// ErrPreviewIdentityRequired — the request carried no verified identity
	// on ctx, an incomplete target triple, or an empty effective agent id.
	// Fails closed (identity is mandatory).
	ErrPreviewIdentityRequired = errors.New("agentcfg/protocol: composition preview requires a complete verified identity and effective agent")
	// ErrPreviewMisconfigured — the service was constructed without a
	// mandatory dependency (the agent-config reader or the boot-pack reader).
	ErrPreviewMisconfigured = errors.New("agentcfg/protocol: composition preview missing a mandatory dependency")
	// ErrPreviewSessionReachDenied — a PRESENT signed session_reach claim
	// does not contain the target session. Loud — the settled reach contract.
	ErrPreviewSessionReachDenied = errors.New("agentcfg/protocol: composition preview session reach denied")
	// ErrPreviewAgentReachDenied — the caller's signed agent_reach does not
	// contain the effective agent, or no reach is established on ctx (the
	// gate fails closed; an unwired gate is an honest "cannot verify reach",
	// never a silent widening).
	ErrPreviewAgentReachDenied = errors.New("agentcfg/protocol: composition preview agent reach denied")
)

// PreviewOutcome is the typed outcome of one composition preview.
type PreviewOutcome string

// Preview outcomes. Each response carries exactly one.
const (
	// PreviewOutcomeAvailable — the composition resolved: deterministic
	// items (possibly empty) plus the deterministic set hashes.
	PreviewOutcomeAvailable PreviewOutcome = "available"
	// PreviewOutcomeUnavailable — there is nothing to compose for the
	// target (no boot baseline AND no active durable revision), or the
	// caller is not entitled to the target. Foreign / cross-tenant /
	// missing are non-oracular: the response is identical, so nothing
	// about the target is revealed.
	PreviewOutcomeUnavailable PreviewOutcome = "unavailable"
	// PreviewOutcomeConflict — the strict composer refused a typed
	// boot/revision conflict: a canonical name whose semantic content
	// differs across (or within) the boot baseline and the active revision.
	// Never a silent last-write-wins overwrite.
	PreviewOutcomeConflict PreviewOutcome = "conflict"
	// PreviewOutcomeRetired — the effective agent's terminal lifecycle
	// tombstone is installed; the composition is no longer readable.
	PreviewOutcomeRetired PreviewOutcome = "retired"
)

// CompositionPreviewItem is ONE composed effective-operator-tier item: the
// canonical name, the canonical attachment-free semantic content hash, the
// strict-merge provenance marker (boot|revision|both), and the deep-copied
// composed skill body.
type CompositionPreviewItem struct {
	// Name is the canonical (lowercase, trimmed) operator-tier name.
	Name string
	// SemanticHash is the canonical attachment-free content hash of Skill
	// (skills.CanonicalContentHash) — the semantic identity the strict
	// merge and every set hash use.
	SemanticHash string
	// Source is the strict-merge provenance marker: exactly
	// "boot" | "revision" | "both".
	Source skills.OperatorTierSource
	// Skill is the deep-copied composed skill body (the boot body is
	// retained when the item is both).
	Skill skills.Skill
}

// CompositionPreviewRequest names the target of a read-only composition
// preview. The caller's VERIFIED identity comes from ctx; the target triple
// may differ from the caller's only for an elevated (admin/console:fleet)
// caller, and only within the caller's tenant.
type CompositionPreviewRequest struct {
	TenantID  string
	UserID    string
	SessionID string
	AgentID   string
}

// CompositionPreviewResponse is the immutable, deterministic preview result.
// Every item and slice is a deep copy; callers may mutate their copy without
// affecting the service or another caller's result.
type CompositionPreviewResponse struct {
	// Outcome is one of available | unavailable | conflict | retired.
	Outcome PreviewOutcome
	// ConflictName is the first (canonical-sorted) offending canonical
	// name when Outcome is conflict, "" otherwise.
	ConflictName string
	// BootPackSetHash is the deterministic set hash over the boot baseline
	// entries only ("" when the boot baseline is empty).
	BootPackSetHash string
	// CombinedHash is the deterministic set hash over the unique combined
	// operator-tier items ("" when the tier is empty).
	CombinedHash string
	// RevisionHash is the deterministic set hash over the active-revision
	// pack items only ("" when no revision pack is bound).
	RevisionHash string
	// RevisionID is the fresh active revision read for this preview ("" when
	// no active revision exists).
	RevisionID string
	// ContentHash is the fresh active revision's content hash ("" when no
	// active revision exists).
	ContentHash string
	// Items are the effective items in deterministic canonical-name order.
	Items []CompositionPreviewItem
	// Widened is true when this preview was an elevated (admin or
	// console:fleet) same-tenant widened read, which was audited before the
	// composition read.
	Widened bool
}

// AgentConfigReader is the read-only slice of the durable agent-config
// registry the preview service needs: the FRESH active revision (read at
// every preview) plus the retirement gate. A RetirementRegistry satisfies it;
// the interface exists so tests inject a narrow fake and the production
// assembler wires the real registry without touching Service construction.
// Both methods are reads — the preview structurally cannot write a revision.
type AgentConfigReader interface {
	Active(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error)
	RetirementStatus(ctx context.Context, id identity.Quadruple, agentID string) (agentcfg.RetirementStatus, bool, error)
}

// BootPackReader is the frozen eager boot-pack index surface the preview
// composes from. *bootpacks.Index satisfies it. Lookup NEVER rereads the
// boot files: the baseline is frozen at boot, and config removal is
// represented by the absence of the key in the next index.
type BootPackReader interface {
	Lookup(tenantID, agentID string) ([]bootpacks.Entry, bool)
}

// CompositionPreviewService implements the read-only effective-composition
// preview. It is immutable after construction and safe for concurrent reuse
// by N goroutines.
type CompositionPreviewService struct {
	registry     AgentConfigReader
	bootIndex    BootPackReader
	sessionReach auth.SessionReachAuthorizer
	agentReach   auth.AgentReachAuthorizer
	bus          events.EventBus // optional — nil ⇒ the widened audit is logged at Info, never silent
	redactor     audit.Redactor  // optional — defence-in-depth on the audit payload
	logger       *slog.Logger
}

// CompositionPreviewOption configures NewCompositionPreviewService.
type CompositionPreviewOption func(*CompositionPreviewService)

// WithPreviewSessionReach wires the canonical signed-session-reach gate. A
// PRESENT session_reach claim must contain the target session; an absent
// claim preserves dynamic selection (the gate encodes that distinction).
// Unsupplied (or nil) leaves the transport edge as the enforcement point.
func WithPreviewSessionReach(a auth.SessionReachAuthorizer) CompositionPreviewOption {
	return func(s *CompositionPreviewService) {
		if a != nil {
			s.sessionReach = a
		}
	}
}

// WithPreviewAgentReach wires the canonical effective-agent gate. The
// effective boot-agent must be a member of the caller's verified agent_reach.
// Unsupplied (or nil) FAILS CLOSED: no preview is served (an unwired gate is
// an honest "cannot verify reach", never a silent widening). The production
// assembler wires auth.NewAgentReachAuthorizer().
func WithPreviewAgentReach(a auth.AgentReachAuthorizer) CompositionPreviewOption {
	return func(s *CompositionPreviewService) {
		if a != nil {
			s.agentReach = a
		}
	}
}

// WithPreviewBus wires the canonical events.EventBus the service publishes
// the widened-operations audit.admin_scope_used event onto. A nil bus is
// treated as "WithPreviewBus not supplied" — the widened preview still works,
// but the audit observation is logged at Info instead of published (the
// admin action is NEVER fully silent).
func WithPreviewBus(b events.EventBus) CompositionPreviewOption {
	return func(s *CompositionPreviewService) {
		if b != nil {
			s.bus = b
		}
	}
}

// WithPreviewRedactor wires the audit.Redactor the service runs the audit
// payload through before publishing. A nil redactor is treated as
// "WithPreviewRedactor not supplied" (the payload is SafePayload by
// construction).
func WithPreviewRedactor(r audit.Redactor) CompositionPreviewOption {
	return func(s *CompositionPreviewService) {
		if r != nil {
			s.redactor = r
		}
	}
}

// WithPreviewLogger sets the slog.Logger the service logs widened previews
// and audit-emit failures to. A nil logger routes to slog.Default().
func WithPreviewLogger(l *slog.Logger) CompositionPreviewOption {
	return func(s *CompositionPreviewService) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewCompositionPreviewService builds the read-only preview over the two
// mandatory read seams: the agent-config reader (fresh active revision +
// retirement gate) and the frozen boot-pack index. A nil seam fails loud with
// ErrPreviewMisconfigured rather than building a service that would
// nil-panic on the first request. The returned *CompositionPreviewService is
// immutable after construction and safe for concurrent use by N goroutines.
func NewCompositionPreviewService(registry AgentConfigReader, bootIndex BootPackReader, opts ...CompositionPreviewOption) (*CompositionPreviewService, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: agent-config reader is nil", ErrPreviewMisconfigured)
	}
	if bootIndex == nil {
		return nil, fmt.Errorf("%w: boot-pack reader is nil", ErrPreviewMisconfigured)
	}
	s := &CompositionPreviewService{registry: registry, bootIndex: bootIndex, logger: slog.Default()}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// CompositionPreview resolves the read-only effective-composition preview for
// the requested target under the caller's verified ctx identity, scopes, and
// signed reach. See the package doc for the authorization model.
func (s *CompositionPreviewService) CompositionPreview(ctx context.Context, req CompositionPreviewRequest) (CompositionPreviewResponse, error) {
	if err := ctx.Err(); err != nil {
		return CompositionPreviewResponse{}, err
	}
	caller, ok := identity.FromVerified(ctx)
	if !ok {
		return CompositionPreviewResponse{}, ErrPreviewIdentityRequired
	}
	if err := identity.Validate(caller); err != nil {
		return CompositionPreviewResponse{}, fmt.Errorf("%w: caller: %w", ErrPreviewIdentityRequired, err)
	}
	target := identity.Identity{
		TenantID:  strings.TrimSpace(req.TenantID),
		UserID:    strings.TrimSpace(req.UserID),
		SessionID: strings.TrimSpace(req.SessionID),
	}
	if err := identity.Validate(target); err != nil {
		return CompositionPreviewResponse{}, fmt.Errorf("%w: target: %w", ErrPreviewIdentityRequired, err)
	}
	agentID := strings.TrimSpace(req.AgentID)
	if agentID == "" {
		return CompositionPreviewResponse{}, fmt.Errorf("%w: effective agent id is empty", ErrPreviewIdentityRequired)
	}

	if auth.HasScope(ctx, auth.ScopeAdmin) || auth.HasScope(ctx, auth.ScopeConsoleFleet) {
		// Elevated path: a same-tenant user is addressable only with signed
		// effective-agent reach. A cross-tenant target is refused NON-ORACULARLY
		// (the unavailable outcome, indistinguishable from a missing target) —
		// no read, no audit, no existence oracle.
		if target.TenantID != caller.TenantID {
			return unavailablePreview(false), nil
		}
		if err := s.authorizeAgentReach(ctx, agentID); err != nil {
			return CompositionPreviewResponse{}, err
		}
		// The widening is audited BEFORE any composition read.
		s.emitWidenedAudit(ctx, caller, target, agentID)
		return s.previewTarget(ctx, target, agentID, true)
	}

	// Ordinary path: the caller's own exact triple only. A foreign triple is
	// refused NON-ORACULARLY — indistinguishable from a target that does not
	// exist — so an ordinary caller can never probe another user's session.
	if target != caller {
		return unavailablePreview(false), nil
	}
	if err := s.authorizeSessionReach(ctx, target.SessionID); err != nil {
		return CompositionPreviewResponse{}, err
	}
	if err := s.authorizeAgentReach(ctx, agentID); err != nil {
		return CompositionPreviewResponse{}, err
	}
	return s.previewTarget(ctx, target, agentID, false)
}

// previewTarget performs the gated composition read for an entitled target:
// retirement gate, frozen boot baseline lookup, fresh active revision read,
// and THE strict composer.
func (s *CompositionPreviewService) previewTarget(ctx context.Context, target identity.Identity, agentID string, widened bool) (CompositionPreviewResponse, error) {
	q := identity.Quadruple{Identity: target}

	// Retirement gate: a tombstoned agent has no readable composition.
	_, retired, err := s.registry.RetirementStatus(ctx, q, agentID)
	if err != nil {
		return CompositionPreviewResponse{}, fmt.Errorf("composition preview: retirement gate: %w", err)
	}
	if retired {
		return retiredPreview(widened), nil
	}

	// Boot baseline: the frozen eager index — NEVER a boot-file reread. A
	// missing (tenant, agent) key (config removal, or an undeclared agent)
	// is NOT terminal: the durable active revision is independently
	// persisted and survives config removal, so the preview falls through
	// and composes it revision-only. Only when NO boot baseline AND NO
	// active revision exist does the target become the non-oracular
	// unavailable outcome.
	bootEntries, _ := s.bootIndex.Lookup(target.TenantID, agentID)

	// Fresh active revision: read at every preview so a new durable pack
	// revision is reflected immediately. The same-hash migration shadow
	// dedupes to `both` and a differing hash fails typed below — inside the
	// strict composer, never a silent overwrite.
	rev, set, err := s.registry.Active(ctx, q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		if errors.Is(err, agentcfg.ErrAgentRetired) {
			// The tombstone landed between the retirement gate and the read.
			return retiredPreview(widened), nil
		}
		return CompositionPreviewResponse{}, fmt.Errorf("composition preview: read active revision: %w", err)
	}
	if !set && len(bootEntries) == 0 {
		// Nothing to compose: no boot baseline and no durable revision.
		// Non-oracular — byte-identical to any other absent target.
		return unavailablePreview(widened), nil
	}
	var revisionSkills []skills.Skill
	if set && len(rev.Payload.AgentPacks) > 0 {
		revisionSkills, err = skills.PackItemsToSkills(rev.Payload.AgentPacks)
		if err != nil {
			return CompositionPreviewResponse{}, fmt.Errorf("composition preview: convert active pack: %w", err)
		}
	}

	// THE strict composer the run-start resolver uses — never a second
	// resolution path. The composer re-validates and re-hashes every input,
	// dedupes same-name/same-hash to `both`, fails typed on same-name /
	// different-hash, and caps the unique combined tier.
	tier, err := sessionoverlay.ComposeOperatorTier(bootEntries, revisionSkills)
	if err != nil {
		if errors.Is(err, sessionoverlay.ErrOperatorTierConflict) {
			return conflictPreview(widened, previewConflictName(bootEntries, revisionSkills)), nil
		}
		// Invalid or over-bounded inputs are integrity failures — loud,
		// never an outcome that could be mistaken for a healthy state.
		return CompositionPreviewResponse{}, fmt.Errorf("composition preview: compose operator tier: %w", err)
	}
	return buildPreview(tier, rev, set, widened), nil
}

// authorizeSessionReach applies the signed-session-reach gate when wired. A
// denial is loud (the settled reach contract); an unwired gate passes (the
// transport edge is the enforcement point) and the exact-triple boundary
// still holds below.
func (s *CompositionPreviewService) authorizeSessionReach(ctx context.Context, sessionID string) error {
	if s.sessionReach == nil {
		return nil
	}
	if err := s.sessionReach.AuthorizeSessionReach(ctx, sessionID); err != nil {
		return fmt.Errorf("%w: %w", ErrPreviewSessionReachDenied, err)
	}
	return nil
}

// authorizeAgentReach applies the signed effective-agent gate. Unwired it
// FAILS CLOSED — the runtime MUST wire the gate for agent-bound previews to
// be readable (an unwired gate is an honest "cannot verify reach", never a
// silent widening).
func (s *CompositionPreviewService) authorizeAgentReach(ctx context.Context, agentID string) error {
	if s.agentReach == nil {
		return fmt.Errorf("%w: agent reach gate not wired on this runtime", ErrPreviewAgentReachDenied)
	}
	if err := s.agentReach.AuthorizeAgentReach(ctx, agentID); err != nil {
		return fmt.Errorf("%w: %w", ErrPreviewAgentReachDenied, err)
	}
	return nil
}

// buildPreview projects the immutable composed tier onto the deterministic
// response: canonical-name order, canonical names + hashes, the exact
// boot|revision|both provenance markers, the three set hashes, and the fresh
// revision identity. Every item is a deep copy.
func buildPreview(tier sessionoverlay.OperatorTier, rev agentcfg.Revision, set bool, widened bool) CompositionPreviewResponse {
	resp := CompositionPreviewResponse{
		Outcome:         PreviewOutcomeAvailable,
		BootPackSetHash: tier.BootPackSetHash(),
		CombinedHash:    tier.CombinedHash(),
		RevisionHash:    tier.RevisionHash(),
		Widened:         widened,
		Items:           make([]CompositionPreviewItem, 0, tier.Len()),
	}
	if set {
		resp.RevisionID = rev.RevisionID
		resp.ContentHash = rev.ContentHash
	}
	for _, item := range tier.Items() {
		resp.Items = append(resp.Items, CompositionPreviewItem{
			Name:         canonicalPreviewName(item.Skill.Name),
			SemanticHash: item.SemanticHash,
			Source:       item.Source,
			Skill:        clonePreviewSkill(item.Skill),
		})
	}
	return resp
}

// unavailablePreview is the NON-ORACULAR outcome: byte-identical for a
// foreign triple, a cross-tenant target, an absent boot baseline AND absent
// active revision, and any other "nothing to preview" state.
func unavailablePreview(widened bool) CompositionPreviewResponse {
	return CompositionPreviewResponse{Outcome: PreviewOutcomeUnavailable, Widened: widened}
}

// retiredPreview is the typed terminal-lifecycle outcome.
func retiredPreview(widened bool) CompositionPreviewResponse {
	return CompositionPreviewResponse{Outcome: PreviewOutcomeRetired, Widened: widened}
}

// conflictPreview is the typed boot/revision conflict outcome. The composer
// remains the authoritative typed detector; ConflictName is deterministic
// enrichment (see previewConflictName).
func conflictPreview(widened bool, name string) CompositionPreviewResponse {
	return CompositionPreviewResponse{Outcome: PreviewOutcomeConflict, ConflictName: name, Widened: widened}
}

// previewConflictName deterministically names the first (canonical-sorted)
// operator-tier item whose canonical semantic content disagrees across (or
// within) the boot baseline and the active-revision inputs. It mirrors the
// strict composer's per-item normalization (JSON-canonical Extra) so the
// named set is exactly the set the composer refuses; the composer stays the
// authoritative typed detector — this helper only enriches the typed
// conflict outcome with a deterministic offending name.
func previewConflictName(boot []bootpacks.Entry, revision []skills.Skill) string {
	byName := make(map[string]map[string]struct{})
	add := func(skill skills.Skill) {
		name := canonicalPreviewName(skill.Name)
		if name == "" {
			return
		}
		normalized, err := normalizePreviewSkill(skill)
		if err != nil {
			return
		}
		if byName[name] == nil {
			byName[name] = make(map[string]struct{})
		}
		byName[name][skills.CanonicalContentHash(normalized)] = struct{}{}
	}
	for i := range boot {
		add(boot[i].Skill)
	}
	for i := range revision {
		add(revision[i])
	}
	names := make([]string, 0, len(byName))
	for name, hashes := range byName {
		if len(hashes) > 1 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// normalizePreviewSkill mirrors the strict composer's per-item normalization
// (validate + canonical-name check + JSON-canonical Extra) so the
// conflict-name scan compares the same hashes the composer compares.
func normalizePreviewSkill(skill skills.Skill) (skills.Skill, error) {
	if err := skill.Validate(); err != nil {
		return skills.Skill{}, err
	}
	if canonicalPreviewName(skill.Name) == "" {
		return skills.Skill{}, errors.New("skill name has no canonical form")
	}
	if skill.Extra == nil {
		return skill, nil
	}
	bytes, err := json.Marshal(skill.Extra)
	if err != nil {
		return skills.Skill{}, err
	}
	var normalized map[string]any
	if err := json.Unmarshal(bytes, &normalized); err != nil {
		return skills.Skill{}, err
	}
	skill.Extra = normalized
	return skill, nil
}

// canonicalPreviewName is the canonical (lowercase, trimmed) operator-tier
// name — the same identity the strict composer's merge and every set hash
// use.
func canonicalPreviewName(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// clonePreviewSkill returns a deep copy of a composed skill: every slice
// field and the full Extra tree are copied, so a caller's mutation of a
// returned item can never affect the service or another caller's copy.
func clonePreviewSkill(skill skills.Skill) skills.Skill {
	out := skill
	out.Tags = append([]string(nil), skill.Tags...)
	out.Steps = append([]string(nil), skill.Steps...)
	out.Preconditions = append([]string(nil), skill.Preconditions...)
	out.FailureModes = append([]string(nil), skill.FailureModes...)
	out.RequiredTools = append([]string(nil), skill.RequiredTools...)
	out.RequiredNS = append([]string(nil), skill.RequiredNS...)
	out.RequiredTags = append([]string(nil), skill.RequiredTags...)
	if len(skill.Extra) > 0 {
		out.Extra = clonePreviewExtra(skill.Extra)
	}
	return out
}

// clonePreviewExtra returns a deep copy of a normalized Extra map. The strict
// composer proves the only reachable values are nil, bool, string, float64,
// []any, and map[string]any, so cloning cannot recurse through cycles.
func clonePreviewExtra(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = clonePreviewExtraValue(value)
	}
	return out
}

func clonePreviewExtraValue(value any) any {
	switch typed := value.(type) {
	case nil, bool, string, float64:
		return typed
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = clonePreviewExtraValue(typed[i])
		}
		return out
	case map[string]any:
		return clonePreviewExtra(typed)
	default:
		// Unreachable after the composer's normalization; keeping the value
		// (rather than exposing a shared mutable reference) preserves the
		// immutable-output contract if an internal invariant ever regresses.
		return typed
	}
}

// CompositionPreviewAdminPayload is the typed SafePayload published on the
// canonical audit.admin_scope_used event when an elevated (admin or
// console:fleet) caller performs a widened composition preview of a
// same-tenant user. SafePayload by construction: every field is a bounded
// identity component, the effective agent id, and the Protocol method name —
// no caller-supplied bytes and no composition content reach the bus.
type CompositionPreviewAdminPayload struct {
	events.SafeSealed
	// Actor is the verified admin/fleet identity at the request edge.
	Actor identity.Identity
	// Target is the same-tenant triple whose composition was previewed.
	Target identity.Identity
	// AgentID is the effective boot-agent whose composition was previewed.
	AgentID string
	// Method is the Protocol method that carried the widened read.
	Method string
}

// emitWidenedAudit publishes the audit.admin_scope_used event recording a
// widened composition preview. It is called BEFORE any composition read. The
// bus + redactor are optional; when the bus is unsupplied the widened preview
// is logged at Info instead of published — the admin action is NEVER fully
// silent.
func (s *CompositionPreviewService) emitWidenedAudit(ctx context.Context, actor, target identity.Identity, agentID string) {
	logAttrs := []any{
		slog.String("method", compositionPreviewMethod),
		slog.String("actor_tenant_id", actor.TenantID),
		slog.String("actor_user_id", actor.UserID),
		slog.String("actor_session_id", actor.SessionID),
		slog.String("target_tenant_id", target.TenantID),
		slog.String("target_user_id", target.UserID),
		slog.String("target_session_id", target.SessionID),
		slog.String("agent_id", agentID),
	}
	if s.bus == nil {
		s.logger.InfoContext(ctx, "agentcfg/protocol: widened composition preview (bus not wired — audit logged only)", logAttrs...)
		return
	}
	payload := CompositionPreviewAdminPayload{Actor: actor, Target: target, AgentID: agentID, Method: compositionPreviewMethod}
	// Defence-in-depth: run the SafePayload through the redactor when one is
	// wired. A redaction error means "do not emit" — log loudly and fall
	// back, never publish unredacted.
	if s.redactor != nil {
		if _, err := s.redactor.Redact(ctx, payload); err != nil {
			s.logger.ErrorContext(ctx, "agentcfg/protocol: composition preview admin audit redaction failed — event NOT published",
				append(logAttrs, slog.String("error", err.Error()))...)
			return
		}
	}
	ev := events.Event{
		Type:       events.EventTypeAdminScopeUsed,
		Identity:   identity.Quadruple{Identity: actor},
		OccurredAt: time.Now().UTC(),
		Payload:    payload,
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		s.logger.WarnContext(ctx, "agentcfg/protocol: composition preview admin_scope_used emit failed",
			append(logAttrs, slog.String("error", err.Error()))...)
	}
}
