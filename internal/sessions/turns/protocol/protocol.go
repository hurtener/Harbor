// Package protocol implements the domain-side service adapters for the
// `sessions.turns.list` / `sessions.turns.get` Protocol reads — the
// two named public conversation-turn methods (the v1.28 Protocol
// surface is EXACTLY those two reads; there is no third activity or
// analytics subresource).
//
// This package is a PROTOCOL-DOMAIN SERVICE ADAPTER, not a wire
// handler: it owns the request/response adaptation, the authority
// matrix (verified identity, session reach, the effective-agent gate,
// and the admin/fleet operations lane), cursor validation, and the
// bounded paging contract over the turn projection's domain core
// (`internal/sessions/turns`). The wire transport, canonical
// wire types, method registry, and docs are a separate lane. Until
// that lane lands, every payload is a DOMAIN DTO (`turns.TurnRow`,
// `turns.OpsTurnRow`, `turns.Page`, `turns.Cursor`) — this package
// deliberately defines no parallel wire structs.
//
// # The seam (CLAUDE.md §4.4)
//
// The Service depends on the narrow `Projector` interface, not on the
// concrete `*turns.Projector`: `*turns.Projector` satisfies it (the
// compile-time assertion below), and a future remote / cross-runtime
// projector slots in behind the same interface without reshaping the
// Service. The projection core itself is the ONLY read authority — the
// Service NEVER falls back to `tasks.get`, `events.list`, or
// `state.history` on any path (there is no fallback seam to reach:
// every read flows through the one `Projector` seam).
//
// # Verified identity is mandatory (CLAUDE.md §6 rule 9)
//
// Every method reads the VERIFIED identity from the request context
// (`identity.FromVerified`) — the request edge's anchor, never a body
// field. A missing or incomplete verified triple fails closed with
// `ErrIdentityRequired`; there is no identity-downgrading knob and no
// package-level identity state.
//
// # Consumer exact-session reads
//
// `sessions.turns.list` / `sessions.turns.get` under the default
// `ProjectionConversation`, and `sessions.turns.get` under
// `ProjectionUsage`, are EXACT-SESSION consumer reads. Three
// gates run, in order:
//
//  1. Verified identity (above).
//  2. Session reach: the request's `session_id` is the effective
//     session. When the Service is wired with the canonical
//     session-reach gate (`auth.NewSessionReachAuthorizer`) and the
//     bearer's JWT carried a signed `session_reach` claim, the
//     effective session must be a member — an absent claim preserves
//     dynamic selection (the gate itself encodes that distinction).
//     A denial is LOUD (`ErrSessionReachDenied` → the wire lane maps
//     it onto the scope-mismatch class), exactly as the signed-reach
//     contract settles. An unwired gate (nil authorizer) is
//     documented as "the transport edge is the enforcement point" —
//     the exact-session identity boundary below still holds.
//  3. Exact-session boundary: for a consumer read the requested
//     session MUST equal the caller's verified session. Any other
//     session — a sibling session of the same user, a foreign user, a
//     foreign tenant — is NON-ORACULAR: it is answered exactly like a
//     session that does not exist (`ErrTurnNotFound` for get; the
//     same typed not-found for list). Cross-identity denial never
//     becomes an existence oracle.
//
// Then the EFFECTIVE-AGENT GATE applies per served row: a turn whose
// row carries a non-empty effective agent id is served ONLY to a
// caller whose verified `agent_reach` (seated on ctx by the auth
// middleware) contains that agent. A row with no effective agent
// binding (empty `Agent.ID`) is served. An unwired gate (nil
// authorizer) FAILS CLOSED — the named-agent row is not served,
// matching the transport gate's nil posture (never a silent widening).
// On `get`, a gated-out turn answers the same typed not-found as an
// absent turn; on `list`, gated rows are filtered out of the page and
// the page's exact remaining-count is degraded to unknown (the count
// must not leak rows the caller may not observe).
//
// # The operations lane (get-only)
//
// `sessions.turns.get` with `ProjectionOperations` is the elevated
// admin/fleet observation lane. It REQUIRES a verified `admin` or
// `console:fleet` claim on the request context (the closed two-scope
// set) and returns the STRUCTURALLY DISTINCT operations DTO
// (`turns.OpsTurnRow`) — which, by construction, carries no query, no
// answer, no reasoning steps, no App resource URI / tool-call id / App
// context, and no pause tokens. The lane reads any session of the
// caller's own `(tenant, user)`; reading a session other than the
// caller's own session is a WIDENED read and emits the canonical
// `audit.admin_scope_used` event (typed `TurnsAdminQueryPayload`) with
// the verified actor and the read target — the admin action is NEVER
// silent. No content-read / impersonation authority is created: the
// operations lane observes lifecycle / usage / activity / counts only,
// and no cross-principal transcript read exists anywhere. There is no
// operations LIST surface — the projection core ships `OpsTurn`, not
// an ops list — so `sessions.turns.list` rejects every non-conversation
// projection as invalid.

// # The consumer usage lane (get-only)

// `sessions.turns.get` with `ProjectionUsage` is a consumer-only,
// exact-session read that reuses the same single durable turn-index read as
// the conversation projection. It needs no operations claim and cannot widen
// across sessions. The returned UsageTurnRow is structurally content-free:
// lifecycle/timing, turn/task/session/agent identifiers, and the canonical
// usage rollup (per-measure states plus an optional reported model). It does
// not create a second event scan, polling loop, store,
// or background worker.
//
// # Paging, cursors, bounds
//
// List pages newest-first by the immutable keys
// `(Sequence DESC, TurnID DESC)` — the domain's snapshot/keyset cursor
// (session + snapshot-generation + boundary-row-sequence bound). The
// request's opaque `older_cursor` decodes via the domain's own
// `turns.DecodeCursor`; the response's `next_older_cursor` is the
// domain's encoded form, so a client feeds the returned cursor back
// verbatim. Invalid / foreign-session / stale-snapshot /
// retention-expired cursors surface as the domain's DISTINCT typed
// sentinels (`turns.ErrInvalidCursor`, `ErrCursorForeignSession`,
// `ErrCursorSnapshotStale`, `ErrCursorExpired`) for the wire lane to
// map — never a silent reset to page one. `limit` defaults to
// `turns.DefaultListLimit` (20) and is capped at `turns.MaxListLimit`
// (50); out-of-range limits fail loud. The response carries the page's
// snapshot id (as-of retention generation), as-of instant, the
// exclusive live-resume sequence (the page-before-subscribe
// boundary: the consumer folds the durable page, establishing
// bounded membership, before opening `events.subscribe` from
// `LiveResumeSeq+1`), and the explicit page completeness / partial
// reason (`retention_eviction`).
//
// # Pause actionability is never read from the projection
//
// The domain `turns.Pause` component carries class / reason /
// lifecycle / availability only — structurally NO pause / resume /
// approval token — and the projection stores no actionability. This
// Service passes the pause component through unmodified and NEVER
// derives an action token or control-tier verdict from any row field.
// If a later lane adds an opaque action token, it must be computed
// from the verified caller's control tier (a runtime-side authority),
// never from the projection — this package's response shapes contain
// no token field by construction, and a pin test holds them that way.
//
// # Concurrent reuse (CLAUDE.md §5)
//
// A constructed *Service is immutable after NewService and safe to
// share across N concurrent goroutines: it holds only the Projector
// reference plus optional gate / bus / redactor / logger; every
// method's per-call state lives in the call's arguments and locals,
// never on the Service.
package protocol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// Sentinel errors the Service returns. The wire handler maps each onto
// a canonical Protocol Code + HTTP status; in-process callers compare
// with errors.Is.
var (
	// ErrIdentityRequired — the request context carried no verified
	// identity or an incomplete verified triple. CLAUDE.md §6 rule 9 —
	// fails closed.
	ErrIdentityRequired = errors.New("sessions/turns/protocol: verified identity missing or incomplete")
	// ErrInvalidRequest — the request was structurally invalid (empty
	// session_id / task_id, an out-of-range limit, an unknown
	// projection, or any get-only projection on the list method).
	ErrInvalidRequest = errors.New("sessions/turns/protocol: invalid request")
	// ErrTurnNotFound — `sessions.turns.get` / `sessions.turns.list`
	// targeted a turn or session with no row visible to the caller:
	// never created, evicted past the retention bound, erased, a
	// foreign (non-exact) session on the consumer lane, or a row whose
	// effective agent is outside the caller's reach. Deliberately
	// NON-ORACULAR: a foreign read is answered exactly like an absent
	// one (CLAUDE.md §13 — cross-identity denial never becomes an
	// existence oracle).
	ErrTurnNotFound = errors.New("sessions/turns/protocol: turn not found")
	// ErrOperationsScopeDenied — a `projection=operations` read
	// reached the Service without a verified `admin` or `console:fleet`
	// claim. The operations lane is the elevated admin/fleet
	// observation lane; an ordinary caller cannot select it.
	ErrOperationsScopeDenied = errors.New("sessions/turns/protocol: operations projection requires a verified admin or console:fleet claim")
	// ErrSessionReachDenied — the request's effective session is
	// outside the bearer's verified signed `session_reach` claim. Loud
	// by the settled signed-reach contract (an absent claim preserves
	// dynamic selection; a present claim that excludes the effective
	// session denies).
	ErrSessionReachDenied = errors.New("sessions/turns/protocol: session outside the caller's verified session reach")
	// ErrMisconfigured — NewService was called with a nil Projector.
	ErrMisconfigured = errors.New("sessions/turns/protocol: NewService missing a mandatory dependency")
)

// Projection selects the read lane of a turn read. The zero value
// (omitted) is the consumer conversation projection.
type Projection string

// Read projections.
const (
	// ProjectionConversation is the consumer-safe exact-session read —
	// the default. Serves `turns.TurnRow` (query / answer union /
	// reasoning / activity / usage / pause / App refs with per-component
	// availability). Supported by list AND get.
	ProjectionConversation Projection = "conversation"
	// ProjectionOperations is the elevated admin/fleet observation lane.
	// GET-ONLY: the projection core ships `OpsTurn`, not an ops list, so
	// `sessions.turns.list` rejects it. Serves the structurally distinct
	// `turns.OpsTurnRow` (no query / answer / reasoning / App URI /
	// tool_call_id / App context / pause tokens).
	ProjectionOperations Projection = "operations"
	// ProjectionUsage is the consumer-safe exact-session usage read. It
	// serves only the structurally distinct UsageTurnRow: lifecycle
	// identity and timing plus the canonical usage rollup (per-measure states
	// and an optional reported model).
	// It does not require, or admit, an elevated operations scope.
	ProjectionUsage Projection = "usage"
)

// Valid reports whether p is one of the declared projections.
func (p Projection) Valid() bool {
	return p == ProjectionConversation || p == ProjectionOperations || p == ProjectionUsage
}

// parseProjection resolves a request projection selector. An empty
// string is the default conversation projection; an unknown value
// fails loud with ErrInvalidRequest (never silently coerced).
func parseProjection(p Projection) (Projection, error) {
	if p == "" {
		return ProjectionConversation, nil
	}
	if !p.Valid() {
		return "", fmt.Errorf("%w: unknown projection %q", ErrInvalidRequest, p)
	}
	return p, nil
}

// Order declares the stable order of a returned page. The client never
// invents sort authority: the projection serves ONE order.
type Order string

// Declared page orders. The projection pages newest-first; the
// oldest-first value exists only to state the closed contract, it is
// never served.
const (
	OrderNewestFirst Order = "newest_first"
	OrderOldestFirst Order = "oldest_first"
)

// SessionHeader is the lightweight per-session header the list
// response carries: the owning session id, the projection snapshot id
// (as-of retention generation) the page — and its cursors — bind to,
// and the as-of instant the page was read.
type SessionHeader struct {
	// SessionID is the session the page belongs to (the caller's own
	// exact session on the consumer lane).
	SessionID string
	// SnapshotID is the projection snapshot generation the page binds
	// to; erasure advances it, and a cursor minted against an older
	// generation is rejected as stale.
	SnapshotID uint64
	// AsOf is the instant the page was read.
	AsOf time.Time
}

// ListRequest is the `sessions.turns.list` request adapter. Identity
// is verified from the request context (never a body field); the body
// carries the session id, the opaque older-page cursor, the bounded
// limit, and the projection selector.
type ListRequest struct {
	// SessionID is the effective session to page. On the consumer lane
	// it must be the caller's own exact session.
	SessionID string
	// OlderCursor is the opaque exclusive older-page cursor returned by
	// the previous page; empty means the newest page. Decoded via the
	// domain's cursor encoding; invalid / foreign / stale-snapshot /
	// retention-expired cursors surface as the domain's distinct typed
	// sentinels.
	OlderCursor string
	// Limit bounds the page: 0 means turns.DefaultListLimit (20);
	// values above turns.MaxListLimit (50) fail loud.
	Limit int
	// Projection selects the read lane. Only the conversation
	// projection is a list surface; every other projection is get-only.
	Projection Projection
}

// ListResponse is the `sessions.turns.list` response adapter. Every
// payload is a domain DTO (turns.TurnRow rows); the envelope carries
// the paging contract fields.
type ListResponse struct {
	// Header is the session header: session id, snapshot id, as-of.
	Header SessionHeader
	// Turns are the page's turns, newest first, already run through the
	// effective-agent gate (gated-out rows are not present).
	Turns []turns.TurnRow
	// Order is the declared stable order — always OrderNewestFirst.
	Order Order
	// NextOlderCursor is the opaque exclusive older-page cursor for the
	// next page; empty when HasMore is false. The client passes it back
	// verbatim as the next request's OlderCursor.
	NextOlderCursor string
	// HasMore reports whether older turns remain within the retained
	// window beyond this page.
	HasMore bool
	// RemainingOlderCount is the exact number of older RETAINED turns
	// beyond this page when the store knows it exactly and no row of
	// this page was gated by the effective-agent filter; nil otherwise
	// (unknown — never a fabricated count).
	RemainingOlderCount *int
	// CountExact reports whether RemainingOlderCount is exact.
	CountExact bool
	// LiveResumeSeq is the durable event-log sequence of the newest
	// observation reflected in this page — the exclusive live-resume
	// boundary for the page-before-subscribe handoff. The consumer
	// folds the durable page (establishing bounded membership) first,
	// then subscribes to the session's event stream from
	// LiveResumeSeq+1 — a gap-free page-to-live handoff: nothing the
	// page reflects is re-processed, and nothing applied after the
	// read is missed.
	LiveResumeSeq uint64
	// PageCompleteness is the explicit page completeness: complete, or
	// partial (retention eviction — older turns exist in the durable
	// event log but were evicted from this projection). Never a
	// fabricated empty.
	PageCompleteness turns.Completeness
	// PartialReason names why the page is partial ("retention_eviction");
	// empty when PageCompleteness is complete.
	PartialReason string
}

// GetRequest is the `sessions.turns.get` request adapter: one
// `(session, task)` read. Identity is verified from the request
// context; the body carries the session id, the task (turn) id, and
// the projection selector.
type GetRequest struct {
	// SessionID is the effective session. On the consumer lane it must
	// be the caller's own exact session; on the operations lane it is
	// any session of the caller's own (tenant, user).
	SessionID string
	// TaskID is the authoritative root foreground task id of the turn —
	// the turn row key.
	TaskID string
	// Projection selects the read lane: conversation (default),
	// operations (admin/fleet-gated), or usage (consumer exact-session).
	Projection Projection
}

// GetResponse is the `sessions.turns.get` response adapter. Exactly one
// of Turn / OpsTurn / UsageTurn is populated, per the request's projection:
// Turn for the consumer conversation lane, OpsTurn for the elevated
// operations lane, and UsageTurn for the consumer usage lane.
type GetResponse struct {
	// SessionID is the session the turn was read from.
	SessionID string
	// Turn is the consumer-safe turn row (ProjectionConversation).
	Turn turns.TurnRow
	// OpsTurn is the structurally distinct operations DTO
	// (ProjectionOperations) — no query / answer / reasoning / App URI
	// / tool_call_id / App context / pause tokens. Nil on the consumer
	// lane.
	OpsTurn *turns.OpsTurnRow
	// UsageTurn is the structurally distinct consumer usage DTO
	// (ProjectionUsage). It is nil unless the caller selected the exact-session
	// usage lane.
	UsageTurn *UsageTurnRow
}

// UsageTurnRow is the content-free, consumer-safe usage projection of one
// durable turn. It is structurally distinct from TurnRow: it exposes only
// stable turn/session/agent identifiers, lifecycle/timing state, and the
// canonical cumulative usage rollup. Query, answer, reasoning, activity,
// pause, applications, attachments, run identity, and all terminal messages
// cannot ride this DTO.
type UsageTurnRow struct {
	// TurnID is the row key.
	TurnID turns.TurnID
	// TaskID is the authoritative root foreground task id.
	TaskID string
	// SessionID is the owning session.
	SessionID string
	// AgentID is the effective agent identifier when available.
	AgentID string
	// Status / Sealed / Version describe the durable lifecycle snapshot.
	Status  turns.Status
	Sealed  bool
	Version int
	// LastAppliedEventSeq is the durable sequence of the latest observation
	// reflected in this snapshot.
	LastAppliedEventSeq uint64
	// StartedAt / UpdatedAt / FinishedAt are the lifecycle timing facts.
	StartedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt time.Time
	// Usage is the canonical cumulative per-measure token/cost/latency rollup.
	Usage turns.Usage
}

// Projector is the read seam the Service depends on (CLAUDE.md §4.4).
// The V1 production implementation is *turns.Projector — the domain
// core over a conformance-ready Store. Every method takes the verified
// identity triple so the implementation scopes its reads; the Service
// applies the authority matrix (session reach, exact-session boundary,
// effective-agent gate, operations scope) on top.
type Projector interface {
	// List serves one newest-first keyset page (see turns.Projector.List
	// for the full cursor / snapshot / completeness contract).
	List(ctx context.Context, id identity.Identity, opts turns.ListOptions) (turns.Page, error)
	// Get serves one consumer-safe row, or turns.ErrTurnNotFound.
	Get(ctx context.Context, id identity.Identity, turnID turns.TurnID) (turns.TurnRow, error)
	// OpsTurn serves the pure operations-safe READ DTO of one row, or
	// turns.ErrTurnNotFound. Structurally no transcript content.
	OpsTurn(ctx context.Context, id identity.Identity, turnID turns.TurnID) (turns.OpsTurnRow, error)
}

// Compile-time assertion: the domain core satisfies the read seam. The
// assertion lives here (the seam's home) — package turns cannot import
// this package without a cycle.
var _ Projector = (*turns.Projector)(nil)

// Service implements the `sessions.turns.*` service adapters over the
// turn projection. It is a safe-for-concurrent-reuse compiled artifact
// — immutable after NewService.
type Service struct {
	projector Projector
	// sessionReach is the canonical signed-session-reach gate: optional
	// — nil means the transport edge is the enforcement point and the
	// Service applies no session-reach check beyond the exact-session
	// identity boundary.
	sessionReach auth.SessionReachAuthorizer
	// agentReach is the canonical effective-agent gate: optional — nil
	// FAILS CLOSED, a turn carrying a non-empty effective agent id is
	// not served (never a silent widening).
	agentReach auth.AgentReachAuthorizer
	// bus is the canonical events.EventBus the Service publishes the
	// widened-operations `audit.admin_scope_used` event onto; nil ⇒ the
	// widened read is logged at Info instead (never silent).
	bus events.EventBus
	// redactor is the audit.Redactor the Service runs the audit payload
	// through before publishing — defence-in-depth, never skipped on a
	// redaction error (CLAUDE.md §7 rule 6).
	redactor audit.Redactor
	logger   *slog.Logger
}

// Option configures NewService.
type Option func(*Service)

// WithSessionReachAuthorizer wires the canonical signed-session-reach
// gate. When wired, a request whose effective session falls outside a
// PRESENT `session_reach` claim is denied loudly; an absent claim
// preserves dynamic selection (the gate itself encodes that
// distinction). When unsupplied (or nil), the Service applies no
// session-reach check beyond the exact-session identity boundary — the
// transport edge is the enforcement point.
func WithSessionReachAuthorizer(a auth.SessionReachAuthorizer) Option {
	return func(s *Service) {
		if a != nil {
			s.sessionReach = a
		}
	}
}

// WithAgentReachAuthorizer wires the canonical effective-agent gate.
// When wired, a turn whose row carries a non-empty effective agent id
// is served only to a caller whose verified `agent_reach` contains it.
// When unsupplied (or nil), the Service FAILS CLOSED: named-agent
// turns are not served (the runtime MUST wire the gate for
// agent-bound transcripts to be readable — an unwired gate is an
// honest "cannot verify reach", never a silent widening).
func WithAgentReachAuthorizer(a auth.AgentReachAuthorizer) Option {
	return func(s *Service) {
		if a != nil {
			s.agentReach = a
		}
	}
}

// WithBus wires the canonical events.EventBus the Service publishes
// the widened-operations `audit.admin_scope_used` event onto. A nil
// bus is treated as "WithBus not supplied" — the widened read still
// works, but the audit observation is logged at Info instead of
// published (the admin action is NEVER fully silent — CLAUDE.md §13).
func WithBus(b events.EventBus) Option {
	return func(s *Service) {
		if b != nil {
			s.bus = b
		}
	}
}

// WithRedactor wires the audit.Redactor the Service runs the audit
// payload through before publishing. A nil redactor is treated as
// "WithRedactor not supplied".
func WithRedactor(r audit.Redactor) Option {
	return func(s *Service) {
		if r != nil {
			s.redactor = r
		}
	}
}

// WithLogger sets the slog.Logger the Service logs widened reads and
// audit-emit failures to. A nil logger routes to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewService builds the turns Protocol service adapter over a
// Projector. The projector is mandatory — a nil fails loud with
// ErrMisconfigured rather than building a Service that would nil-panic
// on the first request (CLAUDE.md §5). The returned *Service is
// immutable after construction and safe for concurrent use by N
// goroutines.
func NewService(projector Projector, opts ...Option) (*Service, error) {
	if projector == nil {
		return nil, fmt.Errorf("%w: Projector is nil", ErrMisconfigured)
	}
	s := &Service{projector: projector, logger: slog.Default()}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// verifiedIdentity reads the VERIFIED identity from the request
// context and fails closed on a missing or incomplete triple
// (CLAUDE.md §6 rule 9). The Service never reads identity from the
// body or from package-level state.
func (s *Service) verifiedIdentity(ctx context.Context) (identity.Identity, error) {
	id, ok := identity.FromVerified(ctx)
	if !ok {
		return identity.Identity{}, ErrIdentityRequired
	}
	if err := identity.Validate(id); err != nil {
		return identity.Identity{}, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	return id, nil
}

// authorizeSessionReach applies the signed-session-reach gate to the
// effective session when the Service is wired with one. A denial is
// loud (ErrSessionReachDenied — the settled signed-reach contract);
// an unwired gate passes (the transport edge is the enforcement point)
// and the exact-session identity boundary still holds below.
func (s *Service) authorizeSessionReach(ctx context.Context, sessionID string) error {
	if s.sessionReach == nil {
		return nil
	}
	if err := s.sessionReach.AuthorizeSessionReach(ctx, sessionID); err != nil {
		return fmt.Errorf("%w: %w", ErrSessionReachDenied, err)
	}
	return nil
}

// operationsScoped reports whether the request context carries the
// closed admin/fleet two-scope set — the admit surface of the
// elevated operations lane.
func (s *Service) operationsScoped(ctx context.Context) bool {
	return auth.HasScope(ctx, auth.ScopeAdmin) || auth.HasScope(ctx, auth.ScopeConsoleFleet)
}

// rowServed reports whether the caller's verified authority admits one
// served row — the EFFECTIVE-AGENT GATE. A row with no effective agent
// binding (empty Agent.ID) is served; a named-agent row requires the
// caller's signed agent reach to contain that agent. An unwired gate
// fails closed. The gate never reads identity or authority from the
// row beyond the agent id itself.
func (s *Service) rowServed(ctx context.Context, row turns.TurnRow) bool {
	if strings.TrimSpace(row.Agent.ID) == "" {
		return true
	}
	if s.agentReach == nil {
		return false
	}
	return s.agentReach.AuthorizeAgentReach(ctx, row.Agent.ID) == nil
}

// List implements the `sessions.turns.list` service adapter: one
// newest-first keyset page of the caller's exact session, bounded to
// [1, turns.MaxListLimit] with the turns.DefaultListLimit default,
// served entirely from the projection (never a raw history / task
// fallback).
func (s *Service) List(ctx context.Context, req ListRequest) (ListResponse, error) {
	id, err := s.verifiedIdentity(ctx)
	if err != nil {
		return ListResponse{}, err
	}
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		return ListResponse{}, fmt.Errorf("%w: session_id is empty", ErrInvalidRequest)
	}
	proj, err := parseProjection(req.Projection)
	if err != nil {
		return ListResponse{}, err
	}
	if proj != ProjectionConversation {
		return ListResponse{}, fmt.Errorf("%w: %q is a get-only projection — sessions.turns.list serves the conversation projection only", ErrInvalidRequest, proj)
	}

	// Session reach: the effective session must satisfy a present
	// signed session_reach claim (loud denial), then the exact-session
	// identity boundary (non-oracular not-found).
	if err := s.authorizeSessionReach(ctx, sessionID); err != nil {
		return ListResponse{}, err
	}
	if sessionID != id.SessionID {
		// Non-oracular: a foreign-session list answers exactly like a
		// session with no projection — typed not-found. Cross-identity
		// denial never becomes an existence oracle (CLAUDE.md §13).
		return ListResponse{}, fmt.Errorf("%w: session %q is not the caller's exact session — consumer reads are exact-session and foreign reads are not observable",
			ErrTurnNotFound, sessionID)
	}

	// Bounded page: 0 ⇒ the Protocol-mandated default (20); above the
	// maximum (50) fails loud.
	limit := req.Limit
	if limit == 0 {
		limit = turns.DefaultListLimit
	}
	if limit < 0 || limit > turns.MaxListLimit {
		return ListResponse{}, fmt.Errorf("%w: limit %d outside [1,%d]", ErrInvalidRequest, limit, turns.MaxListLimit)
	}

	// Opaque cursor: decoded by the domain's own encoding. A malformed
	// cursor fails loud (ErrInvalidCursor); the session/snapshot/
	// boundary-row BINDING is enforced by the store at list time and
	// surfaces as its distinct typed sentinels (foreign-session /
	// stale-snapshot / retention-expired / forged).
	var before *turns.Cursor
	if req.OlderCursor != "" {
		before, err = turns.DecodeCursor(req.OlderCursor)
		if err != nil {
			return ListResponse{}, fmt.Errorf("sessions/turns/protocol: list: %w", err)
		}
	}

	// ONE projection read per list call — the two-read chat open needs
	// exactly one turn-page read; there are no per-row reads and no
	// fallback seams.
	page, err := s.projector.List(ctx, id, turns.ListOptions{Before: before, Limit: limit})
	if err != nil {
		return ListResponse{}, fmt.Errorf("sessions/turns/protocol: list: %w", err)
	}

	// Effective-agent gate: gated rows are filtered out of the page.
	// When any row was gated, the exact remaining count degrades to
	// unknown — the count must not leak rows the caller may not
	// observe.
	gated := make([]turns.TurnRow, 0, len(page.Rows))
	for _, row := range page.Rows {
		if s.rowServed(ctx, row) {
			gated = append(gated, row)
		}
	}
	gatedAny := len(gated) != len(page.Rows)

	var remaining *int
	countExact := page.CountExact && !gatedAny
	if countExact {
		v := page.Remaining
		remaining = &v
	}

	completeness := turns.CompletenessComplete
	partialReason := ""
	if !page.Complete {
		completeness = turns.CompletenessPartial
		partialReason = page.PartialReason
	}

	var nextCursor string
	if page.NextCursor != nil {
		nextCursor = page.NextCursor.Encode()
	}

	return ListResponse{
		Header: SessionHeader{
			SessionID:  sessionID,
			SnapshotID: page.Snapshot,
			AsOf:       page.AsOf,
		},
		Turns:               gated,
		Order:               OrderNewestFirst,
		NextOlderCursor:     nextCursor,
		HasMore:             page.HasMore,
		RemainingOlderCount: remaining,
		CountExact:          countExact,
		LiveResumeSeq:       page.LiveResumeSeq,
		PageCompleteness:    completeness,
		PartialReason:       partialReason,
	}, nil
}

// Get implements the `sessions.turns.get` service adapter: one
// (session, task) read, either the consumer-safe turn row (the bounded
// terminal reconciliation read after live streaming) or — under a
// verified admin / console:fleet claim — the structurally distinct
// operations DTO. Every read flows from the projection; there is no
// raw history / task fallback.
func (s *Service) Get(ctx context.Context, req GetRequest) (GetResponse, error) {
	id, err := s.verifiedIdentity(ctx)
	if err != nil {
		return GetResponse{}, err
	}
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		return GetResponse{}, fmt.Errorf("%w: session_id is empty", ErrInvalidRequest)
	}
	taskID := strings.TrimSpace(req.TaskID)
	if taskID == "" {
		return GetResponse{}, fmt.Errorf("%w: task_id is empty", ErrInvalidRequest)
	}
	proj, err := parseProjection(req.Projection)
	if err != nil {
		return GetResponse{}, err
	}

	// Session reach: the effective session must satisfy a present
	// signed session_reach claim (loud denial), on both lanes.
	if err := s.authorizeSessionReach(ctx, sessionID); err != nil {
		return GetResponse{}, err
	}

	turnID := turns.TurnID(taskID)

	if proj == ProjectionOperations {
		// The elevated admin/fleet observation lane. The closed
		// two-scope set is the admit surface; an ordinary caller cannot
		// select it.
		if !s.operationsScoped(ctx) {
			return GetResponse{}, ErrOperationsScopeDenied
		}
		// The lane reads any session of the caller's own (tenant, user).
		// No cross-principal transcript read exists anywhere (no
		// content-read / impersonation authority).
		target := identity.Identity{TenantID: id.TenantID, UserID: id.UserID, SessionID: sessionID}
		ops, err := s.projector.OpsTurn(ctx, target, turnID)
		if err != nil {
			if errors.Is(err, turns.ErrTurnNotFound) {
				return GetResponse{}, fmt.Errorf("%w: turn %q under session %q", ErrTurnNotFound, taskID, sessionID)
			}
			return GetResponse{}, fmt.Errorf("sessions/turns/protocol: get (operations): %w", err)
		}
		// A read that leaves the caller's own session is a WIDENED read
		// — audit it (never silent). The payload names the verified
		// actor and the read target.
		if target != id {
			s.emitAdminAudit(ctx, id, target)
		}
		return GetResponse{SessionID: sessionID, OpsTurn: &ops}, nil
	}

	// Consumer exact-session lane: the requested session MUST be the
	// caller's own verified session; anything else is non-oracular
	// typed not-found.
	if sessionID != id.SessionID {
		return GetResponse{}, fmt.Errorf("%w: session %q is not the caller's exact session — consumer reads are exact-session and foreign reads are not observable",
			ErrTurnNotFound, sessionID)
	}

	row, err := s.projector.Get(ctx, id, turnID)
	if err != nil {
		if errors.Is(err, turns.ErrTurnNotFound) {
			return GetResponse{}, fmt.Errorf("%w: turn %q under session %q", ErrTurnNotFound, taskID, sessionID)
		}
		return GetResponse{}, fmt.Errorf("sessions/turns/protocol: get: %w", err)
	}
	// Effective-agent gate: a named-agent turn outside the caller's
	// reach answers exactly like an absent turn — non-oracular.
	if !s.rowServed(ctx, row) {
		return GetResponse{}, fmt.Errorf("%w: turn %q under session %q", ErrTurnNotFound, taskID, sessionID)
	}
	if proj == ProjectionUsage {
		usage := UsageTurnRow{
			TurnID:              row.TurnID,
			TaskID:              row.TaskID,
			SessionID:           row.SessionID,
			AgentID:             row.Agent.ID,
			Status:              row.Status,
			Sealed:              row.Sealed,
			Version:             row.Version,
			LastAppliedEventSeq: row.LastAppliedEventSeq,
			StartedAt:           row.StartedAt,
			UpdatedAt:           row.UpdatedAt,
			FinishedAt:          row.FinishedAt,
			Usage:               row.Usage,
		}
		return GetResponse{SessionID: sessionID, UsageTurn: &usage}, nil
	}
	// The pause component passes through unmodified; actionability /
	// tokens are never derived from the projection (see the package
	// doc).
	return GetResponse{SessionID: sessionID, Turn: row}, nil
}
