// Package artifacts implements the Phase 72c `search.artifacts`
// runtime-side index — a server-enforced search over the artifact
// store's catalog, scoped to the caller's identity triple unless the
// `auth.ScopeAdmin` claim is present (D-079).
//
// Every result row carries a populated `Ref` (artifacts are
// by-reference by construction per D-026); `Preview` is the redacted
// filename / mime summary.
package artifacts

import (
	"context"
	"fmt"

	artifactsubsys "github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
)

// Searcher serves the `search.artifacts` index.
type Searcher struct {
	store artifactsubsys.ArtifactStore
	deps  search.Deps
}

// New constructs a Searcher.
func New(store artifactsubsys.ArtifactStore, deps search.Deps) (*Searcher, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: nil ArtifactStore", search.ErrInvalidRequest)
	}
	if err := deps.Validate(); err != nil {
		return nil, err
	}
	return &Searcher{store: store, deps: deps}, nil
}

// Index implements search.Searcher.
func (s *Searcher) Index() types.SearchIndex { return types.SearchIndexArtifacts }

// Search implements search.Searcher. The query matches against the
// artifact ID, Filename, MimeType, and Namespace. Facets honoured:
// `artifacts.mime` (exact prefix match — e.g. `image/`), `artifacts.namespace`.
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

	var mimeFilter, nsFilter string
	for _, f := range req.Facets {
		switch f.Key {
		case "artifacts.mime":
			mimeFilter = f.Value
		case "artifacts.namespace":
			nsFilter = f.Value
		}
	}

	rows := make([]types.SearchResultRow, 0, 32)
	for _, tenant := range tenants {
		if err := ctx.Err(); err != nil {
			return types.SearchResponse{}, err
		}
		scope := artifactsubsys.ArtifactScope{TenantID: tenant}
		if len(req.Filter.UserIDs) > 0 {
			scope.UserID = req.Filter.UserIDs[0]
		}
		if len(req.Filter.SessionIDs) > 0 {
			scope.SessionID = req.Filter.SessionIDs[0]
		}
		refs, err := s.store.List(ctx, scope)
		if err != nil {
			return types.SearchResponse{}, fmt.Errorf("search.artifacts: list tenant=%s: %w", tenant, err)
		}
		for _, ref := range refs {
			if mimeFilter != "" && ref.MimeType != mimeFilter {
				continue
			}
			if nsFilter != "" && ref.Namespace != nsFilter {
				continue
			}
			if !search.MatchesAnyField(req.Query,
				ref.ID, ref.Filename, ref.MimeType, ref.Namespace,
			) {
				continue
			}
			preview := fmt.Sprintf("artifact %s mime=%s size=%d filename=%s",
				ref.ID, ref.MimeType, ref.SizeBytes, ref.Filename)
			out, _, rerr := search.RedactAndCapPreview(ctx, s.deps.Redactor, preview)
			if rerr != nil {
				return types.SearchResponse{}, rerr
			}
			rows = append(rows, types.SearchResultRow{
				Index:     types.SearchIndexArtifacts,
				ID:        ref.ID,
				TenantID:  ref.Scope.TenantID,
				UserID:    ref.Scope.UserID,
				SessionID: ref.Scope.SessionID,
				Preview:   out,
				Ref: &types.SearchArtifactRef{
					ID:        ref.ID,
					MimeType:  ref.MimeType,
					SizeBytes: ref.SizeBytes,
					Filename:  ref.Filename,
					SHA256:    ref.SHA256,
				},
				Facets: map[string]string{
					"mime":      ref.MimeType,
					"namespace": ref.Namespace,
				},
			})
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

// Compile-time assertion.
var _ search.Searcher = (*Searcher)(nil)
