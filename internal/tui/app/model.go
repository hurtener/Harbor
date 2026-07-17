// Package app implements Harbor's deterministic terminal shell.
package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"

	"github.com/hurtener/Harbor/internal/protocol/types"

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
	Route                                        string
	Connection                                   string
	Composer                                     ComposerState
	ComposerText                                 string
	ComposerCursor, SelectionStart, SelectionEnd int
	AutocompleteRows                             []string
	AutocompleteIndex                            int
	AttachmentReady, HasFollowUp                 bool
	// HasTranscript marks that the full conversation projection has blocks even
	// when the shell's windowed copy is empty (the host filters and windows what
	// the shell renders, but transcript commands operate on the whole thing).
	HasTranscript                                                                      bool
	SelectedBlockID                                                                    string
	Negotiated, TaskControl, SessionLifecycle, SessionScope                            bool
	SidebarOpen, AutocompleteOpen, ToastOpen, Startup, Active, CursorHidden            bool
	Scrolled, ReplayGap, Reconciliation, Dropped, Overflow, Truncated, CountersPartial bool
	AggregateTruncated, AggregatesPartial, AnalyticsBounded                            bool
	Closed, Failed, Erased, Intervention, Unknown, Incomplete, Pasted, Focused         bool
	Toast                                                                              string
	DetailRows                                                                         []string
	Health                                                                             string
	// TurnStatus is the canonical per-turn anchor (model · Runtime-reported
	// duration) shown under a finished answer. Empty when there is nothing
	// honest to report.
	TurnStatus string
	// Model is the Runtime's reported LLM model. Empty when the Runtime has not
	// advertised one — never guessed.
	Model string
	// Version / DisplayName / BaseURL feed the session banner. Version and
	// DisplayName come from the Runtime's advertised RuntimeInfo after the
	// post-attach inspect; BaseURL is the attach target. Empty until reported.
	Version, DisplayName, BaseURL string
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
	// scrollLine / followTail drive the session transcript window: line-granular
	// scrolling that follows the tail until the operator scrolls away.
	scrollLine int
	followTail bool
	// layoutCache memoizes per-block layouts (see layoutBlockCached). Shared
	// across the model's value copies: the UI loop is single-goroutine and
	// entries are idempotent.
	layoutCache map[string]layoutCacheEntry
	// themeLocked marks an explicit operator theme choice, which the terminal's
	// reported background must not override.
	themeLocked bool
}

// NewModel constructs the detached terminal foundation.
func NewModel(width, height int, theme ui.Theme, reducedMotion bool, p projection.Projection) Model {
	return Model{width: max(1, width), height: max(1, height), theme: theme, reducedMotion: reducedMotion, registry: DefaultRegistry(), focus: NewFocusStack("composer"), projection: cloneProjection(p), state: State{Route: "session", Connection: "disconnected", Composer: ComposerIdle}, startup: startupWaiting, followTail: true, layoutCache: map[string]layoutCacheEntry{}}
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

// cloneProjection deep-copies the projection so no mutable state crosses the
// model boundary. It is a structured copy, not a JSON round-trip: clone runs on
// every update, and serializing a long conversation cost more per keystroke
// than laying it out.
func cloneProjection(p projection.Projection) projection.Projection {
	out := p
	if p.Blocks != nil {
		out.Blocks = make([]projection.Block, len(p.Blocks))
		for i, b := range p.Blocks {
			b.PayloadKeys = append([]string(nil), b.PayloadKeys...)
			b.Artifacts = append([]types.StateArtifactRef(nil), b.Artifacts...)
			out.Blocks[i] = b
		}
	}
	if p.RunUsage != nil {
		out.RunUsage = make(map[string]projection.Usage, len(p.RunUsage))
		for k, v := range p.RunUsage {
			out.RunUsage[k] = v
		}
	}
	if p.ReplayGap != nil {
		gap := *p.ReplayGap
		out.ReplayGap = &gap
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
// default TTY discovery and raw-mode lifecycle. Alternate-scroll mode is
// enabled for the program's lifetime so mouse-wheel motion arrives as arrow
// keys on the alternate screen — scrolling works without ever capturing the
// mouse, which keeps the terminal's native text selection intact.
func RunTerminal(ctx context.Context, model tea.Model) error {
	// Alternate-scroll only means anything on a real terminal; guarding the
	// writes keeps escape bytes out of a piped stdout (logs, jq consumers)
	// when the program is about to fail loudly anyway.
	if term.IsTerminal(os.Stdout.Fd()) {
		_, _ = os.Stdout.WriteString("\x1b[?1007h")
		defer func() { _, _ = os.Stdout.WriteString("\x1b[?1007l") }()
	}
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

func (m Model) Init() tea.Cmd {
	// Ask the terminal what its background actually is. Environment sniffing
	// (COLORFGBG) is absent on most terminals, so without this a light terminal
	// gets the dark palette and every panel surface fights the background.
	return tea.Batch(m.startupDelayCmd(), m.spinnerCmd(), tea.RequestBackgroundColor)
}

// WithTerminalBackground re-compiles the palette for the terminal's real
// background, preserving the operator's explicit theme override.
func (m Model) WithTerminalBackground(dark bool) Model {
	mode := ui.ModeLight
	if dark {
		mode = ui.ModeDark
	}
	if m.themeLocked || m.theme.Mode() == mode {
		return m
	}
	m = m.clone()
	m.theme = ui.NewTheme(mode, m.theme.Profile())
	return m
}
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
		// Resize reflows every block, so re-anchor a scrolled-away reader to
		// the block they were looking at; a following reader stays on the tail.
		anchorID, anchorDelta, anchored := m.scrollAnchor()
		m.width = max(1, msg.Width)
		m.height = max(1, msg.Height)
		if anchored {
			m.restoreScrollAnchor(anchorID, anchorDelta)
		}
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
		// The pasted draft itself is the feedback — it lands in the composer
		// rather than announcing itself in a banner. (The operational host
		// routes paste through the editor and syncs the same field.)
		m.state.Pasted = true
		m.state.Composer = ComposerFocused
		m.state.ComposerText += msg.Content
		return m, nil
	case tea.BackgroundColorMsg:
		return m.WithTerminalBackground(msg.IsDark()), nil
	case tea.FocusMsg:
		// Regaining terminal focus is not news: it announces nothing and the
		// composer is visibly focused already.
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
	// A held modifier is tolerated on chord continuations: operators routinely
	// keep ctrl down through a ctrl+x leader, so ctrl+x ctrl+r means ctrl+x r
	// unless an explicit ctrl-binding exists for the continuation.
	if len(m.sequence) > 0 {
		if bare, found := strings.CutPrefix(key, "ctrl+"); found && len([]rune(bare)) == 1 {
			if _, exact, pending := m.registry.Prefix(append(append([]string(nil), m.sequence...), key), m.commandContext()); !exact && !pending {
				key = bare
			}
		}
	}
	strokes := append(append([]string(nil), m.sequence...), key)
	view, exact, pending := m.registry.Prefix(strokes, m.commandContext())
	if exact {
		m.clearSequence()
		if view.Enabled {
			return m.execute(view)
		}
		// Disabled commands explain why — a silently-dead chord is
		// indistinguishable from a broken keyboard.
		m.state.ToastOpen = true
		m.state.Toast = "Unavailable: " + view.DisabledReason
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
	inner := max(1, m.sessionContentWidth(layout)-3)
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
			// The sidebar is strictly opt-in (ctrl+x s): the conversation keeps
			// the full width — and clean native selection — until asked.
			if m.state.SidebarOpen {
				m.renderSidebar(&canvas)
			}
		case LayerAutocomplete:
			if m.state.AutocompleteOpen || len(m.sequence) > 0 {
				m.renderAutocomplete(&canvas)
			}
		case LayerToast:
			// The session surface renders its toast above the composer; the
			// top-right overlay stays for the inspection routes.
			if m.state.ToastOpen && m.state.Route != "session" && m.state.Route != "home" {
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
	// The canvas is transparent: it carries no background of its own so the
	// operator's terminal theme shows through, exactly like the surrounding
	// shell. Painting a near-black canvas fought the terminal's own background
	// and left every unbackgrounded text run as a visible band. Discrete
	// surfaces (the user turn, the composer) still own a panel fill.
	c.fill(m.theme.Style(ui.RoleText, nil))
	width := m.sessionContentWidth(l)

	// The bottom-anchored composer is placed first so the transcript region and
	// the single chrome status strip can sit above it.
	composerTop := m.height
	if m.height > 12 || !m.state.Intervention {
		// The composer spans the conversation's full width — a capped input
		// with dead space beside it read as a layout bug on wide terminals.
		composerWidth := width
		status := m.composerStatus()
		// The composer's metadata row is where identity lives (§9) — adjacent to
		// the input. The footer carries key hints only and the status strip owns
		// stream state, so the triple is never printed twice and a non-live
		// connection is never truncated away on a narrow terminal.
		meta := identityLabel(m.projection)
		composer := ui.ComposerWithText(m.theme, composerWidth, string(m.state.Composer), status, meta, m.state.ComposerText)
		if m.operational {
			composer = ui.LiveComposer(m.theme, composerWidth, string(m.state.Composer), status, meta, m.state.ComposerText, m.state.ComposerCursor, m.state.SelectionStart, m.state.SelectionEnd)
		}
		composerRows := strings.Count(composer, "\n") + 1
		composerTop = max(4, m.height-composerRows-1)
		c.styledBlock(ui.OuterPadding, composerTop, composer, m.theme.Style(ui.RoleText, ptrRole(ui.RolePanel)), m.theme.Style(ui.RolePrimary, nil))
	}

	// Runtime inspection routes keep their detail-row rendering.
	if m.state.Route != "session" && m.state.Route != "home" && len(m.state.DetailRows) > 0 {
		c.put(ui.OuterPadding, 1, strings.ToUpper(m.state.Route), m.theme.Style(ui.RoleMuted, nil).Bold(true))
		y := 3
		for _, row := range m.state.DetailRows {
			if y >= composerTop-1 {
				break
			}
			c.put(ui.OuterPadding+1, y, ui.Truncate(row, width-2), m.theme.Style(ui.RoleText, nil))
			y++
		}
		m.renderFooter(c, l)
		return
	}

	// Session / home conversation surface: banner pinned at the top, transcript
	// flowing top-down beneath it, chrome stacked upward from the composer.
	m.renderBanner(c, width)
	chromeRow := composerTop - 1
	// The animated in-flight line sits directly above the composer while a
	// turn runs (the composer's own status row keeps the context readout).
	if working, ok := m.workingLine(); ok && chromeRow >= bannerHeight {
		c.put(ui.OuterPadding, chromeRow, ui.Truncate(working, width), m.theme.Style(ui.RoleAccent, nil))
		chromeRow--
	}
	if m.state.ToastOpen && m.state.Toast != "" && chromeRow >= bannerHeight {
		toast := ui.Truncate(m.state.Toast, width)
		c.put(ui.OuterPadding+max(0, width-ui.Width(toast)), chromeRow, toast, m.theme.Style(ui.RoleInfo, nil))
		chromeRow--
	}
	if strip, role, ok := m.statusStrip(); ok && chromeRow >= bannerHeight {
		c.put(ui.OuterPadding, chromeRow, ui.Truncate(strip, width), m.theme.Style(role, nil))
		chromeRow--
	}
	// A blocked run also shows what the operator can do about it.
	if m.state.Intervention {
		actions := m.interventionActions()
		for i := len(actions) - 1; i >= 0 && chromeRow >= bannerHeight; i-- {
			c.put(ui.OuterPadding+proseIndent, chromeRow, ui.Truncate(actions[i], width-proseIndent), m.theme.Style(ui.RoleWarning, nil).Bold(true))
			chromeRow--
		}
	}
	// Home is the pre-attach surface: it never shows a transcript, only the
	// invitation to attach a Runtime.
	if len(m.projection.Blocks) == 0 || m.state.Route == "home" {
		hint := "Ask anything to begin."
		if m.state.Route == "home" {
			hint = "Attach a Runtime to begin."
		}
		c.put(ui.OuterPadding+5, bannerHeight, hint, m.theme.Style(ui.RoleMuted, nil))
	} else {
		m.renderTranscriptWindow(c, bannerHeight, chromeRow-1, width)
	}
	m.renderFooter(c, l)
}

// statusStrip returns the single most-consequential honesty/lifecycle line to
// pin just above the composer as chrome. Every §9 state stays reachable and
// distinct, but only the most severe one is shown at a time — one quiet line
// instead of a stack of screen-dominating cards. An ordinary live, complete,
// following stream shows nothing at all.
func (m Model) statusStrip() (string, ui.Role, bool) {
	switch {
	// Terminal session states.
	case m.state.Erased:
		return "×  Session erased · terminal, Start Fresh required", ui.RoleError, true
	case m.state.Failed:
		return "×  Session failed · terminal error state, not resumable", ui.RoleError, true
	case m.state.ReplayGap:
		return "×  Replay gap · authoritative reconciliation required", ui.RoleError, true
	// Blocking / attention.
	case m.state.Intervention:
		return "!  Operator approval required", ui.RoleWarning, true
	case m.state.Dropped:
		return "!  Dropped event window · output may be incomplete", ui.RoleWarning, true
	case m.state.Reconciliation:
		return "!  Authoritative reconciliation in progress", ui.RoleWarning, true
	// Partiality — each kind stays distinguishable (§9).
	case m.state.Incomplete:
		return "!  Some blocks are incomplete", ui.RoleWarning, true
	case m.state.Unknown:
		return "?  Unknown event · safe metadata-only fallback", ui.RoleWarning, true
	case m.state.Overflow:
		return "!  Display updates coalesced · showing latest state", ui.RoleMuted, true
	case m.state.Truncated:
		return "!  History truncated · earlier transcript unavailable", ui.RoleMuted, true
	case m.state.AggregateTruncated:
		return "!  Aggregate window truncated · totals cover a bounded slice", ui.RoleMuted, true
	case m.state.CountersPartial:
		return "!  Session counters are partial · exact totals unavailable", ui.RoleMuted, true
	case m.state.AggregatesPartial:
		return "!  Tool aggregates are partial", ui.RoleMuted, true
	case m.state.AnalyticsBounded:
		return "!  Tool analytics are bounded best-effort · absence is not zero", ui.RoleMuted, true
	// Non-terminal informational states.
	case m.state.Closed:
		return "○  Session completed · resumable on a future turn", ui.RoleInfo, true
	case m.state.Scrolled:
		// A scrolled reader is never yanked (§6): say so, rather than letting
		// new output look stalled.
		return "↑  Scrolled away · new output continues below", ui.RoleMuted, true
	}
	return "", ui.RoleMuted, false
}

// renderFooter draws the bottom chrome line: the stream lifecycle first (it is
// an honesty state and must survive truncation on a narrow terminal), then the
// reachable key hints. Identity lives on the composer's metadata row, so the
// triple is never printed twice.
func (m Model) renderFooter(c *canvas, l ui.Layout) {
	parts := make([]string, 0, 2)
	if m.state.Connection != "" {
		parts = append(parts, m.state.Connection)
	}
	if hints := strings.Join(m.registry.Footer(m.commandContext()), "   "); hints != "" {
		parts = append(parts, hints)
	}
	if len(parts) == 0 {
		return
	}
	c.put(ui.OuterPadding, m.height-1, ui.Truncate(strings.Join(parts, "   ·   "), l.ContentWidth), m.theme.Style(ui.RoleMuted, nil))
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
	// Every write carries the panel background: text drawn without it falls back
	// to the terminal default and tears ragged bands through the filled column.
	// Labels stay muted, values bright — hierarchy by brightness, not by boxes.
	type sideRow struct {
		text string
		role ui.Role
		bold bool
	}
	rows := []sideRow{{"RUNTIME CONTEXT", ui.RoleAccent, true}, {"", ui.RoleMuted, false}}
	pair := func(label, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		rows = append(rows, sideRow{label, ui.RoleMuted, false}, sideRow{value, ui.RoleText, false}, sideRow{"", ui.RoleMuted, false})
	}
	// Only rows an operator can act on. Block counts and the stream state carry
	// no decision value (the stream already lives in the footer), so they are
	// not here. Cost / tokens / context need the tasks.get rollup wired through
	// on this route; they are omitted rather than shown as a fake zero.
	pair("Session", m.projection.Identity.Session)
	pair("Model", m.state.Model)
	pair("Status", m.projection.SessionStatus)
	pair("Health", m.state.Health)
	interior := max(1, min(ui.SidebarWidth, m.width-x)-4)
	for i, row := range rows {
		if i+2 >= m.height {
			break
		}
		if row.text == "" {
			continue
		}
		c.put(x+2, i+2, ui.Truncate(row.text, interior), m.theme.Style(row.role, ptrRole(ui.RolePanel)).Bold(row.bold))
	}
}

// composerTopRow returns the top screen row of the rendered composer, computed
// the same way render() places it, so overlays can anchor above the true
// composer height instead of a fixed magic offset.
func (m Model) composerTopRow() int {
	l := m.Layout()
	composerWidth := m.sessionContentWidth(l)
	composer := ui.ComposerWithText(m.theme, composerWidth, string(m.state.Composer), m.composerStatus(), identityLabel(m.projection), m.state.ComposerText)
	if m.operational {
		composer = ui.LiveComposer(m.theme, composerWidth, string(m.state.Composer), m.composerStatus(), identityLabel(m.projection), m.state.ComposerText, m.state.ComposerCursor, m.state.SelectionStart, m.state.SelectionEnd)
	}
	composerRows := strings.Count(composer, "\n") + 1
	return max(0, m.height-composerRows-1)
}

func (m Model) renderAutocomplete(c *canvas) {
	l := m.Layout()
	w := m.sessionContentWidth(l)
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
	ctx := Context{ModalOpen: modal, Connected: m.state.Connection == "live", Erased: m.state.Erased, HasTranscript: len(m.projection.Blocks) > 0 || m.state.HasTranscript, HasAttachment: m.state.AttachmentReady, HasFollowUp: m.state.HasFollowUp, TaskControl: !m.operational, SessionLifecycle: !m.operational, SessionScope: true}
	if m.state.Negotiated {
		ctx.TaskControl, ctx.SessionLifecycle, ctx.SessionScope = m.state.TaskControl, m.state.SessionLifecycle, m.state.SessionScope
	}
	return ctx
}
func ptrRole(role ui.Role) *ui.Role { return &role }
