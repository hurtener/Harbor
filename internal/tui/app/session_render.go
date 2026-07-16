package app

import (
	"strings"

	"github.com/hurtener/Harbor/internal/tui/ui"
)

// The session surface owns the whole alternate screen with a terminal-native
// agent layout: a persistent banner at the top, the conversation flowing
// top-down beneath it, and the composer pinned at the bottom. Content taller
// than the window scrolls line by line; the view follows the tail until the
// operator scrolls away, and never yanks them back.

// bannerHeight is the fixed rows the session banner occupies: a leading blank,
// two content rows, and a trailing blank.
const bannerHeight = 4

// renderBanner draws the persistent session header: the mark, the product
// name, and one muted line of operational identity. Values the Runtime has not
// reported yet are simply absent — never invented.
func (m Model) renderBanner(c *canvas, width int) {
	accent := m.theme.Style(ui.RoleAccent, nil)
	name := m.theme.Style(ui.RolePrimary, nil).Bold(true)
	muted := m.theme.Style(ui.RoleMuted, nil)

	title := "Harbor"
	if m.state.DisplayName != "" && !strings.EqualFold(m.state.DisplayName, title) {
		title += "  ·  " + m.state.DisplayName
	}
	c.put(ui.OuterPadding, 1, "▛▀▖", accent)
	c.put(ui.OuterPadding, 2, "▙▄▘", accent)
	c.put(ui.OuterPadding+5, 1, ui.Truncate(title, max(1, width-16)), name)
	if m.state.Version != "" {
		c.put(ui.OuterPadding+5+ui.Width(title)+2, 1, ui.Truncate(m.state.Version, 12), muted)
	}
	meta := make([]string, 0, 3)
	if m.state.Model != "" {
		meta = append(meta, m.state.Model)
	}
	meta = append(meta, "session "+m.projection.Identity.Session)
	if m.state.BaseURL != "" {
		meta = append(meta, m.state.BaseURL)
	}
	c.put(ui.OuterPadding+5, 2, ui.Truncate(strings.Join(meta, " · "), max(1, width-5)), muted)
}

// layoutTranscript lays out every visible block at the given width, returning
// the laid blocks, each block's starting line offset, and the total line
// height including inter-block gaps.
func (m Model) layoutTranscript(width int) ([]laidBlock, []int, int) {
	blocks := m.projection.Blocks
	laid := make([]laidBlock, 0, len(blocks))
	offsets := make([]int, 0, len(blocks))
	y := 0
	for _, b := range blocks {
		lb := m.layoutBlock(b, width, b.ID != "" && b.ID == m.state.SelectedBlockID)
		if lb.height == 0 {
			continue
		}
		if y > 0 {
			y += blockGap
		}
		laid = append(laid, lb)
		offsets = append(offsets, y)
		y += lb.height
	}
	return laid, offsets, y
}

// blockLineOffset reports the starting transcript line of a block, measuring at
// the session surface's current content width. Used by semantic navigation
// (block jump, search) to scroll the window to a target.
func (m Model) blockLineOffset(id string) (int, bool) {
	_, width := m.transcriptRegionHeight()
	blocks := m.projection.Blocks
	y := 0
	placed := false
	for _, b := range blocks {
		lb := m.layoutBlock(b, width, false)
		if lb.height == 0 {
			continue
		}
		if placed {
			y += blockGap
		}
		if b.ID == id {
			return y, true
		}
		placed = true
		y += lb.height
	}
	return 0, false
}

// transcriptRegionHeight computes the transcript window's height and content
// width exactly as renderBase lays the session surface out, so scroll math and
// rendering can never disagree.
func (m Model) transcriptRegionHeight() (int, int) {
	l := m.Layout()
	width := max(12, l.MainWidth-1)
	top, bottom := m.transcriptRegion()
	_ = l
	return max(0, bottom-top+1), width
}

// transcriptRegion returns the inclusive [top, bottom] rows of the transcript
// window on the session surface: below the banner, above the composer and its
// chrome (toast, status strip, intervention actions, one separator row).
func (m Model) transcriptRegion() (int, int) {
	bottom := m.composerTopRow() - 2
	if m.state.ToastOpen && m.state.Toast != "" {
		bottom--
	}
	if _, _, ok := m.statusStrip(); ok {
		bottom--
	}
	if m.state.Intervention {
		bottom -= len(m.interventionActions())
	}
	return bannerHeight, bottom
}

// renderTranscriptWindow renders the [scroll, scroll+viewH) line window of the
// laid-out transcript into the region. Following pins the window to the tail;
// a scrolled-away reader keeps their fixed line as content grows below.
func (m Model) renderTranscriptWindow(c *canvas, top, bottom, width int) {
	if bottom < top {
		return
	}
	laid, offsets, total := m.layoutTranscript(width)
	if len(laid) == 0 {
		return
	}
	viewH := bottom - top + 1
	maxScroll := max(0, total-viewH)
	scroll := m.scrollLine
	if m.followTail || scroll > maxScroll {
		scroll = maxScroll
	}
	window := newCanvas(width, viewH)
	for i, lb := range laid {
		y := offsets[i] - scroll
		if y+lb.height <= 0 || y >= viewH {
			continue
		}
		// canvas.put clips rows outside the sub-canvas, so partially visible
		// blocks render exactly their visible lines.
		m.placeBlock(&window, 0, y, width, lb)
	}
	c.blit(window, ui.OuterPadding, top)
	if scroll < maxScroll {
		hint := "↓ more below"
		c.put(ui.OuterPadding+max(0, width-ui.Width(hint)), bottom, hint, m.theme.Style(ui.RoleMuted, nil).Faint(true))
	}
}

// scrollTranscript moves the window by deltaLines, clamped to content. Any
// upward movement releases tail-following; landing on the tail re-engages it.
func (m *Model) scrollTranscript(deltaLines int) {
	viewH, width := m.transcriptRegionHeight()
	_, _, total := m.layoutTranscript(width)
	maxScroll := max(0, total-viewH)
	current := m.scrollLine
	if m.followTail {
		current = maxScroll
	}
	m.scrollLine = max(0, min(maxScroll, current+deltaLines))
	m.followTail = m.scrollLine >= maxScroll
}

// scrollTranscriptTo jumps to an absolute line (negative = tail).
func (m *Model) scrollTranscriptTo(line int) {
	viewH, width := m.transcriptRegionHeight()
	_, _, total := m.layoutTranscript(width)
	maxScroll := max(0, total-viewH)
	if line < 0 {
		m.scrollLine = maxScroll
		m.followTail = true
		return
	}
	m.scrollLine = max(0, min(maxScroll, line))
	m.followTail = m.scrollLine >= maxScroll
}

// transcriptPage is the page size for PageUp/PageDown.
func (m Model) transcriptPage() int {
	viewH, _ := m.transcriptRegionHeight()
	return max(1, viewH-2)
}

// ensureBlockVisible scrolls the window so the block's first line is on
// screen, preferring to place it at the top of the window.
func (m *Model) ensureBlockVisible(id string) {
	offset, ok := m.blockLineOffset(id)
	if !ok {
		return
	}
	m.scrollTranscriptTo(offset)
}

// scrollAnchor captures the topmost-visible block and the reader's line delta
// into it, so a reflow (expand/collapse, resize) can restore their view.
func (m Model) scrollAnchor() (string, int, bool) {
	if m.followTail {
		return "", 0, false
	}
	viewH, width := m.transcriptRegionHeight()
	_ = viewH
	blocks := m.projection.Blocks
	y := 0
	placed := false
	for _, b := range blocks {
		lb := m.layoutBlock(b, width, false)
		if lb.height == 0 {
			continue
		}
		if placed {
			y += blockGap
		}
		if y+lb.height > m.scrollLine {
			return b.ID, m.scrollLine - y, true
		}
		placed = true
		y += lb.height
	}
	return "", 0, false
}

// restoreScrollAnchor re-anchors the window after a reflow so the block the
// reader was looking at stays where it was.
func (m *Model) restoreScrollAnchor(id string, delta int) {
	offset, ok := m.blockLineOffset(id)
	if !ok {
		return
	}
	m.scrollTranscriptTo(offset + delta)
}
