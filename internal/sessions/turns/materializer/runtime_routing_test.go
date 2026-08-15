package materializer

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
	turnprotocol "github.com/hurtener/Harbor/internal/sessions/turns/protocol"
	"github.com/hurtener/Harbor/internal/tasks"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// TestMaterialize_RuntimeTaskRunBindingPersistsRichTurnAcrossRestart pins the
// stock Runtime's exact event shape: task.spawned has an empty envelope RunID,
// then planner/tool/App events use TaskID as RunID. The durable projection and
// the public conversation read must retain the derived reasoning, activity,
// and App ref byte-identically across a store/materializer restart.
func TestMaterialize_RuntimeTaskRunBindingPersistsRichTurnAcrossRestart(t *testing.T) {
	dsn := t.TempDir() + "/turns-rich.sqlite"
	h := newHarness(t, dsn)
	reader := newFakeTaskReader().set("task-rich", TaskSnapshot{
		TaskID:        "task-rich",
		QueryPresent:  true,
		Query:         "Build an Atrium view",
		QueryAt:       time.Unix(1_700_100_000, 0),
		AnswerPresent: true,
		Answer: turns.Answer{
			State:  turns.AnswerStateInline,
			Inline: "Atrium is ready.",
		},
	})

	app := appAvailableEv(h.id, "task-rich", "ux-prototype-agent", "atrium", "ui://atrium/dashboard")
	appPayload := app.Payload.(mcpdrv.AppAvailablePayload)
	appPayload.ToolCallID = "call-atrium-01"
	appPayload.ToolName = "atrium_create"
	app.Payload = appPayload

	// RedactedMap is the durable event driver's rehydrated representation,
	// so this exercises the persisted key/value shape rather than typed-only
	// in-process payloads.
	for _, ev := range []events.Event{
		spawnEv(h.id, "", "task-rich", tasks.KindForeground, ""),
		startedEv(h.id, "task-rich"),
		decisionEv(h.id, "task-rich", "CallTool"),
		toolInvokedEv(h.id, "task-rich", "atrium_create", time.Unix(1_700_100_001, 0)),
		toolCompletedEv(h.id, "task-rich", "atrium_create", 17),
		app,
		completedEv(h.id, "task-rich"),
	} {
		ev.Payload = redacted(t, ev.Payload)
		h.src.publish(t, ev)
	}

	m1 := h.newMaterializer(t, WithTaskSnapshotReader(reader))
	if _, err := m1.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize rich runtime shape: %v", err)
	}
	pre := mustGetRow(t, h, "task-rich")
	assertRichRuntimeTurn(t, pre)
	preWire := getConversationJSON(t, h.proj, h.id, "task-rich")

	h.closeStore()
	store2, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	proj2, err := turns.New(store2)
	if err != nil {
		t.Fatalf("reopen projector: %v", err)
	}
	defer func() { _ = proj2.Close(context.Background()) }()
	h2 := &harness{id: h.id, store: store2, proj: proj2, src: h.src}
	m2 := h2.newMaterializer(t, WithTaskSnapshotReader(reader))
	if _, err := m2.Materialize(context.Background()); err != nil {
		t.Fatalf("replay rich runtime shape: %v", err)
	}

	post := mustGetRow(t, h2, "task-rich")
	if !reflect.DeepEqual(post, pre) {
		t.Fatalf("restart changed rich turn:\nbefore: %+v\nafter:  %+v", pre, post)
	}
	postWire := getConversationJSON(t, proj2, h.id, "task-rich")
	if string(postWire) != string(preWire) {
		t.Fatalf("restart changed sessions.turns.get bytes:\nbefore: %s\nafter:  %s", preWire, postWire)
	}
}

func assertRichRuntimeTurn(t *testing.T, row turns.TurnRow) {
	t.Helper()
	if row.RunID != "task-rich" || !row.Sealed || row.Status != turns.StatusComplete || row.Answer.Inline != "Atrium is ready." {
		t.Fatalf("identity/lifecycle/answer = run %q sealed %v status %q answer %q", row.RunID, row.Sealed, row.Status, row.Answer.Inline)
	}
	if len(row.Reasoning.Steps) != 1 || row.Reasoning.Steps[0] != (turns.ReasoningStep{Index: 0, Kind: turns.ReasoningKindToolCall}) {
		t.Fatalf("reasoning = %+v", row.Reasoning)
	}
	if len(row.Activity.Rows) != 1 || row.Activity.Rows[0].Tool != "atrium_create" || row.Activity.Rows[0].Status != turns.ActivitySucceeded || row.Activity.Totals.Succeeded != 1 {
		t.Fatalf("activity = %+v", row.Activity)
	}
	if len(row.Apps) != 1 {
		t.Fatalf("apps = %+v", row.Apps)
	}
	wantApp := turns.AppRef{
		EffectiveAgentID: "ux-prototype-agent",
		ServerID:         "atrium",
		ResourceURI:      "ui://atrium/dashboard",
		DisplayMode:      "inline",
		ToolCallID:       "call-atrium-01",
		ToolName:         "atrium_create",
		Availability:     turns.AppAvailable,
		Complete:         turns.CompletenessComplete,
	}
	if row.Apps[0] != wantApp {
		t.Fatalf("app = %+v, want %+v", row.Apps[0], wantApp)
	}
}

func getConversationJSON(t *testing.T, proj *turns.Projector, id identity.Identity, taskID string) []byte {
	t.Helper()
	svc, err := turnprotocol.NewService(proj,
		turnprotocol.WithSessionReachAuthorizer(auth.NewSessionReachAuthorizer()),
		turnprotocol.WithAgentReachAuthorizer(auth.NewAgentReachAuthorizer()),
	)
	if err != nil {
		t.Fatalf("new turns protocol service: %v", err)
	}
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("verified identity: %v", err)
	}
	resp, err := svc.Get(ctx, turnprotocol.GetRequest{SessionID: id.SessionID, TaskID: taskID})
	if err != nil {
		t.Fatalf("sessions.turns.get: %v", err)
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal sessions.turns.get response: %v", err)
	}
	return b
}
