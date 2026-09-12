package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// This is a restart/client-replay oracle, not just a prompt-string or
// in-memory trajectory test. Only the remote model is scripted: Bifrost,
// ReAct, dispatch, run-loop wiring, durable bus, SQLite and Protocol are real.
func TestE2E_ProgressReplay_SQLiteRestartAndSSECursor(t *testing.T) {
	updates := []string{"I will compare both documents.\n", "One discrepancy needs checking.\n"}
	reasoning := []string{"private-reasoning-one", "private-reasoning-two"}
	const final = "Here is the comparison."
	provider := newScriptedLLMServer(t,
		progressToolResponse(t, "progress-a", updates[0], reasoning[0]),
		progressToolResponse(t, "progress-b", updates[1], reasoning[1]),
		scriptedFinishResponse(final),
	)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	provider.beforeResponse = func(ctx context.Context, index int) bool {
		if index != 1 {
			return true
		}
		close(entered)
		select {
		case <-release:
			return true
		case <-ctx.Done():
			return false
		}
	}
	cfg := phase233cConfig(t, provider.URL()) // durable bus over a file-backed SQLite store
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()
	// Unblock before stack teardown, including assertion failures at the barrier.
	defer unblock()
	id, ctx := phase233cIdentityContext(t)
	filter := events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{llm.EventTypeCompletionChunk}}
	sub, err := stack.Bus.Subscribe(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	handle, err := stack.Tasks.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground,
		Query: "Compare both documents, check the discrepancy, then answer.",
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("second planner step never reached the barrier")
	}
	filter.Run = string(handle.ID)
	mid := progressReplay(t, stack.Bus, filter, 0)
	if got := progressContent(t, mid); !reflect.DeepEqual(got, updates[:1]) {
		t.Fatalf("completed first step not durable while task still running: %q", got)
	}
	if task, err := stack.Tasks.Get(ctx, handle.ID); err != nil || task.Status == tasks.StatusComplete {
		t.Fatalf("progress-only response prematurely finished task: task=%+v err=%v", task, err)
	}
	unblock()
	if status := waitForTaskTerminal(t, stack, ctx, handle.ID, 20*time.Second); status != tasks.StatusComplete {
		t.Fatalf("terminal status=%s, want complete", status)
	}
	wantContent := append(append([]string{}, updates...), final)
	retained := progressReplay(t, stack.Bus, filter, 0)
	if got := progressContent(t, retained); !reflect.DeepEqual(got, wantContent) {
		t.Fatalf("retained content=%q, want separate updates plus final %q", got, wantContent)
	}
	// Each of three responses has a content delta and a content terminator;
	// only the first two have reasoning deltas and reasoning terminators.
	if len(retained) != 10 {
		t.Fatalf("retained %d chunks, want 10 with separate content/reasoning boundaries", len(retained))
	}
	toolFilter := filter
	toolFilter.Types = []events.EventType{tools.EventTypeToolInvoked, tools.EventTypeToolCompleted}
	toolEvents := progressReplay(t, stack.Bus, toolFilter, 0)
	if len(toolEvents) != 4 {
		t.Fatalf("tool lifecycle events=%d, want two invoked/completed pairs", len(toolEvents))
	}
	for i := range updates {
		invoked, completed := toolEvents[2*i], toolEvents[2*i+1]
		if invoked.Type != tools.EventTypeToolInvoked || completed.Type != tools.EventTypeToolCompleted ||
			retained[4*i+3].Sequence >= invoked.Sequence || invoked.Sequence >= completed.Sequence ||
			completed.Sequence >= retained[4*i+4].Sequence {
			t.Fatalf("step %d replay is not update -> tool -> next response", i)
		}
	}
	var live []events.Event
	for len(live) < len(retained) {
		select {
		case ev, open := <-sub.Events():
			if !open {
				t.Fatal("live subscription closed early")
			}
			if ev.Sequence != 0 {
				t.Fatalf("durable flush duplicated live fanout: seq=%d", ev.Sequence)
			}
			live = append(live, ev)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d live chunks for %d retained chunks", len(live), len(retained))
		}
	}
	if got := progressContent(t, live); !reflect.DeepEqual(got, wantContent) {
		t.Fatalf("live content=%q, want %q", got, wantContent)
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("extra live chunk after terminal completion: %+v", ev)
	default:
	}
	task, err := stack.Tasks.Get(ctx, handle.ID)
	if err != nil || task.Result == nil {
		t.Fatalf("final task result missing: err=%v", err)
	}
	var answer planner.AnswerEnvelope
	if err := json.Unmarshal(task.Result.Value, &answer); err != nil || answer.Answer != final {
		t.Fatalf("preambles contaminated final AnswerEnvelope: %s (err=%v)", task.Result.Value, err)
	}
	traj := stack.RunLoopDriver.TrajectoryByTaskID(handle.ID)
	if traj == nil || len(traj.Steps) != 2 {
		t.Fatalf("trajectory=%+v, want two tool steps", traj)
	}
	for i, step := range traj.Steps {
		if step.AssistantPreamble != updates[i] || step.ReasoningTrace != reasoning[i] {
			t.Fatalf("step %d lost content/reasoning separation: %+v", i, step)
		}
	}
	requests := provider.Requests()
	if len(requests) != 3 {
		t.Fatalf("provider calls=%d, want 3", len(requests))
	}
	for i := 1; i < len(requests); i++ {
		var preambles []string
		for _, msg := range requests[i].Messages {
			if strings.Contains(msg.Content, "private-reasoning-") {
				t.Fatal("private reasoning leaked into ordinary model content")
			}
			if msg.Role == "assistant" && len(msg.ToolCalls) > 2 {
				preambles = append(preambles, msg.Content)
			}
		}
		if !reflect.DeepEqual(preambles, updates[:i]) {
			t.Fatalf("model-history preambles=%q, want %q", preambles, updates[:i])
		}
	}

	// Tear down the WHOLE stack (including its SQLite handle), not merely a
	// new bus over an old in-memory store. Reassembly cannot use old trajectory
	// enrichment to fake recovery. No further model request is permitted.
	sub.Cancel()
	stack.Close()
	reopened := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer reopened.Close()
	replayed := progressReplay(t, reopened.Bus, filter, 0)
	if len(replayed) != len(retained) {
		t.Fatalf("restart has %d chunks, want %d", len(replayed), len(retained))
	}
	for i, ev := range replayed {
		if ev.Sequence != retained[i].Sequence || !reflect.DeepEqual(progressPayload(t, ev), progressPayload(t, retained[i])) {
			t.Fatalf("restart changed chunk %d", i)
		}
	}
	if suffix := progressReplay(t, reopened.Bus, filter, retained[2].Sequence); !reflect.DeepEqual(suffix, replayed[3:]) {
		t.Fatal("durable cursor replay repeated or dropped a retained frame")
	}
	httpServer := httptest.NewServer(reopened.Handler)
	defer httpServer.Close()
	progressSSE(t, httpServer.URL, reopened.Token, filter, 0, retained)
	// Resume in the middle of a persisted step: cursor IDs must not repeat and
	// every remaining boundary must survive, including reasoning-only frames.
	progressSSE(t, httpServer.URL, reopened.Token, filter, retained[2].Sequence, retained[3:])
	body := fmt.Sprintf(`{"session_id":%q,"limit":200}`, id.SessionID)
	status, raw := phase233cPost(t, httpServer.URL, "/v1/state/history", body, reopened.Token)
	if status != http.StatusOK {
		t.Fatalf("history status=%d body=%s", status, raw)
	}
	var history prototypes.StateHistoryResponse
	if err := json.Unmarshal(raw, &history); err != nil {
		t.Fatal(err)
	}
	var historyContent []string
	for _, ev := range history.Events {
		if ev.Type == string(llm.EventTypeCompletionChunk) && ev.Run == filter.Run && phase233cPayloadString(ev.Payload, "Kind") == "content" {
			if delta := phase233cPayloadString(ev.Payload, "Delta"); delta != "" {
				historyContent = append(historyContent, delta)
			}
		}
	}
	if !reflect.DeepEqual(historyContent, wantContent) {
		t.Fatalf("history content=%q, want %q", historyContent, wantContent)
	}
	for _, foreign := range []identity.Identity{
		{TenantID: "other-tenant", UserID: id.UserID, SessionID: id.SessionID},
		{TenantID: id.TenantID, UserID: "other-user", SessionID: id.SessionID},
		{TenantID: id.TenantID, UserID: id.UserID, SessionID: "other-session"},
	} {
		token := signPostureToken(t, reopened.SigningKey, foreign, nil)
		foreignBody := fmt.Sprintf(`{"session_id":%q,"limit":200}`, foreign.SessionID)
		code, raw := phase233cPost(t, httpServer.URL, "/v1/state/history", foreignBody, token)
		if strings.Contains(string(raw), filter.Run) || strings.Contains(string(raw), "private-reasoning-") {
			t.Fatalf("foreign history exposed this run: status=%d body=%s", code, raw)
		}
		switch code {
		case http.StatusOK:
			var empty prototypes.StateHistoryResponse
			if err := json.Unmarshal(raw, &empty); err != nil || len(empty.Events) != 0 {
				t.Fatalf("foreign history not empty: err=%v body=%s", err, raw)
			}
		case http.StatusNotFound:
			var denied protoerrors.Error
			if err := json.Unmarshal(raw, &denied); err != nil || denied.Code != protoerrors.CodeNotFound {
				t.Fatalf("foreign history not canonically denied: err=%v body=%s", err, raw)
			}
		default:
			t.Fatalf("foreign history status=%d, want empty 200 or denied 404: %s", code, raw)
		}
	}
	// Same IDs in a different tenant/user/session or run cannot replay this task.
	for _, foreign := range []events.Filter{
		{Tenant: "other", User: filter.User, Session: filter.Session},
		{Tenant: filter.Tenant, User: "other", Session: filter.Session},
		{Tenant: filter.Tenant, User: filter.User, Session: "other"},
		{Tenant: filter.Tenant, User: filter.User, Session: filter.Session, Run: "other"},
	} {
		foreign.Types = filter.Types
		if got := progressReplay(t, reopened.Bus, foreign, 0); len(got) != 0 {
			t.Fatalf("foreign scope replayed %d chunks", len(got))
		}
	}
	if len(provider.Requests()) != 3 {
		t.Fatal("reopen unexpectedly re-ran the model")
	}
}

func progressToolResponse(t *testing.T, callID, content, reasoning string) string {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal([]byte(scriptedToolCallResponse(callID, "text_echo", `{"text":"document checked"}`)), &response); err != nil {
		t.Fatal(err)
	}
	msg := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	msg["content"], msg["reasoning"] = content, reasoning
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func progressReplay(t *testing.T, bus events.EventBus, filter events.Filter, after uint64) []events.Event {
	t.Helper()
	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatal("bus has no replay capability")
	}
	got, err := rp.Replay(context.Background(), events.Cursor{SessionID: filter.Session, Sequence: after}, filter)
	if err != nil {
		t.Fatal(err)
	}
	last := after
	for _, ev := range got {
		if ev.Sequence <= last || !filter.Matches(ev) {
			t.Fatalf("bad replay cursor/identity: %+v", ev)
		}
		last = ev.Sequence
	}
	return got
}

func progressPayload(t *testing.T, ev events.Event) llm.CompletionChunkPayload {
	t.Helper()
	if p, ok := ev.Payload.(llm.CompletionChunkPayload); ok {
		return p
	}
	p, ok := ev.Payload.(events.RedactedMap)
	if !ok {
		t.Fatalf("unexpected replay payload %T", ev.Payload)
	}
	raw, err := json.Marshal(p.Data)
	if err != nil {
		t.Fatal(err)
	}
	var payload llm.CompletionChunkPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func progressContent(t *testing.T, frames []events.Event) []string {
	t.Helper()
	var out []string
	var text strings.Builder
	for _, ev := range frames {
		p := progressPayload(t, ev)
		if p.Kind != "content" {
			continue
		}
		text.WriteString(p.Delta)
		if p.Done {
			out = append(out, text.String())
			text.Reset()
		}
	}
	if text.Len() != 0 {
		t.Fatal("content response has no done boundary")
	}
	return out
}

func progressSSE(t *testing.T, url, token string, filter events.Filter, after uint64, want []events.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Harbor-Session", filter.Session)
	req.Header.Set("X-Harbor-Run", filter.Run)
	req.Header.Set("X-Harbor-Event-Type", string(llm.EventTypeCompletionChunk))
	req.Header.Set("Last-Event-ID", strconv.FormatUint(after, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status=%d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var cursor string
	i := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "stream.replay_unavailable") {
			t.Fatalf("replay gap: %s", line)
		}
		if strings.HasPrefix(line, "id:") {
			cursor = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var wire struct {
			Type                       string `json:"type"`
			Sequence                   uint64 `json:"sequence"`
			Tenant, User, Session, Run string
			Payload                    llm.CompletionChunkPayload `json:"payload"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &wire); err != nil {
			t.Fatal(err)
		}
		if i >= len(want) {
			t.Fatal("extra SSE replay frame")
		}
		if wire.Type != string(llm.EventTypeCompletionChunk) || wire.Sequence != want[i].Sequence || cursor != strconv.FormatUint(want[i].Sequence, 10) ||
			wire.Tenant != filter.Tenant || wire.User != filter.User || wire.Session != filter.Session || wire.Run != filter.Run {
			t.Fatalf("SSE frame %d has wrong cursor, type or identity: %s", i, line)
		}
		if !reflect.DeepEqual(wire.Payload, progressPayload(t, want[i])) {
			t.Fatalf("SSE payload %d changed: %s", i, line)
		}
		i++
		if i == len(want) {
			return
		}
	}
	t.Fatalf("SSE replay stopped at %d/%d: %v", i, len(want), scanner.Err())
}

// Boundary durability is deliberately weaker than token-by-token durability.
// Losing the publisher's process-local buffer must not fabricate a committed
// update or final answer after reopening the backing database.
func TestE2E_ProgressReplay_UnflushedTailIsProvisional(t *testing.T) {
	provider := newScriptedLLMServer(t)
	cfg := phase233cConfig(t, provider.URL())
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()
	id, ctx := phase233cIdentityContext(t)
	q := identity.Quadruple{Identity: id, RunID: "provisional-progress"}
	publisher := llm.NewBufferedChunkPublisherContext(ctx, stack.Bus, q, q.RunID, nil)
	publisher.OnChunk("Committed update.", true, "content")
	if err := publisher.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	publisher.OnChunk("Uncommitted tail.", false, "content")
	filter := events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Run: q.RunID, Types: []events.EventType{llm.EventTypeCompletionChunk}}
	if got := progressReplay(t, stack.Bus, filter, 0); len(got) != 1 {
		t.Fatalf("before restart retained %d chunks, want one committed chunk", len(got))
	}
	// Intentionally discard the publisher without Flush/Seal. Closing the
	// database is not being claimed as a literal SIGKILL/power-loss test.
	stack.Close()
	reopened := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer reopened.Close()
	got := progressContent(t, progressReplay(t, reopened.Bus, filter, 0))
	if !reflect.DeepEqual(got, []string{"Committed update."}) {
		t.Fatalf("restart invented an unflushed update: %q", got)
	}
	if len(provider.Requests()) != 0 {
		t.Fatal("replay called the model")
	}
}
