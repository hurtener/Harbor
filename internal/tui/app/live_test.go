package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	protocolclient "github.com/hurtener/Harbor/internal/protocol/client"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	tuiartifacts "github.com/hurtener/Harbor/internal/tui/artifacts"
	"github.com/hurtener/Harbor/internal/tui/composer"
	"github.com/hurtener/Harbor/internal/tui/conversation"
	tuievents "github.com/hurtener/Harbor/internal/tui/events"
	"github.com/hurtener/Harbor/internal/tui/interventions"
	"github.com/hurtener/Harbor/internal/tui/projection"
	tuitasks "github.com/hurtener/Harbor/internal/tui/tasks"
	tuitools "github.com/hurtener/Harbor/internal/tui/tools"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

type recordingController struct {
	id                                    types.IdentityScope
	projection                            projection.Projection
	runtimeData                           conversation.RuntimeData
	calls                                 []string
	controlPayloads                       []map[string]any
	denyCapability, failUpload, failStart bool
}

func (c *recordingController) Attach(context.Context) error {
	c.calls = append(c.calls, "attach")
	return nil
}
func (c *recordingController) Switch(_ context.Context, id types.IdentityScope) error {
	c.calls = append(c.calls, "switch:"+id.Session)
	c.id = id
	c.projection.Identity = id
	return nil
}
func (c *recordingController) Start(_ context.Context, text string, ids []string, dispositions map[string]string) (types.StartResponse, error) {
	c.calls = append(c.calls, "start:"+text+":"+strings.Join(ids, ",")+":"+dispositions["artifact-upload"])
	if c.failStart {
		return types.StartResponse{}, errors.New("start failed")
	}
	return types.StartResponse{TaskID: "task"}, nil
}
func (c *recordingController) Sessions(_ context.Context, query, cursor string) (types.SessionsListResponse, error) {
	c.calls = append(c.calls, "sessions:"+query+":"+cursor)
	return types.SessionsListResponse{Rows: []types.SessionRow{{SessionID: c.id.Session, Status: types.SessionStatusRunning}, {SessionID: "s2", Status: types.SessionStatusCompleted, Title: "Second"}}}, nil
}
func (c *recordingController) Rename(_ context.Context, title string) (types.SessionsSetTitleResponse, error) {
	c.calls = append(c.calls, "rename:"+title)
	return types.SessionsSetTitleResponse{SessionID: c.id.Session, Title: title}, nil
}
func (c *recordingController) Delete(context.Context) (types.SessionsDeleteResponse, error) {
	c.calls = append(c.calls, "delete:"+c.id.Session)
	return types.SessionsDeleteResponse{SessionID: c.id.Session, Deleted: true}, nil
}
func (c *recordingController) Upload(_ context.Context, name, mime string, body []byte) (types.ArtifactsPutResponse, error) {
	c.calls = append(c.calls, "upload:"+name+":"+string(body))
	if c.failUpload {
		return types.ArtifactsPutResponse{}, errors.New("upload failed")
	}
	return types.ArtifactsPutResponse{Ref: types.ArtifactRef{ID: "artifact-upload", Filename: name, MimeType: mime}}, nil
}
func (c *recordingController) ReplaceToken(context.Context, string) error {
	c.calls = append(c.calls, "replace-token")
	return nil
}
func (c *recordingController) Identity() types.IdentityScope       { return c.id }
func (c *recordingController) Projection() projection.Projection   { return c.projection }
func (c *recordingController) HasCapability(types.Capability) bool { return !c.denyCapability }
func (c *recordingController) Inspect(_ context.Context, request conversation.InspectionRequest) conversation.RuntimeData {
	data := c.runtimeData
	data.Identity, data.Generation, data.RequestEpoch = c.id, request.Generation, request.RequestEpoch
	return data
}
func (c *recordingController) Execute(ctx context.Context, mutation conversation.Mutation) error {
	switch mutation.Method {
	case methods.MethodArtifactsDelete:
		_, err := c.DeleteArtifact(ctx, mutation.ArtifactID)
		return err
	case methods.MethodToolsSetApprovalPolicy:
		policy, _ := mutation.Payload["policy"].(string)
		_, err := c.SetToolApproval(ctx, mutation.ToolID, types.ToolApprovalPolicy(policy))
		return err
	case methods.MethodToolsRevokeOAuth:
		_, err := c.RevokeToolOAuth(ctx, mutation.ToolID)
		return err
	case methods.MethodSessionsDelete:
		_, err := c.Delete(ctx)
		return err
	default:
		_, err := c.Control(ctx, mutation.Method, mutation.RunID, testAuthority(mutation.Method), mutation.Payload)
		return err
	}
}
func testAuthority(method methods.Method) string {
	switch method {
	case methods.MethodInjectContext, methods.MethodUserMessage:
		return "session_user"
	case methods.MethodPrioritize:
		return "admin"
	default:
		return "owner_user"
	}
}
func (c *recordingController) TaskDetail(context.Context, string) (types.TaskDetail, error) {
	return types.TaskDetail{}, nil
}
func (c *recordingController) Control(_ context.Context, method methods.Method, run, scope string, payload map[string]any) (types.ControlResponse, error) {
	c.calls = append(c.calls, "control:"+string(method)+":"+run+":"+scope)
	c.controlPayloads = append(c.controlPayloads, payload)
	return types.ControlResponse{Accepted: true, Method: string(method)}, nil
}

func TestRuntimeModel_ConfirmedActionPreservesInputAndPauseTokenTarget(t *testing.T) {
	m, controller, _ := operationalModel(t)
	m.runtime.Tasks = tuitasks.Derive(types.TaskListResponse{Rows: []types.TaskRow{{ID: "task-target", Status: types.TaskStatusRunning}}}, nil)
	m.selectedTask = "task-target"
	m.runtime.Interventions = interventions.New().Reconcile([]types.PauseSnapshot{{Token: "pause-target", Reason: "approval_required", State: types.PauseStatePaused, Identity: types.IdentityScope{Run: "intervention-run"}}}, 1)
	m.selectedPause = "pause-target"
	m.runtime.Interventions = m.runtime.Interventions.Select(m.selectedPause)
	controller.runtimeData = m.runtime
	action := ActionSpec{ID: "task.redirect", Title: "Redirect task", Target: "run", Scope: "owner_user", Method: methods.MethodRedirect, Confirmation: ConfirmExplicit, Reconcile: ReconcileAccepted}
	m.activeAction = &action
	m.actionInput = "preserved goal"
	intent, err := m.buildIntent(action, "preserved goal")
	if err != nil {
		t.Fatal(err)
	}
	m.activeIntent = &intent
	m.openActionConfirm(intent)
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if !containsCall(controller.calls, "control:redirect:task-target:owner_user") || len(controller.controlPayloads) == 0 || controller.controlPayloads[len(controller.controlPayloads)-1]["goal"] != "preserved goal" {
		t.Fatalf("calls=%v payloads=%v", controller.calls, controller.controlPayloads)
	}
	reject := ActionSpec{ID: "intervention.reject", Title: "Reject", Target: "PauseToken", Scope: "owner_user", Method: methods.MethodReject, Confirmation: ConfirmDestructive, Reconcile: ReconcileAccepted}
	next, cmd := m.executeActionSpec(reject, "unsafe request")
	m = next.(RuntimeModel)
	m = drive(t, m, cmd())
	payload := controller.controlPayloads[len(controller.controlPayloads)-1]
	if !containsCall(controller.calls, "control:reject:intervention-run:owner_user") || payload["token"] != "pause-target" || payload["reason"] != "unsafe request" {
		t.Fatalf("calls=%v payload=%v", controller.calls, payload)
	}
}
func (c *recordingController) DeleteArtifact(_ context.Context, id string) (types.ArtifactsDeleteResponse, error) {
	c.calls = append(c.calls, "artifact-delete:"+id)
	return types.ArtifactsDeleteResponse{Deleted: true}, nil
}
func (c *recordingController) SetToolApproval(_ context.Context, id string, policy types.ToolApprovalPolicy) (types.ToolSetApprovalPolicyResponse, error) {
	c.calls = append(c.calls, "tool-policy:"+id+":"+string(policy))
	return types.ToolSetApprovalPolicyResponse{ID: id, Policy: policy}, nil
}
func (c *recordingController) RevokeToolOAuth(_ context.Context, id string) (types.ToolRevokeOAuthResponse, error) {
	c.calls = append(c.calls, "tool-oauth-revoke:"+id)
	return types.ToolRevokeOAuthResponse{ID: id}, nil
}

type recordingStore struct {
	states []conversation.InteractionState
}

func (s *recordingStore) Save(state conversation.InteractionState) error {
	s.states = append(s.states, state)
	return nil
}

func operationalModel(t *testing.T) (RuntimeModel, *recordingController, *recordingStore) {
	t.Helper()
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	p := projection.Projection{Identity: id, Blocks: []projection.Block{{ID: "user", Kind: "user", Text: "hello"}, {ID: "reason", Kind: "reasoning", Text: "thinking"}, {ID: "tool", Kind: "tool", Tool: "lookup"}}}
	controller := &recordingController{id: id, projection: p}
	store := &recordingStore{}
	m := NewRuntimeModel(t.Context(), 100, 30, ui.NewTheme(ui.ModeDark, ui.ProfileMono), controller, conversation.ChannelSource(make(chan conversation.Update, 16)), RuntimeOptions{Fingerprint: "runtime", ExportPath: filepath.Join(t.TempDir(), "export.md"), Store: store})
	m.applyUpdate(conversation.Update{Identity: id, Generation: 1, State: conversation.StateLive, Projection: p})
	return m, controller, store
}

func drive(t *testing.T, m RuntimeModel, msg tea.Msg) RuntimeModel {
	t.Helper()
	next, cmd := m.Update(msg)
	m = next.(RuntimeModel)
	for cmd != nil {
		produced := cmd()
		if produced == nil {
			return m
		}
		next, cmd = m.Update(produced)
		m = next.(RuntimeModel)
	}
	return m
}
func leader(t *testing.T, m RuntimeModel, key rune) RuntimeModel {
	t.Helper()
	next, _ := m.Update(keyMsg('x', tea.ModCtrl))
	m = next.(RuntimeModel)
	return drive(t, m, keyMsg(key, 0))
}

func TestApplyUpdate_HonestyStateMapping(t *testing.T) {
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	base := projection.Projection{Identity: id, LastSequence: 5}

	failed := base
	failed.SessionStatus = string(types.SessionStatusFailed)
	completed := base
	completed.SessionStatus = string(types.SessionStatusCompleted)
	dropped := base
	dropped.ReplayGap = &projection.ReplayGap{Reason: "live_journal_overflow", LastSequence: 5}
	gap := base
	gap.ReplayGap = &projection.ReplayGap{Reason: "unseen_out_of_order", LastSequence: 5, SeenSequence: 3}
	partial := base
	partial.HistoryTruncated, partial.AggregateTruncated = true, true
	partial.CountersPartial, partial.ToolsAggregatesPartial, partial.ToolAnalyticsBounded = true, true, true

	cases := []struct {
		name   string
		p      projection.Projection
		assert func(*testing.T, State)
	}{
		{"failed-is-not-closed", failed, func(t *testing.T, s State) {
			if s.Closed || !s.Failed {
				t.Fatalf("failed session must set Failed, not Closed: closed=%v failed=%v", s.Closed, s.Failed)
			}
		}},
		{"completed-is-closed-not-failed", completed, func(t *testing.T, s State) {
			if !s.Closed || s.Failed {
				t.Fatalf("completed session must set Closed, not Failed: closed=%v failed=%v", s.Closed, s.Failed)
			}
		}},
		{"overflow-drop-is-dropped-not-replaygap", dropped, func(t *testing.T, s State) {
			if !s.Dropped || s.ReplayGap {
				t.Fatalf("event-window drop must set Dropped, not ReplayGap: dropped=%v replaygap=%v", s.Dropped, s.ReplayGap)
			}
		}},
		{"ordering-gap-is-replaygap-not-dropped", gap, func(t *testing.T, s State) {
			if s.Dropped || !s.ReplayGap {
				t.Fatalf("ordering gap must set ReplayGap, not Dropped: dropped=%v replaygap=%v", s.Dropped, s.ReplayGap)
			}
		}},
		{"partiality-kinds-are-distinct", partial, func(t *testing.T, s State) {
			if !s.Truncated || !s.AggregateTruncated || !s.CountersPartial || !s.AggregatesPartial || !s.AnalyticsBounded {
				t.Fatalf("each partiality kind must stay distinct: %+v", s)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			controller := &recordingController{id: id, projection: tc.p}
			m := NewRuntimeModel(t.Context(), 100, 30, ui.NewTheme(ui.ModeDark, ui.ProfileMono), controller, conversation.ChannelSource(make(chan conversation.Update, 4)), RuntimeOptions{Fingerprint: "runtime"})
			m.applyUpdate(conversation.Update{Identity: id, Generation: 1, State: conversation.StateLive, Projection: tc.p})
			tc.assert(t, m.shell.state)
		})
	}
}

func TestApplyUpdate_DoesNotRegressToOlderProjection(t *testing.T) {
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	controller := &recordingController{id: id}
	m := NewRuntimeModel(t.Context(), 100, 30, ui.NewTheme(ui.ModeDark, ui.ProfileMono), controller, conversation.ChannelSource(make(chan conversation.Update, 4)), RuntimeOptions{Fingerprint: "runtime"})

	newer := projection.Projection{Identity: id, LastSequence: 10, Blocks: []projection.Block{{ID: "tool", Kind: "tool", Tool: "lookup", Status: "completed"}}}
	older := projection.Projection{Identity: id, LastSequence: 7, Blocks: []projection.Block{{ID: "tool", Kind: "tool", Tool: "lookup", Status: "running"}}}
	m.applyUpdate(conversation.Update{Identity: id, Generation: 1, State: conversation.StateLive, Projection: newer})
	m.applyUpdate(conversation.Update{Identity: id, Generation: 1, State: conversation.StateLive, Projection: older})

	if got := m.transcript.Projection.LastSequence; got != 10 {
		t.Fatalf("stale projection at seq 7 must not replace the seq-10 frame; got seq %d", got)
	}
	if status := m.transcript.Projection.Blocks[0].Status; status != "completed" {
		t.Fatalf("completed tool must not regress to %q from a stale frame", status)
	}
}

func runtimeModelForTest(t *testing.T, compact bool) RuntimeModel {
	t.Helper()
	id := types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
	controller, err := conversation.NewController("http://example.test", conversation.NewTokenSource("", "unused"), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	return NewRuntimeModel(t.Context(), 80, 24, ui.NewTheme(ui.ModeDark, ui.ProfileMono), controller, conversation.ChannelSource(make(chan conversation.Update, 4)), RuntimeOptions{Compact: compact, ReducedMotion: true})
}

func TestRuntimeModel_KeyDrivenSessionDialogsCallController(t *testing.T) {
	m, controller, store := operationalModel(t)
	m = leader(t, m, 'l')
	modal, ok := m.shell.focus.Top()
	if !ok || modal.Title != "Sessions" {
		t.Fatalf("modal=%#v", modal)
	}
	m = drive(t, m, keyMsg(tea.KeyDown, 0))
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if controller.id.Session != "s2" || !containsCall(controller.calls, "switch:s2") {
		t.Fatalf("switch calls=%v", controller.calls)
	}
	m = leader(t, m, 'l')
	m = drive(t, m, keyMsg('r', tea.ModCtrl))
	for _, r := range "renamed" {
		m = drive(t, m, keyMsg(r, 0))
	}
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if !containsCall(controller.calls, "rename:renamed") {
		t.Fatalf("rename calls=%v", controller.calls)
	}
	m = leader(t, m, 'l')
	m = drive(t, m, keyMsg('d', tea.ModCtrl))
	if !strings.Contains(m.shell.state.Toast, "attach the target session") {
		t.Fatalf("non-active delete was not rejected: calls=%v toast=%q", controller.calls, m.shell.state.Toast)
	}
	m = drive(t, m, keyMsg(tea.KeyEscape, 0))
	m = leader(t, m, 'n')
	for _, r := range "fresh" {
		m = drive(t, m, keyMsg(r, 0))
	}
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if !containsCall(controller.calls, "switch:fresh") {
		t.Fatalf("fresh calls=%v", controller.calls)
	}
	if len(store.states) == 0 {
		t.Fatal("session state was not persisted")
	}
}

func TestRuntimeModel_KeyDrivenComposerAutocompleteSearchAttachmentExportAndPrefs(t *testing.T) {
	m, controller, store := operationalModel(t)
	file := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(file, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, r := range "draft" {
		m = drive(t, m, keyMsg(r, 0))
	}
	m = drive(t, m, keyMsg(tea.KeyLeft, 0))
	m = drive(t, m, keyMsg(tea.KeyBackspace, 0))
	m = drive(t, m, tea.PasteMsg{Content: "ft\nline"})
	m = drive(t, m, keyMsg('_', tea.ModCtrl))
	m = drive(t, m, keyMsg('_', tea.ModAlt))
	m = leader(t, m, 'b')
	m = leader(t, m, 'p')
	m = drive(t, m, keyMsg('e', tea.ModCtrl))
	m = drive(t, m, tea.PasteMsg{Content: " @"})
	if !m.shell.state.AutocompleteOpen {
		t.Fatal("autocomplete did not open")
	}
	m = drive(t, m, keyMsg(tea.KeyDown, 0))
	m = drive(t, m, keyMsg(tea.KeyTab, 0))
	if !strings.Contains(m.editor.Text(), "@") {
		t.Fatalf("completion=%q", m.editor.Text())
	}
	m = leader(t, m, 'f')
	for _, r := range "hello" {
		m = drive(t, m, keyMsg(r, 0))
	}
	if !m.searchMode || len(m.transcript.Matches) != 1 {
		t.Fatalf("search=%q matches=%v", m.searchQuery, m.transcript.Matches)
	}
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	m = drive(t, m, keyMsg(tea.KeyPgUp, 0))
	m = drive(t, m, keyMsg('j', tea.ModAlt))
	m = leader(t, m, 'r')
	m = leader(t, m, 'o')
	m = leader(t, m, 'c')
	m = leader(t, m, 'm')
	m = leader(t, m, 's')
	m = leader(t, m, 'a')
	spec := file + "|ref"
	for _, r := range spec {
		m = drive(t, m, keyMsg(r, 0))
	}
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if !containsPrefix(controller.calls, "upload:note.txt:payload") {
		t.Fatalf("upload calls=%v", controller.calls)
	}
	if len(m.editor.Attachments()) != 1 || m.editor.Attachments()[0].ID != "artifact-upload" {
		t.Fatalf("attachments=%#v", m.editor.Attachments())
	}
	m = leader(t, m, 'e')
	if len(m.editor.Attachments()) != 0 {
		t.Fatal("attachment remove command did not update editor")
	}
	controller.failUpload = true
	m = leader(t, m, 'a')
	for _, r := range spec {
		m = drive(t, m, keyMsg(r, 0))
	}
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	controller.failUpload = false
	m = leader(t, m, 'u')
	if len(m.editor.Attachments()) != 1 || m.editor.Attachments()[0].Failed {
		t.Fatalf("attachment retry=%#v", m.editor.Attachments())
	}
	m = leader(t, m, 'x')
	body, err := os.ReadFile(m.exportPath)
	if err != nil || !strings.Contains(string(body), "Harbor session") {
		t.Fatalf("export=%q %v", body, err)
	}
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if !containsPrefix(controller.calls, "start:") {
		t.Fatalf("start calls=%v", controller.calls)
	}
	if len(store.states) == 0 {
		t.Fatal("preferences not persisted")
	}
	last := store.states[len(store.states)-1]
	if last.RuntimeFingerprint != "runtime" || last.SidebarWidth != ui.SidebarWidth {
		t.Fatalf("state=%#v", last)
	}
}

func TestRuntimeModel_KeyDrivenReauthenticationAndCapabilityReasons(t *testing.T) {
	m, controller, _ := operationalModel(t)
	m = leader(t, m, 'i')
	m = drive(t, m, tea.PasteMsg{Content: "replacement.jwt.value"})
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if !containsCall(controller.calls, "replace-token") {
		t.Fatalf("replacement calls=%v", controller.calls)
	}
	controller.denyCapability = true
	m.syncAccess()
	if view, ok := m.shell.registry.Command("session-delete", m.shell.commandContext()); !ok || view.Enabled || !strings.Contains(view.DisabledReason, "session_lifecycle") {
		t.Fatalf("delete command=%#v", view)
	}
	if view, ok := m.shell.registry.Command("sessions", m.shell.commandContext()); !ok || !view.Enabled {
		t.Fatalf("sessions command must defer authorization to Protocol=%#v", view)
	}
	if view, ok := m.shell.registry.Command("submit", m.shell.commandContext()); !ok || view.Enabled || !strings.Contains(view.DisabledReason, "task_control") {
		t.Fatalf("submit command=%#v", view)
	}
}

func TestRuntimeModel_RuntimeRoutesActionMatrixAndDestructiveConfirmation(t *testing.T) {
	m, controller, _ := operationalModel(t)
	m.runtime.Tasks = tuitasks.Derive(types.TaskListResponse{Rows: []types.TaskRow{{ID: "run-1", Status: types.TaskStatusRunning}}}, nil)
	m.selectedTask = "run-1"
	m.runtime.Interventions = interventions.New().Reconcile([]types.PauseSnapshot{{Token: "pause-1", State: types.PauseStatePaused, Identity: types.IdentityScope{Run: "run-1"}}}, 1)
	controller.runtimeData = m.runtime
	m = drive(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyF2}))
	if m.shell.state.Route != "tasks" || !strings.Contains(strings.Join(m.shell.state.DetailRows, " "), "run-1") {
		t.Fatalf("route=%s rows=%v", m.shell.state.Route, m.shell.state.DetailRows)
	}
	m = drive(t, m, tea.KeyPressMsg(tea.Key{Code: tea.KeyF9}))
	modal, ok := m.shell.focus.Top()
	if !ok || modal.Title != "Runtime actions" {
		t.Fatalf("modal=%#v", modal)
	}
	m.closeModal()
	action := ActionSpec{ID: "task.cancel", Title: "Cancel task", Target: "run", Scope: "session_user", Method: methods.MethodCancel, Confirmation: ConfirmDestructive, Reconcile: ReconcileAccepted}
	m.activeAction = &action
	intent, err := m.buildIntent(action, "")
	if err != nil {
		t.Fatal(err)
	}
	m.activeIntent = &intent
	m.openActionConfirm(intent)
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if !containsCall(controller.calls, "control:cancel:run-1:owner_user") {
		t.Fatalf("calls=%v", controller.calls)
	}
	if !strings.Contains(m.shell.state.Toast, "accepted") {
		t.Fatalf("toast=%q", m.shell.state.Toast)
	}
}

func TestRuntimeModel_AllRuntimeRouteDerivationsAndActionExecutors(t *testing.T) {
	m, controller, _ := operationalModel(t)
	m.runtime.Tasks = tuitasks.Derive(types.TaskListResponse{Rows: []types.TaskRow{{ID: "run-1", Status: types.TaskStatusRunning}}}, nil)
	m.runtime.Tools = tuitools.Derive(types.ToolListResponse{Tools: []types.Tool{{ID: "tool-1"}}}, true)
	m.runtime.Artifacts = tuiartifacts.Derive(types.ArtifactsListResponse{Rows: []types.ArtifactRow{{Ref: types.ArtifactRef{ID: "artifact-1"}}}})
	m.runtime.Interventions = interventions.New().Reconcile([]types.PauseSnapshot{{Token: "pause-1", State: types.PauseStatePaused, Identity: types.IdentityScope{Run: "run-1"}}}, 1)
	m.selectedTask, m.selectedTool, m.selectedArtifact, m.selectedPause = "run-1", "tool-1", "artifact-1", "pause-1"
	m.runtime.Interventions = m.runtime.Interventions.Select(m.selectedPause)
	controller.runtimeData = m.runtime
	for _, route := range []string{"tasks", "tools", "artifacts", "events", "posture", "interventions", "diagnostics"} {
		m.shell.state.Route = route
		m.syncRuntimeRoute()
		if len(m.shell.state.DetailRows) < 2 {
			t.Fatalf("route %s rows=%v", route, m.shell.state.DetailRows)
		}
	}
	for _, test := range []struct {
		method methods.Method
		input  string
	}{{methods.MethodRedirect, "goal"}, {methods.MethodInjectContext, "context"}, {methods.MethodUserMessage, "message"}, {methods.MethodPrioritize, "4"}, {methods.MethodApprove, ""}, {methods.MethodReject, "reason"}, {methods.MethodResume, ""}, {methods.MethodArtifactsDelete, ""}, {methods.MethodToolsSetApprovalPolicy, "gated"}, {methods.MethodToolsRevokeOAuth, ""}} {
		action := ActionSpec{ID: string(test.method), Title: string(test.method), Target: "run", Scope: "session_user", Method: test.method, Confirmation: ConfirmNone, Reconcile: ReconcileAccepted}
		next, cmd := m.executeActionSpec(action, test.input)
		m = next.(RuntimeModel)
		if cmd == nil {
			t.Fatalf("%s did not dispatch", test.method)
		}
		m = drive(t, m, cmd())
	}
	if !containsPrefix(controller.calls, "artifact-delete:artifact-1") || !containsPrefix(controller.calls, "tool-policy:tool-1:gated") || !containsPrefix(controller.calls, "tool-oauth-revoke:tool-1") {
		t.Fatalf("calls=%v", controller.calls)
	}
}

func TestRuntimeModel_CanonicalAuthFailuresRemainVisible(t *testing.T) {
	m, _, _ := operationalModel(t)
	for _, tc := range []struct {
		status int
		want   string
	}{{401, string(conversation.StateAuthExpired)}, {403, "authorization denied · 403"}, {404, "live"}, {409, "live"}, {501, "live"}} {
		m.shell.state.Connection = "live"
		err := &protocolclient.ProtocolError{Status: tc.status, Message: "denied"}
		m.setError(err)
		if m.shell.state.Connection != tc.want || !strings.Contains(m.shell.state.Toast, fmt.Sprintf("HTTP %d", tc.status)) || !strings.Contains(m.shell.state.Toast, "denied") {
			t.Fatalf("status %d mapped to connection=%q toast=%q", tc.status, m.shell.state.Connection, m.shell.state.Toast)
		}
	}
}

func TestRuntimeModel_RuntimeRouteKeyboardSelectionPagingFilteringAndExport(t *testing.T) {
	m, _, _ := operationalModel(t)
	m.runtime.Tasks = tuitasks.Derive(types.TaskListResponse{Rows: []types.TaskRow{{ID: "task-a", Status: types.TaskStatusRunning}, {ID: "task-b", Status: types.TaskStatusPending}}, Cursor: types.TaskListCursor{NextPageToken: "next"}}, nil)
	m.runtime.Tools = tuitools.Derive(types.ToolListResponse{Tools: []types.Tool{{ID: "tool-a", Name: "alpha"}, {ID: "tool-b", Name: "beta"}}, Page: 1, PageSize: 2}, true)
	m.runtime.Artifacts = tuiartifacts.Derive(types.ArtifactsListResponse{Rows: []types.ArtifactRow{{Ref: types.ArtifactRef{ID: "artifact-a"}}, {Ref: types.ArtifactRef{ID: "artifact-b"}}}})
	m.runtime.Events = tuievents.Derive(types.EventsListResponse{Events: []types.StateEvent{{Sequence: 1, Type: "task.started", Run: "task-a"}, {Sequence: 2, Type: "task.completed", Run: "task-b"}}}, types.EventAggregateResponse{})
	m.runtime.Interventions = interventions.New().Reconcile([]types.PauseSnapshot{{Token: "pause-a", State: types.PauseStatePaused, PausedAt: time.Now()}, {Token: "pause-b", State: types.PauseStatePaused, PausedAt: time.Now().Add(-time.Second)}}, 1)
	m.initializeSelections()
	for _, route := range []string{"tasks", "tools", "artifacts", "events", "interventions"} {
		m.shell.state.Route = route
		before := fmt.Sprint(m.selectedTask, m.selectedTool, m.selectedArtifact, m.runtime.Events.SelectedSequence, m.selectedPause)
		m.moveRouteSelection(1)
		after := fmt.Sprint(m.selectedTask, m.selectedTool, m.selectedArtifact, m.runtime.Events.SelectedSequence, m.selectedPause)
		if before == after {
			t.Fatalf("%s selection did not move", route)
		}
		m.moveRouteSelection(-1)
		m.syncRuntimeRoute()
	}
	m.taskCursors[2] = types.TaskListCursor{NextPageToken: "next"}
	m.shell.state.Route = "tasks"
	m.pageRoute(1)
	if m.runtime.Tasks.Page != 2 || m.selectedTask != "" {
		t.Fatalf("task page=%d selected=%q", m.runtime.Tasks.Page, m.selectedTask)
	}
	m.pageRoute(-1)
	for _, route := range []string{"tools", "artifacts", "events"} {
		m.shell.state.Route = route
		m.pageRoute(1)
	}
	for _, route := range []string{"tasks", "tools", "artifacts", "events"} {
		m.shell.state.Route, m.routeFilter = route, true
		next, _ := m.updateRouteFilter("x")
		m = next.(RuntimeModel)
		if m.currentRouteFilter() != "x" {
			t.Fatalf("%s filter=%q", route, m.currentRouteFilter())
		}
		next, _ = m.updateRouteFilter("backspace")
		m = next.(RuntimeModel)
		next, _ = m.updateRouteFilter("escape")
		m = next.(RuntimeModel)
	}
	m.shell.state.Route = "events"
	m.exportPath = filepath.Join(t.TempDir(), "events.json")
	m = drive(t, m, m.exportCmd()())
	body, err := os.ReadFile(m.exportPath)
	if err != nil || !strings.Contains(string(body), "task.started") {
		t.Fatalf("event export=%q err=%v", body, err)
	}
}

func TestRuntimeModel_SessionDeleteUsesIntentExecutorAndStaleInspectionClosesModal(t *testing.T) {
	m, controller, _ := operationalModel(t)
	next, _ := m.beginSessionDelete()
	m = next.(RuntimeModel)
	modal, ok := m.shell.focus.Top()
	if !ok || modal.Title != "Confirm Runtime action" || m.activeIntent == nil || m.activeIntent.SessionID != "s" {
		t.Fatalf("modal=%#v intent=%#v", modal, m.activeIntent)
	}
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if !containsCall(controller.calls, "delete:s") {
		t.Fatalf("delete did not use executor: %v", controller.calls)
	}
	m, _, _ = operationalModel(t)
	_, _ = m.openActions()
	m.inspectEpoch = 3
	next, _ = m.Update(inspectMsg{data: conversation.RuntimeData{Identity: m.identity, Generation: m.generation, RequestEpoch: 2, Stale: true}})
	m = next.(RuntimeModel)
	if _, open := m.shell.focus.Top(); open {
		t.Fatal("stale inspection left action modal open")
	}
}

func TestRuntimeModel_RuntimeRouteKeysAndInterventionInputWorkflows(t *testing.T) {
	m, _, _ := operationalModel(t)
	m.runtime.Tasks = tuitasks.Derive(types.TaskListResponse{Rows: []types.TaskRow{{ID: "a", Status: types.TaskStatusRunning}, {ID: "b", Status: types.TaskStatusPending}}}, nil)
	m.selectedTask = "a"
	m.shell.state.Route = "tasks"
	for _, key := range []tea.KeyPressMsg{keyMsg(tea.KeyDown, 0), keyMsg(tea.KeyUp, 0), keyMsg(tea.KeyPgDown, 0), keyMsg(tea.KeyPgUp, 0), keyMsg('/', 0)} {
		next, cmd := m.Update(key)
		m = next.(RuntimeModel)
		if cmd != nil {
			_ = cmd()
		}
	}
	if !m.routeFilter {
		t.Fatal("route filter key did not focus filter")
	}
	next, _ := m.Update(keyMsg(tea.KeyEnter, 0))
	m = next.(RuntimeModel)
	m.runtime.Interventions = interventions.New().Reconcile([]types.PauseSnapshot{{Token: "oauth", Reason: "external_event", State: types.PauseStatePaused, Identity: types.IdentityScope{Run: "run"}}}, 1).Select("oauth")
	m.selectedPause = "oauth"
	action := ActionSpec{ID: "intervention.oauth", Title: "Submit OAuth intervention", Target: "PauseToken", Scope: "owner_user", Method: methods.MethodResume, Confirmation: ConfirmExplicit, Reconcile: ReconcileAccepted}
	m.activeAction = &action
	m.shell = m.shell.WithModal(NewSelect("Action input · "+action.Title, nil, "modal"))
	for _, r := range "oauth value" {
		m = drive(t, m, keyMsg(r, 0))
	}
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if m.activeIntent == nil || m.activeIntent.Payload()["oauth_input"] != "oauth value" {
		t.Fatalf("oauth intent=%#v", m.activeIntent)
	}
	m = drive(t, m, keyMsg(tea.KeyDown, 0))
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if m.activeIntent != nil {
		t.Fatal("cancel confirmation retained intent")
	}
}

func TestRuntimeModel_KeyDrivenFollowUpQueueCancelAndDispatch(t *testing.T) {
	m, controller, _ := operationalModel(t)
	m.transcript.Projection.Blocks[0].Status = "running"
	m.shell.state.Composer = ComposerRunning
	for _, r := range "queued" {
		m = drive(t, m, keyMsg(r, 0))
	}
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if len(m.followups.Entries()) != 1 || !strings.Contains(m.shell.state.Toast, "queued locally") {
		t.Fatalf("queue=%#v toast=%q", m.followups.Entries(), m.shell.state.Toast)
	}
	if m.editor.Text() != "queued" || m.editor.DisabledReason() != "follow-up queued locally" {
		t.Fatalf("queued follow-up did not retain exact draft: text=%q disabled=%q", m.editor.Text(), m.editor.DisabledReason())
	}
	m = leader(t, m, 'k')
	if len(m.followups.Entries()) != 0 {
		t.Fatal("queued follow-up was not cancellable")
	}
	if m.editor.Text() != "queued" || m.editor.DisabledReason() != "" {
		t.Fatalf("discard did not restore retained draft: text=%q disabled=%q", m.editor.Text(), m.editor.DisabledReason())
	}
	m.followups = m.followups.Enqueue("dispatch me")
	m.transcript.Projection.Blocks[0].Status = "completed"
	m.shell.state.Composer = ComposerFocused
	cmd := m.dispatchFollowUp()
	if cmd == nil {
		t.Fatal("terminal update did not produce follow-up dispatch")
	}
	m = drive(t, m, cmd())
	if !containsPrefix(controller.calls, "start:dispatch me") || len(m.followups.Entries()) != 0 {
		t.Fatalf("dispatch calls=%v queue=%#v", controller.calls, m.followups.Entries())
	}
}

func containsCall(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func containsPrefix(values []string, want string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, want) {
			return true
		}
	}
	return false
}

func TestRuntimeModel_EditorNavigationUpdatesAndCompactView(t *testing.T) {
	m := runtimeModelForTest(t, true)
	if m.Init() == nil {
		t.Fatal("init missing")
	}
	next, _ := m.Update(attachMsg{err: errors.New("offline")})
	m = next.(RuntimeModel)
	next, _ = m.Update(tea.PasteMsg{Content: "one\ntwo"})
	m = next.(RuntimeModel)
	for _, key := range []tea.KeyPressMsg{{Code: 'a'}, {Code: tea.KeyLeft}, {Code: tea.KeyBackspace}, {Code: tea.KeyPgUp}, {Code: tea.KeyPgDown}, {Code: tea.KeyEnd}} {
		next, _ = m.Update(key)
		m = next.(RuntimeModel)
	}
	p := projection.Projection{Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"}, Blocks: []projection.Block{{ID: "u", Kind: "user", Text: "hello"}}}
	for _, state := range []conversation.ConnectionState{conversation.StateLive, conversation.StateReconnecting, conversation.StateReplaying, conversation.StateAuthExpired, conversation.StateDisconnected, conversation.StateErased} {
		next, _ = m.Update(updateMsg{update: conversation.Update{State: state, Projection: p}})
		m = next.(RuntimeModel)
	}
	next, _ = m.Update(submitMsg{err: errors.New("retry")})
	m = next.(RuntimeModel)
	if m.shell.state.Composer != ComposerRetry {
		t.Fatal("retry hidden")
	}
	if view := m.View(); view.AltScreen {
		t.Fatal("compact uses alternate screen")
	}
}

func TestRuntimeModel_KeyDrivenSelectionReplacesText(t *testing.T) {
	m, _, _ := operationalModel(t)
	for _, r := range "select" {
		m = drive(t, m, keyMsg(r, 0))
	}
	m = drive(t, m, keyMsg(tea.KeyLeft, tea.ModShift))
	m = drive(t, m, keyMsg('X', 0))
	if m.editor.Text() != "selecX" {
		t.Fatalf("selection replacement=%q", m.editor.Text())
	}
}

func TestRuntimeModel_SubmitCommitsOnlyAfterCanonicalSuccess(t *testing.T) {
	m, controller, _ := operationalModel(t)
	for _, r := range "exact payload" {
		m = drive(t, m, keyMsg(r, 0))
	}
	attachment := composer.Attachment{ID: "artifact-upload", Name: "note.txt", Disposition: "context"}
	m.editor = m.editor.SetAttachments([]composer.Attachment{attachment})
	m.syncComposer()
	controller.failStart = true
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if m.editor.Text() != "exact payload" || len(m.editor.Attachments()) != 1 || len(m.editor.HistoryEntries()) != 0 {
		t.Fatalf("failed submit mutated payload: text=%q attachments=%#v history=%#v", m.editor.Text(), m.editor.Attachments(), m.editor.HistoryEntries())
	}
	controller.failStart = false
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if m.editor.Text() != "" || len(m.editor.Attachments()) != 0 || !slices.Equal(m.editor.HistoryEntries(), []string{"exact payload"}) {
		t.Fatalf("successful submit did not commit exactly once: text=%q attachments=%#v history=%#v", m.editor.Text(), m.editor.Attachments(), m.editor.HistoryEntries())
	}
}

func TestRuntimeModel_FailedFollowUpRetryAndDiscardResumeOrder(t *testing.T) {
	m, controller, _ := operationalModel(t)
	m.followups = m.followups.Enqueue("first").Enqueue("second")
	controller.failStart = true
	m = drive(t, m, m.dispatchFollowUp()())
	if m.followups.Entries()[0].State != "failed" {
		t.Fatalf("failure not retained: %#v", m.followups.Entries())
	}
	controller.failStart = false
	m = leader(t, m, 'j')
	entries := m.followups.Entries()
	if len(entries) != 1 || entries[0].Text != "second" || entries[0].State != "dispatching" {
		t.Fatalf("retry did not advance to second intent: calls=%v queue=%#v", controller.calls, entries)
	}
	_, _ = controller.Start(t.Context(), entries[0].Text, entries[0].ArtifactIDs, entries[0].Dispositions)
	m = drive(t, m, followupMsg{id: entries[0].ID})
	if len(m.followups.Entries()) != 0 || !containsPrefix(controller.calls, "start:first") || !containsPrefix(controller.calls, "start:second") {
		t.Fatalf("retry did not resume in order: calls=%v queue=%#v", controller.calls, m.followups.Entries())
	}
	m.followups = m.followups.Enqueue("discard")
	next, entry, _ := m.followups.Begin()
	m.followups = next.Fail(entry.ID, errors.New("discard"))
	m.syncAccess()
	m = leader(t, m, 'k')
	if len(m.followups.Entries()) != 0 {
		t.Fatalf("failed follow-up not discarded: %#v", m.followups.Entries())
	}
}

func TestRuntimeModel_RejectsStaleIdentityAndGenerationUpdates(t *testing.T) {
	m, _, _ := operationalModel(t)
	want := m.transcript.Projection
	other := want.Identity
	other.Session = "other"
	if m.applyUpdate(conversation.Update{Identity: other, Generation: m.generation, State: conversation.StateLive, Projection: projection.Projection{Identity: other, Generation: m.generation}}) {
		t.Fatal("same-generation cross-session update was accepted")
	}
	if m.applyUpdate(conversation.Update{Identity: want.Identity, Generation: m.generation - 1, State: conversation.StateLive, Projection: projection.Projection{Identity: want.Identity}}) {
		t.Fatal("stale generation was accepted")
	}
	if m.transcript.Projection.Identity.Session != want.Identity.Session || len(m.transcript.Projection.Blocks) != len(want.Blocks) {
		t.Fatal("stale update mutated transcript")
	}
}

func TestRuntimeModel_FinalizeSynchronouslyPersistsLatestState(t *testing.T) {
	m, _, store := operationalModel(t)
	m.editor = m.editor.SetText("older draft")
	cmd := m.requestPersist()
	if cmd == nil {
		t.Fatal("persistence command missing")
	}
	m.editor = m.editor.SetText("latest draft at exit")
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	if len(store.states) < 2 || store.states[len(store.states)-1].Draft != "latest draft at exit" {
		t.Fatalf("final save did not win: %#v", store.states)
	}
}
