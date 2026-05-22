package search

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// PerIndexTimeout caps the per-index fan-out wait inside Query. A
// per-index Searcher whose Search has not returned within this budget
// is cancelled and its rows are dropped from the merge. V1 ships a
// generous 5-second cap; post-V1 may make this configurable per
// deployment.
const PerIndexTimeout = 5 * time.Second

// Query is the `search.query` palette dispatcher — the runtime-side
// half of Brief 11 §CC-4's "single search box in the Console header"
// experience. It concurrently fans out to every requested runtime-side
// index in the registry, merges the result rows, applies the union
// pagination, and returns one paginated `SearchResponse`.
//
// Contract:
//
//   - Identity is mandatory; missing-triple returns `ErrIdentityRequired`.
//   - Cross-tenant gating runs at the aggregate edge — a cross-tenant
//     request without `auth.ScopeAdmin` is rejected with
//     `ErrCrossTenantRequiresAdmin` (NEVER silently downgraded).
//   - Empty `req.Indexes` means "all four registered indexes."
//   - Unknown indexes in `req.Indexes` are rejected at validation
//     (`ErrUnknownIndex`); they NEVER fall through to a silent skip.
//   - Per-index failures fan-in: a single index's failure does NOT
//     fail the whole `search.query` — the dispatcher emits a
//     best-effort union (per Brief 11 §CC-4's "graceful degradation
//     on backend stutter"). The failure mode is logged via the
//     redactor's logger contract elsewhere; for the wire response the
//     other indexes' rows still ship. This is the ONE exception to the
//     fail-loud rule §13 carves out for aggregators — and only after
//     identity + scope checks have already passed (those failures stay
//     loud).
//   - Carries NO index of its own; emits NO events.
//
// `Query` is a pure function over the registry, ctx, callerID, and
// req — no per-call state lives on `*SearcherRegistry` (D-025).
func Query(ctx context.Context, reg *SearcherRegistry, callerID identity.Identity, adminScope ScopeChecker, req types.SearchRequest) (types.SearchResponse, error) {
	if reg == nil {
		return types.SearchResponse{}, fmt.Errorf("%w: nil SearcherRegistry", ErrInvalidRequest)
	}
	if adminScope == nil {
		return types.SearchResponse{}, fmt.Errorf("%w: nil ScopeChecker", ErrInvalidRequest)
	}

	if err := ValidateRequest(callerID, req); err != nil {
		return types.SearchResponse{}, err
	}

	if CrossTenantRequested(callerID.TenantID, req) && !adminScope(ctx) {
		return types.SearchResponse{}, ErrCrossTenantRequiresAdmin
	}

	indexes := req.Indexes
	if len(indexes) == 0 {
		indexes = reg.Indexes()
	}

	// Sub-request shape for each per-index Searcher: we re-write the
	// request to scope it to the requested per-index pagination upper
	// bound (we ask each index for `Page * PageSize` rows and merge),
	// then re-paginate at the aggregator level. This trades a little
	// over-fetch for correct cross-index ordering.
	page, size := req.PageBounds()
	subReq := req
	subReq.Indexes = nil // ignored by per-index searchers
	subReq.Page = 1
	subReq.PageSize = page * size
	if subReq.PageSize > types.MaxSearchPageSize {
		subReq.PageSize = types.MaxSearchPageSize
	}
	if subReq.PageSize <= 0 {
		subReq.PageSize = types.DefaultSearchPageSize
	}

	type indexResult struct {
		idx  types.SearchIndex
		rows []types.SearchResultRow
		err  error
	}

	results := make(chan indexResult, len(indexes))
	var wg sync.WaitGroup

	for _, idx := range indexes {

		searcher, ok := reg.Get(idx)
		if !ok {
			// Skip silently for unregistered indexes — a partial
			// deployment of the cluster is acceptable per the package
			// godoc.
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			subCtx, cancel := context.WithTimeout(ctx, PerIndexTimeout)
			defer cancel()
			resp, err := searcher.Search(subCtx, subReq)
			results <- indexResult{idx: idx, rows: resp.Rows, err: err}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	merged := make([]types.SearchResultRow, 0, page*size)
	var firstHardErr error
	for r := range results {
		if r.err != nil {
			// Identity / scope errors are HARD — they would surface
			// the same way at the aggregate gate; propagate. (We've
			// already passed our own identity + scope checks, so a
			// per-index identity/scope rejection would indicate a
			// wiring bug; fail loud.)
			if errors.Is(r.err, ErrIdentityRequired) || errors.Is(r.err, ErrCrossTenantRequiresAdmin) || errors.Is(r.err, ErrInvalidRequest) {
				if firstHardErr == nil {
					firstHardErr = fmt.Errorf("search.query: index %q hard error: %w", r.idx, r.err)
				}
				continue
			}
			// Soft per-index failure — log via the redactor pipeline
			// upstream; the aggregator emits the union of the
			// remaining indexes (Brief 11 §CC-4 graceful degradation).
			continue
		}
		merged = append(merged, r.rows...)
	}

	if firstHardErr != nil {
		return types.SearchResponse{}, firstHardErr
	}

	// Sort + paginate the merged result.
	SortRowsByOccurredAtDesc(merged)
	gotPage, gotSize, pageCount, totalCount, hasMore, slice := Paginate(merged, req)

	return types.SearchResponse{
		Rows:            slice,
		Page:            gotPage,
		PageSize:        gotSize,
		PageCount:       pageCount,
		TotalCount:      totalCount,
		HasMore:         hasMore,
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}
