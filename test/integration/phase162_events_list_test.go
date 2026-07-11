// Phase 162 integration test — the `events.list` durable, time-ranged,
// cross-session raw-event read exercised end-to-end against the REAL
// durable EventBus (over an inmem StateStore) + the REAL ArtifactStore
// behind the REAL EventsListHandler over httptest, gated by the REAL
// auth.Validator (ES256). No mocks at any seam (CLAUDE.md §17.3).
//
// It covers: own-scope isolation across two identities (caller A never
// sees B's rows — §6 rule 10); the admin-widened cross-tenant fleet read
// with `audit.admin_scope_used` asserted on the bus; a cross-tenant read
// WITHOUT the scope claim rejected (403); the missing-identity failure
// mode (401); the tail-first paging round-trip; the `truncated` edge on a
// bounded inmem ring; and an N≥10 concurrency stress under -race.
package integration_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	statinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

const phase162Kid = "harbor-phase162-k1"

var fixedNowPhase162 = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

type phase162Stack struct {
	srv  *httptest.Server
	bus  events.EventBus
	priv *ecdsa.PrivateKey
}

func newPhase162Stack(t *testing.T) *phase162Stack {
	t.Helper()
	priv, pub := loadES256Phase61(t)
	red := auditpatterns.New()

	st, err := statinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	bus, err := durable.New(context.Background(), config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 32,
		SubscriberBufferSize:     256,
		IdleTimeout:              time.Minute,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
	}, red, st)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store: st, Bus: bus, Redactor: audit.Redactor(red),
		Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}
	keys := newES256KeySet(phase162Kid, pub)
	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return fixedNowPhase162 }), auth.WithRedactor(red))
	if err != nil {
		t.Fatalf("auth.NewValidator: %v", err)
	}
	mux, err := transports.NewMux(surface, bus,
		transports.WithValidator(v),
		transports.WithEventsList(store),
	)
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return &phase162Stack{srv: srv, bus: bus, priv: priv}
}

func (s *phase162Stack) token(t *testing.T, id identity.Identity, scopes []string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowPhase162.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase162.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
	return signES256Wave10(t, s.priv, claims, phase162Kid)
}

func (s *phase162Stack) list(t *testing.T, body, token string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.srv.URL+"/v1/events/list", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/events/list: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, raw
}

func phase162Seed(t *testing.T, bus events.EventBus, id identity.Identity, n int) {
	t.Helper()
	for i := range n {
		ev := events.Event{
			Type:     events.EventTypeRuntimeWarning,
			Identity: identity.Quadruple{Identity: id},
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
		}
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatalf("publish #%d: %v", i, err)
		}
	}
}

func TestE2E_Phase162_EventsList_IsolationAndFleetRead(t *testing.T) {
	stack := newPhase162Stack(t)

	idA := identity.Identity{TenantID: "t-A", UserID: "u-A", SessionID: "s-A"}
	idB := identity.Identity{TenantID: "t-B", UserID: "u-B", SessionID: "s-B"}
	phase162Seed(t, stack.bus, idA, 5)
	phase162Seed(t, stack.bus, idB, 4)

	// 1. Own-scope isolation: caller A (non-widened) sees only its own rows.
	status, raw := stack.list(t, `{"filter":{}}`, stack.token(t, idA, nil))
	if status != http.StatusOK {
		t.Fatalf("A own-read status = %d, want 200; body=%s", status, raw)
	}
	var pageA prototypes.EventsListResponse
	mustJSON(t, raw, &pageA)
	if len(pageA.Events) != 5 {
		t.Fatalf("A read %d events, want 5 (own session)", len(pageA.Events))
	}
	for _, ev := range pageA.Events {
		if ev.Tenant != idA.TenantID || ev.Session != idA.SessionID {
			t.Fatalf("caller A leaked a foreign row (%s/%s) — cross-session isolation broken", ev.Tenant, ev.Session)
		}
	}

	// 2. Cross-tenant read WITHOUT the scope claim is rejected 403.
	status, raw = stack.list(t,
		`{"filter":{"tenant_ids":["t-B"],"user_ids":["u-B"],"session_ids":["s-B"]}}`,
		stack.token(t, idA, nil))
	if status != http.StatusForbidden {
		t.Fatalf("A cross-tenant WITHOUT scope status = %d, want 403; body=%s", status, raw)
	}

	// 3. Admin-widened fleet read: subscribe (admin) BEFORE the read to
	//    capture the audit.admin_scope_used notice, then drive the read.
	adminSub, err := stack.bus.Subscribe(context.Background(), events.Filter{Admin: true})
	if err != nil {
		t.Fatalf("admin Subscribe: %v", err)
	}
	defer adminSub.Cancel()
	auditSeen := make(chan struct{}, 1)
	go func() {
		for ev := range adminSub.Events() {
			if ev.Type == events.EventTypeAdminScopeUsed {
				select {
				case auditSeen <- struct{}{}:
				default:
				}
			}
		}
	}()

	status, raw = stack.list(t,
		`{"filter":{"tenant_ids":["t-B"],"user_ids":["u-B"],"session_ids":["s-B"]}}`,
		stack.token(t, idA, []string{string(auth.ScopeAdmin)}))
	if status != http.StatusOK {
		t.Fatalf("A admin fleet-read status = %d, want 200; body=%s", status, raw)
	}
	var fleet prototypes.EventsListResponse
	mustJSON(t, raw, &fleet)
	if len(fleet.Events) != 4 {
		t.Fatalf("admin fleet-read returned %d events, want 4 (B's rows)", len(fleet.Events))
	}
	for _, ev := range fleet.Events {
		if ev.Tenant != idB.TenantID {
			t.Fatalf("admin fleet-read leaked tenant %q, want only t-B", ev.Tenant)
		}
	}
	select {
	case <-auditSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("admin fleet-read did not emit audit.admin_scope_used within 2s")
	}

	// 4. console:fleet satisfies the same gate (live/historical parity).
	status, _ = stack.list(t,
		`{"filter":{"tenant_ids":["t-B"],"user_ids":["u-B"],"session_ids":["s-B"]}}`,
		stack.token(t, idA, []string{string(auth.ScopeConsoleFleet)}))
	if status != http.StatusOK {
		t.Fatalf("console:fleet fleet-read status = %d, want 200", status)
	}

	// 5. Cross-USER, SAME-TENANT disclosure gate (§6). A non-admin caller
	//    in tenant t-A naming a foreign user + session within its OWN
	//    tenant must be REFUSED — the blind spot the cross-tenant-only
	//    suite missed. Seed a sibling user in t-A first.
	idAOther := identity.Identity{TenantID: "t-A", UserID: "u-A2", SessionID: "s-A2"}
	phase162Seed(t, stack.bus, idAOther, 3)
	status, raw = stack.list(t,
		`{"filter":{"user_ids":["u-A2"],"session_ids":["s-A2"]}}`,
		stack.token(t, idA, nil))
	if status != http.StatusForbidden {
		t.Fatalf("cross-user SAME-tenant read WITHOUT scope status = %d, want 403 (§6 disclosure gate); body=%s", status, raw)
	}
	// With the admin claim the same cross-user read succeeds.
	status, raw = stack.list(t,
		`{"filter":{"user_ids":["u-A2"],"session_ids":["s-A2"]}}`,
		stack.token(t, idA, []string{string(auth.ScopeAdmin)}))
	if status != http.StatusOK {
		t.Fatalf("cross-user read WITH admin status = %d, want 200; body=%s", status, raw)
	}
	var crossUser prototypes.EventsListResponse
	mustJSON(t, raw, &crossUser)
	if len(crossUser.Events) != 3 {
		t.Fatalf("admin cross-user read returned %d events, want 3", len(crossUser.Events))
	}
}

func TestE2E_Phase162_EventsList_PagingAndMissingIdentity(t *testing.T) {
	stack := newPhase162Stack(t)
	id := identity.Identity{TenantID: "t-pg", UserID: "u-pg", SessionID: "s-pg"}
	phase162Seed(t, stack.bus, id, 6)

	// Page 1 (tail): 3 rows, next_cursor 4, has_more.
	_, raw := stack.list(t, `{"filter":{},"limit":3}`, stack.token(t, id, nil))
	var p1 prototypes.EventsListResponse
	mustJSON(t, raw, &p1)
	if p1.NextCursor != 4 || !p1.HasMore || len(p1.Events) != 3 {
		t.Fatalf("page1 = %+v, want 3 rows / next_cursor 4 / has_more", p1)
	}
	// Page 2 (scroll up): rows 1..3, head reached.
	body2 := `{"filter":{},"cursor":4,"limit":3}`
	_, raw = stack.list(t, body2, stack.token(t, id, nil))
	var p2 prototypes.EventsListResponse
	mustJSON(t, raw, &p2)
	if p2.HasMore || p2.NextCursor != 0 || len(p2.Events) != 3 || p2.Events[0].Sequence != 1 {
		t.Fatalf("page2 = %+v, want rows 1..3 / next_cursor 0 / no more", p2)
	}

	// Missing identity (no bearer) is rejected — the failure mode (§17.3 #3).
	status, _ := stack.list(t, `{"filter":{}}`, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("missing-bearer status = %d, want 401", status)
	}
}

// TestE2E_Phase162_EventsList_TruncatedEdge pins the honest retention-gap
// flag on a bounded inmem ring: once the ring evicts, a windowed read that
// reaches the ring floor reports truncated=true.
func TestE2E_Phase162_EventsList_TruncatedEdge(t *testing.T) {
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Minute,
		DropWindow:               time.Second,
		ReplayBufferSize:         4, // tiny ring so eviction is forced
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	hr, ok := bus.(events.HistoryReplayer)
	if !ok {
		t.Fatal("inmem bus is not a HistoryReplayer")
	}
	id := identity.Identity{TenantID: "t-tr", UserID: "u-tr", SessionID: "s-tr"}
	phase162Seed(t, bus, id, 10) // 10 events into a 4-slot ring → eviction

	page, err := hr.ListWindow(context.Background(), events.EventListQuery{
		Filter: prototypes.EventFilter{
			TenantIDs: []string{id.TenantID}, UserIDs: []string{id.UserID}, SessionIDs: []string{id.SessionID},
		},
		Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListWindow: %v", err)
	}
	if !page.Truncated {
		t.Fatalf("truncated = false on an evicting ring, want true (honest retention-gap signal)")
	}
	// The ring only retained the last few events — never more than capacity.
	if len(page.Events) > 4 {
		t.Fatalf("ring returned %d events, want <= ring capacity 4", len(page.Events))
	}
}

// TestE2E_Phase162_EventsList_ConcurrencyStress runs N≥10 concurrent
// windowed reads against one shared stack under -race — no cross-caller
// bleed, no torn pages.
func TestE2E_Phase162_EventsList_ConcurrencyStress(t *testing.T) {
	stack := newPhase162Stack(t)
	const identities = 8
	ids := make([]identity.Identity, identities)
	for i := range ids {
		ids[i] = identity.Identity{
			TenantID:  "t-cc" + string(rune('A'+i)),
			UserID:    "u-cc",
			SessionID: "s-cc" + string(rune('A'+i)),
		}
		phase162Seed(t, stack.bus, ids[i], 12)
	}

	const readers = 24
	var wg sync.WaitGroup
	errs := make(chan string, readers)
	for r := range readers {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			id := ids[r%identities]
			tok := stack.token(t, id, nil)
			for range 6 {
				status, raw := stack.list(t, `{"filter":{},"limit":10}`, tok)
				if status != http.StatusOK {
					errs <- "status " + http.StatusText(status)
					return
				}
				var page prototypes.EventsListResponse
				if err := json.Unmarshal(raw, &page); err != nil {
					errs <- "decode: " + err.Error()
					return
				}
				var prev uint64
				for i, ev := range page.Events {
					if ev.Tenant != id.TenantID || ev.Session != id.SessionID {
						errs <- "row bleed from " + ev.Tenant
						return
					}
					if i > 0 && ev.Sequence <= prev {
						errs <- "torn page"
						return
					}
					prev = ev.Sequence
				}
			}
		}(r)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrency stress failure: %s", e)
	}
}
