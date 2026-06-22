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
// a source/tool DISABLE set (never an enable), and the names of the
// session's ephemeral personal skills. Because the shape has no base-prompt
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
	"sync"
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
	// PersonalSkills names the session's ephemeral personal skills (the
	// bodies live in the session-scoped SkillStore under the same triple).
	// They are added back to the session's skill view at projection and
	// never promote to the agent/tenant scope.
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
	// AddPersonalSkill records a personal-skill name (idempotent), preserving
	// every other field.
	AddPersonalSkill(ctx context.Context, id identity.Quadruple, agentID, name string) (Overlay, error)
	// RemovePersonalSkill removes a personal-skill name (idempotent),
	// preserving every other field.
	RemovePersonalSkill(ctx context.Context, id identity.Quadruple, agentID, name string) (Overlay, error)
	// Close releases resources. Idempotent.
	Close(ctx context.Context) error
}

// Persisted-record schema version. Bumped only on a breaking record shape
// change; a forward-schema record fails loud rather than silently reset.
const recordSchema = 1

// kindPrefix is the StateStore Kind prefix every session-overlay record
// persists under. The agent id is appended so distinct agents have distinct
// overlays within the same session.
const kindPrefix = "agentcfg.session_overlay."

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

	// writeLocks serialises the mutate() read-modify-write per overlay slot
	// (the full (tenant, user, session) triple + agent). The overlay row is
	// last-write-wins, so two concurrent SAME-session edits (e.g.
	// AddPersonalSkill racing SetSourceDisables) would each load → apply →
	// save from the other's pre-write snapshot, losing one update. A per-slot
	// lock held across the whole read-modify-write makes it atomic.
	writeLocks sync.Map // map[string]*sync.Mutex
}

// lockSlot acquires the per-overlay-slot write lock and returns the release
// func (call via defer). Keyed by the full triple + agent so distinct
// sessions / agents never contend.
func (s *store) lockSlot(id identity.Quadruple, agentID string) func() {
	key := id.TenantID + "\x00" + id.UserID + "\x00" + id.SessionID + "\x00" + agentID
	mu, _ := s.writeLocks.LoadOrStore(key, &sync.Mutex{})
	m, ok := mu.(*sync.Mutex)
	if !ok {
		// Impossible by construction: writeLocks only ever stores *sync.Mutex.
		panic("agentcfg/sessionoverlay: writeLocks held a non-mutex value")
	}
	m.Lock()
	return m.Unlock
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
	if id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return fmt.Errorf("%w: (tenant=%q user=%q session=%q)", ErrIdentityRequired, id.TenantID, id.UserID, id.SessionID)
	}
	if agentID == "" {
		return fmt.Errorf("%w: agent id is empty", ErrIdentityRequired)
	}
	return nil
}

func (s *store) Get(ctx context.Context, id identity.Quadruple, agentID string) (Overlay, bool, error) {
	if err := s.validate(id, agentID); err != nil {
		return Overlay{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return Overlay{}, false, err
	}
	return s.load(ctx, id, agentID)
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
	if name == "" {
		return Overlay{}, fmt.Errorf("%w: personal-skill name is empty", ErrInvalidInput)
	}
	return s.mutate(ctx, id, agentID, func(o *Overlay) {
		set := make(map[string]struct{}, len(o.PersonalSkills)+1)
		for _, n := range o.PersonalSkills {
			set[n] = struct{}{}
		}
		set[name] = struct{}{}
		o.PersonalSkills = setToSorted(set)
	})
}

func (s *store) RemovePersonalSkill(ctx context.Context, id identity.Quadruple, agentID, name string) (Overlay, error) {
	if name == "" {
		return Overlay{}, fmt.Errorf("%w: personal-skill name is empty", ErrInvalidInput)
	}
	return s.mutate(ctx, id, agentID, func(o *Overlay) {
		set := make(map[string]struct{}, len(o.PersonalSkills))
		for _, n := range o.PersonalSkills {
			if n != name {
				set[n] = struct{}{}
			}
		}
		o.PersonalSkills = setToSorted(set)
	})
}

func (s *store) Close(_ context.Context) error {
	s.closed.Store(true)
	return nil
}

// mutate is the read-modify-write helper every set method shares: it loads
// the current overlay (or the zero overlay when none exists), applies the
// mutation, normalises, and persists. The mutation only ever touches the
// safe-subset fields — there is no base-prompt or enable path to reach.
func (s *store) mutate(ctx context.Context, id identity.Quadruple, agentID string, apply func(*Overlay)) (Overlay, error) {
	if err := s.validate(id, agentID); err != nil {
		return Overlay{}, err
	}
	if err := ctx.Err(); err != nil {
		return Overlay{}, err
	}
	defer s.lockSlot(id, agentID)()
	cur, _, err := s.load(ctx, id, agentID)
	if err != nil {
		return Overlay{}, err
	}
	apply(&cur)
	if err := s.save(ctx, id, agentID, cur); err != nil {
		return Overlay{}, err
	}
	return cur, nil
}

func (s *store) load(ctx context.Context, id identity.Quadruple, agentID string) (Overlay, bool, error) {
	rec, err := s.state.Load(ctx, sessionQuad(id), kindPrefix+agentID)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return Overlay{}, false, nil
		}
		return Overlay{}, false, fmt.Errorf("%w: load overlay: %w", ErrStateUnavailable, err)
	}
	if len(rec.Bytes) == 0 {
		return Overlay{}, false, nil
	}
	var or overlayRecord
	if err := json.Unmarshal(rec.Bytes, &or); err != nil {
		return Overlay{}, false, fmt.Errorf("%w: unmarshal overlay: %w", ErrStateUnavailable, err)
	}
	if or.Schema != 0 && or.Schema != recordSchema {
		return Overlay{}, false, fmt.Errorf("%w: overlay schema=%d, runtime supports %d", ErrStateUnavailable, or.Schema, recordSchema)
	}
	return or.Overlay, true, nil
}

func (s *store) save(ctx context.Context, id identity.Quadruple, agentID string, o Overlay) error {
	now := s.clock().UTC()
	rec := overlayRecord{Schema: recordSchema, Overlay: o, UpdatedAt: now}
	buf, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("agentcfg/sessionoverlay: marshal overlay: %w", err)
	}
	if err := s.state.Save(ctx, state.StateRecord{
		ID:        state.NewEventID(),
		Identity:  sessionQuad(id),
		Kind:      kindPrefix + agentID,
		Bytes:     buf,
		UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("%w: save overlay: %w", ErrStateUnavailable, err)
	}
	return nil
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
	return setToSorted(set)
}

// setToSorted returns the set's members sorted. An empty set returns nil so
// an emptied field drops out of the persisted record.
func setToSorted(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
