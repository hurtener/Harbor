// Phase 73n cross-subsystem integration test per CLAUDE.md §17 — the
// `runs.set_overrides` Protocol method exercised end-to-end against the
// real wire transport + the real runs/protocol.Service (override Store)
// + the real auth.Validator/Middleware (Phase 61), with no mocks at any
// seam.
//
// Surfaces composed:
//
//   - Phase 73n internal/runtime/runs/protocol — the Runs Protocol
//     Service + the in-process override Store.
//   - Phase 60 internal/protocol/transports — the wire surface the
//     `runs.set_overrides` handler is mounted on.
//   - Phase 61 internal/protocol/auth — the JWT validator + middleware
//     verifying the identity triple.
//   - Phase 05 internal/events — the real in-mem bus the
//     `runs.overrides_set` audit event is published onto.
//
// This test ships the §13 primitive-with-consumer discharge for Phase
// 73n's Go-side surface: it is the first end-to-end consumer of the
// `runs.set_overrides` wire method — the next-message-override happy
// path, the cross-session reject path, an invalid-override reject, an
// audit-event assertion, and an N≥10 concurrency stress run proving the
// override Store has no cross-session bleed under the real wire
// transport.
package integration_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

const phase73nKid = "phase73n-kid"

var fixedNowPhase73n = time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

type phase73nDeps struct {
	mux     *http.ServeMux
	priv    *ecdsa.PrivateKey
	bus     events.EventBus
	store   *runsprotocol.Store
	cleanup func()
}

// newPhase73nDeps wires the real dev-stack surfaces for the Playground
// `runs.set_overrides` route.
func newPhase73nDeps(t *testing.T) *phase73nDeps {
	t.Helper()

	priv, pub := loadES256Phase61(t)
	red := auditpatterns.New()

	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	// The real Phase 73n override Store + Service.
	overrideStore := runsprotocol.NewStore()
	runsSvc, err := runsprotocol.NewService(overrideStore,
		runsprotocol.WithBus(bus),
		runsprotocol.WithRedactor(red),
	)
	if err != nil {
		t.Fatalf("runs/protocol.NewService: %v", err)
	}

	keys := newES256KeySet(phase73nKid, pub)
	now := func() time.Time { return fixedNowPhase73n }
	v, err := auth.NewValidator(keys, auth.WithClock(now), auth.WithRedactor(red))
	if err != nil {
		t.Fatalf("auth.NewValidator: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithValidator(v),
		transports.WithRunsService(runsSvc),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &phase73nDeps{
		mux:   mux,
		priv:  priv,
		bus:   bus,
		store: overrideStore,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// phase73nClaims mints a JWT MapClaims for the test's standard shape.
func phase73nClaims(id identity.Identity, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowPhase73n.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase73n.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
}

// postRuns issues a POST /v1/runs/{verb} with the supplied JWT.
func postRuns(t *testing.T, srvURL, verb, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/runs/"+verb, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestE2E_Phase73n_PlaygroundOverrides is the §13 primitive-with-
// consumer binding test for the `runs.set_overrides` Protocol surface.
func TestE2E_Phase73n_PlaygroundOverrides(t *testing.T) {
	deps := newPhase73nDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "sess-A"}
	tok := signES256Wave10(t, deps.priv, phase73nClaims(id, nil), phase73nKid)

	// (a) Happy path: the override is recorded for the operator's
	// session and applies to the NEXT message.
	status, body := postRuns(t, srv.URL, "set_overrides",
		`{"overrides":{"session_id":"sess-A","reasoning_effort":"high","temperature":0.7,"max_tokens":2048}}`,
		tok)
	if status != http.StatusOK {
		t.Fatalf("runs.set_overrides: status = %d, want 200; body=%s", status, body)
	}
	var resp prototypes.RunSetOverridesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode runs.set_overrides: %v", err)
	}
	if resp.AppliedAt.IsZero() {
		t.Error("AppliedAt is zero")
	}
	// The override landed in the Store under the operator's identity.
	po, ok := deps.store.Peek(id)
	if !ok || po.ReasoningEffort == nil || *po.ReasoningEffort != "high" {
		t.Fatalf("override not recorded: %+v ok=%v", po, ok)
	}

	// (a') Next-message-only: consuming the override empties the slot —
	// it does NOT apply retroactively / repeatedly.
	if _, ok := deps.store.Consume(id); !ok {
		t.Fatal("Consume found no override after a recorded set")
	}
	if _, ok := deps.store.Peek(id); ok {
		t.Error("override survived a one-shot consume — not next-message-only")
	}

	// (b) Cross-session override → CodeScopeMismatch (403). The verified
	// session is sess-A; the override names a different session.
	status, body = postRuns(t, srv.URL, "set_overrides",
		`{"overrides":{"session_id":"sess-OTHER","reasoning_effort":"high"}}`, tok)
	if status != http.StatusForbidden {
		t.Fatalf("cross-session override: status = %d, want 403; body=%s", status, body)
	}
	var perr protoerrors.Error
	_ = json.Unmarshal(body, &perr)
	if perr.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("cross-session reject code = %q, want %q", perr.Code, protoerrors.CodeScopeMismatch)
	}

	// (c) Invalid override payload → CodeInvalidRequest (400).
	status, body = postRuns(t, srv.URL, "set_overrides",
		`{"overrides":{"session_id":"sess-A","temperature":9.9}}`, tok)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid override: status = %d, want 400; body=%s", status, body)
	}
	_ = json.Unmarshal(body, &perr)
	if perr.Code != protoerrors.CodeInvalidRequest {
		t.Fatalf("invalid-override reject code = %q, want %q", perr.Code, protoerrors.CodeInvalidRequest)
	}

	// (d) Audit emit: a recorded override publishes a runs.overrides_set
	// event on the bus.
	auditCh := make(chan events.Event, 8)
	auditSub, subErr := deps.bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{events.EventTypeRunOverridesSet},
	})
	if subErr != nil {
		t.Fatalf("Subscribe: %v", subErr)
	}
	defer auditSub.Cancel()
	go func() {
		for ev := range auditSub.Events() {
			auditCh <- ev
		}
	}()
	status, body = postRuns(t, srv.URL, "set_overrides",
		`{"overrides":{"session_id":"sess-A","reasoning_effort":"low"}}`, tok)
	if status != http.StatusOK {
		t.Fatalf("runs.set_overrides for audit: status = %d; body=%s", status, body)
	}
	select {
	case ev := <-auditCh:
		payload, ok := ev.Payload.(events.RunOverridesSetPayload)
		if !ok {
			t.Fatalf("audit payload type = %T, want RunOverridesSetPayload", ev.Payload)
		}
		if payload.SessionID != "sess-A" || !payload.SetReasoningEffort {
			t.Errorf("audit payload = %+v, want sess-A + reasoning-effort set", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runs.overrides_set audit event")
	}

	// (e) Concurrency stress: N≥10 distinct sessions record overrides in
	// parallel through the real wire transport — assert no cross-session
	// bleed in the override Store.
	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			sid := fmt.Sprintf("stress-sess-%02d", i)
			sessID := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: sid}
			stok := signES256Wave10(t, deps.priv, phase73nClaims(sessID, nil), phase73nKid)
			effort := []string{"low", "medium", "high"}[i%3]
			st, b := postRuns(t, srv.URL, "set_overrides",
				fmt.Sprintf(`{"overrides":{"session_id":%q,"reasoning_effort":%q}}`, sid, effort),
				stok)
			if st != http.StatusOK {
				t.Errorf("session %s: status = %d; body=%s", sid, st, b)
			}
		}()
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		sid := fmt.Sprintf("stress-sess-%02d", i)
		wantEffort := []string{"low", "medium", "high"}[i%3]
		sessID := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: sid}
		po, ok := deps.store.Peek(sessID)
		if !ok {
			t.Errorf("session %s: no override recorded", sid)
			continue
		}
		if po.ReasoningEffort == nil || *po.ReasoningEffort != wantEffort {
			t.Errorf("session %s: ReasoningEffort = %v, want %q — cross-session bleed", sid, po.ReasoningEffort, wantEffort)
		}
	}
}
