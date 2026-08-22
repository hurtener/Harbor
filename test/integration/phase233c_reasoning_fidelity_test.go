package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

var phase233cReasoningFragments = []string{
	"**Preparing to send email**",
	"\n\n",
	"I",
	" need",
	" to",
	" compose",
}

const (
	phase233cReasoning = "**Preparing to send email**\n\nI need to compose"
	phase233cAnswer    = "email composition completed"
)

// phase233cProvider is a hermetic OpenAI-compatible endpoint. Calls zero and
// one emit the exact reasoning fixture as independently encoded SSE JSON;
// call zero is the adapter-completion probe and call one drives the runtime's
// CallTool decision. Call two finishes the runtime task.
type phase233cProvider struct {
	t                  *testing.T
	server             *httptest.Server
	calls              atomic.Int64
	escapedNewlineWire atomic.Int64
}

func newPhase233cProvider(t *testing.T) *phase233cProvider {
	t.Helper()
	p := &phase233cProvider{t: t}
	p.server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.server.Close)
	return p
}

func (p *phase233cProvider) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.t.Errorf("phase233c provider read request: %v", err)
		http.Error(w, "read request", http.StatusInternalServerError)
		return
	}
	var request openAIRequestEnvelope
	if err := json.Unmarshal(body, &request); err != nil {
		p.t.Errorf("phase233c provider decode request: %v; body=%s", err, body)
		http.Error(w, "decode request", http.StatusBadRequest)
		return
	}
	if !request.Stream {
		p.t.Errorf("phase233c provider request stream=false; body=%s", body)
		http.Error(w, "stream required", http.StatusBadRequest)
		return
	}

	call := p.calls.Add(1) - 1
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		p.t.Error("phase233c provider response writer does not implement http.Flusher")
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}

	switch call {
	case 0:
		p.writeReasoningFrames(w, flusher, true, false)
		p.writeTerminalFrame(w, flusher, "stop")
	case 1:
		p.writeReasoningFrames(w, flusher, false, true)
		p.writeTerminalFrame(w, flusher, "tool_calls")
	case 2:
		p.writeFrame(w, flusher, map[string]any{"content": phase233cAnswer}, nil, nil)
		p.writeTerminalFrame(w, flusher, "stop")
	default:
		p.t.Errorf("phase233c provider script exhausted at call %d", call)
		http.Error(w, "script exhausted", http.StatusInternalServerError)
		return
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (p *phase233cProvider) writeReasoningFrames(w io.Writer, flusher http.Flusher, includeContent, includeToolCall bool) {
	for i, fragment := range phase233cReasoningFragments {
		delta := map[string]any{"reasoning": fragment}
		if includeContent && i == len(phase233cReasoningFragments)-1 {
			delta["content"] = "adapter completion"
		}
		if includeToolCall && i == 0 {
			delta["tool_calls"] = []map[string]any{{
				"index": 0,
				"id":    "phase233c-call",
				"type":  "function",
				"function": map[string]any{
					"name":      "text_echo",
					"arguments": `{"text":"phase233c"}`,
				},
			}}
		}
		p.writeFrame(w, flusher, delta, nil, nil)
	}
}

func (p *phase233cProvider) writeTerminalFrame(w io.Writer, flusher http.Flusher, finish string) {
	p.writeFrame(w, flusher, map[string]any{}, finish, map[string]any{
		"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20,
	})
}

func (p *phase233cProvider) writeFrame(w io.Writer, flusher http.Flusher, delta map[string]any, finish any, usage map[string]any) {
	frame := map[string]any{
		"id": "chatcmpl-phase233c", "object": "chat.completion.chunk",
		"created": 1700000000, "model": scriptedModel,
		"choices": []map[string]any{{
			"index": 0, "delta": delta, "finish_reason": finish,
		}},
	}
	if usage != nil {
		frame["usage"] = usage
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		p.t.Errorf("phase233c provider marshal frame: %v", err)
		return
	}
	if reasoning, ok := delta["reasoning"].(string); ok && reasoning == "\n\n" {
		if !bytes.Contains(raw, []byte(`"reasoning":"\n\n"`)) {
			p.t.Errorf("phase233c newline frame encoded as %q", raw)
			return
		}
		p.escapedNewlineWire.Add(1)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
	flusher.Flush()
}

func phase233cConfig(t *testing.T, providerURL string) *config.Config {
	t.Helper()
	cfg := phase83lConfig(t, providerURL)
	dsn := filepath.Join(t.TempDir(), "phase233c-state.sqlite")
	cfg.State = config.StateConfig{Driver: "sqlite", DSN: dsn}
	cfg.Events.Driver = "durable"
	// The fresh isolated StateStore starts without legacy sequence authority;
	// this restart fixture explicitly acknowledges the pre-start writer drain.
	cfg.Events.LegacyWritersDrained = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("phase233c config validate: %v", err)
	}
	return cfg
}

func phase233cIdentityContext(t *testing.T) (identity.Identity, context.Context) {
	t.Helper()
	id := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return id, ctx
}

func phase233cPost(t *testing.T, serverURL, path, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, serverURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new POST %s: %v", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read POST %s: %v", path, err)
	}
	return resp.StatusCode, raw
}

func phase233cPayloadString(payload any, names ...string) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	for _, name := range names {
		if value, ok := m[name].(string); ok {
			return value
		}
	}
	return ""
}

// TestE2E_Phase233c_ReasoningFidelity_DurableRestart proves that decoded
// provider reasoning bytes remain identical through live delivery, completed
// responses, planner decisions, live trajectory projections, and the durable
// restart oracle. Reopened tasks.get is intentionally not used because its
// trajectory enrichment is in-memory.
func TestE2E_Phase233c_ReasoningFidelity_DurableRestart(t *testing.T) {
	provider := newPhase233cProvider(t)
	cfg := phase233cConfig(t, provider.server.URL)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	server := httptest.NewServer(stack.Handler)
	firstStackClosed := false
	defer func() {
		if !firstStackClosed {
			server.Close()
			stack.Close()
		}
	}()

	id, idCtx := phase233cIdentityContext(t)
	probe := "adapter fidelity probe"
	var completedDeltas []string
	completedTerminals := 0
	completed, err := stack.LLMClient.Complete(idCtx, llm.CompleteRequest{
		Model: scriptedModel, Stream: true,
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &probe}}},
		OnReasoning: func(delta string, done bool) {
			if done {
				completedTerminals++
				return
			}
			completedDeltas = append(completedDeltas, delta)
		},
	})
	if err != nil {
		t.Fatalf("adapter completion: %v", err)
	}
	if completed.Reasoning != phase233cReasoning || strings.Join(completedDeltas, "") != phase233cReasoning {
		t.Fatalf("adapter reasoning transformed: completed=%q callbacks=%q", completed.Reasoning, strings.Join(completedDeltas, ""))
	}
	if completed.Content != "adapter completion" || completedTerminals != 1 {
		t.Fatalf("adapter completion content=%q reasoning terminals=%d", completed.Content, completedTerminals)
	}
	if len(completedDeltas) != len(phase233cReasoningFragments) || !bytes.Equal([]byte(completedDeltas[1]), []byte{0x0a, 0x0a}) {
		t.Fatalf("adapter decoded deltas=%q; middle bytes=%v", completedDeltas, []byte(completedDeltas[1]))
	}

	sub, err := stack.Bus.Subscribe(idCtx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{
			llm.EventTypeCompletionChunk,
			planner.EventTypePlannerDecision,
			tasks.EventTypeTaskCompleted,
			tasks.EventTypeTaskFailed,
		},
	})
	if err != nil {
		t.Fatalf("Bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	handle, err := stack.Tasks.Spawn(idCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id},
		Kind:     tasks.KindForeground,
		Query:    "compose a phase233c email",
	})
	if err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}
	runID := string(handle.ID)
	var liveDeltas []string
	liveTerminals := 0
	decisionTrace := ""
	decisionKind := ""
	decisionChars := 0
	taskCompleted := false
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for !taskCompleted {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("reasoning subscription closed before task completion")
			}
			if ev.Identity.TenantID != id.TenantID || ev.Identity.UserID != id.UserID || ev.Identity.SessionID != id.SessionID {
				t.Fatalf("event identity bleed: type=%s identity=%+v want=%+v", ev.Type, ev.Identity, id)
			}
			if (ev.Type == llm.EventTypeCompletionChunk || ev.Type == planner.EventTypePlannerDecision) && ev.Identity.RunID != runID {
				t.Fatalf("run identity bleed: type=%s run=%q want=%q", ev.Type, ev.Identity.RunID, runID)
			}
			switch ev.Type {
			case llm.EventTypeCompletionChunk:
				payload, ok := ev.Payload.(llm.CompletionChunkPayload)
				if !ok {
					t.Fatalf("completion chunk payload=%T", ev.Payload)
				}
				if payload.Kind != string(planner.ChunkReasoning) {
					continue
				}
				if payload.Done {
					liveTerminals++
				} else {
					liveDeltas = append(liveDeltas, payload.Delta)
				}
			case planner.EventTypePlannerDecision:
				payload, ok := ev.Payload.(planner.DecisionPayload)
				if !ok {
					t.Fatalf("planner decision payload=%T", ev.Payload)
				}
				if payload.ReasoningTrace != "" {
					decisionTrace = payload.ReasoningTrace
					decisionKind = payload.DecisionKind
					decisionChars = payload.ReasoningChars
				}
			case tasks.EventTypeTaskCompleted:
				payload, ok := ev.Payload.(tasks.TaskCompletedPayload)
				if !ok || payload.TaskID != handle.ID {
					t.Fatalf("task.completed payload=%#v", ev.Payload)
				}
				taskCompleted = true
			case tasks.EventTypeTaskFailed:
				t.Fatalf("runtime task failed: %#v", ev.Payload)
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for task completion; deltas=%q decision=%q", liveDeltas, decisionTrace)
		}
	}

	if strings.Join(liveDeltas, "") != phase233cReasoning || decisionTrace != phase233cReasoning || liveTerminals != 1 {
		t.Fatalf("live reasoning transformed: deltas=%q decision=%q terminals=%d", strings.Join(liveDeltas, ""), decisionTrace, liveTerminals)
	}
	if decisionKind != "CallTool" || decisionChars != len([]rune(phase233cReasoning)) {
		t.Fatalf("planner decision metadata: kind=%q chars=%d", decisionKind, decisionChars)
	}
	if len(liveDeltas) != len(phase233cReasoningFragments) || !bytes.Equal([]byte(liveDeltas[1]), []byte{0x0a, 0x0a}) {
		t.Fatalf("live decoded deltas=%q; middle bytes=%v", liveDeltas, []byte(liveDeltas[1]))
	}
	if provider.escapedNewlineWire.Load() != 2 {
		t.Fatalf("provider encoded newline fixture %d times, want 2", provider.escapedNewlineWire.Load())
	}
	if provider.calls.Load() != 3 {
		t.Fatalf("provider calls=%d, want scripted completion plus two runtime turns", provider.calls.Load())
	}

	task, err := stack.Tasks.Get(idCtx, handle.ID)
	if err != nil {
		t.Fatalf("get completed task: %v", err)
	}
	if task.Status != tasks.StatusComplete {
		t.Fatalf("completed task status=%v", task.Status)
	}
	if task.Result == nil {
		t.Fatal("completed task has no result")
	}
	if !bytes.Contains(task.Result.Value, []byte(phase233cAnswer)) {
		t.Fatalf("completed task response=%s", task.Result.Value)
	}
	trajectory := stack.RunLoopDriver.TrajectoryByTaskID(handle.ID)
	if trajectory == nil || len(trajectory.Steps) == 0 || trajectory.Steps[0].ReasoningTrace != phase233cReasoning {
		t.Fatalf("live task trajectory=%#v", trajectory)
	}

	status, raw := phase233cPost(t, server.URL, "/v1/tasks/get", fmt.Sprintf(`{"id":%q}`, handle.ID), stack.Token)
	if status != http.StatusOK {
		t.Fatalf("tasks.get status=%d body=%s", status, raw)
	}
	var detail prototypes.TaskDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("decode tasks.get: %v; body=%s", err, raw)
	}
	if detail.Trajectory == nil || len(detail.Trajectory.Steps) != 1 || detail.Trajectory.Steps[0].ReasoningTrace != phase233cReasoning {
		t.Fatalf("tasks.get trajectory=%#v", detail.Trajectory)
	}

	// Runtime restart: close every first-generation component, then assemble a
	// fresh stack over the same SQLite DSN. The durable event window—not
	// restarted tasks.get—is the reconstruction oracle.
	server.Close()
	stack.Close()
	firstStackClosed = true
	reopened := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer reopened.Close()
	reopenedServer := httptest.NewServer(reopened.Handler)
	defer reopenedServer.Close()

	historyBody := fmt.Sprintf(`{"session_id":%q,"limit":200}`, id.SessionID)
	status, raw = phase233cPost(t, reopenedServer.URL, "/v1/state/history", historyBody, reopened.Token)
	if status != http.StatusOK {
		t.Fatalf("reopened state.history status=%d body=%s", status, raw)
	}
	var history prototypes.StateHistoryResponse
	if err := json.Unmarshal(raw, &history); err != nil {
		t.Fatalf("decode reopened state.history: %v; body=%s", err, raw)
	}
	reconstructed := ""
	for _, ev := range history.Events {
		if ev.Type != string(planner.EventTypePlannerDecision) || ev.Run != runID {
			continue
		}
		if ev.Tenant != id.TenantID || ev.User != id.UserID || ev.Session != id.SessionID {
			t.Fatalf("reopened event identity=%s/%s/%s", ev.Tenant, ev.User, ev.Session)
		}
		trace := phase233cPayloadString(ev.Payload, "ReasoningTrace", "reasoning_trace")
		if trace != "" {
			reconstructed = trace
		}
	}
	if reconstructed != phase233cReasoning || strings.Contains(reconstructed, `\n\n`) || !bytes.Contains([]byte(reconstructed), []byte{0x0a, 0x0a}) {
		t.Fatalf("reopened reasoning transformed: %q bytes=%v", reconstructed, []byte(reconstructed))
	}

	for _, foreign := range []identity.Identity{
		{TenantID: "other-tenant", UserID: id.UserID, SessionID: id.SessionID},
		{TenantID: id.TenantID, UserID: "other-user", SessionID: id.SessionID},
		{TenantID: id.TenantID, UserID: id.UserID, SessionID: "other-session"},
	} {
		token := signPostureToken(t, reopened.SigningKey, foreign, nil)
		foreignBody := fmt.Sprintf(`{"session_id":%q,"limit":200}`, foreign.SessionID)
		foreignStatus, foreignRaw := phase233cPost(t, reopenedServer.URL, "/v1/state/history", foreignBody, token)
		if bytes.Contains(foreignRaw, []byte(phase233cReasoning)) || bytes.Contains(foreignRaw, []byte(runID)) {
			t.Fatalf("foreign identity %s/%s/%s exposed target data: status=%d body=%s", foreign.TenantID, foreign.UserID, foreign.SessionID, foreignStatus, foreignRaw)
		}
		switch foreignStatus {
		case http.StatusOK:
			var empty prototypes.StateHistoryResponse
			if err := json.Unmarshal(foreignRaw, &empty); err != nil {
				t.Fatalf("decode foreign empty history: %v; body=%s", err, foreignRaw)
			}
			if len(empty.Events) != 0 {
				t.Fatalf("foreign identity %s/%s/%s received %d events: %s", foreign.TenantID, foreign.UserID, foreign.SessionID, len(empty.Events), foreignRaw)
			}
		case http.StatusNotFound:
			var denied protoerrors.Error
			if err := json.Unmarshal(foreignRaw, &denied); err != nil || denied.Code != protoerrors.CodeNotFound {
				t.Fatalf("foreign denied response: err=%v code=%q body=%s", err, denied.Code, foreignRaw)
			}
		default:
			t.Fatalf("foreign identity %s/%s/%s status=%d, want empty 200 or denied 404; body=%s", foreign.TenantID, foreign.UserID, foreign.SessionID, foreignStatus, foreignRaw)
		}
	}
}
