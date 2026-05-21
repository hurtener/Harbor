// Package protocol implements the `runs.set_overrides` Protocol method
// the Console Playground page (Phase 73n / D-130) consumes.
//
// `runs.set_overrides` records the reasoning-effort / temperature /
// max-tokens / system-prompt override an operator applies to the NEXT
// message in a session. The override is:
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
// # Concurrent reuse (D-025)
//
// A constructed *Service is immutable after NewService and safe to
// share across N concurrent goroutines: it holds only the Store
// reference + an optional bus + redactor + logger + clock. Every
// method's per-call state lives in the call's arguments and locals,
// never on the Service. The Store guards its map with a sync.Mutex —
// the only mutable state, and it is documented "internally
// synchronised" per the D-025 carve-out.
package protocol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

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
	// RecordedAt is the runtime instant the override entered the slot.
	RecordedAt time.Time
}

// Store is the in-process, identity-scoped pending-override slot map.
// It is a compiled artifact (D-025): constructed once via NewStore,
// shared across N goroutines, with its single mutable field — the slot
// map — guarded by an internally-synchronised sync.Mutex.
//
// The map is keyed by the identity triple so a session's pending
// override is invisible to every other `(tenant, user, session)` —
// multi-isolation is enforced by the key, not by a post-fetch filter.
type Store struct {
	mu    sync.Mutex
	slots map[identity.Identity]PendingOverride
}

// NewStore builds an empty override Store. The returned *Store is safe
// for concurrent use by N goroutines.
func NewStore() *Store {
	return &Store{slots: make(map[identity.Identity]PendingOverride)}
}

// Set records po into the slot for id, replacing any prior pending
// override for that identity triple. An operator that records two
// overrides before sending a message keeps only the second — the slot
// is last-write-wins, the documented behaviour.
func (s *Store) Set(id identity.Identity, po PendingOverride) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slots[id] = po
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
	po, ok := s.slots[id]
	if ok {
		delete(s.slots, id)
	}
	return po, ok
}

// Peek returns the pending override for id WITHOUT removing it. Used by
// tests and read-side projections that must not consume the slot.
func (s *Store) Peek(id identity.Identity) (PendingOverride, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	po, ok := s.slots[id]
	return po, ok
}

// Clock is the time source the Service stamps RecordedAt / AppliedAt
// from. Injected so tests pin a deterministic instant (CLAUDE.md §11 —
// time-sensitive tests use a controllable clock).
type Clock func() time.Time

// Service implements `runs.set_overrides`. It validates the override
// payload, enforces identity, records the override into the Store, and
// emits the `runs.overrides_set` audit event.
//
// The Service is a compiled artifact (D-025): immutable after
// NewService; every method's per-call state lives in arguments + locals.
type Service struct {
	store    *Store
	bus      events.EventBus // optional — nil ⇒ audit emit is logged only
	redactor audit.Redactor  // optional — defence-in-depth before the emit
	logger   *slog.Logger
	now      Clock
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

// NewService builds the `runs.set_overrides` Service over an override
// Store. store is mandatory — a nil fails loud with ErrMisconfigured
// rather than building a Service that would nil-panic on the first
// request (CLAUDE.md §5).
//
// The returned *Service is immutable after construction (D-025) and
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
		OccurredAt:         at,
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
