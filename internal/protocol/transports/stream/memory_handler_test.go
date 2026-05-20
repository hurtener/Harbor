package stream_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	memoryinmem "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// memHandlerID — documented dummy identity triple (CLAUDE.md §13).
var memHandlerID = identity.Identity{TenantID: "t-mem", UserID: "u-mem", SessionID: "s-mem"}

const memHandlerThreshold = 4096

// Bare URL paths (the route patterns carry the `POST ` ServeMux method
// prefix, which httptest.NewRequest cannot parse as a target).
const (
	memListPath   = "/v1/memory/list"
	memGetPath    = "/v1/memory/get"
	memHealthPath = "/v1/memory/health"
)

// memHandlerFixture bundles the memory handler + the live memory store
// so a test can seed turns before issuing requests.
type memHandlerFixture struct {
	handler *stream.MemoryHandler
	store   memory.MemoryStore
}

func newMemHandlerFixture(t *testing.T) memHandlerFixture {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1024,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	stateStore, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = stateStore.Close(context.Background()) })

	store, err := memoryinmem.New(memory.ConfigSnapshot{
		Driver:       "inmem",
		Strategy:     memory.StrategyTruncation,
		BudgetTokens: 1_000_000,
	}, memory.Deps{State: stateStore, Bus: bus}, memoryinmem.Options{})
	if err != nil {
		t.Fatalf("memoryinmem.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	artStore, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = artStore.Close(context.Background()) })

	agg, err := events.NewAggregator(bus)
	if err != nil {
		t.Fatalf("events.NewAggregator: %v", err)
	}

	h, err := stream.NewMemoryHandler(store, artStore, memHandlerThreshold,
		stream.WithMemoryAggregator(agg),
		stream.WithMemoryDriverName("inmem"))
	if err != nil {
		t.Fatalf("NewMemoryHandler: %v", err)
	}
	return memHandlerFixture{handler: h, store: store}
}

// seedTurn appends one conversation turn to the fixture's memory store.
func (f memHandlerFixture) seedTurn(t *testing.T, id identity.Identity, user, assistant string) {
	t.Helper()
	q := identity.Quadruple{Identity: id}
	if err := f.store.AddTurn(context.Background(), q, memory.ConversationTurn{
		UserMessage:       user,
		AssistantResponse: assistant,
		Timestamp:         time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
}

// doMemReq issues a POST against the supplied handler. id (when
// non-nil) is set via the X-Harbor-* carrier headers; scopes (when
// non-nil) are injected into ctx (simulating auth.Middleware).
func doMemReq(t *testing.T, h http.Handler, route, body string, id *identity.Identity, scopes []auth.Scope) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(body))
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

func TestMemoryHandler_NewRejectsNilDeps(t *testing.T) {
	artStore, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = artStore.Close(context.Background()) })

	if _, err := stream.NewMemoryHandler(nil, artStore, 1024); err == nil {
		t.Error("NewMemoryHandler(nil store): err = nil, want misconfigured")
	}
	// A nil ArtifactStore — build a real store first.
	f := newMemHandlerFixture(t)
	if _, err := stream.NewMemoryHandler(f.store, nil, 1024); err == nil {
		t.Error("NewMemoryHandler(nil artifacts): err = nil, want misconfigured")
	}
	if _, err := stream.NewMemoryHandler(f.store, artStore, 0); err == nil {
		t.Error("NewMemoryHandler(zero threshold): err = nil, want misconfigured")
	}
}

func TestMemoryHandler_ListHappyPath(t *testing.T) {
	f := newMemHandlerFixture(t)
	f.seedTurn(t, memHandlerID, "hello", "world")
	f.seedTurn(t, memHandlerID, "second", "turn")

	status, body := doMemReq(t, f.handler.ListHandler(), memListPath, `{}`, &memHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var resp prototypes.MemoryListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TotalRows != 2 || len(resp.Items) != 2 {
		t.Fatalf("TotalRows=%d len(Items)=%d, want 2/2", resp.TotalRows, len(resp.Items))
	}
	if resp.Page != 1 || resp.PageSize != prototypes.DefaultMemoryListPageSize {
		t.Errorf("Page/PageSize = %d/%d, want 1/%d", resp.Page, resp.PageSize, prototypes.DefaultMemoryListPageSize)
	}
}

func TestMemoryHandler_ListRejectsMissingIdentity(t *testing.T) {
	f := newMemHandlerFixture(t)
	status, body := doMemReq(t, f.handler.ListHandler(), memListPath, `{}`, nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, body)
	}
	assertMemErrCode(t, body, protoerrors.CodeIdentityRequired)
}

func TestMemoryHandler_ListRejectsCrossTenantWithoutScope(t *testing.T) {
	f := newMemHandlerFixture(t)
	// A filter naming a foreign tenant + NO admin scope → 403.
	body := `{"filter":{"tenant_ids":["t-other"]}}`
	status, respBody := doMemReq(t, f.handler.ListHandler(), memListPath, body, &memHandlerID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", status, respBody)
	}
	assertMemErrCode(t, respBody, protoerrors.CodeIdentityScopeRequired)
}

func TestMemoryHandler_ListCrossTenantWithAdminScopeAccepted(t *testing.T) {
	f := newMemHandlerFixture(t)
	// Filter names the caller's OWN tenant (so the foreign-tenant gate
	// does not trip) but the test proves the admin-scoped path runs:
	// with the admin scope a foreign-tenant filter is admitted past
	// the wire gate (List then scopes within the supplied identity).
	body := `{"filter":{"tenant_ids":["t-other"]}}`
	status, respBody := doMemReq(t, f.handler.ListHandler(), memListPath, body,
		&memHandlerID, []auth.Scope{auth.ScopeAdmin})
	// With admin scope the wire gate passes; List filters within the
	// caller's snapshot (no t-other rows) → 200 + empty page.
	if status != http.StatusOK {
		t.Fatalf("admin-scoped cross-tenant: status = %d, want 200; body=%s", status, respBody)
	}
}

func TestMemoryHandler_GetHappyPathLightValue(t *testing.T) {
	f := newMemHandlerFixture(t)
	f.seedTurn(t, memHandlerID, "q", "a")

	_, listBody := doMemReq(t, f.handler.ListHandler(), memListPath, `{}`, &memHandlerID, nil)
	var listResp prototypes.MemoryListResponse
	if err := json.Unmarshal(listBody, &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	key := listResp.Items[0].Key

	status, body := doMemReq(t, f.handler.GetHandler(), memGetPath,
		`{"key":"`+key+`"}`, &memHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var resp prototypes.MemoryGetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Detail.Value) == 0 {
		t.Error("Get(light value): Value empty, want inline bytes")
	}
	if resp.Detail.ValueArtifact != nil {
		t.Error("Get(light value): ValueArtifact populated, want nil (D-026)")
	}
}

func TestMemoryHandler_GetHeavyValueRoutesToArtifact(t *testing.T) {
	f := newMemHandlerFixture(t)
	f.seedTurn(t, memHandlerID, "heavy", strings.Repeat("X", memHandlerThreshold*2))

	_, listBody := doMemReq(t, f.handler.ListHandler(), memListPath, `{}`, &memHandlerID, nil)
	var listResp prototypes.MemoryListResponse
	if err := json.Unmarshal(listBody, &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	key := listResp.Items[0].Key

	status, body := doMemReq(t, f.handler.GetHandler(), memGetPath,
		`{"key":"`+key+`"}`, &memHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var resp prototypes.MemoryGetResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Detail.ValueArtifact == nil {
		t.Fatal("Get(heavy value): ValueArtifact nil, want by-reference stub (D-026)")
	}
	if len(resp.Detail.Value) != 0 {
		t.Error("Get(heavy value): Value populated, want empty (D-026)")
	}
}

func TestMemoryHandler_GetUnknownKeyIsNotFound(t *testing.T) {
	f := newMemHandlerFixture(t)
	f.seedTurn(t, memHandlerID, "q", "a")
	status, body := doMemReq(t, f.handler.GetHandler(), memGetPath,
		`{"key":"mem_does_not_exist"}`, &memHandlerID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", status, body)
	}
	assertMemErrCode(t, body, protoerrors.CodeNotFound)
}

func TestMemoryHandler_HealthHappyPath(t *testing.T) {
	f := newMemHandlerFixture(t)
	f.seedTurn(t, memHandlerID, "q1", "a1")
	f.seedTurn(t, memHandlerID, "q2", "a2")
	f.seedTurn(t, memHandlerID, "q3", "a3")

	status, body := doMemReq(t, f.handler.HealthHandler(), memHealthPath, `{}`, &memHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var resp prototypes.MemoryHealthResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Aggregate.Total != 3 {
		t.Errorf("Health Total = %d, want 3", resp.Aggregate.Total)
	}
	if resp.Aggregate.DriverByScope[string(prototypes.MemoryScopeSession)] != "inmem" {
		t.Errorf("DriverByScope[session] = %q, want inmem", resp.Aggregate.DriverByScope[string(prototypes.MemoryScopeSession)])
	}
}

func TestMemoryHandler_RejectsGET(t *testing.T) {
	f := newMemHandlerFixture(t)
	req := httptest.NewRequest(http.MethodGet, memListPath, nil)
	rec := httptest.NewRecorder()
	f.handler.ListHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET memory.list: status = %d, want 405", rec.Code)
	}
}

func TestMemoryHandler_BodyIdentityMismatchRejected(t *testing.T) {
	f := newMemHandlerFixture(t)
	// Body claims a tenant different from the verified carrier-header
	// identity → defence-in-depth rejection.
	body := `{"identity":{"tenant":"t-evil","user":"u-mem","session":"s-mem"}}`
	status, respBody := doMemReq(t, f.handler.ListHandler(), memListPath, body, &memHandlerID, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, respBody)
	}
	assertMemErrCode(t, respBody, protoerrors.CodeIdentityRequired)
}

func TestMemoryHandler_RejectsUnknownFilterEnum(t *testing.T) {
	f := newMemHandlerFixture(t)
	body := `{"filter":{"scopes":["galaxy"]}}`
	status, respBody := doMemReq(t, f.handler.ListHandler(), memListPath, body, &memHandlerID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, respBody)
	}
	assertMemErrCode(t, respBody, protoerrors.CodeInvalidRequest)
}

func TestMemoryHandler_GetRejectsMissingIdentity(t *testing.T) {
	f := newMemHandlerFixture(t)
	status, body := doMemReq(t, f.handler.GetHandler(), memGetPath, `{"key":"k"}`, nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("memory.get without identity: status = %d, want 401; body=%s", status, body)
	}
	assertMemErrCode(t, body, protoerrors.CodeIdentityRequired)
}

func TestMemoryHandler_HealthRejectsMissingIdentity(t *testing.T) {
	f := newMemHandlerFixture(t)
	status, body := doMemReq(t, f.handler.HealthHandler(), memHealthPath, `{}`, nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("memory.health without identity: status = %d, want 401; body=%s", status, body)
	}
	assertMemErrCode(t, body, protoerrors.CodeIdentityRequired)
}

func TestMemoryHandler_GetRejectsGET(t *testing.T) {
	f := newMemHandlerFixture(t)
	req := httptest.NewRequest(http.MethodGet, memGetPath, nil)
	rec := httptest.NewRecorder()
	f.handler.GetHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET memory.get: status = %d, want 405", rec.Code)
	}
}

func TestMemoryHandler_HealthRejectsGET(t *testing.T) {
	f := newMemHandlerFixture(t)
	req := httptest.NewRequest(http.MethodGet, memHealthPath, nil)
	rec := httptest.NewRecorder()
	f.handler.HealthHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET memory.health: status = %d, want 405", rec.Code)
	}
}

func TestMemoryHandler_GetBodyIdentityMismatchRejected(t *testing.T) {
	f := newMemHandlerFixture(t)
	body := `{"identity":{"tenant":"t-evil","user":"u-mem","session":"s-mem"},"key":"k"}`
	status, respBody := doMemReq(t, f.handler.GetHandler(), memGetPath, body, &memHandlerID, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("memory.get body mismatch: status = %d, want 401; body=%s", status, respBody)
	}
	assertMemErrCode(t, respBody, protoerrors.CodeIdentityRequired)
}

func TestMemoryHandler_HealthBodyIdentityMismatchRejected(t *testing.T) {
	f := newMemHandlerFixture(t)
	body := `{"identity":{"tenant":"t-evil","user":"u-mem","session":"s-mem"}}`
	status, respBody := doMemReq(t, f.handler.HealthHandler(), memHealthPath, body, &memHandlerID, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("memory.health body mismatch: status = %d, want 401; body=%s", status, respBody)
	}
	assertMemErrCode(t, respBody, protoerrors.CodeIdentityRequired)
}

func TestMemoryHandler_GetEmptyKeyIsInvalid(t *testing.T) {
	f := newMemHandlerFixture(t)
	status, body := doMemReq(t, f.handler.GetHandler(), memGetPath, `{"key":""}`, &memHandlerID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("memory.get empty key: status = %d, want 400; body=%s", status, body)
	}
	assertMemErrCode(t, body, protoerrors.CodeInvalidRequest)
}

func TestMemoryHandler_RejectsMalformedBody(t *testing.T) {
	f := newMemHandlerFixture(t)
	status, body := doMemReq(t, f.handler.ListHandler(), memListPath, `{not json`, &memHandlerID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("memory.list malformed body: status = %d, want 400; body=%s", status, body)
	}
	assertMemErrCode(t, body, protoerrors.CodeInvalidRequest)
}

func TestMemoryHandler_RejectsUnknownBodyField(t *testing.T) {
	f := newMemHandlerFixture(t)
	// DisallowUnknownFields → an unrecognised field is a 400.
	status, body := doMemReq(t, f.handler.HealthHandler(), memHealthPath,
		`{"bogus_field":true}`, &memHandlerID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("memory.health unknown field: status = %d, want 400; body=%s", status, body)
	}
}

func TestMemoryHandler_ListRejectsOversizedPageSize(t *testing.T) {
	f := newMemHandlerFixture(t)
	status, body := doMemReq(t, f.handler.ListHandler(), memListPath,
		`{"page_size":99999}`, &memHandlerID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("memory.list oversized page_size: status = %d, want 400; body=%s", status, body)
	}
	assertMemErrCode(t, body, protoerrors.CodeInvalidRequest)
}

func TestMemoryHandler_WithMemoryLoggerOption(t *testing.T) {
	// Exercises the WithMemoryLogger option path.
	f := newMemHandlerFixture(t)
	h, err := stream.NewMemoryHandler(f.store, mustArtStore(t), memHandlerThreshold,
		stream.WithMemoryLogger(nil), // nil logger → default
		stream.WithMemoryLogger(slogDiscard()))
	if err != nil {
		t.Fatalf("NewMemoryHandler with logger: %v", err)
	}
	if h == nil {
		t.Fatal("NewMemoryHandler returned nil")
	}
}

// mustArtStore opens a fresh in-mem ArtifactStore for a handler test.
func mustArtStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	s, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// slogDiscard returns a logger that discards every record.
func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// assertMemErrCode decodes a JSON error body and asserts its Code.
func assertMemErrCode(t *testing.T, body []byte, want protoerrors.Code) {
	t.Helper()
	var e protoerrors.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body: %v; body=%s", err, body)
	}
	if e.Code != want {
		t.Errorf("error Code = %q, want %q; body=%s", e.Code, want, body)
	}
}
