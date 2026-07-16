package app

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

// The transcript renders each block with a treatment keyed on its Kind rather
// than one uniform card: the user turn is the only boxed element (an accent
// left rule over a subtle panel), the assistant answer is borderless flowing
// prose, reasoning is a muted subordinate section, and tools / lifecycle are
// compact one-liners. Blocks are measured so the transcript can be anchored to
// the newest content just above the composer.

const (
	proseIndent     = 3 // assistant / user body indent (2-cell icon gutter + 1)
	reasoningIndent = 5
	blockGap        = 1 // blank row between blocks
)

// drawOp is one positioned write relative to a laid-out block's top-left.
type drawOp struct {
	dx, dy int
	text   string
	style  lipgloss.Style
	fillBG *ui.Role // when set, the whole block-width row is filled first
}

// laidBlock is a fully measured, ready-to-place block visual.
type laidBlock struct {
	height int
	ops    []drawOp
}

// layoutBlock computes the visual for one transcript block at the given content
// width, dispatching on Kind. A height of 0 means the block renders nothing
// (e.g. an empty fallback event) and is skipped by the placer.
func (m Model) layoutBlock(b projection.Block, width int, selected bool) laidBlock {
	switch b.Kind {
	case "user":
		return m.layoutUser(b, width, selected)
	case "reasoning":
		return m.layoutReasoning(b, width)
	case "tool":
		return m.layoutTool(b, width)
	case "task", "session", "result":
		return m.layoutLifecycle(b, width)
	case "intervention":
		return m.layoutInterventionBlock(b, width)
	case "event":
		return m.layoutEvent(b, width)
	default: // "text" and any unknown kind → assistant prose
		return m.layoutAssistant(b, width)
	}
}

// layoutUser boxes the user turn: an accent left rule over a subtle panel fill.
func (m Model) layoutUser(b projection.Block, width int, selected bool) laidBlock {
	panel := ui.RolePanel
	barRole := ui.RoleAccent
	if selected {
		barRole = ui.RolePrimary
	}
	lines := wrapPlain(b.Text, width-proseIndent-1)
	if len(lines) == 0 {
		lines = []string{""}
	}
	bar := m.theme.Style(barRole, &panel)
	body := m.theme.Style(ui.RoleText, &panel)
	ops := make([]drawOp, 0, len(lines)*3)
	for i, line := range lines {
		ops = append(ops,
			drawOp{dy: i, fillBG: &panel},
			drawOp{dx: 0, dy: i, text: "┃", style: bar},
			drawOp{dx: proseIndent, dy: i, text: line, style: body},
		)
	}
	return laidBlock{height: len(lines), ops: ops}
}

// layoutAssistant renders the answer as borderless flowing prose (markdown when
// the source warrants it), indented under a 2-cell gutter, no box, no glyph.
func (m Model) layoutAssistant(b projection.Block, width int) laidBlock {
	lines := m.proseLines(b.Text, width-proseIndent, ui.RoleText)
	if b.Incomplete && len(lines) > 0 {
		lines = append(lines, m.dim("▌", ui.RoleMuted))
	}
	if len(lines) == 0 {
		return laidBlock{}
	}
	ops := make([]drawOp, 0, len(lines))
	for i, styled := range lines {
		ops = append(ops, drawOp{dx: proseIndent, dy: i, text: styled})
	}
	return laidBlock{height: len(lines), ops: ops}
}

// layoutReasoning renders thinking as a muted, subordinate section with a
// distinct header, clearly below the answer in the visual hierarchy.
func (m Model) layoutReasoning(b projection.Block, width int) laidBlock {
	header := "Thought"
	glyph := "─"
	if b.Incomplete {
		header = "Thinking"
		glyph = "⋯"
	}
	if d := reasoningDuration(b); d != "" {
		header += " · " + d
	}
	ops := []drawOp{{dx: 0, dy: 0, text: glyph + " " + header, style: m.theme.Style(ui.RoleWarning, nil).Faint(true)}}
	h := 1
	for _, styled := range m.proseLines(b.Text, width-reasoningIndent, ui.RoleMuted) {
		ops = append(ops, drawOp{dx: reasoningIndent, dy: h, text: styled})
		h++
	}
	return laidBlock{height: h, ops: ops}
}

// layoutTool renders a tool call/result as one compact status line.
func (m Model) layoutTool(b projection.Block, width int) laidBlock {
	name := b.Tool
	if name == "" {
		name = "tool"
	}
	glyph, role := lifecycleGlyph(b.Status)
	label := fmt.Sprintf("%s %s", name, strings.TrimSpace(b.Status))
	if b.Text != "" {
		label = fmt.Sprintf("%s  %s", name, firstLine(b.Text))
	}
	line := ui.Truncate(glyph+"  "+label, width-proseIndent)
	return laidBlock{height: 1, ops: []drawOp{{dx: proseIndent, dy: 0, text: line, style: m.theme.Style(role, nil)}}}
}

// layoutLifecycle renders a task/session/result block as one muted line.
func (m Model) layoutLifecycle(b projection.Block, width int) laidBlock {
	glyph, role := lifecycleGlyph(b.Status)
	text := b.Text
	if text == "" {
		text = strings.TrimSpace(b.Kind + " · " + b.Status)
	}
	line := ui.Truncate(glyph+"  "+text, width-proseIndent)
	return laidBlock{height: 1, ops: []drawOp{{dx: proseIndent, dy: 0, text: line, style: m.theme.Style(role, nil).Faint(true)}}}
}

// layoutInterventionBlock renders a pending intervention as a warning callout.
func (m Model) layoutInterventionBlock(b projection.Block, width int) laidBlock {
	warn := ui.RoleWarning
	panel := ui.RolePanel
	detail := b.Text
	if detail == "" {
		detail = "Operator approval required."
	}
	lines := wrapPlain(detail, width-proseIndent-1)
	bar := m.theme.Style(warn, &panel)
	body := m.theme.Style(ui.RoleWarning, &panel)
	ops := make([]drawOp, 0, len(lines)*3)
	for i, line := range lines {
		ops = append(ops,
			drawOp{dy: i, fillBG: &panel},
			drawOp{dx: 0, dy: i, text: "┃", style: bar},
			drawOp{dx: proseIndent, dy: i, text: line, style: body},
		)
	}
	return laidBlock{height: len(lines), ops: ops}
}

// layoutEvent renders a canonical event only when it carries readable content;
// empty metadata-only fallback events are suppressed to keep the canvas calm.
func (m Model) layoutEvent(b projection.Block, width int) laidBlock {
	text := strings.TrimSpace(b.Text)
	if text == "" {
		return laidBlock{}
	}
	line := ui.Truncate("· "+text, width-proseIndent)
	return laidBlock{height: 1, ops: []drawOp{{dx: proseIndent, dy: 0, text: line, style: m.theme.Style(ui.RoleMuted, nil).Faint(true)}}}
}

// proseLines renders body text to styled, width-bounded lines. Markdown-capable
// content routes through the markdown renderer; everything else is plain
// grapheme-safe word wrap. Both keep earlier lines stable as text streams.
func (m Model) proseLines(text string, width int, role ui.Role) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	style := m.theme.Style(role, nil)
	out := make([]string, 0, 4)
	for _, line := range wrapPlain(text, width) {
		out = append(out, style.Render(line))
	}
	return out
}

func (m Model) dim(text string, role ui.Role) string { return m.theme.Style(role, nil).Faint(true).Render(text) }

// placeBlock draws a laid-out block at (x, y).
func (m Model) placeBlock(c *canvas, x, y, width int, lb laidBlock) {
	for _, op := range lb.ops {
		if op.fillBG != nil {
			c.put(x, y+op.dy, ui.PadRight("", width), m.theme.Style(ui.RoleText, op.fillBG))
			continue
		}
		c.put(x+op.dx, y+op.dy, op.text, op.style)
	}
}

// renderTranscript lays out the windowed blocks and places the newest that fit
// anchored to the bottom of the region [top, bottom], so streamed output is
// always visible just above the composer and short transcripts sit calmly with
// empty space above rather than pinned to the top.
func (m Model) renderTranscript(c *canvas, top, bottom, width int) {
	blocks := m.projection.Blocks
	if len(blocks) == 0 || bottom < top {
		return
	}
	laid := make([]laidBlock, len(blocks))
	for i, b := range blocks {
		laid[i] = m.layoutBlock(b, width, b.ID == m.state.SelectedBlockID)
	}
	avail := bottom - top + 1
	start, used := 0, 0
	for i := len(blocks) - 1; i >= 0; i-- {
		h := laid[i].height
		if h == 0 {
			continue
		}
		step := h
		if used > 0 {
			step += blockGap
		}
		if used+step > avail && used > 0 {
			start = i + 1
			break
		}
		used += step
	}
	y := max(top, bottom+1-used)
	for i := start; i < len(blocks); i++ {
		if laid[i].height == 0 {
			continue
		}
		m.placeBlock(c, ui.OuterPadding, y, width, laid[i])
		y += laid[i].height + blockGap
	}
}

// lifecycleGlyph maps a status to a calm glyph + semantic role.
func lifecycleGlyph(status string) (string, ui.Role) {
	switch status {
	case "completed", "succeeded":
		return "✓", ui.RoleSuccess
	case "failed", "erased":
		return "✗", ui.RoleError
	case "pending", "paused":
		return "◔", ui.RoleWarning
	case "running", "started":
		return "▸", ui.RoleInfo
	default:
		return "·", ui.RoleMuted
	}
}

func reasoningDuration(b projection.Block) string {
	if b.At.IsZero() {
		return ""
	}
	// A completed reasoning block without an end timestamp cannot show a
	// duration honestly yet; the per-turn status work threads CompletedAt.
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// wrapPlain word-wraps text to width visible cells, grapheme-safe, preserving
// hard newlines and hard-breaking words longer than the width.
func wrapPlain(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	var out []string
	for _, para := range strings.Split(text, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line, lineW := "", 0
		flush := func() {
			for ui.Width(line) > width {
				acc, accW := "", 0
				for _, cl := range ui.Clusters(line) {
					if accW+cl.Width > width && acc != "" {
						break
					}
					acc += cl.Text
					accW += cl.Width
				}
				out = append(out, acc)
				line = line[len(acc):]
			}
		}
		for _, w := range words {
			ww := ui.Width(w)
			switch {
			case lineW == 0:
				line, lineW = w, ww
			case lineW+1+ww <= width:
				line += " " + w
				lineW += 1 + ww
			default:
				flush()
				if line != "" {
					out = append(out, line)
				}
				line, lineW = w, ww
			}
		}
		flush()
		out = append(out, line)
	}
	return out
}
