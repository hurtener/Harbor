package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

func testClient(t *testing.T, server *httptest.Server, opts ...Option) Client {
	t.Helper()
	opts = append([]Option{WithHTTPClient(server.Client())}, opts...)
	identity := types.IdentityScope{Tenant: "tenant", User: "user", Session: "session"}
	client, err := New(Connection{
		BaseURL: server.URL,
		Token: TokenSourceFunc(func(context.Context, types.IdentityScope) (string, error) {
			return "test-token", nil
		}),
		Identity: identity,
	}, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestRuntimeClient_ConcurrentInspectionActionsAndCancellationIsolation(t *testing.T) {
	baseline := runtime.NumGoroutine()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/control/runtime.counters" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Harbor-Session") != "session" {
			t.Errorf("identity bleed: %q", r.Header.Get("X-Harbor-Session"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events_per_second":1,"tasks_running":2,"background_jobs_active":0,"mcp_connections_healthy":0,"sessions_active":1,"snapshot_at":42}`))
	}))
	defer server.Close()
	client, ok := testClient(t, server).(RuntimeClient)
	if !ok {
		t.Fatal("New client does not implement RuntimeClient")
	}
	var wait sync.WaitGroup
	for n := range 128 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			ctx := t.Context()
			if n%7 == 0 {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			response, err := client.RuntimeCounters(ctx)
			if n%7 == 0 {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("cancel %d: %v", n, err)
				}
				return
			}
			if err != nil || response.SnapshotAt != 42 || response.TasksRunning != 2 {
				t.Errorf("request %d: %#v %v", n, response, err)
			}
		}()
	}
	wait.Wait()
	server.CloseClientConnections()
	eventuallyGoroutinesSettle(t, baseline, 8)
}

func TestClient_RuntimeInfo_ValidatesHandshakeAndHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/control/runtime.info" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Harbor-Session"); got != "session" {
			t.Errorf("X-Harbor-Session = %q", got)
		}
		_ = json.NewEncoder(w).Encode(types.RuntimeInfo{ProtocolVersion: types.ProtocolVersion})
	}))
	defer server.Close()

	info, err := testClient(t, server).RuntimeInfo(context.Background())
	if err != nil {
		t.Fatalf("RuntimeInfo: %v", err)
	}
	if info.ProtocolVersion != types.ProtocolVersion {
		t.Fatalf("ProtocolVersion = %q", info.ProtocolVersion)
	}
}

func TestClient_RuntimeInfo_RejectsIncompatibleHandshake(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(types.RuntimeInfo{ProtocolVersion: "1.0.0"})
	}))
	defer server.Close()

	_, err := testClient(t, server).RuntimeInfo(context.Background())
	if !errors.Is(err, ErrIncompatibleProtocol) {
		t.Fatalf("error = %v, want ErrIncompatibleProtocol", err)
	}
}

func TestClient_Call_DecodesTypedProtocolError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(&protoerrors.Error{Code: protoerrors.CodeAuthRejected, Message: "expired"})
	}))
	defer server.Close()

	_, err := testClient(t, server).RuntimeHealth(context.Background())
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("error = %T %v, want *ProtocolError", err, err)
	}
	if protocolErr.Status != http.StatusUnauthorized || protocolErr.Code != protoerrors.CodeAuthRejected {
		t.Fatalf("typed error = %+v", protocolErr)
	}
}

func TestClient_Call_RejectsMalformedUnknownAndOversizedResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		{name: "malformed", body: `{`, want: ErrMalformedResponse},
		{name: "unknown field", body: `{"subsystems":[],"unexpected":true}`, want: ErrMalformedResponse},
		{name: "multiple values", body: `{"subsystems":[]} {}`, want: ErrMalformedResponse},
		{name: "oversized", body: strings.Repeat("x", 65), want: ErrResponseTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := testClient(t, server, WithResponseLimits(64, 64))
			_, err := client.RuntimeHealth(context.Background())
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestClient_TokenSource_ResolvesEveryRequestAfterExpiry(t *testing.T) {
	var calls atomic.Int64
	source := TokenSourceFunc(func(context.Context, types.IdentityScope) (string, error) {
		if calls.Add(1) == 1 {
			return "expired", nil
		}
		return "fresh", nil
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer expired" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(&protoerrors.Error{Code: protoerrors.CodeAuthRejected, Message: "expired"})
			return
		}
		_ = json.NewEncoder(w).Encode(types.RuntimeHealth{Subsystems: []types.SubsystemHealth{}})
	}))
	defer server.Close()
	client, err := New(Connection{
		BaseURL:  server.URL,
		Token:    source,
		Identity: types.IdentityScope{Tenant: "tenant", User: "user", Session: "session"},
	}, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.RuntimeHealth(context.Background()); err == nil {
		t.Fatal("expired request succeeded")
	}
	if _, err := client.RuntimeHealth(context.Background()); err != nil {
		t.Fatalf("fresh request: %v", err)
	}
}

// cancelledConcurrentIndex reports whether the concurrent-reuse harness below
// cancels the context of the goroutine at index. It is the single definition
// shared by the request goroutines, the cancellation loop, and the test server
// handler, so no branch can drift out of step with the others.
func cancelledConcurrentIndex(index int) bool { return index%8 <= 1 }

func TestClient_ConcurrentReuse_SessionIsolationCancellationAndLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()
	const count = 128
	inflight := make(chan string, count)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := r.Header.Get("X-Harbor-Session")
		index, indexErr := strconv.Atoi(strings.TrimPrefix(session, "session-"))
		if indexErr != nil {
			t.Errorf("unparseable session header %q: %v", session, indexErr)
			return
		}
		if r.URL.Path == "/v1/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			inflight <- session
			if cancelledConcurrentIndex(index) {
				<-r.Context().Done()
				return
			}
			select {
			case <-release:
				_, _ = fmt.Fprintf(w, "data: {\"session\":%q}\n\n", session)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
			}
			return
		}
		var request types.TaskListRequest
		// Decode BEFORE signalling inflight. The harness fires its cancels the
		// instant the last inflight signal lands, so a decode still in progress
		// would fail with context.Canceled through no fault of the client.
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if request.Identity.Session != session {
			t.Errorf("body session %q != header session %q", request.Identity.Session, session)
		}
		inflight <- session
		// A request whose context the harness cancels must never be ANSWERABLE.
		// Context cancellation is best-effort: net/http's (*persistConn).roundTrip
		// waits in a select over "response headers arrived" and "context done",
		// and when both are ready Go picks pseudo-randomly — so a handler that
		// CAN answer a cancelled request turns the assertion below into a coin
		// flip (issue #599: ~15% of Linux runs, always at the late indices whose
		// cancel lands closest to the release). Serving a cancelled session
		// nothing but its own cancellation makes the guarantee actually under
		// test — a cancel aborts its own call and never disturbs a sibling —
		// deterministic on every platform.
		if cancelledConcurrentIndex(index) {
			<-r.Context().Done()
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(types.TaskListResponse{Rows: []types.TaskRow{{ID: session}}})
	}))
	identity := types.IdentityScope{Tenant: "tenant", User: "user", Session: "session"}
	client, err := New(Connection{
		BaseURL: server.URL,
		Token: TokenSourceFunc(func(_ context.Context, requested types.IdentityScope) (string, error) {
			if requested.Tenant != "tenant" || requested.User != "user" || requested.Session == "" {
				return "", fmt.Errorf("unexpected token scope: %+v", requested)
			}
			return "test-token", nil
		}),
		Identity: identity,
	}, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wait sync.WaitGroup
	errorsCh := make(chan error, count)
	cancels := make([]context.CancelFunc, count)
	for i := range count {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		wait.Add(1)
		go func(index int, ctx context.Context) {
			defer wait.Done()
			session := "session-" + strconv.Itoa(index)
			clone := client.WithSession(session)
			if index%2 == 1 {
				stream, streamErr := clone.Subscribe(ctx, StreamOptions{})
				if streamErr != nil {
					if !cancelledConcurrentIndex(index) {
						errorsCh <- fmt.Errorf("stream %s: %w", session, streamErr)
					}
					return
				}
				_, recvErr := stream.Recv(ctx)
				closeErr := stream.Close()
				if cancelledConcurrentIndex(index) {
					if recvErr == nil {
						errorsCh <- fmt.Errorf("cancelled stream %s succeeded", session)
					}
					return
				}
				if recvErr != nil || closeErr != nil {
					errorsCh <- fmt.Errorf("stream %s: %w", session, errors.Join(recvErr, closeErr))
				}
				return
			}
			response, callErr := clone.TasksList(ctx, types.TaskListRequest{})
			if cancelledConcurrentIndex(index) {
				if callErr == nil {
					errorsCh <- fmt.Errorf("cancelled session %s succeeded", session)
				}
				return
			}
			if callErr != nil {
				errorsCh <- fmt.Errorf("session %s: %w", session, callErr)
				return
			}
			if len(response.Rows) != 1 || response.Rows[0].ID != session {
				errorsCh <- fmt.Errorf("session %s got rows %+v", session, response.Rows)
			}
		}(i, ctx)
	}
	for range count {
		<-inflight
	}
	for i := range count {
		if cancelledConcurrentIndex(i) {
			cancels[i]()
		}
	}
	close(release)
	wait.Wait()
	for _, cancel := range cancels {
		cancel()
	}
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	if client.Identity().Session != "session" {
		t.Fatalf("base client session mutated to %q", client.Identity().Session)
	}
	server.Close()
	eventuallyGoroutinesSettle(t, baseline, 4)
}

func TestClient_New_RejectsMissingIdentityAndToken(t *testing.T) {
	identity := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	partial := types.IdentityScope{Tenant: "t", User: "u"}
	if _, err := New(Connection{BaseURL: "http://example.test", Token: StaticToken("x", identity), Identity: partial}); !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("missing identity error = %v", err)
	}
	_, err := New(Connection{BaseURL: "http://example.test", Identity: identity})
	if !errors.Is(err, ErrInvalidConnection) {
		t.Fatalf("missing source error = %v", err)
	}
}

func TestClient_StaticToken_RejectsCrossSessionRESTAndSSE(t *testing.T) {
	identity := types.IdentityScope{Tenant: "tenant", User: "user", Session: "one"}
	client, err := New(Connection{
		BaseURL: "http://example.test", Token: StaticToken("token", identity), Identity: identity,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clone := client.WithSession("two")
	if _, err := clone.RuntimeHealth(context.Background()); !errors.Is(err, ErrTokenIdentityMismatch) {
		t.Fatalf("REST error = %v", err)
	}
	if _, err := clone.Subscribe(context.Background(), StreamOptions{}); !errors.Is(err, ErrTokenIdentityMismatch) {
		t.Fatalf("SSE error = %v", err)
	}
}

func TestClient_IdentityScope_IsDeepCopiedAcrossBoundaries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(types.RuntimeHealth{})
	}))
	defer server.Close()
	actor := &types.IdentityScope{Tenant: "admin-tenant", User: "admin", Session: "admin-session"}
	requester := &types.IdentityScope{Tenant: actor.Tenant, User: actor.User, Session: actor.Session}
	target := &types.IdentityScope{Tenant: "tenant", User: "user", Session: "one"}
	source := types.IdentityScope{
		Tenant: target.Tenant, User: target.User, Session: target.Session,
		Actor: actor, Requester: requester, Impersonating: target,
	}
	client, err := New(Connection{
		BaseURL: server.URL,
		Token: TokenSourceFunc(func(_ context.Context, requested types.IdentityScope) (string, error) {
			requested.Actor.Session = "mutated-inside-token-source"
			requested.Impersonating.Session = "mutated-inside-token-source"
			return "token", nil
		}),
		Identity: source,
	}, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	actor.Session = "mutated-source"
	requester.Session = "mutated-source"
	target.Session = "mutated-source"
	returned := client.Identity()
	returned.Actor.Session = "mutated-return"
	returned.Requester.Session = "mutated-return"
	returned.Impersonating.Session = "mutated-return"

	const calls = 128
	var wait sync.WaitGroup
	for range calls {
		wait.Add(1)
		go func() {
			defer wait.Done()
			identity := client.Identity()
			identity.Actor.Session = "caller-copy"
			if _, callErr := client.RuntimeHealth(context.Background()); callErr != nil {
				t.Errorf("RuntimeHealth: %v", callErr)
			}
		}()
	}
	wait.Wait()
	got := client.Identity()
	if got.Session != "one" || got.Actor.Session != "admin-session" || got.Requester.Session != "admin-session" || got.Impersonating.Session != "one" {
		t.Fatalf("stored identity mutated: %+v", got)
	}
	clone := client.WithSession("two").Identity()
	if clone.Session != "two" || clone.Impersonating.Session != "two" || clone.Actor.Session != "admin-session" || clone.Requester.Session != "admin-session" {
		t.Fatalf("impersonation clone = %+v", clone)
	}
}

func TestClient_ForSession_DoesNotMutateInputRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(types.StartResponse{TaskID: "task", ProtocolVersion: types.ProtocolVersion})
	}))
	defer server.Close()
	client := testClient(t, server).WithSession("other")
	request := types.StartRequest{Identity: types.IdentityScope{Tenant: "original", User: "original", Session: "original"}}
	if _, err := client.Start(context.Background(), request); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if request.Identity.Session != "original" {
		t.Fatalf("request mutated: %+v", request.Identity)
	}
}

func TestClient_TypedMethods_RouteAndOverlayIdentity(t *testing.T) {
	seen := make(map[string]int)
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path]++
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := testClient(t, server).WithSession("typed-session")
	ctx := context.Background()
	if _, err := client.TasksGet(ctx, types.TaskGetRequest{ID: "task"}); err != nil {
		t.Error(err)
	}
	if _, err := client.SessionsList(ctx, types.SessionsListRequest{}); err != nil {
		t.Error(err)
	}
	if _, err := client.SessionsInspect(ctx, types.SessionsInspectRequest{SessionID: "typed-session"}); err != nil {
		t.Error(err)
	}
	if _, err := client.SessionsSetTitle(ctx, types.SessionsSetTitleRequest{SessionID: "typed-session", Title: "title"}); err != nil {
		t.Error(err)
	}
	if _, err := client.SessionsDelete(ctx); err != nil {
		t.Error(err)
	}
	if _, err := client.StateHistory(ctx, types.StateHistoryRequest{}); err != nil {
		t.Error(err)
	}
	if _, err := client.PauseList(ctx, types.PauseListRequest{}); err != nil {
		t.Error(err)
	}
	if _, err := client.Control(ctx, methods.MethodCancel, types.ControlRequest{Identity: types.IdentityScope{Run: "run", Scope: "owner_user"}}); err != nil {
		t.Error(err)
	}
	if _, err := client.ArtifactsPut(ctx, types.ArtifactsPutRequest{Bytes: []byte("x")}); err != nil {
		t.Error(err)
	}
	if _, err := client.ArtifactsList(ctx, types.ArtifactsListRequest{}); err != nil {
		t.Error(err)
	}
	want := []string{
		"/v1/tasks/get", "/v1/sessions/list", "/v1/sessions/inspect",
		"/v1/sessions/set_title", "/v1/sessions/delete", "/v1/state/history",
		"/v1/pause/list", "/v1/control/cancel", "/v1/control/artifacts.put",
		"/v1/control/artifacts.list",
	}
	for _, path := range want {
		if seen[path] != 1 {
			t.Errorf("route %s count = %d", path, seen[path])
		}
	}
	if _, err := client.Control(ctx, methods.MethodStart, types.ControlRequest{}); err == nil {
		t.Error("non-control method accepted by Control")
	}
}

func TestClient_RequestAndErrorFailureBranches(t *testing.T) {
	t.Run("invalid base URL", func(t *testing.T) {
		identity := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
		_, err := New(Connection{BaseURL: "://bad", Token: StaticToken("x", identity), Identity: identity})
		if !errors.Is(err, ErrInvalidConnection) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("token source failure", func(t *testing.T) {
		want := errors.New("token unavailable")
		client, err := New(Connection{
			BaseURL: "http://example.test",
			Token: TokenSourceFunc(func(context.Context, types.IdentityScope) (string, error) {
				return "", want
			}),
			Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := client.RuntimeHealth(context.Background()); !errors.Is(err, want) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("empty token", func(t *testing.T) {
		identity := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
		client, err := New(Connection{BaseURL: "http://example.test", Token: StaticToken(" ", identity), Identity: identity})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := client.RuntimeHealth(context.Background()); !errors.Is(err, ErrTokenRequired) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("malformed error envelope", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("not-json"))
		}))
		defer server.Close()
		_, err := testClient(t, server).RuntimeHealth(context.Background())
		var protocolErr *ProtocolError
		if !errors.As(err, &protocolErr) || !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("error = %T %v", err, err)
		}
		if protocolErr.Error() == "" || protocolErr.Unwrap() == nil {
			t.Fatal("typed error did not expose text and cause")
		}
	})
	t.Run("oversized error envelope", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(strings.Repeat("x", 9)))
		}))
		defer server.Close()
		_, err := testClient(t, server, WithResponseLimits(1024, 8)).RuntimeHealth(context.Background())
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("nil response drains bounded body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		}))
		defer server.Close()
		if err := testClient(t, server).(*client).Call(context.Background(), "/custom", struct{}{}, nil); err != nil {
			t.Fatalf("Call: %v", err)
		}
	})
	for _, protocolErr := range []*ProtocolError{
		{Status: 500, Code: protoerrors.CodeRuntimeError, Message: "failed"},
		{Status: 500, Cause: errors.New("failed")},
		{Status: 500, Message: "failed"},
	} {
		if protocolErr.Error() == "" {
			t.Error("Error returned empty string")
		}
	}
}
