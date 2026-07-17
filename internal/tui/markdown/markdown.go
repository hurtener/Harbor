// Package markdown renders a lenient, streaming-stable subset of Markdown to
// styled terminal lines using Harbor's semantic theme tokens.
//
// The renderer is deliberately dependency-free and deferred: an inline span is
// only styled once its closing delimiter is present in the source, so rendering
// a progressively growing document never retro-changes an already-completed
// block. That keeps streamed assistant output flicker-free — the rendered
// prefix of a document is stable as more text arrives.
//
// Two output shapes are offered over one shared layout pass, so they cannot
// drift:
//
//   - RenderSpans returns plain text plus the style it renders under, for a
//     cell-grid canvas that applies styling itself. This is the preferred API.
//   - Render returns pre-styled, right-filled strings for callers that write
//     escape sequences straight to a terminal.
//
// Both wrap on grapheme boundaries and route every width calculation through
// the ui package, so east-asian text and emoji stay within budget.
package markdown

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/hurtener/Harbor/internal/tui/ui"
)

// Span is a styled run of plain text within a rendered line.
type Span struct {
	// Text is plain text and never contains escape sequences.
	Text string
	// Style is the span's style, resolved from the theme.
	Style lipgloss.Style
	// Width is the span's visible cell count, equal to ui.Width(Text).
	Width int
}

// RenderSpans renders markdown to wrapped lines of styled spans. Line i is
// composed by drawing each span left-to-right, advancing x by Span.Width.
// Spans include the leading indent as a plain span of spaces, so every line can
// be placed at the same x origin. baseRole is the default text role (RoleText
// for answers, RoleMuted for reasoning).
//
// Span text is always plain, so it is safe to hand to a cell-grid canvas that
// re-splits text on grapheme clusters. The summed Span.Width of a line never
// exceeds width, and lines are not right-filled with trailing padding — a blank
// block separator is returned as a line with no spans. Code blocks and rules do
// paint their full content width, because their background is part of the
// design rather than incidental padding.
func RenderSpans(theme ui.Theme, src string, width int, baseRole ui.Role, indent int) [][]Span {
	lines := renderLines(theme, src, width, baseRole, indent)
	if lines == nil {
		return nil
	}
	out := make([][]Span, 0, len(lines))
	for _, l := range lines {
		out = append(out, coalesce(l))
	}
	return out
}

// Render renders markdown source to a slice of already-styled terminal lines,
// each fitting within width visible cells. baseRole is the default text role
// (RoleText for answers, RoleMuted for reasoning). indent is a left pad in
// cells applied to every line, continuation lines included.
//
// Unlike RenderSpans, every returned line is right-filled to exactly width
// visible cells and carries ANSI escapes. Callers writing into a cell-grid
// canvas want RenderSpans instead: styled strings cannot be re-split into
// grapheme clusters safely.
//
// The parse is lenient: an unmatched inline delimiter or an unclosed fenced
// code block renders as literal text until its closer arrives, guaranteeing
// that earlier completed blocks never change as src grows.
func Render(theme ui.Theme, src string, width int, baseRole ui.Role, indent int) []string {
	lines := renderLines(theme, src, width, baseRole, indent)
	if lines == nil {
		return nil
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		styled, vis := renderCells(l)
		if vis < width {
			styled += strings.Repeat(" ", width-vis)
		}
		out = append(out, styled)
	}
	return out
}

// renderLines is the single layout pass behind both public renderers. It
// returns one cell slice per output line, indent included, with a nil slice
// standing in for the blank separator between blocks.
func renderLines(theme ui.Theme, src string, width int, baseRole ui.Role, indent int) [][]cell {
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

	var out [][]cell
	for _, b := range parseBlocks(src) {
		if len(out) > 0 {
			out = append(out, nil) // blank separator between blocks
		}
		out = append(out, withIndent(indent, b.lines(theme, contentWidth, baseRole))...)
	}
	return out
}

// withIndent prepends a plain pad span of indent spaces to every line.
func withIndent(indent int, lines [][]cell) [][]cell {
	if indent <= 0 {
		return lines
	}
	pad := cell{text: strings.Repeat(" ", indent), width: indent, key: keySpace, style: lipgloss.NewStyle()}
	out := make([][]cell, len(lines))
	for i, l := range lines {
		row := make([]cell, 0, len(l)+1)
		row = append(row, pad)
		out[i] = append(row, l...)
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

// lines lays one block out into per-line cell slices, without the indent.
func (b block) lines(theme ui.Theme, contentWidth int, base ui.Role) [][]cell {
	switch b.kind {
	case kindHeading:
		return wrapWords(splitWords(layoutInline(theme, parseInline(b.text), ui.RolePrimary, true)), contentWidth)
	case kindCode:
		return codeLines(theme, b.code, contentWidth)
	case kindList:
		return listLines(theme, b.items, contentWidth, base)
	case kindQuote:
		return quoteLines(theme, b.text, contentWidth)
	case kindHR:
		rule := cell{
			text:  strings.Repeat("─", contentWidth),
			width: contentWidth,
			key:   keyRule,
			style: theme.Style(ui.RoleMuted, nil),
		}
		return [][]cell{{rule}}
	default:
		return wrapWords(splitWords(layoutInline(theme, parseInline(b.text), base, false)), contentWidth)
	}
}

// listLines lays out list items with a hanging continuation indent aligned
// under the item text.
func listLines(theme ui.Theme, items []listItem, contentWidth int, base ui.Role) [][]cell {
	var out [][]cell
	for _, it := range items {
		mw := ui.Width(it.marker)
		avail := max(1, contentWidth-mw)
		wrapped := wrapWords(splitWords(layoutInline(theme, parseInline(it.text), base, false)), avail)
		marker := cell{text: it.marker, width: mw, key: keyMarker, style: theme.Style(ui.RoleAccent, nil)}
		hang := cell{text: strings.Repeat(" ", mw), width: mw, key: keySpace, style: lipgloss.NewStyle()}
		for idx, wl := range wrapped {
			prefix := marker
			if idx > 0 {
				prefix = hang
			}
			out = append(out, append([]cell{prefix}, wl...))
		}
	}
	return out
}

// quoteLines lays out blockquote text behind a muted left bar.
func quoteLines(theme ui.Theme, text string, contentWidth int) [][]cell {
	const bar = "▏ "
	bw := ui.Width(bar)
	avail := max(1, contentWidth-bw)
	barCell := cell{text: bar, width: bw, key: keyQuoteBar, style: theme.Style(ui.RoleMuted, nil)}
	wrapped := wrapWords(splitWords(layoutInline(theme, parseInline(text), ui.RoleMuted, false)), avail)
	out := make([][]cell, 0, len(wrapped))
	for _, wl := range wrapped {
		out = append(out, append([]cell{barCell}, wl...))
	}
	return out
}

// codeLines lays out verbatim code on an element background behind a muted
// gutter. Code is never wrapped; over-wide lines are truncated with an
// ellipsis. The background deliberately fills the full content width.
func codeLines(theme ui.Theme, code []string, contentWidth int) [][]cell {
	bg := ui.RoleElement
	style := theme.Style(ui.RoleMuted, &bg)
	const gutter = "▏ "
	gw := ui.Width(gutter)
	avail := contentWidth - gw
	gutterCell := cell{text: gutter, width: gw, key: keyCode, style: style}
	withGutter := true
	if avail < 1 {
		withGutter = false
		avail = contentWidth
	}
	out := make([][]cell, 0, len(code))
	for _, raw := range code {
		text := strings.ReplaceAll(raw, "\t", "    ")
		if ui.Width(text) > avail {
			text = ui.Truncate(text, max(0, avail-1)) + "…"
		}
		if vis := ui.Width(text); vis < avail {
			text += strings.Repeat(" ", avail-vis)
		}
		body := cell{text: text, width: avail, key: keyCode, style: style}
		if withGutter {
			out = append(out, []cell{gutterCell, body})
			continue
		}
		out = append(out, []cell{body})
	}
	return out
}

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
	for i := range len(s) {
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
