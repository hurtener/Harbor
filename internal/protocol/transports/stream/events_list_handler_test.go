package stream_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// evListID reuses the documented dummy identity triple (CLAUDE.md §13).
var evListID = identity.Identity{TenantID: "t-el", UserID: "u-el", SessionID: "s-el"}

func newEventsListHandler(t *testing.T) (*stream.EventsListHandler, events.EventBus) {
	t.Helper()
	bus := newDurableBusForHistory(t)
	store := newArtifactStore(t)
	h, err := stream.NewEventsListHandler(bus, store)
	if err != nil {
		t.Fatalf("NewEventsListHandler: %v", err)
	}
	return h, bus
}

func doEventsList(t *testing.T, h http.Handler, body string, id *identity.Identity, scopes []auth.Scope) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/events/list", strings.NewReader(body))
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

func TestEventsList_TailFirstWindow_OwnTriple(t *testing.T) {
	h, bus := newEventsListHandler(t)
	publishPlain(t, bus, evListID, 10)

	body := `{"filter":{},"cursor":0,"limit":3}`
	status, resp := doEventsList(t, h.Handler(), body, &evListID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, resp)
	}
	var got prototypes.EventsListResponse
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Events) != 3 || got.Events[0].Sequence != 8 || got.Events[2].Sequence != 10 {
		t.Fatalf("events = %v, want tail seqs 8,9,10", eventSeqs(got.Events))
	}
	if got.NextCursor != 8 || !got.HasMore {
		t.Errorf("NextCursor/HasMore = %d/%v, want 8/true", got.NextCursor, got.HasMore)
	}
	if got.Truncated {
		t.Errorf("Truncated = true on the gap-free durable log, want false")
	}
}

func TestEventsList_ScrollUpReachesHead(t *testing.T) {
	h, bus := newEventsListHandler(t)
	publishPlain(t, bus, evListID, 6)

	_, b1 := doEventsList(t, h.Handler(), `{"filter":{},"limit":3}`, &evListID, nil)
	var p1 prototypes.EventsListResponse
	if err := json.Unmarshal(b1, &p1); err != nil {
		t.Fatalf("decode p1: %v", err)
	}
	if p1.NextCursor != 4 || !p1.HasMore {
		t.Fatalf("p1 NextCursor/HasMore = %d/%v, want 4/true", p1.NextCursor, p1.HasMore)
	}
	body2 := fmt.Sprintf(`{"filter":{},"cursor":%d,"limit":3}`, p1.NextCursor)
	_, b2 := doEventsList(t, h.Handler(), body2, &evListID, nil)
	var p2 prototypes.EventsListResponse
	if err := json.Unmarshal(b2, &p2); err != nil {
		t.Fatalf("decode p2: %v", err)
	}
	if len(p2.Events) != 3 || p2.Events[0].Sequence != 1 || p2.Events[2].Sequence != 3 {
		t.Fatalf("p2 events = %v, want seqs 1,2,3", eventSeqs(p2.Events))
	}
	if p2.NextCursor != 0 || p2.HasMore {
		t.Errorf("p2 NextCursor/HasMore = %d/%v, want 0/false (head reached)", p2.NextCursor, p2.HasMore)
	}
}

func TestEventsList_InvalidTimeRange_400(t *testing.T) {
	h, _ := newEventsListHandler(t)
	// until <= since is structurally invalid.
	body := `{"filter":{"since":"2026-01-02T00:00:00Z","until":"2026-01-01T00:00:00Z"}}`
	status, resp := doEventsList(t, h.Handler(), body, &evListID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, resp)
	}
	assertEventsListCode(t, resp, protoerrors.CodeInvalidRequest)
}

func TestEventsList_NegativeLimit_400(t *testing.T) {
	h, _ := newEventsListHandler(t)
	status, resp := doEventsList(t, h.Handler(), `{"filter":{},"limit":-1}`, &evListID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, resp)
	}
	assertEventsListCode(t, resp, protoerrors.CodeInvalidRequest)
}

func TestEventsList_MissingIdentity_401(t *testing.T) {
	h, _ := newEventsListHandler(t)
	status, resp := doEventsList(t, h.Handler(), `{"filter":{}}`, nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, resp)
	}
	assertEventsListCode(t, resp, protoerrors.CodeIdentityRequired)
}

func TestEventsList_CrossTenantWithoutScope_403(t *testing.T) {
	h, _ := newEventsListHandler(t)
	// Naming a foreign tenant is a cross-tenant fan-in — rejected without
	// the closed two-scope claim.
	body := `{"filter":{"tenant_ids":["t-other"]}}`
	status, resp := doEventsList(t, h.Handler(), body, &evListID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", status, resp)
	}
	assertEventsListCode(t, resp, protoerrors.CodeIdentityScopeRequired)
}

func TestEventsList_CrossTenantWithAdminScope_OK(t *testing.T) {
	h, bus := newEventsListHandler(t)
	// Seed the caller's own session and a foreign tenant's session.
	publishPlain(t, bus, evListID, 3)
	foreign := identity.Identity{TenantID: "t-other", UserID: "u-el", SessionID: "s-el"}
	publishPlain(t, bus, foreign, 2)

	// Admin naming the foreign tenant's session reads its rows.
	body := `{"filter":{"tenant_ids":["t-other"]}}`
	status, resp := doEventsList(t, h.Handler(), body, &evListID, []auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, resp)
	}
	var got prototypes.EventsListResponse
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Events) != 2 {
		t.Fatalf("admin cross-tenant read returned %d events, want 2 (foreign tenant)", len(got.Events))
	}
	for _, ev := range got.Events {
		if ev.Tenant != "t-other" {
			t.Fatalf("admin read leaked tenant %q, want only t-other", ev.Tenant)
		}
	}
}

// TestEventsList_CrossUserSameTenantWithoutScope_403 pins the §6 cross-USER
// disclosure gate: a NON-admin caller naming {own-tenant, foreign-user,
// foreign-session} is REFUSED — not silently narrowed, and never handed
// another user's rows. This is the blind spot the original suite missed
// (it only tested cross-TENANT-without-scope).
func TestEventsList_CrossUserSameTenantWithoutScope_403(t *testing.T) {
	h, bus := newEventsListHandler(t)
	caller := identity.Identity{TenantID: "t-el", UserID: "u-a", SessionID: "s-a"}
	foreign := identity.Identity{TenantID: "t-el", UserID: "u-b", SessionID: "s-b"}
	publishPlain(t, bus, caller, 2)
	publishPlain(t, bus, foreign, 3)

	// Same tenant, but a foreign user + session — a cross-user read.
	body := `{"filter":{"user_ids":["u-b"],"session_ids":["s-b"]}}`
	status, resp := doEventsList(t, h.Handler(), body, &caller, nil)
	if status != http.StatusForbidden {
		t.Fatalf("cross-user read WITHOUT scope status = %d, want 403; body=%s", status, resp)
	}
	assertEventsListCode(t, resp, protoerrors.CodeIdentityScopeRequired)
}

// TestEventsList_CrossUserSameTenantWithScope_OK proves the widen path
// works: with the admin claim the same cross-user read returns the foreign
// user's rows (and only those).
func TestEventsList_CrossUserSameTenantWithScope_OK(t *testing.T) {
	h, bus := newEventsListHandler(t)
	caller := identity.Identity{TenantID: "t-el", UserID: "u-a", SessionID: "s-a"}
	foreign := identity.Identity{TenantID: "t-el", UserID: "u-b", SessionID: "s-b"}
	publishPlain(t, bus, caller, 2)
	publishPlain(t, bus, foreign, 3)

	body := `{"filter":{"user_ids":["u-b"],"session_ids":["s-b"]}}`
	status, resp := doEventsList(t, h.Handler(), body, &caller, []auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("cross-user read WITH admin status = %d, want 200; body=%s", status, resp)
	}
	var got prototypes.EventsListResponse
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Events) != 3 {
		t.Fatalf("admin cross-user read returned %d events, want 3 (foreign user)", len(got.Events))
	}
	for _, ev := range got.Events {
		if ev.User != "u-b" || ev.Session != "s-b" {
			t.Fatalf("admin cross-user read leaked %s/%s, want only u-b/s-b", ev.User, ev.Session)
		}
	}
}

// TestEventsList_OwnUserOtherSession_OK pins the deliberate session
// non-gate: a caller reading their OWN other session (same tenant + user,
// different session) is NOT elevation-gated — the legitimate Console
// Sessions / Playground history flow.
func TestEventsList_OwnUserOtherSession_OK(t *testing.T) {
	h, bus := newEventsListHandler(t)
	caller := identity.Identity{TenantID: "t-el", UserID: "u-a", SessionID: "s-a"}
	other := identity.Identity{TenantID: "t-el", UserID: "u-a", SessionID: "s-other"}
	publishPlain(t, bus, other, 4)

	body := `{"filter":{"session_ids":["s-other"]}}`
	status, resp := doEventsList(t, h.Handler(), body, &caller, nil)
	if status != http.StatusOK {
		t.Fatalf("own-user other-session read status = %d, want 200 (no elevation needed); body=%s", status, resp)
	}
	var got prototypes.EventsListResponse
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Events) != 4 {
		t.Fatalf("own-user other-session read returned %d events, want 4", len(got.Events))
	}
}

func TestEventsList_CrossTenantWithFleetScope_OK(t *testing.T) {
	h, bus := newEventsListHandler(t)
	foreign := identity.Identity{TenantID: "t-other", UserID: "u-el", SessionID: "s-el"}
	publishPlain(t, bus, foreign, 2)

	body := `{"filter":{"tenant_ids":["t-other"]}}`
	status, _ := doEventsList(t, h.Handler(), body, &evListID, []auth.Scope{auth.ScopeConsoleFleet})
	if status != http.StatusOK {
		t.Fatalf("console:fleet cross-tenant read status = %d, want 200 (live/historical authz parity)", status)
	}
}

func TestEventsList_OwnTripleIsolation(t *testing.T) {
	h, bus := newEventsListHandler(t)
	idA := identity.Identity{TenantID: "t-el", UserID: "u-a", SessionID: "s-a"}
	idB := identity.Identity{TenantID: "t-el", UserID: "u-b", SessionID: "s-b"}
	publishPlain(t, bus, idA, 4)
	publishPlain(t, bus, idB, 4)

	// Caller A (non-widened) sees only its own rows — never B's.
	status, resp := doEventsList(t, h.Handler(), `{"filter":{}}`, &idA, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, resp)
	}
	var got prototypes.EventsListResponse
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Events) != 4 {
		t.Fatalf("A read %d events, want 4 (own session only)", len(got.Events))
	}
	for _, ev := range got.Events {
		if ev.Session != idA.SessionID {
			t.Fatalf("caller A leaked session %q — cross-session isolation broken (§6 rule 10)", ev.Session)
		}
	}
}

// TestEventsList_RowShapeMatchesStateHistory pins the acceptance criterion
// that events.list rows are byte-identical to the state.history projection
// for the same events (same payloadWireValue, same artifact-ref seeding).
func TestEventsList_RowShapeMatchesStateHistory(t *testing.T) {
	// One bus + store feeds BOTH handlers so they project the same events.
	bus := newDurableBusForHistory(t)
	store := newArtifactStore(t)
	ref := seedHeavyArtifact(t, store, evListID)
	ev := events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: identity.Quadruple{Identity: evListID},
		Payload: heavyRefPayload{
			Note:      "tool produced a heavy result",
			Ref:       ref.ID,
			MIME:      "application/json",
			SizeBytes: 4096,
			Hash:      "sha256:fixture",
			Fetch:     map[string]any{"tool": "artifact_fetch", "id": ref.ID},
		},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	shh, err := stream.NewStateHistoryHandler(bus, store)
	if err != nil {
		t.Fatalf("NewStateHistoryHandler: %v", err)
	}
	elh, err := stream.NewEventsListHandler(bus, store)
	if err != nil {
		t.Fatalf("NewEventsListHandler: %v", err)
	}

	_, shResp := doStateHistory(t, shh.Handler(), `{"session_id":"s-el","limit":10}`, &evListID, nil)
	_, elResp := doEventsList(t, elh.Handler(), `{"filter":{},"limit":10}`, &evListID, nil)

	var shPage prototypes.StateHistoryResponse
	var elPage prototypes.EventsListResponse
	if err := json.Unmarshal(shResp, &shPage); err != nil {
		t.Fatalf("decode state.history: %v", err)
	}
	if err := json.Unmarshal(elResp, &elPage); err != nil {
		t.Fatalf("decode events.list: %v", err)
	}
	if len(shPage.Events) != 1 || len(elPage.Events) != 1 {
		t.Fatalf("expected exactly one event on each surface, got state.history=%d events.list=%d",
			len(shPage.Events), len(elPage.Events))
	}
	shRow, _ := json.Marshal(shPage.Events[0])
	elRow, _ := json.Marshal(elPage.Events[0])
	if string(shRow) != string(elRow) {
		t.Fatalf("row shapes diverge:\n state.history: %s\n events.list:   %s", shRow, elRow)
	}
	// The routable artifact ref must be present (no inline heavy bytes).
	if len(elPage.Events[0].Artifacts) != 1 || elPage.Events[0].Artifacts[0].ID != ref.ID {
		t.Fatalf("events.list row missing routable artifact ref %q: %+v", ref.ID, elPage.Events[0].Artifacts)
	}
}

// sentinelPayload is a NON-safe event payload (embeds events.Sealed, not
// SafeSealed) so the bus runs the audit redactor on publish. The `api_key`
// field is a canonical secret-name alias → its value is masked to "***"
// (audit.Placeholder). The sentinel therefore MUST NOT survive into the
// persisted, read-back-able row.
type sentinelPayload struct {
	events.Sealed
	Note   string `json:"note"`
	APIKey string `json:"api_key"`
}

// TestEventsList_SentinelRedactionHolds is the acceptance-named guarantee:
// no raw args/results survive the read-back. A secret-shaped value
// published in a non-safe payload is masked at the bus publish boundary
// (the single redactor), and events.list projects that SAME redacted row —
// so the sentinel appears NOWHERE in the response body.
func TestEventsList_SentinelRedactionHolds(t *testing.T) {
	h, bus := newEventsListHandler(t)
	const sentinel = "SENTINEL-DO-NOT-LEAK-abc123"
	ev := events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: identity.Quadruple{Identity: evListID},
		Payload:  sentinelPayload{Note: "carried a secret", APIKey: sentinel},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	status, resp := doEventsList(t, h.Handler(), `{"filter":{},"limit":10}`, &evListID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, resp)
	}
	if strings.Contains(string(resp), sentinel) {
		t.Fatalf("SECRET LEAKED — sentinel %q present in events.list response: %s", sentinel, resp)
	}
	// Positive control: the redacted row DID come back (masked), so the
	// absence above is redaction, not an empty page.
	if !strings.Contains(string(resp), "***") {
		t.Fatalf("expected the masked placeholder in the read-back row; body=%s", resp)
	}
}

func eventSeqs(evs []prototypes.StateEvent) []uint64 {
	out := make([]uint64, len(evs))
	for i, e := range evs {
		out[i] = e.Sequence
	}
	return out
}

func assertEventsListCode(t *testing.T, body []byte, want protoerrors.Code) {
	t.Helper()
	var e protoerrors.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body: %v (body=%s)", err, body)
	}
	if e.Code != want {
		t.Fatalf("error code = %q, want %q (body=%s)", e.Code, want, body)
	}
}
