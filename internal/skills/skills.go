// Package skills owns Harbor's token-savvy, DB-backed, identity-
// scoped skill subsystem (RFC §6.7). lands the leaf surface:
//
//   - The mandatory `SkillStore` interface every backend implements.
//   - The shared types — `Skill`, `Origin`, `Scope`, `ListFilter`,
//     `RankedSkill`.
//   - Sentinel errors compared via `errors.Is`.
//   - The §4.4 extensibility-seam plumbing (registry + factory).
//
// sibling layers (planner-facing tools, the virtual directory,
// the Skills.md importer, the in-runtime generator with
// persistence) all consume this surface.
//
// Identity is mandatory at every method. The triple
// `(tenant, user, session)` MUST be fully populated; empty `RunID`
// is accepted (skills are session-scoped at the storage layer; the
// generator stamps `OriginRef` with the run id from `ctx`).
// Missing-triple operations fail closed with `ErrIdentityRequired`
// AND emit a `skill.identity_rejected` event on the bus — never
// silent (AGENTS.md §5 "Fail loudly").
//
// Concurrent reuse: one `SkillStore` instance is safe to
// share across N concurrent goroutines. Drivers persist only
// internally-synchronized state on themselves; per-call state lives
// in `ctx` and the supplied `identity.Quadruple`.
package skills

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// Origin is the provenance of a skill — the pack import path or the
// in-runtime generator path. The two flavors share storage but their
// conflict semantics differ: a `Generated` skill MAY NOT overwrite a
// `PackImport` skill of the same name.
type Origin string

// Origin values.
const (
	// OriginPack — imported from a Skills.md pack (lands the
	// importer). Pack rows are immutable from the generator's
	// perspective: `Upsert` refuses to overwrite with
	// `ErrPackOverwriteRefused` when incoming Origin != OriginPack.
	OriginPack Origin = "pack"
	// OriginGenerated — produced by the in-runtime generator
	// (`skill_propose(persist=true)`). Generated→Generated
	// is last-write-wins gated by `ContentHash` change.
	OriginGenerated Origin = "generated"
)

// Scope is the operator-declared visibility of a skill.
type Scope string

// Scope values.
const (
	// ScopeSession — visible only inside the originating session.
	// The narrowest scope; cross-session visibility requires an
	// explicit promotion (RFC §6.7 "isolation
	// conformance"). The storage layer's identity filter already
	// pins rows to a `(tenant, user, session)` triple; ScopeSession
	// is the matching declared-scope marker for rows that should
	// stay session-local.
	ScopeSession Scope = "session"
	// ScopeUser — visible to EVERY session of the same (tenant, user).
	// The durable-by-default rung for a user's personal skills: an
	// authored skill persists across all of that user's conversations
	// rather than dying with the originating session. Rows are stored
	// session-zeroed (the session storage component is emptied via
	// StorageSessionID) so the identity filter resolves them for any
	// session of the same (tenant, user); physical durability rides the
	// driver (the in-memory dev store is ephemeral, sqlite/postgres
	// survive restart). The isolation principal stays (tenant, user) —
	// the row is never widened past the user, and a different user or a
	// different tenant never sees it. A user-scoped skill cannot widen
	// capability: RequiredTools is provenance/filter metadata, and the
	// injection-time redactor scrubs any tool a skill names that is not
	// in the run's allowed set.
	ScopeUser Scope = "user"
	// ScopeProject — visible inside the operator-declared project
	// only. The generator default per RFC §6.7.
	ScopeProject Scope = "project"
	// ScopeTenant — visible to every project inside the same tenant.
	ScopeTenant Scope = "tenant"
	// ScopeGlobal — visible to every tenant. Reserved for operator-
	// managed shared skills.
	ScopeGlobal Scope = "global"
)

// Skill is the canonical skill record. The struct is the storage
// envelope drivers persist; the planner-facing tools wrap
// it with capability filtering + redaction at injection time.
//
// Validation rules at `validate`:
//
//   - `Name` non-empty
//   - `Trigger` non-empty (planner-visible match cue)
//   - `Steps` non-empty (at least one step)
//   - `Origin` ∈ {OriginPack, OriginGenerated}
//   - `Scope` ∈ {ScopeSession, ScopeUser, ScopeProject, ScopeTenant, ScopeGlobal}
type Skill struct {
	Name string
	// AgentID is selection metadata for an agent-owned skill. It is not an
	// isolation principal; tenant/user/session remain the security boundary.
	AgentID        string
	Title          string
	Description    string
	Trigger        string
	TaskType       string
	Tags           []string
	Steps          []string
	Preconditions  []string
	FailureModes   []string
	RequiredTools  []string
	RequiredNS     []string
	RequiredTags   []string
	Origin         Origin
	OriginRef      string
	Scope          Scope
	ScopeTenantID  string
	ScopeProjectID string
	ContentHash    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastUsed       time.Time
	UseCount       int
	Extra          map[string]any
}

// Validate returns `ErrInvalidSkill` when any mandatory field is
// missing or out-of-range. Drivers call this at the boundary so a
// caller's bad payload surfaces at `Upsert` rather than later via a
// corrupt row.
func (s Skill) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("%w: Name empty", ErrInvalidSkill)
	}
	if strings.TrimSpace(s.Trigger) == "" {
		return fmt.Errorf("%w: Trigger empty (planner match cue is mandatory)", ErrInvalidSkill)
	}
	if len(s.Steps) == 0 {
		return fmt.Errorf("%w: Steps empty (skills must declare ≥ 1 step)", ErrInvalidSkill)
	}
	switch s.Origin {
	case OriginPack, OriginGenerated:
	default:
		return fmt.Errorf("%w: Origin=%q (expected pack|generated)", ErrInvalidSkill, s.Origin)
	}
	switch s.Scope {
	case ScopeSession, ScopeUser, ScopeProject, ScopeTenant, ScopeGlobal:
	default:
		return fmt.Errorf("%w: Scope=%q (expected session|user|project|tenant|global)", ErrInvalidSkill, s.Scope)
	}
	return nil
}

// ListFilter narrows the rows `List` returns. Zero-value fields are
// matched as "any". Drivers cap `Limit` at 1000; `Limit == 0` falls
// back to the driver default (100).
type ListFilter struct {
	Scope Scope
	// AgentID selects an agent-owned namespace without widening identity.
	AgentID  string
	TaskType string
	Tags     []string // any-of match against the skill's `Tags`
	Limit    int
	Offset   int
}

// RankedSkill carries the search-time relevance score + the path
// that produced it. `Path` identifies the actual ranking engine (for example,
// `"fts5"`, `"full_text"`, `"regex"`, `"exact"`, or `"semantic"`);
// callers (the planner tools) surface it for observability
// only — it is not part of the ranking math.
//
// `Score` is the normalised 0.0–1.0 score:
//
//   - FTS5 path: `bm25 → 1/(1+raw) → min-max normalised`.
//   - Regex path: `name fullmatch=0.95 | name match=0.90 |
//     name search=0.85 | body search=0.75`.
//   - Exact path: 1.0 (lowercase equality on
//     `name | title | trigger | tags`).
type RankedSkill struct {
	Skill Skill
	Score float64
	Path  string
}

// SnapshotCandidateSearcher ranks one immutable, already-authorized candidate
// view. It is implemented by every configured SkillStore driver, rather than
// by an ad-hoc portable scorer: a frozen composed view must retain the base
// driver's actual full-text availability and ranking semantics.
//
// Candidates are complete copies from a single run-start view. Implementations
// MUST rank only those candidates, preserve their configured retrieval policy,
// and fail loudly rather than falling back from semantic retrieval when its
// Embedder is unavailable or fails. They must honour ctx and are safe for
// concurrent reuse.
type SnapshotCandidateSearcher interface {
	SearchSnapshot(ctx context.Context, id identity.Quadruple, query string, candidates []Skill, limit int) ([]RankedSkill, error)
}

// Search-result paths.
const (
	// PathFTS5 — FTS5 virtual table produced the row.
	PathFTS5 = "fts5"
	// PathFullText — a non-FTS5 backend-native full-text engine produced the
	// row. PostgreSQL's to_tsvector/to_tsquery path uses this value.
	PathFullText = "full_text"
	// PathRegex — regex fallback produced the row.
	PathRegex = "regex"
	// PathExact — exact lowercase-equality fallback produced the row.
	PathExact = "exact"
	// PathSemantic — the opt-in semantic retrieval mode produced the
	// row: embedding-similarity ranking over the identity-scoped
	// catalog (RetrievalSemantic).
	PathSemantic = "semantic"
)

// RetrievalMode declares the opt-in `Search` ranking shape.
type RetrievalMode string

// RetrievalMode values.
const (
	// RetrievalDefault — the zero value: the driver's token-savvy full-text →
	// regex → exact ladder.
	RetrievalDefault RetrievalMode = ""
	// RetrievalSemantic — rank by embedding similarity over the
	// identity-scoped catalog (result path `PathSemantic`). Requires
	// `Deps.Embedder` — no stub fallback. Capability filtering,
	// redaction, and the budgeter apply unchanged downstream.
	RetrievalSemantic RetrievalMode = "semantic"
)

// Embedder is the injectable text→vector callable the semantic
// retrieval mode consumes — the consumer-side-interface shape the
// memory subsystem's `Summarizer` established. The canonical
// implementation is constructed via the embeddings factory
// (`internal/embeddings`); any `embeddings.Embedder` satisfies this
// interface.
//
// Concurrent-reuse contract: one `Embedder` instance is safe to
// share across N concurrent goroutines. Implementers MUST honour
// `ctx.Done()`.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// SkillReader is Harbor's identity-mandatory read-only skill-storage
// interface. It includes both the normal precedence-aware read and the
// exact-rung read used by migrations that must never fall through to a
// wider scope.
type SkillReader interface {
	// Get returns the skill identified by `name` under the supplied
	// identity using the store's normal scope-precedence rules. Missing →
	// `ErrSkillNotFound`.
	Get(ctx context.Context, id identity.Quadruple, name string) (Skill, error)

	// GetScope returns the skill identified by `name` at exactly `scope`.
	// It never falls through to another visibility rung. ScopeUser is keyed
	// by `(tenant, user)`; every other V1 rung is additionally pinned to the
	// supplied session. Missing exact rung → `ErrSkillNotFound`.
	GetScope(ctx context.Context, id identity.Quadruple, name string, scope Scope) (Skill, error)

	// List returns the filtered, paged skills under the supplied
	// identity. Ordering is deterministic: `(UpdatedAt DESC,
	// Name ASC)`.
	List(ctx context.Context, id identity.Quadruple, filter ListFilter) ([]Skill, error)

	// Search returns up to `limit` skills ranked by the FTS5 →
	// regex → exact ladder. `limit == 0` falls back
	// to 20. The driver picks the first path that returns rows;
	// later paths run only when earlier ones produced nothing.
	// Emits `skill.search_executed` with the path that produced the
	// result.
	Search(ctx context.Context, id identity.Quadruple, query string, limit int) ([]RankedSkill, error)
}

// SkillStore is Harbor's mandatory skill-storage interface. A single
// surface; every V1 driver (`localdb` here; Portico post-V1)
// implements every method. No `Supports*` ceremony per AGENTS.md
// §4.4.
//
// Identity-mandatory contract:
//
//   - Every method validates the identity `Quadruple` at the
//     boundary. Empty tenant / user / session returns wrapped
//     `ErrIdentityRequired` AND emits one `skill.identity_rejected`
//     event on the bus.
//
// Concurrent-reuse contract:
//
//   - One instance is safe to share across N concurrent goroutines.
//     Mutable state is internally synchronised; per-call state lives
//     in `ctx` and the supplied `Quadruple`, never on the driver.
type SkillStore interface {
	SkillReader
	SnapshotCandidateSearcher

	// GetScopeAgent selects the requested agent-bound row, falling back to the
	// legacy unbound row at the same scope. A bound row always wins a same-name
	// collision; rows bound to another agent are never visible.
	GetScopeAgent(ctx context.Context, id identity.Quadruple, agentID, name string, scope Scope) (Skill, error)

	// SearchAgent searches rows bound to agentID plus legacy unbound rows,
	// giving bound rows precedence for duplicate names.
	SearchAgent(ctx context.Context, id identity.Quadruple, agentID, query string, limit int) ([]RankedSkill, error)

	// Upsert inserts or updates `skill` under the identity-scoped
	// `(tenant, user, session, scope, agent_id, name)` key. AgentID is
	// selection metadata, not an isolation principal. Conflict policy
	// (RFC §6.7):
	//
	//   - existing.Origin == "pack" && skill.Origin != "pack" →
	//     `ErrPackOverwriteRefused` AND
	//     `skill.pack_overwrite_refused` emit. Row left untouched.
	//   - existing.Origin == "generated" && skill.Origin ==
	//     "generated" && existing.ContentHash == skill.ContentHash →
	//     idempotent no-op; emit a single `skill.upserted` for
	//     observability with `idempotent=true` payload field.
	//   - otherwise: last-write-wins; emit `skill.upserted` with
	//     `idempotent=false`.
	Upsert(ctx context.Context, id identity.Quadruple, skill Skill) error

	// Delete removes the named skill under the identity, at the target
	// `scope`. The scope makes the DESTRUCTIVE op RUNG-PRECISE so an
	// ephemeral session delete can never destroy a durable user-scope skill
	// (and vice versa) — the read filter unions the session and user rungs,
	// but a delete MUST NOT cross that boundary:
	//
	//   - scope == ScopeUser: deletes ONLY the durable user-scope row, keyed
	//     `(tenant, user)` with the session independent (the cross-session
	//     durable delete — the intended semantics of the user verb). Never
	//     touches a same-named session-local row.
	//   - any other scope: deletes ONLY the caller's session-local,
	//     NON-durable row(s) of that name (session pinned, `scope != user`) —
	//     never touches a durable user-scope row. This is the pre-user-rung
	//     behaviour for the session/admin/generator/CLI callers.
	//
	// Missing → `ErrSkillNotFound`. Emits `skill.deleted` on success.
	Delete(ctx context.Context, id identity.Quadruple, name string, scope Scope) error

	// DeleteAgent deletes only the requested agent binding at the exact rung.
	DeleteAgent(ctx context.Context, id identity.Quadruple, agentID, name string, scope Scope) error

	// DeleteSessionScope removes every legacy ScopeSession row under exactly
	// `id`'s (tenant, user, session) triple. It is idempotent: a completed
	// sweep, including one that found no rows, returns nil. It never lists or
	// deletes ScopeUser or any other shared scope. Session erasure calls this
	// destructive operation before clearing the StateStore scope so an
	// interrupted cascade can retry to convergence without widening identity.
	DeleteSessionScope(ctx context.Context, id identity.Quadruple) error

	// Close releases the driver's resources. Subsequent method
	// calls return `ErrStoreClosed`. Close is idempotent.
	Close(ctx context.Context) error
}

// AgentSelectableSkillStore is retained as a source-compatible alias. Agent
// selection is mandatory on SkillStore; it is not an optional capability.
type AgentSelectableSkillStore = SkillStore

// Sentinel errors. Compare via `errors.Is`.
var (
	// ErrSkillNotFound — `Get` / `Delete` against a non-existent
	// row.
	ErrSkillNotFound = errors.New("skills: skill not found")
	// ErrPackOverwriteRefused — `Upsert` attempted to overwrite an
	// `Origin=pack` row with non-pack input.
	ErrPackOverwriteRefused = errors.New("skills: refuse to overwrite pack-origin skill")
	// ErrStoreClosed — store has been closed; further operations
	// are rejected.
	ErrStoreClosed = errors.New("skills: store is closed")
	// ErrInvalidSkill — supplied `Skill` failed validation.
	ErrInvalidSkill = errors.New("skills: invalid skill")
	// ErrUnknownDriver — `Open` was asked for a driver name no
	// factory has been registered under.
	ErrUnknownDriver = errors.New("skills: unknown driver")
	// ErrIdentityRequired — caller passed a `Quadruple` with at
	// least one empty `(tenant, user, session)` component. The
	// store also emits `skill.identity_rejected` on the bus.
	ErrIdentityRequired = errors.New("skills: identity triple incomplete")
)

// ConfigSnapshot is the strict subset of `config.SkillsConfig` the
// skills package consumes. Keeping a snapshot decouples drivers from
// the config package's type evolution.
type ConfigSnapshot struct {
	// Driver names the registered factory. Empty → DefaultDriver.
	Driver string
	// DSN is consumed by the `localdb` driver. Bare file path or
	// SQLite `file:` URI; the special `:memory:` sentinel is
	// honoured for tests. `secret:"true"` redaction lives at the
	// config-package boundary.
	DSN string
	// Retrieval opts in to a `Search` ranking mode. The zero value
	// keeps the FTS5 → regex → exact ladder; `RetrievalSemantic`
	// ranks by embedding similarity (requires `Deps.Embedder`).
	Retrieval RetrievalMode
}

// Deps carries the runtime dependencies a skills driver needs.
//
// `Bus` is mandatory so identity-rejection emits + audit events
// (`skill.upserted`, `skill.deleted`, `skill.pack_overwrite_refused`,
// `skill.search_executed`) land on the audit pipeline.
//
// **Note** (analog): unlike memory drivers, skills drivers do
// NOT receive a `state.StateStore`. The `localdb` driver owns its
// own `skills` + `skills_fts` tables; persistent skill state lives
// in the driver's own DB, not piggybacked onto the StateStore. The
// Portico driver (post-V1) will fetch from a remote MCP surface and
// also has no StateStore need.
// `Embedder` is the injectable text→vector callable the semantic
// retrieval mode consumes. OPTIONAL — required only when
// `cfg.Retrieval == RetrievalSemantic`, ignored otherwise. A
// semantic config without an Embedder fails loudly at `Open`
// (mirroring the memory registry's Summarizer/Embedder rule) —
// never a stub fallback (AGENTS.md §13). Existing callers that
// construct `Deps{Bus}` keep compiling: the zero value is nil,
// valid for the default retrieval mode.
type Deps struct {
	Bus      events.EventBus
	Embedder Embedder
}

// Factory builds a `SkillStore` from a `ConfigSnapshot` + `Deps`.
// Drivers expose one `Factory` each via `init()` → `Register`.
type Factory func(cfg ConfigSnapshot, deps Deps) (SkillStore, error)

// DefaultDriver is the production driver name. later phases
// (Portico) registers additional names.
const DefaultDriver = "localdb"

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

// Register installs a driver factory under `name`. Drivers self-
// register from their package `init()`; `cmd/harbor` blank-imports
// the production driver to trigger registration. Per AGENTS.md §4.4.
//
// Re-registering the same name panics — the registration model is
// write-once-at-init and a duplicate signals a build mis-config.
func Register(name string, factory Factory) {
	if name == "" {
		panic("skills: Register called with empty name")
	}
	if factory == nil {
		panic(fmt.Sprintf("skills: Register(%q) called with nil factory", name))
	}
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("skills: driver %q already registered", name))
	}
	factories[name] = factory
}

// Open returns the `SkillStore` built by the factory whose name
// matches `cfg.Driver` (defaults to `DefaultDriver` when empty).
//
// Deps are validated: a missing EventBus returns a wrapped error
// before the factory runs — fail loudly, never silently degrade.
func Open(_ context.Context, cfg ConfigSnapshot, deps Deps) (SkillStore, error) {
	if err := validateDeps(cfg, deps); err != nil {
		return nil, err
	}
	name := cfg.Driver
	if name == "" {
		name = DefaultDriver
	}
	return open(name, cfg, deps)
}

// OpenDriver opens a specific driver by name; useful for tests that
// want to exercise the registry against a non-default driver.
func OpenDriver(name string, cfg ConfigSnapshot, deps Deps) (SkillStore, error) {
	if err := validateDeps(cfg, deps); err != nil {
		return nil, err
	}
	return open(name, cfg, deps)
}

func validateDeps(cfg ConfigSnapshot, d Deps) error {
	if d.Bus == nil {
		return fmt.Errorf("skills: Deps.Bus is required (events.EventBus)")
	}
	// Fail loudly at the registry boundary when semantic retrieval
	// is configured without an Embedder — surfacing the
	// misconfiguration before any DB connection opens, never a stub
	// fallback (AGENTS.md §13). The driver constructor re-checks.
	switch cfg.Retrieval {
	case RetrievalDefault:
	case RetrievalSemantic:
		if d.Embedder == nil {
			return fmt.Errorf("skills: Deps.Embedder is required for retrieval mode %q (no stub fallback)", RetrievalSemantic)
		}
	default:
		return fmt.Errorf("skills: unknown retrieval mode %q (expected \"\" or %q)", cfg.Retrieval, RetrievalSemantic)
	}
	return nil
}

func open(name string, cfg ConfigSnapshot, deps Deps) (SkillStore, error) {
	factoriesMu.RLock()
	f, ok := factories[name]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %s)",
			ErrUnknownDriver, name, registeredNames())
	}
	return f(cfg, deps)
}

// RegisteredDrivers returns a sorted list of driver names. Useful for
// boot-log emission and for surfacing in error messages.
func RegisteredDrivers() []string {
	factoriesMu.RLock()
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
	factoriesMu.RUnlock()
	sort.Strings(names)
	return names
}

func registeredNames() string {
	names := RegisteredDrivers()
	if len(names) == 0 {
		return "<none>"
	}
	return strings.Join(names, ",")
}

// ValidateIdentity returns wrapped `ErrIdentityRequired` when any of
// `(TenantID, UserID, SessionID)` on `q` is empty. `RunID` is
// allowed to be empty (skills are session-scoped at the storage
// layer). Mirrors `memory.ValidateIdentity` for the identity-
// mandatory contract.
func ValidateIdentity(q identity.Quadruple) error {
	if err := identity.Validate(q.Identity); err != nil {
		return fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	return nil
}

// StorageSessionID returns the session component a skill of the given
// `scope` is PERSISTED under. `ScopeUser` rows are stored session-zeroed
// ("") so the identity filter resolves them for every session of the same
// `(tenant, user)` — the durable-by-default rung. Every other scope pins
// the caller's real `SessionID`, so those rows stay session-local exactly
// as before.
//
// Identity remains mandatory: callers still validate the full triple via
// `ValidateIdentity` BEFORE deriving the storage session — zeroing the
// PERSISTED session for a user-scope row never relaxes the identity gate,
// it only widens the READ visibility to the row's owning `(tenant, user)`.
// This mirrors the agent-config user-scope keying (session + run zeroed in
// storage, identity still validated).
func StorageSessionID(id identity.Quadruple, scope Scope) string {
	if scope == ScopeUser {
		return ""
	}
	return id.SessionID
}

// UserScopeStorageSession is the persisted session sentinel for a
// `ScopeUser` row: the empty string. A row carrying this session value is
// resolvable from every session of the same `(tenant, user)`; because the
// identity contract forbids an empty caller session, no NON-user row ever
// carries it, so it uniquely marks the user rung in the read filter.
const UserScopeStorageSession = ""

// IdentityFromCtx reads the identity `Quadruple` (or bare `Identity`)
// from `ctx` and validates the triple. It returns the (possibly
// partial) Quadruple plus a wrapped `ErrIdentityRequired` on failure;
// the partial value is what callers pass to `EmitIdentityRejected`,
// which substitutes the missing-component sentinel for the bus's
// `ValidateEvent` triple check.
//
// Single source for the planner tools (`internal/skills/tools`) and
// the virtual directory (`internal/skills`) — both consumed the same
// byte-for-byte logic before this was hoisted here.
func IdentityFromCtx(ctx context.Context) (identity.Quadruple, error) {
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		if err := identity.Validate(q.Identity); err != nil {
			return q, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
		}
		return q, nil
	}
	if id, ok := identity.From(ctx); ok {
		q := identity.Quadruple{Identity: id}
		if err := identity.Validate(id); err != nil {
			return q, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
		}
		return q, nil
	}
	return identity.Quadruple{}, fmt.Errorf("%w: no identity in ctx", ErrIdentityRequired)
}
