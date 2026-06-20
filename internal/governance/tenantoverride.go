package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// kindTenantOverrides is the StateStore Kind under which a tenant's
// default LLM-parameter overrides persist. One symbol so every load /
// write path references the same string (matches `kindGovernanceCost`).
const kindTenantOverrides = "governance.tenant_overrides"

// The synthetic identity components a tenant-scoped override record is
// keyed under. `state.Save` validates a full `(tenant, user, session)`
// triple, but the tenant default is a tenant-wide record with no natural
// user / session — so it persists under a reserved, deterministic
// synthetic user + session that no real session can collide with (the
// double-underscore prefix is reserved for runtime-internal scopes). The
// isolation boundary stays the tenant: a record for tenant A is invisible
// to tenant B because the Identity.TenantID differs.
const (
	govOverrideUser    = "__governance__"
	govOverrideSession = "__tenant_overrides__"
)

// tenantOverrideSchema is the current persisted-record schema version.
const tenantOverrideSchema = 1

// TenantOverrideSpec is the admin-set desired-state for a tenant's default
// LLM parameters. Every field is optional (nil = "inherit the config
// default for this dimension"). It is the in-process projection of the
// `governance.set_tenant_overrides` wire shape and the value the run loop
// resolves at run start.
//
//   - Model overrides the default model name (validated against the
//     configured ModelProfiles at set time).
//   - ExtraInstructions is ADDITIVE — appended to the agent's system
//     prompt, never a replacement.
//   - Temperature / MaxTokens / ReasoningEffort override the matching LLM
//     request fields.
type TenantOverrideSpec struct {
	Model             *string
	ExtraInstructions *string
	Temperature       *float64
	MaxTokens         *int
	ReasoningEffort   *string
}

// IsEmpty reports whether the spec sets no dimension at all (a cleared
// desired-state). A cleared record means "the tenant inherits every
// config default."
func (s TenantOverrideSpec) IsEmpty() bool {
	return s.Model == nil && s.ExtraInstructions == nil &&
		s.Temperature == nil && s.MaxTokens == nil && s.ReasoningEffort == nil
}

// tenantOverrideRecord is the JSON-encoded wire shape persisted at
// (syntheticTenantIdentity, Kind="governance.tenant_overrides"). Stable
// across StateStore drivers per the `internal/state` Bytes-opaque
// contract.
type tenantOverrideRecord struct {
	Schema            int       `json:"schema"`
	Model             *string   `json:"model,omitempty"`
	ExtraInstructions *string   `json:"extra_instructions,omitempty"`
	Temperature       *float64  `json:"temperature,omitempty"`
	MaxTokens         *int      `json:"max_tokens,omitempty"`
	ReasoningEffort   *string   `json:"reasoning_effort,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// TenantOverridePolicy realises the RFC §6.15 `ModelOverride` governance
// seam: an admin-set, identity-scoped, StateStore-backed default for a
// tenant's LLM parameters that takes effect on every session's NEXT run
// with no redeploy. It is NOT a `Subsystem` (it does not gate per-call in
// PreCall — a per-call read would change the model mid-run, violating the
// next-turn-only invariant); instead the run loop reads it ONCE at run
// start and pins the snapshot into the run's `RunContext`.
//
// Concurrent reuse: the policy is immutable after construction except for
// its per-tenant cache (a `sync.Map` of `*tenantOverrideEntry`, each
// guarded by its own load mutex) and the `closed` atomic flag. Set / Get
// are safe for N concurrent goroutines; per-call state lives in arguments
// and locals, never on the policy struct.
//
// Freshness scope: the cache is write-through on Set and lazy-loaded once
// per tenant, with no TTL — the same per-process cache model the cost
// accumulator uses. Within ONE runtime process (the writer and the run
// loop share the same instance) a Set is immediately visible to the next
// run, so the "every session's next run" guarantee holds. Across multiple
// runtime replicas over a shared store, freshness holds too: every Get
// re-reads the StateStore (per-read freshness — there is NO permanent
// loaded cache), so a Set on replica A is visible to replica B on B's
// next Get (the run loop's run-start resolution). The per-tenant mutex
// serialises a tenant's read/write; different tenants are independent via
// the sync.Map. The run loop already does StateStore reads at run start,
// so the per-Get Load is in the existing cost envelope.
type TenantOverridePolicy struct {
	state       state.StateStore
	bus         events.EventBus
	clock       Clock
	logger      *slog.Logger
	validModels map[string]struct{}

	cache  sync.Map // map[string]*tenantOverrideEntry, keyed by tenant_id
	closed atomic.Bool
}

// tenantOverrideEntry is the per-tenant slot. It exists to serialise the
// per-tenant StateStore read/write under one mutex; reads are always
// fresh (per-read reload — see the policy godoc on multi-replica
// freshness), so the slot does NOT memoise across Gets. `spec`/`set` hold
// the most-recently-loaded value purely transiently under the lock.
type tenantOverrideEntry struct {
	mu   sync.Mutex
	spec TenantOverrideSpec
	set  bool // whether a record exists for this tenant
}

// NewTenantOverridePolicy constructs the policy. `state` (the persistence
// floor) and `bus` (the audit/observability bus) are mandatory; a nil
// either fails loud with `ErrInvalidConfig`. `validModels` is the set of
// model names with a configured `ModelProfile` — `Set` rejects a model
// outside it with `ErrUnknownModel`. An empty `validModels` means no
// model is acceptable (any model set fails loud) — the binary always
// configures at least the bound default model, so an empty set signals a
// misconfiguration, not a latent default.
func NewTenantOverridePolicy(st state.StateStore, bus events.EventBus, validModels []string, clock Clock) (*TenantOverridePolicy, error) {
	if st == nil {
		return nil, fmt.Errorf("%w: state.StateStore is required", ErrInvalidConfig)
	}
	if bus == nil {
		return nil, fmt.Errorf("%w: events.EventBus is required", ErrInvalidConfig)
	}
	if clock == nil {
		clock = DefaultClock
	}
	vm := make(map[string]struct{}, len(validModels))
	for _, m := range validModels {
		if m != "" {
			vm[m] = struct{}{}
		}
	}
	return &TenantOverridePolicy{
		state:       st,
		bus:         bus,
		clock:       clock,
		logger:      slog.Default(),
		validModels: vm,
	}, nil
}

// govTenantQuad builds the synthetic identity quadruple a tenant-scoped
// override record persists under.
func govTenantQuad(tenant string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID:  tenant,
		UserID:    govOverrideUser,
		SessionID: govOverrideSession,
	}}
}

// Set replaces tenant's default-override record with spec (desired-state
// replace: a nil field in spec clears that dimension; an all-nil spec
// clears the record entirely). It validates the spec, persists it,
// updates the cache, and emits `governance.tenant_overrides_set`.
//
// Authorization (the admin-scope gate) is the Protocol surface's
// responsibility — this method assumes the caller is already authorized
// and focuses on validation + persistence. `tenant` MUST be non-empty
// (fail closed — identity is mandatory).
func (p *TenantOverridePolicy) Set(ctx context.Context, actor identity.Quadruple, tenant string, spec TenantOverrideSpec) error {
	if p.closed.Load() {
		return ErrClosed
	}
	if tenant == "" {
		return fmt.Errorf("%w: tenant is empty", ErrIdentityRequired)
	}
	clean, err := p.validate(spec)
	if err != nil {
		return err
	}
	now := p.clock.Now().UTC()
	rec := tenantOverrideRecord{
		Schema:            tenantOverrideSchema,
		Model:             clean.Model,
		ExtraInstructions: clean.ExtraInstructions,
		Temperature:       clean.Temperature,
		MaxTokens:         clean.MaxTokens,
		ReasoningEffort:   clean.ReasoningEffort,
		UpdatedAt:         now,
	}
	buf, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("governance/tenant-overrides: marshal record: %w", err)
	}
	q := govTenantQuad(tenant)
	if err := p.state.Save(ctx, state.StateRecord{
		ID:        state.NewEventID(),
		Identity:  q,
		Kind:      kindTenantOverrides,
		Bytes:     buf,
		UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("%w: %w", ErrStateUnavailable, err)
	}
	// No cache write-through: reads are per-read fresh (every Get reloads
	// from the StateStore — see reloadLocked + the policy godoc), so the
	// StateStore is the single source of truth on the next Get. Writing the
	// entry here would be dead (the reload overwrites it before any read).
	p.emitSet(ctx, actor, tenant, clean, now)
	return nil
}

// Get returns the tenant's current default-override spec and whether a
// record exists. A tenant with no record returns (zero spec, false, nil)
// — the run loop then falls through to the config defaults. A StateStore
// read failure fails loud with wrapped `ErrStateUnavailable` (no silent
// "no overrides" on read failure — CLAUDE.md §13).
func (p *TenantOverridePolicy) Get(ctx context.Context, tenant string) (TenantOverrideSpec, bool, error) {
	if p.closed.Load() {
		return TenantOverrideSpec{}, false, ErrClosed
	}
	if tenant == "" {
		return TenantOverrideSpec{}, false, fmt.Errorf("%w: tenant is empty", ErrIdentityRequired)
	}
	e := p.entry(tenant)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := p.reloadLocked(ctx, tenant, e); err != nil {
		return TenantOverrideSpec{}, false, err
	}
	return e.spec, e.set, nil
}

// Close marks the policy closed; subsequent Set / Get fail with
// `ErrClosed`. Idempotent.
func (p *TenantOverridePolicy) Close(_ context.Context) error {
	p.closed.Store(true)
	return nil
}

// entry returns the per-tenant cache slot, creating it on first reference.
func (p *TenantOverridePolicy) entry(tenant string) *tenantOverrideEntry {
	if v, ok := p.cache.Load(tenant); ok {
		te, _ := v.(*tenantOverrideEntry) //nolint:errcheck // cache values are always *tenantOverrideEntry by construction
		return te
	}
	fresh := &tenantOverrideEntry{}
	actual, _ := p.cache.LoadOrStore(tenant, fresh)
	te, _ := actual.(*tenantOverrideEntry) //nolint:errcheck // cache values are always *tenantOverrideEntry by construction
	return te
}

// reloadLocked reads the tenant's current record from the StateStore and
// refreshes the entry's transient spec/set. The caller MUST hold e.mu.
// This runs on EVERY Get (per-read freshness — no permanent cache), so a
// cross-replica Set is observed on the next Get. A missing record yields
// set=false (the common "no override" case); a corrupt or forward-schema
// record fails loud (no silent reset).
func (p *TenantOverridePolicy) reloadLocked(ctx context.Context, tenant string, e *tenantOverrideEntry) error {
	rec, err := p.state.Load(ctx, govTenantQuad(tenant), kindTenantOverrides)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			e.spec = TenantOverrideSpec{}
			e.set = false
			return nil
		}
		return fmt.Errorf("%w: %w", ErrStateUnavailable, err)
	}
	if len(rec.Bytes) == 0 {
		e.spec = TenantOverrideSpec{}
		e.set = false
		return nil
	}
	var tr tenantOverrideRecord
	if err := json.Unmarshal(rec.Bytes, &tr); err != nil {
		return fmt.Errorf("%w: unmarshal tenant-override record: %w", ErrStateUnavailable, err)
	}
	if tr.Schema != 0 && tr.Schema != tenantOverrideSchema {
		return fmt.Errorf("%w: tenant-override record schema=%d, runtime supports %d", ErrStateUnavailable, tr.Schema, tenantOverrideSchema)
	}
	e.spec = TenantOverrideSpec{
		Model:             tr.Model,
		ExtraInstructions: tr.ExtraInstructions,
		Temperature:       tr.Temperature,
		MaxTokens:         tr.MaxTokens,
		ReasoningEffort:   tr.ReasoningEffort,
	}
	e.set = !e.spec.IsEmpty()
	return nil
}

// validate normalises a spec into a defensive copy, rejecting
// structurally-invalid values. Each field is copied so the returned spec
// shares no pointer with the caller's.
func (p *TenantOverridePolicy) validate(spec TenantOverrideSpec) (TenantOverrideSpec, error) {
	var out TenantOverrideSpec
	if spec.Model != nil {
		m := *spec.Model
		if m != "" {
			if _, ok := p.validModels[m]; !ok {
				return TenantOverrideSpec{}, fmt.Errorf("%w: %q", ErrUnknownModel, m)
			}
		}
		out.Model = &m
	}
	if spec.ExtraInstructions != nil {
		v := *spec.ExtraInstructions
		out.ExtraInstructions = &v
	}
	if spec.Temperature != nil {
		t := *spec.Temperature
		if t < 0 || t > 2 {
			return TenantOverrideSpec{}, fmt.Errorf("%w: temperature %v outside [0,2]", ErrInvalidOverride, t)
		}
		out.Temperature = &t
	}
	if spec.MaxTokens != nil {
		mt := *spec.MaxTokens
		if mt <= 0 {
			return TenantOverrideSpec{}, fmt.Errorf("%w: max_tokens %d must be positive", ErrInvalidOverride, mt)
		}
		out.MaxTokens = &mt
	}
	if spec.ReasoningEffort != nil {
		re := *spec.ReasoningEffort
		switch re {
		case "low", "medium", "high":
			out.ReasoningEffort = &re
		default:
			return TenantOverrideSpec{}, fmt.Errorf("%w: unknown reasoning_effort %q (want low/medium/high)", ErrInvalidOverride, re)
		}
	}
	return out, nil
}

// emitSet publishes the `governance.tenant_overrides_set` audit event.
// The set outcome is already persisted, so a bus failure does NOT change
// it — but a privileged admin mutation must never go to the bus SILENTLY
// (CLAUDE.md §13 "no silent degradation"): a publish failure is logged
// loudly at Warn so the audit-trail drift is visible. The payload carries
// the model name + per-dimension set flags, never the extra-instructions
// prose.
func (p *TenantOverridePolicy) emitSet(ctx context.Context, actor identity.Quadruple, tenant string, spec TenantOverrideSpec, at time.Time) {
	model := ""
	if spec.Model != nil {
		model = *spec.Model
	}
	if err := p.bus.Publish(ctx, events.Event{
		Type:       EventTypeTenantOverridesSet,
		Identity:   actor,
		OccurredAt: at,
		Payload: TenantOverridesSetPayload{
			Actor:                actor,
			Tenant:               tenant,
			Model:                model,
			SetModel:             spec.Model != nil,
			SetExtraInstructions: spec.ExtraInstructions != nil,
			SetTemperature:       spec.Temperature != nil,
			SetMaxTokens:         spec.MaxTokens != nil,
			SetReasoningEffort:   spec.ReasoningEffort != nil,
			OccurredAt:           at,
		},
	}); err != nil {
		p.logger.WarnContext(ctx, "governance/tenant-overrides: failed to publish tenant_overrides_set audit event",
			slog.String("tenant_id", tenant),
			slog.String("actor_user", actor.UserID),
			slog.String("error", err.Error()))
	}
}
