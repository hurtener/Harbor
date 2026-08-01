// Package protocol implements the `runs.set_overrides` Protocol method
// the Console Playground page consumes.
//
// `runs.set_overrides` records the reasoning-effort / temperature /
// max-tokens / system-prompt / additive-guidance override an operator
// applies to the NEXT message in a session. The override is:
//
//   - Session-scoped — keyed by the full identity triple
//     `(tenant, user, session)`. A second session never sees the
//     first's pending override (CLAUDE.md §6 — multi-isolation).
//   - One-shot — `Consume` removes and returns the pending override.
//     The override applies to exactly the next `user_message` / `start`
//     and is gone afterwards. It does NOT apply retroactively to past
//     messages; a session that records an override then never sends a
//     message simply drops it (documented behaviour — the phase plan's
//     "next-message semantics" risk).
//   - BOUNDED — an unconsumed slot is reclaimed by capacity pressure, not
//     by time. The Store holds at most [DefaultMaxPendingOverrides] slots
//     and evicts the OLDEST-recorded slot to admit a new identity. See
//     [Store] for why the bound exists and why the policy is drop-oldest.
//
// # The seam (CLAUDE.md §4.4)
//
// The Service depends on the in-process `Store` — a mutex-guarded map
// keyed by the identity triple. The Store is the V1 production
// implementation; it is deliberately a single concrete (the override
// slot is ephemeral per-runtime state, not a persistence-shaped
// subsystem with plausible alternate backends — there is no SQLite /
// Postgres override store, so §4.4's interface-plus-driver ceremony
// would be optional-capability smell). When a future durable / remote
// override store becomes a real requirement, it slots in behind a
// promoted interface; today the concrete is correct.
//
// # Identity is mandatory (CLAUDE.md §6 rule 9)
//
// Every method takes the wire request's `IdentityScope`. An incomplete
// triple fails closed with `ErrIdentityRequired` — there is no
// identity-downgrading knob. The Service NEVER reads identity from a
// package-level global; the triple flows in via the request.
//
// # Cross-session gating
//
// `runs.set_overrides` carries both an `IdentityScope` (the verified
// JWT identity) and `RunOverrides.SessionID` (the named target). The
// Service rejects a request whose `SessionID` names a session other
// than the verified `Identity.Session` with `ErrCrossSessionScope` —
// an operator cannot record an override for a session outside its own
// verified scope. Admin impersonation is out of scope for this method
// at V1 (the Playground records overrides for the operator's own
// session); the impersonation triplet on `IdentityScope` is honoured by
// the `user_message` / `start` consumer, not by override recording.
//
// # Concurrent reuse
//
// A constructed *Service is immutable after NewService and safe to
// share across N concurrent goroutines: it holds only the Store
// reference + an optional bus + redactor + logger + clock. Every
// method's per-call state lives in the call's arguments and locals,
// never on the Service. The Store guards its map with a sync.Mutex —
// the only mutable state, and it is documented "internally
// synchronised" per the carve-out.
package protocol

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// ComposeLLMOverrides merges the three per-run LLM-override layers into the
// planner's override bundle in precedence order **session › per-agent ›
// tenant-wide baseline** (the run loop reads it at run start). Per field, a
// non-nil session value wins, then the per-agent (agent-config) value, then
// the tenant-wide baseline fills the rest. The session's SystemPromptOverride
// (full system-prompt REPLACE) is session-only; the per-agent layer carries
// sampling parameters only (model / temperature / max-tokens /
// reasoning-effort) — never prompt text. Returns nil when no layer set
// anything. config defaults are applied last by the planner's
// applyLLMOverrides (an unset field leaves the request untouched).
//
// # ExtraInstructions keeps its producer's authority
//
// The tenant-wide ExtraInstructions value remains trusted additive guidance.
// The session field of the same wire name needs only a verified identity and
// therefore lands in UserPersonalization instead of being concatenated into
// the tenant string. Keeping the values separate lets the prompt builder
// escape and label the lower-authority contribution structurally. A session
// value can neither replace nor clear tenant guidance.
//
// This is the ONE production composition; cmd/harbor's run loop, the devstack
// twin, and the integration test all call it (no re-implemented copy —
// CLAUDE.md §17.4).
func ComposeLLMOverrides(session *PendingOverride, agent, tenant *planner.LLMOverrides) *planner.LLMOverrides {
	if session == nil && agent == nil && tenant == nil {
		return nil
	}
	out := &planner.LLMOverrides{}
	// Base: the tenant-wide baseline (Model/Temperature/MaxTokens/
	// ReasoningEffort/ExtraInstructions).
	if tenant != nil {
		*out = *tenant
		// UserPersonalization has exactly one producer: the authenticated
		// session PendingOverride below. It is an internal carrier, not a
		// tenant-baseline field, even if an in-process caller constructs an
		// over-wide LLMOverrides value.
		out.UserPersonalization = nil
	}
	// Per-agent layer overrides the tenant baseline per field (sampling
	// parameters only — never ExtraInstructions / prompt text).
	if agent != nil {
		if agent.Model != nil {
			out.Model = agent.Model
		}
		if agent.Temperature != nil {
			out.Temperature = agent.Temperature
		}
		if agent.MaxTokens != nil {
			out.MaxTokens = agent.MaxTokens
		}
		if agent.ReasoningEffort != nil {
			out.ReasoningEffort = agent.ReasoningEffort
		}
	}
	// Session override wins over everything, per field.
	if session != nil {
		if session.Model != nil {
			out.Model = session.Model
		}
		if session.Temperature != nil {
			out.Temperature = session.Temperature
		}
		if session.MaxTokens != nil {
			out.MaxTokens = session.MaxTokens
		}
		if session.ReasoningEffort != nil {
			out.ReasoningEffort = session.ReasoningEffort
		}
		if session.SystemPromptOverride != nil {
			out.SystemPromptOverride = session.SystemPromptOverride
		}
		if session.ExtraInstructions != nil && strings.TrimSpace(*session.ExtraInstructions) != "" {
			v := *session.ExtraInstructions
			out.UserPersonalization = &v
		}
	}
	return out
}

// Sentinel errors the Service returns. The wire handler maps each onto
// a canonical Protocol Code + HTTP status; in-process callers compare
// with errors.Is.
var (
	// ErrIdentityRequired — the request carried an incomplete identity
	// triple. RFC §5.5 / CLAUDE.md §6 rule 9 — fails closed.
	ErrIdentityRequired = errors.New("runs/protocol: identity scope incomplete")
	// ErrCrossSessionScope — the request's RunOverrides.SessionID named
	// a session outside the caller's verified Identity.Session.
	ErrCrossSessionScope = errors.New("runs/protocol: override targets a session outside the caller's verified scope")
	// ErrInvalidRequest — the override payload was structurally invalid
	// (an out-of-range temperature, a non-positive max-tokens, an
	// unknown reasoning-effort value).
	ErrInvalidRequest = errors.New("runs/protocol: invalid request")
	// ErrMisconfigured — NewService was called with a nil Store.
	ErrMisconfigured = errors.New("runs/protocol: NewService missing a mandatory dependency")
)

// validReasoningEffort is the closed set of accepted reasoning-effort
// values. The runtime's bound LLM provider taxonomy is the source of
// truth; the three-value scale (low / medium / high) is the V1 common
// denominator across the supported providers. An unknown value fails
// the request closed with ErrInvalidRequest rather than silently
// passing an unrecognised hint to the provider.
var validReasoningEffort = map[string]struct{}{
	"low":    {},
	"medium": {},
	"high":   {},
}

// PendingOverride is the recorded, validated override held in a
// session's one-shot slot. It is the internal projection of a
// types.RunOverrides — the same pointer-optional fields, plus the
// recording instant.
//
// A PendingOverride is immutable once stored; Consume returns it by
// value and removes the slot.
type PendingOverride struct {
	// ReasoningEffort, when non-nil, is the validated reasoning-effort
	// hint for the next message.
	ReasoningEffort *string
	// Temperature, when non-nil, is the validated sampling temperature.
	Temperature *float64
	// MaxTokens, when non-nil, is the validated per-message token ceiling.
	MaxTokens *int
	// SystemPromptOverride, when non-nil, replaces the agent's system
	// prompt for the next message only.
	SystemPromptOverride *string
	// ExtraInstructions, when non-nil, is the one-run user personalization
	// input. ComposeLLMOverrides keeps it separate from tenant guidance so
	// the planner can render it in a lower-authority escaped section. An
	// empty or whitespace-only value contributes nothing and is not an error.
	ExtraInstructions *string
	// Model, when non-nil, is the validated model the next message's run
	// requests (the session-level model swap).
	Model *string
	// RecordedAt is the runtime instant the override entered the slot.
	RecordedAt time.Time
}

// DefaultMaxPendingOverrides is the number of identity triples that may
// hold an unconsumed pending override at once. A slot is a handful of
// pointers, so the default is set for headroom over any plausible count
// of sessions concurrently mid-compose rather than to conserve memory:
// reaching it means slots are being recorded far faster than they are
// consumed, which is the abusive shape, not the operator one.
const DefaultMaxPendingOverrides = 4096

// DefaultMaxPendingOverridesPerTenant is the number of slots ONE tenant
// may hold at once — one sixteenth of the global bound. It is the bound
// that makes the eviction policy's isolation claim true rather than
// merely intended: without it, the global bound alone means the caller
// who fills the map evicts every OTHER tenant's slot, continuously (see
// [Store] "Why a per-tenant sub-bound").
//
// The ratio is what the guarantee is made of, so it is stated rather
// than left to be derived: at 4096 / 256 a single tenant can occupy at
// most one sixteenth of the map, so its churn can never reach the global
// bound and therefore can never displace a sibling tenant. Sixteen
// distinct tenants would have to act together to reach it — and a tenant
// id is a VERIFIED claim, not a caller-chosen string, which is the whole
// asymmetry the sub-bound rests on.
const DefaultMaxPendingOverridesPerTenant = 256

// slotEntry is one identity's pending override plus its position in the
// two recording-order lists — the global one and its tenant's. It is
// heap-allocated once per slot and mutated in place on a re-Set, so a
// re-Set costs no list churn.
type slotEntry struct {
	id identity.Identity
	po PendingOverride
	// globalEl / tenantEl are this entry's elements in Store.order and
	// in Store.byTenant[id.TenantID]. Holding both is what keeps every
	// admit, evict and Consume O(1): a bound whose enforcement is itself
	// a linear scan is a poor answer to an availability defect, and this
	// path's rate is attacker-controlled.
	globalEl *list.Element
	tenantEl *list.Element
}

// Store is the in-process, identity-scoped pending-override slot map.
// It is a compiled artifact: constructed once via NewStore,
// shared across N goroutines, with its mutable fields — the slot map and
// its recording-order list — guarded by an internally-synchronised
// sync.Mutex.
//
// The map is keyed by the identity triple so a session's pending
// override is invisible to every other `(tenant, user, session)` —
// multi-isolation is enforced by the key, not by a post-fetch filter.
//
// # The bound, and its drop policy (CLAUDE.md §5)
//
// A slot is written by `runs.set_overrides` and removed by the Consume
// the next message performs. A session that records an override and then
// never sends a message leaves its slot behind, so an UNBOUNDED map grows
// with the count of such sessions for the lifetime of the process — an
// availability defect any authenticated caller reaches by recording an
// override under a fresh session id in a loop. The Store therefore holds
// at most MaxSlots entries.
//
// The policy is DROP-OLDEST, WITHIN THE ADMITTING TENANT, and it is
// stated here because a silent eviction would itself be the §13
// silent-degradation shape:
//
//   - Evicting rather than REFUSING the write. A capacity refusal denies
//     the surface to the caller that asked for it, and the slot it would
//     have refused is the one that just expressed intent. Eviction spends
//     an ABANDONED slot instead. This is a trade between two costs, not a
//     containment argument — see the next bullet for what actually
//     confines the damage.
//   - A PER-TENANT sub-bound underneath the global one. This is the bullet
//     that carries the isolation property, and it exists because the
//     global bound ALONE does not: with one process-wide order list, a
//     tenant recording overrides under fresh session ids evicts every
//     other tenant's slot, continuously, for as long as it keeps writing.
//     That is the very cross-tenant availability defect the first bullet
//     is often read as ruling out, and eviction does not rule it out —
//     the sub-bound does. A tenant at MaxSlotsPerTenant evicts ITS OWN
//     oldest slot, so its churn is self-inflicted and reaches no sibling.
//   - OLDEST-recorded rather than newest. A slot's whole purpose is to be
//     consumed by the very next message, so the longer one has sat
//     unconsumed the more likely it is already abandoned. Dropping the
//     newest would instead discard the write of the caller who just asked
//     for it, while retaining slots nothing will ever read.
//   - LOUD, once per eviction. Every eviction logs at Warn with the
//     evicted triple, the instant the slot was recorded, and WHICH bound
//     forced it — a tenant evicting itself and a tenant being displaced
//     by the global bound are different operator situations and must not
//     read alike. There is no first-in-window suppression: below the
//     bounds an eviction is impossible, so a line here is never routine,
//     and suppressing the second line would hide the scale of whatever is
//     producing them.
//   - No TTL. The slot already has a lifetime ("until the next message");
//     a second, time-based expiry axis would be a second mechanism
//     answering the same question, needing its own clock and sweeper. The
//     bounds alone close the growth defect.
//
// # Why a per-tenant sub-bound, and what it does NOT claim
//
// The asymmetry that makes the sub-bound work: a SESSION id is a
// caller-chosen string, unbounded and free to mint, which is exactly why
// the growth defect existed. A TENANT id is a verified claim on the
// request identity. So bounding per tenant bounds the axis an attacker
// controls by the axis it does not.
//
// Two residuals are stated rather than engineered away:
//
//  1. Enough DISTINCT tenants acting together still reach the global
//     bound, and the slot evicted there does belong to another tenant.
//     That needs MaxSlots / MaxSlotsPerTenant separately-authenticated
//     tenants (sixteen at the defaults), not one caller with a loop.
//  2. Inside one tenant, a user can still displace a sibling user's slot.
//     The sub-bound is keyed on the tenant because that is the boundary
//     the isolation claim names and the outermost one CLAUDE.md §6 makes
//     integrity-critical; a second sub-bound axis would be a second
//     mechanism answering a question nobody has reported, and the
//     per-tenant bound must remain the outer one in any case or a tenant
//     with many users would exceed its share of the global map.
//
// An evicted slot is indistinguishable to its session from one that was
// never recorded: the next message runs with no override, exactly as the
// documented "recorded, then never sent" path already behaves.
type Store struct {
	mu    sync.Mutex
	slots map[identity.Identity]*slotEntry
	// order holds *slotEntry, front = oldest recorded, back = newest.
	// It carries the GLOBAL bound.
	order *list.List
	// byTenant holds one recording-order list per tenant that currently
	// owns at least one slot; each also holds *slotEntry, front = that
	// tenant's oldest. It carries the PER-TENANT bound. A tenant's list
	// is deleted the moment it empties, so the map does not itself grow
	// without bound over the tenants seen.
	byTenant     map[string]*list.List
	max          int
	maxPerTenant int
	logger       *slog.Logger
}

// StoreOption configures NewStore.
type StoreOption func(*Store)

// WithMaxSlots bounds the number of identity triples that may hold a
// pending override at once. A non-positive value is ignored (the default
// stands) — there is no way to configure the bound AWAY, because an
// unbounded slot map is the defect this bound exists to close.
func WithMaxSlots(n int) StoreOption {
	return func(s *Store) {
		if n > 0 {
			s.max = n
		}
	}
}

// WithMaxSlotsPerTenant bounds the number of slots ONE tenant may hold at
// once. A non-positive value is ignored, for the same reason
// [WithMaxSlots] ignores one: the sub-bound is what confines an evicting
// caller's damage to its own tenant, so there is no way to configure it
// away.
//
// A value above the global bound is CLAMPED to it by NewStore rather than
// honoured, because a per-tenant bound the global bound reaches first can
// never fire and would read as a guarantee that is not there.
func WithMaxSlotsPerTenant(n int) StoreOption {
	return func(s *Store) {
		if n > 0 {
			s.maxPerTenant = n
		}
	}
}

// WithStoreLogger sets the slog.Logger the Store reports evictions on. A
// nil logger routes to slog.Default().
func WithStoreLogger(l *slog.Logger) StoreOption {
	return func(s *Store) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewStore builds an empty override Store bounded at
// [DefaultMaxPendingOverrides] slots globally and
// [DefaultMaxPendingOverridesPerTenant] slots per tenant. The returned
// *Store is safe for concurrent use by N goroutines.
func NewStore(opts ...StoreOption) *Store {
	s := &Store{
		slots:        make(map[identity.Identity]*slotEntry),
		order:        list.New(),
		byTenant:     make(map[string]*list.List),
		max:          DefaultMaxPendingOverrides,
		maxPerTenant: DefaultMaxPendingOverridesPerTenant,
		logger:       slog.Default(),
	}
	for _, o := range opts {
		o(s)
	}
	// A per-tenant bound at or above the global bound can never fire, so
	// it is clamped rather than left to read as a guarantee that is not
	// there. This is also what keeps a small WithMaxSlots (tests, tight
	// embedders) from silently disabling the sub-bound.
	if s.maxPerTenant > s.max {
		s.maxPerTenant = s.max
	}
	return s
}

// Set records po into the slot for id, replacing any prior pending
// override for that identity triple. An operator that records two
// overrides before sending a message keeps only the second — the slot
// is last-write-wins, the documented behaviour.
//
// A re-Set REFRESHES the identity's position in both recording orders, so
// the most recently expressed intent is the last to be evicted.
//
// Admitting a NEW identity consults the per-tenant bound FIRST: a tenant
// already holding MaxSlotsPerTenant slots evicts its OWN oldest, so a
// caller churning session ids can only ever displace itself. Only when
// the admitting tenant is under its own bound and the map is at MaxSlots
// does the global eviction run. See [Store] for the policy, why it is not
// a refusal, and the two residuals it does not claim to close.
func (s *Store) Set(id identity.Identity, po PendingOverride) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ent, ok := s.slots[id]; ok {
		ent.po = po
		s.order.MoveToBack(ent.globalEl)
		if tl := s.byTenant[id.TenantID]; tl != nil {
			tl.MoveToBack(ent.tenantEl)
		}
		return
	}
	// Per-tenant bound first. Checking the global bound first would let a
	// tenant at its own cap displace a SIBLING whenever the map happened
	// to be full — which is the defect the sub-bound exists to close.
	tl := s.byTenant[id.TenantID]
	switch {
	case tl != nil && tl.Len() >= s.maxPerTenant:
		s.evictOldestOfTenantLocked(id.TenantID)
	case len(s.slots) >= s.max:
		s.evictOldestLocked()
	}
	ent := &slotEntry{id: id, po: po}
	ent.globalEl = s.order.PushBack(ent)
	tl = s.byTenant[id.TenantID]
	if tl == nil {
		tl = list.New()
		s.byTenant[id.TenantID] = tl
	}
	ent.tenantEl = tl.PushBack(ent)
	s.slots[id] = ent
}

// evictOldestLocked drops the globally oldest-recorded slot and reports
// it at Warn. The caller holds s.mu. This path runs only when the
// admitting tenant is UNDER its own sub-bound, so the slot it takes may
// belong to another tenant — which is residual 1 in [Store], and the log
// line names the bound so an operator can tell the two situations apart.
func (s *Store) evictOldestLocked() {
	el := s.order.Front()
	if el == nil {
		return
	}
	ent, ok := el.Value.(*slotEntry)
	if !ok {
		// Impossible by construction: order only ever holds *slotEntry.
		// Reported rather than ignored so a future change that broke the
		// invariant does not silently stop evicting.
		s.order.Remove(el)
		s.logger.Error("runs: pending-override order list held a non-entry value")
		return
	}
	s.removeLocked(ent)
	s.logger.Warn("runs: evicted the oldest pending override to admit a new one — the slot map is at its GLOBAL capacity",
		"tenant_id", ent.id.TenantID,
		"user_id", ent.id.UserID,
		"session_id", ent.id.SessionID,
		"recorded_at", ent.po.RecordedAt,
		"bound", "global",
		"max_slots", s.max)
}

// evictOldestOfTenantLocked drops the oldest-recorded slot BELONGING TO
// tenant and reports it at Warn. The caller holds s.mu and has
// established that the tenant is at its sub-bound.
func (s *Store) evictOldestOfTenantLocked(tenant string) {
	tl := s.byTenant[tenant]
	if tl == nil {
		return
	}
	el := tl.Front()
	if el == nil {
		return
	}
	ent, ok := el.Value.(*slotEntry)
	if !ok {
		tl.Remove(el)
		s.logger.Error("runs: pending-override tenant order list held a non-entry value")
		return
	}
	s.removeLocked(ent)
	s.logger.Warn("runs: evicted this tenant's oldest pending override to admit a new one — the tenant is at its PER-TENANT capacity, so no other tenant's slot was touched",
		"tenant_id", ent.id.TenantID,
		"user_id", ent.id.UserID,
		"session_id", ent.id.SessionID,
		"recorded_at", ent.po.RecordedAt,
		"bound", "per_tenant",
		"max_slots_per_tenant", s.maxPerTenant)
}

// removeLocked unlinks ent from the slot map and from BOTH recording
// orders, dropping the tenant's list once it empties. The caller holds
// s.mu.
//
// The tenant-list delete is not tidiness: without it byTenant would grow
// with the count of tenants ever seen and reintroduce the unbounded-map
// defect one level up, on a key the caller does not choose but that a
// long-lived process still accumulates.
func (s *Store) removeLocked(ent *slotEntry) {
	delete(s.slots, ent.id)
	s.order.Remove(ent.globalEl)
	if tl := s.byTenant[ent.id.TenantID]; tl != nil {
		tl.Remove(ent.tenantEl)
		if tl.Len() == 0 {
			delete(s.byTenant, ent.id.TenantID)
		}
	}
}

// Consume removes and returns the pending override for id. The second
// return is false when the identity triple has no pending override —
// the common case (most messages carry no override). Consume is the
// one-shot read: the slot is empty after a Consume until the next Set.
//
// This is the seam the `user_message` / `start` consumer calls at the
// start of the next message in a session.
func (s *Store) Consume(id identity.Identity) (PendingOverride, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ent, ok := s.slots[id]
	if !ok {
		return PendingOverride{}, false
	}
	// Both orders are popped alongside the map. A stale element left in
	// either list is a tombstone that absorbs a later eviction, so the
	// corresponding bound leaks back open by one slot per consumed entry
	// — on the ORDINARY, non-abusive path.
	s.removeLocked(ent)
	return ent.po, true
}

// Peek returns the pending override for id WITHOUT removing it. Used by
// tests and read-side projections that must not consume the slot.
func (s *Store) Peek(id identity.Identity) (PendingOverride, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ent, ok := s.slots[id]
	if !ok {
		return PendingOverride{}, false
	}
	return ent.po, true
}

// Clock is the time source the Service stamps RecordedAt / AppliedAt
// from. Injected so tests pin a deterministic instant (CLAUDE.md §11 —
// time-sensitive tests use a controllable clock).
type Clock func() time.Time

// Service implements `runs.set_overrides`. It validates the override
// payload, enforces identity, records the override into the Store, and
// emits the `runs.overrides_set` audit event.
//
// The Service is a compiled artifact: immutable after
// NewService; every method's per-call state lives in arguments + locals.
type Service struct {
	store    *Store
	bus      events.EventBus // optional — nil ⇒ audit emit is logged only
	redactor audit.Redactor  // optional — defence-in-depth before the emit
	logger   *slog.Logger
	now      Clock
	// validModels is the set of model names with a configured
	// `ModelProfile`. When non-empty, a `Model` override outside it is
	// rejected at set time (fail loud, mirroring the tenant layer). Empty
	// = no model validation (the field is unset; existing callers/tests
	// compile unchanged), in which case an unknown model would surface at
	// the LLM safety edge instead.
	validModels map[string]struct{}
}

// Option configures NewService.
type Option func(*Service)

// WithBus wires the canonical events.EventBus the Service publishes the
// `runs.overrides_set` audit event onto. A nil bus is treated as
// "WithBus not supplied" — the override recording is then logged at
// Info instead of published (the action is NEVER fully silent —
// CLAUDE.md §13).
func WithBus(b events.EventBus) Option {
	return func(s *Service) {
		if b != nil {
			s.bus = b
		}
	}
}

// WithRedactor wires the audit.Redactor. The `runs.overrides_set`
// payload is a SafePayload by construction (it carries no
// caller-supplied bytes — only identity + boolean flags), so the bus
// bypasses the redactor for it; the redactor is held for parity with
// the other Console-page services and for any future non-safe emit.
func WithRedactor(r audit.Redactor) Option {
	return func(s *Service) {
		if r != nil {
			s.redactor = r
		}
	}
}

// WithLogger sets the slog.Logger. A nil logger routes to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithClock injects the time source. Defaults to time.Now. Tests pin a
// deterministic instant.
func WithClock(c Clock) Option {
	return func(s *Service) {
		if c != nil {
			s.now = c
		}
	}
}

// WithValidModels wires the set of model names that have a configured
// `ModelProfile`. When supplied (non-empty), a `Model` override naming a
// model outside the set is rejected at set time with ErrInvalidRequest
// (fail loud, mirroring the tenant-default layer). When unsupplied, the
// Service does not validate the model name — an unknown model surfaces at
// the LLM safety edge instead.
func WithValidModels(models []string) Option {
	return func(s *Service) {
		if len(models) == 0 {
			return
		}
		s.validModels = make(map[string]struct{}, len(models))
		for _, m := range models {
			if m != "" {
				s.validModels[m] = struct{}{}
			}
		}
	}
}

// NewService builds the `runs.set_overrides` Service over an override
// Store. store is mandatory — a nil fails loud with ErrMisconfigured
// rather than building a Service that would nil-panic on the first
// request (CLAUDE.md §5).
//
// The returned *Service is immutable after construction and
// safe for concurrent use by N goroutines.
func NewService(store *Store, opts ...Option) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: override Store is nil", ErrMisconfigured)
	}
	s := &Service{
		store:  store,
		logger: slog.Default(),
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// SetOverrides records the next-message override carried by req. It
// validates identity, cross-session scope, and the override payload,
// writes the validated override into the Store, emits the
// `runs.overrides_set` audit event, and returns the recording instant.
//
// The override applies to the NEXT message in the session — it is not
// retroactive. SetOverrides does not touch any past message.
func (s *Service) SetOverrides(ctx context.Context, req prototypes.RunSetOverridesRequest) (prototypes.RunSetOverridesResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.RunSetOverridesResponse{}, err
	}
	// Identity is mandatory — the verified triple must be complete.
	id := identity.Identity{
		TenantID:  req.Identity.Tenant,
		UserID:    req.Identity.User,
		SessionID: req.Identity.Session,
	}
	if id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return prototypes.RunSetOverridesResponse{}, fmt.Errorf("%w: (tenant=%q user=%q session=%q)",
			ErrIdentityRequired, id.TenantID, id.UserID, id.SessionID)
	}
	// The override's target session must be empty (defaults to the
	// caller's session) OR equal to the verified session. A SessionID
	// naming a different session is a cross-session escalation attempt.
	target := req.Overrides.SessionID
	if target == "" {
		return prototypes.RunSetOverridesResponse{}, fmt.Errorf("%w: overrides.session_id is empty", ErrInvalidRequest)
	}
	if target != id.SessionID {
		return prototypes.RunSetOverridesResponse{}, fmt.Errorf("%w: override session_id=%q, verified session=%q",
			ErrCrossSessionScope, target, id.SessionID)
	}
	// Validate the override payload — fail loud on an out-of-range or
	// unknown value rather than silently passing it to the provider.
	po, err := s.validate(req.Overrides)
	if err != nil {
		return prototypes.RunSetOverridesResponse{}, err
	}
	now := s.now().UTC()
	po.RecordedAt = now
	s.store.Set(id, po)

	s.emitAudit(ctx, id, po, now)

	return prototypes.RunSetOverridesResponse{
		AppliedAt:       now,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// validate converts a wire RunOverrides into a validated
// PendingOverride, rejecting structurally-invalid values. A request
// that sets NO tuning field at all is valid — it is a no-op override
// (the slot is set with all-nil fields, and the next message proceeds
// with runtime defaults); the Playground never sends that shape, but
// the Service does not reject it.
func (s *Service) validate(o prototypes.RunOverrides) (PendingOverride, error) {
	var po PendingOverride
	if o.ReasoningEffort != nil {
		if _, ok := validReasoningEffort[*o.ReasoningEffort]; !ok {
			return PendingOverride{}, fmt.Errorf("%w: unknown reasoning_effort %q (want low/medium/high)",
				ErrInvalidRequest, *o.ReasoningEffort)
		}
		v := *o.ReasoningEffort
		po.ReasoningEffort = &v
	}
	if o.Temperature != nil {
		if *o.Temperature < 0 || *o.Temperature > 2 {
			return PendingOverride{}, fmt.Errorf("%w: temperature %v outside [0,2]", ErrInvalidRequest, *o.Temperature)
		}
		v := *o.Temperature
		po.Temperature = &v
	}
	if o.MaxTokens != nil {
		if *o.MaxTokens <= 0 {
			return PendingOverride{}, fmt.Errorf("%w: max_tokens %d must be positive", ErrInvalidRequest, *o.MaxTokens)
		}
		v := *o.MaxTokens
		po.MaxTokens = &v
	}
	if o.SystemPromptOverride != nil {
		v := *o.SystemPromptOverride
		po.SystemPromptOverride = &v
	}
	if o.ExtraInstructions != nil {
		// Copied BY VALUE, never aliased: the stored slot must not change
		// under a caller that mutates its request struct afterwards. An
		// empty or whitespace-only value is deliberately ACCEPTED — it
		// contributes nothing at composition time and is not a channel for
		// clearing the tenant-wide block (there is no run-level clear).
		v := *o.ExtraInstructions
		po.ExtraInstructions = &v
	}
	if o.Model != nil && *o.Model != "" {
		m := *o.Model
		if len(s.validModels) > 0 {
			if _, ok := s.validModels[m]; !ok {
				return PendingOverride{}, fmt.Errorf("%w: unknown model %q (no configured ModelProfile)",
					ErrInvalidRequest, m)
			}
		}
		// An empty Model is treated as "no model swap" (dropped) so it
		// never clobbers a tenant default down to the config model — a
		// session model override means a REAL model, not a clear.
		po.Model = &m
	}
	return po, nil
}

// emitAudit publishes a `runs.overrides_set` event recording the
// override. The bus is optional (WithBus); when unsupplied the
// recording is logged at Info instead of published — the action is
// NEVER fully silent (CLAUDE.md §13 "no silent degradation").
func (s *Service) emitAudit(ctx context.Context, id identity.Identity, po PendingOverride, at time.Time) {
	logAttrs := []any{
		slog.String("tenant_id", id.TenantID),
		slog.String("user_id", id.UserID),
		slog.String("session_id", id.SessionID),
		slog.Bool("set_reasoning_effort", po.ReasoningEffort != nil),
		slog.Bool("set_temperature", po.Temperature != nil),
		slog.Bool("set_max_tokens", po.MaxTokens != nil),
		slog.Bool("set_system_prompt", po.SystemPromptOverride != nil),
		slog.Bool("set_extra_instructions", po.ExtraInstructions != nil),
		slog.Bool("set_model", po.Model != nil),
	}
	if s.bus == nil {
		s.logger.InfoContext(ctx, "runs/protocol: override recorded (bus not wired — audit logged only)", logAttrs...)
		return
	}
	payload := events.RunOverridesSetPayload{
		Actor:              identity.Quadruple{Identity: id},
		SessionID:          id.SessionID,
		SetReasoningEffort: po.ReasoningEffort != nil,
		SetTemperature:     po.Temperature != nil,
		SetMaxTokens:       po.MaxTokens != nil,
		SetSystemPrompt:    po.SystemPromptOverride != nil,
		// The FLAG only — the additive guidance text is caller-supplied
		// free text and must never ride the bus (CLAUDE.md §7).
		SetExtraInstructions: po.ExtraInstructions != nil,
		SetModel:             po.Model != nil,
		OccurredAt:           at,
	}
	ev := events.Event{
		Type:       events.EventTypeRunOverridesSet,
		Identity:   identity.Quadruple{Identity: id},
		OccurredAt: at,
		Payload:    payload,
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		// A failed publish is logged loudly — never swallowed.
		s.logger.WarnContext(ctx, "runs/protocol: failed to publish runs.overrides_set audit event",
			append(logAttrs, slog.String("error", err.Error()))...)
	}
}
