package projection

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/protocol/client"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

type fixture struct {
	Name                   string                   `json:"name"`
	Identity               types.IdentityScope      `json:"identity"`
	Generation             uint64                   `json:"generation"`
	CapturedSequence       uint64                   `json:"captured_sequence"`
	HistoryTruncated       bool                     `json:"history_truncated"`
	HistoryHasMore         bool                     `json:"history_has_more"`
	SessionStatus          types.SessionStatus      `json:"session_status"`
	CountersPartial        bool                     `json:"counters_partial"`
	AggregateTruncated     bool                     `json:"aggregate_truncated"`
	ToolsAggregatesPartial bool                     `json:"tools_aggregates_partial"`
	ToolAnalyticsBounded   bool                     `json:"tool_analytics_bounded"`
	Unavailable            []string                 `json:"unavailable_capabilities"`
	Events                 []types.StateEvent       `json:"events"`
	Tasks                  []types.TaskRow          `json:"tasks"`
	TaskDetails            []types.TaskDetail       `json:"task_details"`
	Pauses                 []types.PauseSnapshot    `json:"pauses"`
	ExpectedConversation   []normalizedConversation `json:"expected_conversation"`
	Expected               normalizedProjection     `json:"expected"`
}

type normalizedConversation struct {
	RunID     string           `json:"run_id"`
	Answer    string           `json:"answer"`
	Reasoning string           `json:"reasoning"`
	Terminal  bool             `json:"terminal"`
	Tools     []normalizedTool `json:"tools"`
}

type normalizedTool struct {
	Tool   string `json:"tool"`
	Status string `json:"status"`
}

type normalizedProjection struct {
	SessionStatus          string            `json:"session_status"`
	HistoryTruncated       bool              `json:"history_truncated"`
	HistoryHasMore         bool              `json:"history_has_more"`
	AggregateTruncated     bool              `json:"aggregate_truncated"`
	CountersPartial        bool              `json:"counters_partial"`
	ToolsAggregatesPartial bool              `json:"tools_aggregates_partial"`
	ToolAnalyticsBounded   bool              `json:"tool_analytics_bounded"`
	Unavailable            []string          `json:"unavailable_capabilities"`
	Blocks                 []normalizedBlock `json:"blocks"`
}

type normalizedBlock struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	RunID       string   `json:"run_id"`
	Status      string   `json:"status"`
	Text        string   `json:"text,omitempty"`
	Tool        string   `json:"tool,omitempty"`
	EventType   string   `json:"event_type,omitempty"`
	PayloadKeys []string `json:"payload_keys,omitempty"`
	ArtifactIDs []string `json:"artifact_ids,omitempty"`
	Incomplete  bool     `json:"incomplete"`
}

func TestReducer_CapturedCanonicalCorpus_NormalizesDeterministically(t *testing.T) {
	for _, tc := range loadFixtures(t) {
		t.Run(tc.Name, func(t *testing.T) {
			got, err := (&Reducer{}).Hydrate(fixtureBundle(tc))
			if err != nil {
				t.Fatal(err)
			}
			if normalized := normalizeFixture(got); jsonValue(normalized) != jsonValue(tc.Expected) {
				t.Fatalf("normalized mismatch\n got: %s\nwant: %s", jsonValue(normalized), jsonValue(tc.Expected))
			}
			if tc.ExpectedConversation != nil && jsonValue(normalizeConversation(got)) != jsonValue(tc.ExpectedConversation) {
				t.Fatalf("conversation mismatch\n got: %s\nwant: %s", jsonValue(normalizeConversation(got)), jsonValue(tc.ExpectedConversation))
			}
			first, _ := Normalize(got)
			second, _ := Normalize(got)
			if !bytes.Equal(first, second) {
				t.Fatal("Normalize is not byte-stable")
			}
		})
	}
}

// TestReducer_CostRecorded_AccumulatesCacheTokens pins that the
// llm.cost.recorded reducer folds the provider's cache-token counts into
// both the session-level Usage and the per-run RunUsage, without disturbing
// the existing token/cost totals.
func TestReducer_CostRecorded_AccumulatesCacheTokens(t *testing.T) {
	id := testIdentity("cache")
	r := &Reducer{}
	p, err := r.Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"Model": "openai/gpt-5.4",
		"Usage": map[string]any{
			"TotalTokens":      1200,
			"PromptTokens":     1000,
			"CompletionTokens": 200,
			"CacheReadTokens":  800,
			"CacheWriteTokens": 150,
		},
		"Cost": map[string]any{"TotalCost": 0.01},
	}
	p, _, _ = r.Apply(p, wireEvent(id, 1, "llm.cost.recorded", "run-a", payload))
	// A second event on the same run accumulates.
	p, _, _ = r.Apply(p, wireEvent(id, 2, "llm.cost.recorded", "run-a", payload))

	if p.Usage.CacheReadTokens != 1600 {
		t.Errorf("session Usage.CacheReadTokens = %d want 1600", p.Usage.CacheReadTokens)
	}
	if p.Usage.CacheWriteTokens != 300 {
		t.Errorf("session Usage.CacheWriteTokens = %d want 300", p.Usage.CacheWriteTokens)
	}
	if p.Usage.TotalTokens != 2400 || p.Usage.PromptTokens != 2000 {
		t.Errorf("base session totals disturbed: %+v", p.Usage)
	}
	run := p.RunUsage["run-a"]
	if run.CacheReadTokens != 1600 || run.CacheWriteTokens != 300 {
		t.Errorf("RunUsage cache counts = read %d write %d want 1600/300", run.CacheReadTokens, run.CacheWriteTokens)
	}
}

// TestReducer_CostRecorded_AbsentCacheFieldsDecodeToZero pins zero-value
// honesty: an older-shaped cost payload with no cache fields decodes cleanly
// (not a decode failure) and yields zero cache counts.
func TestReducer_CostRecorded_AbsentCacheFieldsDecodeToZero(t *testing.T) {
	id := testIdentity("nocache")
	r := &Reducer{}
	p, err := r.Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"Model": "openai/gpt-5.4",
		"Usage": map[string]any{
			"TotalTokens":      600,
			"PromptTokens":     500,
			"CompletionTokens": 100,
		},
		"Cost": map[string]any{"TotalCost": 0.005},
	}
	next, change, _ := r.Apply(p, wireEvent(id, 1, "llm.cost.recorded", "run-b", payload))
	if !change.Changed {
		t.Fatal("cost payload with no cache fields must still decode and change state, not fall through to generic")
	}
	if next.Usage.CacheReadTokens != 0 || next.Usage.CacheWriteTokens != 0 {
		t.Errorf("absent cache fields must decode to zero, got read %d write %d", next.Usage.CacheReadTokens, next.Usage.CacheWriteTokens)
	}
	if next.Usage.TotalTokens != 600 {
		t.Errorf("base totals broken on cache-absent decode: %+v", next.Usage)
	}
}

func TestReconcile_AuthoritativeSnapshotRepairsRemovalAndPreservesOnlyNewerLive(t *testing.T) {
	id := testIdentity("session")
	r := &Reducer{}
	initial, err := r.Hydrate(SnapshotBundle{
		Generation: 1, CapturedSequence: 10, Identity: id,
		Tasks: types.TaskListResponse{Rows: []types.TaskRow{
			taskRow(id, "snapshot-away", "remove me", types.TaskStatusRunning),
			taskRow(id, "live", "keep me", types.TaskStatusRunning),
		}},
		Pauses:  types.PauseListResponse{Snapshots: []types.PauseSnapshot{pauseSnapshot(id, "stale-pause")}},
		Session: sessionSnapshot(id, types.SessionStatusRunning),
	})
	if err != nil {
		t.Fatal(err)
	}
	initial, _, _ = r.Apply(initial, wireEvent(id, 11, "llm.completion.chunk", "live", map[string]any{"TaskID": "live", "Delta": "newer", "Kind": "content"}))
	initial, _, _ = r.Apply(initial, wireEvent(id, 12, "task.completed", "live", map[string]any{"TaskID": "live"}))

	next, change, err := Reconcile(initial, SnapshotBundle{
		Generation: 2, CapturedSequence: 10, Identity: id,
		Tasks:   types.TaskListResponse{Rows: []types.TaskRow{taskRow(id, "live", "keep me", types.TaskStatusRunning)}},
		Session: sessionSnapshot(id, types.SessionStatusRunning),
	})
	if err != nil || !change.Immediate {
		t.Fatalf("reconcile: %#v %v", change, err)
	}
	if blockIndex(next.Blocks, "user:snapshot-away") >= 0 || blockIndex(next.Blocks, "task:snapshot-away") >= 0 || blockIndex(next.Blocks, "intervention:stale-pause") >= 0 {
		t.Fatalf("stale snapshot-owned state survived: %#v", next.Blocks)
	}
	if blockIndex(next.Blocks, "text:live:0") < 0 || next.Blocks[blockIndex(next.Blocks, "task:live")].Status != "completed" {
		t.Fatalf("newer live mutations lost: %#v", next.Blocks)
	}
	refreshed, _, err := Reconcile(next, SnapshotBundle{Generation: 3, CapturedSequence: 12, Identity: id, Session: sessionSnapshot(id, types.SessionStatusCompleted)})
	if err != nil || len(refreshed.Blocks) != 0 || refreshed.SessionStatus != "completed" {
		t.Fatalf("captured snapshot did not become authoritative: %#v %v", refreshed, err)
	}
}

func TestReducer_OutOfOrder_AllArrivalPermutationsForceReconcileAndReplayDeterministically(t *testing.T) {
	id := testIdentity("permutations")
	permutations := [][]int{{1, 2, 3}, {1, 3, 2}, {2, 1, 3}, {2, 3, 1}, {3, 1, 2}, {3, 2, 1}}
	for _, order := range permutations {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			p, _ := (&Reducer{}).Hydrate(SnapshotBundle{Generation: 1, Identity: id})
			for _, sequence := range order {
				var change ChangeSet
				p, change, _ = (&Reducer{}).Apply(p, wireEvent(id, uint64(sequence), "llm.completion.chunk", "run", map[string]any{"TaskID": "run", "Delta": string(rune('a' + sequence - 1)), "Kind": "content"}))
				if sequence < int(p.LastSequence) && !change.ReconciliationRequired && !p.ReconciliationRequired {
					t.Fatal("unseen out-of-order event was silent")
				}
			}
			if slices.Equal(order, []int{1, 2, 3}) {
				if p.ReconciliationRequired || p.Blocks[0].Text != "abc" {
					t.Fatalf("ordered projection = %#v", p)
				}
				return
			}
			if !p.ReconciliationRequired || p.ReplayGap == nil {
				t.Fatalf("gap not explicit: %#v", p)
			}
			reconciled, _, err := Reconcile(p, SnapshotBundle{Generation: 2, Identity: id})
			if err != nil || reconciled.ReconciliationRequired || reconciled.Blocks[0].Text != "abc" {
				t.Fatalf("replayed projection = %#v, %v", reconciled, err)
			}
		})
	}
}

func TestReducer_SessionCloseReopenCloseAndSnapshotRefresh(t *testing.T) {
	id := testIdentity("lifecycle")
	p, _ := (&Reducer{}).Hydrate(SnapshotBundle{Generation: 1, Identity: id, Session: sessionSnapshot(id, types.SessionStatusRunning)})
	for _, tc := range []struct {
		sequence        uint64
		eventType, want string
	}{
		{1, "session.closed", "completed"}, {2, "session.reopened", "running"}, {3, "session.closed", "completed"},
	} {
		p, _, _ = (&Reducer{}).Apply(p, wireEvent(id, tc.sequence, tc.eventType, "", map[string]any{"SessionID": id.Session}))
		if p.SessionStatus != tc.want {
			t.Fatalf("%s status=%q", tc.eventType, p.SessionStatus)
		}
	}
	p, _, _ = Reconcile(p, SnapshotBundle{Generation: 2, CapturedSequence: 3, Identity: id, Session: sessionSnapshot(id, types.SessionStatusRunning)})
	if p.SessionStatus != "running" {
		t.Fatalf("post-reopen snapshot status=%q", p.SessionStatus)
	}
}

func TestReducer_ErasureRejectsEveryLaterMutationFamily(t *testing.T) {
	id := testIdentity("erased")
	p, _ := (&Reducer{}).Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	p, _, _ = (&Reducer{}).Apply(p, wireEvent(id, 1, "session.erased", "", map[string]any{"SessionID": id.Session}))
	mutations := []WireEvent{
		wireEvent(id, 2, "task.started", "r", map[string]any{"TaskID": "r"}),
		wireEvent(id, 3, "llm.completion.chunk", "r", map[string]any{"TaskID": "r", "Delta": "x", "Kind": "content"}),
		wireEvent(id, 4, "tool.invoked", "r", map[string]any{"ToolName": "t"}),
		wireEvent(id, 5, "pause.requested", "r", map[string]any{"Token": "p", "Reason": "x"}),
		wireEvent(id, 6, "session.reopened", "", map[string]any{"SessionID": id.Session}),
		wireEvent(id, 7, "future.event", "", map[string]any{"Value": "x"}),
	}
	for _, mutation := range mutations {
		next, change, err := (&Reducer{}).Apply(p, mutation)
		if err != nil || !change.IgnoredTerminal || jsonValue(normalizeFixture(next)) != jsonValue(normalizeFixture(p)) {
			t.Fatalf("post-erasure mutation %s changed state: %#v %#v %v", mutation.Type, next, change, err)
		}
	}
	next, change, err := Reconcile(p, SnapshotBundle{Generation: 2, CapturedSequence: 100, Identity: id, Session: sessionSnapshot(id, types.SessionStatusRunning)})
	if err != nil || !change.IgnoredTerminal || !next.SessionErased {
		t.Fatalf("snapshot resurrected erase: %#v %#v %v", next, change, err)
	}
}

func TestReducer_InterventionsJoinByPauseTokenAndResolveIndependently(t *testing.T) {
	id := testIdentity("pauses")
	p, _ := (&Reducer{}).Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	events := []WireEvent{
		wireEvent(id, 1, "tool.approval_requested", "run", map[string]any{"Tool": "a", "PauseToken": "p1", "Reason": "one"}),
		wireEvent(id, 2, "tool.auth_required", "run", map[string]any{"Source": "mcp", "SourceName": "MCP", "PauseToken": "p2"}),
		wireEvent(id, 3, "pause.requested", "run", map[string]any{"Token": "p3", "Reason": "manual"}),
		wireEvent(id, 4, "tool.approved", "run", map[string]any{"Tool": "a", "PauseToken": "p1"}),
		wireEvent(id, 5, "pause.resumed", "run", map[string]any{"Token": "p3", "Decision": "timeout"}),
	}
	for _, event := range events {
		p, _, _ = (&Reducer{}).Apply(p, event)
	}
	if blockIndex(p.Blocks, "intervention:p1") >= 0 || blockIndex(p.Blocks, "intervention:p3") >= 0 || blockIndex(p.Blocks, "intervention:p2") < 0 {
		t.Fatalf("intervention correlation failed: %#v", p.Blocks)
	}
	p, _, _ = (&Reducer{}).Apply(p, wireEvent(id, 6, "tool.auth_completed", "run", map[string]any{"Source": "mcp", "PauseToken": "p2"}))
	if slices.ContainsFunc(p.Blocks, func(block Block) bool { return block.Kind == "intervention" }) {
		t.Fatalf("resolved-elsewhere survived: %#v", p.Blocks)
	}
}

func TestReducer_TypedMalformedFallbackAndRepeatedToolInvocations(t *testing.T) {
	id := testIdentity("tools")
	p, _ := (&Reducer{}).Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	for _, event := range []WireEvent{
		wireEvent(id, 1, "tool.invoked", "run", map[string]any{"ToolName": "same"}),
		wireEvent(id, 2, "tool.completed", "run", map[string]any{"ToolName": "same"}),
		wireEvent(id, 3, "tool.invoked", "run", map[string]any{"ToolName": "same"}),
		wireEvent(id, 4, "tool.failed", "run", map[string]any{"ToolName": "same"}),
		wireEvent(id, 5, "tool.completed", "run", map[string]any{"DurationMS": 1, "Secret": "not-retained"}),
	} {
		p, _, _ = (&Reducer{}).Apply(p, event)
	}
	if blockIndex(p.Blocks, "tool:run:1") < 0 || blockIndex(p.Blocks, "tool:run:3") < 0 {
		t.Fatalf("same-tool calls collapsed: %#v", p.Blocks)
	}
	fallback := p.Blocks[blockIndex(p.Blocks, "event:5")]
	if fallback.EventType != "tool.completed" || !fallback.Incomplete || !slices.Equal(fallback.PayloadKeys, []string{"DurationMS", "Secret"}) {
		t.Fatalf("malformed fallback=%#v", fallback)
	}
	encoded, _ := Normalize(p)
	if bytes.Contains(encoded, []byte("not-retained")) {
		t.Fatalf("payload value leaked: %s", encoded)
	}
}

func TestReducer_MalformedRecognizedFamiliesAlwaysBecomeGeneric(t *testing.T) {
	id := testIdentity("malformed")
	p, _ := (&Reducer{}).Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	cases := []struct {
		eventType, run string
		payload        map[string]any
	}{
		{"llm.completion.chunk", "run", map[string]any{"Delta": "x", "Kind": "invalid"}},
		{"task.spawned", "run", map[string]any{}}, {"task.completed", "run", map[string]any{}},
		{"tool.invoked", "run", map[string]any{}}, {"tool.completed", "run", map[string]any{}},
		{"pause.requested", "run", map[string]any{}}, {"tool.approval_requested", "run", map[string]any{"Tool": "x"}},
		{"tool.auth_required", "run", map[string]any{"Source": "x"}}, {"pause.resumed", "run", map[string]any{}},
		{"tool.approved", "run", map[string]any{}}, {"session.closed", "", map[string]any{"SessionID": "other"}},
		{"session.reopened", "", map[string]any{}}, {"session.erased", "", map[string]any{"SessionID": "other"}},
	}
	for i, tc := range cases {
		sequence := uint64(i + 1)
		var change ChangeSet
		p, change, _ = (&Reducer{}).Apply(p, wireEvent(id, sequence, tc.eventType, tc.run, tc.payload))
		if !change.Immediate || blockIndex(p.Blocks, fmt.Sprintf("event:%d", sequence)) < 0 {
			t.Fatalf("%s did not fall back: %#v", tc.eventType, p.Blocks)
		}
	}
	if p.SessionErased {
		t.Fatal("malformed erasure became terminal")
	}
}

func TestReducer_UnsequencedDedupeAndJournalOverflowAreExplicit(t *testing.T) {
	id := testIdentity("unsequenced")
	p, _ := (&Reducer{}).Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	event := wireEvent(id, 0, "bus.dropped", "run", map[string]any{"Dropped": 1})
	event.OccurredAt = time.Unix(100, 0).UTC()
	p, change, _ := (&Reducer{}).Apply(p, event)
	if !change.Immediate || len(p.Blocks) != 1 {
		t.Fatalf("unsequenced=%#v %#v", p, change)
	}
	again, duplicate, _ := (&Reducer{}).Apply(p, event)
	if duplicate.Changed || len(again.Blocks) != 1 {
		t.Fatalf("duplicate=%#v %#v", again, duplicate)
	}
	for i := range maxLiveJournalEvents + 1 {
		appendLiveEvent(&p, wireEvent(id, uint64(i+1), "future", "", nil))
	}
	if !p.ReconciliationRequired || p.ReplayGap == nil || p.ReplayGap.Reason != "live_journal_overflow" || len(p.liveEvents) != maxLiveJournalEvents {
		t.Fatalf("overflow=%#v", p.ReplayGap)
	}
}

func TestReducer_PauseSnapshotVariantsAndSafeHelpers(t *testing.T) {
	id := testIdentity("pause-snapshots")
	foreign := testIdentity("foreign")
	oldest := time.Unix(1, 0).UTC()
	p, err := (&Reducer{}).Hydrate(SnapshotBundle{Generation: 1, Identity: id, Health: &types.RuntimeHealth{Retention: []types.RetentionHorizon{{Surface: "events", Scope: types.RetentionScopeRuntime, OldestRetainedAt: &oldest}}}, Pauses: types.PauseListResponse{Snapshots: []types.PauseSnapshot{
		{Token: "resumed", State: types.PauseStateResumed, Identity: id}, {Token: "ref", State: types.PauseStatePaused, Identity: id, PayloadRef: &types.PauseArtifactRef{ID: "artifact"}}, {Token: "foreign", State: types.PauseStatePaused, Identity: foreign},
	}}})
	if err != nil || blockIndex(p.Blocks, "intervention:resumed") >= 0 || blockIndex(p.Blocks, "intervention:ref") < 0 || len(p.Retention) != 1 {
		t.Fatalf("snapshot=%#v %v", p, err)
	}
	removeBlock(&p, "intervention:ref")
	removeBlock(&p, "absent")
	if got := object(struct{ Value string }{"x"}); got["Value"] != "x" {
		t.Fatalf("object=%#v", got)
	}
	if got := object(make(chan int)); len(got) != 0 {
		t.Fatalf("marshal failure=%#v", got)
	}
	if got := safeText("a\x00b"); got != "ab" {
		t.Fatalf("safe text=%q", got)
	}
	if got := safeText(string(bytes.Repeat([]byte("z"), 600))); len([]rune(got)) != 515 {
		t.Fatalf("safe bound=%d", len([]rune(got)))
	}
	if toolInvocationID("run", 0, time.Unix(1, 0).UTC()) == "" || genericEventID(wireEvent(id, 0, "x", "", nil)) == "" {
		t.Fatal("unsequenced ids empty")
	}
	if decodePayload(make(chan int), &taskPayload{}) {
		t.Fatal("unserializable payload decoded")
	}
}

func TestReconcile_StaleIdentityAndTerminalBranches(t *testing.T) {
	id := testIdentity("reconcile")
	p, _ := (&Reducer{}).Hydrate(SnapshotBundle{Generation: 2, Identity: id})
	stale, change, err := Reconcile(p, SnapshotBundle{Generation: 1, Identity: id})
	if err != nil || change.Changed || stale.Generation != 2 {
		t.Fatalf("stale=%#v %#v %v", stale, change, err)
	}
	if _, _, err := Reconcile(p, SnapshotBundle{Generation: 3, Identity: testIdentity("other")}); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("identity error=%v", err)
	}
	p, _ = ApplyProtocolError(p, &client.ProtocolError{Code: protoerrors.CodeSessionErased})
	terminal, change, err := Reconcile(p, SnapshotBundle{Generation: 3, Identity: id})
	if err != nil || !change.IgnoredTerminal || !terminal.SessionErased {
		t.Fatalf("terminal=%#v %#v %v", terminal, change, err)
	}
	encoded, err := Normalize(Projection{Identity: id})
	if err != nil || !bytes.Contains(encoded, []byte(`"blocks":[]`)) {
		t.Fatalf("normalize=%s %v", encoded, err)
	}
}

func TestReducer_LongUnicodeCanonicalContentIsLossless(t *testing.T) {
	id := testIdentity("unicode")
	long := "航" + string(bytes.Repeat([]byte("a"), 4096)) + "終"
	p, _ := (&Reducer{}).Hydrate(SnapshotBundle{Generation: 1, Identity: id, Tasks: types.TaskListResponse{Rows: []types.TaskRow{taskRow(id, "run", long, types.TaskStatusRunning)}}})
	p, change, _ := (&Reducer{}).Apply(p, wireEvent(id, 1, "llm.completion.chunk", "run", map[string]any{"TaskID": "run", "Delta": long, "Kind": "content"}))
	if !change.Batchable || p.Blocks[blockIndex(p.Blocks, "text:run:0")].Text != long || p.Blocks[blockIndex(p.Blocks, "user:run")].Text != long {
		t.Fatal("canonical content was truncated")
	}
}

func TestClassifyEventType_EveryRegisteredCanonicalEventHasTypedOrGenericPath(t *testing.T) {
	for _, eventType := range events.EventTypes() {
		classification := ClassifyEventType(string(eventType))
		if classification != EventTyped && classification != EventGeneric {
			t.Fatalf("event %q unclassified", eventType)
		}
	}
}

func TestReducer_ConcurrentReuse_Isolates128Sessions(t *testing.T) {
	baseline := runtime.NumGoroutine()
	r := &Reducer{}
	var wg sync.WaitGroup
	errs := make(chan error, 128)
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := testIdentity(fmt.Sprintf("s-%d", i))
			p, err := r.Hydrate(SnapshotBundle{Generation: 1, Identity: id})
			if err == nil {
				p, _, err = r.Apply(p, wireEvent(id, 1, "llm.completion.chunk", "r", map[string]any{"TaskID": "r", "Delta": id.Session, "Kind": "content"}))
			}
			if err != nil || p.Identity.Session != id.Session || p.Blocks[0].Text != id.Session {
				errs <- fmt.Errorf("session %d: %#v: %w", i, p, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Fatalf("goroutines baseline=%d got=%d", baseline, got)
	}
}

type pagedClient struct {
	id         types.IdentityScope
	mu         sync.Mutex
	history    []types.StateHistoryResponse
	taskPages  map[string]types.TaskListResponse
	details    map[string]types.TaskDetail
	pausePages map[int]types.PauseListResponse
	fail       string
	block      <-chan struct{}
	entered    chan<- struct{}
}

func (f *pagedClient) Identity() types.IdentityScope { return f.id }
func (f *pagedClient) WithSession(session string) client.Client {
	id := f.id
	id.Session = session
	return &pagedClient{id: id, history: f.history, taskPages: f.taskPages, details: f.details, pausePages: f.pausePages, fail: f.fail, block: f.block, entered: f.entered}
}
func (f *pagedClient) wait(ctx context.Context) error {
	if f.block == nil {
		return nil
	}
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.block:
		return nil
	}
}
func (f *pagedClient) RuntimeInfo(ctx context.Context) (types.RuntimeInfo, error) {
	if err := f.wait(ctx); err != nil {
		return types.RuntimeInfo{}, err
	}
	if f.fail == "info" {
		return types.RuntimeInfo{}, errors.New("info")
	}
	return types.RuntimeInfo{ProtocolVersion: types.ProtocolVersion, Capabilities: types.Capabilities()}, nil
}
func (f *pagedClient) RuntimeHealth(ctx context.Context) (types.RuntimeHealth, error) {
	if f.fail == "health" {
		return types.RuntimeHealth{}, errors.New("health")
	}
	return types.RuntimeHealth{Retention: []types.RetentionHorizon{{Surface: "events", Scope: types.RetentionScopeRuntime}}}, nil
}
func (f *pagedClient) StateHistory(context.Context, types.StateHistoryRequest) (types.StateHistoryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail == "history" {
		return types.StateHistoryResponse{}, errors.New("history")
	}
	page := f.history[0]
	f.history = f.history[1:]
	return page, nil
}
func (f *pagedClient) TasksList(_ context.Context, request types.TaskListRequest) (types.TaskListResponse, error) {
	if f.fail == "tasks" {
		return types.TaskListResponse{}, errors.New("tasks")
	}
	return f.taskPages[request.Cursor.NextPageToken], nil
}
func (f *pagedClient) TasksGet(_ context.Context, request types.TaskGetRequest) (types.TaskDetail, error) {
	if f.fail == "detail" {
		return types.TaskDetail{}, errors.New("detail")
	}
	return f.details[request.ID], nil
}
func (f *pagedClient) SessionsInspect(context.Context, types.SessionsInspectRequest) (types.SessionsInspectResponse, error) {
	if f.fail == "session" {
		return types.SessionsInspectResponse{}, errors.New("session")
	}
	return *sessionSnapshot(f.id, types.SessionStatusRunning), nil
}
func (f *pagedClient) PauseList(_ context.Context, request types.PauseListRequest) (types.PauseListResponse, error) {
	if f.fail == "pause" {
		return types.PauseListResponse{}, errors.New("pause")
	}
	return f.pausePages[request.Page], nil
}
func (f *pagedClient) Start(context.Context, types.StartRequest) (types.StartResponse, error) {
	return types.StartResponse{}, nil
}
func (f *pagedClient) SessionsList(context.Context, types.SessionsListRequest) (types.SessionsListResponse, error) {
	return types.SessionsListResponse{}, nil
}
func (f *pagedClient) SessionsSetTitle(context.Context, types.SessionsSetTitleRequest) (types.SessionsSetTitleResponse, error) {
	return types.SessionsSetTitleResponse{}, nil
}
func (f *pagedClient) SessionsDelete(context.Context) (types.SessionsDeleteResponse, error) {
	return types.SessionsDeleteResponse{}, nil
}
func (f *pagedClient) Control(context.Context, methods.Method, types.ControlRequest) (types.ControlResponse, error) {
	return types.ControlResponse{}, nil
}
func (f *pagedClient) ArtifactsPut(context.Context, types.ArtifactsPutRequest) (types.ArtifactsPutResponse, error) {
	return types.ArtifactsPutResponse{}, nil
}
func (f *pagedClient) ArtifactsList(context.Context, types.ArtifactsListRequest) (types.ArtifactsListResponse, error) {
	return types.ArtifactsListResponse{}, nil
}
func (f *pagedClient) Subscribe(context.Context, client.StreamOptions) (*client.EventStream, error) {
	return nil, errors.New("unused")
}

func TestHydrateClient_PaginatesEveryReadAndSeparatesWindowFromRetention(t *testing.T) {
	id := testIdentity("pages")
	f := newPagedClient(id)
	f.history = []types.StateHistoryResponse{
		{Events: []types.StateEvent{wireEvent(id, 3, "future.3", "", nil)}, HasMore: true, NextCursor: 3, Truncated: true, TailSequence: 3},
		{Events: []types.StateEvent{wireEvent(id, 1, "future.1", "", nil), wireEvent(id, 2, "future.2", "", nil)}, HasMore: false, NextCursor: 0, Truncated: false},
	}
	f.taskPages[""] = types.TaskListResponse{Rows: []types.TaskRow{taskRow(id, "t1", "q1", types.TaskStatusRunning)}, Cursor: types.TaskListCursor{NextPageToken: "next"}}
	f.taskPages["next"] = types.TaskListResponse{Rows: []types.TaskRow{taskRow(id, "t2", "q2", types.TaskStatusComplete)}}
	f.details["t1"] = types.TaskDetail{Task: taskRow(id, "t1", "q1", types.TaskStatusRunning)}
	f.details["t2"] = types.TaskDetail{Task: taskRow(id, "t2", "q2", types.TaskStatusComplete)}
	f.pausePages[1] = types.PauseListResponse{Snapshots: []types.PauseSnapshot{pauseSnapshot(id, "p1")}, Page: 1, PageCount: 2, TotalRows: 2}
	f.pausePages[2] = types.PauseListResponse{Snapshots: []types.PauseSnapshot{pauseSnapshot(id, "p2")}, Page: 2, PageCount: 2, TotalRows: 2}
	bundle, err := HydrateClientWithOptions(context.Background(), f, 1, 3, HydrateOptions{MaxHistoryPages: 4, HistoryPageSize: 1, TaskPageSize: 1, PausePageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	sequences := []uint64{bundle.History.Events[0].Sequence, bundle.History.Events[1].Sequence, bundle.History.Events[2].Sequence}
	if !slices.Equal(sequences, []uint64{1, 2, 3}) || len(bundle.Tasks.Rows) != 2 || len(bundle.Pauses.Snapshots) != 2 {
		t.Fatalf("bundle pagination=%#v", bundle)
	}
	if !bundle.History.Truncated || bundle.History.HasMore {
		t.Fatalf("truncated=%v has_more=%v", bundle.History.Truncated, bundle.History.HasMore)
	}
	if bundle.Health == nil || len(bundle.Health.Retention) != 1 || len(bundle.UnavailableCapabilities) != 0 {
		t.Fatalf("posture=%#v", bundle)
	}
}

func TestHydrateClient_128BlockedCancellationsHaveNoCrossTalk(t *testing.T) {
	id := testIdentity("blocked")
	release := make(chan struct{})
	const count = 128
	results := make(chan error, count)
	entered := make(chan struct{}, count)
	cancels := make([]context.CancelFunc, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		go func() {
			defer wg.Done()
			f := newPagedClient(id)
			f.block = release
			f.entered = entered
			_, err := HydrateClient(ctx, f, 1, 0, 1)
			if i%2 == 0 {
				// A cancelled hydration either observes the cancellation
				// (context.Canceled) or completes before the cancel is
				// observed (nil) — both are correct; cancellation is
				// best-effort. Only a DIFFERENT, foreign error would be
				// cross-talk from another run. (The strict "must be
				// Canceled" form flaked on loaded CI when the work won the
				// race — the "no cross-talk" guarantee is the odd-index
				// check below, which stays strict.)
				if err != nil && !errors.Is(err, context.Canceled) {
					results <- fmt.Errorf("cancelled %d: %w", i, err)
				}
			} else if err != nil {
				results <- fmt.Errorf("live %d: %w", i, err)
			}
		}()
	}
	for range count {
		<-entered
	}
	for i, cancel := range cancels {
		if i%2 == 0 {
			cancel()
		} else {
			defer cancel()
		}
	}
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		t.Error(err)
	}
}

func TestHydrateClient_FailsLoudlyAtEveryStageAndCursorLoop(t *testing.T) {
	id := testIdentity("failures")
	for _, stage := range []string{"info", "health", "history", "tasks", "detail", "session", "pause"} {
		t.Run(stage, func(t *testing.T) {
			f := newPagedClient(id)
			f.fail = stage
			f.taskPages[""] = types.TaskListResponse{Rows: []types.TaskRow{taskRow(id, "t", "q", types.TaskStatusRunning)}}
			f.details["t"] = types.TaskDetail{}
			_, err := HydrateClient(context.Background(), f, 1, 0, 1)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
	f := newPagedClient(id)
	f.taskPages[""] = types.TaskListResponse{Cursor: types.TaskListCursor{NextPageToken: "repeat"}}
	f.taskPages["repeat"] = types.TaskListResponse{Cursor: types.TaskListCursor{NextPageToken: "repeat"}}
	if _, err := HydrateClient(context.Background(), f, 1, 0, 1); err == nil {
		t.Fatal("expected repeated cursor error")
	}
}

func TestApplyProtocolError_TypedErasureIsTerminal(t *testing.T) {
	id := testIdentity("typed")
	p, _ := (&Reducer{}).Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	unchanged, change := ApplyProtocolError(p, errors.New("ordinary"))
	if change.Changed || unchanged.SessionErased {
		t.Fatal("ordinary error mutated")
	}
	p, change = ApplyProtocolError(p, &client.ProtocolError{Status: 409, Code: protoerrors.CodeSessionErased})
	if !change.Immediate || !p.SessionErased {
		t.Fatalf("typed erasure=%#v %#v", p, change)
	}
}
func TestReducer_IdentityFailuresFailClosed(t *testing.T) {
	if _, err := (&Reducer{}).Hydrate(SnapshotBundle{}); !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("error=%v", err)
	}
	id := testIdentity("s")
	p, _ := (&Reducer{}).Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	_, _, err := (&Reducer{}).Apply(p, wireEvent(testIdentity("other"), 1, "future", "", nil))
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("error=%v", err)
	}
}

func TestReducer_UserBlockGatedByKindNotQueryPresence(t *testing.T) {
	id := testIdentity("classify")
	cases := []struct {
		name          string
		row           types.TaskRow
		wantUserBlock bool
		wantText      string
	}{
		{
			name:          "background row with non-empty query gets no user block",
			row:           backgroundTaskRow(id, "bg", "spawned query", types.TaskStatusRunning),
			wantUserBlock: false,
		},
		{
			name:          "foreground row with query gets user block",
			row:           taskRow(id, "fg", "composer prompt", types.TaskStatusRunning),
			wantUserBlock: true,
			wantText:      "composer prompt",
		},
		{
			name:          "foreground row with empty query still gets user block, falling back to description",
			row:           foregroundTaskRowNoQuery(id, "fg-empty", "fallback desc", types.TaskStatusRunning),
			wantUserBlock: true,
			wantText:      "fallback desc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := (&Reducer{}).Hydrate(SnapshotBundle{
				Generation: 1, Identity: id,
				Tasks: types.TaskListResponse{Rows: []types.TaskRow{tc.row}},
			})
			if err != nil {
				t.Fatal(err)
			}
			idx := blockIndex(p.Blocks, "user:"+tc.row.ID)
			if tc.wantUserBlock {
				if idx < 0 {
					t.Fatalf("expected user block for %s, blocks=%#v", tc.row.ID, p.Blocks)
				}
				if p.Blocks[idx].Text != tc.wantText {
					t.Fatalf("user block text=%q want=%q", p.Blocks[idx].Text, tc.wantText)
				}
			} else if idx >= 0 {
				t.Fatalf("did not expect user block for background row %s, blocks=%#v", tc.row.ID, p.Blocks)
			}
		})
	}
}

func newPagedClient(id types.IdentityScope) *pagedClient {
	return &pagedClient{id: id, history: []types.StateHistoryResponse{{}}, taskPages: map[string]types.TaskListResponse{"": {}}, details: map[string]types.TaskDetail{}, pausePages: map[int]types.PauseListResponse{1: {Page: 1, PageCount: 1}}}
}
func testIdentity(session string) types.IdentityScope {
	return types.IdentityScope{Tenant: "tenant", User: "user", Session: session}
}
func backgroundTaskRow(id types.IdentityScope, taskID, query string, status types.TaskStatus) types.TaskRow {
	return types.TaskRow{ID: taskID, Kind: types.TaskKindBackground, IsBackground: true, Status: status, Identity: id, ParentSessionID: id.Session, Query: query, StartedAt: time.Unix(1, 0).UTC()}
}
func foregroundTaskRowNoQuery(id types.IdentityScope, taskID, description string, status types.TaskStatus) types.TaskRow {
	return types.TaskRow{ID: taskID, Kind: types.TaskKindForeground, Status: status, Identity: id, ParentSessionID: id.Session, Description: description, StartedAt: time.Unix(1, 0).UTC()}
}
func taskRow(id types.IdentityScope, taskID, query string, status types.TaskStatus) types.TaskRow {
	return types.TaskRow{ID: taskID, Kind: types.TaskKindForeground, Status: status, Identity: id, ParentSessionID: id.Session, Query: query, StartedAt: time.Unix(1, 0).UTC()}
}
func pauseSnapshot(id types.IdentityScope, token string) types.PauseSnapshot {
	return types.PauseSnapshot{Token: token, Reason: "approval_required", State: types.PauseStatePaused, Identity: id, PausedAt: time.Unix(1, 0).UTC()}
}
func sessionSnapshot(id types.IdentityScope, status types.SessionStatus) *types.SessionsInspectResponse {
	return &types.SessionsInspectResponse{Row: types.SessionRow{SessionID: id.Session, TenantID: id.Tenant, UserID: id.User, Identity: id, Status: status}}
}
func wireEvent(id types.IdentityScope, sequence uint64, eventType, run string, payload map[string]any) types.StateEvent {
	return types.StateEvent{Type: eventType, Sequence: sequence, OccurredAt: time.Unix(int64(sequence), 0).UTC(), Tenant: id.Tenant, User: id.User, Session: id.Session, Run: run, Payload: payload}
}
func fixtureBundle(tc fixture) SnapshotBundle {
	return SnapshotBundle{Generation: tc.Generation, CapturedSequence: tc.CapturedSequence, Identity: tc.Identity, History: types.StateHistoryResponse{Events: tc.Events, Truncated: tc.HistoryTruncated, HasMore: tc.HistoryHasMore}, Tasks: types.TaskListResponse{Rows: tc.Tasks}, TaskDetails: tc.TaskDetails, Session: sessionSnapshotWithPartial(tc.Identity, tc.SessionStatus, tc.CountersPartial), Pauses: types.PauseListResponse{Snapshots: tc.Pauses}, AggregateTruncated: tc.AggregateTruncated, ToolsAggregatesPartial: tc.ToolsAggregatesPartial, ToolAnalyticsBounded: tc.ToolAnalyticsBounded, UnavailableCapabilities: tc.Unavailable}
}
func sessionSnapshotWithPartial(id types.IdentityScope, status types.SessionStatus, partial bool) *types.SessionsInspectResponse {
	snapshot := sessionSnapshot(id, status)
	snapshot.Row.CountersPartial = partial
	return snapshot
}
func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	data, err := os.ReadFile("../testdata/projection/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []fixture
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func normalizeFixture(p Projection) normalizedProjection {
	out := normalizedProjection{SessionStatus: p.SessionStatus, HistoryTruncated: p.HistoryTruncated, HistoryHasMore: p.HistoryHasMore, AggregateTruncated: p.AggregateTruncated, CountersPartial: p.CountersPartial, ToolsAggregatesPartial: p.ToolsAggregatesPartial, ToolAnalyticsBounded: p.ToolAnalyticsBounded, Unavailable: p.UnavailableCapabilities, Blocks: []normalizedBlock{}}
	if out.Unavailable == nil {
		out.Unavailable = []string{}
	}
	for _, b := range p.Blocks {
		n := normalizedBlock{ID: b.ID, Kind: b.Kind, RunID: b.RunID, Status: b.Status, Text: b.Text, Tool: b.Tool, EventType: b.EventType, PayloadKeys: b.PayloadKeys, Incomplete: b.Incomplete}
		for _, a := range b.Artifacts {
			n.ArtifactIDs = append(n.ArtifactIDs, a.ID)
		}
		out.Blocks = append(out.Blocks, n)
	}
	return out
}
func jsonValue(value any) string { data, _ := json.Marshal(value); return string(data) }
func normalizeConversation(p Projection) []normalizedConversation {
	order := []string{}
	byRun := map[string]*normalizedConversation{}
	for _, block := range p.Blocks {
		if block.RunID == "" {
			continue
		}
		turn := byRun[block.RunID]
		if turn == nil {
			turn = &normalizedConversation{RunID: block.RunID, Tools: []normalizedTool{}}
			byRun[block.RunID] = turn
			order = append(order, block.RunID)
		}
		switch block.Kind {
		case "text":
			turn.Answer = block.Text
		case "reasoning":
			turn.Reasoning = block.Text
		case "task":
			turn.Terminal = terminalStatus(block.Status)
		case "tool":
			turn.Tools = append(turn.Tools, normalizedTool{Tool: block.Tool, Status: block.Status})
		}
	}
	out := make([]normalizedConversation, 0, len(order))
	for _, run := range order {
		out = append(out, *byRun[run])
	}
	return out
}

func TestFixtureCorpus_IsSequenceOrdered(t *testing.T) {
	for _, tc := range loadFixtures(t) {
		sequences := make([]uint64, len(tc.Events))
		for i, event := range tc.Events {
			sequences[i] = event.Sequence
		}
		if !sort.SliceIsSorted(sequences, func(i, j int) bool { return sequences[i] < sequences[j] }) {
			t.Fatalf("fixture %s not ordered", tc.Name)
		}
	}
}

// TestReducer_MultiStepStreamSegmentsMessagesInOrder pins the fix for the
// out-of-order transcript bug: within one turn the agent streams a preamble,
// reasons, calls a tool, then streams the final answer. Keying streaming blocks
// per LLM step (segmented on the Done terminator) keeps every message and
// reasoning segment a distinct block in arrival order — the preamble and the
// final answer never merge into one block that anchors above the intervening
// reasoning/tool blocks.
func TestReducer_MultiStepStreamSegmentsMessagesInOrder(t *testing.T) {
	id := testIdentity("multistep")
	r := &Reducer{}
	p, err := r.Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	if err != nil {
		t.Fatal(err)
	}
	seq := uint64(0)
	apply := func(eventType string, payload map[string]any) {
		seq++
		p, _, err = r.Apply(p, wireEvent(id, seq, eventType, "run", payload))
		if err != nil {
			t.Fatal(err)
		}
	}
	chunk := func(delta, kind string, done bool) {
		apply("llm.completion.chunk", map[string]any{"TaskID": "run", "Delta": delta, "Kind": kind, "Done": done})
	}

	// Realistic streaming: reasoning and content EACH terminate with their own
	// Done chunk (empty delta), matching the canonical llm.completion.chunk
	// stream (OnReasoning/OnContent are independent callbacks).
	apply("task.started", map[string]any{"TaskID": "run"})
	chunk("thinking about tools", "reasoning", false)
	chunk("", "reasoning", true) // step-0 reasoning terminator
	chunk("Let me check my tools.", "content", false)
	chunk("", "content", true) // step-0 content terminator
	apply("tool.invoked", map[string]any{"ToolName": "catalog_search"})
	apply("tool.completed", map[string]any{"ToolName": "catalog_search"})
	chunk("planning the answer", "reasoning", false)
	chunk("", "reasoning", true) // step-1 reasoning terminator
	chunk("Here is what I can do.", "content", false)
	chunk("", "content", true) // step-1 content terminator

	var order []string
	for _, b := range p.Blocks {
		switch b.Kind {
		case "reasoning", "text", "tool":
			order = append(order, b.Kind+":"+b.Text)
		}
	}
	want := []string{
		"reasoning:thinking about tools",
		"text:Let me check my tools.",
		"tool:",
		"reasoning:planning the answer",
		"text:Here is what I can do.",
	}
	if len(order) != len(want) {
		t.Fatalf("block order = %#v, want %#v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("block[%d] = %q, want %q (full order %#v)", i, order[i], want[i], order)
		}
	}
	// The two content messages must be SEPARATE blocks, never merged.
	if idx := blockIndex(p.Blocks, "text:run:0"); idx < 0 || p.Blocks[idx].Text != "Let me check my tools." {
		t.Fatalf("step-0 message block wrong: %#v", p.Blocks)
	}
	if idx := blockIndex(p.Blocks, "text:run:1"); idx < 0 || p.Blocks[idx].Text != "Here is what I can do." {
		t.Fatalf("step-1 message block wrong: %#v", p.Blocks)
	}
}
