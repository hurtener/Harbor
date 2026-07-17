package app

import (
	"encoding/binary"
	"hash/fnv"
	"strings"

	"github.com/hurtener/Harbor/internal/tui/projection"
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

// layoutCacheEntry memoizes one block's laid-out visual against a content
// signature, so scrolling a long conversation costs map lookups instead of a
// full markdown re-render of every block on every frame.
type layoutCacheEntry struct {
	sig  uint64
	laid laidBlock
}

// blockSig fingerprints everything a block's layout depends on. Any change —
// content, status, width, selection, theme — misses the cache and re-lays out
// exactly that block.
func (m Model) blockSig(b projection.Block, width int, selected bool) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(b.Kind))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(b.Status))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(b.Tool))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(b.Text))
	var meta [8]byte
	binary.LittleEndian.PutUint32(meta[:4], uint32(width))
	flags := byte(0)
	if b.Incomplete {
		flags |= 1
	}
	if selected {
		flags |= 2
	}
	meta[4] = flags
	meta[5] = byte(m.theme.Profile())
	meta[6] = byte(m.theme.Mode())
	_, _ = h.Write(meta[:7])
	return h.Sum64()
}

// layoutBlockCached is the single layout entry point for the session surface.
// The cache map is shared across the model's value copies deliberately: the
// Bubble Tea update/view loop is single-goroutine and entries are idempotent
// (same signature ⇒ same layout), so sharing is a pure win.
func (m Model) layoutBlockCached(b projection.Block, width int, selected bool) laidBlock {
	if m.layoutCache == nil {
		return m.layoutBlock(b, width, selected)
	}
	sig := m.blockSig(b, width, selected)
	if entry, ok := m.layoutCache[b.ID]; ok && entry.sig == sig {
		return entry.laid
	}
	laid := m.layoutBlock(b, width, selected)
	m.layoutCache[b.ID] = layoutCacheEntry{sig: sig, laid: laid}
	return laid
}

// layoutTranscript lays out every visible block at the given width through the
// cache, returning the laid blocks, their ids, each block's starting line
// offset, and the total line height including inter-block gaps. The per-turn
// anchor is appended to the newest text block here, keeping layoutBlock pure.
func (m Model) layoutTranscript(width int) ([]laidBlock, []int, []string, int) {
	blocks := m.projection.Blocks
	if m.layoutCache != nil && len(m.layoutCache) > 2*len(blocks)+64 {
		// A session switch or big reconcile left stale entries behind.
		clear(m.layoutCache)
	}
	anchorID := ""
	if m.state.TurnStatus != "" {
		for i := len(blocks) - 1; i >= 0; i-- {
			if blocks[i].Kind == "text" {
				anchorID = blocks[i].ID
				break
			}
		}
	}
	laid := make([]laidBlock, 0, len(blocks))
	offsets := make([]int, 0, len(blocks))
	ids := make([]string, 0, len(blocks))
	y := 0
	for _, b := range blocks {
		lb := m.layoutBlockCached(b, width, b.ID != "" && b.ID == m.state.SelectedBlockID)
		if lb.height == 0 {
			continue
		}
		if b.ID == anchorID && !b.Incomplete {
			lb = m.withTurnAnchor(lb, width, m.state.TurnStatus)
		}
		if y > 0 {
			y += blockGap
		}
		laid = append(laid, lb)
		offsets = append(offsets, y)
		ids = append(ids, b.ID)
		y += lb.height
	}
	return laid, offsets, ids, y
}

// blockLineOffset reports the starting transcript line of a block. Used by
// semantic navigation (block jump, search) to scroll the window to a target.
func (m Model) blockLineOffset(id string) (int, bool) {
	_, width := m.transcriptRegionHeight()
	_, offsets, ids, _ := m.layoutTranscript(width)
	for i, blockID := range ids {
		if blockID == id {
			return offsets[i], true
		}
	}
	return 0, false
}

// transcriptRegionHeight computes the transcript window's height and content
// width exactly as renderBase lays the session surface out, so scroll math and
// rendering can never disagree.
func (m Model) transcriptRegionHeight() (int, int) {
	top, bottom := m.transcriptRegion()
	return max(0, bottom-top+1), m.sessionContentWidth(m.Layout())
}

// sessionContentWidth is the conversation's content width: the full content
// width normally, narrowed only while the operator has joined the sidebar into
// the layout on a wide terminal.
func (m Model) sessionContentWidth(l ui.Layout) int {
	if m.state.SidebarOpen && l.JoinedSidebar {
		return max(12, l.MainWidth-1)
	}
	return max(12, l.ContentWidth-1)
}

// scrollTranscriptIfOverflow scrolls only when the conversation is taller than
// its window, reporting whether it did — one measurement for both the gate and
// the move, since arrow keys (and wheel-generated arrows) arrive in bursts.
func (m *Model) scrollTranscriptIfOverflow(deltaLines int) bool {
	viewH, width := m.transcriptRegionHeight()
	_, _, _, total := m.layoutTranscript(width)
	if total <= viewH {
		return false
	}
	maxScroll := total - viewH
	current := m.scrollLine
	if m.followTail {
		current = maxScroll
	}
	m.scrollLine = max(0, min(maxScroll, current+deltaLines))
	m.followTail = m.scrollLine >= maxScroll
	return true
}

// transcriptRegion returns the inclusive [top, bottom] rows of the transcript
// window on the session surface: below the banner, above the composer and its
// chrome (toast, status strip, intervention actions, one separator row).
func (m Model) transcriptRegion() (int, int) {
	bottom := m.composerTopRow() - 2
	if _, ok := m.workingLine(); ok {
		bottom--
	}
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
	laid, offsets, _, total := m.layoutTranscript(width)
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
	_, _, _, total := m.layoutTranscript(width)
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
	_, _, _, total := m.layoutTranscript(width)
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
	_, width := m.transcriptRegionHeight()
	laid, offsets, ids, _ := m.layoutTranscript(width)
	for i := range laid {
		if offsets[i]+laid[i].height > m.scrollLine {
			return ids[i], m.scrollLine - offsets[i], true
		}
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
