package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	protocolclient "github.com/hurtener/Harbor/internal/protocol/client"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/composer"
	"github.com/hurtener/Harbor/internal/tui/conversation"
	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

type recordingController struct {
	id                                    types.IdentityScope
	projection                            projection.Projection
	calls                                 []string
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
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if !containsCall(controller.calls, "delete:s2") {
		t.Fatalf("delete calls=%v", controller.calls)
	}
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

func TestRuntimeModel_CanonicalAuthFailuresRemainVisible(t *testing.T) {
	m, _, _ := operationalModel(t)
	for _, tc := range []struct {
		status int
		want   string
	}{{401, string(conversation.StateAuthExpired)}, {403, "authorization denied · 403"}} {
		err := &protocolclient.ProtocolError{Status: tc.status, Message: "denied"}
		m.setError(err)
		if m.shell.state.Connection != tc.want || !strings.Contains(m.shell.state.Toast, "denied") {
			t.Fatalf("status %d mapped to connection=%q toast=%q", tc.status, m.shell.state.Connection, m.shell.state.Toast)
		}
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
