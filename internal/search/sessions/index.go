// Package sessions implements the Phase 72c `search.sessions`
// runtime-side index — a server-enforced search over session lifecycle
// records, scoped to the caller's identity triple unless the
// `auth.ScopeAdmin` claim is present (D-079).
//
// The Searcher consumes the narrow `sessions.SessionLister` interface
// (implemented by `*sessions.Registry`) — it does NOT re-implement
// listing or open the StateStore directly. Filtering, redaction,
// pagination, and heavy-payload bypass all run inside Search per the
// Phase 72c contract.
package sessions

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	sessionsubsys "github.com/hurtener/Harbor/internal/sessions"
)

// Searcher serves the `search.sessions` index. Built once at boot via
// New; immutable after construction (D-025). Safe for N concurrent
// Search calls against one instance.
type Searcher struct {
	lister sessionsubsys.SessionLister
	deps   search.Deps
}

// New constructs a Searcher. The SessionLister + search.Deps are both
// required; a nil either fails loud with ErrInvalidRequest rather than
// building a Searcher that would nil-panic on first Search.
func New(lister sessionsubsys.SessionLister, deps search.Deps) (*Searcher, error) {
	if lister == nil {
		return nil, fmt.Errorf("%w: nil SessionLister", search.ErrInvalidRequest)
	}
	if err := deps.Validate(); err != nil {
		return nil, err
	}
	return &Searcher{lister: lister, deps: deps}, nil
}

// Index implements search.Searcher.
func (s *Searcher) Index() types.SearchIndex { return types.SearchIndexSessions }

// Search implements search.Searcher. The query matches against the
// session's ID, tenant, user, and ClosedReason (the human-readable
// label for closed sessions). Facets honoured: `status` (open /
// closed). Time-window applies to the session's LastSeen.
//
// Identity rejection is loud (`search.ErrIdentityRequired`). Cross-
// tenant gating runs at construction-time-supplied AdminScope.
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

	// Build the lister filter. The lister is identity-blind beyond the
	// tenant/user/session inclusion filter; we apply the time window
	// at the lister boundary too.
	includeClosed := true
	for _, f := range req.Facets {
		if f.Key == "sessions.status" && f.Value == "open" {
			includeClosed = false
		}
	}
	snapshots, err := s.lister.ListSnapshots(ctx, sessionsubsys.SessionListFilter{
		TenantIDs:     tenants,
		UserIDs:       req.Filter.UserIDs,
		SessionIDs:    req.Filter.SessionIDs,
		SinceLastSeen: req.Filter.Since,
		UntilLastSeen: req.Filter.Until,
		IncludeClosed: includeClosed,
	})
	if err != nil {
		return types.SearchResponse{}, fmt.Errorf("search.sessions: list: %w", err)
	}

	rows := make([]types.SearchResultRow, 0, len(snapshots))
	for _, snap := range snapshots {
		if err := ctx.Err(); err != nil {
			return types.SearchResponse{}, err
		}
		// Apply facet filters that the lister did not enforce.
		facetSkip := false
		for _, f := range req.Facets {
			if f.Key == "sessions.status" {
				switch f.Value {
				case "open":
					if snap.Closed {
						facetSkip = true
					}
				case "closed":
					if !snap.Closed {
						facetSkip = true
					}
				}
			}
		}
		if facetSkip {
			continue
		}
		// Apply the free-text query against the session's textual fields.
		if !search.MatchesAnyField(req.Query,
			snap.ID,
			snap.Identity.TenantID,
			snap.Identity.UserID,
			snap.ClosedReason,
		) {
			continue
		}

		preview := buildPreview(snap)
		out, heavy, rerr := search.RedactAndCapPreview(ctx, s.deps.Redactor, preview)
		if rerr != nil {
			return types.SearchResponse{}, rerr
		}
		row := types.SearchResultRow{
			Index:      types.SearchIndexSessions,
			ID:         snap.ID,
			TenantID:   snap.Identity.TenantID,
			UserID:     snap.Identity.UserID,
			SessionID:  snap.ID,
			OccurredAt: snap.LastSeen,
			Facets: map[string]string{
				"status":  statusOf(snap),
				"running": boolStr(snap.Running),
			},
		}
		if heavy {
			row.Ref = &types.SearchArtifactRef{
				ID:        "sessions/" + snap.ID,
				MimeType:  "application/json",
				SizeBytes: int64(len(preview)),
				Filename:  snap.ID + ".session.json",
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

func buildPreview(s sessionsubsys.SessionSnapshot) string {
	state := "open"
	if s.Closed {
		state = "closed"
	}
	reason := ""
	if s.ClosedReason != "" {
		reason = " reason=" + s.ClosedReason
	}
	return fmt.Sprintf("session %s (%s) tenant=%s user=%s%s",
		s.ID, state, s.Identity.TenantID, s.Identity.UserID, reason)
}

func statusOf(s sessionsubsys.SessionSnapshot) string {
	if s.Closed {
		return "closed"
	}
	return "open"
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// Compile-time assertion.
var _ search.Searcher = (*Searcher)(nil)
