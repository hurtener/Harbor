// Package protocol implements the admin-scoped `governance.*`
// tenant-default override Protocol methods the Console control plane
// consumes:
//
//   - governance.set_tenant_overrides — set (or clear) a tenant's default
//     LLM parameters (model / additive extra-instructions / temperature /
//     max-tokens / reasoning-effort) live, with no redeploy.
//   - governance.get_tenant_overrides — read the current tenant default.
//
// Both methods are admin-scoped (the `auth.ScopeAdmin` claim, enforced at
// the wire handler) and identity-mandatory. The override is a tenant-wide
// desired-state record applied to every session in the tenant on its NEXT
// run — never mid-flight (the run loop snapshots it at run start).
//
// # The seam (CLAUDE.md §4.4)
//
// The Service depends on a narrow `TenantOverrideStore` interface — the
// `*governance.TenantOverridePolicy` concrete satisfies it, and tests
// inject a fake. The Service owns wire validation + the wire↔policy spec
// mapping; the policy owns persistence + audit.
//
// # Identity is mandatory (CLAUDE.md §6 rule 9)
//
// Every method takes the wire request's `IdentityScope`. An incomplete
// triple fails closed with `ErrIdentityRequired`. The tenant whose
// defaults are set/read is the caller's verified tenant — the handler
// overlays the verified identity, so a caller cannot target another
// tenant's defaults through this surface (single-tenant V1; a cross-tenant
// admin path is a future extension, mirroring the posture surface).
//
// # Concurrent reuse
//
// A constructed *Service is immutable after NewService and safe to share
// across N goroutines: it holds only the store reference + a clock +
// logger. Per-call state lives in arguments and locals.
package protocol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// Sentinel errors the Service returns. The wire handler maps each onto a
// canonical Protocol Code + HTTP status; in-process callers compare with
// errors.Is.
var (
	// ErrIdentityRequired — the request carried an incomplete identity
	// triple. RFC §5.5 / CLAUDE.md §6 rule 9 — fails closed.
	ErrIdentityRequired = errors.New("governance/protocol: identity scope incomplete")
	// ErrMisconfigured — NewService was called with a nil store.
	ErrMisconfigured = errors.New("governance/protocol: NewService missing a mandatory dependency")
)

// TenantOverrideStore is the narrow seam the Service drives. The
// `*governance.TenantOverridePolicy` concrete satisfies it.
type TenantOverrideStore interface {
	// Set replaces tenant's default-override record with spec (desired-
	// state replace). actor is the admin caller (the audit anchor).
	Set(ctx context.Context, actor identity.Quadruple, tenant string, spec governance.TenantOverrideSpec) error
	// Get returns the tenant's current default-override spec and whether
	// a record exists.
	Get(ctx context.Context, tenant string) (governance.TenantOverrideSpec, bool, error)
}

// Clock is the time source the Service stamps AppliedAt from.
type Clock func() time.Time

// Service implements the admin-scoped governance tenant-override methods.
type Service struct {
	store  TenantOverrideStore
	logger *slog.Logger
	now    Clock
}

// Option configures NewService.
type Option func(*Service)

// WithLogger sets the slog.Logger. A nil logger routes to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithClock injects the time source. Defaults to time.Now.
func WithClock(c Clock) Option {
	return func(s *Service) {
		if c != nil {
			s.now = c
		}
	}
}

// NewService builds the governance tenant-override Service over a
// TenantOverrideStore. store is mandatory — a nil fails loud with
// ErrMisconfigured rather than building a Service that would nil-panic on
// the first request (CLAUDE.md §5).
//
// The returned *Service is immutable after construction and safe for
// concurrent use by N goroutines.
func NewService(store TenantOverrideStore, opts ...Option) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: TenantOverrideStore is nil", ErrMisconfigured)
	}
	s := &Service{store: store, logger: slog.Default(), now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// SetTenantOverrides validates identity, maps the wire override into the
// governance spec, persists it via the store, and returns the apply
// instant. The tenant whose defaults are set is the caller's verified
// tenant. Validation failures (unknown model, out-of-range value) surface
// from the policy and map onto CodeInvalidRequest at the wire edge.
func (s *Service) SetTenantOverrides(ctx context.Context, req prototypes.GovernanceSetTenantOverridesRequest) (prototypes.GovernanceSetTenantOverridesResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.GovernanceSetTenantOverridesResponse{}, err
	}
	id, err := identityFromScope(req.Identity)
	if err != nil {
		return prototypes.GovernanceSetTenantOverridesResponse{}, err
	}
	actor := identity.Quadruple{Identity: id}
	spec := wireToSpec(req.Overrides)
	if err := s.store.Set(ctx, actor, id.TenantID, spec); err != nil {
		return prototypes.GovernanceSetTenantOverridesResponse{}, err
	}
	return prototypes.GovernanceSetTenantOverridesResponse{
		AppliedAt:       s.now().UTC(),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// GetTenantOverrides validates identity and returns the caller tenant's
// current default-override record.
func (s *Service) GetTenantOverrides(ctx context.Context, req prototypes.GovernanceGetTenantOverridesRequest) (prototypes.GovernanceGetTenantOverridesResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.GovernanceGetTenantOverridesResponse{}, err
	}
	id, err := identityFromScope(req.Identity)
	if err != nil {
		return prototypes.GovernanceGetTenantOverridesResponse{}, err
	}
	spec, set, err := s.store.Get(ctx, id.TenantID)
	if err != nil {
		return prototypes.GovernanceGetTenantOverridesResponse{}, err
	}
	return prototypes.GovernanceGetTenantOverridesResponse{
		Overrides:       specToWire(spec),
		Set:             set,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// identityFromScope validates a wire IdentityScope into an
// identity.Identity, failing closed on an incomplete triple.
func identityFromScope(scope prototypes.IdentityScope) (identity.Identity, error) {
	id := identity.Identity{TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session}
	if id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return identity.Identity{}, fmt.Errorf("%w: (tenant=%q user=%q session=%q)",
			ErrIdentityRequired, id.TenantID, id.UserID, id.SessionID)
	}
	return id, nil
}

// wireToSpec maps the wire override record onto the governance spec. Each
// pointer is copied so the spec shares no pointer with the request.
func wireToSpec(o prototypes.GovernanceTenantOverrides) governance.TenantOverrideSpec {
	var spec governance.TenantOverrideSpec
	if o.Model != nil {
		v := *o.Model
		spec.Model = &v
	}
	if o.ExtraInstructions != nil {
		v := *o.ExtraInstructions
		spec.ExtraInstructions = &v
	}
	if o.Temperature != nil {
		v := *o.Temperature
		spec.Temperature = &v
	}
	if o.MaxTokens != nil {
		v := *o.MaxTokens
		spec.MaxTokens = &v
	}
	if o.ReasoningEffort != nil {
		v := *o.ReasoningEffort
		spec.ReasoningEffort = &v
	}
	return spec
}

// specToWire maps the governance spec onto the wire override record.
func specToWire(spec governance.TenantOverrideSpec) prototypes.GovernanceTenantOverrides {
	var o prototypes.GovernanceTenantOverrides
	if spec.Model != nil {
		v := *spec.Model
		o.Model = &v
	}
	if spec.ExtraInstructions != nil {
		v := *spec.ExtraInstructions
		o.ExtraInstructions = &v
	}
	if spec.Temperature != nil {
		v := *spec.Temperature
		o.Temperature = &v
	}
	if spec.MaxTokens != nil {
		v := *spec.MaxTokens
		o.MaxTokens = &v
	}
	if spec.ReasoningEffort != nil {
		v := *spec.ReasoningEffort
		o.ReasoningEffort = &v
	}
	return o
}
