package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

var errStaleActionIntent = errors.New("stale action intent: target context changed; reopen the action")

// ActionIntent is the immutable value displayed by confirmation and executed
// by the mutation executor. Payload is frozen JSON so later model updates cannot
// alter a target, PauseToken, input, or decision.
type ActionIntent struct {
	ActionID, Title, RequiredAuthority string
	Identity                           types.IdentityScope
	Generation, RequestEpoch           uint64
	Method                             methods.Method
	RunID, PauseToken                  string
	ToolID, ArtifactID, SessionID      string
	Input                              string
	Confirmation                       Confirmation
	payload                            json.RawMessage
}

func (i ActionIntent) Payload() map[string]any {
	if len(i.payload) == 0 {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(i.payload, &out) != nil {
		return nil
	}
	return out
}

func freezePayload(payload map[string]any) (json.RawMessage, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("freeze action payload: %w", err)
	}
	return append(json.RawMessage(nil), body...), nil
}

// Confirmation is the action's required operator confirmation level.
type Confirmation string

const (
	ConfirmNone        Confirmation = "none"
	ConfirmExplicit    Confirmation = "explicit"
	ConfirmDestructive Confirmation = "destructive"
)

// ReconcileMode describes what may update optimistically before canonical state.
type ReconcileMode string

const (
	ReconcileNone     ReconcileMode = "no optimistic mutation"
	ReconcileAccepted ReconcileMode = "show accepted pending canonical event"
)

// ActionSpec is the closed action matrix for every displayed mutation.
type ActionSpec struct {
	ID, Title, Target, Scope, Outcome, DisabledReason string
	Method                                            methods.Method
	Capability                                        types.Capability
	Confirmation                                      Confirmation
	Reconcile                                         ReconcileMode
}

// ActionMatrix returns a copied deterministic matrix. No UI action exists
// outside this inventory and no entry names a non-canonical method.
func ActionMatrix() []ActionSpec {
	actions := []ActionSpec{
		{ID: "task.cancel", Title: "Cancel task", Target: "run", Scope: "owner_user (server-enforced)", Outcome: "control.applied or control.rejected", Method: methods.MethodCancel, Capability: types.CapTaskControl, Confirmation: ConfirmDestructive, Reconcile: ReconcileAccepted},
		{ID: "task.pause", Title: "Pause task", Target: "run", Scope: "owner_user (server-enforced)", Outcome: "pause.requested plus control.applied", Method: methods.MethodPause, Capability: types.CapTaskControl, Confirmation: ConfirmExplicit, Reconcile: ReconcileAccepted},
		{ID: "task.resume", Title: "Resume task", Target: "run and PauseToken", Scope: "owner_user (server-enforced)", Outcome: "pause.resumed plus control.applied", Method: methods.MethodResume, Capability: types.CapTaskControl, Confirmation: ConfirmNone, Reconcile: ReconcileAccepted},
		{ID: "task.redirect", Title: "Redirect task", Target: "run", Scope: "owner_user (server-enforced)", Outcome: "control.applied or control.rejected", Method: methods.MethodRedirect, Capability: types.CapTaskControl, Confirmation: ConfirmExplicit, Reconcile: ReconcileAccepted},
		{ID: "task.inject", Title: "Inject context", Target: "run", Scope: "session_user (server-enforced)", Outcome: "control.applied or control.rejected", Method: methods.MethodInjectContext, Capability: types.CapTaskControl, Confirmation: ConfirmNone, Reconcile: ReconcileAccepted},
		{ID: "task.message", Title: "Send user message", Target: "run", Scope: "session_user (server-enforced)", Outcome: "control.applied or control.rejected", Method: methods.MethodUserMessage, Capability: types.CapTaskControl, Confirmation: ConfirmNone, Reconcile: ReconcileAccepted},
		{ID: "task.prioritize", Title: "Prioritize task", Target: "run", Scope: "admin JWT claim (server-enforced; client authority unknown)", Outcome: "control.applied or control.rejected", Method: methods.MethodPrioritize, Capability: types.CapTaskControl, Confirmation: ConfirmExplicit, Reconcile: ReconcileAccepted},
		{ID: "intervention.approve", Title: "Approve intervention", Target: "PauseToken", Scope: "owner_user (server-enforced)", Outcome: "pause.resumed with approved decision", Method: methods.MethodApprove, Capability: types.CapTaskControl, Confirmation: ConfirmExplicit, Reconcile: ReconcileAccepted},
		{ID: "intervention.reject", Title: "Reject intervention", Target: "PauseToken", Scope: "owner_user (server-enforced)", Outcome: "pause.resumed with rejected decision", Method: methods.MethodReject, Capability: types.CapTaskControl, Confirmation: ConfirmDestructive, Reconcile: ReconcileAccepted},
		{ID: "intervention.resume", Title: "Resume intervention", Target: "PauseToken", Scope: "owner_user (server-enforced)", Outcome: "pause.resumed", Method: methods.MethodResume, Capability: types.CapTaskControl, Confirmation: ConfirmNone, Reconcile: ReconcileAccepted},
		{ID: "intervention.oauth", Title: "Submit OAuth intervention", Target: "PauseToken", Scope: "owner_user (server-enforced)", Outcome: "pause.resumed with operator OAuth input", Method: methods.MethodResume, Capability: types.CapTaskControl, Confirmation: ConfirmExplicit, Reconcile: ReconcileAccepted},
		{ID: "intervention.input", Title: "Submit required input", Target: "PauseToken", Scope: "owner_user (server-enforced)", Outcome: "pause.resumed with operator input", Method: methods.MethodResume, Capability: types.CapTaskControl, Confirmation: ConfirmExplicit, Reconcile: ReconcileAccepted},
		{ID: "tool.policy", Title: "Set tool approval policy", Target: "tool", Scope: "admin JWT claim (server-enforced; client authority unknown)", Outcome: "tools.set_approval_policy response then canonical catalog reconciliation", Method: methods.MethodToolsSetApprovalPolicy, Capability: types.CapToolAnnotations, Confirmation: ConfirmExplicit, Reconcile: ReconcileAccepted},
		{ID: "tool.oauth.revoke", Title: "Revoke tool OAuth", Target: "tool", Scope: "admin JWT claim (server-enforced; client authority unknown)", Outcome: "tools.revoke_oauth response then canonical catalog reconciliation", Method: methods.MethodToolsRevokeOAuth, Capability: types.CapToolAnnotations, Confirmation: ConfirmDestructive, Reconcile: ReconcileAccepted},
		{ID: "artifact.delete", Title: "Delete artifact", Target: "artifact", Scope: "admin JWT claim (server-enforced; client authority unknown)", Outcome: "artifacts.delete response then canonical catalog reconciliation", Method: methods.MethodArtifactsDelete, Confirmation: ConfirmDestructive, Reconcile: ReconcileAccepted},
		{ID: "session.delete", Title: "Delete session", Target: "identity triple", Scope: "verified own session (server-enforced)", Outcome: "sessions.delete response then session_erased reconciliation", Method: methods.MethodSessionsDelete, Capability: types.CapSessionLifecycle, Confirmation: ConfirmDestructive, Reconcile: ReconcileAccepted},
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i].ID < actions[j].ID })
	return append([]ActionSpec(nil), actions...)
}

func resolveActions(capabilities map[types.Capability]bool, task *types.TaskRow, interventionKind string, token, toolID, artifactID string) []ActionSpec {
	actions := ActionMatrix()
	for index := range actions {
		action := &actions[index]
		if action.Capability != "" && !capabilities[action.Capability] {
			action.DisabledReason = "Runtime does not advertise " + string(action.Capability)
			continue
		}
		switch action.Target {
		case "run":
			if task == nil {
				action.DisabledReason = "no active task target"
				continue
			}
			allowed := map[methods.Method]map[types.TaskStatus]bool{
				methods.MethodCancel:        {types.TaskStatusPending: true, types.TaskStatusRunning: true, types.TaskStatusPaused: true},
				methods.MethodPause:         {types.TaskStatusRunning: true},
				methods.MethodRedirect:      {types.TaskStatusRunning: true, types.TaskStatusPaused: true},
				methods.MethodInjectContext: {types.TaskStatusRunning: true, types.TaskStatusPaused: true},
				methods.MethodUserMessage:   {types.TaskStatusRunning: true, types.TaskStatusPaused: true},
				methods.MethodPrioritize:    {types.TaskStatusPending: true, types.TaskStatusRunning: true, types.TaskStatusPaused: true},
			}
			if statuses, known := allowed[action.Method]; known && !statuses[task.Status] {
				action.DisabledReason = fmt.Sprintf("task %s is %s", task.ID, task.Status)
			}
		case "run and PauseToken", "PauseToken":
			switch {
			case token == "":
				action.DisabledReason = "no unresolved PauseToken target"
			case (action.Method == methods.MethodApprove || action.Method == methods.MethodReject) && interventionKind != "approval required":
				action.DisabledReason = "selected intervention is " + interventionKind + "; approve/reject requires approval"
			case action.ID == "intervention.oauth" && interventionKind != "OAuth required":
				action.DisabledReason = "selected intervention is " + interventionKind + "; OAuth input requires OAuth"
			case action.ID == "intervention.input" && interventionKind != "input required":
				action.DisabledReason = "selected intervention is " + interventionKind + "; input response requires input"
			}
		case "tool":
			if toolID == "" {
				action.DisabledReason = "no tool selected"
			}
		case "artifact":
			if artifactID == "" {
				action.DisabledReason = "no artifact selected"
			}
		}
	}
	return actions
}

func actionDescription(action ActionSpec) string {
	if action.DisabledReason != "" {
		return "Unavailable: " + action.DisabledReason
	}
	capability := "canonical method"
	if action.Capability != "" {
		capability = "capability " + string(action.Capability)
	}
	return fmt.Sprintf("%s · %s · scope %s · outcome %s · confirm %s · %s", action.Method, capability, action.Scope, action.Outcome, action.Confirmation, action.Reconcile)
}
