// Package registry owns Harbor's Agent Registry — the in-process,
// per-runtime-instance subsystem that owns the *registration identity*
// of agents (RFC §6.16, D-059 / D-060).
//
// There is no central Harbor service and there must not be one: every
// `harbor` process (and every embedding of the library) has its own
// AgentRegistry, persisted via that instance's configured StateStore
// driver (in-mem / SQLite / Postgres — the §9 persistence triad, behind
// the §4.4 seam). The registry consumes the *existing* StateStore seam
// (D-027); it does not define a driver seam of its own.
//
// # The three-ID model (D-059)
//
// Each registered agent carries three identifiers, each answering a
// different question:
//
//   - agent_id      — "which logical agent." Minted once at first
//     registration (ULID), persisted, rehydrated on restart.
//     Runtime-instance-local, collision-free by construction; never
//     assumed globally unique.
//   - incarnation   — "which boot of it." Ephemeral; bumps on every
//     process start (every Register of an already-known agent).
//   - version_hash  — "which configuration." Deterministic content hash
//     over (prompt set, tool set + schemas, planner config, model
//     policy); bumps ONLY when configuration content changes.
//
// A plain restart yields the same agent_id + same version_hash + a new
// incarnation; a restart after a configuration edit bumps both
// incarnation and version_hash. restart != recreate: re-registering the
// same RegistrationKey keeps the agent_id and the StateStore record;
// Deregister-then-Register of the same key mints a fresh agent_id
// because it is a new logical entity.
//
// # agent_id is NOT an isolation principal
//
// Harbor's isolation boundary is and stays the tuple
// (tenant, user, session) (+ run for the quadruple). An agent is a
// runtime *entity* that runs *within* (tenant, user, session); it does
// not widen the isolation boundary. The registry's storage methods
// scope by the tuple, NEVER by agent_id — agent_id is a registration
// identity, not a WHERE-clause isolation filter (D-059, AGENTS.md §6).
//
// # Two creation cases (D-060)
//
//   - Locally-hosted agent — the runtime instance is running the agent;
//     Register mints a local agent_id.
//   - Connect-to-remote agent — the agent runs in another Harbor (or is
//     any A2A-speaking peer); RegisterRemote assigns a *handle*
//     (an agent_id local to this instance, Hosting == HostingRemote)
//     and stores an AgentCardRef pointing at the canonical A2A
//     AgentCard, owned by the remote operator.
//
// # Fleet privilege tiers (D-066)
//
// Fleet *observation* (Get / List / Inspect / ReportHealth) requires the
// ordinary identity scope. Fleet *control* (Pause / Drain / Restart /
// ForceStop) requires a distinct, more-elevated control-scope claim —
// a leaked read-only token must not be able to force-stop a fleet.
// Every fleet-control command is audit-redacted and emitted. The
// control-scope claim is trust-based in V1 (mirrors the events
// package's Phase-05 Admin claim); cryptographic verification arrives
// with Protocol auth (Phase 61).
//
// # Concurrent reuse (D-025)
//
// *Registry is a compiled reusable artifact: one shared instance is
// safe under N concurrent registrations / lookups / control commands.
// Mutable state is guarded; per-call state lives in ctx, never on the
// registry.
package registry

import (
	"context"
	"errors"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// Hosting discriminates the two creation cases (D-060).
type Hosting string

const (
	// HostingLocal — the agent is hosted by this runtime instance; the
	// agent_id was minted here and this instance is authoritative.
	HostingLocal Hosting = "local"
	// HostingRemote — the agent runs elsewhere (another Harbor instance
	// or any A2A-speaking peer). The local agent_id is a *handle*; the
	// canonical identity is the remote A2A AgentCard referenced by
	// AgentRecord.AgentCardRef.
	HostingRemote Hosting = "remote"
)

// Health is the operational-health enum the registry tracks per agent.
// It is push-reported via ReportHealth and mutated by fleet-control
// commands (Drain → HealthDraining, ForceStop → HealthStopped); the
// registry runs no health-polling goroutine in V1.
type Health string

const (
	// HealthUnknown — the initial state at registration; no health has
	// been reported yet.
	HealthUnknown Health = "unknown"
	// HealthHealthy — the agent reported itself operational.
	HealthHealthy Health = "healthy"
	// HealthDegraded — the agent reported partial / impaired operation.
	HealthDegraded Health = "degraded"
	// HealthDraining — a Drain control command is in effect; the agent
	// is finishing in-flight work and accepting no new work.
	HealthDraining Health = "draining"
	// HealthStopped — a ForceStop control command was issued, or the
	// agent reported itself stopped.
	HealthStopped Health = "stopped"
)

// IsValidHealth reports whether h is one of the canonical Health values.
func IsValidHealth(h Health) bool {
	switch h {
	case HealthUnknown, HealthHealthy, HealthDegraded, HealthDraining, HealthStopped:
		return true
	default:
		return false
	}
}

// ToolDescriptor identifies one tool binding for version_hash purposes.
// Name is the tool's catalog name; SchemaDigest is a caller-supplied
// digest of the tool's argument/result schema. The registry hashes
// these into version_hash — it does not interpret or validate them.
type ToolDescriptor struct {
	Name         string
	SchemaDigest string
}

// AgentConfig is the configuration content version_hash is derived
// from (RFC §6.16). It is caller-supplied at Register time and hashed
// (after canonicalisation) — the registry never stores it as a
// user-facing persona object; only the resulting VersionHash is
// persisted on the AgentRecord. version_hash bumps iff this content
// changes (D-068: SHA-256 over canonical JSON).
type AgentConfig struct {
	PlannerConfig map[string]string
	ModelPolicy   map[string]string
	Prompts       []string
	Tools         []ToolDescriptor
}

// RegisterOptions carries the non-identity, non-config metadata for a
// registration. All fields are optional; the zero value is valid.
type RegisterOptions struct {
	// DisplayName is an operator-facing label. Not load-bearing for
	// identity; purely cosmetic for the Console Agents page.
	DisplayName string
}

// AgentRecord is the persisted registration-identity record for one
// agent. It is the unit the registry stores (through the StateStore)
// and the shape the Console Agents page renders (through the Protocol,
// later phases).
//
// AgentRecord is a value type; the registry returns copies so callers
// cannot mutate registry-owned state.
type AgentRecord struct {
	RegisteredAt    time.Time
	UpdatedAt       time.Time
	Identity        identity.Identity
	AgentID         string
	VersionHash     string
	RegistrationKey string
	Hosting         Hosting
	AgentCardRef    string
	DisplayName     string
	Health          Health
	Incarnation     uint64
}

// AgentSnapshot is the read-side projection returned by Inspect. In V1
// it carries the AgentRecord plus a Local convenience boolean derived
// from Hosting; later phases may extend it with derived operational
// fields (active task count, last-seen, etc.) sourced from other
// subsystems.
type AgentSnapshot struct {
	AgentRecord
	// Local is true iff Hosting == HostingLocal. Convenience for the
	// Console lens so it does not string-compare Hosting.
	Local bool
}

// AgentRegistry is the public surface every consumer talks to — the
// Protocol surface (Phase 54+), the Console Agents page lens (Phase
// 72–75), and Phase 30's agent-bound OAuth (which keys tokens by the
// registration AgentID). One concrete impl ships in Phase 53a
// (*Registry); there is no driver pluralism at the registry layer —
// driver pluralism lives at the StateStore layer (D-027).
//
// Identity is mandatory on every method: a context whose identity
// triple is missing or incomplete is rejected with a wrapped
// ErrIdentityRequired before any storage is touched (fail-closed).
type AgentRegistry interface {
	// Register mints (or rehydrates) a locally-hosted agent's
	// registration identity.
	//
	//   - First registration of `key`: mints a fresh agent_id (ULID),
	//     incarnation = 1, version_hash = hash(cfg), emits
	//     agent.registered.
	//   - Re-registration of a known `key` (restart): keeps the
	//     agent_id, bumps incarnation, recomputes version_hash (stable
	//     iff cfg content unchanged), emits agent.restarted.
	//
	// Returns a copy of the resulting AgentRecord.
	Register(ctx context.Context, key string, cfg AgentConfig, opts RegisterOptions) (*AgentRecord, error)

	// RegisterRemote registers a connect-to-remote agent (D-060). The
	// local agent_id is a *handle*; cardRef references the canonical
	// A2A AgentCard owned by the remote operator. Semantics mirror
	// Register (first vs re-registration of `key`), but no version_hash
	// is computed — the configuration is owned remotely.
	RegisterRemote(ctx context.Context, key string, cardRef string, opts RegisterOptions) (*AgentRecord, error)

	// Get returns the AgentRecord for agentID, scoped to the ctx
	// identity. Returns a wrapped ErrAgentNotFound when no record
	// exists for that (identity, agentID). Fleet observation — ordinary
	// identity scope.
	Get(ctx context.Context, agentID string) (*AgentRecord, error)

	// List returns every AgentRecord registered under the ctx
	// identity, in agent_id order. One identity's view never includes
	// another identity's agents. Fleet observation — ordinary identity
	// scope.
	List(ctx context.Context) ([]AgentRecord, error)

	// Inspect returns the read-side AgentSnapshot for agentID. Fleet
	// observation — ordinary identity scope.
	Inspect(ctx context.Context, agentID string) (*AgentSnapshot, error)

	// ReportHealth updates the agent's Health and emits agent.health.
	// Fleet observation tier — health is reported BY the agent, not a
	// privileged control command.
	ReportHealth(ctx context.Context, agentID string, h Health) error

	// Deregister removes the agent's record and emits
	// agent.deregistered. A subsequent Register of the same
	// RegistrationKey mints a FRESH agent_id (recreate != restart).
	Deregister(ctx context.Context, agentID string) error

	// Pause requests the agent pause. FLEET CONTROL — requires the
	// elevated control-scope claim (ErrControlScopeRequired otherwise);
	// audit-redacted and emitted.
	Pause(ctx context.Context, agentID string, reason string) error

	// Drain requests the agent drain in-flight work and accept no new
	// work; sets Health = HealthDraining. FLEET CONTROL — requires the
	// elevated control-scope claim; audit-redacted and emitted as
	// agent.drained.
	Drain(ctx context.Context, agentID string, reason string) error

	// Restart requests the agent restart. FLEET CONTROL — requires the
	// elevated control-scope claim; audit-redacted and emitted.
	Restart(ctx context.Context, agentID string, reason string) error

	// ForceStop force-stops the agent; sets Health = HealthStopped.
	// FLEET CONTROL — requires the elevated control-scope claim;
	// audit-redacted and emitted.
	ForceStop(ctx context.Context, agentID string, reason string) error

	// Close releases the registry. Idempotent. Subsequent operations
	// return ErrRegistryClosed. The V1 registry owns no long-lived
	// goroutine; Close exists for interface symmetry and future-
	// proofing.
	Close(ctx context.Context) error
}

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrIdentityRequired — a method was called with a context whose
	// identity triple is missing or incomplete. Identity is mandatory;
	// the registry fails closed (AGENTS.md §6 rule 9). There is no
	// opt-out knob.
	ErrIdentityRequired = errors.New("registry: identity triple incomplete")
	// ErrControlScopeRequired — a fleet-control command (Pause / Drain
	// / Restart / ForceStop) was called without the elevated
	// control-scope claim. Fleet control is a distinct, more-elevated
	// privilege tier than fleet observation (D-066).
	ErrControlScopeRequired = errors.New("registry: fleet-control requires elevated control-scope claim")
	// ErrAgentNotFound — Get / Inspect / ReportHealth / Deregister /
	// any control command targeting an agent_id with no record under
	// the ctx identity.
	ErrAgentNotFound = errors.New("registry: agent not found")
	// ErrAgentExists — reserved for a future explicit-id registration
	// path; not returned by the V1 Register/RegisterRemote flow (which
	// rehydrates on a known key rather than erroring).
	ErrAgentExists = errors.New("registry: registration key already active")
	// ErrRegistryClosed — any operation called after Close.
	ErrRegistryClosed = errors.New("registry: registry is closed")
	// ErrInvalidConfig — Register / RegisterRemote called with a
	// structurally invalid argument (empty registration key, empty
	// cardRef for a remote agent, invalid Health).
	ErrInvalidConfig = errors.New("registry: invalid agent config")
)

// ctxKey is the unexported key type for the control-scope claim. It is
// independent from identity / audit / events / state ctx keys.
type ctxKey int

const controlScopeKey ctxKey = iota

// WithControlScope attaches the elevated fleet-control-scope claim to
// ctx. Fleet-control commands (Pause / Drain / Restart / ForceStop)
// require this claim; fleet-observation methods do not.
//
// The claim is trust-based in V1 — it is set by the Protocol auth
// layer once it has verified an operator's elevated scope claim
// (Phase 61). Until then, any caller that sets it is trusted; the
// audit emit on every control command makes abuse retroactively
// detectable, mirroring the events package's Admin-claim model.
func WithControlScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, controlScopeKey, true)
}

// HasControlScope reports whether ctx carries the elevated
// fleet-control-scope claim.
func HasControlScope(ctx context.Context) bool {
	v, ok := ctx.Value(controlScopeKey).(bool)
	return ok && v
}
