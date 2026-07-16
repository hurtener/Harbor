package app

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

func baseModel() Model {
	return NewModel(100, 30, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, FixtureProjection()).WithState(State{})
}

func TestModel_MultiKeySequenceDispatchTimeoutEscapeBackspaceAndCollision(t *testing.T) {
	m := baseModel()
	next, cmd := m.Update(keyMsg('x', tea.ModCtrl))
	m = next.(Model)
	if cmd == nil || len(m.sequence) != 1 || !strings.Contains(m.Frame(), "ctrl+x s") {
		t.Fatal("leader did not open which-key")
	}
	next, _ = m.Update(keyMsg('s', 0))
	m = next.(Model)
	if !m.state.SidebarOpen || len(m.sequence) != 0 {
		t.Fatal("ctrl+x s did not dispatch")
	}
	next, _ = m.Update(keyMsg('x', tea.ModCtrl))
	m = next.(Model)
	next, _ = m.Update(keyMsg(tea.KeyBackspace, 0))
	m = next.(Model)
	if len(m.sequence) != 0 {
		t.Fatal("backspace did not clear sequence")
	}
	next, _ = m.Update(keyMsg('x', tea.ModCtrl))
	m = next.(Model)
	generation := m.sequenceGeneration
	next, _ = m.Update(sequenceTimeoutMsg{generation: generation})
	m = next.(Model)
	if len(m.sequence) != 0 {
		t.Fatal("timeout did not clear sequence")
	}
	next, _ = m.Update(keyMsg('x', tea.ModCtrl))
	m = next.(Model)
	next, _ = m.Update(keyMsg(tea.KeyEscape, 0))
	m = next.(Model)
	if len(m.sequence) != 0 {
		t.Fatal("escape did not clear sequence")
	}
	next, _ = m.Update(keyMsg('x', tea.ModCtrl))
	m = next.(Model)
	next, _ = m.Update(keyMsg('q', 0))
	if !next.(Model).quit {
		t.Fatal("unmatched continuation swallowed base command")
	}
	_, suspend := baseModel().Update(keyMsg('z', tea.ModCtrl))
	if suspend == nil {
		t.Fatal("ctrl+z did not produce suspend command")
	}
	if _, ok := suspend().(tea.SuspendMsg); !ok {
		t.Fatal("ctrl+z command was not terminal suspend")
	}
}

func TestModel_ModalInputPrecedesBaseAndRestoresNestedFocus(t *testing.T) {
	m := baseModel().OpenPalette()
	for _, key := range []tea.KeyPressMsg{keyMsg(tea.KeyDown, 0), keyMsg('n', tea.ModCtrl), keyMsg(tea.KeyPgDown, 0), keyMsg(tea.KeyPgUp, 0), keyMsg(tea.KeyHome, 0), keyMsg(tea.KeyEnd, 0), keyMsg('t', 0), keyMsg('h', 0)} {
		next, _ := m.Update(key)
		m = next.(Model)
	}
	modal, _ := m.focus.Top()
	if modal.Query != "th" {
		t.Fatalf("filter=%q", modal.Query)
	}
	next, _ := m.Update(keyMsg('x', tea.ModCtrl))
	m = next.(Model)
	if len(m.sequence) != 0 {
		t.Fatal("modal leaked key to base leader")
	}
	next, _ = m.Update(keyMsg(tea.KeyBackspace, 0))
	m = next.(Model)
	modal, _ = m.focus.Top()
	if modal.Query != "t" {
		t.Fatalf("backspace query=%q", modal.Query)
	}
	m = m.WithModal(NewSelect("Nested", []SelectItem{{ID: "x", Title: "Nested row"}}, "modal"))
	next, _ = m.Update(keyMsg('c', tea.ModCtrl))
	m = next.(Model)
	if top, _ := m.focus.Top(); top.Title != "Commands" {
		t.Fatal("ctrl-c did not pop only top modal")
	}
	next, _ = m.Update(keyMsg(tea.KeyEscape, 0))
	m = next.(Model)
	if m.focus.Focus() != "composer" {
		t.Fatal("nested focus did not restore")
	}
}

func TestModel_ModalBackdropEnterAndDisabledReason(t *testing.T) {
	m := baseModel().OpenPalette()
	if !strings.Contains(m.Frame(), "Unavailable:") {
		t.Fatal("disabled reason not rendered")
	}
	next, _ := m.Update(BackdropMsg{TextSelectionActive: true})
	m = next.(Model)
	if m.focus.Focus() != "modal" {
		t.Fatal("selection release closed modal")
	}
	next, _ = m.Update(BackdropMsg{})
	m = next.(Model)
	if m.focus.Focus() != "composer" {
		t.Fatal("backdrop did not close modal")
	}
	m = baseModel()
	next, _ = m.Update(keyMsg('x', tea.ModCtrl))
	m = next.(Model)
	next, _ = m.Update(keyMsg('t', 0))
	m = next.(Model)
	if top, _ := m.focus.Top(); top.Title != "Themes" {
		t.Fatal("theme command did not open shared modal")
	}
	next, _ = m.Update(keyMsg(tea.KeyDown, 0))
	m = next.(Model)
	next, _ = m.Update(keyMsg(tea.KeyEnter, 0))
	m = next.(Model)
	if m.theme.Mode() != ui.ModeLight {
		t.Fatal("theme selection did not apply")
	}
	contextModal := NewSelect("Context", []SelectItem{{ID: "row", Title: "Row", Actions: []CommandID{"sidebar"}}}, "composer").Move(0)
	m = baseModel().WithModal(contextModal)
	next, _ = m.Update(keyMsg(tea.KeyEnter, tea.ModCtrl))
	if !next.(Model).state.SidebarOpen {
		t.Fatal("modal context action did not dispatch")
	}
}

func TestModel_StartupStateMachineDelayedMinimumCompletionAndCancellation(t *testing.T) {
	m := baseModel()
	if m.startup != startupWaiting || strings.Contains(m.Frame(), "connecting") {
		t.Fatal("indicator visible before delay")
	}
	generation := m.startupGeneration
	next, cmd := m.Update(startupDelayMsg{generation: generation})
	m = next.(Model)
	if cmd == nil || m.startup != startupVisible || !strings.Contains(ansi.Strip(m.Frame()), "connecting") {
		t.Fatalf("delayed indicator missing cmd_nil=%t stage=%d frame=%q", cmd == nil, m.startup, strings.Split(ansi.Strip(m.Frame()), "\n")[0])
	}
	next, _ = m.Update(StartupCompleteMsg{})
	m = next.(Model)
	if m.startup != startupPending {
		t.Fatal("minimum visibility not held")
	}
	next, _ = m.Update(startupMinimumMsg{generation: generation})
	m = next.(Model)
	if m.startup != startupHidden || m.spinnerCmd() != nil {
		t.Fatal("startup did not hide and cancel ticks")
	}
	next, cmd = m.Update(spinnerMsg{})
	if cmd != nil || next.(Model).spinner != m.spinner {
		t.Fatal("stale spinner tick survived hidden state")
	}
	early := baseModel()
	next, _ = early.Update(StartupCompleteMsg{})
	early = next.(Model)
	if early.startup != startupHidden {
		t.Fatal("early completion did not cancel delay")
	}
	next, _ = early.Update(startupDelayMsg{generation: 0})
	if next.(Model).startup != startupHidden {
		t.Fatal("stale delay resurrected indicator")
	}
	reduced := NewModel(80, 24, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, FixtureProjection())
	next, _ = reduced.Update(startupDelayMsg{})
	if next.(Model).spinnerCmd() != nil {
		t.Fatal("reduced motion scheduled spinner")
	}
}

func TestModel_InitSpinnerAndViewContracts(t *testing.T) {
	m := NewModel(80, 24, ui.NewTheme(ui.ModeDark, ui.ProfileANSI16), false, FixtureProjection()).WithState(State{Active: true})
	if m.Init() == nil || m.spinnerCmd() == nil {
		t.Fatal("active model did not schedule startup and spinner")
	}
	next, cmd := m.Update(spinnerMsg{})
	m = next.(Model)
	if m.spinner != 1 || cmd == nil {
		t.Fatal("active spinner did not reschedule")
	}
	view := m.View()
	if !view.AltScreen || !view.ReportFocus || view.WindowTitle != "Harbor" || view.Cursor == nil {
		t.Fatalf("view contract=%#v", view)
	}
	m = m.WithState(State{Composer: ComposerDisabled, CursorHidden: true})
	if m.View().Cursor != nil {
		t.Fatal("disabled composer exposed cursor")
	}
}

func TestModel_InputPasteResizeFocusAndVisibleBreakpoints(t *testing.T) {
	m := baseModel()
	next, _ := m.Update(tea.PasteMsg{Content: "一\ne\u0301\n👩‍👩‍👧‍👦"})
	m = next.(Model)
	// The pasted draft lands in the composer and IS the feedback — a wide CJK
	// grapheme surviving into the frame also pins the width handling.
	if !m.state.Pasted || !strings.Contains(m.Frame(), "一") {
		t.Fatal("paste state missing")
	}
	m = baseModel()
	next, _ = m.Update(tea.FocusMsg{})
	m = next.(Model)
	// Regaining terminal focus restores composer focus. It deliberately
	// announces nothing: the composer is visibly focused, and a banner for it
	// was canvas noise that outlived the event.
	if !m.state.Focused || m.state.Composer != ComposerFocused {
		t.Fatalf("focus not restored: %+v", m.state)
	}
	next, _ = m.Update(tea.WindowSizeMsg{Width: 79, Height: 24})
	m79 := next.(Model).WithState(State{Intervention: true})
	next, _ = m79.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m80 := next.(Model)
	if strings.Contains(ansi.Strip(m79.Frame()), "Approve unavailable ]   [ Reject") || !strings.Contains(ansi.Strip(m80.Frame()), "Approve unavailable ]   [ Reject") {
		t.Fatal("79/80 action transition not visible")
	}
	m120 := NewModel(120, 30, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, FixtureProjection()).WithState(State{SidebarOpen: true})
	m121 := NewModel(121, 30, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, FixtureProjection()).WithState(State{SidebarOpen: true})
	if m120.Layout().JoinedSidebar || !m121.Layout().JoinedSidebar {
		t.Fatal("120/121 sidebar geometry")
	}
	if !strings.Contains(ansi.Strip(m120.Frame()), "HARBOR") || !strings.Contains(ansi.Strip(m120.Frame()), "RUNTIME CONTEXT") {
		t.Fatal("overlay destroyed base")
	}
	if !strings.Contains(m120.Frame(), "\x1b[2m") {
		t.Fatal("overlay did not apply semantic scrim dimming")
	}
}

func TestModel_BoundaryDeepCopiesProjectionRegistryAndSelect(t *testing.T) {
	p := FixtureProjection()
	original := p.Blocks[0].Text
	m := NewModel(80, 24, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, p)
	p.Blocks[0].Text = "mutated"
	p.Blocks[0].PayloadKeys = []string{"mutated"}
	if !strings.Contains(m.Frame(), original) || strings.Contains(m.Frame(), "mutated") {
		t.Fatal("projection alias crossed model boundary")
	}
	bindings := []string{"x"}
	r, err := NewRegistry(Command{ID: "x", Title: "X", Bindings: bindings})
	if err != nil {
		t.Fatal(err)
	}
	bindings[0] = "y"
	view, _ := r.Dispatch("x", Context{})
	view.Command.Bindings[0] = "z"
	if _, ok := r.Dispatch("x", Context{}); !ok {
		t.Fatal("registry binding alias")
	}
	actions := []CommandID{"x"}
	items := []SelectItem{{ID: "i", Title: "I", Actions: actions}}
	selectModel := NewSelect("S", items, "composer")
	actions[0] = "y"
	items[0].Actions[0] = "z"
	visible := selectModel.Visible()
	visible[0].Actions[0] = "q"
	if selectModel.Visible()[0].Actions[0] != "x" {
		t.Fatal("select action alias")
	}
}

func TestModel_ConcurrentReuse128UpdatesNoAliasOrLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()
	base := baseModel()
	var wg sync.WaitGroup
	errs := make(chan string, 128)
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next, _ := base.Update(tea.WindowSizeMsg{Width: 40 + i, Height: 20})
			model := next.(Model)
			if model.width != 40+i || base.width != 100 {
				errs <- "dimension cross-talk"
			}
			model.projection.Blocks[0].Text = "local"
			if base.projection.Blocks[0].Text == "local" {
				errs <- "projection alias"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if runtime.NumGoroutine() > baseline+2 {
		t.Fatal("goroutine leak")
	}
}

type immediateModel struct{}

func (immediateModel) Init() tea.Cmd                         { return tea.Quit }
func (m immediateModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }
func (immediateModel) View() tea.View                        { return tea.NewView("fixture") }
func TestRun_RepeatedInProcessMountUnmountNoLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()
	for range 20 {
		var output bytes.Buffer
		if err := Run(context.Background(), bytes.NewBuffer(nil), &output, immediateModel{}); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.NumGoroutine() > baseline+2 {
		t.Fatalf("in-process mount leak baseline=%d got=%d", baseline, runtime.NumGoroutine())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, bytes.NewBuffer(nil), &bytes.Buffer{}, immediateModel{}); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestModel_StyledProfilesAndMonochromeSemantics(t *testing.T) {
	profiles := []struct {
		theme    ui.Theme
		sequence string
	}{{ui.NewTheme(ui.ModeDark, ui.ProfileTrueColor), "\x1b[38;2;250;178;131m"}, {ui.NewTheme(ui.ModeLight, ui.ProfileTrueColor), "\x1b[38;2;59;125;216m"}, {ui.NewTheme(ui.ModeDark, ui.ProfileANSI256), "\x1b[38;5;216m"}, {ui.NewTheme(ui.ModeLight, ui.ProfileANSI256), "\x1b[38;5;68m"}, {ui.NewTheme(ui.ModeDark, ui.ProfileANSI16), "\x1b[93m"}, {ui.NewTheme(ui.ModeDark, ui.ProfileMono), "\x1b[1m"}}
	for _, profile := range profiles {
		theme := profile.theme
		frame := NewModel(100, 30, theme, true, FixtureProjection()).WithState(State{Intervention: true, ReplayGap: true}).Frame()
		assertFrameGeometry(t, ansi.Strip(frame), 100, 30)
		if !strings.Contains(frame, profile.sequence) {
			t.Fatalf("profile %d/%d missing exact sequence %q", theme.Profile(), theme.Mode(), profile.sequence)
		}
	}
	// Monochrome must carry state meaning by glyph alone. Only the most severe
	// honesty line shows at a time (chrome, not a stack of cards), so each state
	// is asserted in the frame that actually surfaces it.
	for _, tc := range []struct {
		state State
		glyph string
	}{
		{State{Intervention: true}, "!"},
		{State{ReplayGap: true}, "×"},
		{State{Closed: true}, "○"},
		{State{}, "✓"},
	} {
		mono := NewModel(100, 30, profiles[5].theme, true, FixtureProjection()).WithState(tc.state).Frame()
		if !strings.Contains(mono, tc.glyph) {
			t.Fatalf("mono missing semantic glyph %q for state %+v", tc.glyph, tc.state)
		}
	}
}

func TestNewModelFromEnvironment_NOColorCompilesMonochrome(t *testing.T) {
	model := NewModelFromEnvironment(80, 24, ui.Environment{TERM: "xterm-256color", COLORTERM: "truecolor", NoColor: true}, false, FixtureProjection())
	if model.theme.Profile() != ui.ProfileMono {
		t.Fatal("NO_COLOR did not compile monochrome model")
	}
}

func TestCanvas_GraphemeRightEdgeAndOverlayPreserveCells(t *testing.T) {
	c := newCanvas(8, 2)
	style := ui.NewTheme(ui.ModeDark, ui.ProfileMono).Style(ui.RoleText, nil)
	c.put(0, 0, "界e\u0301👩‍👩‍👧‍👦", style)
	c.put(7, 0, "🙂", style)
	line := strings.Split(c.string(), "\n")[0]
	if ui.Width(line) != 8 || strings.Contains(line, "🙂") {
		t.Fatalf("right-edge frame width=%d %q", ui.Width(line), line)
	}
	c.put(1, 0, "X", style)
	line = strings.Split(c.string(), "\n")[0]
	if ui.Width(line) != 8 || strings.Contains(line, "界") {
		t.Fatalf("continuation overwrite corrupt: %q", line)
	}
	c.dim(0, 8, style.Faint(true))
	if ui.Width(strings.Split(ansi.Strip(c.string()), "\n")[0]) != 8 {
		t.Fatal("overlay changed grapheme geometry")
	}
	c.put(6, 1, "ok", style)
	if !strings.Contains(c.string(), "ok") {
		t.Fatalf("right-position text missing: %q", c.string())
	}
}

func TestProjectionClone_EmptyStillRendersHonestHome(t *testing.T) {
	m := NewModel(40, 12, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, projection.Projection{}).WithState(State{Route: "home"})
	if !strings.Contains(m.Frame(), "Attach a Runtime") {
		t.Fatal("empty home honesty missing")
	}
}
