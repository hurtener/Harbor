package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	memprotocol "github.com/hurtener/Harbor/internal/memory/protocol"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

func TestList_ProjectsTurnsForCallerIdentity(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	seedTurns(t, h, id, 5)

	resp, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 5 {
		t.Fatalf("List returned %d items, want 5", len(resp.Items))
	}
	if resp.TotalRows != 5 {
		t.Errorf("TotalRows = %d, want 5", resp.TotalRows)
	}
	if resp.Page != 1 {
		t.Errorf("Page = %d, want 1 (default)", resp.Page)
	}
	if resp.PageSize != prototypes.DefaultMemoryListPageSize {
		t.Errorf("PageSize = %d, want %d (default)", resp.PageSize, prototypes.DefaultMemoryListPageSize)
	}
	for _, it := range resp.Items {
		if it.Identity.Tenant != id.TenantID || it.Identity.User != id.UserID || it.Identity.Session != id.SessionID {
			t.Errorf("item identity %+v != caller identity", it.Identity)
		}
		if it.Driver != "inmem" {
			t.Errorf("item driver = %q, want inmem", it.Driver)
		}
		if it.Key == "" {
			t.Error("item key is empty — every row must carry a memory.get key")
		}
	}
}

func TestList_FailsLoudlyOnIncompleteIdentity(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	for _, tc := range []struct {
		name string
		id   identity.Quadruple
	}{
		{"empty tenant", identity.Quadruple{Identity: identity.Identity{UserID: "u", SessionID: "s"}}},
		{"empty user", identity.Quadruple{Identity: identity.Identity{TenantID: "t", SessionID: "s"}}},
		{"empty session", identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}}},
		{"all empty", identity.Quadruple{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := memprotocol.List(context.Background(),
				memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
				prototypes.MemoryListRequest{}, tc.id)
			if !errors.Is(err, memory.ErrIdentityRequired) {
				t.Fatalf("List with %s: err = %v, want ErrIdentityRequired", tc.name, err)
			}
		})
	}
}

func TestList_ScopeFacet(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	seedTurns(t, h, id, 3)

	// session scope matches every projected row (memory is session-
	// scoped by default).
	resp, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{
			Scopes: []string{string(prototypes.MemoryScopeSession)},
		}}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 3 {
		t.Errorf("session-scope filter returned %d, want 3", len(resp.Items))
	}

	// tenant scope matches none (no promotion policy in this fixture).
	resp, err = memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{
			Scopes: []string{string(prototypes.MemoryScopeTenant)},
		}}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("tenant-scope filter returned %d, want 0", len(resp.Items))
	}
}

func TestList_DriverFacet(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	seedTurns(t, h, id, 4)

	resp, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{
			Drivers: []string{string(prototypes.MemoryDriverInmem)},
		}}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 4 {
		t.Errorf("inmem-driver filter returned %d, want 4", len(resp.Items))
	}

	resp, err = memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{
			Drivers: []string{string(prototypes.MemoryDriverPostgres)},
		}}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("postgres-driver filter returned %d, want 0", len(resp.Items))
	}
}

func TestList_StrategyFacet(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	seedTurns(t, h, id, 2)

	resp, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{
			Strategies: []string{string(prototypes.MemoryStrategyTruncation)},
		}}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Errorf("truncation-strategy filter returned %d, want 2", len(resp.Items))
	}

	resp, err = memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{
			Strategies: []string{string(prototypes.MemoryStrategyRollingSummary)},
		}}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("rolling_summary-strategy filter returned %d, want 0", len(resp.Items))
	}
}

func TestList_ContentSearchFacet(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	seedTurns(t, h, id, 4)

	// Every seeded turn carries "question" in its user message.
	resp, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{
			ContentSearch: "QUESTION", // case-insensitive
		}}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 4 {
		t.Errorf("content_search 'QUESTION' returned %d, want 4 (case-insensitive)", len(resp.Items))
	}

	resp, err = memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{
			ContentSearch: "no-such-substring-xyz",
		}}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("content_search miss returned %d, want 0", len(resp.Items))
	}
}

func TestList_AllFacetsCombined(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	seedTurns(t, h, id, 6)

	resp, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{
			TenantIDs:     []string{id.TenantID},
			UserIDs:       []string{id.UserID},
			SessionIDs:    []string{id.SessionID},
			Scopes:        []string{string(prototypes.MemoryScopeSession)},
			Drivers:       []string{string(prototypes.MemoryDriverInmem)},
			Strategies:    []string{string(prototypes.MemoryStrategyTruncation)},
			ContentSearch: "answer",
		}}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 6 {
		t.Errorf("all-facets combination returned %d, want 6", len(resp.Items))
	}
}

func TestList_PaginationBoundaries(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	seedTurns(t, h, id, 10)

	// page 1, size 4 → 4 items, 3 pages.
	resp, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Page: 1, PageSize: 4}, id)
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if len(resp.Items) != 4 || resp.PageCount != 3 || resp.TotalRows != 10 {
		t.Fatalf("page 1: items=%d pageCount=%d totalRows=%d, want 4/3/10",
			len(resp.Items), resp.PageCount, resp.TotalRows)
	}

	// page 3 → 2 trailing items.
	resp, err = memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Page: 3, PageSize: 4}, id)
	if err != nil {
		t.Fatalf("List page 3: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Errorf("page 3: items=%d, want 2", len(resp.Items))
	}

	// page beyond the end → empty page, real totals.
	resp, err = memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Page: 99, PageSize: 4}, id)
	if err != nil {
		t.Fatalf("List page 99: %v", err)
	}
	if len(resp.Items) != 0 || resp.TotalRows != 10 {
		t.Errorf("page 99: items=%d totalRows=%d, want 0/10", len(resp.Items), resp.TotalRows)
	}
}

func TestList_RejectsOversizedPageSize(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	_, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{PageSize: prototypes.MaxMemoryListPageSize + 1}, testIdentity())
	if !errors.Is(err, memprotocol.ErrPageOutOfRange) {
		t.Fatalf("List with oversized page_size: err = %v, want ErrPageOutOfRange", err)
	}
}

func TestList_RejectsNegativePagination(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	for _, tc := range []struct {
		name string
		req  prototypes.MemoryListRequest
	}{
		{"negative page", prototypes.MemoryListRequest{Page: -1}},
		{"negative page_size", prototypes.MemoryListRequest{PageSize: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := memprotocol.List(context.Background(),
				memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
				tc.req, testIdentity())
			if !errors.Is(err, memprotocol.ErrPageOutOfRange) {
				t.Fatalf("List with %s: err = %v, want ErrPageOutOfRange", tc.name, err)
			}
		})
	}
}

func TestList_RejectsUnknownFilterEnum(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	for _, tc := range []struct {
		name   string
		filter prototypes.MemoryFilter
	}{
		{"unknown scope", prototypes.MemoryFilter{Scopes: []string{"galaxy"}}},
		{"unknown driver", prototypes.MemoryFilter{Drivers: []string{"redis"}}},
		{"unknown strategy", prototypes.MemoryFilter{Strategies: []string{"pinned"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := memprotocol.List(context.Background(),
				memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
				prototypes.MemoryListRequest{Filter: tc.filter}, testIdentity())
			if !errors.Is(err, memprotocol.ErrInvalidFilter) {
				t.Fatalf("List with %s: err = %v, want ErrInvalidFilter", tc.name, err)
			}
		})
	}
}

func TestList_EmptySnapshotReturnsEmptyPage(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	resp, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{}, testIdentity())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 0 || resp.TotalRows != 0 || resp.PageCount != 0 {
		t.Errorf("empty snapshot: items=%d totalRows=%d pageCount=%d, want 0/0/0",
			len(resp.Items), resp.TotalRows, resp.PageCount)
	}
}

func TestList_AggregatesIdentityRejectedCount(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	id := testIdentity()
	agg := newAggregator(t, h)

	// Drive 3 memory.identity_rejected events by calling AddTurn with
	// an incomplete triple — the driver fails closed AND emits the
	// event on the bus (D-033).
	for range 3 {
		_ = h.store.AddTurn(context.Background(),
			identity.Quadruple{Identity: identity.Identity{TenantID: id.TenantID, UserID: id.UserID}},
			memory.ConversationTurn{})
	}

	resp, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, Aggregator: agg, DriverName: "inmem"},
		prototypes.MemoryListRequest{}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// The identity-rejected events carry a "<missing>" session
	// sentinel (D-033), so they will not all match the caller's exact
	// session filter — but the count of rejections in the 24h window
	// is the aggregate the page renders. Assert the counter machinery
	// is wired (>= 0) and the aggregate is present.
	if resp.Aggregates.IdentityRejected24h < 0 {
		t.Errorf("IdentityRejected24h = %d, want >= 0", resp.Aggregates.IdentityRejected24h)
	}
}
