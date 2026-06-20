// Package agentcfg owns Harbor's agent-config control plane: a durable,
// identity-scoped, versioned desired-state registry where every edit to
// an agent's configuration (skills membership now; tool/MCP exposure and
// prompt layers as later consumers extend the envelope) is an immutable,
// content-addressed revision. The active configuration is a pointer to a
// revision; rollback is a repoint; diff is a server-side revision
// compare.
//
// # The seam (CLAUDE.md §4.4)
//
// The Registry interface lives here; drivers live under
// internal/agentcfg/drivers/<driver>/; a factory + registry dispatches by
// name. The StateStore-backed driver reuses the §9 persistence triad for
// identity isolation, exactly as the governance tenant-override policy
// does — no new persistence subsystem.
//
// # Identity (CLAUDE.md §6)
//
// Every record is scoped by the full (tenant, user, session) triple. The
// registry is KEYED by agent_id (the agent's registration identity), but
// agent_id is NEVER an isolation filter: the driver keys each agent's
// config under a synthetic identity whose tenant is the caller's verified
// tenant, so a tenant's config is invisible to another tenant.
//
// # Concurrent reuse (the concurrent-reuse contract)
//
// A constructed Registry is a compiled artifact: immutable after
// construction and safe to share across N concurrent goroutines. Per-run
// state lives in ctx and the supplied arguments, never on the Registry.
package agentcfg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrIdentityRequired — a method was called with an incomplete
	// identity triple or an empty agent id. Fails closed (CLAUDE.md §6).
	ErrIdentityRequired = errors.New("agentcfg: identity triple incomplete")
	// ErrInvalidConfig — a factory was called with a missing mandatory
	// dependency (a nil StateStore or EventBus).
	ErrInvalidConfig = errors.New("agentcfg: invalid configuration")
	// ErrUnknownDriver — Open was asked for a driver name no registered
	// factory handles.
	ErrUnknownDriver = errors.New("agentcfg: unknown driver")
	// ErrClosed — a method was called after Close.
	ErrClosed = errors.New("agentcfg: registry is closed")
	// ErrRevisionNotFound — Get / Diff / Rollback referenced a revision
	// id that has no record. Fails loud (CLAUDE.md §13 — no silent drop).
	ErrRevisionNotFound = errors.New("agentcfg: revision not found")
	// ErrStateUnavailable — a StateStore read/write failed.
	ErrStateUnavailable = errors.New("agentcfg: state store unavailable")
	// ErrInvalidPayload — the supplied ConfigPayload failed validation.
	ErrInvalidPayload = errors.New("agentcfg: invalid config payload")
)

// SkillsSelection is the membership set of skill names active for an
// agent in a revision. The revision records skill MEMBERSHIP (names),
// never skill bodies — the bodies stay in the SkillStore. Resolved at run
// start; applied next-turn.
type SkillsSelection struct {
	// Names is the set of skill names active for the agent in this
	// revision. Order is not significant — the content hash is computed
	// over a canonical sorted, de-duplicated form so a re-ordering does
	// not perturb the hash.
	Names []string `json:"names"`
}

// PromptLayers is the forward-compatible prompt-layer section of the
// config envelope. Reserved for the layered-prompt consumer; declared now
// so the envelope evolves without a schema break. Not wired in the first
// wave.
type PromptLayers struct {
	// Base is the admin-owned, versioned base prompt layer.
	Base *string `json:"base,omitempty"`
	// User is the optional higher user-instruction layer that composes
	// ABOVE the base without mutating it (the composition order is the
	// security boundary — the agent-config authorization model).
	User *string `json:"user,omitempty"`
}

// ToolExposure is the forward-compatible tool/MCP-exposure section of the
// config envelope. Reserved for the MCP pause / per-tool policy consumer;
// declared now so the envelope evolves without a schema break. Not wired
// in the first wave.
type ToolExposure struct {
	// PausedServers names MCP servers excluded from the next run's
	// projection (resume is a flag flip, not a re-dial).
	PausedServers []string `json:"paused_servers,omitempty"`
	// DisabledTools names tools the per-tool policy excludes.
	DisabledTools []string `json:"disabled_tools,omitempty"`
}

// ConfigPayload is the forward-compatible config envelope. Every section
// is an optional pointer so later consumers extend the envelope without a
// schema break; only Skills is wired in the first wave.
type ConfigPayload struct {
	PromptLayers *PromptLayers    `json:"prompt_layers,omitempty"`
	ToolExposure *ToolExposure    `json:"tool_exposure,omitempty"`
	Skills       *SkillsSelection `json:"skills,omitempty"`
}

// Revision is an immutable, content-addressed agent-config record with a
// parent pointer. SetRevision never mutates an existing Revision; it
// writes a new one and advances the active pointer.
type Revision struct {
	// RevisionID is the unique, time-ordered id of this revision.
	RevisionID string
	// ParentRevisionID is the revision this one descends from (the
	// previously-active revision). Empty for the first revision.
	ParentRevisionID string
	// ContentHash is the full hex-encoded SHA-256 over the revision's
	// canonical (sorted-key) payload encoding.
	ContentHash string
	// Author is the identity that wrote the revision (the audit anchor).
	Author identity.Quadruple
	// CreatedAt is the revision's creation instant (UTC).
	CreatedAt time.Time
	// Payload is the config envelope this revision pins.
	Payload ConfigPayload
}

// SkillsDiff is the structured set-diff of the skills membership across
// two revisions. Deterministic: Added / Removed are sorted.
type SkillsDiff struct {
	// Added are the skill names present in the to-revision but not the
	// from-revision.
	Added []string
	// Removed are the skill names present in the from-revision but not
	// the to-revision.
	Removed []string
}

// Changed reports whether the skills membership differs between the two
// revisions.
func (d SkillsDiff) Changed() bool { return len(d.Added) > 0 || len(d.Removed) > 0 }

// Diff is the server-side compare of two revision payloads. The first
// wave carries the structured skills set-diff; later consumers add a text
// diff for the prompt layer and structured diffs for tool exposure.
type Diff struct {
	// FromRevisionID / ToRevisionID name the compared revisions.
	FromRevisionID string
	ToRevisionID   string
	// Skills is the structured set-diff of the skills membership.
	Skills SkillsDiff
	// ToolExposure is the structured set-diff of the MCP-exposure /
	// per-tool policy.
	ToolExposure ToolExposureDiff
}

// Registry is the durable, identity-scoped, versioned desired-state
// store. It is a compiled artifact: immutable after construction and safe
// for concurrent reuse (the concurrent-reuse contract).
type Registry interface {
	// SetRevision writes a NEW immutable revision pinning payload and
	// advances the active pointer to it. An idempotent re-set of
	// byte-identical canonical content (relative to the current active
	// revision) is a no-op that returns the existing active revision.
	SetRevision(ctx context.Context, id identity.Quadruple, agentID string, payload ConfigPayload) (Revision, error)
	// Active returns the agent's current active revision and whether one
	// exists. No active pointer returns (zero, false, nil).
	Active(ctx context.Context, id identity.Quadruple, agentID string) (Revision, bool, error)
	// Get returns the revision identified by revisionID. A missing
	// revision fails loud with ErrRevisionNotFound.
	Get(ctx context.Context, id identity.Quadruple, agentID, revisionID string) (Revision, error)
	// ListRevisions returns the agent's revision chain newest-first,
	// capped at limit (0 = no cap). Enumeration uses the elevated
	// maintenance scan and filters to the agent's identity slot.
	ListRevisions(ctx context.Context, id identity.Quadruple, agentID string, limit int) ([]Revision, error)
	// Rollback writes a new active pointer to an existing revision
	// WITHOUT mutating or deleting any revision. A missing target fails
	// loud with ErrRevisionNotFound.
	Rollback(ctx context.Context, id identity.Quadruple, agentID, revisionID string) (Revision, error)
	// Diff returns the deterministic compare of two existing revisions.
	Diff(ctx context.Context, id identity.Quadruple, agentID, fromRev, toRev string) (Diff, error)
	// Close releases resources. Idempotent.
	Close(ctx context.Context) error
}

// NormalizePayload returns a defensive, canonicalised copy of payload:
// the skills membership and the tool-exposure sets (paused servers,
// disabled tools) are sorted and de-duplicated so a re-ordering does not
// perturb the content hash and the stored form is stable. The prompt-layer
// section is copied verbatim (no nested mutation). It is exported so
// consumers (the skills + tool-exposure services) and the driver share one
// canonical form.
func NormalizePayload(p ConfigPayload) ConfigPayload {
	out := ConfigPayload{
		PromptLayers: p.PromptLayers,
	}
	if p.Skills != nil {
		out.Skills = &SkillsSelection{Names: sortDedup(p.Skills.Names)}
	}
	if p.ToolExposure != nil {
		out.ToolExposure = &ToolExposure{
			PausedServers: sortDedup(p.ToolExposure.PausedServers),
			DisabledTools: sortDedup(p.ToolExposure.DisabledTools),
		}
	}
	return out
}

// ContentHash computes the full hex-encoded SHA-256 over the canonical
// (sorted-key) JSON encoding of the normalised payload. Computing over a
// canonical form means an unrelated re-ordering of a set does not change
// the hash, and the idempotent re-set check is exact.
func ContentHash(p ConfigPayload) (string, error) {
	canon, err := canonicalJSON(NormalizePayload(p))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalJSON marshals v, re-decodes into a generic value, and
// re-marshals so map keys are emitted in sorted order (encoding/json
// sorts map[string]any keys). Struct field order is already deterministic;
// the round-trip makes the encoding canonical across any nested maps a
// later envelope section may introduce.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal: %w", ErrInvalidPayload, err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("%w: canonical re-decode: %w", ErrInvalidPayload, err)
	}
	canon, err := json.Marshal(generic)
	if err != nil {
		return nil, fmt.Errorf("%w: canonical marshal: %w", ErrInvalidPayload, err)
	}
	return canon, nil
}

// sortDedup returns a sorted, de-duplicated copy of in. A nil input
// returns nil (preserving the absent/present distinction) — an empty
// non-nil slice returns an empty non-nil slice.
func sortDedup(in []string) []string {
	if in == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// SkillNames returns the agent's active skills membership from a payload,
// or nil when the payload pins no skills section. A convenience for the
// skills consumer.
func (p ConfigPayload) SkillNames() []string {
	if p.Skills == nil {
		return nil
	}
	return p.Skills.Names
}

// PausedServers returns the agent's paused MCP server set from a payload,
// or nil when the payload pins no tool-exposure section. A convenience for
// the tool-exposure consumer + the run-start projection.
func (p ConfigPayload) PausedServers() []string {
	if p.ToolExposure == nil {
		return nil
	}
	return p.ToolExposure.PausedServers
}

// DisabledTools returns the agent's disabled tool set from a payload, or
// nil when the payload pins no tool-exposure section.
func (p ConfigPayload) DisabledTools() []string {
	if p.ToolExposure == nil {
		return nil
	}
	return p.ToolExposure.DisabledTools
}

// ToolExposureDiff is the structured set-diff of the MCP-exposure /
// per-tool policy across two revisions. Deterministic: every slice is
// sorted.
type ToolExposureDiff struct {
	// PausedAdded / PausedResumed are the MCP servers newly paused / newly
	// resumed (present-in-to-but-not-from / present-in-from-but-not-to).
	PausedAdded   []string
	PausedResumed []string
	// DisabledAdded / DisabledEnabled are the tools newly disabled / newly
	// re-enabled.
	DisabledAdded   []string
	DisabledEnabled []string
}

// Changed reports whether the tool exposure differs between the two
// revisions.
func (d ToolExposureDiff) Changed() bool {
	return len(d.PausedAdded) > 0 || len(d.PausedResumed) > 0 ||
		len(d.DisabledAdded) > 0 || len(d.DisabledEnabled) > 0
}

// DiffToolExposure computes the structured set-diff of two tool-exposure
// states. Exported so the diff is one canonical implementation shared by
// the driver and tests.
func DiffToolExposure(from, to ConfigPayload) ToolExposureDiff {
	pAdded, pResumed := setDiff(from.PausedServers(), to.PausedServers())
	dAdded, dEnabled := setDiff(from.DisabledTools(), to.DisabledTools())
	return ToolExposureDiff{
		PausedAdded:     pAdded,
		PausedResumed:   pResumed,
		DisabledAdded:   dAdded,
		DisabledEnabled: dEnabled,
	}
}

// setDiff returns (in to but not from, in from but not to), each sorted.
func setDiff(from, to []string) (added, removed []string) {
	fromSet := make(map[string]struct{}, len(from))
	for _, s := range from {
		fromSet[s] = struct{}{}
	}
	toSet := make(map[string]struct{}, len(to))
	for _, s := range to {
		toSet[s] = struct{}{}
	}
	for s := range toSet {
		if _, ok := fromSet[s]; !ok {
			added = append(added, s)
		}
	}
	for s := range fromSet {
		if _, ok := toSet[s]; !ok {
			removed = append(removed, s)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// DiffSkills computes the structured set-diff of two skills memberships.
// Exported so the diff is one canonical implementation shared by the
// driver and tests.
func DiffSkills(from, to []string) SkillsDiff {
	fromSet := make(map[string]struct{}, len(from))
	for _, s := range from {
		fromSet[s] = struct{}{}
	}
	toSet := make(map[string]struct{}, len(to))
	for _, s := range to {
		toSet[s] = struct{}{}
	}
	var added, removed []string
	for s := range toSet {
		if _, ok := fromSet[s]; !ok {
			added = append(added, s)
		}
	}
	for s := range fromSet {
		if _, ok := toSet[s]; !ok {
			removed = append(removed, s)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return SkillsDiff{Added: added, Removed: removed}
}
