// Package tasks implements the Phase 72c `search.tasks` runtime-side
// index — a server-enforced search over task lifecycle records, scoped
// to the caller's identity triple unless the `auth.ScopeAdmin` claim
// is present (D-079).
//
// The Searcher consumes the public `tasks.TaskRegistry.List` surface
// per session, fanning across the sessions visible to the caller (via
// `sessions.SessionLister`). For deployments with millions of sessions
// this is a linear scan; the wire shape stays index-strategy-agnostic
// so a post-V1 FTS sidecar can swap in without a Protocol change.
package tasks

import (
	"context"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	sessionsubsys "github.com/hurtener/Harbor/internal/sessions"
	tasksubsys "github.com/hurtener/Harbor/internal/tasks"
)

func unixNanoToTime(ns int64) time.Time {
	if ns <= 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

// Searcher serves the `search.tasks` index.
type Searcher struct {
	lister sessionsubsys.SessionLister
	tasks  tasksubsys.TaskRegistry
	deps   search.Deps
}

// New constructs a Searcher.
func New(lister sessionsubsys.SessionLister, taskReg tasksubsys.TaskRegistry, deps search.Deps) (*Searcher, error) {
	if lister == nil {
		return nil, fmt.Errorf("%w: nil SessionLister", search.ErrInvalidRequest)
	}
	if taskReg == nil {
		return nil, fmt.Errorf("%w: nil TaskRegistry", search.ErrInvalidRequest)
	}
	if err := deps.Validate(); err != nil {
		return nil, err
	}
	return &Searcher{lister: lister, tasks: taskReg, deps: deps}, nil
}

// Index implements search.Searcher.
func (s *Searcher) Index() types.SearchIndex { return types.SearchIndexTasks }

// Search implements search.Searcher. Free-text query matches against
// the task's ID, Description, Query, Kind. Facets honoured:
// `tasks.status`, `tasks.kind`. Time-window applies to UpdatedAt.
func (s *Searcher) Search(ctx context.Context, req types.SearchRequest) (types.SearchResponse, error) {
	callerID, ok := identity.From(ctx)
	if !ok {
		return types.SearchResponse{}, fmt.Errorf("%w: ctx carries no identity", search.ErrIdentityRequired)
	}
	if err := search.ValidateRequest(callerID, req); err != nil {
		return types.SearchResponse{}, err
	}
	if search.CrossTenantRequested(callerID.TenantID, req) && !s.deps.AdminScope(ctx) {
		return types.SearchResponse{}, search.ErrCrossTenantRequiresAdmin
	}

	tenants := search.EffectiveTenantSet(callerID.TenantID, req)
	snapshots, err := s.lister.ListSnapshots(ctx, sessionsubsys.SessionListFilter{
		TenantIDs:     tenants,
		UserIDs:       req.Filter.UserIDs,
		SessionIDs:    req.Filter.SessionIDs,
		IncludeClosed: true,
	})
	if err != nil {
		return types.SearchResponse{}, fmt.Errorf("search.tasks: list sessions: %w", err)
	}

	statusFilter, kindFilter := parseFacets(req.Facets)

	rows := make([]types.SearchResultRow, 0, 32)
	for _, snap := range snapshots {
		if err := ctx.Err(); err != nil {
			return types.SearchResponse{}, err
		}
		filter := tasksubsys.TaskFilter{}
		if statusFilter != nil {
			ts := *statusFilter
			filter.Status = &ts
		}
		if kindFilter != nil {
			tk := *kindFilter
			filter.Kind = &tk
		}
		summaries, err := s.tasks.List(ctx, snap.Identity, filter)
		if err != nil {
			// A closed session may have no live task surface in some
			// drivers; skip rather than failing the whole search.
			continue
		}
		for _, sum := range summaries {
			t, err := s.tasksGetWithIdentity(ctx, snap.Identity, sum.ID)
			if err != nil {
				continue
			}
			if !search.MatchesAnyField(req.Query,
				string(t.ID),
				t.Description,
				t.Query,
				string(t.Kind),
				string(t.Status),
			) {
				continue
			}
			occurredAt := unixNanoToTime(t.UpdatedAt)
			if !search.TimeInWindow(occurredAt, req) {
				continue
			}
			preview := fmt.Sprintf("task %s status=%s kind=%s desc=%s",
				t.ID, t.Status, t.Kind, t.Description)
			out, heavy, rerr := search.RedactAndCapPreview(ctx, s.deps.Redactor, preview)
			if rerr != nil {
				return types.SearchResponse{}, rerr
			}
			row := types.SearchResultRow{
				Index:      types.SearchIndexTasks,
				ID:         string(t.ID),
				TenantID:   t.Identity.TenantID,
				UserID:     t.Identity.UserID,
				SessionID:  t.Identity.SessionID,
				RunID:      t.Identity.RunID,
				OccurredAt: occurredAt,
				Facets: map[string]string{
					"status": string(t.Status),
					"kind":   string(t.Kind),
				},
			}
			if heavy {
				row.Ref = &types.SearchArtifactRef{
					ID:        "tasks/" + string(t.ID),
					MimeType:  "application/json",
					SizeBytes: int64(len(preview)),
					Filename:  string(t.ID) + ".task.json",
				}
			} else {
				row.Preview = out
			}
			rows = append(rows, row)
		}
	}

	search.SortRowsByOccurredAtDesc(rows)
	page, size, pageCount, total, hasMore, slice := search.Paginate(rows, req)
	return types.SearchResponse{
		Rows:            slice,
		Page:            page,
		PageSize:        size,
		PageCount:       pageCount,
		TotalCount:      total,
		HasMore:         hasMore,
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// tasksGetWithIdentity calls TaskRegistry.Get under a ctx carrying the
// session's identity so the per-tenant gate inside Get is satisfied.
func (s *Searcher) tasksGetWithIdentity(ctx context.Context, ident identity.Identity, id tasksubsys.TaskID) (*tasksubsys.Task, error) {
	subCtx, err := identity.With(ctx, ident)
	if err != nil {
		return nil, err
	}
	return s.tasks.Get(subCtx, id)
}

func parseFacets(facets []types.SearchFacet) (*tasksubsys.TaskStatus, *tasksubsys.TaskKind) {
	var status *tasksubsys.TaskStatus
	var kind *tasksubsys.TaskKind
	for _, f := range facets {
		switch f.Key {
		case "tasks.status":
			ts := tasksubsys.TaskStatus(f.Value)
			status = &ts
		case "tasks.kind":
			tk := tasksubsys.TaskKind(f.Value)
			kind = &tk
		}
	}
	return status, kind
}

// Compile-time assertion.
var _ search.Searcher = (*Searcher)(nil)
