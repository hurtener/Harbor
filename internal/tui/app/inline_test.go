package app

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/conversation"
	"github.com/hurtener/Harbor/internal/tui/projection"
	"github.com/hurtener/Harbor/internal/tui/ui"
)

// TestInline_FlushAndLiveRegion pins the inline conversation contract: final
// blocks flush once, in order, into the terminal's normal buffer (user box,
// collapsed thought, answer, per-turn anchor); streaming blocks stay in the
// managed live region and never flush early; flushed content never re-renders.
func TestInline_FlushAndLiveRegion(t *testing.T) {
	now := time.Now()
	id := types.IdentityScope{Tenant: "dev", User: "dev", Session: "dev"}
	p := projection.Projection{Identity: id, SessionStatus: "running",
		Usage: projection.Usage{Model: "z-ai/glm-5.2", TotalTokens: 8900, PromptTokens: 8900, ContextWindow: 200000},
		Blocks: []projection.Block{
			{ID: "user:r1", Kind: "user", Text: "hey there!", At: now, RunID: "r1"},
			{ID: "reasoning:r1", Kind: "reasoning", Text: "Simple greeting, respond warmly.", At: now, RunID: "r1"},
			{ID: "text:r1", Kind: "text", Text: "Hey! **How can I help?**", At: now, RunID: "r1"},
			{ID: "task:r1", Kind: "task", Status: "completed", DurationMS: 3200, RunID: "r1", At: now},
		}}
	m := NewOperationalModel(100, 30, ui.NewTheme(ui.ModeLight, ui.ProfileTrueColor), true, p)
	rm := RuntimeModel{shell: m, transcript: conversation.NewTranscript(p), flushed: map[string]bool{}, anchored: map[string]bool{}}
	rm.shell.state.Route, rm.shell.state.Connection, rm.shell.state.Composer = "session", "live", ComposerFocused
	rm.shell.state.Model = "z-ai/glm-5.2"
	rm.shell.startup = startupHidden

	units := rm.collectFlushes()
	if len(units) != 4 {
		t.Fatalf("expected 4 flush units (user, thought, answer, anchor), got %d", len(units))
	}
	joined := ansi.Strip(strings.Join(units, "\n"))
	for _, want := range []string{"hey there!", "▸  Thought", "How can I help?", "▣ z-ai/glm-5.2  ·  3.2s"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("flush missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "respond warmly") {
		t.Fatal("collapsed thought leaked its body into scrollback")
	}
	if idxUser, idxAnchor := strings.Index(joined, "hey there!"), strings.Index(joined, "▣"); idxUser > idxAnchor {
		t.Fatal("flush order violated: anchor before user turn")
	}
	// second call must be a no-op
	if again := rm.collectFlushes(); len(again) != 0 {
		t.Fatalf("re-flush produced %d units", len(again))
	}

	// streaming: incomplete text block stays live
	p2 := p
	p2.Blocks = append(append([]projection.Block(nil), p.Blocks...), projection.Block{ID: "text:r2", Kind: "text", Text: "Streaming now", Incomplete: true, RunID: "r2", At: now})
	rm.transcript = conversation.NewTranscript(p2)
	rm.shell.state.Composer = ComposerRunning
	if units := rm.collectFlushes(); len(units) != 0 {
		t.Fatalf("incomplete block flushed: %d", len(units))
	}
	content, cx, cy := rm.inlineView()
	if cx <= 0 || cy <= 0 {
		t.Fatalf("cursor not positioned: (%d,%d)", cx, cy)
	}
	if !strings.Contains(content, "Streaming now") {
		t.Fatal("live streaming block missing from managed area")
	}
	if strings.Contains(ansi.Strip(content), "hey there!") {
		t.Fatal("flushed block leaked into managed area")
	}
}
