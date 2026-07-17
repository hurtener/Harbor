package app

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

func scrollTestModel(w, h, turns int) Model {
	now := time.Now()
	blocks := []projection.Block{}
	for i := range turns {
		blocks = append(blocks,
			projection.Block{ID: fmt.Sprintf("user:%d", i), Kind: "user", Text: fmt.Sprintf("question number %d", i), At: now},
			projection.Block{ID: fmt.Sprintf("text:%d", i), Kind: "text", Text: fmt.Sprintf("Answer %d.\n\nSecond paragraph %d.", i, i), At: now},
		)
	}
	id := types.IdentityScope{Tenant: "dev", User: "dev", Session: "dev"}
	m := NewOperationalModel(w, h, ui.NewTheme(ui.ModeDark, ui.ProfileMono), true, projection.Projection{Identity: id, SessionStatus: "running", Blocks: blocks})
	m.state.Route, m.state.Connection, m.state.Composer = "session", "live", ComposerFocused
	m.state.Model, m.state.Version, m.state.BaseURL = "model-x", "v1.14.0", "http://127.0.0.1:18080"
	m.startup = startupHidden
	return m
}

// TestSessionScroll_FollowsTailAndNeverYanks pins the transcript window
// contract: following shows the newest content; scrolling up holds the view
// steady while content grows below; End re-engages the tail.
func TestSessionScroll_FollowsTailAndNeverYanks(t *testing.T) {
	m := scrollTestModel(100, 24, 8)
	frame := ansi.Strip(m.Frame())
	if !strings.Contains(frame, "question number 7") {
		t.Fatalf("following window must show the newest turn:\n%s", frame)
	}
	if !strings.Contains(frame, "Harbor") || !strings.Contains(frame, "v1.14.0") {
		t.Fatal("banner missing")
	}

	m.scrollTranscript(-10)
	if m.followTail {
		t.Fatal("scrolling up must release tail-following")
	}
	scrolled := ansi.Strip(m.Frame())
	if !strings.Contains(scrolled, "more below") {
		t.Fatalf("scrolled-away view must advertise content below:\n%s", scrolled)
	}
	// New content appends while scrolled: the reader's line must not move.
	before := m.scrollLine
	extra := append([]projection.Block(nil), m.projection.Blocks...)
	extra = append(extra, projection.Block{ID: "text:new", Kind: "text", Text: "Fresh streamed content.", At: time.Now()})
	m.projection.Blocks = extra
	after := ansi.Strip(m.Frame())
	if m.scrollLine != before {
		t.Fatal("new content yanked a scrolled-away reader")
	}
	if strings.Contains(after, "Fresh streamed content.") {
		t.Fatal("content below the window leaked into a scrolled-away view")
	}

	m.scrollTranscriptTo(-1)
	if !m.followTail {
		t.Fatal("End must re-engage tail-following")
	}
	if tail := ansi.Strip(m.Frame()); !strings.Contains(tail, "Fresh streamed content.") {
		t.Fatal("tail view must show the newest content")
	}
}

// TestSessionScroll_ShortConversationTopFlows pins the Claude-Code short-
// conversation look: content starts under the banner, no scrolling.
func TestSessionScroll_ShortConversationTopFlows(t *testing.T) {
	m := scrollTestModel(100, 30, 1)
	frame := ansi.Strip(m.Frame())
	lines := strings.Split(frame, "\n")
	found := -1
	for i, line := range lines {
		if strings.Contains(line, "question number 0") {
			found = i
			break
		}
	}
	if found < 0 || found > bannerHeight+1 {
		t.Fatalf("short conversation must start directly under the banner (found at row %d)", found)
	}
	m.scrollTranscript(-5)
	if !m.followTail {
		t.Fatal("content shorter than the window has nothing to scroll")
	}
}

// TestSessionScroll_ReanchorsOnResize pins that a resize keeps a scrolled-away
// reader anchored to the block they were reading.
func TestSessionScroll_ReanchorsOnResize(t *testing.T) {
	m := scrollTestModel(100, 24, 8)
	m.scrollTranscript(-12)
	id, _, ok := m.scrollAnchor()
	if !ok || id == "" {
		t.Fatal("scrolled view must have an anchor block")
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	resized := next.(Model)
	if resized.followTail {
		t.Fatal("resize must not silently re-engage following for a scrolled reader")
	}
	offset, found := resized.blockLineOffset(id)
	if !found {
		t.Fatal("anchor block lost across resize")
	}
	viewH, _ := resized.transcriptRegionHeight()
	if resized.scrollLine < offset-viewH || resized.scrollLine > offset+viewH {
		t.Fatalf("resize lost the reader's place: scroll=%d anchor-offset=%d", resized.scrollLine, offset)
	}
}
