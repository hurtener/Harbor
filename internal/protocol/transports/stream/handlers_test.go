package stream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// aggHandlerNow anchors the aggregate handler tests' window on a fixed instant
// so backdated fixture events land in a deterministic bucket range.
var aggHandlerNow = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

type aggHandlerClock struct{}

func (aggHandlerClock) Now() time.Time { return aggHandlerNow }

// newAggregateHandlerTest builds the events-aggregate HTTP handler over a real
// replay-capable inmem bus + a fixed-clock aggregator. Returns the handler and
// the bus (so a test can seed events and count admin_scope_used emissions).
func newAggregateHandlerTest(t *testing.T) (*stream.AggregateHandler, events.EventBus) {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Second,
		DropWindow:               50 * time.Millisecond,
		ReplayBufferSize:         1024,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("eventsinmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	agg, err := events.NewAggregator(bus, events.WithAggregatorClock(aggHandlerClock{}))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	h, err := stream.NewAggregateHandler(agg)
	if err != nil {
		t.Fatalf("NewAggregateHandler: %v", err)
	}
	return h, bus
}

func publishAggAt(t *testing.T, bus events.EventBus, tenant, user, session string, at time.Time) {
	t.Helper()
	ev := events.Event{
		Type: events.EventTypeRuntimeError,
		Identity: identity.Quadruple{Identity: identity.Identity{
			TenantID: tenant, UserID: user, SessionID: session,
		}},
		OccurredAt: at.UTC(),
		Payload:    events.BusDroppedPayload{FromSeq: 1, ToSeq: 1},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("publishAggAt %s/%s/%s: %v", tenant, user, session, err)
	}
}

func doAggregate(t *testing.T, h http.Handler, body string, id *identity.Identity, scopes []auth.Scope) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/events/aggregate", strings.NewReader(body))
	if id != nil {
		req.Header.Set(stream.HeaderTenant, id.TenantID)
		req.Header.Set(stream.HeaderUser, id.UserID)
		req.Header.Set(stream.HeaderSession, id.SessionID)
	}
	if scopes != nil {
		req = req.WithContext(auth.WithScopes(req.Context(), scopes))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// aggHandlerAdminScopeUsed counts the admin_scope_used notices in the bus,
// subtracting the one the counting Replay itself emits (the same technique the
// aggregator unit tests use).
func aggHandlerAdminScopeUsed(t *testing.T, bus events.EventBus) int {
	t.Helper()
	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatalf("bus %T is not a Replayer", bus)
	}
	snap, err := rp.Replay(context.Background(), events.Cursor{Sequence: 0}, events.Filter{Admin: true})
	if err != nil {
		t.Fatalf("Replay(admin): %v", err)
	}
	n := 0
	for _, ev := range snap {
		if ev.Type == events.EventTypeAdminScopeUsed {
			n++
		}
	}
	return n - 1
}

func sumAggRuntimeError(t *testing.T, raw []byte) int64 {
	t.Helper()
	var resp prototypes.EventAggregateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode aggregate response: %v; body=%s", err, raw)
	}
	var total int64
	for _, b := range resp.Buckets {
		total += b.Counts[string(events.EventTypeRuntimeError)]
	}
	return total
}

var aggCaller = identity.Identity{TenantID: "t-agg", UserID: "u-agg", SessionID: "s-agg"}

// TestAggregateHandler_NonAdminOwnSession_NoAudit_200 — a non-admin own-tuple
// aggregate returns 200 with only the caller's counts and emits ZERO
// admin_scope_used (guarding the real latent defect: the old hardcoded
// Filter{Admin:true} emitted one on every aggregate).
func TestAggregateHandler_NonAdminOwnSession_NoAudit_200(t *testing.T) {
	h, bus := newAggregateHandlerTest(t)
	publishAggAt(t, bus, aggCaller.TenantID, aggCaller.UserID, aggCaller.SessionID, aggHandlerNow.Add(-5*time.Minute))
	publishAggAt(t, bus, aggCaller.TenantID, aggCaller.UserID, aggCaller.SessionID, aggHandlerNow.Add(-6*time.Minute))
	// A sibling session — must not be counted for a non-admin own-tuple read.
	publishAggAt(t, bus, aggCaller.TenantID, aggCaller.UserID, "s-other", aggHandlerNow.Add(-5*time.Minute))

	body := `{"window":1800000000000,"bucket":60000000000}`
	status, raw := doAggregate(t, h, body, &aggCaller, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}
	if got := sumAggRuntimeError(t, raw); got != 2 {
		t.Fatalf("non-admin aggregate counted %d, want 2 (own tuple only)", got)
	}
	if got := aggHandlerAdminScopeUsed(t, bus); got != 0 {
		t.Fatalf("non-admin aggregate emitted %d admin_scope_used, want 0", got)
	}
}

// TestAggregateHandler_CrossTenantWithAdmin_PreFoldWidened_OneAudit_200 pins
// WARN-2: `widened` is computed on the RAW pre-fold filter. The request names a
// FOREIGN tenant with the user/session axes elided; post-fold that yields a
// COMPLETE triple {t-foreign, caller-user, caller-session}, so a wrong
// post-fold derivation would evaluate widened=false and run un-fanned +
// un-audited. Because the handler derives widened pre-fold, the read fans in
// (returns the foreign-tenant events, which share the caller's user/session id)
// and emits EXACTLY ONE admin_scope_used.
func TestAggregateHandler_CrossTenantWithAdmin_PreFoldWidened_OneAudit_200(t *testing.T) {
	h, bus := newAggregateHandlerTest(t)
	// Foreign tenant, but the SAME user/session id as the caller (so the
	// handler's triple-fold does not exclude them) — the events.list admin
	// cross-tenant fixture shape.
	publishAggAt(t, bus, "t-foreign", aggCaller.UserID, aggCaller.SessionID, aggHandlerNow.Add(-5*time.Minute))
	publishAggAt(t, bus, "t-foreign", aggCaller.UserID, aggCaller.SessionID, aggHandlerNow.Add(-6*time.Minute))
	// The caller's own tenant also has an event — the cross-tenant filter names
	// ONLY t-foreign, so this must not be counted.
	publishAggAt(t, bus, aggCaller.TenantID, aggCaller.UserID, aggCaller.SessionID, aggHandlerNow.Add(-5*time.Minute))

	body := `{"filter":{"tenant_ids":["t-foreign"]},"window":1800000000000,"bucket":60000000000}`
	status, raw := doAggregate(t, h, body, &aggCaller, []auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}
	if got := sumAggRuntimeError(t, raw); got != 2 {
		t.Fatalf("widened cross-tenant aggregate counted %d, want 2 (foreign tenant only; a post-fold widening derivation would return 0 un-fanned)", got)
	}
	if got := aggHandlerAdminScopeUsed(t, bus); got != 1 {
		t.Fatalf("widened aggregate emitted %d admin_scope_used, want exactly 1", got)
	}
}

// TestAggregateHandler_WidenedForeignTenant_DistinctPrincipals_FansIn_200 is
// the isolation-critical regression for the fold bug: a widened admin
// aggregate naming ONLY a foreign tenant, with the user/session axes elided,
// must FAN IN across that tenant's DISTINCT users/sessions — not be narrowed to
// {t-foreign, caller-user, caller-session} → EMPTY. The prior widened fixture
// masked the defect by reusing the caller's own user/session id in the foreign
// tenant; this one uses distinct principals so a caller-fold returns 0 and the
// test catches it. The tenant axis stays name-to-widen: the caller's OWN tenant
// event is excluded because the filter named only t-foreign.
func TestAggregateHandler_WidenedForeignTenant_DistinctPrincipals_FansIn_200(t *testing.T) {
	h, bus := newAggregateHandlerTest(t)
	publishAggAt(t, bus, "t-foreign", "u-x", "s-x", aggHandlerNow.Add(-5*time.Minute))
	publishAggAt(t, bus, "t-foreign", "u-y", "s-y", aggHandlerNow.Add(-6*time.Minute))
	// Caller's own tenant event — the filter names ONLY t-foreign, so an elided
	// tenant would NOT reach here; this guards the tenant axis staying scoped.
	publishAggAt(t, bus, aggCaller.TenantID, aggCaller.UserID, aggCaller.SessionID, aggHandlerNow.Add(-5*time.Minute))

	body := `{"filter":{"tenant_ids":["t-foreign"]},"window":1800000000000,"bucket":60000000000}`
	status, raw := doAggregate(t, h, body, &aggCaller, []auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}
	if got := sumAggRuntimeError(t, raw); got != 2 {
		t.Fatalf("widened foreign-tenant aggregate counted %d, want 2 (distinct principals fanned in; a caller-fold would return 0)", got)
	}
	if got := aggHandlerAdminScopeUsed(t, bus); got != 1 {
		t.Fatalf("widened aggregate emitted %d admin_scope_used, want exactly 1", got)
	}
}

// TestAggregateHandler_WidenedSessionLess_FansInAcrossSessions_200 pins HA-21
// through the Protocol METHOD (HA-20 leg 2 — cover the handler, not only the
// bus interface): a session-less admin aggregate (a foreign tenant named, the
// SessionIDs axis elided) must fan in across ALL of that tenant's sessions, NOT
// be re-narrowed to the caller's own session by the handler fold. The bus-level
// conformance row (Admin fan-in with an empty session set) passed green, but
// the shipped handler overwrote the empty SessionIDs before the call, so the
// pinned fan-in was unreachable through the Protocol. This test guards the
// handler can never silently re-narrow the session axis again.
func TestAggregateHandler_WidenedSessionLess_FansInAcrossSessions_200(t *testing.T) {
	h, bus := newAggregateHandlerTest(t)
	// One foreign tenant, one user, THREE distinct sessions — a pure
	// session-axis fan-in.
	publishAggAt(t, bus, "t-fleet", "u-1", "s-1", aggHandlerNow.Add(-5*time.Minute))
	publishAggAt(t, bus, "t-fleet", "u-1", "s-2", aggHandlerNow.Add(-6*time.Minute))
	publishAggAt(t, bus, "t-fleet", "u-1", "s-3", aggHandlerNow.Add(-7*time.Minute))

	body := `{"filter":{"tenant_ids":["t-fleet"]},"window":1800000000000,"bucket":60000000000}`
	status, raw := doAggregate(t, h, body, &aggCaller, []auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}
	// All THREE sessions counted — a session-fold would narrow to the caller's
	// own session (s-agg, absent in t-fleet) → 0.
	if got := sumAggRuntimeError(t, raw); got != 3 {
		t.Fatalf("session-less widened aggregate counted %d, want 3 (fan-in across 3 sessions; a session-fold would return 0)", got)
	}
}

// TestAggregateHandler_ElidedTenantStaysOwnScope_200 proves the tenant axis is
// name-to-widen (D-284 parity): even a widened read (here widened by a foreign
// USER, tenant elided) does NOT fan across every tenant — the elided tenant
// folds to the caller's own, so a foreign tenant's same-user events are
// excluded. Only the caller-tenant's foreign-user events (fanned via the
// un-folded user/session axes) are counted.
func TestAggregateHandler_ElidedTenantStaysOwnScope_200(t *testing.T) {
	h, bus := newAggregateHandlerTest(t)
	// Caller-tenant, a DIFFERENT user — should be counted (user axis wildcarded).
	publishAggAt(t, bus, aggCaller.TenantID, "u-foreign", "s-foreign", aggHandlerNow.Add(-5*time.Minute))
	// A DIFFERENT tenant, same foreign user — must NOT be counted (elided tenant
	// folded to the caller's own; the tenant axis never wildcards on an elided
	// request).
	publishAggAt(t, bus, "t-elsewhere", "u-foreign", "s-elsewhere", aggHandlerNow.Add(-5*time.Minute))

	// Naming a foreign USER (not tenant) makes the read widened; tenant is elided.
	body := `{"filter":{"user_ids":["u-foreign"]},"window":1800000000000,"bucket":60000000000}`
	status, raw := doAggregate(t, h, body, &aggCaller, []auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}
	if got := sumAggRuntimeError(t, raw); got != 1 {
		t.Fatalf("elided-tenant widened aggregate counted %d, want 1 (own tenant only; an elided tenant must NOT fan across all tenants)", got)
	}
}

// TestAggregateHandler_CrossTenantWithoutScope_403 — a cross-tenant filter
// without the elevated scope is rejected 403 (never counted, never a 500).
func TestAggregateHandler_CrossTenantWithoutScope_403(t *testing.T) {
	h, _ := newAggregateHandlerTest(t)
	body := `{"filter":{"tenant_ids":["t-foreign"]},"window":1800000000000,"bucket":60000000000}`
	status, raw := doAggregate(t, h, body, &aggCaller, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", status, raw)
	}
	var perr protoerrors.Error
	if err := json.Unmarshal(raw, &perr); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if perr.Code != protoerrors.CodeIdentityScopeRequired {
		t.Fatalf("code = %q, want %q", perr.Code, protoerrors.CodeIdentityScopeRequired)
	}
}

// TestAggregateHandler_MissingIdentity_401 — an identity-less request fails
// closed at the edge.
func TestAggregateHandler_MissingIdentity_401(t *testing.T) {
	h, _ := newAggregateHandlerTest(t)
	status, _ := doAggregate(t, h, `{"window":1800000000000,"bucket":60000000000}`, nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

// TestAggregateHandler_BadWindow_400 — a non-dividing Window/Bucket pair maps
// through classifyAggregateError to CodeInvalidRequest / 400 (never a bare
// 500), so a rendering client never sees a fractional trailing bucket.
func TestAggregateHandler_BadWindow_400(t *testing.T) {
	h, _ := newAggregateHandlerTest(t)
	// 7-minute bucket does not evenly divide a 1-hour window.
	body := `{"window":3600000000000,"bucket":420000000000}`
	status, raw := doAggregate(t, h, body, &aggCaller, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, raw)
	}
	var perr protoerrors.Error
	if err := json.Unmarshal(raw, &perr); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if perr.Code != protoerrors.CodeInvalidRequest {
		t.Fatalf("code = %q, want %q", perr.Code, protoerrors.CodeInvalidRequest)
	}
}

// TestAggregateHandler_MethodNotAllowed_405 — only POST is accepted.
func TestAggregateHandler_MethodNotAllowed_405(t *testing.T) {
	h, _ := newAggregateHandlerTest(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/events/aggregate", nil)
	req.Header.Set(stream.HeaderTenant, aggCaller.TenantID)
	req.Header.Set(stream.HeaderUser, aggCaller.UserID)
	req.Header.Set(stream.HeaderSession, aggCaller.SessionID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}
