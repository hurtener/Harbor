package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	protocolclient "github.com/hurtener/Harbor/internal/protocol/client"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

func TestE2E_ProtocolClient_AuthStartReadAndSSEReconnect(t *testing.T) {
	stack := devstack.Assemble(t, runtimePostureConfig(t), devstack.AssembleOpts{})
	defer stack.Close()
	server := httptest.NewServer(stack.Handler)
	defer server.Close()

	expired := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": "harbor-test", "sub": devstack.DefaultDevUser, "aud": "harbor",
		"exp": time.Now().Add(-time.Hour).Unix(), "nbf": time.Now().Add(-2 * time.Hour).Unix(),
		"iat":    time.Now().Add(-2 * time.Hour).Unix(),
		"tenant": devstack.DefaultDevTenant, "user": devstack.DefaultDevUser,
		"session": devstack.DefaultDevSession, "scopes": []string{"admin", "console:fleet"},
		"agent_reach": []string{stack.AgentConfigID},
	})
	expired.Header["kid"] = stack.KID
	expiredToken, err := expired.SignedString(stack.SigningKey)
	if err != nil {
		t.Fatalf("sign expired token: %v", err)
	}
	const secondSession = "protocol-client-session-two"
	mintToken := func(session string) string {
		t.Helper()
		token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
			"iss": "harbor-test", "sub": devstack.DefaultDevUser, "aud": "harbor",
			"exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(),
			"iat": time.Now().Unix(), "tenant": devstack.DefaultDevTenant, "user": devstack.DefaultDevUser,
			"session": session, "scopes": []string{"admin", "console:fleet"},
			"agent_reach": []string{stack.AgentConfigID},
		})
		token.Header["kid"] = stack.KID
		signed, signErr := token.SignedString(stack.SigningKey)
		if signErr != nil {
			t.Fatalf("sign token for %s: %v", session, signErr)
		}
		return signed
	}
	tokens := map[string]string{
		devstack.DefaultDevSession: stack.Token,
		secondSession:              mintToken(secondSession),
	}
	var tokenCalls atomic.Int64
	client, err := protocolclient.New(protocolclient.Connection{
		BaseURL: server.URL,
		Token: protocolclient.TokenSourceFunc(func(_ context.Context, requested prototypes.IdentityScope) (string, error) {
			if tokenCalls.Add(1) == 1 {
				return expiredToken, nil
			}
			return tokens[requested.Session], nil
		}),
		Identity: prototypes.IdentityScope{
			Tenant: devstack.DefaultDevTenant, User: devstack.DefaultDevUser, Session: devstack.DefaultDevSession,
		},
	}, protocolclient.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	if _, err := client.RuntimeInfo(t.Context()); err == nil {
		t.Fatal("expired token unexpectedly completed the handshake")
	} else {
		var protocolErr *protocolclient.ProtocolError
		if !errors.As(err, &protocolErr) || protocolErr.Code != protoerrors.CodeAuthRejected {
			t.Fatalf("expired token error = %T %v", err, err)
		}
	}
	info, err := client.RuntimeInfo(t.Context())
	if err != nil {
		t.Fatalf("RuntimeInfo after refresh: %v", err)
	}
	if info.ProtocolVersion != prototypes.ProtocolVersion {
		t.Fatalf("ProtocolVersion = %q", info.ProtocolVersion)
	}
	if _, err := client.RuntimeHealth(t.Context()); err != nil {
		t.Fatalf("RuntimeHealth: %v", err)
	}
	if _, err := client.SessionsList(t.Context(), prototypes.SessionsListRequest{}); err != nil {
		t.Fatalf("SessionsList: %v", err)
	}
	if _, err := client.PauseList(t.Context(), prototypes.PauseListRequest{}); err != nil {
		t.Fatalf("PauseList: %v", err)
	}
	if _, err := client.WithSession(secondSession).RuntimeInfo(t.Context()); err != nil {
		t.Fatalf("second-session RuntimeInfo: %v", err)
	}

	staticIdentity := prototypes.IdentityScope{
		Tenant: devstack.DefaultDevTenant, User: devstack.DefaultDevUser, Session: devstack.DefaultDevSession,
	}
	staticClient, err := protocolclient.New(protocolclient.Connection{
		BaseURL: server.URL, Token: protocolclient.StaticToken(stack.Token, staticIdentity), Identity: staticIdentity,
	}, protocolclient.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("static client: %v", err)
	}
	if _, err := staticClient.WithSession(secondSession).RuntimeInfo(t.Context()); !errors.Is(err, protocolclient.ErrTokenIdentityMismatch) {
		t.Fatalf("static cross-session REST error = %v", err)
	}
	if _, err := staticClient.WithSession(secondSession).Subscribe(t.Context(), protocolclient.StreamOptions{}); !errors.Is(err, protocolclient.ErrTokenIdentityMismatch) {
		t.Fatalf("static cross-session SSE error = %v", err)
	}
	partialIdentity := prototypes.IdentityScope{Tenant: devstack.DefaultDevTenant, User: devstack.DefaultDevUser}
	if _, err := protocolclient.New(protocolclient.Connection{BaseURL: server.URL, Token: protocolclient.StaticToken(stack.Token, staticIdentity), Identity: partialIdentity}); !errors.Is(err, protocolclient.ErrIdentityRequired) {
		t.Fatalf("missing identity error = %v", err)
	}
	emptyTokenClient, err := protocolclient.New(protocolclient.Connection{
		BaseURL: server.URL,
		Token: protocolclient.TokenSourceFunc(func(context.Context, prototypes.IdentityScope) (string, error) {
			return "", nil
		}),
		Identity: staticIdentity,
	}, protocolclient.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("empty-token client: %v", err)
	}
	if _, err := emptyTokenClient.RuntimeInfo(t.Context()); !errors.Is(err, protocolclient.ErrTokenRequired) {
		t.Fatalf("missing token error = %v", err)
	}

	started, err := client.Start(t.Context(), prototypes.StartRequest{Query: "protocol client integration"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.TaskID == "" {
		t.Fatal("Start returned empty task id")
	}
	secondClient := client.WithSession(secondSession)
	secondStarted, err := secondClient.Start(t.Context(), prototypes.StartRequest{Query: "protocol client second session"})
	if err != nil {
		t.Fatalf("second-session Start: %v", err)
	}
	if _, err := client.StateHistory(t.Context(), prototypes.StateHistoryRequest{}); err != nil {
		t.Fatalf("StateHistory: %v", err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	terminal := map[string]bool{}
	for len(terminal) < 2 {
		for taskID, taskClient := range map[string]protocolclient.Client{
			started.TaskID: client, secondStarted.TaskID: secondClient,
		} {
			detail, getErr := taskClient.TasksGet(t.Context(), prototypes.TaskGetRequest{ID: taskID})
			if getErr == nil && detail.Task.ID == taskID && detail.Task.Status != prototypes.TaskStatusPending && detail.Task.Status != prototypes.TaskStatusRunning {
				terminal[taskID] = true
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("TasksGet did not observe both terminal tasks: %v", terminal)
		case <-ticker.C:
		}
	}
	firstTasks, err := client.TasksList(t.Context(), prototypes.TaskListRequest{})
	if err != nil {
		t.Fatalf("first TasksList: %v", err)
	}
	secondTasks, err := secondClient.TasksList(t.Context(), prototypes.TaskListRequest{})
	if err != nil {
		t.Fatalf("second TasksList: %v", err)
	}
	assertOnlyTaskSession := func(name string, rows []prototypes.TaskRow, own, foreign string) {
		t.Helper()
		seenOwn := false
		for _, row := range rows {
			if row.ID == foreign {
				t.Fatalf("%s leaked foreign task %s", name, foreign)
			}
			seenOwn = seenOwn || row.ID == own
		}
		if !seenOwn {
			t.Fatalf("%s did not contain own task %s", name, own)
		}
	}
	assertOnlyTaskSession("first session", firstTasks.Rows, started.TaskID, secondStarted.TaskID)
	assertOnlyTaskSession("second session", secondTasks.Rows, secondStarted.TaskID, started.TaskID)

	publish := func(session, taskID string) {
		t.Helper()
		id := identity.Identity{
			TenantID: devstack.DefaultDevTenant, UserID: devstack.DefaultDevUser, SessionID: session,
		}
		if err := stack.Bus.Publish(t.Context(), events.Event{
			Type:       tasks.EventTypeTaskSpawned,
			Identity:   identity.Quadruple{Identity: id},
			Payload:    tasks.TaskSpawnedPayload{TaskID: tasks.TaskID(taskID)},
			OccurredAt: time.Now(),
		}); err != nil {
			t.Fatalf("publish %s: %v", taskID, err)
		}
	}
	publish(devstack.DefaultDevSession, "client-replay-1")
	publish(devstack.DefaultDevSession, "client-replay-2")
	publish(secondSession, "client-session-two")

	stream, err := client.Subscribe(t.Context(), protocolclient.StreamOptions{
		EventTypes: []string{string(tasks.EventTypeTaskSpawned)}, LastEventID: "0",
	})
	if err != nil {
		t.Fatalf("Subscribe replay: %v", err)
	}
	lastID := ""
	seen := make([]string, 0, 2)
	for len(seen) < 2 {
		frame, recvErr := stream.Recv(t.Context())
		if recvErr != nil {
			t.Fatalf("Recv replay: %v", recvErr)
		}
		if len(frame.Data) == 0 {
			continue
		}
		var event prototypes.StateEvent
		if err := json.Unmarshal(frame.Data, &event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if event.Session != devstack.DefaultDevSession {
			t.Fatalf("default stream leaked session %q", event.Session)
		}
		payload, _ := event.Payload.(map[string]any)
		taskID, _ := payload["TaskID"].(string)
		if taskID == "client-replay-1" || taskID == "client-replay-2" {
			seen = append(seen, taskID)
			lastID = frame.ID
		}
	}
	if seen[0] != "client-replay-1" || seen[1] != "client-replay-2" {
		t.Fatalf("replay order = %v", seen)
	}
	if _, err := strconv.ParseUint(lastID, 10, 64); err != nil {
		t.Fatalf("Last-Event-ID %q is not a sequence: %v", lastID, err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close first stream: %v", err)
	}
	secondStream, err := secondClient.Subscribe(t.Context(), protocolclient.StreamOptions{
		EventTypes: []string{string(tasks.EventTypeTaskSpawned)}, LastEventID: "0",
	})
	if err != nil {
		t.Fatalf("second-session Subscribe: %v", err)
	}
	for {
		frame, recvErr := secondStream.Recv(t.Context())
		if recvErr != nil {
			t.Fatalf("second-session Recv: %v", recvErr)
		}
		if len(frame.Data) == 0 {
			continue
		}
		var event prototypes.StateEvent
		if err := json.Unmarshal(frame.Data, &event); err != nil {
			t.Fatalf("decode second-session event: %v", err)
		}
		if event.Session != secondSession {
			t.Fatalf("second stream leaked session %q", event.Session)
		}
		payload, _ := event.Payload.(map[string]any)
		if payload["TaskID"] == "client-session-two" {
			break
		}
	}
	if err := secondStream.Close(); err != nil {
		t.Fatalf("close second stream: %v", err)
	}

	publish(devstack.DefaultDevSession, "client-replay-3")
	reconnected, err := client.Subscribe(t.Context(), protocolclient.StreamOptions{
		EventTypes: []string{string(tasks.EventTypeTaskSpawned)}, LastEventID: lastID,
	})
	if err != nil {
		t.Fatalf("Subscribe reconnect: %v", err)
	}
	defer func() { _ = reconnected.Close() }()
	for {
		frame, recvErr := reconnected.Recv(t.Context())
		if recvErr != nil {
			t.Fatalf("Recv reconnect: %v", recvErr)
		}
		if len(frame.Data) == 0 {
			continue
		}
		var event prototypes.StateEvent
		if err := json.Unmarshal(frame.Data, &event); err != nil {
			t.Fatalf("decode reconnect event: %v", err)
		}
		payload, _ := event.Payload.(map[string]any)
		if payload["TaskID"] == "client-replay-3" {
			break
		}
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.RuntimeHealth(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled RuntimeHealth error = %v", err)
	}
}
