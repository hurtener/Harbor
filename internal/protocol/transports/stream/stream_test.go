package stream_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
)

// newTestBus builds a real in-mem events.EventBus — no mock at the seam
// (CLAUDE.md §17.3). Replay is enabled so the reconnect-cursor path is
// exercisable.
func newTestBus(t *testing.T) events.EventBus {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     128,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func publishCancelled(t *testing.T, bus events.EventBus, id identity.Identity, runID string) {
	t.Helper()
	err := bus.Publish(context.Background(), events.Event{
		Type:     events.EventTypeRuntimeRunCancelled,
		Identity: identity.Quadruple{Identity: id, RunID: runID},
		Payload:  events.RunCancelledPayload{RunID: runID, CancelledAt: time.Now().UnixNano()},
	})
	if err != nil {
		t.Fatalf("bus.Publish: %v", err)
	}
}

func newStreamServer(t *testing.T, bus events.EventBus, opts ...stream.Option) *httptest.Server {
	t.Helper()
	h, err := stream.NewHandler(bus, opts...)
	if err != nil {
		t.Fatalf("stream.NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(stream.RoutePattern, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestNewHandler_NilBus_FailsLoud(t *testing.T) {
	if _, err := stream.NewHandler(nil); err == nil {
		t.Fatal("NewHandler(nil) returned nil error; want ErrMisconfigured")
	}
}

// TestServeHTTP_MissingIdentity_FailsClosed401 — identity at the edge: a
// stream request with an incomplete triple is rejected closed before any
// subscription is opened (RFC §5.5, CLAUDE.md §6 rule 9).
func TestServeHTTP_MissingIdentity_FailsClosed401(t *testing.T) {
	bus := newTestBus(t)
	srv := newStreamServer(t, bus)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set(stream.HeaderTenant, "t1")
	// User + Session deliberately omitted.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestServeHTTP_NonGET_Rejected — the SSE transport accepts GET only.
func TestServeHTTP_NonGET_Rejected(t *testing.T) {
	bus := newTestBus(t)
	srv := newStreamServer(t, bus)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/events", nil)
	req.Header.Set(stream.HeaderTenant, "t1")
	req.Header.Set(stream.HeaderUser, "u1")
	req.Header.Set(stream.HeaderSession, "s1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("POST on event stream returned 200; want a rejection")
	}
}

// TestServeHTTP_StreamsLiveEvent — a connected client receives an event
// published after it subscribed, framed as an SSE block.
func TestServeHTTP_StreamsLiveEvent(t *testing.T) {
	bus := newTestBus(t)
	srv := newStreamServer(t, bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	req.Header.Set(stream.HeaderTenant, id.TenantID)
	req.Header.Set(stream.HeaderUser, id.UserID)
	req.Header.Set(stream.HeaderSession, id.SessionID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// Give the subscription a beat to register, then publish.
	deadline := time.Now().Add(2 * time.Second)
	publishCancelled(t, bus, id, "r1")

	sc := bufio.NewScanner(resp.Body)
	gotEvent := false
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if strings.HasPrefix(line, "event: runtime.run_cancelled") {
			gotEvent = true
			break
		}
	}
	if !gotEvent {
		// Retry once: the subscription may have registered just after
		// the first publish. Publish again and re-scan briefly.
		publishCancelled(t, bus, id, "r2")
		for sc.Scan() && time.Now().Before(deadline) {
			if strings.HasPrefix(sc.Text(), "event: runtime.run_cancelled") {
				gotEvent = true
				break
			}
		}
	}
	if !gotEvent {
		t.Fatal("did not receive the published event as an SSE frame")
	}
}

// TestServeHTTP_TripleScoped_NoCrossTalk — an event for a different
// session is NOT delivered to a triple-scoped stream (multi-isolation at
// the wire edge).
func TestServeHTTP_TripleScoped_NoCrossTalk(t *testing.T) {
	bus := newTestBus(t)
	srv := newStreamServer(t, bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set(stream.HeaderTenant, "t1")
	req.Header.Set(stream.HeaderUser, "u1")
	req.Header.Set(stream.HeaderSession, "s1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Publish for a DIFFERENT session — must not reach this stream.
	other := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s2"}
	publishCancelled(t, bus, other, "r-other")
	// And one for our own session as a positive control.
	mine := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	publishCancelled(t, bus, mine, "r-mine")

	deadline := time.Now().Add(2 * time.Second)
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if strings.Contains(data, "r-other") {
				t.Fatal("cross-session event leaked onto a triple-scoped stream")
			}
			if strings.Contains(data, "r-mine") {
				return // positive control received, no leak — pass.
			}
		}
	}
}

// TestServeHTTP_Keepalive — with a short keepalive interval the stream
// emits keepalive comment frames on an idle connection.
func TestServeHTTP_Keepalive(t *testing.T) {
	bus := newTestBus(t)
	srv := newStreamServer(t, bus, stream.WithKeepalive(20*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set(stream.HeaderTenant, "t1")
	req.Header.Set(stream.HeaderUser, "u1")
	req.Header.Set(stream.HeaderSession, "s1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	deadline := time.Now().Add(2 * time.Second)
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() && time.Now().Before(deadline) {
		if strings.HasPrefix(sc.Text(), ": keepalive") {
			return // keepalive frame observed — pass.
		}
	}
	t.Fatal("did not observe a keepalive comment frame on an idle stream")
}

// TestServeHTTP_Reconnect_ReplaysFromCursor — a client that reconnects
// with Last-Event-ID gets the events strictly newer than that cursor
// replayed before the live tail (events.Replayer path).
func TestServeHTTP_Reconnect_ReplaysFromCursor(t *testing.T) {
	bus := newTestBus(t)
	srv := newStreamServer(t, bus)

	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	// Publish three events BEFORE any connection — they land in the
	// replay ring.
	publishCancelled(t, bus, id, "r1")
	publishCancelled(t, bus, id, "r2")
	publishCancelled(t, bus, id, "r3")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set(stream.HeaderTenant, id.TenantID)
	req.Header.Set(stream.HeaderUser, id.UserID)
	req.Header.Set(stream.HeaderSession, id.SessionID)
	// Reconnect from sequence 1 — expect events at sequence 2 and 3 to
	// be replayed.
	req.Header.Set("Last-Event-ID", "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	deadline := time.Now().Add(2 * time.Second)
	sc := bufio.NewScanner(resp.Body)
	seenSeqs := map[string]bool{}
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			seenSeqs[strings.TrimPrefix(line, "id: ")] = true
		}
		if seenSeqs["2"] && seenSeqs["3"] {
			break
		}
	}
	if !seenSeqs["2"] || !seenSeqs["3"] {
		t.Fatalf("reconnect replay missing events: seen=%v, want seq 2 and 3", seenSeqs)
	}
	if seenSeqs["1"] {
		t.Error("reconnect replayed event at-or-before the cursor (seq 1)")
	}
}

// newReplayOffBus is an in-mem bus with ReplayBufferSize 0 — replay is
// configured off, so Replay returns ErrReplayUnavailable.
func newReplayOffBus(t *testing.T) events.EventBus {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     128,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         0,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// TestServeHTTP_Reconnect_ReplayUnavailable_SurfacesGap — a reconnecting
// client whose bus has replay configured off gets the gap SURFACED via
// an explicit comment frame, never a silent stream (CLAUDE.md §5).
func TestServeHTTP_Reconnect_ReplayUnavailable_SurfacesGap(t *testing.T) {
	bus := newReplayOffBus(t)
	srv := newStreamServer(t, bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set(stream.HeaderTenant, "t1")
	req.Header.Set(stream.HeaderUser, "u1")
	req.Header.Set(stream.HeaderSession, "s1")
	req.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	deadline := time.Now().Add(2 * time.Second)
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() && time.Now().Before(deadline) {
		if strings.HasPrefix(sc.Text(), ": stream.replay_unavailable") {
			return // gap surfaced — pass.
		}
	}
	t.Fatal("reconnect with replay-off bus did not surface the gap on the wire")
}

// TestServeHTTP_EventTypeFilter — the X-Harbor-Event-Type header narrows
// the subscription; an event of a non-selected type is not delivered.
func TestServeHTTP_EventTypeFilter(t *testing.T) {
	bus := newTestBus(t)
	srv := newStreamServer(t, bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	req.Header.Set(stream.HeaderTenant, id.TenantID)
	req.Header.Set(stream.HeaderUser, id.UserID)
	req.Header.Set(stream.HeaderSession, id.SessionID)
	// Select ONLY task.spawned — runtime.run_cancelled must not arrive.
	req.Header.Set(stream.HeaderEventType, "task.spawned")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	publishCancelled(t, bus, id, "r1") // runtime.run_cancelled — filtered out.

	deadline := time.Now().Add(800 * time.Millisecond)
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() && time.Now().Before(deadline) {
		if strings.HasPrefix(sc.Text(), "event: runtime.run_cancelled") {
			t.Fatal("event-type filter leaked a non-selected event type")
		}
	}
	// No runtime.run_cancelled observed within the window — the filter
	// held. (A keepalive comment may appear; that is fine.)
}
