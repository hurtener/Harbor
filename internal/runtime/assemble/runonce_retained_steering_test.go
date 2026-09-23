package assemble_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/steering"
)

func TestRunOnce_RetainedSteering_ReachesFollowingTurn(t *testing.T) {
	for _, control := range []steering.ControlType{steering.ControlUserMessage, steering.ControlRedirect, steering.ControlInjectContext} {
		t.Run(string(control), func(t *testing.T) {
			s, _, toolCalls := retainedRecordingStack(t)
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: string(control)}
			q := identity.Quadruple{Identity: id, RunID: "steered"}
			marker := "Preserve the approved sage navigation"
			payload := map[string]any{"message": marker}
			if control == steering.ControlRedirect {
				payload = map[string]any{"goal": marker}
			}
			if control == steering.ControlInjectContext {
				payload = map[string]any{"constraint": marker, "version": json.Number("9007199254740993")}
			}
			calls := 0
			s.Planner = react.New(resultClient{fn: func(req llm.CompleteRequest) (llm.CompleteResponse, error) {
				calls++
				if calls == 1 {
					in, err := s.Steering.Lookup(q)
					if err != nil {
						return llm.CompleteResponse{}, err
					}
					if err := in.Enqueue(steering.ControlEvent{Type: control, Identity: q, CallerScope: steering.ScopeAdmin, CallerTenant: id.TenantID, Payload: payload}); err != nil {
						return llm.CompleteResponse{}, err
					}
					return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "read", Name: "retained_read", Args: json.RawMessage(`{}`)}}}, nil
				}
				var text strings.Builder
				for _, msg := range req.Messages {
					if msg.Content.Text != nil {
						text.WriteString(*msg.Content.Text)
					}
				}
				if !strings.Contains(text.String(), marker) {
					t.Error("accepted correction absent from request")
				}
				if control == steering.ControlUserMessage && calls == 2 {
					// The first plan was superseded before dispatch. Execute a
					// fresh read so the following turn still proves no replay.
					if got := toolCalls.Load(); got != 0 {
						t.Fatalf("superseded read executed: %d calls", got)
					}
					return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "fresh-read", Name: "retained_read", Args: json.RawMessage(`{}`)}}}, nil
				}
				return llm.CompleteResponse{Content: "Complete."}, nil
			}})
			if _, err := s.RunOnce(t.Context(), "initial request", id, assemble.WithRunID(q.RunID)); err != nil {
				t.Fatal(err)
			}
			s.Planner = react.New(resultClient{fn: func(req llm.CompleteRequest) (llm.CompleteResponse, error) {
				var text strings.Builder
				for _, msg := range req.Messages {
					if msg.Content.Text == nil {
						continue
					}
					if msg.Role == llm.RoleSystem && strings.Contains(*msg.Content.Text, marker) {
						t.Error("historical correction promoted to system policy")
					}
					text.WriteString(*msg.Content.Text)
				}
				if !strings.Contains(text.String(), marker) {
					t.Error("retained continuation lost applied steering correction")
				}
				if control == steering.ControlInjectContext && !strings.Contains(text.String(), "9007199254740993") {
					t.Error("injected numeric evidence changed")
				}
				return llm.CompleteResponse{Content: "Continued."}, nil
			}})
			if _, err := s.RunOnce(context.Background(), "continue", id, assemble.WithRunID("next")); err != nil {
				t.Fatal(err)
			}
			if toolCalls.Load() != 1 {
				t.Fatal("historical operation replayed")
			}
		})
	}
}

type sharedSteeringClient struct {
	registry *steering.Registry
	mu       sync.Mutex
	calls    map[identity.Quadruple]int
	t        *testing.T
}

func (c *sharedSteeringClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		return llm.CompleteResponse{}, llm.ErrIdentityMissing
	}
	c.mu.Lock()
	c.calls[q]++
	n := c.calls[q]
	c.mu.Unlock()
	marker := "Keep navigation for " + q.SessionID
	if q.RunID == "steered" && n == 1 {
		in, err := c.registry.Lookup(q)
		if err != nil {
			return llm.CompleteResponse{}, err
		}
		if err = in.Enqueue(steering.ControlEvent{Type: steering.ControlUserMessage, Identity: q, CallerScope: steering.ScopeOwnerUser, CallerTenant: q.TenantID, Payload: map[string]any{"message": marker}}); err != nil {
			return llm.CompleteResponse{}, err
		}
		return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "read", Name: "retained_read", Args: json.RawMessage(`{}`)}}}, nil
	}
	if q.RunID == "steered" && n == 2 {
		found := false
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser && m.Content.Text != nil && *m.Content.Text == marker {
				found = true
			}
		}
		if !found {
			c.t.Error("fresh plan missing this session's correction")
		}
		return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "fresh-read", Name: "retained_read", Args: json.RawMessage(`{}`)}}}, nil
	}
	if q.RunID == "next" {
		count := 0
		for _, m := range req.Messages {
			if m.Content.Text != nil {
				count += strings.Count(*m.Content.Text, "Keep navigation for ")
				if strings.Contains(*m.Content.Text, "Keep navigation for ") && !strings.Contains(*m.Content.Text, marker) {
					c.t.Error("steering crossed session boundary")
				}
			}
		}
		if count != 1 {
			c.t.Errorf("session %s: retained correction count=%d", q.SessionID, count)
		}
	}
	return llm.CompleteResponse{Content: "Done"}, nil
}
func (*sharedSteeringClient) Close(context.Context) error { return nil }

func TestRunOnce_RetainedSteering_SharedStackIsolation(t *testing.T) {
	s, _, calls := retainedRecordingStack(t)
	s.Cfg.Memory.RecentTurns = 2
	client := &sharedSteeringClient{registry: s.Steering, t: t, calls: map[identity.Quadruple]int{}}
	s.Planner = react.New(client)
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: fmt.Sprintf("steering-%03d", i)}
			for _, run := range []string{"steered", "next"} {
				if _, err := s.RunOnce(t.Context(), "edit", id, assemble.WithRunID(run)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 128 {
		t.Fatal("historical reads executed again")
	}
	if s.Steering.Len() != 0 {
		t.Fatal("steering inbox leaked after completion")
	}
}
