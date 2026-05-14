package registry

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// StateStore Kind constants. The registry persists through the generic
// StateStore seam (D-027): a per-identity *index* document plus one
// *record* document per agent.
//
//   - indexKind        — one document per (tenant,user,session); maps
//     registration_key <-> agent_id so restart can rehydrate and
//     restart != recreate can be enforced. Also the enumeration source
//     for List (the StateStore surface has no scan operation; the
//     typed-wrapper layer owns enumeration — same shape as
//     sessions.Registry).
//   - recordKindPrefix — one document per agent, at
//     "agent.record." + agent_id.
const (
	indexKind        = "agent.index"
	recordKindPrefix = "agent.record."
)

// Deps bundles the collaborators the Registry needs. All three are
// mandatory — the registry fails loudly at construction if any is nil
// (AGENTS.md §5 "fail loudly"; identity/audit are mandatory).
type Deps struct {
	// Store is the per-runtime-instance StateStore. Driver pluralism
	// (in-mem / SQLite / Postgres) lives here, not at the registry
	// layer (D-027 / D-060).
	Store state.StateStore
	// Bus is the typed event bus the registry emits agent.* events on.
	Bus events.EventBus
	// Redactor redacts the operator-supplied Reason string on every
	// fleet-control command before it is emitted (D-020 / D-066).
	Redactor audit.Redactor
}

// Registry is the StateStore-backed implementation of AgentRegistry.
// It is the single concrete impl in V1 — driver pluralism lives at the
// StateStore layer (D-027).
//
// Concurrency model (D-025):
//   - The StateStore is itself concurrent-safe per D-027.
//   - mu serialises the read-modify-write of a per-identity index
//     document (Register / RegisterRemote / Deregister) so two
//     concurrent registrations under the same identity cannot lose an
//     index entry. It is a single registry-wide mutex; per-identity
//     striping is a future optimisation if contention is ever
//     measured (AGENTS.md §5 — "RWMutex only when contention is
//     measured").
//   - closed is an atomic flag; the registry owns no long-lived
//     goroutine, so Close just flips it.
//   - No mutable per-run state lives on the struct: identity comes
//     from ctx on every call, the index/record live in the StateStore.
type Registry struct {
	store    state.StateStore
	bus      events.EventBus
	redactor audit.Redactor
	clock    Clock

	// mu guards every read-modify-write against the StateStore — both
	// the per-identity index document AND a per-agent record document.
	// The documents themselves live in the StateStore (internally
	// synchronised), but a load→mutate→save sequence is not atomic at
	// the store layer, so mu serialises it. Every method that does a
	// load→mutate→save (register, Deregister, ReportHealth, control)
	// MUST hold mu across the whole sequence; a load under mu followed
	// by a save outside it is the lost-update bug the Wave 9 §17.5
	// audit caught in ReportHealth/control. Read-only paths (Get,
	// Inspect) and List's record fan-out do not need mu — a stale read
	// is acceptable; a lost write is not.
	mu sync.Mutex

	closed atomic.Bool
}

// Clock abstracts time so tests are deterministic without time.Sleep
// (AGENTS.md §11). Production code uses realClock.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Option configures a Registry at construction.
type Option func(*Registry)

// WithClock injects a custom Clock. Production code uses realClock;
// the test suite uses a controllable clock.
func WithClock(c Clock) Option {
	return func(r *Registry) { r.clock = c }
}

// New constructs a Registry. Store, Bus, and Redactor are all required.
//
// There is no eager rehydration step: the StateStore is the source of
// truth and is read on every operation. "Rehydration on restart" is
// therefore automatic — a fresh *Registry over a durable StateStore
// (SQLite / Postgres) sees the prior process's agents; a fresh
// *Registry over a fresh in-mem store does not (the in-mem driver is
// dev-only and non-persistent — D-060).
func New(deps Deps, opts ...Option) (*Registry, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf("registry: New requires a non-nil StateStore")
	}
	if deps.Bus == nil {
		return nil, fmt.Errorf("registry: New requires a non-nil EventBus")
	}
	if deps.Redactor == nil {
		return nil, fmt.Errorf("registry: New requires a non-nil Redactor")
	}
	r := &Registry{
		store:    deps.Store,
		bus:      deps.Bus,
		redactor: deps.Redactor,
		clock:    realClock{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// agentIndex is the per-identity index document. It maps registration
// keys to agent ids (and back) so restart rehydrates and
// restart != recreate is enforceable, and it is the enumeration source
// for List.
type agentIndex struct {
	// ByKey maps RegistrationKey -> AgentID for active agents.
	ByKey map[string]string `json:"by_key"`
}

// Register mints (or rehydrates) a locally-hosted agent's registration
// identity. See AgentRegistry.Register.
func (r *Registry) Register(ctx context.Context, key string, cfg AgentConfig, opts RegisterOptions) (*AgentRecord, error) {
	vh, err := VersionHash(cfg)
	if err != nil {
		return nil, fmt.Errorf("registry: Register: %w", err)
	}
	return r.register(ctx, key, HostingLocal, "", vh, opts)
}

// RegisterRemote registers a connect-to-remote agent. The local
// agent_id is a handle; cardRef references the canonical A2A AgentCard.
// See AgentRegistry.RegisterRemote.
func (r *Registry) RegisterRemote(ctx context.Context, key string, cardRef string, opts RegisterOptions) (*AgentRecord, error) {
	if cardRef == "" {
		return nil, fmt.Errorf("%w: RegisterRemote requires a non-empty AgentCard reference", ErrInvalidConfig)
	}
	// version_hash is empty for remote agents — the configuration is
	// owned by the remote operator (D-060).
	return r.register(ctx, key, HostingRemote, cardRef, "", opts)
}

// register is the shared first-vs-re-registration path.
func (r *Registry) register(
	ctx context.Context,
	key string,
	hosting Hosting,
	cardRef string,
	versionHash string,
	opts RegisterOptions,
) (*AgentRecord, error) {
	if r.closed.Load() {
		return nil, ErrRegistryClosed
	}
	ident, err := r.requireIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, fmt.Errorf("%w: registration key must be non-empty", ErrInvalidConfig)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	idx, err := r.loadIndex(ctx, ident)
	if err != nil {
		return nil, err
	}

	now := r.clock.Now()

	if existingID, ok := idx.ByKey[key]; ok {
		// Re-registration of a known logical agent: restart, not
		// recreate. Keep the agent_id and the StateStore record, bump
		// incarnation, recompute version_hash.
		rec, lerr := r.loadRecord(ctx, ident, existingID)
		if lerr != nil {
			return nil, fmt.Errorf("registry: Register rehydrate %q: %w", existingID, lerr)
		}
		prevHash := rec.VersionHash
		rec.Incarnation++
		rec.VersionHash = versionHash
		rec.Hosting = hosting
		rec.AgentCardRef = cardRef
		if opts.DisplayName != "" {
			rec.DisplayName = opts.DisplayName
		}
		rec.UpdatedAt = now
		if serr := r.saveRecord(ctx, ident, rec); serr != nil {
			return nil, serr
		}
		r.publish(ctx, ident, events.Event{
			Type:     EventTypeAgentRestarted,
			Identity: identity.Quadruple{Identity: ident},
			Payload: AgentRestartedPayload{
				AgentID:            rec.AgentID,
				RegistrationKey:    rec.RegistrationKey,
				Incarnation:        rec.Incarnation,
				VersionHash:        rec.VersionHash,
				VersionHashChanged: prevHash != rec.VersionHash,
				RestartedAt:        now.UnixNano(),
			},
		})
		cp := *rec
		return &cp, nil
	}

	// First registration of this logical agent: mint a fresh agent_id.
	agentID := newAgentID()
	rec := AgentRecord{
		AgentID:         agentID,
		Incarnation:     1,
		VersionHash:     versionHash,
		RegistrationKey: key,
		Identity:        ident,
		Hosting:         hosting,
		AgentCardRef:    cardRef,
		DisplayName:     opts.DisplayName,
		Health:          HealthUnknown,
		RegisteredAt:    now,
		UpdatedAt:       now,
	}
	if err := r.saveRecord(ctx, ident, &rec); err != nil {
		return nil, err
	}
	idx.ByKey[key] = agentID
	if err := r.saveIndex(ctx, ident, idx); err != nil {
		return nil, err
	}
	r.publish(ctx, ident, events.Event{
		Type:     EventTypeAgentRegistered,
		Identity: identity.Quadruple{Identity: ident},
		Payload: AgentRegisteredPayload{
			AgentID:         rec.AgentID,
			RegistrationKey: rec.RegistrationKey,
			Incarnation:     rec.Incarnation,
			VersionHash:     rec.VersionHash,
			Hosting:         string(rec.Hosting),
			RegisteredAt:    now.UnixNano(),
		},
	})
	cp := rec
	return &cp, nil
}

// Get returns the AgentRecord for agentID scoped to the ctx identity.
func (r *Registry) Get(ctx context.Context, agentID string) (*AgentRecord, error) {
	if r.closed.Load() {
		return nil, ErrRegistryClosed
	}
	ident, err := r.requireIdentity(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := r.loadRecord(ctx, ident, agentID)
	if err != nil {
		return nil, err
	}
	cp := *rec
	return &cp, nil
}

// List returns every AgentRecord registered under the ctx identity, in
// agent_id order. One identity's view never includes another's.
func (r *Registry) List(ctx context.Context) ([]AgentRecord, error) {
	if r.closed.Load() {
		return nil, ErrRegistryClosed
	}
	ident, err := r.requireIdentity(ctx)
	if err != nil {
		return nil, err
	}
	// Snapshot the index under mu so a concurrent Register/Deregister
	// cannot tear the read; then load records outside the lock.
	r.mu.Lock()
	idx, err := r.loadIndex(ctx, ident)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	ids := make([]string, 0, len(idx.ByKey))
	for _, id := range idx.ByKey {
		ids = append(ids, id)
	}
	r.mu.Unlock()

	sort.Strings(ids)
	out := make([]AgentRecord, 0, len(ids))
	for _, id := range ids {
		rec, lerr := r.loadRecord(ctx, ident, id)
		if lerr != nil {
			if errors.Is(lerr, ErrAgentNotFound) {
				// Index references a record that is gone — skip it
				// rather than fail the whole List. This is defensive;
				// Deregister keeps the two in sync under mu.
				continue
			}
			return nil, lerr
		}
		out = append(out, *rec)
	}
	return out, nil
}

// Inspect returns the read-side AgentSnapshot for agentID.
func (r *Registry) Inspect(ctx context.Context, agentID string) (*AgentSnapshot, error) {
	rec, err := r.Get(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return &AgentSnapshot{
		AgentRecord: *rec,
		Local:       rec.Hosting == HostingLocal,
	}, nil
}

// ReportHealth updates the agent's Health and emits agent.health.
// Fleet-observation tier — health is reported BY the agent.
func (r *Registry) ReportHealth(ctx context.Context, agentID string, h Health) error {
	if r.closed.Load() {
		return ErrRegistryClosed
	}
	if !IsValidHealth(h) {
		return fmt.Errorf("%w: unknown health value %q", ErrInvalidConfig, h)
	}
	ident, err := r.requireIdentity(ctx)
	if err != nil {
		return err
	}

	// The record load→mutate→save is a read-modify-write on a single
	// agent's record document; it MUST run under r.mu so concurrent
	// mutations of the same agent_id (e.g. ReportHealth racing a
	// fleet-control command, or either racing a re-register) cannot
	// lose an update. r.mu is the registry-wide lock — the same one
	// Deregister and register hold across their RMW. Per-agent locking
	// would be finer-grained but registry ops are not hot; correctness
	// over contention here.
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, err := r.loadRecord(ctx, ident, agentID)
	if err != nil {
		return err
	}
	now := r.clock.Now()
	rec.Health = h
	rec.UpdatedAt = now
	if err := r.saveRecord(ctx, ident, rec); err != nil {
		return err
	}
	r.publish(ctx, ident, events.Event{
		Type:     EventTypeAgentHealth,
		Identity: identity.Quadruple{Identity: ident},
		Payload: AgentHealthPayload{
			AgentID:    rec.AgentID,
			Health:     string(h),
			ReportedAt: now.UnixNano(),
		},
	})
	return nil
}

// Deregister removes the agent's record and emits agent.deregistered.
// A subsequent Register of the same RegistrationKey mints a FRESH
// agent_id (recreate != restart).
func (r *Registry) Deregister(ctx context.Context, agentID string) error {
	if r.closed.Load() {
		return ErrRegistryClosed
	}
	ident, err := r.requireIdentity(ctx)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	rec, err := r.loadRecord(ctx, ident, agentID)
	if err != nil {
		return err
	}
	idx, err := r.loadIndex(ctx, ident)
	if err != nil {
		return err
	}
	delete(idx.ByKey, rec.RegistrationKey)
	if err := r.saveIndex(ctx, ident, idx); err != nil {
		return err
	}
	if err := r.store.Delete(ctx, identity.Quadruple{Identity: ident}, recordKind(agentID)); err != nil {
		return fmt.Errorf("registry: Deregister delete record: %w", err)
	}
	now := r.clock.Now()
	r.publish(ctx, ident, events.Event{
		Type:     EventTypeAgentDeregistered,
		Identity: identity.Quadruple{Identity: ident},
		Payload: AgentDeregisteredPayload{
			AgentID:         rec.AgentID,
			RegistrationKey: rec.RegistrationKey,
			DeregisteredAt:  now.UnixNano(),
		},
	})
	return nil
}

// Pause requests the agent pause. FLEET CONTROL.
func (r *Registry) Pause(ctx context.Context, agentID string, reason string) error {
	return r.control(ctx, agentID, "pause", reason, EventTypeAgentPaused, "")
}

// Drain requests the agent drain; sets Health = HealthDraining. FLEET CONTROL.
func (r *Registry) Drain(ctx context.Context, agentID string, reason string) error {
	return r.control(ctx, agentID, "drain", reason, EventTypeAgentDrained, HealthDraining)
}

// Restart requests the agent restart. FLEET CONTROL.
func (r *Registry) Restart(ctx context.Context, agentID string, reason string) error {
	return r.control(ctx, agentID, "restart", reason, EventTypeAgentRestartRequested, "")
}

// ForceStop force-stops the agent; sets Health = HealthStopped. FLEET CONTROL.
func (r *Registry) ForceStop(ctx context.Context, agentID string, reason string) error {
	return r.control(ctx, agentID, "force_stop", reason, EventTypeAgentForceStopped, HealthStopped)
}

// control is the shared fleet-control path. It enforces the elevated
// control-scope claim (D-066), redacts the operator-supplied reason
// (D-020), optionally transitions Health, and emits the control event.
func (r *Registry) control(
	ctx context.Context,
	agentID string,
	command string,
	reason string,
	evType events.EventType,
	newHealth Health,
) error {
	if r.closed.Load() {
		return ErrRegistryClosed
	}
	ident, err := r.requireIdentity(ctx)
	if err != nil {
		return err
	}
	// Fleet control is a distinct, more-elevated privilege tier than
	// fleet observation (D-066). Fail closed without the claim.
	if !HasControlScope(ctx) {
		return fmt.Errorf("%w: command=%q", ErrControlScopeRequired, command)
	}
	// Redact the operator-supplied reason BEFORE it reaches the bus
	// payload (D-020 — no caller-controlled string bypasses the
	// redactor). Done before the lock — it needs only `reason`, not
	// the record.
	redacted, err := r.redactString(ctx, reason)
	if err != nil {
		return fmt.Errorf("registry: %s: redact reason: %w", command, err)
	}

	// The record load→mutate→save is a read-modify-write on a single
	// agent's record document; it MUST run under r.mu so concurrent
	// mutations of the same agent_id (control racing ReportHealth, two
	// control commands racing, or either racing a re-register) cannot
	// lose an update. Same registry-wide lock Deregister/register hold.
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, err := r.loadRecord(ctx, ident, agentID)
	if err != nil {
		return err
	}
	now := r.clock.Now()
	if newHealth != "" {
		rec.Health = newHealth
		rec.UpdatedAt = now
		if serr := r.saveRecord(ctx, ident, rec); serr != nil {
			return serr
		}
	}
	r.publish(ctx, ident, events.Event{
		Type:     evType,
		Identity: identity.Quadruple{Identity: ident},
		Payload: AgentControlPayload{
			AgentID:  rec.AgentID,
			Command:  command,
			Reason:   redacted,
			IssuedAt: now.UnixNano(),
		},
	})
	return nil
}

// Close releases the registry. Idempotent. The V1 registry owns no
// long-lived goroutine, so Close just flips the closed flag.
func (r *Registry) Close(_ context.Context) error {
	r.closed.Store(true)
	return nil
}

// ---------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------

// requireIdentity reads the (tenant,user,session) triple from ctx and
// validates it. Identity is mandatory — the registry fails closed
// (AGENTS.md §6 rule 9). agent_id is NOT part of this check: it is a
// registration identity, not an isolation principal (D-059).
func (r *Registry) requireIdentity(ctx context.Context) (identity.Identity, error) {
	ident, ok := identity.From(ctx)
	if !ok {
		return identity.Identity{}, fmt.Errorf("%w: no identity in context", ErrIdentityRequired)
	}
	if err := identity.Validate(ident); err != nil {
		return identity.Identity{}, fmt.Errorf("%w: %v", ErrIdentityRequired, err)
	}
	return ident, nil
}

// loadIndex loads the per-identity index document. A missing document
// is NOT an error — it means "no agents registered under this identity
// yet"; an empty index is returned.
func (r *Registry) loadIndex(ctx context.Context, ident identity.Identity) (*agentIndex, error) {
	rec, err := r.store.Load(ctx, identity.Quadruple{Identity: ident}, indexKind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return &agentIndex{ByKey: map[string]string{}}, nil
		}
		return nil, fmt.Errorf("registry: load index: %w", err)
	}
	var idx agentIndex
	if uerr := json.Unmarshal(rec.Bytes, &idx); uerr != nil {
		return nil, fmt.Errorf("registry: unmarshal index: %w", uerr)
	}
	if idx.ByKey == nil {
		idx.ByKey = map[string]string{}
	}
	return &idx, nil
}

// saveIndex persists the per-identity index document.
func (r *Registry) saveIndex(ctx context.Context, ident identity.Identity, idx *agentIndex) error {
	b, err := json.Marshal(idx)
	if err != nil {
		return fmt.Errorf("registry: marshal index: %w", err)
	}
	rec := state.StateRecord{
		ID:        state.NewEventID(),
		Identity:  identity.Quadruple{Identity: ident},
		Kind:      indexKind,
		Bytes:     b,
		UpdatedAt: r.clock.Now(),
	}
	if err := r.store.Save(ctx, rec); err != nil {
		return fmt.Errorf("registry: save index: %w", err)
	}
	return nil
}

// loadRecord loads one agent's record. Returns a wrapped
// ErrAgentNotFound when no record exists for (ident, agentID).
func (r *Registry) loadRecord(ctx context.Context, ident identity.Identity, agentID string) (*AgentRecord, error) {
	if agentID == "" {
		return nil, fmt.Errorf("%w: empty agent_id", ErrAgentNotFound)
	}
	rec, err := r.store.Load(ctx, identity.Quadruple{Identity: ident}, recordKind(agentID))
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return nil, fmt.Errorf("%w: tenant=%q user=%q session=%q agent_id=%q",
				ErrAgentNotFound, ident.TenantID, ident.UserID, ident.SessionID, agentID)
		}
		return nil, fmt.Errorf("registry: load record: %w", err)
	}
	var ar AgentRecord
	if uerr := json.Unmarshal(rec.Bytes, &ar); uerr != nil {
		return nil, fmt.Errorf("registry: unmarshal record: %w", uerr)
	}
	return &ar, nil
}

// saveRecord persists one agent's record.
func (r *Registry) saveRecord(ctx context.Context, ident identity.Identity, ar *AgentRecord) error {
	b, err := json.Marshal(ar)
	if err != nil {
		return fmt.Errorf("registry: marshal record: %w", err)
	}
	rec := state.StateRecord{
		ID:        state.NewEventID(),
		Identity:  identity.Quadruple{Identity: ident},
		Kind:      recordKind(ar.AgentID),
		Bytes:     b,
		UpdatedAt: r.clock.Now(),
	}
	if err := r.store.Save(ctx, rec); err != nil {
		return fmt.Errorf("registry: save record: %w", err)
	}
	return nil
}

// publish best-effort emits the event onto the bus. A publish error is
// swallowed — the registry owns registration-identity correctness (the
// StateStore is the source of truth), not bus delivery. A consumer
// that misses the event can still query the registry. This mirrors
// sessions.Registry.publish.
func (r *Registry) publish(ctx context.Context, _ identity.Identity, ev events.Event) {
	_ = r.bus.Publish(ctx, ev)
}

// redactString runs an operator-supplied string through the configured
// Redactor (same shape as the tasks driver's helper — wrap in a map so
// the redactor's reflective walk has a struct-shaped input). Empty
// strings short-circuit.
func (r *Registry) redactString(ctx context.Context, s string) (string, error) {
	if s == "" {
		return "", nil
	}
	out, err := r.redactor.Redact(ctx, map[string]any{"v": s})
	if err != nil {
		return "", err
	}
	m, ok := out.(map[string]any)
	if !ok {
		return "", fmt.Errorf("registry: redactor returned %T, want map[string]any", out)
	}
	v, ok := m["v"].(string)
	if !ok {
		return fmt.Sprintf("%v", m["v"]), nil
	}
	return v, nil
}

// recordKind builds the StateStore Kind for one agent's record.
func recordKind(agentID string) string {
	return recordKindPrefix + agentID
}

// newAgentID mints a fresh ULID-shaped agent_id using crypto-strong
// entropy. ULID gives a monotonic, lexicographically sortable id that
// is collision-free by construction within this runtime instance
// (D-059 / D-060) — it is never assumed globally unique.
func newAgentID() string {
	return ulid.MustNew(ulid.Now(), rand.Reader).String()
}

// Compile-time assertion: *Registry satisfies AgentRegistry.
var _ AgentRegistry = (*Registry)(nil)
