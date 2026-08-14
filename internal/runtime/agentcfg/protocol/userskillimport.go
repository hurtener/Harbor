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
// — and installs nothing: it runs the ONE production importer/validator and
// returns the closed normalized review plus hashes and warnings together with
// a STATELESS opaque proposal token. Validate performs ZERO writes of any
// kind (no SkillStore body/package write, no agent-config membership write,
// and no StateStore proposal-ledger write — the proposal is sealed into the
// token, never persisted). Commit echoes the token and the reviewed
// `PackageHash`, authenticates and strictly decodes the sealed claims,
// re-derives identity and signed effective-agent reach, rechecks lifecycle /
// policy / ceilings / expected config hash, forces `ScopeUser` + caller
// ownership server-side, and atomically materializes the approved package
// plus membership in ONE conditional `PutInstalledPackage` write (never a
// second legacy membership write), serialized through a token-derived
// StateStore commit ledger. Response-loss retry is idempotent; competing
// commits have one winner across processes.
//
// # The seam (CLAUDE.md §4.4)
//
// The service depends on narrow injected seams — the production importer,
// the caller-owned ArtifactStore, a token sealer, a StateStore commit
// ledger, the mandatory SkillStore package surface, a read-only agent-config
// reader (fresh user-scope active revision + retirement gate), and the
// capability policy projection. Tests inject fakes; the production assembler
// wires the real objects. The service holds no package-level mutable state,
// is immutable after construction, and is safe for N concurrent goroutines:
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
//     closed) run BEFORE any artifact lookup, token decode, skill-name
//     disclosure, lifecycle lookup, or persistence.
//   - The package frontmatter is CLOSED: the importer rejects every
//     authority-bearing field (scope, origin, tenant, user, agent,
//     authority, audience) and every unknown key, so caller YAML can never
//     set the storage/authority envelope. The commit request carries only
//     the opaque proposal token, the reviewed package hash, the expected
//     config hash, the reviewed canonical name, and the replace consent —
//     there is no body to submit back as authority.
//   - `required_tools` / `required_namespaces` / `required_tags` are
//     applicability metadata, never grants: a requirement outside the
//     effective agent's run-visible capability snapshot produces a WARNING
//     (the injection-time filter/redactor scrubs it) and the policy
//     SNAPSHOT + hash is sealed into the token, so a policy change between
//     validate and commit is a typed revocation refusal.
//
// # The stateless proposal token and the commit ledger
//
//   - Validate seals ONE versioned claims payload (schema/version, issued /
//     expiry, a crypto-random 128-bit nonce, the exact actor triple + agent,
//     the artifact id/hash/size, package + content hashes, the canonical
//     name, the closed review + warnings, the expected config hash, the
//     policy snapshot + hash, and the archive/SKILL.md ceiling snapshot)
//     into an opaque base64url token via the injected sealer and performs
//     ZERO durable writes of any kind.
//   - Commit authenticates the token (oversize, malformed base64, failed
//     AEAD authentication, unknown schema, malformed claims, cross
//     actor/agent/session, expiry, and echo mismatch all map to the same
//     typed refusal BEFORE any write — no oracle), re-derives the commit
//     ledger slot from SHA-256 of the authenticated token bytes (never the
//     plaintext review), and re-runs every identity / reach / retirement /
//     artifact / importer / config / policy / ceiling / boot-owned check.
//   - The first commit's ledger slot is ABSENT (validate wrote nothing): the
//     commit creates the current `committing` marker with an absent-slot CAS
//     (a concurrent commit of the same token loses), performs THE ONE
//     conditional `PutInstalledPackage` write, and then records the terminal
//     `committed` marker with the exact receipt. A failed put is compensated
//     by conditionally deleting the `committing` marker (the put is
//     transactional, so no package or membership state needs undoing; the
//     absent pre-operation slot is restored exactly).
//   - Response-loss replay is idempotent: a retry that loads the
//     token-derived ledger in Phase "committed" (or "committing" with the
//     exact winner present) returns the same terminal result WITHOUT a
//     second package write. A retry that loads Phase "committing" with a
//     DIFFERENT winner refuses loudly — a receipt/marker never overwrites
//     another commit's winner.
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
// The claims seal the normalized archive / SKILL.md ceiling snapshot (the
// canonical skillpkg bounds). Commit re-parses under the current ceilings
// and refuses if the current effective ceilings differ from the reviewed
// snapshot — a package above the current ceilings never lands.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// a mandatory dependency (importer / artifact store / token sealer /
	// commit ledger / skill store / registry / capability policy).
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
	// not match the recorded digest/size the claims pinned.
	ErrUserSkillImportArtifactChanged = errors.New("agentcfg/protocol: user skill import artifact changed after validation")
	// ErrUserSkillImportPackageInvalid — the artifact is not a valid
	// complete skill package (archive / path / MIME / SKILL.md / support-ref
	// / frontmatter violations, including every authority-bearing
	// frontmatter field). Wraps the canonical importer / skillpkg sentinel.
	ErrUserSkillImportPackageInvalid = errors.New("agentcfg/protocol: user skill import artifact is not a valid complete skill package")
	// ErrUserSkillImportProposalInvalid — the proposal token is unknown,
	// consumed, foreign, or stale: oversize, malformed base64, failed
	// authentication, unknown schema, malformed claims, or bound to
	// different server-side inputs (actor, agent, name, reviewed hash,
	// expected config hash).
	ErrUserSkillImportProposalInvalid = errors.New("agentcfg/protocol: invalid, consumed, foreign, or stale user skill import proposal token")
	// ErrUserSkillImportExpired — the proposal token's review window elapsed
	// before an explicit commit.
	ErrUserSkillImportExpired = errors.New("agentcfg/protocol: user skill import proposal token expired")
	// ErrUserSkillImportHashMismatch — the reviewed package hash the commit
	// echoes does not equal the package the claims pinned (a changed review
	// is refused before any write).
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
	// different package version than the one this commit's claims
	// reviewed/wrote. One winner only: this commit refuses rather than
	// overwrite.
	ErrUserSkillImportConcurrentWinner = errors.New("agentcfg/protocol: user skill import target is held by a different winner")
)

// userSkillImportCommitKindPrefix namespaces the durable commit-ledger
// slots. The slot kind embeds SHA-256 of the authenticated token bytes (never
// the plaintext review), so every token owns its own slot under the caller's
// identity (a token is never visible to a foreign triple — the store's
// identity key is the first fence).
const userSkillImportCommitKindPrefix = "agentcfg.user_skill_import.commit."

// defaultUserSkillImportProposalTTL bounds the review window between
// validate and an explicit commit. A token past its ExpiresAt is refused on
// every commit path that would mutate.
const defaultUserSkillImportProposalTTL = 24 * time.Hour

// Proposal-token / claims structural bounds. Every bound is enforced before
// the sealer runs (seal) or before any write (open): an oversize, malformed,
// or out-of-bounds form is a typed refusal, never an oracle and never a
// persistence attempt.
const (
	// userSkillImportProposalSchemaVersion is the only claims schema the
	// service seals and accepts. Any other version is refused as malformed.
	userSkillImportProposalSchemaVersion = 1
	// maxUserSkillImportProposalTokenBytes caps the base64url-encoded
	// token the caller echoes. The claims plaintext is itself bounded by
	// maxUserSkillImportProposalClaimsBytes, so this is a strict hard
	// ceiling on the wire form (the sealer envelope for a full review
	// manifest stays far below it).
	maxUserSkillImportProposalTokenBytes = 2 << 20
	// maxUserSkillImportProposalClaimsBytes caps the sealed claims JSON
	// (the review + support manifest + policy snapshot). The package
	// ceilings bound the manifest, so the cap is generous but strict.
	maxUserSkillImportProposalClaimsBytes = 1 << 20
	// maxUserSkillImportClaimsFieldLen caps every scalar string in the
	// claims (identity components, hashes, names, paths).
	maxUserSkillImportClaimsFieldLen = 4096
	// maxUserSkillImportClaimsListLen caps every string slice in the
	// claims (tags, required sets, policy sets, warnings).
	maxUserSkillImportClaimsListLen = 1 << 16
	// maxUserSkillImportClaimsSupportsLen caps the review support manifest.
	maxUserSkillImportClaimsSupportsLen = 1 << 16
	// maxUserSkillImportClaimsLifetime bounds issued→expiry inside the
	// claims. The service TTL default is 24h; the structural bound is a
	// sanity ceiling that cannot silently widen the review window.
	maxUserSkillImportClaimsLifetime = 7 * 24 * time.Hour
)

// userSkillImportPolicyID / userSkillImportPolicyVersion identify the
// capability policy envelope the claims bind by hash.
const (
	userSkillImportPolicyID      = "harbor.user-skill-import"
	userSkillImportPolicyVersion = "1"
)

// UserSkillImportPolicy is the server-owned capability snapshot the import
// validates RequiredTools / RequiredNS / RequiredTags against (as
// applicability metadata — non-fatal warnings) and the claims bind by hash.
// The permitted sets are the effective agent's run-visible catalog
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

// NewUserSkillImportCapabilityPolicy builds the production capability-policy
// adapter — the canonical ActivePlannerCatalogView projection over the
// configured tool catalog under the caller's verified identity — and returns
// it through the UserSkillImportCapability interface, so cross-package
// composition never needs the private concrete type. Policy semantics are
// unchanged: the adapter is immutable after construction, a nil catalog fails
// loud on the first Policy call, and no writes of any kind happen. The
// granted-scope ceiling slice is defensively copied: mutating the caller's
// backing array after construction cannot change the adapter's behavior.
func NewUserSkillImportCapabilityPolicy(registry agentcfg.Registry, overlay sessionoverlay.Store, catalog tools.ToolCatalog, grantedScopes []string) UserSkillImportCapability {
	return agentConfigCapabilityPolicy{
		registry:      registry,
		overlay:       overlay,
		catalog:       catalog,
		grantedScopes: append([]string(nil), grantedScopes...),
	}
}

// userSkillImportPolicyHash is the deterministic content hash of a policy
// snapshot (the claims binding). Field order is fixed; unordered slices
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

// UserSkillImportProposalSealer seals / opens the opaque proposal-token
// envelope. The seal input is the bounded versioned claims JSON; the open
// output is the authenticated claims JSON. The interface is deliberately the
// same shape as internal/tools/auth.Sealer (the AES-256-GCM envelope
// sealer), so the production assembler wires the real sealer and tests wire a
// deterministic dev sealer. The sealer is mandatory and fails construction
// loud when missing: a token that cannot be authenticated is never treated as
// review state.
type UserSkillImportProposalSealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(ciphertext []byte) ([]byte, error)
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
// only the opaque proposal token, the reviewed hash, the expected config
// hash, the reviewed canonical name, and the replace consent).
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
// sealed proposal token, the closed review, the reviewed hashes, the expected
// config content hash the caller must echo on commit, the expiry, and the
// non-fatal warnings. Zero durable skill/package/membership/proposal-ledger
// mutation happened — the review state rides entirely inside the token.
type UserSkillImportValidateResponse struct {
	// ProposalToken is the opaque sealed token the commit echoes. It is
	// base64url of the sealer envelope over the versioned claims.
	ProposalToken string
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
	// Commit requires the echo to match the claims.
	ExpectedContentHash string
	// ExpiresAt bounds the review window; a commit after this time is
	// refused.
	ExpiresAt time.Time
}

// UserSkillImportCommitRequest is the bounded second-phase input: the
// proposal token, the reviewed package hash, the reviewed canonical name, the
// expected config content hash, and the explicit replacement consent.
type UserSkillImportCommitRequest struct {
	// ProposalToken echoes the opaque proposal token from validate.
	ProposalToken string
	// AgentID is the effective agent (must equal the claims').
	AgentID string
	// Name is the reviewed canonical package/skill name (must equal the
	// claims'; used to address the target key).
	Name string
	// ReviewedPackageHash is the versioned package hash the caller
	// reviewed (must equal the claims').
	ReviewedPackageHash string
	// ExpectedContentHash echoes the expected config content hash from
	// validate (must equal the claims').
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
	sealer       UserSkillImportProposalSealer
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

// NewUserSkillImportService builds the two-phase import service over the
// seven mandatory seams: the production importer, the caller-owned
// ArtifactStore, the token sealer, the StateStore commit ledger, the
// mandatory SkillStore package surface, the read-only agent-config reader,
// and the capability-policy projection. A nil seam fails loud with
// ErrUserSkillImportMisconfigured rather than building a service that would
// nil-panic on the first request. The returned *UserSkillImportService is
// immutable after construction and safe for concurrent use by N goroutines.
func NewUserSkillImportService(imp importer.Importer, artifactStore artifacts.ArtifactStore, sealer UserSkillImportProposalSealer, proposals state.StateStore, store skills.SkillStore, registry UserSkillImportConfigReader, capability UserSkillImportCapability, opts ...UserSkillImportOption) (*UserSkillImportService, error) {
	if imp == nil {
		return nil, fmt.Errorf("%w: importer is nil", ErrUserSkillImportMisconfigured)
	}
	if artifactStore == nil {
		return nil, fmt.Errorf("%w: artifact store is nil", ErrUserSkillImportMisconfigured)
	}
	if sealer == nil {
		return nil, fmt.Errorf("%w: proposal token sealer is nil", ErrUserSkillImportMisconfigured)
	}
	if proposals == nil {
		return nil, fmt.Errorf("%w: commit ledger is nil", ErrUserSkillImportMisconfigured)
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
		importer: imp, artifacts: artifactStore, sealer: sealer, proposals: proposals, store: store,
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

// Validate performs the ZERO-write first phase: verified identity + signed
// reach, retirement gate, the caller-owned immutable artifact read, THE
// production importer/validator parse, the capability-policy review
// (warnings, never grants), the user-scope config base snapshot, and the
// sealing of the versioned claims into the opaque proposal token. No
// SkillStore body/package write, no agent-config membership write, and no
// StateStore proposal-ledger write happens.
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
	// refuse BEFORE a token exists (the commit re-checks before any
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

	review := buildUserSkillImportReview(ingest, stored)
	now := s.now().UTC()
	nonce, err := newUserSkillImportNonce()
	if err != nil {
		return UserSkillImportValidateResponse{}, err
	}
	claims := userSkillImportProposalClaims{
		SchemaVersion:       userSkillImportProposalSchemaVersion,
		Nonce:               nonce,
		IssuedAt:            now,
		ExpiresAt:           now.Add(s.proposalTTL),
		Actor:               id,
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
		Review:              review,
		Warnings:            append([]string(nil), warnings...),
	}
	token, err := s.sealProposalClaims(claims)
	if err != nil {
		return UserSkillImportValidateResponse{}, fmt.Errorf("user skill import: seal proposal claims: %w", err)
	}

	return UserSkillImportValidateResponse{
		ProposalToken:       token,
		Review:              review,
		Warnings:            warnings,
		PackageHash:         ingest.Hash,
		ExpectedContentHash: expectedContentHash,
		ExpiresAt:           claims.ExpiresAt,
	}, nil
}

// Commit performs the explicit second phase: it authenticates and strictly
// decodes the sealed proposal token and refuses every stale form (oversize /
// malformed base64 / failed authentication / unknown schema / malformed
// claims / cross actor-agent-session / expired / echo mismatch), re-runs
// every identity / reach / retirement / artifact / importer / config /
// policy / ceiling / boot-owned check, and then performs THE ONE conditional
// PutInstalledPackage write (the atomic package+membership unit),
// serialized through the token-derived commit ledger. Response-loss replay
// returns the same terminal result without a second write; a competing
// winner is never overwritten.
func (s *UserSkillImportService) Commit(ctx context.Context, req UserSkillImportCommitRequest) (UserSkillImportCommitResponse, error) {
	if err := ctx.Err(); err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	id, q, err := s.resolveIdentityAndReach(ctx, req.AgentID)
	if err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	if strings.TrimSpace(req.ProposalToken) == "" {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: proposal token is empty", ErrUserSkillImportProposalInvalid)
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

	// Authenticate and strictly decode the sealed claims. Every failure
	// form (oversize, malformed base64, failed AEAD authentication,
	// unknown schema, malformed / out-of-bounds claims) is the SAME typed
	// refusal — no oracle — and happens before any write.
	claims, err := s.unsealProposalToken(req.ProposalToken)
	if err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	// The review window is enforced before any ledger read or write.
	if !s.now().UTC().Before(claims.ExpiresAt) {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: the proposal token expired", ErrUserSkillImportExpired)
	}
	// The caller's echo must match the sealed review exactly (actor,
	// agent, name, reviewed hash, expected config hash).
	if err := s.verifyClaimsBindings(id, req, claims); err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	// A boot-declared canonical name is read-only to every control
	// plane. The commit refuses it on EVERY path — initial commit,
	// response-loss replay, and prepared/committing resume alike — so
	// no token can ever land a boot-owned name (even at equal hash).
	if err := guardBootOwnedName(bootOwnershipFromContext(ctx), q.TenantID, req.AgentID, claims.Name); err != nil {
		return UserSkillImportCommitResponse{}, err
	}

	// The durable commit ledger slot is derived from the authenticated
	// token bytes (SHA-256 — never the plaintext review), so retries and
	// concurrent commits of the SAME token converge on the SAME slot.
	kind := userSkillImportCommitKind(req.ProposalToken)
	record, err := s.proposals.Load(ctx, q, kind)
	if errors.Is(err, state.ErrNotFound) {
		return s.commitFresh(ctx, q, id, req, claims, kind)
	}
	if err != nil {
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: load commit ledger: %w", err)
	}
	commitRecord, err := unmarshalUserSkillImportCommitRecord(record.Bytes)
	if err != nil {
		return UserSkillImportCommitResponse{}, ErrUserSkillImportProposalInvalid
	}

	switch commitRecord.Phase {
	case userSkillImportPhaseCommitted:
		return s.replayCommitted(ctx, q, req, commitRecord)
	case userSkillImportPhaseCommitting:
		return s.resumeCommitting(ctx, q, req, commitRecord, record.ID, kind)
	default:
		return UserSkillImportCommitResponse{}, ErrUserSkillImportProposalInvalid
	}
}

// userSkillImportCommitPhase values.
const (
	// userSkillImportPhaseCommitting is the pre-write marker: the commit
	// CAS-binds the exact package hash it is about to write onto the
	// token-derived ledger slot.
	userSkillImportPhaseCommitting = "committing"
	// userSkillImportPhaseCommitted is the terminal state: the atomic
	// write landed and the exact receipt is recorded on the ledger slot.
	userSkillImportPhaseCommitted = "committed"
)

// userSkillImportNonce is the crypto-random 128-bit claims nonce. It
// serializes as base64url (16 bytes → 22 chars), giving every sealed token a
// fresh, unpredictable binding even when the reviewed state is identical.
type userSkillImportNonce [16]byte

// MarshalJSON encodes the nonce as base64url text.
func (n userSkillImportNonce) MarshalJSON() ([]byte, error) {
	return json.Marshal(base64.RawURLEncoding.EncodeToString(n[:]))
}

// UnmarshalJSON strictly decodes a base64url nonce; any other shape (wrong
// length, malformed base64) is an error.
func (n *userSkillImportNonce) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return fmt.Errorf("user skill import: nonce is not base64url: %w", err)
	}
	if len(raw) != len(n) {
		return fmt.Errorf("user skill import: nonce must be exactly %d bytes, got %d", len(n), len(raw))
	}
	copy(n[:], raw)
	return nil
}

// userSkillImportProposalClaims is the single versioned sealed claims
// payload. It carries the ENTIRE reviewed state of the former durable
// proposal record — artifact id/hash/size, package + content hashes,
// canonical name, the closed review + warnings, expected config content hash,
// the capability-policy snapshot + hash, and the configured archive/SKILL.md
// ceiling snapshot — plus the claims envelope (schema/version, issued/expiry,
// the exact actor triple + agent, and a crypto-random 128-bit nonce). Raw
// package bytes are never duplicated into the claims.
type userSkillImportProposalClaims struct {
	SchemaVersion int `json:"schema_version"`
	// Nonce is the crypto-random 128-bit claims binding.
	Nonce userSkillImportNonce `json:"nonce"`
	// IssuedAt is when the review was sealed.
	IssuedAt time.Time `json:"issued_at"`
	// ExpiresAt bounds the review window.
	ExpiresAt time.Time `json:"expires_at"`
	// Actor is the exact verified (tenant, user, session) triple that
	// sealed the review.
	Actor identity.Identity `json:"actor"`
	// AgentID is the effective agent the review is bound to.
	AgentID string `json:"agent_id"`
	// ArtifactID is the caller-owned immutable artifact ref.
	ArtifactID string `json:"artifact_id"`
	// ArtifactSHA256 is the exact digest of the reviewed artifact bytes.
	ArtifactSHA256 string `json:"artifact_sha256"`
	// ArtifactSizeBytes is the exact reviewed artifact byte size.
	ArtifactSizeBytes int64 `json:"artifact_size_bytes"`
	// Name is the canonical package/skill name (the stored target-key
	// identity).
	Name string `json:"name"`
	// PackageHash is the versioned reviewed package hash ("v1:<64-hex>").
	PackageHash string `json:"package_hash"`
	// ContentHash is the canonical stored content hash as reviewed.
	ContentHash string `json:"content_hash"`
	// ExpectedContentHash is the caller's user-scope config content hash
	// at validate time ("-" when none).
	ExpectedContentHash string `json:"expected_content_hash"`
	// Policy is the reviewed capability snapshot.
	Policy UserSkillImportPolicy `json:"policy"`
	// PolicyHash is the deterministic hash of the reviewed policy snapshot.
	PolicyHash string `json:"policy_hash"`
	// ArchiveLimits is the normalized archive ceiling snapshot.
	ArchiveLimits skillpkg.ArchiveLimits `json:"archive_limits"`
	// MarkdownLimits is the normalized SKILL.md ceiling snapshot.
	MarkdownLimits skillpkg.MarkdownLimits `json:"markdown_limits"`
	// Review is the closed normalized review as returned to the caller.
	Review UserSkillImportReview `json:"review"`
	// Warnings are the non-fatal review notes as returned to the caller.
	Warnings []string `json:"warnings"`
}

// userSkillImportCommitRecord is the durable commit-ledger payload: the
// phase machine state (committing marker with the exact hash about to be
// written, or the terminal committed marker with the exact receipt) plus the
// canonical name needed to address the target key on resume/replay. The
// reviewed state itself lives in the sealed token — the ledger never
// duplicates plaintext claims.
type userSkillImportCommitRecord struct {
	Phase string `json:"phase,omitempty"`
	// Name is the canonical package/skill name (the target key).
	Name string `json:"name"`
	// WrittenPackageHash is the exact versioned hash this commit wrote
	// (committing marker) or wrote (committed marker).
	WrittenPackageHash string `json:"written_package_hash,omitempty"`
	// Receipt is the exact receipt of the landed atomic write (committed
	// marker only).
	Receipt *skills.InstalledPackageReceipt `json:"receipt,omitempty"`
}

// userSkillImportCommitKind derives the durable commit-ledger slot of a
// proposal token: SHA-256 of the authenticated token bytes, never the
// plaintext review. The caller's exact triple is the store's first fence.
func userSkillImportCommitKind(token string) string {
	sum := sha256.Sum256([]byte(token))
	return userSkillImportCommitKindPrefix + hex.EncodeToString(sum[:])
}

// newUserSkillImportNonce draws a fresh crypto-random 128-bit nonce.
func newUserSkillImportNonce() (userSkillImportNonce, error) {
	var n userSkillImportNonce
	if _, err := io.ReadFull(rand.Reader, n[:]); err != nil {
		return n, fmt.Errorf("user skill import: generate claims nonce: %w", err)
	}
	return n, nil
}

// sealProposalClaims bounds-validates, strictly encodes, seals, and
// base64url-encodes the versioned claims into the opaque proposal token.
func (s *UserSkillImportService) sealProposalClaims(claims userSkillImportProposalClaims) (string, error) {
	plaintext, err := marshalUserSkillImportProposalClaims(claims)
	if err != nil {
		return "", fmt.Errorf("user skill import: encode proposal claims: %w", err)
	}
	envelope, err := s.sealer.Seal(plaintext)
	if err != nil {
		return "", fmt.Errorf("user skill import: seal proposal claims: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(envelope)
	if len(token) > maxUserSkillImportProposalTokenBytes {
		return "", fmt.Errorf("user skill import: sealed proposal token exceeds %d bytes", maxUserSkillImportProposalTokenBytes)
	}
	return token, nil
}

// unsealProposalToken strictly authenticates and decodes the caller-echoed
// proposal token. Every failure form returns the same typed
// ErrUserSkillImportProposalInvalid wrapper — oversize, malformed base64,
// failed AEAD authentication, unknown schema, malformed/out-of-bounds claims
// are indistinguishable to the caller (no oracle).
func (s *UserSkillImportService) unsealProposalToken(token string) (userSkillImportProposalClaims, error) {
	if len(token) > maxUserSkillImportProposalTokenBytes {
		return userSkillImportProposalClaims{}, fmt.Errorf("%w: proposal token exceeds %d bytes", ErrUserSkillImportProposalInvalid, maxUserSkillImportProposalTokenBytes)
	}
	envelope, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return userSkillImportProposalClaims{}, fmt.Errorf("%w: proposal token is not valid base64url", ErrUserSkillImportProposalInvalid)
	}
	plaintext, err := s.sealer.Open(envelope)
	if err != nil {
		return userSkillImportProposalClaims{}, fmt.Errorf("%w: proposal token failed authentication", ErrUserSkillImportProposalInvalid)
	}
	claims, err := unmarshalUserSkillImportProposalClaims(plaintext)
	if err != nil {
		return userSkillImportProposalClaims{}, fmt.Errorf("%w: %w", ErrUserSkillImportProposalInvalid, err)
	}
	return claims, nil
}

// marshalUserSkillImportProposalClaims validates the claims bounds, encodes
// them, and enforces the strict plaintext ceiling.
func marshalUserSkillImportProposalClaims(c userSkillImportProposalClaims) ([]byte, error) {
	if err := validateUserSkillImportProposalClaims(c); err != nil {
		return nil, err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	if len(b) > maxUserSkillImportProposalClaimsBytes {
		return nil, fmt.Errorf("user skill import: claims exceed the %d-byte bound", maxUserSkillImportProposalClaimsBytes)
	}
	return b, nil
}

// unmarshalUserSkillImportProposalClaims strictly decodes the versioned
// claims: unknown fields are refused, trailing data is refused, and every
// structural / cardinality / string / time bound is enforced.
func unmarshalUserSkillImportProposalClaims(b []byte) (userSkillImportProposalClaims, error) {
	if len(b) > maxUserSkillImportProposalClaimsBytes {
		return userSkillImportProposalClaims{}, fmt.Errorf("user skill import: claims exceed the %d-byte bound", maxUserSkillImportProposalClaimsBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c userSkillImportProposalClaims
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("decode user skill import proposal claims: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return c, fmt.Errorf("decode user skill import proposal claims: trailing data")
	}
	if err := validateUserSkillImportProposalClaims(c); err != nil {
		return c, err
	}
	return c, nil
}

// validateUserSkillImportProposalClaims enforces the versioned claims
// bounds: schema version, nonce, actor triple, issued/expiry ordering and
// lifetime, mandatory reviewed-state fields, string / cardinality bounds, and
// the internal review↔compact-state consistency checks.
func validateUserSkillImportProposalClaims(c userSkillImportProposalClaims) error {
	if c.SchemaVersion != userSkillImportProposalSchemaVersion {
		return fmt.Errorf("user skill import: unsupported claims schema version %d", c.SchemaVersion)
	}
	if c.Nonce == (userSkillImportNonce{}) {
		return fmt.Errorf("user skill import: claims nonce is all-zero")
	}
	if err := identity.Validate(c.Actor); err != nil {
		return fmt.Errorf("user skill import: claims actor triple: %w", err)
	}
	if c.IssuedAt.IsZero() {
		return fmt.Errorf("user skill import: claims issued_at missing")
	}
	if c.ExpiresAt.IsZero() {
		return fmt.Errorf("user skill import: claims expires_at missing")
	}
	if !c.ExpiresAt.After(c.IssuedAt) {
		return fmt.Errorf("user skill import: claims expires_at not after issued_at")
	}
	if c.ExpiresAt.Sub(c.IssuedAt) > maxUserSkillImportClaimsLifetime {
		return fmt.Errorf("user skill import: claims lifetime exceeds %s", maxUserSkillImportClaimsLifetime)
	}
	if c.AgentID == "" || c.ArtifactID == "" || c.ArtifactSHA256 == "" || c.Name == "" ||
		c.PackageHash == "" || c.ContentHash == "" || c.ExpectedContentHash == "" || c.PolicyHash == "" {
		return fmt.Errorf("user skill import: claims missing a mandatory reviewed-state field")
	}
	if c.ArtifactSizeBytes < 0 {
		return fmt.Errorf("user skill import: claims artifact size negative")
	}
	for _, str := range []string{
		c.Actor.TenantID, c.Actor.UserID, c.Actor.SessionID, c.AgentID, c.ArtifactID,
		c.ArtifactSHA256, c.Name, c.PackageHash, c.ContentHash, c.ExpectedContentHash, c.PolicyHash,
	} {
		if len(str) > maxUserSkillImportClaimsFieldLen {
			return fmt.Errorf("user skill import: claims string exceeds the %d-byte bound", maxUserSkillImportClaimsFieldLen)
		}
	}
	if err := validateUserSkillImportClaimsStringSlices(c.Policy.PermittedTools, c.Policy.PermittedNS, c.Policy.PermittedTags); err != nil {
		return fmt.Errorf("user skill import: policy snapshot: %w", err)
	}
	if err := validateUserSkillImportClaimsStringSlices(c.Review.Tags, c.Review.RequiredTools, c.Review.RequiredNS, c.Review.RequiredTags); err != nil {
		return fmt.Errorf("user skill import: review: %w", err)
	}
	if err := validateUserSkillImportClaimsStringSlices(c.Warnings); err != nil {
		return fmt.Errorf("user skill import: warnings: %w", err)
	}
	if len(c.Review.SupportFiles) > maxUserSkillImportClaimsSupportsLen {
		return fmt.Errorf("user skill import: review support manifest exceeds the %d-entry bound", maxUserSkillImportClaimsSupportsLen)
	}
	for i, f := range c.Review.SupportFiles {
		if len(f.Path) > maxUserSkillImportClaimsFieldLen || len(f.Mime) > maxUserSkillImportClaimsFieldLen || len(f.Digest) > maxUserSkillImportClaimsFieldLen {
			return fmt.Errorf("user skill import: review support manifest entry %d exceeds the field bound", i)
		}
		if f.Size < 0 {
			return fmt.Errorf("user skill import: review support manifest entry %d size negative", i)
		}
	}
	if c.Review.StepCount < 0 {
		return fmt.Errorf("user skill import: review step count negative")
	}
	// Internal consistency: the compact reviewed-state fields must agree
	// with the sealed review so no field can be dropped or swapped.
	if c.Review.Name != c.Name || c.Review.PackageHash != c.PackageHash || c.Review.ContentHash != c.ContentHash {
		return fmt.Errorf("user skill import: claims review disagrees with the compact reviewed state")
	}
	return nil
}

// validateUserSkillImportClaimsStringSlices enforces the per-slice
// cardinality bound and the per-entry string bound.
func validateUserSkillImportClaimsStringSlices(groups ...[]string) error {
	for _, g := range groups {
		if len(g) > maxUserSkillImportClaimsListLen {
			return fmt.Errorf("slice exceeds the %d-entry bound", maxUserSkillImportClaimsListLen)
		}
		for _, str := range g {
			if len(str) > maxUserSkillImportClaimsFieldLen {
				return fmt.Errorf("entry exceeds the %d-byte bound", maxUserSkillImportClaimsFieldLen)
			}
		}
	}
	return nil
}

func marshalUserSkillImportCommitRecord(r userSkillImportCommitRecord) ([]byte, error) {
	return json.Marshal(r)
}

func unmarshalUserSkillImportCommitRecord(b []byte) (userSkillImportCommitRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r userSkillImportCommitRecord
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("decode user skill import commit ledger: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return r, fmt.Errorf("decode user skill import commit ledger: trailing data")
	}
	if r.Phase != userSkillImportPhaseCommitting && r.Phase != userSkillImportPhaseCommitted {
		return r, fmt.Errorf("decode user skill import commit ledger: unknown phase")
	}
	if strings.TrimSpace(r.Name) == "" {
		return r, fmt.Errorf("decode user skill import commit ledger: name empty")
	}
	if r.Phase == userSkillImportPhaseCommitting && strings.TrimSpace(r.WrittenPackageHash) == "" {
		return r, fmt.Errorf("decode user skill import commit ledger: committing without the written hash")
	}
	if r.Phase == userSkillImportPhaseCommitted && r.Receipt == nil {
		return r, fmt.Errorf("decode user skill import commit ledger: committed without a receipt")
	}
	return r, nil
}

// resolveIdentityAndReach resolves the verified caller identity from ctx and
// applies the two signed-reach gates: the session gate (a PRESENT
// session_reach claim must contain the caller's session) and the effective
// agent gate (fails closed when unwired or when the agent is outside the
// caller's reach). It runs BEFORE any artifact lookup, token decode,
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

// verifyClaimsBindings refuses every stale / foreign / mismatched token
// form: the sealed actor must equal the caller's exact triple, the agent and
// reviewed name must match, the reviewed package hash and the expected config
// hash must echo the claims exactly. No claims content is echoed in the error
// (sealed state never leaks through errors).
func (s *UserSkillImportService) verifyClaimsBindings(id identity.Identity, req UserSkillImportCommitRequest, claims userSkillImportProposalClaims) error {
	if claims.Actor != id {
		return fmt.Errorf("%w: token actor does not match the caller", ErrUserSkillImportProposalInvalid)
	}
	if claims.AgentID != req.AgentID {
		return fmt.Errorf("%w: token agent does not match the request agent", ErrUserSkillImportProposalInvalid)
	}
	if claims.Name != req.Name {
		return fmt.Errorf("%w: token reviewed name does not match the request", ErrUserSkillImportProposalInvalid)
	}
	if claims.PackageHash != req.ReviewedPackageHash {
		return fmt.Errorf("%w: token reviewed package hash does not match the request", ErrUserSkillImportHashMismatch)
	}
	if claims.ExpectedContentHash != req.ExpectedContentHash {
		return fmt.Errorf("%w: token expected config hash does not match the request", ErrUserSkillImportProposalInvalid)
	}
	return nil
}

// commitFresh runs the full explicit commit for a token whose ledger slot is
// ABSENT (the first commit — validate wrote nothing): expiry, artifact
// re-resolution, full re-validation, policy / config / ceiling rechecks, the
// absent-slot CAS creation of the "committing" marker, THE ONE atomic package
// write, and the terminal transition to "committed".
func (s *UserSkillImportService) commitFresh(ctx context.Context, q identity.Quadruple, id identity.Identity, req UserSkillImportCommitRequest, claims userSkillImportProposalClaims, kind string) (UserSkillImportCommitResponse, error) {
	// Re-resolve the exact immutable caller-owned artifact and repeat the
	// FULL validation: a moved / changed / erased source is refused before
	// any write.
	bytes, found, err := s.artifacts.Get(ctx, artifactScope(id), claims.ArtifactID)
	if err != nil {
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: re-read artifact: %w", err)
	}
	if !found {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: the reviewed artifact no longer resolves under the caller's exact triple", ErrUserSkillImportArtifactNotFound)
	}
	if sha256Hex(bytes) != claims.ArtifactSHA256 || int64(len(bytes)) != claims.ArtifactSizeBytes {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: the reviewed artifact changed after validation", ErrUserSkillImportArtifactChanged)
	}
	ingest, err := s.importer.ImportPackage(ctx, importer.PackageSource{Archive: bytes, PathHint: claims.ArtifactID})
	if err != nil {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: %w", ErrUserSkillImportPackageInvalid, err)
	}
	if ingest.Hash != claims.PackageHash {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: re-parsed package hash differs from the reviewed hash (the reviewed package changed)",
			ErrUserSkillImportHashMismatch)
	}
	if ingest.Package.Name != claims.Name {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: re-parsed name differs from the reviewed name (the reviewed package changed)",
			ErrUserSkillImportHashMismatch)
	}

	// Policy revocation: the reviewed capability snapshot must still be
	// current.
	policy, err := s.capability.Policy(ctx, id, req.AgentID)
	if err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	if claims.PolicyHash == "" || userSkillImportPolicyHash(policy) != claims.PolicyHash {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: the reviewed capability snapshot changed", ErrUserSkillImportPolicyRevoked)
	}

	// Moved config base: the caller's user-scope revision must still carry
	// the reviewed content hash.
	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeUser)
	if err != nil {
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: re-read config base: %w", err)
	}
	if err := agentcfg.CheckExpectedRevision(agentcfg.SetOptions{ExpectedContentHash: claims.ExpectedContentHash}, active, hasActive); err != nil {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: %w", ErrUserSkillImportConfigMoved, err)
	}

	// Changed configured ceilings: the effective archive/SKILL.md bounds
	// must still equal the reviewed snapshot.
	if err := s.verifyCeilingSnapshot(claims); err != nil {
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

	// The first commit's ledger slot is genuinely ABSENT (validate wrote
	// nothing): create the "committing" marker with an absent-slot CAS. A
	// concurrent commit of the same token loses this SaveIf and is
	// refused; its retry then resumes onto the winner's terminal state.
	committingID := state.NewEventID()
	committingRecord := userSkillImportCommitRecord{
		Phase: userSkillImportPhaseCommitting, Name: ingest.Package.Name, WrittenPackageHash: ingest.Hash,
	}
	committingBytes, err := marshalUserSkillImportCommitRecord(committingRecord)
	if err != nil {
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: encode committing marker: %w", err)
	}
	if err := s.proposals.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: kind}}, state.StateRecord{
		ID: committingID, Identity: q, Kind: kind, Bytes: committingBytes,
	}); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return UserSkillImportCommitResponse{}, fmt.Errorf("%w: a concurrent commit of this proposal token is in progress", ErrUserSkillImportProposalInvalid)
		}
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: mark commit committing: %w", err)
	}

	receipt, err := s.store.PutInstalledPackage(ctx, q, req.AgentID, unit, cond, replace)
	if err != nil {
		// The put is transactional: a failure means NO package state
		// changed. Compensate by conditionally deleting the committing
		// marker — restoring the genuinely absent pre-operation slot — so
		// the caller can retry (e.g. with replacement consent) or
		// re-validate. No validate-time record exists to restore.
		if _, delErr := s.proposals.DeleteIf(ctx, state.SlotExpectation{Identity: q, Kind: kind, ExpectedEventID: committingID}); delErr != nil {
			s.logger.ErrorContext(ctx, "user skill import: compensate committing marker failed",
				"cause", err.Error(), "restore_error", delErr.Error())
			return UserSkillImportCommitResponse{}, errors.Join(fmt.Errorf("user skill import: put installed package: %w", err), delErr)
		}
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: put installed package: %w", err)
	}
	if receipt.WrittenHash != ingest.Hash {
		s.logger.ErrorContext(ctx, "user skill import: receipt hash diverged from reviewed package",
			"receipt", receipt.WrittenHash)
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: receipt hash diverged from the reviewed package", ErrUserSkillImportProposalInvalid)
	}

	// Terminal transition: record the exact receipt on the ledger. If this
	// bookkeeping write fails because a concurrent resume recorded the same
	// terminal state first, the package IS installed with the reviewed hash
	// and the terminal result is legitimate regardless of who recorded it.
	// A genuine store failure surfaces loud; a retry recognizes the
	// terminal state via the committing marker and returns the same result.
	committedRecord := userSkillImportCommitRecord{
		Phase: userSkillImportPhaseCommitted, Name: ingest.Package.Name, WrittenPackageHash: ingest.Hash, Receipt: &receipt,
	}
	committedBytes, err := marshalUserSkillImportCommitRecord(committedRecord)
	if err != nil {
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: encode committed marker: %w", err)
	}
	if err := s.proposals.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: kind, ExpectedEventID: committingID}}, state.StateRecord{
		ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: committedBytes,
	}); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			resp, terr := s.terminalResponse(ctx, q, req, receipt)
			resp.Replayed = true
			return resp, terr
		}
		return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: record committed marker: %w (the package IS installed; a retry returns the terminal result)", err)
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

// resumeCommitting handles a commit retry whose ledger marker is mid-flight:
// if the exact written package is the winner, the commit landed (response-loss)
// and the same terminal result is returned without a second write; a
// different winner is refused (one winner); an absent winner means the
// reviewed write never landed — the caller re-validates (a stale committing
// marker is never silently converted into a re-install).
func (s *UserSkillImportService) resumeCommitting(ctx context.Context, q identity.Quadruple, req UserSkillImportCommitRequest, commitRecord userSkillImportCommitRecord, slotID state.EventID, kind string) (UserSkillImportCommitResponse, error) {
	winner, found, err := s.currentWinner(ctx, q, req.AgentID, commitRecord.Name)
	if err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	switch {
	case found && winner.PackageHash == commitRecord.WrittenPackageHash:
		// The write landed but the terminal receipt was never returned (or
		// the committed transition failed): recognize the terminal state
		// and record it, then return the same result.
		receipt := receiptFromWinner(q, req.AgentID, commitRecord.Name, winner)
		committedRecord := userSkillImportCommitRecord{
			Phase: userSkillImportPhaseCommitted, Name: commitRecord.Name, WrittenPackageHash: commitRecord.WrittenPackageHash, Receipt: &receipt,
		}
		bytes, err := marshalUserSkillImportCommitRecord(committedRecord)
		if err != nil {
			return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: encode committed marker: %w", err)
		}
		if err := s.proposals.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: kind, ExpectedEventID: slotID}}, state.StateRecord{
			ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: bytes,
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
			return UserSkillImportCommitResponse{}, fmt.Errorf("user skill import: record committed marker: %w (the package IS installed; a retry returns the terminal result)", err)
		}
		resp, err := s.terminalResponse(ctx, q, req, receipt)
		resp.Replayed = true
		return resp, err
	case found:
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: the target is held by a different winner",
			ErrUserSkillImportConcurrentWinner)
	default:
		// The reviewed write never landed (a failed put whose inline
		// compensation also failed, or a retry racing a put that errored).
		// Refuse loudly rather than re-installing under a stale marker:
		// the caller re-validates for a fresh token.
		return UserSkillImportCommitResponse{}, ErrUserSkillImportProposalInvalid
	}
}

// replayCommitted handles a retry whose ledger already recorded the terminal
// receipt: the exact written package must still be the winner and the same
// terminal result is returned without any write.
func (s *UserSkillImportService) replayCommitted(ctx context.Context, q identity.Quadruple, req UserSkillImportCommitRequest, commitRecord userSkillImportCommitRecord) (UserSkillImportCommitResponse, error) {
	if commitRecord.Receipt == nil {
		return UserSkillImportCommitResponse{}, ErrUserSkillImportProposalInvalid
	}
	winner, found, err := s.currentWinner(ctx, q, req.AgentID, commitRecord.Name)
	if err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	if !found || winner.PackageHash != commitRecord.Receipt.WrittenHash {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: the target is held by a different winner",
			ErrUserSkillImportConcurrentWinner)
	}
	resp, err := s.terminalResponse(ctx, q, req, *commitRecord.Receipt)
	resp.Replayed = true
	return resp, err
}

// terminalResponse builds the terminal commit result from the exact receipt
// and the stored winner (the authoritative deep-copied unit).
func (s *UserSkillImportService) terminalResponse(ctx context.Context, q identity.Quadruple, req UserSkillImportCommitRequest, receipt skills.InstalledPackageReceipt) (UserSkillImportCommitResponse, error) {
	winner, found, err := s.currentWinner(ctx, q, req.AgentID, receipt.Name)
	if err != nil {
		return UserSkillImportCommitResponse{}, err
	}
	if !found || winner.PackageHash != receipt.WrittenHash {
		return UserSkillImportCommitResponse{}, fmt.Errorf("%w: the target is held by a different winner",
			ErrUserSkillImportConcurrentWinner)
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
func (s *UserSkillImportService) verifyCeilingSnapshot(claims userSkillImportProposalClaims) error {
	currentArchive := skillpkg.ArchiveLimits{}.Normalize()
	currentMarkdown := skillpkg.MarkdownLimits{}.Normalize()
	if claims.ArchiveLimits != currentArchive || claims.MarkdownLimits != currentMarkdown {
		return fmt.Errorf("%w: the reviewed ceiling snapshot is no longer current", ErrUserSkillImportCeilingChanged)
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
