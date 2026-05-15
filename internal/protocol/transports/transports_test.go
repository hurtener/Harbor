package transports_test

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

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// testDeps holds the real drivers behind the mux — no mocks at the seam
// (CLAUDE.md §17.3).
type testDeps struct {
	surface *protocol.ControlSurface
	bus     events.EventBus
	cleanup func()
}

func newTestDeps(t *testing.T) *testDeps {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
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
	return &testDeps{
		surface: surface,
		bus:     bus,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

func TestNewMux_NilDeps_FailLoud(t *testing.T) {
	if _, err := transports.NewMux(nil, nil); err == nil {
		t.Fatal("NewMux(nil,nil) returned nil error; want ErrMisconfigured")
	}
	deps := newTestDeps(t)
	defer deps.cleanup()
	if _, err := transports.NewMux(deps.surface, nil); err == nil {
		t.Error("NewMux(surface,nil) returned nil error; want ErrMisconfigured")
	}
	if _, err := transports.NewMux(nil, deps.bus); err == nil {
		t.Error("NewMux(nil,bus) returned nil error; want ErrMisconfigured")
	}
}

// TestNewMux_RoutesBothTransports — the composed mux serves the REST
// control route AND the SSE event route.
func TestNewMux_RoutesBothTransports(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()

	mux, err := transports.NewMux(deps.surface, deps.bus)
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// REST control route.
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"q"}`
	resp, err := http.Post(srv.URL+"/v1/control/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/control/start: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("control route status = %d, want 200", resp.StatusCode)
	}
	var sr types.StartResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode StartResponse: %v", err)
	}
	if sr.TaskID == "" {
		t.Error("control route returned empty TaskID")
	}

	// SSE event route — open + immediately confirm the stream headers,
	// then close.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set("X-Harbor-Tenant", "t1")
	req.Header.Set("X-Harbor-User", "u1")
	req.Header.Set("X-Harbor-Session", "s1")
	sresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/events: %v", err)
	}
	defer func() { _ = sresp.Body.Close() }()
	if sresp.StatusCode != http.StatusOK {
		t.Fatalf("stream route status = %d, want 200", sresp.StatusCode)
	}
	if ct := sresp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("stream route Content-Type = %q, want text/event-stream", ct)
	}
}

// TestNewMux_Options — WithLogger + WithKeepalive thread through; a nil
// logger / non-positive keepalive is ignored.
func TestNewMux_Options(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux, err := transports.NewMux(deps.surface, deps.bus,
		transports.WithLogger(logger),
		transports.WithLogger(nil),  // ignored
		transports.WithKeepalive(0), // ignored
		transports.WithKeepalive(time.Second),
	)
	if err != nil {
		t.Fatalf("NewMux with options: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// The mux still serves the control route with the options applied.
	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"q"}`
	resp, err := http.Post(srv.URL+"/v1/control/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestNewMux_UnknownRoute_404 — a path the mux does not route 404s.
func TestNewMux_UnknownRoute_404(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()
	mux, err := transports.NewMux(deps.surface, deps.bus)
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/nonexistent")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown route status = %d, want 404", resp.StatusCode)
	}
}
