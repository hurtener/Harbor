package protocol

import (
	"context"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// ttlExpiryWindow is the lookahead window the `HasTTLExpiring` facet
// and the `ExpiringIn1h` aggregate counter key on — records whose TTL
// expires within (now, now+1h].
const ttlExpiryWindow = time.Hour

// aggregateWindow is the lookback span the 24-hour `memory.*` event
// counters derive over.
const aggregateWindow = 24 * time.Hour

// aggregateBucket is the bucket width used for the 24-hour aggregate
// queries. It must evenly divide aggregateWindow; one hour gives 24
// buckets, which the List / Health counters sum.
const aggregateBucket = time.Hour

// ListDeps carries the dependencies List composes over. All are
// validated at the call site (the stream handler) — a nil Store fails
// loud rather than nil-panicking mid-projection.
type ListDeps struct {
	// Store is the memory subsystem the snapshot is projected from.
	Store memory.MemoryStore
	// Aggregator is the events Aggregator the 24-hour counters derive
	// from. Optional — when nil, the IdentityRejected24h /
	// RecoveryDropped24h counters are reported as 0 (the page still
	// renders; the right-rail cards subscribe to the live event stream
	// for the real-time view). The driver-comparison + record counters
	// do not depend on it.
	Aggregator *events.Aggregator
	// DriverName is the configured memory-driver name surfaced on each
	// row (`inmem` / `sqlite` / `postgres`). The MemoryStore interface
	// does not expose it; the caller supplies it from config.
	DriverName string
	// HeavyThreshold is the configured heavy-content byte size
	// (cfg.Artifacts.HeavyOutputThresholdBytes). It is the single
	// classification point for the per-row HeavyContent flag (D-026);
	// `memory.list` and `memory.get` MUST agree, so both read the same
	// threshold. A zero / non-positive value disables the flag (no row
	// is reported heavy) — the list still renders.
	HeavyThreshold int
}

// List answers the `memory.list` Protocol method: it projects the
// caller's per-identity memory snapshot into the Console-page row
// shape, applies the request's facet filters, paginates, and attaches
// the aggregate counters.
//
// Identity is mandatory (D-001): an incomplete triple on id fails
// loudly with `memory.ErrIdentityRequired`. The cross-tenant scope
// gate is the caller's job (the stream handler checks `auth.HasScope`
// before calling List) — by the time List runs the request is
// authorised; List filters strictly within the supplied identities.
//
// The filter's facets are validated up front: an unknown scope /
// driver / strategy enum, or a page / page-size out of range, fails
// loudly (ErrInvalidFilter / ErrPageOutOfRange) — never a silently
// dropped facet (CLAUDE.md §13).
func List(ctx context.Context, deps ListDeps, req prototypes.MemoryListRequest, id identity.Quadruple) (prototypes.MemoryListResponse, error) {
	if deps.Store == nil {
		return prototypes.MemoryListResponse{}, fmt.Errorf("memory/protocol: List: Store is nil")
	}
	if err := memory.ValidateIdentity(id); err != nil {
		return prototypes.MemoryListResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return prototypes.MemoryListResponse{}, err
	}

	page, pageSize, err := normalisePagination(req.Page, req.PageSize)
	if err != nil {
		return prototypes.MemoryListResponse{}, err
	}
	if err := validateFilterEnums(req.Filter); err != nil {
		return prototypes.MemoryListResponse{}, err
	}

	// Project the caller's per-identity snapshot into rows. The
	// MemoryStore surface is per-identity; the snapshot is the
	// caller's own session memory (CLAUDE.md §6 rule 4). Admin-scoped
	// cross-identity listing is a fan-out the caller arranges by
	// invoking List per identity; V1's surface lists the caller's
	// quadruple.
	snap, err := deps.Store.Snapshot(ctx, id)
	if err != nil {
		return prototypes.MemoryListResponse{}, fmt.Errorf("memory/protocol: List: snapshot: %w", err)
	}
	rows, err := snapshotTurns(snap, id, deps.DriverName, deps.HeavyThreshold)
	if err != nil {
		return prototypes.MemoryListResponse{}, err
	}

	now := time.Now().UTC()
	filtered := applyFilter(rows, req.Filter, id, now)
	sortByLastUpdatedDesc(filtered)

	total := len(filtered)
	pageRows := paginate(filtered, page, pageSize)
	items := make([]prototypes.MemoryItem, 0, len(pageRows))
	for _, r := range pageRows {
		items = append(items, r.item)
	}

	pageCount := 0
	if total > 0 {
		pageCount = (total + pageSize - 1) / pageSize
	}

	aggs := computeAggregates(ctx, deps.Aggregator, filtered, id, now)

	return prototypes.MemoryListResponse{
		Items:           items,
		Page:            page,
		PageSize:        pageSize,
		PageCount:       pageCount,
		TotalRows:       total,
		Aggregates:      aggs,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// normalisePagination validates page / page-size and applies the
// documented defaults. A negative page or page-size, or a page-size
// above the documented max, fails loudly — never a silent clamp
// (the silent clamp would defeat the per-row identity boundary the
// integration test asserts).
func normalisePagination(page, pageSize int) (int, int, error) {
	if page < 0 {
		return 0, 0, fmt.Errorf("%w: page %d is negative", ErrPageOutOfRange, page)
	}
	if pageSize < 0 {
		return 0, 0, fmt.Errorf("%w: page_size %d is negative", ErrPageOutOfRange, pageSize)
	}
	if pageSize > prototypes.MaxMemoryListPageSize {
		return 0, 0, fmt.Errorf("%w: page_size %d exceeds max %d",
			ErrPageOutOfRange, pageSize, prototypes.MaxMemoryListPageSize)
	}
	if page == 0 {
		page = 1
	}
	if pageSize == 0 {
		pageSize = prototypes.DefaultMemoryListPageSize
	}
	return page, pageSize, nil
}

// validateFilterEnums rejects an unknown scope / driver / strategy enum
// on the filter. An out-of-set value fails loudly so a client never
// sees a silently-ignored facet.
func validateFilterEnums(f prototypes.MemoryFilter) error {
	for _, s := range f.Scopes {
		if !prototypes.IsValidMemoryScope(prototypes.MemoryScope(s)) {
			return fmt.Errorf("%w: unknown scope %q", ErrInvalidFilter, s)
		}
	}
	for _, d := range f.Drivers {
		if !prototypes.IsValidMemoryDriver(prototypes.MemoryDriverName(d)) {
			return fmt.Errorf("%w: unknown driver %q", ErrInvalidFilter, d)
		}
	}
	for _, st := range f.Strategies {
		if !prototypes.IsValidMemoryStrategy(prototypes.MemoryStrategyName(st)) {
			return fmt.Errorf("%w: unknown strategy %q", ErrInvalidFilter, st)
		}
	}
	return nil
}

// applyFilter narrows the projected rows by every facet axis on the
// filter. Each axis is an AND: a row survives only if it matches every
// non-empty facet.
func applyFilter(rows []projectedTurn, f prototypes.MemoryFilter, id identity.Quadruple, now time.Time) []projectedTurn {
	out := make([]projectedTurn, 0, len(rows))
	for _, r := range rows {
		if !matchStringSet(f.Scopes, r.item.Scope) {
			continue
		}
		if !matchStringSet(f.Drivers, r.item.Driver) {
			continue
		}
		if !matchStringSet(f.Strategies, r.item.Strategy) {
			continue
		}
		if !matchStringSet(f.SessionIDs, r.item.Identity.Session) {
			continue
		}
		if !matchStringSet(f.UserIDs, r.item.Identity.User) {
			continue
		}
		if !matchStringSet(f.TenantIDs, r.item.Identity.Tenant) {
			continue
		}
		if len(f.AgentIDs) > 0 && !matchStringSet(f.AgentIDs, r.item.AgentID) {
			continue
		}
		if f.HasTTLExpiring && !ttlExpiringWithin(r.item.ExpiresAt, now, ttlExpiryWindow) {
			continue
		}
		if f.ContentSearch != "" && !containsFold(string(r.value), f.ContentSearch) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// matchStringSet reports whether candidate is in set, treating an empty
// set as "match everything" (the facet was not supplied).
func matchStringSet(set []string, candidate string) bool {
	if len(set) == 0 {
		return true
	}
	for _, s := range set {
		if s == candidate {
			return true
		}
	}
	return false
}

// ttlExpiringWithin reports whether expiresAt falls within (now,
// now+window]. A zero ExpiresAt (no TTL) never matches.
func ttlExpiringWithin(expiresAt, now time.Time, window time.Duration) bool {
	if expiresAt.IsZero() {
		return false
	}
	return expiresAt.After(now) && !expiresAt.After(now.Add(window))
}

// paginate slices the 1-based page out of rows. An out-of-range page
// yields an empty slice (the response still carries the real
// TotalRows / PageCount so the Console renders the correct pager).
func paginate(rows []projectedTurn, page, pageSize int) []projectedTurn {
	start := (page - 1) * pageSize
	if start >= len(rows) {
		return nil
	}
	end := start + pageSize
	if end > len(rows) {
		end = len(rows)
	}
	return rows[start:end]
}

// computeAggregates builds the page-level counters. Total / ExpiringIn1h
// derive from the in-hand filtered rows; the 24-hour event counters
// derive from the events Aggregator (when wired).
func computeAggregates(ctx context.Context, agg *events.Aggregator, rows []projectedTurn, id identity.Quadruple, now time.Time) prototypes.MemoryAggregates {
	expiring := int64(0)
	for _, r := range rows {
		if ttlExpiringWithin(r.item.ExpiresAt, now, ttlExpiryWindow) {
			expiring++
		}
	}
	rejected, dropped := eventCounters(ctx, agg, id)
	return prototypes.MemoryAggregates{
		Total:               int64(len(rows)),
		ExpiringIn1h:        expiring,
		IdentityRejected24h: rejected,
		RecoveryDropped24h:  dropped,
	}
}

// eventCounters sums the 24-hour counts of `memory.identity_rejected`
// (D-033) and `memory.recovery_dropped` (D-035) events from the events
// Aggregator. A nil Aggregator — or a forward-only bus without replay
// — yields (0, 0): the page still renders and the right-rail cards
// subscribe to the live stream for the real-time view. The aggregate
// failure is non-fatal for the page; List never fails because the
// counters could not be computed.
//
// The counters are scoped to the caller's TENANT only — NOT the full
// triple. A `memory.identity_rejected` event by construction carries a
// partial identity with `<missing>` substituted for the empty
// component(s) (D-033), so a triple-scoped filter would never match a
// rejection whose session was the missing component. Tenant-scoping
// keeps the rejection count visible while preserving the tenant
// isolation boundary (CLAUDE.md §6 — the tenant is still the outer
// boundary; cross-tenant fan-in still requires the D-079 scope claim,
// enforced at the wire edge before List runs).
func eventCounters(ctx context.Context, agg *events.Aggregator, id identity.Quadruple) (rejected, dropped int64) {
	if agg == nil {
		return 0, 0
	}
	resp, err := agg.Aggregate(ctx, prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{
			EventTypes: []string{
				string(memory.EventTypeMemoryIdentityRejected),
				string(memory.EventTypeMemoryRecoveryDropped),
			},
			TenantIDs: []string{id.TenantID},
		},
		Window: aggregateWindow,
		Bucket: aggregateBucket,
	})
	if err != nil {
		// Replay-unavailable / cancelled — the page-level counters
		// degrade to 0; the live event stream is the real-time
		// source. This is NOT silent degradation of a load-bearing
		// path: the rejection EVENTS still surface verbatim on the
		// right-rail card (D-033), only the rolled-up 24h count is
		// best-effort.
		return 0, 0
	}
	for _, b := range resp.Buckets {
		rejected += b.Counts[string(memory.EventTypeMemoryIdentityRejected)]
		dropped += b.Counts[string(memory.EventTypeMemoryRecoveryDropped)]
	}
	return rejected, dropped
}
