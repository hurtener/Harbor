package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

const serverRetainedKind = state.InternalKindPrefix + "session-execution-context"

type retainedServerClient struct {
	mu     sync.Mutex
	calls  map[string]int
	bodies map[string]string
}

func (c *retainedServerClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		return llm.CompleteResponse{}, llm.ErrIdentityMissing
	}
	first := false
	var body strings.Builder
	for _, m := range req.Messages {
		if m.Content.Text == nil {
			continue
		}
		body.WriteString(*m.Content.Text)
		if m.Role == llm.RoleUser && strings.Contains(*m.Content.Text, "User goal: first root") {
			first = true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[q.RunID]++
	if c.calls[q.RunID] == 1 {
		c.bodies[q.RunID] = body.String()
	}
	if first && c.calls[q.RunID] == 1 {
		return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "read-one", Name: "retained_read", Args: json.RawMessage(`{}`)}}}, nil
	}
	return llm.CompleteResponse{Content: "Completed the requested work.", FinishReason: "stop"}, nil
}
func (*retainedServerClient) Close(context.Context) error { return nil }
func (c *retainedServerClient) body(id tasks.TaskID) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies[string(id)]
}

type retainedServerMemory struct {
	memory.MemoryStore
	calls atomic.Int64
}

func (m *retainedServerMemory) GetContext(context.Context, identity.Quadruple) (memory.LLMContextPatch, error) {
	m.calls.Add(1)
	return memory.LLMContextPatch{}, errors.New("legacy memory was consulted")
}
func (m *retainedServerMemory) AddTurn(context.Context, identity.Quadruple, memory.ConversationTurn) error {
	m.calls.Add(1)
	return errors.New("legacy memory write occurred")
}

func retainedServerHarness(t *testing.T, change func(*RunLoopDriverOptions)) (failDriverEnv, *retainedServerClient, *atomic.Int64, *retainedServerMemory) {
	t.Helper()
	env := newFailDriverEnv(t)
	store, err := state.Open(t.Context(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	redactor, err := audit.Open(t.Context(), config.AuditConfig{})
	if err != nil {
		t.Fatal(err)
	}
	client := &retainedServerClient{calls: map[string]int{}, bodies: map[string]string{}}
	cat := tools.NewCatalog()
	calls := &atomic.Int64{}
	receipt := json.RawMessage(`{"source":"` + strings.Repeat("x", 14585) + `","resource_id":"doc-a","version":9007199254740993,"more":false}`)
	if len(receipt) != 14660 {
		t.Fatal("incorrect receipt fixture")
	}
	if err = cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "retained_read", Description: "Read synthetic source", ArgsSchema: json.RawMessage(`{"type":"object"}`), Transport: tools.TransportInProcess, Source: "retained-fixture", Loading: tools.LoadingAlways}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		calls.Add(1)
		return tools.ToolResult{Value: receipt}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	mem := &retainedServerMemory{}
	startFailDriver(t, env, func(opts *RunLoopDriverOptions) {
		opts.StateStore, opts.Redactor = store, redactor
		opts.RetainedContextTurns, opts.RetainedContextTTL = 4, time.Hour
		opts.Memory, opts.Planner, opts.Catalog = mem, react.New(client), cat
		opts.Executor = dispatch.NewToolExecutor(cat, nil, env.reg)
		opts.DriveBackground = true
		if change != nil {
			change(opts)
		}
	})
	return env, client, calls, mem
}

// Await the real terminal event, not a timing delay. Subscription precedes Spawn
// so a fast completion cannot race test registration.
func retainedServerTurn(t *testing.T, env failDriverEnv, id identity.Identity, query string, parent *tasks.TaskID, inputIDs ...string) *tasks.Task {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	ctx, err := identity.With(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := env.bus.Subscribe(ctx, events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID, Types: []events.EventType{tasks.EventTypeTaskCompleted, tasks.EventTypeTaskFailed, tasks.EventTypeTaskCancelled}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	kind := tasks.KindForeground
	if parent != nil {
		kind = tasks.KindBackground
	}
	h, err := env.reg.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: kind, Query: query, ParentTaskID: parent, InputArtifactIDs: inputIDs})
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("terminal subscription ended")
			}
			var taskID tasks.TaskID
			switch payload := ev.Payload.(type) {
			case tasks.TaskCompletedPayload:
				taskID = payload.TaskID
			case tasks.TaskFailedPayload:
				taskID = payload.TaskID
			case tasks.TaskCancelledPayload:
				taskID = payload.TaskID
			}
			if taskID != h.ID {
				continue
			}
			result, err := env.reg.Get(ctx, h.ID)
			if err != nil {
				t.Fatal(err)
			}
			return result
		case <-ctx.Done():
			t.Fatal("task did not terminate")
		}
	}
}

func TestRetainedServer_ExactRootContextAndPrivateChild(t *testing.T) {
	env, client, calls, mem := retainedServerHarness(t, nil)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	first := retainedServerTurn(t, env, id, "first root", nil)
	if first.Status != tasks.StatusComplete {
		t.Fatalf("first root failed: %+v", first.Error)
	}
	second := retainedServerTurn(t, env, id, "second root", nil)
	if second.Status != tasks.StatusComplete {
		t.Fatalf("continuation failed: %+v", second.Error)
	}
	body := client.body(second.ID)
	for _, wanted := range []string{"doc-a", `"version":9007199254740993`, `"more":false`, strings.Repeat("x", 14585)} {
		if !strings.Contains(body, wanted) {
			t.Fatal("next actual request lost exact retained evidence")
		}
	}
	child := retainedServerTurn(t, env, id, "private-child-query", &first.ID)
	if child.Status != tasks.StatusComplete {
		t.Fatalf("child failed: %+v", child.Error)
	}
	if strings.Contains(client.body(child.ID), "doc-a") {
		t.Fatal("child imported root history")
	}
	third := retainedServerTurn(t, env, id, "third root", nil)
	if third.Status != tasks.StatusComplete {
		t.Fatalf("third root failed: %+v", third.Error)
	}
	if strings.Contains(client.body(third.ID), "private-child-query") {
		t.Fatal("child transcript entered root history")
	}
	if calls.Load() != 1 || mem.calls.Load() != 0 {
		t.Fatalf("replayed tools=%d legacy memory=%d", calls.Load(), mem.calls.Load())
	}
}

type retainedServerFailStore struct {
	state.StateStore
	remaining atomic.Int64
}

func (s *retainedServerFailStore) SaveIf(ctx context.Context, p []state.SlotExpectation, r state.StateRecord) error {
	if r.Kind == serverRetainedKind && s.remaining.Add(-1) == 0 {
		return errors.New("injected required state failure")
	}
	return s.StateStore.SaveIf(ctx, p, r)
}

func TestRetainedServer_RequiredPersistenceBeforeSuccess(t *testing.T) {
	for _, failAt := range []int64{1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			env, client, calls, _ := retainedServerHarness(t, func(opts *RunLoopDriverOptions) {
				s := &retainedServerFailStore{StateStore: opts.StateStore}
				s.remaining.Store(failAt)
				opts.StateStore = s
			})
			task := retainedServerTurn(t, env, identity.Identity{TenantID: "t", UserID: "u", SessionID: "failure"}, "first root", nil)
			if task.Status != tasks.StatusFailed {
				t.Fatal("required persistence failure reported success")
			}
			if calls.Load() != failAt-1 {
				t.Fatalf("admission/terminal order: calls=%d", calls.Load())
			}
			if failAt == 1 && client.body(task.ID) != "" {
				t.Fatal("inference ran before durable admission")
			}
		})
	}
}

func TestRetainedServer_ConcurrentReuse(t *testing.T) {
	env, client, calls, mem := retainedServerHarness(t, nil)
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := identity.Identity{TenantID: fmt.Sprintf("tenant-%d", i%2), UserID: fmt.Sprintf("user-%d", i%3), SessionID: fmt.Sprintf("session-%03d", i)}
			first := retainedServerTurn(t, env, id, "first root "+id.SessionID, nil)
			second := retainedServerTurn(t, env, id, "second root", nil)
			body := client.body(second.ID)
			if first.Status != tasks.StatusComplete || second.Status != tasks.StatusComplete || !strings.Contains(body, "doc-a") || !strings.Contains(body, id.SessionID) {
				t.Errorf("scope %d: first=%s (%+v), second=%s (%+v), source=%t, identity=%t",
					i, first.Status, first.Error, second.Status, second.Error,
					strings.Contains(body, "doc-a"), strings.Contains(body, id.SessionID))
			}
			for other := range 128 {
				if other != i && strings.Contains(body, fmt.Sprintf("session-%03d", other)) {
					t.Error("retained context crossed scope")
				}
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 128 || mem.calls.Load() != 0 {
		t.Fatalf("tools=%d memory=%d", calls.Load(), mem.calls.Load())
	}
}

func TestRetainedServer_ConstructorRejectsIncompleteConfiguration(t *testing.T) {
	env := newFailDriverEnv(t)
	store, err := state.Open(t.Context(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	redactor, err := audit.Open(t.Context(), config.AuditConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RunLoopDriverOptions){
		"negative":    func(o *RunLoopDriverOptions) { o.RetainedContextTurns = -1 },
		"too many":    func(o *RunLoopDriverOptions) { o.RetainedContextTurns = config.MaxRetainedContextTurns + 1 },
		"no state":    func(o *RunLoopDriverOptions) { o.StateStore = nil },
		"no redactor": func(o *RunLoopDriverOptions) { o.Redactor = nil },
		"no ttl":      func(o *RunLoopDriverOptions) { o.RetainedContextTTL = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			o := RunLoopDriverOptions{Bus: env.bus, Tasks: env.reg, RunLoop: env.rl,
				Planner:    &driverTestPlanner{finishGoalImmediately: true},
				StateStore: store, Redactor: redactor, RetainedContextTurns: 4, RetainedContextTTL: time.Hour}
			mutate(&o)
			if got, err := NewRunLoopDriver(o); got != nil || !errors.Is(err, ErrRunLoopDriverMisconfigured) {
				t.Fatalf("invalid retained configuration accepted: %v", err)
			}
		})
	}
}

// Exercise the ordinary production Boot configuration path, not just a manually
// assembled driver. Both modes use the real composed LLM client and task loop.
func TestRetainedServer_BootConfig(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			client := &retainedServerClient{calls: map[string]int{}, bodies: map[string]string{}}
			driver := "retained-boot-" + string(state.NewEventID())
			llm.Register(driver, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return client, nil })
			opts := baseOptions(t)
			if enabled {
				data := strings.Replace(serveTestYAML, "sessions:\n", "sessions:\n  retained_context_turns: 4\n", 1)
				if err := os.WriteFile(opts.ConfigPath, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			opts.BuildLLMSnapshot = func(*config.Config) (*llm.ConfigSnapshot, error) {
				return &llm.ConfigSnapshot{Driver: driver, Model: "fixture", HeavyOutputThreshold: 128 * 1024,
					ContextWindowReserve: .05, DisableCorrections: true, DisableDowngrade: true,
					DisableRetry: true, DisableGovernance: true,
					ModelProfiles: map[string]llm.ModelProfile{"fixture": {ContextWindowTokens: 32768}}}, nil
			}
			opts.RegisterCatalog = func(cat tools.ToolCatalog) error {
				return cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "retained_read", Description: "Read a synthetic document",
					ArgsSchema: json.RawMessage(`{"type":"object"}`), Transport: tools.TransportInProcess,
					Source: "boot-retained-fixture", Loading: tools.LoadingAlways},
					Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
						return tools.ToolResult{Value: json.RawMessage(`{"resource_id":"boot-doc","version":9007199254740993,"more":false}`)}, nil
					}})
			}
			var env failDriverEnv
			opts.PostBoot = func(_ context.Context, h PostBootHandles) error {
				env = failDriverEnv{bus: h.Bus, reg: h.Tasks}
				return nil
			}
			bootTest(t, t.Context(), opts)
			id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "boot"}
			for _, query := range []string{"first root", "second root"} {
				task := retainedServerTurn(t, env, id, query, nil)
				if task.Status != tasks.StatusComplete {
					t.Fatalf("boot task failed: %+v", task.Error)
				}
				if query == "second root" && strings.Contains(client.body(task.ID), "boot-doc") != enabled {
					t.Fatal("Boot did not apply the explicit retention configuration")
				}
			}
		})
	}
}

func (s *retainedServerFailStore) SaveBatchIf(ctx context.Context, p []state.SlotExpectation, writes []state.StateRecord) error {
	for _, record := range writes {
		if record.Kind == serverRetainedKind {
			if s.remaining.Add(-1) == 0 {
				return errors.New("injected required state failure")
			}
		}
	}
	return s.StateStore.SaveBatchIf(ctx, p, writes)
}
