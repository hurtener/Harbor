// Package events implements the Phase 72c `search.events` runtime-side
// index — a server-enforced search over the event bus's replay ring,
// scoped to the caller's identity triple unless the `auth.ScopeAdmin`
// claim is present (D-079).
//
// The Searcher consumes the `events.Replayer` capability (Phase 06)
// and the `events.Filter` server-enforced shape. Free-text search runs
// against the event header fields — type, source, identity. Substring
// search over event payload contents is post-V1 per the Phase 72c
// plan (would force materialisation of heavy payloads through the LLM-
// edge safety net; D-026).
package events

import (
	"context"
	"errors"
	"fmt"

	eventsubsys "github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
)

// Searcher serves the `search.events` index.
type Searcher struct {
	replayer eventsubsys.Replayer
	deps     search.Deps
}

// New constructs a Searcher.
func New(replayer eventsubsys.Replayer, deps search.Deps) (*Searcher, error) {
	if replayer == nil {
		return nil, fmt.Errorf("%w: nil Replayer", search.ErrInvalidRequest)
	}
	if err := deps.Validate(); err != nil {
		return nil, err
	}
	return &Searcher{replayer: replayer, deps: deps}, nil
}

// Index implements search.Searcher.
func (s *Searcher) Index() types.SearchIndex { return types.SearchIndexEvents }

// Search implements search.Searcher. The query matches against the
// event Type string. Facets honoured: `events.type` (exact event-type
// match), `events.tenant`, `events.session`, `events.run`. Time-window
// applies to event OccurredAt.
//
// V1 ships ONE filter shape per call (event Type set + identity scope)
// and fans across the caller's sessions when cross-session search is
// requested. The Admin scope is required for cross-tenant.
func (s *Searcher) Search(ctx context.Context, req types.SearchRequest) (types.SearchResponse, error) {
	callerID, ok := identity.From(ctx)
	if !ok {
		return types.SearchResponse{}, fmt.Errorf("%w: ctx carries no identity", search.ErrIdentityRequired)
	}
	if err := search.ValidateRequest(callerID, req); err != nil {
		return types.SearchResponse{}, err
	}
	crossTenant := search.CrossTenantRequested(callerID.TenantID, req)
	if crossTenant && !s.deps.AdminScope(ctx) {
		return types.SearchResponse{}, search.ErrCrossTenantRequiresAdmin
	}

	// Identity scope for the Replay filter. When the caller scopes
	// the search to a specific session, we pass that triple; the
	// default is the caller's own (tenant, user, session).
	scopeSession := callerID.SessionID
	scopeUser := callerID.UserID
	scopeTenant := callerID.TenantID
	if len(req.Filter.SessionIDs) > 0 {
		// V1 admits only a single-session-targeted replay per call;
		// the aggregate dispatcher can fan out by spawning multiple
		// requests. Post-V1 may add a multi-session predicate.
		scopeSession = req.Filter.SessionIDs[0]
	}
	if len(req.Filter.UserIDs) > 0 {
		scopeUser = req.Filter.UserIDs[0]
	}
	if len(req.Filter.TenantIDs) == 1 {
		scopeTenant = req.Filter.TenantIDs[0]
	}

	filter := eventsubsys.Filter{
		Tenant:  scopeTenant,
		User:    scopeUser,
		Session: scopeSession,
		Admin:   crossTenant,
	}
	for _, f := range req.Facets {
		if f.Key == "events.type" && f.Value != "" {
			filter.Types = append(filter.Types, eventsubsys.EventType(f.Value))
		}
		if f.Key == "events.run" && f.Value != "" {
			filter.Run = f.Value
		}
	}

	// Replay from the beginning to enumerate the live ring. Cursor
	// {Sequence:0} bypasses the ErrCursorTooOld check.
	evs, err := s.replayer.Replay(ctx, eventsubsys.Cursor{SessionID: scopeSession, Sequence: 0}, filter)
	if err != nil {
		// ErrReplayUnavailable is acceptable degradation — the index
		// returns an empty page with a TotalCount of 0; the caller
		// learns there's no replay capability via the empty Rows.
		if errors.Is(err, eventsubsys.ErrReplayUnavailable) {
			return types.SearchResponse{
				Rows:            []types.SearchResultRow{},
				Page:            1,
				PageSize:        types.DefaultSearchPageSize,
				PageCount:       1,
				TotalCount:      0,
				HasMore:         false,
				ProtocolVersion: types.ProtocolVersion,
			}, nil
		}
		return types.SearchResponse{}, fmt.Errorf("search.events: replay: %w", err)
	}

	rows := make([]types.SearchResultRow, 0, len(evs))
	for _, ev := range evs {
		if err := ctx.Err(); err != nil {
			return types.SearchResponse{}, err
		}
		if !search.TimeInWindow(ev.OccurredAt, req) {
			continue
		}
		// Type-based query: substring against the event type string.
		if !search.MatchesAnyField(req.Query, string(ev.Type), ev.Identity.RunID) {
			continue
		}
		preview := fmt.Sprintf("event %s at %s tenant=%s session=%s run=%s",
			ev.Type, ev.OccurredAt.Format("2006-01-02T15:04:05Z07:00"),
			ev.Identity.TenantID, ev.Identity.SessionID, ev.Identity.RunID)
		out, heavy, rerr := search.RedactAndCapPreview(ctx, s.deps.Redactor, preview)
		if rerr != nil {
			return types.SearchResponse{}, rerr
		}
		row := types.SearchResultRow{
			Index:      types.SearchIndexEvents,
			ID:         fmt.Sprintf("%s:%d", ev.Identity.SessionID, ev.Sequence),
			TenantID:   ev.Identity.TenantID,
			UserID:     ev.Identity.UserID,
			SessionID:  ev.Identity.SessionID,
			RunID:      ev.Identity.RunID,
			OccurredAt: ev.OccurredAt,
			Facets: map[string]string{
				"type": string(ev.Type),
			},
		}
		if heavy {
			row.Ref = &types.SearchArtifactRef{
				ID:        fmt.Sprintf("events/%s/%d", ev.Identity.SessionID, ev.Sequence),
				MimeType:  "application/json",
				SizeBytes: int64(len(preview)),
				Filename:  fmt.Sprintf("event-%d.json", ev.Sequence),
			}
		} else {
			row.Preview = out
		}
		rows = append(rows, row)
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

// Compile-time assertion.
var _ search.Searcher = (*Searcher)(nil)
