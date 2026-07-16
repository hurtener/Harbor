package markdown

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// splitWords groups cells into words, dropping the whitespace that separates
// them. Whitespace inside an inline code span is preserved so the code run
// stays a single unbreakable, contiguously-backgrounded word.
func splitWords(cells []cell) [][]cell {
	var words [][]cell
	var cur []cell
	for _, c := range cells {
		if c.text == " " && c.key != "code" {
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
			cur = append(cur, spaceCell())
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

// renderCells styles a line's cells, coalescing runs of identical style into a
// single lipgloss render, and returns the string plus its visible width.
func renderCells(cells []cell) (string, int) {
	var b strings.Builder
	vis := 0
	for i := 0; i < len(cells); {
		j := i + 1
		for j < len(cells) && cells[j].key == cells[i].key {
			j++
		}
		var seg strings.Builder
		for k := i; k < j; k++ {
			seg.WriteString(cells[k].text)
			vis += cells[k].width
		}
		b.WriteString(cells[i].style.Render(seg.String()))
		i = j
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

// spaceCell is the unstyled separator inserted between wrapped words.
func spaceCell() cell {
	return cell{text: " ", width: 1, key: "sp", style: lipgloss.NewStyle()}
}
