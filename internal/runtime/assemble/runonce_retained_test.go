package assemble_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
)

type retainedRecordingClient struct {
	mu       sync.Mutex
	calls    map[string]int
	requests map[string][]llm.CompleteRequest
}

func (c *retainedRecordingClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		return llm.CompleteResponse{}, llm.ErrIdentityMissing
	}
	key := q.SessionID + "/" + q.RunID
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[key]++
	c.requests[key] = append(c.requests[key], req)
	if q.RunID == "first" && c.calls[key] == 1 {
		return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "read-one", Name: "retained_read", Args: json.RawMessage(`{}`)}}, FinishReason: "stop"}, nil
	}
	return llm.CompleteResponse{Content: "The work is complete.", FinishReason: "stop"}, nil
}
func (*retainedRecordingClient) Close(context.Context) error { return nil }
func (c *retainedRecordingClient) body(t *testing.T, key string) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	reqs := c.requests[key]
	if len(reqs) == 0 {
		t.Fatalf("no request for %s", key)
	}
	var result strings.Builder
	for _, msg := range reqs[0].Messages {
		if msg.Content.Text != nil {
			result.WriteString(*msg.Content.Text)
		}
	}
	return result.String()
}

func retainedRecordingStack(t *testing.T) (*assemble.Stack, *retainedRecordingClient, *atomic.Int64) {
	t.Helper()
	s := runnableStack(t)
	s.Cfg.Memory.Strategy = "rolling_summary"
	s.Cfg.Memory.RecentTurns = 4
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	c := &retainedRecordingClient{calls: map[string]int{}, requests: map[string][]llm.CompleteRequest{}}
	s.Planner = react.New(c)
	calls := &atomic.Int64{}
	receipt := json.RawMessage(`{"source":"` + strings.Repeat("x", 14585) + `","resource_id":"doc-a","version":9007199254740993,"more":false}`)
	if len(receipt) != 14660 {
		t.Fatalf("receipt bytes=%d", len(receipt))
	}
	if err := s.Catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "retained_read", Description: "Read the synthetic document", ArgsSchema: json.RawMessage(`{"type":"object"}`), Transport: tools.TransportInProcess, Source: "retained-test", Loading: tools.LoadingAlways}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		calls.Add(1)
		return tools.ToolResult{Value: receipt}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	return s, c, calls
}

type forbiddenRetainedMemory struct {
	memory.MemoryStore
	calls atomic.Int64
}

func (m *forbiddenRetainedMemory) Inspect(context.Context, identity.Quadruple) (memory.Inspection, error) {
	m.calls.Add(1)
	return memory.Inspection{}, errors.New("administrative memory must not be projected twice")
}
func (m *forbiddenRetainedMemory) Put(context.Context, identity.Quadruple, memory.ConversationTurn) (string, error) {
	m.calls.Add(1)
	return "", errors.New("execution must not write through the administrative note API")
}

func TestRunOnce_RetainedContextActualRequestAndHook(t *testing.T) {
	s, c, calls := retainedRecordingStack(t)
	sink := registerHookSink(t, s, "retained_sink")
	mem := &forbiddenRetainedMemory{}
	s.Memory = mem
	// The store is an injected test boundary; do not let Stack.Close call its
	// deliberately absent legacy implementation.
	defer func() { s.Memory = nil }()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "session"}
	for _, run := range []string{"first", "second"} {
		env, err := s.RunOnce(t.Context(), "continue the edit", id, assemble.WithRunID(run), assemble.WithCompletionHook(&steering.CompletionHookSpec{Tool: "retained_sink"}))
		if err != nil || env.FinishReason != string(planner.FinishGoal) {
			t.Fatalf("%s: %v", run, err)
		}
	}
	body := c.body(t, "session/second")
	for _, want := range []string{"doc-a", "9007199254740993", `"more":false`, strings.Repeat("x", 14585)} {
		if !strings.Contains(body, want) {
			t.Fatalf("next request lost %s", want[:min(len(want), 50)])
		}
	}
	if mem.calls.Load() != 0 {
		t.Fatal("legacy memory projected or written despite retained mode")
	}
	if calls.Load() != 1 {
		t.Fatal("historical read was executed again")
	}
	p, ok := sink.get("second")
	if !ok {
		t.Fatal("trusted completion hook did not run")
	}
	for _, entry := range p.Conversation {
		if entry.Kind == "tool" || strings.Contains(entry.Content, "doc-a") {
			t.Fatal("prior run was reingested through completion hook")
		}
	}
}

func TestRunOnce_RetainedContextDisabledAndInvalidConfig(t *testing.T) {
	s, c, _ := retainedRecordingStack(t)
	s.Cfg.Memory.Strategy = "none"
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "legacy"}
	for _, run := range []string{"first", "second"} {
		if _, err := s.RunOnce(t.Context(), "continue", id, assemble.WithRunID(run)); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(c.body(t, "legacy/second"), "doc-a") {
		t.Fatal("stateless path silently enabled retention")
	}
	if _, err := s.State.Load(t.Context(), identity.Quadruple{Identity: id}, state.InternalKindPrefix+"session-execution-context"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("stateless run wrote execution content: %v", err)
	}
	for _, n := range []int{-1} {
		s.Cfg.Memory.Strategy, s.Cfg.Memory.RecentTurns = "rolling_summary", n
		if _, err := s.RunOnce(t.Context(), "bad", id); err == nil {
			t.Fatal("bad retention bound accepted")
		}
	}
}

type failingRetainedWrite struct {
	state.StateStore
	failAt int
	calls  int
}

func (s *failingRetainedWrite) SaveIf(ctx context.Context, p []state.SlotExpectation, r state.StateRecord) error {
	if r.Kind == state.InternalKindPrefix+"session-execution-context" {
		s.calls++
		if s.calls == s.failAt {
			return errors.New("injected retention write failure")
		}
	}
	return s.StateStore.SaveIf(ctx, p, r)
}

func TestRunOnce_RetainedContextRequiredWrites(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			s, _, toolsCalled := retainedRecordingStack(t)
			s.Cfg.Memory.RecentTurns = 2
			store := s.State
			s.State = &failingRetainedWrite{StateStore: store, failAt: failAt}
			defer func() { s.State = store }()
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "failure"}
			_, err := s.RunOnce(t.Context(), "read", id, assemble.WithRunID("first"))
			if err == nil {
				t.Fatal("required persistence failure reported success")
			}
			if got := toolsCalled.Load(); got != int64(failAt-1) {
				t.Fatalf("tools called=%d; admission/terminal order wrong", got)
			}
		})
	}
}

func TestRunOnce_RetainedContextConcurrentReuse(t *testing.T) {
	s, c, calls := retainedRecordingStack(t)
	s.Cfg.Memory.RecentTurns = 2
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: fmt.Sprintf("isolation-%03d", i)}
			for _, run := range []string{"first", "second"} {
				if _, err := s.RunOnce(t.Context(), id.SessionID, id, assemble.WithRunID(run)); err != nil {
					t.Error(err)
					return
				}
			}
			if body := c.body(t, id.SessionID+"/second"); !strings.Contains(body, id.SessionID) || !strings.Contains(body, "doc-a") {
				t.Error("scoped history missing")
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 128 {
		t.Fatalf("historical actions repeated: %d", calls.Load())
	}
}

func TestRunOnce_RetainedContextConfigAndExplicitDisable(t *testing.T) {
	s, client, _ := retainedRecordingStack(t)
	s.Cfg.Memory.Strategy = "rolling_summary"
	s.Cfg.Memory.RecentTurns = 4
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "configured"}
	for _, run := range []string{"first", "second"} {
		if _, err := s.RunOnce(t.Context(), "continue", id, assemble.WithRunID(run)); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(client.body(t, "configured/second"), "doc-a") {
		t.Fatal("configured retention did not reach the next request")
	}
	s.Cfg.Memory.Strategy = "none"
	if _, err := s.RunOnce(t.Context(), "without history", id, assemble.WithRunID("disabled")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(client.body(t, "configured/disabled"), "doc-a") {
		t.Fatal("explicit none did not disable session memory")
	}
	s.Cfg.Memory.Strategy = "rolling_summary"
	if _, err := s.RunOnce(t.Context(), "continue", id, assemble.WithRunID("third")); err != nil {
		t.Fatal(err)
	}
	body := client.body(t, "configured/third")
	if !strings.Contains(body, "doc-a") || strings.Contains(body, "without history") {
		t.Fatal("disabled call erased retained history or persisted its own content")
	}
}

func (s *failingRetainedWrite) SaveBatchIf(ctx context.Context, p []state.SlotExpectation, writes []state.StateRecord) error {
	for _, record := range writes {
		if record.Kind == state.InternalKindPrefix+"session-execution-context" {
			s.calls++
			if s.calls == s.failAt {
				return errors.New("injected retention write failure")
			}
		}
	}
	return s.StateStore.SaveBatchIf(ctx, p, writes)
}
