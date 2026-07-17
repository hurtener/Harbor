package markdown

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/tui/ui"
)

// spanSig is a comparable signature for a span: its plain text, its width, and
// the styling it resolves to. lipgloss.Style has unexported state, so the style
// is compared by what it renders.
func spanSig(s Span) string {
	return fmt.Sprintf("%q|%d|%q", s.Text, s.Width, s.Style.Render("x"))
}

// lineSig is a comparable signature for one line of spans.
func lineSig(spans []Span) string {
	parts := make([]string, 0, len(spans))
	for _, s := range spans {
		parts = append(parts, spanSig(s))
	}
	return strings.Join(parts, "+")
}

// lineWidth sums the visible width of a line's spans.
func lineWidth(spans []Span) int {
	w := 0
	for _, s := range spans {
		w += s.Width
	}
	return w
}

// assertPlainSpans is the canvas-safety gate: span text must carry no escape
// sequences, and its declared width must match the ui width authority. A styled
// string here would hang ui.Clusters inside the canvas, so ESC is checked
// before any width call.
func assertPlainSpans(t *testing.T, lines [][]Span) {
	t.Helper()
	for i, spans := range lines {
		for j, s := range spans {
			if strings.ContainsRune(s.Text, 0x1b) {
				t.Fatalf("line %d span %d contains an escape sequence: %q", i, j, s.Text)
			}
			if got := ui.Width(s.Text); got != s.Width {
				t.Fatalf("line %d span %d width = %d, want ui.Width = %d (%q)", i, j, s.Width, got, s.Text)
			}
		}
	}
}

func TestRenderSpans_PlainTextAndWidthBudget(t *testing.T) {
	theme := testTheme()
	for _, width := range []int{12, 24, 40, 72} {
		lines := RenderSpans(theme, richSource, width, ui.RoleText, 0)
		assertPlainSpans(t, lines)
		for i, spans := range lines {
			if got := lineWidth(spans); got > width {
				t.Fatalf("width=%d line %d sums to %d", width, i, got)
			}
		}
	}
}

// TestRenderSpans_SurvivesUIClusters is the regression gate for the canvas
// integration: the canvas re-splits span text with ui.Clusters, which loops
// forever on escape-carrying input. Plain span text must round-trip.
func TestRenderSpans_SurvivesUIClusters(t *testing.T) {
	lines := RenderSpans(testTheme(), richSource, 40, ui.RoleText, 2)
	assertPlainSpans(t, lines) // proves no ESC before we hand text to Clusters
	for i, spans := range lines {
		for j, s := range spans {
			total := 0
			for _, c := range ui.Clusters(s.Text) {
				total += c.Width
			}
			if total != s.Width {
				t.Fatalf("line %d span %d: clusters sum %d, want %d (%q)", i, j, total, s.Width, s.Text)
			}
		}
	}
}

func TestRenderSpans_NoTrailingPadding(t *testing.T) {
	lines := RenderSpans(testTheme(), "hi", 40, ui.RoleText, 0)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	if got := lineWidth(lines[0]); got != 2 {
		t.Fatalf("line width = %d, want 2 (no right-fill)", got)
	}
	var text string
	for _, s := range lines[0] {
		text += s.Text
	}
	if text != "hi" {
		t.Fatalf("line text = %q, want %q", text, "hi")
	}
}

func TestRenderSpans_BlankSeparatorHasNoSpans(t *testing.T) {
	lines := RenderSpans(testTheme(), "First para.\n\nSecond para.", 40, ui.RoleText, 0)
	if len(lines) != 3 {
		t.Fatalf("want para+spacer+para = 3, got %d", len(lines))
	}
	if len(lines[1]) != 0 {
		t.Fatalf("separator should emit no spans, got %d", len(lines[1]))
	}
}

func TestRenderSpans_IndentIsALeadingPlainSpan(t *testing.T) {
	const indent = 3
	lines := RenderSpans(testTheme(), "hello world", 30, ui.RoleText, indent)
	if len(lines[0]) == 0 {
		t.Fatal("expected spans")
	}
	first := lines[0][0]
	if first.Text != strings.Repeat(" ", indent) || first.Width != indent {
		t.Fatalf("leading span = %q (w=%d), want %d spaces", first.Text, first.Width, indent)
	}
}

// findSpan returns the span whose trimmed text equals want.
func findSpan(spans []Span, want string) *Span {
	for i := range spans {
		if strings.TrimSpace(spans[i].Text) == want {
			return &spans[i]
		}
	}
	return nil
}

func TestRenderSpans_BoldStyleDiffersFromBase(t *testing.T) {
	lines := RenderSpans(testTheme(), "This is **bold** text.", 40, ui.RoleText, 0)
	bold, base := findSpan(lines[0], "bold"), findSpan(lines[0], "This is")
	if bold == nil || base == nil {
		t.Fatalf("expected distinct bold and base spans, got %+v", lines[0])
	}
	if bold.Style.Render("x") == base.Style.Render("x") {
		t.Fatalf("bold span carries the same style as base: %q", bold.Style.Render("x"))
	}
}

func TestRenderSpans_CoalescesAdjacentSameStyle(t *testing.T) {
	// Same-style neighbours merge, so "This is " is one span rather than one
	// span per word or per grapheme. Only the bold run breaks the line up.
	lines := RenderSpans(testTheme(), "This is **bold** text.", 40, ui.RoleText, 0)
	if len(lines[0]) != 3 {
		got := make([]string, 0, len(lines[0]))
		for _, s := range lines[0] {
			got = append(got, s.Text)
		}
		t.Fatalf("want 3 coalesced spans, got %d: %q", len(lines[0]), got)
	}
}

// TestRenderSpans_CodeBackgroundDoesNotBleed guards the separator rule: the gap
// after an inline code span must not inherit the code background.
func TestRenderSpans_CodeBackgroundDoesNotBleed(t *testing.T) {
	lines := RenderSpans(testTheme(), "run `go` now", 40, ui.RoleText, 0)
	code := findSpan(lines[0], "go")
	if code == nil {
		t.Fatalf("expected a code span, got %+v", lines[0])
	}
	for _, s := range lines[0] {
		if s.Text == " " {
			continue // a bare plain separator is exactly what we want here
		}
		if strings.TrimSpace(s.Text) != "go" && s.Style.Render("x") == code.Style.Render("x") {
			t.Fatalf("code styling leaked into span %q", s.Text)
		}
	}
	// The code span itself must be exactly the code text, no padding.
	if code.Text != "go" {
		t.Fatalf("code span = %q, want %q", code.Text, "go")
	}
}

// TestRenderSpans_MatchesRenderText proves the two public APIs share a layout:
// same line count, and the same plain text per line once padding is discounted.
func TestRenderSpans_MatchesRenderText(t *testing.T) {
	theme := testTheme()
	const width = 44
	strLines := Render(theme, richSource, width, ui.RoleText, 2)
	spanLines := RenderSpans(theme, richSource, width, ui.RoleText, 2)
	if len(strLines) != len(spanLines) {
		t.Fatalf("line count drift: Render=%d RenderSpans=%d", len(strLines), len(spanLines))
	}
	for i := range strLines {
		var text string
		for _, s := range spanLines[i] {
			text += s.Text
		}
		if got, want := strings.TrimRight(plain(strLines[i]), " "), strings.TrimRight(text, " "); got != want {
			t.Fatalf("line %d text drift:\n Render: %q\n  Spans: %q", i, got, want)
		}
	}
}

func TestRenderSpans_EmptyAndZeroWidth(t *testing.T) {
	if got := RenderSpans(testTheme(), "", 40, ui.RoleText, 0); got != nil {
		t.Fatalf("empty source should render nothing, got %v", got)
	}
	if got := RenderSpans(testTheme(), "hi", 0, ui.RoleText, 0); got != nil {
		t.Fatalf("zero width should render nothing, got %v", got)
	}
}

// TestRenderSpans_Streaming_NoPanicAndWidthBounded mirrors the string
// renderer's streaming table over the span API.
func TestRenderSpans_Streaming_NoPanicAndWidthBounded(t *testing.T) {
	theme := testTheme()
	for _, width := range []int{10, 24, 40, 72} {
		for i := 0; i <= len(richSource); i++ {
			lines := RenderSpans(theme, richSource[:i], width, ui.RoleText, 1)
			for n, spans := range lines {
				if got := lineWidth(spans); got > width {
					t.Fatalf("width=%d prefix=%d line %d sums to %d", width, i, n, got)
				}
				for _, s := range spans {
					if strings.ContainsRune(s.Text, 0x1b) {
						t.Fatalf("width=%d prefix=%d line %d: escape in span %q", width, i, n, s.Text)
					}
				}
			}
		}
	}
}

// TestRenderSpans_Streaming_CompletedBlockFrozen is the span-API form of the
// core streaming guarantee: a block sealed by a blank line never re-renders.
func TestRenderSpans_Streaming_CompletedBlockFrozen(t *testing.T) {
	theme := testTheme()
	const width = 48
	sealed := "A paragraph with **bold** and `code` that completes here.\n\n"
	frozen := RenderSpans(theme, sealed, width, ui.RoleText, 0)
	if len(frozen) == 0 {
		t.Fatal("expected the sealed block to render")
	}
	for _, suffix := range []string{
		"Next",
		"Next paragraph begins.",
		"Next paragraph begins.\n\n- a list\n",
		"# A later heading\n\nMore text with `code`.",
		richSource,
	} {
		full := RenderSpans(theme, sealed+suffix, width, ui.RoleText, 0)
		if len(full) < len(frozen) {
			t.Fatalf("growth shrank the document: %d < %d", len(full), len(frozen))
		}
		for i := range frozen {
			if got, want := lineSig(full[i]), lineSig(frozen[i]); got != want {
				t.Fatalf("sealed line %d changed after growth\n before: %s\n  after: %s", i, want, got)
			}
		}
	}
}
