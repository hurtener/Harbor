// Package markdown renders a lenient, streaming-stable subset of Markdown to
// styled terminal lines using Harbor's semantic theme tokens.
//
// The renderer is deliberately dependency-free and deferred: an inline span is
// only styled once its closing delimiter is present in the source, so rendering
// a progressively growing document never retro-changes an already-completed
// block. That keeps streamed assistant output flicker-free — the rendered
// prefix of a document is stable as more text arrives.
//
// Every returned line is pre-styled (carries the theme's ANSI escapes), padded,
// and wrapped so it occupies exactly the requested visible width. A caller
// places each line directly onto a canvas without further measurement.
package markdown

import (
	"strings"

	"github.com/hurtener/Harbor/internal/tui/ui"
)

// Render renders markdown source to a slice of already-styled terminal lines,
// each fitting within width visible cells. baseRole is the default text role
// (RoleText for answers, RoleMuted for reasoning). indent is a left pad in
// cells applied to every line, continuation lines included.
//
// The parse is lenient: an unmatched inline delimiter or an unclosed fenced
// code block renders as literal text until its closer arrives, guaranteeing
// that earlier completed blocks never change as src grows.
func Render(theme ui.Theme, src string, width int, baseRole ui.Role, indent int) []string {
	if width <= 0 {
		return nil
	}
	if indent < 0 {
		indent = 0
	}
	if indent > width-1 {
		indent = width - 1
	}
	contentWidth := width - indent

	blocks := parseBlocks(src)
	var out []string
	for _, b := range blocks {
		if len(out) > 0 {
			out = append(out, blankLine(width))
		}
		out = append(out, b.render(theme, contentWidth, baseRole, indent)...)
	}
	return out
}

// blockKind enumerates the supported block-level constructs.
type blockKind int

const (
	kindPara blockKind = iota
	kindHeading
	kindCode
	kindList
	kindQuote
	kindHR
)

// listItem is one rendered list entry with its pre-computed marker.
type listItem struct {
	marker string
	text   string
}

// block is one parsed block-level element.
type block struct {
	kind  blockKind
	text  string
	level int
	code  []string
	items []listItem
}

// parseBlocks splits source into block-level elements. It never fails: any line
// it cannot classify falls through to a paragraph, and an unclosed fence is
// treated as literal paragraph text so a growing document stays stable.
func parseBlocks(src string) []block {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Split(src, "\n")
	var blocks []block
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			i++
		case isFence(trimmed):
			marker := trimmed[:3]
			closeAt := -1
			for j := i + 1; j < len(lines); j++ {
				if strings.HasPrefix(strings.TrimSpace(lines[j]), marker) {
					closeAt = j
					break
				}
			}
			if closeAt < 0 {
				// Unclosed fence: render as literal text until it closes.
				b, ni := parseParagraph(lines, i)
				blocks = append(blocks, b)
				i = ni
				continue
			}
			blocks = append(blocks, block{kind: kindCode, code: lines[i+1 : closeAt]})
			i = closeAt + 1
		case isHR(trimmed):
			blocks = append(blocks, block{kind: kindHR})
			i++
		case headingLevel(trimmed) > 0:
			lvl := headingLevel(trimmed)
			blocks = append(blocks, block{kind: kindHeading, level: lvl, text: strings.TrimSpace(trimmed[lvl:])})
			i++
		case isQuote(trimmed):
			var parts []string
			for i < len(lines) && isQuote(strings.TrimSpace(lines[i])) {
				q := strings.TrimPrefix(strings.TrimSpace(lines[i]), ">")
				parts = append(parts, strings.TrimSpace(q))
				i++
			}
			blocks = append(blocks, block{kind: kindQuote, text: strings.Join(parts, " ")})
		case isListItem(line):
			var items []listItem
			for i < len(lines) {
				if strings.TrimSpace(lines[i]) == "" {
					break
				}
				if m, txt, ok := listMarker(lines[i]); ok {
					items = append(items, listItem{marker: m, text: txt})
				} else if len(items) > 0 {
					items[len(items)-1].text += " " + strings.TrimSpace(lines[i])
				} else {
					break
				}
				i++
			}
			blocks = append(blocks, block{kind: kindList, items: items})
		default:
			b, ni := parseParagraph(lines, i)
			blocks = append(blocks, b)
			i = ni
		}
	}
	return blocks
}

// parseParagraph consumes one paragraph starting at line i: the first line
// unconditionally, then following lines until a blank line or a new block
// construct. Lines are joined with a single space for reflow.
func parseParagraph(lines []string, i int) (block, int) {
	parts := []string{strings.TrimSpace(lines[i])}
	i++
	for i < len(lines) {
		t := strings.TrimSpace(lines[i])
		if t == "" || isFence(t) || isHR(t) || headingLevel(t) > 0 || isQuote(t) || isListItem(lines[i]) {
			break
		}
		parts = append(parts, t)
		i++
	}
	return block{kind: kindPara, text: strings.Join(parts, " ")}, i
}

// render turns one block into finished, indented, width-filled lines.
func (b block) render(theme ui.Theme, contentWidth int, base ui.Role, indent int) []string {
	switch b.kind {
	case kindHeading:
		cells := layoutInline(theme, parseInline(b.text), ui.RolePrimary, true)
		return renderCellLines(cells, contentWidth, indent)
	case kindCode:
		return renderCode(theme, b.code, contentWidth, indent)
	case kindList:
		return renderList(theme, b.items, contentWidth, base, indent)
	case kindQuote:
		return renderQuote(theme, b.text, contentWidth, indent)
	case kindHR:
		styled := theme.Style(ui.RoleMuted, nil).Render(strings.Repeat("─", contentWidth))
		return []string{lineOut(indent, contentWidth, styled, contentWidth)}
	default:
		cells := layoutInline(theme, parseInline(b.text), base, false)
		return renderCellLines(cells, contentWidth, indent)
	}
}

// renderCellLines wraps inline cells to contentWidth and emits finished lines.
func renderCellLines(cells []cell, contentWidth, indent int) []string {
	wrapped := wrapWords(splitWords(cells), contentWidth)
	out := make([]string, 0, len(wrapped))
	for _, wl := range wrapped {
		styled, vis := renderCells(wl)
		out = append(out, lineOut(indent, contentWidth, styled, vis))
	}
	return out
}

// renderList emits one or more lines per item with a hanging continuation
// indent aligned under the item text.
func renderList(theme ui.Theme, items []listItem, contentWidth int, base ui.Role, indent int) []string {
	var out []string
	for _, it := range items {
		mw := ui.Width(it.marker)
		avail := contentWidth - mw
		if avail < 1 {
			avail = 1
		}
		wrapped := wrapWords(splitWords(layoutInline(theme, parseInline(it.text), base, false)), avail)
		if len(wrapped) == 0 {
			wrapped = [][]cell{nil}
		}
		marker := theme.Style(ui.RoleAccent, nil).Render(it.marker)
		for idx, wl := range wrapped {
			styled, vis := renderCells(wl)
			prefix := marker
			if idx > 0 {
				prefix = strings.Repeat(" ", mw)
			}
			out = append(out, lineOut(indent, contentWidth, prefix+styled, mw+vis))
		}
	}
	return out
}

// renderQuote emits blockquote lines behind a muted left bar.
func renderQuote(theme ui.Theme, text string, contentWidth, indent int) []string {
	const bar = "▏ "
	bw := ui.Width(bar)
	avail := contentWidth - bw
	if avail < 1 {
		avail = 1
	}
	barStyled := theme.Style(ui.RoleMuted, nil).Render(bar)
	wrapped := wrapWords(splitWords(layoutInline(theme, parseInline(text), ui.RoleMuted, false)), avail)
	if len(wrapped) == 0 {
		wrapped = [][]cell{nil}
	}
	out := make([]string, 0, len(wrapped))
	for _, wl := range wrapped {
		styled, vis := renderCells(wl)
		out = append(out, lineOut(indent, contentWidth, barStyled+styled, bw+vis))
	}
	return out
}

// renderCode emits verbatim code lines on an element background behind a muted
// gutter. Code is never wrapped; over-wide lines are truncated with an ellipsis.
func renderCode(theme ui.Theme, codeLines []string, contentWidth, indent int) []string {
	bg := ui.RoleElement
	const gutter = "▏ "
	gw := ui.Width(gutter)
	avail := contentWidth - gw
	gutterStyled := theme.Style(ui.RoleMuted, &bg).Render(gutter)
	if avail < 1 {
		gutterStyled = ""
		gw = 0
		avail = contentWidth
	}
	out := make([]string, 0, len(codeLines))
	for _, raw := range codeLines {
		code := strings.ReplaceAll(raw, "\t", "    ")
		if ui.Width(code) > avail {
			code = ui.Truncate(code, max(0, avail-1)) + "…"
		}
		if vis := ui.Width(code); vis < avail {
			code += strings.Repeat(" ", avail-vis)
		}
		styled := theme.Style(ui.RoleMuted, &bg).Render(code)
		out = append(out, lineOut(indent, contentWidth, gutterStyled+styled, gw+avail))
	}
	return out
}

// lineOut left-pads with indent spaces and right-fills styled content so the
// finished line occupies exactly indent+contentWidth visible cells.
func lineOut(indent, contentWidth int, styled string, vis int) string {
	if vis < contentWidth {
		styled += strings.Repeat(" ", contentWidth-vis)
	}
	if indent > 0 {
		return strings.Repeat(" ", indent) + styled
	}
	return styled
}

// blankLine is a full-width spacer separating blocks.
func blankLine(width int) string { return strings.Repeat(" ", width) }

// isFence reports whether a trimmed line opens or closes a fenced code block.
func isFence(t string) bool {
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// headingLevel returns the ATX heading level (1..6) or 0 if t is not a heading.
func headingLevel(t string) int {
	n := 0
	for n < len(t) && n < 6 && t[n] == '#' {
		n++
	}
	if n == 0 {
		return 0
	}
	if n == len(t) || t[n] == ' ' {
		return n
	}
	return 0
}

// isHR reports whether a trimmed line is a horizontal rule (3+ of -, *, or _).
func isHR(t string) bool {
	s := strings.ReplaceAll(t, " ", "")
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != c {
			return false
		}
	}
	return true
}

// isQuote reports whether a trimmed line is a blockquote line.
func isQuote(t string) bool { return strings.HasPrefix(t, ">") }

// listMarker classifies a list line, returning the rendered marker, the item
// text, and whether the line is a list item.
func listMarker(line string) (marker, text string, ok bool) {
	t := strings.TrimLeft(line, " ")
	if len(t) >= 2 && (t[0] == '-' || t[0] == '*' || t[0] == '+') && t[1] == ' ' {
		return "• ", strings.TrimSpace(t[2:]), true
	}
	n := 0
	for n < len(t) && t[n] >= '0' && t[n] <= '9' {
		n++
	}
	if n > 0 && n+1 < len(t) && (t[n] == '.' || t[n] == ')') && t[n+1] == ' ' {
		return t[:n] + ". ", strings.TrimSpace(t[n+2:]), true
	}
	return "", "", false
}

// isListItem reports whether a line begins a list item.
func isListItem(line string) bool {
	_, _, ok := listMarker(line)
	return ok
}
