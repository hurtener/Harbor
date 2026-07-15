package app

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	tuitasks "github.com/hurtener/Harbor/internal/tui/tasks"
)

func TestActionMatrix_CanonicalCompleteAndDestructiveConfirmed(t *testing.T) {
	wantControls := map[methods.Method]bool{methods.MethodCancel: true, methods.MethodPause: true, methods.MethodResume: true, methods.MethodRedirect: true, methods.MethodInjectContext: true, methods.MethodApprove: true, methods.MethodReject: true, methods.MethodPrioritize: true, methods.MethodUserMessage: true}
	seen := map[string]bool{}
	for _, action := range ActionMatrix() {
		if seen[action.ID] || action.Method == "" || action.Scope == "" || action.Outcome == "" || action.Reconcile == "" {
			t.Fatalf("invalid action=%#v", action)
		}
		seen[action.ID] = true
		delete(wantControls, action.Method)
		if action.ID == "task.cancel" || action.ID == "intervention.reject" || action.ID == "tool.oauth.revoke" || action.ID == "artifact.delete" || action.ID == "session.delete" {
			if action.Confirmation != ConfirmDestructive {
				t.Fatalf("destructive action lacks confirmation: %#v", action)
			}
		}
	}
	if len(wantControls) != 0 {
		t.Fatalf("missing controls=%v", wantControls)
	}
	resolved := resolveActions(map[types.Capability]bool{}, nil, "", "", "", "")
	for _, action := range resolved {
		if action.ID != "session.delete" && action.DisabledReason == "" {
			t.Fatalf("action should be disabled without target/capability: %#v", action)
		}
	}
}

func TestActionIntent_ExactTargetPayloadAndStaleFence(t *testing.T) {
	m, controller, _ := operationalModel(t)
	m.runtime.Tasks = tuitasks.Derive(types.TaskListResponse{Rows: []types.TaskRow{{ID: "shown", Status: types.TaskStatusRunning}, {ID: "other", Status: types.TaskStatusRunning}}}, nil)
	m.selectedTask = "shown"
	action := ActionSpec{ID: "task.redirect", Title: "Redirect", Target: "run", Scope: "owner_user (server-enforced)", Method: methods.MethodRedirect, Confirmation: ConfirmExplicit, Reconcile: ReconcileAccepted}
	intent, err := m.buildIntent(action, "exact goal")
	if err != nil {
		t.Fatal(err)
	}
	m.selectedTask = "other"
	next, cmd := m.executeIntent(intent)
	m = next.(RuntimeModel)
	if cmd == nil {
		t.Fatal("exact intent did not execute")
	}
	m = drive(t, m, cmd())
	if !containsCall(controller.calls, "control:redirect:shown:owner_user") || controller.controlPayloads[len(controller.controlPayloads)-1]["goal"] != "exact goal" {
		t.Fatalf("calls=%v payloads=%v", controller.calls, controller.controlPayloads)
	}
	payload := intent.Payload()
	payload["goal"] = "mutated"
	if intent.Payload()["goal"] != "exact goal" {
		t.Fatal("intent payload was mutable")
	}
	intent.Generation--
	next, cmd = m.executeIntent(intent)
	m = next.(RuntimeModel)
	if cmd != nil || !strings.Contains(m.shell.state.Toast, "stale action intent") {
		t.Fatalf("stale execution cmd=%v toast=%q", cmd, m.shell.state.Toast)
	}
}

func TestActionIntent_N128MixedIdentityGenerationAndTargets(t *testing.T) {
	base, _, _ := operationalModel(t)
	var wait sync.WaitGroup
	errorsCh := make(chan error, 128)
	for index := range 128 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			m := base
			m.identity = types.IdentityScope{Tenant: fmt.Sprintf("tenant-%d", index%8), User: fmt.Sprintf("user-%d", index), Session: fmt.Sprintf("session-%d", index)}
			m.generation, m.inspectEpoch = uint64(index+1), uint64(index+1000)
			m.selectedTask = fmt.Sprintf("run-%d", index)
			intent, err := m.buildIntent(ActionSpec{ID: "task.message", Title: "Message", Target: "run", Scope: "session_user (server-enforced)", Method: methods.MethodUserMessage, Confirmation: ConfirmNone}, fmt.Sprintf("input-%d", index))
			if err != nil {
				errorsCh <- err
				return
			}
			if intent.RunID != fmt.Sprintf("run-%d", index) || intent.Identity.Session != fmt.Sprintf("session-%d", index) || intent.Generation != uint64(index+1) || intent.RequestEpoch != uint64(index+1000) || intent.Payload()["message"] != fmt.Sprintf("input-%d", index) {
				errorsCh <- fmt.Errorf("mixed intent leaked at %d: %#v", index, intent)
			}
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
}

func TestActionMatrix_LifecycleAndInterventionKindsDisableInvalidControls(t *testing.T) {
	terminal := types.TaskRow{ID: "done", Status: types.TaskStatusComplete}
	resolved := resolveActions(map[types.Capability]bool{types.CapTaskControl: true}, &terminal, "OAuth required", "pause", "", "")
	for _, action := range resolved {
		if (action.ID == "task.cancel" || action.ID == "task.pause" || action.ID == "intervention.approve" || action.ID == "intervention.reject") && action.DisabledReason == "" {
			t.Fatalf("action should be lifecycle/kind disabled: %#v", action)
		}
	}
}
