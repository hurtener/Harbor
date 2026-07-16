package app

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

// The session conversation renders INLINE, in the terminal's normal buffer —
// the same model as the surrounding shell. Completed blocks are printed once
// into native scrollback (immutable, selectable, copyable, scrolled by the
// terminal itself), and the managed live region holds only what is still
// changing: the streaming tail, the status strip, and the composer. Full-screen
// alternate-screen rendering remains for the Runtime inspection routes, where a
// dashboard genuinely wants to own the whole surface.
//
// This is why terminal selection and copy work: nothing else shares the row
// with the conversation, and long output is reachable with the terminal's own
// scrollback rather than a bespoke pager.

// RenderBlockLines renders one transcript block to styled lines with trailing
// blanks trimmed, ready for the terminal's normal buffer.
func (m Model) RenderBlockLines(b projection.Block, width int) []string {
	laid := m.layoutBlock(b, width, false)
	if laid.height == 0 {
		return nil
	}
	c := newCanvas(width, laid.height)
	m.placeBlock(&c, 0, 0, width, laid)
	return c.rowsTrimmed()
}

// flushableConversational reports whether a conversational block's content is
// final and may be printed into scrollback. Printed lines are immutable, so a
// block flushes only once nothing about it can change. Interventions never
// flush: they are actionable and must stay live until resolved.
func flushableConversational(b projection.Block) bool {
	switch b.Kind {
	case "user":
		return true
	case "text", "reasoning":
		return !b.Incomplete
	case "tool":
		if b.Incomplete {
			return false
		}
		switch b.Status {
		case "", "invoked", "pending", "running", "started", "spawned":
			return false
		}
		return true
	}
	return false
}

func (m RuntimeModel) inlineWidth() int {
	return max(20, m.shell.width-2*ui.OuterPadding)
}

// padLines prefixes every line with the outer gutter.
func padLines(lines []string) []string {
	pad := strings.Repeat(" ", ui.OuterPadding)
	out := make([]string, len(lines))
	for i, line := range lines {
		if line == "" {
			out[i] = ""
			continue
		}
		out[i] = pad + line
	}
	return out
}

// collectFlushes renders every newly-final conversational block (strictly in
// conversation order — a completed block behind a still-streaming one waits)
// plus any due per-turn anchors, marks them flushed, and returns one printable
// unit per block. The caller emits them via tea.Println.
func (m *RuntimeModel) collectFlushes() []string {
	width := m.inlineWidth()
	shell := m.shell
	// The per-turn anchor is flushed as its own line when the run completes,
	// never baked into the answer block.
	shell.state.TurnStatus = ""
	blocks := m.withLocalTurns(m.transcript.Projection.Blocks)
	var units []string
	for _, b := range blocks {
		if !conversational(b.Kind) {
			continue
		}
		if b.Kind == "intervention" {
			continue
		}
		if m.flushed[b.ID] {
			continue
		}
		if !flushableConversational(b) {
			break
		}
		// Reasoning and tool detail flush collapsed unless explicitly expanded:
		// the header records that the agent thought / called a tool, and the
		// body is available on demand before it scrolls away (ctrl+x r / o).
		if b.Kind == "reasoning" && !m.expandedReasoning[b.ID] {
			b.Text = ""
		}
		if b.Kind == "tool" && !m.expandedTools[b.ID] {
			b.Text = ""
		}
		lines := shell.RenderBlockLines(b, width)
		if len(lines) == 0 {
			m.flushed[b.ID] = true
			continue
		}
		m.flushed[b.ID] = true
		units = append(units, strings.Join(padLines(lines), "\n")+"\n")
	}
	units = append(units, m.collectAnchors(width)...)
	return units
}

// collectAnchors emits the per-turn "▣ model · duration" line for every run
// whose task is terminal and whose conversational output has fully flushed.
func (m *RuntimeModel) collectAnchors(width int) []string {
	var units []string
	for _, b := range m.transcript.Projection.Blocks {
		if b.Kind != "task" || !terminalTurnStatus(b.Status) || b.DurationMS <= 0 {
			continue
		}
		if m.anchored[b.ID] {
			continue
		}
		pendingRun := false
		for _, other := range m.transcript.Projection.Blocks {
			if other.RunID == b.RunID && conversational(other.Kind) && other.Kind != "intervention" && !m.flushed[other.ID] {
				pendingRun = true
				break
			}
		}
		if pendingRun {
			continue
		}
		parts := make([]string, 0, 2)
		if m.shell.state.Model != "" {
			parts = append(parts, m.shell.state.Model)
		}
		parts = append(parts, fmt.Sprintf("%.1fs", float64(b.DurationMS)/1000))
		m.anchored[b.ID] = true
		glyph := m.shell.theme.Style(ui.RoleAccent, nil).Render("▣")
		label := m.shell.theme.Style(ui.RoleMuted, nil).Render(strings.Join(parts, "  ·  "))
		pad := strings.Repeat(" ", ui.OuterPadding+proseIndent)
		units = append(units, pad+glyph+" "+label+"\n")
	}
	return units
}

// flushCmd converts pending flushes into a single scrollback write, so block
// order can never interleave.
func (m *RuntimeModel) flushCmd() tea.Cmd {
	units := m.collectFlushes()
	if len(units) == 0 {
		return nil
	}
	content := strings.TrimSuffix(strings.Join(units, "\n"), "\n")
	return tea.Println(content)
}

// latestBodyUnit renders the newest block of a kind with its body expanded, so
// the operator can pull a collapsed thought or tool result into scrollback.
func (m RuntimeModel) latestBodyUnit(kind string) (string, bool) {
	blocks := m.transcript.Projection.Blocks
	for i := len(blocks) - 1; i >= 0; i-- {
		b := blocks[i]
		if b.Kind != kind || strings.TrimSpace(b.Text) == "" {
			continue
		}
		shell := m.shell
		shell.state.TurnStatus = ""
		lines := shell.RenderBlockLines(b, m.inlineWidth())
		if len(lines) == 0 {
			return "", false
		}
		return strings.Join(padLines(lines), "\n"), true
	}
	return "", false
}

// welcomeBanner opens the session the way the empty alternate screen used to:
// the wordmark plus a quiet identity line, printed once into scrollback when
// the app claims the viewport.
func (m RuntimeModel) welcomeBanner() string {
	pad := strings.Repeat(" ", ui.OuterPadding)
	title := m.shell.theme.Style(ui.RolePrimary, nil).Bold(true).Render("Harbor")
	sub := m.shell.theme.Style(ui.RoleMuted, nil).Render("session " + m.identity.Session + "  ·  ask anything to begin")
	return pad + title + "\n" + pad + sub + "\n"
}

// inlineView assembles the managed live region: streaming tail, chrome, and
// composer. It returns the content plus the composer cursor position relative
// to the region's first row.
func (m RuntimeModel) inlineView() (string, int, int) {
	width := m.inlineWidth()
	var rows []string

	// Live (unflushed) conversation: the streaming reasoning/answer tail and any
	// pending intervention. Reasoning streams with its body visible — it
	// collapses only once complete and flushed.
	var liveLines []string
	shell := m.shell
	for _, b := range m.transcript.Projection.Blocks {
		if !conversational(b.Kind) || m.flushed[b.ID] {
			continue
		}
		if lines := shell.RenderBlockLines(b, width); len(lines) > 0 {
			if len(liveLines) > 0 {
				liveLines = append(liveLines, "")
			}
			liveLines = append(liveLines, lines...)
		}
	}
	// The managed region must never exceed the terminal height, or the renderer
	// clips whole sections (a modal opening off-screen looks like dead input).
	// Fixed chrome is measured first; the live tail gets what remains.
	composerLines, cursorX, cursorRow := m.inlineComposer()
	overlay := m.inlineOverlay(width)
	fixed := len(composerLines) + len(overlay) + 3 // toast/strip/footer allowance
	if m.shell.state.Intervention {
		fixed += len(m.shell.interventionActions())
	}
	budget := max(0, m.shell.height-fixed-1)
	if len(liveLines) > budget {
		liveLines = liveLines[len(liveLines)-budget:]
	}
	if len(liveLines) > 0 {
		rows = append(rows, padLines(liveLines)...)
		rows = append(rows, "")
	}

	if m.shell.state.ToastOpen && m.shell.state.Toast != "" {
		rows = append(rows, strings.Repeat(" ", ui.OuterPadding)+m.shell.theme.Style(ui.RoleInfo, nil).Render(ui.Truncate(m.shell.state.Toast, width)))
	}
	if strip, role, ok := m.shell.statusStrip(); ok {
		rows = append(rows, strings.Repeat(" ", ui.OuterPadding)+m.shell.theme.Style(role, nil).Render(ui.Truncate(strip, width)))
	}
	if m.shell.state.Intervention {
		for _, action := range m.shell.interventionActions() {
			rows = append(rows, strings.Repeat(" ", ui.OuterPadding+proseIndent)+m.shell.theme.Style(ui.RoleWarning, nil).Bold(true).Render(action))
		}
	}
	rows = append(rows, overlay...)

	composerStart := len(rows)
	rows = append(rows, padLines(composerLines)...)

	hints := strings.Join(m.shell.registry.Footer(m.shell.commandContext()), "   ")
	footer := m.shell.state.Connection
	if hints != "" {
		if footer != "" {
			footer += "   ·   "
		}
		footer += hints
	}
	if footer != "" {
		rows = append(rows, strings.Repeat(" ", ui.OuterPadding)+m.shell.theme.Style(ui.RoleMuted, nil).Render(ui.Truncate(footer, width)))
	}
	return strings.Join(rows, "\n"), cursorX, composerStart + cursorRow
}

// inlineOverlay renders the modal / which-key / autocomplete affordances as
// plain rows directly above the composer, the inline equivalent of the
// alternate screen's floating layers.
func (m RuntimeModel) inlineOverlay(width int) []string {
	pad := strings.Repeat(" ", ui.OuterPadding)
	theme := m.shell.theme
	var rows []string
	if modal, ok := m.shell.focus.Top(); ok {
		rows = append(rows, pad+theme.Style(ui.RolePrimary, nil).Bold(true).Render(ui.Truncate(modal.Title, width)))
		rows = append(rows, pad+theme.Style(ui.RoleMuted, nil).Render(ui.Truncate("Search: "+modal.Query, width)))
		visible := modal.Visible()
		for i, item := range visible {
			if i >= 8 {
				rows = append(rows, pad+theme.Style(ui.RoleMuted, nil).Render(fmt.Sprintf("… %d more", len(visible)-i)))
				break
			}
			marker, role := "  ", ui.RoleText
			if i == modal.Current {
				marker, role = "● ", ui.RolePrimary
			}
			label := marker + item.Title
			if item.Description != "" {
				label += "  ·  " + item.Description
			}
			rows = append(rows, pad+theme.Style(role, nil).Bold(i == modal.Current).Render(ui.Truncate(label, width)))
		}
		rows = append(rows, pad+theme.Style(ui.RoleMuted, nil).Render("↑/↓ move · enter select · esc close"))
		return rows
	}
	if len(m.shell.sequence) > 0 {
		for i, view := range m.shell.registry.WhichKey(m.shell.sequence[0], m.shell.commandContext()) {
			if i >= ui.AutocompleteRows {
				break
			}
			label := strings.Join(view.Command.Bindings, " ") + "  " + view.Command.Title
			if !view.Enabled {
				label += " · unavailable: " + view.DisabledReason
			}
			rows = append(rows, pad+theme.Style(ui.RoleMuted, nil).Render(ui.Truncate(label, width)))
		}
		return rows
	}
	if m.shell.state.AutocompleteOpen {
		for i, row := range m.shell.state.AutocompleteRows {
			if i >= ui.AutocompleteRows {
				break
			}
			marker, role := "  ", ui.RoleText
			if i == m.shell.state.AutocompleteIndex {
				marker, role = "● ", ui.RolePrimary
			}
			rows = append(rows, pad+theme.Style(role, nil).Bold(i == m.shell.state.AutocompleteIndex).Render(ui.Truncate(marker+row, width)))
		}
	}
	return rows
}

// inlineComposer renders the composer for the inline region and reports the
// cursor position within it.
func (m RuntimeModel) inlineComposer() ([]string, int, int) {
	l := ui.Measure(m.shell.width, m.shell.height)
	composerWidth, _ := ui.ComposerGeometry(l, strings.Count(m.shell.state.ComposerText, "\n")+1)
	composer := ui.LiveComposer(m.shell.theme, composerWidth, string(m.shell.state.Composer), m.shell.composerStatus(), identityLabel(m.shell.projection), m.shell.state.ComposerText, m.shell.state.ComposerCursor, m.shell.state.SelectionStart, m.shell.state.SelectionEnd)
	lines := strings.Split(composer, "\n")

	inner := max(1, composerWidth-3)
	runes := []rune(m.shell.state.ComposerText)
	cursor := max(0, min(len(runes), m.shell.state.ComposerCursor))
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
	visibleRow := row - max(0, row-visibleRows+1)
	return lines, ui.OuterPadding + 3 + column, visibleRow
}
