package protocol

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// Query implements the observability.query domain method. It reads the
// VERIFIED caller identity from the request context (never the request
// body), resolves the effective identity scope, validates the closed
// contract, audits a granted widening before it reaches storage, executes
// the bounded indexed read, and stamps the mandatory freshness block.
//
// Order of operations (each step fails loud, never silently):
//
//  1. Verified identity from ctx — the authoritative scope. Absent or
//     incomplete → ErrIdentityRequired (identity is mandatory).
//  2. Structural validation (mandatory window, mandatory page limit).
//  3. Authority resolution: an ordinary caller is forced to their own
//     verified triple (naming any other tenant / user / session fails
//     closed); an admin|console:fleet caller may widen.
//  4. Closed-contract validation on the effective query (aligned window,
//     closed group_by / measures / sort, budgets, cursor shape) — the
//     rollups domain's own validate, so the closed sets stay single-
//     sourced.
//  5. A widened fan-in emits EXACTLY ONE audit.admin_scope_used BEFORE
//     the read reaches storage; an emit failure fails the read loud.
//  6. The bounded indexed read — never a raw event scan.
//  7. The mandatory freshness block (state, watermark, retention /
//     coverage), read from the quality seam.
func (s *Service) Query(ctx context.Context, req Request) (Response, error) {
	// Step 1: the verified caller identity is the authoritative scope. The
	// request body never supplies tenant / user / session identity for
	// widening, so there is nothing to reconcile — the verified triple IS
	// the scope.
	caller, ok := identity.FromVerified(ctx)
	if !ok {
		return Response{}, ErrIdentityRequired
	}
	if err := identity.Validate(caller); err != nil {
		return Response{}, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}

	// Step 2: structural validation that does not depend on identity.
	if req.From.IsZero() || req.To.IsZero() {
		return Response{}, fmt.Errorf("%w: the time window is mandatory", ErrInvalidRequest)
	}
	if req.Limit <= 0 {
		return Response{}, fmt.Errorf("%w: Limit is mandatory and must be > 0", ErrInvalidRequest)
	}
	if req.Limit > rollups.MaxRowsPerQuery {
		return Response{}, fmt.Errorf("%w: Limit=%d exceeds MaxRowsPerQuery=%d",
			ErrBudgetExceeded, req.Limit, rollups.MaxRowsPerQuery)
	}

	// Step 3: authority resolution. The verified caller tenant is
	// authoritative: an ordinary caller's effective filter is exactly
	// their own triple; an elevated caller's request filter is honored
	// (with the tenant fold) and widening is reported for the audit gate.
	elevated := s.scope(ctx)
	effective, widened, err := resolveFilter(caller, req.Filters, elevated)
	if err != nil {
		return Response{}, err
	}

	// Step 4: the closed contract on the EFFECTIVE query — the window is
	// aligned and bounded, the group_by / measures / sort sets are closed,
	// the page limit is in range, and the cursor (if any) is bound to this
	// exact query shape, which includes the effective identity scope.
	rq := rollups.Query{
		From:        req.From,
		To:          req.To,
		Bucket:      req.Bucket,
		GroupBy:     req.GroupBy,
		Filter:      effective,
		Measures:    req.Measures,
		Sort:        req.Sort,
		SortMeasure: req.SortMeasure,
		Limit:       req.Limit,
		Cursor:      req.Cursor,
	}
	if err := rq.Validate(); err != nil {
		return Response{}, mapRollupError(err)
	}

	// Step 5: a widened fan-in MUST be audited before it reaches storage —
	// exactly one canonical audit.admin_scope_used event. A failure fails
	// the read loud (the read crossed the identity boundary, so the
	// operator MUST see the audit).
	if elevated && widened {
		if err := s.emitAdminAudit(ctx, caller, req.Filters); err != nil {
			return Response{}, err
		}
	}

	// Step 6: the bounded indexed read. The store re-validates (defense in
	// depth); the wrapped sentinels map onto the domain's typed errors.
	res, err := s.querier.Query(ctx, rq)
	if err != nil {
		switch {
		case errors.Is(err, rollups.ErrQueryInvalid):
			return Response{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		case errors.Is(err, rollups.ErrQueryBudget):
			return Response{}, fmt.Errorf("%w: %w", ErrBudgetExceeded, err)
		case errors.Is(err, rollups.ErrBadCursor):
			return Response{}, fmt.Errorf("%w: %w", ErrBadCursor, err)
		default:
			return Response{}, fmt.Errorf("%w: %w", ErrQueryFailed, err)
		}
	}

	// Step 7: the mandatory freshness block. A failure to read it refuses
	// the response — a response without an honest freshness stamp would
	// present rows whose completeness is unknowable.
	q, err := s.quality.Quality(ctx)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %w", ErrQualityFailed, err)
	}

	return Response{
		Rows:       res.Rows,
		NextCursor: res.NextCursor,
		Quality:    buildQualityBlock(q, req.From, req.To),
	}, nil
}

// resolveFilter derives the EFFECTIVE rollup filter for a request from the
// verified caller identity and the request's filter sets. It returns the
// effective filter, whether the effective read widens beyond the caller's
// own verified triple (the audit gate), and a typed scope error when an
// ordinary caller names an identity outside their own scope.
//
// Ordinary caller: the effective filter is EXACTLY the verified
// (tenant, user, session) triple (the model axis is honored from the
// request). Naming any other tenant / user / session in the request fails
// closed with the matching scope sentinel — the request body can never
// widen, and the fold is never silent: a foreign value is rejected, not
// quietly dropped.
//
// Elevated caller (verified admin or console:fleet): the request's filter
// is honored verbatim on the user / session axes (an empty axis is the
// wildcard fan-in) and the tenant axis FOLDS to the caller's own tenant
// when empty (an elided tenant axis is never a cross-tenant wildcard — a
// fleet caller names the tenants it reads). Widening is reported when the
// effective read fans in beyond the caller's own triple on any axis.
func resolveFilter(caller identity.Identity, f Filters, elevated bool) (rollups.Filter, bool, error) {
	if elevated {
		tenantIDs := f.TenantIDs
		if len(tenantIDs) == 0 {
			tenantIDs = []string{caller.TenantID}
		}
		return rollups.Filter{
			TenantIDs:  tenantIDs,
			UserIDs:    f.UserIDs,
			SessionIDs: f.SessionIDs,
			Models:     f.Models,
		}, widensBeyond(f, caller), nil
	}

	for _, t := range f.TenantIDs {
		if t != "" && t != caller.TenantID {
			return rollups.Filter{}, false,
				fmt.Errorf("%w: filter names tenant %q outside the verified tenant %q",
					ErrCrossTenantScope, t, caller.TenantID)
		}
	}
	for _, u := range f.UserIDs {
		if u != "" && u != caller.UserID {
			return rollups.Filter{}, false,
				fmt.Errorf("%w: filter names user %q outside the verified user %q",
					ErrCrossUserScope, u, caller.UserID)
		}
	}
	for _, sid := range f.SessionIDs {
		if sid != "" && sid != caller.SessionID {
			return rollups.Filter{}, false,
				fmt.Errorf("%w: filter names session %q outside the verified session %q",
					ErrCrossSessionScope, sid, caller.SessionID)
		}
	}
	return rollups.Filter{
		TenantIDs:  []string{caller.TenantID},
		UserIDs:    []string{caller.UserID},
		SessionIDs: []string{caller.SessionID},
		Models:     f.Models,
	}, false, nil
}

// widensBeyond reports whether the request's identity filter axes fan in
// beyond the caller's own verified triple — the audit gate for an elevated
// caller. The tenant axis FOLDS when empty (never a widening); the user
// and session axes treat an empty axis as the wildcard fan-in and a
// multi-value axis as a fan-in even when every value is the caller's own
// repeated value (asking for many principals' rows in one read is the
// fan-in trigger).
func widensBeyond(f Filters, id identity.Identity) bool {
	if axisWidens(f.UserIDs, id.UserID) {
		return true
	}
	if axisWidens(f.SessionIDs, id.SessionID) {
		return true
	}
	for _, t := range f.TenantIDs {
		if t != "" && t != id.TenantID {
			return true
		}
	}
	return false
}

// axisWidens reports whether one identity axis fans in beyond the caller's
// own value: an empty axis is the wildcard fan-in, a single foreign value
// is a cross-identity read, and a multi-value axis is a fan-in regardless
// of membership.
func axisWidens(values []string, own string) bool {
	if len(values) == 0 {
		return true
	}
	if len(values) > 1 {
		return true
	}
	return values[0] != own
}

// mapRollupError maps the rollup domain's wrapped validation sentinels
// onto the service's typed errors. The rollup sentinels are already
// wrapped with the detailed reason; the domain sentinel is what the wire
// adapter keys on.
func mapRollupError(err error) error {
	switch {
	case errors.Is(err, rollups.ErrQueryInvalid):
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	case errors.Is(err, rollups.ErrQueryBudget):
		return fmt.Errorf("%w: %w", ErrBudgetExceeded, err)
	case errors.Is(err, rollups.ErrBadCursor):
		return fmt.Errorf("%w: %w", ErrBadCursor, err)
	default:
		return err
	}
}
