package app

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/hurtener/Harbor/internal/tui/ui"
)

type cell struct {
	text         string
	style        lipgloss.Style
	key          string
	continuation bool
}
type canvas struct {
	width, height int
	rows          [][]cell
}

func newCanvas(width, height int) canvas {
	rows := make([][]cell, height)
	for y := range rows {
		rows[y] = make([]cell, width)
		for x := range rows[y] {
			rows[y][x].text = " "
			rows[y][x].key = styleKey(lipgloss.NewStyle())
		}
	}
	return canvas{width: width, height: height, rows: rows}
}
func (c *canvas) fill(style lipgloss.Style) {
	for y := range c.rows {
		for x := range c.rows[y] {
			c.rows[y][x].style = style
			c.rows[y][x].key = styleKey(style)
		}
	}
}
func (c *canvas) put(x, y int, text string, style lipgloss.Style) {
	if y < 0 || y >= c.height || x >= c.width {
		return
	}
	text = ui.Truncate(text, max(0, c.width-max(0, x)))
	for _, cluster := range ui.Clusters(text) {
		if x < 0 {
			x += cluster.Width
			continue
		}
		if x+cluster.Width > c.width {
			break
		}
		if c.rows[y][x].continuation && x > 0 {
			c.rows[y][x-1] = cell{text: " ", style: style, key: styleKey(style)}
		}
		c.rows[y][x] = cell{text: cluster.Text, style: style, key: styleKey(style)}
		for offset := 1; offset < cluster.Width; offset++ {
			c.rows[y][x+offset] = cell{style: style, key: styleKey(style), continuation: true}
		}
		x += cluster.Width
		if x >= c.width {
			break
		}
	}
}

func (c *canvas) dim(x, width int, style lipgloss.Style) {
	start, end := max(0, x), min(c.width, x+width)
	for y := range c.rows {
		for column := start; column < end; column++ {
			c.rows[y][column].style = style
			c.rows[y][column].key = styleKey(style)
		}
	}
}
func (c *canvas) styledBlock(x, y int, block string, body, rule lipgloss.Style) {
	for i, line := range strings.Split(ansi.Strip(block), "\n") {
		if strings.HasPrefix(line, "┃") || strings.HasPrefix(line, "╹") {
			c.put(x, y+i, string([]rune(line)[0]), rule)
			c.put(x+1, y+i, string([]rune(line)[1:]), body)
			continue
		}
		c.put(x, y+i, line, body)
	}
}

// blit copies every cell of src onto c at (x, y), clipping to c's bounds. It is
// how a windowed region (the scrolled transcript) rendered into its own
// sub-canvas lands on the screen canvas.
func (c *canvas) blit(src canvas, x, y int) {
	for sy := range src.rows {
		dy := y + sy
		if dy < 0 || dy >= c.height {
			continue
		}
		for sx := range src.rows[sy] {
			dx := x + sx
			if dx < 0 || dx >= c.width {
				continue
			}
			c.rows[dy][dx] = src.rows[sy][sx]
		}
	}
}

// rowsTrimmed serializes each row with trailing unstyled blanks removed, for
// content that flows into the terminal's normal buffer: full-width padded rows
// would pollute native selection and copy with trailing spaces.
func (c canvas) rowsTrimmed() []string {
	blankKey := styleKey(lipgloss.NewStyle())
	rows := make([]string, c.height)
	for y := range c.rows {
		end := 0
		for x := 0; x < c.width; x++ {
			entry := c.rows[y][x]
			if entry.continuation {
				continue
			}
			if entry.text != " " || entry.key != blankKey {
				end = x + 1
			}
		}
		var b strings.Builder
		var last lipgloss.Style
		lastKey := ""
		var run strings.Builder
		flush := func() {
			if run.Len() > 0 {
				b.WriteString(last.Render(run.String()))
				run.Reset()
			}
		}
		for x := 0; x < end; x++ {
			entry := c.rows[y][x]
			if entry.continuation {
				continue
			}
			if entry.key != lastKey {
				flush()
				last = entry.style
				lastKey = entry.key
			}
			run.WriteString(entry.text)
		}
		flush()
		rows[y] = b.String()
	}
	return rows
}

func (c canvas) string() string {
	rows := make([]string, c.height)
	for y := range c.rows {
		var b strings.Builder
		var last lipgloss.Style
		lastKey := ""
		var run strings.Builder
		flush := func() {
			if run.Len() > 0 {
				b.WriteString(last.Render(run.String()))
				run.Reset()
			}
		}
		for x := range c.width {
			entry := c.rows[y][x]
			if entry.continuation {
				continue
			}
			if entry.key != lastKey {
				flush()
				last = entry.style
				lastKey = entry.key
			}
			run.WriteString(entry.text)
		}
		flush()
		rows[y] = b.String()
	}
	return strings.Join(rows, "\n")
}

func styleKey(style lipgloss.Style) string {
	return fmt.Sprintf("%v|%v|%t|%t|%t|%t", style.GetForeground(), style.GetBackground(), style.GetBold(), style.GetFaint(), style.GetUnderline(), style.GetReverse())
}
