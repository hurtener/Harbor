package protocol

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// List implements the `tasks.list` Protocol method. It validates
// identity, gates a cross-tenant fan-in on the admin scope claim,
// resolves the identity-scoped task set from the Projector, applies the
// facet filter + free-text search, computes the per-status aggregates,
// and returns one cursor-paginated page.
//
// adminScoped is the verified-JWT scope decision the wire handler
// computes from `auth.HasScope(ctx, auth.ScopeAdmin)`. It is consulted
// ONLY when the request is a cross-tenant fan-in (Filter.Identities
// naming more than one distinct tenant); a single-tenant request never
// requires it.
func (s *Service) List(ctx context.Context, req prototypes.TaskListRequest, adminScoped bool) (prototypes.TaskListResponse, error) {
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.TaskListResponse{}, err
	}

	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = prototypes.DefaultTaskListPageSize
	}
	if pageSize < 0 || pageSize > prototypes.MaxTaskListPageSize {
		return prototypes.TaskListResponse{}, fmt.Errorf("%w: page_size %d outside [1,%d]",
			ErrInvalidRequest, pageSize, prototypes.MaxTaskListPageSize)
	}

	if err := validateFilter(req.Filter); err != nil {
		return prototypes.TaskListResponse{}, err
	}

	// Cross-tenant gating (D-079). A query naming more than one distinct
	// tenant is a fan-in — it requires the verified admin scope claim.
	tenants := distinctTenants(id, req.Filter.Identities)
	crossTenant := len(tenants) > 1
	if crossTenant && !adminScoped {
		return prototypes.TaskListResponse{}, fmt.Errorf(
			"%w: tasks.list named %d tenants", ErrScopeMismatch, len(tenants))
	}

	all, err := s.projector.ListTasks(ctx, id)
	if err != nil {
		return prototypes.TaskListResponse{}, fmt.Errorf("tasks/protocol: list: %w", err)
	}

	// Apply the facet filter + free-text search.
	filtered := make([]prototypes.TaskRow, 0, len(all))
	for _, t := range all {
		if filterMatches(req.Filter, t) {
			filtered = append(filtered, t)
		}
	}
	// Newest-first by StartedAt; ID is the tiebreaker so pagination is
	// deterministic when two tasks share a StartedAt.
	sort.SliceStable(filtered, func(i, j int) bool {
		if !filtered[i].StartedAt.Equal(filtered[j].StartedAt) {
			return filtered[i].StartedAt.After(filtered[j].StartedAt)
		}
		return filtered[i].ID < filtered[j].ID
	})

	aggregates := computeAggregates(filtered)

	// Cursor pagination — the cursor is an opaque base64 of the next
	// row offset. An invalid / malformed cursor is a 400, never a
	// silent reset to page 1 (CLAUDE.md §13 fail-loudly).
	offset, err := decodeCursor(req.Cursor)
	if err != nil {
		return prototypes.TaskListResponse{}, err
	}

	rows := []prototypes.TaskRow{}
	var nextCursor prototypes.TaskListCursor
	if offset < len(filtered) {
		end := offset + pageSize
		if end > len(filtered) {
			end = len(filtered)
		}
		rows = filtered[offset:end]
		if end < len(filtered) {
			nextCursor = encodeCursor(end)
		}
	}

	if crossTenant {
		s.emitAdminAudit(ctx, id, "tasks.list", len(tenants))
	}

	resp := prototypes.TaskListResponse{
		Rows:       rows,
		Cursor:     nextCursor,
		Aggregates: aggregates,
	}

	// Phase 73b (D-126): the opt-in status-counter-strip aggregate. It
	// is computed over the FULL identity-scoped task set `all` — NOT the
	// filtered view — so the Live Runtime header strip reports session-
	// wide present-tense posture rather than narrowing with the page's
	// facet filter. The aggregate stays within the call's identity
	// scope: `all` is the Projector's identity-scoped projection, so the
	// counter never crosses the isolation boundary (CLAUDE.md §6 rule 2).
	if req.IncludeStatusCounterStrip {
		strip := computeStatusCounterStrip(all)
		resp.StatusCounterStrip = &strip
	}

	return resp, nil
}

// computeStatusCounterStrip tallies the five-chip Live Runtime header
// strip over the supplied identity-scoped task set. It keys on the
// canonical lifecycle vocabulary the page-spec mockup uses (`completed`
// for the runtime `complete` status); the `cancelled` status is folded
// out — the strip is a five-chip present-tense posture, not the six-
// status kanban tally (see the TasksListStatusCounterStrip godoc).
func computeStatusCounterStrip(rows []prototypes.TaskRow) prototypes.TasksListStatusCounterStrip {
	var strip prototypes.TasksListStatusCounterStrip
	for _, t := range rows {
		switch t.Status {
		case prototypes.TaskStatusPending:
			strip.Pending++
		case prototypes.TaskStatusRunning:
			strip.Running++
		case prototypes.TaskStatusComplete:
			strip.Completed++
		case prototypes.TaskStatusPaused:
			strip.Paused++
		case prototypes.TaskStatusFailed:
			strip.Failed++
		}
	}
	return strip
}

// validateFilter rejects a structurally invalid TaskFilter — an unknown
// status / kind enum, a negative latency bound, or a Since after Until.
func validateFilter(f prototypes.TaskFilter) error {
	for _, st := range f.Statuses {
		if !prototypes.IsValidTaskStatus(st) {
			return fmt.Errorf("%w: unknown task status %q", ErrInvalidRequest, st)
		}
	}
	for _, k := range f.Kinds {
		if !prototypes.IsValidTaskKind(k) {
			return fmt.Errorf("%w: unknown task kind %q", ErrInvalidRequest, k)
		}
	}
	if f.LatencyAboveMS < 0 {
		return fmt.Errorf("%w: latency_above_ms is negative", ErrInvalidRequest)
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && f.Since.After(f.Until) {
		return fmt.Errorf("%w: filter `since` is after `until`", ErrInvalidRequest)
	}
	return nil
}

// distinctTenants returns the set of distinct tenant IDs a request
// touches — the caller's own tenant plus every tenant named in
// Filter.Identities. The caller's tenant is always included so a
// single-identity filter that matches the caller's own tenant is NOT a
// cross-tenant fan-in.
func distinctTenants(caller identity.Identity, filterIDs []prototypes.IdentityScope) map[string]struct{} {
	seen := map[string]struct{}{caller.TenantID: {}}
	for _, fid := range filterIDs {
		if fid.Tenant != "" {
			seen[fid.Tenant] = struct{}{}
		}
	}
	return seen
}

// filterMatches reports whether a TaskRow satisfies every facet of f.
// An empty facet slice matches every value on that axis.
func filterMatches(f prototypes.TaskFilter, t prototypes.TaskRow) bool {
	if len(f.Statuses) > 0 && !containsStatus(f.Statuses, t.Status) {
		return false
	}
	if len(f.Kinds) > 0 && !containsKind(f.Kinds, t.Kind) {
		return false
	}
	if f.ParentTaskID != "" && t.ParentTaskID != f.ParentTaskID {
		return false
	}
	if len(f.Identities) > 0 && !identityMatches(f.Identities, t.Identity) {
		return false
	}
	if !f.Since.IsZero() && t.StartedAt.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && t.StartedAt.After(f.Until) {
		return false
	}
	if len(f.ErrorClasses) > 0 && !containsString(f.ErrorClasses, t.ErrorClass) {
		return false
	}
	if f.LatencyAboveMS > 0 && t.DurationMS < f.LatencyAboveMS {
		return false
	}
	if f.Search != "" {
		needle := strings.ToLower(strings.TrimSpace(f.Search))
		hay := strings.ToLower(t.Description + " " + t.Query)
		if !strings.Contains(hay, needle) {
			return false
		}
	}
	// Phase 73h (D-128): the Background Jobs page's per-job "Related
	// Sessions" tab issues a `tasks.list` with GroupID set to surface
	// the sibling tasks under the same TaskGroup. An empty GroupID is
	// the wildcard — most foreground turns aren't group members.
	if f.GroupID != "" && t.GroupID != f.GroupID {
		return false
	}
	// Phase 73h (D-128): the `Has pending approval` facet. nil = no
	// filter; a non-nil pointer restricts to rows whose
	// HasPendingApproval equals the pointee.
	if f.HasPendingApproval != nil && t.HasPendingApproval != *f.HasPendingApproval {
		return false
	}
	return true
}

func containsStatus(set []prototypes.TaskStatus, v prototypes.TaskStatus) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func containsKind(set []prototypes.TaskKind, v prototypes.TaskKind) bool {
	for _, k := range set {
		if k == v {
			return true
		}
	}
	return false
}

func containsString(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// identityMatches reports whether row identity t is in the filter's
// identity set. A filter entry matches when every NON-EMPTY component
// it specifies equals the corresponding row component — so a
// tenant-only entry (`{tenant: "t1"}`) matches every task in tenant t1.
func identityMatches(set []prototypes.IdentityScope, t prototypes.IdentityScope) bool {
	for _, want := range set {
		if want.Tenant != "" && want.Tenant != t.Tenant {
			continue
		}
		if want.User != "" && want.User != t.User {
			continue
		}
		if want.Session != "" && want.Session != t.Session {
			continue
		}
		return true
	}
	return false
}

// computeAggregates tallies the per-status counters over the filtered
// task set — the per-column counts the kanban renders at each header.
func computeAggregates(rows []prototypes.TaskRow) prototypes.TaskListAggregates {
	var agg prototypes.TaskListAggregates
	for _, t := range rows {
		switch t.Status {
		case prototypes.TaskStatusPending:
			agg.Pending++
		case prototypes.TaskStatusRunning:
			agg.Running++
		case prototypes.TaskStatusPaused:
			agg.Paused++
		case prototypes.TaskStatusComplete:
			agg.Complete++
		case prototypes.TaskStatusFailed:
			agg.Failed++
		case prototypes.TaskStatusCancelled:
			agg.Cancelled++
		}
	}
	return agg
}

// cursorPrefix tags the opaque cursor token so a malformed / foreign
// token is rejected loudly rather than mis-decoded as an offset.
const cursorPrefix = "tasks:"

// encodeCursor builds the opaque continuation cursor for the row at
// offset.
func encodeCursor(offset int) prototypes.TaskListCursor {
	raw := cursorPrefix + strconv.Itoa(offset)
	return prototypes.TaskListCursor{
		NextPageToken: base64.RawURLEncoding.EncodeToString([]byte(raw)),
	}
}

// decodeCursor resolves the opaque cursor token back to a row offset.
// An empty token is the first page (offset 0). A malformed token fails
// loud with ErrInvalidRequest — never a silent reset.
func decodeCursor(c prototypes.TaskListCursor) (int, error) {
	if c.NextPageToken == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.NextPageToken)
	if err != nil {
		return 0, fmt.Errorf("%w: malformed cursor token", ErrInvalidRequest)
	}
	s := string(raw)
	if !strings.HasPrefix(s, cursorPrefix) {
		return 0, fmt.Errorf("%w: foreign cursor token", ErrInvalidRequest)
	}
	offset, err := strconv.Atoi(strings.TrimPrefix(s, cursorPrefix))
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("%w: cursor offset is invalid", ErrInvalidRequest)
	}
	return offset, nil
}
