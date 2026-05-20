// Package protocol implements the two `sessions.*` Protocol methods the
// Console Sessions page (Phase 73c / D-122) consumes:
//
//   - sessions.list    — paginated, filtered SessionRegistry projection.
//   - sessions.inspect — full per-session snapshot for the detail view.
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
// # Cross-tenant gating (D-079)
//
// A `sessions.list` whose `Filter.TenantIDs` names a tenant other than
// the caller's verified tenant requires the verified `auth.ScopeAdmin`
// claim. The Service receives an `adminScoped bool` the wire handler
// computes from the verified JWT scope set; a false value on a
// cross-tenant filter fails closed with `ErrCrossTenantScope`. There is
// NO `sessions.admin` scope — the closed two-scope set (`admin` +
// `console:fleet`) is the only admit surface (D-079). On a successful
// admin-scope query the Service emits an `audit.admin_scope_used`
// event.
//
// # Concurrent reuse (D-025)
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
	// `auth.ScopeAdmin` claim (D-079).
	ErrCrossTenantScope = errors.New("sessions/protocol: cross-tenant filter requires the admin scope claim")
	// ErrInvalidRequest — the request was structurally invalid (an
	// out-of-range limit, an unknown enum, a malformed cursor).
	ErrInvalidRequest = errors.New("sessions/protocol: invalid request")
	// ErrSessionNotFound — `sessions.inspect` targeted a session id with
	// no record visible to the caller's identity scope.
	ErrSessionNotFound = errors.New("sessions/protocol: session not found")
	// ErrMisconfigured — NewService was called with a nil Projector.
	ErrMisconfigured = errors.New("sessions/protocol: NewService missing a mandatory dependency")
)

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
}

// Service implements the two `sessions.*` Protocol methods. It is a
// D-025-safe compiled artifact — immutable after NewService.
type Service struct {
	projector Projector
	bus       events.EventBus // optional — nil ⇒ admin audit emit is logged only
	redactor  audit.Redactor  // optional — defence-in-depth before the emit
	logger    *slog.Logger
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
// construction (D-025) and safe for concurrent use by N goroutines.
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
		return identity.Identity{}, fmt.Errorf("%w: %v", ErrIdentityRequired, err)
	}
	return id, nil
}

// isCrossTenant reports whether the filter names a tenant other than
// the caller's verified tenant — the predicate that gates the D-079
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
// enforces the D-079 cross-tenant gate, resolves the identity-scoped
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

	cursor, err := decodeCursor(req.Cursor)
	if err != nil {
		return prototypes.SessionsListResponse{}, err
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

	// Cursor pagination: drop every row up to and including the cursor
	// position, then page Limit rows. The cursor is the (sort-key,
	// SessionID) of the last row of the previous page.
	start := 0
	if cursor != nil {
		for i, r := range filtered {
			if afterCursor(r, *cursor, srt) {
				start = i
				break
			}
			start = i + 1
		}
	}
	page := filtered[start:]

	// Truncated: the candidate set has more than Limit rows past the
	// cursor — D-026 fail-loudly, never a silent total.
	truncated := len(page) > limit
	if truncated {
		page = page[:limit]
	}

	next := ""
	if truncated && len(page) > 0 {
		last := page[len(page)-1]
		next = encodeCursor(last, srt)
	}

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

// sortRows orders rows in-place per the resolved sort.
func sortRows(rows []prototypes.SessionRow, srt prototypes.SessionSort) {
	sort.SliceStable(rows, func(i, j int) bool {
		return lessForSort(rows[i], rows[j], srt)
	})
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
