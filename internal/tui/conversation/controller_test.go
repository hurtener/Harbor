package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

type controllerFixture struct {
	t                   *testing.T
	mu                  sync.Mutex
	streams             map[string][]chan types.StateEvent
	streamRegistrations map[string]int
	streamSignals       map[string]chan struct{}
	closed              map[string]int
	requests            map[string]int
	disconnect          map[string]chan struct{}
	order               []string
	reject              string
	failPath            map[string]int
	capabilities        []types.Capability
}

func newControllerFixture(t *testing.T) (*controllerFixture, *httptest.Server) {
	f := &controllerFixture{
		t:                   t,
		streams:             map[string][]chan types.StateEvent{},
		streamRegistrations: map[string]int{},
		streamSignals:       map[string]chan struct{}{},
		closed:              map[string]int{},
		requests:            map[string]int{},
		disconnect:          map[string]chan struct{}{},
		failPath:            map[string]int{},
	}
	return f, httptest.NewServer(http.HandlerFunc(f.serve))
}
func (f *controllerFixture) serve(w http.ResponseWriter, r *http.Request) {
	session := r.Header.Get("X-Harbor-Session")
	f.mu.Lock()
	f.requests[r.URL.Path]++
	f.order = append(f.order, r.URL.Path)
	reject := f.reject
	failStatus := f.failPath[r.URL.Path]
	f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		http.Error(w, "missing auth", http.StatusUnauthorized)
		return
	}
	if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == reject {
		http.Error(w, "credential rejected", http.StatusUnauthorized)
		return
	}
	if failStatus != 0 {
		http.Error(w, "forced protocol failure", failStatus)
		return
	}
	write := func(value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	switch r.URL.Path {
	case "/v1/events":
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		f.mu.Lock()
		ch := make(chan types.StateEvent, 8)
		f.streams[session] = append(f.streams[session], ch)
		f.streamRegistrations[session]++
		if signal := f.streamSignals[session]; signal != nil {
			close(signal)
			f.streamSignals[session] = make(chan struct{})
		}
		disconnect := make(chan struct{})
		f.disconnect[session] = disconnect
		f.mu.Unlock()
		for {
			select {
			case <-r.Context().Done():
				f.mu.Lock()
				f.closed[session]++
				f.mu.Unlock()
				return
			case <-disconnect:
				return
			case event := <-ch:
				body, _ := json.Marshal(event)
				_, _ = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Sequence, body)
				w.(http.Flusher).Flush()
			}
		}
	case "/v1/control/runtime.info":
		capabilities := f.capabilities
		if capabilities == nil {
			capabilities = []types.Capability{types.CapTaskControl, types.CapEventsSubscribe, types.CapStateSnapshots, types.CapSessionLifecycle}
		}
		write(types.RuntimeInfo{InstanceID: "fixture", BuildVersion: "test", BuildCommit: "test", BuildGoVersion: "go", ProtocolVersion: types.ProtocolVersion, WireSurfaceDigest: "digest", Capabilities: capabilities})
	case "/v1/control/runtime.health":
		write(types.RuntimeHealth{})
	case "/v1/state/history":
		write(types.StateHistoryResponse{Events: []types.StateEvent{}})
	case "/v1/tasks/list":
		write(types.TaskListResponse{Rows: []types.TaskRow{}})
	case "/v1/sessions/inspect":
		write(types.SessionsInspectResponse{Row: types.SessionRow{SessionID: session, Status: types.SessionStatusRunning, Identity: types.IdentityScope{Tenant: "t", User: "u", Session: session}}})
	case "/v1/sessions/list":
		write(types.SessionsListResponse{Rows: []types.SessionRow{{SessionID: session, Status: types.SessionStatusRunning}}})
	case "/v1/pause/list":
		write(types.PauseListResponse{Snapshots: []types.PauseSnapshot{}, Page: 1, PageCount: 1})
	case "/v1/control/start":
		write(types.StartResponse{TaskID: "task-1", ProtocolVersion: types.ProtocolVersion})
	case "/v1/sessions/set_title":
		write(types.SessionsSetTitleResponse{SessionID: session, Title: "renamed", TitleSource: "manual"})
	case "/v1/sessions/delete":
		write(types.SessionsDeleteResponse{SessionID: session, Deleted: true})
	case "/v1/control/artifacts.put":
		write(types.ArtifactsPutResponse{Ref: types.ArtifactRef{ID: "uploaded", Filename: "note.txt", MimeType: "text/plain"}})
	default:
		http.NotFound(w, r)
	}
}
func (f *controllerFixture) emit(session string, event types.StateEvent) {
	f.mu.Lock()
	streams := append([]chan types.StateEvent(nil), f.streams[session]...)
	f.mu.Unlock()
	if len(streams) == 0 {
		f.t.Fatal("stream unavailable")
	}
	for _, ch := range streams {
		select {
		case ch <- event:
		default:
		}
	}
}

// awaitStreamRegistration waits for the fixture handler to publish the exact
// SSE stream registration. Subscribe returns after the response headers are
// flushed, which is intentionally before the handler reaches that publish.
func (f *controllerFixture) awaitStreamRegistration(ctx context.Context, session string, want int) {
	f.t.Helper()
	for {
		f.mu.Lock()
		if f.streamRegistrations[session] >= want {
			f.mu.Unlock()
			return
		}
		signal := f.streamSignals[session]
		if signal == nil {
			signal = make(chan struct{})
			f.streamSignals[session] = signal
		}
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			f.t.Fatalf("stream registration for session %q did not reach %d", session, want)
		case <-signal:
		}
	}
}

func (f *controllerFixture) drop(session string) {
	f.mu.Lock()
	ch := f.disconnect[session]
	f.mu.Unlock()
	close(ch)
}

func TestController_AttachStartEventSwitchRenameDeleteAndClose(t *testing.T) {
	f, server := newControllerFixture(t)
	defer server.Close()
	now := time.Now()
	one := types.IdentityScope{Tenant: "t", User: "u", Session: "one"}
	two := one
	two.Session = "two"
	tokens := NewTokenSource("", testToken(t, one, now.Add(time.Hour)))
	tokens.now = func() time.Time { return now }
	if err := tokens.Replace(two, testToken(t, two, now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	updates := make(chan Update, 32)
	controller, err := NewController(server.URL, tokens, one, func(update Update) { updates <- update })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err = controller.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	if controller.Identity().Session != "one" {
		t.Fatal("wrong identity")
	}
	if _, err = controller.Start(ctx, "hello", []string{"artifact"}, map[string]string{"artifact": "ref"}); err != nil {
		t.Fatal(err)
	}
	f.emit("one", types.StateEvent{Type: "task.started", Sequence: 1, OccurredAt: now, Tenant: "t", User: "u", Session: "one", Payload: map[string]any{"TaskID": "run-one"}})
	awaitBlock(t, ctx, updates, "run-one")
	if _, err = controller.Rename(ctx, "renamed"); err != nil {
		t.Fatal(err)
	}
	if rows, listErr := controller.Sessions(ctx, "", ""); listErr != nil || len(rows.Rows) != 1 {
		t.Fatalf("sessions=%#v %v", rows, listErr)
	}
	if uploaded, uploadErr := controller.Upload(ctx, "note.txt", "text/plain", []byte("body")); uploadErr != nil || uploaded.Ref.ID != "uploaded" {
		t.Fatalf("upload=%#v %v", uploaded, uploadErr)
	}
	if !controller.HasCapability(types.CapTaskControl) {
		t.Fatalf("negotiated capabilities incorrect")
	}
	if err = controller.Switch(ctx, two); err != nil {
		t.Fatal(err)
	}
	if err = controller.ReplaceToken(ctx, testToken(t, two, now.Add(2*time.Hour))); err != nil {
		t.Fatalf("replace token: %v", err)
	}
	f.emit("one", types.StateEvent{Type: "task.started", Sequence: 2, OccurredAt: now, Tenant: "t", User: "u", Session: "one", Payload: map[string]any{"TaskID": "leak"}})
	f.emit("two", types.StateEvent{Type: "task.started", Sequence: 3, OccurredAt: now, Tenant: "t", User: "u", Session: "two", Payload: map[string]any{"TaskID": "target"}})
	awaitBlock(t, ctx, updates, "target")
	for _, b := range controller.Projection().Blocks {
		if b.RunID == "leak" {
			t.Fatal("old stream leaked")
		}
	}
	if deleted, err := controller.Delete(ctx); err != nil || !deleted.Deleted {
		t.Fatalf("delete=%#v %v", deleted, err)
	}
	if _, err = controller.Start(ctx, "erased", nil, nil); err == nil {
		t.Fatal("erased session started")
	}
	if err = controller.Close(); err != nil {
		t.Fatal(err)
	}
	if err = controller.Close(); err != nil {
		t.Fatal(err)
	}
	if err = controller.Switch(ctx, one); err == nil {
		t.Fatal("closed controller switched")
	}
}
func awaitBlock(t *testing.T, ctx context.Context, updates <-chan Update, run string) {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("missing block %s", run)
		case update := <-updates:
			for _, block := range update.Projection.Blocks {
				if block.RunID == run {
					return
				}
			}
		}
	}
}

func TestController_ValidationAndVisibleFailures(t *testing.T) {
	if _, err := NewController("x", nil, types.IdentityScope{}, nil); err == nil {
		t.Fatal("nil token accepted")
	}
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	now := time.Now()
	source := NewTokenSource("", testToken(t, id, now.Add(-time.Hour)))
	source.now = func() time.Time { return now }
	updates := make(chan Update, 2)
	controller, err := NewController("http://127.0.0.1:1", source, id, func(update Update) { updates <- update })
	if err != nil {
		t.Fatal(err)
	}
	if err = controller.Attach(t.Context()); err == nil {
		t.Fatal("expired token attached")
	}
	if update := <-updates; update.State != StateConnecting {
		t.Fatalf("first=%s", update.State)
	}
	if update := <-updates; update.State != StateAuthExpired {
		t.Fatalf("failure=%s", update.State)
	}
	_ = protoerrors.CodeSessionErased
	if err := requireConversationCapabilities([]types.Capability{types.CapEventsSubscribe}); err == nil || !strings.Contains(err.Error(), string(types.CapStateSnapshots)) {
		t.Fatalf("missing capability=%v", err)
	}
	if err := requireConversationCapabilities([]types.Capability{types.CapEventsSubscribe, types.CapStateSnapshots}); err != nil {
		t.Fatal(err)
	}
	f, server := newControllerFixture(t)
	defer server.Close()
	f.capabilities = []types.Capability{types.CapEventsSubscribe}
	valid := NewTokenSource("", testToken(t, id, now.Add(time.Hour)))
	missingCapability, err := NewController(server.URL, valid, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = missingCapability.Attach(t.Context()); err == nil || !strings.Contains(err.Error(), string(types.CapStateSnapshots)) {
		t.Fatalf("attach missing state snapshots=%v", err)
	}
}

func TestController_UnattachedAndInvalidTargetFailures(t *testing.T) {
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	source := NewTokenSource("", "x")
	controller, err := NewController("http://example.test", source, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = controller.Switch(t.Context(), types.IdentityScope{}); err == nil {
		t.Fatal("invalid switch accepted")
	}
	if _, err = controller.Start(t.Context(), "x", nil, nil); err == nil {
		t.Fatal("unattached start accepted")
	}
	if _, err = controller.Rename(t.Context(), "x"); err == nil {
		t.Fatal("unattached rename accepted")
	}
	if _, err = controller.Sessions(t.Context(), "", ""); err == nil {
		t.Fatal("unattached sessions accepted")
	}
	if _, err = controller.Delete(t.Context()); err == nil {
		t.Fatal("unattached delete accepted")
	}
	if _, err = controller.Upload(t.Context(), "x", "text/plain", nil); err == nil {
		t.Fatal("unattached upload accepted")
	}
	if err = controller.ReplaceToken(t.Context(), "malformed"); err == nil {
		t.Fatal("malformed in-memory replacement accepted")
	}
	if controller.HasCapability(types.CapTaskControl) {
		t.Fatal("unattached controller invented authority")
	}
	if _, err = NewController("x", source, types.IdentityScope{}, nil); err == nil {
		t.Fatal("invalid identity accepted")
	}
}

func TestController_ReconnectRehydratesAndResumesAtCursor(t *testing.T) {
	f, server := newControllerFixture(t)
	defer server.Close()
	now := time.Now()
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "reconnect"}
	source := NewTokenSource("", testToken(t, id, now.Add(time.Hour)))
	source.now = func() time.Time { return now }
	updates := make(chan Update, 64)
	controller, err := NewController(server.URL, source, id, func(update Update) { updates <- update })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err = controller.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	eventsAt, historyAt := -1, -1
	for i, path := range f.order {
		if path == "/v1/events" && eventsAt < 0 {
			eventsAt = i
		}
		if path == "/v1/state/history" && historyAt < 0 {
			historyAt = i
		}
	}
	f.mu.Unlock()
	if eventsAt < 0 || historyAt < 0 || eventsAt >= historyAt {
		t.Fatalf("attach did not open stream before hydration: order=%v", f.order)
	}
	f.awaitStreamRegistration(ctx, id.Session, 1)
	f.emit(id.Session, types.StateEvent{Type: "task.started", Sequence: 1, OccurredAt: now, Tenant: "t", User: "u", Session: id.Session, Payload: map[string]any{"TaskID": "before-drop"}})
	awaitBlock(t, ctx, updates, "before-drop")
	f.drop(id.Session)
	for {
		select {
		case <-ctx.Done():
			t.Fatal("did not reconnect")
		case update := <-updates:
			if update.State == StateLive && update.Attempt > 0 {
				goto reconnected
			}
		}
	}
reconnected:
	f.awaitStreamRegistration(ctx, id.Session, 2)
	f.emit(id.Session, types.StateEvent{Type: "task.started", Sequence: 2, OccurredAt: now, Tenant: "t", User: "u", Session: id.Session, Payload: map[string]any{"TaskID": "after-drop"}})
	awaitBlock(t, ctx, updates, "after-drop")
	if err = controller.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestController_TokenReplacementIsVerifiedTransactionallyAndClearRecoversFileCredential(t *testing.T) {
	f, server := newControllerFixture(t)
	defer server.Close()
	now := time.Now()
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "token-transaction"}
	fileToken := testToken(t, id, now.Add(time.Hour))
	path := t.TempDir() + "/token"
	if err := os.WriteFile(path, []byte(fileToken), 0o600); err != nil {
		t.Fatal(err)
	}
	source := NewTokenSource(path, "")
	source.now = func() time.Time { return now }
	updates := make(chan Update, 32)
	controller, err := NewController(server.URL, source, id, func(update Update) { updates <- update })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = controller.Close() }()
	if err = controller.Attach(t.Context()); err != nil {
		t.Fatal(err)
	}
	candidate := testToken(t, id, now.Add(2*time.Hour))
	f.mu.Lock()
	f.reject = candidate
	f.mu.Unlock()
	if err = controller.ReplaceToken(t.Context(), candidate); err == nil {
		t.Fatal("server-rejected candidate was committed")
	}
	if _, ok := source.Replacement(id); ok {
		t.Fatal("failed candidate remained installed")
	}
	f.mu.Lock()
	f.reject = ""
	f.mu.Unlock()
	if err = controller.ReplaceToken(t.Context(), candidate); err != nil {
		t.Fatalf("verified candidate replacement: %v", err)
	}
	if got, ok := source.Replacement(id); !ok || got != candidate {
		t.Fatal("verified candidate was not committed")
	}
	if err = controller.ReplaceToken(t.Context(), "clear"); err != nil {
		t.Fatalf("clear replacement and recover file token: %v", err)
	}
	if _, ok := source.Replacement(id); ok {
		t.Fatal("clear retained in-memory candidate")
	}
}

func TestController_CanonicalMutationFailuresStayExplicit(t *testing.T) {
	f, server := newControllerFixture(t)
	defer server.Close()
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "failures"}
	controller, err := NewController(server.URL, NewTokenSource("", testToken(t, id, time.Now().Add(time.Hour))), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = controller.Close() }()
	if err = controller.Attach(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		call func() error
	}{
		{"/v1/control/start", func() error { _, callErr := controller.Start(t.Context(), "preserve me", nil, nil); return callErr }},
		{"/v1/sessions/delete", func() error { _, callErr := controller.Delete(t.Context()); return callErr }},
	} {
		f.mu.Lock()
		f.failPath[tc.path] = http.StatusForbidden
		f.mu.Unlock()
		if callErr := tc.call(); callErr == nil || !strings.Contains(callErr.Error(), "403") {
			t.Fatalf("%s failure=%v", tc.path, callErr)
		}
		f.mu.Lock()
		delete(f.failPath, tc.path)
		f.mu.Unlock()
	}
}

func TestController_FailedSwitchKeepsOldSessionLive(t *testing.T) {
	f, server := newControllerFixture(t)
	defer server.Close()
	now := time.Now()
	oldID := types.IdentityScope{Tenant: "t", User: "u", Session: "old"}
	source := NewTokenSource("", testToken(t, oldID, now.Add(time.Hour)))
	updates := make(chan Update, 32)
	controller, err := NewController(server.URL, source, oldID, func(update Update) { updates <- update })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = controller.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err = controller.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	f.emit(oldID.Session, types.StateEvent{Type: "task.started", Sequence: 1, OccurredAt: now, Tenant: "t", User: "u", Session: oldID.Session, Payload: map[string]any{"TaskID": "retained"}})
	awaitBlock(t, ctx, updates, "retained")
	target := oldID
	target.Session = "missing-credential"
	if err = controller.Switch(ctx, target); !errors.Is(err, ErrTokenUnavailable) {
		t.Fatalf("failed switch=%v", err)
	}
	// A failed switch is non-destructive: the previous session republishes
	// its LIVE posture (with the error attached for the surface toast)
	// instead of being torn down into a disconnected dead-end.
	var failure Update
	for failure.State != StateLive || failure.Err == nil {
		select {
		case failure = <-updates:
		case <-ctx.Done():
			t.Fatal("missing live posture republish after failed switch")
		}
	}
	if scopeKey(failure.Identity) != scopeKey(oldID) || failure.Generation == 0 {
		t.Fatalf("failure identity/generation=%#v/%d", failure.Identity, failure.Generation)
	}
	found := false
	for _, block := range failure.Projection.Blocks {
		found = found || block.RunID == "retained"
	}
	if !found || controller.Identity().Session != oldID.Session {
		t.Fatalf("failed switch lost old transcript or identity: update=%#v identity=%#v", failure, controller.Identity())
	}
	// The old stream must still deliver events after the failed switch.
	f.emit(oldID.Session, types.StateEvent{Type: "task.started", Sequence: 2, OccurredAt: now, Tenant: "t", User: "u", Session: oldID.Session, Payload: map[string]any{"TaskID": "still-alive"}})
	awaitBlock(t, ctx, updates, "still-alive")
}

func TestController_ConcurrentSwitchReuseCancellationAndLeak(t *testing.T) {
	_, server := newControllerFixture(t)
	defer server.Close()
	now := time.Now()
	base := types.IdentityScope{Tenant: "t", User: "u", Session: "base"}
	source := NewTokenSource("", testToken(t, base, now.Add(time.Hour)))
	for i := range 110 {
		id := base
		id.Session = fmt.Sprintf("session-%03d", i)
		if err := source.Replace(id, testToken(t, id, now.Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	controller, err := NewController(server.URL, source, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = controller.Attach(t.Context()); err != nil {
		t.Fatal(err)
	}
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errs := make(chan error, 110)
	for i := range 110 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := base
			id.Session = fmt.Sprintf("session-%03d", i)
			ctx := t.Context()
			if i%11 == 0 {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			if switchErr := controller.Switch(ctx, id); switchErr != nil && i%11 != 0 {
				errs <- switchErr
			}
		}()
	}
	wg.Wait()
	close(errs)
	for switchErr := range errs {
		t.Error(switchErr)
	}
	if err = controller.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+4 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if runtime.NumGoroutine() > baseline+4 {
		t.Fatalf("goroutine leak baseline=%d now=%d", baseline, runtime.NumGoroutine())
	}
}
