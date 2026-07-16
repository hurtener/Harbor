package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	protocolclient "github.com/hurtener/Harbor/internal/protocol/client"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/composer"
	"github.com/hurtener/Harbor/internal/tui/conversation"
	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/renderers"
	"github.com/hurtener/Harbor/internal/tui/sessionpicker"
	"github.com/hurtener/Harbor/internal/tui/surface"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

type conversationController interface {
	Attach(context.Context) error
	Switch(context.Context, types.IdentityScope) error
	Start(context.Context, string, []string, map[string]string) (types.StartResponse, error)
	Sessions(context.Context, string, string) (types.SessionsListResponse, error)
	Rename(context.Context, string) (types.SessionsSetTitleResponse, error)
	Delete(context.Context) (types.SessionsDeleteResponse, error)
	Upload(context.Context, string, string, []byte) (types.ArtifactsPutResponse, error)
	ReplaceToken(context.Context, string) error
	Identity() types.IdentityScope
	Projection() projection.Projection
	HasCapability(types.Capability) bool
	Inspect(context.Context, conversation.InspectionRequest) conversation.RuntimeData
	Execute(context.Context, conversation.Mutation) error
	TaskDetail(context.Context, string) (types.TaskDetail, error)
	Control(context.Context, methods.Method, string, string, map[string]any) (types.ControlResponse, error)
	DeleteArtifact(context.Context, string) (types.ArtifactsDeleteResponse, error)
	SetToolApproval(context.Context, string, types.ToolApprovalPolicy) (types.ToolSetApprovalPolicyResponse, error)
	RevokeToolOAuth(context.Context, string) (types.ToolRevokeOAuthResponse, error)
}

type interactionStore interface {
	Save(conversation.InteractionState) error
}

// RuntimeOptions supplies local-only interaction state and output paths.
type RuntimeOptions struct {
	Compact, ReducedMotion  bool
	Fingerprint, ExportPath string
	State                   conversation.InteractionState
	Store                   interactionStore
}

type attachMsg struct{ err error }
type updateMsg struct{ update conversation.Update }
type submitMsg struct{ err error }
type submissionResultMsg struct {
	submission composer.Submission
	err        error
}
type followupMsg struct {
	id  string
	err error
}
type sessionsMsg struct {
	request  sessionpicker.Request
	response types.SessionsListResponse
	err      error
}
type switchedMsg struct {
	session string
	err     error
}
type renamedMsg struct {
	title string
	err   error
}
type deletedMsg struct{ err error }
type uploadMsg struct {
	attachment composer.Attachment
	response   types.ArtifactsPutResponse
	err        error
}
type exportMsg struct {
	path string
	err  error
}
type persistMsg struct{ err error }
type inspectMsg struct{ data conversation.RuntimeData }
type actionMsg struct {
	intent ActionIntent
	err    error
}

// RuntimeModel joins the terminal foundation to one Protocol-only conversation controller.
type RuntimeModel struct {
	shell                              Model
	controller                         conversationController
	editor                             composer.Editor
	transcript                         conversation.Transcript
	picker                             sessionpicker.Model
	updates                            conversation.UpdateSource
	ctx                                context.Context
	compact, searchMode                bool
	searchQuery                        string
	autocompleteIndex                  int
	collapsedReasoning, collapsedTools map[string]bool
	targetSession                      string
	store                              interactionStore
	fingerprint, exportPath            string
	restoreScrollID                    string
	followups                          conversation.Queue
	followupSubmissions                map[string]composer.Submission
	identity                           types.IdentityScope
	generation                         uint64
	lastProjectionSequence             uint64
	inspectEpoch                       uint64
	runtime                            conversation.RuntimeData
	activeAction                       *ActionSpec
	activeIntent                       *ActionIntent
	actionInput                        string
	selectedTask, selectedTool         string
	selectedArtifact, selectedPause    string
	routeFilter                        bool
	pending                            map[string]ActionIntent
	taskCursors                        map[int]types.TaskListCursor
	renderers                          renderers.Registry
}

// NewRuntimeModel constructs the operational one-active-session application.
func NewRuntimeModel(ctx context.Context, width, height int, theme ui.Theme, controller conversationController, updates conversation.UpdateSource, options RuntimeOptions) RuntimeModel {
	id := controller.Identity()
	shell := NewOperationalModel(width, height, theme, options.ReducedMotion, projection.Projection{}).WithState(State{Route: "session", Connection: "connecting", Composer: ComposerFocused, SidebarOpen: options.State.SidebarOpen})
	editor := composer.New().RestoreLocal(options.State.History, options.State.Stash).SetText(options.State.Draft)
	m := RuntimeModel{shell: shell, controller: controller, editor: editor, transcript: conversation.NewTranscript(projection.Projection{}), picker: sessionpicker.New(id, id.Session), updates: updates, ctx: ctx, compact: options.Compact, collapsedReasoning: stringSet(options.State.CollapsedReasoning), collapsedTools: stringSet(options.State.CollapsedTools), store: options.Store, fingerprint: options.Fingerprint, exportPath: options.ExportPath, restoreScrollID: options.State.ScrollBlockID, followupSubmissions: map[string]composer.Submission{}, identity: id, renderers: renderers.Builtins(), pending: map[string]ActionIntent{}, taskCursors: map[int]types.TaskListCursor{1: {}}}
	m.syncComposer()
	m.syncAccess()
	return m
}

func (m RuntimeModel) Init() tea.Cmd {
	return tea.Batch(func() tea.Msg { return attachMsg{m.controller.Attach(m.ctx)} }, m.waitUpdate())
}
func (m RuntimeModel) waitUpdate() tea.Cmd {
	return func() tea.Msg {
		if m.updates == nil {
			<-m.ctx.Done()
			return tea.Quit
		}
		update, ok := m.updates.Next(m.ctx)
		if !ok {
			return tea.Quit
		}
		return updateMsg{update}
	}
}

func (m RuntimeModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case attachMsg:
		if msg.err != nil {
			m.setError(msg.err)
		}
		return m, m.inspectCmd()
	case inspectMsg:
		if msg.data.Stale || msg.data.RequestEpoch != m.inspectEpoch || msg.data.Generation != m.generation || runtimeScopeKey(msg.data.Identity) != runtimeScopeKey(m.identity) {
			m.closeInspectionModals()
			return m, nil
		}
		if m.activeIntent != nil && m.activeIntent.RequestEpoch != msg.data.RequestEpoch {
			m.closeInspectionModals()
		}
		m.runtime = msg.data
		if msg.data.Tasks.NextCursor.NextPageToken != "" {
			m.taskCursors[msg.data.Tasks.Page+1] = msg.data.Tasks.NextCursor
		}
		m.initializeSelections()
		m.reconcilePending()
		m.syncRuntimeRoute()
		return m, nil
	case actionMsg:
		if msg.err != nil {
			m.setError(msg.err)
		} else {
			m.shell.state.ToastOpen = true
			m.pending[intentKey(msg.intent)] = msg.intent
			m.shell.state.Toast = string(msg.intent.Method) + " accepted · pending canonical reconciliation"
		}
		m.activeAction = nil
		m.activeIntent = nil
		m.actionInput = ""
		m.closeModal()
		return m, m.refreshAffected()
	case updateMsg:
		if !m.applyUpdate(msg.update) {
			return m, m.waitUpdate()
		}
		var inspect tea.Cmd
		if len(m.pending) > 0 {
			inspect = m.inspectCmd()
		}
		return m, tea.Batch(m.waitUpdate(), m.dispatchFollowUp(), inspect)
	case CommandMsg:
		return m.runCommand(msg.ID)
	case sessionsMsg:
		return m.applySessions(msg)
	case switchedMsg:
		if msg.err != nil {
			m.setError(msg.err)
		} else {
			m.picker = m.picker.SetCurrent(msg.session)
			m.closeModal()
			m.shell.state.ToastOpen = true
			m.shell.state.Toast = "Attached session " + msg.session
		}
		return m, m.requestPersist()
	case renamedMsg:
		if msg.err != nil {
			m.setError(msg.err)
		} else {
			m.closeModal()
			m.shell.state.ToastOpen = true
			m.shell.state.Toast = "Session renamed: " + msg.title
		}
		return m, nil
	case deletedMsg:
		if msg.err != nil {
			m.setError(msg.err)
		} else {
			m.closeModal()
			m.shell.state.Erased = true
			m.shell.state.Composer = ComposerDisabled
			m.shell.state.ToastOpen = true
			m.shell.state.Toast = "Session erased · Start Fresh required"
		}
		return m, m.requestPersist()
	case uploadMsg:
		m.finishUpload(msg)
		return m, m.requestPersist()
	case exportMsg:
		if msg.err != nil {
			m.setError(msg.err)
		} else {
			m.shell.state.ToastOpen = true
			m.shell.state.Toast = "Exported " + msg.path
		}
		return m, nil
	case submitMsg:
		if msg.err != nil {
			m.setError(msg.err)
			m.shell.state.Composer = ComposerRetry
		} else {
			m.shell.state.Composer = ComposerRunning
		}
		return m, m.requestPersist()
	case submissionResultMsg:
		m.editor = m.editor.SetDisabled(false, "")
		if msg.err != nil {
			m.setError(msg.err)
			m.shell.state.Composer = ComposerRetry
		} else {
			m.editor = m.editor.CommitSubmission(msg.submission)
			m.shell.state.Composer = ComposerRunning
			m.shell.state.ToastOpen = true
			m.shell.state.Toast = "Turn accepted by Runtime"
		}
		m.syncComposer()
		return m, m.requestPersist()
	case followupMsg:
		if msg.err != nil {
			m.followups = m.followups.Fail(msg.id, msg.err)
			m.editor = m.editor.SetDisabled(false, "")
			m.shell.state.Composer = ComposerRetry
			m.setError(msg.err)
		} else {
			if submission, ok := m.followupSubmissions[msg.id]; ok {
				m.editor = m.editor.CommitSubmission(submission)
				delete(m.followupSubmissions, msg.id)
			}
			m.editor = m.editor.SetDisabled(false, "")
			m.followups = m.followups.Complete(msg.id)
			if m.projectionActive() {
				m.shell.state.Composer = ComposerRunning
			} else {
				m.shell.state.Composer = ComposerFocused
			}
			m.shell.state.ToastOpen = true
			m.shell.state.Toast = "Follow-up accepted by Runtime"
		}
		m.syncAccess()
		return m, tea.Batch(m.requestPersist(), m.dispatchFollowUp())
	case persistMsg:
		if msg.err != nil {
			m.setError(msg.err)
		}
		return m, nil
	case tea.PasteMsg:
		if modal, ok := m.shell.focus.Top(); ok {
			modal.Query += strings.ReplaceAll(msg.Content, "\n", "")
			m.shell.focus = m.shell.focus.ReplaceTop(modal)
			return m, nil
		}
		m.editor = m.editor.Paste(msg.Content)
		m.shell.state.Pasted = true
		m.refreshAutocomplete()
		m.syncComposer()
		return m, m.requestPersist()
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	}
	return m.forward(message)
}

func (m RuntimeModel) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := canonicalKey(msg.String())
	if m.routeFilter {
		return m.updateRouteFilter(key)
	}
	if m.searchMode {
		return m.updateSearch(key)
	}
	if modal, ok := m.shell.focus.Top(); ok {
		return m.updateWorkflowModal(modal, key, msg)
	}
	if key == "ctrl+c" || key == "ctrl+d" {
		return m, tea.Quit
	}
	if m.shell.state.Route != "session" {
		switch key {
		case "up", "ctrl+p":
			m.moveRouteSelection(-1)
			m.syncRuntimeRoute()
			return m, m.inspectCmd()
		case "down", "ctrl+n":
			m.moveRouteSelection(1)
			m.syncRuntimeRoute()
			return m, m.inspectCmd()
		case "pgup":
			m.pageRoute(-1)
			return m, m.inspectCmd()
		case "pgdown":
			m.pageRoute(1)
			return m, m.inspectCmd()
		case "/":
			m.routeFilter = true
			m.shell.state.ToastOpen, m.shell.state.Toast = true, "Filter: "+m.currentRouteFilter()
			return m, nil
		}
	}
	if len(m.shell.sequence) > 0 {
		return m.forward(msg)
	}
	if m.shell.state.AutocompleteOpen {
		switch key {
		case "up", "ctrl+p":
			m.editor = m.editor.MoveCompletion(-1)
			m.moveAutocomplete(-1)
			return m, nil
		case "down", "ctrl+n":
			m.editor = m.editor.MoveCompletion(1)
			m.moveAutocomplete(1)
			return m, nil
		case "enter":
			if candidate, ok := m.editor.SelectedCandidate(); ok && candidate.Kind == "command" {
				m.clearAutocomplete()
				return m.runCommand(CommandID(strings.TrimPrefix(candidate.Value, "/")))
			}
			m.acceptCompletion()
			m.clearAutocomplete()
			m.syncComposer()
			return m, m.requestPersist()
		case "tab":
			m.acceptCompletion()
			m.clearAutocomplete()
			m.syncComposer()
			return m, m.requestPersist()
		case "escape":
			m.clearAutocomplete()
			return m, nil
		}
	}
	switch key {
	case "enter":
		return m.runCommand("submit")
	case "alt+enter", "shift+enter":
		m.editor = m.editor.Insert("\n")
	case "backspace":
		m.editor = m.editor.Backspace()
	case "left", "ctrl+b":
		m.editor = m.editor.Move(-1, false)
	case "right", "ctrl+f":
		m.editor = m.editor.Move(1, false)
	case "alt+b":
		m.editor = m.editor.MoveWord(-1, false)
	case "alt+f":
		m.editor = m.editor.MoveWord(1, false)
	case "ctrl+a":
		m.editor = m.editor.MoveLine(false, false)
	case "ctrl+e":
		m.editor = m.editor.MoveLine(true, false)
	case "ctrl+_":
		m.editor = m.editor.Undo()
	case "alt+_":
		m.editor = m.editor.Redo()
	case "up":
		m.editor = m.editor.History(-1)
	case "down":
		m.editor = m.editor.History(1)
	case "pgup":
		m.transcript = m.transcript.Scroll(-1)
		m.syncProjection()
	case "pgdown":
		m.transcript = m.transcript.Scroll(1)
		m.syncProjection()
	case "home":
		m.transcript = m.transcript.Scroll(-len(m.transcript.Projection.Blocks))
		m.syncProjection()
	case "end":
		m.transcript = m.transcript.Scroll(len(m.transcript.Projection.Blocks))
		m.syncProjection()
	case "alt+j":
		m.transcript = m.transcript.Jump(1)
		m.syncProjection()
	case "alt+k":
		m.transcript = m.transcript.Jump(-1)
		m.syncProjection()
	case "shift+left":
		m.editor = m.editor.Move(-1, true)
	case "shift+right":
		m.editor = m.editor.Move(1, true)
	case "alt+shift+b":
		m.editor = m.editor.MoveWord(-1, true)
	case "alt+shift+f":
		m.editor = m.editor.MoveWord(1, true)
	default:
		if len([]rune(key)) != 1 || key == "?" {
			return m.forward(msg)
		}
		m.editor = m.editor.Insert(key)
	}
	m.refreshAutocomplete()
	m.syncComposer()
	return m, m.requestPersist()
}

func (m RuntimeModel) runCommand(id CommandID) (tea.Model, tea.Cmd) {
	view, ok := m.shell.registry.Command(id, m.shell.commandContext())
	if !ok {
		m.setError(fmt.Errorf("unknown command %q", id))
		return m, nil
	}
	if !view.Enabled {
		m.shell.state.ToastOpen = true
		m.shell.state.Toast = "Unavailable: " + view.DisabledReason
		return m, nil
	}
	switch id {
	case "route-session", "route-tasks", "route-tools", "route-artifacts", "route-events", "route-posture", "route-interventions", "route-diagnostics":
		m.shell.state.Route = strings.TrimPrefix(string(id), "route-")
		m.syncRuntimeRoute()
		return m, m.inspectCmd()
	case "actions":
		return m.openActions()
	case "submit":
		submission, err := m.editor.PrepareSubmission()
		if err != nil {
			m.setError(err)
			return m, nil
		}
		if m.hasActiveWork() {
			m.followups = m.followups.EnqueueTurn(submission.Text, attachmentIDs(submission.Attachments), attachmentDispositions(submission.Attachments))
			entries := m.followups.Entries()
			m.followupSubmissions[entries[len(entries)-1].ID] = submission
			m.editor = m.editor.SetDisabled(true, "follow-up queued locally")
			m.shell.state.ToastOpen = true
			m.shell.state.Toast = "Follow-up queued locally · not yet accepted"
			m.syncComposer()
			m.syncAccess()
			return m, m.requestPersist()
		}
		m.editor = m.editor.SetDisabled(true, "submission in progress")
		m.shell.state.Composer = ComposerRunning
		m.syncComposer()
		return m, func() tea.Msg {
			_, startErr := m.controller.Start(m.ctx, submission.Text, attachmentIDs(submission.Attachments), attachmentDispositions(submission.Attachments))
			return submissionResultMsg{submission: submission, err: startErr}
		}
	case "help":
		rows := m.shell.registry.Help(m.shell.commandContext())
		items := make([]SelectItem, 0, len(rows))
		for i, row := range rows {
			items = append(items, SelectItem{ID: fmt.Sprintf("help-%d", i), Title: row})
		}
		m.shell = m.shell.WithModal(NewSelect("Keyboard help", items, "composer"))
		return m, nil
	case "sessions":
		return m.openSessions("")
	case "session-new":
		m.targetSession = ""
		m.shell = m.shell.WithModal(NewSelect("Start Fresh", nil, "composer"))
		return m, nil
	case "session-rename":
		m.targetSession = m.controller.Identity().Session
		m.shell = m.shell.WithModal(NewSelect("Rename session", nil, "composer"))
		return m, nil
	case "session-delete":
		m.targetSession = m.controller.Identity().Session
		return m.beginSessionDelete()
	case "reauthenticate":
		m.shell = m.shell.WithModal(NewSelect("Replace credential · memory only", nil, "composer"))
		return m, nil
	case "search":
		m.searchMode = true
		m.searchQuery = ""
		m.shell.state.ToastOpen = true
		m.shell.state.Toast = "Search: "
		return m, nil
	case "export":
		return m, m.exportCmd()
	case "reasoning":
		m.toggleSelected(m.collapsedReasoning, "reasoning")
		m.syncProjection()
		return m, m.requestPersist()
	case "tool-detail":
		m.toggleSelected(m.collapsedTools, "tool")
		m.syncProjection()
		return m, m.requestPersist()
	case "timestamps":
		m.transcript.ShowTimestamps = !m.transcript.ShowTimestamps
		m.syncProjection()
		return m, m.requestPersist()
	case "compact":
		m.compact = !m.compact
		m.shell.state.ToastOpen = true
		if m.compact {
			m.shell.state.Toast = "Native scrollback on"
		} else {
			m.shell.state.Toast = "Native scrollback off"
		}
		return m, m.requestPersist()
	case "reduced-motion":
		m.shell.reducedMotion = !m.shell.reducedMotion
		return m, m.requestPersist()
	case "stash":
		m.editor = m.editor.Stash()
		m.shell.state.ToastOpen = true
		m.shell.state.Toast = "Draft stashed locally"
		m.syncComposer()
		return m, m.requestPersist()
	case "stash-pop":
		m.editor = m.editor.PopStash()
		m.shell.state.ToastOpen = true
		m.shell.state.Toast = "Stashed draft restored"
		m.syncComposer()
		return m, m.requestPersist()
	case "followup-cancel":
		entries := m.followups.Entries()
		for i := len(entries) - 1; i >= 0; i-- {
			if entries[i].State == "local queue" || entries[i].State == "failed" {
				delete(m.followupSubmissions, entries[i].ID)
				m.followups = m.followups.Discard(entries[i].ID)
				m.editor = m.editor.SetDisabled(false, "")
				m.shell.state.Composer = ComposerFocused
				m.shell.state.ToastOpen = true
				m.shell.state.Toast = "Follow-up discarded locally"
				m.syncComposer()
				break
			}
		}
		m.syncAccess()
		return m, m.dispatchFollowUp()
	case "followup-retry":
		entries := m.followups.Entries()
		for _, entry := range entries {
			if entry.State == "failed" {
				m.followups = m.followups.Retry(entry.ID)
				m.editor = m.editor.SetDisabled(true, "follow-up retry in progress")
				m.shell.state.Composer = ComposerFocused
				m.shell.state.ToastOpen = true
				m.shell.state.Toast = "Retrying failed follow-up"
				m.syncComposer()
				m.syncAccess()
				return m, m.dispatchFollowUp()
			}
		}
		m.setError(errors.New("no failed follow-up to retry"))
		return m, nil
	case "attachment":
		m.shell = m.shell.WithModal(NewSelect("Attach file · path|disposition", nil, "composer"))
		return m, nil
	case "attachment-remove":
		values := m.editor.Attachments()
		if len(values) > 0 {
			values = values[:len(values)-1]
			m.editor = m.editor.SetAttachments(values)
			m.syncComposer()
			m.shell.state.ToastOpen = true
			m.shell.state.Toast = "Attachment removed locally"
		}
		return m, m.requestPersist()
	case "attachment-retry":
		return m.retryAttachment()
	}
	return m, nil
}

func (m RuntimeModel) updateWorkflowModal(modal SelectModel, key string, original tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key == "escape" || key == "ctrl+c" {
		return m.forward(original)
	}
	switch modal.Title {
	case "Runtime actions":
		if key == "enter" {
			rows := modal.Visible()
			if len(rows) == 0 {
				return m, nil
			}
			id := rows[min(modal.Current, len(rows)-1)].ID
			for _, action := range m.availableActions() {
				if action.ID != id {
					continue
				}
				if action.DisabledReason != "" {
					m.shell.state.ToastOpen = true
					m.shell.state.Toast = "Unavailable: " + action.DisabledReason
					return m, nil
				}
				m.activeAction = &action
				if action.Method == methods.MethodRedirect || action.Method == methods.MethodInjectContext || action.Method == methods.MethodUserMessage || action.Method == methods.MethodPrioritize || action.Method == methods.MethodToolsSetApprovalPolicy || action.Method == methods.MethodReject || action.ID == "intervention.oauth" || action.ID == "intervention.input" {
					m.shell = m.shell.WithModal(NewSelect("Action input · "+action.Title, nil, "modal"))
					return m, nil
				}
				if action.Confirmation != ConfirmNone {
					intent, err := m.buildIntent(action, "")
					if err != nil {
						m.setError(err)
						return m, nil
					}
					m.activeIntent = &intent
					m.openActionConfirm(intent)
					return m, nil
				}
				return m.executeActionSpec(action, "")
			}
		}
	case "Confirm Runtime action":
		if key == "enter" && m.activeIntent != nil {
			rows := modal.Visible()
			if len(rows) == 0 {
				return m, nil
			}
			if rows[min(modal.Current, len(rows)-1)].ID == "confirm" {
				return m.executeIntent(*m.activeIntent)
			}
			m.activeAction = nil
			m.activeIntent = nil
			m.closeModal()
			return m, nil
		}
	case "Sessions":
		if key == "enter" {
			rows := modal.Visible()
			if len(rows) == 0 {
				return m, nil
			}
			session := rows[min(modal.Current, len(rows)-1)].ID
			return m.switchSession(session)
		}
		if key == "ctrl+r" {
			if session, ok := selectedModalID(modal); ok {
				m.targetSession = session
				m.shell = m.shell.WithModal(NewSelect("Rename session", nil, "modal"))
			}
			return m, nil
		}
		if key == "ctrl+d" {
			if session, ok := selectedModalID(modal); ok {
				if session != m.identity.Session {
					m.shell.state.ToastOpen, m.shell.state.Toast = true, "Unavailable: attach the target session before destructive deletion"
					return m, nil
				}
				m.targetSession = session
				m.closeModal()
				return m.beginSessionDelete()
			}
			return m, nil
		}
		before := modal.Query
		next, cmd := m.forward(original)
		updated, ok := next.(RuntimeModel)
		if !ok {
			return m, tea.Quit
		}
		top, _ := updated.shell.focus.Top()
		if top.Query != before {
			return updated, updated.sessionSearchCmd(top.Query)
		}
		return updated, cmd
	case "Start Fresh":
		if key == "enter" {
			session := strings.TrimSpace(modal.Query)
			if session == "" {
				m.setError(errors.New("session ID required"))
				return m, nil
			}
			return m.switchSession(session)
		}
	case "Rename session":
		if key == "enter" {
			title := strings.TrimSpace(modal.Query)
			target := m.targetSession
			return m, func() tea.Msg {
				if target != m.controller.Identity().Session {
					if err := m.controller.Switch(m.ctx, types.IdentityScope{Tenant: m.controller.Identity().Tenant, User: m.controller.Identity().User, Session: target}); err != nil {
						return renamedMsg{err: err}
					}
				}
				_, err := m.controller.Rename(m.ctx, title)
				return renamedMsg{title: title, err: err}
			}
		}
	case "Attach file · path|disposition":
		if key == "enter" {
			return m.beginUpload(modal.Query)
		}
	case "Replace credential · memory only":
		if key == "enter" {
			token := strings.TrimSpace(modal.Query)
			return m, func() tea.Msg {
				return switchedMsg{session: m.controller.Identity().Session, err: m.controller.ReplaceToken(m.ctx, token)}
			}
		}
	}
	if strings.HasPrefix(modal.Title, "Action input · ") && key == "enter" && m.activeAction != nil {
		action := *m.activeAction
		value := strings.TrimSpace(modal.Query)
		if value == "" {
			m.setError(errors.New("action input required"))
			return m, nil
		}
		if action.Confirmation != ConfirmNone {
			m.actionInput = value
			intent, err := m.buildIntent(action, value)
			if err != nil {
				m.setError(err)
				return m, nil
			}
			m.activeIntent = &intent
			m.openActionConfirm(intent)
			return m, nil
		}
		return m.executeActionSpec(action, value)
	}
	return m.forward(original)
}

func (m *RuntimeModel) inspectCmd() tea.Cmd {
	m.inspectEpoch++
	request := conversation.InspectionRequest{Identity: m.identity, Generation: m.generation, RequestEpoch: m.inspectEpoch, TaskID: m.selectedTask, ToolID: m.selectedTool, ArtifactID: m.selectedArtifact, TaskFilter: m.runtime.Tasks.Filter, ToolFilter: m.runtime.Tools.Filter, ArtifactFilter: m.runtime.Artifacts.Filter, EventFilter: m.runtime.Events.Filter, TaskPage: m.runtime.Tasks.Page, ToolPage: m.runtime.Tools.Page, ArtifactPage: m.runtime.Artifacts.Page, EventPage: m.runtime.Events.Page, TaskCursor: m.taskCursors[max(1, m.runtime.Tasks.Page)]}
	return func() tea.Msg { return inspectMsg{data: m.controller.Inspect(m.ctx, request)} }
}

func (m RuntimeModel) beginSessionDelete() (tea.Model, tea.Cmd) {
	for _, action := range ActionMatrix() {
		if action.ID != "session.delete" {
			continue
		}
		intent, err := m.buildIntent(action, "")
		if err != nil {
			m.setError(err)
			return m, nil
		}
		m.activeAction, m.activeIntent = &action, &intent
		m.openActionConfirm(intent)
		return m, nil
	}
	m.setError(errors.New("session delete action unavailable"))
	return m, nil
}

func (m *RuntimeModel) initializeSelections() {
	containsTask := false
	for _, row := range m.runtime.Tasks.Rows {
		containsTask = containsTask || row.ID == m.selectedTask
	}
	if !containsTask {
		m.selectedTask = ""
	}
	if m.selectedTask == "" && len(m.runtime.Tasks.Rows) > 0 {
		m.selectedTask = m.runtime.Tasks.Rows[0].ID
	}
	containsTool := false
	for _, row := range m.runtime.Tools.Rows {
		containsTool = containsTool || row.ID == m.selectedTool
	}
	if !containsTool {
		m.selectedTool = ""
	}
	if m.selectedTool == "" && len(m.runtime.Tools.Rows) > 0 {
		m.selectedTool = m.runtime.Tools.Rows[0].ID
	}
	containsArtifact := false
	for _, row := range m.runtime.Artifacts.Rows {
		containsArtifact = containsArtifact || row.Ref.ID == m.selectedArtifact
	}
	if !containsArtifact {
		m.selectedArtifact = ""
	}
	if m.selectedArtifact == "" && len(m.runtime.Artifacts.Rows) > 0 {
		m.selectedArtifact = m.runtime.Artifacts.Rows[0].Ref.ID
	}
	selectedActive := false
	for _, item := range m.runtime.Interventions.Items() {
		if item.Token == m.selectedPause {
			selectedActive = true
			break
		}
	}
	if selectedActive {
		m.runtime.Interventions = m.runtime.Interventions.Select(m.selectedPause)
	} else if selected, ok := m.runtime.Interventions.Selected(); ok && selected.Status == string(types.PauseStatePaused) {
		m.selectedPause = selected.Token
	} else if rows := m.runtime.Interventions.Items(); len(rows) > 0 {
		m.selectedPause = rows[0].Token
		m.runtime.Interventions = m.runtime.Interventions.Select(m.selectedPause)
	} else {
		m.selectedPause = ""
	}
}

func (m *RuntimeModel) moveRouteSelection(delta int) {
	switch m.shell.state.Route {
	case "tasks":
		ids := make([]string, 0, len(m.runtime.Tasks.Visible()))
		for _, row := range m.runtime.Tasks.Visible() {
			ids = append(ids, row.ID)
		}
		m.selectedTask = moveID(ids, m.selectedTask, delta)
	case "tools":
		ids := make([]string, 0, len(m.runtime.Tools.Visible()))
		for _, row := range m.runtime.Tools.Visible() {
			ids = append(ids, row.ID)
		}
		m.selectedTool = moveID(ids, m.selectedTool, delta)
	case "artifacts":
		ids := make([]string, 0, len(m.runtime.Artifacts.Visible()))
		for _, row := range m.runtime.Artifacts.Visible() {
			ids = append(ids, row.Ref.ID)
		}
		m.selectedArtifact = moveID(ids, m.selectedArtifact, delta)
	case "events":
		rows := m.runtime.Events.Visible()
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, fmt.Sprint(row.Sequence))
		}
		selected := moveID(ids, fmt.Sprint(m.runtime.Events.SelectedSequence), delta)
		if _, err := fmt.Sscan(selected, &m.runtime.Events.SelectedSequence); err != nil {
			m.shell.state.ToastOpen, m.shell.state.Toast = true, "invalid event selection"
		}
	case "interventions":
		rows := m.runtime.Interventions.History()
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.Token)
		}
		m.selectedPause = moveID(ids, m.selectedPause, delta)
		m.runtime.Interventions = m.runtime.Interventions.Select(m.selectedPause)
	}
}

func moveID(ids []string, selected string, delta int) string {
	if len(ids) == 0 {
		return ""
	}
	index := 0
	for i, id := range ids {
		if id == selected {
			index = i
			break
		}
	}
	index = (index + delta%len(ids) + len(ids)) % len(ids)
	return ids[index]
}

func (m *RuntimeModel) pageRoute(delta int) {
	switch m.shell.state.Route {
	case "tasks":
		target := max(1, m.runtime.Tasks.Page+delta)
		if _, ok := m.taskCursors[target]; ok {
			m.runtime.Tasks.Page, m.selectedTask = target, ""
		}
	case "tools":
		m.runtime.Tools.Page = max(1, m.runtime.Tools.Page+delta)
	case "artifacts":
		m.runtime.Artifacts.Page = max(1, m.runtime.Artifacts.Page+delta)
	case "events":
		m.runtime.Events.Page = max(1, m.runtime.Events.Page+delta)
	}
}

func (m RuntimeModel) currentRouteFilter() string {
	switch m.shell.state.Route {
	case "tasks":
		return m.runtime.Tasks.Filter
	case "tools":
		return m.runtime.Tools.Filter
	case "artifacts":
		return m.runtime.Artifacts.Filter
	case "events":
		return m.runtime.Events.Filter
	}
	return ""
}

func (m RuntimeModel) updateRouteFilter(key string) (tea.Model, tea.Cmd) {
	value := m.currentRouteFilter()
	switch key {
	case "escape":
		m.routeFilter = false
		return m, nil
	case "enter":
		m.routeFilter = false
		return m, m.inspectCmd()
	case "backspace":
		if value != "" {
			runes := []rune(value)
			value = string(runes[:len(runes)-1])
		}
	default:
		if len([]rune(key)) == 1 {
			value += key
		}
	}
	switch m.shell.state.Route {
	case "tasks":
		m.runtime.Tasks.Filter = value
		m.runtime.Tasks.Page = 1
		m.taskCursors = map[int]types.TaskListCursor{1: {}}
	case "tools":
		m.runtime.Tools.Filter = value
		m.runtime.Tools.Page = 1
	case "artifacts":
		m.runtime.Artifacts.Filter = value
		m.runtime.Artifacts.Page = 1
	case "events":
		m.runtime.Events.Filter = value
		m.runtime.Events.Page = 1
	}
	m.shell.state.ToastOpen, m.shell.state.Toast = true, "Filter: "+value
	m.syncRuntimeRoute()
	return m, nil
}

func intentKey(intent ActionIntent) string {
	return string(intent.Method) + "/" + intent.RunID + "/" + intent.PauseToken + "/" + intent.ToolID + "/" + intent.ArtifactID + "/" + intent.SessionID
}

func (m *RuntimeModel) reconcilePending() {
	for key, intent := range m.pending {
		reconciled := false
		for _, event := range m.runtime.Events.Rows {
			if event.Run == intent.RunID && (event.Type == "control.applied" || event.Type == "control.rejected" || event.Type == "pause.resumed") {
				reconciled = true
				break
			}
		}
		if intent.Method == methods.MethodArtifactsDelete {
			reconciled = true
			for _, row := range m.runtime.Artifacts.Rows {
				if row.Ref.ID == intent.ArtifactID {
					reconciled = false
				}
			}
		}
		if intent.Method == methods.MethodResume || intent.Method == methods.MethodApprove || intent.Method == methods.MethodReject {
			reconciled = true
			for _, item := range m.runtime.Interventions.Items() {
				if item.Token == intent.PauseToken {
					reconciled = false
				}
			}
		}
		if intent.Method == methods.MethodToolsSetApprovalPolicy {
			want, _ := intent.Payload()["policy"].(string) //nolint:errcheck // acknowledged typed-map lookup; absence handled by empty-string compare below
			for _, tool := range m.runtime.Tools.Rows {
				if tool.ID == intent.ToolID && string(tool.ApprovalPolicy) == want {
					reconciled = true
				}
			}
		}
		if reconciled {
			delete(m.pending, key)
		}
	}
}

func (m *RuntimeModel) closeInspectionModals() {
	if modal, ok := m.shell.focus.Top(); ok && (modal.Title == "Runtime actions" || modal.Title == "Confirm Runtime action" || strings.HasPrefix(modal.Title, "Action input")) {
		m.closeModal()
		m.activeAction, m.activeIntent, m.actionInput = nil, nil, ""
	}
}

func (m *RuntimeModel) refreshAffected() tea.Cmd {
	return m.inspectCmd()
}

func (m RuntimeModel) openActions() (tea.Model, tea.Cmd) {
	actions := m.availableActions()
	items := make([]SelectItem, 0, len(actions))
	for _, action := range actions {
		items = append(items, SelectItem{ID: action.ID, Category: action.Target, Title: action.Title, Description: actionDescription(action)})
	}
	m.shell = m.shell.WithModal(NewSelect("Runtime actions", items, "composer"))
	return m, nil
}

func (m RuntimeModel) availableActions() []ActionSpec {
	caps := map[types.Capability]bool{types.CapTaskControl: m.controller.HasCapability(types.CapTaskControl), types.CapToolAnnotations: m.controller.HasCapability(types.CapToolAnnotations), types.CapSessionLifecycle: m.controller.HasCapability(types.CapSessionLifecycle)}
	var task *types.TaskRow
	interventionKind, token, toolID, artifactID := "", "", "", ""
	for _, row := range m.runtime.Tasks.Rows {
		if row.ID == m.selectedTask {
			selected := row
			task = &selected
			break
		}
	}
	if selected, ok := m.runtime.Interventions.Selected(); ok && selected.Token == m.selectedPause && selected.Status == string(types.PauseStatePaused) {
		token, interventionKind = selected.Token, string(selected.Kind)
	}
	toolID, artifactID = m.selectedTool, m.selectedArtifact
	return resolveActions(caps, task, interventionKind, token, toolID, artifactID)
}

func (m *RuntimeModel) openActionConfirm(intent ActionIntent) {
	target := runtimeScopeKey(intent.Identity) + " · run " + intent.RunID + " · PauseToken " + intent.PauseToken + " · tool " + intent.ToolID + " · artifact " + intent.ArtifactID
	description := target + " · authority " + intent.RequiredAuthority + " · input " + intent.Input + " · " + string(intent.Confirmation)
	m.shell = m.shell.WithModal(NewSelect("Confirm Runtime action", []SelectItem{{ID: "confirm", Title: "Confirm " + intent.Title, Description: description}, {ID: "cancel", Title: "Cancel"}}, "modal"))
}

func (m RuntimeModel) executeActionSpec(action ActionSpec, input string) (tea.Model, tea.Cmd) {
	intent, err := m.buildIntent(action, input)
	if err != nil {
		m.setError(err)
		return m, nil
	}
	return m.executeIntent(intent)
}

func (m RuntimeModel) buildIntent(action ActionSpec, input string) (ActionIntent, error) {
	runID, token, toolID, artifactID := m.actionTargets(action)
	payload := map[string]any{}
	if token != "" {
		payload["token"] = token
	}
	switch action.Method {
	case methods.MethodRedirect:
		payload["goal"] = input
	case methods.MethodInjectContext:
		payload["note"] = input
	case methods.MethodUserMessage:
		payload["message"] = input
	case methods.MethodPrioritize:
		var priority int
		if _, err := fmt.Sscan(input, &priority); err != nil {
			return ActionIntent{}, errors.New("priority must be an integer")
		}
		payload["priority"] = priority
	case methods.MethodApprove:
		payload["approved_by"] = "tui operator"
	case methods.MethodReject:
		payload["rejected_by"] = "tui operator"
		if input != "" {
			payload["reason"] = input
		}
	}
	if action.ID == "intervention.oauth" {
		payload["oauth_input"] = input
	}
	if action.ID == "intervention.input" {
		payload["input"] = input
	}
	if action.Method == methods.MethodToolsSetApprovalPolicy {
		policy := types.ToolApprovalPolicy(input)
		if !types.IsValidToolApprovalPolicy(policy) {
			return ActionIntent{}, errors.New("policy must be auto, gated, or denied")
		}
		payload["policy"] = string(policy)
	}
	frozen, err := freezePayload(payload)
	if err != nil {
		return ActionIntent{}, err
	}
	identity := m.identity
	return ActionIntent{ActionID: action.ID, Title: action.Title, RequiredAuthority: action.Scope, Identity: identity, Generation: m.generation, RequestEpoch: m.inspectEpoch, Method: action.Method, RunID: runID, PauseToken: token, ToolID: toolID, ArtifactID: artifactID, SessionID: identity.Session, Input: input, Confirmation: action.Confirmation, payload: frozen}, nil
}

func (m RuntimeModel) executeIntent(intent ActionIntent) (tea.Model, tea.Cmd) {
	if intent.Generation != m.generation || intent.RequestEpoch != m.inspectEpoch || runtimeScopeKey(intent.Identity) != runtimeScopeKey(m.identity) {
		m.setError(errStaleActionIntent)
		m.activeAction, m.activeIntent = nil, nil
		m.closeInspectionModals()
		return m, nil
	}
	return m, func() tea.Msg {
		err := m.controller.Execute(m.ctx, conversation.Mutation{Identity: intent.Identity, Method: intent.Method, RunID: intent.RunID, PauseToken: intent.PauseToken, ToolID: intent.ToolID, ArtifactID: intent.ArtifactID, Payload: intent.Payload()})
		return actionMsg{intent: intent, err: err}
	}
}

func (m RuntimeModel) actionTargets(action ActionSpec) (runID, token, toolID, artifactID string) {
	if strings.HasPrefix(action.ID, "task.") || action.Method == methods.MethodCancel || action.Method == methods.MethodPause || action.Method == methods.MethodRedirect || action.Method == methods.MethodInjectContext || action.Method == methods.MethodUserMessage || action.Method == methods.MethodPrioritize {
		runID = m.selectedTask
	}
	if strings.HasPrefix(action.ID, "intervention.") || action.ID == "task.resume" || action.Method == methods.MethodResume || action.Method == methods.MethodApprove || action.Method == methods.MethodReject {
		if selected, ok := m.runtime.Interventions.Selected(); ok && selected.Token == m.selectedPause {
			token, runID = selected.Token, selected.RunID
		}
	}
	toolID, artifactID = m.selectedTool, m.selectedArtifact
	return runID, token, toolID, artifactID
}

func (m *RuntimeModel) syncRuntimeRoute() {
	if m.shell.state.Route == "session" {
		return
	}
	rows := []string{"Exactly one active session · " + renderers.SafeText(runtimeScopeKey(m.identity)), "↑/↓ select · PgUp/PgDn page · / filter · F9 actions"}
	switch m.shell.state.Route {
	case "tasks":
		rows = append(rows, "TASK LIFECYCLE / TREE / PROGRESS / GROUP / ORPHAN / LATENCY · "+m.runtime.Tasks.Surface.Label(), "Filter: "+renderers.SafeText(m.runtime.Tasks.Filter)+fmt.Sprintf(" · page %d", m.runtime.Tasks.Page))
		known := map[string]bool{}
		for _, task := range m.runtime.Tasks.Rows {
			known[task.ID] = true
		}
		for _, task := range m.runtime.Tasks.Visible() {
			marker, indent, orphan := "  ", "", ""
			if task.ID == m.selectedTask {
				marker = "● "
			}
			if task.ParentTaskID != "" {
				indent = "  └ "
				if !known[task.ParentTaskID] {
					orphan = " · orphan parent unavailable"
				}
			}
			rows = append(rows, marker+indent+renderers.SafeText(task.ID)+"  "+renderers.SafeText(m.runtime.Tasks.Summary(task))+orphan)
		}
		rows = append(rows, "Cost: observed/non-authoritative; inspect canonical events for completeness")
		if detail := m.runtime.Tasks.Selected; detail != nil {
			rows = append(rows, fmt.Sprintf("DETAIL %s · result_ref=%t · inline=%t · trajectory=%t · parent=%s", detail.Task.ID, detail.ResultRef != nil, detail.ResultInline != "", detail.Trajectory != nil, detail.ParentSession.SessionID))
		}
	case "tools":
		rows = append(rows, "TOOLS · schema / transport / OAuth / approval / metrics / content · "+m.runtime.Tools.Surface.Label(), "Filter: "+renderers.SafeText(m.runtime.Tools.Filter)+fmt.Sprintf(" · page %d", m.runtime.Tools.Page))
		if !m.controller.HasCapability(types.CapToolAnnotations) {
			rows = append(rows, "Unavailable: Runtime does not advertise tool_annotations")
		}
		for _, tool := range m.runtime.Tools.Visible() {
			marker := "  "
			if tool.ID == m.selectedTool {
				marker = "● "
			}
			rows = append(rows, renderers.SafeText(fmt.Sprintf("%s%s  %s · OAuth %s · approval %s", marker, tool.ID, tool.Transport, tool.OAuthStatus, tool.ApprovalPolicy)))
		}
		rows = append(rows, "Analytics: bounded best-effort; absence is not zero")
		if detail := m.runtime.Tools.Selected; detail != nil {
			block := m.renderers.Render(renderers.Value{Kind: "tool", ID: detail.Tool.ID, Title: detail.Tool.Name, Status: string(detail.Tool.OAuthStatus), Payload: map[string]any{"schema_manifest": detail.Manifest, "bounded_metrics": detail.Metrics, "content_posture": detail.Content, "unavailable": detail.Unavailable}})
			rows = append(rows, "DETAIL "+block.Title+" · "+block.Summary)
		}
	case "artifacts":
		rows = append(rows, "ARTIFACT REFERENCES · metadata only; heavy content stays by reference · "+m.runtime.Artifacts.Surface.Label(), "Filter: "+renderers.SafeText(m.runtime.Artifacts.Filter)+fmt.Sprintf(" · page %d", m.runtime.Artifacts.Page))
		for _, artifact := range m.runtime.Artifacts.Visible() {
			marker := "  "
			if artifact.Ref.ID == m.selectedArtifact {
				marker = "● "
			}
			rows = append(rows, renderers.SafeText(fmt.Sprintf("%s%s  %s · %d bytes · %s", marker, artifact.Ref.ID, artifact.Ref.MimeType, artifact.Ref.SizeBytes, artifact.Source)))
		}
		if preview := m.runtime.Artifacts.Selected; preview != nil {
			status := preview.URL
			if preview.Unavailable != "" {
				status = "Unavailable: " + preview.Unavailable
			}
			rows = append(rows, "PREVIEW "+preview.Ref.ID+" · "+status)
		}
	case "events":
		rows = append(rows, "EVENT STREAM / AGGREGATE · filter / raw safe payload / export · "+m.runtime.Events.Surface.Label(), "Filter: "+renderers.SafeText(m.runtime.Events.Filter)+fmt.Sprintf(" · page %d", m.runtime.Events.Page))
		for _, event := range m.runtime.Events.Visible() {
			block := m.renderers.Render(renderers.Value{Kind: "event", ID: fmt.Sprint(event.Sequence), Title: event.Type, Status: event.Run, Payload: event.Payload})
			marker := "  "
			if event.Sequence == m.runtime.Events.SelectedSequence {
				marker = "● "
			}
			rows = append(rows, renderers.SafeText(fmt.Sprintf("%s#%d  %s · run %s · raw %s", marker, event.Sequence, event.Type, event.Run, block.Summary)))
		}
		if m.runtime.Events.Truncated {
			rows = append(rows, "Partial: retained event window or aggregate is truncated")
		}
	case "posture":
		p := m.runtime.Posture
		rows = append(rows, fmt.Sprintf("RUNTIME %s · Protocol %s · uptime %ds", p.Info.DisplayName, p.Info.ProtocolVersion, p.Info.UptimeSeconds), "Capabilities: "+fmt.Sprint(p.Info.Capabilities), fmt.Sprintf("LLM: %s / %s · mock=%t", p.LLM.Provider, p.LLM.Model, p.LLM.MockMode), fmt.Sprintf("Governance: resolved=%s default=%s", p.Governance.ResolvedTier, p.Governance.DefaultTier))
		for _, driver := range p.Drivers.Subsystems {
			rows = append(rows, renderers.SafeText("Driver "+driver.Subsystem+": "+driver.Driver+" "+driver.Mode))
		}
		for _, name := range sortedSurfaceKeys(p.Surfaces) {
			rows = append(rows, "Surface "+renderers.SafeText(name)+": "+renderers.SafeText(p.Surfaces[name].Label()))
		}
	case "interventions":
		rows = append(rows, "INTERVENTION INBOX · retained by PauseToken · approval / OAuth / input / expiry / resolved")
		for _, item := range m.runtime.Interventions.History() {
			marker := "  "
			if item.Token == m.selectedPause {
				marker = "● "
			}
			rows = append(rows, renderers.SafeText(fmt.Sprintf("%s%s  %s %s · %s · run %s", marker, item.Token, item.Status, item.Resolution, item.Kind, item.RunID)))
		}
		if len(m.runtime.Interventions.History()) == 0 {
			if m.runtime.Interventions.Surface.Ready() {
				rows = append(rows, "No interventions in the canonical retained window")
			} else {
				rows = append(rows, "Interventions "+m.runtime.Interventions.Surface.Label()+"; emptiness unknown")
			}
		}
	case "diagnostics":
		rows = append(rows, "DIAGNOSTICS · stream / retention / capability / partiality")
		errorKeys := make([]string, 0, len(m.runtime.Errors))
		for name := range m.runtime.Errors {
			errorKeys = append(errorKeys, name)
		}
		sort.Strings(errorKeys)
		for _, name := range errorKeys {
			rows = append(rows, renderers.SafeText("Unavailable "+name+": "+m.runtime.Errors[name]))
		}
		for _, horizon := range m.runtime.Posture.Health.Retention {
			value := "no rows at caller-visible scope"
			if horizon.OldestRetainedAt != nil {
				value = horizon.OldestRetainedAt.Format(time.RFC3339)
			}
			rows = append(rows, "Retention "+horizon.Surface+" ["+horizon.Scope+"]: "+value)
		}
	}
	if len(m.pending) > 0 {
		rows = append(rows, fmt.Sprintf("Pending canonical reconciliation: %d mutation(s)", len(m.pending)))
	}
	if len(rows) == 1 {
		rows = append(rows, "No canonical rows available; this is not a zero")
	}
	m.shell.state.DetailRows = rows
	level, health := m.runtime.Posture.Attention(m.shell.state.Connection, len(m.runtime.Interventions.Items()), int(m.runtime.Tasks.Aggregates.Failed))
	m.shell.state.Health = level + " · " + health
}

func (m *RuntimeModel) openSessions(query string) (tea.Model, tea.Cmd) {
	return *m, m.sessionSearchCmd(query)
}
func (m *RuntimeModel) sessionSearchCmd(query string) tea.Cmd {
	picker, request := m.picker.Search(query)
	m.picker = picker
	return func() tea.Msg {
		response, err := m.controller.Sessions(m.ctx, query, "")
		return sessionsMsg{request, response, err}
	}
}
func (m RuntimeModel) applySessions(msg sessionsMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.setError(msg.err)
		return m, nil
	}
	if !m.picker.IsCurrent(msg.request) {
		return m, nil
	}
	m.picker = m.picker.Apply(msg.request, msg.response)
	items := make([]SelectItem, 0, len(m.picker.Rows()))
	for _, row := range m.picker.Rows() {
		description := string(row.Status)
		if row.Status == types.SessionStatusCompleted || row.Status == types.SessionStatusFailed {
			description += " · Resume on next canonical start"
		}
		if row.CountersPartial {
			description += " · counters partial"
		}
		items = append(items, SelectItem{ID: row.SessionID, Title: sessionTitle(row), Description: description, Current: row.SessionID == m.controller.Identity().Session})
	}
	modal := NewSelect("Sessions", items, "composer").SetQuery(msg.request.Query)
	if _, ok := m.shell.focus.Top(); ok {
		m.shell.focus = m.shell.focus.ReplaceTop(modal)
	} else {
		m.shell = m.shell.WithModal(modal)
	}
	return m, nil
}
func (m RuntimeModel) switchSession(session string) (tea.Model, tea.Cmd) {
	id := m.controller.Identity()
	id.Session = session
	return m, func() tea.Msg { err := m.controller.Switch(m.ctx, id); return switchedMsg{session, err} }
}
func (m *RuntimeModel) closeModal() { m.shell.focus = NewFocusStack("composer") }

func (m RuntimeModel) beginUpload(spec string) (tea.Model, tea.Cmd) {
	parts := strings.SplitN(strings.TrimSpace(spec), "|", 2)
	path := strings.TrimSpace(parts[0])
	if path == "" {
		m.setError(errors.New("attachment path required"))
		return m, nil
	}
	disposition := "ref"
	if len(parts) == 2 {
		disposition = strings.TrimSpace(parts[1])
	}
	attachment := composer.Attachment{Name: filepath.Base(path), Path: path, MIME: mime.TypeByExtension(filepath.Ext(path)), Disposition: disposition, Uploading: true}
	values := append(m.editor.Attachments(), attachment)
	m.editor = m.editor.SetAttachments(values)
	m.closeModal()
	m.syncComposer()
	return m, m.uploadCmd(attachment)
}
func (m RuntimeModel) uploadCmd(attachment composer.Attachment) tea.Cmd {
	return func() tea.Msg {
		body, err := os.ReadFile(attachment.Path)
		if err != nil {
			return uploadMsg{attachment: attachment, err: fmt.Errorf("read attachment: %w", err)}
		}
		response, err := m.controller.Upload(m.ctx, attachment.Name, attachment.MIME, body)
		return uploadMsg{attachment, response, err}
	}
}
func (m *RuntimeModel) finishUpload(msg uploadMsg) {
	values := m.editor.Attachments()
	for i := range values {
		if values[i].Path == msg.attachment.Path && values[i].ID == "" {
			values[i].Uploading = false
			if msg.err != nil {
				values[i].Failed = true
				m.setError(msg.err)
			} else {
				values[i].ID = msg.response.Ref.ID
				values[i].MIME = msg.response.Ref.MimeType
			}
			break
		}
	}
	m.editor = m.editor.SetAttachments(values)
	m.syncComposer()
}
func (m RuntimeModel) retryAttachment() (tea.Model, tea.Cmd) {
	values := m.editor.Attachments()
	for i := len(values) - 1; i >= 0; i-- {
		if values[i].Failed {
			values[i].Failed = false
			values[i].Uploading = true
			m.editor = m.editor.SetAttachments(values)
			m.syncComposer()
			return m, m.uploadCmd(values[i])
		}
	}
	m.setError(errors.New("no failed attachment to retry"))
	return m, nil
}

func (m RuntimeModel) updateSearch(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "escape", "enter":
		m.searchMode = false
		if key == "escape" {
			m.searchQuery = ""
			m.transcript = m.transcript.Search("")
			m.syncProjection()
		}
		m.shell.state.Toast = "Search closed"
		return m, m.requestPersist()
	case "backspace":
		if m.searchQuery != "" {
			r := []rune(m.searchQuery)
			m.searchQuery = string(r[:len(r)-1])
		}
	case "up", "ctrl+p":
		m.transcript = m.transcript.NextMatch(-1)
	case "down", "ctrl+n":
		m.transcript = m.transcript.NextMatch(1)
	default:
		if len([]rune(key)) == 1 {
			m.searchQuery += key
		}
	}
	m.transcript = m.transcript.Search(m.searchQuery)
	m.shell.state.ToastOpen = true
	m.shell.state.Toast = fmt.Sprintf("Search: %s · %d matches", m.searchQuery, len(m.transcript.Matches))
	m.syncProjection()
	return m, nil
}
func (m *RuntimeModel) refreshAutocomplete() {
	text := m.editor.BeforeCursor()
	word := text
	if i := strings.LastIndexAny(text, " \n\t"); i >= 0 {
		word = text[i+1:]
	}
	if !strings.HasPrefix(word, "/") && !strings.HasPrefix(word, "@") {
		m.clearAutocomplete()
		return
	}
	candidates := m.completionCandidates(word)
	m.editor = m.editor.Complete(candidates, strings.TrimLeft(word, "/@"))
	rows := make([]string, len(m.editor.Candidates()))
	for i, c := range m.editor.Candidates() {
		rows[i] = c.Value + "  " + c.Detail
	}
	m.shell.state.AutocompleteRows = rows
	m.shell.state.AutocompleteOpen = len(rows) > 0
	m.autocompleteIndex = 0
	m.shell.state.AutocompleteIndex = 0
}
func (m RuntimeModel) completionCandidates(word string) []composer.Candidate {
	var out []composer.Candidate
	if strings.HasPrefix(word, "/") {
		for _, view := range m.shell.registry.Palette(m.shell.commandContext()) {
			if view.Enabled {
				out = append(out, composer.Candidate{Kind: "command", Value: "/" + string(view.Command.ID), Detail: view.Command.Title})
			}
		}
	} else {
		seen := map[string]bool{}
		add := func(value, detail string) {
			if value != "" && !seen[value] {
				seen[value] = true
				out = append(out, composer.Candidate{Kind: "reference", Value: value, Detail: detail})
			}
		}
		add("@session:"+m.transcript.Projection.Identity.Session, "active canonical session")
		for _, block := range m.transcript.Projection.Blocks {
			if block.RunID != "" {
				add("@task:"+block.RunID, block.Kind)
			}
			if block.Tool != "" {
				add("@tool:"+block.Tool, "canonical tool")
			}
			for _, artifact := range block.Artifacts {
				add("@artifact:"+artifact.ID, artifact.Filename)
			}
		}
	}
	return out
}
func (m *RuntimeModel) moveAutocomplete(delta int) {
	rows := len(m.shell.state.AutocompleteRows)
	if rows == 0 {
		return
	}
	m.autocompleteIndex = (m.autocompleteIndex + delta%rows + rows) % rows
	m.shell.state.AutocompleteIndex = m.autocompleteIndex
}
func (m *RuntimeModel) clearAutocomplete() {
	m.shell.state.AutocompleteOpen = false
	m.shell.state.AutocompleteRows = nil
	m.autocompleteIndex = 0
}
func (m *RuntimeModel) acceptCompletion() {
	text := m.editor.BeforeCursor()
	word := text
	if i := strings.LastIndexAny(text, " \n\t"); i >= 0 {
		word = text[i+1:]
	}
	for range []rune(word) {
		m.editor = m.editor.Backspace()
	}
	m.editor = m.editor.AcceptCompletion()
}

func (m *RuntimeModel) applyUpdate(update conversation.Update) bool {
	if update.Generation < m.generation || (update.Generation == m.generation && m.generation != 0 && runtimeScopeKey(update.Identity) != runtimeScopeKey(m.identity)) {
		return false
	}
	if update.Generation > m.generation {
		if m.generation != 0 {
			m.closeInspectionModals()
			m.inspectEpoch++
		}
		m.generation = update.Generation
		m.identity = update.Identity
		m.lastProjectionSequence = 0
	}
	m.shell.state.Connection = string(update.State)
	// Never regress the transcript to an older cumulative projection. Cross-lane
	// interleave (a critical lifecycle frame after a batched text delta) can
	// otherwise deliver a stale snapshot and, e.g., redraw a completed tool as
	// running. The notifier drops stale resident frames; this is defense in depth.
	if update.Projection.Identity.Session != "" && update.Projection.LastSequence >= m.lastProjectionSequence {
		m.lastProjectionSequence = update.Projection.LastSequence
		m.transcript = m.transcript.Replace(update.Projection)
		if m.restoreScrollID != "" {
			// Restore through the ID-anchored setter so the semantic anchor
			// (selectedID) is set, not just the positional index.
			m.transcript = m.transcript.SelectByID(m.restoreScrollID)
			m.restoreScrollID = ""
		}
		m.syncProjection()
	}
	// A completed session is resumable on a future turn; a failed session is a
	// terminal error state and must be visually distinct (§9 honesty states).
	m.shell.state.Closed = update.Projection.SessionStatus == string(types.SessionStatusCompleted)
	m.shell.state.Failed = update.Projection.SessionStatus == string(types.SessionStatusFailed)
	// A genuine event-window drop (live-journal overflow / bus drop) is a
	// distinct honesty state from an out-of-order replay gap. Route by reason so
	// state.Dropped is reachable from real Runtime behavior, not dead code.
	m.shell.state.Dropped = false
	m.shell.state.ReplayGap = false
	if gap := update.Projection.ReplayGap; gap != nil {
		if strings.Contains(gap.Reason, "overflow") || strings.Contains(gap.Reason, "dropped") {
			m.shell.state.Dropped = true
		} else {
			m.shell.state.ReplayGap = true
		}
	}
	m.shell.state.Reconciliation = update.Projection.ReconciliationRequired
	m.shell.state.Overflow = update.Overflow
	// Keep each partiality kind distinct on the primary surface (§9): history
	// truncation, aggregate-window truncation, partial counters, partial tool
	// aggregates, and bounded tool analytics each carry their own meaning.
	m.shell.state.Truncated = update.Projection.HistoryTruncated
	m.shell.state.AggregateTruncated = update.Projection.AggregateTruncated
	m.shell.state.CountersPartial = update.Projection.CountersPartial
	m.shell.state.AggregatesPartial = update.Projection.ToolsAggregatesPartial
	m.shell.state.AnalyticsBounded = update.Projection.ToolAnalyticsBounded
	m.shell.state.Unknown, m.shell.state.Incomplete, m.shell.state.Intervention = false, false, false
	for _, block := range update.Projection.Blocks {
		m.shell.state.Unknown = m.shell.state.Unknown || block.Kind == "event"
		m.shell.state.Incomplete = m.shell.state.Incomplete || block.Incomplete
		m.shell.state.Intervention = m.shell.state.Intervention || block.Kind == "intervention" && block.Status == "pending"
	}
	switch update.State {
	case conversation.StateLive:
		m.shell.state.Active = true
		m.shell.state.Erased = false
		if m.projectionActive() {
			m.shell.state.Composer = ComposerRunning
		} else {
			m.shell.state.Composer = ComposerFocused
		}
		m.editor = m.editor.SetDisabled(false, "")
	case conversation.StateReconnecting, conversation.StateReplaying:
		m.shell.state.Active = false
	case conversation.StateAuthExpired, conversation.StateDisconnected:
		m.shell.state.Active = false
		m.shell.state.Composer = ComposerDisabled
		m.editor = m.editor.SetDisabled(true, string(update.State))
	case conversation.StateErased:
		m.shell.state.Erased = true
		m.shell.state.Active = false
		m.shell.state.Composer = ComposerDisabled
		m.editor = m.editor.SetDisabled(true, "session erased; Start Fresh required")
	}
	if update.Err != nil {
		m.shell.state.ToastOpen = true
		m.shell.state.Toast = update.Err.Error()
	}
	m.syncComposer()
	m.syncAccess()
	return true
}
func (m *RuntimeModel) syncComposer() {
	m.shell.state.ComposerText = m.editor.Text()
	m.shell.state.ComposerCursor = m.editor.Cursor()
	start, end, selected := m.editor.Selection()
	if selected {
		m.shell.state.SelectionStart, m.shell.state.SelectionEnd = start, end
	} else {
		m.shell.state.SelectionStart, m.shell.state.SelectionEnd = 0, 0
	}
	values := m.editor.Attachments()
	m.shell.state.AttachmentReady = len(values) > 0
	if len(values) == 0 && strings.HasPrefix(string(m.shell.state.Composer), "attachment") {
		m.shell.state.Composer = ComposerFocused
	}
	if len(values) > 0 && m.shell.state.Composer != ComposerRunning && m.shell.state.Composer != ComposerDisabled {
		last := values[len(values)-1]
		switch {
		case last.Uploading:
			m.shell.state.Composer = ComposerState("attachment · " + last.Name + " · uploading")
			m.shell.state.Toast = "Uploading " + last.Name
		case last.Failed:
			m.shell.state.Composer = ComposerRetry
		default:
			m.shell.state.Composer = ComposerState("attachment · " + last.Name + " · " + last.Disposition)
		}
	}
}
func (m *RuntimeModel) syncProjection() {
	p := m.transcript.Projection
	filtered := make([]projection.Block, 0, len(p.Blocks))
	matches := map[int]bool{}
	if m.transcript.Query != "" {
		for _, index := range m.transcript.Matches {
			matches[index] = true
		}
	}
	selectedID := ""
	if len(p.Blocks) > 0 {
		selectedID = p.Blocks[min(max(0, m.transcript.Selected), len(p.Blocks)-1)].ID
	}
	for index, block := range p.Blocks {
		if m.transcript.Query != "" && !matches[index] {
			continue
		}
		if block.Kind == "reasoning" && m.collapsedReasoning[block.ID] {
			continue
		}
		if block.Kind == "tool" && m.collapsedTools[block.ID] {
			continue
		}
		if m.transcript.ShowTimestamps && !block.At.IsZero() {
			block.Text += " · " + block.At.Format("15:04:05")
		}
		filtered = append(filtered, block)
	}
	capacity := max(1, (m.shell.height-14)/4)
	selected := 0
	for i, block := range filtered {
		if block.ID == selectedID {
			selected = i
			break
		}
	}
	start := max(0, selected-capacity+1)
	if m.transcript.Follow {
		start = max(0, len(filtered)-capacity)
	}
	end := min(len(filtered), start+capacity)
	p.Blocks = append([]projection.Block(nil), filtered[start:end]...)
	m.shell.projection = cloneProjection(p)
	m.shell.state.SelectedBlockID = selectedID
	m.shell.state.Scrolled = !m.transcript.Follow
	if m.transcript.NewOutput > 0 {
		m.shell.state.ToastOpen = true
		m.shell.state.Toast = fmt.Sprintf("%d new output blocks", m.transcript.NewOutput)
	}
}
func (m *RuntimeModel) toggleSelected(set map[string]bool, kind string) {
	if len(m.transcript.Projection.Blocks) == 0 {
		return
	}
	i := min(max(0, m.transcript.Selected), len(m.transcript.Projection.Blocks)-1)
	block := m.transcript.Projection.Blocks[i]
	if block.Kind != kind {
		for _, candidate := range m.transcript.Projection.Blocks {
			if candidate.Kind == kind {
				block = candidate
				break
			}
		}
	}
	if block.Kind == kind {
		set[block.ID] = !set[block.ID]
	}
}
func (m *RuntimeModel) setError(err error) {
	m.shell.state.ToastOpen = true
	m.shell.state.Toast = err.Error()
	if state := honestErrorState(err); state != "" {
		m.shell.state.Connection = state
	}
}
func honestErrorState(err error) string {
	if errors.Is(err, conversation.ErrTokenExpired) || errors.Is(err, conversation.ErrTokenUnavailable) {
		return string(conversation.StateAuthExpired)
	}
	var protocolErr *protocolclient.ProtocolError
	if errors.As(err, &protocolErr) {
		if protocolErr.Status == 401 {
			return string(conversation.StateAuthExpired)
		}
		if protocolErr.Status == 403 {
			return "authorization denied · 403"
		}
		if protocolErr.Status == 404 || protocolErr.Status == 409 || protocolErr.Status == 501 {
			return ""
		}
		return string(conversation.StateDisconnected)
	}
	return ""
}

func (m RuntimeModel) forward(message tea.Msg) (tea.Model, tea.Cmd) {
	oldSidebar, oldTheme := m.shell.state.SidebarOpen, m.shell.theme.Mode()
	next, cmd := m.shell.Update(message)
	shell, ok := next.(Model)
	if !ok {
		m.shell.state.Connection = "failed · invalid shell model"
		return m, tea.Quit
	}
	m.shell = shell
	m.syncComposer()
	if oldSidebar != m.shell.state.SidebarOpen || oldTheme != m.shell.theme.Mode() {
		cmd = tea.Batch(cmd, m.requestPersist())
	}
	return m, cmd
}
func (m *RuntimeModel) requestPersist() tea.Cmd {
	if m.store == nil {
		return nil
	}
	state := m.interactionState()
	err := m.store.Save(state)
	return func() tea.Msg { return persistMsg{err} }
}

// Finalize synchronously saves the last model returned by Bubble Tea on every
// exit path, including keys, signals, context cancellation, and host errors.
func (m RuntimeModel) Finalize() error {
	if m.store == nil {
		return nil
	}
	return m.store.Save(m.interactionState())
}
func (m RuntimeModel) interactionState() conversation.InteractionState {
	id := m.controller.Identity()
	theme := string(m.shell.theme.Mode())
	scroll := ""
	if len(m.transcript.Projection.Blocks) > 0 {
		scroll = m.transcript.Projection.Blocks[min(max(0, m.transcript.Selected), len(m.transcript.Projection.Blocks)-1)].ID
	}
	return conversation.InteractionState{Identity: id, RuntimeFingerprint: m.fingerprint, Draft: m.editor.Text(), History: m.editor.HistoryEntries(), Stash: m.editor.StashEntries(), ScrollBlockID: scroll, CollapsedReasoning: setStrings(m.collapsedReasoning), CollapsedTools: setStrings(m.collapsedTools), SidebarWidth: ui.SidebarWidth, SidebarOpen: m.shell.state.SidebarOpen, Theme: theme, ReducedMotion: m.shell.reducedMotion, Compact: m.compact}
}

func (m *RuntimeModel) syncAccess() {
	m.shell.state.Negotiated = true
	m.shell.state.TaskControl = m.controller.HasCapability(types.CapTaskControl)
	m.shell.state.SessionLifecycle = m.controller.HasCapability(types.CapSessionLifecycle)
	m.shell.state.SessionScope = true
	m.shell.state.HasFollowUp = len(m.followups.Entries()) > 0
}

func (m RuntimeModel) hasActiveWork() bool {
	return m.projectionActive() || m.shell.state.Composer == ComposerRunning
}

func (m RuntimeModel) projectionActive() bool {
	for _, block := range m.transcript.Projection.Blocks {
		if block.Status == "pending" || block.Status == "running" || block.Status == "started" {
			return true
		}
	}
	return false
}

func (m *RuntimeModel) dispatchFollowUp() tea.Cmd {
	if m.hasActiveWork() {
		return nil
	}
	next, entry, ok := m.followups.Begin()
	if !ok {
		return nil
	}
	m.followups = next
	m.shell.state.Composer = ComposerRunning
	m.syncComposer()
	return func() tea.Msg {
		_, err := m.controller.Start(m.ctx, entry.Text, entry.ArtifactIDs, entry.Dispositions)
		return followupMsg{id: entry.ID, err: err}
	}
}
func (m RuntimeModel) exportCmd() tea.Cmd {
	path := m.exportPath
	if path == "" {
		path = "harbor-" + m.controller.Identity().Session + ".md"
	}
	view := m.transcript
	eventExport := m.shell.state.Route == "events"
	events := append([]types.StateEvent(nil), m.runtime.Events.Visible()...)
	return func() tea.Msg {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return exportMsg{path, fmt.Errorf("%w: %w", conversation.ErrExportWrite, err)}
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return exportMsg{path, fmt.Errorf("%w: %w", conversation.ErrExportWrite, err)}
		}
		var writeErr error
		if eventExport {
			encoder := json.NewEncoder(file)
			encoder.SetIndent("", "  ")
			writeErr = encoder.Encode(renderers.Normalize(events))
		} else {
			writeErr = view.Export(file, conversation.ExportOptions{Reasoning: true, Tools: true, Metadata: true})
		}
		closeErr := file.Close()
		if writeErr == nil {
			writeErr = closeErr
		}
		return exportMsg{path, writeErr}
	}
}
func (m RuntimeModel) View() tea.View {
	view := m.shell.View()
	view.AltScreen = !m.compact
	view.WindowTitle = "Harbor TUI · " + m.controller.Identity().Session
	return view
}

func attachmentIDs(values []composer.Attachment) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value.ID != "" {
			out = append(out, value.ID)
		}
	}
	return out
}
func attachmentDispositions(values []composer.Attachment) map[string]string {
	out := map[string]string{}
	for _, value := range values {
		if value.ID != "" && value.Disposition != "" {
			out[value.ID] = value.Disposition
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
func selectedModalID(modal SelectModel) (string, bool) {
	rows := modal.Visible()
	if len(rows) == 0 {
		return "", false
	}
	return rows[min(modal.Current, len(rows)-1)].ID, true
}
func sessionTitle(row types.SessionRow) string {
	if row.Title != "" {
		return row.Title + " · " + row.SessionID
	}
	return row.SessionID
}
func stringSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		out[value] = true
	}
	return out
}
func setStrings(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value, on := range values {
		if on {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func runtimeScopeKey(id types.IdentityScope) string {
	return id.Tenant + "/" + id.User + "/" + id.Session
}

func sortedSurfaceKeys(values map[string]surface.State) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
