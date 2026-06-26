// Phase 130 cross-subsystem integration test per CLAUDE.md §17 — the
// `sessions.delete` data-lifecycle erasure exercised end-to-end against
// the real wire transport + the real sessions.Registry + the real
// State / Memory / Artifact production drivers + the real
// auth.Validator/Middleware, with NO mocks at any seam.
//
// Surfaces composed:
//
//   - internal/sessions — the StateStore-backed Registry + the
//     CascadeEraser (the three-store erasure cascade).
//   - internal/state, internal/memory, internal/artifacts — the real
//     scoped stores the cascade deletes.
//   - internal/sessions/protocol + internal/protocol/transports — the
//     `sessions.delete` Service + the wire surface it is mounted on.
//   - internal/protocol/auth — the JWT validator gating identity.
//
// It is the §13 primitive-with-consumer discharge for the
// `sessions.delete` surface: full lifecycle erasure → inspect-404 +
// cross-store erasure + state.history empty, a foreign-target 401, and a
// running-task 409 with stores untouched. Runs under -race.
package integration_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
)

const phase130Kid = "phase130-kid"

var fixedNowPhase130 = time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)

type phase130Deps struct {
	mux     *http.ServeMux
	priv    *ecdsa.PrivateKey
	store   state.StateStore
	mem     memory.MemoryStore
	arts    artifacts.ArtifactStore
	cleanup func()
}

// phase130RunningSession is the session id the running-task probe reports
// as RUNNING, so its erasure is refused with 409.
const phase130RunningSession = "s-running"

func newPhase130DepsWithRegistry(t *testing.T) (*phase130Deps, *sessions.Registry) {
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
	mem, err := memory.Open(ctx, memory.ConfigSnapshot{
		Driver: "inmem", Strategy: memory.StrategyTruncation, BudgetTokens: 1000,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	arts, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	taskReg, err := tasks.Open(ctx, tasks.Dependencies{
		Store: store, Bus: bus, Redactor: audit.Redactor(red),
		Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}

	// Probe: report a RUNNING task only for the dedicated running session.
	probe := func(_ context.Context, q identity.Quadruple) (bool, error) {
		return q.SessionID == phase130RunningSession, nil
	}
	reg, err := sessions.New(store, config.SessionsConfig{}, bus,
		sessions.WithGCPolicy(sessions.GCPolicy{RunningProbe: probe}))
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}

	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: reg, State: store, Memory: mem, Artifacts: arts, Bus: bus, Redactor: red,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	projector, err := sessionsprotocol.NewListerProjector(reg)
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	sessionsSvc, err := sessionsprotocol.NewService(projector,
		sessionsprotocol.WithBus(bus),
		sessionsprotocol.WithRedactor(red),
		sessionsprotocol.WithEraser(eraser),
	)
	if err != nil {
		t.Fatalf("sessions/protocol.NewService: %v", err)
	}

	keys := newES256KeySet(phase130Kid, pub)
	now := func() time.Time { return fixedNowPhase130 }
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

	return &phase130Deps{
		mux: mux, priv: priv, store: store, mem: mem, arts: arts,
		cleanup: func() {
			_ = reg.CloseRegistry(ctx)
			_ = taskReg.Close(ctx)
			_ = mem.Close(ctx)
			_ = arts.Close(ctx)
			_ = bus.Close(ctx)
			_ = store.Close(ctx)
		},
	}, reg
}

func phase130Claims(id identity.Identity) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowPhase130.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase130.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  []string{},
	}
}

func postPhase130(t *testing.T, srvURL, verb, body, token string) (int, []byte) {
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

// TestE2E_Phase130_SessionErasure is the binding consumer test for the
// `sessions.delete` surface.
func TestE2E_Phase130_SessionErasure(t *testing.T) {
	deps, reg := newPhase130DepsWithRegistry(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()
	ctx := context.Background()

	// --- (a) full lifecycle erasure ---
	idErase := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-erase"}
	ictx, _ := identity.With(ctx, idErase)
	if _, err := reg.Open(ictx, idErase.SessionID, idErase); err != nil {
		t.Fatalf("open s-erase: %v", err)
	}
	if err := deps.mem.AddTurn(ictx, identity.Quadruple{Identity: idErase}, memory.ConversationTurn{
		UserMessage: "secret question", AssistantResponse: "secret answer",
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: idErase.TenantID, UserID: idErase.UserID, SessionID: idErase.SessionID}
	if _, err := deps.arts.PutBytes(ctx, scope, []byte("private blob"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	tokErase := signES256Wave10(t, deps.priv, phase130Claims(idErase), phase130Kid)
	status, body := postPhase130(t, srv.URL, "delete",
		`{"identity":{"tenant":"tenant-A","user":"u-A","session":"s-erase"}}`, tokErase)
	if status != http.StatusOK {
		t.Fatalf("delete: status=%d body=%s", status, body)
	}
	var resp prototypes.SessionsDeleteResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	if !resp.Deleted || !resp.MemoryPurged || resp.SessionID != "s-erase" {
		t.Fatalf("delete response = %+v", resp)
	}
	if resp.ArtifactsDeleted != 1 {
		t.Errorf("ArtifactsDeleted = %d, want 1", resp.ArtifactsDeleted)
	}
	if resp.StateRecordsDeleted <= 0 {
		t.Errorf("StateRecordsDeleted = %d, want > 0", resp.StateRecordsDeleted)
	}

	// subsequent inspect → 404 not_found.
	istatus, ibody := postPhase130(t, srv.URL, "inspect",
		`{"session_id":"s-erase","identity":{"tenant":"tenant-A","user":"u-A","session":"s-erase"}}`, tokErase)
	if istatus != http.StatusNotFound {
		t.Fatalf("post-erasure inspect: status=%d, want 404; body=%s", istatus, ibody)
	}

	// artifacts gone.
	refs, err := deps.arts.List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("artifacts survived erasure: %d", len(refs))
	}
	// memory clean.
	patch, err := deps.mem.GetLLMContext(ctx, identity.Quadruple{Identity: idErase})
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if len(patch.RecentTurns) != 0 {
		t.Errorf("memory survived erasure: %d turns", len(patch.RecentTurns))
	}
	// state.history empty: the erased triple's durable event stream is gone.
	if _, err := deps.store.Load(ctx, identity.Quadruple{Identity: idErase}, "events.durable.head"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("erased triple durable head survived (state.history non-empty): err=%v", err)
	}
	if _, err := deps.store.Load(ctx, identity.Quadruple{Identity: idErase}, "session.lifecycle"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("session.lifecycle survived erasure: err=%v", err)
	}

	// --- (b) foreign-target erasure → 401 ---
	// A valid tenant-A token, but the body names a different tenant.
	fstatus, fbody := postPhase130(t, srv.URL, "delete",
		`{"identity":{"tenant":"tenant-OTHER","user":"u-A","session":"s-erase"}}`, tokErase)
	if fstatus != http.StatusUnauthorized {
		t.Fatalf("foreign-target delete: status=%d, want 401; body=%s", fstatus, fbody)
	}

	// --- (c) running-task erasure → 409 with stores untouched ---
	idRun := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: phase130RunningSession}
	rctx, _ := identity.With(ctx, idRun)
	if _, err := reg.Open(rctx, idRun.SessionID, idRun); err != nil {
		t.Fatalf("open running session: %v", err)
	}
	runScope := artifacts.ArtifactScope{TenantID: idRun.TenantID, UserID: idRun.UserID, SessionID: idRun.SessionID}
	if _, err := deps.arts.PutBytes(ctx, runScope, []byte("still here"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes running: %v", err)
	}
	tokRun := signES256Wave10(t, deps.priv, phase130Claims(idRun), phase130Kid)
	rstatus, rbody := postPhase130(t, srv.URL, "delete",
		`{"identity":{"tenant":"tenant-A","user":"u-A","session":"`+phase130RunningSession+`"}}`, tokRun)
	if rstatus != http.StatusConflict {
		t.Fatalf("running-task delete: status=%d, want 409; body=%s", rstatus, rbody)
	}
	var rerr protoerrors.Error
	if err := json.Unmarshal(rbody, &rerr); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if rerr.Code != protoerrors.CodeSessionRunning {
		t.Errorf("409 code = %q, want %q", rerr.Code, protoerrors.CodeSessionRunning)
	}
	// stores untouched: the running session's record + artifact survive.
	if _, err := deps.store.Load(ctx, identity.Quadruple{Identity: idRun}, "session.lifecycle"); err != nil {
		t.Errorf("running-refusal deleted the session record: %v", err)
	}
	runRefs, err := deps.arts.List(ctx, runScope)
	if err != nil {
		t.Fatalf("List running: %v", err)
	}
	if len(runRefs) != 1 {
		t.Errorf("running-refusal touched artifacts: %d, want 1", len(runRefs))
	}
}
