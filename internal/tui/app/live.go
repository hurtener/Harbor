package app

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	protocolclient "github.com/hurtener/Harbor/internal/protocol/client"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/composer"
	"github.com/hurtener/Harbor/internal/tui/conversation"
	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/sessionpicker"
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
}

// NewRuntimeModel constructs the operational one-active-session application.
func NewRuntimeModel(ctx context.Context, width, height int, theme ui.Theme, controller conversationController, updates conversation.UpdateSource, options RuntimeOptions) RuntimeModel {
	id := controller.Identity()
	shell := NewOperationalModel(width, height, theme, options.ReducedMotion, projection.Projection{}).WithState(State{Route: "session", Connection: "connecting", Composer: ComposerFocused, SidebarOpen: options.State.SidebarOpen})
	editor := composer.New().RestoreLocal(options.State.History, options.State.Stash).SetText(options.State.Draft)
	m := RuntimeModel{shell: shell, controller: controller, editor: editor, transcript: conversation.NewTranscript(projection.Projection{}), picker: sessionpicker.New(id, id.Session), updates: updates, ctx: ctx, compact: options.Compact, collapsedReasoning: stringSet(options.State.CollapsedReasoning), collapsedTools: stringSet(options.State.CollapsedTools), store: options.Store, fingerprint: options.Fingerprint, exportPath: options.ExportPath, restoreScrollID: options.State.ScrollBlockID, followupSubmissions: map[string]composer.Submission{}, identity: id}
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
		return m, nil
	case updateMsg:
		if !m.applyUpdate(msg.update) {
			return m, m.waitUpdate()
		}
		return m, tea.Batch(m.waitUpdate(), m.dispatchFollowUp())
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
	if m.searchMode {
		return m.updateSearch(key)
	}
	if modal, ok := m.shell.focus.Top(); ok {
		return m.updateWorkflowModal(modal, key, msg)
	}
	if key == "ctrl+c" || key == "ctrl+d" {
		return m, tea.Quit
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
		m.openDeleteConfirm()
		return m, nil
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
				m.targetSession = session
				m.openDeleteConfirm()
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
	case "Delete session":
		if key == "enter" {
			rows := modal.Visible()
			if len(rows) == 0 {
				return m, nil
			}
			if rows[min(modal.Current, len(rows)-1)].ID == "cancel" {
				m.closeModal()
				return m, nil
			}
			target := m.targetSession
			return m, func() tea.Msg {
				if target != m.controller.Identity().Session {
					if err := m.controller.Switch(m.ctx, types.IdentityScope{Tenant: m.controller.Identity().Tenant, User: m.controller.Identity().User, Session: target}); err != nil {
						return deletedMsg{err}
					}
				}
				_, err := m.controller.Delete(m.ctx)
				return deletedMsg{err}
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
	return m.forward(original)
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
func (m *RuntimeModel) openDeleteConfirm() {
	m.shell = m.shell.WithModal(NewSelect("Delete session", []SelectItem{{ID: "delete", Title: "Erase permanently", Description: "canonical sessions.delete"}, {ID: "cancel", Title: "Cancel"}}, "modal"))
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
		m.transcript = m.transcript.Jump(-1)
	case "down", "ctrl+n":
		m.transcript = m.transcript.Jump(1)
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
		m.generation = update.Generation
		m.identity = update.Identity
	}
	m.shell.state.Connection = string(update.State)
	if update.Projection.Identity.Session != "" {
		m.transcript = m.transcript.Replace(update.Projection)
		if m.restoreScrollID != "" {
			for i, block := range m.transcript.Projection.Blocks {
				if block.ID == m.restoreScrollID {
					m.transcript.Selected = i
					m.transcript.Follow = i == len(m.transcript.Projection.Blocks)-1
					break
				}
			}
			m.restoreScrollID = ""
		}
		m.syncProjection()
	}
	m.shell.state.Closed = update.Projection.SessionStatus == string(types.SessionStatusCompleted) || update.Projection.SessionStatus == string(types.SessionStatusFailed)
	m.shell.state.ReplayGap = update.Projection.ReplayGap != nil
	m.shell.state.Reconciliation = update.Projection.ReconciliationRequired
	m.shell.state.Overflow = update.Overflow
	m.shell.state.Truncated = update.Projection.HistoryTruncated || update.Projection.AggregateTruncated
	m.shell.state.CountersPartial = update.Projection.CountersPartial || update.Projection.ToolsAggregatesPartial || update.Projection.ToolAnalyticsBounded
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
	return func() tea.Msg {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return exportMsg{path, fmt.Errorf("%w: %w", conversation.ErrExportWrite, err)}
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return exportMsg{path, fmt.Errorf("%w: %w", conversation.ErrExportWrite, err)}
		}
		writeErr := view.Export(file, conversation.ExportOptions{Reasoning: true, Tools: true, Metadata: true})
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
