package app

import (
	"fmt"
	"strings"

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
		return laidBlock{height: h + 1, ops: ops}
	}
	// The per-turn anchor sits under the finished answer: which model ran and
	// how long the Runtime says it took. It replaces the raw lifecycle/audit
	// lines that used to interleave with the conversation.
	if status := m.turnStatusFor(b); status != "" {
		ops = append(ops,
			drawOp{dx: proseIndent, dy: h, text: "▣", style: m.theme.Style(ui.RoleAccent, nil)},
			drawOp{dx: proseIndent + 2, dy: h, text: ui.Truncate(status, max(1, width-proseIndent-2)), style: m.theme.Style(ui.RoleMuted, nil)},
		)
		h++
	}
	return laidBlock{height: h, ops: ops}
}

// turnStatusFor returns the turn anchor for the newest answer only, so a long
// transcript is not littered with one status line per historical block.
func (m Model) turnStatusFor(b projection.Block) string {
	if m.state.TurnStatus == "" {
		return ""
	}
	for i := len(m.projection.Blocks) - 1; i >= 0; i-- {
		if k := m.projection.Blocks[i].Kind; k != "text" {
			continue
		}
		if m.projection.Blocks[i].ID == b.ID {
			return m.state.TurnStatus
		}
		return ""
	}
	return ""
}

// layoutReasoning renders thinking as a muted, subordinate section, collapsed by
// default: the header alone says the agent reasoned, and the body opens on
// demand (ctrl+x r). The host blanks the body of a collapsed block, so an empty
// body here means "collapsed", not "no reasoning".
func (m Model) layoutReasoning(b projection.Block, width int) laidBlock {
	body, bh := m.proseOps(b.Text, width, reasoningIndent, 1, ui.RoleMuted)
	header, glyph := "Thought", "▸"
	if b.Incomplete {
		header, glyph = "Thinking", "⋯"
	}
	if bh > 0 {
		glyph = "▾"
	}
	ops := []drawOp{{dx: 0, dy: 0, text: glyph + "  " + header, style: m.theme.Style(ui.RoleWarning, nil).Faint(true)}}
	return laidBlock{height: 1 + bh, ops: append(ops, body...)}
}

// layoutTool renders a tool call collapsed by default: one compact line naming
// the tool and its outcome, with the result body opening on demand (ctrl+x o).
// The host blanks the body of a collapsed block.
func (m Model) layoutTool(b projection.Block, width int) laidBlock {
	name := b.Tool
	if name == "" {
		name = "tool"
	}
	glyph, role := lifecycleGlyph(b.Status)
	summary := strings.TrimSpace(b.Status)
	if summary == "" {
		summary = "called"
	}
	ops := []drawOp{{dx: proseIndent, dy: 0, text: ui.Truncate(fmt.Sprintf("%s  %s  %s", glyph, name, summary), width-proseIndent), style: m.theme.Style(role, nil)}}
	body, bh := m.proseOps(b.Text, width, reasoningIndent, 1, ui.RoleMuted)
	return laidBlock{height: 1 + bh, ops: append(ops, body...)}
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

// layoutInterventionBlock renders a pending intervention as a warning callout —
// the one non-user block that stays boxed, because it blocks the run and must
// not read as ordinary output. Its actions obey the responsive contract (§4):
// stacked below 80 columns, horizontal at 80 and above.
func (m Model) layoutInterventionBlock(b projection.Block, width int) laidBlock {
	warn := ui.RoleWarning
	panel := ui.RolePanel
	detail := b.Text
	if detail == "" {
		detail = "Operator approval required."
	}
	lines := append([]string{"!  Operator approval required"}, wrapPlain(detail, width-proseIndent-1)...)
	bar := m.theme.Style(warn, &panel)
	body := m.theme.Style(ui.RoleWarning, &panel)
	ops := make([]drawOp, 0, len(lines)*3+2)
	for i, line := range lines {
		ops = append(ops,
			drawOp{dy: i, fillBG: &panel},
			drawOp{dx: 0, dy: i, text: "┃", style: bar},
			drawOp{dx: proseIndent, dy: i, text: line, style: body},
		)
	}
	return laidBlock{height: len(lines), ops: ops}
}

// interventionActions returns the approval affordances as chrome rows, obeying
// the responsive contract (§4): stacked below 80 columns, horizontal at 80 and
// above. They live beside the status strip rather than inside the block, so the
// block says what needs approval and the chrome says what you can do.
func (m Model) interventionActions() []string {
	if m.Layout().HorizontalActions {
		return []string{"[ Approve unavailable ]   [ Reject unavailable ]"}
	}
	return []string{"[ Approve unavailable ]", "[ Reject unavailable ]"}
}

// layoutEvent renders a canonical event as one quiet dim line. Unknown and
// metadata-only fallback events stay visible (§6 requires every unknown event
// render through a safe fallback) but cost a single line rather than a card, so
// they never dominate the canvas. Only a wholly empty block renders nothing.
func (m Model) layoutEvent(b projection.Block, width int) laidBlock {
	text := strings.TrimSpace(b.Text)
	if text == "" {
		text = strings.TrimSpace(b.EventType)
	}
	if text == "" {
		return laidBlock{}
	}
	line := ui.Truncate("·  "+text, width-proseIndent)
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
// reports progress and the interrupt affordance so the surface never looks dead
// between submit and first token. It deliberately carries NO elapsed time: the
// only honest duration is the Runtime's own (rendered on the turn-status line
// once the run is terminal), and a local clock would count the operator's
// typing. Reduced motion keeps a stable fallback with no animation.
func (m Model) composerStatus() string {
	if m.hasActiveTurn() {
		label := "◆ Working"
		if !m.reducedMotion {
			label = spinner.Dot.Frames[m.spinner%len(spinner.Dot.Frames)] + " Working"
		}
		return label + "  ·  esc interrupt"
	}
	// Idle: report what an operator actually needs — the model and how much of
	// its context this session has consumed — not the internal composer enum.
	// Actionable postures still speak for themselves; they are set dynamically
	// ("attachment · report.pdf"), so match by prefix, not constant.
	posture := string(m.state.Composer)
	for _, actionable := range []string{"disabled", "retry", "attachment"} {
		if strings.HasPrefix(posture, actionable) {
			return posture
		}
	}
	parts := make([]string, 0, 2)
	if m.state.Model != "" {
		parts = append(parts, m.state.Model)
	}
	if usage := m.contextLabel(); usage != "" {
		parts = append(parts, usage)
	}
	return strings.Join(parts, "  ·  ")
}

// contextLabel reports canonical context consumption, e.g.
// "Context 17.4k/128k (14%)". It reports nothing when the Runtime has not
// advertised a window — a denominator is never invented.
func (m Model) contextLabel() string {
	u := m.projection.Usage
	if u.TotalTokens <= 0 {
		return ""
	}
	if u.ContextWindow <= 0 {
		return fmt.Sprintf("Context %s", compactTokens(u.TotalTokens))
	}
	percent := float64(u.PromptTokens) / float64(u.ContextWindow) * 100
	return fmt.Sprintf("Context %s/%s (%.0f%%)", compactTokens(u.PromptTokens), compactTokens(u.ContextWindow), percent)
}

// compactTokens renders a token count the way an operator reads it: 17.4k.
func compactTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// hasActiveTurn reports whether a turn is genuinely in flight. It keys ONLY on
// the composer posture, which the host drives from the canonical run lifecycle.
// It deliberately ignores state.Active — that means "the stream is live", which
// is the normal idle condition, and folding it in here reported "Working"
// permanently on a connected session.
func (m Model) hasActiveTurn() bool {
	return m.state.Composer == ComposerRunning
}

func (m Model) dim(text string, role ui.Role) string {
	return m.theme.Style(role, nil).Faint(true).Render(text)
}

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

// lifecycleGlyph maps a status to a semantic glyph + role. The glyph vocabulary
// is the contract that carries state meaning without colour (§8 monochrome), so
// it stays stable: ✓ success, × failure, ! attention, ◆ in-flight.
func lifecycleGlyph(status string) (string, ui.Role) {
	switch status {
	case "completed", "succeeded":
		return "✓", ui.RoleSuccess
	case "failed", "erased":
		return "×", ui.RoleError
	case "pending", "paused":
		return "!", ui.RoleWarning
	case "running", "started":
		return "◆", ui.RoleInfo
	default:
		return "·", ui.RoleMuted
	}
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
