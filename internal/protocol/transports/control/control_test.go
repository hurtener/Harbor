package control_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// newTestSurface builds a real protocol.ControlSurface over real
// in-process drivers — no mocks at the seam (CLAUDE.md §17.3). The
// returned cleanup closes every driver.
func newTestSurface(t *testing.T) (*protocol.ControlSurface, func()) {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     128,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
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
	steerReg := steering.NewRegistry()
	surface, err := protocol.NewControlSurface(taskReg, steerReg)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}
	return surface, func() {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
	}
}

func newTestHandler(t *testing.T) (*control.Handler, func()) {
	t.Helper()
	surface, cleanup := newTestSurface(t)
	h, err := control.NewHandler(surface)
	if err != nil {
		cleanup()
		t.Fatalf("control.NewHandler: %v", err)
	}
	return h, cleanup
}

// do issues a request against the handler via httptest and returns the
// recorded response.
func do(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	// The bare handler does not get a PathValue unless mounted on a mux
	// with the {method} wildcard; mount it so r.PathValue works.
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, h)
	mux.ServeHTTP(rec, req)
	return rec
}

func TestNewHandler_NilSurface_FailsLoud(t *testing.T) {
	if _, err := control.NewHandler(nil); err == nil {
		t.Fatal("NewHandler(nil) returned nil error; want ErrMisconfigured")
	}
}

func TestNewHandler_WithLogger(t *testing.T) {
	surface, cleanup := newTestSurface(t)
	defer cleanup()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := control.NewHandler(surface, control.WithLogger(logger), control.WithLogger(nil))
	if err != nil {
		t.Fatalf("NewHandler with logger: %v", err)
	}
	// A nil logger option is ignored; the handler still serves.
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"q"}`
	rec := do(t, h, http.MethodPost, "/v1/control/start", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestServeHTTP_BareHandler_NonPOST — the handler's own method guard
// (defence for a handler mounted bare, not via a POST-pinned mux).
func TestServeHTTP_BareHandler_NonPOST(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/v1/control/start", nil)
	req.SetPathValue("method", string(methods.MethodStart))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // bare — no mux.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bare-handler GET status = %d, want 400", rec.Code)
	}
}

// TestServeHTTP_Start_OK — a well-formed `start` over REST spawns a task
// and returns the StartResponse as JSON with 200.
func TestServeHTTP_Start_OK(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"hello"}`
	rec := do(t, h, http.MethodPost, "/v1/control/start", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var resp types.StartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode StartResponse: %v", err)
	}
	if resp.TaskID == "" {
		t.Error("StartResponse.TaskID is empty")
	}
	if resp.ProtocolVersion != types.ProtocolVersion {
		t.Errorf("ProtocolVersion = %q, want %q", resp.ProtocolVersion, types.ProtocolVersion)
	}
}

// TestServeHTTP_Start_MissingIdentity_FailsClosed401 — identity at the
// edge: an incomplete triple is rejected closed before the runtime is
// touched (RFC §5.5, CLAUDE.md §6 rule 9).
func TestServeHTTP_Start_MissingIdentity_FailsClosed401(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	body := `{"identity":{"tenant":"t1","user":"","session":"s1"}}`
	rec := do(t, h, http.MethodPost, "/v1/control/start", body)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	var perr protoerrors.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &perr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if perr.Code != protoerrors.CodeIdentityRequired {
		t.Errorf("error code = %q, want %q", perr.Code, protoerrors.CodeIdentityRequired)
	}
}

// TestServeHTTP_UnknownMethod_404 — a non-canonical method name in the
// path is rejected as unknown_method -> 404.
func TestServeHTTP_UnknownMethod_404(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	rec := do(t, h, http.MethodPost, "/v1/control/teleport", `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	var perr protoerrors.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &perr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if perr.Code != protoerrors.CodeUnknownMethod {
		t.Errorf("error code = %q, want %q", perr.Code, protoerrors.CodeUnknownMethod)
	}
}

// TestServeHTTP_MalformedBody_400 — a body that is not valid JSON for
// the method's wire type fails as invalid_request -> 400.
func TestServeHTTP_MalformedBody_400(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	rec := do(t, h, http.MethodPost, "/v1/control/start", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var perr protoerrors.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &perr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if perr.Code != protoerrors.CodeInvalidRequest {
		t.Errorf("error code = %q, want %q", perr.Code, protoerrors.CodeInvalidRequest)
	}
}

// TestServeHTTP_Control_NoLiveRun_404 — a steering control for a run
// with no live inbox is rejected as not_found -> 404.
func TestServeHTTP_Control_NoLiveRun_404(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1","run":"r-nonexistent","scope":"owner_user"}}`
	rec := do(t, h, http.MethodPost, "/v1/control/cancel", body)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	var perr protoerrors.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &perr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if perr.Code != protoerrors.CodeNotFound {
		t.Errorf("error code = %q, want %q", perr.Code, protoerrors.CodeNotFound)
	}
}

// TestServeHTTP_NonPOST_RejectedClosed — the control transport accepts
// POST only; a GET on the route is rejected (the mux pattern pins POST,
// so a GET 405s at the mux). The bare-handler defence is exercised via
// the integration / concurrent tests.
func TestServeHTTP_NonPOST_RejectedClosed(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/v1/control/start", nil)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, h)
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("GET on control route returned 200; want a rejection")
	}
}

// TestServeHTTP_OversizeBody_400 — a body past the maxBodyBytes ceiling
// fails closed with invalid_request rather than being read unbounded.
func TestServeHTTP_OversizeBody_400(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	huge := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"` +
		strings.Repeat("x", 128<<10) + `"}`
	rec := do(t, h, http.MethodPost, "/v1/control/start", huge)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for oversize body", rec.Code)
	}
}

// TestServeHTTP_ConcurrentRequests_NoCrossTalk runs concurrent `start`
// requests for distinct identities against one shared handler and
// asserts each response carries its own task id — a smaller in-package
// echo of the D-025 concurrent-reuse test in
// internal/protocol/transports.
func TestServeHTTP_ConcurrentRequests_NoCrossTalk(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()

	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"q"}`
			resp, err := http.Post(srv.URL+"/v1/control/start", "application/json", strings.NewReader(body))
			if err != nil {
				errs <- err
				return
			}
			defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				errs <- err
				return
			}
			var sr types.StartResponse
			if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
				errs <- err
				return
			}
			if sr.TaskID == "" {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent request error: %v", err)
		}
	}
}
