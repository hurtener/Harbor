// Package protocol implements the `sessions.*` Protocol methods the
// Console Sessions page consumes:
//
//   - sessions.list      — paginated, filtered SessionRegistry projection.
//   - sessions.inspect   — full per-session snapshot for the detail view.
//   - sessions.delete    — own-session-only data-lifecycle erasure.
//   - sessions.set_title — sets/clears a session's human-readable title.
//
// # The seam (CLAUDE.md §4.4)
//
// The Service depends on the `Projector` interface, not on a concrete
// session registry. The V1 production implementation is
// `ListerProjector` (lister_projector.go) — a thin read-only projection
// over a `sessions.SessionLister`. A future remote / cross-runtime
// projector slots in behind the same interface without reshaping the
// Service.
//
// # Identity is mandatory (CLAUDE.md §6 rule 9)
//
// Every method takes the wire request's `IdentityScope`. An incomplete
// triple fails closed with `ErrIdentityRequired` — there is no
// identity-downgrading knob. The Service NEVER reads identity from a
// package-level global; the triple flows in via the request.
//
// # Cross-tenant gating
//
// A `sessions.list` whose `Filter.TenantIDs` names a tenant other than
// the caller's verified tenant requires the verified `auth.ScopeAdmin`
// claim. The Service receives an `adminScoped bool` the wire handler
// computes from the verified JWT scope set; a false value on a
// cross-tenant filter fails closed with `ErrCrossTenantScope`. There is
// NO `sessions.admin` scope — the closed two-scope set (`admin` +
// `console:fleet`) is the only admit surface. On a successful
// admin-scope query the Service emits an `audit.admin_scope_used`
// event.
//
// # Concurrent reuse
//
// A constructed *Service is immutable after NewService and safe to
// share across N concurrent goroutines: it holds only the Projector
// reference + an optional bus + redactor + logger; every method's
// per-call state lives in the call's arguments and locals, never on the
// Service.
package protocol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
)

// Sentinel errors the Service returns. The wire handler maps each onto
// a canonical Protocol Code + HTTP status; in-process callers compare
// with errors.Is.
var (
	// ErrIdentityRequired — the request carried an incomplete identity
	// triple. RFC §5.5 / CLAUDE.md §6 rule 9 — fails closed.
	ErrIdentityRequired = errors.New("sessions/protocol: identity scope incomplete")
	// ErrCrossTenantScope — a `sessions.list` filter named a tenant
	// outside the caller's verified tenant without the verified
	// `auth.ScopeAdmin` claim.
	ErrCrossTenantScope = errors.New("sessions/protocol: cross-tenant filter requires the admin scope claim")
	// ErrInvalidRequest — the request was structurally invalid (an
	// out-of-range limit, an unknown enum, a malformed cursor).
	ErrInvalidRequest = errors.New("sessions/protocol: invalid request")
	// ErrSessionNotFound — `sessions.inspect` / `sessions.delete` targeted
	// a session id with no record visible to the caller's identity scope.
	ErrSessionNotFound = errors.New("sessions/protocol: session not found")
	// ErrSessionRunning — `sessions.delete` was refused because the target
	// session has a RUNNING task (mirroring the GC never-reap-running
	// invariant). The handler maps it to CodeSessionRunning (409). No
	// store is touched on refusal.
	ErrSessionRunning = errors.New("sessions/protocol: cannot erase a session with a running task")
	// ErrErasureUnsupported — `sessions.delete` reached a Service that was
	// built without an Eraser (the runtime does not advertise the
	// CapSessionLifecycle capability). The handler maps it to
	// CodeUnknownMethod (404) so a client that probed the route detects
	// the unwired surface exactly as it would a missing route.
	ErrErasureUnsupported = errors.New("sessions/protocol: session erasure is not wired on this runtime")
	// ErrErasureRecordFailed — the erasure cascade's destructive steps
	// completed but the durable `session.erased` record-of-fact could not
	// be completed (a redactor refusal or a bus-publish failure).
	// Delete fails the whole call loud rather than reporting success with
	// a missing audit trail; the session's data IS gone, and a re-invoke
	// converges (it re-attempts only the record, no destructive step
	// re-runs). The handler maps it to CodeRuntimeError (500).
	ErrErasureRecordFailed = errors.New("sessions/protocol: erasure record-of-fact could not be durably completed")
	// ErrMisconfigured — NewService was called with a nil Projector.
	ErrMisconfigured = errors.New("sessions/protocol: NewService missing a mandatory dependency")
	// ErrTitleSetUnsupported — `sessions.set_title` reached a Service that
	// was built without a TitleSetter. The handler maps it to
	// CodeUnknownMethod (404), the same posture as ErrErasureUnsupported.
	ErrTitleSetUnsupported = errors.New("sessions/protocol: session title-set is not wired on this runtime")
)

// Eraser is the seam the Service depends on for `sessions.delete`. The
// V1 production implementation is the in-runtime cascade orchestrator
// (`sessions.CascadeEraser`), which performs the real three-store
// cascade + session-record delete for the verified identity, refusing
// fail-loud on a running task. The Service depends ONLY on this
// interface (CLAUDE.md §4.4) — a future remote / cross-runtime eraser
// slots in behind it without reshaping the Service.
type Eraser interface {
	// Erase performs the full erasure cascade for the verified identity
	// (own-session-only — the scope contract is enforced at the Service
	// edge before Erase is reached). Returns the deletion telemetry, or a
	// refusal: sessions.ErrSessionRunning (a RUNNING task) /
	// sessions.ErrSessionNotFound (absent under the caller's identity).
	Erase(ctx context.Context, id identity.Identity) (prototypes.SessionsDeleteResponse, error)
}

// TitleSetter is the write seam the Service dispatches `sessions.set_title`
// to. The V1 production implementation is `*sessions.Registry` — its
// SetTitle method satisfies this interface directly (Registry already
// implements the wider SessionRegistry, of which SetTitle is one
// method). The Service depends ONLY on this narrow interface (CLAUDE.md
// §4.4) — a future remote / cross-runtime title-set slots in behind it
// without reshaping the Service.
type TitleSetter interface {
	// SetTitle sets or clears the title of session `id` for the CALLER's
	// verified `(tenant, user)` (`ident`). See sessions.SessionRegistry's
	// SetTitle for the full semantics (trim, validate, empty clears,
	// (tenant, user) write scope). Returns sessions.ErrInvalidTitle,
	// sessions.ErrSessionNotFound, or sessions.ErrIdentityMismatch on
	// refusal.
	SetTitle(ctx context.Context, id string, ident identity.Identity, title string) error
}

// Projector is the read seam the Service depends on. The V1 production
// implementation is ListerProjector. Every method takes the verified
// identity triple plus the resolved admin-scope flag so the
// implementation scopes its reads.
type Projector interface {
	// ListSessions returns every session row visible to the caller,
	// already identity-scoped: when adminScoped is false the
	// implementation MUST restrict to the caller's own (tenant, user);
	// when true it MAY honour a cross-tenant TenantIDs filter. The
	// Service applies the facet filter + sort + pagination on top.
	ListSessions(ctx context.Context, id identity.Identity, f prototypes.SessionFilter, adminScoped bool) ([]prototypes.SessionRow, error)
	// InspectSession returns the full snapshot for sessionID, or
	// ErrSessionNotFound. adminScoped widens the lookup across tenants.
	InspectSession(ctx context.Context, id identity.Identity, sessionID string, adminScoped bool) (prototypes.SessionsInspectResponse, error)
	// CountersAvailable reports whether the projector populates the
	// numeric / boolean counters (cost / tokens / tasks / events /
	// intervention / failed-task). False when no counter Enricher is wired:
	// the Service then loud-rejects a facet / sort over those counters
	// rather than returning a false-empty page (WARN-3).
	CountersAvailable() bool
}

// preEnrichmentPager is an internal fast path for projectors that can apply
// lifecycle-only filters, sort, and cursor pagination before counter
// enrichment. The Service selects it only when the request has no
// counter-dependent filter or sort, so it preserves the general ListSessions
// filter, ordering, cursor, and truncation semantics while avoiding reads for
// rows outside the requested page.
//
// It is deliberately private: Projector remains the complete read seam and a
// future projector that does not implement this optimization retains the
// general, semantics-preserving path.
type preEnrichmentPager interface {
	pageBeforeEnrichment(
		ctx context.Context,
		id identity.Identity,
		f prototypes.SessionFilter,
		adminScoped bool,
		srt prototypes.SessionSort,
		cursor *pageCursor,
		limit int,
	) (prototypes.SessionsListResponse, error)
}

// requiresCounters reports whether the request's filter or sort operates
// over the numeric / boolean SessionRow counters — the axes that are
// permanently zero on a projector with no Enricher wired. When true and the
// projector reports CountersAvailable()==false, the Service loud-rejects
// rather than returning a false-empty / mis-ordered page (WARN-3).
func requiresCounters(f prototypes.SessionFilter, srt prototypes.SessionSort) bool {
	return f.CostAboveCents != nil ||
		f.HasFailedTask != nil ||
		f.HasIntervention != nil ||
		srt == prototypes.SessionSortCostDesc
}

// Service implements the `sessions.*` Protocol methods. It is a
// safe for concurrent reuse compiled artifact — immutable after NewService.
type Service struct {
	projector   Projector
	eraser      Eraser          // optional — nil ⇒ sessions.delete is unsupported (capability not wired)
	titleSetter TitleSetter     // optional — nil ⇒ sessions.set_title is unsupported (capability not wired)
	bus         events.EventBus // optional — nil ⇒ admin audit emit is logged only
	redactor    audit.Redactor  // optional — defence-in-depth before the emit
	logger      *slog.Logger
}

// Option configures NewService.
type Option func(*Service)

// WithBus wires the canonical events.EventBus the Service publishes the
// `audit.admin_scope_used` event onto when an admin-scope query
// succeeds. A nil bus is treated as "WithBus not supplied" — the admin
// path still works, but the audit observation is logged at Info instead
// of published (the admin action is NEVER fully silent — CLAUDE.md §13).
func WithBus(b events.EventBus) Option {
	return func(s *Service) {
		if b != nil {
			s.bus = b
		}
	}
}

// WithRedactor wires the audit.Redactor the Service runs the
// `audit.admin_scope_used` payload through before publishing. A nil
// redactor is treated as "WithRedactor not supplied".
func WithRedactor(r audit.Redactor) Option {
	return func(s *Service) {
		if r != nil {
			s.redactor = r
		}
	}
}

// WithEraser wires the Eraser the Service dispatches `sessions.delete`
// to. When unsupplied (or nil) the Service answers `sessions.delete` with
// ErrErasureUnsupported (the handler maps it to a 404) and the runtime
// does NOT advertise the CapSessionLifecycle capability — capability
// gating: a runtime that did not wire an eraser is honestly read-only on
// the Sessions surface. A non-nil eraser enables the erasure path.
func WithEraser(e Eraser) Option {
	return func(s *Service) {
		if e != nil {
			s.eraser = e
		}
	}
}

// WithTitleSetter wires the TitleSetter the Service dispatches
// `sessions.set_title` to. When unsupplied (or nil) the Service answers
// `sessions.set_title` with ErrTitleSetUnsupported (the handler maps it
// to a 404) — a runtime that did not wire a title setter is honestly
// read-only on session titles. A non-nil setter enables the write path.
func WithTitleSetter(ts TitleSetter) Option {
	return func(s *Service) {
		if ts != nil {
			s.titleSetter = ts
		}
	}
}

// WithLogger sets the slog.Logger the Service logs admin actions and
// audit-emit failures to. A nil logger routes to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewService builds the Sessions Protocol service over a Projector. The
// projector is mandatory — a nil fails loud with ErrMisconfigured
// rather than building a Service that would nil-panic on the first
// request (CLAUDE.md §5). The returned *Service is immutable after
// construction and safe for concurrent use by N goroutines.
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

// validIdentity validates the wire IdentityScope into an
// identity.Identity, failing closed on an incomplete triple.
func validIdentity(scope prototypes.IdentityScope) (identity.Identity, error) {
	id := identity.Identity{
		TenantID:  scope.Tenant,
		UserID:    scope.User,
		SessionID: scope.Session,
	}
	if err := identity.Validate(id); err != nil {
		return identity.Identity{}, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	return id, nil
}

// isCrossTenant reports whether the filter names a tenant other than
// the caller's verified tenant — the predicate that gates the
// admin-scope requirement.
func isCrossTenant(callerTenant string, f prototypes.SessionFilter) bool {
	for _, t := range f.TenantIDs {
		if t != "" && t != callerTenant {
			return true
		}
	}
	return false
}

// List implements the `sessions.list` method. It validates identity,
// enforces the cross-tenant gate, resolves the identity-scoped
// rows from the Projector, applies the facet filter + sort + cursor
// pagination, and emits an `audit.admin_scope_used` event on a
// successful admin-scope query.
func (s *Service) List(ctx context.Context, req prototypes.SessionsListRequest, adminScoped bool) (prototypes.SessionsListResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.SessionsListResponse{}, err
	}

	crossTenant := isCrossTenant(id.TenantID, req.Filter)
	if crossTenant && !adminScoped {
		return prototypes.SessionsListResponse{},
			fmt.Errorf("%w: sessions.list filter names a tenant outside %q", ErrCrossTenantScope, id.TenantID)
	}

	// filter.agent_ids is the ONLY facet that keys SOLELY on an unpopulated
	// agent field. There is no single-valued session→agent binding in V1
	// (SessionRow.AgentID is nil), so honouring it would return a silent
	// empty page on a fleet full of sessions — the false-absence defect this
	// phase closes. Fail LOUD (WARN-4). The multi-field Query axis is NOT
	// rejected here: it still matches the populated session_id / user_id
	// (filter.go) and honestly never-matches the nil agent fields — an
	// over-rejection of the whole query would break working id/user search,
	// the inverse of a lying control and equally wrong.
	if len(req.Filter.AgentIDs) > 0 {
		return prototypes.SessionsListResponse{},
			fmt.Errorf("%w: filter.agent_ids operates over an unpopulated session→agent binding "+
				"(no single-valued agent is bound to a session in V1) — this facet cannot be honoured", ErrInvalidRequest)
	}

	limit := req.Limit
	if limit == 0 {
		limit = prototypes.DefaultSessionListLimit
	}
	if limit < 0 || limit > prototypes.MaxSessionListLimit {
		return prototypes.SessionsListResponse{},
			fmt.Errorf("%w: limit %d outside [1,%d]", ErrInvalidRequest, limit, prototypes.MaxSessionListLimit)
	}

	srt := req.Sort
	if srt == "" {
		srt = prototypes.SessionSortStartedDesc
	}
	if !prototypes.IsValidSessionSort(srt) {
		return prototypes.SessionsListResponse{},
			fmt.Errorf("%w: unknown sort %q", ErrInvalidRequest, srt)
	}

	// WARN-3: sessions runs SERVER-SIDE facets / sort over the counters
	// (unlike tasks, which does not). On a projector with no Enricher wired
	// the counters are permanently zero, so a numeric-counter facet /
	// cost_desc sort would reproduce the original false-absence defect
	// (cost_above_cents excludes every row; cost_desc degrades to the id
	// tiebreak). Loud-reject rather than return a believable-but-false page.
	// Production ALWAYS wires the enricher (internal/runtime/serve mux
	// assembly), so this gate fires only on a partial / headless build.
	if requiresCounters(req.Filter, srt) && !s.projector.CountersAvailable() {
		return prototypes.SessionsListResponse{},
			fmt.Errorf("%w: cost_above_cents / has_failed_task / has_intervention facets and the "+
				"cost_desc sort require the session-counter enricher, which is not wired on this runtime", ErrInvalidRequest)
	}

	cursor, err := decodeCursor(req.Cursor)
	if err != nil {
		return prototypes.SessionsListResponse{}, err
	}
	// Lifecycle-only requests can be filtered, sorted, and cursor-paged from
	// the registry projection before the expensive per-row counter reads. The
	// non-counter condition is load-bearing: a counter filter or cost sort must
	// retain the general path because enrichment supplies its predicate/key.
	if !requiresCounters(req.Filter, srt) {
		if pager, ok := s.projector.(preEnrichmentPager); ok {
			resp, pageErr := pager.pageBeforeEnrichment(ctx, id, req.Filter, adminScoped, srt, cursor, limit)
			if pageErr != nil {
				return prototypes.SessionsListResponse{}, fmt.Errorf("sessions/protocol: list: %w", pageErr)
			}
			if crossTenant && adminScoped {
				s.emitAdminAudit(ctx, id, "sessions.list")
			}
			return resp, nil
		}
	}

	rows, err := s.projector.ListSessions(ctx, id, req.Filter, adminScoped)
	if err != nil {
		return prototypes.SessionsListResponse{}, fmt.Errorf("sessions/protocol: list: %w", err)
	}

	// Apply the facet filter post-projection (the projector applies the
	// identity-scope predicate; the Service applies the facet axes).
	filtered := make([]prototypes.SessionRow, 0, len(rows))
	for _, r := range rows {
		if filterMatches(req.Filter, r) {
			filtered = append(filtered, r)
		}
	}
	sortRows(filtered, srt)

	start, end, truncated, next := pageBounds(filtered, cursor, srt, limit)
	page := filtered[start:end]

	if crossTenant && adminScoped {
		s.emitAdminAudit(ctx, id, "sessions.list")
	}

	return prototypes.SessionsListResponse{
		Rows:       page,
		NextCursor: next,
		Truncated:  truncated,
	}, nil
}

// Inspect implements the `sessions.inspect` method — the full
// per-session snapshot the Console Sessions detail view renders.
func (s *Service) Inspect(ctx context.Context, req prototypes.SessionsInspectRequest, adminScoped bool) (prototypes.SessionsInspectResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.SessionsInspectResponse{}, err
	}
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		return prototypes.SessionsInspectResponse{},
			fmt.Errorf("%w: session_id is empty", ErrInvalidRequest)
	}
	resp, err := s.projector.InspectSession(ctx, id, sessionID, adminScoped)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return prototypes.SessionsInspectResponse{}, err
		}
		return prototypes.SessionsInspectResponse{}, fmt.Errorf("sessions/protocol: inspect: %w", err)
	}
	// Cross-tenant inspect (the resolved row's tenant differs from the
	// caller's verified tenant) is an admin action — audit it.
	if resp.Row.TenantID != "" && resp.Row.TenantID != id.TenantID {
		if !adminScoped {
			return prototypes.SessionsInspectResponse{},
				fmt.Errorf("%w: sessions.inspect crossed tenant %q", ErrCrossTenantScope, resp.Row.TenantID)
		}
		s.emitAdminAudit(ctx, id, "sessions.inspect")
	}
	// Defensive caps — never ship more than the documented limits.
	if len(resp.RecentInterventions) > prototypes.MaxSessionInterventionSummaries {
		resp.RecentInterventions = resp.RecentInterventions[:prototypes.MaxSessionInterventionSummaries]
	}
	if len(resp.RecentArtifacts) > prototypes.MaxSessionArtifactSummaries {
		resp.RecentArtifacts = resp.RecentArtifacts[:prototypes.MaxSessionArtifactSummaries]
	}
	return resp, nil
}

// Compile-time assertion: the V1 production cascade orchestrator
// (sessions.CascadeEraser) satisfies the Eraser seam the Service
// dispatches `sessions.delete` to. The assertion lives here (the seam's
// home) rather than in package sessions, which cannot import this package
// without a cycle.
var _ Eraser = (*sessions.CascadeEraser)(nil)

// HasEraser reports whether the Service was wired with an Eraser — i.e.
// whether `sessions.delete` is supported and the runtime should advertise
// the CapSessionLifecycle capability. The wiring layer uses it to gate
// the capability advertisement so a read-only runtime stays honest.
func (s *Service) HasEraser() bool { return s.eraser != nil }

// Compile-time assertion: *sessions.Registry (the V1 production
// SessionRegistry, which SetTitle is one method of) satisfies the
// TitleSetter seam the Service dispatches `sessions.set_title` to. The
// assertion lives here (the seam's home) rather than in package
// sessions, which cannot import this package without a cycle.
var _ TitleSetter = (*sessions.Registry)(nil)

// HasTitleSetter reports whether the Service was wired with a
// TitleSetter — i.e. whether `sessions.set_title` is supported. The
// wiring layer can use it exactly like HasEraser to gate any future
// capability advertisement.
func (s *Service) HasTitleSetter() bool { return s.titleSetter != nil }

// Delete implements the `sessions.delete` method — the identity-scoped,
// own-session-only data-lifecycle erasure of a whole session and its
// scoped State, Memory, and Artifacts.
//
// The scope contract is own-session-only: the request identity IS the
// caller's verified identity (the wire handler overlays the verified
// triple and rejects any body-identity mismatch as identity_required
// before this method is reached), so there is no admin / cross-tenant
// path. A nil eraser (the capability was not wired) is reported as
// ErrErasureUnsupported. A refusal on a RUNNING task surfaces as
// ErrSessionRunning (409); an absent session as ErrSessionNotFound (404).
func (s *Service) Delete(ctx context.Context, req prototypes.SessionsDeleteRequest) (prototypes.SessionsDeleteResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.SessionsDeleteResponse{}, err
	}
	if s.eraser == nil {
		return prototypes.SessionsDeleteResponse{}, ErrErasureUnsupported
	}
	resp, err := s.eraser.Erase(ctx, id)
	if err != nil {
		switch {
		case errors.Is(err, sessions.ErrSessionRunning):
			return prototypes.SessionsDeleteResponse{},
				fmt.Errorf("%w: %w", ErrSessionRunning, err)
		case errors.Is(err, sessions.ErrSessionNotFound):
			return prototypes.SessionsDeleteResponse{},
				fmt.Errorf("%w: %w", ErrSessionNotFound, err)
		case errors.Is(err, sessions.ErrIdentityMismatch):
			return prototypes.SessionsDeleteResponse{},
				fmt.Errorf("%w: %w", ErrIdentityRequired, err)
		case errors.Is(err, sessions.ErrErasureRecordFailed):
			return prototypes.SessionsDeleteResponse{},
				fmt.Errorf("%w: %w", ErrErasureRecordFailed, err)
		default:
			return prototypes.SessionsDeleteResponse{},
				fmt.Errorf("sessions/protocol: delete: %w", err)
		}
	}
	return resp, nil
}

// SetTitle implements the `sessions.set_title` method — sets or clears a
// session's human-readable title.
//
// The write scope is the owning `(tenant, user)`: req.Identity's
// tenant/user MUST equal the caller's verified identity (the wire
// handler overlays the verified triple and rejects any body-identity
// mismatch as identity_required before this method is reached, mirroring
// `sessions.delete`). req.SessionID is a DEDICATED field and MAY name a
// sibling session of the caller's own `(tenant, user)` — the SessionID
// component of req.Identity itself is unused for targeting (only its
// tenant/user matter). A nil TitleSetter (the capability was not wired)
// is reported as ErrTitleSetUnsupported.
func (s *Service) SetTitle(ctx context.Context, req prototypes.SessionsSetTitleRequest) (prototypes.SessionsSetTitleResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.SessionsSetTitleResponse{}, err
	}
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		return prototypes.SessionsSetTitleResponse{},
			fmt.Errorf("%w: session_id is empty", ErrInvalidRequest)
	}
	if s.titleSetter == nil {
		return prototypes.SessionsSetTitleResponse{}, ErrTitleSetUnsupported
	}
	if err := s.titleSetter.SetTitle(ctx, sessionID, id, req.Title); err != nil {
		switch {
		case errors.Is(err, sessions.ErrInvalidTitle):
			return prototypes.SessionsSetTitleResponse{},
				fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		case errors.Is(err, sessions.ErrSessionNotFound):
			return prototypes.SessionsSetTitleResponse{},
				fmt.Errorf("%w: %w", ErrSessionNotFound, err)
		case errors.Is(err, sessions.ErrIdentityMismatch):
			return prototypes.SessionsSetTitleResponse{},
				fmt.Errorf("%w: %w", ErrIdentityRequired, err)
		default:
			return prototypes.SessionsSetTitleResponse{},
				fmt.Errorf("sessions/protocol: set_title: %w", err)
		}
	}
	// The registry already applied the exact trim/clear transform this
	// mirrors — recomputed here (not returned by SetTitle, per its public
	// API surface) purely to shape the response; Title is echoed back to
	// the SAME caller that just supplied it, never onto the event bus.
	title := strings.TrimSpace(req.Title)
	source := string(sessions.TitleSourceManual)
	if title == "" {
		source = string(sessions.TitleSourceUnset)
	}
	return prototypes.SessionsSetTitleResponse{
		SessionID:   sessionID,
		Title:       title,
		TitleSource: source,
	}, nil
}

// sortRows orders rows in-place per the resolved sort.
func sortRows(rows []prototypes.SessionRow, srt prototypes.SessionSort) {
	sort.SliceStable(rows, func(i, j int) bool {
		return lessForSort(rows[i], rows[j], srt)
	})
}

// pageBounds returns the slice range and cursor metadata for an already
// filtered and sorted row set. Keeping this calculation shared by the
// general Service path and ListerProjector's lifecycle-only fast path pins
// their pagination semantics together.
func pageBounds(rows []prototypes.SessionRow, cursor *pageCursor, srt prototypes.SessionSort, limit int) (start, end int, truncated bool, next string) {
	if cursor != nil {
		for i, r := range rows {
			if afterCursor(r, *cursor, srt) {
				start = i
				break
			}
			start = i + 1
		}
	}
	end = len(rows)
	truncated = end-start > limit
	if truncated {
		end = start + limit
	}
	if truncated && start < end {
		next = encodeCursor(rows[end-1], srt)
	}
	return start, end, truncated, next
}

// lessForSort reports whether row a sorts before row b under srt. Ties
// break on SessionID ascending so the order — and hence the cursor — is
// deterministic.
func lessForSort(a, b prototypes.SessionRow, srt prototypes.SessionSort) bool {
	switch srt {
	case prototypes.SessionSortStartedAsc:
		if !a.StartedAt.Equal(b.StartedAt) {
			return a.StartedAt.Before(b.StartedAt)
		}
	case prototypes.SessionSortLastActivityDesc:
		if !a.LastActivityAt.Equal(b.LastActivityAt) {
			return a.LastActivityAt.After(b.LastActivityAt)
		}
	case prototypes.SessionSortCostDesc:
		if a.TotalCostCents != b.TotalCostCents {
			return a.TotalCostCents > b.TotalCostCents
		}
	default: // SessionSortStartedDesc
		if !a.StartedAt.Equal(b.StartedAt) {
			return a.StartedAt.After(b.StartedAt)
		}
	}
	return a.SessionID < b.SessionID
}
