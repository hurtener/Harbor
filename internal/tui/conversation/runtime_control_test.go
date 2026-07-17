package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	protocolclient "github.com/hurtener/Harbor/internal/protocol/client"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/interventions"
	"github.com/hurtener/Harbor/internal/tui/surface"
)

func TestController_RuntimeInspectionAndCanonicalActions(t *testing.T) {
	id := types.IdentityScope{Tenant: "tenant", User: "user", Session: "session"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var value any
		switch r.URL.Path {
		case "/v1/control/runtime.info":
			value = types.RuntimeInfo{InstanceID: "runtime", ProtocolVersion: types.ProtocolVersion, WireSurfaceDigest: "digest", Capabilities: []types.Capability{types.CapTaskControl, types.CapToolAnnotations}}
		case "/v1/control/runtime.health":
			value = types.RuntimeHealth{Subsystems: []types.SubsystemHealth{}}
		case "/v1/control/runtime.counters":
			value = types.RuntimeCounters{}
		case "/v1/control/runtime.drivers":
			value = types.RuntimeDrivers{}
		case "/v1/control/metrics.snapshot":
			value = types.MetricsSnapshot{}
		case "/v1/control/governance.posture":
			value = types.GovernancePostureResponse{IdentityTiers: map[string]types.IdentityTierView{}}
		case "/v1/control/llm.posture":
			value = types.LLMPostureResponse{}
		case "/v1/tasks/list":
			value = types.TaskListResponse{Rows: []types.TaskRow{{ID: "task", Identity: id}}}
		case "/v1/tasks/get":
			value = types.TaskDetail{Task: types.TaskRow{ID: "task"}}
		case "/v1/tools/list":
			value = types.ToolListResponse{Tools: []types.Tool{{ID: "tool"}}}
		case "/v1/tools/get":
			value = types.Tool{ID: "tool"}
		case "/v1/tools/describe":
			value = types.ToolManifest{Tool: types.Tool{ID: "tool"}}
		case "/v1/tools/metrics":
			value = types.ToolMetrics{ID: "tool"}
		case "/v1/tools/content_stats":
			value = types.ToolContentStats{ID: "tool"}
		case "/v1/control/artifacts.list":
			value = types.ArtifactsListResponse{Rows: []types.ArtifactRow{{Ref: types.ArtifactRef{ID: "artifact"}}}}
		case "/v1/events/list":
			value = types.EventsListResponse{Events: []types.StateEvent{}}
		case "/v1/events/aggregate":
			value = types.EventAggregateResponse{Buckets: []types.EventBucket{}, ProtocolVersion: types.ProtocolVersion}
		case "/v1/pause/list":
			value = types.PauseListResponse{Snapshots: []types.PauseSnapshot{{Token: "pause", State: types.PauseStatePaused, Identity: types.IdentityScope{Run: "task"}, PausedAt: time.Now()}}}
		case "/v1/control/cancel":
			value = types.ControlResponse{Accepted: true, Method: "cancel", ProtocolVersion: types.ProtocolVersion}
		case "/v1/control/artifacts.delete":
			value = types.ArtifactsDeleteResponse{Deleted: true, ProtocolVersion: types.ProtocolVersion}
		case "/v1/tools/set_approval_policy":
			value = types.ToolSetApprovalPolicyResponse{ID: "tool", Policy: types.ToolApprovalGated}
		case "/v1/tools/revoke_oauth":
			value = types.ToolRevokeOAuthResponse{ID: "tool", RevokedCount: 1}
		case "/v1/sessions/delete":
			value = types.SessionsDeleteResponse{SessionID: "session", Deleted: true}
		case "/v1/control/resume":
			value = types.ControlResponse{Accepted: true, Method: "resume", ProtocolVersion: types.ProtocolVersion}
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	base, err := protocolclient.New(protocolclient.Connection{BaseURL: server.URL, Token: protocolclient.TokenSourceFunc(func(context.Context, types.IdentityScope) (string, error) { return "token", nil }), Identity: id}, protocolclient.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	runtimeClient, ok := base.(protocolclient.RuntimeClient)
	if !ok {
		t.Fatal("missing RuntimeClient")
	}
	controller := &Controller{client: runtimeClient, identity: id, generation: 2, capabilities: map[types.Capability]bool{types.CapTaskControl: true, types.CapToolAnnotations: true}, onUpdate: func(Update) {}}
	data := controller.Inspect(t.Context(), InspectionRequest{Identity: id, Generation: 2, RequestEpoch: 1, TaskID: "task", ToolID: "tool", ArtifactID: "artifact"})
	if len(data.Errors) != 0 || len(data.Tasks.Rows) != 1 || len(data.Tools.Rows) != 1 || len(data.Artifacts.Rows) != 1 || len(data.Interventions.Items()) != 1 {
		t.Fatalf("data=%#v", data)
	}
	if detail, err := controller.TaskDetail(t.Context(), "task"); err != nil || detail.Task.ID != "task" {
		t.Fatalf("detail=%#v %v", detail, err)
	}
	if detail, err := controller.ToolDetail(t.Context(), "tool"); err != nil || detail.Manifest == nil || detail.Metrics == nil || detail.Content == nil || !detail.BestEffort {
		t.Fatalf("tool=%#v %v", detail, err)
	}
	if response, err := controller.Control(t.Context(), methods.MethodCancel, "task", "session_user", nil); err != nil || !response.Accepted {
		t.Fatalf("control=%#v %v", response, err)
	}
	if response, err := controller.DeleteArtifact(t.Context(), "artifact"); err != nil || !response.Deleted {
		t.Fatalf("delete=%#v %v", response, err)
	}
	if response, err := controller.SetToolApproval(t.Context(), "tool", types.ToolApprovalGated); err != nil || response.Policy != types.ToolApprovalGated {
		t.Fatalf("policy=%#v %v", response, err)
	}
	if response, err := controller.RevokeToolOAuth(t.Context(), "tool"); err != nil || response.RevokedCount != 1 {
		t.Fatalf("oauth=%#v %v", response, err)
	}
	for _, mutation := range []Mutation{
		{Identity: id, Method: methods.MethodCancel, RunID: "task"},
		{Identity: id, Method: methods.MethodResume, RunID: "task", PauseToken: "pause"},
		{Identity: id, Method: methods.MethodArtifactsDelete, ArtifactID: "artifact"},
		{Identity: id, Method: methods.MethodToolsSetApprovalPolicy, ToolID: "tool", Payload: map[string]any{"policy": "gated"}},
		{Identity: id, Method: methods.MethodToolsRevokeOAuth, ToolID: "tool"},
		{Identity: id, Method: methods.MethodSessionsDelete},
	} {
		if err := controller.Execute(t.Context(), mutation); err != nil {
			t.Fatalf("Execute(%s): %v", mutation.Method, err)
		}
	}
	if got := canonicalAuthority(methods.MethodInjectContext); got != "session_user" {
		t.Fatalf("inject authority=%q", got)
	}
	if got := canonicalAuthority(methods.MethodPrioritize); got != "admin" {
		t.Fatalf("prioritize authority=%q", got)
	}
	if state := errorSurface(&protocolclient.ProtocolError{Status: 501, Message: "missing"}); state.Status != surface.Unavailable {
		t.Fatalf("501 surface=%#v", state)
	}
	if state := errorSurface(errors.New("broken")); state.Status != surface.Failed {
		t.Fatalf("error surface=%#v", state)
	}
	controller.capabilities[types.CapToolAnnotations] = false
	if detail, err := controller.ToolDetail(t.Context(), "tool"); err != nil || detail.Unavailable == "" || detail.Manifest != nil {
		t.Fatalf("unannotated tool=%#v %v", detail, err)
	}
	if clone := cloneAnyMap(map[string]any{"key": "value"}); clone["key"] != "value" || cloneAnyMap(nil) != nil {
		t.Fatalf("map clone=%v", clone)
	}
	if clone := mapsClone(map[types.Capability]bool{types.CapTaskControl: true}); !clone[types.CapTaskControl] {
		t.Fatalf("capability clone=%v", clone)
	}
}

func TestController_InspectionWithoutAttachmentIsExplicit(t *testing.T) {
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	controller := &Controller{identity: id, capabilities: map[types.Capability]bool{}, interventions: interventions.New()}
	if data := controller.Inspect(t.Context(), InspectionRequest{}); data.Errors["runtime"] != "not attached" {
		t.Fatalf("data=%#v", data)
	}
	if _, err := controller.TaskDetail(t.Context(), "x"); err == nil {
		t.Fatal("TaskDetail accepted without attachment")
	}
	if _, err := controller.ToolDetail(t.Context(), "x"); err == nil {
		t.Fatal("ToolDetail accepted without attachment")
	}
	if _, err := controller.Control(t.Context(), methods.MethodCancel, "x", "session_user", nil); err == nil {
		t.Fatal("Control accepted without attachment")
	}
	if _, err := controller.DeleteArtifact(t.Context(), "x"); err == nil {
		t.Fatal("DeleteArtifact accepted without attachment")
	}
	if _, err := controller.SetToolApproval(t.Context(), "x", types.ToolApprovalAuto); err == nil {
		t.Fatal("SetToolApproval accepted without attachment")
	}
	if _, err := controller.RevokeToolOAuth(t.Context(), "x"); err == nil {
		t.Fatal("RevokeToolOAuth accepted without attachment")
	}
	for _, mutation := range []Mutation{{Identity: id, Method: methods.MethodCancel, RunID: "run"}, {Identity: id, Method: methods.MethodArtifactsDelete, ArtifactID: "a"}, {Identity: id, Method: methods.MethodToolsSetApprovalPolicy, ToolID: "t", Payload: map[string]any{"policy": "auto"}}, {Identity: id, Method: methods.MethodToolsRevokeOAuth, ToolID: "t"}, {Identity: id, Method: methods.MethodSessionsDelete}} {
		if err := controller.Execute(t.Context(), mutation); err == nil {
			t.Fatalf("Execute(%s) accepted without attachment", mutation.Method)
		}
	}
	controller.inspectEpoch = 4
	if data := controller.Inspect(t.Context(), InspectionRequest{Identity: id, RequestEpoch: 3}); !data.Stale {
		t.Fatalf("old inspection=%#v", data)
	}
	if err := controller.Execute(t.Context(), Mutation{Identity: types.IdentityScope{Tenant: "other", User: "u", Session: "s"}, Method: methods.MethodCancel}); err == nil {
		t.Fatal("stale mutation identity accepted")
	}
}

func TestController_InspectionUnavailableSurfacesRemainExplicit(t *testing.T) {
	id := types.IdentityScope{Tenant: "tenant", User: "user", Session: "session"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(protoerrors.Error{Code: protoerrors.CodeRuntimeError, Message: "surface unavailable"})
	}))
	defer server.Close()
	base, err := protocolclient.New(protocolclient.Connection{BaseURL: server.URL, Token: protocolclient.TokenSourceFunc(func(context.Context, types.IdentityScope) (string, error) { return "token", nil }), Identity: id}, protocolclient.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	controller := &Controller{client: base.(protocolclient.RuntimeClient), identity: id, generation: 1, capabilities: map[types.Capability]bool{}, interventions: interventions.New()}
	data := controller.Inspect(t.Context(), InspectionRequest{Identity: id, Generation: 1, RequestEpoch: 1})
	if len(data.Errors) < 8 || data.Tasks.Surface.Status != surface.Unavailable || data.Tools.Surface.Status != surface.Unavailable || data.Artifacts.Surface.Status != surface.Unavailable || data.Events.Surface.Status != surface.Unavailable || data.Interventions.Surface.Status != surface.Unavailable {
		t.Fatalf("unavailable inspection=%#v", data)
	}
}
