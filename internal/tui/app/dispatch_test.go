package app

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/hurtener/Harbor/internal/tui/conversation"
	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

func typeText(t *testing.T, m RuntimeModel, text string) RuntimeModel {
	t.Helper()
	for _, r := range text {
		m = drive(t, m, keyMsg(r, 0))
	}
	return m
}

// TestRuntimeModel_SlashGuard_DraftCommandsNeverReachTheModel pins the
// trust-critical guarantee: a /command draft executes or errors locally — it
// is never submitted as a chat message, on any path that lands it in the
// draft (tab-accepted completion, hand-typed, closed autocomplete).
func TestRuntimeModel_SlashGuard_DraftCommandsNeverReachTheModel(t *testing.T) {
	m, controller, _ := operationalModel(t)

	// Tab accepts "/help" as draft text; enter must EXECUTE it.
	m = typeText(t, m, "/hel")
	if !m.shell.state.AutocompleteOpen {
		t.Fatal("autocomplete did not open for a slash prefix")
	}
	m = drive(t, m, keyMsg(tea.KeyTab, 0))
	if !strings.HasPrefix(m.editor.Text(), "/help") {
		t.Fatalf("tab-accept draft=%q", m.editor.Text())
	}
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if modal, ok := m.shell.focus.Top(); !ok || modal.Title != "Keyboard help" {
		t.Fatalf("slash draft did not execute /help: modal=%#v", modal)
	}
	if m.editor.Text() != "" {
		t.Fatalf("draft not cleared after execution: %q", m.editor.Text())
	}
	for _, call := range controller.calls {
		if strings.HasPrefix(call, "start:") {
			t.Fatalf("slash command leaked to the model as a turn: %v", controller.calls)
		}
	}
	m = drive(t, m, keyMsg(tea.KeyEscape, 0))

	// Unknown command: toast, draft kept, nothing submitted.
	m = typeText(t, m, "/nosuchcmd")
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if !strings.Contains(m.shell.state.Toast, "Unknown command /nosuchcmd") {
		t.Fatalf("toast=%q", m.shell.state.Toast)
	}
	if m.editor.Text() != "/nosuchcmd" {
		t.Fatalf("unknown-command draft must be kept: %q", m.editor.Text())
	}
	for _, call := range controller.calls {
		if strings.HasPrefix(call, "start:") {
			t.Fatalf("unknown slash draft leaked to the model: %v", controller.calls)
		}
	}

	// Arguments are rejected explicitly, never forwarded as chat.
	m = drive(t, m, keyMsg('_', tea.ModCtrl)) // undo typing is fiddly; clear directly
	m.editor = m.editor.SetText("/help now please")
	m.syncComposer()
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if !strings.Contains(m.shell.state.Toast, "takes no arguments") {
		t.Fatalf("toast=%q", m.shell.state.Toast)
	}

	// A slash mid-sentence is prose and submits normally.
	m.editor = m.editor.SetText("see /help for details")
	m.syncComposer()
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if !containsCall(controller.calls, "start:see /help for details::") {
		t.Fatalf("prose containing a slash must submit: %v", controller.calls)
	}
}

// TestRuntimeModel_ShellOwnedSlashCommandsDispatch pins the unified dispatch:
// shell-owned ids reached via the slash path must act, not silently no-op.
func TestRuntimeModel_ShellOwnedSlashCommandsDispatch(t *testing.T) {
	m, _, _ := operationalModel(t)

	m.editor = m.editor.SetText("/sidebar")
	m.syncComposer()
	before := m.shell.state.SidebarOpen
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if m.shell.state.SidebarOpen == before {
		t.Fatal("/sidebar was a silent no-op")
	}

	m.editor = m.editor.SetText("/theme")
	m.syncComposer()
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if modal, ok := m.shell.focus.Top(); !ok || modal.Title != "Themes" {
		t.Fatalf("/theme did not open the Themes dialog: %#v", modal)
	}
	m = drive(t, m, keyMsg(tea.KeyEscape, 0))

	m.editor = m.editor.SetText("/palette")
	m.syncComposer()
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	if modal, ok := m.shell.focus.Top(); !ok || modal.Title != "Commands" {
		t.Fatalf("/palette did not open the command palette: %#v", modal)
	}
	m = drive(t, m, keyMsg(tea.KeyEscape, 0))

	m.editor = m.editor.SetText("/quit")
	m.syncComposer()
	next, cmd := m.Update(keyMsg(tea.KeyEnter, 0))
	m = next.(RuntimeModel)
	if cmd == nil {
		t.Fatal("/quit was a silent no-op")
	}
	if _, isQuit := cmd().(tea.QuitMsg); !isQuit {
		t.Fatal("/quit did not quit")
	}
}

func activeRunModel(t *testing.T) (RuntimeModel, *recordingController) {
	t.Helper()
	m, controller, _ := operationalModel(t)
	id := controller.Identity()
	p := projection.Projection{Identity: id, LastSequence: 2, Blocks: []projection.Block{
		{ID: "task:r1", Kind: "task", Status: "running", RunID: "r1", At: time.Now().Add(-5 * time.Second)},
		{ID: "text:r1", Kind: "text", Text: "streaming", RunID: "r1", Incomplete: true, At: time.Now()},
	}}
	m.applyUpdate(conversation.Update{Identity: id, Generation: 1, State: conversation.StateLive, Projection: p})
	return m, controller
}

// TestRuntimeModel_DoubleEscInterrupt pins the deliberate-interrupt contract:
// the first esc only ARMS (hint flips, nothing cancelled); the second esc
// inside the window cancels; a timeout or any other key disarms.
func TestRuntimeModel_DoubleEscInterrupt(t *testing.T) {
	m, controller := activeRunModel(t)

	next, _ := m.Update(keyMsg(tea.KeyEscape, 0))
	m = next.(RuntimeModel)
	if !m.interruptArmed || !m.shell.state.InterruptArmed {
		t.Fatal("first esc did not arm the interrupt")
	}
	if containsCall(controller.calls, "control:cancel:r1:owner_user") {
		t.Fatal("first esc must not cancel the run")
	}

	// A stale timeout for an older generation must not disarm a newer arm.
	m = drive(t, m, interruptTimeoutMsg{generation: m.interruptGeneration - 1})
	if !m.interruptArmed {
		t.Fatal("stale timeout disarmed a live interrupt window")
	}

	// The second esc cancels.
	next, cmd := m.Update(keyMsg(tea.KeyEscape, 0))
	m = next.(RuntimeModel)
	if m.interruptArmed || m.shell.state.InterruptArmed {
		t.Fatal("interrupt window not consumed by the second esc")
	}
	if cmd == nil {
		t.Fatal("second esc produced no cancel command")
	}
	cmd()
	if !containsCall(controller.calls, "control:cancel:r1:owner_user") {
		t.Fatalf("second esc did not cancel: %v", controller.calls)
	}

	// Re-arm, then let the timeout fire: disarmed, still no second cancel.
	next, _ = m.Update(keyMsg(tea.KeyEscape, 0))
	m = next.(RuntimeModel)
	m = drive(t, m, interruptTimeoutMsg{generation: m.interruptGeneration})
	if m.interruptArmed || m.shell.state.InterruptArmed {
		t.Fatal("timeout did not disarm")
	}

	// Re-arm, then any other key disarms.
	next, _ = m.Update(keyMsg(tea.KeyEscape, 0))
	m = next.(RuntimeModel)
	m = drive(t, m, keyMsg('h', 0))
	if m.interruptArmed || m.shell.state.InterruptArmed {
		t.Fatal("typing did not disarm the interrupt window")
	}
}

// TestRuntimeModel_EscPrecedence_TransientSurfacesBeforeInterrupt pins the
// hijack fix: esc with the autocomplete open (or a pending chord) dismisses
// the transient surface and leaves the run running.
func TestRuntimeModel_EscPrecedence_TransientSurfacesBeforeInterrupt(t *testing.T) {
	m, controller := activeRunModel(t)

	m = typeText(t, m, "/hel")
	if !m.shell.state.AutocompleteOpen {
		t.Fatal("autocomplete not open")
	}
	next, _ := m.Update(keyMsg(tea.KeyEscape, 0))
	m = next.(RuntimeModel)
	if m.shell.state.AutocompleteOpen {
		t.Fatal("esc did not close the autocomplete")
	}
	if m.interruptArmed {
		t.Fatal("esc on an open autocomplete must not arm the interrupt")
	}
	for _, call := range controller.calls {
		if strings.HasPrefix(call, "control:cancel") {
			t.Fatalf("esc on autocomplete cancelled the run: %v", controller.calls)
		}
	}
	m.editor = m.editor.SetText("")
	m.syncComposer()

	// Pending ctrl+x chord: esc aborts the chord, not the run.
	next, _ = m.Update(keyMsg('x', tea.ModCtrl))
	m = next.(RuntimeModel)
	if len(m.shell.sequence) == 0 {
		t.Fatal("leader chord not pending")
	}
	next, _ = m.Update(keyMsg(tea.KeyEscape, 0))
	m = next.(RuntimeModel)
	if len(m.shell.sequence) != 0 {
		t.Fatal("esc did not abort the pending chord")
	}
	for _, call := range controller.calls {
		if strings.HasPrefix(call, "control:cancel") {
			t.Fatalf("esc on a pending chord cancelled the run: %v", controller.calls)
		}
	}
}

// TestRuntimeModel_CtrlCUnconditionalQuit pins the always-escapable contract
// across every input mode that previously trapped it.
func TestRuntimeModel_CtrlCUnconditionalQuit(t *testing.T) {
	assertQuit := func(t *testing.T, m RuntimeModel, context string) {
		t.Helper()
		_, cmd := m.Update(keyMsg('c', tea.ModCtrl))
		if cmd == nil {
			t.Fatalf("%s: ctrl+c produced nothing", context)
		}
		if _, isQuit := cmd().(tea.QuitMsg); !isQuit {
			t.Fatalf("%s: ctrl+c did not quit", context)
		}
	}

	m, _, _ := operationalModel(t)
	m = leader(t, m, 'f')
	if !m.searchMode {
		t.Fatal("search mode not active")
	}
	assertQuit(t, m, "search mode")

	m, _, _ = operationalModel(t)
	m.shell.state.Route = "tasks"
	next, _ := m.Update(keyMsg('/', 0))
	m = next.(RuntimeModel)
	if !m.routeFilter {
		t.Fatal("route filter not active")
	}
	assertQuit(t, m, "route filter")

	m, _, _ = operationalModel(t)
	m = drive(t, m, CommandMsg{ID: "sessions"})
	if _, open := m.shell.focus.Top(); !open {
		t.Fatal("sessions dialog not open")
	}
	assertQuit(t, m, "workflow modal")
}

// TestRuntimeModel_RouteFilterOnlyWhereFilteringExists pins that '/' does not
// open a dead filter prompt on routes without a canonical filter.
func TestRuntimeModel_RouteFilterOnlyWhereFilteringExists(t *testing.T) {
	m, _, _ := operationalModel(t)
	m.shell.state.Route = "posture"
	next, _ := m.Update(keyMsg('/', 0))
	m = next.(RuntimeModel)
	if m.routeFilter {
		t.Fatal("'/' opened a dead filter prompt on the posture route")
	}
}

// TestInputDialogs_LabelMaskAndInlineValidation pins the honest-input render
// contract: input dialogs caption WHAT the value is, secrets echo masked, and
// validation failures render inline instead of committing.
func TestInputDialogs_LabelMaskAndInlineValidation(t *testing.T) {
	m, controller, _ := operationalModel(t)
	m.shell.startup = startupHidden
	m = leader(t, m, 'i')
	modal, ok := m.shell.focus.Top()
	if !ok || !modal.IsInput() {
		t.Fatalf("reauthenticate did not open an input dialog: %#v", modal)
	}
	m = typeText(t, m, "secret-token")
	frame := ansi.Strip(m.shell.Frame())
	if strings.Contains(frame, "Search:") {
		t.Fatal("input dialog rendered the pick-list 'Search:' caption")
	}
	if strings.Contains(frame, "secret-token") {
		t.Fatal("credential echoed in cleartext")
	}
	if !strings.Contains(frame, "Bearer token") {
		t.Fatal("input dialog missing its label")
	}
	if !strings.Contains(frame, "••••") {
		t.Fatal("masked echo missing")
	}
	// Emptied input fails inline validation on enter — dialog stays, error shows.
	for range "secret-token" {
		m = drive(t, m, keyMsg(tea.KeyBackspace, 0))
	}
	m = drive(t, m, keyMsg(tea.KeyEnter, 0))
	modal, ok = m.shell.focus.Top()
	if !ok || modal.ErrorText == "" {
		t.Fatalf("empty credential did not fail inline: %#v", modal)
	}
	if containsCall(controller.calls, "replace-token") {
		t.Fatal("invalid input committed anyway")
	}
	if !strings.Contains(ansi.Strip(m.shell.Frame()), modal.ErrorText) {
		t.Fatal("inline error not rendered in the dialog")
	}
	// Typing again clears the error.
	m = drive(t, m, keyMsg('x', 0))
	if modal, _ = m.shell.focus.Top(); modal.ErrorText != "" {
		t.Fatal("editing did not clear the inline error")
	}
}

// TestModel_ExplicitThemeChoiceLocksAgainstAutoDetect pins the themeLocked
// contract: a Themes-dialog choice survives background detection; Auto
// releases it.
func TestModel_ExplicitThemeChoiceLocksAgainstAutoDetect(t *testing.T) {
	m := NewOperationalModel(80, 24, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, projection.Projection{})
	next, _ := m.execute(CommandView{Command: Command{ID: "theme"}, Enabled: true})
	m = next.(Model)
	modal, ok := m.focus.Top()
	if !ok || modal.Title != "Themes" {
		t.Fatalf("themes dialog missing: %#v", modal)
	}
	// Choose Light explicitly (second row).
	next, _ = m.Update(keyMsg(tea.KeyDown, 0))
	m = next.(Model)
	next, _ = m.Update(keyMsg(tea.KeyEnter, 0))
	m = next.(Model)
	if m.theme.Mode() != ui.ModeLight || !m.themeLocked {
		t.Fatalf("explicit choice not applied+locked: mode=%v locked=%v", m.theme.Mode(), m.themeLocked)
	}
	if m = m.WithTerminalBackground(true); m.theme.Mode() != ui.ModeLight {
		t.Fatal("auto-detect clobbered an explicit theme choice")
	}
	// Auto releases the lock and re-enables detection.
	next, _ = m.execute(CommandView{Command: Command{ID: "theme"}, Enabled: true})
	m = next.(Model)
	next, _ = m.Update(keyMsg(tea.KeyDown, 0))
	m = next.(Model)
	next, _ = m.Update(keyMsg(tea.KeyDown, 0))
	m = next.(Model)
	next, _ = m.Update(keyMsg(tea.KeyEnter, 0))
	m = next.(Model)
	if m.themeLocked {
		t.Fatal("Auto did not release the theme lock")
	}
	if m = m.WithTerminalBackground(true); m.theme.Mode() != ui.ModeDark {
		t.Fatal("auto-detect inert after choosing Auto")
	}
}

// TestRuntimeModel_SteeringTargetsActiveRunFromConversationSurface pins that
// redirect/inject reach the active run without a tasks-route detour.
func TestRuntimeModel_SteeringTargetsActiveRunFromConversationSurface(t *testing.T) {
	m, controller := activeRunModel(t)
	actions := m.availableActions()
	var redirect *ActionSpec
	for i := range actions {
		if actions[i].ID == "task.redirect" {
			redirect = &actions[i]
		}
	}
	if redirect == nil {
		t.Fatal("task.redirect absent from the matrix")
	}
	if redirect.DisabledReason != "" {
		t.Fatalf("redirect disabled on the conversation surface: %q", redirect.DisabledReason)
	}
	intent, err := m.buildIntent(*redirect, "new goal")
	if err != nil {
		t.Fatal(err)
	}
	if intent.RunID != "r1" {
		t.Fatalf("steering intent targets %q, want the active run r1", intent.RunID)
	}
	next, cmd := m.executeIntent(intent)
	m = next.(RuntimeModel)
	_ = m
	if cmd == nil {
		t.Fatal("no execution command")
	}
	cmd()
	if !containsCall(controller.calls, "control:redirect:r1:owner_user") {
		t.Fatalf("redirect did not reach the controller: %v", controller.calls)
	}
}

// TestSyncProjection_SearchFilterNotAliasedByLocalTurns pins ID-based match
// resolution: a local user echo inserted ahead of the matches must not shift
// the filtered set onto neighbouring blocks.
func TestSyncProjection_SearchFilterNotAliasedByLocalTurns(t *testing.T) {
	m, controller, _ := operationalModel(t)
	id := controller.Identity()
	p := projection.Projection{Identity: id, Blocks: []projection.Block{
		{ID: "a", Kind: "text", Text: "alpha content"},
		{ID: "b", Kind: "text", Text: "needle content"},
	}}
	m.transcript = m.transcript.Replace(p)
	m.localTurns = []localTurn{{runID: "pending", text: "local echo", at: time.Now()}}
	m.transcript = m.transcript.Search("needle")
	m.syncProjection()
	var ids []string
	for _, b := range m.shell.projection.Blocks {
		ids = append(ids, b.ID)
	}
	if len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("filtered ids=%v, want exactly [b]", ids)
	}
}
