package markdown

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

// run is one inline span with its resolved emphasis flags. A span is only ever
// emitted with a flag set once its closing delimiter has been seen, which is
// what makes progressive rendering stable.
type run struct {
	text   string
	bold   bool
	italic bool
	code   bool
	link   bool
}

// cell is one grapheme cluster together with its display width and the style it
// renders under. Clusters are the atomic unit of width-aware wrapping.
type cell struct {
	text  string
	width int
	key   string
	style lipgloss.Style
}

// parseInline splits inline markdown into styled runs. It is deliberately
// lenient: an opening delimiter with no matching closer in s is emitted as
// literal text, so the rendered output of any prefix of a document is a stable
// extension of shorter prefixes.
func parseInline(s string) []run {
	var out []run
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			out = append(out, run{text: lit.String()})
			lit.Reset()
		}
	}
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				flush()
				out = append(out, run{text: s[i+1 : i+1+j], code: true})
				i = i + 1 + j + 1
				continue
			}
			lit.WriteByte(c)
			i++
		case c == '[':
			if text, next, ok := parseLink(s, i); ok {
				flush()
				for _, r := range parseInline(text) {
					r.link = true
					out = append(out, r)
				}
				i = next
				continue
			}
			lit.WriteByte(c)
			i++
		case (c == '*' || c == '_') && i+1 < len(s) && s[i+1] == c:
			delim := s[i : i+2]
			if j := findClose(s, i+2, delim, c == '_', i); j >= 0 {
				flush()
				for _, r := range parseInline(s[i+2 : j]) {
					r.bold = true
					out = append(out, r)
				}
				i = j + 2
				continue
			}
			lit.WriteString(delim)
			i += 2
		case c == '*' || c == '_':
			if j := findCloseByte(s, i+1, c, c == '_', i); j >= 0 {
				flush()
				for _, r := range parseInline(s[i+1 : j]) {
					r.italic = true
					out = append(out, r)
				}
				i = j + 1
				continue
			}
			lit.WriteByte(c)
			i++
		default:
			lit.WriteByte(c)
			i++
		}
	}
	flush()
	return out
}

// parseLink matches a [text](url) span starting at s[i]=='['. The URL is parsed
// but discarded; only the link text is kept for display.
func parseLink(s string, i int) (text string, next int, ok bool) {
	j := i + 1
	for j < len(s) && s[j] != ']' {
		j++
	}
	if j >= len(s) || j+1 >= len(s) || s[j+1] != '(' {
		return "", 0, false
	}
	k := j + 2
	for k < len(s) && s[k] != ')' {
		k++
	}
	if k >= len(s) {
		return "", 0, false
	}
	return s[i+1 : j], k + 1, true
}

// findClose locates the closing two-byte delimiter at or after start, honoring
// underscore word-boundary flanking so identifiers like snake_case are not
// mistaken for emphasis. It returns -1 when no valid closer exists.
func findClose(s string, start int, delim string, underscore bool, openIdx int) int {
	if underscore && !boundaryBefore(s, openIdx) {
		return -1
	}
	for k := start; k+len(delim) <= len(s); k++ {
		if s[k:k+len(delim)] == delim {
			if underscore && !boundaryAfter(s, k+len(delim)) {
				continue
			}
			if k == start {
				return -1
			}
			return k
		}
	}
	return -1
}

// findCloseByte is the single-byte-delimiter form of findClose.
func findCloseByte(s string, start int, ch byte, underscore bool, openIdx int) int {
	if underscore && !boundaryBefore(s, openIdx) {
		return -1
	}
	for k := start; k < len(s); k++ {
		if s[k] == ch {
			if underscore && !boundaryAfter(s, k+1) {
				continue
			}
			if k == start {
				continue
			}
			return k
		}
	}
	return -1
}

// boundaryBefore reports whether the byte before i is a word boundary.
func boundaryBefore(s string, i int) bool { return i == 0 || !isAlnum(s[i-1]) }

// boundaryAfter reports whether the byte at i is a word boundary.
func boundaryAfter(s string, i int) bool { return i >= len(s) || !isAlnum(s[i]) }

// isAlnum reports whether b is an ASCII letter or digit.
func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// layoutInline expands runs into per-grapheme styled cells. base is the default
// text role; forceBold promotes every non-code run (used for headings).
func layoutInline(theme ui.Theme, runs []run, base ui.Role, forceBold bool) []cell {
	var cells []cell
	for _, r := range runs {
		style, key := runStyle(theme, r, base, forceBold)
		for _, cl := range ui.Clusters(r.text) {
			cells = append(cells, cell{text: cl.Text, width: cl.Width, key: key, style: style})
		}
	}
	return cells
}

// runStyle resolves the lipgloss style and a comparable merge key for one run.
func runStyle(theme ui.Theme, r run, base ui.Role, forceBold bool) (lipgloss.Style, string) {
	if r.code {
		bg := ui.RoleElement
		return theme.Style(ui.RoleMuted, &bg), "code"
	}
	role := base
	if r.link {
		role = ui.RoleAccent
	}
	bold := r.bold || forceBold
	if bold {
		role = ui.RolePrimary
	}
	style := theme.Style(role, nil)
	if bold {
		style = style.Bold(true)
	}
	if r.italic {
		style = style.Italic(true)
	}
	return style, fmt.Sprintf("%d-%t-%t-%t", role, bold, r.italic, r.link)
}
