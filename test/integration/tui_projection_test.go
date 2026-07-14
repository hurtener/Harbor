package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	_ "github.com/hurtener/Harbor/internal/llm/mock" // Hermetic devstack LLM explicitly gated at the test boundary.
	"github.com/hurtener/Harbor/internal/protocol/client"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tui/projection"
)

func TestE2E_TUIProjection_AuthenticatedHydrateStreamPauseReopenErase(t *testing.T) {
	cfg := config.Defaults()
	cfg.LLM.Driver = "mock"
	cfg.LLM.Model = "mock/echo"
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"mock/echo": {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4"},
	}
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("ValidateCore: %v", err)
	}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{})
	defer stack.Close()
	server := httptest.NewServer(stack.Handler)
	defer server.Close()

	scope := types.IdentityScope{Tenant: devstack.DefaultDevTenant, User: devstack.DefaultDevUser, Session: devstack.DefaultDevSession}
	protocolClient, err := client.New(client.Connection{BaseURL: server.URL, Token: client.StaticToken(stack.Token, scope), Identity: scope})
	if err != nil {
		t.Fatal(err)
	}
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	stream, err := protocolClient.Subscribe(streamCtx, client.StreamOptions{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = stream.Close() }()

	started, err := protocolClient.Start(context.Background(), types.StartRequest{Query: "projection integration", IdempotencyKey: "projection-integration-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	events := receiveThroughTerminal(t, stream, started.TaskID)
	lastSequence := events[len(events)-1].Sequence
	bundle, err := projection.HydrateClient(context.Background(), protocolClient, 1, lastSequence, 8)
	if err != nil {
		t.Fatalf("HydrateClient: %v", err)
	}
	projected, err := (&projection.Reducer{}).Hydrate(bundle)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if projected.Identity.Session != scope.Session || len(projected.Blocks) == 0 {
		t.Fatalf("hydrated projection = %#v", projected)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close first stream: %v", err)
	}
	stream, err = protocolClient.Subscribe(streamCtx, client.StreamOptions{LastEventID: projected.Cursor})
	if err != nil {
		t.Fatalf("reconnect at cursor %q: %v", projected.Cursor, err)
	}
	second, err := protocolClient.Start(context.Background(), types.StartRequest{Query: "pagination", IdempotencyKey: "projection-integration-page-2"})
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	secondEvents := receiveThroughTerminal(t, stream, second.TaskID)
	lastSequence = secondEvents[len(secondEvents)-1].Sequence

	runtimeID := identity.Identity{TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session}
	pausesCreated := make([]pauseresume.Pause, 3)
	for i := range pausesCreated {
		pausesCreated[i], err = stack.Coordinator.Request(context.Background(), pauseresume.PauseRequest{Identity: runtimeID, Reason: pauseresume.ReasonApprovalRequired})
		if err != nil {
			t.Fatalf("Request pause %d: %v", i, err)
		}
	}
	pauses, err := protocolClient.PauseList(context.Background(), types.PauseListRequest{})
	if err != nil || len(pauses.Snapshots) != 3 {
		t.Fatalf("authenticated pause snapshot: %#v, %v", pauses, err)
	}
	pagedBundle, err := projection.HydrateClientWithOptions(context.Background(), protocolClient, 2, lastSequence, projection.HydrateOptions{MaxHistoryPages: 8, TaskPageSize: 1, PausePageSize: 1})
	if err != nil || len(pagedBundle.Pauses.Snapshots) != 3 || len(pagedBundle.Tasks.Rows) < 2 || pagedBundle.Health == nil {
		t.Fatalf("authenticated paginated hydration: tasks=%d pauses=%d health=%v err=%v", len(pagedBundle.Tasks.Rows), len(pagedBundle.Pauses.Snapshots), pagedBundle.Health != nil, err)
	}
	ctxWithIdentity, err := identity.With(context.Background(), runtimeID)
	if err != nil {
		t.Fatal(err)
	}
	for _, pause := range pausesCreated {
		if err := stack.Coordinator.Resume(ctxWithIdentity, pause.Token, pauseresume.DecisionApprove, nil); err != nil {
			t.Fatalf("approve pause: %v", err)
		}
	}

	if err := stack.Sessions.Close(ctxWithIdentity, scope.Session, "integration"); err != nil {
		t.Fatalf("close session: %v", err)
	}
	reopened, err := protocolClient.Start(context.Background(), types.StartRequest{Query: "reopen", IdempotencyKey: "projection-integration-2"})
	if err != nil {
		t.Fatalf("reopen Start: %v", err)
	}
	reopenEvents := receiveThroughTerminal(t, stream, reopened.TaskID)
	if len(reopenEvents) > 1 {
		gapProjection, _ := (&projection.Reducer{}).Hydrate(projection.SnapshotBundle{Generation: 1, Identity: scope})
		gapProjection, _, _ = (&projection.Reducer{}).Apply(gapProjection, reopenEvents[len(reopenEvents)-1])
		gapProjection, gapChange, _ := (&projection.Reducer{}).Apply(gapProjection, reopenEvents[0])
		if !gapChange.ReconciliationRequired || !gapProjection.ReconciliationRequired || gapProjection.ReplayGap == nil {
			t.Fatalf("forced replay gap was silent: %#v %#v", gapProjection, gapChange)
		}
	}
	foundReopened := false
	for _, event := range reopenEvents {
		if event.Type == "session.reopened" {
			foundReopened = true
		}
		var change projection.ChangeSet
		projected, change, err = (&projection.Reducer{}).Apply(projected, event)
		_ = change
		if err != nil {
			t.Fatalf("Apply stream event: %v", err)
		}
	}
	if !foundReopened || projected.SessionStatus != "running" {
		t.Fatalf("reopen not projected: found=%v status=%q", foundReopened, projected.SessionStatus)
	}
	controlIdentity := identity.Quadruple{Identity: runtimeID, RunID: "projection-control-run"}
	if _, err := stack.Steering.Open(controlIdentity); err != nil {
		t.Fatalf("open control inbox: %v", err)
	}
	controlResponse, err := protocolClient.Control(context.Background(), methods.MethodPause, types.ControlRequest{Identity: types.IdentityScope{Run: controlIdentity.RunID}})
	if err != nil || !controlResponse.Accepted {
		t.Fatalf("authenticated Protocol control: %#v %v", controlResponse, err)
	}
	if err := stack.Steering.Retire(controlIdentity); err != nil {
		t.Fatalf("retire control inbox: %v", err)
	}
	_, controlErr := protocolClient.Control(context.Background(), methods.MethodPause, types.ControlRequest{Identity: types.IdentityScope{Run: "missing-run"}})
	var controlProtocolErr *client.ProtocolError
	if !errors.As(controlErr, &controlProtocolErr) || controlProtocolErr.Code != protoerrors.CodeNotFound {
		t.Fatalf("authenticated Protocol control failure = %v", controlErr)
	}

	deleted, err := protocolClient.SessionsDelete(context.Background())
	if err != nil || !deleted.Deleted {
		t.Fatalf("SessionsDelete: %#v, %v", deleted, err)
	}
	_, err = protocolClient.Start(context.Background(), types.StartRequest{Query: "must fail", IdempotencyKey: "projection-integration-3"})
	var protocolErr *client.ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != protoerrors.CodeSessionErased {
		t.Fatalf("erased Start error = %v", err)
	}
	projected, change := projection.ApplyProtocolError(projected, err)
	if !change.Immediate || !projected.SessionErased {
		t.Fatalf("typed erasure not terminal: %#v %#v", projected, change)
	}
}

func receiveThroughTerminal(t *testing.T, stream *client.EventStream, taskID string) []types.StateEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out []types.StateEvent
	for {
		frame, err := stream.Recv(ctx)
		if err != nil {
			t.Fatalf("stream Recv: %v", err)
		}
		if len(frame.Data) == 0 {
			continue
		}
		var event types.StateEvent
		if err := json.Unmarshal(frame.Data, &event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		out = append(out, event)
		eventTaskID := event.Run
		if payload, ok := event.Payload.(map[string]any); ok && eventTaskID == "" {
			if value, ok := payload["TaskID"].(string); ok {
				eventTaskID = value
			}
		}
		if eventTaskID == taskID && (event.Type == "task.completed" || event.Type == "task.failed" || event.Type == "task.cancelled") {
			return out
		}
	}
}
