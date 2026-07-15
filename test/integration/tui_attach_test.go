package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tui/conversation"
)

func TestE2E_TUIAttach_AuthenticatedSwitchDrainsOldStreamAndRotatesToken(t *testing.T) {
	stack := devstack.Assemble(t, runtimePostureConfig(t), devstack.AssembleOpts{})
	defer stack.Close()
	var authMu sync.Mutex
	var eventTokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			authMu.Lock()
			eventTokens = append(eventTokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			authMu.Unlock()
		}
		stack.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	const secondSession = "tui-session-two"
	mint := func(session string) string {
		token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": "harbor-test", "sub": devstack.DefaultDevUser, "aud": "harbor", "exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(), "iat": time.Now().Unix(), "tenant": devstack.DefaultDevTenant, "user": devstack.DefaultDevUser, "session": session, "scopes": []string{"admin", "console:fleet"}})
		token.Header["kid"] = stack.KID
		signed, err := token.SignedString(stack.SigningKey)
		if err != nil {
			t.Fatal(err)
		}
		return signed
	}
	first := types.IdentityScope{Tenant: devstack.DefaultDevTenant, User: devstack.DefaultDevUser, Session: devstack.DefaultDevSession}
	second := first
	second.Session = secondSession
	tokenPath := filepath.Join(t.TempDir(), "tokens.json")
	writeTokens := func(secondToken string) {
		body, err := json.Marshal(map[string]string{first.Tenant + "/" + first.User + "/" + first.Session: stack.Token, second.Tenant + "/" + second.User + "/" + second.Session: secondToken})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tokenPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeTokens(mint(secondSession))
	updates := make(chan conversation.Update, 128)
	source := conversation.NewTokenSource(tokenPath, "")
	controller, err := conversation.NewController(server.URL, source, first, func(update conversation.Update) { updates <- update })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = controller.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err = controller.Attach(ctx); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err = controller.Start(ctx, "first authenticated turn", nil, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitProjectionSession(t, ctx, updates, first.Session)
	rotated := mint(secondSession)
	writeTokens(rotated)
	if err = controller.Switch(ctx, second); err != nil {
		t.Fatalf("switch with rotated token: %v", err)
	}
	publish := func(session, taskID string) {
		id := identity.Identity{TenantID: first.Tenant, UserID: first.User, SessionID: session}
		if err := stack.Bus.Publish(ctx, events.Event{Type: tasks.EventTypeTaskSpawned, Identity: identity.Quadruple{Identity: id}, Payload: tasks.TaskSpawnedPayload{TaskID: tasks.TaskID(taskID)}}); err != nil {
			t.Fatal(err)
		}
	}
	publish(first.Session, "must-not-cross")
	publish(second.Session, "target-visible")
	foundTarget := false
	for !foundTarget {
		select {
		case <-ctx.Done():
			t.Fatal("target event not projected")
		case update := <-updates:
			for _, block := range update.Projection.Blocks {
				if block.RunID == "must-not-cross" {
					t.Fatal("old-session frame crossed switch")
				}
				if block.RunID == "target-visible" {
					if update.Projection.Identity.Session != second.Session {
						t.Fatalf("target projected under %s", update.Projection.Identity.Session)
					}
					foundTarget = true
				}
			}
		}
	}
	server.CloseClientConnections()
	awaitConnectionState(t, ctx, updates, conversation.StateReconnecting)
	reconnectToken := mint(secondSession)
	writeTokens(reconnectToken)
	awaitConnectionState(t, ctx, updates, conversation.StateLive)
	await(t, func() bool {
		authMu.Lock()
		defer authMu.Unlock()
		return len(eventTokens) >= 3 && eventTokens[len(eventTokens)-1] == reconnectToken
	}, "SSE reconnect resolved rotated token file")

	const memorySession = "tui-memory-token"
	memoryIdentity := first
	memoryIdentity.Session = memorySession
	if err := source.Replace(memoryIdentity, mint(memorySession)); err != nil {
		t.Fatalf("replace in-memory token: %v", err)
	}
	if err := controller.Switch(ctx, memoryIdentity); err != nil {
		t.Fatalf("switch with in-memory token: %v", err)
	}
	uploaded, err := controller.Upload(ctx, "note.txt", "text/plain", []byte("attachment"))
	if err != nil || uploaded.Ref.ID == "" {
		t.Fatalf("attachment upload: %#v %v", uploaded, err)
	}
	initial, err := controller.Start(ctx, "create memory-token session", nil, nil)
	if err != nil {
		t.Fatalf("initial memory session start: %v", err)
	}
	awaitTaskTerminalProjection(t, ctx, updates, initial.TaskID)

	runtimeID := identity.Identity{TenantID: memoryIdentity.Tenant, UserID: memoryIdentity.User, SessionID: memoryIdentity.Session}
	idCtx, err := identity.With(ctx, runtimeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := stack.Sessions.Close(idCtx, memorySession, "integration reopen"); err != nil {
		t.Fatalf("close session: %v", err)
	}
	awaitSessionStatus(t, ctx, updates, "completed")
	started, err := controller.Start(ctx, "reopen closed session", []string{uploaded.Ref.ID}, map[string]string{uploaded.Ref.ID: "ref"})
	if err != nil {
		t.Fatalf("canonical reopen start: %v", err)
	}
	awaitSessionStatus(t, ctx, updates, "running")
	awaitTaskTerminalProjection(t, ctx, updates, started.TaskID)
	if controller.Projection().SessionStatus != "running" {
		t.Fatalf("closed session did not reopen: %#v", controller.Projection())
	}
	deleted, err := controller.Delete(ctx)
	if err != nil || !deleted.Deleted {
		t.Fatalf("erase: %#v %v", deleted, err)
	}
	if _, err := controller.Start(ctx, "must not resurrect", nil, nil); err == nil {
		t.Fatal("erased session ordinary start succeeded")
	}

	const freshSession = "tui-start-fresh"
	fresh := first
	fresh.Session = freshSession
	if err := source.Replace(fresh, mint(freshSession)); err != nil {
		t.Fatal(err)
	}
	if err := controller.Switch(ctx, fresh); err != nil {
		t.Fatalf("Start Fresh switch: %v", err)
	}
	if controller.Identity().Session != freshSession {
		t.Fatalf("fresh identity=%#v", controller.Identity())
	}

	expiredIdentity := first
	expiredIdentity.Session = "expired-session"
	expiredController, err := conversation.NewController(server.URL, conversation.NewTokenSource("", mintExpiredToken(t, stack, expiredIdentity.Session)), expiredIdentity, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := expiredController.Attach(ctx); !errors.Is(err, conversation.ErrTokenExpired) {
		t.Fatalf("expired attach=%v", err)
	}
}

func awaitSessionStatus(t *testing.T, ctx context.Context, updates <-chan conversation.Update, status string) {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("session did not reach %s", status)
		case update := <-updates:
			if update.Projection.SessionStatus == status {
				return
			}
		}
	}
}

func awaitProjectionSession(t *testing.T, ctx context.Context, updates <-chan conversation.Update, session string) {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("no projection for %s", session)
		case update := <-updates:
			if update.Projection.Identity.Session == session && len(update.Projection.Blocks) > 0 {
				return
			}
		}
	}
}

func awaitConnectionState(t *testing.T, ctx context.Context, updates <-chan conversation.Update, state conversation.ConnectionState) {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("no connection state %s", state)
		case update := <-updates:
			if update.State == state {
				return
			}
		}
	}
}

func awaitTaskTerminalProjection(t *testing.T, ctx context.Context, updates <-chan conversation.Update, taskID string) {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("task %s did not become terminal", taskID)
		case update := <-updates:
			for _, block := range update.Projection.Blocks {
				if block.RunID == taskID && (block.Status == "completed" || block.Status == "failed" || block.Status == "cancelled") {
					return
				}
			}
		}
	}
}

func mintExpiredToken(t *testing.T, stack *devstack.DevStack, session string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": "harbor-test", "sub": devstack.DefaultDevUser, "aud": "harbor", "exp": time.Now().Add(-time.Minute).Unix(), "nbf": time.Now().Add(-time.Hour).Unix(), "iat": time.Now().Add(-time.Hour).Unix(), "tenant": devstack.DefaultDevTenant, "user": devstack.DefaultDevUser, "session": session, "scopes": []string{"admin", "console:fleet"}})
	token.Header["kid"] = stack.KID
	signed, err := token.SignedString(stack.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}
