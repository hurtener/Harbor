package materializer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// ---------------------------------------------------------------------------
// fakeSource — a test ProjectionSource over an in-memory sequence-ordered
// event list. It mirrors the durable driver's page semantics via the
// exported events.ProjectionPageFromSnapshot selection, and it can
// simulate the honest source states the materializer must handle:
// retention gaps (ring eviction), a missing substrate
// (ProjectionUnavailable), and source-level fence exclusion (the
// durable driver drops fenced sessions' events from pages).
// ---------------------------------------------------------------------------

type fakeSource struct {
	mu          sync.Mutex
	evs         []events.Event // sequence-ordered
	next        uint64
	evicted     bool // ring wrap: older events may be missing
	unavailable bool // no retained substrate
	skip        func(events.Event) bool
	pageCalls   int
	wake        chan<- uint64 // the registered wake sink (Run loop tests)
	// unsubscribed records that the last Watch handle's Unsubscribe ran
	// — the Run-loop cancellation test asserts the watcher is stopped.
	unsubscribed bool
}

func (f *fakeSource) publish(t *testing.T, ev events.Event) events.Event {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if ev.Sequence != 0 {
		t.Fatalf("fake source: caller pre-filled sequence %d", ev.Sequence)
	}
	f.next++
	ev.Sequence = f.next
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Unix(1_700_000_000, 0).Add(time.Duration(f.next) * time.Second)
	}
	f.evs = append(f.evs, ev)
	return ev
}

func (f *fakeSource) Page(_ context.Context, after uint64, limit int) (events.ProjectionPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pageCalls++
	if f.unavailable {
		return events.ProjectionPage{Next: after, Quality: events.ProjectionUnavailable}, nil
	}
	snapshot := append([]events.Event(nil), f.evs...)
	watermark := uint64(0)
	if len(f.evs) > 0 {
		watermark = f.evs[len(f.evs)-1].Sequence
	}
	return events.ProjectionPageFromSnapshot(snapshot, after, limit, watermark, f.evicted, f.skip), nil
}

func (f *fakeSource) Watermark(_ context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unavailable {
		return 0, events.ErrProjectionUnavailable
	}
	if len(f.evs) == 0 {
		return 0, nil
	}
	return f.evs[len(f.evs)-1].Sequence, nil
}

func (f *fakeSource) Watch(_ context.Context, wake chan<- uint64) (events.ProjectionWatch, error) {
	f.mu.Lock()
	if f.unavailable {
		f.mu.Unlock()
		return nil, events.ErrProjectionUnavailable
	}
	f.wake = wake
	var wm uint64
	if len(f.evs) > 0 {
		wm = f.evs[len(f.evs)-1].Sequence
	}
	f.mu.Unlock()
	// Mirror the real drivers: send the CURRENT watermark once after
	// registration (non-blocking) so a materializer that missed wakes
	// catches up immediately.
	if wm > 0 {
		select {
		case wake <- wm:
		default:
		}
	}
	return events.ProjectionWatchFunc(func() {
		f.mu.Lock()
		f.unsubscribed = true
		f.mu.Unlock()
	}), nil
}

// notify delivers the current watermark to the registered wake sink —
// the test-side analog of the drivers' post-persistence notification.
func (f *fakeSource) notify() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wake == nil || len(f.evs) == 0 {
		return
	}
	select {
	case f.wake <- f.evs[len(f.evs)-1].Sequence:
	default:
	}
}

func (f *fakeSource) pageCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pageCalls
}

func (f *fakeSource) wasUnsubscribed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unsubscribed
}

// ---------------------------------------------------------------------------
// canonical event builders
// ---------------------------------------------------------------------------

func testQuad(id identity.Identity, runID string) identity.Quadruple {
	return identity.Quadruple{Identity: id, RunID: runID}
}

func spawnEv(id identity.Identity, runID, taskID string, kind tasks.TaskKind, parent string) events.Event {
	return events.Event{
		Type:     tasks.EventTypeTaskSpawned,
		Identity: testQuad(id, runID),
		Payload: tasks.TaskSpawnedPayload{
			TaskID:       tasks.TaskID(taskID),
			Kind:         kind,
			ParentTaskID: tasks.TaskID(parent),
		},
	}
}

func startedEv(id identity.Identity, taskID string) events.Event {
	return events.Event{
		Type:     tasks.EventTypeTaskStarted,
		Identity: testQuad(id, ""),
		Payload:  tasks.TaskStartedPayload{TaskID: tasks.TaskID(taskID), PriorState: tasks.StatusPending},
	}
}

func completedEv(id identity.Identity, taskID string) events.Event {
	return events.Event{
		Type:     tasks.EventTypeTaskCompleted,
		Identity: testQuad(id, ""),
		Payload:  tasks.TaskCompletedPayload{TaskID: tasks.TaskID(taskID)},
	}
}

func failedEv(id identity.Identity, taskID, code string) events.Event {
	return events.Event{
		Type:     tasks.EventTypeTaskFailed,
		Identity: testQuad(id, ""),
		Payload:  tasks.TaskFailedPayload{TaskID: tasks.TaskID(taskID), ErrorCode: code},
	}
}

func cancelledEv(id identity.Identity, taskID string) events.Event {
	return events.Event{
		Type:     tasks.EventTypeTaskCancelled,
		Identity: testQuad(id, ""),
		Payload:  tasks.TaskCancelledPayload{TaskID: tasks.TaskID(taskID), Cascaded: false},
	}
}

func decisionEv(id identity.Identity, runID, decisionKind string) events.Event {
	return events.Event{
		Type:     planner.EventTypePlannerDecision,
		Identity: testQuad(id, runID),
		Payload: planner.DecisionPayload{
			Identity:       testQuad(id, runID),
			DecisionKind:   decisionKind,
			ReasoningChars: 0,
			ReasoningTrace: "raw provider thinking that must never reach a row",
		},
	}
}

func toolInvokedEv(id identity.Identity, runID, tool string, startedAt time.Time) events.Event {
	return events.Event{
		Type:     tools.EventTypeToolInvoked,
		Identity: testQuad(id, runID),
		Payload: tools.ToolInvokedPayload{
			Identity:  testQuad(id, runID),
			ToolName:  tool,
			Transport: tools.TransportInProcess,
			StartedAt: startedAt,
		},
	}
}

func toolCompletedEv(id identity.Identity, runID, tool string, durationMS int64) events.Event {
	return events.Event{
		Type:     tools.EventTypeToolCompleted,
		Identity: testQuad(id, runID),
		Payload: tools.ToolCompletedPayload{
			Identity:   testQuad(id, runID),
			ToolName:   tool,
			Transport:  tools.TransportInProcess,
			Attempts:   1,
			DurationMS: durationMS,
		},
	}
}

func toolPolicyExhaustedEv(id identity.Identity, runID, tool, lastClass string) events.Event {
	return events.Event{
		Type:     tools.EventTypeToolPolicyExhausted,
		Identity: testQuad(id, runID),
		Payload: tools.ToolPolicyExhaustedPayload{
			Identity:         testQuad(id, runID),
			ToolName:         tool,
			Transport:        tools.TransportInProcess,
			Attempts:         3,
			LastClass:        tools.ErrorClass(lastClass),
			ConfiguredBudget: 3,
			LastError:        "caller-controlled error text that must never reach a row",
		},
	}
}

func appAvailableEv(id identity.Identity, runID, agentID, serverID, uri string) events.Event {
	return events.Event{
		Type:     mcpdrv.EventTypeMCPAppAvailable,
		Identity: testQuad(id, runID),
		Payload: mcpdrv.AppAvailablePayload{
			Identity:       testQuad(id, runID),
			AgentID:        agentID,
			Binding:        "opaque callback capability that must never reach a row",
			ServerID:       tools.ToolSourceID(serverID),
			ToolCallID:     "",
			ToolName:       "ui",
			ResourceURI:    uri,
			DisplayMode:    "inline",
			RawHTMLTrusted: false,
		},
	}
}

func pauseRequestedEv(id identity.Identity, runID, reason string) events.Event {
	return events.Event{
		Type:     pauseresume.EventTypePauseRequested,
		Identity: testQuad(id, runID),
		Payload: pauseresume.PauseRequestedPayload{
			Token:  "opaque-pause-token-that-must-never-reach-a-row",
			Reason: reason,
		},
	}
}

func pauseResumedEv(id identity.Identity, runID string) events.Event {
	return events.Event{
		Type:     pauseresume.EventTypePauseResumed,
		Identity: testQuad(id, runID),
		Payload: pauseresume.PauseResumedPayload{
			Token:    "opaque-pause-token-that-must-never-reach-a-row",
			Reason:   string(planner.PauseApprovalRequired),
			Decision: pauseresume.DecisionResume,
		},
	}
}

func costRecordedEv(id identity.Identity, runID, model string, usage llm.Usage, totalCostUSD float64) events.Event {
	return events.Event{
		Type:     llm.EventTypeCostRecorded,
		Identity: testQuad(id, runID),
		Payload: llm.CostRecordedPayload{
			Identity: testQuad(id, runID),
			Model:    model,
			Cost:     llm.Cost{TotalCost: totalCostUSD, Currency: "USD"},
			Usage:    usage,
		},
	}
}

func inputDispositionEv(id identity.Identity, taskID, artifactID string) events.Event {
	return events.Event{
		Type:     runctx.EventTypeInputDispositionResolved,
		Identity: testQuad(id, ""),
		Payload: runctx.InputDispositionResolvedPayload{
			Identity:    testQuad(id, ""),
			TaskID:      taskID,
			ArtifactID:  artifactID,
			MIME:        "image/png",
			Disposition: "inline",
			Layer:       "runtime_default",
		},
	}
}

// redacted converts a typed payload to the durable source's rehydrated
// shape (events.RedactedMap over the JSON field map) — exactly what the
// durable driver's ProjectionSource delivers.
func redacted(t *testing.T, p events.EventPayload) events.RedactedMap {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("redact: marshal payload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("redact: unmarshal payload: %v", err)
	}
	return events.RedactedMap{Data: m}
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// harness wires a real SQLite turns.Store (the production durable
// driver), the projector, the fake source, and a materializer.
type harness struct {
	id         identity.Identity
	store      turns.Store
	proj       *turns.Projector
	src        *fakeSource
	closeStore func()
}

// newHarness builds a fresh :memory: harness (or a file-backed store
// when fileDSN is non-empty — for restart tests). The materializer is
// returned un-wired; the caller constructs it via newMaterializer so a
// restart test can rebuild it over the reopened store.
func newHarness(t *testing.T, fileDSN string) *harness {
	t.Helper()
	var store turns.Store
	var err error
	if fileDSN != "" {
		store, err = sqlite.New(sqlite.Config{DSN: fileDSN})
	} else {
		store, err = sqlite.New(sqlite.Config{DSN: ":memory:"})
	}
	if err != nil {
		t.Fatalf("open sqlite turns store: %v", err)
	}
	proj, err := turns.New(store)
	if err != nil {
		_ = store.Close(context.Background())
		t.Fatalf("new projector: %v", err)
	}
	return &harness{
		id:    identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-a"},
		store: store,
		proj:  proj,
		src:   &fakeSource{},
		closeStore: func() {
			_ = proj.Close(context.Background())
		},
	}
}

// newMaterializer wires a materializer over the harness's projector
// and source. Restart tests build a second one over the reopened store
// and the SAME fake source (the durable-log analog).
func (h *harness) newMaterializer(t *testing.T, opts ...Option) *Materializer {
	t.Helper()
	m, err := New(h.src, h.proj, opts...)
	if err != nil {
		t.Fatalf("new materializer: %v", err)
	}
	return m
}

// lifecycle publishes a full canonical lifecycle for one root
// foreground run: spawn → started → planner decision → tool dispatch →
// input attachment → app → usage → pause request → pause resume →
// terminal failure. Returns the published events (with sequences) so a
// test can reference the terminal sequence.
func (h *harness) lifecycle(t *testing.T, quad identity.Quadruple, taskID string) []events.Event {
	t.Helper()
	var evs []events.Event
	for _, ev := range []events.Event{
		spawnEv(quad.Identity, quad.RunID, taskID, tasks.KindForeground, ""),
		startedEv(quad.Identity, taskID),
		decisionEv(quad.Identity, quad.RunID, "CallTool"),
		toolInvokedEv(quad.Identity, quad.RunID, "clock.now", time.Unix(1_700_000_100, 0)),
		toolCompletedEv(quad.Identity, quad.RunID, "clock.now", 5),
		inputDispositionEv(quad.Identity, taskID, "art-1"),
		appAvailableEv(quad.Identity, quad.RunID, "agent-a", "server-1", "ui://doc"),
		costRecordedEv(quad.Identity, quad.RunID, "model-x", llm.Usage{
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, LatencyMS: 120,
		}, 0.25),
		pauseRequestedEv(quad.Identity, quad.RunID, string(planner.PauseApprovalRequired)),
		pauseResumedEv(quad.Identity, quad.RunID),
		failedEv(quad.Identity, taskID, "timeout"),
	} {
		evs = append(evs, h.src.publish(t, ev))
	}
	return evs
}

// eventually polls fn until it returns true or the bounded deadline
// passes — the real-time assertion helper (no time.Sleep as a
// synchronisation primitive).
func eventually(t *testing.T, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return fn()
}

func mustGetRow(t *testing.T, h *harness, turnID string) turns.TurnRow {
	t.Helper()
	row, err := h.proj.Get(context.Background(), h.id, turns.TurnID(turnID))
	if err != nil {
		t.Fatalf("get turn %s: %v", turnID, err)
	}
	return row
}

func wantErrIs(t *testing.T, err error, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}
