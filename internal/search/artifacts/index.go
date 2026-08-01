// Package artifacts implements the `search.artifacts`
// runtime-side index — a server-enforced search over the artifact
// store's catalog, scoped to the caller's own tenant AND own user unless
// an admin-tier claim widens it.
//
// Every result row carries a populated `Ref` (artifacts are
// by-reference by construction); `Preview` is the redacted
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
	decision, err := s.deps.AuthorizeScope(ctx, s.Index(), callerID, req)
	if err != nil {
		return types.SearchResponse{}, err
	}
	elevated := decision.Elevated

	tenants := search.EffectiveTenantSet(callerID.TenantID, req)
	// The user axis FOLDS for an unwidened caller. The store's `List`
	// precondition requires only the tenant and documents an empty
	// `UserID` as a WILDCARD — correct at the store, since a list filter
	// is a predicate over a result set rather than an identity. Deciding
	// what an omitted component MEANS is this calling surface's job, and
	// the answer here is "the caller's own", never "everyone".
	users := search.EffectiveUserSet(callerID.UserID, req)
	if elevated {
		users = search.WidenedUserSet(req)
	}
	// A widened read with an elided user axis is the ONE case where the
	// store's wildcard is the right answer: one pass with an empty
	// `UserID` is the tenant-wide catalog a fleet view asks for.
	userScopes := users
	if len(userScopes) == 0 {
		userScopes = []string{""}
	}

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
		for _, user := range userScopes {
			if err := ctx.Err(); err != nil {
				return types.SearchResponse{}, err
			}
			// One store call per (tenant, user) pair. Reading only the
			// first named user would silently drop users 2..N of a
			// widened read; the store call shape is unchanged.
			scope := artifactsubsys.ArtifactScope{TenantID: tenant, UserID: user}
			if len(req.Filter.SessionIDs) > 0 {
				scope.SessionID = req.Filter.SessionIDs[0]
			}
			refs, err := s.store.List(ctx, scope)
			if err != nil {
				return types.SearchResponse{}, fmt.Errorf("search.artifacts: list tenant=%s user=%s: %w", tenant, user, err)
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
				out, heavy, rerr := search.RedactAndCapPreview(ctx, s.deps.Redactor, preview)
				if rerr != nil {
					return types.SearchResponse{}, rerr
				}
				row := types.SearchResultRow{
					Index:     types.SearchIndexArtifacts,
					ID:        ref.ID,
					TenantID:  ref.Scope.TenantID,
					UserID:    ref.Scope.UserID,
					SessionID: ref.Scope.SessionID,
					// Every artifact row carries a Ref unconditionally —
					// artifacts are by-reference by construction — so the
					// heavy arm below only decides whether a PREVIEW ships
					// beside it, never whether the row is addressable.
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
				}
				if !heavy {
					// Bound rather than discarded, so this call site can
					// tell a CAPPED preview from an empty one — its three
					// siblings all bind it, and a discard made the row
					// shape correct by coincidence instead of by
					// construction.
					row.Preview = out
				}
				rows = append(rows, row)
			}
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
