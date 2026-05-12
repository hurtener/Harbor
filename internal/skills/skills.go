// Package skills owns Harbor's token-savvy, DB-backed, identity-
// scoped skill subsystem (RFC §6.7). Phase 37 lands the leaf surface:
//
//   - The mandatory `SkillStore` interface every backend implements.
//   - The shared types — `Skill`, `Origin`, `Scope`, `ListFilter`,
//     `RankedSkill`.
//   - Sentinel errors compared via `errors.Is`.
//   - The §4.4 extensibility-seam plumbing (registry + factory).
//
// Phase 38 (planner-facing tools), Phase 39 (virtual directory),
// Phase 40 (Skills.md importer), Phase 41 (in-runtime generator with
// persistence) all consume this surface.
//
// Identity is mandatory at every method (D-001). The triple
// `(tenant, user, session)` MUST be fully populated; empty `RunID`
// is accepted (skills are session-scoped at the storage layer; the
// generator stamps `OriginRef` with the run id from `ctx`).
// Missing-triple operations fail closed with `ErrIdentityRequired`
// AND emit a `skill.identity_rejected` event on the bus — never
// silent (AGENTS.md §5 "Fail loudly", brief 04 §4.2).
//
// Concurrent reuse (D-025): one `SkillStore` instance is safe to
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
// `PackImport` skill of the same name (brief 04 §4.8, RFC §6.7).
type Origin string

// Origin values.
const (
	// OriginPack — imported from a Skills.md pack (Phase 40 lands the
	// importer). Pack rows are immutable from the generator's
	// perspective: `Upsert` refuses to overwrite with
	// `ErrPackOverwriteRefused` when incoming Origin != OriginPack.
	OriginPack Origin = "pack"
	// OriginGenerated — produced by the in-runtime generator
	// (`skill_propose(persist=true)`, Phase 41). Generated→Generated
	// is last-write-wins gated by `ContentHash` change.
	OriginGenerated Origin = "generated"
)

// Scope is the operator-declared visibility of a skill.
type Scope string

// Scope values.
const (
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
// envelope drivers persist; the planner-facing tools (Phase 38) wrap
// it with capability filtering + redaction at injection time.
//
// Validation rules at `validate`:
//
//   - `Name` non-empty
//   - `Trigger` non-empty (planner-visible match cue, brief 04 §4.7)
//   - `Steps` non-empty (at least one step)
//   - `Origin` ∈ {OriginPack, OriginGenerated}
//   - `Scope` ∈ {ScopeProject, ScopeTenant, ScopeGlobal}
type Skill struct {
	Name           string
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
	case ScopeProject, ScopeTenant, ScopeGlobal:
	default:
		return fmt.Errorf("%w: Scope=%q (expected project|tenant|global)", ErrInvalidSkill, s.Scope)
	}
	return nil
}

// ListFilter narrows the rows `List` returns. Zero-value fields are
// matched as "any". Drivers cap `Limit` at 1000; `Limit == 0` falls
// back to the driver default (100).
type ListFilter struct {
	Scope    Scope
	TaskType string
	Tags     []string // any-of match against the skill's `Tags`
	Limit    int
	Offset   int
}

// RankedSkill carries the search-time relevance score + the path
// that produced it. `Path` is one of `"fts5" | "regex" | "exact"`;
// callers (Phase 38's planner tools) surface it for observability
// only — it is not part of the ranking math.
//
// `Score` is the normalised 0.0–1.0 score per brief 04 §4.4:
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

// Search-result paths.
const (
	// PathFTS5 — FTS5 virtual table produced the row.
	PathFTS5 = "fts5"
	// PathRegex — regex fallback produced the row.
	PathRegex = "regex"
	// PathExact — exact lowercase-equality fallback produced the row.
	PathExact = "exact"
)

// SkillStore is Harbor's mandatory skill-storage interface. A single
// surface; every V1 driver (`localdb` here; Portico post-V1)
// implements every method. No `Supports*` ceremony per AGENTS.md
// §4.4.
//
// Identity-mandatory contract (D-001):
//
//   - Every method validates the identity `Quadruple` at the
//     boundary. Empty tenant / user / session returns wrapped
//     `ErrIdentityRequired` AND emits one `skill.identity_rejected`
//     event on the bus.
//
// Concurrent-reuse contract (D-025):
//
//   - One instance is safe to share across N concurrent goroutines.
//     Mutable state is internally synchronised; per-call state lives
//     in `ctx` and the supplied `Quadruple`, never on the driver.
type SkillStore interface {
	// Upsert inserts or updates `skill` under the identity-scoped
	// `(tenant, user, session, scope, name)` key. Conflict policy
	// (RFC §6.7, brief 04 §4.8):
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

	// Get returns the skill identified by `name` under the supplied
	// identity. Missing → `ErrSkillNotFound`.
	Get(ctx context.Context, id identity.Quadruple, name string) (Skill, error)

	// List returns the filtered, paged skills under the supplied
	// identity. Ordering is deterministic: `(UpdatedAt DESC,
	// Name ASC)`.
	List(ctx context.Context, id identity.Quadruple, filter ListFilter) ([]Skill, error)

	// Search returns up to `limit` skills ranked by the FTS5 →
	// regex → exact ladder (brief 04 §4.4). `limit == 0` falls back
	// to 20. The driver picks the first path that returns rows;
	// later paths run only when earlier ones produced nothing.
	// Emits `skill.search_executed` with the path that produced the
	// result.
	Search(ctx context.Context, id identity.Quadruple, query string, limit int) ([]RankedSkill, error)

	// Delete removes the named skill under the identity. Missing →
	// `ErrSkillNotFound`. Emits `skill.deleted` on success.
	Delete(ctx context.Context, id identity.Quadruple, name string) error

	// Close releases the driver's resources. Subsequent method
	// calls return `ErrStoreClosed`. Close is idempotent.
	Close(ctx context.Context) error
}

// Sentinel errors. Compare via `errors.Is`.
var (
	// ErrSkillNotFound — `Get` / `Delete` against a non-existent
	// row.
	ErrSkillNotFound = errors.New("skills: skill not found")
	// ErrPackOverwriteRefused — `Upsert` attempted to overwrite an
	// `Origin=pack` row with non-pack input. Brief 04 §4.8.
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
}

// Deps carries the runtime dependencies a skills driver needs.
//
// `Bus` is mandatory so identity-rejection emits + audit events
// (`skill.upserted`, `skill.deleted`, `skill.pack_overwrite_refused`,
// `skill.search_executed`) land on the audit pipeline.
//
// **Note** (D-034 analog): unlike memory drivers, skills drivers do
// NOT receive a `state.StateStore`. The `localdb` driver owns its
// own `skills` + `skills_fts` tables; persistent skill state lives
// in the driver's own DB, not piggybacked onto the StateStore. The
// Portico driver (post-V1) will fetch from a remote MCP surface and
// also has no StateStore need.
type Deps struct {
	Bus events.EventBus
}

// Factory builds a `SkillStore` from a `ConfigSnapshot` + `Deps`.
// Drivers expose one `Factory` each via `init()` → `Register`.
type Factory func(cfg ConfigSnapshot, deps Deps) (SkillStore, error)

// DefaultDriver is the Phase 37 production driver name. Phase 49+
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
	if err := validateDeps(deps); err != nil {
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
	if err := validateDeps(deps); err != nil {
		return nil, err
	}
	return open(name, cfg, deps)
}

func validateDeps(d Deps) error {
	if d.Bus == nil {
		return fmt.Errorf("skills: Deps.Bus is required (events.EventBus)")
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
		return fmt.Errorf("%w: %v", ErrIdentityRequired, err)
	}
	return nil
}
