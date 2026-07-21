package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// setposture.go — the write sibling of posture.go. It graduates the
// identity-tier policy table (per tier: budget-ceiling USD / max-tokens
// cap / rate-limit capacity, plus the default-tier assignment) from
// hot-reloadable boot config to a StateStore-backed record layered over
// the config-declared defaults. A runtime with NO written override
// enforces exactly its config defaults; once written, the StateStore
// record IS the effective policy the posture read projects.
//
// The write is a FULL REPLACE through a shared validator, never a partial
// merge: a submitted table that OMITS or zeroes a ceiling the current
// effective policy enforces is rejected fail-closed BEFORE any StateStore
// mutation (never budget-widening, never a de-enforced tier). Only on a
// valid write is the record persisted and THEN a `governance.posture_set`
// audit event emitted best-effort — an emit failure logs loud but does NOT
// roll back the persisted write (mirrors `tenantoverride.go::emitSet`).

// kindPosturePolicy is the StateStore Kind under which the runtime-level
// identity-tier policy record persists. One symbol so every load / write
// path references the same string (matches `kindTenantOverrides`).
const kindPosturePolicy = "governance.posture_policy"

// The synthetic identity components the runtime-level policy record is
// keyed under. The tier policy is a SINGLE runtime-level record (not
// identity-scoped data — the isolation tuple does not apply; it is one
// policy the runtime enforces for every identity). `state.Save` validates
// a full `(tenant, user, session)` triple, so the record persists under a
// reserved, deterministic synthetic triple that no real session can
// collide with (the double-underscore prefix is reserved for
// runtime-internal scopes).
const (
	govPostureTenant  = "__governance__"
	govPostureUser    = "__governance__"
	govPostureSession = "__posture_policy__"
)

// posturePolicySchema is the current persisted-record schema version.
const posturePolicySchema = 1

// Sentinels callers compare with errors.Is.
var (
	// ErrPolicyWidening — a `set_posture` write OMITS or zeroes an
	// enforced ceiling the current effective policy enforces. Fails closed
	// (never budget-widening, never a de-enforced tier) BEFORE any
	// StateStore mutation.
	ErrPolicyWidening = errors.New("governance: set_posture omits or zeroes an enforced ceiling (fail-closed; never budget-widening)")
	// ErrInvalidPosture — a `set_posture` policy failed structural
	// validation (a negative ceiling / capacity / max-tokens, or an empty
	// default tier when a tier is enforced).
	ErrInvalidPosture = errors.New("governance: set_posture policy failed validation")
)

// SetPostureSpec is the admin-set desired-state for the whole identity-tier
// policy table — the in-process projection of the `governance.set_posture`
// wire shape. It is a FULL REPLACE: the supplied table wholly replaces the
// prior effective policy (subject to the fail-closed widening check).
type SetPostureSpec struct {
	// DefaultTier is the default-tier assignment applied to an identity
	// that resolves to no explicit tier. Required when any tier is
	// enforced (an empty value is rejected in that case).
	DefaultTier string
	// IdentityTiers is the complete tier table keyed by tier name. A tier
	// present in the current effective policy but ABSENT here — or present
	// with a zeroed enforced ceiling — is a fail-closed rejection, not a
	// silent widening.
	IdentityTiers map[string]TierConfig
}

// posturePolicyRecord is the JSON-encoded wire shape persisted at
// (govPostureQuad, Kind="governance.posture_policy"). The payload is opaque
// to the StateStore (stable across drivers per the `internal/state`
// Bytes-opaque contract) and read back only by Go, so `TierConfig` — whose
// `RateLimit.RefillInterval` is a `time.Duration` — round-trips faithfully.
type posturePolicyRecord struct {
	Schema        int                   `json:"schema"`
	DefaultTier   string                `json:"default_tier"`
	IdentityTiers map[string]TierConfig `json:"identity_tiers"`
	UpdatedAt     time.Time             `json:"updated_at"`
}

// SetPosturePolicy owns the StateStore-backed effective identity-tier
// policy record layered over the config-declared defaults. Immutable after
// construction (the StateStore is the mutable seam); safe to share across N
// goroutines.
//
// Concurrent reuse: every field is set once at construction and never
// mutated except the `closed` atomic flag. Set / (the read layering it
// shares with PostureProvider) are safe for N concurrent goroutines; the
// record is a single runtime-level policy, so concurrent Set is
// last-writer-wins linearizable at the StateStore seam — there is no
// per-run mutable state on the struct.
type SetPosturePolicy struct {
	state    state.StateStore
	bus      events.EventBus
	defaults Config
	clock    Clock
	logger   *slog.Logger
	// tierSource is the shared hot-swappable effective policy the enforcement
	// subsystem reads per PreCall. A successful write atomically swaps it so
	// the new ceilings ENFORCE on the next call (not merely surface in the
	// read) — the RFC §6.15 hot-reloadable-tiers path. Production / devstack /
	// assemble always pass a non-nil source (seeded from record-over-config at
	// boot); a nil source is a headless / test posture where the write still
	// persists + surfaces in the read with no in-process enforcement swap.
	tierSource *TierSource
	// enforcementActive reports whether a governance wrapper was composed at
	// boot (a non-empty effective policy). When false, a write persists +
	// swaps the source, but NO enforcer reads that source (the wrapper was not
	// composed), so the new policy does not enforce until the next restart —
	// the write response surfaces this as `enforcement_pending_restart` so the
	// 200 is not a silent "persisted but inert" (CLAUDE.md §13).
	enforcementActive bool
	closed            atomic.Bool
}

// NewSetPosturePolicy constructs the policy. `st` (the persistence floor)
// and `bus` (the audit/observability bus) are mandatory; a nil either
// fails loud with `ErrInvalidConfig`. `defaults` is the config-declared
// governance policy the write layers over — a runtime with no written
// override enforces exactly these defaults. `clock` defaults to
// DefaultClock when nil. `tierSource` is the shared enforcement tier source
// a successful write swaps so the change ENFORCES with no restart; nil is
// valid (the write still persists + surfaces in the read, enforcement seeds
// at the next boot). `enforcementActive` reports whether the boot composed a
// governance wrapper reading that source (a non-empty effective policy);
// when false a write's `enforcement_pending_restart` response flag is true.
func NewSetPosturePolicy(st state.StateStore, bus events.EventBus, defaults Config, clock Clock, tierSource *TierSource, enforcementActive bool) (*SetPosturePolicy, error) {
	if st == nil {
		return nil, fmt.Errorf("%w: state.StateStore is required", ErrInvalidConfig)
	}
	if bus == nil {
		return nil, fmt.Errorf("%w: events.EventBus is required", ErrInvalidConfig)
	}
	if clock == nil {
		clock = DefaultClock
	}
	return &SetPosturePolicy{
		state:             st,
		bus:               bus,
		defaults:          defaults,
		clock:             clock,
		logger:            slog.Default(),
		tierSource:        tierSource,
		enforcementActive: enforcementActive,
	}, nil
}

// EnforcementActive reports whether a governance enforcement wrapper was
// composed at boot (a non-empty effective policy read the tier source). When
// false, a `Set` persists + swaps the source but no enforcer reads it, so the
// policy does not enforce until the next restart — the write response
// surfaces this via `enforcement_pending_restart`.
func (p *SetPosturePolicy) EnforcementActive() bool { return p.enforcementActive }

// NewTierSourceFromState builds the hot-swappable enforcement TierSource
// seeded from the EFFECTIVE policy at boot — the persisted `set_posture`
// record layered over the config-declared defaults (record present ⇒ record
// wins; absent ⇒ config defaults). The enforcement assembly reads through
// the returned source; `SetPosturePolicy` swaps it on a write. A corrupt /
// forward-schema record fails loud (no silent reset).
func NewTierSourceFromState(ctx context.Context, st state.StateStore, defaults Config) (*TierSource, error) {
	dt, tiers, err := loadEffectivePolicy(ctx, st, defaults)
	if err != nil {
		return nil, err
	}
	return NewTierSource(dt, tiers), nil
}

// govPostureQuad builds the synthetic identity quadruple the runtime-level
// policy record persists under.
func govPostureQuad() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID:  govPostureTenant,
		UserID:    govPostureUser,
		SessionID: govPostureSession,
	}}
}

// Set validates the submitted table as a FULL REPLACE through the shared
// validator, rejects a widening / zeroing / empty write fail-closed
// (ErrPolicyWidening / ErrInvalidPosture) BEFORE any StateStore mutation,
// persists the record through the StateStore, and THEN emits
// `governance.posture_set` best-effort. Returns the resolved persisted
// posture (byte-faithful to what the next `governance.posture` read
// returns). `actor` is the admin caller (the audit anchor), server-derived
// from the verified session by the Protocol surface — never the request
// body.
func (p *SetPosturePolicy) Set(ctx context.Context, actor identity.Quadruple, spec SetPostureSpec) (Snapshot, error) {
	if p.closed.Load() {
		return Snapshot{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}

	// Structural validation — reject a negative ceiling / capacity /
	// max-tokens, or an empty default tier when any tier is enforced.
	if err := validatePostureSpec(spec); err != nil {
		return Snapshot{}, err
	}

	// Load the current effective policy — the fail-closed comparison basis
	// (the StateStore record if present, else config defaults). A
	// submitted table that drops enforcement the current effective policy
	// HAS is rejected fail-closed BEFORE any mutation. This is the phase's
	// real fail-loud path.
	beforeDefault, beforeTiers, err := loadEffectivePolicy(ctx, p.state, p.defaults)
	if err != nil {
		return Snapshot{}, err
	}
	if err := checkNoWidening(beforeDefault, beforeTiers, spec.DefaultTier, spec.IdentityTiers); err != nil {
		return Snapshot{}, err
	}

	now := p.clock.Now().UTC()
	rec := posturePolicyRecord{
		Schema:        posturePolicySchema,
		DefaultTier:   spec.DefaultTier,
		IdentityTiers: copyTiers(spec.IdentityTiers),
		UpdatedAt:     now,
	}
	buf, err := json.Marshal(rec)
	if err != nil {
		return Snapshot{}, fmt.Errorf("governance/set-posture: marshal record: %w", err)
	}
	if err := p.state.Save(ctx, state.StateRecord{
		ID:        state.NewEventID(),
		Identity:  govPostureQuad(),
		Kind:      kindPosturePolicy,
		Bytes:     buf,
		UpdatedAt: now,
	}); err != nil {
		return Snapshot{}, fmt.Errorf("%w: %w", ErrStateUnavailable, err)
	}

	// Persisted — now atomically swap the ENFORCED tier policy so the new
	// ceilings take effect on the next PreCall (not merely the read). The
	// swap follows the durable write, so the durable record and the
	// in-memory enforcement source agree. When the boot composed no wrapper
	// (`enforcementActive == false`, the fully-latent default), the swap is
	// INERT — no enforcer reads the source — so the policy does not enforce
	// until the next restart (which seeds a wrapper from the persisted
	// record); the response surfaces this as `enforcement_pending_restart`.
	if p.tierSource != nil {
		p.tierSource.Store(spec.DefaultTier, spec.IdentityTiers)
	}

	// Persisted — now emit the audit event best-effort (mirrors
	// tenantoverride.go::emitSet: an emit/redactor failure logs loud but
	// does NOT roll back the persisted write).
	p.emitSet(ctx, actor, beforeDefault, beforeTiers, spec, now)

	// Resolve the actor's tier under the newly-persisted policy for the
	// echoed snapshot.
	eff := Config{DefaultTier: spec.DefaultTier, IdentityTiers: spec.IdentityTiers, Resolver: p.defaults.Resolver}
	return Snapshot{
		DefaultTier:   spec.DefaultTier,
		ResolvedTier:  eff.resolveTier(actor.Identity),
		IdentityTiers: copyTiers(spec.IdentityTiers),
	}, nil
}

// Close marks the policy closed; subsequent Set fails with `ErrClosed`.
// Idempotent.
func (p *SetPosturePolicy) Close(_ context.Context) error {
	p.closed.Store(true)
	return nil
}

// loadEffectivePolicy returns the effective identity-tier policy — the
// StateStore record (written via set_posture) if present, else the
// config-declared defaults. A nil StateStore (the backward-compatible
// config-only posture) returns the config defaults. A corrupt or
// forward-schema record fails loud with wrapped `ErrStateUnavailable` (no
// silent reset). The returned tier map is always a fresh deep copy — a
// caller mutating it cannot reach the config layer or the persisted record.
func loadEffectivePolicy(ctx context.Context, st state.StateStore, defaults Config) (defaultTier string, tiers map[string]TierConfig, err error) {
	if st == nil {
		return defaults.DefaultTier, copyTiers(defaults.IdentityTiers), nil
	}
	rec, loadErr := st.Load(ctx, govPostureQuad(), kindPosturePolicy)
	if loadErr != nil {
		if errors.Is(loadErr, state.ErrNotFound) {
			return defaults.DefaultTier, copyTiers(defaults.IdentityTiers), nil
		}
		return "", nil, fmt.Errorf("%w: %w", ErrStateUnavailable, loadErr)
	}
	if len(rec.Bytes) == 0 {
		return defaults.DefaultTier, copyTiers(defaults.IdentityTiers), nil
	}
	var pr posturePolicyRecord
	if unmarshalErr := json.Unmarshal(rec.Bytes, &pr); unmarshalErr != nil {
		return "", nil, fmt.Errorf("%w: unmarshal posture-policy record: %w", ErrStateUnavailable, unmarshalErr)
	}
	if pr.Schema != 0 && pr.Schema != posturePolicySchema {
		return "", nil, fmt.Errorf("%w: posture-policy record schema=%d, runtime supports %d", ErrStateUnavailable, pr.Schema, posturePolicySchema)
	}
	return pr.DefaultTier, copyTiers(pr.IdentityTiers), nil
}

// copyTiers returns a deep copy of a tier map. TierConfig is a value type
// whose only nested field (RateLimitConfig) is itself a value type, so a
// plain map copy is a full deep copy. Always non-nil.
func copyTiers(in map[string]TierConfig) map[string]TierConfig {
	out := make(map[string]TierConfig, len(in))
	for name, tc := range in {
		out[name] = tc
	}
	return out
}

// validatePostureSpec rejects structurally-invalid values fail-closed: a
// negative budget ceiling / rate-limit capacity / refill / max-tokens, an
// empty default tier when any tier is enforced, or a `DefaultTier` that
// points at a tier ABSENT from the submitted table (a default resolving to
// no tier is a silent-no-enforcement bug — every default-resolved caller
// would be uncapped while the map still shows enforced-looking tiers).
func validatePostureSpec(spec SetPostureSpec) error {
	anyEnforced := false
	for name, tc := range spec.IdentityTiers {
		if tc.BudgetCeilingUSD < 0 {
			return fmt.Errorf("%w: tier %q budget_ceiling_usd %v is negative", ErrInvalidPosture, name, tc.BudgetCeilingUSD)
		}
		if tc.MaxTokens < 0 {
			return fmt.Errorf("%w: tier %q max_tokens %d is negative", ErrInvalidPosture, name, tc.MaxTokens)
		}
		if tc.RateLimit.Capacity < 0 || tc.RateLimit.RefillTokens < 0 || tc.RateLimit.RefillInterval < 0 {
			return fmt.Errorf("%w: tier %q has a negative rate-limit field", ErrInvalidPosture, name)
		}
		if b, r, m := enforcedDims(tc); b || r || m {
			anyEnforced = true
		}
	}
	if anyEnforced && spec.DefaultTier == "" {
		return fmt.Errorf("%w: default_tier is empty but a tier is enforced", ErrInvalidPosture)
	}
	// A non-empty DefaultTier MUST name a tier present in the submitted
	// table. A default pointing at an absent tier silently resolves to "no
	// enforcement" for every default-resolved caller (the tier lookup misses
	// and PreCall permits) while the posture read still shows the map's
	// enforced tiers — a de-enforcement hidden behind a valid-looking write.
	if spec.DefaultTier != "" {
		if _, ok := spec.IdentityTiers[spec.DefaultTier]; !ok {
			return fmt.Errorf("%w: default_tier %q is absent from the submitted identity_tiers (a default resolving to no tier silently disables enforcement for every default caller)", ErrInvalidPosture, spec.DefaultTier)
		}
	}
	return nil
}

// enforcedDims reports which policy dimensions a tier ENFORCES (a positive
// value). A zero dimension is a latent "no enforcement", not enforcement.
func enforcedDims(tc TierConfig) (budget, rate, maxTokens bool) {
	return tc.BudgetCeilingUSD > 0, tc.RateLimit.Capacity > 0, tc.MaxTokens > 0
}

// checkNoWidening is the fail-closed comparison. The reference point is the
// CURRENT effective policy (`before`): a dimension the current effective
// policy ENFORCES (>0) MUST stay enforced (>0) in the submitted table
// (`after`). An operator MAY lower a ceiling (reading (a) — the current
// effective policy is the basis, not an immovable config floor) but MUST
// NOT drop enforcement of a tier / dimension that is currently enforced —
// omitting a tier, or zeroing an enforced dimension, is a fail-closed
// ErrPolicyWidening. A dimension that was never enforced (zero) stays a
// valid "no enforcement".
//
// The DEFAULT-RESOLVED caller class is treated exactly like a tier: the
// no-Resolver common case resolves every caller to the default tier, so a
// default REPOINT from an enforced tier to an unenforced one (a new/other
// tier with zero ceilings) silently de-enforces the whole default caller
// class even though the old tier stays in the map. `beforeDefault` /
// `afterDefault` are the default-tier names; if the before-default-resolved
// tier was enforced and the after-default-resolved tier is not, that is a
// fail-closed ErrPolicyWidening. (A genuine relaxation sets the new default
// tier's ceilings explicitly, which then satisfies this check.)
func checkNoWidening(beforeDefault string, before map[string]TierConfig, afterDefault string, after map[string]TierConfig) error {
	// Deterministic tier order for a stable error message.
	names := make([]string, 0, len(before))
	for name := range before {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		bTC := before[name]
		bBudget, bRate, bMax := enforcedDims(bTC)
		if !bBudget && !bRate && !bMax {
			continue // the current tier enforces nothing — nothing to preserve.
		}
		aTC, present := after[name]
		if !present {
			return fmt.Errorf("%w: tier %q is currently enforced but absent from the submitted table", ErrPolicyWidening, name)
		}
		aBudget, aRate, aMax := enforcedDims(aTC)
		if bBudget && !aBudget {
			return fmt.Errorf("%w: tier %q drops its currently-enforced budget_ceiling_usd", ErrPolicyWidening, name)
		}
		if bRate && !aRate {
			return fmt.Errorf("%w: tier %q drops its currently-enforced rate-limit capacity", ErrPolicyWidening, name)
		}
		if bMax && !aMax {
			return fmt.Errorf("%w: tier %q drops its currently-enforced max_tokens cap", ErrPolicyWidening, name)
		}
	}
	// The default-resolved caller class: a default repoint must not silently
	// de-enforce ANY dimension it currently enforces. Compare the tier the OLD
	// default resolved to against the tier the NEW default resolves to
	// (no-Resolver semantics — the common case); absent tiers resolve to the
	// zero TierConfig (unenforced). The check is PER-DIMENSION, mirroring the
	// per-tier loop above: a dimension-SWAP (e.g. the old default budget-caps
	// while the new default only enforces max_tokens) drops the budget cap for
	// every default caller and must be rejected, even though the new default
	// tier enforces SOME other dimension.
	if beforeDefault != "" {
		bB, bR, bM := enforcedDims(before[beforeDefault])
		aB, aR, aM := enforcedDims(after[afterDefault])
		if bB && !aB {
			return fmt.Errorf("%w: default_tier repoint from %q to %q drops the default caller class's currently-enforced budget_ceiling_usd", ErrPolicyWidening, beforeDefault, afterDefault)
		}
		if bR && !aR {
			return fmt.Errorf("%w: default_tier repoint from %q to %q drops the default caller class's currently-enforced rate-limit capacity", ErrPolicyWidening, beforeDefault, afterDefault)
		}
		if bM && !aM {
			return fmt.Errorf("%w: default_tier repoint from %q to %q drops the default caller class's currently-enforced max_tokens cap", ErrPolicyWidening, beforeDefault, afterDefault)
		}
	}
	return nil
}

// emitSet publishes the `governance.posture_set` audit event. The write is
// already persisted, so a bus failure does NOT change it — but a privileged
// admin mutation must never go to the bus SILENTLY (CLAUDE.md §13 "no
// silent degradation"): a publish failure is logged loudly at Warn so the
// audit-trail drift is visible. The payload carries only the non-secret
// tier-summary (sorted tier names + counts + default-tier before/after) —
// no ceiling values, no secrets — and runs through the wired bus's audit
// Redactor before delivery (CLAUDE.md §7 rule 6).
func (p *SetPosturePolicy) emitSet(ctx context.Context, actor identity.Quadruple, beforeDefault string, beforeTiers map[string]TierConfig, spec SetPostureSpec, at time.Time) {
	if err := p.bus.Publish(ctx, events.Event{
		Type:       EventTypePostureSet,
		Identity:   actor,
		OccurredAt: at,
		Payload: GovernancePostureSetPayload{
			Actor:             actor,
			DefaultTierBefore: beforeDefault,
			DefaultTierAfter:  spec.DefaultTier,
			TierCountBefore:   len(beforeTiers),
			TierCountAfter:    len(spec.IdentityTiers),
			TiersBefore:       sortedTierNames(beforeTiers),
			TiersAfter:        sortedTierNames(spec.IdentityTiers),
			OccurredAt:        at,
		},
	}); err != nil {
		p.logger.WarnContext(ctx, "governance/set-posture: failed to publish posture_set audit event",
			slog.String("actor_tenant", actor.TenantID),
			slog.String("actor_user", actor.UserID),
			slog.String("error", err.Error()))
	}
}

// sortedTierNames returns the tier names in deterministic order. Non-secret
// (tier names are operator-visible metadata).
func sortedTierNames(tiers map[string]TierConfig) []string {
	names := make([]string, 0, len(tiers))
	for name := range tiers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
