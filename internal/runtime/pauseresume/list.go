package pauseresume

import (
	"context"
	"fmt"
	"sort"

	"github.com/hurtener/Harbor/internal/identity"
)

// Pause-list pagination bounds (Phase 72e / D-110). The defaults mirror
// the Protocol-edge types.PauseListRequest contract — a List caller
// that leaves Page / PageSize zero gets the documented defaults; a
// PageSize above the max fails closed with ErrInvalidPage (never a
// silent clamp). Kept here as the runtime-side single source so the
// Coordinator validation and the Protocol-edge handler agree.
const (
	// DefaultListPageSize is the per-page row count applied when a
	// ListRequest leaves PageSize zero.
	DefaultListPageSize = 50
	// MaxListPageSize is the largest PageSize Coordinator.List accepts;
	// a larger value fails closed with ErrInvalidPage.
	MaxListPageSize = 200
)

// List returns a paginated, identity-scope-filtered snapshot of pause
// records. See the Coordinator interface godoc for the full contract.
//
// Read-only: List snapshots the in-memory registry under the mutex,
// releases the lock, then filters / sorts / paginates the value copies.
// It never mutates the registry, never calls Resume, never touches the
// checkpoint store. The Coordinator's in-memory registry IS the index
// (brief 05 — the runtime owns the index; the Protocol exposes a
// paginated method, never a client-side filter over a full dump).
//
// Resumed-record retention: the Coordinator's resume path is
// destructive (Resume flips the in-memory entry to StatusResumed but a
// fresh Coordinator over the same store does not rehydrate it). A
// List with a status=resumed filter therefore reflects only the
// resumed records still live in this Coordinator instance's registry;
// when that slice is empty the response carries Truncated=true so the
// operator knows to use `events.subscribe` on the `pause.resumed`
// topic for historical resume activity.
func (c *coordinator) List(ctx context.Context, req ListRequest) (ListResponse, error) {
	if err := ctx.Err(); err != nil {
		return ListResponse{}, fmt.Errorf("pauseresume: list cancelled: %w", err)
	}

	// Identity-mandatory — fail closed on an incomplete triple
	// (CLAUDE.md §6 rule 9 + D-001). No identity-downgrading knob.
	if err := identity.Validate(req.Identity); err != nil {
		return ListResponse{}, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}

	// Pagination bounds — fail closed, never silently clamp.
	page, pageSize, err := normalizePagination(req.Page, req.PageSize)
	if err != nil {
		return ListResponse{}, err
	}

	// Cross-tenant gate — a filter naming a tenant other than the
	// caller's own, or more than one tenant, requires AdminScoped.
	if err := checkCrossTenantScope(req); err != nil {
		return ListResponse{}, err
	}

	// Snapshot the registry under the mutex into value copies, then
	// release the lock before the (potentially large) filter/sort work
	// so List never holds the lock across CPU-bound work and a
	// concurrent Request / Resume is not blocked.
	snapshot := c.snapshotEntries()

	// Normalise the filter once: the default state set is [StatusPaused]
	// and the default tenant set is the caller's own tenant.
	stateSet := req.Filter.States
	if len(stateSet) == 0 {
		stateSet = []State{StatusPaused}
	}
	resumedRequested := containsState(stateSet, StatusResumed)

	matched := make([]pauseEntry, 0, len(snapshot))
	resumedSeen := false
	for _, e := range snapshot {
		if e.state == StatusResumed {
			resumedSeen = true
		}
		if matchEntry(e, req, stateSet) {
			matched = append(matched, e)
		}
	}

	// Order PausedAt descending (newest first) for a deterministic
	// intervention queue; ties broken by Token so the order is total.
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].pausedAt.Equal(matched[j].pausedAt) {
			return matched[i].token < matched[j].token
		}
		return matched[i].pausedAt.After(matched[j].pausedAt)
	})

	total := len(matched)
	pageCount := 0
	if total > 0 {
		pageCount = (total + pageSize - 1) / pageSize
	}

	// Slice the requested page. A page past the end yields an empty
	// snapshot slice (not an error) — the response still carries the
	// honest TotalRows / PageCount so the client can correct.
	start := (page - 1) * pageSize
	endIdx := start + pageSize
	var pageRows []pauseEntry
	if start < total {
		if endIdx > total {
			endIdx = total
		}
		pageRows = matched[start:endIdx]
	}

	snapshots := make([]Pause, 0, len(pageRows))
	statuses := make([]Status, 0, len(pageRows))
	for _, e := range pageRows {
		snapshots = append(snapshots, Pause{
			Token:    e.token,
			Reason:   e.reason,
			Payload:  cloneStringMap(e.payload),
			PausedAt: e.pausedAt,
			Identity: e.identity,
		})
		statuses = append(statuses, Status{
			State:     e.state,
			Reason:    e.reason,
			PausedAt:  e.pausedAt,
			ResumedAt: e.resumedAt,
		})
	}

	// Truncated signal: a status=resumed query whose result reflects an
	// aged-out registry. We flag truncation when the caller asked for
	// resumed records but the registry holds no resumed entries at all
	// — the destructive-on-resume contract means resumed records are
	// transient, so an empty resumed slice is the operator-visible
	// "this is not a complete history" signal (Phase 72e plan §"Risks").
	truncated := resumedRequested && !resumedSeen

	return ListResponse{
		Snapshots: snapshots,
		Statuses:  statuses,
		Page:      page,
		PageSize:  pageSize,
		PageCount: pageCount,
		TotalRows: total,
		Truncated: truncated,
	}, nil
}

// snapshotEntries copies every registry entry into value copies under
// the mutex. The returned slice is the caller's own — no pointer into
// the registry escapes, so the filter/sort work runs lock-free.
func (c *coordinator) snapshotEntries() []pauseEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]pauseEntry, 0, len(c.pauses))
	for _, e := range c.pauses {
		// Value copy — the trajectory pointer is shared, but List never
		// reads it; the payload map is cloned downstream per row.
		out = append(out, *e)
	}
	return out
}

// normalizePagination validates and defaults the Page / PageSize pair.
// Page 0 ⇒ 1; PageSize 0 ⇒ DefaultListPageSize. A negative Page, a
// negative PageSize, or a PageSize above MaxListPageSize fails closed
// with ErrInvalidPage — never a silent clamp.
func normalizePagination(page, pageSize int) (int, int, error) {
	if page < 0 {
		return 0, 0, fmt.Errorf("%w: page %d is negative", ErrInvalidPage, page)
	}
	if page == 0 {
		page = 1
	}
	if pageSize < 0 {
		return 0, 0, fmt.Errorf("%w: page_size %d is negative", ErrInvalidPage, pageSize)
	}
	if pageSize == 0 {
		pageSize = DefaultListPageSize
	}
	if pageSize > MaxListPageSize {
		return 0, 0, fmt.Errorf("%w: page_size %d exceeds max %d", ErrInvalidPage, pageSize, MaxListPageSize)
	}
	return page, pageSize, nil
}

// checkCrossTenantScope fails closed when the filter reaches outside
// the caller's own tenant without ListRequest.AdminScoped. A filter
// that names only the caller's own tenant (or no tenant at all) is
// always in-scope.
func checkCrossTenantScope(req ListRequest) error {
	tenants := req.Filter.TenantIDs
	if len(tenants) == 0 {
		return nil
	}
	if req.AdminScoped {
		return nil
	}
	if len(tenants) > 1 {
		return fmt.Errorf("%w: filter names %d tenants", ErrCrossTenantScope, len(tenants))
	}
	if tenants[0] != req.Identity.TenantID {
		return fmt.Errorf("%w: filter tenant %q is not the caller's tenant %q",
			ErrCrossTenantScope, tenants[0], req.Identity.TenantID)
	}
	return nil
}

// matchEntry reports whether a registry entry passes the filter under
// the caller's identity scope. The identity-scope rule:
//
//   - When the filter names no TenantIDs, the entry's tenant MUST equal
//     the caller's own tenant (the default own-scope projection).
//   - When the filter names TenantIDs (admin-scoped — already gated by
//     checkCrossTenantScope), the entry's tenant MUST be in the set.
//   - User / Session / Run filters, when present, are AND-ed in.
func matchEntry(e pauseEntry, req ListRequest, stateSet []State) bool {
	if !containsState(stateSet, e.state) {
		return false
	}

	// Tenant scope.
	if len(req.Filter.TenantIDs) == 0 {
		if e.identity.TenantID != req.Identity.TenantID {
			return false
		}
	} else if !containsString(req.Filter.TenantIDs, e.identity.TenantID) {
		return false
	}

	// User scope. An empty UserIDs filter does NOT narrow to the
	// caller's own user — an operator listing their own tenant's
	// intervention queue legitimately sees pauses across users in that
	// tenant. The isolation boundary enforced here is the tenant; the
	// user/session axes are optional narrowing filters.
	if len(req.Filter.UserIDs) > 0 && !containsString(req.Filter.UserIDs, e.identity.UserID) {
		return false
	}
	if len(req.Filter.SessionIDs) > 0 && !containsString(req.Filter.SessionIDs, e.identity.SessionID) {
		return false
	}
	if len(req.Filter.RunIDs) > 0 && !containsString(req.Filter.RunIDs, e.runID) {
		return false
	}

	// Reason filter.
	if len(req.Filter.Reasons) > 0 && !containsReason(req.Filter.Reasons, e.reason) {
		return false
	}

	// Time window on PausedAt (inclusive bounds).
	if !req.Filter.Since.IsZero() && e.pausedAt.Before(req.Filter.Since) {
		return false
	}
	if !req.Filter.Until.IsZero() && e.pausedAt.After(req.Filter.Until) {
		return false
	}

	return true
}

func containsState(set []State, s State) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}

func containsReason(set []Reason, r Reason) bool {
	for _, v := range set {
		if v == r {
			return true
		}
	}
	return false
}

func containsString(set []string, s string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}
