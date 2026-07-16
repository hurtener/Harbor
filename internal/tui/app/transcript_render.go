package app

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"

	"github.com/hurtener/Harbor/internal/tui/markdown"
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
	ops, h := m.proseOps(b.Text, width, proseIndent, 0, ui.RoleText)
	if h == 0 {
		return laidBlock{}
	}
	if b.Incomplete {
		ops = append(ops, drawOp{dx: proseIndent, dy: h, text: "▌", style: m.theme.Style(ui.RoleMuted, nil).Faint(true)})
		h++
	}
	return laidBlock{height: h, ops: ops}
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
	ops := []drawOp{{dx: 0, dy: 0, text: glyph + "  " + header, style: m.theme.Style(ui.RoleWarning, nil).Faint(true)}}
	body, bh := m.proseOps(b.Text, width, reasoningIndent, 1, ui.RoleMuted)
	return laidBlock{height: 1 + bh, ops: append(ops, body...)}
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

// proseOps lays body text out as canvas draw ops starting at row dy0, returning
// the ops and the row count. The canvas is a cell grid that takes plain text
// plus one style per write, so prose is emitted as styled spans — never as
// pre-styled ANSI, which the cell splitter cannot consume.
func (m Model) proseOps(text string, width, indent, dy0 int, role ui.Role) ([]drawOp, int) {
	if strings.TrimSpace(text) == "" {
		return nil, 0
	}
	spans := markdown.RenderSpans(m.theme, text, width, role, indent)
	ops := make([]drawOp, 0, len(spans))
	for i, line := range spans {
		dx := 0
		for _, span := range line {
			// Blank spans are skipped so the pre-filled canvas shows through
			// (drawing them would repaint the background), unless the span owns
			// a background of its own — a code block's fill is design, not
			// incidental padding.
			if strings.TrimSpace(span.Text) != "" || span.Style.GetBackground() != nil {
				ops = append(ops, drawOp{dx: dx, dy: dy0 + i, text: span.Text, style: span.Style})
			}
			dx += span.Width
		}
	}
	return ops, len(spans)
}

// composerStatus is the composer's status row. While a turn is in flight it
// reports live progress (spinner, elapsed, and the interrupt affordance) so the
// surface never looks dead between submit and first token. Reduced motion keeps
// a stable semantic fallback with no animation or ticking elapsed.
func (m Model) composerStatus() string {
	if !m.hasActiveTurn() {
		return string(m.state.Composer)
	}
	if m.reducedMotion {
		return "Working · esc interrupt"
	}
	frame := spinner.Dot.Frames[m.spinner%len(spinner.Dot.Frames)]
	label := frame + " Working"
	if elapsed := m.activeElapsed(); elapsed != "" {
		label += " · " + elapsed
	}
	return label + " · esc interrupt"
}

// hasActiveTurn reports whether a turn is streaming or otherwise in flight.
func (m Model) hasActiveTurn() bool {
	if m.state.Composer == ComposerRunning {
		return true
	}
	for _, b := range m.projection.Blocks {
		if b.Incomplete && (b.Kind == "text" || b.Kind == "reasoning" || b.Kind == "tool") {
			return true
		}
	}
	return false
}

// activeElapsed reports how long the in-flight turn has been running, from the
// oldest still-incomplete block of the newest run.
func (m Model) activeElapsed() string {
	start := time.Time{}
	for _, b := range m.projection.Blocks {
		if !b.Incomplete || b.At.IsZero() {
			continue
		}
		if start.IsZero() || b.At.Before(start) {
			start = b.At
		}
	}
	if start.IsZero() {
		return ""
	}
	secs := time.Since(start).Seconds()
	if secs < 0 || secs > 86400 {
		return ""
	}
	return fmt.Sprintf("%.1fs", secs)
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
