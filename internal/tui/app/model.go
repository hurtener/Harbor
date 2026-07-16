// Package app implements Harbor's deterministic terminal shell.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
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

// ComposerState selects a composer posture.
type ComposerState string

const (
	ComposerIdle       ComposerState = "idle"
	ComposerFocused    ComposerState = "focused"
	ComposerDisabled   ComposerState = "disabled"
	ComposerRunning    ComposerState = "running"
	ComposerRetry      ComposerState = "retry · submit again"
	ComposerAttachment ComposerState = "attachment · report.pdf"
)

// State selects shell view state.
type State struct {
	Route                                                                              string
	Connection                                                                         string
	Composer                                                                           ComposerState
	ComposerText                                                                       string
	ComposerCursor, SelectionStart, SelectionEnd                                       int
	AutocompleteRows                                                                   []string
	AutocompleteIndex                                                                  int
	AttachmentReady, HasFollowUp                                                       bool
	SelectedBlockID                                                                    string
	Negotiated, TaskControl, SessionLifecycle, SessionScope                            bool
	SidebarOpen, AutocompleteOpen, ToastOpen, Startup, Active, CursorHidden            bool
	Scrolled, ReplayGap, Reconciliation, Dropped, Overflow, Truncated, CountersPartial bool
	AggregateTruncated, AggregatesPartial, AnalyticsBounded                            bool
	Closed, Failed, Erased, Intervention, Unknown, Incomplete, Pasted, Focused         bool
	Toast                                                                              string
	DetailRows                                                                         []string
	Health                                                                             string
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

// CommandMsg asks the operational host to execute one registry command.
type CommandMsg struct{ ID CommandID }

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
	operational        bool
}

// NewModel constructs the detached terminal foundation.
func NewModel(width, height int, theme ui.Theme, reducedMotion bool, p projection.Projection) Model {
	return Model{width: max(1, width), height: max(1, height), theme: theme, reducedMotion: reducedMotion, registry: DefaultRegistry(), focus: NewFocusStack("composer"), projection: cloneProjection(p), state: State{Route: "session", Connection: "disconnected", Composer: ComposerIdle}, startup: startupWaiting}
}

// NewOperationalModel constructs a shell whose chrome is derived only from
// canonical conversation/session projection and local interaction state.
func NewOperationalModel(width, height int, theme ui.Theme, reducedMotion bool, p projection.Projection) Model {
	m := NewModel(width, height, theme, reducedMotion, p)
	m.operational = true
	m.state.Connection = "connecting"
	return m
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
	return runProgram(program)
}

// RunTerminal mounts a model on the process terminal using Bubble Tea's
// default TTY discovery and raw-mode lifecycle.
func RunTerminal(ctx context.Context, model tea.Model) error {
	return runProgram(tea.NewProgram(model, tea.WithContext(ctx)))
}

func runProgram(program *tea.Program) error {
	stopSignals := watchHostSignals(program)
	defer stopSignals()
	model, err := program.Run()
	if finalizer, ok := model.(interface{ Finalize() error }); ok {
		if finalErr := finalizer.Finalize(); err == nil && finalErr != nil {
			err = finalErr
		}
	}
	if err != nil {
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
	if key == "space" {
		return " "
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
			if view, ok := m.registry.Command(modal.ContextActions[0], m.commandContext()); ok && view.Enabled {
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
	view, ok := m.registry.Command(CommandID(item.ID), m.commandContext())
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
	view, exact, pending := m.registry.Prefix(strokes, m.commandContext())
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
		view, ok := m.registry.Dispatch(key, m.commandContext())
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
	case "help":
		rows := m.registry.Help(m.commandContext())
		items := make([]SelectItem, 0, len(rows))
		for i, row := range rows {
			items = append(items, SelectItem{ID: fmt.Sprintf("help-%d", i), Title: row})
		}
		m = m.WithModal(NewSelect("Keyboard help", items, m.focus.Focus()))
	case "sidebar":
		m.state.SidebarOpen = !m.state.SidebarOpen
	case "theme":
		m = m.WithModal(NewSelect("Themes", []SelectItem{{ID: "dark", Category: "Theme", Title: "Dark"}, {ID: "light", Category: "Theme", Title: "Light"}}, m.focus.Focus()))
	case "quit":
		m.quit = true
		return m, tea.Quit
	default:
		id := view.Command.ID
		return m, func() tea.Msg { return CommandMsg{ID: id} }
	}
	return m, nil
}

// OpenPalette opens the registry-backed modal.
func (m Model) OpenPalette() Model {
	m = m.clone()
	views := m.registry.Palette(m.commandContext())
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
		m.state.Connection = "disconnected"
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
	view.WindowTitle = "Harbor"
	if !m.quit && !m.state.CursorHidden && m.state.Composer != ComposerDisabled {
		x, y := min(4, m.width-1), max(0, m.height-6)
		if m.operational {
			x, y = m.operationalCursor()
		}
		view.Cursor = tea.NewCursor(x, y)
	}
	return view
}

func (m Model) operationalCursor() (int, int) {
	layout := m.Layout()
	composerWidth, _ := ui.ComposerGeometry(layout, strings.Count(m.state.ComposerText, "\n")+1)
	inner := max(1, min(composerWidth, layout.MainWidth-1)-3)
	runes := []rune(m.state.ComposerText)
	cursor := max(0, min(len(runes), m.state.ComposerCursor))
	row, column := 0, 0
	for _, r := range runes[:cursor] {
		if r == '\n' || column+1 >= inner {
			row++
			column = 0
			continue
		}
		column++
	}
	visibleRows := min(max(1, row+1), 6)
	composerRows := visibleRows + 3
	visibleRow := row - max(0, row-visibleRows+1)
	return min(m.width-1, ui.OuterPadding+3+column), max(0, m.height-composerRows-1+visibleRow)
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
	title := "HARBOR  /  " + strings.ToUpper(m.state.Route)
	c.put(ui.OuterPadding, 1, title, m.theme.Style(ui.RolePrimary, nil).Bold(true))
	subtitle := "One active session · Protocol-only"
	status := m.state.Connection
	c.put(ui.OuterPadding, 2, ui.PadRight(subtitle, l.MainWidth), m.theme.Style(ui.RoleMuted, nil))
	c.put(ui.OuterPadding, 3, ui.PadRight(status, l.MainWidth), m.theme.Style(ui.RoleMuted, nil))
	y := 5
	width := max(12, l.MainWidth-1)
	if m.state.Route == "home" {
		m.renderCard(c, y, width, ui.RoleInfo, "◇", "Terminal foundation ready", "Attach a Runtime to begin.")
		y += 4
	} else {
		if m.state.Route != "session" && len(m.state.DetailRows) > 0 {
			for _, row := range m.state.DetailRows {
				if y >= m.height-3 {
					break
				}
				c.put(ui.OuterPadding+1, y, ui.Truncate(row, width-2), m.theme.Style(ui.RoleText, nil))
				y++
			}
			return
		}
		compactNotice := false
		if m.height <= 12 && !m.state.Intervention {
			if notices := m.notices(); len(notices) > 0 {
				notice := mostSevere(notices)
				c.put(ui.OuterPadding, 4, ui.Truncate(notice.glyph+" "+notice.text, l.MainWidth), m.theme.Style(notice.role, nil))
				compactNotice = true
			}
		}
		if m.state.Intervention && !m.operational {
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
			if block.ID != "" && block.ID == m.state.SelectedBlockID {
				glyph = "●"
			}
			m.renderCard(c, y, width, role, glyph, label)
			y += 4
		}
	}
	if m.height > 12 || !m.state.Intervention {
		composerWidth, _ := ui.ComposerGeometry(l, strings.Count(m.state.ComposerText, "\n")+1)
		composer := ui.ComposerWithText(m.theme, min(composerWidth, l.MainWidth-1), string(m.state.Composer), identityLabel(m.projection), m.state.ComposerText)
		if m.operational {
			composer = ui.LiveComposer(m.theme, min(composerWidth, l.MainWidth-1), string(m.state.Composer), identityLabel(m.projection), m.state.ComposerText, m.state.ComposerCursor, m.state.SelectionStart, m.state.SelectionEnd)
		}
		composerRows := strings.Count(composer, "\n") + 1
		c.styledBlock(ui.OuterPadding, max(y, m.height-composerRows-1), composer, m.theme.Style(ui.RoleText, ptrRole(ui.RolePanel)), m.theme.Style(ui.RolePrimary, nil))
	}
	hints := strings.Join(m.registry.Footer(m.commandContext()), "   ")
	c.put(ui.OuterPadding, m.height-1, ui.PadRight(hints, l.ContentWidth), m.theme.Style(ui.RoleMuted, nil))
}

type notice struct {
	glyph, text string
	role        ui.Role
}

// noticeSeverity orders honesty notices so the single compact-mode slot surfaces
// the most consequential state (error before warning before info), never hiding
// a failed/erased/dropped state behind a benign "active stream" line.
func noticeSeverity(role ui.Role) int {
	switch role {
	case ui.RoleError:
		return 3
	case ui.RoleWarning:
		return 2
	default:
		return 1
	}
}

// mostSevere returns the highest-severity notice, preserving declaration order
// among equals (first wins).
func mostSevere(notices []notice) notice {
	top := notices[0]
	for _, n := range notices[1:] {
		if noticeSeverity(n.role) > noticeSeverity(top.role) {
			top = n
		}
	}
	return top
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
	if m.state.Reconciliation {
		out = append(out, notice{"!", "Authoritative reconciliation in progress", ui.RoleWarning})
	}
	if m.state.Dropped {
		out = append(out, notice{"!", "Dropped event window · output may be incomplete", ui.RoleWarning})
	}
	if m.state.Overflow {
		out = append(out, notice{"!", "Display updates coalesced · reconciling latest state", ui.RoleWarning})
	}
	if m.state.Truncated {
		out = append(out, notice{"!", "History is truncated · earlier transcript output is unavailable", ui.RoleWarning})
	}
	if m.state.AggregateTruncated {
		out = append(out, notice{"!", "Aggregate window truncated · totals cover a bounded slice, not all history", ui.RoleWarning})
	}
	if m.state.CountersPartial {
		out = append(out, notice{"!", "Session counters are partial · exact totals unavailable", ui.RoleWarning})
	}
	if m.state.AggregatesPartial {
		out = append(out, notice{"!", "Tool aggregates are partial · some tool rollups are incomplete", ui.RoleWarning})
	}
	if m.state.AnalyticsBounded {
		out = append(out, notice{"!", "Tool analytics are bounded best-effort · absence is not zero", ui.RoleWarning})
	}
	if m.state.Incomplete {
		out = append(out, notice{"!", "Some blocks are incomplete", ui.RoleWarning})
	}
	if m.state.Closed {
		out = append(out, notice{"○", "Session completed · resumable on a future turn", ui.RoleInfo})
	}
	if m.state.Failed {
		out = append(out, notice{"×", "Session failed · terminal error state, not resumable", ui.RoleError})
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
	// Order by severity so that when vertical space is scarce the most
	// consequential honesty state (failed/erased/dropped) renders first and is
	// never crowded out by a benign info line (§9). Stable within a severity.
	sort.SliceStable(out, func(i, j int) bool { return noticeSeverity(out[i].role) > noticeSeverity(out[j].role) })
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
	detail := "Canonical intervention is pending."
	for _, block := range m.projection.Blocks {
		if block.Kind == "intervention" && block.Text != "" {
			detail = block.Text
			break
		}
	}
	m.renderCard(c, y, width, ui.RoleWarning, "!", "Operator approval required", detail)
	if horizontal {
		c.put(ui.OuterPadding+3, y+4, "[ Approve unavailable ]   [ Reject unavailable ]", m.theme.Style(ui.RoleWarning, nil).Bold(true))
	} else {
		c.put(ui.OuterPadding+3, y+4, "[ Approve unavailable ]", m.theme.Style(ui.RoleWarning, nil).Bold(true))
		c.put(ui.OuterPadding+3, y+5, "[ Reject unavailable ]", m.theme.Style(ui.RoleWarning, nil).Bold(true))
	}
}

func (m Model) renderSidebar(c *canvas) {
	l := m.Layout()
	// In joined layout the sidebar occupies the slot Measure reserved inside the
	// content width, which preserves the mandated 2-cell right outer padding. An
	// edge-anchored overlay (unjoined) needs no pad and stays flush right.
	x := m.width - ui.SidebarWidth
	if l.JoinedSidebar {
		x = m.width - ui.OuterPadding - ui.SidebarWidth
	}
	if !l.JoinedSidebar {
		c.dim(0, max(0, x), m.theme.Style(ui.RoleMuted, ptrRole(ui.RoleCanvas)).Faint(true))
	}
	x = max(0, x)
	panel := m.theme.Style(ui.RoleText, ptrRole(ui.RolePanel))
	for y := range m.height {
		c.put(x, y, ui.PadRight("", min(ui.SidebarWidth, m.width-x)), panel)
	}
	rows := []string{"RUNTIME CONTEXT", "", "Session", m.projection.Identity.Session, "one active session", ""}
	if m.state.Health != "" {
		rows = append(rows, "Health", m.state.Health)
	}
	rows = append(rows, "Status", m.projection.SessionStatus, "Transcript", fmt.Sprintf("%d blocks", len(m.projection.Blocks)), "Stream", m.state.Connection)
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

// composerTopRow returns the top screen row of the rendered composer, computed
// the same way render() places it, so overlays can anchor above the true
// composer height instead of a fixed magic offset.
func (m Model) composerTopRow() int {
	l := m.Layout()
	composerWidth, _ := ui.ComposerGeometry(l, strings.Count(m.state.ComposerText, "\n")+1)
	composer := ui.ComposerWithText(m.theme, min(composerWidth, l.MainWidth-1), string(m.state.Composer), identityLabel(m.projection), m.state.ComposerText)
	if m.operational {
		composer = ui.LiveComposer(m.theme, min(composerWidth, l.MainWidth-1), string(m.state.Composer), identityLabel(m.projection), m.state.ComposerText, m.state.ComposerCursor, m.state.SelectionStart, m.state.SelectionEnd)
	}
	composerRows := strings.Count(composer, "\n") + 1
	return max(0, m.height-composerRows-1)
}

func (m Model) renderAutocomplete(c *canvas) {
	l := m.Layout()
	w, _ := ui.ComposerGeometry(l, 1)
	w = min(w, l.MainWidth-1)
	// Anchor the popup directly above the composer, clamped to the space between
	// the header (row 3) and the composer top so it can never cover the active
	// input row on short terminals or a tall multi-line draft (§5).
	composerTop := m.composerTopRow()
	available := max(0, composerTop-3)
	if len(m.state.AutocompleteRows) > 0 {
		rows := min(ui.AutocompleteRows, len(m.state.AutocompleteRows), available)
		y := max(3, composerTop-rows)
		for i := range rows {
			prefix := "┃  "
			role := ui.RoleText
			if i == m.state.AutocompleteIndex {
				prefix = "┃  ● "
				role = ui.RolePrimary
			}
			c.put(ui.OuterPadding, y+i, ui.PadRight(prefix+m.state.AutocompleteRows[i], w), m.theme.Style(role, ptrRole(ui.RoleElement)).Bold(i == m.state.AutocompleteIndex))
		}
		return
	}
	var views []CommandView
	if len(m.sequence) > 0 {
		views = m.registry.WhichKey(m.sequence[0], m.commandContext())
	} else {
		views = m.registry.Palette(m.commandContext())
	}
	rows := min(ui.AutocompleteRows, len(views), available)
	y := max(3, composerTop-rows)
	for i := range rows {
		view := views[i]
		label := strings.Join(view.Command.Bindings, " ") + "  " + view.Command.Title
		if !view.Enabled {
			label += " · unavailable: " + view.DisabledReason
		}
		c.put(ui.OuterPadding, y+i, ui.PadRight("┃  "+label, w), m.theme.Style(ui.RoleText, ptrRole(ui.RoleElement)))
	}
}

// dialogWidth selects a fixed dialog tier (60/88/116) large enough for the
// modal's widest content, never exceeding what the terminal can fit (§3 dialog
// width tiers). It only drops below the medium tier on a terminal too narrow to
// hold it.
func (m Model) dialogWidth(l ui.Layout, modal SelectModel) int {
	content := ui.Width(modal.Title) + 6
	for _, item := range modal.Visible() {
		row := "● " + item.Title
		if item.Description != "" {
			row += " · " + item.Description
		}
		if cw := ui.Width(row) + 6; cw > content {
			content = cw
		}
	}
	maxFit := max(1, l.Width-2*ui.OuterPadding)
	w := ui.DialogMedium
	for _, tier := range []int{ui.DialogLarge, ui.DialogXLarge} {
		if content > w && tier <= maxFit {
			w = tier
		}
	}
	return min(w, maxFit)
}

func (m Model) renderModal(c *canvas, modal SelectModel) {
	l := m.Layout()
	w := m.dialogWidth(l, modal)
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
		return "not attached"
	}
	return p.Identity.Tenant + "/" + p.Identity.User + "/" + p.Identity.Session
}
func (m Model) commandContext() Context {
	_, modal := m.focus.Top()
	ctx := Context{ModalOpen: modal, Connected: m.state.Connection == "live", Erased: m.state.Erased, HasTranscript: len(m.projection.Blocks) > 0, HasAttachment: m.state.AttachmentReady, HasFollowUp: m.state.HasFollowUp, TaskControl: !m.operational, SessionLifecycle: !m.operational, SessionScope: true}
	if m.state.Negotiated {
		ctx.TaskControl, ctx.SessionLifecycle, ctx.SessionScope = m.state.TaskControl, m.state.SessionLifecycle, m.state.SessionScope
	}
	return ctx
}
func ptrRole(role ui.Role) *ui.Role { return &role }
