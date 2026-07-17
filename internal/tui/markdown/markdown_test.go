package markdown

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/hurtener/Harbor/internal/tui/ui"
)

func testTheme() ui.Theme { return ui.NewTheme(ui.ModeDark, ui.ProfileTrueColor) }

// plain strips ANSI escapes and trailing pad from a rendered line.
func plain(line string) string { return strings.TrimRight(ansi.Strip(line), " ") }

// assertExactWidth requires every line to occupy exactly width visible cells.
func assertExactWidth(t *testing.T, lines []string, width int) {
	t.Helper()
	for i, l := range lines {
		if got := ui.Width(l); got != width {
			t.Fatalf("line %d width = %d, want %d: %q", i, got, width, ansi.Strip(l))
		}
	}
}

func TestRender_Paragraph_WrapsAndStripsBold(t *testing.T) {
	lines := Render(testTheme(), "This is **bold** text.", 40, ui.RoleText, 0)
	assertExactWidth(t, lines, 40)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	if got := plain(lines[0]); got != "This is bold text." {
		t.Fatalf("plain = %q", got)
	}
}

func TestRender_Paragraph_WrapsLongText(t *testing.T) {
	src := "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda"
	width := 20
	lines := Render(testTheme(), src, width, ui.RoleText, 0)
	if len(lines) < 2 {
		t.Fatalf("expected wrapping, got %d line(s)", len(lines))
	}
	assertExactWidth(t, lines, width)
	// Round-trips to the same words joined by single spaces.
	var words []string
	for _, l := range lines {
		words = append(words, strings.Fields(plain(l))...)
	}
	if got := strings.Join(words, " "); got != src {
		t.Fatalf("reflow lost content: %q", got)
	}
}

func TestRender_Heading_StripsHashes(t *testing.T) {
	lines := Render(testTheme(), "## The Title", 40, ui.RoleText, 0)
	assertExactWidth(t, lines, 40)
	if got := plain(lines[0]); got != "The Title" {
		t.Fatalf("heading plain = %q", got)
	}
}

func TestRender_InlineCode(t *testing.T) {
	lines := Render(testTheme(), "run `go test` now", 40, ui.RoleText, 0)
	if got := plain(lines[0]); got != "run go test now" {
		t.Fatalf("plain = %q", got)
	}
	if strings.Contains(plain(lines[0]), "`") {
		t.Fatalf("backticks leaked into output")
	}
}

func TestRender_UnorderedList(t *testing.T) {
	lines := Render(testTheme(), "- one\n- two\n- three", 40, ui.RoleText, 0)
	assertExactWidth(t, lines, 40)
	if len(lines) != 3 {
		t.Fatalf("want 3 items, got %d", len(lines))
	}
	for i, want := range []string{"• one", "• two", "• three"} {
		if got := plain(lines[i]); got != want {
			t.Fatalf("item %d = %q, want %q", i, got, want)
		}
	}
}

func TestRender_OrderedList(t *testing.T) {
	lines := Render(testTheme(), "1. first\n2. second", 40, ui.RoleText, 0)
	if got := plain(lines[0]); got != "1. first" {
		t.Fatalf("ordered[0] = %q", got)
	}
	if got := plain(lines[1]); got != "2. second" {
		t.Fatalf("ordered[1] = %q", got)
	}
}

func TestRender_ListHangingIndent(t *testing.T) {
	src := "- alpha beta gamma delta epsilon zeta eta theta"
	width := 20
	lines := Render(testTheme(), src, width, ui.RoleText, 0)
	if len(lines) < 2 {
		t.Fatalf("expected wrapped list item, got %d", len(lines))
	}
	assertExactWidth(t, lines, width)
	if !strings.HasPrefix(plain(lines[0]), "• ") {
		t.Fatalf("first line missing bullet: %q", plain(lines[0]))
	}
	// Continuation is indented under the text (marker width = 2 spaces).
	if !strings.HasPrefix(ansi.Strip(lines[1]), "  ") {
		t.Fatalf("continuation not hanging-indented: %q", ansi.Strip(lines[1]))
	}
}

func TestRender_Blockquote(t *testing.T) {
	lines := Render(testTheme(), "> quoted line", 40, ui.RoleText, 0)
	assertExactWidth(t, lines, 40)
	if got := plain(lines[0]); got != "▏ quoted line" {
		t.Fatalf("quote = %q", got)
	}
}

func TestRender_HorizontalRule(t *testing.T) {
	width := 30
	lines := Render(testTheme(), "---", width, ui.RoleText, 0)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	assertExactWidth(t, lines, width)
	if got := plain(lines[0]); got != strings.Repeat("─", width) {
		t.Fatalf("hr = %q", got)
	}
}

func TestRender_FencedCodeBlock(t *testing.T) {
	src := "```go\nfmt.Println(\"hi\")\n```"
	width := 40
	lines := Render(testTheme(), src, width, ui.RoleText, 0)
	if len(lines) != 1 {
		t.Fatalf("want 1 code line, got %d", len(lines))
	}
	assertExactWidth(t, lines, width)
	got := plain(lines[0])
	if !strings.HasPrefix(got, "▏ ") {
		t.Fatalf("code gutter missing: %q", got)
	}
	if !strings.Contains(got, `fmt.Println("hi")`) {
		t.Fatalf("code not verbatim: %q", got)
	}
}

func TestRender_CodeBlockTruncatesWide(t *testing.T) {
	long := strings.Repeat("x", 100)
	lines := Render(testTheme(), "```\n"+long+"\n```", 20, ui.RoleText, 0)
	assertExactWidth(t, lines, 20)
	if !strings.HasSuffix(plain(lines[0]), "…") {
		t.Fatalf("expected ellipsis on truncated code: %q", plain(lines[0]))
	}
}

func TestRender_Link_DropsURL(t *testing.T) {
	lines := Render(testTheme(), "see [the docs](https://example.com/x) now", 60, ui.RoleText, 0)
	got := plain(lines[0])
	if got != "see the docs now" {
		t.Fatalf("link render = %q", got)
	}
	if strings.Contains(got, "example.com") {
		t.Fatalf("URL leaked: %q", got)
	}
}

func TestRender_Indent(t *testing.T) {
	width, indent := 30, 4
	lines := Render(testTheme(), "hello world", width, ui.RoleText, indent)
	assertExactWidth(t, lines, width)
	if !strings.HasPrefix(ansi.Strip(lines[0]), strings.Repeat(" ", indent)+"hello") {
		t.Fatalf("indent not applied: %q", ansi.Strip(lines[0]))
	}
}

func TestRender_MultipleBlocksSpacer(t *testing.T) {
	lines := Render(testTheme(), "First para.\n\nSecond para.", 40, ui.RoleText, 0)
	if len(lines) != 3 {
		t.Fatalf("want para+spacer+para = 3, got %d", len(lines))
	}
	if plain(lines[1]) != "" {
		t.Fatalf("middle line should be a blank spacer: %q", plain(lines[1]))
	}
	assertExactWidth(t, lines, 40)
}

func TestRender_Underscore_DoesNotEmphasizeIdentifiers(t *testing.T) {
	lines := Render(testTheme(), "call snake_case_name here", 40, ui.RoleText, 0)
	if got := plain(lines[0]); got != "call snake_case_name here" {
		t.Fatalf("intraword underscore mangled: %q", got)
	}
}

func TestRender_Emphasis(t *testing.T) {
	lines := Render(testTheme(), "an _emphasized_ word", 40, ui.RoleText, 0)
	if got := plain(lines[0]); got != "an emphasized word" {
		t.Fatalf("emphasis render = %q", got)
	}
}

func TestRender_Grapheme_CJKAndEmoji(t *testing.T) {
	// East-asian and emoji content must never overflow the width budget.
	for _, width := range []int{6, 12, 24, 40} {
		lines := Render(testTheme(), "café 日本語 テスト 🚀 done **bold**", width, ui.RoleText, 0)
		for i, l := range lines {
			if got := ui.Width(l); got > width {
				t.Fatalf("width=%d line %d overflows: %d (%q)", width, i, got, ansi.Strip(l))
			}
		}
	}
}

func TestRender_EmptyAndZeroWidth(t *testing.T) {
	if got := Render(testTheme(), "", 40, ui.RoleText, 0); got != nil {
		t.Fatalf("empty source should render nothing, got %v", got)
	}
	if got := Render(testTheme(), "hi", 0, ui.RoleText, 0); got != nil {
		t.Fatalf("zero width should render nothing, got %v", got)
	}
}

func TestRender_MonoProfile(t *testing.T) {
	// Mono profile styles via attributes; content and geometry must still hold.
	theme := ui.NewTheme(ui.ModeDark, ui.ProfileMono)
	lines := Render(theme, "**bold** and `code`", 40, ui.RoleText, 0)
	assertExactWidth(t, lines, 40)
	if got := plain(lines[0]); got != "bold and code" {
		t.Fatalf("mono plain = %q", got)
	}
}
