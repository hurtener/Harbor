package app

import (
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/tui/markdown"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

func TestZZWrapPlain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		width int
	}{
		{"simple", "hello world", 20},
		{"long word", "supercalifragilisticexpialidocious", 8},
		{"multiline", "a\n\nb", 10},
		{"empty", "", 10},
		{"exact", "abcd efgh", 4},
		{"width1", "hello world", 1},
	} {
		done := make(chan []string, 1)
		go func() { done <- wrapPlain(tc.text, tc.width) }()
		select {
		case got := <-done:
			fmt.Printf("wrapPlain %-12s -> %q\n", tc.name, got)
		case <-timeoutAfter():
			t.Fatalf("wrapPlain HUNG on %s", tc.name)
		}
	}
}

func TestZZMarkdown(t *testing.T) {
	theme := ui.NewTheme(ui.ModeDark, ui.ProfileMono)
	for _, tc := range []struct {
		name  string
		src   string
		width int
	}{
		{"plain", "hello world", 40},
		{"bold", "a **bold** claim", 40},
		{"list", "- one\n- two", 40},
		{"para+bold", "Here's one:\n\n**Why did the CPU go to therapy?** Because it had too many `hanging threads`.", 40},
		{"narrow", "some text that is long", 5},
	} {
		done := make(chan []string, 1)
		go func() { done <- markdown.Render(theme, tc.src, tc.width, ui.RoleText, 3) }()
		select {
		case got := <-done:
			fmt.Printf("markdown %-12s -> %d lines\n", tc.name, len(got))
		case <-timeoutAfter():
			t.Fatalf("markdown.Render HUNG on %s", tc.name)
		}
	}
}

func timeoutAfter() <-chan struct{} {
	ch := make(chan struct{})
	go func() { time.Sleep(3 * time.Second); close(ch) }()
	return ch
}
