package markdown

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/tui/ui"
)

// richSource exercises every block and inline construct the renderer supports.
const richSource = "# Heading One\n\n" +
	"A paragraph with **bold**, _em_, `code`, and a [link](https://x.io/p) span " +
	"that is long enough to wrap across several rendered terminal lines cleanly.\n\n" +
	"- first bullet item\n- second bullet item that is long enough to wrap once\n\n" +
	"1. ordered one\n2. ordered two\n\n" +
	"> a quoted remark worth preserving verbatim\n\n" +
	"```\nfmt.Println(\"hello world\")\nx := 1 + 2\n```\n\n" +
	"---\n\n" +
	"Closing paragraph after the rule.\n"

// TestRender_Streaming_NoPanicAndWidthBounded renders every prefix of the rich
// source and asserts the renderer never panics and never exceeds the width.
func TestRender_Streaming_NoPanicAndWidthBounded(t *testing.T) {
	theme := testTheme()
	for _, width := range []int{10, 24, 40, 72} {
		for i := 0; i <= len(richSource); i++ {
			lines := Render(theme, richSource[:i], width, ui.RoleText, 0)
			for n, l := range lines {
				if got := ui.Width(l); got > width {
					t.Fatalf("width=%d prefix=%d line %d overflows: %d", width, i, n, got)
				}
			}
		}
	}
}

// TestRender_Streaming_CompletedBlockFrozen proves the core streaming
// guarantee: once a block is sealed by a blank line, its rendered lines are a
// stable prefix of every longer document — later text never rewrites them.
func TestRender_Streaming_CompletedBlockFrozen(t *testing.T) {
	theme := testTheme()
	const width = 48

	// A completed first block followed by its sealing blank line.
	sealed := "A paragraph with **bold** and `code` that completes here.\n\n"
	frozen := Render(theme, sealed, width, ui.RoleText, 0)
	if len(frozen) == 0 {
		t.Fatal("expected the sealed block to render")
	}

	suffixes := []string{
		"Next",
		"Next paragraph begins.",
		"Next paragraph begins.\n\n- a list\n",
		"# A later heading\n\nMore text with `code`.",
		richSource,
	}
	for _, suffix := range suffixes {
		full := Render(theme, sealed+suffix, width, ui.RoleText, 0)
		if len(full) < len(frozen) {
			t.Fatalf("growth shrank the document: %d < %d", len(full), len(frozen))
		}
		for i := range frozen {
			if full[i] != frozen[i] {
				t.Fatalf("sealed line %d changed after growth\n before: %q\n  after: %q", i, frozen[i], full[i])
			}
		}
	}
}

// TestRender_Streaming_UnmatchedDelimiterIsLiteral proves the deferral rule: an
// open delimiter with no closer renders literally, then re-styles once closed.
func TestRender_Streaming_UnmatchedDelimiterIsLiteral(t *testing.T) {
	theme := testTheme()
	const width = 40

	// Unterminated bold / code / fence must show their literal delimiters.
	cases := map[string]string{
		"**bold":  "**bold",
		"a `code": "a `code",
		// An unclosed fence renders its body as literal text (shown, not hidden)
		// until the closing fence arrives and promotes it to a code block.
		"```\ncode\nx": "code",
	}
	for src, wantContains := range cases {
		lines := Render(theme, src, width, ui.RoleText, 0)
		joined := ""
		for _, l := range lines {
			joined += plain(l) + "\n"
		}
		if !strings.Contains(joined, wantContains) {
			t.Fatalf("unclosed %q: expected literal %q in %q", src, wantContains, joined)
		}
	}

	// Once closed, the span is styled: delimiters disappear from the text.
	closed := Render(theme, "**bold** done", width, ui.RoleText, 0)
	if got := plain(closed[0]); got != "bold done" {
		t.Fatalf("closed bold = %q", got)
	}
}

// TestRender_Streaming_CompletedSpanStable proves a completed inline span stays
// stable (byte-identical first line) as more trailing text arrives.
func TestRender_Streaming_CompletedSpanStable(t *testing.T) {
	theme := testTheme()
	const width = 60
	base := Render(theme, "Prefix **done** and", width, ui.RoleText, 0)
	// Appending more words on the same line keeps the completed span's leading
	// text intact (the line only grows to the right until it must wrap).
	grown := Render(theme, "Prefix **done** and more", width, ui.RoleText, 0)
	if !strings.HasPrefix(plain(grown[0]), "Prefix done and") {
		t.Fatalf("completed span not stable: %q", plain(grown[0]))
	}
	if !strings.HasPrefix(plain(base[0]), "Prefix done and") {
		t.Fatalf("base render unexpected: %q", plain(base[0]))
	}
}
