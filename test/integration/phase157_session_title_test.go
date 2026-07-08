// Phase 157 cross-subsystem integration test per CLAUDE.md §17 — the
// `sessions.set_title` write exercised end-to-end against the real wire
// transport + the real sessions.Registry + the real auth.Validator, with
// NO mocks at any seam.
//
// Surfaces composed:
//
//   - internal/sessions — the StateStore-backed Registry's SetTitle.
//   - internal/sessions/protocol + internal/protocol/transports — the
//     `sessions.set_title` Service + the wire surface it is mounted on.
//   - internal/protocol/auth — the JWT validator gating identity.
//
// It is the §13 primitive-with-consumer discharge for the
// `sessions.set_title` surface: set -> inspect/list reflect it -> clear ->
// gone; a sibling-session rename within the SAME (tenant, user) succeeds;
// a foreign (tenant, user) caller naming another owner's session is
// refused not_found (existence never revealed across identities); an
// oversize title is refused 400. Runs under -race with an N>=10
// concurrency stress across distinct sessions.
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

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

const phase157Kid = "phase157-kid"

var fixedNowPhase157 = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

type phase157Deps struct {
	mux     *http.ServeMux
	priv    *ecdsa.PrivateKey
	reg     *sessions.Registry
	cleanup func()
}

func newPhase157Deps(t *testing.T) *phase157Deps {
	t.Helper()
	ctx := context.Background()
	priv, pub := loadES256Phase61(t)
	red := auditpatterns.New()

	store, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	bus, err := durable.New(ctx, config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, red, store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	taskReg, err := tasks.Open(ctx, tasks.Dependencies{
		Store: store, Bus: bus, Redactor: red,
		Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}

	reg, err := sessions.New(store, config.SessionsConfig{}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}

	projector, err := sessionsprotocol.NewListerProjector(reg)
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	sessionsSvc, err := sessionsprotocol.NewService(projector,
		sessionsprotocol.WithBus(bus),
		sessionsprotocol.WithRedactor(red),
		sessionsprotocol.WithTitleSetter(reg),
	)
	if err != nil {
		t.Fatalf("sessions/protocol.NewService: %v", err)
	}

	keys := newES256KeySet(phase157Kid, pub)
	now := func() time.Time { return fixedNowPhase157 }
	v, err := auth.NewValidator(keys, auth.WithClock(now), auth.WithRedactor(red))
	if err != nil {
		t.Fatalf("auth.NewValidator: %v", err)
	}
	mux, err := transports.NewMux(surface, bus,
		transports.WithValidator(v),
		transports.WithSessionsService(sessionsSvc),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &phase157Deps{
		mux: mux, priv: priv, reg: reg,
		cleanup: func() {
			_ = reg.CloseRegistry(ctx)
			_ = taskReg.Close(ctx)
			_ = bus.Close(ctx)
			_ = store.Close(ctx)
		},
	}
}

func phase157Claims(id identity.Identity) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowPhase157.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase157.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  []string{},
	}
}

func postPhase157(t *testing.T, srvURL, verb, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/sessions/"+verb, strings.NewReader(body))
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

// TestE2E_Phase157_SessionSetTitle is the binding consumer test for the
// `sessions.set_title` surface.
func TestE2E_Phase157_SessionSetTitle(t *testing.T) {
	deps := newPhase157Deps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()
	ctx := context.Background()

	// --- (a) set -> inspect/list reflect it -> clear -> gone ---
	idOwner := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-own"}
	ictx, _ := identity.With(ctx, idOwner)
	if _, err := deps.reg.Open(ictx, idOwner.SessionID, idOwner); err != nil {
		t.Fatalf("open s-own: %v", err)
	}
	tokOwner := signES256Wave10(t, deps.priv, phase157Claims(idOwner), phase157Kid)

	setBody := `{"identity":{"tenant":"tenant-A","user":"u-A","session":"s-own"},"session_id":"s-own","title":"  My E2E Title  "}`
	status, body := postPhase157(t, srv.URL, "set_title", setBody, tokOwner)
	if status != http.StatusOK {
		t.Fatalf("set_title: status=%d body=%s", status, body)
	}
	var setResp prototypes.SessionsSetTitleResponse
	if err := json.Unmarshal(body, &setResp); err != nil {
		t.Fatalf("decode set_title response: %v", err)
	}
	if setResp.Title != "My E2E Title" || setResp.TitleSource != "manual" {
		t.Fatalf("set_title response = %+v, want trimmed title + manual", setResp)
	}

	inspectBody := `{"session_id":"s-own","identity":{"tenant":"tenant-A","user":"u-A","session":"s-own"}}`
	istatus, ibody := postPhase157(t, srv.URL, "inspect", inspectBody, tokOwner)
	if istatus != http.StatusOK {
		t.Fatalf("inspect: status=%d body=%s", istatus, ibody)
	}
	var inspectResp prototypes.SessionsInspectResponse
	if err := json.Unmarshal(ibody, &inspectResp); err != nil {
		t.Fatalf("decode inspect response: %v", err)
	}
	if inspectResp.Row.Title != "My E2E Title" || inspectResp.Row.TitleSource != "manual" {
		t.Fatalf("inspect row = %+v, want the set title", inspectResp.Row)
	}

	listBody := `{"identity":{"tenant":"tenant-A","user":"u-A","session":"s-own"},"filter":{}}`
	lstatus, lbody := postPhase157(t, srv.URL, "list", listBody, tokOwner)
	if lstatus != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", lstatus, lbody)
	}
	var listResp prototypes.SessionsListResponse
	if err := json.Unmarshal(lbody, &listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	found := false
	for _, r := range listResp.Rows {
		if r.SessionID == "s-own" {
			found = true
			if r.Title != "My E2E Title" {
				t.Errorf("list row title = %q, want %q", r.Title, "My E2E Title")
			}
		}
	}
	if !found {
		t.Fatalf("s-own not found in sessions.list rows: %+v", listResp.Rows)
	}

	// Clear: empty title.
	clearBody := `{"identity":{"tenant":"tenant-A","user":"u-A","session":"s-own"},"session_id":"s-own","title":""}`
	cstatus, cbody := postPhase157(t, srv.URL, "set_title", clearBody, tokOwner)
	if cstatus != http.StatusOK {
		t.Fatalf("clear set_title: status=%d body=%s", cstatus, cbody)
	}
	istatus2, ibody2 := postPhase157(t, srv.URL, "inspect", inspectBody, tokOwner)
	if istatus2 != http.StatusOK {
		t.Fatalf("post-clear inspect: status=%d body=%s", istatus2, ibody2)
	}
	var inspectResp2 prototypes.SessionsInspectResponse
	if err := json.Unmarshal(ibody2, &inspectResp2); err != nil {
		t.Fatalf("decode post-clear inspect response: %v", err)
	}
	if inspectResp2.Row.Title != "" || inspectResp2.Row.TitleSource != "" {
		t.Fatalf("post-clear inspect row = %+v, want both empty", inspectResp2.Row)
	}

	// --- (b) sibling-session rename within the SAME (tenant, user) ---
	idSibling := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-sibling"}
	sctx, _ := identity.With(ctx, idSibling)
	if _, err := deps.reg.Open(sctx, idSibling.SessionID, idSibling); err != nil {
		t.Fatalf("open s-sibling: %v", err)
	}
	// The caller's OWN connecting session is s-own; the TARGET is the
	// sibling s-sibling — the (tenant, user) write scope (D-288).
	siblingBody := `{"identity":{"tenant":"tenant-A","user":"u-A","session":"s-own"},"session_id":"s-sibling","title":"Sibling"}`
	sstatus, sbody := postPhase157(t, srv.URL, "set_title", siblingBody, tokOwner)
	if sstatus != http.StatusOK {
		t.Fatalf("sibling set_title: status=%d body=%s", sstatus, sbody)
	}
	siblingInspectBody := `{"session_id":"s-sibling","identity":{"tenant":"tenant-A","user":"u-A","session":"s-own"}}`
	sistatus, sibody := postPhase157(t, srv.URL, "inspect", siblingInspectBody, tokOwner)
	if sistatus != http.StatusOK {
		t.Fatalf("sibling inspect: status=%d body=%s", sistatus, sibody)
	}
	var siblingInspect prototypes.SessionsInspectResponse
	if err := json.Unmarshal(sibody, &siblingInspect); err != nil {
		t.Fatalf("decode sibling inspect: %v", err)
	}
	if siblingInspect.Row.Title != "Sibling" {
		t.Fatalf("sibling row title = %q, want %q", siblingInspect.Row.Title, "Sibling")
	}

	// --- (c) foreign (tenant, user) caller naming another owner's session ---
	// A valid tenant-B/u-B token targeting tenant-A's s-own session id.
	idAttacker := identity.Identity{TenantID: "tenant-B", UserID: "u-B", SessionID: "s-attacker"}
	actx, _ := identity.With(ctx, idAttacker)
	if _, err := deps.reg.Open(actx, idAttacker.SessionID, idAttacker); err != nil {
		t.Fatalf("open s-attacker: %v", err)
	}
	tokAttacker := signES256Wave10(t, deps.priv, phase157Claims(idAttacker), phase157Kid)
	attackBody := `{"identity":{"tenant":"tenant-B","user":"u-B","session":"s-attacker"},"session_id":"s-own","title":"pwned"}`
	astatus, abody := postPhase157(t, srv.URL, "set_title", attackBody, tokAttacker)
	if astatus != http.StatusNotFound {
		t.Fatalf("cross-identity set_title: status=%d, want 404 not_found; body=%s", astatus, abody)
	}
	// The victim's title is untouched (still cleared from step (a)).
	istatus3, ibody3 := postPhase157(t, srv.URL, "inspect", inspectBody, tokOwner)
	if istatus3 != http.StatusOK {
		t.Fatalf("post-attack inspect: status=%d body=%s", istatus3, ibody3)
	}
	var inspectResp3 prototypes.SessionsInspectResponse
	if err := json.Unmarshal(ibody3, &inspectResp3); err != nil {
		t.Fatalf("decode post-attack inspect: %v", err)
	}
	if inspectResp3.Row.Title != "" {
		t.Fatalf("victim title was mutated by the cross-identity attempt: %+v", inspectResp3.Row)
	}

	// --- (d) foreign body identity (mismatching the verified JWT) -> 401 ---
	foreignIdentityBody := `{"identity":{"tenant":"tenant-OTHER","user":"u-A","session":"s-own"},"session_id":"s-own","title":"x"}`
	fistatus, fibody := postPhase157(t, srv.URL, "set_title", foreignIdentityBody, tokOwner)
	if fistatus != http.StatusUnauthorized {
		t.Fatalf("foreign body identity set_title: status=%d, want 401; body=%s", fistatus, fibody)
	}

	// --- (e) oversize title -> 400, never a silent clamp ---
	oversize := strings.Repeat("x", sessions.MaxSessionTitleLen+1)
	oversizeBody := fmt.Sprintf(`{"identity":{"tenant":"tenant-A","user":"u-A","session":"s-own"},"session_id":"s-own","title":%q}`, oversize)
	ostatus, obody := postPhase157(t, srv.URL, "set_title", oversizeBody, tokOwner)
	if ostatus != http.StatusBadRequest {
		t.Fatalf("oversize set_title: status=%d, want 400; body=%s", ostatus, obody)
	}
}

// TestE2E_Phase157_ConcurrentSetTitle_NoCrossTalk is the §17.3 concurrency
// stress: N>=10 concurrent producers exercise `sessions.set_title` against
// DISTINCT sessions over the wire transport, asserting no cross-talk and
// no goroutine leak after teardown.
func TestE2E_Phase157_ConcurrentSetTitle_NoCrossTalk(t *testing.T) {
	deps := newPhase157Deps(t)
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()
	ctx := context.Background()

	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		id := identity.Identity{TenantID: fmt.Sprintf("t-%d", i), UserID: fmt.Sprintf("u-%d", i), SessionID: fmt.Sprintf("s-%d", i)}
		ictx, _ := identity.With(ctx, id)
		if _, err := deps.reg.Open(ictx, id.SessionID, id); err != nil {
			t.Fatalf("open %s: %v", id.SessionID, err)
		}
		wg.Add(1)
		go func(id identity.Identity, i int) {
			defer wg.Done()
			tok := signES256Wave10(t, deps.priv, phase157Claims(id), phase157Kid)
			title := fmt.Sprintf("concurrent-title-%d", i)
			body := fmt.Sprintf(`{"identity":{"tenant":%q,"user":%q,"session":%q},"session_id":%q,"title":%q}`,
				id.TenantID, id.UserID, id.SessionID, id.SessionID, title)
			status, respBody := postPhase157(t, srv.URL, "set_title", body, tok)
			if status != http.StatusOK {
				t.Errorf("set_title %s: status=%d body=%s", id.SessionID, status, respBody)
				return
			}
			inspectBody := fmt.Sprintf(`{"session_id":%q,"identity":{"tenant":%q,"user":%q,"session":%q}}`,
				id.SessionID, id.TenantID, id.UserID, id.SessionID)
			istatus, ibody := postPhase157(t, srv.URL, "inspect", inspectBody, tok)
			if istatus != http.StatusOK {
				t.Errorf("inspect %s: status=%d body=%s", id.SessionID, istatus, ibody)
				return
			}
			var resp prototypes.SessionsInspectResponse
			if err := json.Unmarshal(ibody, &resp); err != nil {
				t.Errorf("decode inspect %s: %v", id.SessionID, err)
				return
			}
			if resp.Row.Title != title {
				t.Errorf("cross-talk: session %s has title %q, want %q", id.SessionID, resp.Row.Title, title)
			}
			if resp.Row.TenantID != id.TenantID {
				t.Errorf("identity bleed: session %s tenant = %q, want %q", id.SessionID, resp.Row.TenantID, id.TenantID)
			}
		}(id, i)
	}
	wg.Wait()

	deps.cleanup()
}
