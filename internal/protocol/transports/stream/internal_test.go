package stream

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// idQuad builds an identity.Quadruple for test fixtures.
func idQuad(tenant, user, session, run string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: session},
		RunID:    run,
	}
}

func TestParseEventTypes(t *testing.T) {
	mk := func(vals ...string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
		for _, v := range vals {
			r.Header.Add(HeaderEventType, v)
		}
		return r
	}
	if got := parseEventTypes(mk()); got != nil {
		t.Errorf("no headers => %v, want nil", got)
	}
	got := parseEventTypes(mk("task.spawned", "", "task.started"))
	if len(got) != 2 || got[0] != "task.spawned" || got[1] != "task.started" {
		t.Errorf("parseEventTypes = %v, want [task.spawned task.started] (empty skipped)", got)
	}
}

func TestParseLastEventID(t *testing.T) {
	mk := func(v string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
		if v != "" {
			r.Header.Set("Last-Event-ID", v)
		}
		return r
	}
	if _, ok := parseLastEventID(mk(""), "s1"); ok {
		t.Error("missing Last-Event-ID => ok=true, want false")
	}
	if _, ok := parseLastEventID(mk("not-a-number"), "s1"); ok {
		t.Error("malformed Last-Event-ID => ok=true, want false")
	}
	cur, ok := parseLastEventID(mk("7"), "s1")
	if !ok || cur.Sequence != 7 || cur.SessionID != "s1" {
		t.Errorf("parseLastEventID(7,s1) = (%+v,%v), want ({s1 7},true)", cur, ok)
	}
}

func TestWithKeepalive_ClampAndIgnore(t *testing.T) {
	h := &Handler{keepalive: defaultKeepalive}
	WithKeepalive(0)(h)
	if h.keepalive != defaultKeepalive {
		t.Errorf("WithKeepalive(0) changed keepalive to %v, want default kept", h.keepalive)
	}
	WithKeepalive(time.Nanosecond)(h)
	if h.keepalive != minKeepalive {
		t.Errorf("WithKeepalive(1ns) = %v, want clamped to minKeepalive %v", h.keepalive, minKeepalive)
	}
	WithKeepalive(5 * time.Second)(h)
	if h.keepalive != 5*time.Second {
		t.Errorf("WithKeepalive(5s) = %v, want 5s", h.keepalive)
	}
}

func TestWithLogger_NilIgnored(t *testing.T) {
	h := &Handler{logger: slog.Default()}
	WithLogger(nil)(h)
	if h.logger == nil {
		t.Error("WithLogger(nil) nilled the logger; want default kept")
	}
	custom := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	WithLogger(custom)(h)
	if h.logger != custom {
		t.Error("WithLogger(custom) did not set the logger")
	}
}

// flushRecorder is an httptest.ResponseRecorder that also satisfies
// http.Flusher — used to drive surfaceReplayGap / replayFromCursor
// directly without a full server.
type flushRecorder struct{ *httptest.ResponseRecorder }

func (flushRecorder) Flush() {}

func newFlushRecorder() flushRecorder {
	return flushRecorder{httptest.NewRecorder()}
}

func TestSurfaceReplayGap_WritesCommentFrame(t *testing.T) {
	h := &Handler{logger: slog.Default()}
	rec := newFlushRecorder()
	h.surfaceReplayGap(context.Background(), rec, rec, "test reason", h.logger)
	body := rec.Body.String()
	if want := ": stream.replay_unavailable test reason\n\n"; body != want {
		t.Errorf("surfaceReplayGap body = %q, want %q", body, want)
	}
}

// nonReplayerBus is an events.EventBus that does NOT implement
// events.Replayer — used to exercise replayFromCursor's "driver does not
// support replay" branch.
type nonReplayerBus struct{}

func (nonReplayerBus) Publish(context.Context, events.Event) error { return nil }
func (nonReplayerBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, events.ErrBusClosed
}
func (nonReplayerBus) Close(context.Context) error { return nil }

func TestReplayFromCursor_NoReplayerSupport_SurfacesGap(t *testing.T) {
	h := &Handler{bus: nonReplayerBus{}, logger: slog.Default()}
	rec := newFlushRecorder()
	h.replayFromCursor(context.Background(), rec, rec,
		events.Filter{Tenant: "t1", User: "u1", Session: "s1"},
		events.Cursor{SessionID: "s1", Sequence: 1}, h.logger)
	if got := rec.Body.String(); got == "" ||
		!bytes.Contains([]byte(got), []byte("stream.replay_unavailable")) {
		t.Errorf("replayFromCursor with non-Replayer bus did not surface the gap: %q", got)
	}
}

// replayerBus is an events.EventBus + events.Replayer whose Replay
// returns a caller-set snapshot / error — exercises every
// replayFromCursor branch directly.
type replayerBus struct {
	snapshot []events.Event
	err      error
}

func (replayerBus) Publish(context.Context, events.Event) error { return nil }
func (replayerBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, events.ErrBusClosed
}
func (replayerBus) Close(context.Context) error { return nil }
func (b replayerBus) Replay(context.Context, events.Cursor, events.Filter) ([]events.Event, error) {
	return b.snapshot, b.err
}

func TestReplayFromCursor_Branches(t *testing.T) {
	filter := events.Filter{Tenant: "t1", User: "u1", Session: "s1"}
	cursor := events.Cursor{SessionID: "s1", Sequence: 1}

	cases := []struct {
		name    string
		bus     events.EventBus
		wantGap string // substring expected in the surfaced comment frame; "" => no gap, frames written
	}{
		{"replay_unavailable", replayerBus{err: events.ErrReplayUnavailable}, "stream.replay_unavailable"},
		{"cursor_too_old", replayerBus{err: events.ErrCursorTooOld}, "stream.replay_unavailable"},
		{"generic_error", replayerBus{err: context.DeadlineExceeded}, "stream.replay_unavailable"},
		{"success_snapshot", replayerBus{snapshot: []events.Event{{
			Type:     events.EventTypeRuntimeRunCancelled,
			Sequence: 2,
			Identity: idQuad("t1", "u1", "s1", "r2"),
			Payload:  events.RunCancelledPayload{RunID: "r2"},
		}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{bus: tc.bus, logger: slog.Default()}
			rec := newFlushRecorder()
			h.replayFromCursor(context.Background(), rec, rec, filter, cursor, h.logger)
			body := rec.Body.String()
			if tc.wantGap != "" {
				if !bytes.Contains([]byte(body), []byte(tc.wantGap)) {
					t.Errorf("%s: body %q missing %q", tc.name, body, tc.wantGap)
				}
				return
			}
			// success: a replayed event frame must be on the wire.
			if !bytes.Contains([]byte(body), []byte("event: runtime.run_cancelled")) {
				t.Errorf("%s: replayed snapshot not framed onto the wire: %q", tc.name, body)
			}
			if !bytes.Contains([]byte(body), []byte("id: 2")) {
				t.Errorf("%s: replayed frame missing id: 2: %q", tc.name, body)
			}
		})
	}
}

// nonFlushRecorder is an http.ResponseWriter that does NOT implement
// http.Flusher — used to exercise ServeHTTP's "response writer does not
// support streaming" guard.
type nonFlushRecorder struct {
	header http.Header
	status int
}

func (n *nonFlushRecorder) Header() http.Header {
	if n.header == nil {
		n.header = http.Header{}
	}
	return n.header
}
func (n *nonFlushRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (n *nonFlushRecorder) WriteHeader(s int)           { n.status = s }

func TestServeHTTP_NonFlushWriter_FailsClosed(t *testing.T) {
	h := &Handler{bus: nonReplayerBus{}, logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set(HeaderTenant, "t1")
	req.Header.Set(HeaderUser, "u1")
	req.Header.Set(HeaderSession, "s1")
	rec := &nonFlushRecorder{}
	h.ServeHTTP(rec, req)
	if rec.status != http.StatusInternalServerError {
		t.Errorf("non-flush writer status = %d, want 500", rec.status)
	}
}

// closedBus is an events.EventBus whose Subscribe always reports the bus
// is closed — exercises ServeHTTP's Subscribe-error -> 503 branch.
type closedBus struct{}

func (closedBus) Publish(context.Context, events.Event) error { return events.ErrBusClosed }
func (closedBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, events.ErrBusClosed
}
func (closedBus) Close(context.Context) error { return nil }

func TestServeHTTP_SubscribeClosedBus_503(t *testing.T) {
	h := &Handler{bus: closedBus{}, logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set(HeaderTenant, "t1")
	req.Header.Set(HeaderUser, "u1")
	req.Header.Set(HeaderSession, "s1")
	rec := newFlushRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("closed-bus Subscribe status = %d, want 503", rec.Code)
	}
}

// scopeRequiredBus reports ErrIdentityScopeRequired from Subscribe —
// exercises ServeHTTP's Subscribe-error -> 401 branch.
type scopeRequiredBus struct{}

func (scopeRequiredBus) Publish(context.Context, events.Event) error { return nil }
func (scopeRequiredBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, events.ErrIdentityScopeRequired
}
func (scopeRequiredBus) Close(context.Context) error { return nil }

func TestServeHTTP_SubscribeScopeRequired_401(t *testing.T) {
	h := &Handler{bus: scopeRequiredBus{}, logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set(HeaderTenant, "t1")
	req.Header.Set(HeaderUser, "u1")
	req.Header.Set(HeaderSession, "s1")
	rec := newFlushRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("scope-required Subscribe status = %d, want 401", rec.Code)
	}
}

// stubSub is an events.Subscription with a caller-driven channel — used
// to drive streamLoop's branches directly.
type stubSub struct {
	ch        chan events.Event
	cancelled bool
}

func (s *stubSub) Events() <-chan events.Event { return s.ch }
func (s *stubSub) Cancel()                     { s.cancelled = true }

// TestStreamLoop_ChannelClosed_ReturnsClean — streamLoop returns when the
// subscription channel closes (the bus-closed / idle-reap path).
func TestStreamLoop_ChannelClosed_ReturnsClean(t *testing.T) {
	h := &Handler{bus: nonReplayerBus{}, logger: slog.Default(), keepalive: time.Hour}
	sub := &stubSub{ch: make(chan events.Event)}
	rec := newFlushRecorder()
	close(sub.ch) // channel closed before the loop starts.
	done := make(chan struct{})
	go func() {
		h.streamLoop(context.Background(), rec, rec, sub, h.logger)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streamLoop did not return after the subscription channel closed")
	}
}

// TestStreamLoop_CtxCancelled_ReturnsClean — streamLoop returns when the
// request ctx is cancelled (client disconnect).
func TestStreamLoop_CtxCancelled_ReturnsClean(t *testing.T) {
	h := &Handler{bus: nonReplayerBus{}, logger: slog.Default(), keepalive: time.Hour}
	sub := &stubSub{ch: make(chan events.Event)}
	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.streamLoop(ctx, rec, rec, sub, h.logger)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streamLoop did not return after ctx cancellation")
	}
}

// TestStreamLoop_ForwardsEventAndKeepalive — streamLoop frames a
// delivered event and emits a keepalive comment, then returns on ctx
// cancel.
func TestStreamLoop_ForwardsEventAndKeepalive(t *testing.T) {
	h := &Handler{bus: nonReplayerBus{}, logger: slog.Default(), keepalive: 5 * time.Millisecond}
	sub := &stubSub{ch: make(chan events.Event, 1)}
	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(context.Background())

	sub.ch <- events.Event{
		Type:     events.EventTypeRuntimeRunCancelled,
		Sequence: 1,
		Identity: idQuad("t1", "u1", "s1", "r1"),
		Payload:  events.RunCancelledPayload{RunID: "r1"},
	}
	done := make(chan struct{})
	go func() {
		h.streamLoop(ctx, rec, rec, sub, h.logger)
		close(done)
	}()
	time.Sleep(40 * time.Millisecond) // let the event + a keepalive flush.
	cancel()
	<-done

	body := rec.Body.String()
	if !bytes.Contains([]byte(body), []byte("event: runtime.run_cancelled")) {
		t.Errorf("streamLoop did not frame the delivered event:\n%s", body)
	}
	if !bytes.Contains([]byte(body), []byte(": keepalive")) {
		t.Errorf("streamLoop did not emit a keepalive comment:\n%s", body)
	}
}
