// Package protocol implements the two `tasks.*` read methods the
// Console Tasks page (Phase 73d / D-123) consumes:
//
//   - tasks.list — paginated, faceted task-row projection + per-status
//     aggregates + cursor pagination.
//   - tasks.get  — enriched single-task detail: parent-session ref,
//     parent-task ref, cost rollup, planner-snapshot ref.
//
// The Console Tasks page consumes the EXISTING Phase 54 task-control
// verbs (`cancel` / `pause` / `resume` / `prioritize` / `approve` /
// `reject`) for mutation — there is NO `tasks.*` mutating method
// (CLAUDE.md §13 "no parallel implementations"). Both methods here are
// pure reads.
//
// # The seam (CLAUDE.md §4.4)
//
// The Service depends on the `Projector` interface, not on a concrete
// task registry. The V1 production implementation is `RegistryProjector`
// (registry_projector.go) — a thin read-only projection over a
// `tasks.TaskRegistry`. A future remote / aggregated-runtime projector
// slots in behind the same interface without reshaping the Service.
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
// A `tasks.list` whose `Filter.Identities` names more than one distinct
// tenant is a cross-tenant fan-in. The Service receives an
// `adminScoped bool` the wire handler computes from the verified JWT
// scope set; a false value on a cross-tenant request fails closed with
// `ErrScopeMismatch`. There is NO `tasks.admin` scope — the closed
// two-scope set (`admin` + `console:fleet`) is the only admit surface,
// and the cross-tenant gate is `ScopeAdmin`. On an accepted
// cross-tenant request the Service emits an `audit.admin_scope_used`
// event through the shipped audit.Redactor.
//
// A `tasks.get` for a TaskID outside the caller's tenant returns
// `ErrTaskNotFound` — existence is never revealed across tenants.
//
// # Concurrent reuse (D-025)
//
// A constructed *Service is immutable after NewService and safe to
// share across N concurrent goroutines: it holds only the Projector
// reference + an optional bus / redactor / logger; every method's
// per-call state lives in the call's arguments and locals, never on
// the Service.
package protocol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
	ErrIdentityRequired = errors.New("tasks/protocol: identity scope incomplete")
	// ErrScopeMismatch — a cross-tenant `tasks.list` fan-in was issued
	// without the verified `auth.ScopeAdmin` claim (D-079).
	ErrScopeMismatch = errors.New("tasks/protocol: cross-tenant query requires the admin scope claim")
	// ErrTaskNotFound — the requested TaskID is not visible to the
	// caller's identity scope (covers both genuine absence and a
	// cross-tenant lookup — existence is never revealed across tenants).
	ErrTaskNotFound = errors.New("tasks/protocol: task not found")
	// ErrInvalidRequest — the request was structurally invalid (an empty
	// task ID, an out-of-range page size, an unknown enum value).
	ErrInvalidRequest = errors.New("tasks/protocol: invalid request")
	// ErrMisconfigured — NewService was called with a nil Projector.
	ErrMisconfigured = errors.New("tasks/protocol: NewService missing a mandatory dependency")
)

// Projector is the read seam the Service depends on. The V1 production
// implementation is RegistryProjector. Every method takes the verified
// identity triple so the implementation scopes its reads — the Service
// never trusts a Projector to apply identity itself; it passes the
// triple so the implementation scopes a per-tenant view.
type Projector interface {
	// ListTasks returns every task-row projection visible to id. The
	// Service applies the facet filter + pagination + aggregate
	// computation on top; the Projector returns the full identity-scoped
	// set, newest-first by StartedAt.
	ListTasks(ctx context.Context, id identity.Identity) ([]prototypes.TaskRow, error)
	// GetTask returns the enriched detail for taskID, or ErrTaskNotFound
	// when the task is not visible to id (cross-tenant lookups return
	// ErrTaskNotFound — existence is never revealed).
	GetTask(ctx context.Context, id identity.Identity, taskID string) (prototypes.TaskDetail, error)
}

// Service implements the two `tasks.*` read methods. It is a D-025-safe
// compiled artifact — immutable after NewService.
type Service struct {
	projector Projector
	bus       events.EventBus // optional — nil ⇒ audit emit is logged only
	redactor  audit.Redactor  // optional — nil ⇒ audit emit is logged only
	logger    *slog.Logger
}

// Option configures NewService.
type Option func(*Service)

// WithBus wires the canonical events.EventBus the Service publishes the
// `audit.admin_scope_used` event onto when a cross-tenant `tasks.list`
// fan-in succeeds. A nil bus is treated as "WithBus not supplied" — the
// cross-tenant path still works, but the audit observation is logged at
// Info instead of published.
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

// WithLogger sets the slog.Logger the Service logs cross-tenant
// fan-ins and audit-emit failures to. A nil logger routes to
// slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewService builds the Tasks Protocol service over a Projector. The
// projector is mandatory — a nil fails loud with ErrMisconfigured
// rather than building a Service that would nil-panic on the first
// request (CLAUDE.md §5). The returned *Service is immutable after
// construction (D-025) and safe for concurrent use by N goroutines.
func NewService(projector Projector, opts ...Option) (*Service, error) {
	if projector == nil {
		return nil, fmt.Errorf("%w: Projector is nil", ErrMisconfigured)
	}
	s := &Service{
		projector: projector,
		logger:    slog.Default(),
	}
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
