// Package app implements Harbor's deterministic terminal shell.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

const (
	StandardSpinnerInterval = 80 * time.Millisecond
	ActiveSpinnerInterval   = 40 * time.Millisecond
	SequenceTimeout         = 2 * time.Second
	StartupDelay            = 500 * time.Millisecond
	StartupMinimum          = 3 * time.Second
)

type Layer uint8

const (
	LayerBase Layer = iota
	LayerSidebar
	LayerAutocomplete
	LayerToast
	LayerModal
	LayerStartup
)

var LayerOrder = [...]Layer{LayerBase, LayerSidebar, LayerAutocomplete, LayerToast, LayerModal, LayerStartup}

// ComposerState selects a visual-only fixture composer posture.
type ComposerState string

const (
	ComposerIdle       ComposerState = "idle"
	ComposerFocused    ComposerState = "focused"
	ComposerDisabled   ComposerState = "disabled"
	ComposerRunning    ComposerState = "running"
	ComposerRetry      ComposerState = "retry 2/3 · 4s"
	ComposerAttachment ComposerState = "attachment · report.pdf"
)

// State selects applicable fixture views without implementing Runtime workflows.
type State struct {
	Route                                                                                string
	Connection                                                                           string
	Composer                                                                             ComposerState
	SidebarOpen, AutocompleteOpen, ToastOpen, Startup, Active, CursorHidden              bool
	Scrolled, ReplayGap, Dropped, Closed, Erased, Intervention, Unknown, Pasted, Focused bool
	Toast                                                                                string
}

type startupStage uint8

const (
	startupWaiting startupStage = iota
	startupVisible
	startupPending
	startupHidden
)

type startupDelayMsg struct{ generation uint64 }
type startupMinimumMsg struct{ generation uint64 }
type spinnerMsg struct{}
type sequenceTimeoutMsg struct{ generation uint64 }

// StartupCompleteMsg explicitly completes startup work.
type StartupCompleteMsg struct{}

// BackdropMsg reports a backdrop release and whether terminal text selection is active.
type BackdropMsg struct{ TextSelectionActive bool }

// Model is a value-updated Bubble Tea shell. All constructor inputs are cloned.
type Model struct {
	width, height      int
	theme              ui.Theme
	reducedMotion      bool
	registry           Registry
	focus              FocusStack
	projection         projection.Projection
	state              State
	spinner            int
	sequence           []string
	sequenceGeneration uint64
	startup            startupStage
	startupGeneration  uint64
	startupMinimumDone bool
	quit               bool
}

// NewModel constructs the fixture-backed terminal foundation.
func NewModel(width, height int, theme ui.Theme, reducedMotion bool, p projection.Projection) Model {
	return Model{width: max(1, width), height: max(1, height), theme: theme, reducedMotion: reducedMotion, registry: DefaultRegistry(), focus: NewFocusStack("composer"), projection: cloneProjection(p), state: State{Route: "session", Connection: "fixture · disconnected", Composer: ComposerIdle}, startup: startupWaiting}
}

// NewModelFromEnvironment compiles terminal capabilities before construction.
func NewModelFromEnvironment(width, height int, environment ui.Environment, reducedMotion bool, p projection.Projection) Model {
	return NewModel(width, height, ui.CompileTheme(environment), reducedMotion, p)
}

func cloneProjection(p projection.Projection) projection.Projection {
	data, err := json.Marshal(p)
	if err != nil {
		return projection.Projection{}
	}
	var out projection.Projection
	if json.Unmarshal(data, &out) != nil {
		return projection.Projection{}
	}
	return out
}

func (m Model) clone() Model {
	m.projection = cloneProjection(m.projection)
	m.sequence = append([]string(nil), m.sequence...)
	m.focus = FocusStack{base: m.focus.base, modals: cloneModals(m.focus.modals)}
	return m
}

// Run mounts one model and waits for Bubble Tea terminal cleanup on every path.
func Run(ctx context.Context, input io.Reader, output io.Writer, model tea.Model) error {
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output))
	stopSignals := watchHostSignals(program)
	defer stopSignals()
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("tui terminal host: %w", err)
	}
	return nil
}

func (m Model) Init() tea.Cmd { return tea.Batch(m.startupDelayCmd(), m.spinnerCmd()) }
func (m Model) startupDelayCmd() tea.Cmd {
	if m.startup != startupWaiting {
		return nil
	}
	generation := m.startupGeneration
	return tea.Tick(StartupDelay, func(time.Time) tea.Msg { return startupDelayMsg{generation: generation} })
}
func (m Model) startupMinimumCmd() tea.Cmd {
	generation := m.startupGeneration
	return tea.Tick(StartupMinimum, func(time.Time) tea.Msg { return startupMinimumMsg{generation: generation} })
}
func (m Model) spinnerCmd() tea.Cmd {
	if m.reducedMotion || (!m.state.Active && m.startup != startupVisible && m.startup != startupPending) {
		return nil
	}
	interval := StandardSpinnerInterval
	if m.state.Active {
		interval = ActiveSpinnerInterval
	}
	return tea.Tick(interval, func(time.Time) tea.Msg { return spinnerMsg{} })
}

// Update applies one deterministic model transition. Modal input always wins.
func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	m = m.clone()
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width = max(1, msg.Width)
		m.height = max(1, msg.Height)
		return m, nil
	case startupDelayMsg:
		if msg.generation != m.startupGeneration || m.startup != startupWaiting {
			return m, nil
		}
		m.startup = startupVisible
		return m, tea.Batch(m.startupMinimumCmd(), m.spinnerCmd())
	case startupMinimumMsg:
		if msg.generation != m.startupGeneration {
			return m, nil
		}
		m.startupMinimumDone = true
		if m.startup == startupPending {
			m.hideStartup()
		}
		return m, nil
	case StartupCompleteMsg:
		if m.startup == startupWaiting {
			m.hideStartup()
			return m, nil
		}
		if m.startup == startupVisible {
			if m.startupMinimumDone {
				m.hideStartup()
			} else {
				m.startup = startupPending
			}
		}
		return m, nil
	case spinnerMsg:
		if m.reducedMotion || (!m.state.Active && m.startup != startupVisible && m.startup != startupPending) {
			return m, nil
		}
		m.spinner = (m.spinner + 1) % len(spinner.Dot.Frames)
		return m, m.spinnerCmd()
	case sequenceTimeoutMsg:
		if msg.generation == m.sequenceGeneration {
			m.sequence = nil
		}
		return m, nil
	case tea.ResumeMsg:
		m.state.ToastOpen = true
		m.state.Toast = "Terminal resumed"
		return m, nil
	case tea.PasteMsg:
		m.state.Pasted = true
		m.state.Composer = ComposerFocused
		return m, nil
	case tea.FocusMsg:
		m.state.Focused = true
		m.state.Composer = ComposerFocused
		return m, nil
	case tea.BlurMsg:
		m.state.Focused = false
		return m, nil
	case BackdropMsg:
		if modal, ok := m.focus.Top(); ok && modal.BackdropClose(msg.TextSelectionActive) {
			m.focus, _, _ = m.focus.Pop()
		}
		return m, nil
	case tea.KeyPressMsg:
		key := canonicalKey(msg.String())
		if _, ok := m.focus.Top(); ok {
			return m.updateModal(key)
		}
		return m.updateBase(key)
	}
	return m, nil
}

func (m *Model) hideStartup() {
	m.startup = startupHidden
	m.startupGeneration++
	m.state.Startup = false
}
func canonicalKey(key string) string {
	if key == "esc" {
		return "escape"
	}
	return key
}

func (m Model) updateModal(key string) (tea.Model, tea.Cmd) {
	modal, _ := m.focus.Top()
	switch key {
	case "escape", "ctrl+c":
		m.focus, _, _ = m.focus.Pop()
		return m, nil
	case "up", "ctrl+p":
		modal = modal.Move(-1)
	case "down", "ctrl+n":
		modal = modal.Move(1)
	case "pgup":
		modal = modal.PageBy(-1)
	case "pgdown":
		modal = modal.PageBy(1)
	case "home":
		modal.Page = 0
		modal.Current = 0
		modal = modal.Move(0)
	case "end":
		modal.Page = max(0, (len(modal.Items)-1)/max(1, modal.PageSize))
		modal.Current = max(0, len(modal.Visible())-1)
		modal = modal.Move(0)
	case "backspace":
		if modal.Query != "" {
			_, size := utf8.DecodeLastRuneInString(modal.Query)
			modal = modal.SetQuery(modal.Query[:len(modal.Query)-size])
		}
	case "enter":
		return m.activateModal(modal)
	case "ctrl+enter":
		if len(modal.ContextActions) > 0 {
			if view, ok := m.registry.Command(modal.ContextActions[0], Context{}); ok && view.Enabled {
				m.focus, _, _ = m.focus.Pop()
				return m.execute(view)
			}
		}
	default:
		if utf8.RuneCountInString(key) == 1 && key != " " || key == " " {
			modal = modal.SetQuery(modal.Query + key)
		}
	}
	m.focus = m.focus.ReplaceTop(modal)
	return m, nil
}

func (m Model) activateModal(modal SelectModel) (tea.Model, tea.Cmd) {
	rows := modal.Visible()
	if len(rows) == 0 {
		return m, nil
	}
	item := rows[min(modal.Current, len(rows)-1)]
	if modal.Title == "Themes" {
		switch item.ID {
		case "light":
			m.theme = ui.NewTheme(ui.ModeLight, m.theme.Profile())
		case "dark":
			m.theme = ui.NewTheme(ui.ModeDark, m.theme.Profile())
		}
		m.focus, _, _ = m.focus.Pop()
		return m, nil
	}
	view, ok := m.registry.Command(CommandID(item.ID), Context{})
	if !ok || !view.Enabled {
		return m, nil
	}
	m.focus, _, _ = m.focus.Pop()
	return m.execute(view)
}

func (m Model) updateBase(key string) (tea.Model, tea.Cmd) {
	if key == "ctrl+z" {
		return m, tea.Suspend
	}
	if key == "escape" {
		if len(m.sequence) > 0 {
			m.clearSequence()
		}
		return m, nil
	}
	if key == "backspace" && len(m.sequence) > 0 {
		m.sequence = m.sequence[:len(m.sequence)-1]
		if len(m.sequence) == 0 {
			m.clearSequence()
		}
		return m, nil
	}
	strokes := append(append([]string(nil), m.sequence...), key)
	view, exact, pending := m.registry.Prefix(strokes, Context{})
	if exact {
		m.clearSequence()
		if view.Enabled {
			return m.execute(view)
		}
		return m, nil
	}
	if pending {
		m.sequence = strokes
		m.sequenceGeneration++
		generation := m.sequenceGeneration
		return m, tea.Tick(SequenceTimeout, func(time.Time) tea.Msg { return sequenceTimeoutMsg{generation: generation} })
	}
	if len(m.sequence) > 0 {
		m.clearSequence()
		view, ok := m.registry.Dispatch(key, Context{})
		if ok && view.Enabled {
			return m.execute(view)
		}
		return m, nil
	}
	return m, nil
}
func (m *Model) clearSequence() { m.sequence = nil; m.sequenceGeneration++ }
func (m Model) execute(view CommandView) (tea.Model, tea.Cmd) {
	switch view.Command.ID {
	case "palette":
		m = m.OpenPalette()
	case "sidebar":
		m.state.SidebarOpen = !m.state.SidebarOpen
	case "theme":
		m = m.WithModal(NewSelect("Themes", []SelectItem{{ID: "dark", Category: "Theme", Title: "Dark"}, {ID: "light", Category: "Theme", Title: "Light"}}, m.focus.Focus()))
	case "quit":
		m.quit = true
		return m, tea.Quit
	}
	return m, nil
}

// OpenPalette opens the registry-backed modal.
func (m Model) OpenPalette() Model {
	m = m.clone()
	views := m.registry.Palette(Context{})
	items := make([]SelectItem, 0, len(views))
	for _, view := range views {
		description := view.Command.Description
		if !view.Enabled {
			description = "Unavailable: " + view.DisabledReason
		}
		items = append(items, SelectItem{ID: string(view.Command.ID), Category: view.Command.Category, Title: view.Command.Title, Description: description, Current: view.Command.ID == "palette"})
	}
	m.focus = m.focus.Push(NewSelect("Commands", items, m.focus.Focus()))
	return m
}
func (m Model) WithState(state State) Model {
	m = m.clone()
	m.state = state
	if state.Route == "" {
		m.state.Route = "session"
	}
	if state.Connection == "" {
		m.state.Connection = "fixture · disconnected"
	}
	if state.Composer == "" {
		m.state.Composer = ComposerIdle
	}
	if state.Startup {
		m.startup = startupVisible
	}
	return m
}
func (m Model) WithModal(modal SelectModel) Model {
	m = m.clone()
	m.focus = m.focus.Push(modal)
	return m
}
func (m Model) Layout() ui.Layout { return ui.Measure(m.width, m.height) }

func (m Model) View() tea.View {
	view := tea.NewView(m.render())
	view.AltScreen = true
	view.ReportFocus = true
	view.WindowTitle = fmt.Sprintf("Harbor fixture %dx%d", m.width, m.height)
	if !m.quit && !m.state.CursorHidden && m.state.Composer != ComposerDisabled {
		view.Cursor = tea.NewCursor(min(4, m.width-1), max(0, m.height-6))
	}
	return view
}
func (m Model) Frame() string { return m.render() }

func (m Model) render() string {
	canvas := newCanvas(m.width, m.height)
	for _, layer := range LayerOrder {
		switch layer {
		case LayerBase:
			m.renderBase(&canvas)
		case LayerSidebar:
			if m.state.SidebarOpen || m.Layout().JoinedSidebar {
				m.renderSidebar(&canvas)
			}
		case LayerAutocomplete:
			if m.state.AutocompleteOpen || len(m.sequence) > 0 {
				m.renderAutocomplete(&canvas)
			}
		case LayerToast:
			if m.state.ToastOpen {
				canvas.put(max(0, m.width-34), 1, ui.PadRight(" "+m.state.Toast+" ", min(32, m.width)), m.theme.Style(ui.RoleInfo, nil))
			}
		case LayerModal:
			if modal, ok := m.focus.Top(); ok {
				m.renderModal(&canvas, modal)
			}
		case LayerStartup:
			if m.startup == startupVisible || m.startup == startupPending {
				label := "[connecting]"
				if !m.reducedMotion {
					label = spinner.Dot.Frames[m.spinner%len(spinner.Dot.Frames)] + " connecting"
				}
				canvas.put(max(0, m.width-ui.Width(label)-2), 0, label, m.theme.Style(ui.RoleWarning, nil))
			}
		}
	}
	return canvas.string()
}

func (m Model) renderBase(c *canvas) {
	l := m.Layout()
	c.fill(m.theme.Style(ui.RoleText, ptrRole(ui.RoleCanvas)))
	title := "HARBOR  /  " + strings.ToUpper(m.state.Route) + " FIXTURE"
	c.put(ui.OuterPadding, 1, title, m.theme.Style(ui.RolePrimary, nil).Bold(true))
	c.put(ui.OuterPadding, 2, ui.PadRight("One active session · Protocol-only fixture projection", l.MainWidth), m.theme.Style(ui.RoleMuted, nil))
	c.put(ui.OuterPadding, 3, ui.PadRight(fmt.Sprintf("terminal size %dx%d · %s", m.width, m.height, m.state.Connection), l.MainWidth), m.theme.Style(ui.RoleMuted, nil))
	y := 5
	width := max(12, l.MainWidth-1)
	if m.state.Route == "home" {
		m.renderCard(c, y, width, ui.RoleInfo, "◇", "Terminal foundation ready", "No Runtime is attached; preview data is local and non-operational.")
		y += 4
	} else {
		compactNotice := false
		if m.height <= 12 && !m.state.Intervention {
			if notices := m.notices(); len(notices) > 0 {
				notice := notices[0]
				c.put(ui.OuterPadding, 4, ui.Truncate(notice.glyph+" "+notice.text, l.MainWidth), m.theme.Style(notice.role, nil))
				compactNotice = true
			}
		}
		if m.state.Intervention {
			if m.height <= 12 {
				c.put(ui.OuterPadding, 5, "! Approval required", m.theme.Style(ui.RoleWarning, nil).Bold(true))
				c.put(ui.OuterPadding, 6, "Approve/Reject unavailable", m.theme.Style(ui.RoleWarning, nil))
				y = 7
			} else {
				m.renderIntervention(c, y, width, l.HorizontalActions)
				y += 7
			}
		}
		for _, notice := range m.notices() {
			if compactNotice {
				break
			}
			if y >= m.height-9 {
				break
			}
			m.renderCard(c, y, width, notice.role, notice.glyph, notice.text)
			y += 4
		}
		for _, block := range m.projection.Blocks {
			if y >= m.height-9 {
				break
			}
			glyph, role := semantic(block.Status)
			label := strings.TrimSpace(block.Text)
			if label == "" {
				label = block.Kind + " · " + block.Status
			}
			m.renderCard(c, y, width, role, glyph, label)
			y += 4
		}
	}
	if m.width >= 160 && y+10 < m.height-7 {
		column := min(56, max(24, (l.MainWidth-5)/2))
		m.renderCardAt(c, ui.OuterPadding, y, column, ui.RoleInfo, "◇", "Planner posture", "ReAct · fixture · no live reasoning")
		m.renderCardAt(c, ui.OuterPadding+column+1, y, column, ui.RoleSuccess, "✓", "Tool posture", "runtime.health · succeeded")
		m.renderCardAt(c, ui.OuterPadding, y+5, column, ui.RoleWarning, "!", "Intervention posture", "1 pending · controls unavailable")
		m.renderCardAt(c, ui.OuterPadding+column+1, y+5, column, ui.RoleInfo, "◇", "Artifact posture", "report.json · 2.4 KB · reference only")
		m.renderCardAt(c, ui.OuterPadding, y+10, column, ui.RoleInfo, "◇", "Identity scope", "acme/operator/01K0… · one active session")
		m.renderCardAt(c, ui.OuterPadding+column+1, y+10, column, ui.RoleSuccess, "✓", "Session lifecycle", "running · durable reference retained")
		m.renderCardAt(c, ui.OuterPadding, y+15, column, ui.RoleWarning, "!", "Projection honesty", "counters partial · analytics bounded")
		m.renderCardAt(c, ui.OuterPadding+column+1, y+15, column, ui.RoleInfo, "◇", "Version posture", "Runtime 1.15 · Protocol 0.1")
		m.renderCardAt(c, ui.OuterPadding, y+20, column, ui.RoleSuccess, "✓", "Recent event", "tool.completed · sequence 42")
		m.renderCardAt(c, ui.OuterPadding+column+1, y+20, column, ui.RoleInfo, "◇", "Retention", "events · runtime scope · complete")
	}
	if m.height > 12 || !m.state.Intervention {
		composerWidth, _ := ui.ComposerGeometry(l, 1)
		composer := ui.Composer(m.theme, min(composerWidth, l.MainWidth-1), string(m.state.Composer), identityLabel(m.projection))
		c.styledBlock(ui.OuterPadding, max(y, m.height-6), composer, m.theme.Style(ui.RoleText, ptrRole(ui.RolePanel)), m.theme.Style(ui.RolePrimary, nil))
	}
	hints := strings.Join(m.registry.Footer(Context{}), "   ")
	c.put(ui.OuterPadding, m.height-1, ui.PadRight(hints, l.ContentWidth), m.theme.Style(ui.RoleMuted, nil))
}

type notice struct {
	glyph, text string
	role        ui.Role
}

func (m Model) notices() []notice {
	var out []notice
	if m.state.Active {
		out = append(out, notice{"◆", "Active stream · following newest output", ui.RoleInfo})
	}
	if m.state.Scrolled {
		out = append(out, notice{"↑", "Scrolled away · new output will not move this view", ui.RoleWarning})
	}
	if m.state.ReplayGap {
		out = append(out, notice{"×", "Replay gap · authoritative reconciliation required", ui.RoleError})
	}
	if m.state.Dropped {
		out = append(out, notice{"!", "Dropped event window · output may be incomplete", ui.RoleWarning})
	}
	if m.state.Closed {
		out = append(out, notice{"○", "Session closed · resumable on a future turn", ui.RoleInfo})
	}
	if m.state.Erased {
		out = append(out, notice{"×", "Session erased · terminal state, start fresh required", ui.RoleError})
	}
	if m.state.Unknown {
		out = append(out, notice{"?", "Unknown event · safe metadata-only fallback", ui.RoleWarning})
	}
	if m.state.Pasted {
		out = append(out, notice{"▣", "Bracketed paste · 3 lines captured locally", ui.RoleInfo})
	}
	if m.state.Focused {
		out = append(out, notice{"●", "Composer focus restored", ui.RoleSuccess})
	}
	return out
}
func semantic(status string) (string, ui.Role) {
	switch status {
	case "completed", "succeeded":
		return "✓", ui.RoleSuccess
	case "failed", "erased":
		return "×", ui.RoleError
	case "pending":
		return "!", ui.RoleWarning
	case "running", "started":
		return "◆", ui.RoleInfo
	default:
		return "◇", ui.RoleInfo
	}
}
func (m Model) renderCard(c *canvas, y, width int, role ui.Role, glyph string, lines ...string) {
	m.renderCardAt(c, ui.OuterPadding, y, width, role, glyph, lines...)
}
func (m Model) renderCardAt(c *canvas, x, y, width int, role ui.Role, glyph string, lines ...string) {
	payload := append([]string{glyph + "  " + lines[0]}, lines[1:]...)
	card := ui.HeavyCard(m.theme, role, width, payload...)
	c.styledBlock(x, y, card, m.theme.Style(ui.RoleText, ptrRole(ui.RolePanel)), m.theme.Style(role, nil))
}
func (m Model) renderIntervention(c *canvas, y, width int, horizontal bool) {
	m.renderCard(c, y, width, ui.RoleWarning, "!", "Operator approval required", "Tool: fixture.health · no action is sent from this preview")
	if horizontal {
		c.put(ui.OuterPadding+3, y+4, "[ Approve unavailable ]   [ Reject unavailable ]", m.theme.Style(ui.RoleWarning, nil).Bold(true))
	} else {
		c.put(ui.OuterPadding+3, y+4, "[ Approve unavailable ]", m.theme.Style(ui.RoleWarning, nil).Bold(true))
		c.put(ui.OuterPadding+3, y+5, "[ Reject unavailable ]", m.theme.Style(ui.RoleWarning, nil).Bold(true))
	}
}

func (m Model) renderSidebar(c *canvas) {
	l := m.Layout()
	x := m.width - ui.SidebarWidth
	if !l.JoinedSidebar {
		c.dim(0, max(0, x), m.theme.Style(ui.RoleMuted, ptrRole(ui.RoleCanvas)).Faint(true))
	}
	x = max(0, x)
	panel := m.theme.Style(ui.RoleText, ptrRole(ui.RolePanel))
	for y := range m.height {
		c.put(x, y, ui.PadRight("", min(ui.SidebarWidth, m.width-x)), panel)
	}
	rows := []string{"RUNTIME CONTEXT", "", "Session", "01K0HARBORFIXTURE0000000000", "one active session", "", "Planner", "ReAct · fixture", "Task", "inspect-runtime · running", "Tool", "runtime.health · succeeded", "Intervention", "1 pending · controls unavailable", "Artifact", "report.json · 2.4 KB", "Stream", m.state.Connection, "Versions", "Runtime 1.15 · Protocol 0.1"}
	for i, row := range rows {
		if i+2 >= m.height {
			break
		}
		role := ui.RoleText
		if row == "RUNTIME CONTEXT" {
			role = ui.RoleAccent
		}
		c.put(x+2, i+2, ui.Truncate(row, ui.SidebarWidth-4), m.theme.Style(role, nil).Bold(i == 0))
	}
}

func (m Model) renderAutocomplete(c *canvas) {
	l := m.Layout()
	w, _ := ui.ComposerGeometry(l, 1)
	w = min(w, l.MainWidth-1)
	var views []CommandView
	if len(m.sequence) > 0 {
		views = m.registry.WhichKey(m.sequence[0], Context{})
	} else {
		views = m.registry.Palette(Context{})
	}
	rows := min(ui.AutocompleteRows, len(views))
	y := max(3, m.height-6-rows)
	for i := range rows {
		view := views[i]
		label := strings.Join(view.Command.Bindings, " ") + "  " + view.Command.Title
		if !view.Enabled {
			label += " · unavailable: " + view.DisabledReason
		}
		c.put(ui.OuterPadding, y+i, ui.PadRight("┃  "+label, w), m.theme.Style(ui.RoleText, ptrRole(ui.RoleElement)))
	}
}

func (m Model) renderModal(c *canvas, modal SelectModel) {
	l := m.Layout()
	w := min(ui.DialogMedium, max(1, l.Width-2))
	maxHeight := max(3, l.Height-2)
	availableRows := max(0, maxHeight-6)
	modal.PageSize = min(max(1, modal.PageSize), max(1, availableRows))
	rows := modal.Visible()
	h := min(maxHeight, max(6, len(rows)+6))
	if l.Height <= 12 {
		h = maxHeight
		availableRows = max(0, h-5)
		if len(rows) > availableRows {
			rows = rows[:availableRows]
		}
	}
	_, _, top := ui.DialogGeometry(l, w, h)
	left := max(1, (m.width-w)/2)
	c.dim(0, m.width, m.theme.Style(ui.RoleMuted, ptrRole(ui.RoleCanvas)).Faint(true))
	panel := m.theme.Style(ui.RoleText, ptrRole(ui.RolePanel))
	for y := range h {
		c.put(left, top+y, ui.PadRight("", w), panel)
	}
	c.put(left+min(4, max(1, w/8)), top+1, ui.Truncate(modal.Title, max(1, w-6)), m.theme.Style(ui.RolePrimary, nil).Bold(true))
	if h > 5 {
		c.put(left+3, top+2, ui.PadRight("Search: "+modal.Query, max(1, w-6)), m.theme.Style(ui.RoleMuted, nil))
	}
	listTop := top + 3
	for i, item := range rows {
		if listTop+i >= top+h-2 {
			break
		}
		role := ui.RoleText
		marker := "  "
		if i == modal.Current {
			role = ui.RolePrimary
			marker = "● "
		}
		label := marker + item.Title
		if item.Description != "" && w >= 40 {
			label += " · " + item.Description
		}
		c.put(left+2, listTop+i, ui.PadRight(label, max(1, w-4)), m.theme.Style(role, ptrRole(ui.RoleElement)).Bold(i == modal.Current))
	}
	if h >= 4 {
		c.put(left+2, top+h-1, ui.Truncate("↑/↓ move · enter select · esc close", max(1, w-4)), m.theme.Style(ui.RoleMuted, nil))
	}
}

func identityLabel(p projection.Projection) string {
	if p.Identity.Tenant == "" {
		return "fixture/preview/no-session"
	}
	return p.Identity.Tenant + "/" + p.Identity.User + "/" + p.Identity.Session
}
func ptrRole(role ui.Role) *ui.Role { return &role }
