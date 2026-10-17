package integration

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions/turns"

	// A minimal real SQLite store proves turn replay across runtime shutdown.
	turnsqlite "github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
	turnprotocol "github.com/hurtener/Harbor/internal/sessions/turns/protocol"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"

	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

func emptyCompletionWireResponse(t *testing.T, content, reasoning string) string {
	t.Helper()
	// This is the same canonical OpenAI response shape as the existing real
	// Bifrost fixture, deliberately omitting provider usage and cost reports.
	raw, err := json.Marshal(map[string]any{
		"id": "empty-fixture", "object": "chat.completion", "created": 1700000000, "model": scriptedModel,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content, "reasoning": reasoning}, "finish_reason": "length"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestE2E_EmptyCompletion_RealBifrostHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"empty", "whitespace", "reasoning-only", "valid-text", "valid-tool"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, kind), func(t *testing.T) {
				response := emptyCompletionWireResponse(t, "", "")
				switch kind {
				case "whitespace":
					response = emptyCompletionWireResponse(t, " \t\n", "")
				case "reasoning-only":
					response = emptyCompletionWireResponse(t, "", "fixture reasoning without an answer")
				case "valid-text":
					response = scriptedFinishResponse("Fixture answer")
				case "valid-tool":
					response = scriptedToolCallResponse("call-fixture", "text_echo", `{"text":"fixture"}`)
				}
				server := newScriptedLLMServer(t, response)
				cfg := phase83lConfig(t, server.URL())
				enabled := true
				cfg.LLM.Corrections.Enabled = &enabled
				profile := cfg.LLM.ModelProfiles[scriptedModel]
				profile.MaxRetries = 3
				profile.Corrections = &config.LLMCorrectionsProfileConfig{UsageBackfillEnabled: true}
				cfg.LLM.ModelProfiles[scriptedModel] = profile
				stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
				defer stack.Close()
				id := identity.Identity{TenantID: devstack.DefaultDevTenant, UserID: devstack.DefaultDevUser, SessionID: devstack.DefaultDevSession}
				ctx, err := identity.WithRun(t.Context(), id, "empty-fixture-run")
				if err != nil {
					t.Fatal(err)
				}
				text := "Read the fixture"
				var validations atomic.Int64
				req := llm.CompleteRequest{Model: scriptedModel, Stream: stream, Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}, Tools: []llm.ToolDeclaration{{Name: "text_echo", Description: "fixture echo", Schema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)}}, Validator: func(llm.CompleteResponse) error { validations.Add(1); return nil }}
				result, err := stack.LLMClient.Complete(ctx, req)
				valid := strings.HasPrefix(kind, "valid-")
				if valid {
					if err != nil {
						t.Fatal(err)
					}
					if validations.Load() != 1 {
						t.Fatalf("valid response validations=%d", validations.Load())
					}
					if kind == "valid-text" && result.Content != "Fixture answer" {
						t.Fatalf("text=%q", result.Content)
					}
					if kind == "valid-tool" && (len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "text_echo") {
						t.Fatalf("tools=%+v", result.ToolCalls)
					}
				} else {
					if !errors.Is(err, llm.ErrEmptyCompletion) {
						t.Fatalf("error=%v", err)
					}
					var detail *llm.EmptyCompletionError
					if !errors.As(err, &detail) || detail.FinishReason() != "length" {
						t.Fatalf("detail=%#v", detail)
					}
					if result.FinishReason != "length" {
						t.Fatalf("selected finish reason lost: %q", result.FinishReason)
					}
					if validations.Load() != 0 {
						t.Fatalf("empty completion reached validator %d times", validations.Load())
					}
					// SDK-normalized report presence is not upstream measurement. The
					// error must leave its response accounting intact and unestimated.
					if result.Usage.Estimated || result.Cost.Estimated || result.Usage.TotalTokens != 0 || result.Cost.TotalCost != 0 {
						t.Fatalf("invented accounting: %+v %+v", result.Usage, result.Cost)
					}
				}
				if len(server.Requests()) != 1 {
					t.Fatalf("provider attempts=%d, want1", len(server.Requests()))
				}
				if server.Requests()[0].Stream != stream {
					t.Fatal("fixture did not exercise requested wire mode")
				}
			})
		}
	}
}

func TestE2E_EmptyCompletion_TaskFailurePersistsAndReplays(t *testing.T) {
	server := newScriptedLLMServer(t, emptyCompletionWireResponse(t, "", "fixture reasoning without answer"))
	cfg := phase83lConfig(t, server.URL())
	dsn := filepath.Join(t.TempDir(), "turns.sqlite")
	cfg.Sessions.Turns = config.TurnsConfig{Driver: "sqlite", DSN: dsn}
	var domainCalls atomic.Int64
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{PreRegisterTools: []tools.ToolDescriptor{{Tool: tools.Tool{Name: "fixture_read", ArgsSchema: json.RawMessage(`{"type":"object"}`)}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		domainCalls.Add(1)
		return tools.ToolResult{}, nil
	}}}})
	closed := false
	defer func() {
		if !closed {
			stack.Close()
		}
	}()
	id := identity.Identity{TenantID: devstack.DefaultDevTenant, UserID: devstack.DefaultDevUser, SessionID: devstack.DefaultDevSession}
	ctx, err := identity.With(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := stack.Bus.Subscribe(ctx, events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID, Types: []events.EventType{tasks.EventTypeTaskFailed, tasks.EventTypeTaskCompleted}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	task, err := stack.Tasks.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground, Query: "Read a synthetic fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if status := waitForTaskTerminal(t, stack, ctx, task.ID, 10*time.Second); status != tasks.StatusFailed {
		t.Fatalf("terminal=%s", status)
	}
	terminalCtx, stopTerminal := context.WithTimeout(t.Context(), 10*time.Second)
	defer stopTerminal()
	select {
	case event := <-sub.Events():
		payload, ok := event.Payload.(tasks.TaskFailedPayload)
		if event.Type != tasks.EventTypeTaskFailed || !ok || payload.TaskID != task.ID || payload.ErrorCode != planner.TaskErrorCodeRunLoopError {
			t.Fatalf("wrong emitted terminal event: %+v", event)
		}
	case <-terminalCtx.Done():
		t.Fatal("runtime did not emit task.failed")
	}
	stored, err := stack.Tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Error == nil || stored.Error.Code != planner.TaskErrorCodeRunLoopError || !strings.Contains(stored.Error.Message, "finish_reason=length") {
		t.Fatalf("task failure=%+v", stored.Error)
	}
	// Read the actual consumer turn endpoint until its persisted projection seals.
	body, err := json.Marshal(map[string]any{"session_id": id.SessionID, "task_id": string(task.ID)})
	if err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var before prototypes.SessionTurnsGetResponse
	for {
		request := httptest.NewRequest(http.MethodPost, "/v1/sessions/turns/get", bytes.NewReader(body)).WithContext(deadline)
		request.Header.Set("Authorization", "Bearer "+stack.Token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		stack.Handler.ServeHTTP(recorder, request)
		if recorder.Code == http.StatusOK {
			if err := json.Unmarshal(recorder.Body.Bytes(), &before); err != nil {
				t.Fatal(err)
			}
			if before.Turn != nil && before.Turn.Sealed {
				break
			}
		}
		select {
		case <-deadline.Done():
			t.Fatalf("turn did not seal: HTTP%d %s", recorder.Code, recorder.Body.String())
		case <-ticker.C:
		}
	}
	if before.Turn.Status != "failed" || !strings.Contains(before.Turn.ErrorMessage, "finish_reason=length") || before.Turn.FinishReason == "no_path" {
		t.Fatalf("consumer turn=%+v", before.Turn)
	}
	agentID := stack.AgentConfigID
	stack.Close()
	closed = true
	// A fresh projector/service reads the same SQLite turn after runtime shutdown.
	store, err := turnsqlite.New(turnsqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	projector, err := turns.New(store)
	if err != nil {
		_ = store.Close(t.Context())
		t.Fatal(err)
	}
	defer func() { _ = projector.Close(t.Context()) }()
	svc, err := turnprotocol.NewService(projector, turnprotocol.WithSessionReachAuthorizer(auth.NewSessionReachAuthorizer()), turnprotocol.WithAgentReachAuthorizer(auth.NewAgentReachAuthorizer()))
	if err != nil {
		t.Fatal(err)
	}
	verified, err := identity.WithVerified(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	after, err := svc.Get(auth.WithAgentReach(verified, []string{agentID}), turnprotocol.GetRequest{SessionID: id.SessionID, TaskID: string(task.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if !after.Turn.Sealed || after.Turn.Status != turns.StatusFailed || after.Turn.ErrorMessage != before.Turn.ErrorMessage {
		t.Fatalf("restart changed failure: %+v", after.Turn)
	}
	if after.Turn.Answer.Inline != "" {
		t.Fatalf("empty failure replayed as an answer: %+v", after.Turn.Answer)
	}
	if len(server.Requests()) != 1 {
		t.Fatalf("failure replay made provider attempts=%d", len(server.Requests()))
	}
	if domainCalls.Load() != 0 {
		t.Fatalf("empty completion dispatched %d domain handlers", domainCalls.Load())
	}
}
