package events_test

// Branch-tail coverage for the events searcher: the constructor guards,
// the index identity, the `events.run` facet, the time window, the heavy
// arm, and the replay-unavailable degradation the package documents.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsubsys "github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	eventsearch "github.com/hurtener/Harbor/internal/search/events"
)

func TestEventsSearcher_New_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()
	replayer := h.bus.(eventsubsys.Replayer)

	if _, err := eventsearch.New(nil, search.Deps{
		Redactor: patterns.New(), AdminScope: func(context.Context) bool { return false }, Audit: testAudit,
	}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil replayer: got %v, want ErrInvalidRequest", err)
	}
	if _, err := eventsearch.New(replayer, search.Deps{
		AdminScope: func(context.Context) bool { return false }, Audit: testAudit,
	}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil redactor: got %v, want ErrInvalidRequest", err)
	}
	if _, err := eventsearch.New(replayer, search.Deps{Redactor: patterns.New(), Audit: testAudit}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil AdminScope: got %v, want ErrInvalidRequest", err)
	}
	if _, err := eventsearch.New(replayer, search.Deps{
		Redactor: patterns.New(), AdminScope: func(context.Context) bool { return false },
	}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil Audit: got %v, want ErrInvalidRequest", err)
	}
}

func TestEventsSearcher_Index_IsTheEventsIndex(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()
	if got := denyingSearcher(t, h).Index(); got != types.SearchIndexEvents {
		t.Errorf("Index() = %q, want %q", got, types.SearchIndexEvents)
	}
}

// TestEventsSearcher_RunFacetAndQueryNarrow covers the `events.run` facet
// and the run-id arm of the free-text match — both narrowings INSIDE an
// already-scoped triple, which is why neither takes a claim.
func TestEventsSearcher_RunFacetAndQueryNarrow(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()

	ident := identity.Identity{TenantID: oneTenant, UserID: attacker, SessionID: attacker + "-sess"}
	for _, run := range []string{"run-alpha", "run-beta"} {
		if err := h.bus.Publish(context.Background(), eventsubsys.Event{
			Type:     eventsubsys.EventTypeRuntimeError,
			Identity: identity.Quadruple{Identity: ident, RunID: run},
			Payload:  eventsubsys.RedactedMap{Data: map[string]any{"msg": run}},
		}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	s := denyingSearcher(t, h)
	byRun, err := s.Search(attackerCtx(), types.SearchRequest{
		Facets: []types.SearchFacet{{Key: "events.run", Value: "run-alpha"}},
	})
	if err != nil {
		t.Fatalf("run facet: %v", err)
	}
	if len(byRun.Rows) != 1 || byRun.Rows[0].RunID != "run-alpha" {
		t.Errorf("run facet returned %v, want just run-alpha", byRun.Rows)
	}

	byQuery, err := s.Search(attackerCtx(), types.SearchRequest{Query: "run-beta"})
	if err != nil {
		t.Fatalf("run query: %v", err)
	}
	if len(byQuery.Rows) != 1 || byQuery.Rows[0].RunID != "run-beta" {
		t.Errorf("run-id query returned %v, want just run-beta", byQuery.Rows)
	}
}

// TestEventsSearcher_TimeWindowExcludes — the window drops rows outside
// [Since, Until] rather than failing the read.
func TestEventsSearcher_TimeWindowExcludes(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	future := time.Now().Add(24 * time.Hour)
	resp, err := denyingSearcher(t, h).Search(attackerCtx(), types.SearchRequest{
		Filter: types.SearchFilter{Since: future},
	})
	if err != nil {
		t.Fatalf("windowed search: %v", err)
	}
	if len(resp.Rows) != 0 {
		t.Errorf("a window starting tomorrow returned %d rows", len(resp.Rows))
	}
}

// TestEventsSearcher_HeavyPreviewShipsRefNotBytes covers the heavy arm.
func TestEventsSearcher_HeavyPreviewShipsRefNotBytes(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()

	// The preview is built from the event's own header fields, so a run
	// id past the bound is the only way to reach the heavy arm.
	huge := strings.Repeat("r", search.HeavyPreviewThreshold+1)
	ident := identity.Identity{TenantID: oneTenant, UserID: attacker, SessionID: attacker + "-sess"}
	if err := h.bus.Publish(context.Background(), eventsubsys.Event{
		Type:     eventsubsys.EventTypeRuntimeError,
		Identity: identity.Quadruple{Identity: ident, RunID: huge},
		Payload:  eventsubsys.RedactedMap{Data: map[string]any{"msg": "big"}},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	resp, err := denyingSearcher(t, h).Search(attackerCtx(), types.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(resp.Rows))
	}
	if resp.Rows[0].Preview != "" {
		t.Errorf("a preview past the bound must not ship inline: got %d bytes", len(resp.Rows[0].Preview))
	}
	if resp.Rows[0].Ref == nil {
		t.Fatal("a heavy row must carry a Ref")
	}
}

// TestEventsSearcher_ReplayUnavailableDegradesToAnEmptyPage — the ONE
// documented soft path on this index: a bus built with no replay ring
// answers an empty page rather than an error, because the caller learns
// there is no replay capability from the empty Rows.
func TestEventsSearcher_ReplayUnavailableDegradesToAnEmptyPage(t *testing.T) {
	t.Parallel()
	bus, err := inmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 4,
		SubscriberBufferSize:     16,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         0, // replay disabled
	}, patterns.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	defer bus.Close(context.Background())

	s, err := eventsearch.New(bus.(eventsubsys.Replayer), search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
		Audit:      testAudit,
	})
	if err != nil {
		t.Fatalf("eventsearch.New: %v", err)
	}
	resp, err := s.Search(attackerCtx(), types.SearchRequest{})
	if err != nil {
		t.Fatalf("replay-disabled search: %v", err)
	}
	if len(resp.Rows) != 0 || resp.TotalCount != 0 || resp.HasMore {
		t.Errorf("replay-disabled page: %+v", resp)
	}
	if resp.ProtocolVersion == "" {
		t.Error("the degraded page must still carry a ProtocolVersion")
	}
}

// TestEventsSearcher_ClosedBusPropagates — a bus failure that is NOT the
// documented replay-unavailable case stays loud.
func TestEventsSearcher_ClosedBusPropagates(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	seedTwoUsersOneTenant(t, h)
	s := denyingSearcher(t, h)
	if err := h.bus.Close(context.Background()); err != nil {
		t.Fatalf("bus.Close: %v", err)
	}
	if _, err := s.Search(attackerCtx(), types.SearchRequest{}); err == nil {
		t.Fatal("a closed bus must not degrade to an empty page")
	}
}

func TestEventsSearcher_RedactorFailureRefusesTheRow(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	s, err := eventsearch.New(h.bus.(eventsubsys.Replayer), search.Deps{
		Redactor:   failingRedactor{},
		AdminScope: func(context.Context) bool { return false },
		Audit:      testAudit,
	})
	if err != nil {
		t.Fatalf("eventsearch.New: %v", err)
	}
	if _, serr := s.Search(attackerCtx(), types.SearchRequest{}); !errors.Is(serr, search.ErrRedactionFailed) {
		t.Fatalf("got %v, want ErrRedactionFailed", serr)
	}
}

type failingRedactor struct{}

func (failingRedactor) Redact(context.Context, any) (any, error) {
	return nil, errors.New("forced redaction failure")
}
