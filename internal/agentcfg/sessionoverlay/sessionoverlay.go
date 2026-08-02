// Package sessionoverlay owns the SESSION-scoped safe-subset overlay of the
// agent-config control plane: the lower tier of the authorization matrix. A
// session-scoped (non-admin) end user may set a user-instruction prompt
// layer, NARROW (never widen) source/tool enablement within the
// admin-allowed set, and manage ephemeral personal skills. The overlay
// composes OVER the admin agent config at run start; it can only ever
// narrow a capability the admin granted, never grant a new one.
//
// # Keying — the session is the isolation boundary (CLAUDE.md §6)
//
// Unlike the admin agent-config registry (which keys by a synthetic
// {tenant, "__agentcfg__", agentID} identity so config is agent-level), the
// session overlay is keyed by the caller's REAL (tenant, user, session)
// triple plus the agent id in the record Kind. The StateStore is
// identity-scoped by the triple, so one session's overlay is invisible to
// another session by construction — there is no code path where session A's
// overlay reaches session B's run.
//
// # The safe subset is structurally narrow-only
//
// The persisted Overlay carries ONLY a user prompt layer (never a base),
// a source/tool DISABLE set (never an enable), and the read-only legacy names
// of the session's ephemeral personal skills. Because the shape has no base-prompt
// field and no enable field, a session caller PHYSICALLY cannot widen a
// capability or edit the operator base — the data model carries the
// guarantee; the projection's union-only composition is the second layer.
//
// # Concurrent reuse (the concurrent-reuse contract)
//
// A constructed Store is immutable after construction except for the closed
// atomic flag; every read is fresh from the StateStore (no cache). Safe to
// share across N concurrent goroutines; per-call state lives in arguments
// and locals.
package sessionoverlay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrIdentityRequired — a method was called with an incomplete identity
	// triple or an empty agent id. Fails closed (CLAUDE.md §6).
	ErrIdentityRequired = errors.New("agentcfg/sessionoverlay: identity triple incomplete")
	// ErrInvalidConfig — NewStore was called with a nil StateStore.
	ErrInvalidConfig = errors.New("agentcfg/sessionoverlay: invalid configuration")
	// ErrClosed — a method was called after Close.
	ErrClosed = errors.New("agentcfg/sessionoverlay: store is closed")
	// ErrStateUnavailable — a StateStore read/write failed.
	ErrStateUnavailable = errors.New("agentcfg/sessionoverlay: state store unavailable")
	// ErrInvalidInput — a method was called with an empty/invalid argument
	// (an empty personal-skill name on add/remove).
	ErrInvalidInput = errors.New("agentcfg/sessionoverlay: invalid input")
)

// Overlay is the session-scoped safe-subset desired state. Every field is a
// SAFE capability: a user prompt layer that composes ABOVE the operator
// base, a narrow-only source/tool disable set, and the names of the
// session's ephemeral personal skills.
type Overlay struct {
	// UserPrompt is the session's user-instruction prompt layer. It composes
	// ABOVE the admin base in the lower-trust `<user_instructions>` position
	// (escaped); it can extend the operator's guidance but never precede,
	// replace, or weaken the operator base. There is intentionally NO Base
	// field — a session caller cannot write the operator base.
	UserPrompt string `json:"user_prompt,omitempty"`
	// DisabledServers names MCP servers the session has DISABLED. At
	// projection these are UNIONED into the admin exclusion set (the session
	// can only ADD to the disabled set, never remove an admin exclusion) — so
	// the result can only ever NARROW the admin-allowed exposure.
	DisabledServers []string `json:"disabled_servers,omitempty"`
	// DisabledTools names tools the session has DISABLED. Unioned into the
	// admin exclusion set at projection (narrow-only).
	DisabledTools []string `json:"disabled_tools,omitempty"`
	// PersonalSkills is schema-1 migration-eligibility input only. New code
	// never mutates this field; agent-owned personal records are body and
	// membership after cutover.
	PersonalSkills []string `json:"personal_skills,omitempty"`
}

// Store is the session-scoped overlay persistence surface. It is a compiled
// artifact: immutable after construction and safe for concurrent reuse.
type Store interface {
	// Get returns the session's overlay for the agent and whether one exists.
	// No overlay returns (zero, false, nil).
	Get(ctx context.Context, id identity.Quadruple, agentID string) (Overlay, bool, error)
	// SetUserPrompt sets ONLY the user prompt layer, preserving the disable
	// set + personal skills. An empty prompt clears the user layer.
	SetUserPrompt(ctx context.Context, id identity.Quadruple, agentID, prompt string) (Overlay, error)
	// SetSourceDisables replaces the narrow-only DISABLE set (servers +
	// tools), preserving the user prompt + personal skills. There is no
	// "enable" — the set names what the session wants OFF; the projection
	// unions it into the admin exclusion set, so it can only narrow.
	SetSourceDisables(ctx context.Context, id identity.Quadruple, agentID string, servers, tools []string) (Overlay, error)
	// AddPersonalSkill is retained for interface compatibility and fails loud
	// with ErrCutoverPending without writing the legacy overlay.
	AddPersonalSkill(ctx context.Context, id identity.Quadruple, agentID, name string) (Overlay, error)
	// RemovePersonalSkill is retained for interface compatibility and fails
	// loud with ErrCutoverPending without writing the legacy overlay.
	RemovePersonalSkill(ctx context.Context, id identity.Quadruple, agentID, name string) (Overlay, error)
	// Close releases resources. Idempotent.
	Close(ctx context.Context) error
}

// Persisted-record schema version. Bumped only on a breaking record shape
// change; a forward-schema record fails loud rather than silently reset.
const recordSchema = 1

// overlayRecord is the JSON-encoded session-overlay record.
type overlayRecord struct {
	Schema    int       `json:"schema"`
	Overlay   Overlay   `json:"overlay"`
	UpdatedAt time.Time `json:"updated_at"`
}

// store is the StateStore-backed Store implementation.
type store struct {
	state  state.StateStore
	clock  func() time.Time
	closed atomic.Bool
}

// NewStore builds a session-overlay Store over a StateStore. st is
// mandatory — a nil fails loud with ErrInvalidConfig rather than building a
// store that would nil-panic on the first request (CLAUDE.md §5).
//
// The returned Store is immutable after construction and safe for concurrent
// use by N goroutines.
func NewStore(st state.StateStore, clock func() time.Time) (Store, error) {
	if st == nil {
		return nil, fmt.Errorf("%w: state.StateStore is required", ErrInvalidConfig)
	}
	if clock == nil {
		clock = time.Now
	}
	return &store{state: st, clock: clock}, nil
}

// sessionQuad zeroes the RunID so the overlay is SESSION-scoped (it spans
// runs within a session). The real (tenant, user, session) triple is the
// isolation boundary.
func sessionQuad(id identity.Quadruple) identity.Quadruple {
	return identity.Quadruple{Identity: id.Identity}
}

func (s *store) validate(id identity.Quadruple, agentID string) error {
	if s.closed.Load() {
		return ErrClosed
	}
	return validateSessionInput(id, agentID)
}

func (s *store) Get(ctx context.Context, id identity.Quadruple, agentID string) (Overlay, bool, error) {
	if err := s.validate(id, agentID); err != nil {
		return Overlay{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return Overlay{}, false, err
	}
	for range MaxSessionSkillReadAttempts {
		if err := ctx.Err(); err != nil {
			return Overlay{}, false, err
		}
		before, err := loadFences(ctx, s.state, id, agentID)
		if err != nil {
			return Overlay{}, false, err
		}
		if before.erased() {
			return Overlay{}, false, ErrSessionErased
		}
		if err := before.lifecycleError(); err != nil {
			return Overlay{}, false, err
		}
		overlay, found, _, err := s.loadSlot(ctx, id, agentID)
		if err != nil {
			return Overlay{}, false, err
		}
		stable, err := before.stable(ctx, s.state)
		if err != nil {
			return Overlay{}, false, err
		}
		if stable {
			return overlay, found, nil
		}
	}
	return Overlay{}, false, ErrSessionSkillReadUnstable
}

func (s *store) SetUserPrompt(ctx context.Context, id identity.Quadruple, agentID, prompt string) (Overlay, error) {
	return s.mutate(ctx, id, agentID, func(o *Overlay) {
		o.UserPrompt = prompt
	})
}

func (s *store) SetSourceDisables(ctx context.Context, id identity.Quadruple, agentID string, servers, tools []string) (Overlay, error) {
	return s.mutate(ctx, id, agentID, func(o *Overlay) {
		o.DisabledServers = sortDedup(servers)
		o.DisabledTools = sortDedup(tools)
	})
}

func (s *store) AddPersonalSkill(ctx context.Context, id identity.Quadruple, agentID, name string) (Overlay, error) {
	if err := s.validate(id, agentID); err != nil {
		return Overlay{}, err
	}
	if err := ctx.Err(); err != nil {
		return Overlay{}, err
	}
	if canonicalNameFor(name) == "" {
		return Overlay{}, fmt.Errorf("%w: personal-skill name is empty", ErrInvalidInput)
	}
	return Overlay{}, ErrCutoverPending
}

func (s *store) RemovePersonalSkill(ctx context.Context, id identity.Quadruple, agentID, name string) (Overlay, error) {
	if err := s.validate(id, agentID); err != nil {
		return Overlay{}, err
	}
	if err := ctx.Err(); err != nil {
		return Overlay{}, err
	}
	if canonicalNameFor(name) == "" {
		return Overlay{}, fmt.Errorf("%w: personal-skill name is empty", ErrInvalidInput)
	}
	return Overlay{}, ErrCutoverPending
}

func (s *store) Close(_ context.Context) error {
	s.closed.Store(true)
	return nil
}

// mutate is the four-slot CAS helper shared by the writable prompt/disable
// verbs. PersonalSkills is decoded and re-encoded unchanged; it is read-only
// migration input, never a writable membership projection.
func (s *store) mutate(ctx context.Context, id identity.Quadruple, agentID string, apply func(*Overlay)) (Overlay, error) {
	if err := s.validate(id, agentID); err != nil {
		return Overlay{}, err
	}
	if err := ctx.Err(); err != nil {
		return Overlay{}, err
	}
	fences, err := loadFences(ctx, s.state, id, agentID)
	if err != nil {
		return Overlay{}, err
	}
	if fences.erased() {
		return Overlay{}, ErrSessionErased
	}
	if err := fences.lifecycleError(); err != nil {
		return Overlay{}, err
	}
	cur, _, target, err := s.loadSlot(ctx, id, agentID)
	if err != nil {
		return Overlay{}, err
	}
	apply(&cur)
	now := s.clock().UTC()
	bytes, err := encodeOverlayRecord(cur, now)
	if err != nil {
		return Overlay{}, err
	}
	next := state.StateRecord{ID: state.NewEventID(), Identity: sessionQuad(id), Kind: LegacyOverlayKind(agentID), Bytes: bytes, UpdatedAt: now}
	expectations := append([]state.SlotExpectation{target}, fences.expectations()...)
	if err := s.state.SaveIf(ctx, expectations, next); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return Overlay{}, err
		}
		converged, ok, reconcileErr := s.exactOverlay(ctx, id, agentID, next, fences)
		if reconcileErr == nil && ok {
			return converged, nil
		}
		if reconcileErr != nil {
			return Overlay{}, fmt.Errorf("%w: conditional overlay save: %w; exact reread: %w", ErrStateUnavailable, err, reconcileErr)
		}
		return Overlay{}, fmt.Errorf("%w: conditional overlay save outcome uncertain: %w", ErrStateUnavailable, err)
	}
	return cur, nil
}

func (s *store) loadSlot(ctx context.Context, id identity.Quadruple, agentID string) (Overlay, bool, state.SlotExpectation, error) {
	q := sessionQuad(id)
	kind := LegacyOverlayKind(agentID)
	rec, err := s.state.Load(ctx, q, kind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return Overlay{}, false, state.SlotExpectation{Identity: q, Kind: kind}, nil
		}
		return Overlay{}, false, state.SlotExpectation{}, fmt.Errorf("%w: load overlay: %w", ErrStateUnavailable, err)
	}
	overlay, err := decodeOverlayRecord(rec.Bytes)
	if err != nil {
		return Overlay{}, false, state.SlotExpectation{}, err
	}
	return overlay, true, state.SlotExpectation{Identity: q, Kind: kind, ExpectedEventID: rec.ID}, nil
}

func (s *store) exactOverlay(ctx context.Context, id identity.Quadruple, agentID string, expected state.StateRecord, before fences) (Overlay, bool, error) {
	rec, err := s.state.Load(ctx, expected.Identity, expected.Kind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return Overlay{}, false, nil
		}
		return Overlay{}, false, err
	}
	if rec.ID != expected.ID || string(rec.Bytes) != string(expected.Bytes) {
		return Overlay{}, false, nil
	}
	after, err := loadFences(ctx, s.state, id, agentID)
	if err != nil {
		return Overlay{}, false, err
	}
	if after.erased() || !before.equal(after) {
		return Overlay{}, false, nil
	}
	if err := after.lifecycleError(); err != nil {
		return Overlay{}, false, err
	}
	overlay, err := decodeOverlayRecord(rec.Bytes)
	return overlay, err == nil, err
}

func encodeOverlayRecord(o Overlay, now time.Time) ([]byte, error) {
	rec := overlayRecord{Schema: recordSchema, Overlay: o, UpdatedAt: now}
	bytes, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("agentcfg/sessionoverlay: marshal overlay: %w", err)
	}
	if len(bytes) == 0 || len(bytes) > MaxLegacySessionOverlayRecordBytes {
		return nil, fmt.Errorf("%w: overlay record size %d is outside 1..%d", ErrInvalidInput, len(bytes), MaxLegacySessionOverlayRecordBytes)
	}
	return bytes, nil
}

func decodeOverlayRecord(data []byte) (Overlay, error) {
	if len(data) == 0 || len(data) > MaxLegacySessionOverlayRecordBytes {
		return Overlay{}, fmt.Errorf("%w: overlay record size %d is outside 1..%d", ErrLegacyOverlayInvalid, len(data), MaxLegacySessionOverlayRecordBytes)
	}
	if err := rejectDuplicateJSONObjectFields(data); err != nil {
		return Overlay{}, fmt.Errorf("%w: duplicate envelope field: %w", ErrLegacyOverlayInvalid, err)
	}
	var envelope struct {
		Schema    *int            `json:"schema"`
		Overlay   json.RawMessage `json:"overlay"`
		UpdatedAt *time.Time      `json:"updated_at"`
	}
	if err := decodeStrictJSON(data, &envelope); err != nil {
		return Overlay{}, fmt.Errorf("%w: decode schema-1 envelope: %w", ErrLegacyOverlayInvalid, err)
	}
	if envelope.Schema == nil || *envelope.Schema != recordSchema || len(envelope.Overlay) == 0 || string(envelope.Overlay) == "null" || envelope.UpdatedAt == nil || envelope.UpdatedAt.IsZero() {
		return Overlay{}, fmt.Errorf("%w: incompatible schema-1 envelope", ErrLegacyOverlayInvalid)
	}
	if err := rejectDuplicateJSONObjectFields(envelope.Overlay); err != nil {
		return Overlay{}, fmt.Errorf("%w: duplicate overlay field: %w", ErrLegacyOverlayInvalid, err)
	}
	var overlay Overlay
	if err := decodeStrictJSON(envelope.Overlay, &overlay); err != nil {
		return Overlay{}, fmt.Errorf("%w: decode overlay body: %w", ErrLegacyOverlayInvalid, err)
	}
	for _, values := range [][]string{overlay.DisabledServers, overlay.DisabledTools, overlay.PersonalSkills} {
		if !sort.StringsAreSorted(values) {
			return Overlay{}, fmt.Errorf("%w: overlay lists must be canonical sorted sets", ErrLegacyOverlayInvalid)
		}
		for i, value := range values {
			if value == "" || (i > 0 && values[i-1] == value) {
				return Overlay{}, fmt.Errorf("%w: overlay lists must contain unique non-empty values", ErrLegacyOverlayInvalid)
			}
		}
	}
	return overlay, nil
}

// sortDedup returns a sorted, de-duplicated copy. A nil input returns nil.
func sortDedup(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(in))
	for _, s := range in {
		if s != "" {
			set[s] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
