package stream_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// pauseListID is a documented dummy identity triple — no secrets
// (CLAUDE.md §13: fixtures carry documented dummy values).
var pauseListID = identity.Identity{TenantID: "t-pl", UserID: "u-pl", SessionID: "s-pl"}

const heavyThresholdBytes = 1024

// newArtifactStore opens a fresh in-memory ArtifactStore for the
// heavy-content bypass tests.
func newArtifactStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	s, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// newPauseListHandler builds a PauseListHandler over a fresh Coordinator
// + in-mem ArtifactStore; returns both so the test can seed pauses.
func newPauseListHandler(t *testing.T, opts ...stream.PauseListOption) (*stream.PauseListHandler, pauseresume.Coordinator) {
	t.Helper()
	coord := pauseresume.New()
	store := newArtifactStore(t)
	h, err := stream.NewPauseListHandler(coord, store, heavyThresholdBytes, opts...)
	if err != nil {
		t.Fatalf("NewPauseListHandler: %v", err)
	}
	return h, coord
}

// seedPause records a pause on coord under the given identity.
func seedPause(t *testing.T, coord pauseresume.Coordinator, id identity.Identity, payload map[string]any) {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), id, "run-x")
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

// doRequest issues a POST against the handler. id (when non-nil) is set
// via the X-Harbor-* carrier headers; scopes (when non-nil) are injected
// into the request context (simulating auth.Middleware).
func doRequest(t *testing.T, h http.Handler, body string, id *identity.Identity, scopes []auth.Scope) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/pause/list", strings.NewReader(body))
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

func TestPauseList_HappyPathReturnsPaginatedRows(t *testing.T) {
	h, coord := newPauseListHandler(t)
	seedPause(t, coord, pauseListID, map[string]any{"k": "v"})

	status, body := doRequest(t, h, `{}`, &pauseListID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var resp prototypes.PauseListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TotalRows != 1 || len(resp.Snapshots) != 1 {
		t.Fatalf("TotalRows=%d len(Snapshots)=%d, want 1/1", resp.TotalRows, len(resp.Snapshots))
	}
	if resp.Page != 1 || resp.PageSize != prototypes.DefaultPauseListPageSize {
		t.Fatalf("Page/PageSize = %d/%d, want 1/%d", resp.Page, resp.PageSize, prototypes.DefaultPauseListPageSize)
	}
	snap := resp.Snapshots[0]
	if snap.State != prototypes.PauseStatePaused {
		t.Errorf("State = %q, want paused", snap.State)
	}
	if snap.Payload["k"] != "v" {
		t.Errorf("Payload = %+v, want inline {k:v}", snap.Payload)
	}
	if snap.PayloadRef != nil {
		t.Errorf("PayloadRef = %+v, want nil for a small payload", snap.PayloadRef)
	}
}

func TestPauseList_MissingIdentityRejected401(t *testing.T) {
	h, _ := newPauseListHandler(t)
	status, body := doRequest(t, h, `{}`, nil, nil) // no identity carrier
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, body)
	}
	assertErrorCode(t, body, protoerrors.CodeIdentityRequired)
}

func TestPauseList_CrossTenantWithoutAdminRejected403(t *testing.T) {
	h, coord := newPauseListHandler(t)
	seedPause(t, coord, pauseListID, nil)

	body := `{"filter":{"tenant_ids":["foreign-tenant"]}}`
	status, respBody := doRequest(t, h, body, &pauseListID, nil) // no admin scope
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", status, respBody)
	}
	assertErrorCode(t, respBody, protoerrors.CodeIdentityScopeRequired)
}

func TestPauseList_CrossTenantWithAdminAccepted(t *testing.T) {
	h, coord := newPauseListHandler(t)
	foreign := identity.Identity{TenantID: "foreign-tenant", UserID: "u", SessionID: "s"}
	seedPause(t, coord, pauseListID, nil)
	seedPause(t, coord, foreign, nil)

	body := `{"filter":{"tenant_ids":["t-pl","foreign-tenant"]}}`
	status, respBody := doRequest(t, h, body, &pauseListID, []auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, respBody)
	}
	var resp prototypes.PauseListResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TotalRows != 2 {
		t.Fatalf("TotalRows = %d, want 2 (both tenants)", resp.TotalRows)
	}
}

func TestPauseList_MalformedPageSizeRejected400(t *testing.T) {
	h, _ := newPauseListHandler(t)
	body := `{"page_size":5000}` // > MaxPauseListPageSize
	status, respBody := doRequest(t, h, body, &pauseListID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, respBody)
	}
	assertErrorCode(t, respBody, protoerrors.CodeInvalidRequest)
}

func TestPauseList_MalformedStatusEnumRejected400(t *testing.T) {
	h, _ := newPauseListHandler(t)
	body := `{"filter":{"status":["bogus"]}}`
	status, respBody := doRequest(t, h, body, &pauseListID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, respBody)
	}
	assertErrorCode(t, respBody, protoerrors.CodeInvalidRequest)
}

func TestPauseList_HeavyPayloadRoutedToArtifactStore(t *testing.T) {
	h, coord := newPauseListHandler(t)
	// A payload whose JSON serialised size exceeds the threshold.
	heavy := strings.Repeat("x", heavyThresholdBytes+512)
	seedPause(t, coord, pauseListID, map[string]any{"blob": heavy})

	status, body := doRequest(t, h, `{}`, &pauseListID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var resp prototypes.PauseListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Snapshots) != 1 {
		t.Fatalf("len(Snapshots) = %d, want 1", len(resp.Snapshots))
	}
	snap := resp.Snapshots[0]
	if snap.PayloadRef == nil {
		t.Fatal("PayloadRef = nil, want a populated ref for a heavy payload (D-026)")
	}
	if snap.Payload != nil {
		t.Errorf("Payload = %+v, want nil when PayloadRef is set", snap.Payload)
	}
	if snap.PayloadRef.ID == "" || snap.PayloadRef.SHA256 == "" {
		t.Errorf("PayloadRef incomplete: %+v", snap.PayloadRef)
	}
}

func TestPauseList_BodyIdentityMismatchRejected401(t *testing.T) {
	h, _ := newPauseListHandler(t)
	// The carrier headers say t-pl; the body claims a different tenant.
	body := `{"identity":{"tenant":"other","user":"u-pl","session":"s-pl"}}`
	status, respBody := doRequest(t, h, body, &pauseListID, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, respBody)
	}
	assertErrorCode(t, respBody, protoerrors.CodeIdentityRequired)
}

func TestPauseList_NonPOSTRejected(t *testing.T) {
	h, _ := newPauseListHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/pause/list", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

func TestNewPauseListHandler_FailsClosedOnNilDeps(t *testing.T) {
	store := newArtifactStore(t)
	coord := pauseresume.New()
	if _, err := stream.NewPauseListHandler(nil, store, heavyThresholdBytes); err == nil {
		t.Error("NewPauseListHandler(nil coord): err = nil, want ErrPauseListMisconfigured")
	}
	if _, err := stream.NewPauseListHandler(coord, nil, heavyThresholdBytes); err == nil {
		t.Error("NewPauseListHandler(nil store): err = nil, want ErrPauseListMisconfigured")
	}
	if _, err := stream.NewPauseListHandler(coord, store, 0); err == nil {
		t.Error("NewPauseListHandler(zero threshold): err = nil, want ErrPauseListMisconfigured")
	}
}

// TestPauseList_HeavyPayloadEmitsRoutedEventOnBus exercises the
// WithPauseListBus option: a heavy payload routed through the
// ArtifactStore also publishes a pause.payload_artifact_routed event.
func TestPauseList_HeavyPayloadEmitsRoutedEventOnBus(t *testing.T) {
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	coord := pauseresume.New()
	store := newArtifactStore(t)
	h, err := stream.NewPauseListHandler(coord, store, heavyThresholdBytes,
		stream.WithPauseListBus(bus), stream.WithPauseListLogger(slog.Default()))
	if err != nil {
		t.Fatalf("NewPauseListHandler: %v", err)
	}

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: pauseListID.TenantID, User: pauseListID.UserID, Session: pauseListID.SessionID,
		Types: []events.EventType{pauseresume.EventTypePausePayloadArtifactRouted},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	seedPause(t, coord, pauseListID, map[string]any{"blob": strings.Repeat("q", heavyThresholdBytes+256)})

	status, body := doRequest(t, h, `{}`, &pauseListID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != pauseresume.EventTypePausePayloadArtifactRouted {
			t.Fatalf("event type = %q, want pause.payload_artifact_routed", ev.Type)
		}
		routed, ok := ev.Payload.(pauseresume.PausePayloadArtifactRoutedPayload)
		if !ok {
			t.Fatalf("event payload type = %T, want PausePayloadArtifactRoutedPayload", ev.Payload)
		}
		if routed.ThresholdBytes != heavyThresholdBytes {
			t.Errorf("routed ThresholdBytes = %d, want %d", routed.ThresholdBytes, heavyThresholdBytes)
		}
		if routed.ArtifactID == "" {
			t.Error("routed ArtifactID is empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pause.payload_artifact_routed event")
	}
}

// TestPauseList_ResumedRecordRendersResumedState — a resumed pause
// record surfaces with State=resumed and a populated ResumedAt.
func TestPauseList_ResumedRecordRendersResumedState(t *testing.T) {
	h, coord := newPauseListHandler(t)
	ctx, err := identity.WithRun(context.Background(), pauseListID, "run-r")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	p, err := coord.Request(ctx, pauseresume.PauseRequest{
		Identity: pauseListID, Reason: pauseresume.ReasonApprovalRequired,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := coord.Resume(ctx, p.Token, pauseresume.DecisionApprove, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	status, body := doRequest(t, h, `{"filter":{"status":["resumed"]}}`, &pauseListID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var resp prototypes.PauseListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Snapshots) != 1 {
		t.Fatalf("len(Snapshots) = %d, want 1", len(resp.Snapshots))
	}
	if resp.Snapshots[0].State != prototypes.PauseStateResumed {
		t.Errorf("State = %q, want resumed", resp.Snapshots[0].State)
	}
	if resp.Snapshots[0].ResumedAt.IsZero() {
		t.Error("ResumedAt is zero for a resumed record")
	}
}

// TestPauseList_EmptyBodyAccepted — a request with a zero-length body
// (no JSON at all) is treated as an empty PauseListRequest.
func TestPauseList_EmptyBodyAccepted(t *testing.T) {
	h, coord := newPauseListHandler(t)
	seedPause(t, coord, pauseListID, nil)
	status, body := doRequest(t, h, ``, &pauseListID, nil)
	if status != http.StatusOK {
		t.Fatalf("empty body: status = %d, want 200; body=%s", status, body)
	}
}

// TestPauseList_MalformedJSONRejected400 — a non-JSON body fails closed.
func TestPauseList_MalformedJSONRejected400(t *testing.T) {
	h, _ := newPauseListHandler(t)
	status, body := doRequest(t, h, `{not json`, &pauseListID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("malformed JSON: status = %d, want 400; body=%s", status, body)
	}
	assertErrorCode(t, body, protoerrors.CodeInvalidRequest)
}

// assertErrorCode decodes a JSON Protocol error body and asserts its
// Code matches want.
func assertErrorCode(t *testing.T, body []byte, want protoerrors.Code) {
	t.Helper()
	var e protoerrors.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body: %v (body=%s)", err, body)
	}
	if e.Code != want {
		t.Fatalf("error Code = %q, want %q (body=%s)", e.Code, want, body)
	}
}
