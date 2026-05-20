// Phase 72e cross-subsystem integration test per CLAUDE.md §17 — the
// `pause.list` Protocol method exercised end-to-end against the real
// wire transport + the real pauseresume.Coordinator (Phase 50) + the
// real auth.Validator/Middleware (Phase 61) + the real in-mem
// ArtifactStore, with no mocks at any seam.
//
// Surfaces composed:
//
//   - Phase 50 internal/runtime/pauseresume — the unified Coordinator
//     whose registry the `pause.list` snapshot projects.
//   - Phase 60 internal/protocol/transports — the wire surface the
//     pause.list handler is mounted on.
//   - Phase 61 internal/protocol/auth — the JWT validator + middleware
//     gating the cross-tenant `admin` scope claim (D-079).
//   - Phase 17/18/19 internal/artifacts — the ArtifactStore the D-026
//     heavy-content bypass routes oversized pause payloads through.
//
// This test ships the §13 primitive-with-consumer discharge for Phase
// 72e: it is the first consumer of the `pause.list` wire surface,
// exercising it end-to-end at the wire boundary — two-tenant scope, the
// cross-tenant reject path without the admin claim, the admin-claim
// accept path, and the D-026 heavy-payload bypass with a bus assertion.
package integration_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
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
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

const phase72eKid = "phase72e-kid"

var fixedNowPhase72e = time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)

const phase72eHeavyThreshold = 1024

type phase72eDeps struct {
	mux     *http.ServeMux
	coord   pauseresume.Coordinator
	bus     events.EventBus
	priv    *ecdsa.PrivateKey
	cleanup func()
}

func newPhase72eDeps(t *testing.T) *phase72eDeps {
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

	artStore, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("artifacts.Open: %v", err)
	}

	// The real unified Coordinator — bus-wired so its own
	// pause.requested events flow, exactly as production wires it.
	coord := pauseresume.New(pauseresume.WithBus(bus))

	keys := newES256KeySet(phase72eKid, pub)
	now := func() time.Time { return fixedNowPhase72e }
	v, err := auth.NewValidator(keys, auth.WithClock(now), auth.WithRedactor(red))
	if err != nil {
		_ = artStore.Close(context.Background())
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("auth.NewValidator: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithValidator(v),
		transports.WithPauseList(coord, artStore, phase72eHeavyThreshold),
	)
	if err != nil {
		_ = artStore.Close(context.Background())
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &phase72eDeps{
		mux:   mux,
		coord: coord,
		bus:   bus,
		priv:  priv,
		cleanup: func() {
			_ = artStore.Close(context.Background())
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// phase72eClaims mints a JWT MapClaims with the test's standard
// expiry / issuer / identity shape.
func phase72eClaims(id identity.Identity, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowPhase72e.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase72e.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
}

// seedPhase72ePause records a pause on the Coordinator under id.
func seedPhase72ePause(t *testing.T, coord pauseresume.Coordinator, id identity.Identity, runID string, payload map[string]any) {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), id, runID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	if _, err := coord.Request(ctx, pauseresume.PauseRequest{
		Identity: id,
		Reason:   pauseresume.ReasonApprovalRequired,
		Payload:  payload,
	}); err != nil {
		t.Fatalf("Request: %v", err)
	}
}

// postPauseList issues a POST /v1/pause/list with the supplied JWT.
func postPauseList(t *testing.T, srvURL, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/pause/list", strings.NewReader(body))
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

// TestE2E_Phase72e_PauseListTwoTenantScope is the §13 primitive-with-
// consumer binding test. Two tenants each pause a run; pause.list from
// tenant A returns only A's row; a cross-tenant filter without the
// admin claim is rejected 403; an admin-scoped caller naming both
// tenants gets both rows.
func TestE2E_Phase72e_PauseListTwoTenantScope(t *testing.T) {
	deps := newPhase72eDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	idA := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	idB := identity.Identity{TenantID: "tenant-B", UserID: "u-B", SessionID: "s-B"}
	seedPhase72ePause(t, deps.coord, idA, "run-a", map[string]any{"who": "A"})
	seedPhase72ePause(t, deps.coord, idB, "run-b", map[string]any{"who": "B"})

	// (1) pause.list from A's identity returns only A's row.
	tokA := signES256Wave10(t, deps.priv, phase72eClaims(idA, nil), phase72eKid)
	status, body := postPauseList(t, srv.URL, `{}`, tokA)
	if status != http.StatusOK {
		t.Fatalf("A self-scope: status = %d, want 200; body=%s", status, body)
	}
	var respA prototypes.PauseListResponse
	if err := json.Unmarshal(body, &respA); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if respA.TotalRows != 1 || len(respA.Snapshots) != 1 {
		t.Fatalf("A self-scope: TotalRows=%d len=%d, want 1/1", respA.TotalRows, len(respA.Snapshots))
	}
	if respA.Snapshots[0].Identity.Tenant != "tenant-A" {
		t.Fatalf("A self-scope: snapshot tenant = %q, want tenant-A", respA.Snapshots[0].Identity.Tenant)
	}

	// (2) pause.list from A with TenantIDs=["tenant-B"] and NO admin
	// claim → 403 CodeIdentityScopeRequired.
	status, body = postPauseList(t, srv.URL, `{"filter":{"tenant_ids":["tenant-B"]}}`, tokA)
	if status != http.StatusForbidden {
		t.Fatalf("A cross-tenant no-admin: status = %d, want 403; body=%s", status, body)
	}
	var perr protoerrors.Error
	if err := json.Unmarshal(body, &perr); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if perr.Code != protoerrors.CodeIdentityScopeRequired {
		t.Fatalf("A cross-tenant no-admin: Code = %q, want %q", perr.Code, protoerrors.CodeIdentityScopeRequired)
	}

	// (3) pause.list from an admin identity with TenantIDs=["tenant-A",
	// "tenant-B"] returns both rows.
	adminID := identity.Identity{TenantID: "tenant-A", UserID: "u-admin", SessionID: "s-admin"}
	tokAdmin := signES256Wave10(t, deps.priv,
		phase72eClaims(adminID, []string{string(auth.ScopeAdmin)}), phase72eKid)
	status, body = postPauseList(t, srv.URL,
		`{"filter":{"tenant_ids":["tenant-A","tenant-B"]}}`, tokAdmin)
	if status != http.StatusOK {
		t.Fatalf("admin cross-tenant: status = %d, want 200; body=%s", status, body)
	}
	var respAdmin prototypes.PauseListResponse
	if err := json.Unmarshal(body, &respAdmin); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if respAdmin.TotalRows != 2 {
		t.Fatalf("admin cross-tenant: TotalRows = %d, want 2", respAdmin.TotalRows)
	}
	// Deterministic PausedAt-descending order — both rows present.
	tenants := map[string]bool{}
	for _, s := range respAdmin.Snapshots {
		tenants[s.Identity.Tenant] = true
	}
	if !tenants["tenant-A"] || !tenants["tenant-B"] {
		t.Fatalf("admin cross-tenant: missing tenant rows: %+v", tenants)
	}
}

// TestE2E_Phase72e_PauseListMissingIdentityFailsLoudly — a request with
// no bearer token is rejected 401, never silently downgraded.
func TestE2E_Phase72e_PauseListMissingIdentityFailsLoudly(t *testing.T) {
	deps := newPhase72eDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	// No Authorization header — the auth middleware rejects before the
	// handler runs.
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/pause/list", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token: status = %d, want 401", resp.StatusCode)
	}
}

// TestE2E_Phase72e_PauseListMalformedPageSizeRejected — a PageSize above
// the max is rejected 400, never silently clamped.
func TestE2E_Phase72e_PauseListMalformedPageSizeRejected(t *testing.T) {
	deps := newPhase72eDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	tok := signES256Wave10(t, deps.priv, phase72eClaims(id, nil), phase72eKid)
	status, body := postPauseList(t, srv.URL, `{"page_size":9999}`, tok)
	if status != http.StatusBadRequest {
		t.Fatalf("oversized page_size: status = %d, want 400; body=%s", status, body)
	}
	var perr protoerrors.Error
	if err := json.Unmarshal(body, &perr); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if perr.Code != protoerrors.CodeInvalidRequest {
		t.Fatalf("oversized page_size: Code = %q, want %q", perr.Code, protoerrors.CodeInvalidRequest)
	}
}

// TestE2E_Phase72e_PauseListHeavyPayloadRoutedThroughArtifactStore — a
// pause whose payload exceeds the heavy-content threshold round-trips
// through the ArtifactStore: the snapshot row carries PayloadRef (not
// inline bytes) and a pause.payload_artifact_routed event fired on the
// bus (D-026 — the bypass is loud).
func TestE2E_Phase72e_PauseListHeavyPayloadRoutedThroughArtifactStore(t *testing.T) {
	deps := newPhase72eDeps(t)
	defer deps.cleanup()

	// Subscribe to the bus BEFORE seeding so we catch the routed event.
	id := identity.Identity{TenantID: "tenant-heavy", UserID: "u-h", SessionID: "s-h"}
	sub, err := deps.bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{pauseresume.EventTypePausePayloadArtifactRouted},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	heavy := strings.Repeat("z", phase72eHeavyThreshold+2048)
	seedPhase72ePause(t, deps.coord, id, "run-heavy", map[string]any{"blob": heavy})

	tok := signES256Wave10(t, deps.priv, phase72eClaims(id, nil), phase72eKid)
	status, body := postPauseList(t, srv.URL, `{}`, tok)
	if status != http.StatusOK {
		t.Fatalf("heavy payload: status = %d, want 200; body=%s", status, body)
	}
	var resp prototypes.PauseListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Snapshots) != 1 {
		t.Fatalf("heavy payload: len(Snapshots) = %d, want 1", len(resp.Snapshots))
	}
	snap := resp.Snapshots[0]
	if snap.PayloadRef == nil {
		t.Fatal("heavy payload: PayloadRef = nil, want a populated ref (D-026)")
	}
	if snap.Payload != nil {
		t.Errorf("heavy payload: inline Payload = %+v, want nil when PayloadRef set", snap.Payload)
	}
	if !bytes.Contains(body, []byte(snap.PayloadRef.ID)) || snap.PayloadRef.ID == "" {
		t.Errorf("heavy payload: PayloadRef.ID empty/missing")
	}

	// The pause.payload_artifact_routed observation must have fired.
	select {
	case ev := <-sub.Events():
		if ev.Type != pauseresume.EventTypePausePayloadArtifactRouted {
			t.Fatalf("bus event type = %q, want pause.payload_artifact_routed", ev.Type)
		}
		routed, ok := ev.Payload.(pauseresume.PausePayloadArtifactRoutedPayload)
		if !ok {
			t.Fatalf("bus event payload type = %T, want PausePayloadArtifactRoutedPayload", ev.Payload)
		}
		if routed.ArtifactID != snap.PayloadRef.ID {
			t.Errorf("routed ArtifactID = %q, want %q", routed.ArtifactID, snap.PayloadRef.ID)
		}
		if routed.ThresholdBytes != phase72eHeavyThreshold {
			t.Errorf("routed ThresholdBytes = %d, want %d", routed.ThresholdBytes, phase72eHeavyThreshold)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pause.payload_artifact_routed event")
	}
}

// TestE2E_Phase72e_PauseListConcurrentStress runs N≥10 concurrent
// pause.list callers against one shared wire surface, asserting no
// cross-talk and no goroutine leak (CLAUDE.md §17.3).
func TestE2E_Phase72e_PauseListConcurrentStress(t *testing.T) {
	deps := newPhase72eDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	const N = 24
	ids := make([]identity.Identity, N)
	for i := 0; i < N; i++ {
		ids[i] = identity.Identity{
			TenantID:  "tenant-" + string(rune('A'+i%26)) + strconv.Itoa(i),
			UserID:    "user-" + strconv.Itoa(i),
			SessionID: "session-" + strconv.Itoa(i),
		}
		seedPhase72ePause(t, deps.coord, ids[i], "run-"+strconv.Itoa(i), nil)
	}

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	wg.Add(N)
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			tok := signES256Wave10(t, deps.priv, phase72eClaims(ids[i], nil), phase72eKid)
			status, body := postPauseList(t, srv.URL, `{}`, tok)
			if status != http.StatusOK {
				errCh <- fmt.Errorf("g%d: status %d body %s", i, status, body)
				return
			}
			var resp prototypes.PauseListResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				errCh <- fmt.Errorf("g%d: decode %v", i, err)
				return
			}
			// Context-bleed check: each caller sees ONLY its own row.
			if resp.TotalRows != 1 || len(resp.Snapshots) != 1 {
				errCh <- fmt.Errorf("g%d: TotalRows=%d len=%d, want 1/1", i, resp.TotalRows, len(resp.Snapshots))
				return
			}
			if resp.Snapshots[0].Identity.Tenant != ids[i].TenantID {
				errCh <- fmt.Errorf("g%d: context bleed — tenant %q, want %q",
					i, resp.Snapshots[0].Identity.Tenant, ids[i].TenantID)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	// httptest keeps per-connection serve goroutines and the client
	// keeps idle-connection goroutines alive briefly after the final
	// response; they drain within tens of ms. Poll with a bounded
	// timeout rather than snapshotting instantly (CLAUDE.md §17.4
	// eventually-style assertion) so a loaded CI runner does not flake.
	deadline := time.Now().Add(2 * time.Second)
	var after int
	for {
		after = runtime.NumGoroutine()
		if after <= baseline+8 {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("goroutine leak: baseline=%d after=%d", baseline, after)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}
