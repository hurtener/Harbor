// Package events implements the `search.events` runtime-side
// index — a server-enforced search over the event bus's replay ring,
// scoped to the caller's own tenant AND own user unless an admin-tier
// claim widens it.
//
// The Searcher consumes the `events.Replayer` capability
// and the `events.Filter` server-enforced shape. Free-text search runs
// against the event header fields — type, source, identity. Substring
// search over event payload contents is post-V1 per the
// plan (would force materialisation of heavy payloads through the LLM-
// edge safety net).
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
	// The identity-axis gate, read once for all three axes.
	elevated := s.deps.AdminScope(ctx)
	crossTenant := search.CrossTenantRequested(callerID.TenantID, req)
	crossUser := search.CrossUserRequested(callerID.UserID, req)
	if !elevated {
		if crossTenant {
			return types.SearchResponse{}, search.ErrCrossTenantRequiresAdmin
		}
		if crossUser {
			return types.SearchResponse{}, search.ErrCrossUserRequiresAdmin
		}
		if search.CrossSessionFanInRequested(req) {
			return types.SearchResponse{}, search.ErrCrossSessionRequiresAdmin
		}
	}

	// Identity scope for the Replay filter. When the caller scopes
	// the search to a specific session, we pass that triple; the
	// default is the caller's own (tenant, user, session).
	//
	// The user axis FOLDS to the caller's own when the filter elides it —
	// this index is the one that already defaulted to the caller and so
	// never leaked on elision, and the fold makes that a decision rather
	// than a coincidence. The fold is KEPT on an ordinary widened read:
	// the bus filter is single-valued, and its fan-in flag WRITES an
	// admin-scope notice into the ring, so an elided axis is not turned
	// into an unrequested deployment-wide replay here.
	//
	// It is released on exactly ONE input: a read the caller ALREADY
	// widened across tenants. Folding there is not a narrower answer, it
	// is a WRONG one — the caller's own user id is a principal of the
	// caller's OWN tenant, so bounding a foreign tenant's rows by it
	// matches nothing and the granted, explicitly-requested crossing
	// answers an empty page indistinguishable from "that tenant has no
	// events". That silent-empty is the failure this surface's identity
	// bound exists to prevent, so the crossing widens instead. Nothing is
	// widened that the request did not ask for: the fan-in flag and the
	// admin-scope notice are already set by the tenant crossing itself,
	// and the per-row tenant bound below still holds the read to the
	// tenants the caller named.
	users := search.EffectiveUserSet(callerID.UserID, req)
	if elevated && crossTenant {
		users = search.WidenedUserSet(req)
	}
	scopeSession := callerID.SessionID
	// A widened read with an elided user axis has no single user to name;
	// the bus's fan-in flag makes the named user irrelevant on that path,
	// and the per-row bound below is what actually scopes it.
	scopeUser := callerID.UserID
	if len(users) > 0 {
		scopeUser = users[0]
	}
	scopeTenant := callerID.TenantID
	if len(req.Filter.SessionIDs) > 0 {
		// V1 admits only a single-session-targeted replay per call;
		// the aggregate dispatcher can fan out by spawning multiple
		// requests. Post-V1 may add a multi-session predicate.
		scopeSession = req.Filter.SessionIDs[0]
	}
	if len(req.Filter.TenantIDs) == 1 {
		scopeTenant = req.Filter.TenantIDs[0]
	}

	// `Filter.Matches` short-circuits its whole identity comparison when
	// Admin is set, so the flag MUST be set for a granted cross-user read
	// — otherwise the bus would match the widened read against the single
	// scoped user and answer nothing, which looks exactly like a working
	// gate while the widening is silently inert.
	filter := eventsubsys.Filter{
		Tenant:  scopeTenant,
		User:    scopeUser,
		Session: scopeSession,
		Admin:   crossTenant || crossUser,
	}
	// The same short-circuit is why a widened read re-applies its axis
	// bounds per row below: an Admin filter fans across EVERY identity in
	// the ring, so a read widened on ONE axis would otherwise also cross
	// the other. The bounds are the effective sets, so the fan is exactly
	// as wide as the request asked for and no wider.
	tenantBound := setOf(search.EffectiveTenantSet(callerID.TenantID, req))
	// An EMPTY user bound is the wildcard, and it is reachable from exactly
	// one input: the widened cross-tenant read whose user axis was elided.
	// Every other path produces a non-empty set (the fold guarantees at
	// least the caller), so the bound never degrades into "match nothing"
	// and never silently becomes "match everything".
	userBound := setOf(users)
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
		if filter.Admin {
			if _, ok := tenantBound[ev.Identity.TenantID]; !ok {
				continue
			}
			if len(userBound) > 0 {
				if _, ok := userBound[ev.Identity.UserID]; !ok {
					continue
				}
			}
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

// setOf builds a membership set over an effective identity-axis set. A
// per-call value; nothing package-level is mutated.
func setOf(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		out[v] = struct{}{}
	}
	return out
}

// Compile-time assertion.
var _ search.Searcher = (*Searcher)(nil)
