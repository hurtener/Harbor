package markdown

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Style merge keys for cells whose style is not derived from an inline run.
// Cells sharing a key are coalesced into a single span.
const (
	keySpace    = "sp"
	keyCode     = "code"
	keyMarker   = "marker"
	keyQuoteBar = "quotebar"
	keyRule     = "rule"
)

// splitWords groups cells into words, dropping the whitespace that separates
// them. Whitespace inside an inline code span is preserved so the code run
// stays a single unbreakable, contiguously-backgrounded word.
func splitWords(cells []cell) [][]cell {
	var words [][]cell
	var cur []cell
	for _, c := range cells {
		if c.text == " " && c.key != keyCode {
			if len(cur) > 0 {
				words = append(words, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, c)
	}
	if len(cur) > 0 {
		words = append(words, cur)
	}
	return words
}

// wrapWords greedily packs words into lines no wider than width visible cells,
// inserting a single space between words. A word wider than the whole line is
// hard-split on grapheme boundaries.
func wrapWords(words [][]cell, width int) [][]cell {
	if width < 1 {
		width = 1
	}
	var lines [][]cell
	var cur []cell
	curW := 0
	for _, w := range words {
		ww := clustersWidth(w)
		if ww > width {
			if len(cur) > 0 {
				lines = append(lines, cur)
				cur, curW = nil, 0
			}
			chunks := hardSplit(w, width)
			for k := 0; k < len(chunks)-1; k++ {
				lines = append(lines, chunks[k])
			}
			cur = append([]cell(nil), chunks[len(chunks)-1]...)
			curW = clustersWidth(cur)
			continue
		}
		need := ww
		if len(cur) > 0 {
			need++ // separating space
		}
		if curW+need > width {
			lines = append(lines, cur)
			cur = append([]cell(nil), w...)
			curW = ww
			continue
		}
		if len(cur) > 0 {
			cur = append(cur, separatorCell(cur[len(cur)-1]))
			curW++
		}
		cur = append(cur, w...)
		curW += ww
	}
	if len(cur) > 0 || len(lines) == 0 {
		lines = append(lines, cur)
	}
	return lines
}

// hardSplit breaks an over-wide word into chunks each within width.
func hardSplit(w []cell, width int) [][]cell {
	var chunks [][]cell
	var cur []cell
	curW := 0
	for _, c := range w {
		if curW+c.width > width && len(cur) > 0 {
			chunks = append(chunks, cur)
			cur, curW = nil, 0
		}
		cur = append(cur, c)
		curW += c.width
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// coalesce merges consecutive cells sharing a style key into spans of plain
// text. It is the one place cells become output, so the span renderer and the
// string renderer cannot disagree about styling or width.
func coalesce(cells []cell) []Span {
	var spans []Span
	for i := 0; i < len(cells); {
		j := i + 1
		for j < len(cells) && cells[j].key == cells[i].key {
			j++
		}
		var b strings.Builder
		w := 0
		for k := i; k < j; k++ {
			b.WriteString(cells[k].text)
			w += cells[k].width
		}
		spans = append(spans, Span{Text: b.String(), Style: cells[i].style, Width: w})
		i = j
	}
	return spans
}

// renderCells styles a line's cells into one escape-carrying string and returns
// it together with its visible width.
func renderCells(cells []cell) (string, int) {
	var b strings.Builder
	vis := 0
	for _, s := range coalesce(cells) {
		b.WriteString(s.Style.Render(s.Text))
		vis += s.Width
	}
	return b.String(), vis
}

// clustersWidth sums the visible width of a cell slice.
func clustersWidth(cells []cell) int {
	w := 0
	for _, c := range cells {
		w += c.width
	}
	return w
}

// separatorCell is the space inserted between two wrapped words. It adopts the
// preceding cell's style so the gap coalesces into the neighbouring span
// instead of fragmenting the line — a foreground-only style is invisible on
// whitespace. A code span is the exception: it carries a background, which must
// not bleed across the gap, so the separator falls back to a plain space.
func separatorCell(prev cell) cell {
	if prev.key == keyCode || prev.key == "" {
		return spaceCell()
	}
	return cell{text: " ", width: 1, key: prev.key, style: prev.style}
}

// spaceCell is the unstyled separator used where no style may be inherited.
func spaceCell() cell {
	return cell{text: " ", width: 1, key: keySpace, style: lipgloss.NewStyle()}
}
