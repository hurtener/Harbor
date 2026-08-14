package protocol

// userskillimport.go — the verified-caller two-phase complete-skill-package
// import service backing `agent_config.user.skills.import_validate` and
// `agent_config.user.skills.import_commit`.
//
// A caller uploads a bounded complete skill package (a zip archive carrying
// exactly one root-level case-exact `SKILL.md`, or a single Markdown
// document) through the existing `artifacts.put` method under its verified
// (tenant, user, session) and receives an immutable content-addressed
// `ArtifactRef`. Validation accepts only that caller-owned artifact ref plus
// the effective `agent_id` — neither tenant nor user is selectable authority
// — and installs nothing: it runs the ONE production importer/validator,
// records a durable proposal, and returns the closed normalized review plus
// hashes and warnings. Commit echoes the proposal and the reviewed
// `PackageHash`, re-derives identity and signed effective-agent reach,
// rechecks lifecycle / policy / ceilings / expected config hash, forces
// `ScopeUser` + caller ownership server-side, and atomically materializes the
// approved package plus membership in ONE conditional `PutInstalledPackage`
// write (never a second legacy membership write). Response-loss retry is
// idempotent; competing commits have one winner across processes.
//
// # The seam (CLAUDE.md §4.4)
//
// The service depends on narrow injected seams — the production importer,
// the caller-owned ArtifactStore, a StateStore proposal ledger, the
// mandatory SkillStore package surface, a read-only agent-config reader
// (fresh user-scope active revision + retirement gate), and the capability
// policy projection. Tests inject fakes; the production assembler wires the
// real objects. The service holds no package-level mutable state, is
// immutable after construction, and is safe for N concurrent goroutines:
// per-call state lives in ctx and arguments, and cross-process serialization
// rides the durable conditional primitives (StateStore SaveIf + the
// SkillStore package CAS), never a process-local lock.
//
// # Authority model
//
//   - Identity is mandatory and comes from the VERIFIED context identity
//     (`identity.FromVerified`); the request carries no selectable
//     tenant/user/session/scope.
//   - The signed session-reach gate (a PRESENT session_reach claim must
//     contain the caller's session) and the signed effective-agent gate
//     (the agent must be in the caller's agent_reach; an unwired gate fails
//     closed) run BEFORE any artifact lookup, proposal lookup, skill-name
//     disclosure, lifecycle lookup, or persistence.
//   - The package frontmatter is CLOSED: the importer rejects every
//     authority-bearing field (scope, origin, tenant, user, agent,
//     authority, audience) and every unknown key, so caller YAML can never
//     set the storage/authority envelope. The commit request carries only
//     the opaque proposal id, the reviewed package hash, the expected
//     config hash, the reviewed canonical name, and the replace consent —
//     there is no body to submit back as authority.
//   - `required_tools` / `required_namespaces` / `required_tags` are
//     applicability metadata, never grants: a requirement outside the
//     effective agent's run-visible capability snapshot produces a WARNING
//     (the injection-time filter/redactor scrubs it) and the policy
//     SNAPSHOT + hash is bound to the proposal, so a policy change between
//     validate and commit is a typed revocation refusal.
//
// # The durable proposal state machine
//
//   - Validate saves the proposal with Phase "". ZERO durable
//     SkillStore/package or agent-config membership mutation happens.
//   - The first commit transitions the proposal to Phase "committing" via
//     StateStore SaveIf (the exact generation CAS — a concurrent commit of
//     the same proposal loses), records the exact package hash it is about
//     to write, and then performs THE ONE conditional
//     `PutInstalledPackage` write. A failed put is compensated by restoring
//     the proposal to Phase "" (the put is transactional, so no package or
//     membership state needs undoing). A successful put transitions the
//     proposal to Phase "committed", storing the exact receipt.
//   - Response-loss replay is idempotent: a retry that loads Phase
//     "committed" (or "committing" with the exact winner present) returns
//     the same terminal result WITHOUT a second package write. A retry that
//     loads Phase "committing" with a DIFFERENT winner refuses loudly — a
//     receipt/proposal never overwrites another commit's winner.
//   - Compensation is exact-receipt: the committed receipt names the exact
//     version written, and any conditional compensation through
//     `DeleteInstalledPackage` / `RestoreInstalledPackage` undoes ONLY that
//     complete unit/version. Because the package write IS the membership
//     (the stored ScopeUser row + the package + the support manifest are one
//     atomic unit), a compensation can never leave an orphan body or
//     membership, and this service never performs a second legacy
//     membership write.
//
// # Ceiling snapshot
//
// The proposal records the normalized archive / SKILL.md ceiling snapshot
// (the canonical skillpkg bounds). Commit re-parses under the current
// ceilings and refuses if the current effective ceilings differ from the
// reviewed snapshot — a package above the current ceilings never lands.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/importer"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
)

// User-skill-import sentinel errors. In-process callers compare with
// errors.Is; the wire handler maps each onto a canonical Protocol code.
var (
	// ErrUserSkillImportMisconfigured — the service was constructed without
	// a mandatory dependency (importer / artifact store / proposal ledger /
	// skill store / registry / capability policy).
	ErrUserSkillImportMisconfigured = errors.New("agentcfg/protocol: user skill import missing a mandatory dependency")
	// ErrUserSkillImportIdentityRequired — the request carried no verified
	// identity on ctx, an incomplete triple, or an empty effective agent id.
	ErrUserSkillImportIdentityRequired = errors.New("agentcfg/protocol: user skill import requires a complete verified identity and effective agent")
	// ErrUserSkillImportSessionReachDenied — a PRESENT signed session_reach
	// claim does not contain the caller's session.
	ErrUserSkillImportSessionReachDenied = errors.New("agentcfg/protocol: user skill import session reach denied")
	// ErrUserSkillImportAgentReachDenied — the effective agent is outside
	// the caller's signed agent_reach, or no reach is established on ctx
	// (the gate fails closed; an unwired gate is an honest "cannot verify
	// reach", never a silent widening).
	ErrUserSkillImportAgentReachDenied = errors.New("agentcfg/protocol: user skill import effective-agent reach denied")
	// ErrUserSkillImportArtifactNotFound — the artifact id does not resolve
	// under the caller's exact (tenant, user, session) triple. Non-oracular:
	// a foreign / erased / cross-session reference and a never-uploaded id
	// return the same typed not-found.
	ErrUserSkillImportArtifactNotFound = errors.New("agentcfg/protocol: user skill import artifact not found or not caller-owned")
	// ErrUserSkillImportArtifactChanged — the re-resolved artifact bytes do
	// not match the recorded digest/size the proposal pinned.
	ErrUserSkillImportArtifactChanged = errors.New("agentcfg/protocol: user skill import artifact changed after validation")
	// ErrUserSkillImportPackageInvalid — the artifact is not a valid
	// complete skill package (archive / path / MIME / SKILL.md / support-ref
	// / frontmatter violations, including every authority-bearing
	// frontmatter field). Wraps the canonical importer / skillpkg sentinel.
	ErrUserSkillImportPackageInvalid = errors.New("agentcfg/protocol: user skill import artifact is not a valid complete skill package")
	// ErrUserSkillImportProposalInvalid — the proposal id is unknown,
	// consumed, foreign, or bound to different server-side inputs (agent,
	// actor, name, reviewed hash, expected config hash).
	ErrUserSkillImportProposalInvalid = errors.New("agentcfg/protocol: invalid, consumed, foreign, or stale user skill import proposal")
	// ErrUserSkillImportExpired — the proposal's review window elapsed
	// before an explicit commit.
	ErrUserSkillImportExpired = errors.New("agentcfg/protocol: user skill import proposal expired")
	// ErrUserSkillImportHashMismatch — the reviewed package hash the commit
	// echoes does not equal the package the proposal pinned (a changed
	// review is refused before any write).
	ErrUserSkillImportHashMismatch = errors.New("agentcfg/protocol: user skill import reviewed package hash mismatch")
	// ErrUserSkillImportPolicyRevoked — the capability policy snapshot
	// changed between validate and commit (the review is no longer
	// authoritative).
	ErrUserSkillImportPolicyRevoked = errors.New("agentcfg/protocol: user skill import capability policy revoked (the reviewed snapshot changed)")
	// ErrUserSkillImportConfigMoved — the caller's user-scope config base
	// moved between validate and commit (the expected content hash is no
	// longer active).
	ErrUserSkillImportConfigMoved = errors.New("agentcfg/protocol: user skill import config base moved (the expected content hash is no longer active)")
	// ErrUserSkillImportCeilingChanged — the effective archive/SKILL.md
	// ceilings changed between validate and commit (the reviewed ceiling
	// snapshot is no longer current).
	ErrUserSkillImportCeilingChanged = errors.New("agentcfg/protocol: user skill import configured ceilings changed after validation")
	// ErrUserSkillImportReplaceRequired — a different package already wins
	// the target key and the commit did not carry explicit replacement
	// consent.
	ErrUserSkillImportReplaceRequired = errors.New("agentcfg/protocol: user skill import replacement requires explicit consent")
	// ErrUserSkillImportConcurrentWinner — the target key is held by a
	// different package version than the one this commit's proposal
	// reviewed/wrote. One winner only: this commit refuses rather than
	// overwrite.
	ErrUserSkillImportConcurrentWinner = errors.New("agentcfg/protocol: user skill import target is held by a different winner")
)

// userSkillImportProposalKindPrefix namespaces the durable proposal ledger
// slots. The proposal id is embedded in the Kind, so every proposal owns its
// own slot under the caller's identity (a proposal is never visible to a
// foreign triple — the store's identity key is the first fence).
const userSkillImportProposalKindPrefix = "agentcfg.user_skill_import.proposal."

// defaultUserSkillImportProposalTTL bounds the review window between
// validate and an explicit commit. A proposal past its ExpiresAt is refused
// on every commit path that would mutate.
const defaultUserSkillImportProposalTTL = 24 * time.Hour

// userSkillImportPolicyID / userSkillImportPolicyVersion identify the
// capability policy envelope the proposal binds by hash.
const (
	userSkillImportPolicyID      = "harbor.user-skill-import"
	userSkillImportPolicyVersion = "1"
)

// UserSkillImportPolicy is the server-owned capability snapshot the import
// validates RequiredTools / RequiredNS / RequiredTags against (as
// applicability metadata — non-fatal warnings) and the proposal binds by
// hash. The permitted sets are the effective agent's run-visible catalog
// projection — the SAME projection the operator pack authoring policy uses.
type UserSkillImportPolicy struct {
	// ID is the policy envelope identity.
	ID string `json:"id"`
	// Version is the policy envelope version.
	Version string `json:"version"`
	// PermittedTools are the run-visible tool names of the effective agent.
	PermittedTools []string `json:"permitted_tools,omitempty"`
	// PermittedNS are the run-visible namespaces (currently empty — the
	// catalog projection exposes tool names only; kept for the closed
	// shape so a future projection cannot widen the review).
	PermittedNS []string `json:"permitted_ns,omitempty"`
	// PermittedTags are the run-visible tags (same note as PermittedNS).
	PermittedTags []string `json:"permitted_tags,omitempty"`
}

// UserSkillImportCapability resolves the CURRENT capability policy snapshot
// for an effective agent under the caller's verified identity. The
// production adapter (agentConfigCapabilityPolicy) projects the configured
// tool catalog through the canonical ActivePlannerCatalogView; tests inject
// a fixed fake. The returned value is a deep copy.
type UserSkillImportCapability interface {
	Policy(ctx context.Context, id identity.Identity, agentID string) (UserSkillImportPolicy, error)
}

// agentConfigCapabilityPolicy is the production capability-policy adapter:
// it resolves the effective agent's run-visible tool set through the ONE
// canonical projection the run-start resolver uses. A nil catalog fails loud
// (no stub fallback).
type agentConfigCapabilityPolicy struct {
	registry      agentcfg.Registry
	overlay       sessionoverlay.Store
	catalog       tools.ToolCatalog
	grantedScopes []string
}

// Policy implements UserSkillImportCapability.
func (a agentConfigCapabilityPolicy) Policy(ctx context.Context, id identity.Identity, agentID string) (UserSkillImportPolicy, error) {
	if a.catalog == nil {
		return UserSkillImportPolicy{}, fmt.Errorf("%w: tool catalog is nil", ErrUserSkillImportMisconfigured)
	}
	view, err := projection.ActivePlannerCatalogView(ctx, a.registry, a.overlay, agentID, identity.Quadruple{Identity: id}, a.catalog, tools.CatalogFilter{
		TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID,
		GrantedScopes: a.grantedScopes,
		LoadingModes:  []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred},
	})
	if err != nil {
		return UserSkillImportPolicy{}, fmt.Errorf("user skill import: project capability snapshot: %w", err)
	}
	policy := UserSkillImportPolicy{ID: userSkillImportPolicyID, Version: userSkillImportPolicyVersion}
	for _, item := range view.List() {
		policy.PermittedTools = append(policy.PermittedTools, item.Name)
	}
	policy.PermittedTools = sortedSet(toSet(policy.PermittedTools))
	return policy, nil
}

// userSkillImportPolicyHash is the deterministic content hash of a policy
// snapshot (the proposal binding). Field order is fixed; unordered slices
// are sorted before hashing so ordering noise cannot perturb the binding.
func userSkillImportPolicyHash(p UserSkillImportPolicy) string {
	var b strings.Builder
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Quote(p.ID))
	b.WriteString(`,"version":`)
	b.WriteString(strconv.Quote(p.Version))
	b.WriteString(`,"permitted_tools":`)
	writeCanonicalStrings(&b, p.PermittedTools)
	b.WriteString(`,"permitted_ns":`)
	writeCanonicalStrings(&b, p.PermittedNS)
	b.WriteString(`,"permitted_tags":`)
	writeCanonicalStrings(&b, p.PermittedTags)
	b.WriteByte('}')
	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("sha256:%x", sum[:])
}

// UserSkillImportConfigReader is the read-only slice of the durable
// agent-config registry the import service needs: the FRESH active
// user-scope revision (the expected-config-hash base) plus the retirement
// gate. A RetirementRegistry satisfies it; the interface exists so tests
// inject a narrow fake and the production assembler wires the real registry.
// Both methods are reads — the import service structurally cannot write a
// config revision.
type UserSkillImportConfigReader interface {
	Active(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error)
	RetirementStatus(ctx context.Context, id identity.Quadruple, agentID string) (agentcfg.RetirementStatus, bool, error)
}

// UserSkillImportValidateRequest names the bounded input of the first phase:
// the caller-owned immutable artifact ref (the `artifacts.put` output under
// the caller's exact triple) and the effective agent. No tenant, user,
// session, scope, origin, or audience is selectable.
type UserSkillImportValidateRequest struct {
	// ArtifactID is the content-addressed ref of the caller-owned package
	// artifact (zip archive or single SKILL.md document).
	ArtifactID string
	// AgentID is the effective agent the reviewed package would be bound
	// to. Agent reach must be signed.
	AgentID string
}

// UserSkillImportSupportSummary is ONE bounded entry of the normalized
// support-manifest review: canonical path, MIME, exact size, digest.
type UserSkillImportSupportSummary struct {
	Path   string
	Mime   string
	Size   int64
	Digest string
}

// UserSkillImportReview is the closed, bounded, normalized review of one
// parsed package. Every field is server-derived from the canonical package;
// none of them can be submitted back to Commit as authority (Commit carries
// only the opaque proposal id, the reviewed hash, the expected config hash,
// the reviewed canonical name, and the replace consent).
type UserSkillImportReview struct {
	// Name is the CANONICAL package/skill name (the stored target-key
	// identity).
	Name string
	// Title is the human-readable title (may be empty).
	Title string
	// Trigger is the planner-visible match cue.
	Trigger string
	// TaskType is the planner-facing task class (may be empty).
	TaskType string
	// Tags are the search/classification tags.
	Tags []string
	// StepCount is the ordered procedural step count.
	StepCount int
	// RequiredTools / RequiredNS / RequiredTags are the applicability
	// metadata (never grants).
	RequiredTools []string
	RequiredNS    []string
	RequiredTags  []string
	// SupportFiles is the ordered normalized support manifest (canonical
	// path, MIME, exact size, digest per entry). A resource-free package
	// carries an empty manifest.
	SupportFiles []UserSkillImportSupportSummary
	// ContentHash is the canonical stored-row content hash of the skill
	// AS STORED (ScopeUser, effective agent, canonical name).
	ContentHash string
	// PackageHash is the versioned reviewed package hash
	// ("v1:<64-hex>") — the hash the caller reviews and Commit echoes.
	PackageHash string
}

// UserSkillImportValidateResponse is the first-phase outcome: the opaque
// durable proposal id, the closed review, the reviewed hashes, the expected
// config content hash the caller must echo on commit, the expiry, and the
// non-fatal warnings. Zero durable skill/package/membership mutation
// happened.
type UserSkillImportValidateResponse struct {
	// ProposalID is the opaque durable ledger key the commit echoes.
	ProposalID string
	// Review is the closed normalized review.
	Review UserSkillImportReview
	// Warnings are non-fatal review notes (e.g. a required tool that is
	// not currently run-visible — applicability metadata only).
	Warnings []string
	// PackageHash is the reviewed versioned package hash
	// (== Review.PackageHash).
	PackageHash string
	// ExpectedContentHash is the caller's user-scope config content hash
	// at validate time ("-" when the caller has no active user revision).
	// Commit requires the echo to match the proposal.
	ExpectedContentHash string
	// ExpiresAt bounds the review window; a commit after this time is
	// refused.
	ExpiresAt time.Time
}

// UserSkillImportCommitRequest is the bounded second-phase input: the
// proposal id, the reviewed package hash, the reviewed canonical name, the
// expected config content hash, and the explicit replacement consent.
type UserSkillImportCommitRequest struct {
	// ProposalID echoes the opaque proposal id from validate.
	ProposalID string
	// AgentID is the effective agent (must equal the proposal's).
	AgentID string
	// Name is the reviewed canonical package/skill name (must equal the
	// proposal's; used to address the target key).
	Name string
	// ReviewedPackageHash is the versioned package hash the caller
	// reviewed (must equal the proposal's).
	ReviewedPackageHash string
	// ExpectedContentHash echoes the expected config content hash from
	// validate (must equal the proposal's).
	ExpectedContentHash string
	// Replace is the explicit replacement consent. A different package
	// already at the target key is refused without it.
	Replace bool
}

// UserSkillImportCommitResponse is the terminal commit result: the exact
// versioned receipt of the ONE atomic package+membership write, the stored
// skill summary (deep copy), and the Replayed flag (true when the result was
// recognized from a prior landed commit rather than written by this call).
type UserSkillImportCommitResponse struct {
	// Receipt is the exact versioned receipt of the atomic write — the
	// conditional-compensation handle for THIS unit/version only.
	Receipt skills.InstalledPackageReceipt
	// Skill is the stored skill (ScopeUser, effective agent, canonical
	// name) as installed.
	Skill skills.Skill
	// PackageHash is the written versioned package hash.
	PackageHash string
	// Replayed is true when the terminal result was recognized from an
	// already-landed commit (response-loss replay) and no second package
	// write happened.
	Replayed bool
}

// UserSkillImportService implements the two-phase verified-caller import. It
// is immutable after construction and safe for concurrent reuse by N
// goroutines: it holds only the injected seams, the reach gates, and a clock
// + logger; per-call state lives in arguments, and cross-process
// serialization rides the durable conditional primitives.
type UserSkillImportService struct {
	importer     importer.Importer
	artifacts    artifacts.ArtifactStore
	proposals    state.StateStore
	store        skills.SkillStore
	registry     UserSkillImportConfigReader
	capability   UserSkillImportCapability
	agentReach   auth.AgentReachAuthorizer
	sessionReach auth.SessionReachAuthorizer
	proposalTTL  time.Duration
	logger       *slog.Logger
	now          Clock
}

// UserSkillImportOption configures NewUserSkillImportService.
type UserSkillImportOption func(*UserSkillImportService)

// WithImportSessionReach wires the canonical signed-session-reach gate. A
// PRESENT session_reach claim must contain the caller's session; an absent
// claim preserves dynamic selection. Unsupplied (or nil) leaves the
// transport edge as the enforcement point.
func WithImportSessionReach(a auth.SessionReachAuthorizer) UserSkillImportOption {
	return func(s *UserSkillImportService) {
		if a != nil {
			s.sessionReach = a
		}
	}
}

// WithImportAgentReach wires the canonical effective-agent gate. The
// effective agent must be a member of the caller's verified agent_reach.
// Unsupplied (or nil) FAILS CLOSED: no import is served (an unwired gate is
// an honest "cannot verify reach", never a silent widening). The production
// assembler wires auth.NewAgentReachAuthorizer().
func WithImportAgentReach(a auth.AgentReachAuthorizer) UserSkillImportOption {
	return func(s *UserSkillImportService) {
		if a != nil {
			s.agentReach = a
		}
	}
}

// WithImportProposalTTL bounds the review window between validate and an
// explicit commit. Unsupplied (or non-positive) uses the default TTL.
func WithImportProposalTTL(d time.Duration) UserSkillImportOption {
	return func(s *UserSkillImportService) {
		if d > 0 {
			s.proposalTTL = d
		}
	}
}

// WithImportClock injects the time source. Defaults to time.Now.
func WithImportClock(c Clock) UserSkillImportOption {
	return func(s *UserSkillImportService) {
		if c != nil {
			s.now = c
		}
	}
}

// WithImportLogger sets the slog.Logger. A nil logger routes to slog.Default().
func WithImportLogger(l *slog.Logger) UserSkillImportOption {
	return func(s *UserSkillImportService) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewUserSkillImportService builds the two-phase import service over the six
// mandatory seams: the production importer, the caller-owned ArtifactStore,
// the StateStore proposal ledger, the mandatory SkillStore package surface,
// the read-only agent-config reader, and the capability-policy projection. A
// nil seam fails loud with ErrUserSkillImportMisconfigured rather than
// building a service that would nil-panic on the first request. The returned
// *UserSkillImportService is immutable after construction and safe for
// concurrent use by N goroutines.
func NewUserSkillImportService(imp importer.Importer, artifactStore artifacts.ArtifactStore, proposals state.StateStore, store skills.SkillStore, registry UserSkillImportConfigReader, capability UserSkillImportCapability, opts ...UserSkillImportOption) (*UserSkillImportService, error) {
	if imp == nil {
		return nil, fmt.Errorf("%w: importer is nil", ErrUserSkillImportMisconfigured)
	}
	if artifactStore == nil {
		return nil, fmt.Errorf("%w: artifact store is nil", ErrUserSkillImportMisconfigured)
	}
	if proposals == nil {
		return nil, fmt.Errorf("%w: proposal ledger is nil", ErrUserSkillImportMisconfigured)
	}
	if store == nil {
		return nil, fmt.Errorf("%w: skill store is nil", ErrUserSkillImportMisconfigured)
	}
	if registry == nil {
		return nil, fmt.Errorf("%w: agent-config reader is nil", ErrUserSkillImportMisconfigured)
	}
	if capability == nil {
		return nil, fmt.Errorf("%w: capability policy is nil", ErrUserSkillImportMisconfigured)
	}
	s := &UserSkillImportService{
		importer: imp, artifacts: artifactStore, proposals: proposals, store: store,
		registry: registry, capability: capability,
		proposalTTL: defaultUserSkillImportProposalTTL,
		logger:      slog.Default(),
		now:         time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Validate performs the ZERO-mutation first phase: verified identity +
// signed reach, retirement gate, the caller-owned immutable artifact read,
// THE production importer/validator parse, the capability-policy review
// (warnings, never grants), the user-scope config base snapshot, and the
// durable proposal write. No SkillStore body/package write and no
// agent-config membership write happens.
func (s *UserSkillImportService) Validate(ctx context.Context, req UserSkillImportValidateRequest) (UserSkillImportValidateResponse, error) {
	if err := ctx.Err(); err != nil {
		return UserSkillImportValidateResponse{}, err
	}
	id, q, err := s.resolveIdentityAndReach(ctx, req.AgentID)
	if err != nil {
		return UserSkillImportValidateResponse{}, err
	}
	if strings.TrimSpace(req.ArtifactID) == "" {
		return UserSkillImportValidateResponse{}, fmt.Errorf("%w: artifact_id is empty", ErrUserSkillImportIdentityRequired)
	}
	if err := s.ensureNotRetiredImport(ctx, q, req.AgentID); err != nil {
		return UserSkillImportValidateResponse{}, err
	}

	bytes, found, err := s.artifacts.Get(ctx, artifactScope(id), req.ArtifactID)
	if err != nil {
		return UserSkillImportValidateResponse{}, fmt.Errorf("user skill import: read artifact: %w", err)
	}
	if !found {
		return UserSkillImportValidateResponse{}, fmt.Errorf("%w: artifact_id=%q under the caller's exact triple",
			ErrUserSkillImportArtifactNotFound, req.ArtifactID)
	}

	ingest, err := s.importer.ImportPackage(ctx, importer.PackageSource{Archive: bytes, PathHint: req.ArtifactID})
	if err != nil {
		return UserSkillImportValidateResponse{}, fmt.Errorf("%w: %w", ErrUserSkillImportPackageInvalid, err)
	}

	stored := storedSkillPreview(ingest, req.AgentID)
	unit := installedUnit(ingest, req.AgentID)
	if err := skills.ValidateInstalledPackage(unit); err != nil {
		return UserSkillImportValidateResponse{}, fmt.Errorf("%w: %w", ErrUserSkillImportPackageInvalid, err)
	}

	// Boot-owned canonical names are read-only to every control plane:
	// refuse BEFORE a proposal exists (the commit re-checks before any
	// write — a boot baseline wins even at equal hash).
	if err := guardBootOwnedName(bootOwnershipFromContext(ctx), q.TenantID, req.AgentID, ingest.Package.Name); err != nil {
		return UserSkillImportValidateResponse{}, err
	}

	policy, err := s.capability.Policy(ctx, id, req.AgentID)
	if err != nil {
		return UserSkillImportValidateResponse{}, err
	}
	warnings := userSkillImportWarnings(stored, policy, req.AgentID)

	expectedContentHash, err := s.expectedUserConfigHash(ctx, q, req.AgentID)
	if err != nil {
		return UserSkillImportValidateResponse{}, err
	}

	expiresAt := s.now().UTC().Add(s.proposalTTL)
	proposalID := state.NewEventID()
	record := userSkillImportProposalRecord{
		AgentID:             req.AgentID,
		ArtifactID:          req.ArtifactID,
		ArtifactSHA256:      sha256Hex(bytes),
		ArtifactSizeBytes:   int64(len(bytes)),
		Name:                ingest.Package.Name,
		PackageHash:         ingest.Hash,
		ContentHash:         stored.ContentHash,
		ExpectedContentHash: expectedContentHash,
		Policy:              policy,
		PolicyHash:          userSkillImportPolicyHash(policy),
		ArchiveLimits:       skillpkg.ArchiveLimits{}.Normalize(),
		MarkdownLimits:      skillpkg.MarkdownLimits{}.Normalize(),
		Actor:               id,
		CreatedAt:           s.now().UTC(),
		ExpiresAt:           expiresAt,
	}
	recordBytes, err := marshalUserSkillImportProposal(record)
	if err != nil {
		return UserSkillImportValidateResponse{}, fmt.Errorf("user skill import: encode proposal: %w", err)
	}
	if err := s.proposals.Save(ctx, state.StateRecord{
		ID: proposalID, Identity: q, Kind: userSkillImportProposalKind(string(proposalID)), Bytes: recordBytes,
	}); err != nil {
		return UserSkillImportValidateResponse{}, fmt.Errorf("user skill import: save proposal: %w", err)
	}

	return UserSkillImportValidateResponse{
		ProposalID:          string(proposalID),
		Review:              buildUserSkillImportReview(ingest, stored),
		Warnings:            warnings,
		PackageHash:         ingest.Hash,
		ExpectedContentHash: expectedContentHash,
		ExpiresAt:           expiresAt,
	}, nil
}

// Commit performs the explicit second phase: it reloads the durable proposal
// and refuses every stale form (unknown/foreign/consumed id, actor/agent/
// name/hash/config-base mismatch, expiry, lost reach, policy revocation,
// changed ceilings, boot-owned name), re-resolves the exact immutable
// artifact and repeats full validation, forces ScopeUser + caller ownership,
// and then performs THE ONE conditional PutInstalledPackage write (the
// atomic package+membership unit). Response-loss replay returns the same
// terminal result without a second write; a competing winner is never
// overwritten.
func (s *UserSkillImportService) Commit(ctx context.Context, req UserSkillImportCommitRequest) (UserSkillImportCommitResponse, error) {
	if err := ctx.Err(); err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	id, q, err := s.resolveIdentityAndReach(ctx, req.AgentID)
	if err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	if strings.TrimSpace(req.ProposalID) == "" {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: proposal_id is empty", ErrUserSkillImportProposalInvalid)
	}
	if strings.TrimSpace(req.ReviewedPackageHash) == "" {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: reviewed package hash is empty", ErrUserSkillImportProposalInvalid)
	}
	if strings.TrimSpace(req.Name) == "" {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: reviewed name is empty", ErrUserSkillImportProposalInvalid)
	}
	if err := s.ensureNotRetiredImport(ctx, q, req.AgentID); err != nil {
		return UserSkillImportCommitResponse{}, err
	}

	kind := userSkillImportProposalKind(req.ProposalID)
	record, err := s.proposals.Load(ctx, q, kind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return UserSkillImportCommitResponse{}, ErrUserSkillImportProposalInvalid
		}
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: load proposal: %w", err)
	}
	proposal, err := unmarshalUserSkillImportProposal(record.Bytes)
	if err != nil {
		return UserSkillImportCommitResponse{}, ErrUserSkillImportProposalInvalid
	}
	if err := s.verifyProposalBindings(id, req, proposal); err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	// A boot-declared canonical name is read-only to every control
	// plane. The commit refuses it on EVERY path — initial commit,
	// response-loss replay, and prepared/committing resume alike — so
	// no proposal can ever land a boot-owned name (even at equal hash).
	if err := guardBootOwnedName(bootOwnershipFromContext(ctx), q.TenantID, req.AgentID, proposal.Name); err != nil {
		return UserSkillImportCommitResponse{}, err
	}

	switch proposal.Phase {
	case userSkillImportPhaseCommitted:
		return s.replayCommitted(ctx, q, req, proposal)
	case userSkillImportPhaseCommitting:
		return s.resumeCommitting(ctx, q, req, proposal, record.ID)
	default:
		return s.commitFresh(ctx, q, id, req, proposal, record.ID)
	}
}

// userSkillImportProposalPhase values.
const (
	// userSkillImportPhaseReview is the post-validate state: no write has
	// been attempted.
	userSkillImportPhaseReview = ""
	// userSkillImportPhaseCommitting is the pre-write marker: the commit
	// CAS-binds the exact package hash it is about to write.
	userSkillImportPhaseCommitting = "committing"
	// userSkillImportPhaseCommitted is the terminal state: the atomic
	// write landed and the exact receipt is recorded.
	userSkillImportPhaseCommitted = "committed"
)

// userSkillImportProposalRecord is the durable proposal payload: actor +
// effective-agent binding, source reference + digest, versioned PackageHash,
// expected config content hash, the capability-policy snapshot + hash, the
// configured-ceiling snapshot, expiry, and the phase machine state. Raw
// package bytes are never duplicated into the record.
type userSkillImportProposalRecord struct {
	AgentID             string                          `json:"agent_id"`
	ArtifactID          string                          `json:"artifact_id"`
	ArtifactSHA256      string                          `json:"artifact_sha256"`
	ArtifactSizeBytes   int64                           `json:"artifact_size_bytes"`
	Name                string                          `json:"name"`
	PackageHash         string                          `json:"package_hash"`
	ContentHash         string                          `json:"content_hash"`
	ExpectedContentHash string                          `json:"expected_content_hash"`
	Policy              UserSkillImportPolicy           `json:"policy"`
	PolicyHash          string                          `json:"policy_hash"`
	ArchiveLimits       skillpkg.ArchiveLimits          `json:"archive_limits"`
	MarkdownLimits      skillpkg.MarkdownLimits         `json:"markdown_limits"`
	Actor               identity.Identity               `json:"actor"`
	CreatedAt           time.Time                       `json:"created_at"`
	ExpiresAt           time.Time                       `json:"expires_at"`
	Phase               string                          `json:"phase,omitempty"`
	WrittenPackageHash  string                          `json:"written_package_hash,omitempty"`
	Receipt             *skills.InstalledPackageReceipt `json:"receipt,omitempty"`
}

// userSkillImportProposalKind derives the durable ledger slot of a proposal
// id.
func userSkillImportProposalKind(id string) string { return userSkillImportProposalKindPrefix + id }

func marshalUserSkillImportProposal(r userSkillImportProposalRecord) ([]byte, error) {
	return json.Marshal(r)
}

func unmarshalUserSkillImportProposal(b []byte) (userSkillImportProposalRecord, error) {
	var r userSkillImportProposalRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("decode user skill import proposal: %w", err)
	}
	return r, nil
}

// resolveIdentityAndReach resolves the verified caller identity from ctx and
// applies the two signed-reach gates: the session gate (a PRESENT
// session_reach claim must contain the caller's session) and the effective
// agent gate (fails closed when unwired or when the agent is outside the
// caller's reach). It runs BEFORE any artifact lookup, proposal lookup,
// skill-name disclosure, lifecycle lookup, or persistence.
func (s *UserSkillImportService) resolveIdentityAndReach(ctx context.Context, agentID string) (identity.Identity, identity.Quadruple, error) {
	caller, ok := identity.FromVerified(ctx)
	if !ok {
		return identity.Identity{}, identity.Quadruple{}, ErrUserSkillImportIdentityRequired
	}
	if err := identity.Validate(caller); err != nil {
		return identity.Identity{}, identity.Quadruple{}, fmt.Errorf("%w: %w", ErrUserSkillImportIdentityRequired, err)
	}
	if strings.TrimSpace(agentID) == "" {
		return identity.Identity{}, identity.Quadruple{}, fmt.Errorf("%w: effective agent id is empty", ErrUserSkillImportIdentityRequired)
	}
	if s.sessionReach != nil {
		if err := s.sessionReach.AuthorizeSessionReach(ctx, caller.SessionID); err != nil {
			return identity.Identity{}, identity.Quadruple{}, fmt.Errorf("%w: %w", ErrUserSkillImportSessionReachDenied, err)
		}
	}
	if s.agentReach == nil {
		return identity.Identity{}, identity.Quadruple{}, fmt.Errorf("%w: agent reach gate not wired on this runtime", ErrUserSkillImportAgentReachDenied)
	}
	if err := s.agentReach.AuthorizeAgentReach(ctx, agentID); err != nil {
		return identity.Identity{}, identity.Quadruple{}, fmt.Errorf("%w: %w", ErrUserSkillImportAgentReachDenied, err)
	}
	return caller, identity.Quadruple{Identity: caller}, nil
}

// ensureNotRetiredImport refuses every import operation once the effective
// agent's terminal lifecycle tombstone is installed.
func (s *UserSkillImportService) ensureNotRetiredImport(ctx context.Context, q identity.Quadruple, agentID string) error {
	_, retired, err := s.registry.RetirementStatus(ctx, q, agentID)
	if err != nil {
		return fmt.Errorf("user skill import: retirement gate: %w", err)
	}
	if retired {
		return agentcfg.ErrAgentRetired
	}
	return nil
}

// expectedUserConfigHash snapshots the caller's user-scope config base: the
// active revision's content hash, or the ExpectNoActiveRevision sentinel when
// the caller has no user revision yet.
func (s *UserSkillImportService) expectedUserConfigHash(ctx context.Context, q identity.Quadruple, agentID string) (string, error) {
	active, hasActive, err := s.registry.Active(ctx, q, agentID, agentcfg.ConfigScopeUser)
	if err != nil {
		return "", fmt.Errorf("user skill import: read expected config base: %w", err)
	}
	if !hasActive {
		return agentcfg.ExpectNoActiveRevision, nil
	}
	return active.ContentHash, nil
}

// verifyProposalBindings refuses every stale / foreign / mismatched proposal
// form: the recorded actor must equal the caller's exact triple, the agent
// and reviewed name must match, the reviewed package hash and the expected
// config hash must echo the proposal exactly.
func (s *UserSkillImportService) verifyProposalBindings(id identity.Identity, req UserSkillImportCommitRequest, proposal userSkillImportProposalRecord) error {
	if proposal.Actor != id {
		return fmt.Errorf("%w: proposal actor %+v does not match caller %+v", ErrUserSkillImportProposalInvalid, proposal.Actor, id)
	}
	if proposal.AgentID != req.AgentID {
		return fmt.Errorf("%w: proposal agent %q does not match request agent %q", ErrUserSkillImportProposalInvalid, proposal.AgentID, req.AgentID)
	}
	if proposal.Name != req.Name {
		return fmt.Errorf("%w: proposal name %q does not match request name %q", ErrUserSkillImportProposalInvalid, proposal.Name, req.Name)
	}
	if proposal.PackageHash != req.ReviewedPackageHash {
		return fmt.Errorf("%w: proposal reviewed hash %q does not match request hash %q", ErrUserSkillImportHashMismatch, proposal.PackageHash, req.ReviewedPackageHash)
	}
	if proposal.ExpectedContentHash != req.ExpectedContentHash {
		return fmt.Errorf("%w: proposal expected config hash %q does not match request hash %q", ErrUserSkillImportProposalInvalid, proposal.ExpectedContentHash, req.ExpectedContentHash)
	}
	return nil
}

// commitFresh runs the full explicit commit for a proposal still in the
// review phase: expiry, artifact re-resolution, full re-validation, policy /
// config / ceiling rechecks, the CAS transition to "committing", THE ONE
// atomic package write, and the terminal transition to "committed".
func (s *UserSkillImportService) commitFresh(ctx context.Context, q identity.Quadruple, id identity.Identity, req UserSkillImportCommitRequest, proposal userSkillImportProposalRecord, slotID state.EventID) (UserSkillImportCommitResponse, error) {
	now := s.now().UTC()
	if !now.Before(proposal.ExpiresAt) {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: proposal %q expired at %s", ErrUserSkillImportExpired, req.ProposalID, proposal.ExpiresAt.Format(time.RFC3339))
	}

	// Re-resolve the exact immutable caller-owned artifact and repeat the
	// FULL validation: a moved / changed / erased source is refused before
	// any write.
	bytes, found, err := s.artifacts.Get(ctx, artifactScope(id), proposal.ArtifactID)
	if err != nil {
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: re-read artifact: %w", err)
	}
	if !found {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: artifact_id=%q", ErrUserSkillImportArtifactNotFound, proposal.ArtifactID)
	}
	if sha256Hex(bytes) != proposal.ArtifactSHA256 || int64(len(bytes)) != proposal.ArtifactSizeBytes {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: artifact_id=%q", ErrUserSkillImportArtifactChanged, proposal.ArtifactID)
	}
	ingest, err := s.importer.ImportPackage(ctx, importer.PackageSource{Archive: bytes, PathHint: proposal.ArtifactID})
	if err != nil {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: %w", ErrUserSkillImportPackageInvalid, err)
	}
	if ingest.Hash != proposal.PackageHash {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: re-parsed package hash %q != reviewed %q (the reviewed package changed)",
			ErrUserSkillImportHashMismatch, ingest.Hash, proposal.PackageHash)
	}
	if ingest.Package.Name != proposal.Name {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: re-parsed name %q != reviewed %q (the reviewed package changed)",
			ErrUserSkillImportHashMismatch, ingest.Package.Name, proposal.Name)
	}

	// Policy revocation: the reviewed capability snapshot must still be
	// current.
	policy, err := s.capability.Policy(ctx, id, req.AgentID)
	if err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	if proposal.PolicyHash == "" || userSkillImportPolicyHash(policy) != proposal.PolicyHash {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: reviewed=%s current=%s", ErrUserSkillImportPolicyRevoked, proposal.PolicyHash, userSkillImportPolicyHash(policy))
	}

	// Moved config base: the caller's user-scope revision must still carry
	// the reviewed content hash.
	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeUser)
	if err != nil {
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: re-read config base: %w", err)
	}
	if err := agentcfg.CheckExpectedRevision(agentcfg.SetOptions{ExpectedContentHash: proposal.ExpectedContentHash}, active, hasActive); err != nil {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: %w", ErrUserSkillImportConfigMoved, err)
	}

	// Changed configured ceilings: the effective archive/SKILL.md bounds
	// must still equal the reviewed snapshot.
	if err := s.verifyCeilingSnapshot(proposal); err != nil {
		return UserSkillImportCommitResponse{}, err
	}

	unit := installedUnit(ingest, req.AgentID)
	if err := skills.ValidateInstalledPackage(unit); err != nil {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: %w", ErrUserSkillImportPackageInvalid, err)
	}

	// Determine the exact conditional write against the CURRENT winner:
	// create-only on an absent key; an explicit-replace CAS on a
	// different winner. The store's origin-precedence gate still applies
	// (pack input replacing pack/generated is the only reachable pair —
	// the importer fixes Origin to pack).
	cond, replace, err := s.packagePutCondition(ctx, q, req, unit)
	if err != nil {
		return UserSkillImportCommitResponse{}, err
	}

	// CAS transition to "committing": the exact pre-write marker records
	// the hash about to be written. A concurrent commit of the same
	// proposal loses this SaveIf and is refused; its retry then resumes
	// onto the winner's terminal state.
	committingID := state.NewEventID()
	committingProposal := proposal
	committingProposal.Phase = userSkillImportPhaseCommitting
	committingProposal.WrittenPackageHash = ingest.Hash
	committingBytes, err := marshalUserSkillImportProposal(committingProposal)
	if err != nil {
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: encode committing proposal: %w", err)
	}
	if err := s.proposals.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: userSkillImportProposalKind(req.ProposalID), ExpectedEventID: slotID}}, state.StateRecord{
		ID: committingID, Identity: q, Kind: userSkillImportProposalKind(req.ProposalID), Bytes: committingBytes,
	}); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return UserSkillImportCommitResponse{}, fmt.Errorf("%w: a concurrent commit of proposal %q is in progress", ErrUserSkillImportProposalInvalid, req.ProposalID)
		}
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: mark proposal committing: %w", err)
	}

	receipt, err := s.store.PutInstalledPackage(ctx, q, req.AgentID, unit, cond, replace)
	if err != nil {
		// The put is transactional: a failure means NO package state
		// changed. Restore the proposal to the review phase so the caller
		// can retry (e.g. with replacement consent) or re-validate.
		restoreErr := s.restoreProposal(ctx, q, req.ProposalID, committingID, proposal)
		if restoreErr != nil {
			s.logger.ErrorContext(ctx, "user skill import: compensate committing proposal failed",
				"proposal_id", req.ProposalID, "cause", err.Error(), "restore_error", restoreErr.Error())
			return UserSkillImportCommitResponse{}, errors.Join(fmt.Errorf("user skill import: put installed package: %w", err), restoreErr)
		}
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: put installed package: %w", err)
	}
	if receipt.WrittenHash != ingest.Hash {
		s.logger.ErrorContext(ctx, "user skill import: receipt hash diverged from reviewed package",
			"proposal_id", req.ProposalID, "receipt", receipt.WrittenHash, "reviewed", ingest.Hash)
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: receipt hash %q != reviewed %q", ErrUserSkillImportProposalInvalid, receipt.WrittenHash, ingest.Hash)
	}

	// Terminal transition: record the exact receipt. If this bookkeeping
	// write fails because a concurrent resume recorded the same terminal
	// state first, the package IS installed with the reviewed hash and the
	// terminal result is legitimate regardless of who recorded it. A
	// genuine store failure surfaces loud; a retry recognizes the terminal
	// state via the committing marker and returns the same result.
	committedProposal := proposal
	committedProposal.Phase = userSkillImportPhaseCommitted
	committedProposal.WrittenPackageHash = ingest.Hash
	committedProposal.Receipt = &receipt
	committedBytes, err := marshalUserSkillImportProposal(committedProposal)
	if err != nil {
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: encode committed proposal: %w", err)
	}
	if err := s.proposals.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: userSkillImportProposalKind(req.ProposalID), ExpectedEventID: committingID}}, state.StateRecord{
		ID: state.NewEventID(), Identity: q, Kind: userSkillImportProposalKind(req.ProposalID), Bytes: committedBytes,
	}); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			resp, terr := s.terminalResponse(ctx, q, req, receipt)
			resp.Replayed = true
			return resp, terr
		}
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: record committed proposal: %w (the package IS installed; a retry returns the terminal result)", err)
	}
	return s.terminalResponse(ctx, q, req, receipt)
}

// packagePutCondition derives the exact conditional write predicate from the
// CURRENT winner at the target key: an absent key is create-only; the exact
// reviewed package already installed is an idempotent no-op (the store's
// exact-replay branch); a DIFFERENT winner requires explicit replacement
// consent and CAS-binds the winner's hash.
func (s *UserSkillImportService) packagePutCondition(ctx context.Context, q identity.Quadruple, req UserSkillImportCommitRequest, unit skills.InstalledPackage) (skills.InstalledPackageCondition, bool, error) {
	winner, found, err := s.currentWinner(ctx, q, req.AgentID, unit.Package.Name)
	if err != nil {
		return skills.InstalledPackageCondition{}, false, err
	}
	switch {
	case !found:
		return skills.InstalledPackageCondition{ExpectedAbsent: true}, false, nil
	case winner.PackageHash == unit.PackageHash:
		// The exact reviewed package is already the winner: the put is an
		// idempotent no-op regardless of replace (the store's exact-replay
		// branch), so no consent is needed — there is nothing to replace.
		return skills.InstalledPackageCondition{ExpectedHash: winner.PackageHash}, false, nil
	case !req.Replace:
		return skills.InstalledPackageCondition{}, false, fmt.Errorf("%w: name=%q winner=%q reviewed=%q",
			ErrUserSkillImportReplaceRequired, unit.Package.Name, winner.PackageHash, unit.PackageHash)
	default:
		return skills.InstalledPackageCondition{ExpectedHash: winner.PackageHash}, true, nil
	}
}

// currentWinner reads the installed package at the session-zeroed
// (tenant, user, effective-agent, name) key, reporting absence distinctly
// from a read failure.
func (s *UserSkillImportService) currentWinner(ctx context.Context, q identity.Quadruple, agentID, name string) (skills.InstalledPackage, bool, error) {
	winner, err := s.store.GetInstalledPackage(ctx, q, agentID, name)
	if err != nil {
		if errors.Is(err, skills.ErrInstalledPackageNotFound) {
			return skills.InstalledPackage{}, false, nil
		}
		return skills.InstalledPackage{}, false, fmt.Errorf("user skill import: read target winner: %w", err)
	}
	return winner, true, nil
}

// resumeCommitting handles a commit retry whose proposal is mid-flight: if
// the exact written package is the winner, the commit landed (response-loss)
// and the same terminal result is returned without a second write; a
// different winner is refused (one winner); an absent winner means the
// reviewed write never landed — the caller re-validates (a stale committing
// marker is never silently converted into a re-install).
func (s *UserSkillImportService) resumeCommitting(ctx context.Context, q identity.Quadruple, req UserSkillImportCommitRequest, proposal userSkillImportProposalRecord, slotID state.EventID) (UserSkillImportCommitResponse, error) {
	winner, found, err := s.currentWinner(ctx, q, req.AgentID, proposal.Name)
	if err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	switch {
	case found && winner.PackageHash == proposal.WrittenPackageHash:
		// The write landed but the terminal receipt was never returned (or
		// the committed transition failed): recognize the terminal state
		// and record it, then return the same result.
		receipt := receiptFromWinner(q, req.AgentID, proposal.Name, winner)
		committedProposal := proposal
		committedProposal.Phase = userSkillImportPhaseCommitted
		committedProposal.Receipt = &receipt
		bytes, err := marshalUserSkillImportProposal(committedProposal)
		if err != nil {
			return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: encode committed proposal: %w", err)
		}
		if err := s.proposals.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: userSkillImportProposalKind(req.ProposalID), ExpectedEventID: slotID}}, state.StateRecord{
			ID: state.NewEventID(), Identity: q, Kind: userSkillImportProposalKind(req.ProposalID), Bytes: bytes,
		}); err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				// A concurrent resume recorded the same terminal state
				// first: the package IS installed with the reviewed hash,
				// so the terminal result is legitimate regardless of who
				// recorded it.
				resp, terr := s.terminalResponse(ctx, q, req, receipt)
				resp.Replayed = true
				return resp, terr
			}
			return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: record committed proposal: %w (the package IS installed; a retry returns the terminal result)", err)
		}
		resp, err := s.terminalResponse(ctx, q, req, receipt)
		resp.Replayed = true
		return resp, err
	case found:
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: name=%q winner=%q written=%q",
			ErrUserSkillImportConcurrentWinner, proposal.Name, winner.PackageHash, proposal.WrittenPackageHash)
	default:
		// The reviewed write never landed (a failed put whose inline
		// compensation also failed, or a retry racing a put that errored).
		// Refuse loudly rather than re-installing under a stale marker:
		// the caller re-validates for a fresh proposal.
		return UserSkillImportCommitResponse{}, ErrUserSkillImportProposalInvalid
	}
}

// replayCommitted handles a retry whose proposal already recorded the
// terminal receipt: the exact written package must still be the winner and
// the same terminal result is returned without any write.
func (s *UserSkillImportService) replayCommitted(ctx context.Context, q identity.Quadruple, req UserSkillImportCommitRequest, proposal userSkillImportProposalRecord) (UserSkillImportCommitResponse, error) {
	if proposal.Receipt == nil {
		return UserSkillImportCommitResponse{}, ErrUserSkillImportProposalInvalid
	}
	winner, found, err := s.currentWinner(ctx, q, req.AgentID, proposal.Name)
	if err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	if !found || winner.PackageHash != proposal.Receipt.WrittenHash {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: name=%q written=%q",
			ErrUserSkillImportConcurrentWinner, proposal.Name, proposal.Receipt.WrittenHash)
	}
	resp, err := s.terminalResponse(ctx, q, req, *proposal.Receipt)
	resp.Replayed = true
	return resp, err
}

// restoreProposal compensates a failed fresh-commit put by restoring the
// proposal slot to the review phase (the pre-committing bytes). The put is
// transactional, so no package or membership state needs undoing — the
// restoration is the whole compensation.
func (s *UserSkillImportService) restoreProposal(ctx context.Context, q identity.Quadruple, proposalID string, committingID state.EventID, original userSkillImportProposalRecord) error {
	restore := original
	restore.Phase = userSkillImportPhaseReview
	restore.WrittenPackageHash = ""
	restore.Receipt = nil
	bytes, err := marshalUserSkillImportProposal(restore)
	if err != nil {
		return fmt.Errorf("encode restored proposal: %w", err)
	}
	if err := s.proposals.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: userSkillImportProposalKind(proposalID), ExpectedEventID: committingID}}, state.StateRecord{
		ID: state.NewEventID(), Identity: q, Kind: userSkillImportProposalKind(proposalID), Bytes: bytes,
	}); err != nil {
		return err
	}
	return nil
}

// terminalResponse builds the terminal commit result from the exact receipt
// and the stored winner (the authoritative deep-copied unit).
func (s *UserSkillImportService) terminalResponse(ctx context.Context, q identity.Quadruple, req UserSkillImportCommitRequest, receipt skills.InstalledPackageReceipt) (UserSkillImportCommitResponse, error) {
	winner, found, err := s.currentWinner(ctx, q, req.AgentID, receipt.Name)
	if err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	if !found || winner.PackageHash != receipt.WrittenHash {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: name=%q written=%q",
			ErrUserSkillImportConcurrentWinner, receipt.Name, receipt.WrittenHash)
	}
	return UserSkillImportCommitResponse{
		Receipt:     receipt,
		Skill:       cloneStoredSkill(winner.Skill),
		PackageHash: winner.PackageHash,
	}, nil
}

// verifyCeilingSnapshot refuses a commit whose current effective archive /
// SKILL.md ceilings differ from the reviewed snapshot (a changed configured
// ceiling is a changed review).
func (s *UserSkillImportService) verifyCeilingSnapshot(proposal userSkillImportProposalRecord) error {
	currentArchive := skillpkg.ArchiveLimits{}.Normalize()
	currentMarkdown := skillpkg.MarkdownLimits{}.Normalize()
	if proposal.ArchiveLimits != currentArchive || proposal.MarkdownLimits != currentMarkdown {
		return fmt.Errorf("%w: reviewed archive=%+v markdown=%+v current archive=%+v markdown=%+v",
			ErrUserSkillImportCeilingChanged, proposal.ArchiveLimits, proposal.MarkdownLimits, currentArchive, currentMarkdown)
	}
	return nil
}

// storedSkillPreview projects the parsed package onto the STORED skill form
// the atomic unit commits: the canonical package name (the target-key
// identity), the effective agent forced server-side, ScopeUser forced (the
// durable user rung), Origin fixed to pack, and the canonical stored
// ContentHash recomputed over that exact stored envelope. The review and the
// installed unit share this one projection so what is reviewed is exactly
// what is stored.
func storedSkillPreview(ingest importer.PackageIngest, agentID string) skills.Skill {
	skill := cloneStoredSkill(ingest.Skill)
	skill.Name = ingest.Package.Name
	skill.AgentID = agentID
	skill.Scope = skills.ScopeUser
	skill.Origin = skills.OriginPack
	skill.ContentHash = skills.CanonicalContentHash(skill)
	return skill
}

// installedUnit builds the atomic installed-package unit: the stored skill
// plus the canonical package (with its bounded immutable support bytes) plus
// the versioned PackageHash.
func installedUnit(ingest importer.PackageIngest, agentID string) skills.InstalledPackage {
	return skills.InstalledPackage{
		Skill:       storedSkillPreview(ingest, agentID),
		Package:     ingest.Package,
		PackageHash: ingest.Hash,
	}
}

// buildUserSkillImportReview projects the closed bounded review from the
// ingest and the stored-skill preview.
func buildUserSkillImportReview(ingest importer.PackageIngest, stored skills.Skill) UserSkillImportReview {
	review := UserSkillImportReview{
		Name:          stored.Name,
		Title:         stored.Title,
		Trigger:       stored.Trigger,
		TaskType:      stored.TaskType,
		Tags:          append([]string(nil), stored.Tags...),
		StepCount:     len(stored.Steps),
		RequiredTools: append([]string(nil), stored.RequiredTools...),
		RequiredNS:    append([]string(nil), stored.RequiredNS...),
		RequiredTags:  append([]string(nil), stored.RequiredTags...),
		ContentHash:   stored.ContentHash,
		PackageHash:   ingest.Hash,
		SupportFiles:  make([]UserSkillImportSupportSummary, 0, len(ingest.Package.Supports)),
	}
	for _, f := range ingest.Package.Supports {
		review.SupportFiles = append(review.SupportFiles, UserSkillImportSupportSummary{
			Path: f.Path, Mime: f.Mime, Size: f.Size, Digest: f.Digest,
		})
	}
	return review
}

// userSkillImportWarnings computes the non-fatal review notes: applicability
// requirements (required tools / namespaces / tags) that are not in the
// effective agent's run-visible capability snapshot. They are metadata only
// — the injection-time filter/redactor scrubs them and they never widen
// catalog visibility, approval, OAuth, or tool-exposure policy.
func userSkillImportWarnings(stored skills.Skill, policy UserSkillImportPolicy, agentID string) []string {
	var warnings []string
	note := func(field, value string) {
		warnings = append(warnings, fmt.Sprintf("%s %q is not currently run-visible for agent %q; it is applicability metadata and is filtered/redacted at injection time, never a grant", field, value, agentID))
	}
	for _, tool := range stored.RequiredTools {
		if !stringSetHas(policy.PermittedTools, tool) {
			note("required_tool", tool)
		}
	}
	for _, ns := range stored.RequiredNS {
		if !stringSetHas(policy.PermittedNS, ns) {
			note("required_namespace", ns)
		}
	}
	for _, tag := range stored.RequiredTags {
		if !stringSetHas(policy.PermittedTags, tag) {
			note("required_tag", tag)
		}
	}
	sort.Strings(warnings)
	return warnings
}

func stringSetHas(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// receiptFromWinner synthesizes the exact receipt of a landed write from the
// stored winner when the original receipt was lost mid-flight (the written
// version IS the winner, so the synthesized WrittenHash is exact).
func receiptFromWinner(q identity.Quadruple, agentID, name string, winner skills.InstalledPackage) skills.InstalledPackageReceipt {
	return skills.InstalledPackageReceipt{
		TenantID: q.TenantID, UserID: q.UserID, AgentID: agentID, Name: name,
		WrittenHash: winner.PackageHash, WrittenVersion: winner.Package.Version,
	}
}

// artifactScope derives the caller-owned artifact read scope (the exact
// (tenant, user, session) triple; TaskID is provenance only and never
// participates in resolution).
func artifactScope(id identity.Identity) artifacts.ArtifactScope {
	return artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
}

// sha256Hex returns the lowercase hex sha256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// cloneStoredSkill returns a deep copy of a stored skill: every slice field
// is copied so a caller's mutation can never affect the service or another
// caller's copy.
func cloneStoredSkill(skill skills.Skill) skills.Skill {
	out := skill
	out.Tags = append([]string(nil), skill.Tags...)
	out.Steps = append([]string(nil), skill.Steps...)
	out.Preconditions = append([]string(nil), skill.Preconditions...)
	out.FailureModes = append([]string(nil), skill.FailureModes...)
	out.RequiredTools = append([]string(nil), skill.RequiredTools...)
	out.RequiredNS = append([]string(nil), skill.RequiredNS...)
	out.RequiredTags = append([]string(nil), skill.RequiredTags...)
	if len(skill.Extra) > 0 {
		out.Extra = make(map[string]any, len(skill.Extra))
		for k, v := range skill.Extra {
			out.Extra[k] = v
		}
	}
	return out
}
